package com.venfriti.stream.data

import kotlin.math.roundToInt
import kotlinx.serialization.SerialName
import kotlinx.serialization.Serializable

// Mirrors main.go's fileEntry JSON exactly the same way FileEntry.cs does on
// the Windows client -- same field set, same computed-property fallbacks
// (CardImageUrl/ThumbnailImageUrl/HeroArt/etc.), so both native clients stay
// in sync with the same server contract rather than drifting independently.
@Serializable
data class FileEntry(
    val key: String = "",
    val name: String = "",
    @SerialName("is_video") val isVideo: Boolean = false,
    @SerialName("thumb_url") var thumbUrl: String? = null,
    @SerialName("poster_url") val posterUrl: String? = null,
    val duration: Int = 0,
    @SerialName("hls_base") val hlsBase: String? = null,
    @SerialName("direct_play") val directPlay: Boolean = false,
    val title: String? = null,
    val year: Int = 0,
    @SerialName("media_type") val mediaType: String? = null,
    val season: Int = 0,
    val episode: Int = 0,
    @SerialName("subtitle_url") var subtitleUrl: String? = null,
    @SerialName("proc_state") val procState: String? = null,
    val rating: Double = 0.0,
    val overview: String? = null,
    @SerialName("tmdb_id") val tmdbId: Int = 0,
    @SerialName("tmdb_backdrop") val backdrop: String? = null,
    @SerialName("episode_title") val episodeTitle: String? = null,
    @SerialName("episode_still") val episodeStill: String? = null,
    @SerialName("episode_overview") val episodeOverview: String? = null,
    @SerialName("is_local") val isLocal: Boolean = false,
    val playable: Boolean = false,
    @SerialName("is_music_video") val isMusicVideo: Boolean = false,
    @SerialName("is_note") val isNote: Boolean = false,
    val snippet: String? = null,
    val genres: List<String>? = null,
    // artist/album: parsed straight from a music video's filename (see
    // parseMusicVideoFilename in main.go), not looked up -- no iTunes/other
    // API call backs these (see memory: no iTunes music lookup).
    val artist: String? = null,
    val album: String? = null,
    @SerialName("is_extra") val isExtra: Boolean = false,
    @SerialName("main_key") val mainKey: String? = null,
    // The language actually embedded in this file's default audio track --
    // distinct from `language` below (TMDB's listing for the production as
    // a whole), and the field that answers "what will I actually hear".
    @SerialName("audio_lang") val audioLang: String? = null,
    val runtime: Int = 0,
    val director: String? = null,
    val cast: List<String>? = null,
    val country: String? = null,
    val language: String? = null,
    val collection: TmdbCollection? = null,

    // Set once after GET /files, from a separate GET /api/progress?all=1
    // call -- not part of the /files JSON itself. See ServerClient.
    var progress: Double = 0.0,
    var positionSeconds: Double = 0.0,
    var durationSeconds: Double = 0.0,
    var progressUpdated: Long = 0,
) {
    // Prefer the TMDB poster (movies/TV); music videos and anything
    // unmatched only ever have a locally-extracted thumbnail. Used by the
    // tall 2:3 card (Films/Series rows).
    val cardImageUrl: String? get() = posterUrl ?: thumbUrl

    // Opposite priority: a real frame grabbed from the video itself, not a
    // poster. Used by the wide 16:9 card (Continue watching/Music
    // videos/Library), matching the web UI's own .cw-shot cards.
    val thumbnailImageUrl: String? get() = thumbUrl ?: posterUrl

    // Backdrop first, then the episode-specific still -- matches artFor(f,
    // true)/renderHero's own "art" fallback in main.go's frontend JS.
    val heroArt: String? get() = backdrop ?: episodeStill

    // Mirrors fmtLeft(p) in main.go's frontend JS exactly (including its
    // same rounding quirk at the top of a minute, e.g. 3599s -> "60m left"
    // rather than "1h 0m left") -- a faithful port, not a fix.
    val remainingLabel: String
        get() {
            if (durationSeconds <= 0) return ""
            val left = maxOf(0, (durationSeconds - positionSeconds).roundToInt())
            if (left == 0) return ""
            val h = left / 3600
            val m = ((left % 3600) / 60.0).roundToInt()
            return when {
                h > 0 -> "${h}h ${m}m left"
                m > 0 -> "${m}m left"
                else -> "less than a minute left"
            }
        }

    val episodeRowTitle: String get() = episodeTitle ?: name

    val durationLabel: String
        get() {
            if (duration > 0) {
                val h = duration / 3600
                val m = duration % 3600 / 60
                val s = duration % 60
                return if (h > 0) "$h:${"%02d".format(m)}:${"%02d".format(s)}" else "$m:${"%02d".format(s)}"
            }
            return if (runtime > 0) "${runtime}m" else ""
        }

    val continueWatchingLine: String
        get() {
            val base = title ?: name
            return if (mediaType == "tv" && season > 0 && episode > 0)
                "$base · S${"%02d".format(season)}E${"%02d".format(episode)}"
            else base
        }

    // Playable (see browserPlayableExts in main.go) means "a browser <video>
    // tag can decode this container" -- a web UI constraint, not a
    // native-engine one. Media3/ExoPlayer opens every extension videoExts
    // already scanned it as, same reasoning as IsPlayable on the Windows
    // client -- a local entry just needs to not still be mid-download.
    val isPlayable: Boolean
        get() = isVideo && if (isLocal) procState != "processing" else (!hlsBase.isNullOrEmpty() || directPlay)

    val displayTitle: String
        get() {
            val base = title ?: name
            if (mediaType == "tv" && season > 0 && episode > 0) return "$base — S${"%02d".format(season)}E${"%02d".format(episode)}"
            return if (year > 0) "$base ($year)" else base
        }

    val section: ContentSection
        get() = when {
            isMusicVideo -> ContentSection.MusicVideo
            mediaType == "tv" -> ContentSection.Series
            mediaType == "movie" -> ContentSection.Film
            else -> ContentSection.Library
        }
}

enum class ContentSection { Film, Series, MusicVideo, Library }

@Serializable
data class TmdbCollection(
    val id: Int = 0,
    val name: String = "",
)
