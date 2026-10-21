package com.venfriti.stream.data

import kotlinx.serialization.Serializable

// Mirrors the playlist struct in main.go (see GET/POST /playlists).
@Serializable
data class PlaylistDto(
    val id: String = "",
    val name: String = "",
    val items: MutableList<String> = mutableListOf(),
    val created: Long = 0,
    val updated: Long = 0,
)

// Body shape for POST /playlists -- action selects which fields matter (see
// handlePlaylists in main.go: create/add/remove/rename/reorder/delete).
@Serializable
data class PlaylistAction(
    val action: String,
    val id: String? = null,
    val name: String? = null,
    val key: String? = null,
)
