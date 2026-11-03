# xlan 调用说明（给 agent / 脚本）

这份文档只讲「怎么可靠地调用」，不讲原理。设计背景见 [设计文档](./specs/2026-11-03-xlan-design.md)。

## 0. 心智模型

- 本机跑一个**常驻服务**，CLI 是它的瘦客户端。所有网络动作都由服务执行。
- **服务不会自动启动**：必须先 `xlan serve`。没起服务时，除 `serve` / `status` / `doctor` / `config` 之外的命令都会报 `NO_SERVICE` 并退出 1。
- 加 `--json`，stdout 就是**一行一个 JSON 对象**（NDJSON），stderr 只有进度和日志。
  不加 `--json` 是人类可读格式，**不要解析**。

## 1. 硬约定

| 约定 | 说明 |
|---|---|
| stdout | 只出结果。`--json` 时是 NDJSON，每行一个完整 JSON 对象 |
| stderr | 进度、警告、日志。不要读它做判断 |
| 交互 | **非 TTY 时绝不交互**。多个设备时必须给 `--to` 或 `--all`，否则报错退出 2 |
| 阻塞 | `send` 会一直等到对方确认并传完（默认 60 秒超时）。要非阻塞请自己加超时 |
| 进度 | 进度画在 **stderr**，只在终端里出现。`--json` 时 stdout 依然是纯 NDJSON |
| 幂等 | 同一份文件重发会覆盖成新记录；接收端文件名重名会自动加 ` (1)` |

### 退出码

| 码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | 一般错误（文件不存在、服务异常、校验失败） |
| 2 | 用法错误（参数不对、非 TTY 下没指定目标） |
| 3 | 找不到设备 |
| 4 | 对方拒绝 |
| 5 | 超时 |
| 6 | 校验失败 |

### 失败时的输出

`--json` 下失败也写 stdout，退出码非 0：

```json
{"ok":false,"code":"NO_PEER","msg":"没发现在线设备，先跑 xlan scan"}
```

`code` 是稳定枚举，`msg` 仅供人看，**不要用它做判断**。

## 2. 命令

### 2.0 `xlan serve [-d]` / `xlan stop`

```json
{"ok":true,"base":"http://127.0.0.1:43003","port":43003,"detached":true}
```

- `xlan serve` 前台跑，占住终端，Ctrl-C 停
- `xlan serve -d` 后台跑，命令立刻返回，适合 agent
- `xlan stop` 停掉，没在跑时也是退出 0

服务不会自动启动，必须先起。

### 2.0.1 `xlan autostart on|off|status` / `xlan path on|off|status`

```json
{"ok":true,"installed":true,"detail":"..."}
```

- `autostart`：Windows 写 HKCU 的 Run 键，Linux 装 systemd 用户服务，macOS 装 LaunchAgent
- `path`：把可执行文件所在目录加进用户 PATH（unix 写 shell 启动文件，Windows 写用户注册表 PATH）
- 两者都是幂等的，重复执行不会写重复内容

### 2.1 `xlan scan [--deep]`

发现局域网设备。先返回缓存（立即），再刷新。

```json
{"ok":true,"peers":[{"id":"a1b2c3d4e5f60718","name":"mbp","addr":"10.0.0.9","port":43003,"platform":"darwin","last_seen":1730000000,"trusted":false}]}
```

- `--deep`：额外并发探测本机 /24 网段的 HTTP 端口，**约 4 秒**，但在组播被广播过滤的网络里是唯一能发现设备的手段
- 扫描结果不会包含本机
- 空数组是正常结果，不是错误

### 2.2 `xlan status`

服务是否在跑、端口、在线设备、收件目录。

```json
{"ok":true,"running":true,"base":"http://127.0.0.1:43003","ports":[43003,43004,43005],"recv_dir":"/sdcard/Download/xlan","peers":[]}
```

`running:false` 时 `base` 为空。

### 2.3 `xlan text <内容|-> [--to 设备]`

省略 `--to` → 公共消息（UDP 广播给所有设备）；给了 → 私聊。
内容为 `-` 或省略且 stdin 不是终端 → 从 stdin 读。

```json
{"ok":true,"channel":"public","acked":["mbp"],"degraded":false,"warn":""}
```

