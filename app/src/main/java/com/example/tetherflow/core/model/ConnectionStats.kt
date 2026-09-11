package com.example.tetherflow.core.model

enum class SharingMode {
    AUTO,
    USB_TETHERING,
    WIFI_DIRECT,
    LOCAL_HOTSPOT,
    LAN_PROXY
}

data class LocalNetworkInterface(
    val name: String,
    val ipAddress: String,
    val type: SharingMode
)

data class ConnectionStats(
    val isRunning: Boolean = false,
    val activeClients: Int = 0,
    val downloadSpeedBps: Long = 0L,
    val uploadSpeedBps: Long = 0L,
    val totalBytesDownloaded: Long = 0L,
    val totalBytesUploaded: Long = 0L,
    val localInterfaces: List<LocalNetworkInterface> = emptyList(),
    val wifiSsid: String? = null,
    val wifiPassword: String? = null,
    val sharingMode: SharingMode = SharingMode.AUTO
)
