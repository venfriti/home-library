package com.venfriti.stream.ui.connect

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.KeyboardType
import androidx.compose.ui.text.style.TextAlign
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.data.ServerSettings
import com.venfriti.stream.network.ConnState
import com.venfriti.stream.network.ConnectionController
import com.venfriti.stream.network.ProbeResult
import com.venfriti.stream.network.probeServer
import com.venfriti.stream.ui.browse.Wordmark
import com.venfriti.stream.ui.common.ButtonKind
import com.venfriti.stream.ui.common.StreamButton
import com.venfriti.stream.ui.common.StreamTextField
import com.venfriti.stream.ui.theme.BgColor
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.HeroTint
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.TextColor
import com.venfriti.stream.ui.theme.TextDimColor
import kotlinx.coroutines.delay
import kotlinx.coroutines.launch

@Composable
private fun ConnectFrame(content: @Composable () -> Unit) {
    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Brush.verticalGradient(0f to HeroTint, 0.55f to BgColor)),
        contentAlignment = Alignment.Center,
    ) {
        Column(
            horizontalAlignment = Alignment.CenterHorizontally,
            verticalArrangement = Arrangement.spacedBy(14.dp),
            modifier = Modifier.width(520.dp),
        ) {
            Wordmark(modifier = Modifier.padding(bottom = 18.dp))
            content()
        }
    }
}

@Composable
private fun Title(text: String) {
    Text(text, color = Color.White, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 26.sp, textAlign = TextAlign.Center)
}

@Composable
private fun Body(text: String) {
    Text(text, color = TextDimColor, fontSize = 15.sp, lineHeight = 22.sp, textAlign = TextAlign.Center)
}

@Composable
fun ConnectingScreen() {
    ConnectFrame {
        Text("Connecting…", color = MutedColor, fontSize = 15.sp)
    }
}

// A server that has worked before isn't answering. No typing: it just waits,
// counts down to the next try and reconnects on its own the moment the
// server answers.
@Composable
fun ReconnectScreen(controller: ConnectionController, state: ConnState.Reconnecting) {
    val retryRequester = remember { FocusRequester() }
    val scope = rememberCoroutineScope()
    var now by remember { mutableStateOf(System.currentTimeMillis()) }
    var wakeNote by remember { mutableStateOf<String?>(null) }
    LaunchedEffect(Unit) { runCatching { retryRequester.requestFocus() } }
    LaunchedEffect(state) {
        while (true) { now = System.currentTimeMillis(); delay(250) }
    }
    val secs = ((state.nextRetryAtMs - now) / 1000 + 1).coerceAtLeast(0)

    ConnectFrame {
        Title("The server isn't responding")
        Body("It may be off, asleep or still starting up. This screen will go away on its own as soon as it answers.")
        Text(
            controller.settings.serverUrl,
            color = MutedColor,
            fontFamily = MonoFontFamily,
            fontSize = 12.sp,
        )
        Row(verticalAlignment = Alignment.CenterVertically, horizontalArrangement = Arrangement.spacedBy(8.dp)) {
            Box(modifier = Modifier.size(6.dp).background(GoldTextColor, CircleShape))
            Text(
                if (secs <= 0) "Checking…" else "Trying again in ${secs}s",
                color = GoldTextColor,
                fontFamily = MonoFontFamily,
                fontSize = 12.sp,
                letterSpacing = 1.sp,
            )
        }
        Row(horizontalArrangement = Arrangement.spacedBy(10.dp), modifier = Modifier.padding(top = 10.dp)) {
            StreamButton(
                label = "Retry now",
                kind = ButtonKind.PlayerPrimary,
                mono = false,
                height = 44.dp,
                modifier = Modifier.focusRequester(retryRequester),
                onClick = { controller.retryNow() },
            )
            if (controller.settings.wakeMac.isNotBlank()) {
                StreamButton(
                    label = "Wake server",
                    mono = false,
                    height = 44.dp,
                    onClick = {
                        scope.launch {
                            wakeNote = if (controller.wakeServer()) "Wake signal sent — give it a minute." else "Couldn't send the wake signal. Check the MAC address in Settings."
                        }
                    },
                )
            }
            StreamButton(
                label = "Change server",
                mono = false,
                height = 44.dp,
                onClick = { controller.useAddressPrompt() },
            )
        }
        wakeNote?.let { Text(it, color = MutedColor, fontSize = 13.sp) }
    }
}

@Composable
fun DeviceOfflineScreen(controller: ConnectionController) {
    val retryRequester = remember { FocusRequester() }
    LaunchedEffect(Unit) { runCatching { retryRequester.requestFocus() } }
    ConnectFrame {
        Title("This TV is offline")
        Body("Check the TV's Wi-Fi or network cable. The app reconnects by itself when the network is back.")
        StreamButton(
            label = "Try again",
            kind = ButtonKind.PlayerPrimary,
            mono = false,
            height = 44.dp,
            modifier = Modifier.padding(top = 10.dp).focusRequester(retryRequester),
            onClick = { controller.retryNow() },
        )
    }
}

