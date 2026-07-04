package com.openrung.client

import android.Manifest
import android.content.Context
import android.content.Intent
import android.net.VpnService
import android.os.Build
import android.os.Bundle
import androidx.activity.compose.BackHandler
import androidx.activity.compose.rememberLauncherForActivityResult
import androidx.activity.compose.setContent
import androidx.activity.result.contract.ActivityResultContracts
import androidx.annotation.StringRes
import androidx.appcompat.app.AppCompatActivity
import androidx.appcompat.app.AppCompatDelegate
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material.icons.automirrored.outlined.KeyboardArrowRight
import androidx.compose.material.icons.outlined.Settings
import androidx.compose.material3.Button
import androidx.compose.material3.ButtonDefaults
import androidx.compose.material3.DropdownMenu
import androidx.compose.material3.DropdownMenuItem
import androidx.compose.material3.FloatingActionButton
import androidx.compose.material3.FloatingActionButtonDefaults
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.Text
import androidx.compose.material3.TextButton
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.semantics.contentDescription
import androidx.compose.ui.semantics.semantics
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.core.content.ContextCompat
import androidx.core.os.LocaleListCompat
import androidx.core.view.WindowCompat
import androidx.lifecycle.compose.collectAsStateWithLifecycle
import com.openrung.client.model.RecentNode
import com.openrung.client.net.SpeedTestClient
import com.openrung.client.net.SpeedTestResult
import com.openrung.client.state.ConnectionStatus
import com.openrung.client.state.DirectoryStatus
import com.openrung.client.state.OpenRungStatusStore
import com.openrung.client.state.OpenRungUiState
import com.openrung.client.telemetry.TelemetryManager
import com.openrung.client.ui.ExitNodeMap
import com.openrung.client.vpn.OpenRungVpnService
import kotlinx.coroutines.launch

class MainActivity : AppCompatActivity() {
    override fun onCreate(savedInstanceState: Bundle?) {
        super.onCreate(savedInstanceState)
        OpenRungStatusStore.initialize(applicationContext)
        // App draws edge-to-edge on a near-black background, so use light system-bar icons.
        WindowCompat.getInsetsController(window, window.decorView).run {
            isAppearanceLightStatusBars = false
            isAppearanceLightNavigationBars = false
        }

        setContent {
            OpenRungApp()
        }
    }
}

private enum class OpenRungScreenRoute {
    MAIN,
    SETTINGS,
    DEBUG,
    LICENSES,
    LICENSE_TEXT,
}

private data class LanguageOption(
    val tag: String,
    @StringRes val labelResId: Int,
)

private val languageOptions = listOf(
    LanguageOption("", R.string.language_system),
    LanguageOption("en", R.string.language_english),
    LanguageOption("zh-CN", R.string.language_simplified_chinese),
    LanguageOption("zh-TW", R.string.language_traditional_chinese),
    LanguageOption("fa", R.string.language_persian),
    LanguageOption("ru", R.string.language_russian),
    LanguageOption("ar", R.string.language_arabic),
    LanguageOption("tr", R.string.language_turkish),
    LanguageOption("vi", R.string.language_vietnamese),
    LanguageOption("my", R.string.language_burmese),
)

