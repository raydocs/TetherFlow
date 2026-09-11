package com.example.tetherflow.ui

import android.app.Application
import android.content.Context
import android.content.Intent
import android.net.Uri
import android.os.Build
import android.os.PowerManager
import android.provider.Settings
import androidx.lifecycle.AndroidViewModel
import androidx.lifecycle.viewModelScope
import com.example.tetherflow.core.model.ConnectionStats
import com.example.tetherflow.core.model.SharingMode
import com.example.tetherflow.core.network.NetworkInterfaceManager
import com.example.tetherflow.core.wifi.WifiSharingManager
import com.example.tetherflow.core.wifi.WifiSharingState
import com.example.tetherflow.service.ProxyForegroundService
import kotlinx.coroutines.delay
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update
import kotlinx.coroutines.launch

data class UiDashboardState(
    val isProxyRunning: Boolean = false,
    val activeClients: Int = 0,
    val downloadSpeedBps: Long = 0L,
    val uploadSpeedBps: Long = 0L,
    val totalBytesDownloaded: Long = 0L,
    val totalBytesUploaded: Long = 0L,
    val primaryIp: String = "192.168.49.1",
    val primaryPort: Int = 8282,
    val activeMode: SharingMode = SharingMode.AUTO,
    val wifiState: WifiSharingState = WifiSharingState(),
    val isBatteryOptimized: Boolean = true
)

class TetherFlowViewModel(application: Application) : AndroidViewModel(application) {

    private val wifiManager = WifiSharingManager(application)
    private val _uiState = MutableStateFlow(UiDashboardState())
    val uiState: StateFlow<UiDashboardState> = _uiState.asStateFlow()

    init {
        observeWifiState()
        startStatsPolling()
        checkBatteryOptimization()
    }

    private fun observeWifiState() {
        viewModelScope.launch {
            wifiManager.state.collect { wifi ->
                _uiState.update { it.copy(wifiState = wifi) }
            }
        }
    }

    private fun startStatsPolling() {
        viewModelScope.launch {
            while (true) {
                val running = ProxyForegroundService.isRunning()
                val interfaces = NetworkInterfaceManager.getAvailableInterfaces()
                val primaryIntf = interfaces.firstOrNull()

                val primaryIp = primaryIntf?.ipAddress ?: "192.168.49.1"
                val activeMode = primaryIntf?.type ?: SharingMode.AUTO

                val stats = ProxyForegroundService.getStatsFlow()?.value

                _uiState.update { current ->
                    current.copy(
                        isProxyRunning = running,
                        activeClients = stats?.activeClients ?: 0,
                        downloadSpeedBps = stats?.downloadSpeedBps ?: 0L,
                        uploadSpeedBps = stats?.uploadSpeedBps ?: 0L,
                        totalBytesDownloaded = stats?.totalBytesDownloaded ?: 0L,
                        totalBytesUploaded = stats?.totalBytesUploaded ?: 0L,
                        primaryIp = primaryIp,
                        activeMode = activeMode
                    )
                }
                delay(800)
            }
        }
    }

    fun toggleProxy() {
        val context = getApplication<Application>()
        if (ProxyForegroundService.isRunning()) {
            ProxyForegroundService.stopService(context)
            wifiManager.stopSharing()
        } else {
            ProxyForegroundService.startService(context)
        }
    }

    fun startWifiDirect() {
        wifiManager.startWifiDirect()
    }

    fun startLocalHotspot() {
        wifiManager.startLocalHotspot()
    }

    fun stopWifiSharing() {
        wifiManager.stopSharing()
    }

    fun checkBatteryOptimization() {
        val context = getApplication<Application>()
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            val pm = context.getSystemService(Context.POWER_SERVICE) as? PowerManager
            val isIgnored = pm?.isIgnoringBatteryOptimizations(context.packageName) == true
            _uiState.update { it.copy(isBatteryOptimized = !isIgnored) }
        } else {
            _uiState.update { it.copy(isBatteryOptimized = false) }
        }
    }

    fun requestDisableBatteryOptimization(context: Context) {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.M) {
            try {
                val intent = Intent(Settings.ACTION_REQUEST_IGNORE_BATTERY_OPTIMIZATIONS).apply {
                    data = Uri.parse("package:${context.packageName}")
                    flags = Intent.FLAG_ACTIVITY_NEW_TASK
                }
                context.startActivity(intent)
            } catch (_: Exception) {
                try {
                    val intent = Intent(Settings.ACTION_IGNORE_BATTERY_OPTIMIZATION_SETTINGS).apply {
                        flags = Intent.FLAG_ACTIVITY_NEW_TASK
                    }
                    context.startActivity(intent)
                } catch (_: Exception) {}
            }
        }
    }
}
