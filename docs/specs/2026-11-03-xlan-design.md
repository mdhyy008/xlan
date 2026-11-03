# xlan 设计文档

版本 0.2 · 2026-11-03

## 1. 目标

一个跨平台的局域网传输命令行工具，同时把「可被其他程序调用」当作一等需求。

- 内网发现：扫描其他设备，同时开放服务供别人扫描
- 私聊发文本、公共发文本、私聊发文件
- 任何工具（脚本、GUI、AI agent、以后的安卓 App）通过 `exec` + JSON，或直接订阅本地事件流即可复用，不必重造发现与传输

核心判断：**这套东西的价值不在「能传文件」，而在契约稳定**。所以输出的稳定性、退出码、事件流，和功能本身同等重要。

## 2. 非目标（第一版）

- 跨网中继、NAT 穿透
- 目录双向同步
- TUI 界面（交互只用一个数字菜单）
- 系统桌面通知
- mDNS（留 `Provider` 接口，1.1 再插）
- TLS
- gomobile 壳（只保证 `core/` 不被写死成「只能当命令行用」）

## 3. 命名与路径

- 命令：`xlan`
- Linux/macOS：配置 `~/.config/xlan`，数据 `~/.local/share/xlan`，缓存 `~/.cache/xlan`
- Windows：`%APPDATA%\xlan`、`%LOCALAPPDATA%\xlan`
- 数据库：`<数据目录>/xlan.db`
- 收件目录：默认 `~/Downloads/xlan`，配置可改
- 所有运行时文件都在上述目录，不写系统临时目录

## 4. 端口

TCP 与 UDP 的端口空间互相独立，可同号复用。

| 端口 | 协议 | 用途 | 是否可变 |
|---|---|---|---|
| 43001 | UDP | 发现广播（probe / announce） | 固定 |
| 43002 | UDP | 公共文本广播 + 回执 | 固定 |
| 43003 | TCP | HTTP API（首选） | 占用时依次 43004、43005 |

- TCP 实际端口写进 `runtime.json`，并放进 announce 包与 `/v1/info`
- 不要求两端端口一致
- 只有常驻服务持有这些 socket，CLI 不直接绑定端口

## 5. 命令面

```
xlan scan                        发现局域网设备
xlan serve [--foreground]        前台跑服务
xlan send <路径> --to 名字        发文件
xlan send <路径> --all            群发文件
xlan text "内容" [--to 名字]      发文本，省略 --to = 公共
xlan inbox [-n 20] [--since 2h]  收件箱，默认最新 20 条
xlan inbox --watch               实时跟随新消息
xlan inbox accept|reject <id>    处理待确认的传输
xlan recv <id> [--out 路径] [-o -]  把收件箱某项取到文件或 stdout
xlan status                      服务状态、端口、在线设备
xlan trust add|rm|list           信任白名单
xlan doctor                      网络自检（端口、广播、防火墙）
```

全局参数：`--json`、`--quiet`、`--timeout`、`--config`、`--as 名字`。

管道语义：

- `cat a.zip | xlan send - --to mbp`：从 stdin 流式发送
- `git log | xlan text -`：从 stdin 读文本
- `xlan recv 7 -o - | tar x`：输出到 stdout

**目标选择规则**

- `send`：0 台在线 → 报错，退出码 3；1 台 → 直接发；多台 → 终端里弹数字菜单选择，`--all` 群发
- 非 TTY（管道、脚本、`--json`）时**不交互**，多台直接报错，退出码 2，并列出候选
- `--to` 支持设备名（大小写不敏感、前缀匹配）或设备 id 前缀；歧义时报错并列出候选 id

## 6. 网络协议

**原则：所有网络动作由常驻服务执行，CLI 只是瘦客户端。** 避免两个命令抢同一个端口，也避免状态分裂。

### 6.1 发现（UDP 43001）

包格式（单条 JSON，≤ 512 字节）：

```json
{"v":1,"t":"probe","id":"<本机id>","name":"<本机名>"}
{"v":1,"t":"announce","id":"...","name":"...","port":43003,"platform":"linux","ts":1730000000}
```

- 服务启动、以及每 30 秒发一次 `announce`
- `xlan scan` 触发 `probe`，收到的人单播回 `announce`
- 广播同时发 `255.255.255.255` 和每个活动网卡的定向广播地址（如 `192.168.1.255`）
- 结果写库，`scan` 先立即输出缓存，再刷新
- 两层兜底：UDP 广播 → 子网 TCP 并发探测（受限网络、安卓 Termux 下只有第二层能用）
- 发现层定义 `Provider` 接口，mDNS 在 1.1 作为第三个实现插入

### 6.2 公共文本（UDP 43002）

```json
{"v":1,"t":"msg","id":"<msgid>","sid":"<发送者id>","sname":"...","channel":"public","body":"...","ts":1730000000}
{"v":1,"t":"ack","id":"<msgid>","rid":"<接收者id>","rname":"..."}
```

- 正文上限 **1000 字节**，整包控制在 1200 字节内
- 发送方连发 2 遍，间隔 30ms
- 接收方按 `msgid` 去重，收到后单播回 `ack` 到源地址:43002
- 发送方等 600ms 收集回执，输出 `{"ok":true,"acked":["peer-a"]}`
- 零回执不报错（消息确实发出去了），但 stderr 提示可能被 AP 过滤，建议 `xlan doctor`
- **正文超过 1000 字节自动降级**：改为对所有在线设备逐一走 TCP 投递，`channel` 仍标 `public`

### 6.3 私聊文本与文件（TCP）