@Composable
private fun OpenRungApp() {
    val context = LocalContext.current
    val state by OpenRungStatusStore.uiState.collectAsStateWithLifecycle()
    var currentRoute by remember { mutableStateOf(OpenRungScreenRoute.MAIN) }
    // Broker URL is fixed to the configured default and not user-editable.
    val brokerUrl = state.brokerUrl
    // Country to connect to once the VPN-consent dialog returns (null = let the broker pick any).
    var pendingCountry by remember { mutableStateOf<String?>(null) }
    val vpnPermissionLauncher = rememberLauncherForActivityResult(ActivityResultContracts.StartActivityForResult()) {
        startVpn(context, brokerUrl, pendingCountry)
    }
    val notificationPermissionLauncher = rememberLauncherForActivityResult(ActivityResultContracts.RequestPermission()) {}

    // Starts (or switches) the VPN, requesting consent first if needed. [targetCountry] null = any.
    val beginConnect: (String?) -> Unit = { targetCountry ->
        pendingCountry = targetCountry
        if (Build.VERSION.SDK_INT >= Build.VERSION_CODES.TIRAMISU) {
            notificationPermissionLauncher.launch(Manifest.permission.POST_NOTIFICATIONS)
        }
        val prepareIntent = VpnService.prepare(context)
        if (prepareIntent != null) {
            vpnPermissionLauncher.launch(prepareIntent)
        } else {
            startVpn(context, brokerUrl, targetCountry)
        }
    }

    // Hardware back mirrors the in-app top-bar back arrow:
    // DEBUG/LICENSES -> SETTINGS, LICENSE_TEXT -> LICENSES, else -> MAIN.
    BackHandler(enabled = currentRoute != OpenRungScreenRoute.MAIN) {
        currentRoute = when (currentRoute) {
            OpenRungScreenRoute.DEBUG -> OpenRungScreenRoute.SETTINGS
            OpenRungScreenRoute.LICENSES -> OpenRungScreenRoute.SETTINGS
            OpenRungScreenRoute.LICENSE_TEXT -> OpenRungScreenRoute.LICENSES
            else -> OpenRungScreenRoute.MAIN
        }
    }

    MaterialTheme {
        when (currentRoute) {
            OpenRungScreenRoute.MAIN -> {
                Box(Modifier.fillMaxSize()) {
                    OpenRungMainScreen(
                        state = state,
                        onToggle = {
                            if (state.isConnected || state.isWorking) {
                                stopVpn(context)
                            } else {
                                beginConnect(null)
                            }
                        },
                        onConnectRegion = { countryCode -> beginConnect(countryCode) },
                    )
                    FloatingActionButton(
                        onClick = { currentRoute = OpenRungScreenRoute.SETTINGS },
                        modifier = Modifier
                            .align(Alignment.BottomEnd)
                            .navigationBarsPadding()
                            .padding(20.dp),
                        containerColor = Color(0xFF0D1C12),
                        contentColor = Color(0xFF65F58A),
                        elevation = FloatingActionButtonDefaults.elevation(defaultElevation = 2.dp),
                    ) {
                        Icon(
                            imageVector = Icons.Outlined.Settings,
                            contentDescription = stringResource(R.string.settings_content_description),
                        )
                    }
                }
            }

            OpenRungScreenRoute.SETTINGS -> {
                OpenRungSettingsScreen(
                    state = state,
                    onBack = { currentRoute = OpenRungScreenRoute.MAIN },
                    onOpenDebug = { currentRoute = OpenRungScreenRoute.DEBUG },
                    onOpenLicenses = { currentRoute = OpenRungScreenRoute.LICENSES },
                )
            }

            OpenRungScreenRoute.DEBUG -> {
                OpenRungDebugScreen(
                    state = state,
                    onBack = { currentRoute = OpenRungScreenRoute.SETTINGS },
                )
            }

            OpenRungScreenRoute.LICENSES -> {
                OpenRungLicensesScreen(
                    onBack = { currentRoute = OpenRungScreenRoute.SETTINGS },
                    onOpenFullText = { currentRoute = OpenRungScreenRoute.LICENSE_TEXT },
                )
            }

            OpenRungScreenRoute.LICENSE_TEXT -> {
                OpenRungLicenseTextScreen(
                    onBack = { currentRoute = OpenRungScreenRoute.LICENSES },
                )
            }
        }
    }
}

