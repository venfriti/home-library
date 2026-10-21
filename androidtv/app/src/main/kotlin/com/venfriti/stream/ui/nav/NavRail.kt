package com.venfriti.stream.ui.nav

import androidx.compose.animation.AnimatedVisibility
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.ui.focus.FocusDirection
import androidx.compose.ui.platform.LocalFocusManager
import com.venfriti.stream.ui.common.LocalScreenFocusFallback
import com.venfriti.stream.ui.common.LocalTileTracker
import androidx.compose.runtime.withFrameNanos
import androidx.compose.ui.input.key.Key
import androidx.compose.ui.input.key.KeyEventType
import androidx.compose.ui.input.key.key
import androidx.compose.ui.input.key.onPreviewKeyEvent
import androidx.compose.ui.input.key.type
import androidx.compose.animation.core.animateDpAsState
import androidx.compose.animation.fadeIn
import androidx.compose.animation.fadeOut
import androidx.compose.foundation.background
import androidx.compose.foundation.focusGroup
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.layout.size
import androidx.compose.foundation.layout.width
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.ExperimentalComposeUiApi
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.focusRestorer
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.graphics.vector.ImageVector
import androidx.compose.ui.text.font.FontStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.ui.common.FocusSurface
import com.venfriti.stream.ui.theme.GoldTextColor
import androidx.compose.ui.text.style.TextOverflow
import com.venfriti.stream.ui.common.StreamIcon
import com.venfriti.stream.ui.common.StreamIcons
import com.venfriti.stream.ui.theme.BorderColor
import com.venfriti.stream.ui.theme.Dimens
import com.venfriti.stream.ui.theme.GoldColor
import com.venfriti.stream.ui.theme.InkColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.SheetColor
import com.venfriti.stream.ui.theme.TextColor
import com.venfriti.stream.ui.theme.TonalColor

enum class Section(val label: String, val icon: ImageVector) {
    Search("Search", StreamIcons.Search),
    Home("Home", StreamIcons.Home),
    Continue("Continue watching", StreamIcons.History),
    Films("Films", StreamIcons.Films),
    Series("TV", StreamIcons.Series),
    Playlists("Playlists", StreamIcons.Playlists),
    Music("Music videos", StreamIcons.Music),
    Library("Library", StreamIcons.Library),
    Settings("Settings", StreamIcons.Settings),
}

