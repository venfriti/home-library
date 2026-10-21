package com.venfriti.stream.ui.browse

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxHeight
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.LazyColumn
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.GridItemSpan
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.grid.rememberLazyGridState
import androidx.compose.foundation.lazy.items
import androidx.compose.foundation.lazy.rememberLazyListState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.Color
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.data.Library
import com.venfriti.stream.data.ShowGroup
import com.venfriti.stream.ui.common.FocusSurface
import com.venfriti.stream.ui.common.LocalTileTracker
import com.venfriti.stream.ui.common.RestoreFocus
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.rememberUpdatedState
import com.venfriti.stream.ui.common.rememberFocusRestore
import com.venfriti.stream.ui.common.restorable
import com.venfriti.stream.ui.nav.Section
import com.venfriti.stream.ui.theme.BgColor
import com.venfriti.stream.ui.theme.BorderColor
import com.venfriti.stream.ui.theme.Dimens
import com.venfriti.stream.ui.theme.HeroTint
import com.venfriti.stream.ui.theme.InkColor
import com.venfriti.stream.ui.theme.MonoFontFamily
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.TextColor
import com.venfriti.stream.ui.theme.TextDimColor

private enum class Sort(val label: String) { AZ("A–Z"), Year("Year"), Unwatched("Unwatched") }

// A whole section as a wrapping grid -- the website's #file-grid layout,
// which Home now only previews as capped rails.
@Composable
fun SectionScreen(
    section: Section,
    library: Library,
    onPlay: (FileEntry) -> Unit,
    onOpenShow: (ShowGroup) -> Unit,
    onOpenMovie: (FileEntry) -> Unit,
    onLongPress: (FileEntry) -> Unit,
    // Music videos play through a list (Next / auto-advance): this one.
    onPlayQueue: (FileEntry, List<FileEntry>) -> Unit = { f, _ -> onPlay(f) },
) {
    if (section == Section.Playlists) {
        PlaylistsScreen(library, onPlayQueue, onLongPress)
        return
    }

    var sortName by rememberSaveable(section) { mutableStateOf(Sort.AZ.name) }
    val sort = Sort.valueOf(sortName)
    val wide = section == Section.Continue || section == Section.Music
    val restore = rememberFocusRestore()
    val firstChip = remember { FocusRequester() }

    // Series tiles are one per show, everything else one per file.
    val files: List<FileEntry> = when (section) {
        Section.Continue -> library.continueWatching
        Section.Films -> library.films
        Section.Music -> library.musicVideos
        Section.Library -> library.other
        else -> emptyList()
    }
    val sortedFiles = remember(files, sort) {
        when (sort) {
            Sort.AZ -> if (section == Section.Continue) files else files.sortedBy { (it.title ?: it.name).lowercase() }
            Sort.Year -> files.sortedByDescending { it.year }
            Sort.Unwatched -> files.filter { it.progress == 0.0 }
        }
    }
    val shows = remember(library.files, sort) {
        val all = library.shows
        when (sort) {
            Sort.AZ -> all
            Sort.Year -> all.sortedByDescending { it.year }
            Sort.Unwatched -> all.filter { it.progress == 0.0 }
        }
    }
    val count = if (section == Section.Series) library.shows.size else files.size

    RestoreFocus(restore, ready = !library.loading, fallback = firstChip)

    // Lets the app scroll a song's tile into view (focus after minimizing
    // lands on the song now playing, which may be off screen in a long grid).
    val gridState = rememberLazyGridState()
    val tracker = LocalTileTracker.current
    val currentFiles by rememberUpdatedState(sortedFiles)
    DisposableEffect(tracker, gridState) {
        val reveal: suspend (String) -> Boolean = reveal@{ key ->
            val i = currentFiles.indexOfFirst { it.key == key }
            if (i < 0) return@reveal false
            gridState.scrollToItem(i + 1) // +1: the header row is item 0
            true
        }
        tracker?.reveal = reveal
        onDispose { if (tracker?.reveal === reveal) tracker?.reveal = null }
    }

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Brush.verticalGradient(0f to HeroTint, 0.45f to BgColor)),
    ) {
        LazyVerticalGrid(
            columns = GridCells.Fixed(if (wide) 4 else 7),
            state = gridState,
            contentPadding = PaddingValues(start = Dimens.ContentStart, end = Dimens.Gutter, top = 56.dp, bottom = 48.dp),
            horizontalArrangement = Arrangement.spacedBy(18.dp),
            verticalArrangement = Arrangement.spacedBy(if (wide) 22.dp else 18.dp),
            modifier = Modifier.fillMaxSize(),
        ) {
            item(span = { GridItemSpan(maxLineSpan) }) {
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(10.dp),
                    modifier = Modifier.fillMaxWidth().padding(bottom = 14.dp),
                ) {
                    Text(section.label, color = TextColor, fontFamily = SerifFontFamily, fontWeight = FontWeight.Bold, fontSize = 32.sp)
                    Text(count.toString(), color = MutedColor, fontFamily = MonoFontFamily, fontSize = 12.sp, modifier = Modifier.padding(top = 10.dp))
                    Box(modifier = Modifier.weight(1f))
                    if (section != Section.Continue) {
                        Sort.entries.forEachIndexed { i, s ->
                            Chip(
                                label = s.label,
                                selected = s == sort,
                                modifier = if (i == 0) Modifier.focusRequester(firstChip) else Modifier,
                                onClick = { sortName = s.name },
                            )
                        }
                    }
                }
            }

            if (section == Section.Series) {
                items(shows, key = { it.tmdbId }) { s ->
                    PosterCard(
                        imageUrl = s.cardImageUrl,
                        title = s.displayTitle,
                        year = s.year.takeIf { it > 0 }?.toString(),
                        extra = s.countLabel,
                        kind = "TV",
                        progress = s.progress,
                        modifier = Modifier.restorable(restore, "show:${s.tmdbId}"),
                        onClick = { onOpenShow(s) },
                    )
                }
            } else if (wide) {
                items(sortedFiles, key = { it.key }) { f ->
                    WideCard(
                        imageUrl = f.thumbnailImageUrl,
                        title = if (section == Section.Continue) f.continueWatchingLine else (f.title ?: f.name),
                        subtitle = if (section == Section.Continue) f.remainingLabel else f.artist,
                        // Music videos always start from the beginning: no
                        // progress sliver.
                        progress = if (f.isMusicVideo) 0.0 else f.progress,
                        modifier = Modifier.restorable(restore, f.key, fileKey = f.key),
                        onLongClick = { onLongPress(f) },
                        onClick = { if (section == Section.Music) onPlayQueue(f, sortedFiles) else onPlay(f) },
                    )
                }
            } else {
                items(sortedFiles, key = { it.key }) { f ->
                    PosterCard(
                        imageUrl = f.cardImageUrl,
                        title = f.title ?: f.name,
                        year = f.year.takeIf { it > 0 }?.toString(),
                        kind = if (section == Section.Films) "Film" else "No artwork",
                        progress = f.progress,
                        modifier = Modifier.restorable(restore, f.key),
                        onLongClick = { onLongPress(f) },
                        onClick = { if (section == Section.Films) onOpenMovie(f) else onPlay(f) },
                    )
                }
            }
        }
    }
}

