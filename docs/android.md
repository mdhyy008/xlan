# Android 集成（gomobile AAR）

把 xlan 服务跑在 Android 应用进程内，App 通过回环 HTTP API 收消息、发文本、管理收件箱。
产物是 `bin/xlan.aar`，目前只打包 `arm64-v8a`。

AAR **只负责启动和停止服务**：发现、接收、确认这些动作都由服务完成，App 通过本地 HTTP 调它。
发文件需要 App 自己按第 6 节的契约去调用对端接口（CLI 的完整发送逻辑在 `internal/cli/cli.go`，可对照实现）。

## 1. 构建

需要 Go、Android SDK、NDK 和 JDK。**SDK / NDK 路径不能带空格**，Windows 上先建 junction：

```bat
mklink /J D:\androidsdk "D:\Android Studio\Sdk"
mklink /J D:\ndk28     "D:\Android Studio\Sdk\ndk\28.2.13676358"
```

安装工具并生成 AAR：

```bash
go install golang.org/x/mobile/cmd/gomobile@latest
"$(go env GOPATH)/bin/gomobile" init
go get -tool golang.org/x/mobile/cmd/gobind

mkdir -p bin
ANDROID_HOME=D:/androidsdk ANDROID_NDK_HOME=D:/ndk28 \
  PATH="$(go env GOPATH)/bin:$PATH" \
  gomobile bind -target=android/arm64 -androidapi 24 -o bin/xlan.aar ./mobile
```

产物：

- `bin/xlan.aar`：给 App 依赖
- `bin/xlan-sources.jar`：Java 源码，方便在 IDE 里看签名

需要 32 位设备或模拟器时把 `-target` 换成 `android/arm,android/arm64,android/amd64`。

## 2. Java / Kotlin 接口

```text
mobile.Mobile.start(dataDir, recvDir, autoAccept) : String   // 返回本地 API 地址，阻塞至多 8 秒
mobile.Mobile.stop() : void
mobile.Mobile.getBaseURL() : String
mobile.Mobile.isRunning() : boolean
mobile.Mobile.version() : String
mobile.Mobile.setLogger(mobile.Logger)
```

- `dataDir` 必须是应用私有可写目录（`filesDir`），设备身份、数据库、证书和配置都放这里。
- `recvDir` 建议也传私有目录，为空则按配置（可能落到 `/sdcard/Download`）。
- `autoAccept = true` 时不等待确认，直接接收所有人的文件。
- 默认开启 TLS，`start` 返回 `https://127.0.0.1:43503`；只有配置里关了 TLS 才是 `http://`。
- `start` 会阻塞到服务可访问，**不要在 UI 线程调用**；重复调用直接返回现有地址。
- 服务占用的端口固定：发现 UDP `43501`、公共消息 UDP `43502`、本地 API `43503`（被占用时顺延 43504、43505）。

## 3. 生命周期：放在前台 Service 里

服务和 App 同进程，进程被回收服务就停。常驻场景用 `dataSync` 类型的前台 Service：

```kotlin
class XlanService : Service() {
    private var multicastLock: WifiManager.MulticastLock? = null

    override fun onCreate() {
        super.onCreate()
        createChannel()
        startForeground(1, buildNotification())

        // 安卓会过滤组播/广播，不拿锁可能收不到别的设备的发现包
        multicastLock = (getSystemService(Context.WIFI_SERVICE) as WifiManager)
            .createMulticastLock("xlan").apply {
                setReferenceCounted(false)
                acquire()
            }

        mobile.Mobile.setLogger(object : mobile.Logger {
            override fun log(msg: String) = Log.i("xlan", msg)
        })

        thread(name = "xlan-start") {
            runCatching {
                val base = mobile.Mobile.start(
                    filesDir.absolutePath,
                    File(filesDir, "recv").absolutePath,
                    false
                )
                Log.i("xlan", "服务已就绪：$base")
                // 把 base 交给 UI 层，之后所有请求都发到这里
            }.onFailure { Log.e("xlan", "启动失败", it) }
        }
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int) = START_STICKY

    override fun onDestroy() {
        // stop 会阻塞到服务退出，别放在 UI 线程
        thread(name = "xlan-stop") { mobile.Mobile.stop() }
        multicastLock?.release()
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null

    private fun createChannel() {
        if (Build.VERSION.SDK_INT >= 26) {
            val ch = NotificationChannel("xlan", "xlan", NotificationManager.IMPORTANCE_LOW)
            getSystemService(NotificationManager::class.java).createNotificationChannel(ch)
        }
    }

    private fun buildNotification(): Notification {
        val open = PendingIntent.getActivity(
            this, 0, Intent(this, MainActivity::class.java),
            PendingIntent.FLAG_IMMUTABLE
        )
        val builder = if (Build.VERSION.SDK_INT >= 26) {
            Notification.Builder(this, "xlan")
        } else {
            Notification.Builder(this)
        }
        return builder
            .setContentTitle("xlan")
            .setSmallIcon(android.R.drawable.stat_sys_upload)
            .setContentIntent(open)
            .build()
    }
}
```

