package com.example.tetherflow.core.network

import com.example.tetherflow.core.model.LocalNetworkInterface
import com.example.tetherflow.core.model.SharingMode
import java.net.Inet4Address
import java.net.NetworkInterface
import java.util.Collections

object NetworkInterfaceManager {

    fun getAvailableInterfaces(): List<LocalNetworkInterface> {
        val list = mutableListOf<LocalNetworkInterface>()
        try {
            val interfaces = Collections.list(NetworkInterface.getNetworkInterfaces())
            for (intf in interfaces) {
                if (!intf.isUp || intf.isLoopback) continue
                val name = intf.name.lowercase()
                val addresses = Collections.list(intf.inetAddresses)
                for (addr in addresses) {
                    if (addr is Inet4Address && !addr.isLoopbackAddress) {
                        val ip = addr.hostAddress ?: continue
                        val mode = when {
                            name.startsWith("rndis") || name.startsWith("usb") || name.startsWith("ncm") -> {
                                SharingMode.USB_TETHERING
                            }
                            name.startsWith("p2p") -> {
                                SharingMode.WIFI_DIRECT
                            }
                            name.startsWith("ap") || name.startsWith("softap") -> {
                                SharingMode.LOCAL_HOTSPOT
                            }
                            name.startsWith("wlan") -> {
                                if (ip.startsWith("192.168.43.")) SharingMode.LOCAL_HOTSPOT
                                else if (ip.startsWith("192.168.49.")) SharingMode.WIFI_DIRECT
                                else SharingMode.LAN_PROXY
                            }
                            else -> SharingMode.LAN_PROXY
                        }
                        list.add(LocalNetworkInterface(name = intf.name, ipAddress = ip, type = mode))
                    }
                }
            }
        } catch (e: Exception) {
            e.printStackTrace()
        }

        // Sort: USB tethering & Wi-Fi Direct first, then Hotspot, then LAN
        return list.sortedBy {
            when (it.type) {
                SharingMode.USB_TETHERING -> 1
                SharingMode.WIFI_DIRECT -> 2
                SharingMode.LOCAL_HOTSPOT -> 3
                SharingMode.LAN_PROXY -> 4
                else -> 5
            }
        }
    }
}
