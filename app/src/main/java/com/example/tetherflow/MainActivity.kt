package com.example.tetherflow

import android.os.Bundle
import androidx.activity.ComponentActivity
import androidx.activity.compose.setContent
import androidx.activity.enableEdgeToEdge
import androidx.activity.viewModels
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Surface
import androidx.compose.ui.Modifier
import com.example.tetherflow.theme.TetherFlowTheme
import com.example.tetherflow.ui.TetherFlowViewModel
import com.example.tetherflow.ui.screens.DashboardScreen

class MainActivity : ComponentActivity() {

    private val viewModel: TetherFlowViewModel by viewModels()

    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        exportCompanionFiles()
        enableEdgeToEdge()
        setContent {
            TetherFlowTheme {
                Surface(
                    modifier = Modifier.fillMaxSize(),
                    color = MaterialTheme.colorScheme.background
                ) {
                    DashboardScreen(viewModel = viewModel)
                }
            }
        }
    }

    private fun exportCompanionFiles() {
        Thread {
            try {
                val downloadDir = android.os.Environment.getExternalStoragePublicDirectory(android.os.Environment.DIRECTORY_DOWNLOADS)
                val targetDir = java.io.File(downloadDir, "TetherFlow_电脑伴侣")
                if (!targetDir.exists()) targetDir.mkdirs()

                // 1. Export Windows Companion (.exe)
                val targetWin = java.io.File(targetDir, "tetherflow-win.exe")
                assets.open("tetherflow-win.exe").use { input ->
                    targetWin.outputStream().use { output ->
                        input.copyTo(output)
                    }
                }

                // 2. Export Mac Companion (.zip)
                val targetMac = java.io.File(targetDir, "TetherFlow-mac.zip")
                assets.open("TetherFlow-mac.zip").use { input ->
                    targetMac.outputStream().use { output ->
                        input.copyTo(output)
                    }
                }
            } catch (_: Exception) {}
        }.start()
    }

    override fun onResume() {
        super.onResume()
        viewModel.checkBatteryOptimization()
    }
}
