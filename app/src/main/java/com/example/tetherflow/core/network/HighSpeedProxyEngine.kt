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
    private val scope = CoroutineScope(Dispatchers.IO + SupervisorJob())
    private var serverSocket: ServerSocket? = null
    private var cellularNetwork: Network? = null
    private var networkCallback: ConnectivityManager.NetworkCallback? = null

    @Volatile
    private var isRunning = false

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

    // 64 KB Buffer pool for zero-allocation streaming
    private val BUFFER_SIZE = 64 * 1024
    private val bufferPool = ConcurrentLinkedQueue<ByteArray>()

    private fun acquireBuffer(): ByteArray = bufferPool.poll() ?: ByteArray(BUFFER_SIZE)
    private fun releaseBuffer(buf: ByteArray) {
        if (bufferPool.size < 64) bufferPool.offer(buf)
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
                    receiveBufferSize = 1024 * 1024
                    bind(InetSocketAddress("0.0.0.0", port), 256)
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

    private fun createConnectedRemoteSocket(host: String, port: Int): Socket {
        val cm = context.getSystemService(Context.CONNECTIVITY_SERVICE) as? ConnectivityManager
        val net = cellularNetwork ?: cm?.let { findActiveCellularNetwork(it) }

        val addresses = try {
            net?.getAllByName(host) ?: java.net.InetAddress.getAllByName(host)
        } catch (_: Exception) {
            java.net.InetAddress.getAllByName(host)
        }

        var lastException: Exception? = null
        for (addr in addresses) {
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
                s.connect(InetSocketAddress(addr, port), 4000)
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
            socket.receiveBufferSize = 2 * 1024 * 1024
            socket.sendBufferSize = 2 * 1024 * 1024
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

    private suspend fun handleConnection(clientSocket: Socket) = withContext(Dispatchers.IO) {
        var remoteSocket: Socket? = null
        try {
            val clientIn = clientSocket.getInputStream()
            val clientOut = clientSocket.getOutputStream()

            val firstLine = readAsciiLine(clientIn) ?: return@withContext
            val parts = firstLine.trim().split(" ")
            if (parts.size < 2) return@withContext

            val method = parts[0].uppercase()
            val rawUri = parts[1]

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
            } else if (rawUri.startsWith("/download/win")) {
                skipHeaders(clientIn)
                serveAssetFile("tetherflow-win.exe", "application/vnd.microsoft.portable-executable", clientOut)
            } else if (rawUri.startsWith("/download/mac")) {
                skipHeaders(clientIn)
                serveAssetFile("tetherflow-mac-arm64", "application/octet-stream", clientOut)
            } else if (rawUri == "/pac" || rawUri == "/proxy.pac") {
                // Return dynamic PAC script
                skipHeaders(clientIn)
                val hostHeader = clientSocket.localAddress?.hostAddress ?: "192.168.42.129"
                val pacContent = """
                    function FindProxyForURL(url, host) {
                        if (isPlainHostName(host) || 
                            shExpMatch(host, "*.local") || 
                            isInNet(dnsResolve(host), "10.0.0.0", "255.0.0.0") || 
                            isInNet(dnsResolve(host), "172.16.0.0", "255.240.0.0") || 
                            isInNet(dnsResolve(host), "192.168.0.0", "255.255.0.0") ||
                            isInNet(dnsResolve(host), "127.0.0.0", "255.0.0.0")) {
                            return "DIRECT";
                        }
                        return "PROXY $hostHeader:$port; DIRECT";
                    }
                """.trimIndent()

                val response = "HTTP/1.1 200 OK\r\n" +
                        "Content-Type: application/x-ns-proxy-autoconfig\r\n" +
                        "Content-Length: ${pacContent.toByteArray().size}\r\n" +
                        "Connection: close\r\n\r\n" +
                        pacContent
                clientOut.write(response.toByteArray())
                clientOut.flush()
            } else if (rawUri == "/" || rawUri == "/setup") {
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
                            .card { background: #1A1F2C; max-width: 580px; margin: 20px auto; padding: 28px; border-radius: 20px; box-shadow: 0 10px 40px rgba(0,0,0,0.5); }
                            h1 { color: #00E676; font-size: 24px; margin-bottom: 6px; }
                            .badge { background: #00E676; color: #000; font-weight: bold; padding: 4px 12px; border-radius: 20px; font-size: 12px; display: inline-block; margin-bottom: 16px; }
                            .code-box { background: #0A0D14; padding: 14px; border-radius: 10px; font-family: 'Consolas', monospace; font-size: 13px; color: #80D8FF; margin: 10px 0; word-break: break-all; border: 1px solid #2B3345; }
                            h3 { color: #FFF; font-size: 16px; margin-top: 20px; margin-bottom: 6px; }
                            p { color: #A0AAB8; font-size: 14px; line-height: 1.5; margin: 4px 0; }
                            .btn-download { display: inline-block; background: #00E676; color: #000; text-decoration: none; padding: 12px 20px; border-radius: 12px; font-weight: bold; font-size: 14px; margin: 8px 6px 8px 0; }
                            .btn-download:hover { background: #00C853; }
                        </style>
                    </head>
                    <body>
                        <div class="card">
                            <span class="badge">已连接到三星 5G 高速网关</span>
                            <h1>TetherFlow 电脑端无感伴侣</h1>
                            <p>速度与手机本体 5G 满速 1:1 一致，零扣除 Hotspot 热点流量。</p>
                            
                            <hr style="border: 0; border-top: 1px solid #2B3345; margin: 20px 0;">

                            <h3>🚀 电脑端无感神器（下载即用，免配置）</h3>
                            <p>只需在电脑上运行一次，之后每次插上 USB 或连上 Wi-Fi 电脑<b>自动接通上网</b>，拔出<b>自动断开恢复</b>：</p>
                            <div style="margin: 14px 0;">
                                <a class="btn-download" href="/download/win">⬇️ 下载 Windows 纯无感伴侣 (.exe)</a>
                                <a class="btn-download" href="/download/mac" style="background: #80D8FF;">⬇️ 下载 Mac 纯无感伴侣</a>
                            </div>

                            <hr style="border: 0; border-top: 1px solid #2B3345; margin: 20px 0;">

                            <h3>🪟 免下载：Windows 11 / 10 终端一键命令</h3>
                            <p>在 PowerShell 中回车运行一次：</p>
                            <div class="code-box">Set-ItemProperty -Path 'HKCU:\Software\Microsoft\Windows\CurrentVersion\Internet Settings' -Name AutoConfigURL -Value 'http://$ip:$port/pac'</div>

                            <h3>🍎 免下载：macOS 终端一键命令</h3>
                            <div class="code-box">sudo networksetup -setautoproxyurl "Wi-Fi" "http://$ip:$port/pac"</div>
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

        val uploadJob = launch(Dispatchers.IO) {
            try {
                pumpStream(clientIn, remoteOut, totalUp)
            } finally {
                try { remote.shutdownOutput() } catch (_: Exception) {}
            }
        }
        val downloadJob = launch(Dispatchers.IO) {
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
                output.flush()
                counter.addAndGet(bytesRead.toLong())
            }
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
