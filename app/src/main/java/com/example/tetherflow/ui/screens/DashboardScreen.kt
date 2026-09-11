package com.example.tetherflow.ui.screens

import android.content.ClipData
import android.content.ClipboardManager
import android.content.Context
import android.widget.Toast
import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.animateColorAsState
import androidx.compose.animation.core.*
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.*
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.*
import androidx.compose.runtime.*
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.scale
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.example.tetherflow.core.model.SharingMode
import com.example.tetherflow.ui.UiDashboardState
import com.example.tetherflow.ui.TetherFlowViewModel

@OptIn(ExperimentalMaterial3Api::class)
@Composable
fun DashboardScreen(
    viewModel: TetherFlowViewModel,
    modifier: Modifier = Modifier
) {
    val state by viewModel.uiState.collectAsState()
    val context = LocalContext.current
    val scrollState = rememberScrollState()

    Scaffold(
        topBar = {
            TopAppBar(
                title = {
                    Row(verticalAlignment = Alignment.CenterVertically) {
                        Text(
                            text = "TetherFlow",
                            fontWeight = FontWeight.Bold,
                            fontSize = 22.sp
                        )
                        Spacer(modifier = Modifier.width(8.dp))
                        Surface(
                            shape = RoundedCornerShape(12.dp),
                            color = if (state.isProxyRunning) Color(0xFF00E676).copy(alpha = 0.2f) else Color.Gray.copy(alpha = 0.2f)
                        ) {
                            Text(
                                text = if (state.isProxyRunning) "RUNNING" else "STANDBY",
                                color = if (state.isProxyRunning) Color(0xFF00E676) else Color.Gray,
                                fontSize = 11.sp,
                                fontWeight = FontWeight.Bold,
                                modifier = Modifier.padding(horizontal = 8.dp, vertical = 3.dp)
                            )
                        }
                    }
                },
                colors = TopAppBarDefaults.topAppBarColors(
                    containerColor = MaterialTheme.colorScheme.background
                )
            )
        }
    ) { innerPadding ->
        Column(
            modifier = modifier
                .fillMaxSize()
                .padding(innerPadding)
                .verticalScroll(scrollState)
                .padding(horizontal = 20.dp, vertical = 12.dp),
            horizontalAlignment = Alignment.CenterHorizontally
        ) {
            // 1. Hero Power Button & Status Card
            HeroSwitchCard(
                isRunning = state.isProxyRunning,
                onToggle = { viewModel.toggleProxy() }
            )

            Spacer(modifier = Modifier.height(20.dp))

            // 2. Real-time Speeds & Telemetry
            TelemetryDashboard(state = state)

            Spacer(modifier = Modifier.height(20.dp))

            // 3. Active Gateway & Copy Details Card
            GatewayAddressCard(state = state, onCopy = { text, label ->
                copyToClipboard(context, text, label)
            })

            Spacer(modifier = Modifier.height(20.dp))

            // 4. Wi-Fi Sharing Controller Card
            WifiSharingCard(
                state = state,
                onStartDirect = { viewModel.startWifiDirect() },
                onStartHotspot = { viewModel.startLocalHotspot() },
                onStop = { viewModel.stopWifiSharing() }
            )

            // 5. Samsung One UI Optimization Banner
            if (state.isBatteryOptimized) {
                Spacer(modifier = Modifier.height(16.dp))
                Card(
                    shape = RoundedCornerShape(16.dp),
                    colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.tertiaryContainer),
                    modifier = Modifier.fillMaxWidth().clickable {
                        viewModel.requestDisableBatteryOptimization(context)
                    }
                ) {
                    Row(
                        modifier = Modifier.padding(16.dp),
                        verticalAlignment = Alignment.CenterVertically
                    ) {
                        Text(text = "⚡", fontSize = 24.sp)
                        Spacer(modifier = Modifier.width(12.dp))
                        Column(modifier = Modifier.weight(1f)) {
                            Text(
                                text = "Keep Active When Locked",
                                fontWeight = FontWeight.SemiBold,
                                color = MaterialTheme.colorScheme.onTertiaryContainer
                            )
                            Text(
                                text = "Tap to exempt TetherFlow from One UI battery suspension",
                                fontSize = 12.sp,
                                color = MaterialTheme.colorScheme.onTertiaryContainer.copy(alpha = 0.8f)
                            )
                        }
                    }
                }
            }

            Spacer(modifier = Modifier.height(20.dp))

            // 6. Fast Connection Guides for Windows / Mac
            ClientSetupGuideCard(state = state, onCopy = { text, label ->
                copyToClipboard(context, text, label)
            })

            Spacer(modifier = Modifier.height(32.dp))
        }
    }
}

