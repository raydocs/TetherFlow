package com.example.tetherflow.service

import android.os.Build
import android.service.quicksettings.Tile
import android.service.quicksettings.TileService
import androidx.annotation.RequiresApi

@RequiresApi(Build.VERSION_CODES.N)
class ProxyTileService : TileService() {

    override fun onStartListening() {
        super.onStartListening()
        updateTileState()
    }

    override fun onClick() {
        super.onClick()
        val isCurrentlyRunning = ProxyForegroundService.isRunning()
        if (isCurrentlyRunning) {
            ProxyForegroundService.stopService(applicationContext)
        } else {
            ProxyForegroundService.startService(applicationContext)
        }
        // Small delay to allow state change
        qsTile?.state = if (!isCurrentlyRunning) Tile.STATE_ACTIVE else Tile.STATE_INACTIVE
        qsTile?.updateTile()
    }

    private fun updateTileState() {
        val tile = qsTile ?: return
        val running = ProxyForegroundService.isRunning()
        tile.state = if (running) Tile.STATE_ACTIVE else Tile.STATE_INACTIVE
        tile.subtitle = if (running) "8282 Active" else "Off"
        tile.updateTile()
    }
}