- `acked`：600ms 内回执的设备名。**为空不代表失败**，消息确实广播出去了，只说明没人应答（可能被 AP 隔离）
- `degraded:true`：正文超过 1000 字节，已自动改为逐台 TCP 投递，此时 `acked` 是逐台投递结果
- 私聊时 `channel:"dm"`，`acked` 是目标设备名

### 2.4 `xlan send <文件|目录|-> (--to 设备 | --all)`

发文件或整个目录。目录会先发结构清单，再逐个文件传。

```json
{"ok":true,"peer":"mbp","id":"a1b2c3d4e5f60718","size":1048576}
```

- 目录也返回同样结构，`size` 是总字节数
- 对方不信任本机时会挂起，命令阻塞等对方确认，超时退出 5
- 对方拒绝 → 退出 4
- 多个目标时每个目标输出一行 JSON

### 2.5 `xlan inbox [-n 20] [--since 2h]`

```json
{"ok":true,"items":[{"id":"...","kind":"file","from_id":"tt","from_name":"同事","to_id":"<本机id>","channel":"dm","name":"报告.pdf","size":1024,"sha256":"...","status":"done","path":"/sdcard/Download/xlan/报告.pdf","created_at":1730000000}]}
```

`kind` 取值 `text` / `file` / `folder`；`status` 取值 `pending` / `active` / `done` / `failed`。
`pending` 表示等你确认。

### 2.6 `xlan inbox accept|reject [序号|id]`

```json
{"ok":true,"id":"...","action":"accept"}
```

参数三种写法：

| 写法 | 含义 |
|---|---|
| 不写 | 恰好一条 `pending` 时自动选中；0 条或 2 条以上都会报错退出 2 |
| `1`–`20` | 按 `xlan inbox` 输出的序号，**顺序与列表一致（新→旧）** |
| `1793…` | 条目 id 或其前缀 |

只能处理 `pending` 状态的条目，其余状态返回 2。
agent 建议直接用完整 `id`，不要依赖序号——列表会随新消息变化。

`accept` 在终端里默认会**跟到传完**才返回（带实时进度）；加 `--no-wait` 立即返回。管
道或 `--json` 下不会等待。

### 2.7 `xlan recv <id> [--out 路径] [-o -]`

把收件箱里某一项取出来。文本直接输出正文，文件输出内容。`--out -` 写到 stdout。

注意：`recv` 输出的是**原始内容**，`--json` 对它不生效（这是故意的，它是给管道用的）。想拿元数据用 `xlan inbox --json` 或 `/v1/item/{id}`。

### 2.8 `xlan trust add|rm <设备> | list`

```json
{"ok":true,"id":"a1b2...","trusted":true}
```

加了信任之后，对方发文件不再需要确认。

### 2.9 `xlan config [list | get 键 | set 键 值 | unset 键]`

可改的键：

| 键 | 作用 | 需要重启服务 |
|---|---|---|
| `recv_dir` | 收件目录 | 是（空闲时立即，有传输时等传完） |
| `name` | 本机显示名 | 是 |
| `threads` | 发送时并行分块数，1–8，默认 4 | 否，改完立刻生效 |

```json
{"ok":true,"key":"recv_dir","value":"/data/xlan","restarted":true,"deferred":false,"inflight":0}
```

- `deferred:true` 表示当前有传输在跑，改动已保存，等传输做完服务自动重启才生效
- 服务没在跑时，改动下次启动生效

### 2.10 `xlan doctor`

网络自检，排查「扫不到设备」时用。

## 3. 事件流（推荐给长驻 agent）

SSE，比解析 CLI 输出稳定。

```bash
curl -N http://127.0.0.1:43003/v1/events
```

每行 `data: {json}`：

```json
{"type":"item.new","ts":1730000000,"data":{ /* 完整 Item */ }}
{"type":"item.update","ts":1730000000,"data":{"id":"...","status":"done","path":"..."}}
{"type":"peer.online","ts":1730000000,"data":{ /* 完整 Peer */ }}
```

- `item.new`：收到新消息/文件，或自己发出了公共消息
- `item.update`：状态变化，`done` / `failed` / `active`
- `peer.online`：发现新设备

事件只推送**增量**，断线重连后不会补发历史，需要历史就查 `/v1/inbox`。

## 4. 本地 HTTP API

