package com.example.tetherflow.receiver

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent
import android.net.ConnectivityManager
import android.os.BatteryManager
import com.example.tetherflow.core.network.NetworkInterfaceManager
import com.example.tetherflow.service.ProxyForegroundService

/**
 * 自动感知并秒级自启广播接收器 (Auto-starts proxy when Tethering / USB is enabled)
 */
class TetheringAutoStartReceiver : BroadcastReceiver() {

    override fun onReceive(context: Context, intent: Intent?) {
        val action = intent?.action ?: return

        when (action) {
            // 1. USB 连接状态变更 (插线/拔线/开启 USB 网络共享)
            "android.hardware.usb.action.USB_STATE" -> {
                val connected = intent.getBooleanExtra("connected", false)
                val configured = intent.getBooleanExtra("configured", false)
                val rndis = intent.getBooleanExtra("rndis", false)
                val ncm = intent.getBooleanExtra("ncm", false)

                if (rndis || ncm) {
                    // USB 网络共享开启，后台秒级自启代理引擎
                    if (!ProxyForegroundService.isRunning()) {
                        ProxyForegroundService.startService(context.applicationContext)
                    }
                }
            }

            // 2. 系统原生热点 / Tethering 状态变更
            "android.net.conn.TETHER_STATE_CHANGED" -> {
                val interfaces = NetworkInterfaceManager.getAvailableInterfaces()
                val hasTethering = interfaces.any {
                    it.name.startsWith("rndis") || it.name.startsWith("usb") ||
                    it.name.startsWith("p2p") || it.name.startsWith("ap")
                }

                if (hasTethering) {
                    if (!ProxyForegroundService.isRunning()) {
                        ProxyForegroundService.startService(context.applicationContext)
                    }
                }
            }

            // 3. Wi-Fi AP 状态广播
            "android.net.wifi.WIFI_AP_STATE_CHANGED" -> {
                val state = intent.getIntExtra("wifi_state", 0)
                // 13 = WIFI_AP_STATE_ENABLED
                if (state == 13) {
                    if (!ProxyForegroundService.isRunning()) {
                        ProxyForegroundService.startService(context.applicationContext)
                    }
                }
            }
        }
    }
}
