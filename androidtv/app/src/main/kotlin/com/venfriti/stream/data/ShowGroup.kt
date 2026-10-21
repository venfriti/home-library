package com.venfriti.stream.data

// Mirrors groupBySeries' {id, episodes} shape in main.go's frontend JS (and
// ShowGroup.cs on the Windows client): the Series row is one tile per show,
// grouped by tmdb_id, not one per episode.
class ShowGroup(val tmdbId: Int, episodes: List<FileEntry>) {
    val episodes: List<FileEntry> = episodes.sortedWith(compareBy({ it.season }, { it.episode }))

    private val first: FileEntry get() = episodes[0]

    val cardImageUrl: String? get() = first.cardImageUrl
    val posterUrl: String? get() = first.posterUrl
    val displayTitle: String get() = first.title ?: first.name
    val year: Int get() = first.year

    // Matches seriesCount(show) in main.go exactly: "N seasons" once more
    // than one season is present, otherwise "N episode(s)".
    val countLabel: String
        get() {
            val seasons = episodes.mapNotNull { it.season.takeIf { s -> s > 0 } }.distinct().size
            return if (seasons > 1) "$seasons seasons"
            else if (episodes.size == 1) "1 episode" else "${episodes.size} episodes"
        }

    // Matches renderShowCard's own "a show is in progress if any of its
    // episodes is" -- whichever episode was most recently updated.
    val progress: Double
        get() = episodes.filter { it.progress > 0 }
            .maxByOrNull { it.progressUpdated }
            ?.progress ?: 0.0
}