服务监听 `127.0.0.1:43003`（被占用时顺延到 43004、43005）。比 CLI 更细粒度，适合常驻程序。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/info` | 设备信息、端口、uptime |
| GET | `/v1/peers` | 已知设备 |
| POST | `/v1/scan?deep=1` | 主动扫描 |
| GET | `/v1/inbox?limit=20&since=unix` | 收件箱 |
| GET | `/v1/item/{id}` | 单条记录 |
| GET | `/v1/events` | SSE 事件流 |
| POST | `/v1/text` | `{"body":"...","to":""}`，`to` 空就是公共 |
| POST | `/v1/offer` | 发起传输（一般不用手写，CLI 已封装） |
| POST | `/v1/offer/{id}/decision` | `{"action":"accept"\|"reject"}` |
| POST | `/v1/restart` | 请求重启，有传输时会推迟 |
| POST | `/v1/shutdown` | 停服务 |

**只有回环地址能调用**这些管理端点，远程设备调用会返回 403。
`/v1/offer`、`/v1/offer/{id}/data`、`/v1/offer/{id}/file/{rel}` 是对外接口，允许局域网访问。

## 5. 常见流程

### 5.1 发文件给指定设备，确认真的到了

```bash
xlan scan --json                      # 拿 peers[].name / id
xlan send 报告.pdf --to mbp --json    # 阻塞到传完
[ $? -eq 0 ] && echo "已送达"
```

### 5.2 发一条公共消息，并知道谁收到了

```bash
out=$(xlan text "机房要断电了" --json)
echo "$out" | jq -r '.acked[]?'       # 空表示没人应答
```

### 5.3 当接收端机器人：等文件进来

```bash
# 先信任对方，避免每次都要人工确认
xlan trust add mbp

# 订阅事件，落盘后处理
curl -sN http://127.0.0.1:43003/v1/events |
  grep --line-buffered '^data: ' |
  sed 's/^data: //' |
  jq -c 'select(.type=="item.update" and .data.status=="done")' |
  while read -r ev; do
    path=$(echo "$ev" | jq -r '.data.path // empty')
    [ -n "$path" ] && echo "收到文件: $path"
  done
```

### 5.4 先确认环境可用，再干活

```bash
xlan status --json | jq -e '.running' >/dev/null || xlan serve -d
sleep 1
xlan status --json | jq -e '.running' >/dev/null || { echo "服务没起来"; exit 1; }
xlan doctor --json
```

## 6. 数据在哪

`config.json`、设备 `id`、`xlan.db` 都写在**可执行文件所在目录**，不碰系统配置目录。
只有收件目录可配（`xlan config set recv_dir`），默认是系统的下载目录。

## 7. 坑（务必知道）

1. **设备名不唯一**，两台机器可能都叫 `macbook`。`--to` 歧义时命令会报错并列出候选 id，此时改用 id 前缀。
2. **公共消息的 `acked` 为空是正常现象**，不是发送失败。判断发送失败看退出码。
3. **`send` 是阻塞的**，默认最长 60 秒。批量发请串行并自己控超时，别并发几十个。
4. **不信任的设备会挂起**，对方不点接受就一直等到超时。要自动化就先 `xlan trust add`。
5. **服务必须自己起**（`xlan serve`）。agent 想后台跑就 `xlan serve -d`，再用 `xlan status --json` 确认 `running:true`。
6. **传输中途改配置不会立即生效**，服务会等当前传输做完再重启（`deferred:true`）。
7. **Windows 首次运行会弹防火墙授权**，必须允许，否则只能单向发现。
8. **收件目录里的文件可能同名**，重名会自动变成 `报告 (1).pdf`，以 `items[].path` 为准，不要自己拼路径。
9. **`/v1/events` 断线不补发**，长跑程序要重连并回补一次 `/v1/inbox`。

## 8. 不要做的事

- 不要解析不带 `--json` 的输出（文案会变）
- 不要用 `msg` 文本做判断，用 `code` 和退出码
- 不要假设 `scan` 立刻返回全部设备（广播有丢包，必要时再扫一次或加 `--deep`）
- 不要并发调用 `xlan serve`，第二个会因为端口占用直接退出（这是预期行为）
- 不要把 `xlan` 当作公网传输工具，它默认明文、只适合可信内网