`AndroidManifest.xml`：

```xml
<uses-permission android:name="android.permission.INTERNET" />
<uses-permission android:name="android.permission.ACCESS_NETWORK_STATE" />
<uses-permission android:name="android.permission.ACCESS_WIFI_STATE" />
<uses-permission android:name="android.permission.CHANGE_WIFI_MULTICAST_STATE" />
<uses-permission android:name="android.permission.FOREGROUND_SERVICE" />
<uses-permission android:name="android.permission.FOREGROUND_SERVICE_DATA_SYNC" />
<uses-permission android:name="android.permission.POST_NOTIFICATIONS" />

<service
    android:name=".XlanService"
    android:exported="false"
    android:foregroundServiceType="dataSync" />
```

- API 34 起前台服务必须声明类型并带对应权限，否则启动即崩。
- Android 13 起 `POST_NOTIFICATIONS` 是运行时权限，上架前记得申请。
- App 里用 `ContextCompat.startForegroundService(...)` 启动这个 Service。

## 4. 连接本地服务（TLS）

`start` 返回的地址就是本地 API。默认是自签名的 TLS 证书，位于 `<dataDir>/tls.crt`。
用 OkHttp 时把这份证书作为唯一信任锚：

```kotlin
fun localClient(dataDir: File): OkHttpClient {
    val cf = CertificateFactory.getInstance("X.509")
    val cert = File(dataDir, "tls.crt").inputStream().use { cf.generateCertificate(it) }
    val ks = KeyStore.getInstance(KeyStore.getDefaultType()).apply {
        load(null, null)
        setCertificateEntry("xlan", cert)
    }
    val tmf = TrustManagerFactory.getInstance(TrustManagerFactory.getDefaultAlgorithm())
    tmf.init(ks)
    val tm = tmf.trustManagers.filterIsInstance<X509TrustManager>().first()
    val ssl = SSLContext.getInstance("TLS").apply { init(null, arrayOf(tm), null) }

    return OkHttpClient.Builder()
        .sslSocketFactory(ssl.socketFactory, tm)
        // 证书 CN=xlan，没有 127.0.0.1 的 SAN；信任范围已被上面的证书限定死
        .hostnameVerifier { _, _ -> true }
        .build()
}
```

- 证书在首次 `start` 后才会生成，`localClient` 要在服务起来之后调用。
- 如果配置里关掉了 TLS，`start` 返回 `http://...`，直接用普通 `OkHttpClient` 即可。
- 关闭 TLS 时 Android 9+ 默认禁止明文 HTTP，需要在 manifest 里加
  `android:usesCleartextTraffic="true"`（或配 network security config 只放行回环地址和局域网）。

## 5. 常用操作

表里的管理接口只接受回环地址，App 直连即可，无需额外鉴权。请求体都是 JSON。

| 方法 | 路径 | 说明 |
|---|---|---|
| GET | `/v1/info` | 本机 `id`、`name`、`port`、`tls`、`fingerprint`、`auto_accept` |
| GET | `/v1/peers` | 已知设备 |
| POST | `/v1/scan?deep=1` | 主动发现，`deep=1` 会额外探测 /24（约 4 秒） |
| POST | `/v1/text` | `{"body":"...","to":""}`，`to` 空就是公共消息 |
| GET | `/v1/inbox?limit=20&since=unix` | 收件箱 |
| GET | `/v1/item/{id}` | 单条记录 |
| POST | `/v1/offer/{id}/decision` | `{"action":"accept"}` / `{"action":"reject"}` / `{"action":"cancel"}` |
| POST | `/v1/trust` | `{"id":"...","trusted":true}`，之后该设备的文件自动接收 |
| GET | `/v1/events` | SSE 事件流 |
| DELETE | `/v1/item/{id}?files=1` | 删收件箱记录，`files=1` 连文件一起删 |
| POST | `/v1/inbox/clean` | 清理旧记录 |
| POST | `/v1/cache/clean` | 清理未完成的分片 |
| POST | `/v1/restart` / `/v1/shutdown` | 重启 / 停止服务 |

一个最小的 JSON 请求封装：

