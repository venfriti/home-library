package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/png"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	awsv2 "github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	qrcode "github.com/skip2/go-qrcode"
	"golang.org/x/crypto/bcrypt"
	"golang.org/x/net/webdav"
	"golang.org/x/sys/windows"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
	"golang.org/x/term"
)

// ─── Constants ────────────────────────────────────────────────────────────────

const (
	serviceName       = "stream"
	serviceDispName   = "Stream Video Server"
	cookieName        = "st_sess"
	sessionTTL        = 7 * 24 * time.Hour
	defaultPort       = 5746
	maxLoginFailures  = 10
	loginLockDuration = 15 * time.Minute
)

var videoExts = map[string]bool{
	".mp4": true, ".mkv": true, ".avi": true, ".mov": true,
	".m4v": true, ".flv": true, ".wmv": true, ".webm": true,
}

// ─── Config ───────────────────────────────────────────────────────────────────

type Config struct {
	PasswordHash    string `json:"password_hash"`
	Secret          string `json:"secret"`
	Port            int    `json:"port"`
	LoginDisabled   bool   `json:"login_disabled,omitempty"`
	R2AccountID     string `json:"r2_account_id"`
	R2AccessKey     string `json:"r2_access_key"`
	R2SecretKey     string `json:"r2_secret_key"`
	R2Bucket        string `json:"r2_bucket"`
	R2PublicURL     string `json:"r2_public_url"`
	FFmpegPath      string `json:"ffmpeg_path,omitempty"`
	FFprobePath     string `json:"ffprobe_path,omitempty"`
	LLMProvider     string `json:"llm_provider,omitempty"`
	LLMModel        string `json:"llm_model,omitempty"`
	LLMBaseURL      string `json:"llm_base_url,omitempty"`
	LLMAPIKey       string `json:"llm_api_key,omitempty"`
	TMDBKey         string `json:"tmdb_api_key,omitempty"`
	OpenSubsKey     string `json:"opensubtitles_api_key,omitempty"`
	OpenSubsUser    string `json:"opensubtitles_username,omitempty"`
	OpenSubsPass    string `json:"opensubtitles_password,omitempty"`
	JackettURL      string `json:"jackett_url,omitempty"`
	JackettAPIKey   string `json:"jackett_api_key,omitempty"`
	QBitURL         string `json:"qbittorrent_url,omitempty"`
	QBitUser        string `json:"qbittorrent_username,omitempty"`
	QBitPass        string `json:"qbittorrent_password,omitempty"`
	WatchFolder     string `json:"watch_folder,omitempty"`
	// Folders served raw over the LAN as the "local library" — never pushed to
	// R2/Cloudflare, no transcoding. Played inline when browser-compatible,
	// otherwise opened in a desktop app on the host. Type defaults to "video"
	// (TMDB/LLM movie & TV enrichment); "music_video" routes filename parsing
	// + metadata lookup to iTunes instead, skipping TMDB/Ollama entirely.
	LocalLibraryFolders []LocalFolder `json:"local_library_folders,omitempty"`
	WebDAVPort          int           `json:"webdav_port,omitempty"`
	WebDAVUser          string        `json:"webdav_user,omitempty"`
	WebDAVPass          string        `json:"webdav_pass,omitempty"`
}

type LocalFolder struct {
	Path string `json:"path"`
	Type string `json:"type,omitempty"` // "video" (default) | "music_video"
}

var (
	cfg     Config
	cfgPath string
	logger  *log.Logger
)

// tmdbHTTPClient bounds TMDB requests — package-level http.Get has no
// timeout at all, so a single stalled connection would otherwise hang
// whichever goroutine made it forever.
var tmdbHTTPClient = &http.Client{Timeout: 12 * time.Second}

func loadConfig() error {
	data, err := os.ReadFile(cfgPath)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &cfg)
}

func mustLoadConfig() {
	if err := loadConfig(); err != nil {
		fmt.Fprintln(os.Stderr, "config not found — run: stream.exe setup")
		os.Exit(1)
	}
}

func saveConfig() error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(cfgPath, data, 0600)
}

func ffmpegBin() string {
	if cfg.FFmpegPath != "" {
		return cfg.FFmpegPath
	}
	return "ffmpeg"
}

func ffprobeBin() string {
	if cfg.FFprobePath != "" {
		return cfg.FFprobePath
	}
	return "ffprobe"
}

// ─── Rate limiting ────────────────────────────────────────────────────────────

type rateEntry struct {
	failures  int
	lockedAt  time.Time
}

// ─── Server context ───────────────────────────────────────────────────────────

type serverCtx struct {
	passHash      string
	secret        []byte
	port          int
	r2            *s3.Client
	r2Bucket      string
	r2PubURL      string
	loginDisabled atomic.Bool
	rateMu        sync.Mutex
	rateMap       map[string]*rateEntry
	procMu        sync.Mutex
	procMap       map[string]procState // r2Key → state
	llmProvider   string
	llmModel      string
	llmBaseURL    string
	llmAPIKey     string
	tmdbKey       string
	openSubsKey   string
	openSubsUser  string
	openSubsPass  string
	jackettURL    string
	jackettKey    string
	qbitURL       string
	qbitUser      string
	qbitPass      string
	watchFolder   string
	localFolders  []LocalFolder
}

type procState struct {
	Status string `json:"status"` // "processing" | "error"
	Err    string `json:"error,omitempty"`
}

// ─── Auth ─────────────────────────────────────────────────────────────────────

func (c *serverCtx) sessionMAC(expiry string) string {
	h := hmac.New(sha256.New, c.secret)
	h.Write([]byte(expiry))
	return hex.EncodeToString(h.Sum(nil))
}

func (c *serverCtx) makeSessionValue() string {
	expiry := strconv.FormatInt(time.Now().Add(sessionTTL).Unix(), 10)
	return expiry + ":" + c.sessionMAC(expiry)
}

func (c *serverCtx) validSession(value string) bool {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return false
	}
	expiry, mac := parts[0], parts[1]
	if !hmac.Equal([]byte(mac), []byte(c.sessionMAC(expiry))) {
		return false
	}
	ts, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < ts
}

// fileTokenMAC/makeFileToken/validFileToken mint short-lived, path-scoped
// tokens so a third-party app (VLC, opened via a vlc:// deep link) can fetch
// one local file without ever holding the browser's session cookie.
func (c *serverCtx) fileTokenMAC(path, expiry string) string {
	h := hmac.New(sha256.New, c.secret)
	h.Write([]byte(path))
	h.Write([]byte(":"))
	h.Write([]byte(expiry))
	return hex.EncodeToString(h.Sum(nil))
}

func (c *serverCtx) makeFileToken(path string) string {
	// VLC reuses this same URL — token and all — for every byte-range request
	// it makes while seeking throughout playback, not just the initial fetch.
	// A short TTL here breaks seeking the moment it lapses mid-viewing, so
	// this needs to comfortably outlast one sitting, not just the handoff.
	expiry := strconv.FormatInt(time.Now().Add(12*time.Hour).Unix(), 10)
	return expiry + ":" + c.fileTokenMAC(path, expiry)
}

func (c *serverCtx) validFileToken(path, value string) bool {
	parts := strings.SplitN(value, ":", 2)
	if len(parts) != 2 {
		return false
	}
	expiry, mac := parts[0], parts[1]
	if !hmac.Equal([]byte(mac), []byte(c.fileTokenMAC(path, expiry))) {
		return false
	}
	ts, err := strconv.ParseInt(expiry, 10, 64)
	if err != nil {
		return false
	}
	return time.Now().Unix() < ts
}

func (c *serverCtx) isAuthed(r *http.Request) bool {
	ck, err := r.Cookie(cookieName)
	if err != nil {
		return false
	}
	return c.validSession(ck.Value)
}

// isLocalRequest reports whether a request originated on the local machine /
// LAN rather than through the Cloudflare Tunnel. Tunneled requests always
// carry Cloudflare headers; a direct hit on localhost or a LAN IP does not.
// Used to keep management actions (upload, delete, torrent search, reprocess)
// off the public endpoint while leaving viewing open to everyone.
func isLocalRequest(r *http.Request) bool {
	if r.Header.Get("CF-Connecting-IP") != "" || r.Header.Get("CF-Ray") != "" {
		return false
	}
	host := r.RemoteAddr
	if h, _, err := net.SplitHostPort(host); err == nil {
		host = h
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback() || ip.IsPrivate()
}

// localOnly wraps a handler so it only responds to local/LAN requests.
func (c *serverCtx) localOnly(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLocalRequest(r) {
			http.Error(w, "this action is only available on the local network", http.StatusForbidden)
			return
		}
		h(w, r)
	}
}

func isHTTPS(r *http.Request) bool {
	if r.TLS != nil {
		return true
	}
	if r.Header.Get("X-Forwarded-Proto") == "https" {
		return true
	}
	if v := r.Header.Get("CF-Visitor"); strings.Contains(v, `"https"`) {
		return true
	}
	return false
}

func (c *serverCtx) setSessionCookie(w http.ResponseWriter, r *http.Request) {
	http.SetCookie(w, &http.Cookie{
		Name:     cookieName,
		Value:    c.makeSessionValue(),
		Path:     "/",
		HttpOnly: true,
		Secure:   isHTTPS(r),
		SameSite: http.SameSiteStrictMode,
		MaxAge:   int(sessionTTL.Seconds()),
	})
}

func realIP(r *http.Request) string {
	if ip := r.Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	if ip := r.Header.Get("X-Forwarded-For"); ip != "" {
		return strings.TrimSpace(strings.SplitN(ip, ",", 2)[0])
	}
	host, _, _ := net.SplitHostPort(r.RemoteAddr)
	return host
}

func (c *serverCtx) checkRateLimit(ip string) bool {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	e, ok := c.rateMap[ip]
	if !ok {
		return true
	}
	if !e.lockedAt.IsZero() && time.Since(e.lockedAt) > loginLockDuration {
		delete(c.rateMap, ip)
		return true
	}
	return e.lockedAt.IsZero()
}

func (c *serverCtx) recordFailure(ip string) {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	e := c.rateMap[ip]
	if e == nil {
		e = &rateEntry{}
		c.rateMap[ip] = e
	}
	e.failures++
	if e.failures >= maxLoginFailures {
		e.lockedAt = time.Now()
	}
}

func (c *serverCtx) recordSuccess(ip string) {
	c.rateMu.Lock()
	defer c.rateMu.Unlock()
	delete(c.rateMap, ip)
}

func (c *serverCtx) sessionHoursLeft(r *http.Request) int {
	ck, err := r.Cookie(cookieName)
	if err != nil {
		return 0
	}
	parts := strings.SplitN(ck.Value, ":", 2)
	if len(parts) != 2 {
		return 0
	}
	ts, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0
	}
	left := time.Until(time.Unix(ts, 0))
	if left < 0 {
		return 0
	}
	return int(left.Hours())
}

// ─── R2 ───────────────────────────────────────────────────────────────────────

func newR2Client(accountID, accessKey, secretKey string) *s3.Client {
	return s3.New(s3.Options{
		Region:       "auto",
		Credentials:  credentials.NewStaticCredentialsProvider(accessKey, secretKey, ""),
		BaseEndpoint: awsv2.String(fmt.Sprintf("https://%s.r2.cloudflarestorage.com", accountID)),
		UsePathStyle: true,
	})
}


// ─── FFmpeg helpers ───────────────────────────────────────────────────────────

type tmdbCollection struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type videoMeta struct {
	Duration    int      `json:"duration"`
	Width       int      `json:"width"`
	Height      int      `json:"height"`
	HLSPrefix   string   `json:"hls_prefix,omitempty"`
	Resolutions []string `json:"resolutions,omitempty"`
	VideoCodec  string   `json:"video_codec,omitempty"`
	AudioCodec  string   `json:"audio_codec,omitempty"`
	DirectPlay  bool     `json:"direct_play,omitempty"` // served as a single MP4 (remux), no HLS
	// AI-enriched fields
	Title          string   `json:"title,omitempty"`
	Year           int      `json:"year,omitempty"`
	MediaType      string   `json:"media_type,omitempty"` // "movie" | "tv"
	Season         int      `json:"season,omitempty"`
	Episode        int      `json:"episode,omitempty"`
	Overview       string   `json:"overview,omitempty"`
	PosterURL      string   `json:"poster_url,omitempty"`
	Rating         float64  `json:"rating,omitempty"`
	Genres         []string `json:"genres,omitempty"`
	TMDBID         int      `json:"tmdb_id,omitempty"`
	TMDBBackdrop   string   `json:"tmdb_backdrop,omitempty"`
	EpisodeTitle   string   `json:"episode_title,omitempty"`
	EpisodeOverview string  `json:"episode_overview,omitempty"`
	EpisodeStill   string   `json:"episode_still,omitempty"`
	SubtitleURL    string   `json:"subtitle_url,omitempty"`
	Runtime        int      `json:"runtime,omitempty"` // TMDB's official runtime in minutes (movies only)
	// Music video fields (iTunes Search API, not TMDB) — Title holds the song.
	Artist string `json:"artist,omitempty"`
	Album  string `json:"album,omitempty"`
	// Film-level detail (movies only)
	Director   string          `json:"director,omitempty"`
	Cast       []string        `json:"cast,omitempty"`
	Country    string          `json:"country,omitempty"`
	Language   string          `json:"language,omitempty"`
	Collection *tmdbCollection `json:"collection,omitempty"`
	// Local-library extras classification, decided once during enrichment
	// (see startLocalEnrichment) by comparing a file's own duration against
	// its resolved title's real-world runtime, then persisted here so later
	// scans trust it instead of recomputing a guess every request.
	IsExtra bool   `json:"is_extra,omitempty"`
	MainKey string `json:"main_key,omitempty"`
}

type rendition struct {
	name    string
	height  int
	crf     string
	audioBR string
}

var allRenditions = []rendition{
	{"1080p", 1080, "23", "192k"},
	{"720p", 720, "25", "128k"},
	{"480p", 480, "27", "96k"},
	{"360p", 360, "28", "64k"},
}

func targetRenditions(srcHeight int) []rendition {
	var result []rendition
	for _, r := range allRenditions {
		if srcHeight >= r.height {
			result = append(result, r)
		}
	}
	if len(result) == 0 {
		result = append(result, allRenditions[len(allRenditions)-1])
	}
	// 4K-class sources need much more memory per simultaneous rendition
	// (decode + N parallel scale/encode pipelines via filter_complex). This
	// machine regularly runs with only a few GB available, so for these
	// sources drop to a single rendition — that skips filter_complex
	// entirely in favor of the lighter single-pass encode path.
	maxRends := 3
	if srcHeight >= 1600 {
		maxRends = 1
	}
	if len(result) > maxRends {
		result = result[:maxRends]
	}
	return result
}

// nvencAvailable is set once at startup. When true, HLS encodes use the
// GPU (h264_nvenc) instead of libx264 — far faster and off the CPU/RAM.
var nvencAvailable bool

// probeNvenc runs a tiny throwaway encode to confirm h264_nvenc actually
// initializes on this machine/session (the encoder being *listed* doesn't
// guarantee the GPU is reachable, e.g. under a service session).
func probeNvenc() bool {
	cmd := exec.Command(ffmpegBin(), "-hide_banner",
		"-f", "lavfi", "-i", "testsrc=duration=1:size=256x256:rate=5",
		"-c:v", "h264_nvenc", "-f", "null", "-")
	return cmd.Run() == nil
}

func buildHLSCommand(inPath, hlsDir string, rends []rendition, lowMemory, useNvenc bool) *exec.Cmd {
	n := len(rends)
	args := []string{"-y"}
	if lowMemory && !useNvenc {
		// Caps decode + encode thread count so fewer parallel frame buffers
		// are held at once — 4K/AV1 sources on a memory-constrained machine
		// can OOM otherwise even with a single output rendition. Not needed on
		// the GPU path, which keeps encode off system RAM entirely.
		args = append(args, "-threads", "2")
	}
	if useNvenc {
		// Decode on the GPU too (the RTX card has AV1/HEVC hardware decode),
		// roughly doubling throughput vs CPU decode on 4K AV1 sources.
		args = append(args, "-hwaccel", "cuda")
	}
	args = append(args, "-i", inPath)

	preset := "veryfast"
	if lowMemory {
		preset = "ultrafast"
	}

	if n == 1 {
		// Single rendition — no filter_complex needed. Map explicit single
		// video/audio stream indices: sources with many audio/subtitle
		// tracks (multi-language releases) can otherwise have ambiguous or
		// all-matching default stream selection blow up resource usage.
		args = append(args, "-map", "0:v:0", "-vf", fmt.Sprintf("scale=-2:%d,format=yuv420p", rends[0].height))
		if useNvenc {
			args = append(args, "-c:v", "h264_nvenc", "-preset", "p5",
				"-rc", "vbr", "-cq", rends[0].crf, "-b:v", "0",
				"-g", "60", "-keyint_min", "60")
		} else {
			args = append(args, "-c:v", "libx264", "-preset", preset, "-crf", rends[0].crf,
				"-g", "60", "-keyint_min", "60", "-sc_threshold", "0")
		}
		args = append(args,
			"-map", "0:a:0?",
			"-c:a", "aac", "-b:a", rends[0].audioBR, "-ac", "2",
			"-f", "hls", "-hls_time", "10", "-hls_playlist_type", "vod",
			"-hls_flags", "independent_segments", "-hls_segment_type", "mpegts",
			"-hls_segment_filename", filepath.Join(hlsDir, rends[0].name+"_seg%03d.ts"),
			"-master_pl_name", "master.m3u8",
			"-var_stream_map", "v:0,a:0,name:"+rends[0].name,
			filepath.Join(hlsDir, "%v.m3u8"),
		)
		return exec.Command(ffmpegBin(), args...)
	}

	// Multi-rendition: use filter_complex to split and scale in one pass
	var fc strings.Builder
	fc.WriteString(fmt.Sprintf("[0:v]split=%d", n))
	for i := range rends {
		fc.WriteString(fmt.Sprintf("[vin%d]", i))
	}
	for i, r := range rends {
		fc.WriteString(fmt.Sprintf(";[vin%d]scale=-2:%d,format=yuv420p[vout%d]", i, r.height, i))
	}
	args = append(args, "-filter_complex", fc.String())

	for i, r := range rends {
		args = append(args, "-map", fmt.Sprintf("[vout%d]", i))
		if useNvenc {
			args = append(args,
				fmt.Sprintf("-c:v:%d", i), "h264_nvenc",
				fmt.Sprintf("-preset:v:%d", i), "p5",
				fmt.Sprintf("-rc:v:%d", i), "vbr",
				fmt.Sprintf("-cq:v:%d", i), r.crf,
				fmt.Sprintf("-b:v:%d", i), "0",
				fmt.Sprintf("-g:v:%d", i), "60",
				fmt.Sprintf("-keyint_min:v:%d", i), "60",
			)
		} else {
			args = append(args,
				fmt.Sprintf("-c:v:%d", i), "libx264",
				fmt.Sprintf("-preset:v:%d", i), preset,
				fmt.Sprintf("-crf:%d", i), r.crf,
				fmt.Sprintf("-g:%d", i), "60",
				fmt.Sprintf("-keyint_min:%d", i), "60",
				fmt.Sprintf("-sc_threshold:%d", i), "0",
			)
		}
	}
	for i, r := range rends {
		args = append(args,
			// Explicit index — "0:a?" with no index maps ALL audio streams,
			// which multiplies per rendition for multi-audio-track sources.
			"-map", "0:a:0?",
			fmt.Sprintf("-c:a:%d", i), "aac",
			fmt.Sprintf("-b:a:%d", i), r.audioBR,
			fmt.Sprintf("-ac:%d", i), "2",
		)
	}

	var streamMap strings.Builder
	for i, r := range rends {
		if i > 0 {
			streamMap.WriteString(" ")
		}
		streamMap.WriteString(fmt.Sprintf("v:%d,a:%d,name:%s", i, i, r.name))
	}
	args = append(args,
		"-f", "hls",
		"-hls_time", "10",
		"-hls_playlist_type", "vod",
		"-hls_flags", "independent_segments",
		"-hls_segment_type", "mpegts",
		"-hls_segment_filename", filepath.Join(hlsDir, "%v_seg%03d.ts"),
		"-master_pl_name", "master.m3u8",
		"-var_stream_map", streamMap.String(),
		filepath.Join(hlsDir, "%v.m3u8"),
	)
	return exec.Command(ffmpegBin(), args...)
}

func buildDownloadMP4Args(inPath, outPath string) []string {
	// Stream-copy video (no re-encode), transcode audio to AAC, faststart for progressive download
	return []string{
		"-y", "-i", inPath,
		"-c:v", "copy",
		"-c:a", "aac", "-b:a", "192k", "-ac", "2",
		"-movflags", "+faststart",
		outPath,
	}
}

// browserVideoCodecs lists video codecs that play natively in a <video> tag
// once repackaged into an MP4 — so we can remux (stream-copy) instead of
// re-encoding. AV1/HEVC are excluded: Safari/iOS can't decode them, so those
// still go through the HLS transcode path.
var browserVideoCodecs = map[string]bool{"h264": true}

// buildRemuxArgs repackages a browser-playable source into a faststart MP4
// without re-encoding the video. Audio is stream-copied when already AAC,
// otherwise transcoded to AAC (cheap relative to a video encode).
func buildRemuxArgs(inPath, outPath, audioCodec string) []string {
	aCodec := "copy"
	if audioCodec != "aac" {
		aCodec = "aac"
	}
	args := []string{
		"-y", "-i", inPath,
		"-map", "0:v:0", "-map", "0:a:0?",
		"-c:v", "copy",
		"-c:a", aCodec,
	}
	if aCodec == "aac" {
		args = append(args, "-b:a", "192k", "-ac", "2")
	}
	args = append(args, "-movflags", "+faststart", outPath)
	return args
}

func probeVideo(path string) (videoMeta, bool, string) {
	cmd := exec.Command(ffprobeBin(),
		"-v", "error",
		"-print_format", "json",
		"-show_streams",
		"-show_format",
		path,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return videoMeta{}, false, fmt.Sprintf("ffprobe exit: %v\noutput: %s", err, string(out))
	}
	var result struct {
		Streams []struct {
			CodecType string `json:"codec_type"`
			CodecName string `json:"codec_name"`
			Width     int    `json:"width"`
			Height    int    `json:"height"`
		} `json:"streams"`
		Format struct {
			Duration string `json:"duration"`
		} `json:"format"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return videoMeta{}, false, fmt.Sprintf("json parse: %v\nraw: %s", err, string(out))
	}
	var meta videoMeta
	isVideo := false
	for _, s := range result.Streams {
		if s.CodecType == "video" && !isVideo {
			isVideo = true
			meta.Width = s.Width
			meta.Height = s.Height
			meta.VideoCodec = s.CodecName
		}
		if s.CodecType == "audio" && meta.AudioCodec == "" {
			meta.AudioCodec = s.CodecName
		}
	}
	if !isVideo {
		return videoMeta{}, false, fmt.Sprintf("no video stream found\nstreams: %s", string(out))
	}
	if dur, err := strconv.ParseFloat(result.Format.Duration, 64); err == nil {
		meta.Duration = int(dur)
	}
	return meta, true, ""
}

// textSubtitleCodecs lists subtitle codecs FFmpeg can re-encode to WebVTT.
// Bitmap formats like PGS/VobSub aren't included — they'd need OCR.
var textSubtitleCodecs = map[string]bool{
	"subrip": true, "srt": true, "ass": true, "ssa": true, "mov_text": true, "webvtt": true, "text": true,
}

// findEmbeddedSubtitle returns the absolute stream index of the best text
// subtitle track in the source (preferring English), or found=false if the
// source has no convertible embedded subtitles.
func findEmbeddedSubtitle(path string) (index int, found bool) {
	cmd := exec.Command(ffprobeBin(),
		"-v", "error",
		"-print_format", "json",
		"-show_entries", "stream=index,codec_name:stream_tags=language",
		"-select_streams", "s",
		path,
	)
	out, err := cmd.Output()
	if err != nil {
		return 0, false
	}
	var result struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecName string `json:"codec_name"`
			Tags      struct {
				Language string `json:"language"`
			} `json:"tags"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return 0, false
	}
	for _, s := range result.Streams {
		if !textSubtitleCodecs[s.CodecName] {
			continue
		}
		if !found {
			index, found = s.Index, true
		}
		if s.Tags.Language == "eng" || s.Tags.Language == "en" {
			return s.Index, true
		}
	}
	return index, found
}

func extractEmbeddedSubtitle(inPath string, streamIndex int, outPath string) error {
	cmd := exec.Command(ffmpegBin(), "-y", "-i", inPath,
		"-map", fmt.Sprintf("0:%d", streamIndex),
		"-c:s", "webvtt",
		outPath,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, truncateForLog(out))
	}
	return nil
}

// localSubtitleCache maps a local video's absolute path to its cached
// WebVTT subtitle file's absolute path, or "" once confirmed to have none —
// findEmbeddedSubtitle spawns ffprobe, so a confirmed-absent result is worth
// remembering instead of re-probing on every modal open.
var (
	localSubtitleMu    sync.Mutex
	localSubtitleCache = map[string]string{}
)

func localSubtitleCacheDir() string {
	dir := filepath.Join(filepath.Dir(cfgPath), "local_subs_cache")
	os.MkdirAll(dir, 0755)
	return dir
}