@Composable
fun HeroSwitchCard(
    isRunning: Boolean,
    onToggle: () -> Unit
) {
    val infiniteTransition = rememberInfiniteTransition(label = "pulse")
    val pulseScale by infiniteTransition.animateFloat(
        initialValue = 1f,
        targetValue = if (isRunning) 1.08f else 1f,
        animationSpec = infiniteRepeatable(
            animation = tween(1200, easing = FastOutSlowInEasing),
            repeatMode = RepeatMode.Reverse
        ),
        label = "scale"
    )

    val buttonBg by animateColorAsState(
        targetValue = if (isRunning) Color(0xFF00E676) else MaterialTheme.colorScheme.surfaceVariant,
        label = "btnColor"
    )
    val buttonText by animateColorAsState(
        targetValue = if (isRunning) Color.Black else MaterialTheme.colorScheme.onSurfaceVariant,
        label = "textColor"
    )

    Card(
        shape = RoundedCornerShape(24.dp),
        colors = CardDefaults.cardColors(
            containerColor = if (isRunning) MaterialTheme.colorScheme.primaryContainer.copy(alpha = 0.4f)
            else MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.5f)
        ),
        modifier = Modifier.fillMaxWidth()
    ) {
        Column(
            modifier = Modifier
                .fillMaxWidth()
                .padding(28.dp),
            horizontalAlignment = Alignment.CenterHorizontally
        ) {
            Box(
                contentAlignment = Alignment.Center,
                modifier = Modifier
                    .size(130.dp)
                    .scale(if (isRunning) pulseScale else 1f)
                    .clip(CircleShape)
                    .background(buttonBg)
                    .clickable { onToggle() }
            ) {
                Column(horizontalAlignment = Alignment.CenterHorizontally) {
                    Text(
                        text = if (isRunning) "STOP" else "START",
                        fontSize = 20.sp,
                        fontWeight = FontWeight.Black,
                        color = buttonText
                    )
                    Text(
                        text = "8282",
                        fontSize = 12.sp,
                        fontWeight = FontWeight.Medium,
                        color = buttonText.copy(alpha = 0.8f)
                    )
                }
            }

            Spacer(modifier = Modifier.height(16.dp))

            Text(
                text = if (isRunning) "Zero-Loss Proxy Active" else "Ready to Share",
                fontSize = 17.sp,
                fontWeight = FontWeight.Bold
            )
            Text(
                text = if (isRunning) "All traffic routed through native 5G cellular APN"
                else "Tap START to begin zero-metering tethering",
                fontSize = 12.sp,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                textAlign = TextAlign.Center
            )
        }
    }
}

@Composable
fun TelemetryDashboard(state: UiDashboardState) {
    val downMbps = (state.downloadSpeedBps * 8.0 / (1024 * 1024))
    val upMbps = (state.uploadSpeedBps * 8.0 / (1024 * 1024))

    Row(modifier = Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        MetricBox(
            title = "Download",
            value = if (downMbps >= 1.0) "%.1f".format(downMbps) else "%.0f".format(state.downloadSpeedBps / 1024.0),
            unit = if (downMbps >= 1.0) "Mbps" else "KB/s",
            accentColor = Color(0xFF00E676),
            modifier = Modifier.weight(1f)
        )
        MetricBox(
            title = "Upload",
            value = if (upMbps >= 1.0) "%.1f".format(upMbps) else "%.0f".format(state.uploadSpeedBps / 1024.0),
            unit = if (upMbps >= 1.0) "Mbps" else "KB/s",
            accentColor = Color(0xFF29B6F6),
            modifier = Modifier.weight(1f)
        )
    }

    Spacer(modifier = Modifier.height(12.dp))

    Row(modifier = Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(12.dp)) {
        MetricBox(
            title = "Active Devices",
            value = state.activeClients.toString(),
            unit = "Clients",
            accentColor = Color(0xFFFFB300),
            modifier = Modifier.weight(1f)
        )
        MetricBox(
            title = "Total Data",
            value = formatTotalData(state.totalBytesDownloaded + state.totalBytesUploaded),
            unit = "",
            accentColor = Color(0xFFAB47BC),
            modifier = Modifier.weight(1f)
        )
    }
}