```kotlin
fun getJson(client: OkHttpClient, url: String): JSONObject {
    val req = Request.Builder().url(url).build()
    client.newCall(req).execute().use { resp ->
        val text = resp.body?.string().orEmpty()
        check(resp.isSuccessful) { "HTTP ${resp.code}: $text" }
        return JSONObject(text)
    }
}

fun postJson(client: OkHttpClient, url: String, body: String): JSONObject {
    val req = Request.Builder()
        .url(url)
        .post(body.toRequestBody("application/json".toMediaType()))
        .build()
    client.newCall(req).execute().use { resp ->
        val text = resp.body?.string().orEmpty()
        check(resp.isSuccessful) { "HTTP ${resp.code}: $text" }
        return JSONObject(text)
    }
}
```

### 5.1 发文本

```kotlin
postJson(client, "$base/v1/text", """{"body":"在吗","to":""}""")
```

- `to` 为空：公共消息，UDP 广播给所有人。
- `to`：可以填设备 id、id 前缀、局域网 IP 或设备名，服务端会解析。
- 返回里的 `acked` 是 600ms 内应答的设备名；**为空不代表失败**，只说明没人应答。
- 正文超过 1000 字节会自动降级为逐台 TCP 投递，此时 `degraded:true`。

### 5.2 收消息 / 收文件

订阅 SSE，断线后重连并回补一次 `/v1/inbox`：

```kotlin
fun watchEvents(base: String, client: OkHttpClient, onEvent: (JSONObject) -> Unit) {
    val stream = client.newBuilder().readTimeout(0, TimeUnit.MILLISECONDS).build()
    thread(name = "xlan-events") {
        while (true) {
            runCatching {
                stream.newCall(Request.Builder().url("$base/v1/events").build())
                    .execute().use { resp ->
                        val src = resp.body?.source() ?: return@use
                        while (true) {
                            val line = src.readUtf8Line() ?: break
                            if (line.startsWith("data: ")) {
                                onEvent(JSONObject(line.substring(6)))
                            }
                        }
                    }
            }
            Thread.sleep(1000)
        }
    }
}
```

事件格式：

```json
{"type":"item.new","ts":1730000000,"data":{ /* 完整 Item */ }}
{"type":"item.update","ts":1730000000,"data":{"id":"...","kind":"file","from_id":"...","status":"done","path":"/data/.../a.bin","name":"a.bin","size":1024}}
{"type":"peer.online","ts":1730000000,"data":{ /* 完整 Peer */ }}
```

收到文件时：

```kotlin
when (ev.optString("type")) {
    "item.new" -> {
        val item = ev.getJSONObject("data")
        if (item.optString("kind") != "text" && item.optString("status") == "pending") {
            // 通知用户；用户点“接收”后：
            postJson(client, "$base/v1/offer/${item.getString("id")}/decision",
                """{"action":"accept"}""")
        }
    }
    "item.update" -> {
        val item = ev.getJSONObject("data")
        if (item.optString("status") == "done") {
            val path = item.optString("path")   // 文件已落在 recvDir，以此为准
            val name = item.optString("name")
        }
    }
}
```

- 文本消息的正文在 `item.body`，`item.kind == "text"` 时不用确认。
- 文件在 `done` 之前没有最终路径；不要自己拼文件名，重名会自动变成 `报告 (1).pdf`。
- 已经信任过的设备或开了 `autoAccept` 时，文件会直接进入 `active` → `done`，不会出现 `pending`。
- `active` 且 5 分钟没有新数据的传输会被服务端标记为 `failed`（`reason:"stale"`），不会永久挂着。

### 5.3 把收到的文件放进系统下载目录

`recvDir` 是应用私有目录，用户看不到，也不建议直接写 `/sdcard/Download`（Android 10+ 会被拦）。
用 MediaStore 复制一份：

```kotlin
fun publishToDownloads(context: Context, file: File) {
    val values = ContentValues().apply {
        put(MediaStore.Downloads.DISPLAY_NAME, file.name)
        put(MediaStore.Downloads.MIME_TYPE, "application/octet-stream")
        put(MediaStore.Downloads.RELATIVE_PATH, Environment.DIRECTORY_DOWNLOADS + "/xlan")
    }
    val uri = context.contentResolver.insert(
        MediaStore.Downloads.EXTERNAL_CONTENT_URI, values
    ) ?: return
    context.contentResolver.openOutputStream(uri)?.use { out ->
        file.inputStream().use { it.copyTo(out) }
    }
}
```

### 5.4 信任设备

```kotlin
postJson(client, "$base/v1/trust", """{"id":"a1b2c3d4e5f60718","trusted":true}""")
```

