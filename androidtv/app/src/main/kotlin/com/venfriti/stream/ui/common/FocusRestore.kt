package com.venfriti.stream.ui.common

import androidx.compose.runtime.Composable
import androidx.compose.runtime.DisposableEffect
import androidx.compose.runtime.LaunchedEffect
import androidx.compose.runtime.staticCompositionLocalOf
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.saveable.rememberSaveable
import androidx.compose.runtime.setValue
import androidx.compose.runtime.withFrameNanos
import androidx.compose.ui.Modifier
import androidx.compose.ui.composed
import androidx.compose.ui.focus.FocusRequester
import androidx.compose.ui.focus.focusRequester
import androidx.compose.ui.focus.onFocusChanged

// Remembers which tile had focus on a screen, so coming back from the player
// (or a show page) puts focus back on the exact card you left -- not on the
// hero at the top of a 30-film wall. The key lives in rememberSaveable, which
// MainActivity scopes per back-stack entry with a SaveableStateHolder, so it
// survives the screen leaving composition while the player is up.
class FocusRestore internal constructor(
    val restoreKey: String?,
    private val save: (String) -> Unit,
) {
    val requester = FocusRequester()
    fun onFocused(key: String) = save(key)
}

@Composable
fun rememberFocusRestore(): FocusRestore {
    var lastKey by rememberSaveable { mutableStateOf<String?>(null) }
    // Snapshot of the key at the moment this screen (re)entered composition.
    return remember { FocusRestore(lastKey) { lastKey = it } }
}

// App-level record of tiles on the current page: the last one that had focus
// (Compose's focusRestorer only records when focus leaves by D-pad movement,
// not when it's moved programmatically -- e.g. Back jumping to the corner
// video), and which tile shows which file, so focus can go to the song
// that's now playing. `reveal` lets the current screen scroll a file's tile
// into view when it isn't composed yet (a lazy grid only builds what's
// visible).
class TileTracker {
    var lastFocused: FocusRequester? = null
    // Bumped every time a tile actually gains focus: the proof a focus
    // request landed on a tile rather than stranding on a group.
    var focusEvents = 0
        private set
    private val byFile = mutableMapOf<String, MutableList<FocusRequester>>()
    var reveal: (suspend (fileKey: String) -> Boolean)? = null

    fun onTileFocused(req: FocusRequester) {
        lastFocused = req
        focusEvents++
    }

    fun register(fileKey: String, req: FocusRequester) {
        byFile.getOrPut(fileKey) { mutableListOf() }.add(req)
    }

    fun unregister(fileKey: String, req: FocusRequester) {
        byFile[fileKey]?.let { it.remove(req); if (it.isEmpty()) byFile.remove(fileKey) }
        if (lastFocused === req) lastFocused = null
    }

    fun requesterFor(fileKey: String): FocusRequester? = byFile[fileKey]?.lastOrNull()
}

val LocalTileTracker = staticCompositionLocalOf<TileTracker?> { null }

// hasFocus, not isFocused: WideCard puts this on its Column, whose child is
// the focusable card. A FocusRequester on a parent focuses its first child.
// `fileKey`: the file this tile plays, so the app can find it by file.
fun Modifier.restorable(restore: FocusRestore, key: String, fileKey: String? = null): Modifier = composed {
    val tracker = LocalTileTracker.current
    val req = remember { FocusRequester() }
    if (tracker != null) {
        DisposableEffect(tracker, fileKey, req) {
            if (fileKey != null) tracker.register(fileKey, req)
            onDispose { tracker.unregister(fileKey ?: "", req); if (tracker.lastFocused === req) tracker.lastFocused = null }
        }
    }
    (if (key == restore.restoreKey) this.focusRequester(restore.requester) else this)
        .focusRequester(req)
        .onFocusChanged {
            if (it.hasFocus) {
                restore.onFocused(key)
                tracker?.onTileFocused(req)
            }
        }
}

// The current screen's natural first stop, published by RestoreFocus, so
// app-level code (closing the side menu, closing the corner video) always
// has somewhere real to put focus when restoring the last tile doesn't land.
class ScreenFocusFallback {
    var requester: FocusRequester? = null
}

val LocalScreenFocusFallback = staticCompositionLocalOf<ScreenFocusFallback?> { null }

// Once content is on screen (`ready`), focus the remembered tile, or fall back
// to the screen's natural first stop. A remote has no pointer to recover
// with, so a screen must never appear with nothing focused.
@Composable
fun RestoreFocus(restore: FocusRestore, ready: Boolean, fallback: FocusRequester?) {
    val holder = LocalScreenFocusFallback.current
    DisposableEffect(holder, fallback) {
        holder?.requester = fallback
        onDispose { if (holder?.requester === fallback) holder?.requester = null }
    }
    LaunchedEffect(ready) {
        if (!ready) return@LaunchedEffect
        withFrameNanos { }
        withFrameNanos { }
        val restored = restore.restoreKey != null && runCatching { restore.requester.requestFocus() }.isSuccess
        if (!restored && fallback != null) runCatching { fallback.requestFocus() }
    }
}