// detectLocalSubtitlePath finds or creates a cached WebVTT subtitle for a
// local video file, preferring an exact-stem sidecar .srt/.vtt (the most
// explicit "this is the subtitle for this file" signal a release can carry,
// and free of any ffmpeg work for an already-.vtt sidecar) over an embedded
// text subtitle stream. Returns "" if there's nothing to show.
func detectLocalSubtitlePath(absVideoPath string) string {
	localSubtitleMu.Lock()
	if p, ok := localSubtitleCache[absVideoPath]; ok {
		localSubtitleMu.Unlock()
		return p
	}
	localSubtitleMu.Unlock()

	stem := strings.TrimSuffix(absVideoPath, filepath.Ext(absVideoPath))
	cachePath := filepath.Join(localSubtitleCacheDir(), hex.EncodeToString(sha256Sum(absVideoPath))+".vtt")
	result := ""

	switch {
	case fileExists(cachePath):
		result = cachePath
	case fileExists(stem + ".vtt"):
		if data, err := os.ReadFile(stem + ".vtt"); err == nil {
			os.WriteFile(cachePath, data, 0644)
			result = cachePath
		}
	case fileExists(stem + ".srt"):
		if data, err := os.ReadFile(stem + ".srt"); err == nil {
			os.WriteFile(cachePath, srtToVTT(data), 0644)
			result = cachePath
		}
	default:
		if idx, found := findEmbeddedSubtitle(absVideoPath); found {
			if err := extractEmbeddedSubtitle(absVideoPath, idx, cachePath); err == nil {
				result = cachePath
			} else {
				logger.Printf("embedded subtitle extract %s: %v", absVideoPath, err)
			}
		}
	}

	localSubtitleMu.Lock()
	localSubtitleCache[absVideoPath] = result
	localSubtitleMu.Unlock()
	return result
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

func sha256Sum(s string) []byte {
	h := sha256.Sum256([]byte(s))
	return h[:]
}

// localThumbnailCache maps a local video's absolute path to its cached
// still-frame thumbnail's absolute path, or "" once confirmed unextractable.
var (
	localThumbnailMu    sync.Mutex
	localThumbnailCache = map[string]string{}
)

func localThumbnailCacheDir() string {
	dir := filepath.Join(filepath.Dir(cfgPath), "local_thumbs_cache")
	os.MkdirAll(dir, 0755)
	return dir
}

// localThumbnailPath finds or creates a cached still frame pulled from the
// video itself — used for music video cards, which otherwise had nothing
// but an iTunes album-cover for a thumbnail, a fundamentally different
// piece of art than what's actually playing. Returns "" if extraction fails.
func localThumbnailPath(absVideoPath string) string {
	localThumbnailMu.Lock()
	if p, ok := localThumbnailCache[absVideoPath]; ok {
		localThumbnailMu.Unlock()
		return p
	}
	localThumbnailMu.Unlock()

	cachePath := filepath.Join(localThumbnailCacheDir(), hex.EncodeToString(sha256Sum(absVideoPath))+".jpg")
	result := cachePath
	if !fileExists(cachePath) {
		meta, isVideo, _ := probeVideo(absVideoPath)
		dur := 0
		if isVideo {
			dur = meta.Duration
		}
		if err := extractThumbnail(absVideoPath, cachePath, dur); err != nil || !fileExists(cachePath) {
			result = ""
		}
	}
	localThumbnailMu.Lock()
	localThumbnailCache[absVideoPath] = result
	localThumbnailMu.Unlock()
	return result
}

func extractThumbnail(inPath, outPath string, durationSec int) error {
	seek := "00:00:05"
	if durationSec > 0 && durationSec < 10 {
		seek = "00:00:00"
	}
	return exec.Command(ffmpegBin(),
		"-y",
		"-ss", seek,
		"-i", inPath,
		"-frames:v", "1",
		"-q:v", "3",
		"-vf", "scale=640:-1",
		outPath,
	).Run()
}

// ─── LLM + TMDB enrichment ───────────────────────────────────────────────────

type parsedMedia struct {
	Title   string `json:"title"`
	Year    int    `json:"year"`
	Type    string `json:"type"`    // "movie" | "tv"
	Season  int    `json:"season"`
	Episode int    `json:"episode"`
}

func (c *serverCtx) llmChat(prompt string) (string, error) {
	baseURL := c.llmBaseURL
	if baseURL == "" {
		switch c.llmProvider {
		case "groq":
			baseURL = "https://api.groq.com/openai"
		case "gemini":
			baseURL = "https://generativelanguage.googleapis.com/v1beta/openai"
		default: // ollama
			baseURL = "http://localhost:11434"
		}
	}
	model := c.llmModel
	if model == "" {
		switch c.llmProvider {
		case "groq":
			model = "llama-3.3-70b-versatile"
		case "gemini":
			model = "gemini-2.0-flash"
		default:
			model = "llama3.2:3b"
		}
	}
	body, _ := json.Marshal(map[string]interface{}{
		"model":       model,
		"temperature": 0.1,
		"messages":    []map[string]string{{"role": "user", "content": prompt}},
	})
	req, err := http.NewRequest("POST", baseURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	if c.llmAPIKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.llmAPIKey)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var out struct {
		Choices []struct {
			Message struct{ Content string `json:"content"` } `json:"message"`
		} `json:"choices"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return "", err
	}
	if len(out.Choices) == 0 {
		return "", fmt.Errorf("empty LLM response")
	}
	return strings.TrimSpace(out.Choices[0].Message.Content), nil
}

// seasonEpisodeRe matches the standard "SxxEyy" release-naming marker. When
// present, season/episode/type are pulled from it directly instead of trusting
// the LLM — a 3B model reliably gets the *numbers* right but, since the
// episode title text usually comes right after this marker (e.g.
// "Atlanta.S02E01.Alligator.Man..."), it unreliably pollutes the show title
// with the episode title, or misses that "tv" applies, sending the TMDB
// lookup to the wrong endpoint entirely. Anchoring on the regex match and
// only asking the LLM to clean up the text *before* it removes both
// failure modes at the source.
var seasonEpisodeRe = regexp.MustCompile(`(?i)\bS(\d{1,2})E(\d{1,3})\b`)

func (c *serverCtx) parseMediaFilename(filename string) (parsedMedia, error) {
	name := strings.TrimSuffix(filename, filepath.Ext(filename))

	if m := seasonEpisodeRe.FindStringSubmatchIndex(name); m != nil {
		season, _ := strconv.Atoi(name[m[2]:m[3]])
		episode, _ := strconv.Atoi(name[m[4]:m[5]])
		titleHint := strings.Trim(name[:m[0]], " .-_")
		prompt := fmt.Sprintf(`You are a media filename parser. This is a TV show release name; the part
before the season/episode code is the show title, optionally followed by a
year. Reply with ONLY a raw JSON object — no markdown, no explanation.
Fields: title (string, the show title only), year (int, 0 if unknown).

Examples:
"Arcane (2021)" → {"title":"Arcane","year":2021}
"Atlanta" → {"title":"Atlanta","year":0}
"Breaking.Bad" → {"title":"Breaking Bad","year":0}

Text: %q`, titleHint)

		content, err := c.llmChat(prompt)
		if err != nil {
			return parsedMedia{}, err
		}
		if i := strings.Index(content, "{"); i >= 0 {
			if j := strings.LastIndex(content, "}"); j >= i {
				content = content[i : j+1]
			}
		}
		var p parsedMedia
		if err := json.Unmarshal([]byte(content), &p); err != nil {
			return parsedMedia{}, fmt.Errorf("parse LLM JSON: %v | raw: %s", err, content)
		}
		p.Type = "tv"
		p.Season = season
		p.Episode = episode
		return p, nil
	}

	prompt := fmt.Sprintf(`You are a media filename parser. Extract info from this video filename.
Reply with ONLY a raw JSON object — no markdown, no explanation.
Fields: title (string), year (int, 0 if unknown), type ("movie" or "tv"), season (int, 0 if movie/unknown), episode (int, 0 if movie/unknown).

Examples:
"The.Dark.Knight.2008.1080p.BluRay" → {"title":"The Dark Knight","year":2008,"type":"movie","season":0,"episode":0}
"Breaking.Bad.S02E05.720p" → {"title":"Breaking Bad","year":0,"type":"tv","season":2,"episode":5}
"Invincible.Presenting.Atom.Eve.Special.Episode.1080p.AMZN" → {"title":"Invincible Presenting Atom Eve","year":0,"type":"tv","season":0,"episode":0}

Filename: %q`, name)

	content, err := c.llmChat(prompt)
	if err != nil {
		return parsedMedia{}, err
	}
	// Strip markdown code fences if the model added them
	if i := strings.Index(content, "{"); i >= 0 {
		if j := strings.LastIndex(content, "}"); j >= i {
			content = content[i : j+1]
		}
	}
	var p parsedMedia
	if err := json.Unmarshal([]byte(content), &p); err != nil {
		return parsedMedia{}, fmt.Errorf("parse LLM JSON: %v | raw: %s", err, content)
	}
	return p, nil
}

// buildVideoMetaForID fetches the full detail record (genres, backdrop,
// runtime, and — for a TV episode — the episode-specific title/overview/
// still) for an already-known TMDB id, and assembles it into a videoMeta.
func (c *serverCtx) buildVideoMetaForID(id int, title string, year int, overview, posterPath string, rating float64, mediaType string, season, episode int) videoMeta {
	if mediaType != "tv" {
		mediaType = "movie"
	}
	var genres []string
	var backdropURL string
	var runtime int
	var director string
	var cast []string
	var country, language string
	var collection *tmdbCollection
	if dr, err := tmdbHTTPClient.Get(fmt.Sprintf("https://api.themoviedb.org/3/%s/%d?api_key=%s", mediaType, id, c.tmdbKey)); err == nil {
		defer dr.Body.Close()
		var d struct {
			Genres       []struct{ Name string `json:"name"` } `json:"genres"`
			BackdropPath string                               `json:"backdrop_path"`
			Runtime      int                                  `json:"runtime"`
			ProductionCountries []struct {
				Name string `json:"name"`
			} `json:"production_countries"`
			SpokenLanguages []struct {
				EnglishName string `json:"english_name"`
			} `json:"spoken_languages"`
			BelongsToCollection *struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"belongs_to_collection"`
		}
		if json.NewDecoder(dr.Body).Decode(&d) == nil {
			for _, g := range d.Genres {
				genres = append(genres, g.Name)
			}
			if d.BackdropPath != "" {
				backdropURL = "https://image.tmdb.org/t/p/w1280" + d.BackdropPath
			}
			runtime = d.Runtime
			if len(d.ProductionCountries) > 0 {
				country = d.ProductionCountries[0].Name
			}
			if len(d.SpokenLanguages) > 0 {
				language = d.SpokenLanguages[0].EnglishName
			}
			if d.BelongsToCollection != nil {
				collection = &tmdbCollection{ID: d.BelongsToCollection.ID, Name: d.BelongsToCollection.Name}
			}
		}
	}

	// Credits (movies only — director + top cast)
	if mediaType == "movie" {
		credURL := fmt.Sprintf("https://api.themoviedb.org/3/movie/%d/credits?api_key=%s", id, c.tmdbKey)
		if cr, err := tmdbHTTPClient.Get(credURL); err == nil {
			defer cr.Body.Close()
			var cred struct {
				Crew []struct {
					Job  string `json:"job"`
					Name string `json:"name"`
				} `json:"crew"`
				Cast []struct {
					Name string `json:"name"`
				} `json:"cast"`
			}
			if json.NewDecoder(cr.Body).Decode(&cred) == nil {
				for _, p := range cred.Crew {
					if p.Job == "Director" {
						director = p.Name
						break
					}
				}
				for i, p := range cred.Cast {
					if i >= 5 {
						break
					}
					cast = append(cast, p.Name)
				}
			}
		}
	}

	var epTitle, epOverview, epStill string
	if mediaType == "tv" && season > 0 && episode > 0 {
		epURL := fmt.Sprintf("https://api.themoviedb.org/3/tv/%d/season/%d/episode/%d?api_key=%s",
			id, season, episode, c.tmdbKey)
		if er, err := tmdbHTTPClient.Get(epURL); err == nil {
			defer er.Body.Close()
			var ep struct {
				Name      string `json:"name"`
				Overview  string `json:"overview"`
				StillPath string `json:"still_path"`
			}
			if json.NewDecoder(er.Body).Decode(&ep) == nil {
				epTitle = ep.Name
				epOverview = ep.Overview
				if ep.StillPath != "" {
					epStill = "https://image.tmdb.org/t/p/w500" + ep.StillPath
				}
			}
		}
	}

	posterURL := ""
	if posterPath != "" {
		posterURL = "https://image.tmdb.org/t/p/w500" + posterPath
	}
	return videoMeta{
		Title:           title,
		Year:            year,
		MediaType:       mediaType,
		Season:          season,
		Episode:         episode,
		Overview:        overview,
		PosterURL:       posterURL,
		Rating:          rating,
		Genres:          genres,
		TMDBID:          id,
		TMDBBackdrop:    backdropURL,
		EpisodeTitle:    epTitle,
		EpisodeOverview: epOverview,
		EpisodeStill:    epStill,
		Runtime:         runtime,
		Director:        director,
		Cast:            cast,
		Country:         country,
		Language:        language,
		Collection:      collection,
	}
}

func (c *serverCtx) fetchTMDB(p parsedMedia) (videoMeta, error) {
	mediaType := p.Type
	if mediaType != "tv" {
		mediaType = "movie"
	}
	q := url.Values{"api_key": {c.tmdbKey}, "query": {p.Title}}
	if p.Year > 0 {
		if mediaType == "movie" {
			q.Set("primary_release_year", strconv.Itoa(p.Year))
		} else {
			q.Set("first_air_date_year", strconv.Itoa(p.Year))
		}
	}
	searchURL := "https://api.themoviedb.org/3/search/" + mediaType + "?" + q.Encode()
	resp, err := http.Get(searchURL)
	if err != nil {
		return videoMeta{}, err
	}
	defer resp.Body.Close()
	var sr struct {
		Results []struct {
			ID           int     `json:"id"`
			Title        string  `json:"title"`
			Name         string  `json:"name"`
			Overview     string  `json:"overview"`
			PosterPath   string  `json:"poster_path"`
			VoteAverage  float64 `json:"vote_average"`
			ReleaseDate  string  `json:"release_date"`
			FirstAirDate string  `json:"first_air_date"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil || len(sr.Results) == 0 {
		return videoMeta{}, fmt.Errorf("no TMDB results for %q", p.Title)
	}
	r := sr.Results[0]
	title := r.Title
	if title == "" {
		title = r.Name
	}
	dateStr := r.ReleaseDate
	if dateStr == "" {
		dateStr = r.FirstAirDate
	}
	year := p.Year
	if year == 0 && len(dateStr) >= 4 {
		year, _ = strconv.Atoi(dateStr[:4])
	}
	return c.buildVideoMetaForID(r.ID, title, year, r.Overview, r.PosterPath, r.VoteAverage, mediaType, p.Season, p.Episode), nil
}

// musicNoiseRe strips common music-video filename annotations (quality tags,
// "Official Video"-style suffixes) that would otherwise pollute the search
// query or end up stuck in the parsed song title.
var musicNoiseRe = regexp.MustCompile(`(?i)[\(\[]\s*(official\s*(music\s*)?(video|audio)|lyric\s*video|live\s*video|hd|4k|explicit|\d{3,4}p)[^\)\]]*[\)\]]`)
var musicTrailingNumRe = regexp.MustCompile(`_\d+$`)

// cleanExtraTitle turns a bonus-feature filename into a display label
// without any lookup — there's no database of Blu-ray extras to match
// against, so the filename (minus extension, with separators normalized) is
// the best title available.
func cleanExtraTitle(filename string) string {
	name := strings.TrimSuffix(filename, filepath.Ext(filename))
	name = strings.ReplaceAll(name, "_", " ")
	name = strings.ReplaceAll(name, ".", " ")
	return strings.Join(strings.Fields(name), " ")
}

// parseMusicVideoFilename extracts "Artist" and "Song" from the near-universal
// "Artist - Song Title (Official Video).ext" music-video naming convention.
// Pure string work — no LLM call needed, this format is regular enough that
// a model would just add latency without improving on a regex split.
func parseMusicVideoFilename(filename string) (artist, song string) {
	name := strings.TrimSuffix(filename, filepath.Ext(filename))
	name = musicTrailingNumRe.ReplaceAllString(name, "")
	name = strings.ReplaceAll(name, "_", " ")
	name = musicNoiseRe.ReplaceAllString(name, "")
	for _, dash := range []string{"—", "–"} {
		name = strings.ReplaceAll(name, dash, "-")
	}
	parts := strings.SplitN(name, "-", 2)
	if len(parts) != 2 {
		return strings.TrimSpace(name), ""
	}
	return strings.TrimSpace(parts[0]), strings.TrimSpace(parts[1])
}


func (c *serverCtx) enrichMeta(filename string, meta *videoMeta) {
	if c.llmProvider == "" {
		return
	}
	parsed, err := c.parseMediaFilename(filename)
	if err != nil {
		logger.Printf("LLM parse %s: %v", filename, err)
		return
	}
	logger.Printf("LLM parsed %q → %+v", filename, parsed)
	if c.tmdbKey == "" {
		// No TMDB — at least store what we got from LLM
		meta.Title = parsed.Title
		meta.Year = parsed.Year
		meta.MediaType = parsed.Type
		meta.Season = parsed.Season
		meta.Episode = parsed.Episode
		return
	}
	info, err := c.fetchTMDB(parsed)
	if err != nil {
		logger.Printf("TMDB %s: %v", filename, err)
		// Fall back to LLM-only data
		meta.Title = parsed.Title
		meta.Year = parsed.Year
		meta.MediaType = parsed.Type
		return
	}
	meta.Title = info.Title
	meta.Year = info.Year
	meta.MediaType = info.MediaType
	meta.Season = info.Season
	meta.Episode = info.Episode
	meta.Overview = info.Overview
	meta.PosterURL = info.PosterURL
	meta.Rating = info.Rating
	meta.Genres = info.Genres
	meta.TMDBID = info.TMDBID
	meta.TMDBBackdrop = info.TMDBBackdrop
	meta.EpisodeTitle = info.EpisodeTitle
	meta.EpisodeOverview = info.EpisodeOverview
	meta.EpisodeStill = info.EpisodeStill
	meta.Runtime = info.Runtime
	logger.Printf("enriched %s → %s (%d) %.1f★", filename, meta.Title, meta.Year, meta.Rating)
}

// ─── Search & Download ────────────────────────────────────────────────────────

type tmdbSearchHit struct {
	ID         int     `json:"id"`
	Title      string  `json:"title"`
	Year       int     `json:"year"`
	MediaType  string  `json:"media_type"`
	PosterURL  string  `json:"poster_url"`
	PosterPath string  `json:"-"` // raw path, for re-fetching full detail at a different size
	Overview   string  `json:"overview"`
	Rating     float64 `json:"rating"`
}

type torrentResult struct {
	Title      string `json:"title"`
	MagnetURI  string `json:"magnet_uri"`
	TorrentURL string `json:"torrent_url"`
	Seeders    int    `json:"seeders"`
	Size       int64  `json:"size"`
	Resolution string `json:"resolution"`
	Source     string `json:"source"`
	Tracker    string `json:"tracker"`
}

func (c *serverCtx) searchTMDBMulti(query string) ([]tmdbSearchHit, error) {
	if c.tmdbKey == "" {
		return nil, fmt.Errorf("tmdb not configured")
	}
	resp, err := tmdbHTTPClient.Get(fmt.Sprintf("https://api.themoviedb.org/3/search/multi?api_key=%s&query=%s&page=1",
		c.tmdbKey, url.QueryEscape(query)))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var sr struct {
		Results []struct {
			ID           int     `json:"id"`
			MediaType    string  `json:"media_type"`
			Title        string  `json:"title"`
			Name         string  `json:"name"`
			Overview     string  `json:"overview"`
			PosterPath   string  `json:"poster_path"`
			VoteAverage  float64 `json:"vote_average"`
			ReleaseDate  string  `json:"release_date"`
			FirstAirDate string  `json:"first_air_date"`
		} `json:"results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}
	var hits []tmdbSearchHit
	for _, r := range sr.Results {
		if r.MediaType == "person" {
			continue
		}
		title := r.Title
		if title == "" {
			title = r.Name
		}
		dateStr := r.ReleaseDate
		if dateStr == "" {
			dateStr = r.FirstAirDate
		}
		year := 0
		if len(dateStr) >= 4 {
			year, _ = strconv.Atoi(dateStr[:4])
		}
		posterURL := ""
		if r.PosterPath != "" {
			posterURL = "https://image.tmdb.org/t/p/w300" + r.PosterPath
		}
		hits = append(hits, tmdbSearchHit{
			ID:         r.ID,
			Title:      title,
			Year:       year,
			MediaType:  r.MediaType,
			PosterURL:  posterURL,
			PosterPath: r.PosterPath,
			Overview:   r.Overview,
			Rating:     r.VoteAverage,
		})
		if len(hits) == 8 {
			break
		}
	}
	return hits, nil
}

var torrentMetaTokenRe = regexp.MustCompile(`^((19|20)\d{2}|s\d{1,2}(e\d{1,3})?|\d{3,4}p|x264|x265|h264|h265|hevc|av1|web|webrip|webdl|bluray|bdrip|hdtv|dvdrip|complete|season|ep|episode|uhd|hdr|10bit|6ch|2ch|amzn|nf|hulu|dsnp)$`)

// normalizeTitle lowercases, strips a leading article, and collapses
// non-alphanumeric runs to single spaces, so torrent release names can be
// compared against a clean show/movie title.
func normalizeTitle(s string) string {
	s = strings.ToLower(s)
	for _, art := range []string{"the ", "a ", "an "} {
		if strings.HasPrefix(s, art) {
			s = s[len(art):]
			break
		}
	}
	var b strings.Builder
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		} else {
			b.WriteRune(' ')
		}
	}
	return strings.Join(strings.Fields(b.String()), " ")
}

// isRelevantTorrent rejects results that merely contain the show/movie name
// as a coincidental substring (e.g. "The Invincible Samurai" when searching
// "Invincible"). A release is relevant if its title starts with the search
// title and is immediately followed by release metadata (year, SxxEyy,
// resolution, codec, etc.) rather than more title words.
func isRelevantTorrent(resultTitle, showTitle string) bool {
	normShow := normalizeTitle(showTitle)
	if normShow == "" {
		return true
	}
	normResult := normalizeTitle(resultTitle)
	if !strings.HasPrefix(normResult, normShow) {
		return false
	}
	rest := strings.TrimSpace(strings.TrimPrefix(normResult, normShow))
	if rest == "" {
		return true
	}
	return torrentMetaTokenRe.MatchString(strings.Fields(rest)[0])
}

func parseTorrentQuality(title string) (resolution, source string) {
	t := strings.ToUpper(title)
	for _, r := range []string{"2160P", "4K", "UHD", "1080P", "720P", "480P", "360P"} {
		if strings.Contains(t, r) {
			resolution = strings.ToLower(strings.ReplaceAll(r, "UHD", "2160p"))
			break
		}
	}
	for _, s := range []string{"BLURAY", "BLU-RAY", "BDRIP", "WEB-DL", "WEBDL", "WEBRIP", "HDTV", "CAM"} {
		if strings.Contains(t, s) {
			source = s
			break
		}
	}
	return
}

func (c *serverCtx) searchJackett(query, titleFilter string) ([]torrentResult, error) {
	if c.jackettURL == "" || c.jackettKey == "" {
		return nil, fmt.Errorf("jackett not configured")
	}
	params := url.Values{
		"apikey":   {c.jackettKey},
		"Query":    {query},
		"Category": {"2000,5000,5030,5040"},
	}
	resp, err := http.Get(c.jackettURL + "/api/v2.0/indexers/all/results?" + params.Encode())
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var jr struct {
		Results []struct {
			Title     string `json:"Title"`
			MagnetUri string `json:"MagnetUri"`
			Link      string `json:"Link"`
			Seeders   int    `json:"Seeders"`
			Size      int64  `json:"Size"`
			TrackerId string `json:"TrackerId"`
		} `json:"Results"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&jr); err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var results []torrentResult
	for _, r := range jr.Results {
		if r.MagnetUri == "" && r.Link == "" {
			continue
		}
		key := r.Title + r.TrackerId
		if seen[key] {
			continue
		}
		seen[key] = true
		res, src := parseTorrentQuality(r.Title)
		results = append(results, torrentResult{
			Title:      r.Title,
			MagnetURI:  r.MagnetUri,
			TorrentURL: r.Link,
			Seeders:    r.Seeders,
			Size:       r.Size,
			Resolution: res,
			Source:     src,
			Tracker:    r.TrackerId,
		})
	}
	if titleFilter != "" {
		var relevant []torrentResult
		for _, res := range results {
			if isRelevantTorrent(res.Title, titleFilter) {
				relevant = append(relevant, res)
			}
		}
		if len(relevant) > 0 {
			results = relevant
		}
	}
	// Sort: seeders desc, prefer 1080p
	sort.Slice(results, func(i, j int) bool {
		si, sj := results[i].Seeders, results[j].Seeders
		ri, rj := results[i].Resolution, results[j].Resolution
		scoreFn := func(res string, seeders int) int {
			s := seeders
			switch res {
			case "1080p":
				s += 2000
			case "720p":
				s += 1000
			case "2160p":
				s += 1500
			}
			return s
		}
		return scoreFn(ri, si) > scoreFn(rj, sj)
	})
	if len(results) > 30 {
		results = results[:30]
	}
	return results, nil
}

func (c *serverCtx) qbitAddTorrent(magnetURI, torrentURL string) error {
	if c.qbitURL == "" {
		return fmt.Errorf("qbittorrent not configured")
	}
	client := &http.Client{Timeout: 10 * time.Second}

	// Login
	loginForm := url.Values{"username": {c.qbitUser}, "password": {c.qbitPass}}
	loginResp, err := client.PostForm(c.qbitURL+"/api/v2/auth/login", loginForm)
	if err != nil {
		return fmt.Errorf("qbit login: %v", err)
	}
	defer loginResp.Body.Close()
	var sessionCookie *http.Cookie
	for _, ck := range loginResp.Cookies() {
		if ck.Name == "SID" {
			sessionCookie = ck
			break
		}
	}
	if sessionCookie == nil {
		return fmt.Errorf("qbit login failed — check credentials")
	}

	// Add torrent
	savePath := c.watchFolder
	if savePath == "" {
		savePath = os.TempDir()
	}
	form := url.Values{
		"savepath": {savePath},
		"category": {"stream"},
	}
	if magnetURI != "" {
		form.Set("urls", magnetURI)
	} else {
		form.Set("urls", torrentURL)
	}
	req, _ := http.NewRequest("POST", c.qbitURL+"/api/v2/torrents/add",
		strings.NewReader(form.Encode()))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.AddCookie(sessionCookie)
	addResp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("qbit add: %v", err)
	}
	defer addResp.Body.Close()
	body, _ := io.ReadAll(addResp.Body)
	if string(body) != "Ok." {
		return fmt.Errorf("qbit add unexpected response: %s", string(body))
	}
	return nil
}

type tmdbSeasonHit struct {
	Number       int    `json:"season_number"`
	Name         string `json:"name"`
	EpisodeCount int    `json:"episode_count"`
}

func (c *serverCtx) fetchTVSeasons(tmdbID int) ([]tmdbSeasonHit, error) {
	if c.tmdbKey == "" {
		return nil, fmt.Errorf("tmdb not configured")
	}
	resp, err := http.Get(fmt.Sprintf("https://api.themoviedb.org/3/tv/%d?api_key=%s", tmdbID, c.tmdbKey))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var d struct {
		Seasons []struct {
			SeasonNumber int    `json:"season_number"`
			Name         string `json:"name"`
			EpisodeCount int    `json:"episode_count"`
		} `json:"seasons"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	var out []tmdbSeasonHit
	for _, s := range d.Seasons {
		if s.SeasonNumber == 0 {
			continue // skip "Specials"
		}
		out = append(out, tmdbSeasonHit{Number: s.SeasonNumber, Name: s.Name, EpisodeCount: s.EpisodeCount})
	}
	return out, nil
}

type tmdbEpisodeHit struct {
	Number int    `json:"episode_number"`
	Name   string `json:"name"`
}

func (c *serverCtx) fetchSeasonEpisodes(tmdbID, season int) ([]tmdbEpisodeHit, error) {
	if c.tmdbKey == "" {
		return nil, fmt.Errorf("tmdb not configured")
	}
	resp, err := http.Get(fmt.Sprintf("https://api.themoviedb.org/3/tv/%d/season/%d?api_key=%s", tmdbID, season, c.tmdbKey))
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var d struct {
		Episodes []struct {
			EpisodeNumber int    `json:"episode_number"`
			Name          string `json:"name"`
		} `json:"episodes"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&d); err != nil {
		return nil, err
	}
	var out []tmdbEpisodeHit
	for _, e := range d.Episodes {
		out = append(out, tmdbEpisodeHit{Number: e.EpisodeNumber, Name: e.Name})
	}
	return out, nil
}

func (c *serverCtx) handleTMDBSeasons(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	if id == 0 {
		http.Error(w, "missing id", 400)
		return
	}
	seasons, err := c.fetchTVSeasons(id)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(seasons)
}

func (c *serverCtx) handleTMDBEpisodes(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	id, _ := strconv.Atoi(r.URL.Query().Get("id"))
	season, _ := strconv.Atoi(r.URL.Query().Get("season"))
	if id == 0 {
		http.Error(w, "missing id", 400)
		return
	}
	episodes, err := c.fetchSeasonEpisodes(id, season)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(episodes)
}

func (c *serverCtx) handleSearchTMDB(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	q := r.URL.Query().Get("q")
	if q == "" {
		http.Error(w, "missing q", 400)
		return
	}
	hits, err := c.searchTMDBMulti(q)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(hits)
}

func (c *serverCtx) handleSearchTorrents(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	q := r.URL.Query().Get("q")
	baseTitle := q
	year := r.URL.Query().Get("year")
	season := r.URL.Query().Get("season")
	episode := r.URL.Query().Get("episode")
	if season != "" && season != "0" {
		if episode != "" && episode != "0" {
			s, _ := strconv.Atoi(season)
			e, _ := strconv.Atoi(episode)
			q = fmt.Sprintf("%s S%02dE%02d", q, s, e)
		} else {
			q = fmt.Sprintf("%s Season %s", q, season)
		}
	} else if year != "" && year != "0" {
		q = fmt.Sprintf("%s %s", q, year)
	}
	results, err := c.searchJackett(q, baseTitle)
	if err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(results)
}

func (c *serverCtx) handleDownload(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	var body struct {
		MagnetURI  string `json:"magnet_uri"`
		TorrentURL string `json:"torrent_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", 400)
		return
	}
	if err := c.qbitAddTorrent(body.MagnetURI, body.TorrentURL); err != nil {
		http.Error(w, err.Error(), 500)
		return
	}
	w.WriteHeader(200)
}

