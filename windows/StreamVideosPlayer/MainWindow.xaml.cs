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
    private bool _isDraggingSlider;
    private bool _useLocalSource;
    private UpdateInfo? _pendingUpdate;
    private readonly AppSettings _settings;

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

        Closing += async (_, _) =>
        {
            _uiTimer.Stop();
            _progressTimer.Stop();
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

        var continueWatching = Filter(_allFiles.Where(f => f.Progress > 0)).Take(7).ToList();
        AddHeroBanner(continueWatching.FirstOrDefault() ?? _allFiles.FirstOrDefault());
        AddFileSection("rail-continue-watching", "Continue watching", continueWatching, wide: true);
        AddFileSection("rail-films", "Films", Filter(_allFiles.Where(f => f.Section == ContentSection.Film)), wide: false);
        AddFileSection("rail-series", "Series", Filter(_allFiles.Where(f => f.Section == ContentSection.Series)), wide: false);
        AddPlaylistsSection();
        AddFileSection("rail-music", "Music videos", Filter(_allFiles.Where(f => f.Section == ContentSection.MusicVideo)), wide: true);
        AddFileSection("rail-library", "Library", Filter(_allFiles.Where(f => f.Section == ContentSection.Library)), wide: true);

        if (_sectionTagButtons.Count > 0)
            SetActiveSectionTag(_sectionTagButtons.Keys.First());
    }

    // Big backdrop banner at the top of the page, matching the web's actual
    // .hero/.hero-scrim/.hero-in/renderHero() exactly (verified against
    // main.go's own CSS and frontend JS, not built from memory): a two-layer
    // gradient scrim (left-to-right, layered with bottom-to-top), a gold
    // mono kicker line, a serif title, a 2-line-clamped description, and a
    // solid-gold play/resume button -- not the plain single-gradient banner
    // this used to be.
    private void AddHeroBanner(FileEntry? hero)
    {
        if (hero is null) return;
        var imageUrl = hero.HeroArt;
        if (string.IsNullOrEmpty(imageUrl)) return;

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
        SectionsPanel.Children.Add(container);
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

    private void AddFileSection(string id, string label, List<FileEntry> items, bool wide)
    {
        if (items.Count == 0) return;

        var panel = new StackPanel();
        panel.Children.Add(BuildRailHead(label, items.Count));

        // Continue watching gets its own template (permanent below-image
        // caption + time-left, matching .cw-shot/.cw-meta) rather than the
        // generic hover-reveal wide card the other wide sections use.
        var styleKey = id == "rail-continue-watching" ? "ContinueWatchingCardListBox" : (wide ? "WideCardListBox" : "CardListBox");
        var listBox = new ListBox
        {
            Style = (Style)FindResource(styleKey),
            ItemsSource = items,
        };
        listBox.MouseDoubleClick += async (_, _) =>
        {
            if (listBox.SelectedItem is FileEntry entry) await PlayAsync(entry);
        };
        AttachCardMoreMenu(listBox);
        AttachWheelForwarding(listBox);
        panel.Children.Add(listBox);

        SectionsPanel.Children.Add(panel);
        _sectionElements[id] = panel;
        AddSectionTag(id, label, items.Count);
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
        listBox.PreviewMouseLeftButtonUp += (_, e) =>
        {
            var item = FindAncestor<ListBoxItem>(e.OriginalSource as DependencyObject);
            if (item?.DataContext is PlaylistDto playlist) OpenPlaylistDetail(playlist);
        };
        AttachWheelForwarding(listBox);
        panel.Children.Add(listBox);

        SectionsPanel.Children.Add(panel);
        _sectionElements["rail-playlists"] = panel;
        AddSectionTag("rail-playlists", "Playlists", _playlists.Count);
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

            var menu = new ContextMenu();
            var queueItem = new MenuItem { Header = "Add to Queue" };
            queueItem.Click += (_, _) => _queue.Add(entry);
            var playlistItem = new MenuItem { Header = "Add to Playlist…" };
            playlistItem.Click += (_, _) =>
            {
                var dialog = new AddToPlaylistWindow(_client, entry.Key) { Owner = this };
                if (dialog.ShowDialog() == true) _ = LoadPlaylistsAsync();
            };
            menu.Items.Add(queueItem);
            menu.Items.Add(playlistItem);
            menu.PlacementTarget = fe;
            menu.IsOpen = true;
        }), true);
    }

    // A ListBox's own ScrollViewer is disabled (see CardListBox/
    // WideCardListBox), but a nested ItemsControl still captures the mouse
    // wheel for itself rather than letting it bubble to the page's outer
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

        var resume = await _client.GetProgressAsync(entry.Key);
        if (resume is { Position: > 0 })
            media.AddOption($":start-time={(int)resume.Position}");

        _current = entry;
        NowPlayingText.Text = entry.DisplayTitle;
        PlayerOverlay.Visibility = Visibility.Visible;
        _mediaPlayer.Play(media);
    }

    private async void ClosePlayer_Click(object sender, RoutedEventArgs e)
    {
        await SaveProgressAsync();
        _mediaPlayer.Stop();
        PlayerOverlay.Visibility = Visibility.Collapsed;
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
