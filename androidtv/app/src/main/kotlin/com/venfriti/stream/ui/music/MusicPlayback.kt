package com.venfriti.stream.ui.music

import android.content.Context
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import androidx.media3.common.C
import androidx.media3.common.MediaItem
import androidx.media3.common.Player
import androidx.media3.exoplayer.ExoPlayer
import com.venfriti.stream.data.FileEntry
import com.venfriti.stream.network.ServerClient
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.Job
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.delay
import kotlinx.coroutines.isActive
import kotlinx.coroutines.launch

enum class MusicMode { Hidden, Full, Mini }

// App-level music video playback -- the TV version of the web's
// minimizePlayer()/restorePlayer(). Unlike PlayerScreen (one player per
// film/episode, torn down on Back), this one player lives as long as the app
// so a video can keep playing in the corner while you browse.
//
// Music videos always start from 0 (resumePlayback's fromStart rule on the
// web); progress is still reported so Continue watching stays accurate.
class MusicPlayback(
    context: Context,
    private val client: ServerClient,
    private val onProgress: (key: String, positionSeconds: Double, durationSeconds: Double) -> Unit,
    // Each video as it starts, with the list it's playing in (QueueMemory).
    private val onPlayed: (entry: FileEntry, queue: List<FileEntry>) -> Unit = { _, _ -> },
) {
    private val scope = CoroutineScope(SupervisorJob() + Dispatchers.Main.immediate)
    private val saveScope = CoroutineScope(SupervisorJob() + Dispatchers.IO)

    val player: ExoPlayer = ExoPlayer.Builder(context.applicationContext).build()

    var mode by mutableStateOf(MusicMode.Hidden)
        private set
    var queue by mutableStateOf<List<FileEntry>>(emptyList())
        private set
    var index by mutableStateOf(0)
        private set
    var isPlaying by mutableStateOf(false)
        private set
    var positionMs by mutableStateOf(0L)
        private set
    var bufferedMs by mutableStateOf(0L)
        private set
    var durationMs by mutableStateOf(0L)
        private set
    var error by mutableStateOf<String?>(null)
        private set

    val current: FileEntry? get() = queue.getOrNull(index)
    val hasNext: Boolean get() = index + 1 < queue.size
    val hasPrevious: Boolean get() = index > 0
    val active: Boolean get() = mode != MusicMode.Hidden

    private var loadJob: Job? = null

    init {
        player.addListener(object : Player.Listener {
            override fun onIsPlayingChanged(playing: Boolean) { isPlaying = playing }
            override fun onPlaybackStateChanged(state: Int) {
                // End of a song: on to the next one in the list it was started
                // from; after the last one, the mini player goes away.
                if (state == Player.STATE_ENDED) {
                    flush()
                    if (hasNext) playAt(index + 1) else close()
                }
            }
            override fun onPlayerError(e: androidx.media3.common.PlaybackException) {
                error = if (e.errorCode in 2000..2999) "Lost the server — retrying" else "Can't play this one (${e.errorCodeName})"
                if (e.errorCode in 2000..2999) retryLater()
            }
        })
        scope.launch {
            var sinceSave = 0L
            while (isActive) {
                // Nothing loaded: poll slowly and skip the work instead of
                // waking twice a second for the life of the app.
                if (mode == MusicMode.Hidden) { delay(2_000); continue }
                positionMs = player.currentPosition
                bufferedMs = player.bufferedPosition
                player.duration.takeIf { it > 0 && it != C.TIME_UNSET }?.let { durationMs = it }
                sinceSave += 500
                if (sinceSave >= 10_000 && player.isPlaying) { sinceSave = 0; flush() }
                delay(500)
            }
        }
    }

    // Start (or replace) playback: `list` is where the video was picked from
    // -- the music rail, the music grid, a playlist -- and is what Next/auto-
    // advance walk through.
    fun start(list: List<FileEntry>, startIndex: Int) {
        flush()
        queue = list.filter { it.isPlayable }
        index = queue.indexOfFirst { it.key == list.getOrNull(startIndex)?.key }.coerceAtLeast(0)
        mode = MusicMode.Full
        playAt(index)
    }

    fun playAt(i: Int) {
        if (i !in queue.indices) return
        if (i != index) flush()
        index = i
        error = null
        val entry = queue[i]
        onPlayed(entry, queue)
        loadJob?.cancel()
        loadJob = scope.launch {
            try {
                val url = client.resolvePlaybackUrl(entry)
                player.setMediaItem(MediaItem.fromUri(url))
                player.prepare()
                player.playWhenReady = true
            } catch (e: Exception) {
                error = "Couldn't start this one: ${e.message}"
            }
        }
    }

    // The video already playing, picked again from a different list: keep it
    // going exactly where it is and make that list the one Next walks.
    fun adoptQueue(list: List<FileEntry>) {
        val cur = current ?: return
        val q = list.filter { it.isPlayable }
        val i = q.indexOfFirst { it.key == cur.key }
        if (i < 0) return
        queue = q
        index = i
        onPlayed(cur, q)
    }

    fun next() { if (hasNext) playAt(index + 1) }

    fun previous() {
        // Like every music player: a few seconds in, "previous" restarts.
        if (player.currentPosition > 3_000 || !hasPrevious) player.seekTo(0) else playAt(index - 1)
    }

    fun togglePlay() { player.playWhenReady = !player.playWhenReady }
    fun seekTo(ms: Long) = player.seekTo(ms.coerceAtLeast(0))
    fun seekBy(ms: Long) = player.seekTo((player.currentPosition + ms).coerceAtLeast(0))
    fun pause() = player.pause()

    fun minimize() { if (mode == MusicMode.Full) mode = MusicMode.Mini }
    fun restore() { if (mode == MusicMode.Mini) mode = MusicMode.Full }

    fun close() {
        flush()
        loadJob?.cancel()
        player.stop()
        player.clearMediaItems()
        mode = MusicMode.Hidden
        queue = emptyList()
        error = null
    }

    fun release() {
        flush()
        scope.cancel()
        player.release()
    }

    private fun retryLater() {
        scope.launch {
            while (error != null && mode != MusicMode.Hidden) {
                delay(5_000)
                player.prepare()
                delay(3_000)
                if (player.playerError == null && player.playbackState != Player.STATE_IDLE) error = null
            }
        }
    }

    private fun flush() {
        val entry = current ?: return
        val d = player.duration
        if (d <= 0 || d == C.TIME_UNSET) return
        val pos = player.currentPosition / 1000.0
        val dur = d / 1000.0
        onProgress(entry.key, pos, dur)
        saveScope.launch { client.postProgress(entry.key, pos, dur) }
    }
}