@Composable
private fun OpenRungMainScreen(
    state: OpenRungUiState,
    onToggle: () -> Unit,
    onConnectRegion: (countryCode: String) -> Unit,
) {
    val terminalGreen = Color(0xFF65F58A)
    val dimGreen = Color(0xFF294F35)
    val panelBlack = Color(0xFF07110B)
    val buttonColor = if (state.isConnected || state.isWorking) Color(0xFFB6F579) else terminalGreen

    val mapDescription = stringResource(R.string.map_content_description)

    // Populate the exit-node map directory when the main screen is shown (no-op once loaded).
    LaunchedEffect(Unit) { OpenRungStatusStore.refreshDirectory() }

    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(Color(0xFF030604))
            .statusBarsPadding()
            .padding(start = 20.dp, top = 20.dp, end = 20.dp, bottom = 84.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        Text(
            text = stringResource(R.string.main_title),
            color = terminalGreen,
            fontFamily = FontFamily.Monospace,
            fontWeight = FontWeight.Bold,
            fontSize = 22.sp,
        )
        Text(
            text = stringResource(R.string.status_format, stringResource(state.status.labelResId)),
            color = Color(0xFFD8FFE0),
            fontFamily = FontFamily.Monospace,
        )
        state.relayLabel?.let {
            Text(
                text = stringResource(R.string.relay_format, it),
                color = Color(0xFFA5F2B5),
                fontFamily = FontFamily.Monospace,
                fontSize = 13.sp,
            )
        }

        Button(
            onClick = onToggle,
            modifier = Modifier
                .fillMaxWidth()
                .height(58.dp),
            shape = RoundedCornerShape(8.dp),
            colors = ButtonDefaults.buttonColors(
                containerColor = buttonColor,
                contentColor = Color(0xFF061008),
            ),
        ) {
            Text(
                text = if (state.isConnected || state.isWorking) {
                    stringResource(R.string.action_disconnect)
                } else {
                    stringResource(R.string.action_connect)
                },
                fontFamily = FontFamily.Monospace,
                fontWeight = FontWeight.Black,
                letterSpacing = 1.sp,
            )
        }

        Box(
            modifier = Modifier
                .fillMaxWidth()
                .weight(1f)
                .clip(RoundedCornerShape(12.dp))
                .border(1.dp, dimGreen, RoundedCornerShape(12.dp)),
        ) {
            ExitNodeMap(
                regions = state.availableRegions,
                onRegionClick = onConnectRegion,
                modifier = Modifier
                    .fillMaxSize()
                    .semantics { contentDescription = mapDescription },
            )
            MapStatusChip(
                state = state,
                onRetry = { OpenRungStatusStore.refreshDirectory(force = true) },
                modifier = Modifier
                    .align(Alignment.TopStart)
                    .padding(10.dp),
            )
        }

        RecentsSection(
            recents = state.recentRegions,
            dimGreen = dimGreen,
            panelBlack = panelBlack,
        )

        Text(
            text = if (state.status == ConnectionStatus.CONNECTED) {
                stringResource(R.string.traffic_route_connected)
            } else {
                stringResource(R.string.traffic_route_disconnected)
            },
            color = Color(0xFF7DA989),
            fontFamily = FontFamily.Monospace,
            fontSize = 12.sp,
            modifier = Modifier.align(Alignment.CenterHorizontally),
        )
    }
}

@Composable
private fun MapStatusChip(
    state: OpenRungUiState,
    onRetry: () -> Unit,
    modifier: Modifier = Modifier,
) {
    val isFailed = state.directoryStatus == DirectoryStatus.FAILED
    val isEmptyLoaded = state.directoryStatus == DirectoryStatus.LOADED && state.availableRegions.isEmpty()
    val text = when {
        state.directoryStatus == DirectoryStatus.LOADING -> stringResource(R.string.map_loading)
        isFailed -> stringResource(R.string.map_failed)
        isEmptyLoaded -> stringResource(R.string.map_no_nodes)
        else -> stringResource(R.string.map_nodes_available, state.availableRegions.size)
    }
    val canRetry = isFailed || isEmptyLoaded
    Text(
        text = text,
        color = if (isFailed) Color(0xFFFFC0C0) else Color(0xFFD8FFE0),
        fontFamily = FontFamily.Monospace,
        fontSize = 12.sp,
        modifier = modifier
            .clip(RoundedCornerShape(6.dp))
            .background(Color(0xCC07110B))
            .then(if (canRetry) Modifier.clickable(onClick = onRetry) else Modifier)
            .padding(horizontal = 10.dp, vertical = 6.dp),
    )
}