```
GET  /v1/info                    设备名、id、平台、能力、服务端口
POST /v1/offer                   发起传输（文件或文本），返回 offer_id
GET  /v1/offer/{id}              状态、已接收 offset（续传用）
PUT  /v1/offer/{id}/data         流式上传，Content-Range 定位
PUT  /v1/offer/{id}/file/{rel}   目录传输里的单个文件
POST /v1/offer/{id}/finish       目录传完，接收方做完整性检查
POST /v1/offer/{id}/decision     accept / reject
GET  /v1/inbox                   JSON 列表（limit / since）
GET  /v1/events                  SSE 事件流
```

`/v1/events` 是真正的复用接口：其他程序不必解析 CLI 输出，直接订阅即可。

本地管理端点（`/v1/scan`、`/v1/text` 的广播分支、`/v1/inbox` 写操作）**只接受来自 127.0.0.1 / ::1 的连接**。

**目录传输**：先发一份 manifest（目录结构 + 每个文件的大小与 sha256），接收方按结构建好目录树，然后逐个文件上传，最后 `/finish` 做一次全量校验。空目录由 manifest 里的目录条目还原；空文件由 `/finish` 补建；符号链接发送端直接跳过。

## 7. 数据模型

SQLite（`modernc.org/sqlite`，纯 Go，可交叉编译）。

```sql
items(id TEXT PK, kind TEXT, from_id TEXT, from_name TEXT, to_id TEXT,
      channel TEXT, body TEXT, name TEXT, size INTEGER, sha256 TEXT,
      status TEXT, path TEXT, manifest TEXT, created_at INTEGER)
peers(id TEXT PK, name TEXT, addr TEXT, port INTEGER, platform TEXT,
      last_seen INTEGER, trusted INTEGER)
```

- `kind`：`text` / `file`；`channel`：`dm` / `public`；`status`：`pending` / `active` / `done` / `failed` / `expired`
- 公共消息的 `to_id` 为 `"*"`
- 不设保留上限，`inbox` 默认只查最新 20 条
- 数据库只存元数据；文件本体在收件目录

## 8. 服务生命周期

**监听端口本身就是锁**，不用文件锁、不用 PID 校验：

- `serve` 依次尝试绑定 43003、43004、43005，第一个成功的端口即本机端口
- 全部绑定失败 → 检查 43003 的健康状况，能通就说明已有实例在跑，静默退出 0
- 任何 CLI 命令先 `Ensure()`：逐个端口试 `/v1/info`，有活的就复用；否则 spawn 一个 `xlan serve --detach`
- 平台差异：Unix `Setsid`，Windows `DETACHED_PROCESS | CREATE_NO_WINDOW`
- UDP 43001/43002 只能被一个进程绑定，天然互斥

## 9. 信任与收件箱

- 收到 offer 先查白名单：信任的直接开传
- 不信任的挂起为 `pending`，24 小时内有效，`xlan inbox` 里可见
- 发送方只等 `--timeout`（默认 60 秒），超时退出码 5；此时对方的 offer 仍在收件箱挂着，但标记「发送方已离开」，再 accept 也无法传输
- 不做系统通知，靠 `xlan inbox --watch` 实时看
- `xlan trust add <peer-id>` 之后免确认

## 10. 输出契约

- **stdout 只出结果**：`--json` 时一行一个 JSON 对象（NDJSON）；默认是人类可读的简洁文本
- **stderr 只出进度与日志**
- 退出码：`0` 成功 / `1` 一般错误 / `2` 用法错误 / `3` 找不到设备 / `4` 被拒绝 / `5` 超时 / `6` 校验失败
- `--json` 时错误也写 stdout：`{"ok":false,"code":"NOT_FOUND","msg":"..."}`
- 同一份 JSON 结构同时用于 CLI 输出和 SSE 事件

## 11. 可靠性

- `transfer_id = sha256(filename + size + mtime + sender_id)`，重试不重复写盘
- 接收：写 `.part`，完成后校验 sha256，再原子 `rename`
- 发送：1MB 分块，失败重试 3 次，指数退避
- 续传：`GET /v1/offer/{id}` 返回 `offset`，从该处继续
- 重名落盘时自动加 ` (1)`、` (2)`，**绝不覆盖**
- 传输中断超过 10 分钟标记 failed，`.part` 保留 24 小时

## 12. 安全

- 第一版 HTTP 明文，仅限可信内网
- 接收端防路径穿越：拒绝绝对路径、`..`、Windows 保留名，文件名清洗后落盘
- 单文件大小上限默认 4GB（可配置）
- 本地管理接口绑定回环

## 13. 仓库结构

```
Go/xlan/
  cmd/xlan/main.go
  core/                  公开包：纯逻辑，不碰 os.Args / stdout / os.Exit / 全局状态
    ports.go wire.go peer.go item.go event.go device.go
  internal/config/       路径与配置
  internal/store/        SQLite
  internal/events/       进程内事件总线
  internal/discover/     UDP 广播、TCP 探测
  internal/pubtext/      公共文本广播与回执
  internal/httpapi/      HTTP 服务与 handler
  internal/client/       面向对端设备的 HTTP 客户端
  internal/spawn/        自拉起（分平台 detach）
  internal/cli/          子命令、输出格式、交互选择
  docs/
```

`core/` 放顶层而非 `internal/`，因为以后 `gomobile bind` 不能绑定 internal 包。

## 14. 里程碑

1. **M1 骨架**：`scan` + `serve` + `/v1/info`，UDP 广播发现跑通
2. **M2 文本**：公共文本（广播 + 回执 + 降级）、私聊文本
3. **M3 文件**：单文件传输、进度、sha256 校验、原子落盘
4. **M4 体验**：收件箱、信任白名单、断点续传、`doctor`
5. **M5 固化**：`--json` 全量、SSE、退出码、README
