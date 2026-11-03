# xlan

局域网文件与文本传输命令行工具。一个静态二进制，Windows / macOS / Linux 通用。

设计目标是**契约稳定**：任何脚本、GUI 或 AI agent 只要 `exec` 一次，就能复用发现、传输、确认这一整套能力，不用重复实现。

## 安装

```bash
go build -o xlan ./cmd/xlan
```

想把「哪次构建」烧进二进制（推荐，之后 `xlan --version` 能直接看出来）：

```bash
COMMIT=$(git rev-parse --short HEAD)
BUILT=$(TZ=Asia/Shanghai date '+%Y-%m-%dT%H:%M')
go build -trimpath -ldflags "-s -w -X xlan/core.Commit=$COMMIT -X xlan/core.BuiltAt=$BUILT" -o xlan ./cmd/xlan
```

```console
$ xlan --version
xlan 0.1.0 (a1b2c3d, 2026-11-04T00:14)
```

**配置和数据都放在程序自己所在的目录**（`config.json`、`id`、`xlan.db`），不写系统目录，拷走整个文件夹就是完整迁移。只有收件目录可以改，默认是系统的下载目录。

交叉编译（纯 Go，含 SQLite，不需要 C 工具链），把上面 `go build` 换成对应目标即可：

```bash
GOOS=windows GOARCH=amd64 go build -o xlan.exe ./cmd/xlan
GOOS=darwin  GOARCH=arm64 go build -o xlan     ./cmd/xlan
GOOS=linux   GOARCH=arm64 go build -o xlan     ./cmd/xlan
```

## 快速上手

```bash
xlan serve            # 前台跑服务（接收端）
xlan scan             # 看局域网里有谁
xlan text "在吗"       # 公共消息，广播给所有人
xlan text "在吗" --to mbp
xlan send 报告.pdf --to mbp
xlan send ~/照片 --to mbp   # 目录也行，保留目录结构
xlan inbox            # 收件箱
xlan recv <id> -o -   # 把内容管道出去
```

服务不用手动起：任何命令发现没有服务时，会自动在后台拉起一个。

## 命令

| 命令 | 说明 |
|---|---|
| `xlan scan [--deep]` | 发现设备；`--deep` 额外扫本机 /24 网段 |
| `xlan status` | 服务状态、端口、在线设备 |
| `xlan serve [-d]` | 前台跑服务，`-d` 放后台 |
| `xlan stop` | 停掉后台服务 |
| `xlan autostart on\|off\|status` | 开机自启（Windows 注册表 / systemd / LaunchAgent） |
| `xlan path on\|off\|status` | 把程序目录加进 PATH |
| `xlan text <内容\|->` | 发文本，省略 `--to` 就是公共 |
| `xlan send <文件\|目录\|->` | 发文件或目录，`--to` 指定设备，`--all` 群发 |
| `xlan inbox` | 收件箱，默认最新 20 条 |
| `xlan inbox --watch` | 实时跟随 |
| `xlan inbox accept\|reject [序号]` | 处理待确认的传输，只有一条时可不写 |
| `xlan recv <id> [--out 路径] [-o -]` | 取出内容，`-` 表示 stdout |
| `xlan trust add\|rm\|list` | 信任白名单 |
| `xlan config [get\|set\|unset]` | 查看和修改配置 |
| `xlan doctor` | 网络自检 |

全局参数：`--json`、`--quiet`、`--timeout 60s`、`--config 目录`。

## 端口

| 端口 | 协议 | 用途 |
|---|---|---|
| 43001 | UDP | 发现广播 |
| 43002 | UDP | 公共文本广播 + 回执 |
| 43003–43005 | TCP | HTTP API，被占用时顺延 |

TCP 与 UDP 端口空间独立，可以同号。

## 输出契约

给程序调用时开 `--json`，stdout 每行一个 JSON 对象（NDJSON），stderr 只有进度和日志。

```console
$ xlan text "hi" --json
{"acked":["mbp"],"channel":"public","ok":true}

$ xlan scan --json
{"ok":true,"peers":[{"id":"a1b2...","name":"mbp","addr":"10.0.0.9","port":43003,...}]}
```

退出码：

| 码 | 含义 |
|---|---|
| 0 | 成功 |
| 1 | 一般错误 |
| 2 | 用法错误 |
| 3 | 找不到设备 |
| 4 | 被拒绝 |
| 5 | 超时 |
| 6 | 校验失败 |

事件流（给长驻程序用，比解析 CLI 输出更稳）：

```bash
curl -N http://127.0.0.1:43003/v1/events
```

## 行为约定

- 收到的文件落在下载目录下的 `xlan`（安卓 `/sdcard/Download/xlan`，其余 `~/Downloads/xlan`），可用 `xlan config set recv_dir 路径` 改，重名自动加 ` (1)`，**绝不覆盖**
- 发目录时先发结构清单，再逐个文件传输；保留空目录，不保留权限位，跳过符号链接
- 目录里的每个文件单独校验 sha256，某个文件失败只影响它自己
- 文件超过 1MB 分块上传，支持断点续传，落地前校验 sha256，`rename` 原子完成
- 发送和接收都会在 stderr 上实时显示百分比、已传/总量和速度；只有终端里才画，管道里不刷屏
- 公共文本走 UDP 广播，单包 1000 字节以内；超长自动降级为逐台 TCP 投递
- 公共文本会等 600ms 收对方回执，零回执不等于失败，但会提示可能被 AP 隔离
- 不信任的设备发文件会挂起，等你 `xlan inbox accept`；文本自动接收
- 待确认的传输保留 24 小时，但发送方只等 `--timeout`（默认 60 秒）
- 改配置后：服务空闲就立刻重启；有传输在跑就等它们跑完再重启，不打断传输

## 隐私与安全

- 默认明文 HTTP，只适合可信内网
- 接收端做了路径穿越防护（拒绝 `..`、绝对路径、Windows 保留名）
- 本地管理接口（扫描、广播、收件箱写操作）只接受 127.0.0.1 连接

## 已验证范围

Linux（arm64）实测通过，包含发现、公共文本、文件传输、校验落盘。
Windows / macOS 只验证了交叉编译通过，未在真机运行。

## 开发

```bash
go test ./...
```

```
core/               核心类型与协议，纯逻辑
internal/config     路径与配置
internal/store      SQLite
internal/events     进程内事件总线
internal/discover   UDP 广播、TCP 探测
internal/pubtext    公共文本广播与回执
internal/httpapi    HTTP API
internal/client     对端客户端
internal/serve      服务主体
internal/spawn      按需拉起
internal/cli        子命令
```