// The web's right-hand section index (#toc), moved to the left edge where a
// remote's "back/left" instinct expects navigation on TV. At rest it's a
// slim column of icons; the moment focus enters it, it opens into a drawer
// with labels and counts over a dimmed page, and closes again when focus
// moves back right. focusRestorer makes "left" from anywhere land on the
// section you're in rather than whichever icon happens to be nearest.
@OptIn(ExperimentalComposeUiApi::class)
@Composable
fun NavShell(
    current: Section,
    sections: List<Section>,
    counts: Map<Section, Int>,
    onSelect: (Section) -> Unit,
    // A music video playing in the corner: shown as the rail's first item,
    // since the corner card itself is deliberately not focusable.
    nowPlaying: FileEntry? = null,
    onNowPlaying: () -> Unit = {},
    content: @Composable () -> Unit,
) {
    var expanded by remember { mutableStateOf(false) }
    val width by animateDpAsState(if (expanded) Dimens.DrawerWidth else Dimens.RailWidth, label = "drawerWidth")
    val currentRequester = remember { FocusRequester() }
    // The page remembers its last-focused item, so leaving the drawer goes
    // straight back there (a "move right" from the open drawer could miss
    // the cards it covers). The drawer collapses once the rail loses focus.
    val contentFocus = remember { FocusRequester() }
    var contentHasFocus by remember { mutableStateOf(false) }
    val screenFallback = LocalScreenFocusFallback.current
    val tileTracker = LocalTileTracker.current
    val focusManager = LocalFocusManager.current
    var enterContentTick by remember { mutableStateOf(0) }
    LaunchedEffect(enterContentTick) {
        if (enterContentTick > 0) {
            // Two frames: a newly selected section has to compose first.
            withFrameNanos { }
            withFrameNanos { }
            // Restoring the last tile can silently land nowhere (it may have
            // scrolled out and been discarded), so check each step: then the
            // screen's own first stop, then a plain move right. Focus must
            // never be left on nothing.
            // The exact tile you were on, when it's still there.
            tileTracker?.lastFocused?.let { runCatching { it.requestFocus() } }
            withFrameNanos { }
            if (!contentHasFocus) {
                runCatching { contentFocus.requestFocus() }
                withFrameNanos { }
            }
            if (!contentHasFocus) {
                screenFallback?.requester?.let { runCatching { it.requestFocus() } }
                withFrameNanos { }
            }
            if (!contentHasFocus) focusManager.moveFocus(FocusDirection.Right)
        }
    }
    fun leaveDrawer() { enterContentTick++ }
    fun openSection(section: Section) {
        if (section != current) onSelect(section)
        leaveDrawer()
    }

    Box(modifier = Modifier.fillMaxSize()) {
        Box(
            modifier = Modifier
                .fillMaxSize()
                .onFocusChanged { contentHasFocus = it.hasFocus }
                .focusRequester(contentFocus)
                // No focusRestorer: see MainActivity's page group. Leaving
                // the drawer goes via TileTracker instead.
                .focusGroup(),
        ) {
            content()
        }

        AnimatedVisibility(visible = expanded, enter = fadeIn(), exit = fadeOut()) {
            Box(modifier = Modifier.fillMaxSize().background(Color(0xFF050606).copy(alpha = 0.62f)))
        }

        Column(
            modifier = Modifier
                .width(width)
                .fillMaxHeight()
                .background(
                    if (expanded) Brush.horizontalGradient(listOf(SheetColor, SheetColor))
                    else Brush.horizontalGradient(listOf(Color.Black.copy(alpha = 0.55f), Color.Transparent)),
                )
                .onFocusChanged { expanded = it.hasFocus }
                // Back closes the drawer. Handled on the key itself rather
                // than a BackHandler: on TV, Compose uses Back to move focus
                // before the activity's back dispatch ever sees it.
                .onPreviewKeyEvent { e ->
                    if (e.key != Key.Back) return@onPreviewKeyEvent false
                    if (e.type == KeyEventType.KeyUp) leaveDrawer()
                    true
                }
                .focusRestorer { currentRequester }
                .focusGroup()
                .padding(horizontal = if (expanded) 20.dp else 12.dp, vertical = 30.dp),
            verticalArrangement = Arrangement.spacedBy(5.dp),
        ) {
            Box(modifier = Modifier.height(48.dp).padding(start = 14.dp)) {
                if (expanded) {
                    Row(verticalAlignment = Alignment.Bottom) {
                        Text("Stream", color = TextColor, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 19.sp)
                        Text("Videos", color = GoldColor, fontFamily = SerifFontFamily, fontStyle = FontStyle.Italic, fontSize = 19.sp)
                    }
                }
            }
            if (nowPlaying != null) {
                FocusSurface(
                    onClick = onNowPlaying,
                    modifier = Modifier.fillMaxWidth().height(44.dp).padding(top = 6.dp),
                    shape = RoundedCornerShape(50),
                    containerColor = GoldTextColor.copy(alpha = 0.16f),
                    focusedContainerColor = GoldTextColor,
                    focusedScale = 1.04f,
                    outline = false,
                    contentModifier = Modifier.fillMaxSize(),
                ) { focused ->
                    val fg = if (focused) InkColor else GoldTextColor
                    Row(
                        modifier = Modifier.fillMaxSize().padding(horizontal = 11.dp),
                        verticalAlignment = Alignment.CenterVertically,
                        horizontalArrangement = Arrangement.spacedBy(11.dp),
                    ) {
                        StreamIcon(StreamIcons.Music, tint = fg, size = 15.dp)
                        if (expanded) {
                            Column(modifier = Modifier.weight(1f)) {
                                Text("NOW PLAYING", color = fg, fontFamily = MonoFontFamily, fontSize = 9.sp, letterSpacing = 1.5.sp)
                                Text(
                                    nowPlaying.title ?: nowPlaying.name,
                                    color = if (focused) InkColor else TextColor,
                                    fontSize = 13.sp,
                                    maxLines = 1,
                                    overflow = TextOverflow.Ellipsis,
                                )
                            }
                        }
                    }
                }
            }
            Spacer(modifier = Modifier.weight(1f))
            sections.forEach { section ->
                val isCurrent = section == current
                FocusSurface(
                    onClick = { openSection(section) },
                    modifier = Modifier
                        .fillMaxWidth()
                        .height(38.dp)
                        .let { if (isCurrent) it.focusRequester(currentRequester) else it }
                        // Right opens the section too (and closes the drawer),
                        // like OK.
                        .onPreviewKeyEvent { e ->
                            if (e.key != Key.DirectionRight) return@onPreviewKeyEvent false
                            if (e.type == KeyEventType.KeyDown) openSection(section)
                            true
                        },
                    shape = RoundedCornerShape(50),
                    containerColor = if (isCurrent && !expanded) TonalColor else Color.Transparent,
                    focusedContainerColor = TextColor,
                    focusedScale = 1.04f,
                    outline = false,
                    contentModifier = Modifier.fillMaxSize(),
                ) { focused ->
                    val fg = when {
                        focused -> InkColor
                        isCurrent -> TextColor
                        else -> MutedColor
                    }
                    Row(
                        modifier = Modifier.fillMaxSize().padding(horizontal = 11.dp),
                        verticalAlignment = Alignment.CenterVertically,
                        horizontalArrangement = Arrangement.spacedBy(11.dp),
                    ) {
                        StreamIcon(section.icon, tint = fg, size = 15.dp)
                        if (expanded) {
                            Text(
                                text = section.label,
                                color = fg,
                                fontSize = 15.sp,
                                fontWeight = if (focused || isCurrent) FontWeight.SemiBold else FontWeight.Normal,
                                maxLines = 1,
                                modifier = Modifier.weight(1f),
                            )
                            counts[section]?.let { n ->
                                Text(
                                    text = n.toString(),
                                    color = if (focused) InkColor.copy(alpha = 0.6f) else MutedColor,
                                    fontFamily = MonoFontFamily,
                                    fontSize = 11.sp,
                                )
                            }
                        }
                    }
                }
            }
            Spacer(modifier = Modifier.weight(1f))
            if (expanded) {
                Box(modifier = Modifier.fillMaxWidth().padding(horizontal = 14.dp).height(1.dp).background(BorderColor))
                Text(
                    text = "Hold OK on a title to add it to a playlist",
                    color = MutedColor,
                    fontSize = 11.sp,
                    modifier = Modifier.padding(start = 14.dp, top = 10.dp),
                )
            } else {
                Spacer(modifier = Modifier.size(1.dp))
            }
        }
    }
}