// ─── Watch folder ─────────────────────────────────────────────────────────────

// scanWatchFolderVideos recursively finds video files under root — torrent
// clients commonly nest a download inside a per-release subfolder rather
// than dropping it flat in the watch folder.
func scanWatchFolderVideos(root string) []string {
	var out []string
	filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return nil
		}
		if videoExts[strings.ToLower(filepath.Ext(d.Name()))] {
			out = append(out, path)
		}
		return nil
	})
	return out
}

func (c *serverCtx) startWatchFolder() {
	if c.watchFolder == "" {
		return
	}
	os.MkdirAll(c.watchFolder, 0755)
	seen := map[string]bool{}
	logger.Printf("watch folder active: %s", c.watchFolder)
	for {
		for _, fullPath := range scanWatchFolderVideos(c.watchFolder) {
			if seen[fullPath] {
				continue
			}
			// Only pick up files stable for at least 60s (not still being written)
			fi, err := os.Stat(fullPath)
			if err != nil || time.Since(fi.ModTime()) < 60*time.Second {
				continue
			}
			seen[fullPath] = true
			go c.ingestLocalFile(fullPath)
		}
		time.Sleep(30 * time.Second)
	}
}

func (c *serverCtx) ingestLocalFile(localPath string) {
	key := filepath.Base(localPath)
	logger.Printf("watch folder: ingesting %s", key)
	c.procMu.Lock()
	existing := c.procMap[key]
	alreadyRunning := existing.Status == "processing"
	if !alreadyRunning {
		c.procMap[key] = procState{Status: "processing"}
	}
	c.procMu.Unlock()
	if alreadyRunning {
		return
	}
	// Transcode directly from the watched file (no R2 round-trip). This runs in
	// its own goroutine (from the poll loop), so processing synchronously here
	// is safe and tells us whether the job succeeded before deleting the source.
	c.processVideo(key, localPath)
	if ps, tracked := c.getProcState(key); !tracked || ps.Status != "error" {
		if err := os.Remove(localPath); err != nil {
			logger.Printf("watch folder cleanup %s: %v", localPath, err)
		} else {
			logger.Printf("watch folder cleanup: deleted %s", localPath)
		}
	}
}

// ─── OpenSubtitles ────────────────────────────────────────────────────────────

func truncateForLog(b []byte) string {
	const max = 300
	s := string(b)
	if len(s) > max {
		return s[:max] + "…"
	}
	return s
}

func (c *serverCtx) openSubsLogin() (string, error) {
	body, _ := json.Marshal(map[string]string{"username": c.openSubsUser, "password": c.openSubsPass})
	req, _ := http.NewRequest("POST", "https://api.opensubtitles.com/api/v1/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Api-Key", c.openSubsKey)
	req.Header.Set("User-Agent", "StreamServer/1.0")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	var r struct {
		Token string `json:"token"`
	}
	json.NewDecoder(resp.Body).Decode(&r)
	if r.Token == "" {
		return "", fmt.Errorf("opensubtitles login failed (status %d)", resp.StatusCode)
	}
	return r.Token, nil
}

var subtitleAttemptMu sync.Mutex
var subtitleAttemptAt = map[string]time.Time{}

const subtitleCooldown = 3 * time.Minute

