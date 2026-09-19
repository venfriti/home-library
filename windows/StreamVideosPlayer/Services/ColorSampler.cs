using System.IO;
using System.Net.Http;
using System.Windows.Media;
using System.Windows.Media.Imaging;

namespace StreamVideosPlayer.Services;

// Ports dominantColour()/normaliseAccent() from main.go's frontend JS
// exactly, for the ambient tint system (see MainWindow's AttachHoverTint
// and the hero/now-playing tint wiring). Two passes: GetDominantColor finds
// a saturation-weighted average colour (falling back to a plain mean if too
// few pixels are saturated enough), then NormalizeAccent locks every result
// to the same saturation/lightness band so the effect always reads as a
// changing hue, not a flickering brightness -- a dark poster would
// otherwise produce an invisible wash and a bright one glare.
public static class ColorSampler
{
    private static readonly Dictionary<string, Color?> UrlCache = new();
    private static readonly HttpClient Http = new();

    // Downscaling to a small decode (32px, matching the JS sample's own
    // 32x32 canvas) keeps this cheap; letting width drive it and height
    // follow the source's own aspect ratio (rather than squashing to a
    // square canvas like the JS version does) samples the same pixels
    // without introducing distortion that never mattered to the average.
    //
    // The network fetch happens on a plain HttpClient (thread-agnostic) and
    // only the resulting bytes are handed to Task.Run for decoding --
    // BitmapImage.UriSource previously did both in one step, but that
    // downloads through WPF's own imaging pipeline, which expects a
    // Dispatcher/message pump on whatever thread called it. A bare
    // ThreadPool thread from Task.Run has neither, and the app hung
    // (AppHangTransient) exactly the way that mismatch predicts once enough
    // hover-triggered samples piled up. Decoding from an in-memory stream
    // afterward has no such dependency.
    public static async Task<Color?> SampleFromUrlAsync(string? url)
    {
        if (string.IsNullOrEmpty(url)) return null;
        if (UrlCache.TryGetValue(url, out var cached)) return cached;

        Color? result;
        try
        {
            var bytes = await Http.GetByteArrayAsync(url);
            result = await Task.Run(() =>
            {
                try
                {
                    using var stream = new MemoryStream(bytes);
                    var bmp = new BitmapImage();
                    bmp.BeginInit();
                    bmp.CacheOption = BitmapCacheOption.OnLoad;
                    bmp.DecodePixelWidth = 32;
                    bmp.StreamSource = stream;
                    bmp.EndInit();
                    bmp.Freeze();
                    return GetDominantColor(bmp);
                }
                catch
                {
                    return (Color?)null;
                }
            });
        }
        catch
        {
            result = null;
        }

        UrlCache[url] = result;
        return result;
    }

    public static Color? GetDominantColor(BitmapSource source)
    {
        var converted = new FormatConvertedBitmap(source, PixelFormats.Bgra32, null, 0);
        var w = converted.PixelWidth;
        var h = converted.PixelHeight;
        if (w == 0 || h == 0) return null;

        var stride = w * 4;
        var pixels = new byte[stride * h];
        converted.CopyPixels(pixels, stride, 0);

        double r = 0, g = 0, b = 0, weightSum = 0;
        double fr = 0, fg = 0, fb = 0, fw = 0;

        for (var i = 0; i < pixels.Length; i += 4)
        {
            double bC = pixels[i], gC = pixels[i + 1], rC = pixels[i + 2];
            var mx = Math.Max(rC, Math.Max(gC, bC));
            var mn = Math.Min(rC, Math.Min(gC, bC));
            var l = (mx + mn) / 510.0;
            if (l < 0.05 || l > 0.97) continue;
            fr += rC; fg += gC; fb += bC; fw++;
            var s = mx == mn ? 0 : (mx - mn) / mx;
            if (s < 0.08) continue;
            var weight = s * s;
            r += rC * weight; g += gC * weight; b += bC * weight; weightSum += weight;
        }

        if (weightSum > 0.35) { r /= weightSum; g /= weightSum; b /= weightSum; }
        else if (fw > 0) { r = fr / fw; g = fg / fw; b = fb / fw; }
        else return null;

        return NormalizeAccent(r, g, b);
    }

    private static Color NormalizeAccent(double r255, double g255, double b255)
    {
        var r = r255 / 255.0;
        var g = g255 / 255.0;
        var b = b255 / 255.0;
        var mx = Math.Max(r, Math.Max(g, b));
        var mn = Math.Min(r, Math.Min(g, b));
        double h = 0, s = 0;
        var l = (mx + mn) / 2;
        if (mx != mn)
        {
            var d = mx - mn;
            s = l > 0.5 ? d / (2 - mx - mn) : d / (mx + mn);
            if (mx == r) h = ((g - b) / d + (g < b ? 6 : 0)) / 6;
            else if (mx == g) h = ((b - r) / d + 2) / 6;
            else h = ((r - g) / d + 4) / 6;
        }
        s = Math.Min(0.72, Math.Max(0.38, s));
        l = 0.52;

        double Hue(double p, double q, double t)
        {
            if (t < 0) t += 1;
            if (t > 1) t -= 1;
            if (t < 1.0 / 6) return p + (q - p) * 6 * t;
            if (t < 1.0 / 2) return q;
            if (t < 2.0 / 3) return p + (q - p) * (2.0 / 3 - t) * 6;
            return p;
        }

        var q = l < 0.5 ? l * (1 + s) : l + s - l * s;
        var pp = 2 * l - q;
        var red = (byte)Math.Round(Hue(pp, q, h + 1.0 / 3) * 255);
        var green = (byte)Math.Round(Hue(pp, q, h) * 255);
        var blue = (byte)Math.Round(Hue(pp, q, h - 1.0 / 3) * 255);
        return Color.FromRgb(red, green, blue);
    }
}
