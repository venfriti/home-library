using System.Windows;
using System.Windows.Input;
using StreamVideosPlayer.Services;

namespace StreamVideosPlayer;

// Hosts one playlist's resolved items in their own window rather than an
// inline tab (see the section-tags redesign in MainWindow -- Playlists is
// now a horizontal row of tiles on the main page, and opening one drills in
// here instead of switching the whole page's content).
public partial class PlaylistDetailWindow : Window
{
    private readonly ServerClient _client;
    private readonly PlaylistDto _playlist;
    private readonly Dictionary<string, FileEntry> _byKey;
    private readonly Func<FileEntry, Task> _onPlay;

    // Set to true whenever this window changes the playlist (delete/remove),
    // so the caller knows to reload its own playlist list after ShowDialog().
    public bool Changed { get; private set; }

    public PlaylistDetailWindow(ServerClient client, PlaylistDto playlist, List<FileEntry> allFiles,
        Func<FileEntry, Task> onPlay)
    {
        InitializeComponent();
        _client = client;
        _playlist = playlist;
        _byKey = allFiles.ToDictionary(f => f.Key);
        _onPlay = onPlay;

        TitleText.Text = playlist.Name;
        RefreshItems();
    }

    private void RefreshItems()
    {
        ItemsList.ItemsSource = _playlist.Items
            .Select(key => _byKey.GetValueOrDefault(key))
            .Where(f => f is not null)
            .ToList();
    }

    private async void ItemsList_MouseDoubleClick(object sender, MouseButtonEventArgs e)
    {
        if (ItemsList.SelectedItem is not FileEntry entry) return;
        await _onPlay(entry);
        Close();
    }

    private async void RemoveItem_Click(object sender, RoutedEventArgs e)
    {
        if (ItemsList.SelectedItem is not FileEntry entry) return;
        try
        {
            await _client.RemoveFromPlaylistAsync(_playlist.Id, entry.Key);
            _playlist.Items.Remove(entry.Key);
            RefreshItems();
            Changed = true;
        }
        catch (Exception ex)
        {
            MessageBox.Show($"Couldn't remove item: {ex.Message}", "Stream",
                MessageBoxButton.OK, MessageBoxImage.Warning);
        }
    }

    private async void DeletePlaylist_Click(object sender, RoutedEventArgs e)
    {
        try
        {
            await _client.DeletePlaylistAsync(_playlist.Id);
            Changed = true;
            Close();
        }
        catch (Exception ex)
        {
            MessageBox.Show($"Couldn't delete playlist: {ex.Message}", "Stream",
                MessageBoxButton.OK, MessageBoxImage.Warning);
        }
    }
}