@Composable
private fun RecentsSection(
    recents: List<RecentNode>,
    dimGreen: Color,
    panelBlack: Color,
) {
    Column(verticalArrangement = Arrangement.spacedBy(10.dp)) {
        Text(
            text = stringResource(R.string.recents_label),
            color = Color(0xFFD8FFE0),
            fontFamily = FontFamily.Monospace,
            fontWeight = FontWeight.Bold,
            fontSize = 14.sp,
        )
        if (recents.isEmpty()) {
            Text(
                text = stringResource(R.string.recents_empty),
                color = Color(0xFF7DA989),
                fontFamily = FontFamily.Monospace,
                fontSize = 12.sp,
            )
        } else {
            LazyRow(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
                items(recents, key = { it.countryCode }) { node ->
                    RecentNodeCard(node = node, dimGreen = dimGreen, panelBlack = panelBlack)
                }
            }
        }
    }
}

@Composable
private fun RecentNodeCard(
    node: RecentNode,
    dimGreen: Color,
    panelBlack: Color,
) {
    // Display-only: recents are recorded from past connections (the broker picks the relay), so the
    // card is not a tap-to-connect affordance and intentionally carries no "select" subtitle.
    Column(
        modifier = Modifier
            .width(140.dp)
            .clip(RoundedCornerShape(10.dp))
            .background(panelBlack)
            .border(1.dp, dimGreen, RoundedCornerShape(10.dp))
            .padding(12.dp),
        verticalArrangement = Arrangement.spacedBy(6.dp),
    ) {
        Text(text = countryFlag(node.countryCode), fontSize = 22.sp)
        Text(
            text = node.label,
            color = Color(0xFFD8FFE0),
            fontFamily = FontFamily.Monospace,
            fontSize = 13.sp,
            maxLines = 2,
        )
    }
}

@Composable
private fun OpenRungSettingsScreen(
    state: OpenRungUiState,
    onBack: () -> Unit,
    onOpenDebug: () -> Unit,
    onOpenLicenses: () -> Unit,
) {
    val terminalGreen = Color(0xFF65F58A)
    val dimGreen = Color(0xFF294F35)
    val panelBlack = Color(0xFF07110B)
    val coroutineScope = rememberCoroutineScope()
    var speedTestRunning by remember { mutableStateOf(false) }
    var speedTestResult by remember { mutableStateOf<SpeedTestResult?>(null) }
    var speedTestError by remember { mutableStateOf<String?>(null) }

    val speedTestSubtitle = when {
        speedTestRunning -> stringResource(R.string.speed_test_running)
        speedTestError != null -> stringResource(R.string.speed_test_error, speedTestError.orEmpty())
        speedTestResult != null -> stringResource(R.string.speed_test_result, speedTestResult!!.downloadMbps)
        !state.isConnected -> stringResource(R.string.speed_test_requires_connection)
        else -> stringResource(R.string.speed_test_ready)
    }

    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(Color(0xFF030604))
            .statusBarsPadding()
            .navigationBarsPadding()
            .verticalScroll(rememberScrollState())
            .padding(20.dp),
        verticalArrangement = Arrangement.spacedBy(18.dp),
    ) {
        Row(
            verticalAlignment = Alignment.CenterVertically,
        ) {
            IconButton(onClick = onBack) {
                Icon(
                    imageVector = Icons.AutoMirrored.Outlined.ArrowBack,
                    contentDescription = stringResource(R.string.back_content_description),
                    tint = terminalGreen,
                )
            }
            Spacer(Modifier.width(8.dp))
            Text(
                text = stringResource(R.string.settings_title),
                color = terminalGreen,
                fontFamily = FontFamily.Monospace,
                fontWeight = FontWeight.Bold,
                fontSize = 22.sp,
            )
        }

        SettingPanel(
            title = stringResource(R.string.language_setting_title),
            subtitle = stringResource(R.string.language_setting_subtitle),
            panelBlack = panelBlack,
            dimGreen = dimGreen,
            trailing = { LanguagePicker() },
        )

        SettingPanel(
            title = stringResource(R.string.speed_test_setting_title),
            subtitle = speedTestSubtitle,
            panelBlack = panelBlack,
            dimGreen = dimGreen,
            trailing = {
                Button(
                    enabled = state.isConnected && !speedTestRunning,
                    onClick = {
                        speedTestRunning = true
                        speedTestResult = null
                        speedTestError = null
                        coroutineScope.launch {
                            val session = TelemetryManager.activeSession()
                            runCatching {
                                checkNotNull(session) { "No active VPN session" }
                                SpeedTestClient(session.brokerUrl).run()
                            }
                                .onSuccess { result ->
                                    speedTestResult = result
                                    TelemetryManager.recordSpeedTest(result)
                                }
                                .onFailure { error ->
                                    speedTestError = error.message ?: error::class.java.simpleName
                                    TelemetryManager.record(
                                        event = "speed_test_failed",
                                        attributes = mapOf(
                                            "provider" to "openrung_broker",
                                            "error_type" to error::class.java.simpleName,
                                        ),
                                    )
                                }
                            session?.let {
                                runCatching { TelemetryManager.flush(it.brokerUrl) }
                            }
                            speedTestRunning = false
                        }
                    },
                    colors = ButtonDefaults.buttonColors(
                        containerColor = terminalGreen,
                        contentColor = Color(0xFF061008),
                    ),
                ) {
                    Text(
                        text = stringResource(R.string.speed_test_action),
                        fontFamily = FontFamily.Monospace,
                        fontWeight = FontWeight.Bold,
                    )
                }
            },
        )

        SettingPanel(
            title = stringResource(R.string.version_setting_title),
            subtitle = BuildConfig.VERSION_NAME,
            panelBlack = panelBlack,
            dimGreen = dimGreen,
        )

        SettingPanel(
            title = stringResource(R.string.debug_setting_title),
            subtitle = stringResource(R.string.debug_setting_subtitle),
            panelBlack = panelBlack,
            dimGreen = dimGreen,
            onClick = onOpenDebug,
        )

        SettingPanel(
            title = stringResource(R.string.licenses_setting_title),
            subtitle = stringResource(R.string.licenses_setting_subtitle),
            panelBlack = panelBlack,
            dimGreen = dimGreen,
            onClick = onOpenLicenses,
        )
    }
}

