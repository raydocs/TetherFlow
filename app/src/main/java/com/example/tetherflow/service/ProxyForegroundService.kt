package com.example.tetherflow.service

import android.annotation.SuppressLint
import android.app.Notification
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.content.pm.ServiceInfo
import android.net.wifi.WifiManager
import android.os.Build
import android.os.IBinder
import android.os.PowerManager
import androidx.core.app.NotificationCompat
import com.example.tetherflow.MainActivity
import com.example.tetherflow.R
import com.example.tetherflow.TetherFlowApp
import com.example.tetherflow.core.model.ConnectionStats
import com.example.tetherflow.core.network.HighSpeedProxyEngine
import kotlinx.coroutines.*
import kotlinx.coroutines.flow.StateFlow

class ProxyForegroundService : Service() {

    companion object {
        const val ACTION_START = "com.example.tetherflow.action.START"
        const val ACTION_STOP = "com.example.tetherflow.action.STOP"
        private const val NOTIFICATION_ID = 1001

        private var engineInstance: HighSpeedProxyEngine? = null

        fun getStatsFlow(): StateFlow<ConnectionStats>? = engineInstance?.stats
        fun isRunning(): Boolean = engineInstance?.stats?.value?.isRunning == true

        fun startService(context: Context) {
            val intent = Intent(context, ProxyForegroundService::class.java).apply {
                action = ACTION_START
            }
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
                context.startForegroundService(intent)
            } else {
                context.startService(intent)
            }
        }

        fun stopService(context: Context) {
            val intent = Intent(context, ProxyForegroundService::class.java).apply {
                action = ACTION_STOP
            }
            context.startService(intent)
        }
    }

    private val serviceScope = CoroutineScope(Dispatchers.Main + SupervisorJob())
    private var wakeLock: PowerManager.WakeLock? = null
    private var wifiLock: WifiManager.WifiLock? = null
    private var notificationManager: NotificationManager? = null

    override fun onCreate() {
        super.onCreate()
        notificationManager = getSystemService(Context.NOTIFICATION_SERVICE) as NotificationManager
        acquireLocks()
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        when (intent?.action) {
            ACTION_STOP -> {
                stopProxy()
                stopForeground(STOP_FOREGROUND_REMOVE)
                stopSelf()
                return START_NOT_STICKY
            }
            ACTION_START, null -> {
                startProxy()
            }
        }
        return START_STICKY
    }

    @SuppressLint("WakelockTimeout")
    private fun acquireLocks() {
        val powerManager = getSystemService(Context.POWER_SERVICE) as? PowerManager
        wakeLock = powerManager?.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "TetherFlow::ProxyCpuLock")?.apply {
            setReferenceCounted(false)
            acquire()
        }

        val wifiManager = applicationContext.getSystemService(Context.WIFI_SERVICE) as? WifiManager
        wifiLock = wifiManager?.createWifiLock(
            if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) WifiManager.WIFI_MODE_FULL_HIGH_PERF
            else WifiManager.WIFI_MODE_FULL,
            "TetherFlow::ProxyWifiLock"
        )?.apply {
            setReferenceCounted(false)
            acquire()
        }
    }

    private fun startProxy() {
        if (engineInstance == null) {
            engineInstance = HighSpeedProxyEngine(applicationContext, port = 8282).apply {
                start()
            }
        }

        val initialNotification = buildNotification("Starting high-speed engine...", "Port 8282")
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            startForeground(
                NOTIFICATION_ID,
                initialNotification,
                ServiceInfo.FOREGROUND_SERVICE_TYPE_CONNECTED_DEVICE
            )
        } else {
            startForeground(NOTIFICATION_ID, initialNotification)
        }

        // Collect stats and update notification dynamically
        serviceScope.launch {
            engineInstance?.stats?.collect { stats ->
                if (stats.isRunning) {
                    val downMbps = (stats.downloadSpeedBps * 8.0 / (1024 * 1024))
                    val upMbps = (stats.uploadSpeedBps * 8.0 / (1024 * 1024))
                    val title = "⬇️ %.1f Mbps  ⬆️ %.1f Mbps".format(downMbps, upMbps)
                    val content = "${stats.activeClients} Active Devices | Total: ${formatBytes(stats.totalBytesDownloaded)}"
                    notificationManager?.notify(NOTIFICATION_ID, buildNotification(title, content))
                }
            }
        }
    }

    private fun stopProxy() {
        engineInstance?.stop()
        engineInstance = null
        serviceScope.cancel()

        try {
            if (wakeLock?.isHeld == true) wakeLock?.release()
            if (wifiLock?.isHeld == true) wifiLock?.release()
        } catch (_: Exception) {}
    }

    private fun buildNotification(title: String, content: String): Notification {
        val openAppIntent = Intent(this, MainActivity::class.java)
        val pendingOpen = PendingIntent.getActivity(
            this, 0, openAppIntent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )

        val stopIntent = Intent(this, ProxyForegroundService::class.java).apply {
            action = ACTION_STOP
        }
        val pendingStop = PendingIntent.getService(
            this, 1, stopIntent,
            PendingIntent.FLAG_IMMUTABLE or PendingIntent.FLAG_UPDATE_CURRENT
        )

        return NotificationCompat.Builder(this, TetherFlowApp.NOTIFICATION_CHANNEL_ID)
            .setSmallIcon(R.drawable.ic_stat_tetherflow)
            .setContentTitle(title)
            .setContentText(content)
            .setOngoing(true)
            .setOnlyAlertOnce(true)
            .setContentIntent(pendingOpen)
            .addAction(R.drawable.ic_stat_tetherflow, getString(R.string.stop_service), pendingStop)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .build()
    }

    private fun formatBytes(bytes: Long): String {
        val mb = bytes / (1024.0 * 1024.0)
        return if (mb >= 1024) "%.2f GB".format(mb / 1024.0) else "%.1f MB".format(mb)
    }

    override fun onDestroy() {
        stopProxy()
        super.onDestroy()
    }

    override fun onBind(intent: Intent?): IBinder? = null
}