@Composable
fun MetricBox(
    title: String,
    value: String,
    unit: String,
    accentColor: Color,
    modifier: Modifier = Modifier
) {
    Card(
        shape = RoundedCornerShape(18.dp),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surface),
        modifier = modifier
    ) {
        Column(modifier = Modifier.padding(16.dp)) {
            Text(text = title, fontSize = 12.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
            Spacer(modifier = Modifier.height(4.dp))
            Row(verticalAlignment = Alignment.Bottom) {
                Text(
                    text = value,
                    fontSize = 24.sp,
                    fontWeight = FontWeight.Bold,
                    color = accentColor
                )
                if (unit.isNotEmpty()) {
                    Spacer(modifier = Modifier.width(4.dp))
                    Text(
                        text = unit,
                        fontSize = 12.sp,
                        color = MaterialTheme.colorScheme.onSurfaceVariant,
                        modifier = Modifier.padding(bottom = 3.dp)
                    )
                }
            }
        }
    }
}

@Composable
fun GatewayAddressCard(
    state: UiDashboardState,
    onCopy: (String, String) -> Unit
) {
    Card(
        shape = RoundedCornerShape(20.dp),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surface),
        modifier = Modifier.fillMaxWidth()
    ) {
        Column(modifier = Modifier.padding(20.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    text = when (state.activeMode) {
                        SharingMode.USB_TETHERING -> "⚡ USB Tethering Mode"
                        SharingMode.WIFI_DIRECT -> "📶 Wi-Fi Direct Mode"
                        SharingMode.LOCAL_HOTSPOT -> "🔥 Local Hotspot Mode"
                        else -> "🌐 Proxy Gateway"
                    },
                    fontWeight = FontWeight.Bold,
                    fontSize = 16.sp
                )
                Spacer(modifier = Modifier.weight(1f))
                Text(
                    text = "Port ${state.primaryPort}",
                    fontSize = 12.sp,
                    color = MaterialTheme.colorScheme.primary,
                    fontWeight = FontWeight.SemiBold
                )
            }

            Spacer(modifier = Modifier.height(12.dp))

            Surface(
                shape = RoundedCornerShape(12.dp),
                color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.5f),
                modifier = Modifier.fillMaxWidth()
            ) {
                Row(
                    modifier = Modifier.padding(horizontal = 16.dp, vertical = 12.dp),
                    verticalAlignment = Alignment.CenterVertically
                ) {
                    Column(modifier = Modifier.weight(1f)) {
                        Text(text = "Proxy Host IP", fontSize = 11.sp, color = MaterialTheme.colorScheme.onSurfaceVariant)
                        Text(
                            text = state.primaryIp,
                            fontSize = 18.sp,
                            fontFamily = FontFamily.Monospace,
                            fontWeight = FontWeight.Bold
                        )
                    }
                    Button(
                        onClick = { onCopy(state.primaryIp, "Proxy Host IP") },
                        contentPadding = PaddingValues(horizontal = 12.dp, vertical = 6.dp)
                    ) {
                        Text("Copy", fontSize = 12.sp)
                    }
                }
            }

            Spacer(modifier = Modifier.height(8.dp))

            Row(
                modifier = Modifier.fillMaxWidth(),
                horizontalArrangement = Arrangement.SpaceBetween
            ) {
                Text(
                    text = "PAC URL: http://${state.primaryIp}:${state.primaryPort}/pac",
                    fontSize = 11.sp,
                    fontFamily = FontFamily.Monospace,
                    color = MaterialTheme.colorScheme.onSurfaceVariant,
                    modifier = Modifier.clickable {
                        onCopy("http://${state.primaryIp}:${state.primaryPort}/pac", "PAC Script URL")
                    }
                )
            }
        }
    }
}

