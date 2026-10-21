package com.venfriti.stream.data

import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.setValue
import com.venfriti.stream.network.ServerClient
import java.io.File
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.withContext
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json

// What's kept on the TV between launches, so the wall can open instantly --
// even with the server off -- and only Play needs the server.
@Serializable
private data class LibrarySnapshot(val files: List<FileEntry>, val playlists: List<PlaylistDto>)

private val cacheJson = Json { ignoreUnknownKeys = true }

// One cached copy of the library for the whole app session. It used to be
// fetched by BrowseScreen itself, so every trip back from the player
// re-downloaded /files and lost the viewer's place on the wall. Now screens
// only read from here, and returning from playback just re-pulls progress.
//
// Progress is applied by copy(), never by mutating a FileEntry in place:
// Compose only notices a new list of new objects, so an in-place write to
// `progress` left the gold bars stale until the next full reload.
class Library(private val client: ServerClient, private val cacheFile: File? = null) {
    var files by mutableStateOf<List<FileEntry>>(emptyList())
        private set
    var playlists by mutableStateOf<List<PlaylistDto>>(emptyList())
        private set
    var loading by mutableStateOf(true)
        private set
    // True while what's on screen came from the on-TV copy, not the server.
    var fromCache by mutableStateOf(false)
        private set

    suspend fun load() {
        // Refreshing over a cached copy keeps the wall on screen.
        if (files.isEmpty()) loading = true
        // Mirrors the web's default source: the LAN-only local library first,
        // the cloud listing only as a fallback (see ServerClient.getLocalFiles).
        // Already showing the local library: a failed request is a hiccup,
        // not a reason to swap the whole wall for the cloud listing -- throw,
        // and the caller keeps what's on screen.
        val local = try {
            client.getLocalFiles()
        } catch (e: Exception) {
            if (files.any { it.isLocal }) throw e
            emptyList()
        }
        val raw = (local.ifEmpty { client.getFiles() }).filter { it.isPlayable }
        files = withProgress(raw, client.getAllProgress())
        playlists = try { client.getPlaylists() } catch (e: Exception) { emptyList() }
        loading = false
        fromCache = false
        saveCache()
    }

    suspend fun loadCached(): Boolean = withContext(Dispatchers.IO) {
        val f = cacheFile ?: return@withContext false
        if (!f.exists()) return@withContext false
        try {
            val snap = cacheJson.decodeFromString<LibrarySnapshot>(f.readText())
            withContext(Dispatchers.Main) {
                files = snap.files
                playlists = snap.playlists
                loading = false
                fromCache = true
            }
            true
        } catch (e: Exception) {
            false
        }
    }

    private suspend fun saveCache() {
        val f = cacheFile ?: return
        val snap = LibrarySnapshot(files, playlists)
        withContext(Dispatchers.IO) {
            runCatching {
                val tmp = File(f.parentFile, f.name + ".tmp")
                tmp.writeText(cacheJson.encodeToString(snap))
                tmp.renameTo(f)
            }
        }
    }

    // The player reports its last position here the moment it closes, so the
    // wall's progress bars are right immediately instead of waiting on a
    // round trip that could race the player's own final POST.
    fun applyLocalProgress(key: String, positionSeconds: Double, durationSeconds: Double) {
        if (durationSeconds <= 0) return
        // Newest in the list whatever unit the server's `updated` uses, so the
        // title jumps to the front of Continue watching straight away.
        val newest = (files.maxOfOrNull { it.progressUpdated } ?: 0L) + 1
        files = files.map { f ->
            if (f.key != key) f else f.copy(
                progress = (positionSeconds / durationSeconds).coerceIn(0.0, 1.0),
                positionSeconds = positionSeconds,
                durationSeconds = durationSeconds,
                progressUpdated = newest,
            )
        }
    }

    suspend fun refreshPlaylists() {
        playlists = try { client.getPlaylists() } catch (e: Exception) { playlists }
    }

    suspend fun refreshProgress() {
        files = withProgress(files, client.getAllProgress())
    }

    private fun withProgress(list: List<FileEntry>, progress: Map<String, PlaybackProgress>): List<FileEntry> =
        list.map { f ->
            val p = progress[f.key]
            if (p != null && p.duration > 0) {
                f.copy(
                    progress = (p.position / p.duration).coerceIn(0.0, 1.0),
                    positionSeconds = p.position,
                    durationSeconds = p.duration,
                    progressUpdated = p.updated,
                )
            } else {
                f.copy(progress = 0.0, positionSeconds = 0.0, durationSeconds = 0.0, progressUpdated = 0)
            }
        }

    // continueItems() in main.go: recency-sorted, music videos capped at 4,
    // 8 total. A title counts as finished (and drops out) past 97%.
    val continueWatching: List<FileEntry>
        get() {
            val sorted = files.filter { it.progress > 0 && it.progress < 0.97 }.sortedByDescending { it.progressUpdated }
            var musicSeen = 0
            val result = mutableListOf<FileEntry>()
            for (f in sorted) {
                if (f.isMusicVideo && ++musicSeen > 4) continue
                result.add(f)
                if (result.size == 8) break
            }
            return result
        }

    val films: List<FileEntry> get() = files.filter { it.section == ContentSection.Film }
    val musicVideos: List<FileEntry> get() = files.filter { it.section == ContentSection.MusicVideo }
    val other: List<FileEntry> get() = files.filter { it.section == ContentSection.Library }

    val shows: List<ShowGroup>
        get() = files.filter { it.section == ContentSection.Series }
            .groupBy { showId(it) }
            .map { (id, eps) -> ShowGroup(id, eps) }
            .sortedBy { it.displayTitle }

    fun show(id: Int): ShowGroup? = shows.firstOrNull { it.tmdbId == id }

    fun byKey(key: String): FileEntry? = files.firstOrNull { it.key == key }

    fun playlistItems(p: PlaylistDto): List<FileEntry> {
        val index = files.associateBy { it.key }
        return p.items.mapNotNull { index[it] }
    }

    companion object {
        // Stable across copies (the old -hashCode() key changed whenever
        // progress did, which broke re-finding a show after playback).
        fun showId(f: FileEntry): Int = if (f.tmdbId > 0) f.tmdbId else -((f.title ?: f.name).hashCode() and 0x7fffffff) - 1
    }
}
