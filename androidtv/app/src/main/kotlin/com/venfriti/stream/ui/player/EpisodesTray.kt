package com.venfriti.stream.ui.player

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.animation.slideInVertically
import androidx.compose.animation.slideOutVertically
import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.aspectRatio
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.CircleShape
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.runtime.withFrameNanos
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.clip
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.ui.common.Artwork
import com.venfriti.stream.ui.common.FocusSurface
import com.venfriti.stream.ui.common.ProgressBar
import com.venfriti.stream.ui.common.StreamIcon
import com.venfriti.stream.ui.common.StreamIcons
import com.venfriti.stream.ui.theme.Bg2Color
import com.venfriti.stream.ui.theme.BorderColor
import com.venfriti.stream.ui.theme.GoldColor
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.InkColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.TextColor
import com.venfriti.stream.ui.theme.TextDimColor

// Height the transport lifts by while the tray is up (canvas option C2).
val EpisodesTrayHeight = 244.dp

private val TrayCardWidth = 190.dp
private val TraySheet = Color(0xF7151818)

private fun code(e: FileEntry) = "S${"%02d".format(e.season)}E${"%02d".format(e.episode)}"

// Episodes tray that rises from under the player controls (option C on the
// canvas), opened from the Episodes button. The video keeps playing above
// it; picking an episode switches in place, so you never leave the player.
// Up from the list (via the season tabs, when there are several) hands focus
// back to the Episodes button with the tray still open (`onUpOut`); closing
// it is Up again from the controls, or Back. `currentRequester` is the
// playing episode's card, so Down from the controls can land on it.
@Composable
fun EpisodesTray(
    visible: Boolean,
    episodes: List<FileEntry>,
    current: FileEntry,
    currentRequester: FocusRequester,
    onPick: (FileEntry) -> Unit,
    onClose: () -> Unit,
    onUpOut: () -> Unit,
    onFocusInside: (Boolean) -> Unit,
    modifier: Modifier = Modifier,
) {
    AnimatedVisibility(
        visible = visible,
        enter = slideInVertically { it },
        exit = slideOutVertically { it },
        modifier = modifier,
    ) {
        val seasons = remember(episodes) { episodes.map { it.season }.distinct().sorted() }
        var season by remember { mutableStateOf(current.season) }
        val shown = remember(episodes, season) { episodes.filter { it.season == season }.sortedBy { it.episode } }
        var focusedEp by remember { mutableStateOf<FileEntry?>(null) }
        val firstRequester = remember { FocusRequester() }
        val listState = rememberLazyListState(
            initialFirstVisibleItemIndex = shown.indexOfFirst { it.key == current.key }.coerceAtLeast(0).let { (it - 1).coerceAtLeast(0) },
        )

        // Opening lands on the episode that's playing, so left/right is
        // "previous/next episode" from where you are.
        LaunchedEffect(Unit) {
            withFrameNanos { }
            val ok = runCatching { currentRequester.requestFocus() }.isSuccess
            if (!ok) runCatching { firstRequester.requestFocus() }
        }

        Box(
            modifier = Modifier
                .fillMaxWidth()
                .height(EpisodesTrayHeight)
                .onFocusChanged { onFocusInside(it.hasFocus) }
                .background(TraySheet, RoundedCornerShape(topStart = 24.dp, topEnd = 24.dp)),
        ) {
            Column(modifier = Modifier.fillMaxSize().padding(top = 20.dp)) {
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(18.dp),
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(horizontal = 48.dp)
                        .onPreviewKeyEvent { e ->
                            // Up from the tabs: back to the Episodes button.
                            if (e.type == KeyEventType.KeyDown && e.key == Key.DirectionUp) { onUpOut(); true } else false
                        },
                ) {
                    Text("Episodes", color = Color.White, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 20.sp)
                    if (seasons.size > 1) {
                        seasons.forEach { s ->
                            TrayTab(if (s == 0) "Specials" else "Season $s", active = s == season) { season = s }
                        }
                    }
                    Box(modifier = Modifier.weight(1f).height(1.dp).background(BorderColor))
                }

                LazyRow(
                    state = listState,
                    contentPadding = PaddingValues(start = 48.dp, end = 48.dp, top = 14.dp, bottom = 8.dp),
                    horizontalArrangement = Arrangement.spacedBy(18.dp),
                    modifier = Modifier
                        .fillMaxWidth()
                        .onPreviewKeyEvent { e ->
                            if (e.type != KeyEventType.KeyDown) return@onPreviewKeyEvent false
                            when (e.key) {
                                // One season: no tabs above, so Up goes straight
                                // to the Episodes button. Several: Up reaches the
                                // tabs first (default focus movement).
                                Key.DirectionUp -> if (seasons.size <= 1) { onUpOut(); true } else false
                                // Nothing below the list.
                                Key.DirectionDown -> true
                                else -> false
                            }
                        },
                ) {
                    items(shown, key = { it.key }) { ep ->
                        val isCurrent = ep.key == current.key
                        val mod = when {
                            isCurrent -> Modifier.focusRequester(currentRequester)
                            ep == shown.firstOrNull() -> Modifier.focusRequester(firstRequester)
                            else -> Modifier
                        }
                        TrayCard(
                            ep = ep,
                            isCurrent = isCurrent,
                            modifier = mod,
                            onFocus = { if (it) focusedEp = ep },
                            onClick = { if (isCurrent) onClose() else onPick(ep) },
                        )
                    }
                }

                // One line about whichever episode has focus.
                val f = focusedEp
                if (f != null) {
                    Row(
                        horizontalArrangement = Arrangement.spacedBy(10.dp),
                        verticalAlignment = Alignment.CenterVertically,
                        modifier = Modifier.padding(start = 48.dp, end = 48.dp),
                    ) {
                        Text(
                            listOfNotNull(code(f), f.durationLabel.takeIf { it.isNotEmpty() }).joinToString(" · "),
                            color = GoldTextColor,
                            fontFamily = MonoFontFamily,
                            fontSize = 10.sp,
                            letterSpacing = 1.5.sp,
                        )
                        val overview = f.episodeOverview ?: f.overview
                        if (!overview.isNullOrEmpty()) {
                            Text(overview, color = Color(0xFFB8B2A8), fontSize = 12.sp, maxLines = 1, overflow = TextOverflow.Ellipsis)
                        }
                    }
                }
            }
        }
    }
}

