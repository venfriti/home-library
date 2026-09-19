using System.Windows;
using System.Windows.Controls;
using System.Windows.Input;
using System.Windows.Media;
using System.Windows.Media.Imaging;
using StreamVideosPlayer.Services;

namespace StreamVideosPlayer;

public partial class ShowDetailWindow : Window
{
    private readonly ShowGroup _show;
    private readonly Func<FileEntry, Task> _onPlay;
    private readonly List<int> _seasons;
    private int _currentSeason;
    private readonly Dictionary<int, Button> _seasonTabButtons = new();
    private Button? _playButton;
    private FileEntry? _playTarget;
    private StackPanel? _episodeSection;

    public ShowDetailWindow(ShowGroup show, Func<FileEntry, Task> onPlay)
    {
        InitializeComponent();
        _show = show;
        _onPlay = onPlay;

        _seasons = show.Episodes.Select(e => e.Season).Distinct().OrderBy(s => s).ToList();
        _currentSeason = _seasons.Count > 0 ? _seasons[0] : 0;

        BuildHeader();
        if (_seasons.Count > 1) BuildSeasonTabs();
        BuildEpisodeSection();
    }

    private void CloseWindow_Click(object sender, RoutedEventArgs e) => Close();

    // Matches renderShowView: the header always reflects the show's
    // chronologically-first episode (season/episode ordering, not whichever
    // season tab is selected) -- only the episode list below responds to
    // that.
    private void BuildHeader()
    {
        var ep = _show.Episodes[0];
        var backdropUrl = ep.Backdrop ?? ep.PosterUrl ?? ep.ThumbUrl;
        if (!string.IsNullOrEmpty(backdropUrl))
        {
            var backdropBox = new Grid { Height = 320, ClipToBounds = true };
            backdropBox.Children.Add(new Image
            {
                Source = new BitmapImage(new Uri(backdropUrl)),
                Stretch = Stretch.UniformToFill,
            });
            var bgColor = ((SolidColorBrush)FindResource("BgBrush")).Color;
            backdropBox.Children.Add(new Border
            {
                Background = new LinearGradientBrush(new GradientStopCollection
                {
                    new GradientStop(Colors.Transparent, 0),
                    new GradientStop(bgColor, 1),
                }, new Point(0, 0), new Point(0, 1)),
            });
            ContentPanel.Children.Add(backdropBox);
        }

        var headerRow = new Grid { Margin = new Thickness(28, backdropUrl is null ? 24 : -70, 28, 0) };
        headerRow.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
        headerRow.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(1, GridUnitType.Star) });

        var posterUrl = ep.PosterUrl ?? ep.ThumbUrl;
        var posterBorder = new Border
        {
            Width = 150, Height = 225, CornerRadius = new CornerRadius(6), ClipToBounds = true,
            Background = (Brush)FindResource("Bg3Brush"),
            Effect = new System.Windows.Media.Effects.DropShadowEffect { Color = Colors.Black, BlurRadius = 24, ShadowDepth = 6, Opacity = 0.6 },
        };
        if (!string.IsNullOrEmpty(posterUrl))
            posterBorder.Child = new Image { Source = new BitmapImage(new Uri(posterUrl)), Stretch = Stretch.UniformToFill };
        Grid.SetColumn(posterBorder, 0);
        headerRow.Children.Add(posterBorder);

        var info = new StackPanel { Margin = new Thickness(20, 0, 0, 0), VerticalAlignment = VerticalAlignment.Bottom };
        Grid.SetColumn(info, 1);
        info.Children.Add(new TextBlock
        {
            Text = string.IsNullOrEmpty(ep.Title) ? ep.Name : ep.Title,
            Foreground = Brushes.White,
            FontFamily = (FontFamily)FindResource("SerifFont"),
            FontWeight = FontWeights.Bold,
            FontSize = 26,
            TextWrapping = TextWrapping.Wrap,
        });

        var metaParts = new List<string>();
        if (ep.Year > 0) metaParts.Add(ep.Year.ToString());
        if (_seasons.Count > 1) metaParts.Add($"{_seasons.Count} seasons");
        if (ep.Rating > 0) metaParts.Add($"★ {ep.Rating:0.0}");
        if (metaParts.Count > 0)
        {
            info.Children.Add(new TextBlock
            {
                Text = string.Join("  ·  ", metaParts),
                Foreground = (Brush)FindResource("MutedBrush"),
                FontFamily = (FontFamily)FindResource("MonoFont"),
                FontSize = 12,
                Margin = new Thickness(0, 8, 0, 0),
            });
        }
        if (ep.Genres is { Count: > 0 })
        {
            info.Children.Add(new TextBlock
            {
                Text = string.Join("  ·  ", ep.Genres),
                Foreground = (Brush)FindResource("GoldTextBrush"),
                FontFamily = (FontFamily)FindResource("SansFont"),
                FontSize = 12,
                Margin = new Thickness(0, 4, 0, 0),
            });
        }
        headerRow.Children.Add(info);
        ContentPanel.Children.Add(headerRow);

        if (!string.IsNullOrEmpty(ep.Overview))
        {
            ContentPanel.Children.Add(new TextBlock
            {
                Text = ep.Overview,
                Foreground = (Brush)FindResource("TextBrush"),
                FontFamily = (FontFamily)FindResource("SansFont"),
                FontSize = 13,
                LineHeight = 21,
                TextWrapping = TextWrapping.Wrap,
                Margin = new Thickness(28, 18, 28, 0),
            });
        }

        _playButton = new Button
        {
            Style = (Style)FindResource("HeroPlayButton"),
            HorizontalAlignment = HorizontalAlignment.Left,
            Margin = new Thickness(28, 18, 0, 0),
        };
        _playButton.Click += async (_, _) =>
        {
            if (_playTarget is not null) { await _onPlay(_playTarget); Close(); }
        };
        ContentPanel.Children.Add(_playButton);
        UpdatePlayButton();
    }

    private void BuildSeasonTabs()
    {
        var tabs = new WrapPanel { Margin = new Thickness(28, 24, 28, 0) };
        foreach (var season in _seasons)
        {
            var button = new Button
            {
                Style = (Style)FindResource("SeasonTabButton"),
                Content = season == 0 ? "Specials" : $"Season {season}",
                Tag = season == _currentSeason ? "Active" : null,
            };
            button.Click += (_, _) => SelectSeason(season);
            tabs.Children.Add(button);
            _seasonTabButtons[season] = button;
        }
        ContentPanel.Children.Add(tabs);
    }

    private void SelectSeason(int season)
    {
        if (season == _currentSeason) return;
        if (_seasonTabButtons.TryGetValue(_currentSeason, out var previous)) previous.Tag = null;
        _currentSeason = season;
        if (_seasonTabButtons.TryGetValue(_currentSeason, out var active)) active.Tag = "Active";
        UpdatePlayButton();
        BuildEpisodeSection();
    }

    private void UpdatePlayButton()
    {
        if (_playButton is null) return;
        _playTarget = _show.Episodes.Where(e => e.Season == _currentSeason).OrderBy(e => e.Episode).FirstOrDefault();
        _playButton.Content = _playTarget is { Season: > 0, Episode: > 0 }
            ? $"▶  PLAY S{_playTarget.Season:D2}E{_playTarget.Episode:D2}"
            : "▶  PLAY";
    }

    // Note: renderShowView also lists a season's own Extras (its Featurettes
    // folder, see markShowFeaturettes server-side) below the episode list --
    // left out here since ShowGroup only carries this show's own episodes,
    // not the wider library needed to find them.
    private void BuildEpisodeSection()
    {
        if (_episodeSection is not null) ContentPanel.Children.Remove(_episodeSection);
        _episodeSection = new StackPanel();

        var episodes = _show.Episodes.Where(e => e.Season == _currentSeason).OrderBy(e => e.Episode).ToList();
        var list = new ListBox { Style = (Style)FindResource("EpisodeRowListBox"), ItemsSource = episodes, Margin = new Thickness(28, 18, 28, 0) };
        list.MouseDoubleClick += async (_, _) =>
        {
            if (list.SelectedItem is FileEntry entry) { await _onPlay(entry); Close(); }
        };
        _episodeSection.Children.Add(list);

        ContentPanel.Children.Add(_episodeSection);
    }
}
