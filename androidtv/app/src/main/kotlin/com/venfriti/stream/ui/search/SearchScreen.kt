package com.venfriti.stream.ui.search

import androidx.compose.foundation.background
import androidx.compose.foundation.border
import androidx.compose.foundation.layout.Arrangement
import androidx.compose.foundation.layout.Box
import androidx.compose.foundation.layout.PaddingValues
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.lazy.grid.GridCells
import androidx.compose.foundation.lazy.grid.GridItemSpan
import androidx.compose.foundation.lazy.grid.LazyVerticalGrid
import androidx.compose.foundation.lazy.grid.items
import androidx.compose.foundation.lazy.grid.rememberLazyGridState
import androidx.compose.foundation.shape.RoundedCornerShape
import androidx.compose.foundation.text.BasicTextField
import androidx.compose.foundation.text.KeyboardActions
import androidx.compose.foundation.text.KeyboardOptions
import androidx.compose.runtime.Composable
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.focus.FocusDirection
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.onFocusChanged
import androidx.compose.ui.graphics.Brush
import androidx.compose.ui.graphics.SolidColor
import androidx.compose.ui.platform.LocalFocusManager
import androidx.compose.ui.text.TextStyle
import androidx.compose.ui.text.font.FontWeight
import androidx.compose.ui.text.input.ImeAction
import androidx.compose.ui.unit.dp
import androidx.compose.ui.unit.sp
import androidx.tv.material3.Text
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.data.Library
import com.venfriti.stream.data.ShowGroup
import com.venfriti.stream.ui.browse.PosterCard
import com.venfriti.stream.ui.common.StreamIcon
import com.venfriti.stream.ui.common.StreamIcons
import com.venfriti.stream.ui.theme.BgColor
import com.venfriti.stream.ui.theme.BorderColor
import com.venfriti.stream.ui.theme.Dimens
import com.venfriti.stream.ui.theme.GoldTextColor
import com.venfriti.stream.ui.theme.HeroTint
import com.venfriti.stream.ui.theme.MutedColor
import com.venfriti.stream.ui.theme.SansFontFamily
import com.venfriti.stream.ui.theme.SerifFontFamily
import com.venfriti.stream.ui.theme.TextColor
import com.venfriti.stream.ui.theme.TonalColor

private sealed class Hit(val key: String) {
    class Show(val show: ShowGroup) : Hit("show:${show.tmdbId}")
    class Item(val entry: FileEntry) : Hit(entry.key)
}

private fun FileEntry.matches(q: String): Boolean {
    val fields = listOfNotNull(title, name, artist, album, director, episodeTitle) + cast.orEmpty()
    return fields.any { it.lowercase().contains(q) }
}

// Local search over the already-loaded library -- title, artist, cast,
// director -- so results update on every keystroke with no server round
// trip. The field opens the TV's own on-screen keyboard (which on most
// Google TV devices also offers the mic button for voice input).
@Composable
fun SearchScreen(library: Library, onPlay: (FileEntry) -> Unit, onOpenShow: (ShowGroup) -> Unit) {
    var query by rememberSaveable { mutableStateOf("") }
    var fieldFocused by remember { mutableStateOf(false) }
    val fieldRequester = remember { FocusRequester() }
    val focusManager = LocalFocusManager.current

    LaunchedEffect(Unit) { runCatching { fieldRequester.requestFocus() } }

    val hits: List<Hit> = remember(query, library.files) {
        val q = query.trim().lowercase()
        if (q.length < 2) return@remember emptyList()
        val shows = library.shows.filter { s -> s.displayTitle.lowercase().contains(q) || s.episodes.any { it.matches(q) } }
            .map { Hit.Show(it) }
        val files = library.files.filter { it.mediaType != "tv" && it.matches(q) }.map { Hit.Item(it) }
        (shows + files).take(70)
    }

    Box(
        modifier = Modifier
            .fillMaxSize()
            .background(Brush.verticalGradient(0f to HeroTint, 0.45f to BgColor)),
    ) {
        LazyVerticalGrid(
            columns = GridCells.Fixed(7),
            state = rememberLazyGridState(),
            contentPadding = PaddingValues(start = Dimens.ContentStart, end = Dimens.Gutter, top = 56.dp, bottom = 48.dp),
            horizontalArrangement = Arrangement.spacedBy(18.dp),
            verticalArrangement = Arrangement.spacedBy(18.dp),
            modifier = Modifier.fillMaxSize(),
        ) {
            item(span = { GridItemSpan(maxLineSpan) }) {
                Row(
                    verticalAlignment = Alignment.CenterVertically,
                    horizontalArrangement = Arrangement.spacedBy(14.dp),
                    modifier = Modifier
                        .fillMaxWidth()
                        .padding(bottom = 18.dp)
                        .height(56.dp)
                        .background(if (fieldFocused) TextColor.copy(alpha = 0.16f) else TonalColor, RoundedCornerShape(50))
                        .border(2.dp, if (fieldFocused) TextColor else BorderColor, RoundedCornerShape(50))
                        .padding(horizontal = 22.dp),
                ) {
                    StreamIcon(StreamIcons.Search, tint = if (fieldFocused) TextColor else MutedColor, size = 20.dp)
                    Box(modifier = Modifier.weight(1f)) {
                        if (query.isEmpty()) {
                            Text("Films, series, artists, cast…", color = MutedColor, fontSize = 19.sp)
                        }
                        BasicTextField(
                            value = query,
                            onValueChange = { query = it },
                            singleLine = true,
                            textStyle = TextStyle(color = TextColor, fontSize = 19.sp, fontFamily = SansFontFamily),
                            cursorBrush = SolidColor(GoldTextColor),
                            keyboardOptions = KeyboardOptions(imeAction = ImeAction.Search),
                            keyboardActions = KeyboardActions(onSearch = { focusManager.moveFocus(FocusDirection.Down) }),
                            modifier = Modifier
                                .fillMaxWidth()
                                .focusRequester(fieldRequester)
                                .onFocusChanged { fieldFocused = it.isFocused },
                        )
                    }
                }
            }

            if (query.trim().length >= 2 && hits.isEmpty()) {
                item(span = { GridItemSpan(maxLineSpan) }) {
                    Text("Nothing matches “${query.trim()}”", color = MutedColor, fontSize = 16.sp)
                }
            }
            if (query.trim().length < 2) {
                item(span = { GridItemSpan(maxLineSpan) }) {
                    Text(
                        "Search",
                        color = TextColor,
                        fontFamily = SerifFontFamily,
                        fontWeight = FontWeight.Bold,
                        fontSize = 32.sp,
                    )
                }
            }

            items(hits, key = { it.key }) { hit ->
                when (hit) {
                    is Hit.Show -> PosterCard(
                        imageUrl = hit.show.cardImageUrl,
                        title = hit.show.displayTitle,
                        extra = hit.show.countLabel,
                        kind = "TV",
                        progress = hit.show.progress,
                        onClick = { onOpenShow(hit.show) },
                    )
                    is Hit.Item -> PosterCard(
                        imageUrl = hit.entry.cardImageUrl,
                        title = hit.entry.title ?: hit.entry.name,
                        year = hit.entry.year.takeIf { it > 0 }?.toString(),
                        extra = hit.entry.artist,
                        kind = if (hit.entry.isMusicVideo) "Music video" else "Film",
                        progress = hit.entry.progress,
                        onClick = { onPlay(hit.entry) },
                    )
                }
            }
        }
    }
}