只影响**本机接收**：被信任的设备再发文件时不用确认。它不会让对方单方面信任你。

## 6. 给其他设备发文件

AAR 没有封装发送，因为发送需要直接访问**对端**的 HTTP 接口（不是本地服务）。流程与 CLI 完全一致：

1. 发现设备，拿到 `peers[].id / name / addr / port`。
2. 建立到对端的 HTTPS 连接（默认开启 TLS，首次连接按 TOFU 记住证书指纹）。
3. `POST {peerBase}/v1/offer` 发起传输。
4. 轮询 `GET {peerBase}/v1/offer/{id}`，等对方接受。
5. 分块 `PUT {peerBase}/v1/offer/{id}/data`，每块带 `Content-Range`。
6. 最后一块完成后服务端校验 SHA-256 并落盘。

### 6.1 对端 TLS

对端默认也是自签名证书。首次连接时记住它的证书指纹，之后不一致就拒绝（和桌面端的 `pins.json` 是同一套思路）：

```kotlin
class PinStore(context: Context) {
    private val prefs = context.getSharedPreferences("xlan_pins", Context.MODE_PRIVATE)
    fun get(host: String): String? = prefs.getString(host, null)
    fun put(host: String, fp: String) = prefs.edit().putString(host, fp).apply()
    fun forget(host: String) = prefs.edit().remove(host).apply()
}

fun peerClient(pins: PinStore, host: String): OkHttpClient {
    val tm = object : X509TrustManager {
        override fun checkClientTrusted(chain: Array<X509Certificate>, authType: String) = Unit

        override fun checkServerTrusted(chain: Array<X509Certificate>, authType: String) {
            val fp = MessageDigest.getInstance("SHA-256")
                .digest(chain[0].encoded)
                .joinToString("") { "%02x".format(it) }
            val known = pins.get(host)
            if (known == null) {
                pins.put(host, fp)               // 首次连接直接记住
            } else {
                check(known.equals(fp, ignoreCase = true)) {
                    "证书指纹变了：$host\n之前 $known\n现在 $fp"
                }
            }
        }

        override fun getAcceptedIssuers(): Array<X509Certificate> = emptyArray()
    }
    val ssl = SSLContext.getInstance("TLS").apply { init(null, arrayOf(tm), null) }
    return OkHttpClient.Builder()
        .sslSocketFactory(ssl.socketFactory, tm)
        .hostnameVerifier { _, _ -> true }
        .build()
}
```

指纹变了就提示用户；确认对方重装过系统后，`pins.forget(host)` 重新信任。
如果对方关了 TLS，就用 `http://addr:port` 和普通客户端（注意明文流量限制）。

### 6.2 发起 offer

```kotlin
val me = getJson(client, "$base/v1/info")
val peers = postJson(client, "$base/v1/scan?deep=1", "{}").getJSONArray("peers")
val peer = (0 until peers.length())
    .map { peers.getJSONObject(it) }
    .first { it.getString("name") == "mbp" }
val host = "${peer.getString("addr")}:${peer.getInt("port")}"
val peerBase = "https://$host"
val peerHttp = peerClient(pins, host)

val offer = JSONObject()
    .put("from_id", me.getString("id"))
    .put("from_name", me.getString("name"))
    .put("to_id", peer.getString("id"))
    .put("kind", "file")
    .put("name", file.name)
    .put("size", file.length())
    .put("sha256", file.sha256Hex())

val resp = postJson(peerHttp, "$peerBase/v1/offer", offer.toString())
if (resp.optBoolean("duplicate")) return   // 对方已有同名同内容文件，不用传
val offerId = resp.getString("id")
```

`sha256Hex()` 需要自己实现：用 `MessageDigest("SHA-256")` 流式读文件，输出**小写十六进制**，与桌面端一致。

响应：

```json
{"ok":true,"id":"...","accepted":false,"status":"pending"}
```

`accepted:false` 表示要等对方点确认。对方信任你或开了自动接收时直接是 `active`。

### 6.3 等对方接受

```kotlin
fun waitAccepted(client: OkHttpClient, peerBase: String, id: String, timeoutMs: Long = 60_000) {
    val deadline = System.currentTimeMillis() + timeoutMs
    while (System.currentTimeMillis() < deadline) {
        val st = getJson(client, "$peerBase/v1/offer/$id")
        when (st.optString("status")) {
            "active", "done" -> return
            "failed", "expired" -> error("对方拒绝或传输已取消")
        }
        Thread.sleep(400)
    }
    error("等待对方确认超时")
}
```

### 6.4 分块上传

块大小与 CLI 一致，用 4 MiB；块与块可以并发（默认 4 路，见配置 `threads`），失败重试 3 次。