@Composable
private fun OpenRungDebugScreen(
    state: OpenRungUiState,
    onBack: () -> Unit,
) {
    val terminalGreen = Color(0xFF65F58A)
    val dimGreen = Color(0xFF294F35)
    val panelBlack = Color(0xFF07110B)

    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(Color(0xFF030604))
            .statusBarsPadding()
            .navigationBarsPadding()
            .padding(20.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        Row(
            verticalAlignment = Alignment.CenterVertically,
        ) {
            IconButton(onClick = onBack) {
                Icon(
                    imageVector = Icons.AutoMirrored.Outlined.ArrowBack,
                    contentDescription = stringResource(R.string.back_content_description),
                    tint = terminalGreen,
                )
            }
            Spacer(Modifier.width(8.dp))
            Text(
                text = stringResource(R.string.debug_title),
                color = terminalGreen,
                fontFamily = FontFamily.Monospace,
                fontWeight = FontWeight.Bold,
                fontSize = 22.sp,
            )
        }

        ConsolePanel(
            state = state,
            terminalGreen = terminalGreen,
            dimGreen = dimGreen,
            panelBlack = panelBlack,
            modifier = Modifier
                .fillMaxWidth()
                .weight(1f),
        )

        Text(
            text = if (state.status == ConnectionStatus.CONNECTED) {
                stringResource(R.string.traffic_route_connected)
            } else {
                stringResource(R.string.traffic_route_disconnected)
            },
            color = Color(0xFF7DA989),
            fontFamily = FontFamily.Monospace,
            fontSize = 12.sp,
            modifier = Modifier.align(Alignment.CenterHorizontally),
        )
    }
}

@Composable
private fun ConsolePanel(
    state: OpenRungUiState,
    terminalGreen: Color,
    dimGreen: Color,
    panelBlack: Color,
    modifier: Modifier = Modifier,
) {
    Box(
        modifier = modifier
            .background(panelBlack, RoundedCornerShape(8.dp))
            .border(1.dp, dimGreen, RoundedCornerShape(8.dp))
            .padding(14.dp),
    ) {
        Column(
            modifier = Modifier
                .fillMaxSize()
                .verticalScroll(rememberScrollState()),
            verticalArrangement = Arrangement.spacedBy(6.dp),
        ) {
            val lines = state.logLines.ifEmpty {
                listOf(stringResource(R.string.ready_log))
            }
            lines.forEach { line ->
                Text(
                    text = stringResource(R.string.log_line_format, line),
                    color = terminalGreen,
                    fontFamily = FontFamily.Monospace,
                    fontSize = 13.sp,
                    lineHeight = 18.sp,
                )
            }
            state.lastError?.let {
                Spacer(Modifier.size(8.dp))
                Text(
                    text = stringResource(R.string.error_line_format, it),
                    color = Color(0xFFFFA0A0),
                    fontFamily = FontFamily.Monospace,
                    fontSize = 13.sp,
                    lineHeight = 18.sp,
                )
            }
        }
    }
}

