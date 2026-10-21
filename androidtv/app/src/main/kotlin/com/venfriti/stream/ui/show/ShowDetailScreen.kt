package com.venfriti.stream.ui.show

import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyRow
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.ExperimentalComposeUiApi
import androidx.compose.ui.Modifier
import androidx.compose.ui.draw.drawBehind
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.focusRestorer
import androidx.compose.ui.geometry.Offset
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.style.TextOverflow
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.data.ShowGroup
import com.venfriti.stream.ui.browse.Backdrop
import com.venfriti.stream.ui.browse.WideCard
import com.venfriti.stream.ui.common.ButtonKind
import com.venfriti.stream.ui.common.FocusSurface
import com.venfriti.stream.ui.common.IconCircleButton
import com.venfriti.stream.ui.common.RestoreFocus
import com.venfriti.stream.ui.common.StreamButton
import com.venfriti.stream.ui.common.StreamIcons
import com.venfriti.stream.ui.common.rememberFocusRestore
import com.venfriti.stream.ui.common.restorable
import com.venfriti.stream.ui.theme.BorderColor
import com.venfriti.stream.ui.theme.Dimens
import com.venfriti.stream.ui.theme.GoldColor
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.TextColor

// Where "Resume" should land: the most recently touched unfinished episode;
// else the one after the last finished; else the very first.
fun nextUp(show: ShowGroup): FileEntry {
    val eps = show.episodes
    eps.filter { it.progress > 0 && it.progress < 0.97 }.maxByOrNull { it.progressUpdated }?.let { return it }
    val lastDone = eps.filter { it.progress >= 0.97 }.maxByOrNull { it.progressUpdated }
    if (lastDone != null) {
        val i = eps.indexOf(lastDone)
        if (i >= 0 && i + 1 < eps.size) return eps[i + 1]
    }
    return eps.first()
}

private fun code(e: FileEntry) = "S${"%02d".format(e.season)}E${"%02d".format(e.episode)}"

