using System.Windows;
using System.Windows.Controls;
using System.Windows.Media;
using System.Windows.Media.Imaging;
using StreamVideosPlayer.Services;

namespace StreamVideosPlayer;

public partial class FilmDetailWindow : Window
{
    private readonly FileEntry _film;
    private readonly Func<FileEntry, Task> _onPlay;

    public FilmDetailWindow(FileEntry film, List<FileEntry> extras, Func<FileEntry, Task> onPlay)
    {
        InitializeComponent();
        _film = film;
        _onPlay = onPlay;

        Build(extras);
    }

    private void CloseWindow_Click(object sender, RoutedEventArgs e) => Close();

    private void Build(List<FileEntry> extras)
    {
        var backdropUrl = _film.Backdrop ?? _film.PosterUrl ?? _film.ThumbUrl;
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

        var posterUrl = _film.CardImageUrl;
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
            Text = string.IsNullOrEmpty(_film.Title) ? _film.Name : _film.Title,
            Foreground = Brushes.White,
            FontFamily = (FontFamily)FindResource("SerifFont"),
            FontWeight = FontWeights.Bold,
            FontSize = 26,
            TextWrapping = TextWrapping.Wrap,
        });

        var metaParts = new List<string>();
        if (_film.Year > 0) metaParts.Add(_film.Year.ToString());
        if (_film.Runtime > 0) metaParts.Add($"{_film.Runtime} min");
        if (_film.Rating > 0) metaParts.Add($"★ {_film.Rating:0.0}");
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
        if (_film.Genres is { Count: > 0 })
        {
            info.Children.Add(new TextBlock
            {
                Text = string.Join("  ·  ", _film.Genres),
                Foreground = (Brush)FindResource("GoldTextBrush"),
                FontFamily = (FontFamily)FindResource("SansFont"),
                FontSize = 12,
                Margin = new Thickness(0, 4, 0, 0),
            });
        }
        headerRow.Children.Add(info);
        ContentPanel.Children.Add(headerRow);

        if (!string.IsNullOrEmpty(_film.Overview))
        {
            ContentPanel.Children.Add(new TextBlock
            {
                Text = _film.Overview,
                Foreground = (Brush)FindResource("TextBrush"),
                FontFamily = (FontFamily)FindResource("SansFont"),
                FontSize = 13,
                LineHeight = 21,
                TextWrapping = TextWrapping.Wrap,
                Margin = new Thickness(28, 18, 28, 0),
            });
        }

        var playButton = new Button
        {
            Style = (Style)FindResource("HeroPlayButton"),
            Content = "▶  PLAY",
            HorizontalAlignment = HorizontalAlignment.Left,
            Margin = new Thickness(28, 18, 0, 0),
        };
        playButton.Click += async (_, _) => { await _onPlay(_film); Close(); };
        ContentPanel.Children.Add(playButton);

        AddMetaGrid();

        if (_film.Collection is { } collection)
        {
            ContentPanel.Children.Add(new TextBlock
            {
                Margin = new Thickness(28, 18, 28, 0),
                FontFamily = (FontFamily)FindResource("SansFont"),
                FontSize = 12,
                Foreground = (Brush)FindResource("MutedBrush"),
                Inlines = { new System.Windows.Documents.Run("Part of "), new System.Windows.Documents.Run(collection.Name) { Foreground = (Brush)FindResource("GoldTextBrush") } },
            });
        }

        if (extras.Count > 0)
        {
            var head = new TextBlock
            {
                Text = "Extras",
                Foreground = (Brush)FindResource("TextBrush"),
                FontFamily = (FontFamily)FindResource("SerifFont"),
                FontWeight = FontWeights.Bold,
                FontSize = 16,
                Margin = new Thickness(28, 30, 28, 6),
            };
            ContentPanel.Children.Add(head);

            var list = new ListBox { Style = (Style)FindResource("ContinueWatchingCardListBox"), ItemsSource = extras, Margin = new Thickness(20, 0, 20, 0) };
            list.MouseDoubleClick += async (_, _) =>
            {
                if (list.SelectedItem is FileEntry entry) { await _onPlay(entry); Close(); }
            };
            ContentPanel.Children.Add(list);
        }
    }

    private void AddMetaGrid()
    {
        var cells = new List<(string Label, string Value)>();
        if (!string.IsNullOrEmpty(_film.Director)) cells.Add(("Director", _film.Director));
        if (_film.Cast is { Count: > 0 }) cells.Add(("Cast", string.Join(", ", _film.Cast)));
        if (_film.Runtime > 0) cells.Add(("Runtime", $"{_film.Runtime} min"));
        if (!string.IsNullOrEmpty(_film.Country)) cells.Add(("Country", _film.Country));
        if (!string.IsNullOrEmpty(_film.Language)) cells.Add(("Language", _film.Language));
        if (!string.IsNullOrEmpty(_film.AudioLang)) cells.Add(("Audio", _film.AudioLang));
        if (cells.Count == 0) return;

        var wrap = new WrapPanel { Margin = new Thickness(28, 24, 28, 0) };
        foreach (var (label, value) in cells)
        {
            var cell = new StackPanel { Margin = new Thickness(0, 0, 40, 16), Width = 220 };
            cell.Children.Add(new TextBlock
            {
                Text = label.ToUpperInvariant(),
                Foreground = (Brush)FindResource("MutedBrush"),
                FontFamily = (FontFamily)FindResource("MonoFont"),
                FontSize = 10,
                Margin = new Thickness(0, 0, 0, 3),
            });
            cell.Children.Add(new TextBlock
            {
                Text = value,
                Foreground = (Brush)FindResource("TextBrush"),
                FontFamily = (FontFamily)FindResource("SansFont"),
                FontSize = 13,
                TextWrapping = TextWrapping.Wrap,
            });
            wrap.Children.Add(cell);
        }

        var rule = new Border { BorderBrush = (Brush)FindResource("BorderBrush2"), BorderThickness = new Thickness(0, 1, 0, 1), Margin = new Thickness(28, 8, 28, 0), Padding = new Thickness(0, 16, 0, 0) };
        rule.Child = wrap;
        ContentPanel.Children.Add(rule);
    }
}
