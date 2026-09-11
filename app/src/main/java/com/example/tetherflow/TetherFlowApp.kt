package com.example.tetherflow

import android.app.Application
import android.app.NotificationChannel
import android.app.NotificationManager
import android.os.Build

class TetherFlowApp : Application() {

    companion object {
        const val NOTIFICATION_CHANNEL_ID = "tetherflow_service_channel"
    }

    override fun onCreate() {
        super.onCreate()
        createNotificationChannel()
    }

    private fun createNotificationChannel() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            val channel = NotificationChannel(
                NOTIFICATION_CHANNEL_ID,
                "TetherFlow Active Service",
                NotificationManager.IMPORTANCE_LOW
            ).apply {
                description = "Shows real-time connection status and transfer speeds"
                setShowBadge(false)
            }
            val manager = getSystemService(NotificationManager::class.java)
            manager?.createNotificationChannel(channel)
        }
    }
}
