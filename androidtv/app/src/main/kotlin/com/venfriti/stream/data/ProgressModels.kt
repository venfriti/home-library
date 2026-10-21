package com.venfriti.stream.data

import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// Mirrors playbackProgress in main.go.
@Serializable
data class PlaybackProgress(
    val position: Double = 0.0,
    val duration: Double = 0.0,
    val updated: Long = 0,
)

// Mirrors the POST /api/progress request body handleProgress decodes.
@Serializable
data class ProgressUpdate(
    val key: String,
    val position: Double,
    val duration: Double,
)

// Mirrors GET /windows-app/version's shape -- the same version-check pattern
// applies here as data/androidtv_app_version.json once that route exists
// server-side (see the Windows client's own README for the precedent).
@Serializable
data class UpdateInfo(
    val version: String = "",
    val url: String = "",
    val notes: String = "",
)

// Mirrors trickplayInfo in main.go: tile N (0-based) covers N*interval
// seconds and lives in sheet N/(cols*rows)+1 at column/row N%(cols*rows).
@Serializable
data class TrickplayInfo(
    val ready: Boolean = false,
    val interval: Int = 10,
    @SerialName("tile_w") val tileW: Int = 0,
    @SerialName("tile_h") val tileH: Int = 0,
    val cols: Int = 0,
    val rows: Int = 0,
    val count: Int = 0,
    val sheets: Int = 0,
)