// fetchSubtitle hits OpenSubtitles' search+download endpoints. A previous
// restart/retry loop hammered these in quick succession for the same
// episode and likely triggered a transient IP-level rate limit on their
// side — this cooldown keeps that from happening again.
func (c *serverCtx) fetchSubtitle(tmdbID int, mediaType string, season, episode int) ([]byte, error) {
	cooldownKey := fmt.Sprintf("%d:%s:%d:%d", tmdbID, mediaType, season, episode)
	subtitleAttemptMu.Lock()
	if last, ok := subtitleAttemptAt[cooldownKey]; ok && time.Since(last) < subtitleCooldown {
		subtitleAttemptMu.Unlock()
		return nil, fmt.Errorf("subtitle fetch on cooldown — last attempt %s ago, try again in %s",
			time.Since(last).Round(time.Second), (subtitleCooldown - time.Since(last)).Round(time.Second))
	}
	subtitleAttemptAt[cooldownKey] = time.Now()
	subtitleAttemptMu.Unlock()

	params := url.Values{
		"tmdb_id":   {strconv.Itoa(tmdbID)},
		"languages": {"en"},
		"order_by":  {"download_count"},
	}
	if mediaType == "tv" && season > 0 && episode > 0 {
		params.Set("type", "episode")
		params.Set("season_number", strconv.Itoa(season))
		params.Set("episode_number", strconv.Itoa(episode))
	} else {
		params.Set("type", "movie")
	}
	req, _ := http.NewRequest("GET", "https://api.opensubtitles.com/api/v1/subtitles?"+params.Encode(), nil)
	req.Header.Set("Api-Key", c.openSubsKey)
	req.Header.Set("User-Agent", "StreamServer/1.0")
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	searchBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("opensubtitles search status %d: %s", resp.StatusCode, truncateForLog(searchBody))
	}
	var sr struct {
		Data []struct {
			Attributes struct {
				Files []struct {
					FileID int `json:"file_id"`
				} `json:"files"`
			} `json:"attributes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(searchBody, &sr); err != nil {
		return nil, fmt.Errorf("opensubtitles search decode: %v: %s", err, truncateForLog(searchBody))
	}
	if len(sr.Data) == 0 || len(sr.Data[0].Attributes.Files) == 0 {
		return nil, fmt.Errorf("no subtitles found on opensubtitles")
	}
	fileID := sr.Data[0].Attributes.Files[0].FileID

	token, err := c.openSubsLogin()
	if err != nil {
		return nil, fmt.Errorf("opensubtitles login: %v", err)
	}
	dlBody, _ := json.Marshal(map[string]int{"file_id": fileID})
	dlReq, _ := http.NewRequest("POST", "https://api.opensubtitles.com/api/v1/download", bytes.NewReader(dlBody))
	dlReq.Header.Set("Content-Type", "application/json")
	dlReq.Header.Set("Api-Key", c.openSubsKey)
	dlReq.Header.Set("Authorization", "Bearer "+token)
	dlReq.Header.Set("User-Agent", "StreamServer/1.0")
	dlResp, err := (&http.Client{Timeout: 5 * time.Second}).Do(dlReq)
	if err != nil {
		return nil, err
	}
	defer dlResp.Body.Close()
	dlRespBody, _ := io.ReadAll(dlResp.Body)
	if dlResp.StatusCode != 200 {
		return nil, fmt.Errorf("opensubtitles download status %d: %s", dlResp.StatusCode, truncateForLog(dlRespBody))
	}
	var dlResult struct {
		Link string `json:"link"`
	}
	json.Unmarshal(dlRespBody, &dlResult)
	if dlResult.Link == "" {
		return nil, fmt.Errorf("no download link from opensubtitles: %s", truncateForLog(dlRespBody))
	}
	srtResp, err := (&http.Client{Timeout: 8 * time.Second}).Get(dlResult.Link)
	if err != nil {
		return nil, err
	}
	defer srtResp.Body.Close()
	srt, err := io.ReadAll(srtResp.Body)
	if err != nil {
		return nil, err
	}
	return srtToVTT(srt), nil
}

func srtToVTT(srt []byte) []byte {
	var buf bytes.Buffer
	buf.WriteString("WEBVTT\n\n")
	lines := strings.Split(strings.ReplaceAll(string(srt), "\r\n", "\n"), "\n")
	for _, line := range lines {
		line = strings.TrimSpace(line)
		if _, err := strconv.Atoi(line); err == nil {
			continue // skip sequence numbers
		}
		buf.WriteString(strings.ReplaceAll(line, ",", "."))
		buf.WriteByte('\n')
	}
	return buf.Bytes()
}

// ─── Processing pipeline ──────────────────────────────────────────────────────

func thumbKey(key string) string { return key + ".thumb.jpg" }
func metaKey(key string) string  { return key + ".meta.json" }

func stemToMP4(key string) string {
	if strings.ToLower(filepath.Ext(key)) == ".mp4" {
		return key
	}
	return strings.TrimSuffix(key, filepath.Ext(key)) + ".mp4"
}

func (c *serverCtx) uploadR2File(ctx context.Context, r2Key, contentType, localPath string) error {
	f, err := os.Open(localPath)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, _ := f.Stat()
	_, err = c.r2.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        awsv2.String(c.r2Bucket),
		Key:           awsv2.String(r2Key),
		Body:          f,
		ContentLength: awsv2.Int64(fi.Size()),
		ContentType:   awsv2.String(contentType),
	})
	return err
}

func (c *serverCtx) uploadR2Dir(ctx context.Context, dir, r2Prefix string) error {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return err
	}
	// HLS output is often hundreds of small segment files (e.g. a 47-minute
	// episode at 3 renditions × 10s segments ≈ 800+ files) — uploading them
	// one at a time made this phase look "stuck" for many minutes.
	const concurrency = 8
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		ct := "video/MP2T"
		if strings.HasSuffix(name, ".m3u8") {
			ct = "application/vnd.apple.mpegurl"
		}
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer wg.Done()
			defer func() { <-sem }()
			if err := c.uploadR2File(ctx, r2Prefix+name, ct, filepath.Join(dir, name)); err != nil {
				logger.Printf("upload HLS file %s: %v", name, err)
			}
		}()
	}
	wg.Wait()
	return nil
}

func (c *serverCtx) writeEarlyMeta(ctx context.Context, key string, meta videoMeta) {
	data, _ := json.Marshal(meta)
	c.r2.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        awsv2.String(c.r2Bucket),
		Key:           awsv2.String(metaKey(key)),
		Body:          strings.NewReader(string(data)),
		ContentLength: awsv2.Int64(int64(len(data))),
		ContentType:   awsv2.String("application/json"),
	})
}

// processSem bounds how many videos transcode at once. HLS encoding is
// memory-hungry (filter_complex holds N parallel scale/encode pipelines per
// job), so concurrent jobs compound and can exhaust memory — serialize them.
var processSem = make(chan struct{}, 1)

// processVideo transcodes a video and uploads its HLS/MP4 outputs to R2.
// When localSrc is non-empty the encode runs directly from that local file
// (web upload / watch folder) — the raw source is never round-tripped through
// R2. When localSrc is empty the source is downloaded from R2 under key (used
// for reprocessing files already in the library).
func (c *serverCtx) processVideo(key, localSrc string) {
	processSem <- struct{}{}
	defer func() { <-processSem }()

	inPlace := localSrc != ""
	ctx := context.Background()
	tmpDir, err := os.MkdirTemp("", "stream-proc-*")
	if err != nil {
		c.setProcState(key, procState{Status: "error", Err: "temp dir: " + err.Error()})
		return
	}
	defer os.RemoveAll(tmpDir)

	// 1. Source: use the local file directly when processing in place, else
	// download it from R2.
	var origPath string
	if inPlace {
		origPath = localSrc
	} else {
		obj, err := c.r2.GetObject(ctx, &s3.GetObjectInput{
			Bucket: awsv2.String(c.r2Bucket),
			Key:    awsv2.String(key),
		})
		if err != nil {
			c.setProcState(key, procState{Status: "error", Err: "download: " + err.Error()})
			return
		}
		origPath = filepath.Join(tmpDir, "src"+strings.ToLower(filepath.Ext(key)))
		of, err := os.Create(origPath)
		if err != nil {
			obj.Body.Close()
			c.setProcState(key, procState{Status: "error", Err: "temp file: " + err.Error()})
			return
		}
		_, copyErr := io.Copy(of, obj.Body)
		obj.Body.Close()
		of.Close()
		if copyErr != nil {
			c.setProcState(key, procState{Status: "error", Err: "download: " + copyErr.Error()})
			return
		}
	}

	// 2. Probe — log and error-state if ffprobe can't read it
	meta, isVid, probeErr := probeVideo(origPath)
	if !isVid {
		logger.Printf("probe failed for %s: %s", key, probeErr)
		c.setProcState(key, procState{Status: "error", Err: "probe: " + probeErr})
		return
	}
	logger.Printf("probe OK: %s — %dx%d %ds", key, meta.Width, meta.Height, meta.Duration)

	// 3. Enrich with LLM + TMDB (fast — runs before HLS encode)
	c.enrichMeta(filepath.Base(key), &meta)

	stem := strings.TrimSuffix(key, filepath.Ext(key))
	finalKey := stem + ".mp4"
	hlsPrefix := "hls/" + stem + "/"

	// 3.5 Subtitles — prefer ones already embedded in the source (many torrent
	// releases bundle them); only hit OpenSubtitles if the source has none.
	vttKey := stem + ".en.vtt"
	subtitleDone := false
	if subIdx, found := findEmbeddedSubtitle(origPath); found {
		vttPath := filepath.Join(tmpDir, "embedded.vtt")
		if err := extractEmbeddedSubtitle(origPath, subIdx, vttPath); err == nil {
			if vttBytes, readErr := os.ReadFile(vttPath); readErr == nil {
				_, uploadErr := c.r2.PutObject(ctx, &s3.PutObjectInput{
					Bucket:        awsv2.String(c.r2Bucket),
					Key:           awsv2.String(vttKey),
					Body:          bytes.NewReader(vttBytes),
					ContentLength: awsv2.Int64(int64(len(vttBytes))),
					ContentType:   awsv2.String("text/vtt"),
				})
				if uploadErr == nil {
					meta.SubtitleURL = strings.TrimRight(c.r2PubURL, "/") + "/" + vttKey
					subtitleDone = true
					logger.Printf("embedded subtitle extracted: %s", vttKey)
				}
			}
		} else {
			logger.Printf("embedded subtitle extract %s: %v", key, err)
		}
	}
	if !subtitleDone && c.openSubsKey != "" && c.openSubsUser != "" && meta.TMDBID > 0 {
		if vtt, err := c.fetchSubtitle(meta.TMDBID, meta.MediaType, meta.Season, meta.Episode); err == nil {
			_, uploadErr := c.r2.PutObject(ctx, &s3.PutObjectInput{
				Bucket:        awsv2.String(c.r2Bucket),
				Key:           awsv2.String(vttKey),
				Body:          bytes.NewReader(vtt),
				ContentLength: awsv2.Int64(int64(len(vtt))),
				ContentType:   awsv2.String("text/vtt"),
			})
			if uploadErr == nil {
				meta.SubtitleURL = strings.TrimRight(c.r2PubURL, "/") + "/" + vttKey
				logger.Printf("subtitle uploaded: %s", vttKey)
			}
		} else {
			logger.Printf("subtitle fetch %s: %v", key, err)
		}
	}

	// 4. Write early metadata so UI shows title/poster/duration while encoding.
	// In-place jobs have no raw object in R2 to pair it with — the in-memory
	// procMap drives the "processing" tile instead (see handleFiles).
	if !inPlace {
		c.writeEarlyMeta(ctx, key, meta)
	}

	// 5. Extract thumbnail (upload against finalKey so it's ready after conversion)
	thumbPath := filepath.Join(tmpDir, "thumb.jpg")
	if err := extractThumbnail(origPath, thumbPath, meta.Duration); err == nil {
		c.uploadR2File(ctx, thumbKey(finalKey), "image/jpeg", thumbPath)
	}

	// 5.5 Fast path — if the source video is already browser-playable (H.264),
	// just remux it into a faststart MP4 (no video re-encode) and serve it
	// directly from the CDN. Skips the slow/expensive HLS transcode entirely.
	if browserVideoCodecs[meta.VideoCodec] {
		remuxPath := filepath.Join(tmpDir, "remux.mp4")
		if err := exec.Command(ffmpegBin(), buildRemuxArgs(origPath, remuxPath, meta.AudioCodec)...).Run(); err == nil {
			if upErr := c.uploadR2File(ctx, finalKey, "video/mp4", remuxPath); upErr == nil {
				if !inPlace && key != finalKey {
					c.r2.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: awsv2.String(c.r2Bucket), Key: awsv2.String(key)})
					c.r2.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: awsv2.String(c.r2Bucket), Key: awsv2.String(metaKey(key))})
				}
				meta.DirectPlay = true
				c.writeEarlyMeta(ctx, finalKey, meta)
				logger.Printf("remuxed (direct play): %s → %s [%s/%s]", key, finalKey, meta.VideoCodec, meta.AudioCodec)
				c.clearProcState(key)
				return
			} else {
				logger.Printf("remux upload %s: %v — falling back to HLS", key, upErr)
			}
		} else {
			logger.Printf("remux %s: %v — falling back to HLS", key, err)
		}
	}

	// 6. Determine renditions based on source height
	rends := targetRenditions(meta.Height)

	// 7. Generate HLS (all quality levels in one FFmpeg pass)
	hlsDir := filepath.Join(tmpDir, "hls")
	os.MkdirAll(hlsDir, 0755)
	useNvenc := nvencAvailable
	hlsCmd := buildHLSCommand(origPath, hlsDir, rends, meta.Height >= 1600, useNvenc)
	out, err := hlsCmd.CombinedOutput()
	if err != nil && useNvenc {
		// GPU encode failed (NVENC may be unavailable under the LocalSystem
		// service session, or hit a concurrent-session limit). Clear partial
		// output and retry on CPU so the job still completes.
		logger.Printf("HLS encode (nvenc) %s failed: %v — retrying on libx264", key, err)
		os.RemoveAll(hlsDir)
		os.MkdirAll(hlsDir, 0755)
		hlsCmd = buildHLSCommand(origPath, hlsDir, rends, meta.Height >= 1600, false)
		out, err = hlsCmd.CombinedOutput()
	}
	if err != nil {
		c.setProcState(key, procState{Status: "error", Err: "hls encode: " + err.Error()})
		logger.Printf("HLS encode %s: %v\n%s", key, err, string(out))
		return
	}

	// 8. Upload all HLS files to R2
	if err := c.uploadR2Dir(ctx, hlsDir, hlsPrefix); err != nil {
		c.setProcState(key, procState{Status: "error", Err: "hls upload: " + err.Error()})
		return
	}

	// 9. Generate a single best-quality MP4 for download
	dlPath := filepath.Join(tmpDir, "download.mp4")
	dlArgs := buildDownloadMP4Args(origPath, dlPath)
	if err := exec.Command(ffmpegBin(), dlArgs...).Run(); err != nil {
		logger.Printf("download MP4 %s: %v (non-fatal)", key, err)
	} else {
		c.uploadR2File(ctx, finalKey, "video/mp4", dlPath)
	}

	// 10. Delete original if key changed, and its early meta (R2-source jobs
	// only — in-place jobs never uploaded a raw object).
	if !inPlace && key != finalKey {
		if _, err := c.r2.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: awsv2.String(c.r2Bucket), Key: awsv2.String(key)}); err != nil {
			logger.Printf("WARNING: failed to delete original %s: %v", key, err)
		}
		c.r2.DeleteObject(ctx, &s3.DeleteObjectInput{Bucket: awsv2.String(c.r2Bucket), Key: awsv2.String(metaKey(key))})
	}

	// 11. Write final metadata with HLS info
	rendNames := make([]string, len(rends))
	for i, r := range rends {
		rendNames[i] = r.name
	}
	meta.HLSPrefix = hlsPrefix
	meta.Resolutions = rendNames
	c.writeEarlyMeta(ctx, finalKey, meta)

	logger.Printf("processed OK: %s → %s [%s] HLS: %s", key, finalKey, strings.Join(rendNames, ","), hlsPrefix)
	c.clearProcState(key)
}

func (c *serverCtx) setProcState(key string, ps procState) {
	c.procMu.Lock()
	c.procMap[key] = ps
	c.procMu.Unlock()
}

func (c *serverCtx) clearProcState(key string) {
	c.procMu.Lock()
	delete(c.procMap, key)
	c.procMu.Unlock()
}

func (c *serverCtx) getProcState(key string) (procState, bool) {
	c.procMu.Lock()
	defer c.procMu.Unlock()
	ps, ok := c.procMap[key]
	return ps, ok
}

func (c *serverCtx) processingKeys() []string {
	c.procMu.Lock()
	defer c.procMu.Unlock()
	var out []string
	for k, ps := range c.procMap {
		if ps.Status == "processing" {
			out = append(out, k)
		}
	}
	return out
}

// ─── Handlers ─────────────────────────────────────────────────────────────────

func (c *serverCtx) handleLogin(w http.ResponseWriter, r *http.Request) {
	if c.loginDisabled.Load() {
		http.NotFound(w, r)
		return
	}
	if c.isAuthed(r) {
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	if r.Method == http.MethodPost {
		ip := realIP(r)
		if !c.checkRateLimit(ip) {
			http.Error(w, "too many failed attempts — try again later", http.StatusTooManyRequests)
			return
		}
		pw := r.FormValue("password")
		if bcrypt.CompareHashAndPassword([]byte(c.passHash), []byte(pw)) != nil {
			c.recordFailure(ip)
			w.WriteHeader(http.StatusUnauthorized)
			loginTmpl.Execute(w, map[string]interface{}{"Error": "wrong password"})
			return
		}
		c.recordSuccess(ip)
		c.setSessionCookie(w, r)
		http.Redirect(w, r, "/", http.StatusFound)
		return
	}
	loginTmpl.Execute(w, nil)
}

func (c *serverCtx) handleToggleLogin(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	c.loginDisabled.Store(!c.loginDisabled.Load())
	w.WriteHeader(200)
}

func (c *serverCtx) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	if !c.isAuthed(r) {
		if c.loginDisabled.Load() {
			http.Error(w, "access denied", http.StatusForbidden)
			return
		}
		http.Redirect(w, r, "/login", http.StatusFound)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	local := isLocalRequest(r)
	mainTmpl.Execute(w, map[string]interface{}{
		"SessionHours":  c.sessionHoursLeft(r),
		"LoginDisabled": c.loginDisabled.Load(),
		"R2PubURL":      strings.TrimRight(c.r2PubURL, "/"),
		"IsLocal":       local,
		"HasLocalLib":   local && len(c.localFolders) > 0,
	})
}

func (c *serverCtx) handlePresign(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	name := filepath.Base(r.URL.Query().Get("name"))
	if name == "" || name == "." {
		http.Error(w, "invalid name", 400)
		return
	}
	pc := s3.NewPresignClient(c.r2)
	result, err := pc.PresignPutObject(context.Background(), &s3.PutObjectInput{
		Bucket: awsv2.String(c.r2Bucket),
		Key:    awsv2.String(name),
	}, s3.WithPresignExpires(15*time.Minute))
	if err != nil {
		http.Error(w, "presign failed", 500)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"url": result.URL, "key": name})
}

func (c *serverCtx) handleConfirm(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
		http.Error(w, "bad request", 400)
		return
	}
	ext := strings.ToLower(filepath.Ext(body.Key))
	if videoExts[ext] {
		c.procMu.Lock()
		existing := c.procMap[body.Key]
		alreadyRunning := existing.Status == "processing"
		if !alreadyRunning {
			c.procMap[body.Key] = procState{Status: "processing"}
		}
		c.procMu.Unlock()
		if !alreadyRunning {
			go c.processVideo(body.Key, "")
			logger.Printf("processing queued: %s", body.Key)
		}
	}
	w.WriteHeader(200)
	fmt.Fprint(w, "ok")
}

func (c *serverCtx) handleNote(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil || len(body) == 0 {
		http.Error(w, "empty note", 400)
		return
	}
	name := fmt.Sprintf("note_%d.txt", time.Now().UnixMilli())
	_, err = c.r2.PutObject(context.Background(), &s3.PutObjectInput{
		Bucket:        awsv2.String(c.r2Bucket),
		Key:           awsv2.String(name),
		Body:          strings.NewReader(string(body)),
		ContentLength: awsv2.Int64(int64(len(body))),
		ContentType:   awsv2.String("text/plain; charset=utf-8"),
	})
	if err != nil {
		http.Error(w, "save failed", 500)
		return
	}
	logger.Printf("note saved: %s", name)
	w.WriteHeader(200)
	fmt.Fprint(w, "ok")
}

type fileEntry struct {
	Key         string    `json:"key"`
	Name        string    `json:"name"`
	Size        int64     `json:"size"`
	Modified    time.Time `json:"modified"`
	IsVideo     bool      `json:"is_video"`
	IsNote      bool      `json:"is_note"`
	ThumbURL    string    `json:"thumb_url,omitempty"`
	Duration    int       `json:"duration,omitempty"`
	Width       int       `json:"width,omitempty"`
	Height      int       `json:"height,omitempty"`
	Snippet     string    `json:"snippet,omitempty"`
	ProcState   string    `json:"proc_state,omitempty"`
	HLSBase     string    `json:"hls_base,omitempty"`
	DirectPlay  bool      `json:"direct_play,omitempty"`
	Resolutions []string  `json:"resolutions,omitempty"`
	IsLocal     bool      `json:"is_local,omitempty"`
	Playable    bool      `json:"playable,omitempty"`
	// AI-enriched fields
	Title           string   `json:"title,omitempty"`
	Year            int      `json:"year,omitempty"`
	MediaType       string   `json:"media_type,omitempty"`
	Season          int      `json:"season,omitempty"`
	Episode         int      `json:"episode,omitempty"`
	Overview        string   `json:"overview,omitempty"`
	PosterURL       string   `json:"poster_url,omitempty"`
	Rating          float64  `json:"rating,omitempty"`
	Genres          []string `json:"genres,omitempty"`
	TMDBID          int      `json:"tmdb_id,omitempty"`
	TMDBBackdrop    string   `json:"tmdb_backdrop,omitempty"`
	EpisodeTitle    string   `json:"episode_title,omitempty"`
	EpisodeOverview string   `json:"episode_overview,omitempty"`
	EpisodeStill    string   `json:"episode_still,omitempty"`
	SubtitleURL     string   `json:"subtitle_url,omitempty"`
	IsMusicVideo    bool     `json:"is_music_video,omitempty"`
	Artist          string   `json:"artist,omitempty"`
	Album           string   `json:"album,omitempty"`
	IsExtra         bool            `json:"is_extra,omitempty"`
	MainKey         string          `json:"main_key,omitempty"`
	Runtime         int             `json:"runtime,omitempty"`
	Director        string          `json:"director,omitempty"`
	Cast            []string        `json:"cast,omitempty"`
	Country         string          `json:"country,omitempty"`
	Language        string          `json:"language,omitempty"`
	Collection      *tmdbCollection `json:"collection,omitempty"`
}

func (c *serverCtx) handleFiles(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	out, err := c.r2.ListObjectsV2(context.Background(), &s3.ListObjectsV2Input{
		Bucket: awsv2.String(c.r2Bucket),
	})
	if err != nil {
		http.Error(w, "list failed", 500)
		return
	}

	// Build lookup sets for internal objects
	thumbSet := map[string]bool{}
	metaSet := map[string]bool{}
	for _, obj := range out.Contents {
		k := awsv2.ToString(obj.Key)
		if strings.HasSuffix(k, ".thumb.jpg") {
			thumbSet[k] = true
		}
		if strings.HasSuffix(k, ".meta.json") {
			metaSet[k] = true
		}
	}
	pubBase := strings.TrimRight(c.r2PubURL, "/")

	// Fetch metadata for processed videos
	metaCache := map[string]videoMeta{}
	for mk := range metaSet {
		mr, err := c.r2.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: awsv2.String(c.r2Bucket),
			Key:    awsv2.String(mk),
		})
		if err != nil {
			continue
		}
		var vm videoMeta
		if err := json.NewDecoder(mr.Body).Decode(&vm); err == nil {
			metaCache[mk] = vm
		}
		mr.Body.Close()
	}

	var result []fileEntry
	for _, obj := range out.Contents {
		key := awsv2.ToString(obj.Key)
		// Skip all internal objects (thumbnails, metadata, HLS segments/playlists)
		if strings.HasSuffix(key, ".thumb.jpg") || strings.HasSuffix(key, ".meta.json") ||
			strings.HasPrefix(key, "hls/") || strings.HasSuffix(key, ".en.vtt") ||
			strings.HasPrefix(filepath.Base(key), "_") {
			continue
		}

		entry := fileEntry{
			Key:      key,
			Name:     filepath.Base(key),
			Size:     awsv2.ToInt64(obj.Size),
			Modified: awsv2.ToTime(obj.LastModified),
		}

		ext := strings.ToLower(filepath.Ext(key))
		if videoExts[ext] {
			entry.IsVideo = true
			if thumbSet[thumbKey(key)] {
				entry.ThumbURL = pubBase + "/" + thumbKey(key)
			}
			if vm, ok := metaCache[metaKey(key)]; ok {
				entry.Duration = vm.Duration
				entry.Width = vm.Width
				entry.Height = vm.Height
				if vm.HLSPrefix != "" {
					entry.HLSBase = pubBase + "/" + vm.HLSPrefix
					entry.Resolutions = vm.Resolutions
				}
				entry.DirectPlay = vm.DirectPlay
				entry.Title = vm.Title
				entry.Year = vm.Year
				entry.MediaType = vm.MediaType
				entry.Season = vm.Season
				entry.Episode = vm.Episode
				entry.Overview = vm.Overview
				entry.PosterURL = vm.PosterURL
				entry.Rating = vm.Rating
				entry.Genres = vm.Genres
				entry.TMDBID = vm.TMDBID
				entry.TMDBBackdrop = vm.TMDBBackdrop
				entry.EpisodeTitle = vm.EpisodeTitle
				entry.EpisodeOverview = vm.EpisodeOverview
				entry.EpisodeStill = vm.EpisodeStill
				entry.SubtitleURL = vm.SubtitleURL
				entry.Runtime = vm.Runtime
				entry.Director = vm.Director
				entry.Cast = vm.Cast
				entry.Country = vm.Country
				entry.Language = vm.Language
				entry.Collection = vm.Collection
			}
		}

		if strings.HasPrefix(key, "note_") && strings.HasSuffix(key, ".txt") {
			entry.IsNote = true
			if nr, err := c.r2.GetObject(context.Background(), &s3.GetObjectInput{
				Bucket: awsv2.String(c.r2Bucket),
				Key:    awsv2.String(key),
				Range:  awsv2.String("bytes=0-159"),
			}); err == nil {
				raw, _ := io.ReadAll(nr.Body)
				nr.Body.Close()
				s := string(raw)
				if awsv2.ToInt64(obj.Size) > 160 {
					s += "..."
				}
				entry.Snippet = s
			}
		}

		// Processing state
		if ps, ok := c.getProcState(key); ok {
			entry.ProcState = ps.Status
		}

		result = append(result, entry)
	}

	// Deduplicate: if foo.mp4 exists (processed), hide the original foo.mkv/foo.webm etc.
	mp4Stems := map[string]bool{}
	for _, e := range result {
		if strings.ToLower(filepath.Ext(e.Key)) == ".mp4" {
			mp4Stems[strings.TrimSuffix(e.Key, filepath.Ext(e.Key))] = true
		}
	}
	filtered := result[:0]
	for _, e := range result {
		ext := strings.ToLower(filepath.Ext(e.Key))
		stem := strings.TrimSuffix(e.Key, filepath.Ext(e.Key))
		if ext != ".mp4" && mp4Stems[stem] {
			continue
		}
		filtered = append(filtered, e)
	}
	result = filtered

	// Surface in-place jobs (web upload / watch folder) that don't yet have any
	// object in R2 — we no longer upload the raw source, so the encode runs from
	// disk and the only signal is the in-memory procMap.
	present := map[string]bool{}
	for _, e := range result {
		present[e.Key] = true
	}
	for _, pk := range c.processingKeys() {
		if present[pk] || present[stemToMP4(pk)] {
			continue
		}
		result = append(result, fileEntry{
			Key:       pk,
			Name:      filepath.Base(pk),
			IsVideo:   videoExts[strings.ToLower(filepath.Ext(pk))],
			ProcState: "processing",
			Modified:  time.Now(),
		})
	}

	sort.Slice(result, func(i, j int) bool {
		return result[i].Modified.After(result[j].Modified)
	})

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(result)
}

func (c *serverCtx) handleFile(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key", 400)
		return
	}

	// Notes: proxy through server
	if strings.HasPrefix(filepath.Base(key), "note_") && strings.HasSuffix(key, ".txt") {
		obj, err := c.r2.GetObject(context.Background(), &s3.GetObjectInput{
			Bucket: awsv2.String(c.r2Bucket),
			Key:    awsv2.String(key),
		})
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		defer obj.Body.Close()
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.Copy(w, obj.Body)
		return
	}

	// Download: proxy through our own origin with an attachment header so the
	// browser saves the file (with its real name) instead of navigating to the
	// raw r2.dev URL and playing it inline on a foreign domain.
	if r.URL.Query().Get("dl") == "1" {
		in := &s3.GetObjectInput{Bucket: awsv2.String(c.r2Bucket), Key: awsv2.String(key)}
		if rng := r.Header.Get("Range"); rng != "" {
			in.Range = awsv2.String(rng)
		}
		obj, err := c.r2.GetObject(context.Background(), in)
		if err != nil {
			http.Error(w, "not found", 404)
			return
		}
		defer obj.Body.Close()
		w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(key)))
		w.Header().Set("Accept-Ranges", "bytes")
		if obj.ContentType != nil {
			w.Header().Set("Content-Type", *obj.ContentType)
		} else {
			w.Header().Set("Content-Type", "application/octet-stream")
		}
		if obj.ContentLength != nil {
			w.Header().Set("Content-Length", strconv.FormatInt(*obj.ContentLength, 10))
		}
		if obj.ContentRange != nil {
			w.Header().Set("Content-Range", *obj.ContentRange)
			w.WriteHeader(http.StatusPartialContent)
		}
		io.Copy(w, obj.Body)
		return
	}

	// All other files: redirect to CDN
	cdnURL := strings.TrimRight(c.r2PubURL, "/") + "/" + key
	http.Redirect(w, r, cdnURL, http.StatusFound)
}

// handleUpload accepts a local (LAN) multipart upload, stages it on disk, and
// transcodes it in place — videos never round-trip through R2. Local-only.
func (c *serverCtx) handleUpload(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	mr, err := r.MultipartReader()
	if err != nil {
		http.Error(w, "bad multipart: "+err.Error(), 400)
		return
	}
	stagingDir := filepath.Join(os.TempDir(), "stream-upload")
	os.MkdirAll(stagingDir, 0755)
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			http.Error(w, "read part: "+err.Error(), 400)
			return
		}
		if part.FormName() != "file" {
			continue
		}
		name := filepath.Base(part.FileName())
		if name == "" || name == "." {
			http.Error(w, "invalid filename", 400)
			return
		}
		dstPath := filepath.Join(stagingDir, name)
		dst, err := os.Create(dstPath)
		if err != nil {
			http.Error(w, "staging: "+err.Error(), 500)
			return
		}
		_, copyErr := io.Copy(dst, part)
		dst.Close()
		if copyErr != nil {
			os.Remove(dstPath)
			http.Error(w, "save: "+copyErr.Error(), 500)
			return
		}
		key := name
		// Non-video: keep the old "drop any file" behavior — straight to R2.
		if !videoExts[strings.ToLower(filepath.Ext(name))] {
			if err := c.uploadR2File(context.Background(), key, "application/octet-stream", dstPath); err != nil {
				os.Remove(dstPath)
				http.Error(w, "upload: "+err.Error(), 500)
				return
			}
			os.Remove(dstPath)
			continue
		}
		c.procMu.Lock()
		alreadyRunning := c.procMap[key].Status == "processing"
		if !alreadyRunning {
			c.procMap[key] = procState{Status: "processing"}
		}
		c.procMu.Unlock()
		if alreadyRunning {
			os.Remove(dstPath)
			continue
		}
		go func(k, p string) {
			c.processVideo(k, p)
			os.Remove(p)
		}(key, dstPath)
		logger.Printf("upload: processing %s in place", key)
	}
	w.WriteHeader(200)
	fmt.Fprint(w, "ok")
}

// browserPlayableExts are containers/codecs a <video> tag can play directly
// off disk without any transcoding.
var browserPlayableExts = map[string]bool{".mp4": true, ".m4v": true, ".webm": true, ".mov": true}

// resolveLocalPath maps a request's relative path back to an absolute path,
// guaranteeing it stays inside one of the configured library roots (no
// traversal out via .. or absolute paths).
func (c *serverCtx) resolveLocalPath(rel string) (string, bool) {
	rel = filepath.FromSlash(rel)
	for _, f := range c.localFolders {
		absRoot, err := filepath.Abs(f.Path)
		if err != nil {
			continue
		}
		full := filepath.Join(absRoot, rel)
		if full == absRoot || strings.HasPrefix(full, absRoot+string(os.PathSeparator)) {
			if _, err := os.Stat(full); err == nil {
				return full, true
			}
		}
	}
	return "", false
}

// localVideo is one file found while walking the configured local library
// roots: an absolute path, its root-relative forward-slashed form (the
// stable identifier used in API responses and the metadata cache key), and
// the type inherited from its root folder's config ("video"/"music_video").
type localVideo struct {
	Abs     string
	Rel     string
	Type    string
	Size    int64
	IsExtra bool
	MainRel string // the Rel of the main feature this belongs to, if IsExtra
}

// markExtras applies whatever extras/main-feature classification has
// already been decided for a file and persisted in localMetaCache (set
// directly, by hand, when a specific file needs it — see project notes).
// It deliberately does not try to guess this automatically anymore: both a
// fixed heuristic (longest file in the folder wins) and an LLM-driven
// cross-referencing system were tried here, and both produced real false
// positives — hiding a genuine standalone movie that happens to share a
// folder with another one (a library root, or a generic "Movies" folder
// with no per-title subfolders has no other shape), or attaching a
// confident-looking but wrong match to a bonus featurette. A newly-seen
// file in an ambiguous folder is left alone (rendered as its own standalone
// item) until it is explicitly marked otherwise in the cache.
func markExtras(videos []localVideo) {
	for i, v := range videos {
		if vm, ok := getLocalMeta(v.Rel); ok && vm.IsExtra {
			videos[i].IsExtra = true
			videos[i].MainRel = vm.MainKey
		}
	}
}

func (c *serverCtx) scanLocalVideos() []localVideo {
	// A folder can itself be a separately-configured root nested inside
	// another (e.g. "D:\Videos\Music Videos" inside "D:\Videos") so it gets
	// its own type tag instead of inheriting its parent's. Skip descending
	// into it during the parent's walk — its own pass below covers it.
	rootSet := map[string]bool{}
	for _, f := range c.localFolders {
		if abs, err := filepath.Abs(f.Path); err == nil {
			rootSet[abs] = true
		}
	}
	var out []localVideo
	for _, f := range c.localFolders {
		absRoot, err := filepath.Abs(f.Path)
		if err != nil {
			continue
		}
		ftype := f.Type
		if ftype == "" {
			ftype = "video"
		}
		filepath.WalkDir(absRoot, func(path string, d fs.DirEntry, err error) error {
			if err != nil {
				return nil
			}
			if d.IsDir() {
				if path != absRoot && rootSet[path] {
					return filepath.SkipDir
				}
				return nil
			}
			if !videoExts[strings.ToLower(filepath.Ext(d.Name()))] {
				return nil
			}
			rel, err := filepath.Rel(absRoot, path)
			if err != nil {
				return nil
			}
			var size int64
			if fi, err := d.Info(); err == nil {
				size = fi.Size()
			}
			out = append(out, localVideo{Abs: path, Rel: filepath.ToSlash(rel), Type: ftype, Size: size})
			return nil
		})
	}
	markExtras(out)
	return out
}

// ─── Local library metadata cache ──────────────────────────────────────────
//
// The local library deliberately never transcodes or touches R2 (see
// startWatchFolder/processVideo for the cloud path), but we still want the
// same TMDB/LLM-driven title/poster/show-grouping the cloud library gets.
// Results are cached to a small JSON file next to config.json, keyed by the
// file's root-relative path, so enrichment only ever runs once per file.

var (
	localMetaMu    sync.Mutex
	localMetaCache = map[string]videoMeta{}
)

func localMetaPath() string {
	return filepath.Join(filepath.Dir(cfgPath), "local_meta.json")
}

func loadLocalMetaCache() {
	data, err := os.ReadFile(localMetaPath())
	if err != nil {
		return
	}
	localMetaMu.Lock()
	defer localMetaMu.Unlock()
	json.Unmarshal(data, &localMetaCache)
}

func saveLocalMetaCache() {
	localMetaMu.Lock()
	data, err := json.Marshal(localMetaCache)
	localMetaMu.Unlock()
	if err != nil {
		return
	}
	os.WriteFile(localMetaPath(), data, 0644)
}

func getLocalMeta(rel string) (videoMeta, bool) {
	localMetaMu.Lock()
	defer localMetaMu.Unlock()
	vm, ok := localMetaCache[rel]
	return vm, ok
}

// startLocalEnrichment runs the LLM+TMDB filename lookup (the same one the
// cloud pipeline uses) for every local-library file that doesn't have a
// cached result yet, a few at a time, persisting after each one so progress
// survives a restart. Rescans periodically to pick up newly added files.
func (c *serverCtx) startLocalEnrichment() {
	if len(c.localFolders) == 0 {
		return
	}
	loadLocalMetaCache()
	const concurrency = 3
	for {
		videos := c.scanLocalVideos()
		var pending []localVideo
		for _, v := range videos {
			if _, ok := getLocalMeta(v.Rel); !ok {
				pending = append(pending, v)
			}
		}
		if len(pending) > 0 {
			logger.Printf("local library: enriching %d file(s)", len(pending))
			var musicPending []localVideo
			var videoPending []localVideo
			for _, v := range pending {
				if v.Type == "music_video" {
					musicPending = append(musicPending, v)
				} else {
					videoPending = append(videoPending, v)
				}
			}

			sem := make(chan struct{}, concurrency)
			var wg sync.WaitGroup
			for _, v := range videoPending {
				wg.Add(1)
				sem <- struct{}{}
				go func(v localVideo) {
					defer wg.Done()
					defer func() { <-sem }()
					var meta videoMeta
					c.enrichMeta(filepath.Base(v.Rel), &meta)
					localMetaMu.Lock()
					localMetaCache[v.Rel] = meta
					localMetaMu.Unlock()
					saveLocalMetaCache()
				}(v)
			}
			wg.Wait()

			// Artist/title comes straight from the filename's own "Artist -
			// Song" structure, with no external lookup. A free-text search
			// (this used to call iTunes) kept matching short, dash-less
			// names — "Slur", "miserere", "BREAK" — to unrelated tracks by
			// some other artist entirely, and cover art now comes from the
			// video itself rather than album art, so there's nothing left
			// an API lookup was actually buying here.
			for _, v := range musicPending {
				var meta videoMeta
				artist, song := parseMusicVideoFilename(filepath.Base(v.Rel))
				meta.MediaType = "music_video"
				if song != "" {
					meta.Artist = artist
					meta.Title = song
				} else {
					meta.Title = artist
				}
				localMetaMu.Lock()
				localMetaCache[v.Rel] = meta
				localMetaMu.Unlock()
				saveLocalMetaCache()
			}
			logger.Printf("local library: enrichment pass complete")
		}
		time.Sleep(60 * time.Second)
	}
}

func (c *serverCtx) handleLocalFiles(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	var out []fileEntry
	for _, v := range c.scanLocalVideos() {
		ext := strings.ToLower(filepath.Ext(v.Rel))
		entry := fileEntry{
			Key:          v.Rel,
			Name:         filepath.Base(v.Rel),
			Size:         v.Size,
			IsVideo:      true,
			IsLocal:      true,
			Playable:     browserPlayableExts[ext],
			IsMusicVideo: v.Type == "music_video",
			IsExtra:      v.IsExtra,
			MainKey:      v.MainRel,
		}
		if vm, ok := getLocalMeta(v.Rel); ok {
			entry.Title = vm.Title
			entry.Year = vm.Year
			entry.MediaType = vm.MediaType
			entry.Season = vm.Season
			entry.Episode = vm.Episode
			entry.Overview = vm.Overview
			entry.PosterURL = vm.PosterURL
			entry.Rating = vm.Rating
			entry.Genres = vm.Genres
			entry.TMDBID = vm.TMDBID
			entry.TMDBBackdrop = vm.TMDBBackdrop
			entry.EpisodeTitle = vm.EpisodeTitle
			entry.EpisodeOverview = vm.EpisodeOverview
			entry.EpisodeStill = vm.EpisodeStill
			entry.Artist = vm.Artist
			entry.Album = vm.Album
			entry.Runtime = vm.Runtime
			entry.Director = vm.Director
			entry.Cast = vm.Cast
			entry.Country = vm.Country
			entry.Language = vm.Language
			entry.Collection = vm.Collection
		}
		out = append(out, entry)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

func (c *serverCtx) handleLocalFile(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	// A vlc:// deep link hands this URL to a separate app that has no access
	// to the browser's session cookie, so a path-scoped token (see
	// handleLocalFileToken) is accepted as an alternative to a real session.
	if !c.isAuthed(r) && !c.validFileToken(path, r.URL.Query().Get("tok")) {
		http.Error(w, "unauthorized", 401)
		return
	}
	full, ok := c.resolveLocalPath(path)
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	// Go's content sniffer doesn't recognize Matroska's EBML signature and
	// would otherwise serve octet-stream, which makes some clients/players
	// treat a streamed MKV as an opaque download rather than a video. Setting
	// an explicit video/* type up front (http.ServeFile only sniffs when the
	// header isn't already set) keeps client-side handoff predictable.
	if ct, ok := videoContentTypes[strings.ToLower(filepath.Ext(full))]; ok {
		w.Header().Set("Content-Type", ct)
	}
	// http.ServeFile handles Range requests (seeking).
	http.ServeFile(w, r, full)
}

// handleLocalFileToken mints a short-lived, path-scoped token (see
// validFileToken) for the currently-authed browser session, so it can hand a
// playable URL to an external app like VLC without sharing its own cookie.
func (c *serverCtx) handleLocalFileToken(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	path := r.URL.Query().Get("path")
	if path == "" {
		http.Error(w, "missing path", 400)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{"token": c.makeFileToken(path)})
}

func (c *serverCtx) handleLocalSubtitle(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	full, ok := c.resolveLocalPath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	vttPath := detectLocalSubtitlePath(full)
	if vttPath == "" {
		http.Error(w, "no subtitle", 404)
		return
	}
	w.Header().Set("Content-Type", "text/vtt")
	http.ServeFile(w, r, vttPath)
}

func (c *serverCtx) handleLocalThumbnail(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	full, ok := c.resolveLocalPath(r.URL.Query().Get("path"))
	if !ok {
		http.Error(w, "not found", 404)
		return
	}
	thumbPath := localThumbnailPath(full)
	if thumbPath == "" {
		http.Error(w, "no thumbnail", 404)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	http.ServeFile(w, r, thumbPath)
}

var videoContentTypes = map[string]string{
	".mkv":  "video/x-matroska",
	".mp4":  "video/mp4",
	".m4v":  "video/x-m4v",
	".mov":  "video/quicktime",
	".webm": "video/webm",
	".avi":  "video/x-msvideo",
	".flv":  "video/x-flv",
	".wmv":  "video/x-ms-wmv",
}

func (c *serverCtx) handleDelete(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, "missing key", 400)
		return
	}

	// Delete main object + thumb + meta
	for _, k := range []string{key, thumbKey(key), metaKey(key)} {
		c.r2.DeleteObject(context.Background(), &s3.DeleteObjectInput{
			Bucket: awsv2.String(c.r2Bucket),
			Key:    awsv2.String(k),
		})
	}
	logger.Printf("deleted: %s", key)
	w.WriteHeader(200)
	fmt.Fprint(w, "ok")
}

func (c *serverCtx) handleQR(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	scheme := "http"
	if isHTTPS(r) {
		scheme = "https"
	}
	url := fmt.Sprintf("%s://%s", scheme, r.Host)
	png, err := qrcode.Encode(url, qrcode.Medium, 256)
	if err != nil {
		http.Error(w, "qr error", 500)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Write(png)
}

func (c *serverCtx) handleProcStatus(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	key := r.URL.Query().Get("key")
	ps, ok := c.getProcState(key)
	w.Header().Set("Content-Type", "application/json")
	if !ok {
		json.NewEncoder(w).Encode(map[string]string{"status": "done"})
		return
	}
	json.NewEncoder(w).Encode(ps)
}

func (c *serverCtx) readMeta(ctx context.Context, key string) (videoMeta, error) {
	obj, err := c.r2.GetObject(ctx, &s3.GetObjectInput{
		Bucket: awsv2.String(c.r2Bucket),
		Key:    awsv2.String(metaKey(key)),
	})
	if err != nil {
		return videoMeta{}, err
	}
	defer obj.Body.Close()
	var vm videoMeta
	if err := json.NewDecoder(obj.Body).Decode(&vm); err != nil {
		return videoMeta{}, err
	}
	return vm, nil
}

// handleFetchSubtitle retries an OpenSubtitles lookup for an already-processed
// video without re-running ffmpeg — just refreshes meta.json's subtitle_url.
func (c *serverCtx) handleFetchSubtitle(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if c.openSubsKey == "" || c.openSubsUser == "" {
		http.Error(w, "opensubtitles not configured", 400)
		return
	}
	ctx := context.Background()
	vm, err := c.readMeta(ctx, body.Key)
	if err != nil {
		http.Error(w, "no metadata for this file: "+err.Error(), 404)
		return
	}
	if vm.TMDBID == 0 {
		http.Error(w, "no TMDB match for this file, can't search subtitles", 400)
		return
	}
	vtt, err := c.fetchSubtitle(vm.TMDBID, vm.MediaType, vm.Season, vm.Episode)
	if err != nil {
		logger.Printf("subtitle retry %s: %v", body.Key, err)
		http.Error(w, err.Error(), 502)
		return
	}
	stem := strings.TrimSuffix(body.Key, filepath.Ext(body.Key))
	vttKey := stem + ".en.vtt"
	if _, err := c.r2.PutObject(ctx, &s3.PutObjectInput{
		Bucket:        awsv2.String(c.r2Bucket),
		Key:           awsv2.String(vttKey),
		Body:          bytes.NewReader(vtt),
		ContentLength: awsv2.Int64(int64(len(vtt))),
		ContentType:   awsv2.String("text/vtt"),
	}); err != nil {
		http.Error(w, "upload failed: "+err.Error(), 500)
		return
	}
	vm.SubtitleURL = strings.TrimRight(c.r2PubURL, "/") + "/" + vttKey
	c.writeEarlyMeta(ctx, body.Key, vm)
	logger.Printf("subtitle retry OK: %s", body.Key)
	w.WriteHeader(200)
	fmt.Fprint(w, "ok")
}

// handleRefreshMetadata re-runs LLM+TMDB enrichment for an already-processed
// video without re-encoding. Useful when enrichment failed the first time
// around (e.g. Ollama wasn't up yet right after a reboot) — the processed
// filename still carries everything parseMediaFilename needs.
func (c *serverCtx) handleRefreshMetadata(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		Key string `json:"key"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
		http.Error(w, "bad request", 400)
		return
	}
	ctx := context.Background()
	vm, err := c.readMeta(ctx, body.Key)
	if err != nil {
		http.Error(w, "no metadata for this file: "+err.Error(), 404)
		return
	}
	c.enrichMeta(filepath.Base(body.Key), &vm)
	if vm.TMDBID == 0 {
		http.Error(w, "still no TMDB match — check ollama/TMDB config and try again", 502)
		return
	}
	c.writeEarlyMeta(ctx, body.Key, vm)
	logger.Printf("metadata refresh OK: %s -> %s", body.Key, vm.Title)
	w.WriteHeader(200)
	fmt.Fprint(w, "ok")
}