@Composable
fun WifiSharingCard(
    state: UiDashboardState,
    onStartDirect: () -> Unit,
    onStartHotspot: () -> Unit,
    onStop: () -> Unit
) {
    val wifi = state.wifiState

    Card(
        shape = RoundedCornerShape(20.dp),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surface),
        modifier = Modifier.fillMaxWidth()
    ) {
        Column(modifier = Modifier.padding(20.dp)) {
            Text(
                text = "Wi-Fi Broadcasting (No-Root)",
                fontWeight = FontWeight.Bold,
                fontSize = 16.sp
            )
            Text(
                text = "Broadcasts a 5GHz Wi-Fi network for PC/Mac to join",
                fontSize = 12.sp,
                color = MaterialTheme.colorScheme.onSurfaceVariant
            )

            Spacer(modifier = Modifier.height(14.dp))

            if (!wifi.isSharing) {
                Row(modifier = Modifier.fillMaxWidth(), horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                    FilledTonalButton(
                        onClick = onStartDirect,
                        modifier = Modifier.weight(1f)
                    ) {
                        Text("Wi-Fi Direct (5G)", fontSize = 12.sp)
                    }
                    FilledTonalButton(
                        onClick = onStartHotspot,
                        modifier = Modifier.weight(1f)
                    ) {
                        Text("Local Hotspot", fontSize = 12.sp)
                    }
                }
            } else {
                Surface(
                    shape = RoundedCornerShape(12.dp),
                    color = MaterialTheme.colorScheme.secondaryContainer.copy(alpha = 0.5f),
                    modifier = Modifier.fillMaxWidth()
                ) {
                    Column(modifier = Modifier.padding(16.dp)) {
                        Text(text = "SSID: ${wifi.ssid ?: "Unknown"}", fontWeight = FontWeight.Bold)
                        if (wifi.password != null) {
                            Text(text = "Password: ${wifi.password}", fontFamily = FontFamily.Monospace)
                        }
                        Spacer(modifier = Modifier.height(8.dp))
                        OutlinedButton(
                            onClick = onStop,
                            modifier = Modifier.fillMaxWidth()
                        ) {
                            Text("Stop Wi-Fi Broadcast", color = MaterialTheme.colorScheme.error)
                        }
                    }
                }
            }
        }
    }
}

@Composable
fun ClientSetupGuideCard(
    state: UiDashboardState,
    onCopy: (String, String) -> Unit
) {
    var selectedTab by remember { mutableStateOf(0) }
    val tabs = listOf("Windows (全自动)", "macOS (全自动)", "手动模式")

    Card(
        shape = RoundedCornerShape(20.dp),
        colors = CardDefaults.cardColors(containerColor = MaterialTheme.colorScheme.surface),
        modifier = Modifier.fillMaxWidth()
    ) {
        Column(modifier = Modifier.padding(20.dp)) {
            Row(verticalAlignment = Alignment.CenterVertically) {
                Text(
                    text = "电脑端自动连上指南 (Win & Mac)",
                    fontWeight = FontWeight.Bold,
                    fontSize = 16.sp
                )
                Spacer(modifier = Modifier.weight(1f))
                Surface(
                    shape = RoundedCornerShape(8.dp),
                    color = Color(0xFF00E676).copy(alpha = 0.15f)
                ) {
                    Text(
                        text = "只需配置1次 终身即插即连",
                        fontSize = 10.sp,
                        color = Color(0xFF00E676),
                        fontWeight = FontWeight.Bold,
                        modifier = Modifier.padding(horizontal = 6.dp, vertical = 2.dp)
                    )
                }
            }

            Spacer(modifier = Modifier.height(12.dp))

            TabRow(selectedTabIndex = selectedTab) {
                tabs.forEachIndexed { index, title ->
                    Tab(
                        selected = selectedTab == index,
                        onClick = { selectedTab = index },
                        text = { Text(title, fontSize = 12.sp) }
                    )
                }
            }

            Spacer(modifier = Modifier.height(16.dp))

            when (selectedTab) {
                0 -> WindowsAutoGuide(state = state, onCopy = onCopy)
                1 -> MacAutoGuide(state = state, onCopy = onCopy)
                2 -> ManualGuide(state = state, onCopy = onCopy)
            }
        }
    }
}

