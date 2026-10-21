package com.venfriti.stream.ui.settings

import androidx.compose.foundation.background
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import com.venfriti.stream.ui.common.StreamButton
import com.venfriti.stream.ui.common.StreamIcons
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.verticalScroll
import androidx.compose.runtime.Composable
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.BuildConfig
import com.venfriti.stream.network.ConnState
import com.venfriti.stream.network.ConnectionController
import com.venfriti.stream.ui.connect.ServerForm
import com.venfriti.stream.ui.theme.BgColor
import com.venfriti.stream.ui.theme.Dimens
import com.venfriti.stream.ui.theme.HeroTint
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.TextColor
import java.text.DateFormat
import java.util.Date

@Composable
fun SettingsScreen(controller: ConnectionController) {
    val settings = controller.settings
    val (dot, status) = when (controller.state) {
        is ConnState.Online -> Color(0xFF7FB77E) to "Connected to ${settings.serverUrl}"
        is ConnState.Reconnecting -> Color(0xFFE0A07A) to "Server not responding — retrying"
        is ConnState.DeviceOffline -> Color(0xFFE0A07A) to "This TV is offline"
        is ConnState.Checking -> MutedColor to "Checking…"
        is ConnState.NeedsAddress -> Color(0xFFE0A07A) to "Couldn't use ${settings.serverUrl}"
    }
    val last = settings.lastConnectedAt.takeIf { it > 0 }?.let {
        DateFormat.getDateTimeInstance(DateFormat.MEDIUM, DateFormat.SHORT).format(Date(it))
    }

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Brush.verticalGradient(0f to HeroTint, 0.45f to BgColor)),
    ) {
        Column(
            modifier = Modifier
                .padding(start = Dimens.ContentStart, top = 56.dp, end = Dimens.Gutter, bottom = 40.dp)
                .width(560.dp)
                .verticalScroll(rememberScrollState()),
            verticalArrangement = Arrangement.spacedBy(14.dp),
        ) {
            Text("Settings", color = TextColor, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 32.sp)
            Row(
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(10.dp),
                modifier = Modifier
                    .background(Color(0x14ECE8E1), RoundedCornerShape(16.dp))
                    .padding(horizontal = 18.dp, vertical = 14.dp),
            ) {
                Box(modifier = Modifier.size(8.dp).background(dot, CircleShape))
                Column(verticalArrangement = Arrangement.spacedBy(3.dp)) {
                    Text(status, color = TextColor, fontSize = 15.sp)
                    Text(
                        listOfNotNull(last?.let { "Last connected $it" }, "App ${BuildConfig.VERSION_NAME}").joinToString("  ·  "),
                        color = MutedColor,
                        fontFamily = MonoFontFamily,
                        fontSize = 11.sp,
                    )
                }
            }
            // The library also reloads on its own (back to the app, every few
            // minutes, when the server returns); this is for not waiting.
            val refreshedAt = controller.lastRefreshedAt.takeIf { it > 0 }?.let {
                DateFormat.getTimeInstance(DateFormat.SHORT).format(Date(it))
            }
            Row(
                verticalAlignment = Alignment.CenterVertically,
                horizontalArrangement = Arrangement.spacedBy(14.dp),
                modifier = Modifier
                    .fillMaxWidth()
                    .background(Color(0x14ECE8E1), RoundedCornerShape(16.dp))
                    .padding(horizontal = 18.dp, vertical = 12.dp),
            ) {
                Column(modifier = Modifier.weight(1f), verticalArrangement = Arrangement.spacedBy(3.dp)) {
                    Text("Library", color = TextColor, fontSize = 15.sp)
                    Text(
                        listOfNotNull(
                            controller.library?.files?.size?.let { "$it titles" },
                            refreshedAt?.let { "updated $it" },
                        ).joinToString("  ·  "),
                        color = MutedColor,
                        fontFamily = MonoFontFamily,
                        fontSize = 11.sp,
                    )
                }
                StreamButton(
                    label = if (controller.refreshing) "Refreshing…" else "Refresh library",
                    icon = StreamIcons.Restart,
                    mono = false,
                    height = 38.dp,
                    onClick = { if (!controller.refreshing) controller.requestLibraryRefresh() },
                )
            }
            ServerForm(controller = controller, autoFocus = true, modifier = Modifier.padding(top = 8.dp))
        }
    }
}