@Composable
private fun Chip(label: String, selected: Boolean, modifier: Modifier = Modifier, onClick: () -> Unit) {
    val shape = RoundedCornerShape(50)
    FocusSurface(
        onClick = onClick,
        modifier = modifier.height(30.dp),
        shape = shape,
        containerColor = if (selected) TextColor else Color.Transparent,
        focusedContainerColor = TextColor,
        focusedScale = 1.06f,
        outline = false,
        contentModifier = Modifier.fillMaxHeight(),
    ) { focused ->
        Box(
            modifier = Modifier
                .fillMaxHeight()
                .let { if (!selected && !focused) it.border(1.dp, BorderColor, shape) else it }
                .padding(horizontal = 14.dp),
            contentAlignment = Alignment.Center,
        ) {
            Text(
                text = label,
                color = if (selected || focused) InkColor else TextDimColor,
                fontSize = 13.sp,
            )
        }
    }
}

// Playlists: one rail per playlist, in the order the server returns them.
@Composable
private fun PlaylistsScreen(library: Library, onPlayQueue: (FileEntry, List<FileEntry>) -> Unit, onLongPress: (FileEntry) -> Unit) {
    val restore = rememberFocusRestore()
    RestoreFocus(restore, ready = !library.loading, fallback = null)
    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Brush.verticalGradient(0f to HeroTint, 0.45f to BgColor)),
    ) {
        LazyColumn(
            state = rememberLazyListState(),
            contentPadding = PaddingValues(top = 56.dp, bottom = 48.dp),
            verticalArrangement = Arrangement.spacedBy(22.dp),
            modifier = Modifier.fillMaxSize(),
        ) {
            item {
                Text(
                    "Playlists",
                    color = TextColor,
                    fontFamily = SerifFontFamily,
                    fontWeight = FontWeight.Bold,
                    fontSize = 32.sp,
                    modifier = Modifier.padding(start = Dimens.ContentStart, bottom = 6.dp),
                )
            }
            if (library.playlists.isEmpty()) {
                item {
                    Text(
                        "No playlists yet — hold OK on any title to start one.",
                        color = MutedColor,
                        fontSize = 15.sp,
                        modifier = Modifier.padding(start = Dimens.ContentStart),
                    )
                }
            }
            items(library.playlists, key = { it.id }) { p ->
                val entries = library.playlistItems(p)
                Column {
                    if (entries.isEmpty()) {
                        RailHead(p.name, 0)
                        Text("Empty", color = MutedColor, fontSize = 13.sp, modifier = Modifier.padding(start = Dimens.ContentStart, top = 12.dp))
                    } else {
                        Rail(p.name, entries.size, entries, key = { "${p.id}:${it.key}" }) { f ->
                            WideCard(
                                imageUrl = f.thumbnailImageUrl,
                                title = f.continueWatchingLine,
                                subtitle = f.artist ?: f.durationLabel,
                                progress = if (f.isMusicVideo) 0.0 else f.progress,
                                width = Dimens.WideCardWidth,
                                modifier = Modifier.restorable(restore, "${p.id}:${f.key}", fileKey = f.key),
                                onLongClick = { onLongPress(f) },
                                onClick = { onPlayQueue(f, entries) },
                            )
                        }
                    }
                }
            }
        }
    }
}