// ─── Templates ────────────────────────────────────────────────────────────────

var loginTmpl = template.Must(template.New("login").Parse(`<!DOCTYPE html>
<html>
<head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Stream · Login</title>
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#0d0d0d;color:#e0e0e0;font-family:'SF Mono',Consolas,monospace;display:flex;align-items:center;justify-content:center;min-height:100vh}
.box{width:100%;max-width:340px;padding:2rem}
h1{font-size:1rem;font-weight:400;letter-spacing:.1em;color:#9A7D4A;margin-bottom:2rem}
input{width:100%;background:#1a1a1a;border:1px solid #333;color:#e0e0e0;padding:.75rem 1rem;font-family:inherit;font-size:.9rem;border-radius:4px;outline:none;margin-bottom:1rem}
input:focus{border-color:#9A7D4A}
button{width:100%;background:#9A7D4A;border:none;color:#0d0d0d;padding:.75rem;font-family:inherit;font-size:.9rem;font-weight:600;border-radius:4px;cursor:pointer}
.err{color:#e06c6c;font-size:.8rem;margin-bottom:1rem}
</style>
</head>
<body>
<div class="box">
  <h1>// stream</h1>
  {{if .Error}}<div class="err">{{.Error}}</div>{{end}}
  <form method="POST">
    <input type="password" name="password" placeholder="password" autofocus autocomplete="current-password">
    <button type="submit">enter</button>
  </form>
</div>
</body>
</html>`))

