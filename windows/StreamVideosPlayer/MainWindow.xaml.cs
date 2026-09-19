using System.Collections.ObjectModel;
using System.Diagnostics;
using System.Reflection;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Controls.Primitives;
using System.Windows.Input;
using System.Windows.Threading;
using Geometry = System.Windows.Media.Geometry;
using LibVLCSharp.Shared;
using LibVLCSharp.Shared.Structures;
using StreamVideosPlayer.Services;

namespace StreamVideosPlayer;

public partial class MainWindow : Window
{
    private readonly ServerClient _client;
    private readonly LibVLC _libVlc;
    private readonly MediaPlayer _mediaPlayer;
    private readonly DispatcherTimer _uiTimer;
    private readonly DispatcherTimer _progressTimer;
    private readonly DispatcherTimer _controlsIdleTimer;
    private readonly ObservableCollection<FileEntry> _queue = new();

    // Section id -> the StackPanel holding that section's heading + card
    // list, for scroll-spy position lookups (MainScroll_ScrollChanged).
    private readonly Dictionary<string, FrameworkElement> _sectionElements = new();
    // Section id -> its pill in SectionTagsPanel, for active-state styling.
    private readonly Dictionary<string, Button> _sectionTagButtons = new();
    private string? _activeSectionId;
    // True until the first rail-head is built in a given RebuildSections
    // pass -- matches .rail-head:first-child's smaller top margin (the one
    // right after the hero banner needs less breathing room than the rest).
    private bool _firstRailHead;

    private List<FileEntry> _allFiles = new();
    private List<PlaylistDto> _playlists = new();
    private FileEntry? _current;
    private bool _isMini;
    private bool _isDraggingSlider;
    private bool _useLocalSource;
    private UpdateInfo? _pendingUpdate;
    private readonly AppSettings _settings;

    // Ambient tint (see #tint/bindTint/dominantColour in main.go): the wall
    // wash resets to this -- the hero's own sampled colour -- whenever
    // nothing is hovered, rather than back to the plain brand gold every
    // time. Brand gold (matching CSS's own --tint default) until the hero
    // actually renders and gets sampled.
    private System.Windows.Media.Color _restingTintColor =
        (System.Windows.Media.Color)System.Windows.Media.ColorConverter.ConvertFromString("#9A7D4A")!;
    private FileEntry? _hoveredTintEntry;

    // Hero rotation (see HERO_WINDOW_MS/heroPick/scheduleHeroRoll in
    // main.go's frontend JS): the banner holds for a fixed window, then
    // rerolls to a new (deterministically-picked, not just random) choice
    // without a full page reload.
    private const long HeroWindowMs = 6L * 60 * 60 * 1000;
    private DispatcherTimer? _heroRollTimer;
    private Border? _heroElement;
    private readonly Random _heroRandom = new();

    public MainWindow(ServerClient client, StoredCredentials creds)
    {
        InitializeComponent();
        Title = $"Stream — {creds.ServerUrl}";
        _client = client;
        QueueList.ItemsSource = _queue;

        // Remembers Online vs Local across launches instead of always
        // reopening to Online.
        _settings = AppSettings.Load();
        _useLocalSource = _settings.UseLocalSource;
        SourceToggleText.Text = _useLocalSource ? "LOCAL" : "ONLINE";

        _libVlc = new LibVLC();
        _mediaPlayer = new MediaPlayer(_libVlc);
        Player.MediaPlayer = _mediaPlayer;
        // Set after construction, not via XAML Value=, so VolumeSlider's
        // ValueChanged handler never fires before _mediaPlayer exists.
        VolumeSlider.Value = 100;

        SpeedCombo.ItemsSource = new[] { 0.5, 0.75, 1.0, 1.25, 1.5, 1.75, 2.0 };
        SpeedCombo.SelectedItem = 1.0;
        AspectCombo.ItemsSource = new[] { "Default", "16:9", "4:3", "1:1", "16:10", "21:9" };
        AspectCombo.SelectedItem = "Default";

        _mediaPlayer.Playing += (_, _) => Dispatcher.Invoke(() =>
        {
            PlayPauseIcon.Data = (Geometry)FindResource("IconPause");
            // Track lists (which languages/subtitles actually exist) are
            // only populated once libVLC has parsed the media, which isn't
            // done until playback has actually started.
            RefreshTrackMenus();
        });
        _mediaPlayer.Paused += (_, _) => Dispatcher.Invoke(() =>
            PlayPauseIcon.Data = (Geometry)FindResource("IconPlay"));
        // BeginInvoke (not Invoke): this fires on libVLC's own internal
        // thread, and calling back into Play() synchronously from inside
        // that callback is a known source of deadlocks/misbehavior. Queuing
        // it to run slightly later on the UI thread avoids that.
        _mediaPlayer.EndReached += (_, _) => Dispatcher.BeginInvoke(new Action(async () =>
        {
            PlayPauseIcon.Data = (Geometry)FindResource("IconPlay");
            if (_queue.Count == 0) return;
            var next = _queue[0];
            _queue.RemoveAt(0);
            await PlayAsync(next);
        }));

        _uiTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(500) };
        _uiTimer.Tick += UiTimer_Tick;
        _uiTimer.Start();

