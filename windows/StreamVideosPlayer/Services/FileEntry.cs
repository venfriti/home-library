using System.Text.Json.Serialization;

namespace StreamVideosPlayer.Services;

// Mirrors the subset of main.go's fileEntry JSON (see handleFiles/GET /files)
// that the player actually needs, including the poster/rating fields the
// card grid renders. Deeper detail-pane-only fields (cast, director, ...)
// are still left out -- add them here if the native UI grows one.
public class FileEntry
{
    [JsonPropertyName("key")]
    public string Key { get; set; } = "";

    [JsonPropertyName("name")]
    public string Name { get; set; } = "";

    [JsonPropertyName("is_video")]
    public bool IsVideo { get; set; }

    [JsonPropertyName("thumb_url")]
    public string? ThumbUrl { get; set; }

    [JsonPropertyName("poster_url")]
    public string? PosterUrl { get; set; }

    [JsonPropertyName("duration")]
    public int Duration { get; set; }

    [JsonPropertyName("hls_base")]
    public string? HlsBase { get; set; }

    [JsonPropertyName("direct_play")]
    public bool DirectPlay { get; set; }

    [JsonPropertyName("title")]
    public string? Title { get; set; }

    [JsonPropertyName("year")]
    public int Year { get; set; }

    [JsonPropertyName("media_type")]
    public string? MediaType { get; set; }

    [JsonPropertyName("season")]
    public int Season { get; set; }

    [JsonPropertyName("episode")]
    public int Episode { get; set; }

    [JsonPropertyName("subtitle_url")]
    public string? SubtitleUrl { get; set; }

    [JsonPropertyName("proc_state")]
    public string? ProcState { get; set; }

    [JsonPropertyName("rating")]
    public double Rating { get; set; }

    [JsonPropertyName("overview")]
    public string? Overview { get; set; }

    [JsonPropertyName("tmdb_backdrop")]
    public string? Backdrop { get; set; }

    [JsonPropertyName("episode_title")]
    public string? EpisodeTitle { get; set; }

    [JsonPropertyName("episode_still")]
    public string? EpisodeStill { get; set; }

    // GET /local/files sets these instead of hls_base/direct_play -- see
    // handleLocalFiles in main.go. Local entries never carry thumb_url from
    // the API at all (the web UI fetches /local/thumbnail on demand); the
    // loader that populates the local catalog fills ThumbUrl in for us with
    // that same endpoint (see ServerClient.GetLocalFilesAsync), so
    // CardImageUrl below doesn't need to know the difference.
    [JsonPropertyName("is_local")]
    public bool IsLocal { get; set; }

    [JsonPropertyName("playable")]
    public bool Playable { get; set; }

    [JsonPropertyName("is_music_video")]
    public bool IsMusicVideo { get; set; }

    // Prefer the TMDB poster (movies/TV); music videos and anything
    // unmatched only ever have a locally-extracted thumbnail. Used by the
    // tall 2:3 card (Films/Series sections).
    public string? CardImageUrl => string.IsNullOrEmpty(PosterUrl) ? ThumbUrl : PosterUrl;

    // Opposite priority: a real frame grabbed from the video itself, not a
    // poster. Used by the wide 16:9 card (Continue watching/Music
    // videos/Library), matching the web UI's own .cw-shot cards, which show
    // a still of what's actually playing rather than cover art.
    public string? ThumbnailImageUrl => string.IsNullOrEmpty(ThumbUrl) ? PosterUrl : ThumbUrl;

    // Backdrop first, then the episode-specific still -- matches artFor(f,
    // true)/renderHero's own "art" fallback in main.go's frontend JS. Only
    // used by the hero banner; the card templates above have their own
    // (different) priorities.
    public string? HeroArt => string.IsNullOrEmpty(Backdrop) ? EpisodeStill : Backdrop;

    // Set once after GET /files, from a separate GET /api/progress?all=1 call
    // (see ServerClient.GetAllProgressAsync) -- 0..1, drives the card's
    // progress sliver, mirroring .pc-prog on the web UI's cards. Not part of
    // the /files JSON itself, and never mutated after the card is bound, so
    // no change notification is needed.
    public double Progress { get; set; }

    // Raw seconds behind Progress above, set alongside it -- needed to render
    // "1h 32m left" (RemainingLabel below), which a 0..1 fraction alone can't
    // reconstruct without the real duration.
    public double PositionSeconds { get; set; }
    public double DurationSeconds { get; set; }

    // Mirrors fmtLeft(p) in main.go's frontend JS exactly (including its
    // same rounding quirk at the top of a minute, e.g. 3599s -> "60m left"
    // rather than "1h 0m left") -- this is a faithful port, not a fix.
    public string RemainingLabel
    {
        get
        {
            if (DurationSeconds <= 0) return "";
            var left = Math.Max(0, (int)Math.Round(DurationSeconds - PositionSeconds));
            if (left == 0) return "";
            var h = left / 3600;
            var m = (int)Math.Round((left % 3600) / 60.0);
            if (h > 0) return $"{h}h {m}m left";
            if (m > 0) return $"{m}m left";
            return "less than a minute left";
        }
    }

    // Matches renderContinueCard's own "line" variable: title alone, or
    // "title · SxxExx" for a TV episode.
    public string ContinueWatchingLine
    {
        get
        {
            var baseTitle = string.IsNullOrEmpty(Title) ? Name : Title;
            if (MediaType == "tv" && Season > 0 && Episode > 0)
                return $"{baseTitle} · S{Season:D2}E{Episode:D2}";
            return baseTitle;
        }
    }

    // Local entries carry their own Playable flag (a container/codec the
    // native engine -- or a browser -- can open directly, see
    // browserPlayableExts in main.go); CDN entries are playable once HLS or
    // a direct-play remux exists.
    public bool IsPlayable => IsVideo && (IsLocal ? Playable : (!string.IsNullOrEmpty(HlsBase) || DirectPlay));

    public string DisplayTitle
    {
        get
        {
            var baseTitle = string.IsNullOrEmpty(Title) ? Name : Title;
            if (MediaType == "tv" && Season > 0 && Episode > 0)
                return $"{baseTitle} — S{Season:D2}E{Episode:D2}";
            return Year > 0 ? $"{baseTitle} ({Year})" : baseTitle;
        }
    }

    // Mirrors the web UI's own wall sections (see buildWall/railAnchors in
    // main.go's frontend JS): every playable item lands in exactly one of
    // these, with "Continue watching" layered on top as a cross-cutting
    // filter (Progress > 0) rather than a fifth mutually-exclusive bucket.
    public ContentSection Section
    {
        get
        {
            if (IsMusicVideo) return ContentSection.MusicVideo;
            if (MediaType == "tv") return ContentSection.Series;
            if (MediaType == "movie") return ContentSection.Film;
            return ContentSection.Library;
        }
    }
}

public enum ContentSection
{
    Film,
    Series,
    MusicVideo,
    // The web UI calls this "Other" (TMDB's catch-all for unmatched files);
    // renamed here since "Library" reads clearer to someone browsing their
    // own collection rather than "whatever TMDB couldn't place."
    Library,
}
