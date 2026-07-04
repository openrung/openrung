package com.openrung.client

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.clickable
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.navigationBarsPadding
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.statusBarsPadding
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.material.icons.Icons
import androidx.compose.material.icons.automirrored.outlined.ArrowBack
import androidx.compose.material.icons.automirrored.outlined.KeyboardArrowRight
import androidx.compose.material3.Icon
import androidx.compose.material3.IconButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.platform.LocalUriHandler
import androidx.compose.ui.res.stringResource
import androidx.compose.ui.text.font.FontFamily
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import com.openrung.client.config.AppConfig

private val TerminalGreen = Color(0xFF65F58A)
private val DimGreen = Color(0xFF294F35)
private val PanelBlack = Color(0xFF07110B)
private val ScreenBlack = Color(0xFF030604)
private val BodyText = Color(0xFFD8FFE0)
private val SubtitleText = Color(0xFF7DA989)

/** One bundled/linked third-party component shown on the licenses screen. */
private data class LicenseEntry(val name: String, val license: String)

// Mirrors the distributed components in THIRD_PARTY_NOTICES.md (mobile surface).
private val licenseEntries = listOf(
    LicenseEntry("sing-box (libbox)", "GPL-3.0-or-later"),
    LicenseEntry("gVisor", "Apache-2.0"),
    LicenseEntry("quic-go", "MIT"),
    LicenseEntry("wireguard-go", "MIT"),
    LicenseEntry("utls", "BSD-3-Clause"),
    LicenseEntry("MapLibre Native Android SDK", "BSD-2-Clause"),
    LicenseEntry("Jetpack Compose / AndroidX", "Apache-2.0"),
    LicenseEntry("Kotlin standard library", "Apache-2.0"),
    LicenseEntry("kotlinx-coroutines", "Apache-2.0"),
    LicenseEntry("kotlinx-serialization", "Apache-2.0"),
    LicenseEntry("golang.org/x/* + Go stdlib", "BSD-3-Clause"),
)

/** The open-source licenses list: GPL notice, source link, per-component licenses. */
@Composable
fun OpenRungLicensesScreen(
    onBack: () -> Unit,
    onOpenFullText: () -> Unit,
) {
    val uriHandler = LocalUriHandler.current

    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(ScreenBlack)
            .statusBarsPadding()
            .navigationBarsPadding()
            .verticalScroll(rememberScrollState())
            .padding(20.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        ScreenHeader(title = stringResource(R.string.licenses_title), onBack = onBack)

        Text(
            text = stringResource(R.string.licenses_intro),
            color = BodyText,
            fontFamily = FontFamily.Monospace,
            fontSize = 13.sp,
            lineHeight = 19.sp,
        )

        LicensePanel(
            title = stringResource(R.string.licenses_source_title),
            subtitle = AppConfig.SOURCE_URL,
            onClick = { uriHandler.openUri(AppConfig.SOURCE_URL) },
            showChevron = true,
        )

        LicensePanel(
            title = stringResource(R.string.licenses_full_text_title),
            subtitle = stringResource(R.string.licenses_full_text_subtitle),
            onClick = onOpenFullText,
            showChevron = true,
        )

        Text(
            text = stringResource(R.string.licenses_components_header),
            color = TerminalGreen,
            fontFamily = FontFamily.Monospace,
            fontWeight = FontWeight.Bold,
            fontSize = 14.sp,
            modifier = Modifier.padding(top = 4.dp),
        )

        licenseEntries.forEach { entry ->
            LicensePanel(title = entry.name, subtitle = entry.license)
        }
    }
}

/** Full bundled notices (component summary + complete GNU GPL-3.0 text), read from res/raw. */
@Composable
fun OpenRungLicenseTextScreen(onBack: () -> Unit) {
    val context = LocalContext.current
    val notices = remember {
        context.resources.openRawResource(R.raw.third_party_notices)
            .bufferedReader()
            .use { it.readText() }
    }

    Column(
        modifier = Modifier
            .fillMaxSize()
            .background(ScreenBlack)
            .statusBarsPadding()
            .navigationBarsPadding()
            .padding(20.dp),
        verticalArrangement = Arrangement.spacedBy(16.dp),
    ) {
        ScreenHeader(title = stringResource(R.string.licenses_full_text_title), onBack = onBack)

        Column(
            modifier = Modifier
                .fillMaxSize()
                .verticalScroll(rememberScrollState()),
        ) {
            Text(
                text = notices,
                color = BodyText,
                fontFamily = FontFamily.Monospace,
                fontSize = 11.sp,
                lineHeight = 16.sp,
            )
        }
    }
}

@Composable
private fun ScreenHeader(title: String, onBack: () -> Unit) {
    Row(verticalAlignment = Alignment.CenterVertically) {
        IconButton(onClick = onBack) {
            Icon(
                imageVector = Icons.AutoMirrored.Outlined.ArrowBack,
                contentDescription = stringResource(R.string.back_content_description),
                tint = TerminalGreen,
            )
        }
        Spacer(Modifier.width(8.dp))
        Text(
            text = title,
            color = TerminalGreen,
            fontFamily = FontFamily.Monospace,
            fontWeight = FontWeight.Bold,
            fontSize = 22.sp,
        )
    }
}

@Composable
private fun LicensePanel(
    title: String,
    subtitle: String,
    onClick: (() -> Unit)? = null,
    showChevron: Boolean = false,
) {
    Row(
        modifier = Modifier
            .fillMaxWidth()
            .clip(RoundedCornerShape(8.dp))
            .background(PanelBlack)
            .border(1.dp, DimGreen, RoundedCornerShape(8.dp))
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
                color = BodyText,
                fontFamily = FontFamily.Monospace,
                fontWeight = FontWeight.Bold,
                fontSize = 14.sp,
            )
            Text(
                text = subtitle,
                color = SubtitleText,
                fontFamily = FontFamily.Monospace,
                fontSize = 13.sp,
            )
        }
        if (showChevron) {
            Spacer(Modifier.width(12.dp))
            Icon(
                imageVector = Icons.AutoMirrored.Outlined.KeyboardArrowRight,
                contentDescription = stringResource(R.string.open_content_description),
                tint = TerminalGreen,
            )
        }
    }
}
