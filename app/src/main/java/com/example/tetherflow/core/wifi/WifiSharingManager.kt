package com.example.tetherflow.core.wifi

import android.annotation.SuppressLint
import android.content.Context
import android.net.wifi.WifiManager
import android.net.wifi.p2p.WifiP2pConfig
import android.net.wifi.p2p.WifiP2pGroup
import android.net.wifi.p2p.WifiP2pManager
import android.os.Build
import kotlinx.coroutines.flow.MutableStateFlow
import kotlinx.coroutines.flow.StateFlow
import kotlinx.coroutines.flow.asStateFlow
import kotlinx.coroutines.flow.update

data class WifiSharingState(
    val isSharing: Boolean = false,
    val ssid: String? = null,
    val password: String? = null,
    val errorMessage: String? = null
)

class WifiSharingManager(private val context: Context) {

    private val p2pManager = context.getSystemService(Context.WIFI_P2P_SERVICE) as? WifiP2pManager
    private var p2pChannel: WifiP2pManager.Channel? = null

    private val wifiManager = context.applicationContext.getSystemService(Context.WIFI_SERVICE) as? WifiManager
    private var localHotspotReservation: WifiManager.LocalOnlyHotspotReservation? = null

    private val _state = MutableStateFlow(WifiSharingState())
    val state: StateFlow<WifiSharingState> = _state.asStateFlow()

    init {
        p2pChannel = p2pManager?.initialize(context, context.mainLooper, null)
    }

    @SuppressLint("MissingPermission")
    fun startWifiDirect(desiredSsid: String = "DIRECT-TetherFlow", desiredPass: String = "12345678") {
        val manager = p2pManager
        val channel = p2pChannel
        if (manager == null || channel == null) {
            _state.update { it.copy(errorMessage = "Wi-Fi Direct not supported on this device") }
            return
        }

        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.Q) {
            val config = WifiP2pConfig.Builder()
                .setNetworkName(desiredSsid)
                .setPassphrase(desiredPass)
                .build()

            manager.createGroup(channel, config, object : WifiP2pManager.ActionListener {
                override fun onSuccess() {
                    queryP2pGroup()
                }

                override fun onFailure(reason: Int) {
                    createDefaultGroup()
                }
            })
        } else {
            createDefaultGroup()
        }
    }

    @SuppressLint("MissingPermission")
    private fun createDefaultGroup() {
        val manager = p2pManager ?: return
        val channel = p2pChannel ?: return

        manager.createGroup(channel, object : WifiP2pManager.ActionListener {
            override fun onSuccess() {
                queryP2pGroup()
            }

            override fun onFailure(reason: Int) {
                _state.update { it.copy(errorMessage = "Failed to create Wi-Fi Direct group (code: $reason)") }
            }
        })
    }

    @SuppressLint("MissingPermission")
    private fun queryP2pGroup() {
        val manager = p2pManager ?: return
        val channel = p2pChannel ?: return

        manager.requestGroupInfo(channel) { group: WifiP2pGroup? ->
            if (group != null) {
                _state.update {
                    it.copy(
                        isSharing = true,
                        ssid = group.networkName,
                        password = group.passphrase,
                        errorMessage = null
                    )
                }
            }
        }
    }

    @SuppressLint("MissingPermission")
    fun startLocalHotspot() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O && wifiManager != null) {
            try {
                wifiManager.startLocalOnlyHotspot(object : WifiManager.LocalOnlyHotspotCallback() {
                    override fun onStarted(reservation: WifiManager.LocalOnlyHotspotReservation) {
                        super.onStarted(reservation)
                        localHotspotReservation = reservation
                        val config = reservation.wifiConfiguration
                        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.R) {
                            val softApConfig = reservation.softApConfiguration
                            _state.update {
                                it.copy(
                                    isSharing = true,
                                    ssid = softApConfig.ssid,
                                    password = softApConfig.passphrase,
                                    errorMessage = null
                                )
                            }
                        } else {
                            _state.update {
                                it.copy(
                                    isSharing = true,
                                    ssid = config?.SSID,
                                    password = config?.preSharedKey,
                                    errorMessage = null
                                )
                            }
                        }
                    }

                    override fun onStopped() {
                        super.onStopped()
                        _state.update { it.copy(isSharing = false, ssid = null, password = null) }
                    }

                    override fun onFailed(reason: Int) {
                        super.onFailed(reason)
                        _state.update { it.copy(errorMessage = "LocalHotspot failed with code $reason") }
                    }
                }, null)
            } catch (e: Exception) {
                _state.update { it.copy(errorMessage = e.message) }
            }
        } else {
            startWifiDirect()
        }
    }

    @SuppressLint("MissingPermission")
    fun stopSharing() {
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.O) {
            localHotspotReservation?.close()
            localHotspotReservation = null
        }

        val manager = p2pManager
        val channel = p2pChannel
        if (manager != null && channel != null) {
            manager.removeGroup(channel, object : WifiP2pManager.ActionListener {
                override fun onSuccess() {}
                override fun onFailure(reason: Int) {}
            })
        }

        _state.update { it.copy(isSharing = false, ssid = null, password = null) }
    }
}