@Composable
private fun SettingPanel(
    title: String,
    subtitle: String,
    panelBlack: Color,
    dimGreen: Color,
    onClick: (() -> Unit)? = null,
    trailing: (@Composable () -> Unit)? = null,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(8.dp))
            .background(panelBlack)
            .border(1.dp, dimGreen, RoundedCornerShape(8.dp))
            .then(if (onClick != null) Modifier.clickable(onClick = onClick) else Modifier)
            .padding(14.dp),
        verticalAlignment = Alignment.CenterVertically,
        horizontalArrangement = Arrangement.SpaceBetween,
    ) {
        Column(
            modifier = Modifier.weight(1f),
            verticalArrangement = Arrangement.spacedBy(4.dp),
        ) {
            Text(
                text = title,
                color = Color(0xFFD8FFE0),
                fontFamily = FontFamily.Monospace,
                fontWeight = FontWeight.Bold,
            )
            Text(
                text = subtitle,
                color = Color(0xFF7DA989),
                fontFamily = FontFamily.Monospace,
                fontSize = 13.sp,
            )
        }
        when {
            trailing != null -> {
                Spacer(Modifier.width(12.dp))
                trailing()
            }
            onClick != null -> {
                Spacer(Modifier.width(12.dp))
                Icon(
                    imageVector = Icons.AutoMirrored.Outlined.KeyboardArrowRight,
                    contentDescription = stringResource(R.string.open_content_description),
                    tint = Color(0xFF65F58A),
                )
            }
        }
    }
}

@Composable
private fun LanguagePicker() {
    var expanded by remember { mutableStateOf(false) }
    var selectedTag by remember { mutableStateOf(currentApplicationLanguageTag()) }
    val selectedOption = languageOptions.firstOrNull { it.tag == selectedTag } ?: languageOptions.first()

    Box {
        TextButton(onClick = { expanded = true }) {
            Text(
                text = stringResource(selectedOption.labelResId),
                color = Color(0xFF65F58A),
                fontFamily = FontFamily.Monospace,
            )
        }
        DropdownMenu(
            expanded = expanded,
            onDismissRequest = { expanded = false },
        ) {
            languageOptions.forEach { option ->
                DropdownMenuItem(
                    text = { Text(stringResource(option.labelResId)) },
                    onClick = {
                        selectedTag = option.tag
                        expanded = false
                        AppCompatDelegate.setApplicationLocales(option.toLocaleList())
                    },
                )
            }
        }
    }
}

private fun currentApplicationLanguageTag(): String =
    AppCompatDelegate.getApplicationLocales().toLanguageTags()

private fun LanguageOption.toLocaleList(): LocaleListCompat =
    if (tag.isBlank()) {
        LocaleListCompat.getEmptyLocaleList()
    } else {
        LocaleListCompat.forLanguageTags(tag)
    }

/** Converts an ISO 3166-1 alpha-2 country code into its flag emoji, or a neutral flag if invalid. */
private fun countryFlag(code: String): String {
    val upper = code.trim().uppercase()
    if (upper.length != 2 || !upper.all { it in 'A'..'Z' }) return "🏳"
    val first = 0x1F1E6 + (upper[0] - 'A')
    val second = 0x1F1E6 + (upper[1] - 'A')
    return String(Character.toChars(first)) + String(Character.toChars(second))
}

private fun startVpn(context: Context, brokerUrl: String, targetCountry: String? = null) {
    val intent = OpenRungVpnService.connectIntent(context, brokerUrl, targetCountry)
    ContextCompat.startForegroundService(context, intent)
}

private fun stopVpn(context: Context) {
    context.startService(OpenRungVpnService.disconnectIntent(context))
}