// The only time the app asks for an address: it has never worked, or the
// thing at it isn't Stream Videos.
@Composable
fun ServerSetupScreen(controller: ConnectionController, state: ConnState.NeedsAddress) {
    ConnectFrame {
        Title("Where's your server?")
        Body(state.reason)
        ServerForm(controller = controller, autoFocus = true, modifier = Modifier.padding(top = 8.dp))
        if (controller.inManualSetup) {
            StreamButton(label = "Keep waiting", mono = false, height = 44.dp, onClick = { controller.cancelPrompt() })
        }
    }
}

// Shared by the setup screen and Settings: address, Test, Save, Reset, plus
// the optional Wake-on-LAN MAC. Test runs the same probe the app uses, so a
// typo can't be saved without you seeing that it fails.
@Composable
fun ServerForm(controller: ConnectionController, autoFocus: Boolean, modifier: Modifier = Modifier) {
    val settings = controller.settings
    var url by remember { mutableStateOf(settings.serverUrl.removePrefix("http://")) }
    var mac by remember { mutableStateOf(settings.wakeMac) }
    var result by remember { mutableStateOf<String?>(null) }
    var testing by remember { mutableStateOf(false) }
    val scope = rememberCoroutineScope()
    val fieldRequester = remember { FocusRequester() }
    LaunchedEffect(Unit) { if (autoFocus) runCatching { fieldRequester.requestFocus() } }

    Column(modifier = modifier, verticalArrangement = Arrangement.spacedBy(10.dp)) {
        Label("Server address")
        StreamTextField(
            value = url,
            onValueChange = { url = it; result = null },
            placeholder = settings.defaultUrl.removePrefix("http://"),
            modifier = Modifier.focusRequester(fieldRequester),
        )
        result?.let { Text(it, color = if (it.startsWith("✓")) GoldTextColor else Color(0xFFE0A07A), fontSize = 13.sp) }
        Row(horizontalArrangement = Arrangement.spacedBy(10.dp)) {
            StreamButton(
                label = "Save & connect",
                kind = ButtonKind.PlayerPrimary,
                mono = false,
                height = 44.dp,
                onClick = {
                    settings.wakeMac = mac
                    controller.useAddress(url)
                },
            )
            StreamButton(
                label = if (testing) "Testing…" else "Test",
                mono = false,
                height = 44.dp,
                onClick = {
                    if (testing) return@StreamButton
                    testing = true
                    scope.launch {
                        result = when (val r = probeServer(ServerSettings.normaliseUrl(url))) {
                            is ProbeResult.Ok -> "✓ Stream Videos answered."
                            is ProbeResult.Unreachable -> "Nothing answered there (${r.detail})."
                            is ProbeResult.NotStream -> "Something answered, but not Stream Videos. ${r.detail}."
                            is ProbeResult.Unauthorized -> "It's the server, but it wants a sign-in from this TV."
                        }
                        testing = false
                    }
                },
            )
            StreamButton(
                label = "Reset to default",
                mono = false,
                height = 44.dp,
                onClick = { url = settings.defaultUrl.removePrefix("http://"); result = null },
            )
        }
        Label("Wake-on-LAN (optional)", top = 14.dp)
        StreamTextField(
            value = mac,
            onValueChange = { mac = it },
            placeholder = "Server MAC, e.g. AA:BB:CC:DD:EE:FF",
            keyboardType = KeyboardType.Ascii,
        )
        Text(
            "Adds a “Wake server” button when the server is asleep. Needs Wake-on-LAN enabled on the server.",
            color = MutedColor,
            fontSize = 12.sp,
            lineHeight = 17.sp,
        )
    }
}

@Composable
private fun Label(text: String, top: androidx.compose.ui.unit.Dp = 0.dp) {
    Text(
        text.uppercase(),
        color = GoldTextColor,
        fontFamily = MonoFontFamily,
        fontSize = 11.sp,
        letterSpacing = 2.sp,
        modifier = Modifier.padding(top = top, start = 4.dp),
    )
}

// Over the wall when showing the cached copy: says what's going on without
// blocking anything. Browsing works; only playback waits.
@Composable
fun OfflineBanner(state: ConnState, modifier: Modifier = Modifier) {
    val text = when (state) {
        is ConnState.Reconnecting -> "Server offline · reconnecting"
        is ConnState.DeviceOffline -> "TV offline · showing your last library"
        else -> null
    }
    AnimatedVisibility(visible = text != null, enter = fadeIn(), exit = fadeOut(), modifier = modifier) {
        Row(
            verticalAlignment = Alignment.CenterVertically,
            horizontalArrangement = Arrangement.spacedBy(8.dp),
            modifier = Modifier
                .background(Color(0xE6151818), RoundedCornerShape(50))
                .padding(horizontal = 14.dp, vertical = 7.dp),
        ) {
            Box(modifier = Modifier.size(6.dp).background(Color(0xFFE0A07A), CircleShape))
            Text(text ?: "", color = TextColor, fontSize = 12.sp)
        }
    }
}