@Composable
fun WindowsAutoGuide(state: UiDashboardState, onCopy: (String, String) -> Unit) {
    val psCmd = "Set-ItemProperty -Path 'HKCU:\\Software\\Microsoft\\Windows\\CurrentVersion\\Internet Settings' -Name AutoConfigURL -Value 'http://${state.primaryIp}:${state.primaryPort}/pac'"

    Column {
        Text("🎯 目标：像原生热点一样，插上线就自动连上，拔出自动恢复正常网络。", fontSize = 12.sp, color = MaterialTheme.colorScheme.primary)
        Spacer(modifier = Modifier.height(8.dp))
        Text("1. 手机插上 USB 或连上热点。", fontSize = 13.sp)
        Text("2. 在 Windows 电脑按 Win + X 打开【终端】或【PowerShell】。", fontSize = 13.sp)
        Text("3. 粘贴并回车运行下方的一键配置命令：", fontSize = 13.sp)
        Spacer(modifier = Modifier.height(6.dp))
        Surface(
            shape = RoundedCornerShape(8.dp),
            color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.6f),
            modifier = Modifier.fillMaxWidth().clickable { onCopy(psCmd, "Windows 一键命令") }
        ) {
            Text(
                text = psCmd,
                fontFamily = FontFamily.Monospace,
                fontSize = 11.sp,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(10.dp)
            )
        }
        Spacer(modifier = Modifier.height(8.dp))
        Button(
            onClick = { onCopy(psCmd, "Windows 一键命令") },
            modifier = Modifier.fillMaxWidth()
        ) {
            Text("复制 Windows 一键自连命令")
        }
    }
}

@Composable
fun MacAutoGuide(state: UiDashboardState, onCopy: (String, String) -> Unit) {
    val macCmd = "sudo networksetup -setautoproxyurl \"Wi-Fi\" \"http://${state.primaryIp}:${state.primaryPort}/pac\""

    Column {
        Text("🎯 Mac 终身免配置：只需在终端执行一次，以后连上自动分流。", fontSize = 12.sp, color = MaterialTheme.colorScheme.primary)
        Spacer(modifier = Modifier.height(8.dp))
        Text("1. 打开 Mac【终端】(Terminal)。", fontSize = 13.sp)
        Text("2. 粘贴并回车运行：", fontSize = 13.sp)
        Spacer(modifier = Modifier.height(6.dp))
        Surface(
            shape = RoundedCornerShape(8.dp),
            color = MaterialTheme.colorScheme.surfaceVariant.copy(alpha = 0.6f),
            modifier = Modifier.fillMaxWidth().clickable { onCopy(macCmd, "Mac 一键命令") }
        ) {
            Text(
                text = macCmd,
                fontFamily = FontFamily.Monospace,
                fontSize = 11.sp,
                color = MaterialTheme.colorScheme.onSurfaceVariant,
                modifier = Modifier.padding(10.dp)
            )
        }
        Spacer(modifier = Modifier.height(8.dp))
        Button(
            onClick = { onCopy(macCmd, "Mac 一键命令") },
            modifier = Modifier.fillMaxWidth()
        ) {
            Text("复制 Mac 一键自连命令")
        }
    }
}

@Composable
fun ManualGuide(state: UiDashboardState, onCopy: (String, String) -> Unit) {
    Column {
        Text("手动输入代理参数：", fontWeight = FontWeight.Bold, fontSize = 13.sp)
        Text("• 代理 IP：${state.primaryIp}", fontSize = 13.sp)
        Text("• 代理端口：${state.primaryPort}", fontSize = 13.sp)
        Text("• PAC 脚本地址：http://${state.primaryIp}:${state.primaryPort}/pac", fontSize = 13.sp)
        Spacer(modifier = Modifier.height(8.dp))
        Button(
            onClick = { onCopy("${state.primaryIp}:${state.primaryPort}", "代理地址") },
            modifier = Modifier.fillMaxWidth()
        ) {
            Text("复制 IP:端口 (${state.primaryIp}:${state.primaryPort})")
        }
    }
}

fun copyToClipboard(context: Context, text: String, label: String) {
    val clipboard = context.getSystemService(Context.CLIPBOARD_SERVICE) as ClipboardManager
    clipboard.setPrimaryClip(ClipData.newPlainText(label, text))
    Toast.makeText(context, "Copied $label: $text", Toast.LENGTH_SHORT).show()
}

fun formatTotalData(bytes: Long): String {
    val mb = bytes / (1024.0 * 1024.0)
    return if (mb >= 1024) "%.2f GB".format(mb / 1024.0) else "%.1f MB".format(mb)
}