@Composable
private fun TrayTab(label: String, active: Boolean, onClick: () -> Unit) {
    FocusSurface(
        onClick = onClick,
        shape = RoundedCornerShape(50),
        containerColor = Color.Transparent,
        focusedContainerColor = TextColor.copy(alpha = 0.14f),
        focusedScale = 1.04f,
        outline = false,
        glow = false,
    ) { focused ->
        Text(
            text = label,
            color = if (active || focused) TextColor else MutedColor,
            fontFamily = SerifFontFamily,
            fontWeight = if (active) FontWeight.Bold else FontWeight.Normal,
            fontSize = 15.sp,
            modifier = Modifier
                .padding(horizontal = 10.dp, vertical = 6.dp)
                .drawBehind {
                    if (active) {
                        val y = size.height + 3.dp.toPx()
                        drawLine(GoldColor, Offset(0f, y), Offset(size.width, y), strokeWidth = 2.dp.toPx())
                    }
                },
        )
    }
}

@Composable
private fun TrayCard(ep: FileEntry, isCurrent: Boolean, modifier: Modifier, onFocus: (Boolean) -> Unit, onClick: () -> Unit) {
    var focused by remember { mutableStateOf(false) }
    val shape = RoundedCornerShape(6.dp)
    Column(modifier = modifier.width(TrayCardWidth)) {
        FocusSurface(
            onClick = onClick,
            onFocus = { focused = it; onFocus(it) },
            modifier = Modifier
                .fillMaxWidth()
                .aspectRatio(16f / 9f),
            shape = shape,
            containerColor = Bg2Color,
            focusedScale = 1.08f,
            contentModifier = Modifier.fillMaxSize(),
        ) {
            Artwork(url = ep.episodeStill ?: ep.thumbnailImageUrl, description = ep.episodeRowTitle, modifier = Modifier.fillMaxSize())
            if (isCurrent) {
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(4.dp),
                    modifier = Modifier
                        .align(Alignment.TopStart)
                        .padding(7.dp)
                        .background(GoldTextColor, RoundedCornerShape(50))
                        .padding(horizontal = 7.dp, vertical = 2.dp),
                ) {
                    Box(modifier = Modifier.size(4.dp).clip(CircleShape).background(InkColor))
                    Text("NOW PLAYING", color = InkColor, fontFamily = MonoFontFamily, fontSize = 9.sp, letterSpacing = 1.sp)
                }
            } else {
                Text(
                    "E${"%02d".format(ep.episode)}",
                    color = Color.White.copy(alpha = 0.9f),
                    fontFamily = MonoFontFamily,
                    fontSize = 10.sp,
                    letterSpacing = 1.sp,
                    modifier = Modifier.align(Alignment.TopStart).padding(8.dp),
                )
            }
            ProgressBar(progress = ep.progress, modifier = Modifier.align(Alignment.BottomStart))
            // The playing episode keeps a gold outline even without focus.
            // Drawn inside the card so it zooms with the picture on focus (as
            // a modifier outside it, it stayed put while the card grew).
            if (isCurrent) {
                Box(modifier = Modifier.matchParentSize().border(2.dp, GoldTextColor, shape))
            }
        }
        // Title only: "now playing" is the badge on the still, and how far
        // in you are is the progress sliver along its bottom.
        Text(
            ep.episodeRowTitle,
            color = if (focused) Color.White else TextDimColor,
            fontSize = 13.sp,
            maxLines = 1,
            overflow = TextOverflow.Ellipsis,
            modifier = Modifier.padding(top = 10.dp),
        )
    }
}
