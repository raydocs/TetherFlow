package com.example.tetherflow.core.network

import android.content.Context
import android.net.ConnectivityManager
import android.net.Network
import android.net.NetworkCapabilities
import android.net.NetworkRequest
import com.example.tetherflow.core.model.ConnectionStats
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import java.io.InputStream
import java.io.OutputStream
import java.net.InetSocketAddress
import java.net.ServerSocket
import java.net.Socket
import java.util.concurrent.ConcurrentLinkedQueue
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong

class HighSpeedProxyEngine(
    private val context: Context,
    val port: Int = 8282
) {
    @OptIn(ExperimentalCoroutinesApi::class)
    private val proxyDispatcher = Dispatchers.IO.limitedParallelism(512)
    private val scope = CoroutineScope(proxyDispatcher + SupervisorJob())
    private var serverSocket: ServerSocket? = null
    private var cellularNetwork: Network? = null
    private var networkCallback: ConnectivityManager.NetworkCallback? = null

    @Volatile
    private var isRunning = false

    // High-speed DNS cache (5 min TTL) to eliminate per-request cellular DNS latency
    private val dnsCache = java.util.concurrent.ConcurrentHashMap<String, Pair<Long, Array<java.net.InetAddress>>>()

    // Real-time Traffic Counters
    private val totalDown = AtomicLong(0L)
    private val totalUp = AtomicLong(0L)
    private val activeConnections = AtomicInteger(0)

    // Rate calculation
    private var lastDownBytes = 0L
    private var lastUpBytes = 0L
    private var lastRateCalcTime = System.currentTimeMillis()

    private val _stats = MutableStateFlow(ConnectionStats())
    val stats: StateFlow<ConnectionStats> = _stats.asStateFlow()

    // 512 KB Buffer pool for Multi-Gigabit zero-allocation streaming
    private val BUFFER_SIZE = 512 * 1024
    private val bufferPool = ConcurrentLinkedQueue<ByteArray>()

    private fun acquireBuffer(): ByteArray = bufferPool.poll() ?: ByteArray(BUFFER_SIZE)
    private fun releaseBuffer(buf: ByteArray) {
        if (bufferPool.size < 256) bufferPool.offer(buf)
    }

    fun start() {
        if (isRunning) return
        isRunning = true

        bindCellularUpstream()
        startStatsTicker()

        scope.launch {
            try {
                serverSocket = ServerSocket().apply {
                    reuseAddress = true
                    receiveBufferSize = 4 * 1024 * 1024
                    bind(InetSocketAddress("0.0.0.0", port), 1024)
                }

                _stats.update {
                    it.copy(
                        isRunning = true,
                        localInterfaces = NetworkInterfaceManager.getAvailableInterfaces()
                    )
                }

                while (isRunning) {
                    val clientSocket = serverSocket?.accept() ?: break
                    tuneSocket(clientSocket)
                    activeConnections.incrementAndGet()

                    scope.launch {
                        try {
                            handleConnection(clientSocket)
                        } finally {
                            activeConnections.decrementAndGet()
                        }
                    }
                }
            } catch (e: Exception) {
                if (isRunning) e.printStackTrace()
            }
        }
    }

    private fun bindCellularUpstream() {
        val cm = context.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager ?: return
        cellularNetwork = findActiveCellularNetwork(cm)

        val request = NetworkRequest.Builder()
            .addTransportType(NetworkCapabilities.TRANSPORT_CELLULAR)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET)
            .addCapability(NetworkCapabilities.NET_CAPABILITY_NOT_RESTRICTED)
            .build()

        val callback = object : ConnectivityManager.NetworkCallback() {
            override fun onAvailable(network: Network) {
                val caps = cm.getNetworkCapabilities(network)
                if (caps?.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_RESTRICTED) == true) {
                    cellularNetwork = network
                }
            }
            override fun onCapabilitiesChanged(network: Network, caps: NetworkCapabilities) {
                if (caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) &&
                    caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_RESTRICTED)) {
                    cellularNetwork = network
                }
            }
            override fun onLost(network: Network) {
                if (cellularNetwork == network) {
                    cellularNetwork = findActiveCellularNetwork(cm)
                }
            }
        }
        networkCallback = callback
        try {
            cm.requestNetwork(request, callback)
        } catch (e: Exception) {
            e.printStackTrace()
        }
    }

    private fun findActiveCellularNetwork(cm: ConnectivityManager): Network? {
        try {
            for (net in cm.allNetworks) {
                val caps = cm.getNetworkCapabilities(net) ?: continue
                if (caps.hasTransport(NetworkCapabilities.TRANSPORT_CELLULAR) &&
                    caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_INTERNET) &&
                    caps.hasCapability(NetworkCapabilities.NET_CAPABILITY_NOT_RESTRICTED)) {
                    return net
                }
            }
        } catch (_: Exception) {}
        return null
    }

    private fun resolveDns(net: Network?, host: String): Array<java.net.InetAddress> {
        val now = System.currentTimeMillis()
        dnsCache[host]?.let { (expireAt, addrs) ->
            if (now < expireAt) return addrs
        }
        val addrs = try {
            net?.getAllByName(host) ?: java.net.InetAddress.getAllByName(host)
        } catch (_: Exception) {
            java.net.InetAddress.getAllByName(host)
        }
        dnsCache[host] = Pair(now + 300_000L, addrs)
        return addrs
    }

    private fun createConnectedRemoteSocket(host: String, port: Int): Socket {
        val cm = context.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager
        val net = cellularNetwork ?: cm?.let { findActiveCellularNetwork(it) }
        val addresses = resolveDns(net, host)
        // Prefer IPv4 first for lowest latency and carrier CGNAT compatibility
        val sortedAddresses = addresses.sortedBy { if (it is java.net.Inet4Address) 0 else 1 }

        var lastException: Exception? = null
        for (addr in sortedAddresses) {
            val s = if (net != null) {
                try {
                    net.socketFactory.createSocket()
                } catch (_: Exception) {
                    Socket().also { net.bindSocket(it) }
                }
            } else {
                Socket()
            }

            tuneSocket(s)
            try {
                s.connect(InetSocketAddress(addr, port), 1200)
                return s
            } catch (e: Exception) {
                try { s.close() } catch (_: Exception) {}
                lastException = e
            }
        }
        throw lastException ?: java.io.IOException("Unable to connect to $host:$port")
    }

    private fun tuneSocket(socket: Socket) {
        try {
            socket.tcpNoDelay = true
            socket.receiveBufferSize = 4 * 1024 * 1024
            socket.sendBufferSize = 4 * 1024 * 1024
            socket.soTimeout = 0 // Infinite for streaming
            socket.keepAlive = true
        } catch (_: Exception) {}
    }

    private fun startStatsTicker() {
        scope.launch {
            while (isRunning) {
                delay(1000)
                val now = System.currentTimeMillis()
                val elapsedSec = (now - lastRateCalcTime).coerceAtLeast(1) / 1000.0

                val currentDown = totalDown.get()
                val currentUp = totalUp.get()

                val downSpeed = ((currentDown - lastDownBytes) / elapsedSec).toLong().coerceAtLeast(0)
                val upSpeed = ((currentUp - lastUpBytes) / elapsedSec).toLong().coerceAtLeast(0)

                lastDownBytes = currentDown
                lastUpBytes = currentUp
                lastRateCalcTime = now

                _stats.update {
                    it.copy(
                        isRunning = true,
                        activeClients = activeConnections.get(),
                        downloadSpeedBps = downSpeed,
                        uploadSpeedBps = upSpeed,
                        totalBytesDownloaded = currentDown,
                        totalBytesUploaded = currentUp,
                        localInterfaces = NetworkInterfaceManager.getAvailableInterfaces()
                    )
                }
            }
        }
    }

    private suspend fun handleConnection(clientSocket: Socket) = withContext(proxyDispatcher) {
        var remoteSocket: Socket? = null
        try {
            val clientIn = clientSocket.getInputStream()
            val clientOut = clientSocket.getOutputStream()

            val firstLine = readAsciiLine(clientIn) ?: return@withContext
            val parts = firstLine.trim().split(" ")
            if (parts.size < 2) return@withContext

            val method = parts[0].uppercase()
            val rawUri = parts[1]

            val uriPath = if (rawUri.startsWith("http://", ignoreCase = true)) {
                val afterProto = rawUri.substring(7)
                val slashIdx = afterProto.indexOf('/')
                if (slashIdx != -1) afterProto.substring(slashIdx) else "/"
            } else rawUri

            if (method == "CONNECT") {
                // HTTPS Tunnel
                val hostPort = rawUri.split(":")
                val host = hostPort[0]
                val port = if (hostPort.size > 1) hostPort[1].toIntOrNull() ?: 443 else 443

                skipHeaders(clientIn)

                remoteSocket = createConnectedRemoteSocket(host, port)

                val established = "HTTP/1.1 200 Connection Established\r\n\r\n".toByteArray()
                clientOut.write(established)
                clientOut.flush()

                pumpDuplex(clientSocket, remoteSocket)
            } else if (uriPath.startsWith("/download/win")) {
                skipHeaders(clientIn)
                serveAssetFile("tetherflow-win.exe", "application/vnd.microsoft.portable-executable", clientOut)
            } else if (uriPath.startsWith("/download/mac")) {
                skipHeaders(clientIn)
                serveAssetFile("tetherflow-mac-arm64", "application/octet-stream", clientOut)
            } else if (uriPath.startsWith("/pac") || uriPath.startsWith("/proxy.pac")) {
                // Return dynamic PAC script with Smart Split & Dual-Net Load Balancing
                skipHeaders(clientIn)
                val hostHeader = clientSocket.localAddress?.hostAddress ?: "192.168.42.129"
                val queryIdx = rawUri.indexOf('?')
                val query = if (queryIdx != -1) rawUri.substring(queryIdx + 1) else ""
                var mode = "split"
                for (pair in query.split("&")) {
                    val kv = pair.split("=")
                    if (kv.size == 2 && kv[0].equals("mode", ignoreCase = true)) {
                        mode = kv[1].lowercase()
                    }
                }
                val pacContent = generatePacScript(hostHeader, port, mode)

                val response = "HTTP/1.1 200 OK\r\n" +
                        "Content-Type: application/x-ns-proxy-autoconfig\r\n" +
                        "Content-Length: ${pacContent.toByteArray().size}\r\n" +
                        "Connection: close\r\n\r\n" +
                        pacContent
                clientOut.write(response.toByteArray())
                clientOut.flush()
            } else if (uriPath == "/" || uriPath == "/setup") {
                // Landing page for easy 1-click Win/Mac setup
                skipHeaders(clientIn)
                val ip = clientSocket.localAddress?.hostAddress ?: "192.168.42.129"
                val html = """
                    <!DOCTYPE html>
                    <html>
                    <head>
                        <meta charset="utf-8">
                        <meta name="viewport" content="width=device-width,initial-scale=1">
                        <title>TetherFlow 零感高速网关</title>
                        <style>
                            body { font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; background: #0E1117; color: #E0E0E0; padding: 20px; }
                            .card { background: #1A1F2C; max-width: 620px; margin: 20px auto; padding: 28px; border-radius: 20px; box-shadow: 0 10px 40px rgba(0,0,0,0.5); }
                            h1 { color: #00E676; font-size: 24px; margin-bottom: 6px; }
                            .badge { background: #00E676; color: #000; font-weight: bold; padding: 4px 12px; border-radius: 20px; font-size: 12px; display: inline-block; margin-bottom: 16px; }
                            .code-box { background: #0A0D14; padding: 12px; border-radius: 10px; font-family: 'Consolas', monospace; font-size: 12px; color: #80D8FF; margin: 8px 0; word-break: break-all; border: 1px solid #2B3345; }
                            h3 { color: #FFF; font-size: 16px; margin-top: 20px; margin-bottom: 6px; }
                            p { color: #A0AAB8; font-size: 14px; line-height: 1.5; margin: 4px 0; }
                            .btn-download { display: inline-block; background: #00E676; color: #000; text-decoration: none; padding: 12px 20px; border-radius: 12px; font-weight: bold; font-size: 14px; margin: 8px 6px 8px 0; }
                            .btn-download:hover { background: #00C853; }
                            .mode-tag { background: #2B3345; color: #00E676; padding: 2px 8px; border-radius: 6px; font-size: 12px; margin-right: 6px; }
                        </style>
                    </head>
                    <body>
                        <div class="card">
                            <span class="badge">已连接到三星 5G 高速网关</span>
                            <h1>TetherFlow 电脑端极速伴侣</h1>
                            <p>速度与手机本体 5G 满速 1:1 一致，零扣除 Hotspot 热点流量。</p>
                            
                            <hr style="border: 0; border-top: 1px solid #2B3345; margin: 20px 0;">

                            <h3>🚀 电脑端无感神器（托盘/菜单栏，支持 4 大模式一键切换）</h3>
                            <p>只需在电脑上运行一次，之后插上线或连上 Wi-Fi 电脑<b>自动接通上网</b>，拔出<b>自动恢复</b>：</p>
                            <div style="margin: 14px 0;">
                                <a class="btn-download" href="/download/win">⬇️ 下载 Windows 纯无感伴侣 (.exe)</a>
                                <a class="btn-download" href="/download/mac" style="background: #80D8FF;">⬇️ 下载 Mac 纯无感伴侣</a>
                            </div>

                            <hr style="border: 0; border-top: 1px solid #2B3345; margin: 20px 0;">

                            <h3>🎯 核心模式说明</h3>
                            <p><span class="mode-tag">🎯 动静分流</span> 游戏、语音、局域网直连家庭 Wi-Fi (3ms光纤极速)；YouTube 4K、Steam下载走 5G (1.2Gbps)。</p>
                            <p><span class="mode-tag">⚖️ 双网叠加</span> 大文件多线程下载时，家庭 Wi-Fi (150M) + 5G (1050M) 并发拉取，叠加突破千兆！</p>
                            <p><span class="mode-tag">🚀 5G 独享</span> 100% 流量直通手机 5G 满血通道，用于 Speedtest / 测速跑分。</p>

                            <hr style="border: 0; border-top: 1px solid #2B3345; margin: 20px 0;">

                            <h3>🪟 Windows 终端一键配置</h3>
                            <div class="code-box">Set-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings' -Name AutoConfigURL -Value 'http://$ip:$port/pac?mode=split'</div>

                            <h3>🍎 macOS 终端一键配置</h3>
                            <div class="code-box">sudo networksetup -setautoproxyurl "Wi-Fi" "http://$ip:$port/pac?mode=split"</div>
                        </div>
                    </body>
                    </html>
                """.trimIndent()
                val response = "HTTP/1.1 200 OK\r\n" +
                        "Content-Type: text/html; charset=utf-8\r\n" +
                        "Content-Length: ${html.toByteArray().size}\r\n" +
                        "Connection: close\r\n\r\n" +
                        html
                clientOut.write(response.toByteArray())
                clientOut.flush()
            } else {
                // Standard HTTP Proxy request
                forwardPlainHttp(firstLine, clientIn, clientOut)
            }
        } catch (_: Exception) {
        } finally {
            try { clientSocket.close() } catch (_: Exception) {}
            try { remoteSocket?.close() } catch (_: Exception) {}
        }
    }

    private suspend fun forwardPlainHttp(
        firstLine: String,
        clientIn: InputStream,
        clientOut: OutputStream
    ) = withContext(Dispatchers.IO) {
        var remoteSocket: Socket? = null
        try {
            val parts = firstLine.trim().split(" ")
            val method = parts[0]
            val fullUrl = parts[1]
            val version = if (parts.size > 2) parts[2] else "HTTP/1.1"

            val urlWithoutScheme = if (fullUrl.startsWith("http://", ignoreCase = true)) {
                fullUrl.substring(7)
            } else fullUrl

            val slashIdx = urlWithoutScheme.indexOf('/')
            val hostAndPort = if (slashIdx != -1) urlWithoutScheme.substring(0, slashIdx) else urlWithoutScheme
            val path = if (slashIdx != -1) urlWithoutScheme.substring(slashIdx) else "/"

            val hp = hostAndPort.split(":")
            val host = hp[0]
            val port = if (hp.size > 1) hp[1].toIntOrNull() ?: 80 else 80

            remoteSocket = createConnectedRemoteSocket(host, port)

            val remoteOut = remoteSocket.getOutputStream()
            val remoteIn = remoteSocket.getInputStream()

            val newFirstLine = "$method $path $version\r\n"
            remoteOut.write(newFirstLine.toByteArray())

            while (true) {
                val header = readAsciiLine(clientIn) ?: break
                if (header.isEmpty()) {
                    remoteOut.write("\r\n".toByteArray())
                    break
                }
                if (!header.startsWith("Proxy-", ignoreCase = true)) {
                    remoteOut.write("$header\r\n".toByteArray())
                }
            }
            remoteOut.flush()

            pumpStream(remoteIn, clientOut, totalDown)
        } catch (_: Exception) {
        } finally {
            try { remoteSocket?.close() } catch (_: Exception) {}
        }
    }

    private suspend fun pumpDuplex(client: Socket, remote: Socket) = coroutineScope {
        val clientIn = client.getInputStream()
        val clientOut = client.getOutputStream()
        val remoteIn = remote.getInputStream()
        val remoteOut = remote.getOutputStream()

        val uploadJob = launch(proxyDispatcher) {
            try {
                pumpStream(clientIn, remoteOut, totalUp)
            } finally {
                try { remote.shutdownOutput() } catch (_: Exception) {}
            }
        }
        val downloadJob = launch(proxyDispatcher) {
            try {
                pumpStream(remoteIn, clientOut, totalDown)
            } finally {
                try { client.shutdownOutput() } catch (_: Exception) {}
            }
        }

        uploadJob.join()
        downloadJob.join()
    }

    private fun pumpStream(input: InputStream, output: OutputStream, counter: AtomicLong) {
        val buffer = acquireBuffer()
        try {
            var bytesRead: Int
            while (input.read(buffer).also { bytesRead = it } != -1) {
                output.write(buffer, 0, bytesRead)
                counter.addAndGet(bytesRead.toLong())
            }
            output.flush()
        } catch (_: Exception) {
        } finally {
            releaseBuffer(buffer)
        }
    }

    private fun readAsciiLine(input: InputStream): String? {
        val sb = StringBuilder()
        var c: Int
        while (input.read().also { c = it } != -1) {
            if (c == '\n'.code) break
            if (c != '\r'.code) sb.append(c.toChar())
        }
        return if (sb.isEmpty() && c == -1) null else sb.toString()
    }

    private fun skipHeaders(input: InputStream) {
        while (true) {
            val line = readAsciiLine(input) ?: break
            if (line.isEmpty()) break
        }
    }

    private fun serveAssetFile(assetName: String, contentType: String, output: OutputStream) {
        try {
            val assetStream = context.assets.open(assetName)
            val size = assetStream.available()
            val header = "HTTP/1.1 200 OK\r\n" +
                    "Content-Type: $contentType\r\n" +
                    "Content-Disposition: attachment; filename=\"$assetName\"\r\n" +
                    "Content-Length: $size\r\n" +
                    "Connection: close\r\n\r\n"
            output.write(header.toByteArray())
            assetStream.copyTo(output)
            output.flush()
            assetStream.close()
        } catch (e: Exception) {
            val notFound = "HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"
            output.write(notFound.toByteArray())
            output.flush()
        }
    }

    private fun generatePacScript(proxyHost: String, proxyPort: Int, mode: String): String {
        return """
            var _p = "PROXY $proxyHost:$proxyPort; DIRECT";
            var _d = "DIRECT";
            var _counter = 0;

            function isPrivate(h) {
                if (h === "localhost" || h === "router.miwifi.com") return true;
                if (shExpMatch(h, "*.local") || shExpMatch(h, "*.lan") || shExpMatch(h, "*.arpa")) return true;
                if (/^127\./.test(h) || /^10\./.test(h) || /^192\.168\./.test(h) || /^169\.254\./.test(h)) return true;
                if (/^172\.(1[6-9]|2[0-9]|3[0-1])\./.test(h)) return true;
                return false;
            }

            function isVoiceOrGame(h) {
                return (
                    shExpMatch(h, "*.zoom.us") ||
                    shExpMatch(h, "*.zoomgov.com") ||
                    shExpMatch(h, "*.tencent.com") ||
                    shExpMatch(h, "*.wechat.com") ||
                    shExpMatch(h, "*.weixin.qq.com") ||
                    shExpMatch(h, "*.qq.com") ||
                    shExpMatch(h, "*.feishu.cn") ||
                    shExpMatch(h, "*.larksuite.com") ||
                    shExpMatch(h, "*.dingtalk.com") ||
                    shExpMatch(h, "*.teams.microsoft.com") ||
                    shExpMatch(h, "*.skype.com") ||
                    shExpMatch(h, "*.discord.gg") ||
                    shExpMatch(h, "*.discord.com") ||
                    shExpMatch(h, "*.riotgames.com") ||
                    shExpMatch(h, "*.leagueoflegends.com") ||
                    shExpMatch(h, "*.blizzard.com") ||
                    shExpMatch(h, "*.battle.net") ||
                    shExpMatch(h, "*.ea.com") ||
                    shExpMatch(h, "*.origin.com") ||
                    shExpMatch(h, "*.alipay.com") ||
                    shExpMatch(h, "*.alipayobjects.com") ||
                    shExpMatch(h, "*.cmbchina.com") ||
                    shExpMatch(h, "*.boc.cn") ||
                    shExpMatch(h, "*.icbc.com.cn") ||
                    shExpMatch(h, "*.ccb.com") ||
                    shExpMatch(h, "*.chase.com")
                );
            }

            function isBulkMediaOrDownload(h, u) {
                return (
                    shExpMatch(h, "*.googlevideo.com") ||
                    shExpMatch(h, "*.youtube.com") ||
                    shExpMatch(h, "*.ytimg.com") ||
                    shExpMatch(h, "*.nflxvideo.net") ||
                    shExpMatch(h, "*.nflximg.net") ||
                    shExpMatch(h, "*.netflix.com") ||
                    shExpMatch(h, "*.bilivideo.com") ||
                    shExpMatch(h, "*.bilibili.com") ||
                    shExpMatch(h, "*.hdslb.com") ||
                    shExpMatch(h, "*.disneyplus.com") ||
                    shExpMatch(h, "*.steamcontent.com") ||
                    shExpMatch(h, "*.steampipe.akamaized.net") ||
                    shExpMatch(h, "*.epicgames.com") ||
                    shExpMatch(h, "*.githubusercontent.com") ||
                    shExpMatch(h, "*.fast.com") ||
                    shExpMatch(h, "*.speedtest.net") ||
                    shExpMatch(h, "*.windowsupdate.com") ||
                    shExpMatch(h, "*.1drv.ms") ||
                    shExpMatch(u, "*/download/*") ||
                    shExpMatch(u, "*.zip") ||
                    shExpMatch(u, "*.iso") ||
                    shExpMatch(u, "*.dmg") ||
                    shExpMatch(u, "*.pkg") ||
                    shExpMatch(u, "*.exe")
                );
            }

            function FindProxyForURL(url, host) {
                if (isPlainHostName(host) || isPrivate(host)) {
                    return _d;
                }

                // Mode: Full 5G
                if ("$mode" === "full") {
                    return _p;
                }

                // Mode: Dual-Net Aggregation (1:5 Concurrent Balancing)
                if ("$mode" === "dual") {
                    if (isVoiceOrGame(host)) {
                        return _d; // 3ms low latency Home Wi-Fi
                    }
                    if (isBulkMediaOrDownload(host, url)) {
                        _counter = (_counter + 1) % 6;
                        if (_counter === 0) {
                            return _d; // 1 out of 6 chunks to Home Wi-Fi (~150Mbps)
                        }
                        return _p;     // 5 out of 6 chunks to 5G (~1050Mbps)
                    }
                    return _p;
                }

                // Mode: Smart Split (Default)
                if (isVoiceOrGame(host)) {
                    return _d; // Gaming & Voice stay on Home Wi-Fi 3ms
                }
                return _p;     // Video, CDN, & Downloads accelerate on 5G 1.2G
            }
        """.trimIndent()
    }

    fun stop() {
        isRunning = false
        try {
            serverSocket?.close()
        } catch (_: Exception) {}
        serverSocket = null

        val cm = context.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager
        networkCallback?.let {
            try { cm?.unregisterNetworkCallback(it) } catch (_: Exception) {}
        }
        networkCallback = null
        cellularNetwork = null

        _stats.update { it.copy(isRunning = false, activeClients = 0, downloadSpeedBps = 0, uploadSpeedBps = 0) }
        scope.cancel()
    }
}
