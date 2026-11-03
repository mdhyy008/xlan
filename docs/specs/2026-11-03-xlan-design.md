# xlan 设计文档

版本 0.1 · 2026-11-03

## 1. 目标

一个跨平台的局域网传输命令行工具，同时把「可被其他程序调用」当作一等需求。

- 内网发现：扫描其他设备，同时开放服务供别人扫描
- 私聊发文本、公共发文本、私聊发文件
- 任何工具（脚本、GUI、AI agent、以后的安卓 App）通过 `exec` + JSON，或直接订阅本地事件流即可复用，不必重造发现与传输

核心判断：**这套东西的价值不在「能传文件」，而在契约稳定**。所以输出的稳定性、退出码、事件流，和功能本身同等重要。

## 2. 非目标（第一版）

- 跨网中继、NAT 穿透
- 目录双向同步
- TUI 界面
- gomobile 壳（只保证 `core/` 不被写死成「只能当命令行用」）

## 3. 命名与路径

- 命令：`xlan`
- Linux/macOS：配置 `~/.config/xlan`，数据 `~/.local/share/xlan`，缓存 `~/.cache/xlan`
- Windows：`%APPDATA%\xlan`、`%LOCALAPPDATA%\xlan`
- 所有运行时文件（锁、日志、`.part`）都在上述目录，不写系统临时目录

## 4. 端口

TCP 与 UDP 的端口空间互相独立，可同号复用。

| 端口 | 协议 | 用途 | 是否可变 |
|---|---|---|---|
| 43001 | UDP | 发现广播（probe / announce） | 固定 |
| 43002 | UDP | 公共文本广播 + 回执 | 固定 |
| 43003 | TCP | HTTP API（首选） | 占用时依次 43004、43005 |

- TCP 实际端口写入锁文件，并放进 announce 包与 `/v1/info`，对方以此为准
- 不要求两端端口一致
- 只有常驻服务持有这些 socket，CLI 不直接绑定端口

## 5. 命令面

```
xlan scan                        发现局域网设备（先出缓存，再刷新）
xlan serve [--foreground]        前台跑服务
xlan send <路径> [--to 名字]      发文件，省略 --to = 群发
xlan text "内容" [--to 名字]      发文本，省略 --to = 公共
xlan inbox [--watch]             收件箱：文本、文件、待确认
xlan status                      本机服务状态、在线设备、端口
xlan trust add|rm|list           信任白名单
xlan doctor                      网络自检（端口、广播、防火墙）
```

全局参数：`--json`、`--quiet`、`--timeout`、`--config`、`--port`、`--as 名字`。

管道语义（CLI 最有价值的部分）：

- `cat a.zip | xlan send -`：从 stdin 流式发送
- `git log | xlan text -`：从 stdin 读文本
- `xlan recv <id> -o -`：输出到 stdout，可接 `| tar x`

## 6. 网络协议

**原则：所有网络动作由常驻服务执行，CLI 只是瘦客户端。** 避免两个命令同时抢同一个端口，也避免状态分裂。

### 6.1 发现（UDP 43001）

包格式（单条 JSON，≤ 512 字节）：

```json
{"v":1,"t":"probe","id":"<本机id>","name":"<本机名>"}
{"v":1,"t":"announce","id":"...","name":"...","port":43003,"platform":"linux","ts":1730000000}
```

- 服务启动、以及每 30 秒发一次 `announce`
- `xlan scan` 触发 `probe`，收到的人单播回 `announce`
- 广播同时发 `255.255.255.255` 和每个活动网卡的定向广播地址（如 `192.168.1.255`），因为部分系统/路由只认其中一种
- 结果写入 `peers.json`，TTL 5 分钟；`scan` 先立即输出缓存，再后台刷新
- 三层兜底：mDNS（`_xlan._tcp`）→ UDP 广播 → 子网 TCP 并发探测（受限网络、安卓 Termux 下只有第三层能用）

### 6.2 公共文本（UDP 43002）

包格式：

```json
{"v":1,"t":"msg","id":"<msgid>","sid":"<发送者id>","sname":"...","channel":"public","body":"...","ts":1730000000}
{"v":1,"t":"ack","id":"<msgid>","rid":"<接收者id>","rname":"..."}
```

- 正文上限 **1000 字节**（UTF-8，按 rune 边界截断判断），整包控制在 1200 字节内
- 发送方连发 2 遍，间隔 30ms
- 接收方按 `msgid` 去重，收到后单播回 `ack` 到源地址:43002
- 发送方等 600ms 收集回执，输出 `{"ok":true,"acked":["peer-a","peer-b"]}`
- 零回执时降级为警告：`{"ok":true,"acked":[],"warn":"no_ack"}`，退出码仍为 0（消息确实发出去了），但 stderr 提示可能被 AP 过滤，建议 `xlan doctor`
- **正文超过 1000 字节自动降级**：改为对所有在线设备逐一走 TCP 投递，`channel` 仍标 `public`，调用方无感

