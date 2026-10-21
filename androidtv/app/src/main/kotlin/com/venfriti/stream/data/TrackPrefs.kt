package com.venfriti.stream.data

import android.content.Context

// The last audio/subtitle choice made by hand in the player's Audio &
// subtitles sheet, applied to every video after it. Until something is
// picked, subtitles default to the first English track.
class TrackPrefs(context: Context) {
    private val prefs = context.applicationContext.getSharedPreferences("player_tracks", Context.MODE_PRIVATE)

    sealed interface Subtitles {
        data object Default : Subtitles
        data object Off : Subtitles
        data class Language(val code: String) : Subtitles
    }

    var subtitles: Subtitles
        get() = when (val v = prefs.getString(KEY_SUB, null)) {
            null -> Subtitles.Default
            OFF -> Subtitles.Off
            else -> Subtitles.Language(v)
        }
        set(value) {
            prefs.edit().apply {
                when (value) {
                    Subtitles.Default -> remove(KEY_SUB)
                    Subtitles.Off -> putString(KEY_SUB, OFF)
                    is Subtitles.Language -> putString(KEY_SUB, value.code)
                }
            }.apply()
        }

    // null until an audio track is picked by hand: the file's default plays.
    var audioLanguage: String?
        get() = prefs.getString(KEY_AUDIO, null)
        set(value) = prefs.edit().putString(KEY_AUDIO, value).apply()

    private companion object {
        const val KEY_SUB = "subtitles"
        const val KEY_AUDIO = "audio"
        const val OFF = "off"
    }
}
