package com.venfriti.stream.data

import android.content.Context
import kotlinx.serialization.Serializable
import kotlinx.serialization.encodeToString
import kotlinx.serialization.json.Json

// Which list each music video was actually played from (a playlist, the
// music grid in its sort order, the library), so picking it again from
// Continue watching resumes in that same list rather than the Continue
// watching row. Only videos that really played are recorded, and only the
// most recent MaxVideos are kept.
class QueueMemory(context: Context) {
    private val prefs = context.applicationContext.getSharedPreferences("music_queues", Context.MODE_PRIVATE)
    private val json = Json { ignoreUnknownKeys = true }

    @Serializable
    private data class Store(
        // queue id -> the keys of that queue, in order
        val queues: Map<String, List<String>> = emptyMap(),
        // video key -> queue id it was last played from, oldest first
        val playedFrom: List<Pair<String, String>> = emptyList(),
    )

    private fun load(): Store =
        prefs.getString(KEY, null)?.let { runCatching { json.decodeFromString<Store>(it) }.getOrNull() } ?: Store()

    fun recordPlayed(videoKey: String, queue: List<String>) {
        val store = load()
        val id = queue.joinToString("\n").hashCode().toString()
        var playedFrom = (store.playedFrom.filter { it.first != videoKey } + (videoKey to id)).takeLast(MaxVideos)
        // At most MaxQueues distinct lists (the most recently used): a
        // re-sorted grid is a new list each time, and each can be ~100 keys.
        val live = playedFrom.asReversed().map { it.second }.distinct().take(MaxQueues).toSet()
        playedFrom = playedFrom.filter { it.second in live }
        val queues = (store.queues + (id to queue)).filterKeys { it in live }
        prefs.edit().putString(KEY, json.encodeToString(Store(queues, playedFrom))).apply()
    }

    fun queueFor(videoKey: String): List<String>? {
        val store = load()
        val id = store.playedFrom.lastOrNull { it.first == videoKey }?.second ?: return null
        return store.queues[id]
    }

    private companion object {
        const val KEY = "store"
        const val MaxVideos = 200
        const val MaxQueues = 20
    }
}