var mainTmpl = template.Must(template.New("main").Parse(`<!DOCTYPE html>
<html>
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Stream</title>
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=DM+Sans:wght@300;400;500&family=JetBrains+Mono:wght@300;400;500&display=swap" rel="stylesheet">
<style>
*{box-sizing:border-box;margin:0;padding:0}
:root{--bg:#0a0a0a;--bg2:#141414;--bg3:#1e1e1e;--border:#2a2a2a;--gold:#9A7D4A;--text:#e0e0e0;--muted:#666;--font-mono:'JetBrains Mono',Consolas,monospace;--font-sans:'DM Sans',-apple-system,sans-serif}
body{background:var(--bg);color:var(--text);font-family:var(--font-mono);font-weight:300;font-size:.875rem;min-height:100vh;overflow-x:hidden}
a{color:inherit;text-decoration:none}

/* Header */
header{display:flex;align-items:center;gap:1rem;padding:.75rem 1.25rem;border-bottom:1px solid var(--border);position:sticky;top:0;background:var(--bg);z-index:10}
.logo{color:var(--gold);font-size:.9rem;font-weight:300;letter-spacing:.1em;white-space:nowrap}
.spacer{flex:1}
.header-btn{background:none;border:1px solid var(--border);color:var(--muted);padding:.35rem .75rem;font-family:inherit;font-size:.75rem;border-radius:3px;cursor:pointer;transition:border-color .15s,color .15s}
.header-btn:hover,.header-btn.active{border-color:var(--gold);color:var(--gold)}
.session{color:var(--muted);font-size:.7rem}

/* Management controls hidden when viewing remotely (through the tunnel) */
body.remote .mgmt{display:none!important}

/* Toast */
#toast{position:fixed;left:50%;bottom:2rem;transform:translateX(-50%) translateY(1rem);background:var(--bg3);border:1px solid var(--gold);color:var(--text);padding:.65rem 1.1rem;border-radius:6px;font-size:.8rem;box-shadow:0 6px 24px rgba(0,0,0,.5);opacity:0;pointer-events:none;transition:opacity .2s,transform .2s;z-index:300;max-width:80vw}
#toast.show{opacity:1;transform:translateX(-50%) translateY(0)}
#toast.err{border-color:#e06c6c}

/* Section heading — a wayfinding label, not a heading: small, wide-tracked,
   no divider line, just air above it. */
.section-heading{grid-column:1/-1;color:var(--muted);font-family:var(--font-mono);font-size:9px;font-weight:300;letter-spacing:.15em;text-transform:uppercase;margin-top:1.5rem}
#file-list .section-heading{margin:1.5rem 0 .5rem}
#music-heading{color:var(--muted);font-family:var(--font-mono);font-size:9px;font-weight:300;letter-spacing:.15em;text-transform:uppercase;margin:56px 64px 1rem;display:none}

/* Music video cards (poster/gallery view) — plain 16:9 cover art. The
   landscape shape alone signals "this is video", same as the 2:3 shape
   signals "this is a film" — no film-strip decoration needed. */
.mv2-cover{aspect-ratio:16/9;border-radius:6px;overflow:hidden;background:#111;position:relative;display:flex;align-items:center;justify-content:center}
.mv2-cover img{width:100%;height:100%;object-fit:cover;display:block;transition:filter .2s}
.mv2-card:hover .mv2-cover img{filter:brightness(1.18)}
.mv2-noart{width:28%;opacity:.25;color:var(--muted)}

/* Music video cards (list view) — a plain 16:9 video frame, same as any
   other video thumbnail (it's a still pulled from the video itself now,
   not album art, so it should look like one). */
.mv-cover{aspect-ratio:16/9;border-radius:8px;overflow:hidden;background:#111;position:relative;display:flex;align-items:center;justify-content:center}
.mv-cover img{width:100%;height:100%;object-fit:cover;display:block}
.mv-noart{width:28%;opacity:.25}
.lr-poster.mv-cover{width:128px;height:72px;flex-shrink:0;border-radius:6px;aspect-ratio:unset}

/* Drop zone */
/* Controls */
#controls{display:flex;align-items:center;gap:.35rem;padding:0 1.25rem .85rem}
.view-btn{background:none;border:1px solid var(--border);color:var(--muted);padding:.3rem .6rem;font-family:inherit;font-size:1rem;line-height:1;border-radius:4px;cursor:pointer;transition:border-color .15s,color .15s}
.view-btn:hover{border-color:var(--gold);color:var(--gold)}
.view-btn.active{border-color:var(--gold);color:var(--gold);background:rgba(154,125,74,.12)}
#msg{margin-left:auto;font-size:.75rem}
#msg.ok{color:#6dbf6d}
#msg.err{color:#e06c6c}

/* ── Grid container ───────────────────────────────────────────────────────── */
#file-grid{display:grid;gap:24px;padding:0 64px 2rem;grid-template-columns:repeat(auto-fill,minmax(175px,1fr))}
#music-grid{display:none;grid-template-columns:repeat(auto-fill,minmax(215px,1fr));gap:24px 24px;row-gap:28px;padding:0 64px 2rem}

/* ── Shared card base ─────────────────────────────────────────────────────── */
.card{cursor:pointer;position:relative;background:none;border:none;border-radius:0;transition:transform .18s}
.card:focus,.lr:focus,.ep-row:focus,.view-btn:focus,#modal-actions button:focus,#modal-quality:focus{outline:2px solid var(--gold);outline-offset:2px}

/* ── POSTER cards — a gallery, not a data grid: cover art does the work,
   nothing sits on top of it. Dark theme hovers via a brightness lift; the
   light theme below overrides this with a physical lift + shadow instead. ── */
.pc-cover{aspect-ratio:2/3;border-radius:8px;overflow:hidden;background:var(--bg2);position:relative}
.pc-cover img{width:100%;height:100%;object-fit:cover;display:block;transition:filter .2s}
.card:hover .pc-cover img,.card:focus .pc-cover img{filter:brightness(1.18)}
.pc-proc{position:absolute;inset:0;display:flex;align-items:center;justify-content:center;font-size:1.5rem;color:var(--gold)}
.pc-badge-ep{position:absolute;top:.4rem;left:.4rem;font-size:.62rem;background:rgba(0,0,0,.78);padding:.12rem .35rem;border-radius:3px;line-height:1}
.pc-info{padding:.6rem .05rem .15rem}
.pc-title{font-family:var(--font-sans);font-size:12px;font-weight:400;color:#d0d0d0;line-height:1.3;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden;margin-bottom:.2rem}
.pc-year{font-family:var(--font-sans);font-size:10px;font-weight:300;color:#686868}

/* ── Light theme — warm parchment, not clinical white. Cards read as
   physical objects: a subtle shadow at rest that blooms on hover, lifted
   slightly off the surface, rather than the dark theme's brightness pop. ── */
body.light{--bg:#f3ede5;--bg2:#ede7df;--bg3:#e4dcd2;--border:#e4dcd2;--text:#1e1a16;--muted:#9a9088}
body.light .pc-title{color:#1e1a16}
body.light .pc-year{color:#9a9088}
body.light .pc-cover,body.light .mv2-cover{border-radius:3px;box-shadow:0 1px 8px rgba(60,40,20,.09);transition:box-shadow .2s ease}
body.light .card{transition:transform .2s ease}
body.light .card:hover{transform:translateY(-2px)}
body.light .card:hover .pc-cover,body.light .card:hover .mv2-cover{box-shadow:0 6px 24px rgba(60,40,20,.14)}
body.light .card:hover .pc-cover img,body.light .card:focus .pc-cover img{filter:none}
body.light .card:hover .mv2-cover img{filter:none}

/* shared legacy classes still used by show-view/note */
.card-note{padding:.75rem;font-size:.8rem;color:var(--muted);line-height:1.5;white-space:pre-wrap;word-break:break-word}
.card-actions{display:flex;gap:.4rem;padding:.45rem .75rem;border-top:1px solid var(--border)}
.card-actions button{flex:1;background:none;border:1px solid var(--border);color:var(--muted);padding:.32rem;font-family:inherit;font-size:.72rem;border-radius:3px;cursor:pointer;transition:border-color .12s,color .12s}
.card-actions button:hover{border-color:var(--gold);color:var(--gold)}
/* Search overlay */
#search-overlay{display:none;position:fixed;inset:0;background:rgba(0,0,0,.85);z-index:200;align-items:flex-start;justify-content:center;padding-top:6vh}
#search-overlay.open{display:flex}
#search-box{width:100%;max-width:760px;background:#111;border:1px solid var(--border);border-radius:8px;overflow:hidden;max-height:88vh;display:flex;flex-direction:column}
#search-top{display:flex;gap:.5rem;padding:.75rem}
#search-input{flex:1;background:#1a1a1a;border:1px solid var(--border);color:var(--fg);padding:.6rem .9rem;font-family:inherit;font-size:.9rem;border-radius:4px;outline:none}
#search-input:focus{border-color:var(--gold)}
#search-close-btn{background:none;border:1px solid var(--border);color:var(--muted);width:2.2rem;height:2.2rem;border-radius:4px;cursor:pointer;font-size:1.1rem;flex-shrink:0}
#search-tv-opts{display:none;flex-direction:column;gap:.7rem;padding:.85rem .9rem;border-top:1px solid var(--border);font-size:.8rem;color:var(--muted)}
#search-selected{display:flex;gap:.7rem;align-items:center}
#search-selected img{width:40px;border-radius:3px;flex-shrink:0;object-fit:cover}
#search-selected-info{flex:1;min-width:0}
#search-selected-title{font-size:.88rem;font-weight:500;color:var(--fg)}
#search-selected-meta{font-size:.7rem;color:var(--muted)}
.st-mode-row{display:flex;gap:.4rem}
.st-mode-btn{flex:1;background:none;border:1px solid var(--border);color:var(--muted);padding:.4rem .5rem;border-radius:4px;cursor:pointer;font-family:inherit;font-size:.74rem;text-align:center;transition:border-color .12s,color .12s}
.st-mode-btn.active{border-color:var(--gold);color:var(--gold);background:rgba(154,125,74,.1)}
.st-pick-row{display:flex;gap:.5rem}
.st-pick-row select{flex:1;background:#1a1a1a;border:1px solid var(--border);color:var(--fg);padding:.4rem .5rem;font-family:inherit;font-size:.78rem;border-radius:4px;outline:none}
.st-pick-row select:disabled{opacity:.4}
#search-torrents-btn,#search-back-btn{background:none;border:1px solid var(--border);color:var(--fg);padding:.45rem .9rem;border-radius:4px;cursor:pointer;font-family:inherit;font-size:.8rem}
#search-torrents-btn{border-color:var(--gold);color:var(--gold);flex:1;font-weight:500}
.st-btn-row{display:flex;gap:.5rem}
#search-results{overflow-y:auto;flex:1}
.sr-tmdb{display:flex;gap:.85rem;padding:.75rem;border-bottom:1px solid var(--border);cursor:pointer;align-items:flex-start}
.sr-tmdb:hover{background:rgba(255,255,255,.04)}
.sr-tmdb img{width:50px;border-radius:3px;flex-shrink:0;object-fit:cover}
.sr-tmdb-info{flex:1;min-width:0}
.sr-tmdb-title{font-size:.88rem;font-weight:500;margin-bottom:.2rem}
.sr-tmdb-meta{font-size:.73rem;color:var(--muted);margin-bottom:.3rem}
.sr-tmdb-overview{font-size:.73rem;color:var(--muted);line-height:1.5;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.sr-torrent{display:flex;align-items:center;gap:.75rem;padding:.6rem .75rem;border-bottom:1px solid var(--border);font-size:.8rem}
.sr-torrent:hover{background:rgba(255,255,255,.04)}
.sr-qual{padding:.1rem .45rem;border-radius:2px;font-size:.68rem;font-weight:600;background:#1e3a5f;color:#6eb3f7;flex-shrink:0}
.sr-qual.hd{background:#1a3a1a;color:#6fcf6f}
.sr-torrent-title{flex:1;min-width:0;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;color:var(--muted)}
.sr-seeds{color:#6fcf6f;flex-shrink:0;min-width:3rem;text-align:right}
.sr-size{color:var(--muted);flex-shrink:0;min-width:4rem;text-align:right}
.sr-dl-btn{flex-shrink:0;background:none;border:1px solid var(--gold);color:var(--gold);padding:.25rem .65rem;border-radius:3px;cursor:pointer;font-family:inherit;font-size:.75rem}
.sr-dl-btn:disabled{opacity:.4;cursor:default}
.sr-msg{padding:1.5rem;text-align:center;color:var(--muted);font-size:.85rem}
/* Film detail metadata grid */
.fm-meta-grid{grid-column:1/-1;display:grid;grid-template-columns:repeat(3,1fr);gap:1.2rem 2rem;margin:2rem 0 1.5rem;padding:1.5rem 0;border-top:1px solid var(--border)}
.fm-meta-cell{}
.fm-meta-label{font-family:var(--font-mono);font-size:.68rem;letter-spacing:.08em;color:var(--muted);text-transform:uppercase;margin-bottom:.35rem}
.fm-meta-value{font-family:var(--font-sans);font-size:.88rem;color:var(--text);line-height:1.5}
.fm-collection{grid-column:1/-1;font-family:var(--font-mono);font-size:.75rem;color:var(--muted);padding-top:.5rem;border-top:1px solid var(--border);margin-top:.5rem}
.fm-collection span{color:var(--gold);cursor:pointer}
.fm-collection span:hover{text-decoration:underline}

/* ── Responsive ───────────────────────────────────────────────────────────── */
@media(max-width:900px){
  #file-grid{padding:0 32px 2rem}
  #music-grid{padding:0 32px 2rem}
  #music-heading{margin:56px 32px 1rem}
  .sv-back{left:32px}
  .sv-hero-top{gap:32px}
  .sv-poster{width:130px}
  .sv-title{font-size:2rem}
  .sv-info{padding-top:88px}
  .sv-overview{margin-left:162px}
  .sv-play-btn{margin-left:162px}
}
@media(max-width:600px){
  header{padding:.6rem 1rem;gap:.5rem;flex-wrap:nowrap}
  .session{display:none}
  #toggle-login-btn{display:none}
  #search-open-btn{display:none}
  .header-btn{padding:.28rem .55rem;font-size:.7rem}
  #controls{padding:0 1rem .7rem}
  #file-grid{padding:0 1rem 2rem;gap:14px;grid-template-columns:repeat(auto-fill,minmax(130px,1fr))}
  #music-grid{padding:0 1rem 2rem;grid-template-columns:repeat(auto-fill,minmax(160px,1fr));gap:16px}
  #music-heading{margin:40px 1rem .75rem}
  #file-list{padding:0 1rem 2rem}

  /* Cinematic detail — backdrop */
  .sv-backdrop{height:220px}
  .sv-back{left:1rem;font-size:.75rem}

  /* Hero: poster+title side by side, description+play full-width below */
  .sv-hero{margin-top:-56px}
  .sv-hero-top{gap:14px}
  .sv-poster{width:90px}
  .sv-info{padding-top:58px}
  .sv-title{font-size:1.4rem}
  .sv-meta{font-size:.72rem;gap:.4rem;flex-wrap:wrap}
  .sv-genres{font-size:.7rem}
  .sv-overview{font-size:.85rem;max-width:100%;margin-left:0}
  .sv-play-btn{margin-left:0;font-size:.72rem;padding:.45rem .9rem;align-self:flex-start}

  .sv-season-tabs{gap:1rem;margin:1.2rem 0 0}
  .sv-season-tab{font-size:.75rem}
  .sv-ep-still{width:96px}
  .sv-ep-title{font-size:.9rem}
  .sv-ep-desc{font-size:.8rem}
  .sv-ep-dur{font-size:.72rem}
  .fm-meta-grid{grid-template-columns:repeat(2,1fr);gap:.9rem 1.5rem}
  .fm-meta-label{font-size:.63rem}
  .fm-meta-value{font-size:.82rem}
  .lr{gap:.75rem;padding:.75rem 0}
  .lr-poster{width:56px}
  .lr-right{min-width:52px}
  .lr-overview{display:none}
  #search-box{max-height:95vh;border-radius:0}
  #search-overlay{padding-top:0}
}
.season-picker{grid-column:1/-1;display:flex;gap:.5rem;flex-wrap:wrap;padding:0 0 1rem;margin-bottom:.5rem;border-bottom:1px solid var(--border)}
.season-btn{background:none;border:1px solid var(--border);color:var(--muted);padding:.4rem .9rem;font-family:inherit;font-size:.8rem;border-radius:4px;cursor:pointer;transition:border-color .15s,color .15s}
.season-btn:hover{border-color:var(--gold);color:var(--gold)}
.season-btn.active{border-color:var(--gold);color:var(--gold);background:rgba(154,125,74,.12)}
.ep-row{grid-column:1/-1;display:flex;gap:1rem;padding:.85rem 0;border-bottom:1px solid var(--border);align-items:center;cursor:pointer}
.ep-row:hover{background:rgba(255,255,255,.03)}
.ep-still{width:160px;height:90px;object-fit:cover;border-radius:4px;flex-shrink:0}
.ep-no-still{width:160px;height:90px;background:var(--border);border-radius:4px;display:flex;align-items:center;justify-content:center;font-size:1.5rem;color:var(--muted);flex-shrink:0}
.ep-info{flex:1;min-width:0}
.ep-code{font-size:.7rem;color:var(--gold);margin-bottom:.2rem}
.ep-title{font-size:.88rem;font-weight:500;margin-bottom:.3rem;white-space:nowrap;overflow:hidden;text-overflow:ellipsis}
.ep-desc{font-size:.75rem;color:var(--muted);line-height:1.5;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.ep-dur{font-size:.7rem;color:var(--muted);margin-top:.3rem}
.ep-play-btn{flex-shrink:0;background:none;border:1px solid var(--border);color:var(--fg);padding:.4rem .9rem;border-radius:3px;cursor:pointer;font-family:inherit;font-size:.85rem}
.card-meta{font-size:.7rem;color:var(--muted);display:flex;justify-content:space-between}

/* ── Series detail (cinematic) — full-bleed backdrop, poster floats over
   the fade. Most immersive of the layouts tried, closest to Apple TV. ── */
.sv-wrap{grid-column:1/-1}
.sv-backdrop{position:relative;height:400px;overflow:hidden;width:100vw;margin-left:calc(50% - 50vw)}
.sv-backdrop img{width:100%;height:100%;object-fit:cover;object-position:center top;display:block}
.sv-backdrop-fade{position:absolute;inset:0;background:linear-gradient(to bottom,rgba(0,0,0,.1) 0%,var(--bg) 100%)}
.sv-back{position:absolute;top:16px;left:64px;background:none;border:none;color:var(--muted);font-family:var(--font-mono);font-size:.82rem;letter-spacing:.05em;cursor:pointer;z-index:2}
.sv-back:hover{color:var(--gold)}
.sv-hero{display:flex;flex-direction:column;margin-top:-96px;position:relative;z-index:2}
.sv-hero-top{display:flex;gap:40px;align-items:flex-start}
.sv-poster{flex-shrink:0;width:160px;aspect-ratio:2/3;border-radius:4px;object-fit:cover;box-shadow:0 8px 36px rgba(0,0,0,.5)}
.sv-poster-ph{display:flex;align-items:center;justify-content:center;background:var(--bg2);font-size:2.5rem}
.sv-info{flex:1;padding-top:104px;min-width:0}
.sv-title{font-family:var(--font-sans);font-size:2.5rem;font-weight:500;color:var(--text);letter-spacing:-.02em;margin:0 0 .5rem;line-height:1.1}
.sv-meta{display:flex;align-items:center;gap:.7rem;font-family:var(--font-mono);font-size:.82rem;color:var(--muted);margin-bottom:.6rem}
.sv-meta-sep{color:var(--border)}
.sv-rating{color:var(--gold)}
.sv-genres{font-family:var(--font-mono);font-size:.8rem;color:var(--muted);margin-bottom:1rem}
.sv-overview{font-family:var(--font-sans);font-size:.95rem;line-height:1.7;color:var(--text);max-width:520px;margin:0 0 1.3rem 200px}
.sv-play-btn{display:inline-flex;margin-left:200px;padding:.55rem 1.1rem;border:1px solid var(--border);border-radius:2px;background:none;color:var(--text);font-family:var(--font-mono);font-size:.8rem;letter-spacing:.08em;cursor:pointer;transition:border-color .15s,color .15s}
.sv-play-btn:hover{border-color:var(--gold);color:var(--gold)}

.sv-season-tabs{grid-column:1/-1;display:flex;gap:1.5rem;border-bottom:1px solid var(--border);margin:1.8rem 0 0}
.sv-season-tab{font-family:var(--font-mono);font-size:.82rem;letter-spacing:.05em;color:var(--muted);padding-bottom:.7rem;border-bottom:1px solid transparent;cursor:pointer}
.sv-season-tab:hover{color:var(--text)}
.sv-season-tab.active{color:var(--text);border-color:var(--text)}

.sv-ep-row{grid-column:1/-1;display:flex;align-items:flex-start;gap:1.25rem;padding:1rem 0;border-bottom:1px solid var(--border);cursor:pointer}
.sv-ep-still{flex-shrink:0;width:140px;aspect-ratio:16/9;object-fit:cover;border-radius:3px}
.sv-ep-no-still{display:flex;align-items:center;justify-content:center;background:var(--bg2);color:var(--muted);font-size:1.3rem}
.sv-ep-info{flex:1;min-width:0;padding-top:.1rem}
.sv-ep-code{font-family:var(--font-mono);font-size:.75rem;color:var(--muted);margin-bottom:.4rem}
.sv-ep-title{font-family:var(--font-sans);font-size:1rem;color:var(--text);margin-bottom:.3rem}
.sv-ep-desc{font-family:var(--font-sans);font-size:.88rem;line-height:1.5;color:var(--muted);display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.sv-ep-dur{flex-shrink:0;font-family:var(--font-mono);font-size:.8rem;color:var(--muted);padding-top:.15rem}
.card-note{padding:.75rem;font-size:.8rem;color:var(--muted);line-height:1.5;white-space:pre-wrap;word-break:break-word}
.card-actions{display:flex;gap:.4rem;padding:.5rem .75rem;border-top:1px solid var(--border)}
.card-actions button{flex:1;background:none;border:1px solid var(--border);color:var(--muted);padding:.3rem;font-family:inherit;font-size:.7rem;border-radius:3px;cursor:pointer}
.card-actions button:hover{border-color:var(--gold);color:var(--gold)}

/* ── LIST view ────────────────────────────────────────────────────────────── */
#file-list{display:none;padding:0 1.25rem 2rem}
.lr{display:flex;gap:1.1rem;padding:1rem 0;border-bottom:1px solid var(--border);cursor:pointer;align-items:flex-start;border-radius:6px;transition:background .1s}
.lr:hover{background:rgba(255,255,255,.03)}
.lr-poster{width:72px;flex-shrink:0;border-radius:6px;overflow:hidden;background:var(--bg2);position:relative;aspect-ratio:2/3}
.lr-poster img{width:100%;height:100%;object-fit:cover;display:block}
.lr-poster-ph{width:100%;height:100%;display:flex;align-items:center;justify-content:center;font-size:1.6rem}
.lr-ep-badge{position:absolute;bottom:0;left:0;right:0;text-align:center;font-size:.6rem;background:rgba(0,0,0,.8);padding:.15rem 0;letter-spacing:.03em}
.lr-body{flex:1;min-width:0;padding-top:.05rem}
.lr-type{font-size:.63rem;letter-spacing:.1em;text-transform:uppercase;color:var(--gold);margin-bottom:.28rem;font-weight:600}
.lr-title{font-size:.98rem;font-weight:600;line-height:1.3;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden;margin-bottom:.28rem}
.lr:hover .lr-title{color:var(--gold)}
.lr-genres{font-size:.72rem;color:var(--muted);margin-bottom:.3rem}
.lr-overview{font-size:.73rem;color:var(--muted);line-height:1.55;display:-webkit-box;-webkit-line-clamp:3;-webkit-box-orient:vertical;overflow:hidden}
.lr-right{display:flex;flex-direction:column;align-items:flex-end;gap:.55rem;flex-shrink:0;padding-top:.05rem;min-width:72px}
.lr-rating{font-size:.9rem;color:#f5c518;font-weight:500}
.lr-dur{font-size:.7rem;color:var(--muted)}
.lr-btns{display:flex;gap:.3rem;margin-top:auto}
.lr-btn{background:none;border:1px solid var(--border);color:var(--muted);padding:.3rem .5rem;font-family:inherit;font-size:.72rem;border-radius:3px;cursor:pointer;transition:border-color .12s,color .12s}
.lr-btn:hover{border-color:var(--gold);color:var(--gold)}

/* Video modal */
#modal{display:none;position:fixed;inset:0;background:rgba(0,0,0,.92);z-index:100;align-items:center;justify-content:center;flex-direction:column;padding:1rem}
#modal.open{display:flex}
#modal-close{position:absolute;top:1rem;right:1.25rem;background:none;border:none;color:var(--muted);font-size:1.5rem;cursor:pointer;line-height:1}
#modal-close:hover{color:var(--text)}
#modal video{max-width:100%;max-height:80vh;border-radius:4px;background:#000}
#modal-note{max-width:640px;width:100%;background:var(--bg2);border-radius:6px;padding:1.5rem;font-size:.9rem;line-height:1.7;white-space:pre-wrap;word-break:break-word;max-height:70vh;overflow-y:auto}
#modal-filename{font-size:.8rem;color:var(--muted);margin-top:.75rem;text-align:center}
#modal-actions{display:flex;gap:.75rem;margin-top:.75rem}
#modal-actions button{background:none;border:1px solid var(--border);color:var(--muted);padding:.4rem 1rem;font-family:inherit;font-size:.8rem;border-radius:3px;cursor:pointer}
#modal-actions button:hover{border-color:var(--gold);color:var(--gold)}

/* File input hidden */
#file-input{display:none}

/* Scrollbar */
::-webkit-scrollbar{width:6px}
::-webkit-scrollbar-track{background:var(--bg)}
::-webkit-scrollbar-thumb{background:var(--border);border-radius:3px}
</style>
</head>
<body>

<header>
  <span class="logo">// stream</span>
  <button class="view-btn" id="btn-poster" title="Poster">⊞</button>
  <button class="view-btn" id="btn-list" title="List">≡</button>
  <span class="spacer"></span>
  <span class="session">{{.SessionHours}}h</span>
  {{if .HasLocalLib}}<button class="header-btn mgmt" id="btn-source" title="Toggle cloud / local library">cloud</button>{{end}}
  <button class="header-btn mgmt" id="toggle-login-btn" title="Toggle login page">{{if .LoginDisabled}}login off{{else}}login on{{end}}</button>
  <button class="header-btn" id="theme-toggle-btn" title="Toggle light/dark theme">light</button>
  <a href="/qr.png" target="_blank"><button class="header-btn">qr</button></a>
  <button class="header-btn mgmt" id="search-open-btn">search</button>
</header>

<div id="search-overlay">
  <div id="search-box">
    <div id="search-top">
      <input id="search-input" type="text" placeholder="search for a movie or show…" autocomplete="off">
      <button id="search-close-btn">&times;</button>
    </div>
    <div id="search-tv-opts">
      <div id="search-selected"></div>
      <div class="st-mode-row">
        <button class="st-mode-btn" data-mode="episode">Episode</button>
        <button class="st-mode-btn" data-mode="season">Season Pack</button>
        <button class="st-mode-btn" data-mode="all">Complete Series</button>
      </div>
      <div class="st-pick-row">
        <select id="search-season-sel" disabled><option value="">Season…</option></select>
        <select id="search-episode-sel" disabled><option value="">Episode…</option></select>
      </div>
      <div class="st-btn-row">
        <button id="search-back-btn">← back</button>
        <button id="search-torrents-btn">Search Torrents</button>
      </div>
    </div>
    <div id="search-results"></div>
  </div>
</div>

<div id="controls"><span id="msg"></span></div>

<div id="file-grid"></div>
<div id="music-heading">Music Videos</div>
<div id="music-grid"></div>
<div id="file-list"></div>
<div id="toast"></div>

<div id="modal">
  <button id="modal-close">&times;</button>
  <video id="modal-video" controls crossorigin="anonymous" style="display:none"></video>
  <div id="modal-note" style="display:none"></div>
  <div id="modal-filename"></div>
  <div id="modal-actions">
    <select id="modal-quality" style="display:none"></select>
    <button id="modal-dl">↓ download</button>
    <button id="modal-del">✕ delete</button>
    <button id="modal-copy" style="display:none">⎘ copy text</button>
    <button id="modal-cc" style="display:none">CC get subtitles</button>
    <button id="modal-meta" style="display:none">↻ redo metadata</button>
  </div>
</div>

<script src="https://cdn.jsdelivr.net/npm/hls.js@1.5.15/dist/hls.min.js"></script>
<script>
// ── State ──────────────────────────────────────────────────────────────────
var R2_PUB = "{{.R2PubURL}}";
var IS_LOCAL = {{if .IsLocal}}true{{else}}false{{end}};
var HAS_LOCAL_LIB = {{if .HasLocalLib}}true{{else}}false{{end}};
var currentFiles = [];
var cloudFiles = [];
var localFiles = [];
var currentKey = '';
var currentIsNote = false;
var currentShow = null;
var hlsInstance = null;
var libSource = localStorage.getItem('stream-source') || (HAS_LOCAL_LIB ? 'local' : 'cloud'); // 'cloud' | 'local'

var msg        = document.getElementById('msg');
var fileGrid   = document.getElementById('file-grid');
var musicGrid  = document.getElementById('music-grid');
var musicHeading = document.getElementById('music-heading');
var fileList   = document.getElementById('file-list');
var modal      = document.getElementById('modal');
var modalVideo = document.getElementById('modal-video');
var modalNote  = document.getElementById('modal-note');
var modalFname = document.getElementById('modal-filename');
var modalDl    = document.getElementById('modal-dl');
var modalDel   = document.getElementById('modal-del');
var modalCopy  = document.getElementById('modal-copy');
var modalCC    = document.getElementById('modal-cc');
var modalMeta  = document.getElementById('modal-meta');
var modalQuality = document.getElementById('modal-quality');
var btnPoster  = document.getElementById('btn-poster');
var btnList    = document.getElementById('btn-list');
var toggleLoginBtn = document.getElementById('toggle-login-btn');

// ── View toggle ────────────────────────────────────────────────────────────
function setView(v) {
  viewMode = v;
  localStorage.setItem('stream-view', v);
  [btnPoster, btnList].forEach(function(b){ b.classList.remove('active'); });
  if (v === 'list') {
    fileGrid.style.display = 'none';
    musicGrid.style.display = 'none';
    musicHeading.style.display = 'none';
    fileList.style.display = 'block';
    btnList.classList.add('active');
    renderList(currentFiles);
  } else {
    fileGrid.style.display = 'grid';
    fileList.style.display = 'none';
    btnPoster.classList.add('active');
    renderGrid(currentFiles);
  }
}
btnPoster.addEventListener('click', function(){ setView('poster'); });
btnList.addEventListener('click', function(){ setView('list'); });
var viewMode = localStorage.getItem('stream-view') === 'list' ? 'list' : 'poster';
setView(viewMode);

// ── Theme toggle ───────────────────────────────────────────────────────────
var themeToggleBtn = document.getElementById('theme-toggle-btn');
function setTheme(t) {
  document.body.classList.toggle('light', t === 'light');
  themeToggleBtn.textContent = t === 'light' ? 'dark' : 'light';
  localStorage.setItem('stream-theme', t);
}
themeToggleBtn.addEventListener('click', function(){
  setTheme(document.body.classList.contains('light') ? 'dark' : 'light');
});
setTheme(localStorage.getItem('stream-theme') || 'dark');

// ── Toggle login ───────────────────────────────────────────────────────────
toggleLoginBtn.addEventListener('click', function(){
  fetch('/toggle-login', {method:'POST', credentials:'include'}).then(function(){
    var on = toggleLoginBtn.textContent.trim() === 'login on';
    toggleLoginBtn.textContent = on ? 'login off' : 'login on';
  });
});

// ── Remote vs local: hide management UI when viewing through the tunnel ──────
document.body.classList.toggle('remote', !IS_LOCAL);

// ── Cloud / Local library source toggle ─────────────────────────────────────
// The local library reuses the exact same grouping/card rendering as the
// cloud library (groupBySeries, renderGrid/renderList, openFile) — it's just
// a different array behind currentFiles, with playback/handoff branching on
// f.is_local inside openFile instead of separate markup.
var btnSource = document.getElementById('btn-source');
function setSource(src) {
  libSource = src;
  localStorage.setItem('stream-source', src);
  currentShow = null;
  currentMovieDetail = null;
  if (btnSource) btnSource.textContent = src;
  if (src === 'local') {
    currentFiles = localFiles;
    setView(viewMode);
    loadLocalFiles();
  } else {
    currentFiles = cloudFiles;
    setView(viewMode);
  }
}
if (btnSource) {
  btnSource.addEventListener('click', function(){ setSource(libSource === 'cloud' ? 'local' : 'cloud'); });
}

// ── Local library (LAN-served, no conversion, TMDB/LLM-enriched) ────────────
function loadLocalFiles() {
  fetch('/local/files', {credentials:'include'})
    .then(function(r){ return r.json(); })
    .then(function(items){
      localFiles = items || [];
      if (libSource === 'local') {
        currentFiles = localFiles;
        if (viewMode === 'list') { renderList(currentFiles); } else { renderGrid(currentFiles); }
      }
      // Keep polling while any file is still waiting on its LLM/TMDB lookup
      // so tiles fill in with posters/titles as enrichment completes.
      var anyIdentifying = localFiles.some(function(f){ return !f.title && !f.tmdb_id; });
      if (anyIdentifying) setTimeout(loadLocalFiles, 4000);
    });
}

var toastTimer = null;
function showToast(text, isErr) {
  var t = document.getElementById('toast');
  t.textContent = text;
  t.className = isErr ? 'err show' : 'show';
  clearTimeout(toastTimer);
  toastTimer = setTimeout(function(){ t.className = isErr ? 'err' : ''; }, 3000);
}

// True Safari only — excludes Chrome/Firefox/Edge on iOS, which report a
// Safari-looking UA but add their own token (CriOS/FxiOS/Edg) since they're
// just a UI shell over WebKit, not actually Safari.
function isSafariBrowser() {
  return /Safari/.test(navigator.userAgent) && /Apple Computer/.test(navigator.vendor || '') &&
    !/Chrome|CriOS|Chromium|Edg|OPR|FxiOS/.test(navigator.userAgent);
}

// Hands an unplayable local file's direct URL to THIS device's OS — the
// server can never open an app on a screen it isn't running on, so playback
// has to be delegated to whatever's installed where the page is being viewed.
function handOffLocalFile(f) {
  var fileUrl = location.origin + '/local/file?path=' + encodeURIComponent(f.key);
  showToast('Opening ' + (f.title || f.name) + ' in your video app…');
  if (/Android/i.test(navigator.userAgent)) {
    // Android: trigger the native "open with" chooser for installed video
    // apps (VLC, MX Player, etc.), which then stream the URL directly.
    // Deliberately no S.browser_fallback_url and no window.location.href:
    // Chrome sometimes can't confirm the external app actually launched and
    // queues the fallback navigation to fire later, when you return to this
    // tab — that's the "opens again with the file path in the URL" bug.
    // Firing the intent from a throwaway iframe instead means even a wrongly
    // queued fallback navigates that iframe, not this page.
    var scheme = fileUrl.indexOf('https://') === 0 ? 'https' : 'http';
    var bare = fileUrl.replace(/^https?:\/\//, '');
    var intentUrl = 'intent://' + bare +
      '#Intent;scheme=' + scheme + ';type=video/*;action=android.intent.action.VIEW;end';
    var frame = document.createElement('iframe');
    frame.style.display = 'none';
    frame.src = intentUrl;
    document.body.appendChild(frame);
    setTimeout(function(){ frame.remove(); }, 3000);
  } else if (isSafariBrowser()) {
    // Safari has no Android-style "open with" chooser — handing it a URL
    // whose content-type it can't render just downloads the file. VLC
    // registers a vlc:// scheme on both iOS and macOS that streams the URL
    // straight into the app instead, but VLC's own request for that URL
    // never carries this page's session cookie, so it needs a short-lived
    // token (path-scoped, 5min) minted just for this handoff.
    fetch('/local/file-token?path=' + encodeURIComponent(f.key), {credentials:'include'})
      .then(function(r){ return r.json(); })
      .then(function(d){
        window.location.href = 'vlc://' + fileUrl + '&tok=' + encodeURIComponent(d.token);
      });
  } else {
    // Desktop / other: open the file URL in a new tab so the SPA isn't
    // navigated away. The OS or browser's built-in handler picks it up
    // (download prompt, a registered video player, or the browser's own
    // player), and the user stays on the SPA in the original tab.
    window.open(fileUrl, '_blank');
  }
}

function fmtDur(s) {
  if (!s) return '';
  var h = Math.floor(s/3600), m = Math.floor((s%3600)/60), sec = s%60;
  if (h > 0) return h+':'+pad(m)+':'+pad(sec);
  return m+':'+pad(sec);
}
function pad(n){ return n < 10 ? '0'+n : ''+n; }
function fmtSize(b) {
  if (b >= 1073741824) return (b/1073741824).toFixed(1)+' GB';
  if (b >= 1048576) return (b/1048576).toFixed(1)+' MB';
  if (b >= 1024) return (b/1024).toFixed(1)+' KB';
  return b+' B';
}

// ── File listing ───────────────────────────────────────────────────────────
function loadFiles() {
  fetch('/files', {credentials:'include'})
    .then(function(r){ return r.json(); })
    .then(function(files){
      cloudFiles = files || [];
      if (libSource === 'cloud') {
        currentFiles = cloudFiles;
        if (viewMode === 'list') { renderList(currentFiles); } else { renderGrid(currentFiles); }
      }
      // Poll if any are processing
      var anyProc = cloudFiles.some(function(f){ return f.proc_state === 'processing'; });
      if (anyProc) setTimeout(loadFiles, 3000);
    });
}

function fileIcon(f) {
  if (f.is_video) return '▶';
  if (f.is_note) return '✎';
  var ext = f.name.split('.').pop().toLowerCase();
  if (['jpg','jpeg','png','gif','webp','svg'].includes(ext)) return '🖼';
  if (['pdf'].includes(ext)) return '📄';
  if (['zip','rar','7z','tar','gz'].includes(ext)) return '📦';
  if (['mp3','wav','flac','ogg','aac'].includes(ext)) return '♫';
  return '📁';
}

function groupBySeries(files) {
  var shows = {}, standalone = [];
  files.forEach(function(f) {
    if (f.tmdb_id && f.media_type === 'tv') {
      if (!shows[f.tmdb_id]) shows[f.tmdb_id] = {id: f.tmdb_id, episodes: []};
      shows[f.tmdb_id].episodes.push(f);
    } else {
      standalone.push(f);
    }
  });
  return {shows: Object.values(shows), standalone: standalone};
}

function titleOf(x) {
  var t = x._show ? (x._show.episodes[0].title || '') : (x.title || x.name || '');
  return t.toLowerCase();
}
function sortByTitle(arr) {
  return arr.slice().sort(function(a,b){ return titleOf(a).localeCompare(titleOf(b)); });
}

function renderFileCard(f) {
  var card = document.createElement('div');
  card.className = 'card';
  card.tabIndex = 0;
  if (f.is_note) {
    card.innerHTML = '<div class="card-note">'+esc(f.snippet||f.name)+'</div>' +
      '<div class="card-actions"><button onclick="openFile(event,\''+esc(f.key)+'\',true)">▶</button><button onclick="deleteFile(event,\''+esc(f.key)+'\')">✕</button></div>';
    card.addEventListener('click', function(e){ if (e.target.tagName==='BUTTON') return; openFile(e, f.key, true); });
    fileGrid.appendChild(card); return;
  }
  var title = f.title || f.name;
  var stillIdentifying = f.is_local && !f.title && !f.tmdb_id;
  var img = f.poster_url || f.thumb_url;
  var cover = f.proc_state==='processing'
    ? '<div class="pc-cover"><div class="pc-proc">⟳</div></div>'
    : f.proc_state==='error'
      ? '<div class="pc-cover" style="background:rgba(90,20,20,.7)"><div class="pc-proc" style="color:#e06c6c">✕</div></div>'
      : stillIdentifying
        ? '<div class="pc-cover" style="display:flex;align-items:center;justify-content:center"><span style="font-size:1.6rem;color:var(--muted)">⟳</span></div>'
      : img
        ? '<div class="pc-cover"><img src="'+esc(img)+'" loading="lazy"></div>'
        : '<div class="pc-cover" style="display:flex;align-items:center;justify-content:center"><span style="font-size:2.5rem">'+fileIcon(f)+'</span></div>';
  card.innerHTML = cover + '<div class="pc-info"><div class="pc-title">'+esc(title)+'</div>'+(f.year?'<div class="pc-year">'+f.year+'</div>':'')+'</div>';
  card.addEventListener('click', function(e){
    if (e.target.tagName==='BUTTON') return;
    if (f.proc_state==='processing') return;
    openFile(e, f.key, false);
  });
  fileGrid.appendChild(card);
}

function renderShowCard(show) {
  var card = document.createElement('div');
  card.className = 'card';
  card.tabIndex = 0;
  var ep = show.episodes[0];
  var title = ep.title || '';
  var img = ep.poster_url || ep.thumb_url;
  card.innerHTML =
    '<div class="pc-cover">' +
      (img?'<img src="'+esc(img)+'" loading="lazy">':'<div style="display:flex;align-items:center;justify-content:center;height:100%;font-size:2.5rem">📺</div>') +
      '<span class="pc-badge-ep">'+show.episodes.length+' ep</span>' +
    '</div>' +
    '<div class="pc-info"><div class="pc-title">'+esc(title)+'</div>'+(ep.year?'<div class="pc-year">'+ep.year+'</div>':'')+'</div>';
  card.addEventListener('click', function() { openShow(show); });
  fileGrid.appendChild(card);
}

var currentSeason = null;

function openShow(show) {
  currentShow = show;
  currentSeason = null;
  history.pushState({type:'show', id: show.id}, '', '#show=' + show.id);
  window.scrollTo(0, 0);
  renderGrid(currentFiles);
}

function closeShow() {
  currentShow = null;
  currentSeason = null;
  history.back();
}

function renderShowView(show) {
  var ep = show.episodes[0];
  var backdropSrc = ep.tmdb_backdrop || ep.poster_url;
  var posterSrc = ep.poster_url || ep.thumb_url;

  var episodes = show.episodes.slice().sort(function(a, b) {
    if ((a.season||0) !== (b.season||0)) return (a.season||0) - (b.season||0);
    return (a.episode||0) - (b.episode||0);
  });
  var seasons = [];
  episodes.forEach(function(f) {
    var sn = f.season || 0;
    if (seasons.indexOf(sn) === -1) seasons.push(sn);
  });
  seasons.sort(function(a,b){ return a-b; });
  if (seasons.length > 0 && (currentSeason === null || seasons.indexOf(currentSeason) === -1)) {
    currentSeason = seasons[0];
  }

  var wrap = document.createElement('div');
  wrap.className = 'sv-wrap';
  wrap.innerHTML =
    '<div class="sv-backdrop">' +
      (backdropSrc ? '<img src="'+esc(backdropSrc)+'">' : '') +
      '<div class="sv-backdrop-fade"></div>' +
      '<button class="sv-back" onclick="closeShow()">← all</button>' +
    '</div>' +
    '<div class="sv-hero">' +
      '<div class="sv-hero-top">' +
        (posterSrc ? '<img class="sv-poster" src="'+esc(posterSrc)+'">' : '<div class="sv-poster sv-poster-ph">📺</div>') +
        '<div class="sv-info">' +
          '<h2 class="sv-title">'+esc(ep.title || '')+'</h2>' +
          '<div class="sv-meta">' +
            (ep.year?'<span>'+ep.year+'</span>':'') +
            (ep.year && seasons.length>1?'<span class="sv-meta-sep">·</span>':'') +
            (seasons.length>1?'<span>'+seasons.length+' seasons</span>':'') +
            (ep.rating?'<span class="sv-meta-sep">·</span><span class="sv-rating">'+ep.rating.toFixed(1)+'</span>':'') +
          '</div>' +
          (ep.genres && ep.genres.length ? '<div class="sv-genres">'+esc(ep.genres.join(' · '))+'</div>' : '') +
        '</div>' +
      '</div>' +
      (ep.overview ? '<p class="sv-overview">'+esc(ep.overview)+'</p>' : '') +
      '<button class="sv-play-btn" id="sv-play-btn"></button>' +
    '</div>';
  fileGrid.appendChild(wrap);

  if (seasons.length > 1) {
    var picker = document.createElement('div');
    picker.className = 'sv-season-tabs';
    seasons.forEach(function(sn) {
      var tab = document.createElement('div');
      tab.className = 'sv-season-tab' + (sn === currentSeason ? ' active' : '');
      tab.textContent = sn === 0 ? 'Specials' : 'Season ' + sn;
      tab.tabIndex = 0;
      tab.addEventListener('click', function(){ currentSeason = sn; renderGrid(currentFiles); });
      picker.appendChild(tab);
    });
    fileGrid.appendChild(picker);
    episodes = episodes.filter(function(f){ return (f.season||0) === currentSeason; });
  }

  var playBtn = document.getElementById('sv-play-btn');
  if (playBtn) {
    var first = episodes[0];
    playBtn.textContent = first && first.season && first.episode
      ? 'play s'+String(first.season).padStart(2,'0')+'e'+String(first.episode).padStart(2,'0')
      : 'play';
    if (first) playBtn.addEventListener('click', function(e){ openFile(e, first.key, false); });
  }

  episodes.forEach(function(f) {
    var row = document.createElement('div');
    row.className = 'sv-ep-row';
    row.tabIndex = 0;
    row.addEventListener('click', function(e){ openFile(e, f.key, false); });
    var stillSrc = f.episode_still || f.thumb_url;
    row.innerHTML =
      (stillSrc ? '<img class="sv-ep-still" src="'+esc(stillSrc)+'">' : '<div class="sv-ep-still sv-ep-no-still">▶</div>') +
      '<div class="sv-ep-info">' +
        (f.season && f.episode ? '<div class="sv-ep-code">S'+String(f.season).padStart(2,'0')+'E'+String(f.episode).padStart(2,'0')+'</div>' : '') +
        '<div class="sv-ep-title">'+esc(f.episode_title || f.name)+'</div>' +
        (f.episode_overview ? '<div class="sv-ep-desc">'+esc(f.episode_overview)+'</div>' : '') +
      '</div>' +
      (f.duration ? '<div class="sv-ep-dur">'+fmtDur(f.duration)+'</div>' : '');
    fileGrid.appendChild(row);
  });
}

function closeMovieDetail() {
  currentMovieDetail = null;
  history.back();
}

function renderMovieView(main, extras) {
  var posterSrc = main.poster_url || main.thumb_url;
  var backdropSrc = main.tmdb_backdrop || posterSrc;

  // Cinematic hero — same layout as series detail
  var wrap = document.createElement('div');
  wrap.className = 'sv-wrap';
  wrap.innerHTML =
    '<div class="sv-backdrop">' +
      (backdropSrc ? '<img src="'+esc(backdropSrc)+'">' : '') +
      '<div class="sv-backdrop-fade"></div>' +
      '<button class="sv-back" onclick="closeMovieDetail()">← all</button>' +
    '</div>' +
    '<div class="sv-hero">' +
      '<div class="sv-hero-top">' +
        (posterSrc ? '<img class="sv-poster" src="'+esc(posterSrc)+'">' : '<div class="sv-poster sv-poster-ph">🎬</div>') +
        '<div class="sv-info">' +
          '<h2 class="sv-title">'+esc(main.title || main.name)+'</h2>' +
          '<div class="sv-meta">' +
            (main.year ? '<span>'+main.year+'</span>' : '') +
            (main.runtime ? '<span class="sv-meta-sep">·</span><span>'+main.runtime+' min</span>' : '') +
            (main.rating ? '<span class="sv-meta-sep">·</span><span class="sv-rating">'+main.rating.toFixed(1)+'</span>' : '') +
          '</div>' +
          (main.genres && main.genres.length ? '<div class="sv-genres">'+esc(main.genres.join(' · '))+'</div>' : '') +
        '</div>' +
      '</div>' +
      (main.overview ? '<p class="sv-overview">'+esc(main.overview)+'</p>' : '') +
      '<button class="sv-play-btn" onclick="openFile(event,\''+esc(main.key)+'\',false,true)">play</button>' +
    '</div>';
  fileGrid.appendChild(wrap);

  // Metadata grid
  var cells = [];
  if (main.director) cells.push({label:'Director', value: esc(main.director)});
  if (main.cast && main.cast.length) cells.push({label:'Cast', value: esc(main.cast.join(', '))});
  if (main.runtime) cells.push({label:'Runtime', value: main.runtime+' min'});
  if (main.country) cells.push({label:'Country', value: esc(main.country)});
  if (main.language) cells.push({label:'Language', value: esc(main.language)});

  if (cells.length || main.collection) {
    var grid = document.createElement('div');
    grid.className = 'fm-meta-grid';
    cells.forEach(function(c) {
      grid.innerHTML += '<div class="fm-meta-cell"><div class="fm-meta-label">'+c.label+'</div><div class="fm-meta-value">'+c.value+'</div></div>';
    });
    if (main.collection) {
      grid.innerHTML += '<div class="fm-collection">Part of <span>'+esc(main.collection.name)+'</span></div>';
    }
    fileGrid.appendChild(grid);
  }

  if (!extras.length) return;
  appendSectionHeading(fileGrid, 'Extras');
  extras.forEach(function(f) {
    var row = document.createElement('div');
    row.className = 'ep-row';
    row.tabIndex = 0;
    row.addEventListener('click', function(e){
      if (e.target.tagName==='BUTTON') return;
      openFile(e, f.key, false, true);
    });
    row.innerHTML =
      '<div class="ep-still ep-no-still">🎬</div>' +
      '<div class="ep-info"><div class="ep-title">'+esc(f.title || f.name)+'</div></div>' +
      '<button class="ep-play-btn" onclick="openFile(event,\''+esc(f.key)+'\',false,true)">▶</button>';
    fileGrid.appendChild(row);
  });
}

function appendSectionHeading(target, text) {
  var h = document.createElement('div');
  h.className = 'section-heading';
  h.textContent = text;
  target.appendChild(h);
}

function renderGrid(files) {
  fileGrid.innerHTML = '';
  musicGrid.innerHTML = '';
  musicHeading.style.display = 'none';
  musicGrid.style.display = 'none';
  if (currentShow) {
    renderShowView(currentShow);
    return;
  }
  if (currentMovieDetail) {
    renderMovieView(currentMovieDetail.main, currentMovieDetail.extras);
    return;
  }
  var musicItems = files.filter(function(f){ return f.is_music_video; });
  var regularItems = files.filter(function(f){ return !f.is_music_video && !f.is_extra; });
  var grouped = groupBySeries(regularItems);
  if (!grouped.standalone.length && !grouped.shows.length && !musicItems.length) {
    fileGrid.innerHTML = '<div style="color:var(--muted);font-size:.8rem;grid-column:1/-1;padding:2rem 0">no files yet</div>';
    return;
  }
  var combined = grouped.shows.map(function(s){ return {_show: s}; }).concat(grouped.standalone);
  sortByTitle(combined).forEach(function(item){
    if (item._show) renderShowCard(item._show); else renderFileCard(item);
  });
  musicItems = sortByTitle(musicItems);
  if (musicItems.length) {
    musicHeading.style.display = 'block';
    musicGrid.style.display = 'grid';
    musicItems.forEach(function(f) { renderMusicCard(f); });
  }
}

function musicNoArtSvg(cls) {
  return '<svg class="'+cls+'" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.2" stroke-linecap="round" stroke-linejoin="round"><rect x="2" y="2" width="20" height="20" rx="2"/><line x1="7" y1="2" x2="7" y2="22"/><line x1="17" y1="2" x2="17" y2="22"/><line x1="2" y1="7" x2="7" y2="7"/><line x1="17" y1="7" x2="22" y2="7"/><line x1="2" y1="12" x2="7" y2="12"/><line x1="17" y1="12" x2="22" y2="12"/><line x1="2" y1="17" x2="7" y2="17"/><line x1="17" y1="17" x2="22" y2="17"/></svg>';
}

// showNoArt swaps a broken thumbnail <img> for the placeholder icon —
// extraction is lazy (first request triggers it), so a 404 here just means
// "haven't tried yet" or "ffmpeg couldn't pull a frame", not a real error.
function showNoArt(imgEl, cls) {
  imgEl.outerHTML = musicNoArtSvg(cls);
}

function renderMusicCard(f) {
  var card = document.createElement('div');
  card.className = 'card mv2-card';
  card.tabIndex = 0;
  // A still pulled from the video itself, not the song's album art — that
  // way it always reflects what's actually playing, and shows up even for
  // a music video iTunes never managed to identify.
  var thumbUrl = '/local/thumbnail?path=' + encodeURIComponent(f.key);
  var stillId = !f.title && !f.artist;
  var titleText = stillId ? '⟳ identifying…' : (f.title || f.name);
  var cover = '<div class="mv2-cover"><img src="'+thumbUrl+'" loading="lazy" onerror="showNoArt(this,\'mv2-noart\')"></div>';
  card.innerHTML = cover + '<div class="pc-info"><div class="pc-title">'+esc(titleText)+'</div><div class="pc-year">'+esc(stillId?'':(f.artist||''))+'</div></div>';
  card.addEventListener('click', function(e){ openFile(e, f.key, false); });
  musicGrid.appendChild(card);
}

function renderMusicRow(f) {
  var row = document.createElement('div');
  row.className = 'lr';
  row.tabIndex = 0;
  var thumbUrl = '/local/thumbnail?path=' + encodeURIComponent(f.key);
  var stillId = !f.title && !f.artist;
  row.innerHTML =
    '<div class="lr-poster mv-cover">' +
      '<img src="'+thumbUrl+'" loading="lazy" onerror="showNoArt(this,\'mv-noart\')">' +
    '</div>' +
    '<div class="lr-body">' +
      '<div class="lr-type">MUSIC VIDEO'+(f.year?' · '+f.year:'')+'</div>' +
      '<div class="lr-title">'+esc(stillId?'⟳ identifying…':(f.title||f.name))+'</div>' +
      (f.artist?'<div class="lr-genres">'+esc(f.artist)+(f.album?' · '+esc(f.album):'')+'</div>':'') +
    '</div>' +
    '<div class="lr-right">' +
      '<div class="lr-btns"><button class="lr-btn" onclick="openFile(event,\''+esc(f.key)+'\',false)">▶</button></div>' +
    '</div>';
  row.addEventListener('click', function(e){ if (e.target.tagName==='BUTTON') return; openFile(e, f.key, false); });
  fileList.appendChild(row);
}

function renderList(files) {
  fileList.innerHTML = '';
  if (!files.length) { fileList.innerHTML = '<div style="color:var(--muted);font-size:.8rem;padding:2rem 0">no files yet</div>'; return; }
  var musicItems = files.filter(function(f){ return f.is_music_video; });
  var regularFiles = files.filter(function(f){ return !f.is_music_video && !f.is_extra; });
  var grouped = groupBySeries(regularFiles);
  var items = [];
  grouped.shows.forEach(function(show) { items.push({_show: show}); });
  grouped.standalone.forEach(function(f) { items.push(f); });
  items = sortByTitle(items);

  items.forEach(function(item) {
    var row = document.createElement('div');
    row.className = 'lr';
    row.tabIndex = 0;

    if (item._show) {
      var s = item._show, ep = s.episodes[0];
      var img = ep.poster_url || ep.thumb_url;
      var genres = ep.genres && ep.genres.length ? ep.genres.slice(0,2).join(' · ') : '';
      row.innerHTML =
        '<div class="lr-poster">' +
          (img?'<img src="'+esc(img)+'" loading="lazy">':'<div class="lr-poster-ph">📺</div>') +
          '<div class="lr-ep-badge">'+s.episodes.length+' episodes</div>' +
        '</div>' +
        '<div class="lr-body">' +
          '<div class="lr-type">SERIES'+(ep.year?' · '+ep.year:'')+'</div>' +
          '<div class="lr-title">'+esc(ep.title||'')+'</div>' +
          (genres?'<div class="lr-genres">'+esc(genres)+'</div>':'') +
          (ep.overview?'<div class="lr-overview">'+esc(ep.overview)+'</div>':'') +
        '</div>' +
        '<div class="lr-right">' +
          (ep.rating?'<div class="lr-rating">★ '+ep.rating.toFixed(1)+'</div>':'') +
          '<div class="lr-btns"><button class="lr-btn">▶ Series</button></div>' +
        '</div>';
      row.addEventListener('click', function(e){ if (e.target.tagName==='BUTTON') return; openShow(s); });
      row.querySelector('.lr-btn').addEventListener('click', function(e){ e.stopPropagation(); openShow(s); });
      fileList.appendChild(row);
      return;
    }

    var f = item;
    var img = f.poster_url || f.thumb_url;
    var typeLabel = f.media_type==='movie' ? 'MOVIE' : f.media_type==='tv' ? 'TV EPISODE' : f.is_video ? 'VIDEO' : f.is_note ? 'NOTE' : 'FILE';
    var yearEp = [];
    if (f.year) yearEp.push(String(f.year));
    if (f.season&&f.episode) yearEp.push('S'+String(f.season).padStart(2,'0')+'E'+String(f.episode).padStart(2,'0'));
    var genres = f.genres&&f.genres.length ? f.genres.slice(0,2).join(' · ') : '';
    var displayTitle = f.title || (f.is_note ? (f.snippet||f.name) : f.name);
    var overview = f.overview || f.episode_overview || '';
    var stillIdLr = f.is_local && !f.title && !f.tmdb_id;
    row.innerHTML =
      '<div class="lr-poster">' +
        (img?'<img src="'+esc(img)+'" loading="lazy">':'<div class="lr-poster-ph">'+(stillIdLr?'⟳':fileIcon(f))+'</div>') +
        (f.duration?'<div class="lr-ep-badge">'+fmtDur(f.duration)+'</div>':'') +
        (f.is_local && !f.playable?'<div class="lr-ep-badge">open in app</div>':'') +
      '</div>' +
      '<div class="lr-body">' +
        '<div class="lr-type">'+typeLabel+(yearEp.length?' · '+yearEp.join(' · '):'')+'</div>' +
        '<div class="lr-title">'+esc(displayTitle)+'</div>' +
        (genres?'<div class="lr-genres">'+esc(genres)+'</div>':'') +
        (overview?'<div class="lr-overview">'+esc(overview)+'</div>':'') +
      '</div>' +
      '<div class="lr-right">' +
        (f.rating?'<div class="lr-rating">★ '+f.rating.toFixed(1)+'</div>':'') +
        '<div class="lr-btns">' +
          (f.proc_state!=='processing'?'<button class="lr-btn" onclick="openFile(event,\''+esc(f.key)+'\','+f.is_note+')">▶</button>':'<button class="lr-btn" disabled style="opacity:.4">▶</button>') +
          (f.is_local ? '' :
            '<button class="lr-btn" onclick="dlFile(event,\''+esc(f.key)+'\')">↓</button>' +
            '<button class="lr-btn" onclick="deleteFile(event,\''+esc(f.key)+'\')">✕</button>') +
        '</div>' +
      '</div>';
    row.addEventListener('click', function(e){
      if (e.target.tagName==='BUTTON') return;
      if (f.proc_state==='processing') return;
      openFile(e, f.key, f.is_note);
    });
    fileList.appendChild(row);
  });

  if (musicItems.length) {
    appendSectionHeading(fileList, 'Music Videos');
    sortByTitle(musicItems).forEach(function(f) { renderMusicRow(f); });
  }
}

// ── Modal ──────────────────────────────────────────────────────────────────
var currentMovieDetail = null;

function openFile(e, key, isNote, skipDetailCheck) {
  if (e) e.stopPropagation();
  if (!isNote && !skipDetailCheck) {
    var fEarly = currentFiles.find(function(x){ return x.key === key; });
    if (fEarly && fEarly.media_type === 'movie') {
      var extras = currentFiles.filter(function(x){ return x.is_extra && x.main_key === key; });
      currentMovieDetail = {main: fEarly, extras: extras};
      history.pushState({type:'movie', key: key}, '', '#movie=' + encodeURIComponent(key));
      window.scrollTo(0, 0);
      if (viewMode === 'list') { renderList(currentFiles); } else { renderGrid(currentFiles); }
      return;
    }
  }
  currentKey = key;
  currentIsNote = isNote;
  modalVideo.style.display = 'none';
  modalVideo.innerHTML = '';
  modalNote.style.display = 'none';
  modalCopy.style.display = 'none';
  modalCC.style.display = 'none';
  modalCC.disabled = false;
  modalCC.textContent = 'CC get subtitles';
  modalMeta.style.display = 'none';
  modalMeta.disabled = false;
  modalMeta.textContent = '↻ redo metadata';
  modalQuality.style.display = 'none';
  modalDl.style.display = 'inline-block';
  modalDel.style.display = 'inline-block';
  var fMod = currentFiles.find(function(x){ return x.key === key; });
  modalFname.textContent = (fMod && fMod.title)
    ? fMod.title + (fMod.year ? ' ('+fMod.year+')' : '')
    : key.split('/').pop();
  modal.classList.add('open');

  if (isNote) {
    modalNote.style.display = 'block';
    modalNote.textContent = 'loading…';
    modalCopy.style.display = 'inline-block';
    fetch('/file?key='+encodeURIComponent(key), {credentials:'include'})
      .then(function(r){ return r.text(); })
      .then(function(t){ modalNote.textContent = t; });
  } else {
    var f = currentFiles.find(function(x){ return x.key === key; });
    var ext = key.split('.').pop().toLowerCase();
    var vidExts = ['mp4','webm','mov','m4v'];
    var imgExts = ['jpg','jpeg','png','gif','webp','svg'];
    if (f && f.is_local) {
      // The "playable" extension hint is just that — a hint. Some MKVs (e.g.
      // H.264 inside Matroska) actually play fine in desktop Chrome/Edge
      // despite MKV not being "officially" supported, so always try inline
      // playback first and only hand off to a device video app if the
      // browser actually can't decode it (the 'error' event fires).
      //
      // Safari is the exception: WebKit has no Matroska demuxer at all, and
      // unlike Chrome/Edge it doesn't raise a catchable 'error' for it —
      // the video element just sits there black/frozen forever. So for
      // Safari + mkv specifically, skip the inline attempt entirely and
      // hand off straight to the device's video app.
      if (ext === 'mkv' && isSafariBrowser()) {
        modal.classList.remove('open');
        handOffLocalFile(f);
        return;
      }
      modalVideo.style.display = 'block';
      modalDl.style.display = 'none';
      modalDel.style.display = 'none';
      if (hlsInstance) { hlsInstance.destroy(); hlsInstance = null; }
      modalVideo.onerror = function() {
        // Minimizing/defocusing a video that's already playing fine can
        // interrupt its network stream in some browsers, firing this same
        // 'error' event well after playback started — that's a transient
        // interruption, not a codec failure, and must never trigger a
        // handoff (it was reopening a new tab every time the window lost
        // focus). Only a file that never got anywhere — still at 0:00 — is
        // actually undecodable.
        if (modalVideo.currentTime > 0) return;
        modalVideo.onerror = null;
        modal.classList.remove('open');
        handOffLocalFile(f);
      };
      // Always offer a subtitle track — sidecar .srt/.vtt or an embedded
      // text subtitle stream both get detected and converted server-side
      // on first request; if the file genuinely has neither, this 404s and
      // the browser just shows no subtitle option, no harm done.
      var ltrack = document.createElement('track');
      ltrack.kind = 'subtitles'; ltrack.src = '/local/subtitle?path=' + encodeURIComponent(f.key);
      ltrack.srclang = 'en'; ltrack.label = 'English'; ltrack.default = true;
      modalVideo.appendChild(ltrack);
      modalVideo.src = '/local/file?path=' + encodeURIComponent(f.key);
      modalVideo.play().catch(function(){});
    } else if (f && f.hls_base) {
      modalVideo.style.display = 'block';
      if (f.subtitle_url) {
        var track = document.createElement('track');
        track.kind = 'subtitles'; track.src = f.subtitle_url;
        track.srclang = 'en'; track.label = 'English'; track.default = true;
        modalVideo.appendChild(track);
      } else if (f.tmdb_id) {
        modalCC.style.display = 'inline-block';
        modalCC.onclick = function(){ retrySubtitle(key); };
      }
      if (!f.tmdb_id) {
        modalMeta.style.display = 'inline-block';
        modalMeta.onclick = function(){ retryMetadata(key); };
      }
      playHLS(f.hls_base + 'master.m3u8', modalVideo);
    } else if (f && f.is_video) {
      // Video exists but HLS not ready yet — could be unprocessed or mid-convert
      if (vidExts.includes(ext)) {
        // Natively playable format (incl. direct-play remux), stream from CDN.
        modalVideo.style.display = 'block';
        if (f.subtitle_url) {
          var dtrack = document.createElement('track');
          dtrack.kind = 'subtitles'; dtrack.src = f.subtitle_url;
          dtrack.srclang = 'en'; dtrack.label = 'English'; dtrack.default = true;
          modalVideo.appendChild(dtrack);
        } else if (f.tmdb_id) {
          modalCC.style.display = 'inline-block';
          modalCC.onclick = function(){ retrySubtitle(key); };
        }
        if (!f.tmdb_id) {
          modalMeta.style.display = 'inline-block';
          modalMeta.onclick = function(){ retryMetadata(key); };
        }
        modalVideo.src = '/file?key='+encodeURIComponent(key);
        modalVideo.play();
        requestVideoFullscreen(modalVideo);
      } else {
        // Format Safari/browsers can't play (mkv, avi, wmv etc)
        modalNote.style.display = 'block';
        if (f.proc_state === 'error') {
          modalNote.innerHTML = '<div style="text-align:center;padding:2rem;color:var(--muted)">' +
            '<div style="font-size:2rem;margin-bottom:1rem">✕</div>' +
            '<div style="margin-bottom:.5rem">Processing failed</div>' +
            '<div style="font-size:.75rem;margin-bottom:1rem">The video could not be converted. Check service logs for details.</div>' +
            '<button onclick="retryProc(\''+esc(key)+'\')" style="font-size:.75rem;padding:.25rem .75rem">Retry</button>' +
            '</div>';
        } else {
          modalNote.innerHTML = '<div style="text-align:center;padding:2rem;color:var(--muted)" id="conv-state">' +
            '<div style="font-size:2rem;margin-bottom:1rem">⟳</div>' +
            '<div style="margin-bottom:.5rem">Converting for playback…</div>' +
            '<div style="font-size:.75rem">This may take a few minutes.</div>' +
            '</div>';
          var convKey = key;
          var convStem = convKey.replace(/\.[^.]+$/, '');
          fetch('/confirm', {method:'POST', credentials:'include',
            headers:{'Content-Type':'application/json'},
            body: JSON.stringify({key: convKey})
          });
          var convPoll = setInterval(function() {
            fetch('/files', {credentials:'include'})
              .then(function(r){ return r.json(); })
              .then(function(files) {
                currentFiles = files || [];
                var done = currentFiles.find(function(x){
                  return (x.key === convKey || x.key === convStem+'.mp4') && (x.hls_base || x.direct_play);
                });
                if (done) {
                  clearInterval(convPoll);
                  if (viewMode==='list') { renderList(currentFiles); } else { renderGrid(currentFiles); }
                  modal.classList.remove('open');
                  setTimeout(function(){ openFile(null, done.key, false); }, 150);
                  return;
                }
                var errFile = currentFiles.find(function(x){ return x.key === convKey && x.proc_state === 'error'; });
                if (errFile) {
                  clearInterval(convPoll);
                  var el = document.getElementById('conv-state');
                  if (el) el.innerHTML = '<div style="font-size:2rem;margin-bottom:1rem">✕</div><div style="margin-bottom:.5rem">Processing failed</div><button onclick="retryProc(\''+esc(convKey)+'\')" style="font-size:.75rem;padding:.25rem .75rem">Retry</button>';
                }
              });
          }, 5000);
        }
      }
    } else if (imgExts.includes(ext)) {
      modalNote.style.display = 'block';
      modalNote.innerHTML = '<img src="/file?key='+encodeURIComponent(key)+'" style="max-width:100%;border-radius:4px">';
    } else {
      window.open('/file?key='+encodeURIComponent(key), '_blank');
      modal.classList.remove('open');
    }
  }
}

function dlFile(e, key) {
  if (e) e.stopPropagation();
  var a = document.createElement('a');
  a.href = '/file?key='+encodeURIComponent(key)+'&dl=1';
  a.download = key.split('/').pop();
  document.body.appendChild(a);
  a.click();
  a.remove();
}

function deleteFile(e, key) {
  if (e) e.stopPropagation();
  if (!confirm('Delete '+key.split('/').pop()+'?')) return;
  fetch('/delete?key='+encodeURIComponent(key), {method:'POST', credentials:'include'})
    .then(function(){ loadFiles(); });
}

document.getElementById('modal-close').addEventListener('click', closeModal);
modal.addEventListener('click', function(e){ if (e.target === modal) closeModal(); });
document.addEventListener('keydown', function(e){ if (e.key === 'Escape') closeModal(); });

// ── D-pad / arrow-key navigation (TV remotes, keyboard) ─────────────────────
function navTiles() {
  var container = viewMode === 'list' ? fileList : fileGrid;
  return Array.prototype.slice.call(container.querySelectorAll('[tabindex="0"]'));
}
function moveNavFocus(dir) {
  var tiles = navTiles();
  if (!tiles.length) return;
  var idx = tiles.indexOf(document.activeElement);
  if (idx === -1) { tiles[0].focus(); return; }
  var cur = tiles[idx].getBoundingClientRect();
  var curCx = cur.left + cur.width/2, curCy = cur.top + cur.height/2;
  var best = null, bestDist = Infinity;
  tiles.forEach(function(t, i){
    if (i === idx) return;
    var r = t.getBoundingClientRect();
    var dx = (r.left + r.width/2) - curCx;
    var dy = (r.top + r.height/2) - curCy;
    var inDir =
      dir === 'left'  ? dx < -4 :
      dir === 'right' ? dx > 4  :
      dir === 'up'    ? dy < -4 :
                         dy > 4;
    if (!inDir) return;
    var dist = (dir === 'left' || dir === 'right')
      ? Math.abs(dx) + Math.abs(dy) * 3
      : Math.abs(dy) + Math.abs(dx) * 3;
    if (dist < bestDist) { bestDist = dist; best = t; }
  });
  if (best) { best.focus(); best.scrollIntoView({block: 'nearest', behavior: 'smooth'}); }
}
document.addEventListener('keydown', function(e){
  if (modal.classList.contains('open') || searchOverlay.classList.contains('open')) return;
  var dirs = {ArrowLeft: 'left', ArrowRight: 'right', ArrowUp: 'up', ArrowDown: 'down'};
  if (dirs[e.key]) {
    if (document.activeElement && document.activeElement.tagName === 'INPUT') return;
    e.preventDefault();
    moveNavFocus(dirs[e.key]);
  } else if (e.key === 'Enter' && document.activeElement && document.activeElement.getAttribute('tabindex') === '0') {
    e.preventDefault();
    document.activeElement.click();
  }
});
function requestVideoFullscreen(el) {
  try {
    var req = el.requestFullscreen || el.webkitRequestFullscreen || el.webkitEnterFullscreen;
    if (!req) return;
    var p = req.call(el);
    if (p && p.catch) p.catch(function(){});
  } catch (e) {}
}

function populateQualityMenu() {
  if (!hlsInstance || !hlsInstance.levels || hlsInstance.levels.length <= 1) {
    modalQuality.style.display = 'none';
    return;
  }
  modalQuality.innerHTML = '<option value="-1">Auto</option>' +
    hlsInstance.levels.map(function(lvl, i){
      var label = lvl.height ? lvl.height+'p' : Math.round(lvl.bitrate/1000)+'kbps';
      return '<option value="'+i+'">'+label+'</option>';
    }).join('');
  modalQuality.value = '-1';
  modalQuality.style.display = 'inline-block';
}
modalQuality.addEventListener('change', function(){
  if (hlsInstance) hlsInstance.currentLevel = parseInt(modalQuality.value, 10);
});

function playHLS(masterURL, videoEl) {
  if (hlsInstance) { hlsInstance.destroy(); hlsInstance = null; }
  modalQuality.style.display = 'none';
  if (typeof Hls !== 'undefined' && Hls.isSupported()) {
    hlsInstance = new Hls({maxBufferLength: 30, maxMaxBufferLength: 120, enableWorker: true});
    hlsInstance.loadSource(masterURL);
    hlsInstance.attachMedia(videoEl);
    hlsInstance.on(Hls.Events.MANIFEST_PARSED, function(){
      videoEl.play();
      populateQualityMenu();
      requestVideoFullscreen(videoEl);
    });
  } else if (videoEl.canPlayType('application/vnd.apple.mpegurl')) {
    videoEl.src = masterURL;
    videoEl.play();
    requestVideoFullscreen(videoEl);
  }
}

function retryProc(key) {
  closeModal();
  fetch('/confirm', {method:'POST', credentials:'include',
    headers:{'Content-Type':'application/json'},
    body: JSON.stringify({key: key})
  }).then(function(){ loadFiles(); });
}

function retrySubtitle(key) {
  modalCC.disabled = true;
  modalCC.textContent = 'CC fetching…';
  fetch('/fetch-subtitle', {method:'POST', credentials:'include',
    headers:{'Content-Type':'application/json'},
    body: JSON.stringify({key: key})
  }).then(function(r){
    if (!r.ok) return r.text().then(function(t){ throw new Error(t); });
    return fetch('/files', {credentials:'include'}).then(function(r2){ return r2.json(); });
  }).then(function(files){
    currentFiles = files || [];
    var updated = currentFiles.find(function(x){ return x.key === key; });
    if (updated && updated.subtitle_url && currentKey === key) {
      var track = document.createElement('track');
      track.kind = 'subtitles'; track.src = updated.subtitle_url;
      track.srclang = 'en'; track.label = 'English'; track.default = true;
      modalVideo.appendChild(track);
      modalCC.style.display = 'none';
    } else {
      modalCC.disabled = false;
      modalCC.textContent = 'CC retry failed';
    }
  }).catch(function(e){
    modalCC.disabled = false;
    modalCC.textContent = 'CC retry failed';
    alert('Subtitle fetch failed: ' + (e.message || 'unknown error'));
  });
}

function retryMetadata(key) {
  modalMeta.disabled = true;
  modalMeta.textContent = '↻ refreshing…';
  fetch('/refresh-metadata', {method:'POST', credentials:'include',
    headers:{'Content-Type':'application/json'},
    body: JSON.stringify({key: key})
  }).then(function(r){
    if (!r.ok) return r.text().then(function(t){ throw new Error(t); });
    return fetch('/files', {credentials:'include'}).then(function(r2){ return r2.json(); });
  }).then(function(files){
    currentFiles = files || [];
    if (viewMode === 'list') { renderList(currentFiles); } else { renderGrid(currentFiles); }
    var updated = currentFiles.find(function(x){ return x.key === key; });
    if (updated && updated.tmdb_id && currentKey === key) {
      modalMeta.style.display = 'none';
      modalFname.textContent = updated.title ? updated.title + (updated.year ? ' ('+updated.year+')' : '') : key.split('/').pop();
      if (!updated.subtitle_url) {
        modalCC.style.display = 'inline-block';
        modalCC.onclick = function(){ retrySubtitle(key); };
      }
    } else {
      modalMeta.disabled = false;
      modalMeta.textContent = '↻ retry failed';
    }
  }).catch(function(e){
    modalMeta.disabled = false;
    modalMeta.textContent = '↻ retry failed';
    alert('Metadata refresh failed: ' + (e.message || 'unknown error'));
  });
}

function closeModal() {
  modal.classList.remove('open');
  // Clearing .src below is itself what fires a 'error' event on a video
  // that was loaded/playing (and resets currentTime to 0 first, ahead of
  // any handler reading it) — with the local-file onerror handler still
  // attached, closing a perfectly-working video was misread as a decode
  // failure and handed off to a video app, popping a new tab every time.
  // Detach it before touching .src so a closed video never re-triggers it.
  modalVideo.onerror = null;
  modalVideo.pause();
  modalVideo.src = '';
  while (modalVideo.firstChild) { modalVideo.removeChild(modalVideo.firstChild); }
  if (hlsInstance) { hlsInstance.destroy(); hlsInstance = null; }
}

modalDl.addEventListener('click', function(){ dlFile(null, currentKey); });
modalDel.addEventListener('click', function(){
  if (!confirm('Delete '+currentKey.split('/').pop()+'?')) return;
  fetch('/delete?key='+encodeURIComponent(currentKey), {method:'POST', credentials:'include'})
    .then(function(){ closeModal(); loadFiles(); });
});
modalCopy.addEventListener('click', function(){
  navigator.clipboard.writeText(modalNote.textContent);
});

function esc(s) {
  return String(s).replace(/&/g,'&amp;').replace(/</g,'&lt;').replace(/>/g,'&gt;').replace(/"/g,'&quot;');
}

// ── Init ───────────────────────────────────────────────────────────────────
history.replaceState({type:'home'}, '', location.pathname + location.search);

window.addEventListener('popstate', function(e) {
  var st = e.state;
  if (!st || st.type === 'home') {
    currentShow = null;
    currentMovieDetail = null;
    currentSeason = null;
    window.scrollTo(0, 0);
    if (viewMode === 'list') { renderList(currentFiles); } else { renderGrid(currentFiles); }
  } else if (st.type === 'show') {
    var regularItems = currentFiles.filter(function(f){ return !f.is_music_video && !f.is_extra; });
    var grouped = groupBySeries(regularItems);
    var found = grouped.shows.find(function(s){ return s.id === st.id; });
    currentShow = found || null;
    currentMovieDetail = null;
    currentSeason = null;
    window.scrollTo(0, 0);
    renderGrid(currentFiles);
  } else if (st.type === 'movie') {
    var f = currentFiles.find(function(x){ return x.key === st.key; });
    currentMovieDetail = f ? {main: f, extras: currentFiles.filter(function(x){ return x.is_extra && x.main_key === st.key; })} : null;
    currentShow = null;
    window.scrollTo(0, 0);
    if (viewMode === 'list') { renderList(currentFiles); } else { renderGrid(currentFiles); }
  }
});

loadFiles();
setInterval(loadFiles, 10000);
setSource(libSource);

// ── Search ─────────────────────────────────────────────────────────────────
var searchOverlay   = document.getElementById('search-overlay');
var searchInput     = document.getElementById('search-input');
var searchResults   = document.getElementById('search-results');
var searchTvOpts    = document.getElementById('search-tv-opts');
var searchSelected  = document.getElementById('search-selected');
var seasonSel       = document.getElementById('search-season-sel');
var episodeSel      = document.getElementById('search-episode-sel');
var modeBtns        = document.querySelectorAll('.st-mode-btn');
var currentTMDBHit  = null;
var searchTimer     = null;
var searchMode      = 'episode';

document.getElementById('search-open-btn').addEventListener('click', function(){
  searchOverlay.classList.add('open');
  searchInput.focus();
});
document.getElementById('search-close-btn').addEventListener('click', closeSearch);
searchOverlay.addEventListener('click', function(e){ if(e.target===searchOverlay) closeSearch(); });
document.addEventListener('keydown', function(e){ if(e.key==='Escape') closeSearch(); });

function closeSearch() {
  searchOverlay.classList.remove('open');
  searchInput.value = '';
  searchResults.innerHTML = '';
  searchTvOpts.style.display = 'none';
  currentTMDBHit = null;
}

document.getElementById('search-back-btn').addEventListener('click', function(){
  searchTvOpts.style.display = 'none';
  currentTMDBHit = null;
  if (searchInput.value) doTMDBSearch(searchInput.value);
});

modeBtns.forEach(function(btn){
  btn.addEventListener('click', function(){ setSearchMode(btn.dataset.mode); });
});

function setSearchMode(mode) {
  searchMode = mode;
  modeBtns.forEach(function(b){ b.classList.toggle('active', b.dataset.mode === mode); });
  if (mode === 'episode') {
    seasonSel.style.display = '';
    episodeSel.style.display = '';
    episodeSel.disabled = !seasonSel.value;
  } else if (mode === 'season') {
    seasonSel.style.display = '';
    episodeSel.style.display = 'none';
  } else {
    seasonSel.style.display = 'none';
    episodeSel.style.display = 'none';
  }
}

seasonSel.addEventListener('change', function(){
  episodeSel.innerHTML = '<option value="">Episode…</option>';
  var s = parseInt(seasonSel.value)||0;
  if (!s || !currentTMDBHit || searchMode !== 'episode') { episodeSel.disabled = true; return; }
  episodeSel.disabled = true;
  fetch('/search/tmdb/episodes?id='+currentTMDBHit.id+'&season='+s, {credentials:'include'})
    .then(function(r){ return r.ok ? r.json() : []; })
    .then(function(eps){
      (eps||[]).forEach(function(ep){
        var opt = document.createElement('option');
        opt.value = ep.episode_number;
        opt.textContent = 'E'+String(ep.episode_number).padStart(2,'0')+(ep.name?' · '+ep.name:'');
        episodeSel.appendChild(opt);
      });
      episodeSel.disabled = false;
    });
});

document.getElementById('search-torrents-btn').addEventListener('click', function(){
  if (!currentTMDBHit) return;
  var opts = {};
  if (currentTMDBHit.media_type === 'tv') {
    if (searchMode === 'episode') {
      var s = parseInt(seasonSel.value)||0, e = parseInt(episodeSel.value)||0;
      if (!s || !e) { alert('Pick a season and episode, or switch to Season Pack / Complete Series.'); return; }
      opts.season = s; opts.episode = e;
    } else if (searchMode === 'season') {
      var s2 = parseInt(seasonSel.value)||0;
      if (!s2) { alert('Pick a season.'); return; }
      opts.season = s2;
    }
  }
  doTorrentSearch(currentTMDBHit, opts);
});

searchInput.addEventListener('input', function(){
  clearTimeout(searchTimer);
  var q = this.value.trim();
  if (!q) { searchResults.innerHTML = ''; return; }
  searchTimer = setTimeout(function(){ doTMDBSearch(q); }, 400);
});

function doTMDBSearch(q) {
  searchTvOpts.style.display = 'none';
  searchResults.innerHTML = '<div class="sr-msg">searching…</div>';
  fetch('/search/tmdb?q='+encodeURIComponent(q), {credentials:'include'})
    .then(function(r){ return r.ok ? r.json() : Promise.reject(r.status); })
    .then(function(hits){
      searchResults.innerHTML = '';
      if (!hits || !hits.length) {
        searchResults.innerHTML = '<div class="sr-msg">no results</div>';
        return;
      }
      hits.forEach(function(h) {
        var row = document.createElement('div');
        row.className = 'sr-tmdb';
        var badge = h.media_type === 'tv' ? 'TV' : 'Movie';
        row.innerHTML =
          (h.poster_url ? '<img src="'+esc(h.poster_url)+'" loading="lazy">' : '<div style="width:50px;height:70px;background:var(--border);border-radius:3px;flex-shrink:0"></div>') +
          '<div class="sr-tmdb-info">' +
            '<div class="sr-tmdb-title">'+esc(h.title)+(h.year?' ('+h.year+')':'')+'</div>' +
            '<div class="sr-tmdb-meta">'+badge+(h.rating?' · ★ '+h.rating.toFixed(1):'')+'</div>' +
            (h.overview ? '<div class="sr-tmdb-overview">'+esc(h.overview)+'</div>' : '') +
          '</div>';
        row.addEventListener('click', function(){ selectTMDBHit(h); });
        searchResults.appendChild(row);
      });
    })
    .catch(function(){ searchResults.innerHTML = '<div class="sr-msg">search failed</div>'; });
}

function selectTMDBHit(h) {
  currentTMDBHit = h;
  if (h.media_type !== 'tv') {
    doTorrentSearch(h, {});
    return;
  }
  searchTvOpts.style.display = 'flex';
  searchResults.innerHTML = '<div class="sr-msg">choose what you want, then hit "Search Torrents"</div>';
  searchSelected.innerHTML =
    (h.poster_url ? '<img src="'+esc(h.poster_url)+'">' : '') +
    '<div id="search-selected-info">' +
      '<div id="search-selected-title">'+esc(h.title)+(h.year?' ('+h.year+')':'')+'</div>' +
      '<div id="search-selected-meta">TV Series'+(h.rating?' · ★ '+h.rating.toFixed(1):'')+'</div>' +
    '</div>';
  seasonSel.innerHTML = '<option value="">Season…</option>';
  episodeSel.innerHTML = '<option value="">Episode…</option>';
  seasonSel.disabled = true; episodeSel.disabled = true;
  setSearchMode('episode');
  fetch('/search/tmdb/seasons?id='+h.id, {credentials:'include'})
    .then(function(r){ return r.ok ? r.json() : []; })
    .then(function(seasons){
      (seasons||[]).forEach(function(s){
        var opt = document.createElement('option');
        opt.value = s.season_number;
        opt.textContent = (s.name || ('Season '+s.season_number)) + ' ('+s.episode_count+' ep)';
        seasonSel.appendChild(opt);
      });
      seasonSel.disabled = false;
    });
}

function doTorrentSearch(hit, opts) {
  opts = opts || {};
  searchResults.innerHTML = '<div class="sr-msg">searching torrents…</div>';
  var params = '?q='+encodeURIComponent(hit.title);
  if (hit.year) params += '&year='+hit.year;
  if (opts.season) params += '&season='+opts.season;
  if (opts.episode) params += '&episode='+opts.episode;
  fetch('/search/torrents'+params, {credentials:'include'})
    .then(function(r){ return r.ok ? r.json() : r.text().then(function(t){ throw new Error(t); }); })
    .then(function(results){
      searchResults.innerHTML = '';
      if (!results || !results.length) {
        searchResults.innerHTML = '<div class="sr-msg">no torrent results — check Jackett is running and has indexers configured</div>';
        return;
      }
      results.forEach(function(t) {
        var row = document.createElement('div');
        row.className = 'sr-torrent';
        var qual = t.resolution || '?';
        var isHD = qual === '1080p' || qual === '2160p';
        row.innerHTML =
          '<span class="sr-qual'+(isHD?' hd':'')+'">'+qual+'</span>' +
          '<span class="sr-torrent-title" title="'+esc(t.title)+'">'+esc(t.title)+'</span>' +
          '<span class="sr-seeds">▲ '+t.seeders+'</span>' +
          '<span class="sr-size">'+fmtSize(t.size)+'</span>' +
          '<button class="sr-dl-btn" onclick="downloadTorrent(event,this,\''+esc(t.magnet_uri)+'\',\''+esc(t.torrent_url||'')+'\')">↓ get</button>';
        searchResults.appendChild(row);
      });
    })
    .catch(function(e){ searchResults.innerHTML = '<div class="sr-msg">'+esc(e.message||'torrent search failed')+'</div>'; });
}

function downloadTorrent(e, btn, magnetURI, torrentURL) {
  e.stopPropagation();
  btn.disabled = true; btn.textContent = '…';
  fetch('/download', {
    method:'POST', credentials:'include',
    headers:{'Content-Type':'application/json'},
    body: JSON.stringify({magnet_uri: magnetURI, torrent_url: torrentURL})
  }).then(function(r){
    if (r.ok) { btn.textContent = '✓ sent'; }
    else { r.text().then(function(t){ btn.disabled=false; btn.textContent='↓ get'; alert('Error: '+t); }); }
  }).catch(function(){ btn.disabled=false; btn.textContent='↓ get'; });
}
</script>
</body>
</html>`))