// Mirrors ShowDetailWindow on the Windows client, laid out like the canvas:
// backdrop on the right, title + actions on the left, season tabs and a rail
// of episode stills below. Back returns here from the player now (it used to
// drop you on Browse).
@OptIn(ExperimentalComposeUiApi::class)
@Composable
fun ShowDetailScreen(
    show: ShowGroup,
    onPlay: (FileEntry, Boolean) -> Unit,
    onAddToPlaylist: (FileEntry) -> Unit,
) {
    val header = show.episodes.first()
    val seasons = remember(show) { show.episodes.map { it.season }.distinct().sorted() }
    val target = remember(show) { nextUp(show) }
    var seasonState by rememberSaveable(show.tmdbId) { mutableStateOf(target.season) }
    val currentSeason = if (seasonState in seasons) seasonState else seasons.first()
    val episodes = show.episodes.filter { it.season == currentSeason }
    val resuming = target.progress > 0 && target.progress < 0.97

    val restore = rememberFocusRestore()
    val playRequester = remember { FocusRequester() }
    RestoreFocus(restore, ready = true, fallback = playRequester)

    Box(modifier = Modifier.fillMaxSize()) {
        Backdrop(art = header.backdrop ?: header.episodeStill ?: show.posterUrl, heightFraction = 0.76f)

        Column(modifier = Modifier.fillMaxSize()) {
            Column(
                modifier = Modifier.padding(start = Dimens.ContentStart, top = 60.dp).fillMaxWidth(0.52f).height(260.dp),
                verticalArrangement = Arrangement.spacedBy(10.dp),
            ) {
                val meta = listOfNotNull(
                    "TV",
                    show.year.takeIf { it > 0 }?.toString(),
                    show.countLabel,
                    header.rating.takeIf { it > 0 }?.let { "★ ${"%.1f".format(it)}" },
                ).joinToString(" · ").uppercase()
                Text(meta, color = GoldTextColor, fontFamily = MonoFontFamily, fontSize = 12.sp, letterSpacing = 2.sp)
                Text(
                    show.displayTitle,
                    color = Color.White,
                    fontFamily = SerifFontFamily,
                    fontWeight = FontWeight.Bold,
                    fontSize = 50.sp,
                    lineHeight = 54.sp,
                    maxLines = 2,
                    overflow = TextOverflow.Ellipsis,
                )
                if (!header.overview.isNullOrEmpty()) {
                    // Clamped to three lines so the buttons never move between shows.
                    Text(
                        header.overview!!,
                        color = Color(0xFFB8B2A8),
                        fontSize = 15.sp,
                        lineHeight = 22.sp,
                        maxLines = 3,
                        overflow = TextOverflow.Ellipsis,
                    )
                }
                Row(horizontalArrangement = Arrangement.spacedBy(10.dp), modifier = Modifier.padding(top = 8.dp)) {
                    StreamButton(
                        label = (if (resuming) "Resume " else "Play ") + code(target),
                        icon = StreamIcons.Play,
                        kind = ButtonKind.Primary,
                        modifier = Modifier.focusRequester(playRequester),
                        onClick = { onPlay(target, false) },
                    )
                    if (resuming) {
                        StreamButton(label = "Start over", icon = StreamIcons.Restart, onClick = { onPlay(target, true) })
                    }
                    IconCircleButton(
                        icon = StreamIcons.Add,
                        description = "Add to playlist",
                        size = 38.dp,
                        onClick = { onAddToPlaylist(target) },
                    )
                }
            }

            if (seasons.size > 1) {
                Row(
                    horizontalArrangement = Arrangement.spacedBy(22.dp),
                    verticalAlignment = Alignment.CenterVertically,
                    modifier = Modifier.padding(start = Dimens.ContentStart, end = Dimens.Gutter, top = 18.dp).fillMaxWidth(),
                ) {
                    seasons.forEach { s ->
                        SeasonTab(
                            label = if (s == 0) "Specials" else "Season $s",
                            active = s == currentSeason,
                            onClick = { seasonState = s },
                        )
                    }
                    Box(
                        modifier = Modifier
                            .weight(1f)
                            .height(1.dp)
                            .drawBehind { drawRect(BorderColor) },
                    )
                }
            } else {
                Box(modifier = Modifier.height(18.dp))
            }

            LazyRow(
                state = rememberLazyListState(),
                contentPadding = PaddingValues(start = Dimens.ContentStart, end = Dimens.Gutter, top = 16.dp, bottom = 16.dp),
                horizontalArrangement = Arrangement.spacedBy(Dimens.RowGap),
                modifier = Modifier.fillMaxWidth().focusRestorer(),
            ) {
                items(episodes, key = { it.key }) { ep ->
                    val sub = when {
                        ep.progress >= 0.97 -> "Watched"
                        ep.progress > 0 -> ep.remainingLabel
                        else -> ep.durationLabel
                    }
                    WideCard(
                        imageUrl = ep.episodeStill ?: ep.thumbnailImageUrl,
                        title = ep.episodeRowTitle,
                        subtitle = sub,
                        badge = "E${"%02d".format(ep.episode)}",
                        progress = ep.progress,
                        width = 190.dp,
                        modifier = Modifier.restorable(restore, ep.key),
                        onLongClick = { onAddToPlaylist(ep) },
                        onClick = { onPlay(ep, false) },
                    )
                }
            }
        }
    }
}

// Material 3 primary tab: serif label, gold underline when active.
@Composable
private fun SeasonTab(label: String, active: Boolean, onClick: () -> Unit) {
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
            fontSize = 17.sp,
            modifier = Modifier
                .padding(horizontal = 12.dp, vertical = 8.dp)
                .drawBehind {
                    if (active) {
                        val y = size.height + 4.dp.toPx()
                        drawLine(GoldColor, Offset(0f, y), Offset(size.width, y), strokeWidth = 2.dp.toPx())
                    }
                },
        )
    }
}