### 6.3 私聊文本与文件（TCP）

```
GET  /v1/info                    设备名、id、平台、能力、服务端口、指纹
POST /v1/offer                   发起传输（文件或文本），返回 offer_id
GET  /v1/offer/{id}              状态、已接收 offset（续传用）
PUT  /v1/offer/{id}/data         流式上传，支持 Content-Range
POST /v1/offer/{id}/decision     accept / reject
GET  /v1/inbox                   JSON 列表
GET  /v1/events                  SSE 事件流
```

`/v1/events` 是真正的复用接口：其他程序不必解析 CLI 输出，直接订阅即可。这比 `--json` 更重要。

本地管理端点（`/v1/scan`、`/v1/text` 的广播分支、`/v1/inbox` 写操作）**只接受来自 127.0.0.1 / ::1 的连接**，防止局域网内被别人当跳板。

## 7. 数据模型

```
Peer   { id, name, addr, port, platform, last_seen, trusted, fingerprint }
Item   { id, kind: text|file, from, to, channel: dm|public, size, sha256,
         status: pending|active|done|failed, path, created_at, thread }
Offer  { id, item_id, from, to, filename, size, sha256, offset, expires_at }
```

私聊的 `to` 是 peer id，公共的 `to` 为 `"*"`，两者走同一套 Item 结构。线程键：私聊为 `(peerA, peerB)`，公共为 `*`。

## 8. 服务生命周期

- 锁文件 `serve.lock` 存 PID + 实际端口，`flock` 加锁（Windows 用 `LockFileEx`）
- 任何命令先 `ensureServe()`：锁在、进程活着、`/v1/info` 能通 → 复用；否则 spawn 一个 `xlan serve --detach`
- 平台差异：Unix 用 `Setsid`，Windows 用 `DETACHED_PROCESS | CREATE_NO_WINDOW`
- `xlan status` 能看出服务是否在跑、端口、监听地址

## 9. 信任与收件箱

- 收到 offer 先查白名单：信任的直接落盘
- 不信任的挂起为 pending，发系统通知，`xlan inbox` 里可见，60 秒内未处理即进收件箱排队
- 收件箱是唯一的人工确认入口，第一版不做交互 TUI
- `xlan trust add <peer-id>` 之后免确认

## 10. 输出契约

- **stdout 只出结果**：`--json` 时一行一个 JSON 对象（NDJSON）；默认是人类可读的简洁文本
- **stderr 只出进度与日志**
- 退出码：`0` 成功 / `1` 一般错误 / `2` 用法错误 / `3` 找不到设备 / `4` 被拒绝 / `5` 超时 / `6` 校验失败
- `--json` 时错误也写 stdout：`{"ok":false,"code":"NOT_FOUND","msg":"..."}`
- 同一份 JSON 结构同时用于 CLI 输出和 SSE 事件，调用方只需实现一套解析

## 11. 可靠性

- `transfer_id = sha256(filename + size + mtime + sender_id)`，重试不重复写盘
- 接收：写 `.part`，完成后校验 sha256，再原子 `rename`
- 发送：1MB 分块，失败重试 3 次，指数退避
- 续传：`GET /v1/offer/{id}` 返回 `offset`，从该处继续
- 传输中断超过 10 分钟的任务标记 failed，`.part` 保留 24 小时

## 12. 安全

- 第一版 HTTP 明文，仅限可信内网；`--tls` 留到 1.1（自签 + 指纹确认）
- 接收端必须防路径穿越：拒绝绝对路径、`..`、Windows 保留名，文件名清洗后落盘
- 单文件大小上限默认 4GB（可配置），文本上限见 6.2
- 本地管理接口绑定回环

## 13. 仓库结构

```
Go/xlan/
  cmd/xlan/main.go
  core/                  公开包：纯逻辑，不碰 os.Args / stdout / os.Exit / 全局状态
    peer.go item.go offer.go session.go events.go
  internal/cli/          子命令、输出格式
  internal/discover/     udp broadcast、mdns、tcp probe
  internal/httpapi/      路由、handler
  internal/store/        peers.json、inbox、config
  internal/spawn/        锁与自拉起（分平台 build tag）
  internal/platform/     路径、通知、文件锁
  docs/specs/
```

`core/` 放在顶层而非 `internal/`，因为以后 `gomobile bind` 不能绑定 internal 包。

## 14. 里程碑

1. **M1 骨架**：`scan` + `serve` + `/v1/info`，UDP 广播发现跑通
2. **M2 文本**：公共文本（UDP 广播 + 回执 + 降级）、私聊文本（TCP）
3. **M3 文件**：单文件传输、进度、sha256 校验、原子落盘
4. **M4 体验**：收件箱、信任白名单、断点续传、`doctor`
5. **M5 契约固化**：`--json` 全量、SSE 事件流、退出码、README 示例