// ─── Favicon ──────────────────────────────────────────────────────────────────

func handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Write([]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 200"><rect width="200" height="200" rx="36" fill="#110F0D"/><polygon points="75,55 155,100 75,145" fill="#9A7D4A"/></svg>`))
}

func handleFaviconPNG(w http.ResponseWriter, r *http.Request) {
	img := image.NewRGBA(image.Rect(0, 0, 32, 32))
	bg := color.RGBA{17, 15, 13, 255}
	gold := color.RGBA{154, 125, 74, 255}
	for y := 0; y < 32; y++ {
		for x := 0; x < 32; x++ {
			img.Set(x, y, bg)
		}
	}
	for y := 8; y < 24; y++ {
		for x := 10; x < 22; x++ {
			if x > 10+(y-8)/2 && x < 22-(y-8)/2 {
				img.Set(x, y, gold)
			}
		}
	}
	w.Header().Set("Content-Type", "image/png")
	png.Encode(w, img)
}

// ─── Mux builder ──────────────────────────────────────────────────────────────

func buildServer(c *serverCtx) *http.Server {
	mux := http.NewServeMux()
	mux.HandleFunc("/favicon.svg", handleFavicon)
	mux.HandleFunc("/favicon.png", handleFaviconPNG)
	mux.HandleFunc("/favicon.ico", handleFaviconPNG)
	mux.HandleFunc("/login", c.handleLogin)
	// Public (viewing): library listing, playback/download, status, QR.
	mux.HandleFunc("/files", c.handleFiles)
	mux.HandleFunc("/file", c.handleFile)
	mux.HandleFunc("/proc-status", c.handleProcStatus)
	mux.HandleFunc("/qr.png", c.handleQR)
	// Local-only (management): uploads, deletes, torrent search/download,
	// reprocess, and the local library — gated to LAN/localhost.
	mux.HandleFunc("/toggle-login", c.localOnly(c.handleToggleLogin))
	mux.HandleFunc("/upload", c.localOnly(c.handleUpload))
	mux.HandleFunc("/confirm", c.localOnly(c.handleConfirm))
	mux.HandleFunc("/note", c.localOnly(c.handleNote))
	mux.HandleFunc("/delete", c.localOnly(c.handleDelete))
	mux.HandleFunc("/fetch-subtitle", c.localOnly(c.handleFetchSubtitle))
	mux.HandleFunc("/refresh-metadata", c.localOnly(c.handleRefreshMetadata))
	mux.HandleFunc("/search/tmdb", c.localOnly(c.handleSearchTMDB))
	mux.HandleFunc("/search/tmdb/seasons", c.localOnly(c.handleTMDBSeasons))
	mux.HandleFunc("/search/tmdb/episodes", c.localOnly(c.handleTMDBEpisodes))
	mux.HandleFunc("/search/torrents", c.localOnly(c.handleSearchTorrents))
	mux.HandleFunc("/download", c.localOnly(c.handleDownload))
	mux.HandleFunc("/local/files", c.localOnly(c.handleLocalFiles))
	mux.HandleFunc("/local/file", c.localOnly(c.handleLocalFile))
	mux.HandleFunc("/local/file-token", c.localOnly(c.handleLocalFileToken))
	mux.HandleFunc("/local/subtitle", c.localOnly(c.handleLocalSubtitle))
	mux.HandleFunc("/local/thumbnail", c.localOnly(c.handleLocalThumbnail))
	mux.HandleFunc("/", c.handleIndex)
	return &http.Server{
		Addr:              fmt.Sprintf(":%d", c.port),
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

// ─── Windows Service ──────────────────────────────────────────────────────────

type winSvc struct{}

func (w *winSvc) Execute(args []string, req <-chan svc.ChangeRequest, status chan<- svc.Status) (bool, uint32) {
	status <- svc.Status{State: svc.StartPending}
	srv := startServer()
	status <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for c := range req {
		if c.Cmd == svc.Stop || c.Cmd == svc.Shutdown {
			break
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	srv.Shutdown(ctx)
	logger.Println("service stopped")
	status <- svc.Status{State: svc.StopPending}
	return false, 0
}

func startServer() *http.Server {
	secret, _ := hex.DecodeString(cfg.Secret)
	c := &serverCtx{
		passHash:    cfg.PasswordHash,
		secret:      secret,
		port:        cfg.Port,
		r2Bucket:    cfg.R2Bucket,
		r2PubURL:    cfg.R2PublicURL,
		rateMap:     make(map[string]*rateEntry),
		procMap:     make(map[string]procState),
		llmProvider:  cfg.LLMProvider,
		llmModel:     cfg.LLMModel,
		llmBaseURL:   cfg.LLMBaseURL,
		llmAPIKey:    cfg.LLMAPIKey,
		tmdbKey:      cfg.TMDBKey,
		openSubsKey:  cfg.OpenSubsKey,
		openSubsUser: cfg.OpenSubsUser,
		openSubsPass: cfg.OpenSubsPass,
		jackettURL:   cfg.JackettURL,
		jackettKey:   cfg.JackettAPIKey,
		qbitURL:      cfg.QBitURL,
		qbitUser:     cfg.QBitUser,
		qbitPass:     cfg.QBitPass,
		watchFolder:  cfg.WatchFolder,
		localFolders: cfg.LocalLibraryFolders,
	}
	c.loginDisabled.Store(cfg.LoginDisabled)
	c.r2 = newR2Client(cfg.R2AccountID, cfg.R2AccessKey, cfg.R2SecretKey)
	nvencAvailable = probeNvenc()
	logger.Printf("hardware H.264 encode (nvenc) available: %v", nvencAvailable)
	go c.startWatchFolder()
	go c.startLocalEnrichment()
	srv := buildServer(c)
	go func() {
		logger.Printf("stream server starting on :%d", cfg.Port)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			logger.Fatalf("server error: %v", err)
		}
	}()
	// WebDAV server on its own port so Android TV file managers can connect
	// with just an IP + port (no path needed in the connection dialog).
	davPort := cfg.WebDAVPort
	if davPort == 0 {
		davPort = 5747
	}
	var davRoot string
	for _, f := range cfg.LocalLibraryFolders {
		if f.Type == "" || f.Type == "video" {
			davRoot = f.Path
			break
		}
	}
	if davRoot == "" && len(cfg.LocalLibraryFolders) > 0 {
		davRoot = cfg.LocalLibraryFolders[0].Path
	}
	if davRoot != "" {
		dav := &webdav.Handler{
			FileSystem: webdav.Dir(davRoot),
			LockSystem: webdav.NewMemLS(),
		}
		wantUser := cfg.WebDAVUser
		wantPass := cfg.WebDAVPass
		// Wrap with Basic Auth so file managers get the expected 401 challenge.
		// If webdav_user/webdav_pass are unset, any credentials are accepted.
		davHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			user, pass, ok := r.BasicAuth()
			if !ok || (wantUser != "" && (user != wantUser || pass != wantPass)) {
				w.Header().Set("WWW-Authenticate", `Basic realm="Stream Videos"`)
				http.Error(w, "Unauthorized", http.StatusUnauthorized)
				return
			}
			dav.ServeHTTP(w, r)
		})
		// Try davPort, then davPort+1, until we find a free one.
		go func() {
			for port := davPort; port <= davPort+10; port++ {
				ln, err := net.Listen("tcp", fmt.Sprintf(":%d", port))
				if err != nil {
					logger.Printf("webdav port %d in use, trying %d", port, port+1)
					continue
				}
				logger.Printf("webdav server listening on :%d (root: %s)", port, davRoot)
				davSrv := &http.Server{Handler: davHandler}
				if err := davSrv.Serve(ln); err != nil && err != http.ErrServerClosed {
					logger.Printf("webdav server error: %v", err)
				}
				return
			}
			logger.Printf("webdav: could not find a free port in range %d-%d", davPort, davPort+10)
		}()
	}
	return srv
}

func runAsService(exePath string) {
	logPath := filepath.Join(filepath.Dir(exePath), "stream.log")
	lf, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0644)
	if err == nil {
		logger = log.New(lf, "", log.LstdFlags)
	} else {
		logger = log.Default()
	}
	mustLoadConfig()
	svc.Run(serviceName, &winSvc{})
}