```kotlin
fun uploadFile(client: OkHttpClient, peerBase: String, id: String, file: File) {
    val total = file.length()
    val chunk = 4L * 1024 * 1024
    var off = 0L
    while (off < total) {
        val n = minOf(chunk, total - off)
        val buf = ByteArray(n.toInt())
        RandomAccessFile(file, "r").use { raf ->
            raf.seek(off)
            raf.readFully(buf)
        }
        val req = Request.Builder()
            .url("$peerBase/v1/offer/$id/data")
            .put(buf.toRequestBody("application/octet-stream".toMediaType()))
            .header("Content-Range", "bytes $off-${off + n - 1}/$total")
            .build()
        client.newCall(req).execute().use { resp ->
            val text = resp.body?.string().orEmpty()
            check(resp.isSuccessful) { "上传失败 HTTP ${resp.code}: $text" }
        }
        off += n
    }

    // 最后一块返回 done:true；并发上传时再轮询到 status=done 更稳
}
```

- `Content-Range` 的 `end` 是**闭区间**，服务端按偏移直接写，乱序、并发都可以。
- 校验失败返回 `422 CHECKSUM`；缺块或大小对不上会返回 `400`。
- 上传完成后对方收件箱里会出现一条 `done` 记录，路径由对方生成。

## 7. 发目录（可选）

目录发送比单文件多一层清单：

1. `POST {peerBase}/v1/offer`，`kind:"folder"`，带 `manifest`：

```json
{
  "from_id": "...", "from_name": "...", "to_id": "...",
  "kind": "folder", "transfer_id": "稳定的目录 id",
  "manifest": {
    "root": "照片",
    "entries": [
      {"rel":"2026/a.jpg","size":1024,"sha256":"..."},
      {"rel":"空目录","dir":true}
    ]
  }
}
```

2. 对方接受后，逐个文件 `PUT {peerBase}/v1/offer/{id}/file/{rel}`。
   `rel` 里的每一段都要 URL 编码；`Content-Range` 与单文件相同，`total` 必须是该文件大小。
3. 全部传完调 `POST {peerBase}/v1/offer/{id}/finish`，服务端检查清单完整性，然后把整条记录置为 `done`。
4. `transfer_id` 用目录清单算一个稳定值：重发同一目录时服务端返回 `resumed:true` 和 `done_files`，跳过已完成的文件。
   空文件不会走上传，`finish` 时会自动补出来。

## 8. 与桌面版的差异

- 重启在进程内完成，不会 exec 新进程；桌面 CLI 仍然换进程，行为不变。
- `xlan stop`、`pause/resume`、`autostart`、`path`、`sends` 这些 CLI 子命令在 AAR 里不存在（停服务用 `Mobile.stop()`），需要 App 自己实现等价功能。
- 服务与 App 同进程：Go 侧 panic 会带崩整个应用。对稳定性要求高的场景仍然建议用独立可执行文件的方式（jniLibs + 前台服务）。
- 发现依赖 UDP 广播，部分 Wi-Fi 会过滤；失败时用 `POST /v1/scan?deep=1`（TCP 探测 /24），并确认 MulticastLock 已获取。
- 要把收到的文件放进系统下载目录，用 MediaStore 从私有目录复制，不要直接写 `/sdcard/Download`。
- 0 字节文件不会触发上传请求，接收端会停在 `active`；需要空文件时用目录方式发送。

## 9. 常见问题

**`start` 返回超时**：8 秒内服务没有可访问。看 `Mobile.setLogger` 的输出，常见原因是端口 43503–43505 被占、数据目录不可写，或同一设备已经跑着另一份服务。

**扫描不到设备**：先确认两台设备在同一个网段；关掉 Wi‑Fi 的客户端隔离；确认已获取 `MulticastLock`；仍然不行就调 `/v1/scan?deep=1`。

**TLS 握手失败**：确认 `localClient` 是在 `start` 之后创建的，读的是同一份 `<dataDir>/tls.crt`；重装应用后旧证书会变，重建客户端即可。

**事件流很快断开**：SSE 的客户端必须把读超时设为 0（`readTimeout(0, TimeUnit.MILLISECONDS)`），否则 OkHttp 默认 10 秒就会断。

**收到 `pending` 但一直不动**：只有 `pending` 能 `accept`；`active` / `done` / `failed` 再操作会返回冲突。传输卡住可以用 `{"action":"cancel"}` 清掉。

**App 退到后台就收不到**：Android 会冻结后台进程，服务必须放在前台 Service 里；部分厂商还需要在电池优化里放行。