        // Mirrors the web player's periodic save to /api/progress in main.go
        // (see resumePlayback in the embedded frontend JS), so resume position
        // stays in sync between the web UI and this app.
        _progressTimer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(10) };
        _progressTimer.Tick += async (_, _) => await SaveProgressAsync();
        _progressTimer.Start();

        // Matches showPlayerChrome/idleFadeTimer in main.go's frontend JS:
        // 3 seconds of no mouse movement over the player fades the chrome
        // out, any movement brings it back. The web gets the actual video
        // controls hidden for free from the browser's native <video
        // controls> idle behavior; this app's whole control bar is
        // hand-built WPF, so it needs the same idle timer done by hand.
        _controlsIdleTimer = new DispatcherTimer { Interval = TimeSpan.FromSeconds(3) };
        _controlsIdleTimer.Tick += (_, _) =>
        {
            _controlsIdleTimer.Stop();
            SetPlayerControlsVisible(false);
        };

        Closing += async (_, _) =>
        {
            _uiTimer.Stop();
            _progressTimer.Stop();
            _controlsIdleTimer.Stop();
            _heroRollTimer?.Stop();
            await SaveProgressAsync();
            _mediaPlayer.Dispose();
            _libVlc.Dispose();
        };

        _ = LoadLibraryAsync();
        _ = LoadPlaylistsAsync();
        _ = CheckForUpdateAsync();
    }

    // ── Title bar ────────────────────────────────────────────────────────

    private void Minimize_Click(object sender, RoutedEventArgs e) => WindowState = WindowState.Minimized;

    private void MaximizeRestore_Click(object sender, RoutedEventArgs e)
    {
        WindowState = WindowState == WindowState.Maximized ? WindowState.Normal : WindowState.Maximized;
        MaximizeRestoreButton.Content = WindowState == WindowState.Maximized ? "" : "";
    }

    private void CloseWindow_Click(object sender, RoutedEventArgs e) => Close();

    // ── Update banner ────────────────────────────────────────────────────

    private async Task CheckForUpdateAsync()
    {
        var info = await _client.GetLatestVersionAsync();
        if (info is null || string.IsNullOrEmpty(info.Url)) return;
        if (!Version.TryParse(info.Version, out var latest)) return;

        var current = Assembly.GetExecutingAssembly().GetName().Version ?? new Version(0, 0, 0, 0);
        if (latest <= current) return;

        _pendingUpdate = info;
        UpdateBannerText.Text = string.IsNullOrEmpty(info.Notes)
            ? $"Update available: v{info.Version}"
            : $"Update available: v{info.Version} — {info.Notes}";
        UpdateBanner.Visibility = Visibility.Visible;
    }

    private void DownloadUpdate_Click(object sender, RoutedEventArgs e)
    {
        if (_pendingUpdate is null) return;
        Process.Start(new ProcessStartInfo(_pendingUpdate.Url) { UseShellExecute = true });
    }

    private void DismissUpdate_Click(object sender, RoutedEventArgs e) => UpdateBanner.Visibility = Visibility.Collapsed;

    // ── Source toggle (Online/Local) ────────────────────────────────────

    private void SourceToggle_Click(object sender, RoutedEventArgs e)
    {
        _useLocalSource = !_useLocalSource;
        SourceToggleText.Text = _useLocalSource ? "LOCAL" : "ONLINE";
        _settings.UseLocalSource = _useLocalSource;
        _settings.Save();
        _ = LoadLibraryAsync();
    }

    private void SwitchToOnlineFromBanner_Click(object sender, RoutedEventArgs e)
    {
        _useLocalSource = false;
        SourceToggleText.Text = "ONLINE";
        _settings.UseLocalSource = false;
        _settings.Save();
        LocalFailureBanner.Visibility = Visibility.Collapsed;
        _ = LoadLibraryAsync();
    }

    // ── Search overlay ───────────────────────────────────────────────────

    private void ToggleSearchOverlay_Click(object sender, RoutedEventArgs e)
    {
        var opening = SearchOverlay.Visibility != Visibility.Visible;
        SearchOverlay.Visibility = opening ? Visibility.Visible : Visibility.Collapsed;
        if (opening) SearchOverlayBox.Focus();
    }

    private void SearchBox_TextChanged(object sender, TextChangedEventArgs e) => RebuildSections();

    // ── Queue drawer ─────────────────────────────────────────────────────

    private void ToggleQueueDrawer_Click(object sender, RoutedEventArgs e) =>
        QueueDrawer.Visibility = QueueDrawer.Visibility == Visibility.Visible
            ? Visibility.Collapsed
            : Visibility.Visible;

    private void ClearQueue_Click(object sender, RoutedEventArgs e) => _queue.Clear();

    private async void QueueList_MouseDoubleClick(object sender, MouseButtonEventArgs e)
    {
        if (QueueList.SelectedItem is not FileEntry entry) return;
        _queue.Remove(entry);
        await PlayAsync(entry);
    }

    // ── Library loading & sections ──────────────────────────────────────

    private async Task LoadLibraryAsync()
    {
        try
        {
            var files = _useLocalSource ? await _client.GetLocalFilesAsync() : await _client.GetFilesAsync();
            LocalFailureBanner.Visibility = Visibility.Collapsed;
            _allFiles = files.Where(f => f.IsPlayable).ToList();

            // Best-effort: a failed progress fetch just means no cards show a
            // progress sliver, not worth failing the whole library load over.
            try
            {
                var progress = await _client.GetAllProgressAsync();
                foreach (var f in _allFiles)
                {
                    if (progress.TryGetValue(f.Key, out var p) && p.Duration > 0)
                    {
                        f.Progress = Math.Clamp(p.Position / p.Duration, 0, 1);
                        f.PositionSeconds = p.Position;
                        f.DurationSeconds = p.Duration;
                        f.ProgressUpdated = p.Updated;
                    }
                }
            }
            catch { /* see above */ }

            RebuildSections();
        }
        catch (Exception ex)
        {
            if (_useLocalSource)
            {
                // Typically means this device isn't on the same network as
                // the server -- /local/files is LAN-only (see localOnly in
                // main.go). Keep whatever was on screen before (don't clear
                // _allFiles) and surface a specific, actionable message
                // instead of a generic error popup.
                LocalFailureBanner.Visibility = Visibility.Visible;
            }
            else
            {
                MessageBox.Show($"Couldn't load the library: {ex.Message}", "Stream",
                    MessageBoxButton.OK, MessageBoxImage.Warning);
            }
        }
    }

    // Rebuilds the whole scrollable wall from _allFiles: a hero banner, then
    // one section per category with a matching jump-tag, mirroring
    // buildWall/railAnchors in main.go's frontend JS (a section with zero
    // matches just doesn't appear, same as there). Called on load, on search
    // text changes, and after a source switch.
    private void RebuildSections()
    {
        SectionsPanel.Children.Clear();
        SectionTagsPanel.Children.Clear();
        _sectionElements.Clear();
        _sectionTagButtons.Clear();
        _activeSectionId = null;
        _firstRailHead = true;

        var term = (SearchOverlayBox.Text ?? "").Trim();
        List<FileEntry> Filter(IEnumerable<FileEntry> src) => string.IsNullOrEmpty(term)
            ? src.ToList()
            : src.Where(f => f.DisplayTitle.Contains(term, StringComparison.OrdinalIgnoreCase)).ToList();

        var continueWatching = ContinueWatchingItems(Filter(_allFiles.Where(f => f.Progress > 0)));
        // Hero picking uses the unfiltered library (matching heroCandidates
        // taking the full file list in main.go, not whatever a search term
        // narrows the wall down to) -- a hero disappearing or jumping around
        // while typing a search would be a worse experience than just
        // holding still.
        _heroElement = AddHeroBanner(PickHero());
        if (_heroElement is not null) SectionsPanel.Children.Add(_heroElement);
        ScheduleHeroRoll();
        AddFileSection("rail-continue-watching", "Continue watching", continueWatching, "ContinueWatchingCardListBox");
        AddFileSection("rail-films", "Films", Filter(_allFiles.Where(f => f.Section == ContentSection.Film)), "CardListBox");
        AddSeriesSection(Filter(_allFiles.Where(f => f.Section == ContentSection.Series)));
        AddPlaylistsSection();
        AddFileSection("rail-music", "Music videos", Filter(_allFiles.Where(f => f.Section == ContentSection.MusicVideo)), "MusicVideoCardListBox");
        // Library mirrors #other-grid on the web -- the same 2:3 poster grid
        // as Films/Series, not a wide 16:9 one; unmatched files just get
        // whatever art they have cropped into that shape, same as there.
        AddFileSection("rail-library", "Library", Filter(_allFiles.Where(f => f.Section == ContentSection.Library)), "CardListBox");

        if (_sectionTagButtons.Count > 0)
            SetActiveSectionTag(_sectionTagButtons.Keys.First());
    }

    // Mirrors continueItems() in main.go's frontend JS exactly: sorted by
    // recency (most recently saved progress first, not whatever order the
    // catalog lists files in), with music videos capped at 4 anywhere in the
    // row -- applied here, before the row's own 8-item limit, so a movie or
    // episode just past a 5th music video gets pulled up into contention
    // rather than losing its slot to it. A binge of short music videos would
    // otherwise crowd out every slot in the row that actually exists to
    // surface progress on shows and movies.
    private static List<FileEntry> ContinueWatchingItems(IEnumerable<FileEntry> src)
    {
        var sorted = src.OrderByDescending(f => f.ProgressUpdated);
        var musicSeen = 0;
        var result = new List<FileEntry>();
        foreach (var f in sorted)
        {
            if (f.IsMusicVideo)
            {
                musicSeen++;
                if (musicSeen > 4) continue;
            }
            result.Add(f);
            if (result.Count == 8) break;
        }
        return result;
    }

    // Matches heroCandidates/heroPick/pickHeroFilm in main.go's frontend JS:
    // prefer a movie/show from Continue Watching (stable within a 6-hour
    // window, scrambled between windows, never a music video -- the banner
    // is for something part-way through, not a 3-minute clip), falling back
    // to a random movie with usable art, then anything with usable art at
    // all, when there's nothing to resume.
    private FileEntry? PickHero()
    {
        var continueWatchingUnfiltered = ContinueWatchingItems(_allFiles.Where(f => f.Progress > 0));
        var candidates = continueWatchingUnfiltered
            .Where(f => (f.MediaType == "movie" || f.MediaType == "tv") && !string.IsNullOrEmpty(f.HeroArt))
            .Take(5)
            .ToList();
        if (candidates.Count > 0)
        {
            var windowIdx = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds() / HeroWindowMs;
            var picked = HeroPick(candidates, windowIdx);
            if (picked is not null) return picked;
        }

        // pickHeroFilm's own "standalone" pool: not a tv item grouped into a
        // show (see groupBySeries -- tv with a real tmdb_id).
        var standalone = _allFiles.Where(f => !(f.MediaType == "tv" && f.TmdbId > 0)).ToList();
        var pool = standalone.Where(f => f.MediaType == "movie" && !string.IsNullOrEmpty(f.HeroArt)).ToList();
        if (pool.Count == 0) pool = standalone.Where(f => !string.IsNullOrEmpty(f.HeroArt)).ToList();
        return pool.Count > 0 ? pool[_heroRandom.Next(pool.Count)] : null;
    }

    // A faithful-in-spirit, not bit-identical, port of heroPick's xorshift-
    // style mix: JS fakes 32-bit integer ops via bitwise operators on
    // doubles, C# just uses real unsigned 32-bit arithmetic -- same
    // multiplier constants, same "stable within a window, scrambled between
    // windows" property, different exact numbers.
    private static FileEntry? HeroPick(List<FileEntry> candidates, long windowIdx)
    {
        if (candidates.Count == 0) return null;
        unchecked
        {
            var h = (uint)((windowIdx + 1) * 2654435761L);
            h ^= h >> 13;
            h *= 1274126177u;
            h ^= h >> 16;
            return candidates[(int)(h % (uint)candidates.Count)];
        }
    }

    // Matches scheduleHeroRoll: fires exactly at the next window boundary
    // (not just "every 6 hours" from whenever the app happened to launch),
    // so a screen left running lines up with the same reroll moment the web
    // would.
    private void ScheduleHeroRoll()
    {
        _heroRollTimer?.Stop();
        var now = DateTimeOffset.UtcNow.ToUnixTimeMilliseconds();
        var wait = HeroWindowMs - (now % HeroWindowMs) + 1000;
        _heroRollTimer = new DispatcherTimer { Interval = TimeSpan.FromMilliseconds(wait) };
        _heroRollTimer.Tick += (_, _) =>
        {
            _heroRollTimer!.Stop();
            RerollHero();
        };
        _heroRollTimer.Start();
    }

    private void RerollHero()
    {
        if (_heroElement is not null) SectionsPanel.Children.Remove(_heroElement);
        _heroElement = AddHeroBanner(PickHero());
        if (_heroElement is not null) SectionsPanel.Children.Insert(0, _heroElement);
        ScheduleHeroRoll();
    }

    // Big backdrop banner at the top of the page, matching the web's actual
    // .hero/.hero-scrim/.hero-in/renderHero() exactly (verified against
    // main.go's own CSS and frontend JS, not built from memory): a two-layer
    // gradient scrim (left-to-right, layered with bottom-to-top), a gold
    // mono kicker line, a serif title, a 2-line-clamped description, and a
    // solid-gold play/resume button -- not the plain single-gradient banner
    // this used to be.
    private Border? AddHeroBanner(FileEntry? hero)
    {
        if (hero is null) return null;
        var imageUrl = hero.HeroArt;
        if (string.IsNullOrEmpty(imageUrl)) return null;

        // tintDefault in main.go's own updateHero: the wall tint settles
        // back to the hero's own colour on mouseleave, not the plain brand
        // gold, once it's actually available.
        _ = SampleAndSetRestingTintAsync(imageUrl);

        var resuming = hero.Progress > 0 && !string.IsNullOrEmpty(hero.RemainingLabel);

        var container = new Border
        {
            Height = 340,
            Margin = new Thickness(0, 0, 0, 4),
            CornerRadius = new CornerRadius(8),
            ClipToBounds = true,
            Background = (System.Windows.Media.Brush)FindResource("Bg2Brush"),
        };
        var grid = new Grid();
        container.Child = grid;

        grid.Children.Add(new Image
        {
            Source = new System.Windows.Media.Imaging.BitmapImage(new Uri(imageUrl)),
            Stretch = System.Windows.Media.Stretch.UniformToFill,
        });

        // linear-gradient(to right, rgba(10,10,10,.95) 0%, rgba(10,10,10,.72)
        // 42%, rgba(10,10,10,.15) 82%) -- puts the darkest end under the text.
        grid.Children.Add(new Border
        {
            Background = new System.Windows.Media.LinearGradientBrush(new System.Windows.Media.GradientStopCollection
            {
                new System.Windows.Media.GradientStop(System.Windows.Media.Color.FromArgb(242, 10, 10, 10), 0),
                new System.Windows.Media.GradientStop(System.Windows.Media.Color.FromArgb(184, 10, 10, 10), 0.42),
                new System.Windows.Media.GradientStop(System.Windows.Media.Color.FromArgb(38, 10, 10, 10), 0.82),
            }, new Point(0, 0.5), new Point(1, 0.5)),
        });
        // linear-gradient(to top, rgba(10,10,10,.9), transparent 55%) --
        // keeps the bottom edge dark regardless of the image underneath it.
        grid.Children.Add(new Border
        {
            Background = new System.Windows.Media.LinearGradientBrush(new System.Windows.Media.GradientStopCollection
            {
                new System.Windows.Media.GradientStop(System.Windows.Media.Color.FromArgb(230, 10, 10, 10), 0),
                new System.Windows.Media.GradientStop(System.Windows.Media.Colors.Transparent, 0.55),
            }, new Point(0, 1), new Point(0, 0)),
        });

        var info = new StackPanel
        {
            VerticalAlignment = VerticalAlignment.Bottom,
            HorizontalAlignment = HorizontalAlignment.Left,
            MaxWidth = 560,
            Margin = new Thickness(26, 0, 26, 24),
        };
        info.Children.Add(BuildHeroKicker(hero, resuming));
        info.Children.Add(new TextBlock
        {
            Text = string.IsNullOrEmpty(hero.Title) ? hero.Name : hero.Title,
            Foreground = System.Windows.Media.Brushes.White,
            FontFamily = (System.Windows.Media.FontFamily)FindResource("SerifFont"),
            FontWeight = FontWeights.Bold,
            FontSize = 30,
            TextWrapping = TextWrapping.Wrap,
            Margin = new Thickness(0, 0, 0, 8),
        });

        var sub = (hero.MediaType == "tv" && !string.IsNullOrEmpty(hero.EpisodeTitle)) ? hero.EpisodeTitle : hero.Overview;
        if (!string.IsNullOrEmpty(sub))
        {
            info.Children.Add(new TextBlock
            {
                Text = sub,
                Foreground = (System.Windows.Media.Brush)new System.Windows.Media.BrushConverter().ConvertFromString("#CFC9BF")!,
                FontFamily = (System.Windows.Media.FontFamily)FindResource("SansFont"),
                FontSize = 13,
                LineHeight = 21,
                TextWrapping = TextWrapping.Wrap,
                MaxHeight = 42,
                TextTrimming = TextTrimming.CharacterEllipsis,
                Margin = new Thickness(0, 0, 0, 14),
            });
        }

        var playButton = new Button
        {
            Style = (Style)FindResource("HeroPlayButton"),
            Content = resuming ? "▶  RESUME" : "▶  PLAY",
            HorizontalAlignment = HorizontalAlignment.Left,
        };
        playButton.Click += async (_, _) => await PlayAsync(hero);
        info.Children.Add(playButton);

        grid.Children.Add(info);
        return container;
    }

    // Matches renderHero's own kicker composition: "Resume · 1h 32m left"
    // while resuming, else "Film/Series · year · ★rating" -- the web relies
    // on CSS text-transform:uppercase for the visual casing, which WPF has
    // no equivalent of, so it's applied to the string directly here instead.
    private TextBlock BuildHeroKicker(FileEntry hero, bool resuming)
    {
        string text;
        if (resuming)
        {
            text = $"Resume · {hero.RemainingLabel}";
        }
        else
        {
            var parts = new List<string> { hero.MediaType == "tv" ? "Series" : "Film" };
            if (hero.Year > 0) parts.Add(hero.Year.ToString());
            if (hero.Rating > 0) parts.Add($"★ {hero.Rating:0.0}");
            text = string.Join(" · ", parts);
        }
        return new TextBlock
        {
            Text = text.ToUpperInvariant(),
            Foreground = (System.Windows.Media.Brush)FindResource("GoldBrush"),
            FontFamily = (System.Windows.Media.FontFamily)FindResource("MonoFont"),
            FontSize = 11,
            Margin = new Thickness(0, 0, 0, 7),
        };
    }

    // Matches .rail-head/.rh-t/.rh-c/.rh-l: a serif-bold title, a muted mono
    // count, and a 1px rule filling the rest of the row -- replacing the
    // plain "Label (count)" TextBlock this used to be, which read as one
    // more line of body text rather than a section break.
    private FrameworkElement BuildRailHead(string label, int count)
    {
        var topMargin = _firstRailHead ? 3 : 30;
        _firstRailHead = false;

        var grid = new Grid { Margin = new Thickness(0, topMargin, 0, 2) };
        grid.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
        grid.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
        grid.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(1, GridUnitType.Star) });

        var title = new TextBlock
        {
            Text = label,
            Foreground = (System.Windows.Media.Brush)FindResource("TextBrush"),
            FontFamily = (System.Windows.Media.FontFamily)FindResource("SerifFont"),
            FontWeight = FontWeights.Bold,
            FontSize = 16,
            VerticalAlignment = VerticalAlignment.Bottom,
        };
        Grid.SetColumn(title, 0);

        var countText = new TextBlock
        {
            Text = count.ToString(),
            Foreground = (System.Windows.Media.Brush)FindResource("MutedBrush"),
            FontFamily = (System.Windows.Media.FontFamily)FindResource("MonoFont"),
            FontSize = 11,
            Margin = new Thickness(11, 0, 11, 2),
            VerticalAlignment = VerticalAlignment.Bottom,
        };
        Grid.SetColumn(countText, 1);

        var rule = new Border
        {
            Height = 1,
            Background = (System.Windows.Media.Brush)FindResource("BorderBrush2"),
            VerticalAlignment = VerticalAlignment.Center,
        };
        Grid.SetColumn(rule, 2);

        grid.Children.Add(title);
        grid.Children.Add(countText);
        grid.Children.Add(rule);
        return grid;
    }

    private void AddFileSection(string id, string label, List<FileEntry> items, string listStyleKey)
    {
        if (items.Count == 0) return;

        var panel = new StackPanel();
        panel.Children.Add(BuildRailHead(label, items.Count));

        var listBox = new ListBox
        {
            Style = (Style)FindResource(listStyleKey),
            ItemsSource = items,
        };
        // Every real card on the web opens on a single click (renderFileCard/
        // renderContinueCard/renderMusicCard all wire a plain 'click'
        // listener, not a double-click) -- MouseDoubleClick here was a WPF
        // ListBox convention nobody had actually checked against the site.
        // The overflow button is the one exception: AttachCardMoreMenu below
        // already marks that click Handled, but Preview tunnels down before
        // that bubble-phase handling runs, so it still needs its own
        // exclusion here to avoid also starting playback.
        //
        // Films are a second exception: openFile in main.go only plays a
        // movie directly when the caller explicitly passes skipDetailCheck
        // (Continue Watching, the hero's Resume button, a context menu's
        // Play) -- a plain click on a Films-rail card opens the detail page
        // instead. Every other section here (Continue Watching/Music/
        // Library) genuinely does play on a plain click on the real site.
        var isFilmsRail = id == "rail-films";
        listBox.PreviewMouseLeftButtonUp += async (_, e) =>
        {
            var source = e.OriginalSource as DependencyObject;
            if (FindAncestor<Button>(source) is { Name: "CardMoreButton" }) return;
            var item = FindAncestor<ListBoxItem>(source);
            if (item?.DataContext is not FileEntry entry) return;
            if (isFilmsRail) OpenFilmDetail(entry);
            else await PlayAsync(entry);
        };
        AttachHoverTint(listBox);
        AttachCardMoreMenu(listBox);
        AttachWheelForwarding(listBox);
        AttachDragSource(listBox);
        panel.Children.Add(listBox);

        SectionsPanel.Children.Add(panel);
        _sectionElements[id] = panel;
        AddSectionTag(id, label, items.Count);
    }

    // Mirrors groupBySeries in main.go's frontend JS: the Series rail is one
    // tile per show, not one per episode -- an unmatched episode (no
    // tmdb_id) still gets its own one-episode "show" tile rather than
    // silently disappearing, since there's nothing else to group it under.
    private void AddSeriesSection(List<FileEntry> episodes)
    {
        if (episodes.Count == 0) return;

        var shows = episodes
            .GroupBy(f => f.TmdbId > 0 ? f.TmdbId : -f.GetHashCode())
            .Select(g => new ShowGroup(g.Key, g.ToList()))
            .OrderBy(s => s.DisplayTitle, StringComparer.OrdinalIgnoreCase)
            .ToList();

        var panel = new StackPanel();
        panel.Children.Add(BuildRailHead("Series", shows.Count));

        var listBox = new ListBox { Style = (Style)FindResource("ShowCardListBox"), ItemsSource = shows };
        listBox.PreviewMouseLeftButtonUp += (_, e) =>
        {
            var item = FindAncestor<ListBoxItem>(e.OriginalSource as DependencyObject);
            if (item?.DataContext is ShowGroup show) OpenShowDetail(show);
        };
        AttachWheelForwarding(listBox);
        panel.Children.Add(listBox);

        SectionsPanel.Children.Add(panel);
        _sectionElements["rail-series"] = panel;
        AddSectionTag("rail-series", "Series", shows.Count);
    }

    private void OpenShowDetail(ShowGroup show)
    {
        var win = new ShowDetailWindow(show, PlayAsync) { Owner = this };
        win.ShowDialog();
    }

    private void OpenFilmDetail(FileEntry film)
    {
        var extras = _allFiles.Where(f => f.IsExtra && f.MainKey == film.Key).ToList();
        var win = new FilmDetailWindow(film, extras, PlayAsync) { Owner = this };
        win.ShowDialog();
    }

    private void AddPlaylistsSection()
    {
        // The web UI always shows a Playlists rail regardless of count (see
        // main.go's railAnchors.push for it, unconditional unlike the
        // others) -- matched here rather than hiding it when empty.
        var panel = new StackPanel();
        panel.Children.Add(BuildRailHead("Playlists", _playlists.Count));

        var listBox = new ListBox { Style = (Style)FindResource("PlaylistCardListBox"), ItemsSource = _playlists };
        // A single click opens a playlist (there's no "select vs. play"
        // ambiguity for a playlist tile the way there is for a file card),
        // via direct hit-testing rather than relying on ListBox.SelectedItem
        // timing -- simpler and more reliably "just works" than a
        // double-click, which was reported as sometimes not opening at all.
        // The card's own play button (.pl-play on the web) is the one
        // exception -- it starts playback directly instead of opening the
        // detail window, matching startPlay's stopPropagation there.
        listBox.PreviewMouseLeftButtonUp += (_, e) =>
        {
            var source = e.OriginalSource as DependencyObject;
            var item = FindAncestor<ListBoxItem>(source);
            if (item?.DataContext is not PlaylistDto playlist) return;

            if (FindAncestor<Button>(source) is { Name: "PlayButton" })
            {
                e.Handled = true;
                _ = PlayFirstInPlaylistAsync(playlist);
                return;
            }
            OpenPlaylistDetail(playlist);
        };
        AttachWheelForwarding(listBox);

        // Matches wirePlaylistDropTarget's drop side: a file card dragged
        // from AttachDragSource lands here, resolved to whichever playlist
        // tile is actually under the cursor.
        listBox.AllowDrop = true;
        listBox.DragOver += (_, e) =>
        {
            e.Effects = e.Data.GetDataPresent(DragFileKeyFormat) ? DragDropEffects.Copy : DragDropEffects.None;
            e.Handled = true;
        };
        listBox.Drop += async (_, e) =>
        {
            if (!e.Data.GetDataPresent(DragFileKeyFormat)) return;
            var item = FindAncestor<ListBoxItem>(e.OriginalSource as DependencyObject);
            if (item?.DataContext is not PlaylistDto playlist) return;
            var key = (string)e.Data.GetData(DragFileKeyFormat)!;
            try
            {
                await _client.AddToPlaylistAsync(playlist.Id, key);
                await LoadPlaylistsAsync();
            }
            catch (Exception ex)
            {
                MessageBox.Show($"Couldn't add to playlist: {ex.Message}", "Stream",
                    MessageBoxButton.OK, MessageBoxImage.Warning);
            }
        };

        panel.Children.Add(listBox);

        SectionsPanel.Children.Add(panel);
        _sectionElements["rail-playlists"] = panel;
        AddSectionTag("rail-playlists", "Playlists", _playlists.Count);
    }

    // Matches the web's startPlay: the first still-resolvable item in
    // playlist order (a member since removed from the library is skipped
    // rather than blocking playback of everything after it).
    private async Task PlayFirstInPlaylistAsync(PlaylistDto playlist)
    {
        var byKey = _allFiles.ToDictionary(f => f.Key);
        var first = playlist.Items.Select(k => byKey.GetValueOrDefault(k)).FirstOrDefault(f => f is not null);
        if (first is not null) await PlayAsync(first);
    }

    private void OpenPlaylistDetail(PlaylistDto playlist)
    {
        var win = new PlaylistDetailWindow(_client, playlist, _allFiles, PlayAsync) { Owner = this };
        win.ShowDialog();
        if (win.Changed) _ = LoadPlaylistsAsync();
    }

    private void AddSectionTag(string id, string label, int count)
    {
        var button = new Button
        {
            Style = (Style)FindResource("SectionTagButton"),
            Content = $"{label} ({count})",
        };
        button.Click += (_, _) => ScrollToSection(id);
        SectionTagsPanel.Children.Add(button);
        _sectionTagButtons[id] = button;
    }

    private void ScrollToSection(string id)
    {
        if (!_sectionElements.TryGetValue(id, out var element)) return;
        var position = element.TranslatePoint(new Point(0, 0), SectionsPanel);
        MainScroll.ScrollToVerticalOffset(Math.Max(0, position.Y));
        SetActiveSectionTag(id);
    }

    // Tracks which section is currently under the top of the viewport as the
    // page scrolls, mirroring markActiveTag in main.go's frontend JS.
    private void MainScroll_ScrollChanged(object sender, ScrollChangedEventArgs e)
    {
        if (_sectionElements.Count == 0) return;
        const double thresholdPx = 40;
        string? best = null;
        foreach (var (id, element) in _sectionElements)
        {
            var top = element.TranslatePoint(new Point(0, 0), SectionsPanel).Y - e.VerticalOffset;
            if (top <= thresholdPx) best = id;
        }
        SetActiveSectionTag(best ?? _sectionElements.Keys.First());
    }

    private void SetActiveSectionTag(string id)
    {
        if (_activeSectionId == id) return;
        var mutedBrush = (System.Windows.Media.Brush)FindResource("MutedBrush");
        var goldBrush = (System.Windows.Media.Brush)FindResource("GoldBrush");
        var textBrush = (System.Windows.Media.Brush)FindResource("TextBrush");

        if (_activeSectionId is not null && _sectionTagButtons.TryGetValue(_activeSectionId, out var prev))
        {
            prev.Foreground = mutedBrush;
            prev.BorderBrush = System.Windows.Media.Brushes.Transparent;
        }
        if (_sectionTagButtons.TryGetValue(id, out var next))
        {
            next.Foreground = textBrush;
            next.BorderBrush = goldBrush;
        }
        _activeSectionId = id;
    }

    // The small "⋮" button in each card's corner (CardMoreButton in the
    // shared FileCardTemplate/FileCardTemplateWide) opens Add to
    // Queue/Add to Playlist. Handled by bubbling up to the owning ListBox
    // rather than a Click on the template's own button, since the template
    // is shared across windows with different code-behind and can't
    // reference a named handler directly. This replaced a right-click menu
    // that fired anywhere on the card, which read as an unwanted "detail
    // menu" popping up on a plain click.
    private void AttachCardMoreMenu(ListBox listBox)
    {
        listBox.AddHandler(ButtonBase.ClickEvent, new RoutedEventHandler((_, e) =>
        {
            if (e.OriginalSource is not FrameworkElement { Name: "CardMoreButton" } fe) return;
            if (fe.DataContext is not FileEntry entry) return;
            e.Handled = true;

            // Matches openCtxMenu's own row set (Play/Play next/Add to
            // queue/Add to playlist): "Play" always plays directly
            // (skipDetailCheck there), bypassing the Films-rail detail-page
            // click behavior on purpose, same as the web's own menu does.
            var menu = new ContextMenu();
            var playItem = new MenuItem { Header = "Play" };
            playItem.Click += async (_, _) => await PlayAsync(entry);
            var playNextItem = new MenuItem { Header = "Play Next" };
            playNextItem.Click += (_, _) => _queue.Insert(0, entry);
            var queueItem = new MenuItem { Header = "Add to Queue" };
            queueItem.Click += (_, _) => _queue.Add(entry);
            var playlistItem = new MenuItem { Header = "Add to Playlist…" };
            playlistItem.Click += (_, _) =>
            {
                var dialog = new AddToPlaylistWindow(_client, entry.Key) { Owner = this };
                if (dialog.ShowDialog() == true) _ = LoadPlaylistsAsync();
            };
            menu.Items.Add(playItem);
            menu.Items.Add(playNextItem);
            menu.Items.Add(queueItem);
            menu.Items.Add(playlistItem);
            menu.PlacementTarget = fe;
            menu.IsOpen = true;
        }), true);
    }

    // A ListBox's own ScrollViewer is disabled (see CardListBox and its
    // variants), but a nested ItemsControl still captures the mouse wheel
    // for itself rather than letting it bubble to the page's outer
    // ScrollViewer -- a well-known WPF quirk, and the reason scrolling over
    // any card section didn't work. Forwarding it by hand fixes that.
    private void AttachWheelForwarding(ListBox listBox)
    {
        listBox.PreviewMouseWheel += (_, e) =>
        {
            MainScroll.ScrollToVerticalOffset(MainScroll.VerticalOffset - e.Delta);
            e.Handled = true;
        };
    }

    // Matches wirePlaylistDropTarget's drag side in main.go's frontend JS
    // (see renderMusicCard's own dragstart wiring): any file card can be
    // dragged onto a playlist tile to add it, mouse/trackpad only, same as
    // there (HTML5 drag-and-drop has no touch equivalent either). Standard
    // WPF drag-detection pattern: a real drag only starts once the mouse has
    // moved past the OS's own drag threshold while still held, so a plain
    // click still reaches the PreviewMouseLeftButtonUp handler that opens/
    // plays the card -- DoDragDrop's own nested message loop consumes the
    // eventual mouse-up when a drag genuinely happened, so the two never
    // both fire for the same gesture.
    private static void AttachDragSource(ListBox listBox)
    {
        Point? dragStart = null;
        listBox.PreviewMouseLeftButtonDown += (_, e) => dragStart = e.GetPosition(null);
        listBox.PreviewMouseMove += (_, e) =>
        {
            if (dragStart is not { } start || e.LeftButton != MouseButtonState.Pressed) return;
            var pos = e.GetPosition(null);
            if (Math.Abs(pos.X - start.X) < SystemParameters.MinimumHorizontalDragDistance &&
                Math.Abs(pos.Y - start.Y) < SystemParameters.MinimumVerticalDragDistance) return;
            dragStart = null;

            var item = FindAncestor<ListBoxItem>(e.OriginalSource as DependencyObject);
            if (item?.DataContext is not FileEntry entry) return;
            DragDrop.DoDragDrop(item, new DataObject(DragFileKeyFormat, entry.Key), DragDropEffects.Copy);
        };
    }

    private const string DragFileKeyFormat = "StreamVideosPlayer.FileEntryKey";

    // Matches bindTint's mouseenter/mouseleave pairing in main.go's frontend
    // JS, adapted to WPF: MouseEnter/MouseLeave are Direct-routed (no
    // bubbling), so there's no way to catch them for dynamically-generated
    // children from a single handler on the ListBox the way Click/wheel
    // events above are handled. MouseMove (which does bubble) plus hit-
    // testing the currently-covered ListBoxItem, only acting when it
    // actually changed, is the standard substitute.
    private void AttachHoverTint(ListBox listBox)
    {
        listBox.MouseMove += (_, e) =>
        {
            var item = FindAncestor<ListBoxItem>(e.OriginalSource as DependencyObject);
            var entry = item?.DataContext as FileEntry;
            if (ReferenceEquals(entry, _hoveredTintEntry)) return;
            _hoveredTintEntry = entry;
            _ = UpdateWallTintAsync(entry?.CardImageUrl ?? entry?.ThumbnailImageUrl);
        };
        listBox.MouseLeave += (_, _) =>
        {
            _hoveredTintEntry = null;
            _ = UpdateWallTintAsync(null);
        };
    }

    private async Task SampleAndSetRestingTintAsync(string imageUrl)
    {
        var color = await ColorSampler.SampleFromUrlAsync(imageUrl);
        if (color is null) return;
        _restingTintColor = color.Value;
        // Nothing hovered right now -- the wash should already be showing
        // the hero's colour at rest, not waiting for the first mouseleave
        // to ever apply it.
        if (_hoveredTintEntry is null) AnimateTintBrush(WallTintBrush, _restingTintColor);
    }

    // Matches applyTint/tintReset: url null means "nothing hovered", which
    // settles back to _restingTintColor (the hero's own sampled colour, set
    // once in AddHeroBanner) rather than the plain brand gold every time.
    private async Task UpdateWallTintAsync(string? url)
    {
        var color = url is null ? _restingTintColor : await ColorSampler.SampleFromUrlAsync(url) ?? _restingTintColor;
        AnimateTintBrush(WallTintBrush, color);
    }

    // Matches #tint's own transition:background 420ms cubic-bezier(.4,0,.2,1).
    private static void AnimateTintBrush(System.Windows.Media.SolidColorBrush brush, System.Windows.Media.Color to)
    {
        var animation = new System.Windows.Media.Animation.ColorAnimation(to, TimeSpan.FromMilliseconds(420))
        {
            EasingFunction = new System.Windows.Media.Animation.CubicEase { EasingMode = System.Windows.Media.Animation.EasingMode.EaseOut },
        };
        brush.BeginAnimation(System.Windows.Media.SolidColorBrush.ColorProperty, animation);
    }

    private static T? FindAncestor<T>(DependencyObject? current) where T : DependencyObject
    {
        while (current is not null && current is not T)
            current = System.Windows.Media.VisualTreeHelper.GetParent(current);
        return current as T;
    }

    // ── Playlists (data only -- the UI lives in AddPlaylistsSection) ────

    private async Task LoadPlaylistsAsync()
    {
        try
        {
            _playlists = await _client.GetPlaylistsAsync();
            // Fetched (and bound) before RebuildSections runs, same
            // convention as FileEntry.Progress -- plain settable properties
            // populated once up front rather than raising change
            // notifications after the ListBox has already bound.
            await Task.WhenAll(_playlists.Where(p => p.Items.Count > 0).Select(async p =>
            {
                p.CoverImage = await _client.GetPlaylistCoverAsync(p.Id, p.Updated);
            }));
            RebuildSections();
        }
        catch (Exception ex)
        {
            MessageBox.Show($"Couldn't load playlists: {ex.Message}", "Stream",
                MessageBoxButton.OK, MessageBoxImage.Warning);
        }
    }

    // ── Playback ─────────────────────────────────────────────────────────

    private async Task PlayAsync(FileEntry entry)
    {
        await SaveProgressAsync(); // flush the outgoing item's position first

        string url;
        try
        {
            url = await _client.ResolvePlaybackUrlAsync(entry);
        }
        catch (Exception ex)
        {
            MessageBox.Show($"Couldn't start playback: {ex.Message}", "Stream",
                MessageBoxButton.OK, MessageBoxImage.Warning);
            return;
        }

        using var media = new Media(_libVlc, new Uri(url));

        if (!string.IsNullOrEmpty(entry.SubtitleUrl))
            media.AddOption(":sub-file=" + entry.SubtitleUrl);

        // Matches resumePlayback's own fromStart rule in main.go's frontend
        // JS: a music video is a few minutes long and gets rewatched, not
        // continued, so it always opens at the start even though progress is
        // still recorded for it (still drives the Continue Watching row and
        // its own progress bar -- only the seek-on-open is skipped).
        if (!entry.IsMusicVideo)
        {
            var resume = await _client.GetProgressAsync(entry.Key);
            if (resume is { Position: > 0 })
                media.AddOption($":start-time={(int)resume.Position}");
        }

        _current = entry;
        NowPlayingText.Text = entry.DisplayTitle;
        PlayerOverlay.Visibility = Visibility.Visible;
        SetPlayerControlsVisible(true);
        _controlsIdleTimer.Stop();
        _controlsIdleTimer.Start();
        StartPlayerTint(entry);
        _mediaPlayer.Play(media);
    }

    private async void ClosePlayer_Click(object sender, RoutedEventArgs e)
    {
        await SaveProgressAsync();
        _mediaPlayer.Stop();
        _controlsIdleTimer.Stop();
        StopPlayerTint();
        PlayerOverlay.Visibility = Visibility.Collapsed;
        // Only reachable from the full-size layout (the close button lives
        // in BackButtonBar, which minimizing hides -- restore first is the
        // only path back to it), but reset defensively so the next PlayAsync
        // always opens full-size regardless.
        if (_isMini) RestorePlayer();
    }

    // Matches setNowPlayingTint/updateNowPlayingWash in main.go's frontend
    // JS, just always-on for the whole time a video is open rather than
    // gated on the minimized mini-player's own queue state -- there's no
    // wall showing through here to reserve the effect for (see #modal's own
    // CSS comment making the same call).
    private void StartPlayerTint(FileEntry entry)
    {
        var art = entry.CardImageUrl ?? entry.ThumbnailImageUrl ?? entry.HeroArt;
        _ = SampleAndSetPlayerTintAsync(art);

        var breathe = new System.Windows.Media.Animation.DoubleAnimation(0.28, 0.5, TimeSpan.FromSeconds(8))
        {
            AutoReverse = true,
            RepeatBehavior = System.Windows.Media.Animation.RepeatBehavior.Forever,
            EasingFunction = new System.Windows.Media.Animation.SineEase(),
        };
        PlayerTint.BeginAnimation(UIElement.OpacityProperty, breathe);

        // Independent, non-matching durations for X vs Y (13s/17s) so the
        // drift never quite repeats the same beat twice -- same reasoning
        // main.go gives for its own wash's opacity/path timing mismatch.
        var driftX = new System.Windows.Media.Animation.DoubleAnimation(-40, 40, TimeSpan.FromSeconds(13))
        {
            AutoReverse = true,
            RepeatBehavior = System.Windows.Media.Animation.RepeatBehavior.Forever,
            EasingFunction = new System.Windows.Media.Animation.SineEase(),
        };
        var driftY = new System.Windows.Media.Animation.DoubleAnimation(-25, 25, TimeSpan.FromSeconds(17))
        {
            AutoReverse = true,
            RepeatBehavior = System.Windows.Media.Animation.RepeatBehavior.Forever,
            EasingFunction = new System.Windows.Media.Animation.SineEase(),
        };
        PlayerTintDrift.BeginAnimation(System.Windows.Media.TranslateTransform.XProperty, driftX);
        PlayerTintDrift.BeginAnimation(System.Windows.Media.TranslateTransform.YProperty, driftY);
    }

    private void StopPlayerTint()
    {
        PlayerTint.BeginAnimation(UIElement.OpacityProperty, null);
        PlayerTintDrift.BeginAnimation(System.Windows.Media.TranslateTransform.XProperty, null);
        PlayerTintDrift.BeginAnimation(System.Windows.Media.TranslateTransform.YProperty, null);
    }

    private async Task SampleAndSetPlayerTintAsync(string? art)
    {
        var color = art is null ? null : await ColorSampler.SampleFromUrlAsync(art);
        AnimateTintBrush(PlayerTintBrush, color ?? _restingTintColor);
    }

    private void PlayerOverlay_MouseMove(object sender, MouseEventArgs e)
    {
        if (_isMini) return; // no chrome to fade in the mini box -- see MinimizePlayer
        SetPlayerControlsVisible(true);
        _controlsIdleTimer.Stop();
        _controlsIdleTimer.Start();
    }

    // Matches modalTap's own click handler: clicking the mini box restores
    // it to full size, rather than doing anything else with the click.
    private void PlayerOverlay_MouseLeftButtonUp(object sender, MouseButtonEventArgs e)
    {
        if (_isMini) RestorePlayer();
    }

    // Matches minimizePlayer/restorePlayer in main.go's frontend JS: shrinks
    // PlayerOverlay itself down to a small corner box rather than punching
    // an interactive hole through a full-window overlay -- a WPF element
    // only occupies and hit-tests its own rendered bounds, so once
    // PlayerOverlay is sized and anchored to a small corner rect instead of
    // stretched to fill the window, clicks everywhere else already fall
    // through to BrowseRoot underneath with no extra plumbing needed.
    private void MinimizePlayer_Click(object sender, RoutedEventArgs e) => MinimizePlayer();

    private void MinimizePlayer()
    {
        _isMini = true;
        _controlsIdleTimer.Stop();
        PlayerOverlay.Background = System.Windows.Media.Brushes.Transparent;
        PlayerOverlay.HorizontalAlignment = HorizontalAlignment.Right;
        PlayerOverlay.VerticalAlignment = VerticalAlignment.Bottom;
        PlayerOverlay.Width = 340;
        PlayerOverlay.Height = 191;
        PlayerOverlay.Margin = new Thickness(0, 0, 16, 16);
        PlayerTint.Visibility = Visibility.Collapsed;
        PlayerFrame.Margin = new Thickness(0);
        BackButtonBar.Visibility = Visibility.Collapsed;
        ControlsBar.Visibility = Visibility.Collapsed;
    }

    private void RestorePlayer()
    {
        _isMini = false;
        PlayerOverlay.Background = new System.Windows.Media.SolidColorBrush(System.Windows.Media.Color.FromArgb(0xD2, 0, 0, 0));
        PlayerOverlay.HorizontalAlignment = HorizontalAlignment.Stretch;
        PlayerOverlay.VerticalAlignment = VerticalAlignment.Stretch;
        PlayerOverlay.Width = double.NaN;
        PlayerOverlay.Height = double.NaN;
        PlayerOverlay.Margin = new Thickness(0);
        PlayerTint.Visibility = Visibility.Visible;
        PlayerFrame.Margin = new Thickness(56);
        BackButtonBar.Visibility = Visibility.Visible;
        ControlsBar.Visibility = Visibility.Visible;
        SetPlayerControlsVisible(true);
        _controlsIdleTimer.Stop();
        _controlsIdleTimer.Start();
    }

    // Fades the back button and the whole control bar (not the video itself,
    // which lives in its own Grid row and is never touched) -- Opacity +
    // IsHitTestVisible rather than Visibility=Collapsed, since collapsing an
    // Auto-height row would let the VideoView's Grid.Row="1" (Height="*")
    // expand into the freed space, resizing the video every time the
    // controls hide/show. Matches the web's own .chrome-off, which uses
    // opacity+pointer-events for exactly this reason (see main.go's CSS).
    private void SetPlayerControlsVisible(bool visible)
    {
        var opacity = visible ? 1.0 : 0.0;
        BackButtonBar.Opacity = opacity;
        BackButtonBar.IsHitTestVisible = visible;
        ControlsBar.Opacity = opacity;
        ControlsBar.IsHitTestVisible = visible;
        if (!visible) SettingsPopup.IsOpen = false;
    }

    private void PlayPause_Click(object sender, RoutedEventArgs e)
    {
        if (_mediaPlayer.IsPlaying) _mediaPlayer.Pause();
        else _mediaPlayer.Play();
    }

    private void VolumeSlider_ValueChanged(object sender, RoutedPropertyChangedEventArgs<double> e)
    {
        // XAML doesn't set an initial Value on this slider (see the
        // VolumeSlider.Value=100 line in the constructor) specifically so
        // this handler can't fire before _mediaPlayer exists; the guard
        // stays as a defense against that regressing.
        if (_mediaPlayer is null) return;
        _mediaPlayer.Volume = (int)e.NewValue;
    }

    private void ToggleSettingsPanel_Click(object sender, RoutedEventArgs e)
    {
        RefreshTrackMenus();
        SettingsPopup.IsOpen = !SettingsPopup.IsOpen;
    }

    // This is real libVLC underneath, not a browser <video> element, so
    // every track libVLC itself demuxed from the file is available here --
    // not just whatever single audio/subtitle track the server happened to
    // default to (see checkAudioTrack/ensureAudioDefault in main.go, which
    // exists purely because browsers can't do this kind of switching).
    private void RefreshTrackMenus()
    {
        var audioTracks = _mediaPlayer.AudioTrackDescription ?? Array.Empty<TrackDescription>();
        AudioTrackCombo.DisplayMemberPath = nameof(TrackDescription.Name);
        AudioTrackCombo.SelectedValuePath = nameof(TrackDescription.Id);
        AudioTrackCombo.ItemsSource = audioTracks;
        AudioTrackCombo.SelectedValue = _mediaPlayer.AudioTrack;

        // Id -1 is libVLC's own "disabled" track -- included here (rather
        // than only ever showing real subtitle tracks) so there's a way to
        // turn subtitles back off after turning them on.
        var subTracks = _mediaPlayer.SpuDescription ?? Array.Empty<TrackDescription>();
        SubtitleTrackCombo.DisplayMemberPath = nameof(TrackDescription.Name);
        SubtitleTrackCombo.SelectedValuePath = nameof(TrackDescription.Id);
        SubtitleTrackCombo.ItemsSource = subTracks;
        SubtitleTrackCombo.SelectedValue = _mediaPlayer.Spu;
    }

    private void AudioTrackCombo_SelectionChanged(object sender, SelectionChangedEventArgs e)
    {
        if (AudioTrackCombo.SelectedValue is int id) _mediaPlayer.SetAudioTrack(id);
    }

    private void SubtitleTrackCombo_SelectionChanged(object sender, SelectionChangedEventArgs e)
    {
        if (SubtitleTrackCombo.SelectedValue is int id) _mediaPlayer.SetSpu(id);
    }

    private void SpeedCombo_SelectionChanged(object sender, SelectionChangedEventArgs e)
    {
        if (SpeedCombo.SelectedItem is double rate && _mediaPlayer is not null)
            _mediaPlayer.SetRate((float)rate);
    }

    private void AspectCombo_SelectionChanged(object sender, SelectionChangedEventArgs e)
    {
        if (_mediaPlayer is null) return;
        var value = AspectCombo.SelectedItem as string;
        _mediaPlayer.AspectRatio = (string.IsNullOrEmpty(value) || value == "Default") ? null : value;
    }

    private void PositionSlider_DragStarted(object sender, DragStartedEventArgs e) => _isDraggingSlider = true;

    private void PositionSlider_DragCompleted(object sender, DragCompletedEventArgs e)
    {
        _isDraggingSlider = false;
        if (_mediaPlayer.Length > 0)
            _mediaPlayer.Position = (float)PositionSlider.Value;
    }

    private void UiTimer_Tick(object? sender, EventArgs e)
    {
        if (_isDraggingSlider || _mediaPlayer.Length <= 0) return;
        PositionSlider.Value = _mediaPlayer.Position;
        var elapsed = TimeSpan.FromMilliseconds(_mediaPlayer.Time);
        var total = TimeSpan.FromMilliseconds(_mediaPlayer.Length);
        TimeText.Text = $"{elapsed:mm\\:ss} / {total:mm\\:ss}";
    }

    private async Task SaveProgressAsync()
    {
        if (_current is null || _mediaPlayer.Length <= 0) return;
        var positionSeconds = _mediaPlayer.Time / 1000.0;
        var durationSeconds = _mediaPlayer.Length / 1000.0;
        try
        {
            await _client.PostProgressAsync(_current.Key, positionSeconds, durationSeconds);
        }
        catch
        {
            // Best-effort -- see ServerClient.PostProgressAsync.
        }
    }
}