// ─── CLI ──────────────────────────────────────────────────────────────────────

func cmdInstall(exePath string) {
	m, err := mgr.Connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect SCM:", err)
		os.Exit(1)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err == nil {
		s.Close()
		fmt.Println("service already installed")
		return
	}
	s, err = m.CreateService(serviceName, exePath, mgr.Config{
		DisplayName: serviceDispName,
		StartType:   mgr.StartAutomatic,
		Description: "Stream video server",
	})
	if err != nil {
		fmt.Fprintln(os.Stderr, "create service:", err)
		os.Exit(1)
	}
	s.Close()
	fmt.Println("service installed — run: net start stream")
}

func cmdUninstall() {
	m, err := mgr.Connect()
	if err != nil {
		fmt.Fprintln(os.Stderr, "connect SCM:", err)
		os.Exit(1)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		fmt.Fprintln(os.Stderr, "service not found:", err)
		os.Exit(1)
	}
	defer s.Close()
	if err := s.Delete(); err != nil {
		fmt.Fprintln(os.Stderr, "delete:", err)
		os.Exit(1)
	}
	fmt.Println("service uninstalled")
}

func cmdSetup() {
	r := bufio.NewReader(os.Stdin)
	ask := func(prompt, def string) string {
		if def != "" {
			fmt.Printf("%s [%s]: ", prompt, def)
		} else {
			fmt.Printf("%s: ", prompt)
		}
		line, _ := r.ReadString('\n')
		line = strings.TrimSpace(line)
		if line == "" {
			return def
		}
		return line
	}

	fmt.Println("// stream setup")

	// Password
	fmt.Print("password: ")
	pw, _ := term.ReadPassword(int(syscall.Stdin))
	fmt.Println()
	hash, err := bcrypt.GenerateFromPassword(pw, bcrypt.DefaultCost)
	if err != nil {
		fmt.Fprintln(os.Stderr, "bcrypt:", err)
		os.Exit(1)
	}
	cfg.PasswordHash = string(hash)

	// Secret
	if cfg.Secret == "" {
		var sec [32]byte
		rand.Read(sec[:])
		cfg.Secret = hex.EncodeToString(sec[:])
	}

	// Port
	portStr := ask("port", strconv.Itoa(defaultPort))
	if p, err := strconv.Atoi(portStr); err == nil {
		cfg.Port = p
	} else {
		cfg.Port = defaultPort
	}

	// R2
	fmt.Println("\n// Cloudflare R2")
	cfg.R2AccountID = ask("account ID", cfg.R2AccountID)
	cfg.R2AccessKey = ask("access key ID", cfg.R2AccessKey)
	cfg.R2SecretKey = ask("secret access key", cfg.R2SecretKey)
	cfg.R2Bucket = ask("bucket name", cfg.R2Bucket)
	cfg.R2PublicURL = ask("public bucket URL (e.g. https://pub-xxx.r2.dev)", cfg.R2PublicURL)

	// FFmpeg
	fmt.Println("\n// FFmpeg")
	cfg.FFmpegPath = ask("ffmpeg path", "ffmpeg")
	cfg.FFprobePath = ask("ffprobe path", "ffprobe")

	if err := saveConfig(); err != nil {
		fmt.Fprintln(os.Stderr, "save config:", err)
		os.Exit(1)
	}
	fmt.Println("\nconfig saved →", cfgPath)
}

func cmdSend(args []string) {
	if err := loadConfig(); err != nil {
		fmt.Fprintln(os.Stderr, "config not found — run: stream.exe setup")
		os.Exit(1)
	}
	if len(args) == 0 {
		fmt.Fprintln(os.Stderr, "usage: stream send <file>...")
		os.Exit(1)
	}
	// Simple local HTTP upload via presign not suitable from CLI without session.
	// Use direct R2 upload for the send command.
	r2 := newR2Client(cfg.R2AccountID, cfg.R2AccessKey, cfg.R2SecretKey)
	for _, path := range args {
		f, err := os.Open(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "open %s: %v\n", path, err)
			continue
		}
		fi, _ := f.Stat()
		key := filepath.Base(path)
		_, err = r2.PutObject(context.Background(), &s3.PutObjectInput{
			Bucket:        awsv2.String(cfg.R2Bucket),
			Key:           awsv2.String(key),
			Body:          f,
			ContentLength: awsv2.Int64(fi.Size()),
		})
		f.Close()
		if err != nil {
			fmt.Fprintf(os.Stderr, "upload %s: %v\n", path, err)
		} else {
			fmt.Printf("uploaded: %s\n", key)
		}
	}
}

// ─── Main ─────────────────────────────────────────────────────────────────────

func main() {
	exePath, _ := os.Executable()
	cfgPath = filepath.Join(filepath.Dir(exePath), "config.json")
	logger = log.Default()

	// Check if running as Windows service
	isService, err := svc.IsWindowsService()
	if err == nil && isService {
		runAsService(exePath)
		return
	}

	// Elevate check for install/uninstall
	needsAdmin := len(os.Args) > 1 && (os.Args[1] == "install" || os.Args[1] == "uninstall")
	if needsAdmin {
		isAdmin := windows.GetCurrentProcessToken().IsElevated()
		if !isAdmin {
			fmt.Fprintln(os.Stderr, "run as Administrator for install/uninstall")
			os.Exit(1)
		}
	}

	if len(os.Args) < 2 {
		fmt.Println("usage: stream.exe <setup|install|uninstall|serve|send>")
		os.Exit(1)
	}

	switch os.Args[1] {
	case "setup":
		cmdSetup()
	case "install":
		mustLoadConfig()
		cmdInstall(exePath)
	case "uninstall":
		cmdUninstall()
	case "serve":
		mustLoadConfig()
		logger.Printf("stream serving on :%d", cfg.Port)
		srv := startServer()
		// Block until interrupt
		select {}
		_ = srv
	case "send":
		cmdSend(os.Args[2:])
	default:
		fmt.Fprintf(os.Stderr, "unknown command: %s\n", os.Args[1])
		os.Exit(1)
	}
}
