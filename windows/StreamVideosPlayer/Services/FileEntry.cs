using System.Text.Json.Serialization;

namespace StreamVideosPlayer.Services;

// Mirrors main.go's fileEntry JSON (see handleFiles/GET /files), including
// the film/show detail-pane fields (cast, director, collection, ...) now
// that FilmDetailWindow/ShowDetailWindow exist to use them.
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

    [JsonPropertyName("tmdb_id")]
    public int TmdbId { get; set; }

    [JsonPropertyName("tmdb_backdrop")]
    public string? Backdrop { get; set; }

    [JsonPropertyName("episode_title")]
    public string? EpisodeTitle { get; set; }

    [JsonPropertyName("episode_still")]
    public string? EpisodeStill { get; set; }

    [JsonPropertyName("episode_overview")]
    public string? EpisodeOverview { get; set; }

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

    [JsonPropertyName("is_note")]
    public bool IsNote { get; set; }

    [JsonPropertyName("snippet")]
    public string? Snippet { get; set; }

    [JsonPropertyName("genres")]
    public List<string>? Genres { get; set; }

    // artist/album: parsed straight from a music video's filename (see
    // parseMusicVideoFilename in main.go), not looked up -- no iTunes/other
    // API call backs these. Was bound in MusicVideoCardTemplate with no
    // backing property here at all, so the artist line always rendered
    // blank; this is the actual fix, not just an addition.
    [JsonPropertyName("artist")]
    public string? Artist { get; set; }

    [JsonPropertyName("album")]
    public string? Album { get; set; }

    // Detail-pane-only fields (film/show detail windows): main.go's own
    // fileEntry doc comment used to say these were deliberately left off
    // this DTO "add them here if the native UI grows one" -- it has now.
    [JsonPropertyName("is_extra")]
    public bool IsExtra { get; set; }

    [JsonPropertyName("main_key")]
    public string? MainKey { get; set; }

    // The language actually embedded in this file's default audio track --
    // distinct from Language below (TMDB's listing for the production as a
    // whole), and the field that answers "what will I actually hear".
    [JsonPropertyName("audio_lang")]
    public string? AudioLang { get; set; }

    [JsonPropertyName("runtime")]
    public int Runtime { get; set; }

    [JsonPropertyName("director")]
    public string? Director { get; set; }

    [JsonPropertyName("cast")]
    public List<string>? Cast { get; set; }

    [JsonPropertyName("country")]
    public string? Country { get; set; }

    [JsonPropertyName("language")]
    public string? Language { get; set; }

    [JsonPropertyName("collection")]
    public TmdbCollection? Collection { get; set; }

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

    // Mirrors playbackProgress.Updated (see ProgressModels.cs) -- when this
    // position was last saved. Continue Watching orders by this (most
    // recently watched first, matching continueItems' own sort in main.go's
    // frontend JS), not by whatever order the catalog itself happens to
    // list files in.
    public long ProgressUpdated { get; set; }

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

    // Matches renderShowView's episode row title exactly: f.episode_title,
    // falling straight back to f.name (not Title) when there's no episode
    // title -- ShowDetailWindow's own episode rows use this.
    public string EpisodeRowTitle => string.IsNullOrEmpty(EpisodeTitle) ? Name : EpisodeTitle;

    // Matches fmtDur(f.duration) with a fallback to "{runtime}m" (TMDB's
    // minutes) when there's no probed duration yet, and nothing at all
    // rather than a guess -- same as renderShowView's own "time" variable.
    public string DurationLabel
    {
        get
        {
            if (Duration > 0)
            {
                var h = Duration / 3600;
                var m = Duration % 3600 / 60;
                var s = Duration % 60;
                return h > 0 ? $"{h}:{m:D2}:{s:D2}" : $"{m}:{s:D2}";
            }
            return Runtime > 0 ? $"{Runtime}m" : "";
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

    // Playable (see browserPlayableExts in main.go) means "a browser <video>
    // tag can decode this container" -- mp4/m4v/webm/mov only. That's a web
    // UI constraint, not a native-engine one: libVLC opens every extension
    // videoExts already scanned it as (mkv/avi/flv/wmv included), which is
    // the entire reason this app exists instead of just using the browser.
    // Trusting the server's browser-oriented flag here excluded almost the
    // whole local library (nearly every .mkv rip) -- a local entry just
    // needs to not still be mid-download. CDN entries are playable once HLS
    // or a direct-play remux exists.
    public bool IsPlayable => IsVideo && (IsLocal ? ProcState != "processing" : (!string.IsNullOrEmpty(HlsBase) || DirectPlay));

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

// Mirrors tmdbCollection in main.go: {id, name} for a movie's franchise
// grouping (e.g. "The Dark Knight" -> "The Dark Knight Collection").
public class TmdbCollection
{
    [JsonPropertyName("id")]
    public int Id { get; set; }

    [JsonPropertyName("name")]
    public string Name { get; set; } = "";
}
