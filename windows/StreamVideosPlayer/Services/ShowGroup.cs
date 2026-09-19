namespace StreamVideosPlayer.Services;

// Mirrors groupBySeries' {id, episodes} shape in main.go's frontend JS: the
// Series rail shows one tile per show (grouped by tmdb_id), not one per
// episode -- otherwise a library's worth of individual episodes (350 of
// them, in one real case) shows up as 350 separate tiles instead of
// however many actual shows they belong to. Exposes the same property
// names FileCardTemplate's binding paths already use (CardImageUrl/
// PosterUrl/DisplayTitle/Progress), so a dedicated ShowCardTemplate can
// stay a close copy of it rather than a divergent one-off.
public class ShowGroup
{
    public int TmdbId { get; }
    public List<FileEntry> Episodes { get; }

    public ShowGroup(int tmdbId, List<FileEntry> episodes)
    {
        TmdbId = tmdbId;
        Episodes = episodes.OrderBy(e => e.Season).ThenBy(e => e.Episode).ToList();
    }

    private FileEntry First => Episodes[0];

    // Matches renderShowCard's own choice: show.episodes[0]'s art/title
    // stand in for the whole show, not an aggregate.
    public string? CardImageUrl => First.CardImageUrl;
    public string? PosterUrl => First.PosterUrl;
    public string DisplayTitle => string.IsNullOrEmpty(First.Title) ? First.Name : First.Title;
    public int Year => First.Year;

    // Matches seriesCount(show) exactly: "N seasons" once more than one
    // season is present in this show's episodes, otherwise "N episode(s)".
    public string CountLabel
    {
        get
        {
            var seasons = Episodes.Select(e => e.Season).Where(s => s > 0).Distinct().Count();
            if (seasons > 1) return $"{seasons} seasons";
            return Episodes.Count == 1 ? "1 episode" : $"{Episodes.Count} episodes";
        }
    }

    // Matches renderShowCard's own "a show is in progress if any of its
    // episodes is" -- whichever episode was most recently updated, not
    // just the first one found with progress.
    public double Progress => Episodes
        .Where(e => e.Progress > 0)
        .OrderByDescending(e => e.ProgressUpdated)
        .Select(e => e.Progress)
        .DefaultIfEmpty(0)
        .First();
}
