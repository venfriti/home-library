package com.venfriti.stream.ui.show

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.runtime.Composable
import androidx.compose.runtime.remember
import androidx.compose.ui.Modifier
import androidx.compose.ui.ExperimentalComposeUiApi
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.ui.browse.Backdrop
import com.venfriti.stream.ui.common.ButtonKind
import com.venfriti.stream.ui.common.IconCircleButton
import com.venfriti.stream.ui.common.RestoreFocus
import com.venfriti.stream.ui.common.StreamButton
import com.venfriti.stream.ui.common.StreamIcons
import com.venfriti.stream.ui.common.rememberFocusRestore
import com.venfriti.stream.ui.theme.Dimens
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.SerifFontFamily

// A film's counterpart to ShowDetailScreen: no episodes, so it's just the
// left-hand info column over the backdrop -- meta line, title, overview,
// cast/director, and Play/Resume actions.
@OptIn(ExperimentalComposeUiApi::class)
@Composable
fun MovieDetailScreen(
    entry: FileEntry,
    onPlay: (FileEntry, Boolean) -> Unit,
    onAddToPlaylist: (FileEntry) -> Unit,
) {
    val resuming = entry.progress > 0 && entry.progress < 0.97

    val restore = rememberFocusRestore()
    val playRequester = remember { FocusRequester() }
    RestoreFocus(restore, ready = true, fallback = playRequester)

    Box(modifier = Modifier.fillMaxSize()) {
        Backdrop(art = entry.backdrop ?: entry.posterUrl, heightFraction = 0.76f)

        Column(
            modifier = Modifier.padding(start = Dimens.ContentStart, top = 60.dp).fillMaxWidth(0.52f),
            verticalArrangement = Arrangement.spacedBy(10.dp),
        ) {
            val meta = listOfNotNull(
                "Film",
                entry.year.takeIf { it > 0 }?.toString(),
                entry.durationLabel.takeIf { it.isNotEmpty() },
                entry.rating.takeIf { it > 0 }?.let { "★ ${"%.1f".format(it)}" },
            ).joinToString(" · ").uppercase()
            Text(meta, color = GoldTextColor, fontFamily = MonoFontFamily, fontSize = 12.sp, letterSpacing = 2.sp)
            Text(
                entry.title ?: entry.name,
                color = Color.White,
                fontFamily = SerifFontFamily,
                fontWeight = FontWeight.Bold,
                fontSize = 50.sp,
                lineHeight = 54.sp,
                maxLines = 2,
                overflow = TextOverflow.Ellipsis,
            )
            if (!entry.genres.isNullOrEmpty()) {
                Text(entry.genres!!.joinToString("  ·  "), color = Color(0xFFB8B2A8), fontSize = 14.sp)
            }
            if (!entry.overview.isNullOrEmpty()) {
                // Clamped to three lines so the buttons never move between titles.
                Text(
                    entry.overview!!,
                    color = Color(0xFFB8B2A8),
                    fontSize = 15.sp,
                    lineHeight = 22.sp,
                    maxLines = 3,
                    overflow = TextOverflow.Ellipsis,
                )
            }
            val facts = listOfNotNull(
                entry.director?.let { "Dir. $it" },
                entry.cast?.take(3)?.joinToString(", ")?.takeIf { it.isNotEmpty() },
            )
            if (facts.isNotEmpty()) {
                Text(facts.joinToString("   "), color = Color(0xFF8A857C), fontFamily = MonoFontFamily, fontSize = 11.sp)
            }
            Row(horizontalArrangement = Arrangement.spacedBy(10.dp), modifier = Modifier.padding(top = 8.dp)) {
                StreamButton(
                    label = if (resuming) "Resume" else "Play",
                    icon = StreamIcons.Play,
                    kind = ButtonKind.Primary,
                    modifier = Modifier.focusRequester(playRequester),
                    onClick = { onPlay(entry, false) },
                )
                if (resuming) {
                    StreamButton(label = "Start over", icon = StreamIcons.Restart, onClick = { onPlay(entry, true) })
                }
                IconCircleButton(
                    icon = StreamIcons.Add,
                    description = "Add to playlist",
                    size = 38.dp,
                    onClick = { onAddToPlaylist(entry) },
                )
            }
        }
    }
}
