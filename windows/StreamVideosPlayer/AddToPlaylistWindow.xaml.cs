using System.Windows;
using System.Windows.Input;
using StreamVideosPlayer.Services;

namespace StreamVideosPlayer;

public partial class AddToPlaylistWindow : Window
{
    private readonly ServerClient _client;
    private readonly string _key;

    public AddToPlaylistWindow(ServerClient client, string key)
    {
        InitializeComponent();
        _client = client;
        _key = key;
        _ = LoadPlaylistsAsync();
    }

    private async Task LoadPlaylistsAsync()
    {
        try
        {
            PlaylistsList.ItemsSource = await _client.GetPlaylistsAsync();
        }
        catch (Exception ex)
        {
            MessageBox.Show($"Couldn't load playlists: {ex.Message}", "Stream",
                MessageBoxButton.OK, MessageBoxImage.Warning);
        }
    }

    private async void PlaylistsList_MouseDoubleClick(object sender, MouseButtonEventArgs e)
    {
        if (PlaylistsList.SelectedItem is not PlaylistDto playlist) return;
        try
        {
            await _client.AddToPlaylistAsync(playlist.Id, _key);
            DialogResult = true;
        }
        catch (Exception ex)
        {
            MessageBox.Show($"Couldn't add to playlist: {ex.Message}", "Stream",
                MessageBoxButton.OK, MessageBoxImage.Warning);
        }
    }

    private async void CreateButton_Click(object sender, RoutedEventArgs e) => await CreateAsync();

    private async void NewPlaylistNameBox_KeyDown(object sender, KeyEventArgs e)
    {
        if (e.Key == Key.Enter) await CreateAsync();
    }

    private async Task CreateAsync()
    {
        var name = NewPlaylistNameBox.Text.Trim();
        if (string.IsNullOrEmpty(name)) return;
        try
        {
            await _client.CreatePlaylistAsync(name, _key);
            DialogResult = true;
        }
        catch (Exception ex)
        {
            MessageBox.Show($"Couldn't create playlist: {ex.Message}", "Stream",
                MessageBoxButton.OK, MessageBoxImage.Warning);
        }
    }
}
