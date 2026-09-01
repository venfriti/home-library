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
	"errors"
	"fmt"
	"html/template"
	"image"
	"image/color"
	"image/draw"
	"image/jpeg"
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
	// How many times a file with no TMDB match is retried before it is left
	// alone. Manual re-enrichment always overrides this.
	maxEnrichTries = 4
	// How many times the audio/subtitle check is retried on a file that keeps
	// failing it before giving up on that file for good. A low number: a
	// file still arriving (see isFileStable) is never even attempted, so a
	// failure that makes it this far is a real, probably permanent one
	// (an incompatible codec/stream layout, say) — not worth retrying forever.
	maxAudioCheckTries = 3

	// localMetaVersion is bumped whenever enrichment starts producing a field
	// the cache did not previously hold. Entries below it are re-fetched from
	// TMDB by backfillLocalMeta — additively, so hand-corrected titles and ids
	// survive.
	// v4 adds OriginalLang (TMDB's original_language), which the audio-track
	// fix needs to know a Japanese film or anime should default to its
	// Japanese track rather than being forced to English.
	localMetaVersion = 4
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
	// PgsToSrtPath is Tentacule/PgsToSrt.exe — OCRs a Blu-ray's bitmap (PGS)
	// subtitles into real text, for the releases whose only subtitle track
	// ffmpeg can copy-extract nothing useful from (see textSubtitleCodecs).
	// Needs the .NET 8.0 runtime installed and a tessdata language file next
	// to it; empty disables PGS OCR entirely (a text subtitle, when one
	// exists, is unaffected either way).
	PgsToSrtPath string `json:"pgstosrt_path,omitempty"`
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
	// When true a finished download is filed into the local library instead of
	// being transcoded up to R2 and deleted. This is the path a torrent takes:
	// qBittorrent drops it in the watch folder, it gets classified and moved
	// under Movies/ or Tv Series/, and the enrichment pass then picks it up.
	SortDownloadsToLibrary bool `json:"sort_downloads_to_library,omitempty"`
	// yt-dlp, for the music-video search. Empty falls back to whatever is on
	// PATH. The cookie file is the same one the standalone yt-dlp UI uses, so
	// age-gated videos work without a second login.
	YtdlpPath   string `json:"ytdlp_path,omitempty"`
	YtdlpCookie string `json:"ytdlp_cookie_file,omitempty"`
	// Folders served raw over the LAN as the "local library" — never pushed to
	// R2/Cloudflare, no transcoding. Played inline when browser-compatible,
	// otherwise opened in a desktop app on the host. Type defaults to "video"
	// (TMDB/LLM movie & TV enrichment); "music_video" routes filename parsing
	// + metadata lookup to iTunes instead, skipping TMDB/Ollama entirely.
	LocalLibraryFolders []LocalFolder `json:"local_library_folders,omitempty"`
	// Sharing a session with FileDrop. Cookies are scoped by host and ignore
	// the port, so both apps on the same machine see the same cookie jar:
	// matching the cookie name and secret means one login covers both, and the
	// hours-remaining figure is read off the same expiry. Leave unset and the
	// server keeps its own independent session.
	AuthCookieName string `json:"auth_cookie_name,omitempty"`
	AuthSecret     string `json:"auth_secret,omitempty"`

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

// pgsToSrtBin returns "" when PGS OCR isn't configured, which every caller
// treats as "skip this, there's nothing to run" rather than falling back to
// a bare "PgsToSrt" on PATH the way ffmpeg/ffprobe do — unlike those, this
// tool has no common system-wide install a bare command name would find.
func pgsToSrtBin() string {
	return cfg.PgsToSrtPath
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
	cookie        string
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
	sortToLibrary bool
	ytdlpPath     string
	ytdlpCookie   string
	localFolders  []LocalFolder
}

type procState struct {
	Status string `json:"status"` // "processing" | "error"
	Err    string `json:"error,omitempty"`
	// Filled in by the music fetcher, which streams yt-dlp's output rather
	// than waiting for it — a download that reports nothing until it finishes
	// is indistinguishable from one that has hung.
	Percent float64 `json:"percent,omitempty"`
	Stage   string  `json:"stage,omitempty"`
	Title   string  `json:"title,omitempty"`
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

func (c *serverCtx) cookieKey() string {
	if c.cookie != "" {
		return c.cookie
	}
	return cookieName
}

func (c *serverCtx) isAuthed(r *http.Request) bool {
	ck, err := r.Cookie(c.cookieKey())
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
		Name:     c.cookieKey(),
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
	ck, err := r.Cookie(c.cookieKey())
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
	// OriginalLang is TMDB's original_language for the title (e.g. "ja") —
	// what the audio-fix checks to know a Japanese film or anime should keep
	// its Japanese track as default rather than being forced to an English
	// dub. See AudioLang below for what the file actually carries.
	OriginalLang string          `json:"original_lang,omitempty"`
	Collection   *tmdbCollection `json:"collection,omitempty"`
	// Local-library extras classification. For a movie's bonus material this
	// is set by hand in this cache file — see markExtras — and MainKey is the
	// movie's own file key. For a TV show's bonus material (a season's
	// Featurettes folder) this is instead set automatically at scan time from
	// the folder's own naming — see markShowFeaturettes — and MainKey is
	// "show:<tmdb id>:s<season>" rather than a file key, since a show has no
	// single file to anchor to, and season-scoping keeps a season's extras
	// with that season instead of piling every season's into one list.
	IsExtra bool   `json:"is_extra,omitempty"`
	MainKey string `json:"main_key,omitempty"`
	// AudioLang is the language of the audio track that will actually play —
	// probed from the file's own streams, not TMDB's listing of a
	// production's original language, which is a different fact (a Japanese
	// film can carry an English dub track, and the two fields disagree on
	// purpose in that case). Empty means the file has not been checked yet.
	// See AudioCheckVer, checkAudioTrack, ensureEnglishDefault.
	AudioLang string `json:"audio_lang,omitempty"`
	// AudioCheckVer gates the one-time-per-file audio probe/fix the same way
	// MetaVersion gates TMDB re-enrichment: bump audioCheckVersion to force
	// every file through the check again (e.g. if the fix logic changes).
	// Only ever set on a successful check (or one that's exhausted
	// AudioCheckTries) — see checkAudioTrack — so a file that errors because
	// it's still mid-download keeps getting retried once it's finished,
	// instead of being marked done on a check that never actually ran.
	AudioCheckVer int `json:"audio_check_ver,omitempty"`
	// AudioCheckTries counts failed attempts, same idea as EnrichTries —
	// see maxAudioCheckTries.
	AudioCheckTries int `json:"audio_check_tries,omitempty"`
	// PGSOCRVer gates the one-time-per-file PGS (bitmap) subtitle OCR pass —
	// see checkPGSSubtitle, pgsOCRVersion. Set whether OCR actually ran or
	// wasn't applicable (a text subtitle already covered the file, or there
	// was no PGS track at all) — either way there's nothing more to do for
	// this file until pgsOCRVersion changes.
	PGSOCRVer int `json:"pgs_ocr_ver,omitempty"`
	// MetaVersion records which enrichment schema wrote this entry. Cached
	// local-library entries predating a field stay at the older number until
	// backfillLocalMeta refreshes them; see localMetaVersion.
	MetaVersion int `json:"meta_version,omitempty"`
	// EnrichTries counts how many times enrichment has run for this file
	// without resolving a TMDB id. A miss used to be cached as though it were
	// an answer, so a file that failed once — an LLM timeout, TMDB briefly
	// unreachable, a download still settling — stayed nameless forever. It is
	// now retried a few times before the server gives up on it.
	EnrichTries int `json:"enrich_tries,omitempty"`
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

// findEmbeddedPGSSubtitle returns the absolute stream index of the source's
// best PGS (bitmap) subtitle track, preferring English, or found=false if
// there is none. Separate from findEmbeddedSubtitle because a PGS stream
// needs OCR (see ocrPGSSubtitle) rather than the plain codec re-encode that
// covers every textSubtitleCodecs entry — a Blu-ray rip's subtitle is a
// picture of the text, not the text itself.
func findEmbeddedPGSSubtitle(path string) (index int, found bool) {
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
		if s.CodecName != "hdmv_pgs_subtitle" {
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

// ocrPGSSubtitle runs PgsToSrt (see pgsToSrtBin) against one PGS subtitle
// track and writes the result straight into the local-subtitle cache as
// WebVTT, at the exact path detectLocalSubtitlePath already checks first —
// so once this has run, the existing serving path picks up the result with
// no changes of its own.
func ocrPGSSubtitle(absVideoPath string, trackIndex int, outVTTPath string) error {
	tmpSRT := outVTTPath + ".ocr.srt"
	os.Remove(tmpSRT)
	cmd := exec.Command(pgsToSrtBin(),
		"--input", absVideoPath,
		"--track", strconv.Itoa(trackIndex),
		"--output", tmpSRT,
		"--tesseractlanguage", "eng",
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		os.Remove(tmpSRT)
		return fmt.Errorf("%v: %s", err, truncateForLog(out))
	}
	data, err := os.ReadFile(tmpSRT)
	os.Remove(tmpSRT)
	if err != nil {
		return fmt.Errorf("reading OCR output: %v", err)
	}
	// This model reliably reads a capital "I" as a pipe in this font — "|"
	// never legitimately appears in subtitle text, so a blanket replacement
	// is safe and fixes the single most common artifact for free.
	data = bytes.ReplaceAll(data, []byte("|"), []byte("I"))
	return os.WriteFile(outVTTPath, srtToVTT(data), 0644)
}

// pgsOCRVersion gates the one-time-per-file PGS OCR pass the same way
// audioCheckVersion gates the audio/subtitle default check — bump it to send
// every file through OCR again (e.g. after a tessdata/model upgrade).
const pgsOCRVersion = 1

// checkPGSSubtitle is the per-file entry point the enrichment loop calls. A
// text subtitle, when one exists, is always preferred — faster, exact, and
// already what detectLocalSubtitlePath serves — so OCR only ever runs when
// there is no better option.
func checkPGSSubtitle(v localVideo) {
	vm, had := getLocalMeta(v.Rel)
	if had && vm.PGSOCRVer >= pgsOCRVersion {
		return
	}
	markDone := func() {
		localMetaMu.Lock()
		cur := localMetaCache[v.Rel]
		cur.PGSOCRVer = pgsOCRVersion
		localMetaCache[v.Rel] = cur
		localMetaMu.Unlock()
		saveLocalMetaCache()
	}
	if _, found := findEmbeddedSubtitle(v.Abs); found {
		markDone()
		return
	}
	idx, found := findEmbeddedPGSSubtitle(v.Abs)
	if !found {
		markDone()
		return
	}
	cachePath := filepath.Join(localSubtitleCacheDir(), hex.EncodeToString(sha256Sum(v.Abs))+".vtt")
	if err := ocrPGSSubtitle(v.Abs, idx, cachePath); err != nil {
		logger.Printf("PGS OCR %s: %v", v.Rel, err)
		markDone()
		return
	}
	// The lazy request-time cache (detectLocalSubtitlePath) hasn't seen this
	// file yet — prime it directly rather than waiting for the first
	// /local/subtitle request to stat what was just written.
	localSubtitleMu.Lock()
	localSubtitleCache[v.Abs] = cachePath
	localSubtitleMu.Unlock()
	logger.Printf("PGS OCR: extracted subtitles for %s", v.Rel)
	markDone()
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
var thumbSem = make(chan struct{}, 3)

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
		thumbSem <- struct{}{}
		defer func() { <-thumbSem }()
		// Another request may have produced it while this one waited.
		if fileExists(cachePath) {
			localThumbnailMu.Lock()
			localThumbnailCache[absVideoPath] = cachePath
			localThumbnailMu.Unlock()
			return cachePath
		}
		meta, isVideo, _ := probeVideo(absVideoPath)
		dur := 0
		if isVideo {
			dur = meta.Duration
		}
		if err := extractThumbnail(absVideoPath, cachePath, dur); err != nil || !fileExists(cachePath) {
			result = ""
		}
	}
	// A miss is never cached: unlike a genuinely codec-less file, a file that
	// failed because it's still mid-download (see isFileStable) will succeed
	// on a later request once it's finished, and there is nothing here to
	// tell the two cases apart. Only a real still is worth remembering.
	if result != "" {
		localThumbnailMu.Lock()
		localThumbnailCache[absVideoPath] = result
		localThumbnailMu.Unlock()
	}
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
			// 127.0.0.1, not "localhost": ollama binds IPv4 only, and Go's
			// resolver tries ::1 first on Windows, so every call sat there
			// until the 30s client timeout and came back as a failure. That
			// silently broke all filename parsing, which is why downloads
			// arrived on the wall nameless.
			baseURL = "http://127.0.0.1:11434"
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
// tmdbDetail is the slice of TMDB's detail + credits responses the UI renders.
// It is split out of buildVideoMetaForID so backfillLocalMeta can refresh just
// these fields against an already-resolved TMDB id, without re-running the
// filename parse that picked that id — parses that have in several cases been
// corrected by hand.
type tmdbDetail struct {
	Genres     []string
	Backdrop   string
	Runtime    int
	Country    string
	Language   string
	// OriginalLang is TMDB's original_language (ISO 639-1, e.g. "ja") — the
	// production's native language, distinct from Language above (which is
	// merely the first of its spoken_languages). This is what tells the
	// audio-track fix that a Japanese-original film or anime should default
	// to its Japanese track rather than being forced to an English dub.
	OriginalLang string
	Collection   *tmdbCollection
	Director     string // TV: the show's creator
	Cast         []string
}

func (c *serverCtx) fetchTMDBDetail(id int, mediaType string) tmdbDetail {
	var out tmdbDetail
	if c.tmdbKey == "" || id <= 0 {
		return out
	}
	if mediaType != "tv" {
		mediaType = "movie"
	}
	if dr, err := tmdbHTTPClient.Get(fmt.Sprintf("https://api.themoviedb.org/3/%s/%d?api_key=%s", mediaType, id, c.tmdbKey)); err == nil {
		defer dr.Body.Close()
		var d struct {
			Genres         []struct{ Name string `json:"name"` } `json:"genres"`
			BackdropPath   string                                `json:"backdrop_path"`
			Runtime        int                                   `json:"runtime"`
			OriginalLang   string                                `json:"original_language"`
			// TV-only. episode_run_time is often empty (Arcane returns []),
			// so the per-episode fetch in buildVideoMetaForID overrides this
			// whenever it can.
			EpisodeRunTime      []int `json:"episode_run_time"`
			CreatedBy           []struct{ Name string `json:"name"` } `json:"created_by"`
			ProductionCountries []struct{ Name string `json:"name"` } `json:"production_countries"`
			SpokenLanguages     []struct {
				EnglishName string `json:"english_name"`
			} `json:"spoken_languages"`
			BelongsToCollection *struct {
				ID   int    `json:"id"`
				Name string `json:"name"`
			} `json:"belongs_to_collection"`
		}
		if json.NewDecoder(dr.Body).Decode(&d) == nil {
			for _, g := range d.Genres {
				out.Genres = append(out.Genres, g.Name)
			}
			if d.BackdropPath != "" {
				out.Backdrop = "https://image.tmdb.org/t/p/w1280" + d.BackdropPath
			}
			out.Runtime = d.Runtime
			if out.Runtime == 0 && len(d.EpisodeRunTime) > 0 {
				out.Runtime = d.EpisodeRunTime[0]
			}
			if len(d.CreatedBy) > 0 {
				out.Director = d.CreatedBy[0].Name
			}
			if len(d.ProductionCountries) > 0 {
				out.Country = d.ProductionCountries[0].Name
			}
			if len(d.SpokenLanguages) > 0 {
				out.Language = d.SpokenLanguages[0].EnglishName
			}
			out.OriginalLang = d.OriginalLang
			if d.BelongsToCollection != nil {
				out.Collection = &tmdbCollection{ID: d.BelongsToCollection.ID, Name: d.BelongsToCollection.Name}
			}
		}
	}

	// Credits: a film's director comes from the crew; a show's creator already
	// came from created_by above, so only the cast is taken here.
	credURL := fmt.Sprintf("https://api.themoviedb.org/3/%s/%d/credits?api_key=%s", mediaType, id, c.tmdbKey)
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
			if mediaType == "movie" {
				for _, p := range cred.Crew {
					if p.Job == "Director" {
						out.Director = p.Name
						break
					}
				}
			}
			for i, p := range cred.Cast {
				if i >= 5 {
					break
				}
				out.Cast = append(out.Cast, p.Name)
			}
		}
	}
	return out
}

// fetchEpisodeRuntime asks the episode endpoint for a single episode's length.
// The series-level episode_run_time is empty for a lot of shows (Arcane returns
// []), so this is the only reliable source — and an episode list without
// runtimes is missing the one number people actually scan for.
func (c *serverCtx) fetchEpisodeRuntime(id, season, episode int) int {
	if c.tmdbKey == "" || id <= 0 || season <= 0 || episode <= 0 {
		return 0
	}
	u := fmt.Sprintf("https://api.themoviedb.org/3/tv/%d/season/%d/episode/%d?api_key=%s",
		id, season, episode, c.tmdbKey)
	resp, err := tmdbHTTPClient.Get(u)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	var ep struct {
		Runtime int `json:"runtime"`
	}
	if json.NewDecoder(resp.Body).Decode(&ep) != nil {
		return 0
	}
	return ep.Runtime
}

func (c *serverCtx) buildVideoMetaForID(id int, title string, year int, overview, posterPath string, rating float64, mediaType string, season, episode int) videoMeta {
	if mediaType != "tv" {
		mediaType = "movie"
	}
	det := c.fetchTMDBDetail(id, mediaType)
	runtime := det.Runtime

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
				Runtime   int    `json:"runtime"`
			}
			if json.NewDecoder(er.Body).Decode(&ep) == nil {
				epTitle = ep.Name
				epOverview = ep.Overview
				if ep.StillPath != "" {
					epStill = "https://image.tmdb.org/t/p/w500" + ep.StillPath
				}
				// Per-episode runtime beats the series average, and is present
				// far more often than episode_run_time.
				if ep.Runtime > 0 {
					runtime = ep.Runtime
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
		Genres:          det.Genres,
		TMDBID:          id,
		TMDBBackdrop:    det.Backdrop,
		EpisodeTitle:    epTitle,
		EpisodeOverview: epOverview,
		EpisodeStill:    epStill,
		Runtime:         runtime,
		Director:        det.Director,
		Cast:            det.Cast,
		Country:         det.Country,
		Language:        det.Language,
		OriginalLang:    det.OriginalLang,
		Collection:      det.Collection,
		MetaVersion:     localMetaVersion,
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
	// A dot between two digits is a version number ("Shirtless Montage 3.0"),
	// not a word separator — only the latter should become a space.
	runes := []rune(name)
	for i, r := range runes {
		if r != '.' {
			continue
		}
		digitBefore := i > 0 && runes[i-1] >= '0' && runes[i-1] <= '9'
		digitAfter := i < len(runes)-1 && runes[i+1] >= '0' && runes[i+1] <= '9'
		if !(digitBefore && digitAfter) {
			runes[i] = ' '
		}
	}
	return strings.Join(strings.Fields(string(runes)), " ")
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


// isNetworkError reports whether err means the request never got a real
// answer at all (connection refused, DNS failure, timeout) rather than the
// remote service actually responding. This distinction has bitten this
// project before — see the 127.0.0.1-vs-localhost note on llmChat, where an
// unreachable Ollama silently broke every filename parse — so enrichMeta
// surfaces it instead of letting the caller treat "couldn't even ask" the
// same as "asked and got no match".
func isNetworkError(err error) bool {
	var netErr net.Error
	return errors.As(err, &netErr)
}

func (c *serverCtx) enrichMeta(filename string, meta *videoMeta) error {
	if c.llmProvider == "" {
		return nil
	}
	parsed, err := c.parseMediaFilename(filename)
	if err != nil {
		logger.Printf("LLM parse %s: %v", filename, err)
		if isNetworkError(err) {
			return err
		}
		return nil
	}
	logger.Printf("LLM parsed %q → %+v", filename, parsed)
	if c.tmdbKey == "" {
		// No TMDB — at least store what we got from LLM
		meta.Title = parsed.Title
		meta.Year = parsed.Year
		meta.MediaType = parsed.Type
		meta.Season = parsed.Season
		meta.Episode = parsed.Episode
		return nil
	}
	info, err := c.fetchTMDB(parsed)
	if err != nil {
		logger.Printf("TMDB %s: %v", filename, err)
		if isNetworkError(err) {
			return err
		}
		// Fall back to LLM-only data
		meta.Title = parsed.Title
		meta.Year = parsed.Year
		meta.MediaType = parsed.Type
		return nil
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
	return nil
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
			if c.sortToLibrary {
				// Filed into the library rather than transcoded to R2. The
				// enrichment pass below picks it up on its next sweep.
				go func(pth string) {
					if _, ok := c.sortDownload(pth); ok {
						c.kickEnrichment()
					}
				}(fullPath)
				continue
			}
			go c.ingestLocalFile(fullPath)
		}
		time.Sleep(30 * time.Second)
	}
}

// videoLibraryRoot returns the folder configured for ordinary video, which is
// where sorted downloads are filed.
func (c *serverCtx) videoLibraryRoot() string {
	for _, f := range c.localFolders {
		if f.Type == "" || f.Type == "video" {
			return f.Path
		}
	}
	return ""
}

// unsafeInPath reports the characters Windows will not accept in a
// folder name, plus any control character.
func unsafeInPath(r rune) bool {
	if r < 0x20 {
		return true
	}
	return strings.ContainsRune(`<>:"/\|?*`, r)
}

// safeSegment makes a title usable as a folder name on Windows without
// mangling it beyond recognition.
func safeSegment(s string) string {
	s = strings.Map(func(r rune) rune {
		if unsafeInPath(r) {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	s = strings.TrimRight(s, " .")
	if len(s) > 120 {
		s = strings.TrimSpace(s[:120])
	}
	return s
}

// sortDownload classifies a finished download by its filename and files it
// into the library — Movies/<Title (Year)>/ for a film, Tv Series/<Title>/
// Season NN/ for an episode. Anything it cannot identify is left exactly where
// it is rather than guessed at, so nothing disappears into a wrong folder.
//
// Music videos are deliberately not handled here: they arrive through the
// yt-dlp path, which already knows what it fetched, and guessing "Artist -
// Song" against torrent release names produced false positives.
func (c *serverCtx) sortDownload(localPath string) (string, bool) {
	root := c.videoLibraryRoot()
	if root == "" {
		return "", false
	}
	base := filepath.Base(localPath)
	p, err := c.parseMediaFilename(base)
	if err != nil || p.Title == "" {
		logger.Printf("sort: could not identify %q — leaving in place", base)
		return "", false
	}

	var destDir string
	title := safeSegment(p.Title)
	if title == "" {
		return "", false
	}
	if p.Type == "tv" && p.Season > 0 {
		destDir = filepath.Join(root, "Tv Series", title, fmt.Sprintf("Season %02d", p.Season))
	} else {
		name := title
		if p.Year > 0 {
			name = fmt.Sprintf("%s (%d)", title, p.Year)
		}
		destDir = filepath.Join(root, "Movies", name)
	}
	if err := os.MkdirAll(destDir, 0755); err != nil {
		logger.Printf("sort: mkdir %s: %v", destDir, err)
		return "", false
	}

	dest := filepath.Join(destDir, base)
	if fileExists(dest) {
		logger.Printf("sort: %s already present at %s — leaving the download in place", base, destDir)
		return "", false
	}
	if err := os.Rename(localPath, dest); err != nil {
		// A rename across volumes fails on Windows, so fall back to a copy.
		if cpErr := copyFile(localPath, dest); cpErr != nil {
			logger.Printf("sort: move %s: rename %v, copy %v", base, err, cpErr)
			return "", false
		}
		os.Remove(localPath)
	}
	logger.Printf("sort: %s -> %s", base, destDir)
	return dest, true
}

func copyFile(from, to string) error {
	in, err := os.Open(from)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(to)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		os.Remove(to)
		return err
	}
	return out.Close()
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

// ─── Music video search (yt-dlp) ──────────────────────────────────────────────
//
// Searching and fetching both go through yt-dlp rather than the YouTube Data
// API: no key to manage, and it is the same tool (and the same cookie jar)
// the standalone downloader already uses.

type musicHit struct {
	ID        string  `json:"id"`
	Title     string  `json:"title"`
	Channel   string  `json:"channel"`
	Duration  float64 `json:"duration"`
	Views     int64   `json:"views"`
	Thumbnail string  `json:"thumbnail"`
	URL       string  `json:"url"`
	Official  bool    `json:"official"`
}

func (c *serverCtx) ytdlpBin() string {
	if c.ytdlpPath != "" {
		return c.ytdlpPath
	}
	return "yt-dlp"
}

// musicLibraryRoot is where a fetched music video is written — the folder
// already configured as music_video, so the existing wall picks it up.
func (c *serverCtx) musicLibraryRoot() string {
	for _, f := range c.localFolders {
		if f.Type == "music_video" {
			return f.Path
		}
	}
	return ""
}

// leadArtist takes the part of a query before any dash, which is where the
// artist sits in the way people type these ("Charli XCX - 360", or just
// "Charli XCX 360").
func leadArtist(q string) string {
	if i := strings.Index(q, "-"); i > 0 {
		return normalizeTitle(q[:i])
	}
	return normalizeTitle(q)
}

func (c *serverCtx) searchMusicVideos(query string) ([]musicHit, error) {
	if strings.TrimSpace(query) == "" {
		return nil, fmt.Errorf("empty query")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	args := []string{
		"ytsearch12:" + query,
		"--dump-json", "--flat-playlist", "--no-warnings", "--ignore-errors",
	}
	if c.ytdlpCookie != "" && fileExists(c.ytdlpCookie) {
		args = append(args, "--cookies", c.ytdlpCookie)
	}
	out, err := exec.CommandContext(ctx, c.ytdlpBin(), args...).Output()
	if err != nil && len(out) == 0 {
		return nil, fmt.Errorf("yt-dlp search: %v", err)
	}

	artist := leadArtist(query)
	var hits []musicHit
	for _, line := range strings.Split(string(out), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || !strings.HasPrefix(line, "{") {
			continue
		}
		var d struct {
			ID        string  `json:"id"`
			Title     string  `json:"title"`
			Channel   string  `json:"channel"`
			Uploader  string  `json:"uploader"`
			Duration  float64 `json:"duration"`
			ViewCount int64   `json:"view_count"`
			Thumbnails []struct {
				URL string `json:"url"`
			} `json:"thumbnails"`
			URL string `json:"url"`
		}
		if json.Unmarshal([]byte(line), &d) != nil || d.ID == "" {
			continue
		}
		ch := d.Channel
		if ch == "" {
			ch = d.Uploader
		}
		thumb := ""
		if len(d.Thumbnails) > 0 {
			thumb = d.Thumbnails[len(d.Thumbnails)-1].URL
		}
		u := d.URL
		if u == "" {
			u = "https://www.youtube.com/watch?v=" + d.ID
		}
		// "Official" here means the upload is on the artist's own channel,
		// which is the signal that actually separates the real video from a
		// lyric-video reupload.
		chNorm := normalizeTitle(ch)
		official := artist != "" && chNorm != "" &&
			(strings.Contains(chNorm, artist) || strings.Contains(artist, chNorm))
		hits = append(hits, musicHit{
			ID: d.ID, Title: d.Title, Channel: ch, Duration: d.Duration,
			Views: d.ViewCount, Thumbnail: thumb, URL: u, Official: official,
		})
	}
	// Stable order, official first — the ranking YouTube returned is kept
	// within each group.
	sort.SliceStable(hits, func(i, j int) bool { return hits[i].Official && !hits[j].Official })
	return hits, nil
}

func (c *serverCtx) handleSearchMusic(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	hits, err := c.searchMusicVideos(r.URL.Query().Get("q"))
	if err != nil {
		http.Error(w, err.Error(), 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(hits)
}

// fetchMusicVideo downloads into the music-video folder and reports progress
// through the same procMap the transcoder uses, so the UI needs no second
// mechanism to watch it.
// ytProgressRe matches yt-dlp's per-line download percentage.
var ytProgressRe = regexp.MustCompile(`\[download\]\s+(\d+\.?\d*)%`)

func (c *serverCtx) fetchMusicVideo(url, title string) {
	dest := c.musicLibraryRoot()
	if dest == "" {
		logger.Printf("music fetch: no music_video folder configured")
		return
	}
	key := "yt:" + url
	c.setProcState(key, procState{Status: "processing"})
	os.MkdirAll(dest, 0755)

	// Prefer H.264 video with AAC audio. "Best available" in the raw sense
	// selects AV1 or VP9, which the wall cannot play inline and which YouTube
	// serves from streams that 403 part-way far more often — the first
	// downloads through here landed as bare .f399 video fragments with no
	// audio because the audio stream was refused and the merge never ran.
	// This still takes the best H.264 on offer (1080p, YouTube's ceiling for
	// it) and only falls back to other codecs if there is no H.264 at all.
	//
	// --no-part is deliberately absent: the partial files are how a retried
	// fragment resumes rather than restarting.
	args := []string{
		"-f", "bv*[vcodec^=avc1][ext=mp4]+ba[ext=m4a]/b[ext=mp4]/bv*+ba/b",
		"--merge-output-format", "mp4",
		"-o", filepath.Join(dest, "%(title)s.%(ext)s"),
		"--no-playlist", "--newline", "--no-warnings",
		"-R", "5", "--fragment-retries", "10",
	}
	if c.ytdlpCookie != "" && fileExists(c.ytdlpCookie) {
		args = append(args, "--cookies", c.ytdlpCookie)
	}
	if cfg.FFmpegPath != "" {
		args = append(args, "--ffmpeg-location", filepath.Dir(cfg.FFmpegPath))
	}
	args = append(args, url)

	logger.Printf("music fetch: %s", title)
	cmd := exec.Command(c.ytdlpBin(), args...)
	stdout, pipeErr := cmd.StdoutPipe()
	if pipeErr != nil {
		c.setProcState(key, procState{Status: "error", Err: pipeErr.Error()})
		return
	}
	cmd.Stderr = cmd.Stdout
	if startErr := cmd.Start(); startErr != nil {
		c.setProcState(key, procState{Status: "error", Err: startErr.Error()})
		return
	}

	// yt-dlp reports one line per progress tick with --newline, so the
	// percentage can be surfaced live instead of after the fact.
	var tail []string
	sc := bufio.NewScanner(stdout)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		tail = append(tail, line)
		if len(tail) > 12 {
			tail = tail[1:]
		}
		if m := ytProgressRe.FindStringSubmatch(line); m != nil {
			if pct, perr := strconv.ParseFloat(m[1], 64); perr == nil {
				c.setProcState(key, procState{Status: "processing", Percent: pct, Stage: "downloading", Title: title})
			}
			continue
		}
		if strings.HasPrefix(line, "[Merger]") {
			c.setProcState(key, procState{Status: "processing", Percent: 99, Stage: "merging", Title: title})
		}
	}
	if err := cmd.Wait(); err != nil {
		logger.Printf("music fetch failed %s: %v\n%s", title, err, strings.Join(tail, "\n"))
		msg := "yt-dlp: " + err.Error()
		if len(tail) > 0 {
			msg = tail[len(tail)-1]
		}
		c.setProcState(key, procState{Status: "error", Err: msg, Title: title})
		return
	}
	logger.Printf("music fetch complete: %s", title)
	c.clearProcState(key)
	// The file is in the library folder now; wake the enrichment sweep so it
	// appears with an artist and title rather than waiting out the interval.
	c.kickEnrichment()
}

func (c *serverCtx) handleDownloadMusic(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", 405)
		return
	}
	var body struct {
		URL   string `json:"url"`
		Title string `json:"title"`
	}
	if json.NewDecoder(r.Body).Decode(&body) != nil || body.URL == "" {
		http.Error(w, "bad request", 400)
		return
	}
	if !strings.HasPrefix(body.URL, "https://www.youtube.com/") &&
		!strings.HasPrefix(body.URL, "https://youtu.be/") &&
		!strings.HasPrefix(body.URL, "https://music.youtube.com/") {
		http.Error(w, "unsupported source", 400)
		return
	}
	go c.fetchMusicVideo(body.URL, body.Title)
	w.WriteHeader(202)
	fmt.Fprint(w, "started")
}



// ─── Watch history ────────────────────────────────────────────────────────────
//
// playback_progress.json is keyed by a file's path, which is the right key for
// resuming the copy you have. It is the wrong key for surviving that copy being
// deleted and fetched again at a different path — a different release, a
// different resolution, a different folder — which loses the position and the
// fact you ever watched it.
//
// So every position is also written here under a key derived from what the
// file *is* rather than where it sits: its TMDB id (plus season and episode
// for television), falling back to a normalised filename for the 28% of the
// library TMDB never matched. A re-downloaded file resolves to the same key
// and picks up where it left off.

type watchRecord struct {
	Key       string  `json:"key"`             // most recent library key seen
	Title     string  `json:"title,omitempty"` // kept so history reads even when the file is gone
	MediaType string  `json:"media_type,omitempty"`
	Season    int     `json:"season,omitempty"`
	Episode   int     `json:"episode,omitempty"`
	Position  float64 `json:"position"`
	Duration  float64 `json:"duration"`
	Updated   int64   `json:"updated"`
	Completed bool    `json:"completed,omitempty"`
	Plays     int     `json:"plays,omitempty"`
}

var (
	historyMu    sync.Mutex
	historyCache = map[string]watchRecord{}
)

func historyPath() string { return filepath.Join(filepath.Dir(cfgPath), "watch_history.json") }

func loadHistory() {
	data, err := os.ReadFile(historyPath())
	if err != nil {
		return
	}
	historyMu.Lock()
	defer historyMu.Unlock()
	json.Unmarshal(data, &historyCache)
}

func saveHistory() {
	historyMu.Lock()
	data, err := json.Marshal(historyCache)
	historyMu.Unlock()
	if err != nil {
		return
	}
	os.WriteFile(historyPath(), data, 0644)
}

// watchKey identifies a piece of content independently of the file holding it.
func watchKey(libKey string) (string, videoMeta) {
	vm, ok := getLocalMeta(libKey)
	if ok && vm.TMDBID > 0 {
		if vm.MediaType == "tv" && vm.Season > 0 && vm.Episode > 0 {
			return fmt.Sprintf("tmdb:tv:%d:s%02de%02d", vm.TMDBID, vm.Season, vm.Episode), vm
		}
		mt := vm.MediaType
		if mt == "" {
			mt = "movie"
		}
		return fmt.Sprintf("tmdb:%s:%d", mt, vm.TMDBID), vm
	}
	// No TMDB match: normalise the filename, which already strips resolution,
	// codec and release-group noise, so two rips of the same thing agree.
	stem := strings.TrimSuffix(filepath.Base(libKey), filepath.Ext(libKey))
	n := normalizeTitle(stem)
	if n == "" {
		n = strings.ToLower(stem)
	}
	return "name:" + n, vm
}

// recordWatch updates the durable history for a position report.
func recordWatch(libKey string, pos, dur float64, completed bool) {
	wk, vm := watchKey(libKey)
	historyMu.Lock()
	rec := historyCache[wk]
	if rec.Plays == 0 {
		rec.Plays = 1
	} else if completed && !rec.Completed {
		rec.Plays++
	}
	rec.Key = libKey
	if vm.Title != "" {
		rec.Title = vm.Title
	} else if rec.Title == "" {
		rec.Title = strings.TrimSuffix(filepath.Base(libKey), filepath.Ext(libKey))
	}
	rec.MediaType, rec.Season, rec.Episode = vm.MediaType, vm.Season, vm.Episode
	rec.Position, rec.Duration = pos, dur
	rec.Updated = time.Now().Unix()
	if completed {
		rec.Completed = true
		rec.Position = 0
	}
	historyCache[wk] = rec
	historyMu.Unlock()
	saveHistory()
}

// resumeFromHistory supplies a position for a file the progress cache has
// never seen — the case where the same content was re-downloaded elsewhere.
func resumeFromHistory(libKey string) (playbackProgress, bool) {
	wk, _ := watchKey(libKey)
	historyMu.Lock()
	rec, ok := historyCache[wk]
	historyMu.Unlock()
	if !ok || rec.Completed || rec.Position <= 5 || rec.Duration <= 0 {
		return playbackProgress{}, false
	}
	return playbackProgress{Position: rec.Position, Duration: rec.Duration, Updated: rec.Updated}, true
}

// seedHistoryFromProgress backfills history for positions saved before the
// history existed, so the things you are part-way through right now are
// carried across rather than starting empty.
func seedHistoryFromProgress() {
	progressMu.Lock()
	snapshot := make(map[string]playbackProgress, len(progressCache))
	for k, v := range progressCache {
		snapshot[k] = v
	}
	progressMu.Unlock()

	added := 0
	for key, p := range snapshot {
		wk, _ := watchKey(key)
		historyMu.Lock()
		_, exists := historyCache[wk]
		historyMu.Unlock()
		if exists {
			continue
		}
		recordWatch(key, p.Position, p.Duration, false)
		added++
	}
	if added > 0 {
		logger.Printf("watch history: seeded %d entr(ies) from saved positions", added)
	}
}

func (c *serverCtx) handleHistory(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	historyMu.Lock()
	out := make([]watchRecord, 0, len(historyCache))
	for _, rec := range historyCache {
		out = append(out, rec)
	}
	historyMu.Unlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Updated > out[j].Updated })
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(out)
}

// ─── Playlists ────────────────────────────────────────────────────────────────
//
// An ordered list of library keys with a name, persisted beside the metadata
// and progress caches. Items may be anything on the wall — a film, an episode,
// a music video — because playback just walks the list.

type playlist struct {
	ID      string   `json:"id"`
	Name    string   `json:"name"`
	Items   []string `json:"items"`
	Cover   string   `json:"cover,omitempty"` // uploaded cover filename, else composed
	Created int64    `json:"created"`
	Updated int64    `json:"updated"`
}

var (
	playlistMu    sync.Mutex
	playlistCache = map[string]*playlist{}
)

func playlistPath() string { return filepath.Join(filepath.Dir(cfgPath), "playlists.json") }

func playlistCoverDir() string {
	d := filepath.Join(filepath.Dir(cfgPath), "playlist_covers")
	os.MkdirAll(d, 0755)
	return d
}

func loadPlaylists() {
	data, err := os.ReadFile(playlistPath())
	if err != nil {
		return
	}
	playlistMu.Lock()
	defer playlistMu.Unlock()
	json.Unmarshal(data, &playlistCache)
}

func savePlaylists() {
	playlistMu.Lock()
	data, err := json.MarshalIndent(playlistCache, "", "  ")
	playlistMu.Unlock()
	if err != nil {
		return
	}
	os.WriteFile(playlistPath(), data, 0644)
}

func newPlaylistID() string {
	b := make([]byte, 8)
	rand.Read(b)
	return hex.EncodeToString(b)
}

func (c *serverCtx) handlePlaylists(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch r.Method {
	case http.MethodGet:
		playlistMu.Lock()
		out := make([]*playlist, 0, len(playlistCache))
		for _, pl := range playlistCache {
			out = append(out, pl)
		}
		playlistMu.Unlock()
		sort.Slice(out, func(i, j int) bool { return out[i].Created < out[j].Created })
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(out)

	case http.MethodPost:
		var body struct {
			Action string   `json:"action"`
			ID     string   `json:"id"`
			Name   string   `json:"name"`
			Key    string   `json:"key"`
			Items  []string `json:"items"`
		}
		if json.NewDecoder(r.Body).Decode(&body) != nil {
			http.Error(w, "bad request", 400)
			return
		}
		now := time.Now().Unix()
		playlistMu.Lock()
		var result *playlist
		switch body.Action {
		case "create":
			name := strings.TrimSpace(body.Name)
			if name == "" {
				name = "Untitled playlist"
			}
			pl := &playlist{ID: newPlaylistID(), Name: name, Created: now, Updated: now}
			if body.Key != "" {
				pl.Items = []string{body.Key}
			}
			playlistCache[pl.ID] = pl
			result = pl
		case "add":
			if pl := playlistCache[body.ID]; pl != nil && body.Key != "" {
				dup := false
				for _, k := range pl.Items {
					if k == body.Key {
						dup = true
						break
					}
				}
				if !dup {
					pl.Items = append(pl.Items, body.Key)
					pl.Updated = now
				}
				result = pl
			}
		case "remove":
			if pl := playlistCache[body.ID]; pl != nil {
				kept := pl.Items[:0]
				for _, k := range pl.Items {
					if k != body.Key {
						kept = append(kept, k)
					}
				}
				pl.Items = kept
				pl.Updated = now
				result = pl
			}
		case "rename":
			if pl := playlistCache[body.ID]; pl != nil && strings.TrimSpace(body.Name) != "" {
				pl.Name = strings.TrimSpace(body.Name)
				pl.Updated = now
				result = pl
			}
		case "reorder":
			if pl := playlistCache[body.ID]; pl != nil && body.Items != nil {
				pl.Items = body.Items
				pl.Updated = now
				result = pl
			}
		case "delete":
			if pl := playlistCache[body.ID]; pl != nil {
				if pl.Cover != "" {
					os.Remove(filepath.Join(playlistCoverDir(), pl.Cover))
				}
				delete(playlistCache, body.ID)
			}
		default:
			playlistMu.Unlock()
			http.Error(w, "unknown action", 400)
			return
		}
		playlistMu.Unlock()
		savePlaylists()
		w.Header().Set("Content-Type", "application/json")
		if result != nil {
			json.NewEncoder(w).Encode(result)
		} else {
			fmt.Fprint(w, "{}")
		}

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// scaleInto does a nearest-neighbour draw of src into the given rect. A cover
// tile is 320x180 and never looked at closely, so this is enough and avoids
// pulling in an image-resizing dependency.
func scaleInto(dst *image.RGBA, r image.Rectangle, src image.Image) {
	sb := src.Bounds()
	dw, dh := r.Dx(), r.Dy()
	if dw <= 0 || dh <= 0 || sb.Dx() == 0 || sb.Dy() == 0 {
		return
	}
	// Cover rather than fit: crop the long side so tiles are not letterboxed.
	scale := float64(dw) / float64(sb.Dx())
	if s := float64(dh) / float64(sb.Dy()); s > scale {
		scale = s
	}
	cw, ch := int(float64(dw)/scale), int(float64(dh)/scale)
	ox, oy := sb.Min.X+(sb.Dx()-cw)/2, sb.Min.Y+(sb.Dy()-ch)/2
	for y := 0; y < dh; y++ {
		sy := oy + y*ch/dh
		for x := 0; x < dw; x++ {
			sx := ox + x*cw/dw
			dst.Set(r.Min.X+x, r.Min.Y+y, src.At(sx, sy))
		}
	}
}

// composePlaylistCover builds the 2x2 grid of the first four items' stills.
// It is regenerated whenever the membership changes rather than cached
// forever, since the cover is meant to describe what is currently in the list.
func (c *serverCtx) composePlaylistCover(pl *playlist) ([]byte, error) {
	const w, h = 640, 360
	canvas := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{color.RGBA{20, 19, 15, 255}}, image.Point{}, draw.Src)

	// Gather what artwork actually exists first. A playlist whose files have
	// not had a frame pulled yet would otherwise compose to a grid of holes.
	var art []image.Image
	for _, key := range pl.Items {
		if len(art) >= 4 {
			break
		}
		abs, ok := c.resolveLocalPath(key)
		if !ok {
			continue
		}
		tp := localThumbnailPath(abs)
		if tp == "" {
			continue
		}
		f, err := os.Open(tp)
		if err != nil {
			continue
		}
		img, _, derr := image.Decode(f)
		f.Close()
		if derr == nil {
			art = append(art, img)
		}
	}
	if len(art) == 0 {
		return nil, fmt.Errorf("no artwork yet")
	}

	// The layout follows the count, so the frame is always fully covered:
	// one fills it, two split it, three give the first the left half, four
	// make the quarters.
	var cells []image.Rectangle
	switch len(art) {
	case 1:
		cells = []image.Rectangle{image.Rect(0, 0, w, h)}
	case 2:
		cells = []image.Rectangle{image.Rect(0, 0, w/2, h), image.Rect(w/2, 0, w, h)}
	case 3:
		cells = []image.Rectangle{
			image.Rect(0, 0, w/2, h),
			image.Rect(w/2, 0, w, h/2),
			image.Rect(w/2, h/2, w, h),
		}
	default:
		cells = []image.Rectangle{
			image.Rect(0, 0, w/2, h/2), image.Rect(w/2, 0, w, h/2),
			image.Rect(0, h/2, w/2, h), image.Rect(w/2, h/2, w, h),
		}
	}
	for i, img := range art {
		scaleInto(canvas, cells[i], img)
	}
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, canvas, &jpeg.Options{Quality: 82}); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func (c *serverCtx) handlePlaylistCover(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	id := r.URL.Query().Get("id")
	playlistMu.Lock()
	pl := playlistCache[id]
	playlistMu.Unlock()
	if pl == nil {
		http.Error(w, "not found", 404)
		return
	}

	if r.Method == http.MethodPost {
		// An uploaded cover replaces the composed one.
		r.Body = http.MaxBytesReader(w, r.Body, 8<<20)
		data, err := io.ReadAll(r.Body)
		if err != nil || len(data) == 0 {
			http.Error(w, "bad upload", 400)
			return
		}
		if _, _, derr := image.Decode(bytes.NewReader(data)); derr != nil {
			http.Error(w, "not an image", 400)
			return
		}
		name := pl.ID + ".jpg"
		img, _, _ := image.Decode(bytes.NewReader(data))
		out := image.NewRGBA(image.Rect(0, 0, 640, 360))
		draw.Draw(out, out.Bounds(), &image.Uniform{color.RGBA{20, 19, 15, 255}}, image.Point{}, draw.Src)
		scaleInto(out, out.Bounds(), img)
		var buf bytes.Buffer
		if err := jpeg.Encode(&buf, out, &jpeg.Options{Quality: 86}); err != nil {
			http.Error(w, "encode failed", 500)
			return
		}
		if err := os.WriteFile(filepath.Join(playlistCoverDir(), name), buf.Bytes(), 0644); err != nil {
			http.Error(w, "write failed", 500)
			return
		}
		playlistMu.Lock()
		pl.Cover = name
		pl.Updated = time.Now().Unix()
		playlistMu.Unlock()
		savePlaylists()
		w.WriteHeader(200)
		fmt.Fprint(w, "ok")
		return
	}

	if pl.Cover != "" {
		fp := filepath.Join(playlistCoverDir(), pl.Cover)
		if fileExists(fp) {
			w.Header().Set("Content-Type", "image/jpeg")
			http.ServeFile(w, r, fp)
			return
		}
	}
	data, err := c.composePlaylistCover(pl)
	if err != nil {
		http.Error(w, "no cover", 404)
		return
	}
	w.Header().Set("Content-Type", "image/jpeg")
	w.Header().Set("Cache-Control", "no-cache")
	w.Write(data)
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
	// Enriched reports that enrichment has already run for this file, whether
	// or not it found anything. The library polls on this rather than on the
	// presence of a title, so a permanently unmatched file stops the poll.
	Enriched bool `json:"enriched,omitempty"`
	Artist          string   `json:"artist,omitempty"`
	Album           string   `json:"album,omitempty"`
	IsExtra         bool            `json:"is_extra,omitempty"`
	MainKey         string          `json:"main_key,omitempty"`
	AudioLang       string          `json:"audio_lang,omitempty"`
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
				entry.AudioLang = vm.AudioLang
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
	// Downloading is true while this file's size is still changing between
	// scans — a torrent still writing straight into a library folder, most
	// likely. See isFileStable: nothing should enrich, audio-check, or
	// thumbnail a file that hasn't finished arriving yet.
	Downloading bool
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

// markShowFeaturettes catches the one shape markExtras' by-hand-only rule
// leaves stranded: a season's bonus material, sitting in a folder literally
// named "Featurettes" or "Extras" alongside that season's own episode files.
// Unlike a movie's extras (which can share an ambiguous folder with a wholly
// unrelated standalone film, the false-positive case markExtras' comment
// describes), this signal isn't a guess about content — it's the release's
// own folder naming, checked against a season that TMDB has already, and
// separately, matched. A featurette's own filename ("Hoyt.mkv") never carries
// enough to identify its show by itself, so the show comes from a sibling
// episode file already sitting in the same season folder.
//
// Only fills in videos not already marked an extra by something else (hand or
// otherwise) — never overrides an existing classification, hand-set or not.
func markShowFeaturettes(videos []localVideo) {
	type showSeason struct {
		TMDBID int
		Season int
	}
	seasonShow := map[string]showSeason{}
	for _, v := range videos {
		vm, ok := getLocalMeta(v.Rel)
		if !ok || vm.MediaType != "tv" || vm.TMDBID <= 0 {
			continue
		}
		parts := strings.Split(v.Rel, "/")
		if len(parts) < 2 {
			continue
		}
		dir := strings.Join(parts[:len(parts)-1], "/")
		seasonShow[dir] = showSeason{TMDBID: vm.TMDBID, Season: vm.Season}
	}
	if len(seasonShow) == 0 {
		return
	}
	for i, v := range videos {
		if videos[i].IsExtra {
			continue
		}
		anchor, ok := featuretteAnchorDir(v.Rel)
		if !ok {
			continue
		}
		ss, ok := seasonShow[anchor]
		if !ok {
			continue
		}
		videos[i].IsExtra = true
		// The season number is folded into MainKey rather than needing its
		// own field, so an extra sorts under the same season tab as the
		// episodes it shipped alongside — see renderShowView.
		videos[i].MainRel = fmt.Sprintf("show:%d:s%d", ss.TMDBID, ss.Season)
	}
}

// featuretteAnchorDir walks a file's path looking for an ancestor folder
// named for exactly what isUnidentifiable already trusts as bonus material
// (see extrasFolders) and, if found, returns the directory that folder
// itself sits in — the season folder its bonus material belongs to.
func featuretteAnchorDir(rel string) (string, bool) {
	parts := strings.Split(rel, "/")
	for i := 0; i < len(parts)-1; i++ {
		seg := strings.ToLower(parts[i])
		for _, x := range extrasFolders {
			if seg == x {
				return strings.Join(parts[:i], "/"), true
			}
		}
	}
	return "", false
}

// fileStability tracks, per file, the size last seen for it and how long
// that size has held. A torrent client can write straight into a library
// folder (rather than a separate watch folder that only hands a file over
// once complete), so a file mid-download looks, at a glance, exactly like a
// corrupt one: ffprobe fails on it, its header can be zero-filled, its size
// keeps changing. isFileStable is what tells the two apart.
//
// Size alone isn't enough, though: a client with disk pre-allocation on
// (qBittorrent's default) writes the file at its full final size the moment
// the download starts, so the size never changes at all — only which bytes
// have actually arrived does. HeaderOK covers that case by confirming the
// file's opening bytes are real data, not the zero-fill a not-yet-written
// region reads back as; checked once and remembered; a header, once real,
// stays real.
var (
	fileStabilityMu sync.Mutex
	fileStability   = map[string]fileStabilityState{}
)

type fileStabilityState struct {
	Size        int64
	StableSince time.Time
	HeaderOK    bool
}

// fileStabilityWindow is how long a file's size must hold steady before it's
// worth even checking whether it's finished.
const fileStabilityWindow = 20 * time.Second

// isFileStable costs a map lookup on every call; the actual file read behind
// HeaderOK happens at most once per file, the first time its size settles.
func isFileStable(rel string, size int64, abs string) bool {
	now := time.Now()
	fileStabilityMu.Lock()
	defer fileStabilityMu.Unlock()
	st, ok := fileStability[rel]
	if !ok || st.Size != size {
		fileStability[rel] = fileStabilityState{Size: size, StableSince: now}
		return false
	}
	if now.Sub(st.StableSince) < fileStabilityWindow {
		return false
	}
	if !st.HeaderOK {
		if looksIncomplete(abs, size) {
			return false
		}
		st.HeaderOK = true
		fileStability[rel] = st
	}
	return true
}

// looksIncomplete reports whether a file's opening bytes are all zero — the
// telltale of a pre-allocated region a torrent client hasn't written yet. A
// real video file's container header is never a run of zero bytes at
// offset 0, so this is a cheap, low-noise second signal a sparse-allocated
// download's constant file size can't provide on its own.
func looksIncomplete(path string, size int64) bool {
	if size == 0 {
		return true
	}
	f, err := os.Open(path)
	if err != nil {
		return true
	}
	defer f.Close()
	buf := make([]byte, 4096)
	n, _ := io.ReadFull(f, buf)
	if n == 0 {
		return true
	}
	for _, b := range buf[:n] {
		if b != 0 {
			return false
		}
	}
	return true
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
			relSlash := filepath.ToSlash(rel)
			out = append(out, localVideo{Abs: path, Rel: relSlash, Type: ftype, Size: size, Downloading: !isFileStable(relSlash, size, path)})
			return nil
		})
	}
	markExtras(out)
	markShowFeaturettes(out)
	return out
}


// ─── Audio / subtitle language enforcement ────────────────────────────────────
//
// A track's language tag alone does not decide what plays: a release with no
// disposition flags set, or with the wrong one set, hands playback to whatever
// a given player falls back to -- which in practice can mean a disc's Italian
// dub, a director's commentary, or a foreign film's original-language track
// ends up as the one that actually plays, silently. This probes every local
// video once, and where an English audio track exists but is not the one that
// would play, remuxes the container (stream copy -- no re-encode, nothing
// dropped) so it explicitly is. A file with no English track at all is left
// alone; there is nothing to force it to.
//
// audioCheckVersion gates this the same way localMetaVersion gates TMDB
// enrichment -- bump it to send every file through the check again.
//
// v2: a lot of real playback hardware (TV browsers especially -- confirmed
// on a Hisense set) never consults the container's default-disposition flag
// at all and just plays audio/subtitle stream 0 of each type, full stop. v1
// only set the flag; v2 additionally reorders the streams themselves so the
// preferred track physically is stream 0, which is what actually changes
// what such a device plays.
const audioCheckVersion = 2

var languageNames = map[string]string{
	"eng": "English", "en": "English",
	"ita": "Italian", "it": "Italian",
	"jpn": "Japanese", "ja": "Japanese",
	"spa": "Spanish", "es": "Spanish",
	"fre": "French", "fra": "French", "fr": "French",
	"ger": "German", "deu": "German", "de": "German",
	"por": "Portuguese", "pt": "Portuguese",
	"kor": "Korean", "ko": "Korean",
	"chi": "Chinese", "zho": "Chinese", "zh": "Chinese",
	"hin": "Hindi", "hi": "Hindi",
	"rus": "Russian", "ru": "Russian",
	"ara": "Arabic", "ar": "Arabic",
	"nld": "Dutch", "dut": "Dutch", "nl": "Dutch",
	"swe": "Swedish", "sv": "Swedish",
	"pol": "Polish", "pl": "Polish",
	"tur": "Turkish", "tr": "Turkish",
	"und": "", "": "",
}

func languageName(code string) string {
	code = strings.ToLower(strings.TrimSpace(code))
	if n, ok := languageNames[code]; ok {
		return n
	}
	if code == "" {
		return ""
	}
	return strings.ToUpper(code[:1]) + code[1:]
}

type mediaStream struct {
	Index   int
	Type    string // "audio" | "subtitle"
	Lang    string
	Title   string
	Default bool
}

func probeAudioSubtitleStreams(path string) ([]mediaStream, error) {
	out, err := exec.Command(ffprobeBin(), "-v", "error", "-print_format", "json",
		"-show_entries", "stream=index,codec_type:stream_tags=language,title:stream_disposition=default",
		path).Output()
	if err != nil {
		return nil, err
	}
	var result struct {
		Streams []struct {
			Index     int    `json:"index"`
			CodecType string `json:"codec_type"`
			Tags      struct {
				Language string `json:"language"`
				Title    string `json:"title"`
			} `json:"tags"`
			Disposition struct {
				Default int `json:"default"`
			} `json:"disposition"`
		} `json:"streams"`
	}
	if err := json.Unmarshal(out, &result); err != nil {
		return nil, err
	}
	var streams []mediaStream
	for _, s := range result.Streams {
		if s.CodecType != "audio" && s.CodecType != "subtitle" {
			continue
		}
		streams = append(streams, mediaStream{
			Index: s.Index, Type: s.CodecType,
			Lang: strings.ToLower(s.Tags.Language), Title: s.Tags.Title,
			Default: s.Disposition.Default == 1,
		})
	}
	return streams, nil
}

var audioFixSem = make(chan struct{}, 1)

// ensureAudioDefault is the decision plus fix for one file. preferLang is the
// audio language to promote — "" means English, and checkAudioTrack passes a
// specific code (e.g. "ja") for a title whose TMDB original_language says it
// should keep its native track rather than an English dub. The subtitle
// choice is not affected by preferLang: an English subtitle is always the
// target there, independent of which audio language is preferred, so a
// Japanese-original film still gets English subs by default.
//
// It returns the audio language that will actually play once it returns,
// whether a remux was needed to get there, and any error the probe or the
// remux hit.
func ensureAudioDefault(path string, preferLang string) (lang string, fixed bool, err error) {
	streams, perr := probeAudioSubtitleStreams(path)
	if perr != nil {
		return "", false, perr
	}
	var audios, subs []mediaStream
	for _, s := range streams {
		if s.Type == "audio" {
			audios = append(audios, s)
		} else {
			subs = append(subs, s)
		}
	}
	if len(audios) == 0 {
		return "", false, nil
	}

	isCommentary := func(s mediaStream) bool {
		return strings.Contains(strings.ToLower(s.Title), "commentary")
	}
	isEnglish := func(s mediaStream) bool { return s.Lang == "eng" || s.Lang == "en" }
	// Matching by resolved display name rather than raw code absorbs the
	// 2-letter/3-letter mismatch between TMDB's original_language ("ja") and
	// ffprobe's stream tags ("jpn") for free.
	isPreferred := func(s mediaStream) bool {
		if preferLang == "" {
			return isEnglish(s)
		}
		return languageName(s.Lang) == languageName(preferLang)
	}

	// The track a player lands on with nothing forced: whichever carries the
	// default flag, or the first audio stream in the file if none does.
	effective := audios[0]
	for _, a := range audios {
		if a.Default {
			effective = a
			break
		}
	}

	// The preferred-language track to promote, favouring a non-commentary
	// one; a commentary track is still the only match on offer if that is
	// all there is.
	var preferred *mediaStream
	for i := range audios {
		if isPreferred(audios[i]) && !isCommentary(audios[i]) {
			preferred = &audios[i]
			break
		}
	}
	if preferred == nil {
		for i := range audios {
			if isPreferred(audios[i]) {
				preferred = &audios[i]
				break
			}
		}
	}
	// A Japanese-original title with no Japanese track on this particular
	// rip (an English-only release, say) has nothing to prefer it to —
	// fall back to the ordinary English-default behaviour instead of
	// leaving it on whatever happens to be effective.
	if preferred == nil && preferLang != "" {
		for i := range audios {
			if isEnglish(audios[i]) && !isCommentary(audios[i]) {
				preferred = &audios[i]
				break
			}
		}
		if preferred == nil {
			for i := range audios {
				if isEnglish(audios[i]) {
					preferred = &audios[i]
					break
				}
			}
		}
	}
	if preferred == nil {
		return languageName(effective.Lang), false, nil
	}

	defaultCount := 0
	for _, a := range audios {
		if a.Default {
			defaultCount++
		}
	}

	var englishSub *mediaStream
	for i := range subs {
		if isEnglish(subs[i]) {
			englishSub = &subs[i]
			break
		}
	}
	subFixNeeded := false
	if englishSub != nil {
		subDefaults := 0
		for _, s := range subs {
			if s.Default {
				subDefaults++
			}
		}
		// subs[0] is the physically first subtitle stream -- see the note by
		// audioFixNeeded below on why the flag alone isn't enough.
		subFixNeeded = !englishSub.Default || subDefaults != 1 || subs[0].Index != englishSub.Index
	}

	// audios[0] is the physically first audio stream in the container. A lot
	// of real playback hardware never looks at the default flag and just
	// plays that one, so "already fixed" has to mean the preferred track
	// *is* audios[0], not merely that it is flagged as default.
	audioFixNeeded := effective.Index != preferred.Index || defaultCount != 1 || audios[0].Index != preferred.Index
	if !audioFixNeeded && !subFixNeeded {
		return languageName(preferred.Lang), false, nil
	}

	audioFixSem <- struct{}{}
	rerr := remuxWithDefaults(path, audios, preferred.Index, subs, englishSub)
	<-audioFixSem
	if rerr != nil {
		return languageName(effective.Lang), false, rerr
	}
	return languageName(preferred.Lang), true, nil
}

// errFileInUse marks a remux failure that's just someone actively watching
// the file right now (Windows won't let the swap's rename-aside touch an
// open file) -- transient, and gone the moment playback stops, unlike a
// genuine codec/stream-layout incompatibility. checkAudioTrack treats it
// like a file still downloading: retried next pass, no attempt spent.
var errFileInUse = errors.New("file is open elsewhere")

// isSharingViolation reports whether err is Windows' ERROR_SHARING_VIOLATION
// (0x20) -- another process has the file open without allowing this access.
func isSharingViolation(err error) bool {
	var errno syscall.Errno
	return errors.As(err, &errno) && errno == 0x20
}

// remuxWithDefaults rewrites path so the chosen audio and subtitle streams
// are both flagged default AND physically reordered to stream 0 of their
// type -- via a stream-copy remux (-c copy), so every track is kept and none
// is re-encoded; this only ever touches container structure/metadata. The
// flag alone is not enough: plenty of real playback hardware (TV browsers in
// particular) ignores the container's default disposition entirely and just
// plays audio/subtitle stream 0, so that is the thing that actually has to
// move. The swap is staged through a temp file and a rename-aside of the
// original, so a failure at any point leaves the original file exactly as it
// was.
func remuxWithDefaults(path string, audios []mediaStream, preferredAudioIdx int, subs []mediaStream, englishSub *mediaStream) error {
	dir := filepath.Dir(path)
	tmp := filepath.Join(dir, "."+hex.EncodeToString(sha256Sum(path))[:12]+".audiofix"+filepath.Ext(path))
	os.Remove(tmp)

	orderedAudioIdx := []int{preferredAudioIdx}
	for _, a := range audios {
		if a.Index != preferredAudioIdx {
			orderedAudioIdx = append(orderedAudioIdx, a.Index)
		}
	}
	var orderedSubIdx []int
	if englishSub != nil {
		orderedSubIdx = append(orderedSubIdx, englishSub.Index)
	}
	for _, s := range subs {
		if englishSub == nil || s.Index != englishSub.Index {
			orderedSubIdx = append(orderedSubIdx, s.Index)
		}
	}

	args := []string{"-y", "-i", path, "-map", "0:v"}
	for _, idx := range orderedAudioIdx {
		args = append(args, "-map", fmt.Sprintf("0:%d", idx))
	}
	for _, idx := range orderedSubIdx {
		args = append(args, "-map", fmt.Sprintf("0:%d", idx))
	}
	args = append(args, "-map", "0:t?", "-c", "copy")
	for i := range orderedAudioIdx {
		val := "0"
		if i == 0 {
			val = "default"
		}
		args = append(args, fmt.Sprintf("-disposition:a:%d", i), val)
	}
	for i := range orderedSubIdx {
		val := "0"
		if i == 0 {
			val = "default"
		}
		args = append(args, fmt.Sprintf("-disposition:s:%d", i), val)
	}
	args = append(args, tmp)

	out, err := exec.Command(ffmpegBin(), args...).CombinedOutput()
	if err != nil {
		os.Remove(tmp)
		return fmt.Errorf("ffmpeg: %v: %s", err, truncateForLog(out))
	}

	origInfo, oerr := os.Stat(path)
	newInfo, nerr := os.Stat(tmp)
	if oerr != nil || nerr != nil || newInfo.Size() < origInfo.Size()/2 {
		os.Remove(tmp)
		return fmt.Errorf("remux output failed the size sanity check")
	}
	if _, isVideo, _ := probeVideo(tmp); !isVideo {
		os.Remove(tmp)
		return fmt.Errorf("remux output does not probe as a valid video")
	}

	backup := path + ".preaudiofix.bak"
	os.Remove(backup)
	if err := os.Rename(path, backup); err != nil {
		os.Remove(tmp)
		if isSharingViolation(err) {
			return fmt.Errorf("%w: %v", errFileInUse, err)
		}
		return fmt.Errorf("could not stage original aside: %v", err)
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Rename(backup, path) // put it back rather than leave the entry missing
		os.Remove(tmp)
		if isSharingViolation(err) {
			return fmt.Errorf("%w: %v", errFileInUse, err)
		}
		return fmt.Errorf("could not move remuxed file into place: %v", err)
	}
	os.Remove(backup)
	return nil
}

// checkAudioTrack is the per-file entry point the enrichment loop calls. It
// is independent of TMDB matching -- a file's own audio tracks are a property
// of the file, not of whatever title it resolves to -- so it runs for every
// local video, matched or not, gated only on AudioCheckVer.
func checkAudioTrack(v localVideo) {
	vm, had := getLocalMeta(v.Rel)
	if had && vm.AudioCheckVer >= audioCheckVersion {
		return
	}
	// A Japanese-original film or show (anime included) keeps its Japanese
	// track as the default rather than being forced to an English dub —
	// everything else still defaults to English. English subtitles are the
	// target either way; see ensureAudioDefault.
	preferLang := ""
	if vm.OriginalLang == "ja" {
		preferLang = "ja"
	}
	lang, fixed, err := ensureAudioDefault(v.Abs, preferLang)
	if errors.Is(err, errFileInUse) {
		// Someone's watching it right now -- try again next pass, same as a
		// file still downloading, rather than spending one of its limited
		// attempts on a failure that has nothing to do with the file itself.
		logger.Printf("audio check %s: %v (retrying later)", v.Rel, err)
		return
	}
	localMetaMu.Lock()
	cur := localMetaCache[v.Rel]
	if err != nil {
		cur.AudioCheckTries++
		if cur.AudioCheckTries >= maxAudioCheckTries {
			// Given up: this isn't a file that's still arriving (those never
			// reach here at all — see the Downloading guard in
			// startLocalEnrichment) so a failure this persistent is a real,
			// probably permanent one. Recording it as done stops the noise;
			// it still shows up in the log every time, so it isn't silent.
			cur.AudioCheckVer = audioCheckVersion
			logger.Printf("audio check %s: %v (giving up after %d attempts)", v.Rel, err, cur.AudioCheckTries)
		} else {
			logger.Printf("audio check %s: %v (attempt %d of %d)", v.Rel, err, cur.AudioCheckTries, maxAudioCheckTries)
		}
	} else {
		cur.AudioLang = lang
		cur.AudioCheckVer = audioCheckVersion
		cur.AudioCheckTries = 0
		if fixed {
			logger.Printf("audio: set %s default for %s", lang, v.Rel)
		}
	}
	localMetaCache[v.Rel] = cur
	localMetaMu.Unlock()
	saveLocalMetaCache()
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

// ─── Playback progress (resume across devices) ─────────────────────────────
//
// A single shared position per file, keyed the same way the rest of the app
// already identifies a file (its R2 key for cloud items, its root-relative
// path for local ones — the `key` used throughout openFile() client-side).
// Persisted to a JSON file next to config.json, same pattern as
// local_meta.json, so any device hitting this server resumes the same spot.

type playbackProgress struct {
	Position float64 `json:"position"`
	Duration float64 `json:"duration"`
	Updated  int64   `json:"updated"`
}

var (
	progressMu    sync.Mutex
	progressCache = map[string]playbackProgress{}
)

func progressPath() string {
	return filepath.Join(filepath.Dir(cfgPath), "playback_progress.json")
}

func loadProgressCache() {
	data, err := os.ReadFile(progressPath())
	if err != nil {
		return
	}
	progressMu.Lock()
	defer progressMu.Unlock()
	json.Unmarshal(data, &progressCache)
}

func saveProgressCache() {
	progressMu.Lock()
	data, err := json.Marshal(progressCache)
	progressMu.Unlock()
	if err != nil {
		return
	}
	os.WriteFile(progressPath(), data, 0644)
}

// handleProgress lets any authenticated device read or update the saved
// position for a file. A position within the first 5s, the last 30s, or past
// 95% counts as "nothing to resume" and is evicted, so a finished episode
// never reopens at the end.
func (c *serverCtx) handleProgress(w http.ResponseWriter, r *http.Request) {
	if !c.isAuthed(r) {
		http.Error(w, "unauthorized", 401)
		return
	}
	switch r.Method {
	case http.MethodGet:
		// The library view needs every saved position at once to build its
		// continue-watching row; asking per key would be one request per tile.
		if r.URL.Query().Get("all") == "1" {
			progressMu.Lock()
			all := make(map[string]playbackProgress, len(progressCache))
			for k, v := range progressCache {
				all[k] = v
			}
			progressMu.Unlock()
			// Fold in positions held only in history, so a re-downloaded file
			// shows its bar and its place in continue-watching immediately
			// rather than after it is played once.
			for _, v := range c.scanLocalVideos() {
				if _, seen := all[v.Rel]; seen {
					continue
				}
				if p, ok := resumeFromHistory(v.Rel); ok {
					all[v.Rel] = p
				}
			}
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(all)
			return
		}
		key := r.URL.Query().Get("key")
		if key == "" {
			http.Error(w, "missing key", 400)
			return
		}
		progressMu.Lock()
		p, ok := progressCache[key]
		progressMu.Unlock()
		if !ok {
			// Never seen this path, but the same content may have been
			// watched as a different file before it was replaced.
			p, ok = resumeFromHistory(key)
		}
		w.Header().Set("Content-Type", "application/json")
		if !ok {
			w.Write([]byte("{}"))
			return
		}
		json.NewEncoder(w).Encode(p)

	case http.MethodPost:
		var body struct {
			Key      string  `json:"key"`
			Position float64 `json:"position"`
			Duration float64 `json:"duration"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Key == "" {
			http.Error(w, "bad request", 400)
			return
		}
		finished := body.Duration > 0 && (body.Position/body.Duration > 0.95 || body.Duration-body.Position < 30)
		progressMu.Lock()
		if finished || body.Position < 5 {
			delete(progressCache, body.Key)
		} else {
			progressCache[body.Key] = playbackProgress{Position: body.Position, Duration: body.Duration, Updated: time.Now().Unix()}
		}
		progressMu.Unlock()
		saveProgressCache()
		// The history keeps the finished ones too — that is the difference
		// between "what am I part-way through" and "what have I watched".
		if body.Duration > 0 && (body.Position > 5 || finished) {
			recordWatch(body.Key, body.Position, body.Duration, finished)
		}
		w.WriteHeader(200)

	default:
		http.Error(w, "method not allowed", 405)
	}
}

// backfillLocalMeta refreshes the TMDB-owned detail fields on cache entries
// written by an older build — director, cast, country, language, collection,
// and runtime, none of which existed when most of this cache was populated.
//
// It is deliberately additive. Several entries have had their title, year or
// TMDB id corrected by hand after the LLM parse got them wrong (and is_extra
// is only ever set by hand — see markExtras), so re-running enrichment from
// the filename would silently undo that work. Instead this refetches against
// the id the entry already holds and replaces nothing that identifies the file.
//
// Lookups are keyed by (tmdb id, media type), so a 117-episode show costs one
// pair of requests rather than 117.
func (c *serverCtx) backfillLocalMeta() {
	if c.tmdbKey == "" {
		return
	}
	type job struct {
		rel     string
		id      int
		mt      string
		season  int
		episode int
	}
	var jobs []job
	localMetaMu.Lock()
	for rel, vm := range localMetaCache {
		// Music videos are identified from their own filenames and carry no
		// TMDB id, so there is nothing here to refresh them against.
		if vm.MetaVersion >= localMetaVersion || vm.TMDBID <= 0 || vm.MediaType == "music_video" {
			continue
		}
		jobs = append(jobs, job{rel, vm.TMDBID, vm.MediaType, vm.Season, vm.Episode})
	}
	localMetaMu.Unlock()
	if len(jobs) == 0 {
		return
	}

	type dkey struct {
		id int
		mt string
	}
	cache := map[dkey]tmdbDetail{}
	logger.Printf("local library: backfilling detail metadata for %d entry(s)", len(jobs))
	for _, j := range jobs {
		k := dkey{j.id, j.mt}
		det, ok := cache[k]
		if !ok {
			det = c.fetchTMDBDetail(j.id, j.mt)
			cache[k] = det
		}
		// A show's per-episode runtime beats its series average, and for many
		// shows the average does not exist at all.
		epRuntime := 0
		if j.mt == "tv" && j.season > 0 && j.episode > 0 {
			epRuntime = c.fetchEpisodeRuntime(j.id, j.season, j.episode)
		}

		localMetaMu.Lock()
		vm, still := localMetaCache[j.rel]
		if still {
			if len(det.Genres) > 0 {
				vm.Genres = det.Genres
			}
			if det.Backdrop != "" {
				vm.TMDBBackdrop = det.Backdrop
			}
			if epRuntime > 0 {
				vm.Runtime = epRuntime
			} else if det.Runtime > 0 {
				vm.Runtime = det.Runtime
			}
			vm.Director = det.Director
			vm.Cast = det.Cast
			vm.Country = det.Country
			vm.Language = det.Language
			vm.OriginalLang = det.OriginalLang
			vm.Collection = det.Collection
			vm.MetaVersion = localMetaVersion
			localMetaCache[j.rel] = vm
		}
		localMetaMu.Unlock()
	}
	saveLocalMetaCache()
	logger.Printf("local library: backfill complete — %d entries from %d TMDB lookup(s)", len(jobs), len(cache))
}

// startLocalEnrichment runs the LLM+TMDB filename lookup (the same one the
// cloud pipeline uses) for every local-library file that doesn't have a
// cached result yet, a few at a time, persisting after each one so progress
// survives a restart. Rescans periodically to pick up newly added files.
// enrichWake lets a freshly-filed download trigger an enrichment sweep instead
// of waiting out the poll interval.
var enrichWake = make(chan struct{}, 1)

func (c *serverCtx) kickEnrichment() {
	select {
	case enrichWake <- struct{}{}:
	default:
	}
}

// extrasFolders are the folder names a disc rip puts its bonus material in.
var extrasFolders = []string{"featurettes", "extras", "bonus", "behind the scenes", "deleted scenes"}

// isUnidentifiable reports that a file should never be matched against TMDB.
//
// Bonus features are named for what they are — "Behind The Scenes.mkv",
// "Characters.mkv", "OP.mkv" — not for a title, and TMDB will always return
// *something* for a search like that. Left to run, enrichment matched
// "ED.mkv" to Ed Wood and "Notice #2.mkv" to Two Weeks Notice. A file in an
// extras folder, one already marked as an extra by hand, or one whose name is
// too short to carry a title gets its name used as its title and nothing more.
func isUnidentifiable(rel string, vm videoMeta, hadMeta bool) bool {
	if hadMeta && vm.IsExtra {
		return true
	}
	lower := strings.ToLower(filepath.ToSlash(rel))
	for _, seg := range strings.Split(lower, "/") {
		for _, x := range extrasFolders {
			if seg == x {
				return true
			}
		}
	}
	stem := strings.TrimSuffix(filepath.Base(rel), filepath.Ext(rel))
	return len(strings.TrimSpace(stem)) <= 4
}

func (c *serverCtx) startLocalEnrichment() {
	if len(c.localFolders) == 0 {
		return
	}
	loadLocalMetaCache()
	c.backfillLocalMeta()
	const concurrency = 3
	for {
		videos := c.scanLocalVideos()
		var pending []localVideo
		for _, v := range videos {
			// Still being written -- a half-arrived file's own name, size,
			// and probed streams are all meaningless until it stops changing;
			// leave it for a later pass (see isFileStable) instead of
			// spending an enrichment attempt, or an audio-check pass below,
			// on it now.
			if v.Downloading {
				continue
			}
			vm, ok := getLocalMeta(v.Rel)
			if !ok {
				pending = append(pending, v)
				continue
			}
			// Retry a file that has been through enrichment but never resolved
			// to anything, up to a limit. Entries that did resolve — including
			// every hand-corrected one — are never touched.
			if vm.TMDBID == 0 && vm.MediaType != "music_video" && vm.EnrichTries < maxEnrichTries {
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
					prev, had := getLocalMeta(v.Rel)
					var meta videoMeta
					if isUnidentifiable(v.Rel, prev, had) {
						// Its own name is the most accurate title available.
						meta.Title = cleanExtraTitle(filepath.Base(v.Rel))
						meta.MetaVersion = localMetaVersion
						meta.EnrichTries = maxEnrichTries // never retried
						meta.IsExtra = prev.IsExtra
						meta.MainKey = prev.MainKey
						localMetaMu.Lock()
						localMetaCache[v.Rel] = meta
						localMetaMu.Unlock()
						saveLocalMetaCache()
						return
					}
					if err := c.enrichMeta(filepath.Base(v.Rel), &meta); err != nil {
						// Couldn't even reach the LLM/TMDB this time (e.g. Ollama
						// wasn't running) -- leave the cache entry exactly as it
						// was and retry next pass, rather than burning one of
						// this file's limited attempts on an outage that has
						// nothing to do with it.
						logger.Printf("enrich %s: %v (retrying later)", filepath.Base(v.Rel), err)
						return
					}
					meta.MetaVersion = localMetaVersion
					if had {
						// is_extra and main_key are only ever set by hand
						// (see markExtras), so a retry must carry them over
						// or it would quietly undo that work.
						meta.IsExtra = prev.IsExtra
						meta.MainKey = prev.MainKey
						meta.EnrichTries = prev.EnrichTries
					}
					if meta.TMDBID == 0 {
						meta.EnrichTries++
						logger.Printf("enrich: no match for %s (attempt %d of %d)",
							filepath.Base(v.Rel), meta.EnrichTries, maxEnrichTries)
					}
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
				meta.MetaVersion = localMetaVersion
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

		// Audio/subtitle default check: runs across every video, matched or
		// not -- a file's own tracks are a property of the file, independent
		// of whatever title it resolves to -- gated on AudioCheckVer so a
		// given file is only probed once until audioCheckVersion changes.
		var audioPending []localVideo
		for _, v := range videos {
			if v.Type == "music_video" || v.Downloading {
				continue
			}
			vm, ok := getLocalMeta(v.Rel)
			if !ok || vm.AudioCheckVer < audioCheckVersion {
				audioPending = append(audioPending, v)
			}
		}
		if len(audioPending) > 0 {
			logger.Printf("local library: checking audio tracks for %d file(s)", len(audioPending))
			asem := make(chan struct{}, concurrency)
			var awg sync.WaitGroup
			for _, v := range audioPending {
				awg.Add(1)
				asem <- struct{}{}
				go func(v localVideo) {
					defer awg.Done()
					defer func() { <-asem }()
					checkAudioTrack(v)
				}(v)
			}
			awg.Wait()
			logger.Printf("local library: audio track check complete")
		}

		// PGS (bitmap) subtitle OCR: only worth attempting where there's no
		// text subtitle already (see checkPGSSubtitle), and only when a
		// PgsToSrt path is actually configured -- most libraries have none
		// of this shape, so skip building the list at all rather than
		// spending an ffprobe call per file establishing that.
		if pgsToSrtBin() != "" {
			var pgsPending []localVideo
			for _, v := range videos {
				if v.Type == "music_video" || v.Downloading {
					continue
				}
				vm, ok := getLocalMeta(v.Rel)
				if !ok || vm.PGSOCRVer < pgsOCRVersion {
					pgsPending = append(pgsPending, v)
				}
			}
			if len(pgsPending) > 0 {
				logger.Printf("local library: checking %d file(s) for PGS subtitles to OCR", len(pgsPending))
				// OCR is real CPU work (roughly a minute per episode) --
				// one at a time so it doesn't compete with everything else
				// this loop is already doing concurrently.
				osem := make(chan struct{}, 1)
				var owg sync.WaitGroup
				for _, v := range pgsPending {
					owg.Add(1)
					osem <- struct{}{}
					go func(v localVideo) {
						defer owg.Done()
						defer func() { <-osem }()
						checkPGSSubtitle(v)
					}(v)
				}
				owg.Wait()
				logger.Printf("local library: PGS OCR pass complete")
			}
		}

		select {
		case <-enrichWake:
		case <-time.After(60 * time.Second):
		}
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
		if v.Downloading {
			// Reuses the cloud pipeline's own "still working on it" state, so
			// the wall shows the same spinner card it already knows how to
			// render — see renderFileCard — rather than a broken thumbnail or
			// a false enrichment failure for a file that just isn't here yet.
			entry.ProcState = "processing"
		}
		if vm, ok := getLocalMeta(v.Rel); ok {
			entry.Enriched = true
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
			entry.AudioLang = vm.AudioLang
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
	path := r.URL.Query().Get("path")
	if !c.isAuthed(r) && !c.validFileToken(path, r.URL.Query().Get("tok")) {
		http.Error(w, "unauthorized", 401)
		return
	}
	full, ok := c.resolveLocalPath(path)
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

// handleLocalXSPF serves an XSPF playlist that pairs a local video file with
// its extracted WebVTT subtitle, so VLC on iOS loads the external subtitle
// instead of trying to render the embedded ASS track directly over HTTP.
func (c *serverCtx) handleLocalXSPF(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Query().Get("path")
	tok := r.URL.Query().Get("tok")
	if !c.isAuthed(r) && !c.validFileToken(path, tok) {
		http.Error(w, "unauthorized", 401)
		return
	}
	if _, ok := c.resolveLocalPath(path); !ok {
		http.Error(w, "not found", 404)
		return
	}
	scheme := "http"
	if r.TLS != nil {
		scheme = "https"
	}
	base := scheme + "://" + r.Host
	xmlEsc := func(s string) string { return strings.ReplaceAll(s, "&", "&amp;") }
	encodedPath := url.QueryEscape(path)
	encodedTok := url.QueryEscape(tok)
	videoURL := xmlEsc(base + "/local/file?path=" + encodedPath + "&tok=" + encodedTok)
	subURL := xmlEsc(base + "/local/subtitle?path=" + encodedPath + "&tok=" + encodedTok)
	xspf := `<?xml version="1.0" encoding="UTF-8"?>
<playlist xmlns="http://xspf.org/ns/0/" xmlns:vlc="http://www.videolan.org/vlc/playlist/0" version="1">
  <trackList>
    <track>
      <location>` + videoURL + `</location>
      <extension application="http://www.videolan.org/vlc/playlist/0">
        <vlc:option>sub-file=` + subURL + `</vlc:option>
      </extension>
    </track>
  </trackList>
</playlist>`
	w.Header().Set("Content-Type", "application/xspf+xml")
	w.Write([]byte(xspf))
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
<link rel="icon" href="/favicon.svg" type="image/svg+xml"><link rel="icon" href="/favicon.png" type="image/png">
<link rel="preconnect" href="https://fonts.googleapis.com">
<link rel="preconnect" href="https://fonts.gstatic.com" crossorigin>
<link href="https://fonts.googleapis.com/css2?family=Libre+Baskerville:ital,wght@0,400;0,700;1,400&display=swap" rel="stylesheet">
<style>
*{box-sizing:border-box;margin:0;padding:0}
body{background:#0d0d0d;color:#e0e0e0;font-family:'SF Mono',Consolas,monospace;display:flex;align-items:center;justify-content:center;min-height:100vh}
.box{width:100%;max-width:340px;padding:2rem}
h1{font-family:'Libre Baskerville',Georgia,serif;font-size:1.5rem;font-weight:700;letter-spacing:.03em;color:#e0e0e0;margin-bottom:2rem}
h1 span{color:#9A7D4A;font-weight:400;font-style:italic}
input{width:100%;background:#1a1a1a;border:1px solid #333;color:#e0e0e0;padding:.75rem 1rem;font-family:inherit;font-size:.9rem;border-radius:4px;outline:none;margin-bottom:1rem}
input:focus{border-color:#9A7D4A}
button{width:100%;background:#9A7D4A;border:none;color:#0d0d0d;padding:.75rem;font-family:inherit;font-size:.9rem;font-weight:600;border-radius:4px;cursor:pointer}
.err{color:#e06c6c;font-size:.8rem;margin-bottom:1rem}
</style>
</head>
<body>
<div class="box">
  <h1>Stream<span>Videos</span></h1>
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
<link href="https://fonts.googleapis.com/css2?family=DM+Sans:wght@300;400;500&family=JetBrains+Mono:wght@300;400;500&family=Libre+Baskerville:ital,wght@0,400;0,700;1,400&display=swap" rel="stylesheet">
<style>
*{box-sizing:border-box;margin:0;padding:0}
:root{--bg:#0a0a0a;--bg2:#141414;--bg3:#1e1e1e;--border:#2a2a2a;--gold:#9A7D4A;--text:#e0e0e0;--muted:#666;--font-mono:'JetBrains Mono',Consolas,monospace;--font-sans:'DM Sans',-apple-system,sans-serif;--font-serif:'Libre Baskerville',Georgia,serif;--gutter:2rem;--shell:980px;--play:#d9282d;--play-lit:#ef3237;--detail-indent:200px;--detail-measure:520px;--detail-top:104px}
body{background:var(--bg);color:var(--text);font-family:var(--font-mono);font-weight:300;font-size:.875rem;min-height:100vh;overflow-x:hidden}
a{color:inherit;text-decoration:none}

/* ── Header — a serif two-tone wordmark on the left, monospace pills on the
   right. The split does the work the old flat "// stream" label couldn't:
   the brand reads as typography, the controls read as chrome, and nothing
   competes with the cover art below. Pill geometry (999px radius, .72rem
   mono, .06em tracking) is shared with the FileDrop bar so the two apps
   read as one family. ── */
/* Header and section strip travel together, so the strip never has to guess
   the header's height to stick beneath it. */
#topbar{
  position:sticky;top:0;z-index:10;
  background-color:var(--bg);
  background-image:radial-gradient(80% 55% at 50% 0%, color-mix(in srgb, var(--tint, rgb(154,125,74)) 28%, transparent), transparent 72%);
  background-size:100% 100vh;background-repeat:no-repeat;background-position:left top;
  transition:background-image 420ms cubic-bezier(.4,0,.2,1);
}
body.light #topbar{
  background-image:radial-gradient(80% 55% at 50% 0%, color-mix(in srgb, var(--tint, rgb(154,125,74)) 17%, transparent), transparent 72%);
}
header{display:flex;align-items:center;gap:.6rem;padding:1.05rem max(var(--gutter),calc((100% - var(--shell))/2 + var(--gutter)));border-bottom:1px solid rgba(255,255,255,.05)}
body.light header{border-bottom-color:rgba(0,0,0,.08)}
/* Only present while a show/movie/playlist detail page is open (see
   renderGrid) -- living in the topbar rather than the page below it means it
   rides along for free wherever the topbar's own sticky behaviour already
   keeps it, with no bespoke stickiness of its own to maintain. */
#topbar-back{display:none;background:none;border:none;color:var(--muted);font-family:var(--font-mono);font-size:.78rem;letter-spacing:.05em;cursor:pointer;padding:0;white-space:nowrap}
#topbar-back:hover{color:var(--gold)}
.wordmark{font-family:var(--font-serif);font-size:1.1rem;font-weight:700;letter-spacing:.03em;color:var(--text);white-space:nowrap;margin-right:.35rem}
.wordmark span{color:var(--gold);font-weight:400;font-style:italic}
.spacer{flex:1}
.pill{font-size:.72rem;font-weight:500;letter-spacing:.06em;color:var(--muted);border:1px solid var(--border);padding:.3rem .8rem;border-radius:999px;white-space:nowrap}

/* ── Section chips ─────────────────────────────────────────────────────────
   The bar had a wide empty middle and the wall below it now has named
   sections, so that space earns its keep as a jump list. Built from whichever
   rails actually rendered rather than hardcoded, and the active one tracks
   what you have scrolled to. ── */
/* ── Section list ──────────────────────────────────────────────────────────
   Fixed in the margin outside the centred column, so it takes none of the
   shelf's width and stays put as the page scrolls. Positioned from the centre
   line rather than the viewport edge, so it tracks the column exactly whatever
   the scrollbar is doing. It only exists where there is margin to put it in;
   below that width there is no padded space to occupy. ── */
#section-tags{display:none}
@media(min-width:1380px){
  #section-tags{
    display:flex;flex-direction:column;
    position:fixed;z-index:5;
    left:calc(50% + var(--shell) / 2 + 26px);
    top:104px;
    width:168px;
    border-left:1px solid var(--border);padding-left:1.15rem;
  }
  #section-tags:empty{display:none}
}
.sec-tag{
  display:flex;align-items:baseline;justify-content:space-between;gap:.7rem;
  background:none;border:none;border-left:2px solid transparent;
  color:var(--muted);font-family:inherit;font-size:.82rem;font-weight:400;
  text-align:left;padding:.5rem 0 .5rem .8rem;margin-left:-1.15rem;
  cursor:pointer;white-space:nowrap;
  transition:color .18s ease,border-color .18s ease,padding-left .18s ease;
}
.sec-tag:hover{color:var(--text)}
.sec-tag.on{color:var(--text);border-left-color:var(--text);padding-left:1rem}
.sec-tag .n{font-family:var(--font-mono);font-size:.64rem;color:var(--muted)}
.sec-tag:focus-visible{outline:2px solid var(--text);outline-offset:-2px;border-radius:2px}
@media(prefers-reduced-motion:reduce){.sec-tag{transition:none}}
.header-btn{background:none;border:1px solid var(--border);color:var(--muted);font-family:inherit;font-size:.72rem;font-weight:500;letter-spacing:.06em;padding:.3rem .8rem;border-radius:999px;cursor:pointer;white-space:nowrap;transition:border-color .15s,color .15s}
.header-btn:hover,.header-btn.active{border-color:var(--gold);color:var(--gold)}

/* ── Ambient tint ──────────────────────────────────────────────────────────
   A wash of the colour of whatever poster is under the cursor, thrown from
   the top of the page. It sits above the body's flat ground but beneath all
   content, and stays deliberately faint — the posters supply the colour, this
   only lets a little of it into the room.

   --tint is registered as a <color> rather than left as a bare "r,g,b" triplet
   specifically so this can transition at all: an unregistered custom property
   is just a string substitution, and swapping it inside rgba(var(--tint),a)
   changes the rendered colour instantly no matter what transition is declared
   on the element -- registering the type is what tells the browser there is
   an actual colour to interpolate between old and new. color-mix() takes the
   place of rgba(var(--tint),a) for the same reason: you cannot nest a <color>
   inside rgba()'s own r/g/b slots. ── */
@property --tint {
  syntax: '<color>';
  inherits: true;
  initial-value: rgb(154,125,74);
}
#tint{
  position:fixed;inset:0;z-index:0;pointer-events:none;
  background:
    radial-gradient(80% 55% at 50% 0%, color-mix(in srgb, var(--tint, rgb(154,125,74)) 28%, transparent), transparent 72%),
    radial-gradient(120% 90% at 50% 100%, color-mix(in srgb, var(--tint, rgb(154,125,74)) 9%, transparent), transparent 60%);
  transition:background 420ms cubic-bezier(.4,0,.2,1),opacity .6s ease;
}
body.light #tint{
  background:
    radial-gradient(80% 55% at 50% 0%, color-mix(in srgb, var(--tint, rgb(154,125,74)) 17%, transparent), transparent 72%),
    radial-gradient(120% 90% at 50% 100%, color-mix(in srgb, var(--tint, rgb(154,125,74)) 6%, transparent), transparent 60%);
}
/* Lift content above the wash. #topbar is deliberately absent — it is already
   position:sticky and relisting it here would quietly override that. */
#controls,#file-grid,#music-grid,#music-heading,#file-list,#toast{position:relative;z-index:1}
@media(prefers-reduced-motion:reduce){#tint{transition:none}}

/* ── Now-playing wash ─────────────────────────────────────────────────────
   Something is minimized and queued up behind several more (see
   hasActiveQueue) -- a session, not a single thing paused in the corner --
   and the wall says so on its own: one large radial tint of the playing
   track's own colour (--tint-playing, set once per track in openFile — see
   setNowPlayingTint), sitting behind everything and centred near the
   player's own corner, breathing in opacity rather than moving or changing
   size. Home wall only, playing only -- a show/movie/playlist page is
   untouched, and pausing (or ending the session) fades it out entirely
   rather than just freezing it in place; see updateNowPlayingWash. While
   it's showing, the wash is the *only* accent: the ordinary top-of-page
   #tint hides outright (html.wash-active below) rather than just sitting
   there unchanging underneath it, and hovering a card is suspended too (see
   tintWashActive) so nothing fights the wash for the same visual role. */
@property --tint-playing {
  syntax: '<color>';
  inherits: true;
  initial-value: rgb(154,125,74);
}
html.wash-active #tint{opacity:0}
#tint-wash{
  position:fixed;inset:0;z-index:0;pointer-events:none;
  background:radial-gradient(ellipse at 82% 92%, color-mix(in srgb, var(--tint-playing, rgb(154,125,74)) 28%, transparent), transparent 60%);
  opacity:0;transition:opacity .6s ease;
}
#tint-wash.on{opacity:.4;animation:tintWashPulse 7s ease-in-out infinite}
@keyframes tintWashPulse{0%,100%{opacity:.4}50%{opacity:.75}}
@media(prefers-reduced-motion:reduce){#tint-wash.on{animation:none;opacity:.55}}

/* Management controls hidden when viewing remotely (through the tunnel) */
body.remote .mgmt{display:none!important}

/* ── Motion ────────────────────────────────────────────────────────────────
   One idea only: tiles settle into place as the wall paints. The stagger is
   capped by the renderer so the last tile in a long grid is not still
   waiting half a second after the first. ── */
@keyframes tileIn{from{opacity:0;transform:translateY(9px)}to{opacity:1;transform:none}}
@keyframes heroIn{from{opacity:0}to{opacity:1}}
.card,.cw-card{animation:tileIn .42s cubic-bezier(.22,.61,.36,1) both;animation-delay:var(--in,0ms)}
.hero{animation:heroIn .55s ease both}
.card{transition:transform .2s cubic-bezier(.22,.61,.36,1)}
.card:hover{transform:translateY(-3px)}
@media(prefers-reduced-motion:reduce){
  .card,.cw-card,.hero{animation:none}
  .card:hover{transform:none}
  .card:hover .pc-cover img,.card:focus .pc-cover img,
  .cw-card:hover .cw-shot img,.cw-card:focus .cw-shot img,
  .mv2-card:hover .mv2-cover img,.mv2-card:focus .mv2-cover img{transform:none}
  *{transition-duration:.01ms!important}
}

/* Toast */
#toast{position:fixed;left:50%;bottom:2rem;transform:translateX(-50%) translateY(1rem);background:var(--bg3);border:1px solid var(--gold);color:var(--text);padding:.65rem 1.1rem;border-radius:6px;font-size:.8rem;box-shadow:0 6px 24px rgba(0,0,0,.5);opacity:0;pointer-events:none;transition:opacity .2s,transform .2s;z-index:300;max-width:80vw}
#toast.show{opacity:1;transform:translateX(-50%) translateY(0)}
#toast.err{border-color:#e06c6c}

/* Section heading — a wayfinding label, not a heading: small, wide-tracked,
   no divider line, just air above it. */
.section-heading{grid-column:1/-1;color:var(--muted);font-family:var(--font-mono);font-size:9px;font-weight:300;letter-spacing:.15em;text-transform:uppercase;margin-top:1.5rem}
#file-list .section-heading{margin:1.5rem 0 .5rem}
#music-heading{color:var(--muted);font-family:var(--font-mono);font-size:9px;font-weight:300;letter-spacing:.15em;text-transform:uppercase;margin:56px auto 1rem;padding:0 var(--gutter);max-width:var(--shell);display:none}

/* Music video cards (poster/gallery view) — plain 16:9 cover art. The
   landscape shape alone signals "this is video", same as the 2:3 shape
   signals "this is a film" — no film-strip decoration needed. */
.mv2-cover{aspect-ratio:16/9;border-radius:6px;overflow:hidden;background:#111;position:relative;display:flex;align-items:center;justify-content:center}
.mv2-cover img{width:100%;height:100%;object-fit:cover;display:block;transition:filter .2s,transform .35s cubic-bezier(.22,.61,.36,1)}
.mv2-card:hover .mv2-cover img,.mv2-card:focus .mv2-cover img{filter:brightness(1.18);transform:scale(1.045)}
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
/* Holds only a status line, so it reserves no height until there is one to
   show — the banner sits directly under the bar. */
#controls{display:flex;align-items:center;gap:.35rem;padding:0 var(--gutter);max-width:var(--shell);margin:0 auto}
#msg:not(:empty){padding-bottom:.55rem}
.view-btn{background:none;border:1px solid var(--border);color:var(--muted);width:28px;height:28px;flex-shrink:0;display:inline-flex;align-items:center;justify-content:center;font-family:inherit;font-size:.95rem;line-height:1;border-radius:999px;cursor:pointer;transition:border-color .15s,color .15s}
.view-btn:hover{border-color:var(--gold);color:var(--gold)}
.view-btn.active{border-color:var(--gold);color:var(--gold);background:rgba(154,125,74,.12)}
#msg{margin-left:auto;font-size:.75rem}
#msg.ok{color:#6dbf6d}
#msg.err{color:#e06c6c}

/* ── Grid container ───────────────────────────────────────────────────────── */
/* Five to a row rather than four, which makes each poster smaller and the
   wall read as a contact sheet — the way a Letterboxd list does. The rails
   inside (continue watching, and the music grid below) set their own sizes
   and are deliberately left alone. */
#file-grid{display:grid;gap:18px;padding:0 var(--gutter) 2rem;max-width:var(--shell);margin:0 auto;grid-template-columns:repeat(5,1fr)}
#music-grid{display:none;grid-template-columns:repeat(auto-fill,minmax(215px,1fr));gap:24px 24px;row-gap:28px;padding:0 var(--gutter) 2rem;max-width:var(--shell);margin:0 auto}

/* ── Shared card base ─────────────────────────────────────────────────────── */
.card{cursor:pointer;position:relative;background:none;border:none;border-radius:0;transition:transform .18s}
/* The highlight is a soft glow rather than a hard-edged ring, and it is
   light rather than gold: gold now means "part-watched" and must not also
   mean "hovered". A crisp 2px line read as a defect more than a highlight;
   spreading the same light out into a blur keeps the cue without it. */
.pc-cover,.mv2-cover{box-shadow:0 0 0 0 rgba(255,255,255,0);transition:box-shadow .18s ease}
.card:hover .pc-cover,.card:focus-visible .pc-cover,
.mv2-card:hover .mv2-cover,.mv2-card:focus-visible .mv2-cover{box-shadow:0 0 16px 1px rgba(255,255,255,.22)}
body.light .card:hover .pc-cover,body.light .card:focus-visible .pc-cover{box-shadow:0 0 16px 1px rgba(40,30,20,.32)}
/* Chrome keeps the gold focus ring — it is brand furniture. Content tiles
   take a neutral one, so gold on the wall only ever means "you have watched
   part of this". */
.view-btn:focus-visible,.header-btn:focus-visible,#modal-close:focus-visible,#modal-actions button:focus-visible,#modal-quality:focus-visible,.sr-tmdb:focus-visible,.sr-torrent:focus-visible{outline:2px solid var(--gold);outline-offset:2px}
/* :focus-visible rather than :focus: clicking (or right-clicking) a button
   focuses it in some browsers, which otherwise drew this same outline on
   nothing more than a click -- :focus-visible's own heuristic already knows
   to skip that and only show for keyboard/remote navigation. */
.card:focus-visible,.lr:focus-visible,.ep-row:focus-visible,.sv-ep-row:focus-visible,.cw-card:focus-visible,.hero-btn:focus-visible{outline:2px solid rgba(255,255,255,.55);outline-offset:2px}

/* ── POSTER cards — a gallery, not a data grid: cover art does the work,
   nothing sits on top of it. Dark theme hovers via a brightness lift; the
   light theme below overrides this with a physical lift + shadow instead. ── */
.pc-cover{aspect-ratio:2/3;border-radius:8px;overflow:hidden;background:var(--bg2);position:relative}
.pc-cover img{width:100%;height:100%;object-fit:cover;display:block;transition:filter .25s,transform .35s cubic-bezier(.22,.61,.36,1)}
.card:hover .pc-cover img,.card:focus .pc-cover img{filter:brightness(1.1);transform:scale(1.045)}

/* ── Title on demand ───────────────────────────────────────────────────────
   The poster is the identifier, so the caption only appears when a tile is
   actually under scrutiny (hover, or keyboard/remote focus). At rest the
   wall is uninterrupted art — the Letterboxd move. Music-video cards keep
   their permanent captions instead: their art is a frame grabbed from the
   video, which identifies nothing on its own. ── */
.pc-ovl{
  position:absolute;inset:0;display:flex;flex-direction:column;justify-content:flex-end;
  padding:.55rem .6rem;opacity:0;transition:opacity .22s ease;
  background:linear-gradient(to top,rgba(0,0,0,.92) 0%,rgba(0,0,0,.45) 38%,transparent 68%);
}
.card:hover .pc-ovl,.card:focus .pc-ovl,.card:focus-within .pc-ovl{opacity:1}
.pc-ovl-t{font-family:var(--font-sans);font-size:.78rem;font-weight:400;color:#fff;line-height:1.28;
  display:-webkit-box;-webkit-line-clamp:3;-webkit-box-orient:vertical;overflow:hidden}
.pc-ovl-y{font-family:var(--font-mono);font-size:.66rem;color:#c9a765;margin-top:.18rem;letter-spacing:.06em}

/* ── Card overflow menu ───────────────────────────────────────────────────
   Anchored inside the cover box itself (top-right), not the card as a
   whole, so it sits in the same corner whether the cover is a tall poster
   or a wide still -- on demand like the caption above, permanent on touch
   for the same reason. Opens the same menu a right-click does (see
   openCtxMenu); it's just a discoverable, tap-friendly way in to it. ── */
.card-more{
  position:absolute;top:.4rem;right:.4rem;z-index:2;
  width:1.6rem;height:1.6rem;border-radius:50%;
  display:flex;align-items:center;justify-content:center;
  background:rgba(10,10,10,.72);border:1px solid rgba(255,255,255,.22);color:#fff;
  font-size:.9rem;line-height:1;cursor:pointer;
  opacity:0;transition:opacity .18s ease,border-color .15s,color .15s;
}
.card:hover .card-more,.card:focus-within .card-more,
.cw-card:hover .card-more,.cw-card:focus-within .card-more{opacity:1}
.card-more:hover{border-color:var(--gold);color:var(--gold)}
@media(max-width:600px){ .card-more{opacity:1} }

/* Artless tiles get a set title card rather than a broken-image box — 28% of
   the library has no TMDB match, and hiding captions would leave them blank. */
.pc-noart{
  position:absolute;inset:0;display:flex;flex-direction:column;justify-content:center;
  padding:.9rem .8rem;background:linear-gradient(160deg,var(--bg3),var(--bg2));
  border:1px solid var(--border);border-radius:8px;
}
.pc-noart-t{font-family:var(--font-serif);font-size:.86rem;font-weight:700;color:var(--text);
  line-height:1.3;display:-webkit-box;-webkit-line-clamp:4;-webkit-box-orient:vertical;overflow:hidden}
.pc-noart-k{font-family:var(--font-mono);font-size:.6rem;color:var(--muted);letter-spacing:.1em;
  text-transform:uppercase;margin-top:.5rem}

/* Gold is reserved for your own state: how far into something you are.
   Hover and focus use a plain light rule so the accent keeps its meaning. */
/* ── Playlists ─────────────────────────────────────────────────────────────
   A playlist card is 16:9 like the continue row rather than 2:3 like a
   poster, because its cover is a grid of stills, not cover art. ── */
.pl-card{cursor:pointer;position:relative;background:none;border:none;padding:0;text-align:left}
.pl-cover{aspect-ratio:16/9;border-radius:6px;overflow:hidden;background:var(--bg2);position:relative;
  box-shadow:0 0 0 0 rgba(255,255,255,0);transition:box-shadow .18s ease}
.pl-card:hover .pl-cover,.pl-card:focus-visible .pl-cover{box-shadow:0 0 16px 1px rgba(255,255,255,.22)}
body.light .pl-card:hover .pl-cover{box-shadow:0 0 16px 1px rgba(40,30,20,.32)}
.pl-cover img{width:100%;height:100%;object-fit:cover;display:block}
.pl-empty{display:flex;align-items:center;justify-content:center;height:100%;color:var(--muted);font-size:1.5rem}
.pl-meta{padding-top:.5rem}
.pl-name{font-family:var(--font-sans);font-size:.82rem;color:var(--text);line-height:1.3;
  display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.pl-count{font-family:var(--font-mono);font-size:.66rem;color:var(--muted);margin-top:.22rem;letter-spacing:.05em}

/* ── Right-click menu ──────────────────────────────────────────────────────
   The wall has no room for a per-tile control, and a hover affordance would
   fight the caption, so adding to a playlist lives where people already
   right-click. ── */
#ctx-menu{
  position:fixed;z-index:400;min-width:210px;display:none;
  background:var(--bg2);border:1px solid var(--border);border-radius:7px;
  box-shadow:0 10px 34px rgba(0,0,0,.5);padding:.3rem;overflow:hidden;
}
#ctx-menu.open{display:block}
.ctx-head{
  font-family:var(--font-mono);font-size:.6rem;letter-spacing:.12em;text-transform:uppercase;
  color:var(--muted);padding:.45rem .6rem .35rem;white-space:nowrap;overflow:hidden;text-overflow:ellipsis;
}
.ctx-item{
  display:flex;align-items:center;justify-content:space-between;gap:.7rem;width:100%;
  background:none;border:none;color:var(--text);font-family:inherit;font-size:.8rem;
  text-align:left;padding:.45rem .6rem;border-radius:4px;cursor:pointer;
}
.ctx-item:hover,.ctx-item:focus{background:var(--bg3);outline:none}
.ctx-item .n{font-family:var(--font-mono);font-size:.65rem;color:var(--muted)}
.ctx-sep{height:1px;background:var(--border);margin:.3rem .2rem}
/* Opened from the player's own right-click while minimized (see
   openPlayerCtxMenu) -- the regular menu size next to a 340px corner
   player reads as oversized, so it scales down to match. */
#ctx-menu.ctx-compact{min-width:150px}
#ctx-menu.ctx-compact .ctx-head{font-size:.54rem;padding:.35rem .5rem .25rem}
#ctx-menu.ctx-compact .ctx-item{font-size:.7rem;padding:.35rem .5rem}
#ctx-menu.ctx-compact .ctx-item .n{font-size:.58rem}
.ctx-new{color:var(--gold)}

.pc-prog{position:absolute;left:0;right:0;bottom:0;height:3px;background:rgba(255,255,255,.18)}
.pc-prog i{position:absolute;left:0;top:0;bottom:0;background:var(--gold);display:block}
.pc-proc{position:absolute;inset:0;display:flex;align-items:center;justify-content:center;font-size:1.5rem;color:var(--gold)}
/* ── Library sections ──────────────────────────────────────────────────────
   Rails live inside #file-grid and span every column, so poster tiles keep
   flowing in the same grid underneath each heading. ── */
.rail-head{grid-column:1/-1;display:flex;align-items:baseline;gap:.7rem;margin:1.9rem 0 .1rem}
.rail-head:first-child{margin-top:.2rem}
.rail-head .rh-t{font-family:var(--font-serif);font-size:1.02rem;font-weight:700;color:var(--text);white-space:nowrap}
.rail-head .rh-c{font-family:var(--font-mono);font-size:.68rem;color:var(--muted);letter-spacing:.08em}
.rail-head .rh-l{flex:1;height:1px;background:var(--border)}

/* Continue watching: 16:9 stills, wider than a poster, so the row reads as a
   different kind of object from the wall below it. */
.cw-rail{grid-column:1/-1;display:grid;grid-template-columns:repeat(auto-fill,minmax(230px,1fr));gap:16px;margin-top:.9rem}
.cw-card{cursor:pointer;background:none;border:none;padding:0;text-align:left;position:relative}
.cw-shot{aspect-ratio:16/9;border-radius:6px;overflow:hidden;background:var(--bg2);position:relative}
.cw-shot img{width:100%;height:100%;object-fit:cover;display:block;transition:filter .25s,transform .35s cubic-bezier(.22,.61,.36,1)}
.cw-card:hover .cw-shot img,.cw-card:focus .cw-shot img{filter:brightness(1.1);transform:scale(1.04)}
.cw-shot-ph{display:flex;align-items:center;justify-content:center;height:100%;font-size:1.7rem;color:var(--muted)}
.cw-bar{position:absolute;left:0;right:0;bottom:0;height:3px;background:rgba(255,255,255,.2)}
.cw-bar i{position:absolute;left:0;top:0;bottom:0;background:var(--gold);display:block}
.cw-meta{padding-top:.5rem}
.cw-t{font-family:var(--font-sans);font-size:.8rem;color:var(--text);line-height:1.3;
  display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.cw-s{font-family:var(--font-mono);font-size:.66rem;color:var(--muted);margin-top:.22rem;letter-spacing:.05em}

/* Hero — the one thing on the page allowed to be bigger than everything else.
   Uses the tmdb_backdrop that 72% of entries carry and the grid never showed. */
.hero{grid-column:1/-1;position:relative;border-radius:8px;overflow:hidden;margin-bottom:.4rem;cursor:pointer;
  min-height:340px;display:flex;align-items:flex-end;background:var(--bg)}
.hero img{position:absolute;inset:0;width:100%;height:100%;object-fit:cover;object-position:center 22%}
.hero-scrim{position:absolute;inset:0;
  background:linear-gradient(to right,rgba(10,10,10,.95) 0%,rgba(10,10,10,.72) 42%,rgba(10,10,10,.15) 82%),
             linear-gradient(to top,rgba(10,10,10,.9),transparent 55%)}
.hero-in{position:relative;z-index:2;padding:1.5rem 1.6rem;max-width:min(560px,72%)}
.hero-kicker{font-family:var(--font-mono);font-size:.66rem;letter-spacing:.13em;text-transform:uppercase;color:var(--gold);margin-bottom:.45rem}
.hero-t{font-family:var(--font-serif);font-size:1.9rem;font-weight:700;color:#fff;line-height:1.12;margin-bottom:.5rem;text-wrap:balance}
.hero-d{font-family:var(--font-sans);font-size:.85rem;line-height:1.6;color:#cfc9bf;margin-bottom:.9rem;
  display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.hero-btn{display:inline-flex;align-items:center;gap:.45rem;font-family:var(--font-mono);font-size:.75rem;
  letter-spacing:.09em;color:#0a0a0a;background:var(--gold);border:none;padding:.5rem 1.15rem;
  border-radius:3px;cursor:pointer;transition:filter .15s}
.hero-btn:hover{filter:brightness(1.12)}

.pc-info{padding:.6rem .05rem .15rem}
.pc-title{font-family:var(--font-sans);font-size:12px;font-weight:400;color:#d0d0d0;line-height:1.3;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden;margin-bottom:.2rem}
.pc-year{font-family:var(--font-sans);font-size:10px;font-weight:300;color:#686868}

/* ── Light theme — warm parchment, not clinical white. Cards read as
   physical objects: a subtle shadow at rest that blooms on hover, lifted
   slightly off the surface, rather than the dark theme's brightness pop. ── */
body.light{--bg:#d9d1c3;--bg2:#d0c7b7;--bg3:#c6bca9;--border:#a89d8b;--text:#1f1a13;--muted:#524a3c;--gold:#634d21;--play:#a5171c;--play-lit:#c01f24}
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
#search-kind{display:flex;gap:.4rem;padding:0 .75rem .7rem}
.sk-btn{
  flex:1;background:none;border:1px solid var(--border);color:var(--muted);
  padding:.42rem .5rem;border-radius:999px;cursor:pointer;font-family:inherit;
  font-size:.76rem;transition:border-color .15s,color .15s,background .15s;
}
.sk-btn.active{border-color:var(--gold);color:var(--gold);background:rgba(154,125,74,.1)}
.sk-btn:focus-visible{outline:2px solid var(--text);outline-offset:2px}

/* Music results: the thumbnail carries the identification, so the row is a
   still plus two lines, with the artist's own uploads marked. */
.sr-music{display:flex;gap:.8rem;padding:.6rem .75rem;border-bottom:1px solid var(--border);align-items:center}
.sr-music-thumb{width:104px;aspect-ratio:16/9;border-radius:4px;object-fit:cover;flex-shrink:0;background:var(--bg2)}
.sr-music-info{flex:1;min-width:0}
.sr-music-t{font-size:.82rem;color:var(--text);line-height:1.3;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.sr-music-m{font-family:var(--font-mono);font-size:.68rem;color:var(--muted);margin-top:.25rem;display:flex;align-items:center;gap:.5rem;flex-wrap:wrap}
.sr-official{color:var(--gold);border:1px solid var(--gold);border-radius:999px;padding:.02rem .4rem;font-size:.6rem;letter-spacing:.06em}
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
  .sv-hero-top{gap:32px}
  .sv-poster{width:130px}
  .sv-title{font-size:2rem}
  :root{--detail-indent:162px;--detail-top:88px}

}
@media(max-width:600px){
  /* Two things give way on a phone. The 2rem gutter costs the bar 32px it
     does not have, so the whole page steps down together — the header must
     never drift out of alignment with the grid under it. And the pill
     tracking that gives the bar its air on a desktop is exactly what
     overflows here, so the chrome tightens rather than the mark truncating. */
  :root{--gutter:1rem}
  header{padding:.7rem var(--gutter);gap:.35rem;flex-wrap:nowrap}
  .wordmark{font-size:.92rem;margin-right:0}
  .pill{display:none}
  #search-open-btn{display:none}
  .header-btn{padding:.3rem .62rem;font-size:.68rem;letter-spacing:.04em}
  .view-btn{width:27px;height:27px;font-size:.9rem}
  .hero{min-height:0;aspect-ratio:16/10}
  .hero-in{padding:1.05rem 1.1rem;max-width:100%}
  .hero-t{font-size:1.3rem}
  .hero-d{font-size:.78rem}
  .cw-rail{grid-template-columns:repeat(auto-fill,minmax(165px,1fr));gap:12px}
  .rail-head{margin:1.4rem 0 .1rem}
  .rail-head .rh-t{font-size:.92rem}
  /* Touch has no hover, so an on-demand caption would never appear — the
     overlay becomes permanent rather than the title becoming unreachable. */
  .pc-ovl{opacity:1;background:linear-gradient(to top,rgba(0,0,0,.9) 0%,rgba(0,0,0,.3) 42%,transparent 66%)}
  .pc-ovl-t{font-size:.7rem;-webkit-line-clamp:2}
  .pc-ovl-y{font-size:.6rem}
}

/* Smallest phones: the bar runs out of room before the controls do, so the
   wordmark drops its second half rather than a control disappearing —
   "Stream" alone still carries the mark, a missing view toggle would not. */
@media(max-width:400px){
  .wordmark{font-size:.88rem}
  .wordmark span{display:none}
  .header-btn{padding:.28rem .55rem;font-size:.66rem}
  .view-btn{width:26px;height:26px}
  #topbar-back{font-size:.68rem}
  #file-grid{gap:14px;grid-template-columns:repeat(auto-fill,minmax(130px,1fr))}
  #music-grid{grid-template-columns:repeat(auto-fill,minmax(160px,1fr));gap:16px}

  /* Cinematic detail — backdrop */
  .sv-backdrop{height:220px}

  /* Hero: poster+title side by side, description+play full-width below */
  .sv-hero{margin-top:-56px}
  .sv-hero-top{gap:14px}
  .sv-poster{width:90px}
  .sv-info{padding-top:58px}
  .sv-title{font-size:1.4rem}
  .sv-meta{font-size:.72rem;gap:.4rem;flex-wrap:wrap}
  .sv-genres{font-size:.7rem}
  .sv-overview{font-size:.85rem;max-width:100%;margin-left:0}
  /* No room for a 520px measure on a phone, so the disc returns to the
     right-hand edge, still on the title's line. */
  .sv-play-btn{left:auto;right:0;top:.2rem;width:38px;height:38px}
  .sv-play-btn::before{border-width:7px 0 7px 11px;margin-left:3px}

  .sv-season-tabs{gap:1rem;margin:1.2rem 0 0}
  .sv-season-tab{font-size:.75rem}
  .sv-ep-row{gap:.8rem;padding:.85rem .6rem;margin:0 -.6rem}
  .sv-ep-shot{width:112px}
  .sv-ep-num{width:1.1rem;font-size:1.1rem}
  .sv-ep-name{font-size:.88rem}
  .sv-ep-desc{font-size:.78rem;-webkit-line-clamp:2}
  .sv-ep-time{font-size:.68rem}
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
/* Getting back used to live inside the backdrop image, which scrolled away
   with it -- reaching it again meant scrolling back to the top first. It now
   lives in the topbar itself (see #topbar-back), which is already sticky, so
   there's nothing bespoke to keep stuck here any more. */
.sv-hero{display:flex;flex-direction:column;margin-top:-96px;position:relative;z-index:2}
.sv-hero-top{display:flex;gap:40px;align-items:flex-start}
.sv-poster{flex-shrink:0;width:160px;aspect-ratio:2/3;border-radius:4px;object-fit:cover;box-shadow:0 8px 36px rgba(0,0,0,.5)}
.sv-poster-ph{display:flex;align-items:center;justify-content:center;background:var(--bg2);font-size:2.5rem}
.sv-info{flex:1;padding-top:var(--detail-top);min-width:0}
.sv-title{font-family:var(--font-serif);font-size:2.3rem;font-weight:700;color:var(--text);letter-spacing:-.01em;margin:0 0 .55rem;line-height:1.14;text-wrap:balance}
.sv-meta{display:flex;align-items:center;gap:.7rem;font-family:var(--font-mono);font-size:.82rem;color:var(--muted);margin-bottom:.6rem}
.sv-meta-sep{color:var(--border)}
.sv-rating{color:var(--gold)}
.sv-genres{font-family:var(--font-mono);font-size:.8rem;color:var(--muted);margin-bottom:1rem}
.sv-overview{font-family:var(--font-sans);font-size:.95rem;line-height:1.7;color:var(--text);max-width:var(--detail-measure);margin:0 0 1.3rem var(--detail-indent)}
/* ── Play ──────────────────────────────────────────────────────────────────
   One large red disc floating clear of the text column on the right. It is the
   only saturated colour in the app, which is what lets it read instantly as
   the thing to press — and it is far enough from the gold that the two never
   compete. ── */
/* ── Play ──────────────────────────────────────────────────────────────────
   Sized to the title's line box and sitting on the same baseline, at the x
   where the description's measure ends — so it reads as part of the title
   line rather than as a badge dropped on the artwork. Both coordinates come
   from the same tokens the indent and measure use, so they cannot drift. ── */
.sv-play-btn{
  position:absolute;z-index:3;
  left:calc(var(--detail-indent) + var(--detail-measure));
  top:var(--detail-top);
  width:44px;height:44px;border-radius:50%;border:none;padding:0;
  display:flex;align-items:center;justify-content:center;
  background:var(--play);color:#fff;cursor:pointer;
  box-shadow:0 3px 14px rgba(0,0,0,.4);
  transition:transform .18s cubic-bezier(.22,.61,.36,1),box-shadow .18s ease,background .18s ease;
}
.sv-play-btn::before{
  content:"";display:block;
  border-style:solid;border-width:8px 0 8px 13px;
  border-color:transparent transparent transparent #fff;
  margin-left:3px;
}
.sv-play-btn:hover{transform:scale(1.09);background:var(--play-lit);box-shadow:0 5px 20px rgba(0,0,0,.48)}
.sv-play-btn:active{transform:scale(1.02)}
.sv-play-btn:focus-visible{outline:3px solid #fff;outline-offset:3px}
@media(prefers-reduced-motion:reduce){.sv-play-btn{transition:none}}

.sv-season-tabs{grid-column:1/-1;display:flex;gap:1.5rem;border-bottom:1px solid var(--border);margin:1.8rem 0 0}
.sv-season-tab{font-family:var(--font-mono);font-size:.82rem;letter-spacing:.05em;color:var(--muted);padding-bottom:.7rem;border-bottom:1px solid transparent;cursor:pointer}
.sv-season-tab:hover{color:var(--text)}
.sv-season-tab.active{color:var(--text);border-color:var(--text)}

/* ── Episode rows ──────────────────────────────────────────────────────────
   The pattern the streaming services converged on: a big quiet episode number
   anchoring the left, a 16:9 still, then title and runtime on one line with
   the synopsis under it on a constrained measure. The old row ran the synopsis
   the full width of the page, which is why it read as a wall. ── */
.sv-ep-row{
  grid-column:1/-1;display:flex;align-items:flex-start;gap:1.15rem;
  padding:1rem .9rem;margin:0 -.9rem;border-radius:8px;
  border-bottom:1px solid var(--border);cursor:pointer;
  transition:background .18s ease;
}
.sv-ep-row:hover{background:var(--bg2)}
.sv-ep-num{
  flex-shrink:0;width:1.6rem;text-align:center;padding-top:.15rem;
  font-family:var(--font-serif);font-size:1.5rem;font-weight:400;
  color:var(--muted);line-height:1.1;font-variant-numeric:tabular-nums;
}
.sv-ep-head{display:flex;align-items:baseline;gap:.75rem;margin-bottom:.32rem}
.sv-ep-name{
  font-family:var(--font-sans);font-size:1rem;font-weight:500;color:var(--text);
  line-height:1.3;flex:1;min-width:0;
}
.sv-ep-time{
  flex-shrink:0;font-family:var(--font-mono);font-size:.72rem;color:var(--muted);
  font-variant-numeric:tabular-nums;
}
.sv-ep-prog{position:absolute;left:0;right:0;bottom:0;height:3px;background:rgba(255,255,255,.2)}
.sv-ep-prog i{position:absolute;left:0;top:0;bottom:0;background:var(--gold);display:block}
.sv-ep-shot{flex-shrink:0;width:176px;aspect-ratio:16/9;border-radius:5px;overflow:hidden;position:relative;background:var(--bg2)}
.sv-ep-shot img{width:100%;height:100%;object-fit:cover;display:block;transition:filter .2s,transform .35s cubic-bezier(.22,.61,.36,1)}
.sv-ep-row:hover .sv-ep-shot img{filter:brightness(1.08);transform:scale(1.04)}
.sv-ep-no-still{display:flex;align-items:center;justify-content:center;height:100%;color:var(--muted);font-size:1.3rem}
.sv-ep-info{flex:1;min-width:0;padding-top:.05rem}
/* A synopsis that runs the full page width is unreadable; hold it near 62
   characters the way the services do. */
.sv-ep-desc{font-family:var(--font-sans);font-size:.85rem;line-height:1.55;color:var(--muted);max-width:62ch;display:-webkit-box;-webkit-line-clamp:2;-webkit-box-orient:vertical;overflow:hidden}
.card-note{padding:.75rem;font-size:.8rem;color:var(--muted);line-height:1.5;white-space:pre-wrap;word-break:break-word}
.card-actions{display:flex;gap:.4rem;padding:.5rem .75rem;border-top:1px solid var(--border)}
.card-actions button{flex:1;background:none;border:1px solid var(--border);color:var(--muted);padding:.3rem;font-family:inherit;font-size:.7rem;border-radius:3px;cursor:pointer}
.card-actions button:hover{border-color:var(--gold);color:var(--gold)}

/* ── LIST view ────────────────────────────────────────────────────────────── */
#file-list{display:none;padding:0 var(--gutter) 2rem;max-width:var(--shell);margin:0 auto}
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
/* Not flat black: the same wash the wall uses, thrown behind the picture at
   a fraction of its strength. Enough to tell you the room is still the room,
   not enough to compete with what is playing. */
#modal{display:none;position:fixed;inset:0;z-index:100;align-items:center;justify-content:center;flex-direction:column;padding:1rem;
  overflow-y:auto;
  background-color:rgba(0,0,0,.94);
  background-repeat:no-repeat;
  background-image:
    radial-gradient(38% 38% at center, color-mix(in srgb, var(--tint, rgb(154,125,74)) 17%, transparent), transparent 72%),
    radial-gradient(32% 32% at center, color-mix(in srgb, var(--tint, rgb(154,125,74)) 9%, transparent), transparent 74%);
  background-size:170% 170%,200% 200%;
  background-position:18% -8%,86% 104%;
  transition:background-color 420ms cubic-bezier(.4,0,.2,1),background-image 420ms cubic-bezier(.4,0,.2,1);
  animation:modalTintDrift 24s ease-in-out infinite alternate;
}
@keyframes modalTintDrift{
  0%   {background-position:18% -8%, 86% 104%}
  100% {background-position:58% 38%, 30% 55%}
}
@media(prefers-reduced-motion:reduce){#modal{transition:none;animation:none;background-position:35% 15%,65% 85%}}

/* ── Modal chrome ──────────────────────────────────────────────────────────
   While something is playing, the filename, action row and close button are
   chrome rather than content. A click on the picture dismisses them instead
   of pausing: #modal-tap covers the video but stops short of the native
   control bar, so scrubbing and play/pause still work. It is positioned from
   the video's measured rect, so the video keeps its place in the flex column. */
#modal-tap{position:fixed;display:none;cursor:pointer;z-index:1}
/* Subtitles now lives in the player's own right-click menu (see
   openPlayerCtxMenu) rather than as a button over the picture -- the
   element and its state-tracking logic stay (retrySubtitle, the local
   <track> wiring), only the on-video presence goes. */
#modal-cc-ov{display:none!important}
/* An icon over the picture rather than a labelled pill beside it -- same
   visual language as the pause card (see .pause-kicker): legibility from a
   shadow straight on the glyph, no background wash, resting at the same
   quiet white-at-62% as everything else that sits on top of the video. */
#modal-next-ov{
  position:fixed;display:none;z-index:3;
  background:none;border:none;cursor:pointer;line-height:1;
  color:rgba(255,255,255,.62);font-size:1.6rem;padding:.5rem;
  text-shadow:0 1px 3px rgba(0,0,0,.95), 0 2px 18px rgba(0,0,0,.85);
  transition:color .15s;
}
#modal-next-ov:hover{color:#fff}
#modal.mini #modal-next-ov{display:none}

/* ── Paused ────────────────────────────────────────────────────────────────
   A pause is a held moment, not an error state, so the frame stays visible
   and a quiet plate settles over its lower edge naming what you are watching
   and how much is left. It is pointer-transparent: the tap layer underneath
   still takes the click that resumes. ── */
#pause-card{
  position:fixed;display:none;z-index:2;pointer-events:none;
  align-items:flex-end;padding:0 0 1.5rem 1.6rem;
  opacity:0;transform:translateY(14px);
  transition:opacity .38s ease,transform .38s cubic-bezier(.2,.7,.3,1);
}
#pause-card.on{display:flex;opacity:1;transform:translateY(0)}
/* A paused frame is a held moment, not one still in motion -- the modal's
   own slow tint drift holds too, rather than visibly carrying on around a
   picture that isn't. (The now-playing wash — see #tint-wash — goes further
   and fades out entirely on pause, handled by its own show condition rather
   than this.) See showPauseCard. */
html.media-paused #modal{animation-play-state:paused}
/* Legibility comes from a shadow on the type rather than a wash over the
   picture, so nothing is hidden behind it. */
.pause-in{
  max-width:min(560px,72%);
  text-shadow:0 1px 3px rgba(0,0,0,.95), 0 2px 18px rgba(0,0,0,.85);
}
.pause-kicker{
  font-family:var(--font-mono);font-size:.66rem;letter-spacing:.2em;text-transform:uppercase;
  color:rgba(255,255,255,.62);margin-bottom:.5rem;display:flex;align-items:center;gap:.55rem;
}
.pause-kicker::before{
  content:"";width:9px;height:11px;flex-shrink:0;
  border-left:3px solid rgba(255,255,255,.62);border-right:3px solid rgba(255,255,255,.62);
}
.pause-t{font-family:var(--font-serif);font-size:1.7rem;font-weight:700;color:#fff;line-height:1.15;text-wrap:balance}
.pause-s{font-family:var(--font-sans);font-size:.88rem;color:rgba(255,255,255,.74);margin-top:.35rem}
/* A 340px corner player has no room for full-size type -- same idea as the
   narrow-viewport shrink below, just smaller again to match. */
#modal.mini #pause-card{padding:0 0 .45rem .55rem}
#modal.mini .pause-in{max-width:92%}
#modal.mini .pause-kicker{font-size:.5rem;gap:.3rem;margin-bottom:.2rem}
#modal.mini .pause-kicker::before{width:6px;height:8px;border-width:2px}
#modal.mini .pause-t{font-size:.78rem;line-height:1.2}
#modal.mini .pause-s{font-size:.6rem;margin-top:.15rem}
@media(prefers-reduced-motion:reduce){#pause-card{transition:none}}
@media(max-width:600px){
  #pause-card{padding:0 0 1rem 1.05rem}
  .pause-t{font-size:1.15rem}
  .pause-s{font-size:.78rem}
  .pause-in{max-width:88%}
}
#modal.chrome-off #modal-close,
#modal.chrome-off #modal-filename,
#modal.chrome-off #modal-actions{opacity:0;pointer-events:none}
#modal-close,#modal-filename,#modal-actions{transition:opacity .2s ease}
#modal.open{display:flex}
/* ── Minimize ─────────────────────────────────────────────────────────────
   A small corner player rather than a real (separate-window) Picture-in-
   Picture: shrinking #modal down to a fixed box, instead of covering the
   viewport, is what actually frees the rest of the page back up -- nothing
   under it needs pointer-events tricks, there is just no full-size overlay
   sitting over it any more. Playback is untouched; only the box changes. ── */
#modal.mini{
  inset:auto;left:auto;right:1rem;bottom:1rem;top:auto;
  width:min(390px,34vw);height:auto;padding:0;
  background-color:#000;background-image:none;animation:none;
  border-radius:8px;box-shadow:0 14px 44px rgba(0,0,0,.6);z-index:150;
}
#modal.mini #modal-filename,
#modal.mini #modal-actions{display:none}
/* The close button stays, restyled to fit and hover-revealed rather than
   permanent -- clicking the video itself restores it to full size, so a
   quiet way to cancel outright without restoring first has to live
   somewhere else on the same small box. */
#modal.mini #modal-close{
  top:.3rem;right:.3rem;width:1.5rem;height:1.5rem;font-size:1.1rem;
  display:flex;align-items:center;justify-content:center;
  /* Above #modal-tap's z-index:1 -- otherwise the tap-catcher, added after
     this button in the markup, would paint over it and swallow the click. */
  background:rgba(10,10,10,.72);border:1px solid rgba(255,255,255,.22);border-radius:50%;color:#fff;
  opacity:0;transition:opacity .18s ease;z-index:4;
}
/* Previous/play/next: centred over the middle of the mini box, same reveal
   as the close button in the corner -- one selector list, so all four show
   and hide together rather than the transport controls having their own
   independent timing. */
#modal-mini-controls{display:none}
#modal.mini #modal-mini-controls{
  position:absolute;top:50%;left:50%;transform:translate(-50%,-50%);
  display:flex;gap:.5rem;z-index:4;
}
#modal.mini #modal-mini-controls button{
  width:1.7rem;height:1.7rem;border-radius:50%;padding:0;
  display:flex;align-items:center;justify-content:center;
  background:rgba(10,10,10,.72);border:1px solid rgba(255,255,255,.22);color:#fff;
  font-size:.8rem;cursor:pointer;
  opacity:0;transition:opacity .18s ease,border-color .15s,color .15s;
}
#modal.mini #modal-mini-controls button svg{width:.85rem;height:.85rem;display:block}
#modal.mini:hover #modal-close,#modal.mini #modal-close:focus-visible,
#modal.mini:hover #modal-mini-controls button,#modal.mini #modal-mini-controls button:focus-visible{opacity:1}
#modal.mini #modal-mini-controls button:hover{border-color:var(--gold);color:var(--gold)}
#modal.mini #modal-mini-controls button:disabled{opacity:.25!important;cursor:default}
#modal.mini video{max-width:100%;max-height:100%;border-radius:8px}
#modal-close{position:absolute;top:1rem;right:1.25rem;background:none;border:none;color:var(--muted);font-size:1.5rem;cursor:pointer;line-height:1}
#modal-close:hover{color:var(--text)}
#modal video{max-width:100%;max-height:82vh;border-radius:4px;background:#000}
/* Right-clicking the video focuses it in some browsers, which otherwise
   draws the browser's own default focus rectangle (a harsh white/blue box)
   right over the picture for no interaction reason worth marking that hard. */
#modal video:focus-visible{outline:none;box-shadow:0 0 0 2px color-mix(in srgb, var(--gold) 55%, transparent)}
#modal-note{max-width:640px;width:100%;background:var(--bg2);border-radius:6px;padding:1.5rem;font-size:.9rem;line-height:1.7;white-space:pre-wrap;word-break:break-word;max-height:70vh;overflow-y:auto}
/* Floating below the video (position computed in syncTapArea) rather than
   stacked under it in normal flow -- that way #modal centers the video
   alone, so the gap above it and the gap below it come out equal instead of
   the caption's own height quietly eating into what looks like "the bottom
   margin" while never touching the top. */
#modal-filename{position:fixed;font-size:.8rem;color:var(--muted);text-align:center;transform:translateX(-50%);max-width:90vw}
#modal-actions{position:fixed;display:flex;gap:.75rem;transform:translateX(-50%)}
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

<div id="tint"></div>
<div id="tint-wash"></div>
<div id="ctx-menu" role="menu"></div>
<div id="topbar">
<header>
  <button id="topbar-back" aria-label="Back to library">← all</button>
  <span class="wordmark">Stream<span>Videos</span></span>
  <span class="spacer"></span>
  <button class="view-btn" id="btn-view" title="Switch between poster and list view" tabindex="0">⊞</button>
  <button class="header-btn" id="theme-toggle-btn" title="Toggle light/dark theme" tabindex="0">light</button>
  <a href="/qr.png" target="_blank"><button class="header-btn" tabindex="0">qr</button></a>
  <button class="header-btn mgmt" id="search-open-btn" tabindex="0">search</button>
</header>
</div>
<nav id="section-tags"></nav>

<div id="search-overlay">
  <div id="search-box">
    <div id="search-top">
      <input id="search-input" type="text" placeholder="search for a movie or show…" autocomplete="off">
      <button id="search-close-btn">&times;</button>
    </div>
    <div id="search-kind">
      <button class="sk-btn active" data-kind="media">Films &amp; TV</button>
      <button class="sk-btn" data-kind="music">Music videos</button>
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
  <div id="modal-mini-controls">
    <button id="modal-mini-prev" aria-label="Previous"><svg viewBox="0 0 24 24" fill="currentColor"><path d="M6 6h2v12H6zm3.5 6l8.5 6V6z"/></svg></button>
    <button id="modal-mini-play" aria-label="Play/Pause"><svg viewBox="0 0 24 24" fill="currentColor"><path d="M6 19h4V5H6v14zm8-14v14h4V5h-4z"/></svg></button>
    <button id="modal-mini-next" aria-label="Next"><svg viewBox="0 0 24 24" fill="currentColor"><path d="M6 18l8.5-6L6 6v12zM16 6v12h2V6h-2z"/></svg></button>
  </div>
  <div id="modal-tap"></div>
  <div id="pause-card"></div>
  <button id="modal-cc-ov" style="display:none">CC get subtitles</button>
  <button id="modal-next-ov" style="display:none" aria-label="Next episode" title="Next episode">⏭</button>
  <video id="modal-video" controls crossorigin="anonymous" style="display:none"></video>
  <div id="modal-note" style="display:none"></div>
  <div id="modal-filename"></div>
  <div id="modal-actions">
    <select id="modal-quality" style="display:none"></select>
    <button id="modal-dl">↓ download</button>
    <button id="modal-del">✕ delete</button>
    <button id="modal-copy" style="display:none">⎘ copy text</button>
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
var progressMap = {};       // key → {position,duration,updated} for every part-watched file
var playlists = [];         // every playlist, as returned by /playlists
var currentPlaylist = null; // the playlist detail view, when one is open
var lastLocalSig = null;    // last /local/files payload, so a poll that changed nothing repaints nothing
var lastCloudSig = null;
var progressKey = null;     // key of the file modalVideo is currently tracking, or null if nothing to resume
var lastProgressPost = 0;   // ms timestamp of the last /api/progress POST, for throttling
var lastFocusedCard = null; // card/row that was focused before the modal opened, restored on close
var modalInHistory = false; // true while the open player owns a history entry
var closingFromPop = false; // guards closeModal against calling back() during a popstate
// closeModal's own history.back() (below) triggers a popstate just like any
// other back-navigation does, and that popstate has to actually finish the
// close even while minimized -- unlike an unrelated one (browsing back out
// of a show page), which must leave a minimized player alone. This is what
// tells the two apart: set right before that self-triggered history.back(),
// checked by the popstate handler.
var closingIntentionally = false;
var currentNextKey  = null; // key of the next episode, or null if none
// upNextQueue: an ad-hoc "play after this" list, independent of both a
// playlist's own queue and a show's next episode -- queued from the wall
// while something else plays (minimized or not), consulted first when the
// current video ends (see the 'ended' handler), and cleared when the player
// closes; it doesn't survive past that.
var upNextQueue = [];
// Whether a queued session is in progress at all -- distinct from
// upNextQueue.length, which hits 0 the moment the *last* queued item starts
// playing (queueNext shifts it off before opening it). Without this, that
// last track would read as "nothing queued" and drop the wash effect right
// before the one item it was queued for actually played -- see
// updateNowPlayingWash.
var queueSessionActive = false;
function queueAdd(key, front) {
  var i = upNextQueue.indexOf(key);
  if (i !== -1) upNextQueue.splice(i, 1); // re-queuing moves it rather than duplicating
  if (front) upNextQueue.unshift(key); else upNextQueue.push(key);
  queueSessionActive = true;
  updateNowPlayingWash();
  syncMiniControls();
}
function queueNext() {
  if (!upNextQueue.length) return false;
  openFile(null, upNextQueue.shift(), false);
  return true;
}

// peekNext/goNext: the same queue > playlist > next-episode priority the
// 'ended' handler uses, exposed for the Next button and the player's own
// right-click menu -- both used to only ever look at currentNextKey, so a
// queued item with no natural next-episode behind it (a movie, say, or the
// last episode of a season) never showed a working Next at all.
function peekNext() {
  if (upNextQueue.length) return upNextQueue[0];
  if (playlistQueue && playlistPos < playlistQueue.length - 1) return playlistQueue[playlistPos + 1].key;
  return currentNextKey;
}
function goNext() {
  if (queueNext()) return true;
  if (playlistNext()) return true;
  if (currentNextKey) { openFile(null, currentNextKey, false); return true; }
  return false;
}

// hasActiveQueue: is there a sequence in progress at all -- a queued
// session (see queueSessionActive above) or a playlist actually playing
// (its very last item included) -- rather than just one thing paused in a
// corner. See the #tint-wash CSS comment for why that gates the effect.
function hasActiveQueue() {
  return queueSessionActive || playlistQueue !== null;
}

// nowPlayingTint/setNowPlayingTint: the currently-playing track's own
// colour, tracked independently of --tint (which a hovered wall card keeps
// overriding, then settles back to the wall's hero colour on mouseleave —
// never back to whatever's actually playing). Set once per track from
// openFile; feeds #tint-wash via --tint-playing.
var nowPlayingTint = null;
function setNowPlayingTint(rgb) {
  if (!rgb) return;
  nowPlayingTint = rgb;
  document.documentElement.style.setProperty('--tint-playing', 'rgb(' + rgb + ')');
  updateNowPlayingWash();
}

// tintWashActive mirrors #tint-wash's own on/off state -- checked by
// tintFrom/tintReset below to suspend the ordinary per-card hover tint while
// the wash is showing, so a hovered card never fights it for the same job.
var tintWashActive = false;

// updateNowPlayingWash shows the effect only for: minimized and actually
// playing right now (not paused, not just open full-size), an active
// queue/playlist behind it, and the home wall specifically -- a show, movie
// or playlist page leaves its own tint completely alone, unchanged from
// before this existed.
function updateNowPlayingWash() {
  var el = document.getElementById('tint-wash');
  if (!el) return;
  var onHomeWall = !currentShow && !currentMovieDetail && !currentPlaylist;
  var show = modal.classList.contains('open') && modal.classList.contains('mini') &&
    !modalVideo.paused && hasActiveQueue() && onHomeWall;
  tintWashActive = show;
  el.classList.toggle('on', show);
  // The wash is the only accent while it's showing -- see html.wash-active
  // hiding #tint outright, not just leaving it unchanging underneath.
  document.documentElement.classList.toggle('wash-active', show);
}
// Safari private mode throws on localStorage rather than returning null, and
// every one of these calls used to sit ahead of a render.
function lsGet(k) { try { return localStorage.getItem(k); } catch (e) { return null; } }
function lsSet(k, v) { try { localStorage.setItem(k, v); } catch (e) {} }

var libSource = lsGet('stream-source') || (HAS_LOCAL_LIB ? 'local' : 'cloud'); // 'cloud' | 'local'

var msg        = document.getElementById('msg');
var fileGrid   = document.getElementById('file-grid');
var musicGrid  = document.getElementById('music-grid');
var musicHeading = document.getElementById('music-heading');
var fileList   = document.getElementById('file-list');
var topbarBack = document.getElementById('topbar-back');
// One button for all three detail pages -- whichever is open decides where
// "back" actually goes, same as before, just triggered from a fixed spot in
// the header instead of a per-page copy of the button.
function backFromDetail() {
  if (currentShow) { closeShow(); return; }
  if (currentMovieDetail) { closeMovieDetail(); return; }
  if (currentPlaylist) { closePlaylist(); return; }
}
if (topbarBack) topbarBack.addEventListener('click', backFromDetail);
var modal      = document.getElementById('modal');
var modalVideo = document.getElementById('modal-video');
var modalNote  = document.getElementById('modal-note');
var modalFname = document.getElementById('modal-filename');
var modalActions = document.getElementById('modal-actions');
var modalDl    = document.getElementById('modal-dl');
var modalDel   = document.getElementById('modal-del');
var modalCopy  = document.getElementById('modal-copy');
var modalCC    = document.getElementById('modal-cc-ov');
var modalMeta  = document.getElementById('modal-meta');
var modalNext  = document.getElementById('modal-next-ov');
var modalMiniPrev = document.getElementById('modal-mini-prev');
var modalMiniPlay = document.getElementById('modal-mini-play');
var modalMiniNext = document.getElementById('modal-mini-next');
var modalQuality = document.getElementById('modal-quality');
var btnView = document.getElementById('btn-view');

// ── View toggle ────────────────────────────────────────────────────────────
// One control rather than a pair. It shows the view you are in, and clicking
// it moves to the other — the two glyphs are deliberately close in weight so
// the button reads as one thing changing state, not two different buttons.
function setView(v) {
  viewMode = v;
  lsSet('stream-view', v);
  if (v === 'list') {
    fileGrid.style.display = 'none';
    musicGrid.style.display = 'none';
    musicHeading.style.display = 'none';
    fileList.style.display = 'block';
    btnView.textContent = '\u2261';
    btnView.title = 'List view \u2014 switch to posters';
    renderList(currentFiles);
  } else {
    fileGrid.style.display = 'grid';
    fileList.style.display = 'none';
    btnView.textContent = '\u229e';
    btnView.title = 'Poster view \u2014 switch to list';
    renderGrid(currentFiles);
  }
}
btnView.addEventListener('click', function(){ setView(viewMode === 'list' ? 'poster' : 'list'); });
var viewMode = lsGet('stream-view') === 'list' ? 'list' : 'poster';
setView(viewMode);

// ── Theme toggle ───────────────────────────────────────────────────────────
var themeToggleBtn = document.getElementById('theme-toggle-btn');
function setTheme(t) {
  document.body.classList.toggle('light', t === 'light');
  themeToggleBtn.textContent = t === 'light' ? 'dark' : 'light';
  lsSet('stream-theme', t);
}
themeToggleBtn.addEventListener('click', function(){
  setTheme(document.body.classList.contains('light') ? 'dark' : 'light');
});
setTheme(lsGet('stream-theme') || 'dark');

// ── Remote vs local: hide management UI when viewing through the tunnel ──────
document.body.classList.toggle('remote', !IS_LOCAL);

// ── Cloud / Local library source toggle ─────────────────────────────────────
// The local library reuses the exact same grouping/card rendering as the
// cloud library (groupBySeries, renderGrid/renderList, openFile) — it's just
// a different array behind currentFiles, with playback/handoff branching on
// f.is_local inside openFile instead of separate markup.
// The bar no longer carries a source button. setSource is still the single
// place the two libraries swap, so it stays reachable from the console and
// from the stored preference on load.
function setSource(src) {
  libSource = src;
  lsSet('stream-source', src);
  currentShow = null;
  currentMovieDetail = null;
  if (src === 'local') {
    currentFiles = localFiles;
    setView(viewMode);
    loadLocalFiles();
  } else {
    currentFiles = cloudFiles;
    setView(viewMode);
  }
}

// ── Local library (LAN-served, no conversion, TMDB/LLM-enriched) ────────────
// loadProgressMap pulls every saved position in one request. The library
// needs all of them up front to build its continue-watching row; asking per
// tile would be one round trip per file on screen.
function loadPlaylists() {
  return fetch('/playlists', {credentials:'include'})
    .then(function(r){ return r.ok ? r.json() : []; })
    .then(function(pl){ playlists = pl || []; })
    .catch(function(){ playlists = []; });
}

// playlistItems resolves a playlist's stored keys back to library entries,
// dropping any whose file has since gone. Order is the playlist's, not the
// library's.
function playlistItems(pl) {
  if (!pl) return [];
  var byKey = {};
  currentFiles.forEach(function(f){ byKey[f.key] = f; });
  return pl.items.map(function(k){ return byKey[k]; }).filter(Boolean);
}

function playlistAction(body) {
  return fetch('/playlists', {
    method:'POST', credentials:'include',
    headers:{'Content-Type':'application/json'},
    body: JSON.stringify(body)
  }).then(function(r){ return r.ok ? r.json() : Promise.reject(r.status); });
}

// ── Right-click menu ───────────────────────────────────────────────────────
var ctxMenu = document.getElementById('ctx-menu');

function closeCtxMenu() {
  if (ctxMenu) ctxMenu.classList.remove('open');
}

function openCtxMenu(e, f) {
  if (!ctxMenu || !f) return;
  e.preventDefault();
  e.stopPropagation();
  ctxMenu.classList.remove('ctx-compact');
  var rows = [
    '<div class="ctx-head">' + esc(f.title || f.name) + '</div>',
    '<button class="ctx-item" data-act="play">Play</button>'
  ];
  if (!f.is_note) {
    // Queuing only means something once there's a "current" for it to play
    // after -- with nothing playing, both just start it immediately instead
    // of silently queuing something that would otherwise never be reached.
    rows.push('<button class="ctx-item" data-act="play-next">Play next</button>');
    rows.push('<button class="ctx-item" data-act="queue-add">Add to queue</button>');
  }
  if (currentPlaylist) {
    rows.push('<button class="ctx-item" data-act="pl-remove">Remove from this playlist</button>');
  }
  rows.push('<div class="ctx-sep"></div>');
  rows.push('<div class="ctx-head">Add to playlist</div>');
  playlists.forEach(function(pl){
    var has = pl.items.indexOf(f.key) !== -1;
    rows.push('<button class="ctx-item" data-act="add" data-id="' + esc(pl.id) + '"' +
      (has ? ' disabled style="opacity:.45"' : '') + '>' +
      '<span>' + esc(pl.name) + '</span><span class="n">' + (has ? 'added' : pl.items.length) + '</span></button>');
  });
  rows.push('<button class="ctx-item ctx-new" data-act="new">+ New playlist\u2026</button>');
  ctxMenu.innerHTML = rows.join('');

  ctxMenu.classList.add('open');
  // Placed at the pointer, then nudged back inside the viewport.
  var r = ctxMenu.getBoundingClientRect();
  var x = Math.min(e.clientX, window.innerWidth - r.width - 8);
  var y = Math.min(e.clientY, window.innerHeight - r.height - 8);
  ctxMenu.style.left = Math.max(8, x) + 'px';
  ctxMenu.style.top = Math.max(8, y) + 'px';

  Array.prototype.forEach.call(ctxMenu.querySelectorAll('.ctx-item'), function(b){
    b.addEventListener('click', function(){
      var act = b.dataset.act;
      closeCtxMenu();
      if (act === 'play') { openFile(null, f.key, false, true); return; }
      if (act === 'play-next') {
        if (modal.classList.contains('open')) { queueAdd(f.key, true); showToast('Playing next'); }
        else { openFile(null, f.key, false, true); }
        return;
      }
      if (act === 'queue-add') {
        if (modal.classList.contains('open')) { queueAdd(f.key, false); showToast('Added to queue'); }
        else { openFile(null, f.key, false, true); }
        return;
      }
      if (act === 'add') {
        playlistAction({action:'add', id:b.dataset.id, key:f.key}).then(function(){
          return loadPlaylists();
        }).then(function(){
          showToast('Added to playlist');
          renderGrid(currentFiles);
        });
        return;
      }
      if (act === 'pl-remove' && currentPlaylist) {
        playlistAction({action:'remove', id:currentPlaylist.id, key:f.key}).then(function(pl){
          return loadPlaylists().then(function(){
            currentPlaylist = playlists.filter(function(x){ return x.id === pl.id; })[0] || null;
            renderGrid(currentFiles);
          });
        });
        return;
      }
      if (act === 'new') {
        var name = prompt('Name this playlist');
        if (name === null) return;
        playlistAction({action:'create', name:name, key:f.key}).then(function(){
          return loadPlaylists();
        }).then(function(){
          showToast('Playlist created');
          renderGrid(currentFiles);
        });
      }
    });
  });
}

document.addEventListener('click', function(e){
  if (ctxMenu && !ctxMenu.contains(e.target)) closeCtxMenu();
});
document.addEventListener('keydown', function(e){ if (e.key === 'Escape') closeCtxMenu(); });
window.addEventListener('scroll', closeCtxMenu, {passive:true});

function loadProgressMap() {
  return fetch('/api/progress?all=1', {credentials:'include'})
    .then(function(r){ return r.json(); })
    .then(function(m){ progressMap = m || {}; })
    .catch(function(){ progressMap = {}; });
}

// fmtLeft turns the seconds remaining into "1h 32m left". The progress record
// already carries the media's real duration (the <video> reported it), so this
// never needs TMDB's runtime, which is missing for most of the library.
function fmtLeft(p) {
  var left = Math.max(0, Math.round((p.duration || 0) - (p.position || 0)));
  if (!left) return '';
  var h = Math.floor(left/3600), m = Math.round((left%3600)/60);
  if (h > 0) return h + 'h ' + m + 'm left';
  if (m > 0) return m + 'm left';
  return 'less than a minute left';
}

// artFor returns the best available still for a file. TMDB art first, then
// the frame the server extracts from the video itself, which is all an
// unmatched file (28% of the library) will ever have.
function artFor(f, wide) {
  if (wide) {
    if (f.episode_still) return f.episode_still;
    if (f.tmdb_backdrop) return f.tmdb_backdrop;
  } else {
    if (f.poster_url) return f.poster_url;
  }
  if (f.poster_url) return f.poster_url;
  if (f.thumb_url) return f.thumb_url;
  if (f.is_local) return '/local/thumbnail?path=' + encodeURIComponent(f.key);
  return '';
}

function progressOf(f) {
  var p = progressMap[f.key];
  return (p && p.duration > 0 && p.position > 5) ? p : null;
}

// continueItems is every part-watched file, most recently touched first. The
// server already evicts anything finished or barely started, so whatever is
// in the map is genuinely resumable.
function continueItems(files) {
  return files.filter(progressOf).sort(function(a,b){
    return (progressMap[b.key].updated||0) - (progressMap[a.key].updated||0);
  });
}

function loadLocalFiles() {
  fetch('/local/files', {credentials:'include'})
    .then(function(r){ return r.json(); })
    .then(function(items){
      var sig = JSON.stringify(items || []);
      var changed = sig !== lastLocalSig;
      lastLocalSig = sig;
      localFiles = items || [];
      if (libSource === 'local' && changed) {
        currentFiles = localFiles;
        Promise.all([loadProgressMap(), loadPlaylists()]).then(function(){
          if (viewMode === 'list') { renderList(currentFiles); } else { renderGrid(currentFiles); }
        });
      }
      // Poll only while some file has not yet been through enrichment at all.
      // Asking "has it got a title?" never goes false for the files TMDB has
      // no match for, which is what made the library repaint every 4 seconds.
      var anyPending = localFiles.some(function(f){ return !f.enriched; });
      if (anyPending) setTimeout(loadLocalFiles, 4000);
    })
    .catch(function(err){
      // A 403 through the tunnel, or any non-JSON body, used to reject here
      // with nothing downstream — the page simply stayed empty.
      console.warn('local library load failed:', err);
      if (libSource === 'local' && !currentFiles.length) {
        fileGrid.innerHTML = '<div style="color:var(--muted);font-size:.85rem;grid-column:1/-1;padding:2.5rem 0">' +
          'Could not reach the local library. It is only served on the home network.</div>';
      }
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
    // token (path-scoped, 12h) minted just for this handoff.
    // We open an XSPF playlist rather than the bare video URL so VLC loads
    // the server-extracted WebVTT subtitle alongside the video — VLC on iOS
    // does not reliably render embedded ASS subtitle tracks over HTTP.
    fetch('/local/file-token?path=' + encodeURIComponent(f.key), {credentials:'include'})
      .then(function(r){ return r.json(); })
      .then(function(d){
        var xspfUrl = location.origin + '/local/xspf?path=' + encodeURIComponent(f.key) + '&tok=' + encodeURIComponent(d.token);
        window.location.href = 'vlc://' + xspfUrl;
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
      var sig = JSON.stringify(files || []);
      var changed = sig !== lastCloudSig;
      lastCloudSig = sig;
      cloudFiles = files || [];
      if (libSource === 'cloud' && changed) {
        currentFiles = cloudFiles;
        loadProgressMap().then(function(){
          if (viewMode === 'list') { renderList(currentFiles); } else { renderGrid(currentFiles); }
        });
      }
      // Poll if any are processing
      var anyProc = cloudFiles.some(function(f){ return f.proc_state === 'processing'; });
      if (anyProc) setTimeout(loadFiles, 3000);
    })
    .catch(function(err){ console.warn('library load failed:', err); });
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
  stagger(card);
  if (f.is_note) {
    card.innerHTML = '<div class="card-note">'+esc(f.snippet||f.name)+'</div>' +
      '<div class="card-actions"><button onclick="openFile(event,\''+esc(f.key)+'\',true)">▶</button><button onclick="deleteFile(event,\''+esc(f.key)+'\')">✕</button></div>';
    card.addEventListener('click', function(e){ if (e.target.tagName==='BUTTON') return; openFile(e, f.key, true); });
    return card;
  }
  var title = f.title || f.name;
  var stillIdentifying = f.is_local && !f.title && !f.tmdb_id;
  var img = artFor(f, false);
  var cover = f.proc_state==='processing'
    ? '<div class="pc-cover"><div class="pc-proc">⟳</div></div>'
    : f.proc_state==='error'
      ? '<div class="pc-cover" style="background:rgba(90,20,20,.7)"><div class="pc-proc" style="color:#e06c6c">✕</div></div>'
      : stillIdentifying
        ? '<div class="pc-cover" style="display:flex;align-items:center;justify-content:center"><span style="font-size:1.6rem;color:var(--muted)">⟳</span></div>'
      : img
        ? '<div class="pc-cover"><img src="'+esc(img)+'" loading="lazy" crossorigin="anonymous" onerror="pcNoArt(this)">'+posterOverlay(title, f.year)+progressBar(f)+cardMoreHTML()+'</div>'
        : '<div class="pc-cover">'+noArtCard(title, f)+progressBar(f)+cardMoreHTML()+'</div>';
  card.innerHTML = cover;
  card.addEventListener('click', function(e){
    if (e.target.tagName==='BUTTON') return;
    if (f.proc_state==='processing') return;
    openFile(e, f.key, false);
  });
  card.addEventListener('contextmenu', function(e){ openCtxMenu(e, f); });
  bindTint(card, img);
  wireCardMore(card, f);
  return card;
}

// ── Ambient tint ───────────────────────────────────────────────────────────
// TMDB serves its artwork with Access-Control-Allow-Origin: *, so the poster
// can be read back off a canvas and its colour reused. Results are cached by
// URL — the read only ever happens once per image per session.
var tintCache = {};
var tintEl = null;
var tintPending = null;
var tintDefault = null;

function tintLayer() {
  if (!tintEl) tintEl = document.getElementById('tint');
  return tintEl;
}

// A flat average of a poster is almost always mud, because most of a poster is
// dark, so each pixel is weighted by its own saturation — that pulls out the
// colour a person would actually name. The earlier version bailed to null when
// too few pixels cleared its filters, which silently left the previous poster's
// colour on screen; a monochrome or very dark poster now falls back to a plain
// mean instead of giving up.
function dominantColour(img) {
  var n = 32;
  var c = document.createElement('canvas');
  c.width = n; c.height = n;
  var ctx = c.getContext('2d', {willReadFrequently: true});
  ctx.drawImage(img, 0, 0, n, n);
  var d;
  try { d = ctx.getImageData(0, 0, n, n).data; } catch (e) { return null; }

  var r = 0, g = 0, b = 0, w = 0;      // saturation-weighted
  var fr = 0, fg = 0, fb = 0, fw = 0;  // plain mean, as the fallback
  for (var i = 0; i < d.length; i += 4) {
    var R = d[i], G = d[i+1], B = d[i+2];
    var mx = Math.max(R, G, B), mn = Math.min(R, G, B);
    var l = (mx + mn) / 510;
    if (l < 0.05 || l > 0.97) continue;
    fr += R; fg += G; fb += B; fw++;
    var s = mx === mn ? 0 : (mx - mn) / mx;
    if (s < 0.08) continue;
    var weight = s * s;
    r += R * weight; g += G * weight; b += B * weight; w += weight;
  }
  if (w > 0.35) { r /= w; g /= w; b /= w; }
  else if (fw > 0) { r = fr / fw; g = fg / fw; b = fb / fw; }
  else return null;

  return normaliseAccent(r, g, b);
}

// Push every extracted colour to the same saturation and lightness, so the
// accent is a hue that changes and not a brightness that flickers. Without
// this a dark poster produces a wash you cannot see and a bright one produces
// glare.
function normaliseAccent(r, g, b) {
  r /= 255; g /= 255; b /= 255;
  var mx = Math.max(r, g, b), mn = Math.min(r, g, b);
  var h = 0, s = 0, l = (mx + mn) / 2;
  if (mx !== mn) {
    var dd = mx - mn;
    s = l > 0.5 ? dd / (2 - mx - mn) : dd / (mx + mn);
    if (mx === r)      h = ((g - b) / dd + (g < b ? 6 : 0)) / 6;
    else if (mx === g) h = ((b - r) / dd + 2) / 6;
    else               h = ((r - g) / dd + 4) / 6;
  }
  s = Math.min(0.72, Math.max(0.38, s));
  l = 0.52;
  function hue(p, q, t) {
    if (t < 0) t += 1;
    if (t > 1) t -= 1;
    if (t < 1/6) return p + (q - p) * 6 * t;
    if (t < 1/2) return q;
    if (t < 2/3) return p + (q - p) * (2/3 - t) * 6;
    return p;
  }
  var q = l < 0.5 ? l * (1 + s) : l + s - l * s;
  var pp = 2 * l - q;
  return Math.round(hue(pp, q, h + 1/3) * 255) + ',' +
         Math.round(hue(pp, q, h) * 255) + ',' +
         Math.round(hue(pp, q, h - 1/3) * 255);
}

function withTint(url, cb) {
  if (!url) return;
  if (tintCache[url] !== undefined) { cb(tintCache[url]); return; }
  var img = new Image();
  img.crossOrigin = 'anonymous';
  img.onload = function(){
    var rgb = null;
    try { rgb = dominantColour(img); } catch (e) { rgb = null; }
    tintCache[url] = rgb;
    if (rgb) cb(rgb);
  };
  img.onerror = function(){ tintCache[url] = null; };
  img.src = url;
}

function applyTint(rgb) {
  // Set on the root, not on the tint layer: the header paints the same
  // gradient so the wash is continuous across it, and a custom property set
  // on #tint would not inherit sideways to a sibling.
  // Wrapped in rgb(...): --tint is registered as an actual <color> (see the
  // @property rule by #tint's CSS) specifically so it can transition
  // smoothly, and a registered <color> property has to hold a real color
  // value rather than the bare "r,g,b" triplet this produces.
  if (rgb) document.documentElement.style.setProperty('--tint', 'rgb(' + rgb + ')');
}

// Hovering moves the wash; leaving settles it back to whatever the banner is
// showing, so the page always has a resting colour rather than snapping to
// grey between tiles. Suspended entirely while the now-playing wash is
// showing (see tintWashActive) -- that wash is already saying what's
// playing in the same visual language, and a hovered card overriding it
// would read as noise, not as another piece of information.
function tintFrom(url) {
  if (!url || tintWashActive) return;
  clearTimeout(tintPending);
  tintPending = setTimeout(function(){ withTint(url, applyTint); }, 40);
}

function tintReset() {
  if (tintWashActive) return;
  clearTimeout(tintPending);
  tintPending = setTimeout(function(){
    if (tintDefault) withTint(tintDefault, applyTint);
  }, 260);
}

// Wire a card to the wash. Pointer and keyboard both count as "in focus" —
// this is the same signal that reveals the caption.
function bindTint(el, url) {
  if (!url) return;
  el.addEventListener('mouseenter', function(){ tintFrom(url); });
  el.addEventListener('focus', function(){ tintFrom(url); });
  el.addEventListener('mouseleave', tintReset);
  el.addEventListener('blur', tintReset);
}

// posterOverlay is the caption that appears on hover/focus (and always, on
// touch — see the 600px block). Kept out of the layout flow so the wall is
// pure art at rest.
function posterOverlay(title, year, extra) {
  var line = [year || '', extra || ''].filter(Boolean).join('  \u00b7  ');
  return '<div class="pc-ovl"><div class="pc-ovl-t">'+esc(title||'')+'</div>' +
    (line ? '<div class="pc-ovl-y">'+esc(line)+'</div>' : '') + '</div>';
}

// cardMoreHTML/wireCardMore: the hover/tap overflow trigger a playable-video
// card gets, opening the same menu a right-click does (see openCtxMenu). A
// <div role="button"> rather than a real <button> so it nests safely inside
// a card that is itself a <button> (continue-watching).
function cardMoreHTML() {
  return '<div class="card-more" role="button" tabindex="0" aria-label="More options">\u22ef</div>';
}
function wireCardMore(card, f) {
  var btn = card.querySelector('.card-more');
  if (!btn) return;
  var open = function(e){ e.stopPropagation(); openCtxMenu(e, f); };
  btn.addEventListener('click', open);
  btn.addEventListener('keydown', function(e){
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); open(e); }
  });
}

// Seasons once there is more than one, episodes otherwise — the phrasing the
// services settled on, and the one that answers "how much is there".
function seriesCount(show) {
  var seasons = {};
  show.episodes.forEach(function(e){ if (e.season) seasons[e.season] = 1; });
  var n = Object.keys(seasons).length;
  if (n > 1) return n + ' seasons';
  return show.episodes.length + ' episode' + (show.episodes.length === 1 ? '' : 's');
}

// noArtCard stands in for a poster when TMDB never matched the file — 28% of
// the library. Setting the title rather than showing a broken-image glyph
// means an artless tile still reads as a title, not as an error.
function noArtCard(title, f) {
  var kind = f && f.media_type === 'tv' ? 'Series'
           : f && f.is_music_video ? 'Music video'
           : f && f.media_type === 'movie' ? 'Film' : 'No artwork';
  return '<div class="pc-noart"><div class="pc-noart-t">'+esc(title||'')+'</div>' +
    '<div class="pc-noart-k">'+kind+'</div></div>';
}

function pcNoArt(img) {
  var card = img.closest('.card');
  var t = card ? card.querySelector('.pc-ovl-t') : null;
  img.outerHTML = '<div class="pc-noart"><div class="pc-noart-t">' +
    esc(t ? t.textContent : '') + '</div><div class="pc-noart-k">No artwork</div></div>';
}

function progressBar(f) {
  var p = progressOf(f);
  if (!p) return '';
  var pct = Math.max(2, Math.min(100, (p.position / p.duration) * 100));
  return '<div class="pc-prog"><i style="width:'+pct.toFixed(1)+'%"></i></div>';
}

function renderShowCard(show) {
  var card = document.createElement('div');
  card.className = 'card';
  card.tabIndex = 0;
  stagger(card);
  var ep = show.episodes[0];
  var title = ep.title || '';
  var img = ep.poster_url || ep.thumb_url;
  // A show is "in progress" if any of its episodes is; the bar reflects that
  // episode, which is the one the card would resume into.
  var inProg = continueItems(show.episodes)[0];
  card.innerHTML =
    '<div class="pc-cover">' +
      (img ? '<img src="'+esc(img)+'" loading="lazy" crossorigin="anonymous">' : noArtCard(title, ep)) +
      posterOverlay(title, ep.year, seriesCount(show)) +
      (inProg ? progressBar(inProg) : '') +
    '</div>';
  card.addEventListener('click', function() { openShow(show); });
  card.addEventListener('contextmenu', function(e){ openCtxMenu(e, show.episodes[0]); });
  bindTint(card, img);
  return card;
}

var currentSeason = null;

function openShow(show) {
  currentShow = show;
  currentSeason = null;
  history.pushState({type:'show', id: show.id}, '', '#show=' + show.id);
  window.scrollTo(0, 0);
  renderGrid(currentFiles);
}

// backToHome is what a detail page's own "back" actually needs, rather than
// a plain history.back(): normally one entry is enough, but if a player was
// opened from here and then minimized, its own history entry (see
// modalInHistory) is deliberately left untouched by minimizing -- it still
// sits on top of this page's entry. One press of "all" would only pop that,
// landing right back on this same page and reading as if the press did
// nothing -- a second press was what actually reached home. Skipping past
// it here means "all" always reaches home in one, and the minimized player
// underneath is still never touched by any of this either way.
function backToHome() {
  var skip = (modalInHistory && modal.classList.contains('open') && modal.classList.contains('mini')) ? 2 : 1;
  history.go(-skip);
}

function closeShow() {
  currentShow = null;
  currentSeason = null;
  // Leaving the title behind: hand the wash back to the wall's ambient tint.
  clearTimeout(tintPending);
  if (tintDefault) withTint(tintDefault, applyTint);
  backToHome();
}

function renderShowView(show) {
  var ep = show.episodes[0];
  var backdropSrc = ep.tmdb_backdrop || ep.poster_url;
  var posterSrc = ep.poster_url || ep.thumb_url;
  // Hold this show's own colour rather than the hover/reset system, which
  // would otherwise revert to the wall's hero tint on the next tick.
  clearTimeout(tintPending);
  withTint(backdropSrc || posterSrc, applyTint);

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
      '<button class="sv-play-btn" id="sv-play-btn" aria-label="Play"></button>' +
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
    var label = first && first.season && first.episode
      ? 'S' + pad2(first.season) + 'E' + pad2(first.episode)
      : 'Play';
    playBtn.setAttribute('aria-label', 'Play ' + label);
    playBtn.title = 'Play ' + label;
    if (first) playBtn.addEventListener('click', function(e){ openFile(e, first.key, false); });
  }

  episodes.forEach(function(f) {
    var row = document.createElement('div');
    row.className = 'sv-ep-row';
    row.tabIndex = 0;
    row.addEventListener('click', function(e){ openFile(e, f.key, false); });
    // Still downloading (see isFileStable server-side): there is nothing to
    // pull a frame from yet, so don't even ask — just show the same spinner
    // the wall's own cards use for a processing file.
    var stillSrc = f.proc_state === 'processing' ? '' : (f.episode_still || artFor(f, true));
    var p = progressOf(f);
    var pct = p ? Math.max(2, Math.min(100, (p.position / p.duration) * 100)) : 0;
    // Runtime: the file's own probed duration when we have it, TMDB's minutes
    // otherwise — and nothing at all rather than a guess.
    var time = f.duration ? fmtDur(f.duration) : (f.runtime ? f.runtime + 'm' : '');
    row.innerHTML =
      '<div class="sv-ep-num">' + (f.episode || '') + '</div>' +
      '<div class="sv-ep-shot">' +
        (f.proc_state === 'processing' ? '<div class="sv-ep-no-still">\u27f3</div>'
          : stillSrc ? '<img src="' + esc(stillSrc) + '" loading="lazy" alt="" onerror="epNoStill(this)">'
                  : '<div class="sv-ep-no-still">\u25b6</div>') +
        (p ? '<div class="sv-ep-prog"><i style="width:' + pct.toFixed(1) + '%"></i></div>' : '') +
      '</div>' +
      '<div class="sv-ep-info">' +
        '<div class="sv-ep-head">' +
          '<span class="sv-ep-name">' + esc(f.episode_title || f.name) + '</span>' +
          (time ? '<span class="sv-ep-time">' + esc(time) + '</span>' : '') +
        '</div>' +
        (f.episode_overview ? '<div class="sv-ep-desc">' + esc(f.episode_overview) + '</div>' : '') +
      '</div>';
    fileGrid.appendChild(row);
  });

  // Bonus material that came packaged with a specific season (that season's
  // own Featurettes folder) rather than the show as a whole — see
  // markShowFeaturettes for how main_key gets set to "show:<tmdb id>:s<n>"
  // for these server-side. Scoped to the season currently showing, same as
  // the episode list just above, so a season's extras sit with that season
  // rather than all piling into one list regardless of which tab is open.
  var showExtras = currentFiles.filter(function(x){
    return x.is_extra && x.main_key === 'show:' + show.id + ':s' + currentSeason;
  });
  if (showExtras.length) {
    appendSectionHeading(fileGrid, 'Extras');
    showExtras.forEach(renderExtraRow);
  }
}

function epNoStill(img) {
  img.outerHTML = '<div class="sv-ep-no-still">\u25b6</div>';
}

function closeMovieDetail() {
  currentMovieDetail = null;
  clearTimeout(tintPending);
  if (tintDefault) withTint(tintDefault, applyTint);
  backToHome();
}

function renderMovieView(main, extras) {
  var posterSrc = main.poster_url || main.thumb_url;
  var backdropSrc = main.tmdb_backdrop || posterSrc;
  clearTimeout(tintPending);
  withTint(backdropSrc || posterSrc, applyTint);

  // Cinematic hero — same layout as series detail
  var wrap = document.createElement('div');
  wrap.className = 'sv-wrap';
  wrap.innerHTML =
    '<div class="sv-backdrop">' +
      (backdropSrc ? '<img src="'+esc(backdropSrc)+'">' : '') +
      '<div class="sv-backdrop-fade"></div>' +
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
      '<button class="sv-play-btn" aria-label="Play" onclick="openFile(event,\''+esc(main.key)+'\',false,true)"></button>' +
    '</div>';
  fileGrid.appendChild(wrap);

  // Metadata grid
  var cells = [];
  if (main.director) cells.push({label:'Director', value: esc(main.director)});
  if (main.cast && main.cast.length) cells.push({label:'Cast', value: esc(main.cast.join(', '))});
  if (main.runtime) cells.push({label:'Runtime', value: main.runtime+' min'});
  if (main.country) cells.push({label:'Country', value: esc(main.country)});
  if (main.language) cells.push({label:'Language', value: esc(main.language)});
  // audio_lang is the language actually embedded in this file's default
  // track — distinct from language above (TMDB's listing for the production
  // as a whole), and the field that answers "what will I actually hear".
  if (main.audio_lang) cells.push({label:'Audio', value: esc(main.audio_lang)});

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
  extras.forEach(renderExtraRow);
}

// Shared between a movie's Extras section and a show's: same row markup
// either way, whether the extra is attached to a single film (main_key is
// the movie's own file key) or to a whole show across its seasons (main_key
// is "show:<tmdb id>:s<season>" — see markShowFeaturettes).
function renderExtraRow(f) {
    var row = document.createElement('div');
    row.className = 'sv-ep-row';
    row.tabIndex = 0;
    row.addEventListener('click', function(e){
      if (e.target.tagName==='BUTTON') return;
      openFile(e, f.key, false, true);
    });
    // Extras never asked for artwork at all — the placeholder was hardcoded.
    // They have no TMDB match, but the server can still pull a frame out of
    // the file itself, which is the same still the music wall uses. Unless
    // it's still downloading (see isFileStable server-side), in which case
    // there's nothing to pull yet — show the spinner instead of asking.
    var xArt = f.proc_state === 'processing' ? '' : artFor(f, true);
    var xp = progressOf(f);
    row.innerHTML =
      '<div class="sv-ep-shot">' +
        (f.proc_state === 'processing' ? '<div class="sv-ep-no-still">\u27f3</div>'
          : xArt ? '<img src="'+esc(xArt)+'" loading="lazy" alt="" crossorigin="anonymous" onerror="epNoStill(this)">'
              : '<div class="sv-ep-no-still">\u25b6</div>') +
        (xp ? '<div class="sv-ep-prog"><i style="width:' +
              Math.max(2, Math.min(100, (xp.position / xp.duration) * 100)).toFixed(1) + '%"></i></div>' : '') +
      '</div>' +
      '<div class="sv-ep-info"><div class="sv-ep-head">' +
        '<span class="sv-ep-name">'+esc(f.title || f.name)+'</span>' +
        (f.duration ? '<span class="sv-ep-time">'+fmtDur(f.duration)+'</span>' : '') +
      '</div></div>';
  fileGrid.appendChild(row);
}

function appendSectionHeading(target, text) {
  var h = document.createElement('div');
  h.className = 'section-heading';
  h.textContent = text;
  target.appendChild(h);
}

// reconcileKeyed keeps a container's existing card nodes when the item behind
// them hasn't changed, instead of the old approach of nuking the whole
// section every render. That used to mean a single file's audio-check
// finishing in the background repainted every poster on the wall: every
// mount animation replayed, every hover/focus state was lost, and a rail's
// own horizontal scroll position reset to 0. sigFn decides "did this item
// change"; when it hasn't, the existing node is left completely untouched
// (not even repositioned unless its order actually moved).
function reconcileKeyed(container, items, keyFn, createFn, sigFn) {
  var existing = {};
  Array.prototype.forEach.call(container.children, function(ch) {
    if (ch.dataset && ch.dataset.rkey !== undefined) existing[ch.dataset.rkey] = ch;
  });
  var seen = {};
  var prev = null;
  items.forEach(function(item) {
    var key = String(keyFn(item));
    seen[key] = true;
    var sig = sigFn ? sigFn(item) : null;
    var node = existing[key];
    if (!node || (sigFn && node.dataset.rsig !== sig)) {
      var fresh = createFn(item);
      fresh.dataset.rkey = key;
      if (sigFn) fresh.dataset.rsig = sig;
      if (node && node.parentNode === container) container.replaceChild(fresh, node);
      node = fresh;
    }
    var wantAfter = prev ? prev.nextSibling : container.firstChild;
    if (wantAfter !== node) container.insertBefore(node, wantAfter);
    prev = node;
  });
  Object.keys(existing).forEach(function(k) {
    if (!seen[k] && existing[k].parentNode === container) container.removeChild(existing[k]);
  });
}

// The wall's structural chrome (hero slot, rail headers, rail/grid
// containers) is built once and then only ever has its children reconciled —
// see reconcileKeyed. filmsGrid/seriesGrid/otherGrid use display:contents so
// wrapping their cards in a persistent container doesn't opt them out of
// #file-grid's own CSS grid (grid layout only auto-flows direct children).
var wallSkeletonMounted = false;
var wallEls = null;
function railHeadEl(id) {
  var h = document.createElement('div');
  h.className = 'rail-head';
  h.id = id;
  h.innerHTML = '<span class="rh-t"></span><span class="rh-c"></span><span class="rh-l"></span>';
  return h;
}
function contentsGrid(id) {
  var d = document.createElement('div');
  d.id = id;
  d.style.display = 'contents';
  return d;
}
function ensureWallSkeleton() {
  if (wallSkeletonMounted) return;
  fileGrid.innerHTML = '';
  wallEls = {
    hero: document.createElement('div'),
    cwHead: railHeadEl('rail-continue-watching'),
    cwRail: (function(){ var r = document.createElement('div'); r.className = 'cw-rail'; return r; })(),
    filmsHead: railHeadEl('rail-films'),
    filmsGrid: contentsGrid('wall-films-grid'),
    seriesHead: railHeadEl('rail-series'),
    seriesGrid: contentsGrid('wall-series-grid'),
    otherHead: railHeadEl('rail-other'),
    otherGrid: contentsGrid('wall-other-grid'),
    plHead: railHeadEl('rail-playlists'),
    plRail: (function(){ var r = document.createElement('div'); r.className = 'cw-rail'; return r; })(),
    empty: document.createElement('div')
  };
  wallEls.hero.id = 'wall-hero-slot';
  wallEls.hero.style.display = 'contents';
  wallEls.empty.style.cssText = 'display:none;color:var(--muted);font-size:.8rem;grid-column:1/-1;padding:2rem 0';
  wallEls.empty.textContent = 'no files yet';
  [wallEls.hero, wallEls.cwHead, wallEls.cwRail, wallEls.filmsHead, wallEls.filmsGrid,
   wallEls.seriesHead, wallEls.seriesGrid, wallEls.otherHead, wallEls.otherGrid,
   wallEls.plHead, wallEls.plRail, wallEls.empty
  ].forEach(function(el){ fileGrid.appendChild(el); });
  wallSkeletonMounted = true;
}
// Hides a rail entirely when it has nothing to show, and keeps its label/count
// current otherwise -- the count used to only ever be set once, at creation.
function setRailHead(h, text, count) {
  if (!count) { h.style.display = 'none'; return; }
  h.style.display = '';
  h.querySelector('.rh-t').textContent = text;
  h.querySelector('.rh-c').textContent = count;
}

function renderGrid(files) {
  updateNowPlayingWash(); // home wall vs. a detail page -- see updateNowPlayingWash
  if (topbarBack) topbarBack.style.display = (currentShow || currentMovieDetail || currentPlaylist) ? 'inline-block' : 'none';
  // A detail page is one title, not a shelf, so the section list has nothing
  // to point at and is cleared rather than left showing the previous view's.
  // Unlike the wall below, these always do a full rebuild -- landing on a
  // title's own page is a real navigation, not a background poll repainting
  // data you're already looking at.
  if (currentShow) {
    fileGrid.innerHTML = ''; musicGrid.innerHTML = '';
    musicHeading.style.display = 'none'; musicGrid.style.display = 'none';
    wallSkeletonMounted = false;
    clearSectionTags();
    renderShowView(currentShow);
    return;
  }
  if (currentMovieDetail) {
    fileGrid.innerHTML = ''; musicGrid.innerHTML = '';
    musicHeading.style.display = 'none'; musicGrid.style.display = 'none';
    wallSkeletonMounted = false;
    clearSectionTags();
    renderMovieView(currentMovieDetail.main, currentMovieDetail.extras);
    return;
  }
  if (currentPlaylist) {
    fileGrid.innerHTML = ''; musicGrid.innerHTML = '';
    musicHeading.style.display = 'none'; musicGrid.style.display = 'none';
    wallSkeletonMounted = false;
    clearSectionTags();
    renderPlaylistView(currentPlaylist);
    return;
  }

  ensureWallSkeleton();

  var musicItems = sortByTitle(files.filter(function(f){ return f.is_music_video; }));
  var regularItems = files.filter(function(f){ return !f.is_music_video && !f.is_extra; });
  var grouped = groupBySeries(regularItems);
  if (!grouped.standalone.length && !grouped.shows.length && !musicItems.length) {
    updateHero(null);
    wallEls.cwRail.innerHTML = ''; wallEls.cwRail.style.display = 'none'; setRailHead(wallEls.cwHead, '', 0);
    wallEls.filmsGrid.innerHTML = ''; setRailHead(wallEls.filmsHead, '', 0);
    wallEls.seriesGrid.innerHTML = ''; setRailHead(wallEls.seriesHead, '', 0);
    wallEls.otherGrid.innerHTML = ''; setRailHead(wallEls.otherHead, '', 0);
    wallEls.plRail.innerHTML = ''; wallEls.plRail.style.display = 'none'; setRailHead(wallEls.plHead, '', 0);
    musicGrid.innerHTML = ''; musicHeading.style.display = 'none'; musicGrid.style.display = 'none';
    wallEls.empty.style.display = 'block';
    railAnchors = [];
    buildSectionTags();
    return;
  }
  wallEls.empty.style.display = 'none';

  // Tiles settle in sequence as the wall paints; the delay is capped so a long
  // grid does not leave its last row visibly trailing the first. Only newly
  // created cards call stagger (see reconcileKeyed), so a card that survives
  // this render unchanged keeps whatever entrance timing it already had
  // (irrelevant, since animation-fill-mode:both means it's long since settled).
  tileSeq = 0;

  var fileSig = function(f){ return JSON.stringify(f) + '|' + JSON.stringify(progressOf(f)); };

  // Continue watching is drawn from every file, not just films and series: a
  // half-finished featurette or music video is just as resumable.
  var cont = continueItems(files).slice(0, 8);
  var cands = heroCandidates(files);
  var heroResume = heroPick(cands, Math.floor(Date.now() / HERO_WINDOW_MS));
  var hero = heroResume || pickHeroFilm(grouped.standalone);
  updateHero(hero, !!heroResume);
  scheduleHeroRoll();
  setRailHead(wallEls.cwHead, 'Continue watching', cont.length);
  wallEls.cwRail.style.display = cont.length ? '' : 'none';
  reconcileKeyed(wallEls.cwRail, cont, function(f){ return f.key; }, renderContinueCard, fileSig);

  // Previously one alphabetical run, which put Arcane between Annihilation and
  // Atlanta. A film and a 117-episode show are different objects.
  var films = sortByTitle(grouped.standalone.filter(function(f){ return f.media_type === 'movie'; }));
  var other = sortByTitle(grouped.standalone.filter(function(f){ return f.media_type !== 'movie'; }));
  var shows = sortByTitle(grouped.shows.map(function(s){ return {_show: s}; }));

  setRailHead(wallEls.filmsHead, 'Films', films.length);
  reconcileKeyed(wallEls.filmsGrid, films, function(f){ return f.key; }, renderFileCard, fileSig);

  setRailHead(wallEls.seriesHead, 'Series', shows.length);
  reconcileKeyed(wallEls.seriesGrid, shows, function(item){ return item._show.id; },
    function(item){ return renderShowCard(item._show); },
    function(item){
      var show = item._show;
      var inProg = continueItems(show.episodes)[0];
      return JSON.stringify(show) + '|' + JSON.stringify(inProg ? progressOf(inProg) : null);
    });

  setRailHead(wallEls.otherHead, 'Other', other.length);
  reconcileKeyed(wallEls.otherGrid, other, function(f){ return f.key; }, renderFileCard, fileSig);

  setRailHead(wallEls.plHead, 'Playlists', playlists.length);
  wallEls.plRail.style.display = playlists.length ? '' : 'none';
  reconcileKeyed(wallEls.plRail, playlists, function(pl){ return pl.id; }, renderPlaylistCard,
    function(pl){ return JSON.stringify(pl); });

  if (musicItems.length) {
    musicHeading.style.display = 'block';
    musicGrid.style.display = 'grid';
  } else {
    musicHeading.style.display = 'none';
    musicGrid.style.display = 'none';
  }
  reconcileKeyed(musicGrid, musicItems, function(f){ return f.key; }, renderMusicCard,
    function(f){ return JSON.stringify(f); });

  railAnchors = [];
  if (cont.length) railAnchors.push({id: 'rail-continue-watching', label: 'Continue watching', count: cont.length});
  if (films.length) railAnchors.push({id: 'rail-films', label: 'Films', count: films.length});
  if (shows.length) railAnchors.push({id: 'rail-series', label: 'Series', count: shows.length});
  if (other.length) railAnchors.push({id: 'rail-other', label: 'Other', count: other.length});
  if (playlists.length) railAnchors.push({id: 'rail-playlists', label: 'Playlists', count: playlists.length});
  if (musicItems.length) railAnchors.push({id: 'music-heading', label: 'Music videos', count: musicItems.length});
  buildSectionTags();
}

var tileSeq = 0;
// stagger hands each tile its slot in the entrance sequence, flattening out
// after ~18 tiles so nothing on a long wall arrives conspicuously late.
function stagger(el) {
  el.style.setProperty('--in', Math.min(tileSeq++ * 22, 400) + 'ms');
}

var railAnchors = [];

function clearSectionTags() {
  railAnchors = [];
  var nav = document.getElementById('section-tags');
  if (nav) nav.innerHTML = '';
}

// buildSectionTags fills the bar's empty middle with a jump list built from
// whichever rails this render actually produced — so it is never a menu of
// sections that do not exist.
function buildSectionTags() {
  var nav = document.getElementById('section-tags');
  if (!nav) return;
  nav.innerHTML = '';
  if (railAnchors.length < 2) return;
  railAnchors.forEach(function(a){
    var b = document.createElement('button');
    b.className = 'sec-tag';
    b.type = 'button';
    b.tabIndex = 0;
    b.innerHTML = esc(a.label) + (a.count ? '<span class="n">' + a.count + '</span>' : '');
    b.dataset.target = a.id;
    b.addEventListener('click', function(){
      var el = document.getElementById(a.id);
      if (!el) return;
      var bar = document.getElementById('topbar');
      var top = el.getBoundingClientRect().top + window.scrollY -
                ((bar ? bar.offsetHeight : 60) + 14);
      window.scrollTo({top: Math.max(0, top), behavior: 'smooth'});
    });
    nav.appendChild(b);
  });
  markActiveTag();
}

// The active chip follows the rail you have scrolled into, so the bar reports
// where you are as well as offering somewhere to go.
function markActiveTag() {
  var tags = document.querySelectorAll('.sec-tag');
  if (!tags.length) return;
  var best = null;
  railAnchors.forEach(function(a){
    var el = document.getElementById(a.id);
    var bar = document.getElementById('topbar');
    var line = (bar ? bar.offsetHeight : 60) + 30;
    if (el && el.getBoundingClientRect().top <= line) best = a.id;
  });
  if (!best && railAnchors.length) best = railAnchors[0].id;
  tags.forEach(function(t){ t.classList.toggle('on', t.dataset.target === best); });
}

var tagScrollTick = false;
window.addEventListener('scroll', function(){
  if (tagScrollTick) return;
  tagScrollTick = true;
  requestAnimationFrame(function(){ markActiveTag(); tagScrollTick = false; });
}, {passive: true});

// pickHeroFilm is the fallback when nothing has been watched yet: a film with
// a backdrop, chosen at random so the shelf looks different between visits.
function pickHeroFilm(standalone) {
  var pool = standalone.filter(function(f){ return f.tmdb_backdrop && f.media_type === 'movie'; });
  if (!pool.length) pool = standalone.filter(function(f){ return f.tmdb_backdrop; });
  return pool.length ? pool[Math.floor(Math.random() * pool.length)] : null;
}

// ── Which of your recent watches the banner shows ──────────────────────────
// One title, not a carousel: the pick is drawn from the five films and shows
// you touched most recently and holds for six hours, so the shelf changes
// through the day without shuffling under you mid-visit. Music videos are
// excluded — the banner is for things you are part-way through, and a
// three-minute video is not that.
var HERO_WINDOW_MS = 6 * 60 * 60 * 1000;

function heroCandidates(files) {
  return continueItems(files).filter(function(f){
    return (f.media_type === 'movie' || f.media_type === 'tv') &&
           (f.tmdb_backdrop || f.episode_still);
  }).slice(0, 5);
}

// Stable within a window, and scrambled between them, so consecutive windows
// do not simply walk the list in order.
function heroPick(cands, windowIdx) {
  if (!cands.length) return null;
  var h = (windowIdx + 1) * 2654435761 % 4294967296;
  h = h ^ (h >>> 13);
  h = (h * 1274126177) % 4294967296;
  h = h ^ (h >>> 16);
  return cands[Math.abs(h) % cands.length];
}

var heroTimer = null;
// Re-render at the window boundary so a screen left running all day does not
// sit on the same banner until someone reloads it.
function scheduleHeroRoll() {
  if (heroTimer) clearTimeout(heroTimer);
  var wait = HERO_WINDOW_MS - (Date.now() % HERO_WINDOW_MS) + 1000;
  heroTimer = setTimeout(function(){
    if (!currentShow && !currentMovieDetail && viewMode !== 'list') renderGrid(currentFiles);
  }, Math.min(wait, 2147483000));
}

function renderHero(f, resuming) {
  var art = f.tmdb_backdrop || f.episode_still;
  if (!art) return null;
  var p = progressOf(f);
  var kicker = (resuming && p)
    ? 'Resume \u00b7 ' + fmtLeft(p)
    : [f.media_type === 'tv' ? 'Series' : 'Film', f.year, f.rating ? '\u2605 ' + f.rating.toFixed(1) : '']
        .filter(Boolean).join(' \u00b7 ');
  var title = f.title || f.name;
  var sub = (f.media_type === 'tv' && f.episode_title) ? f.episode_title : f.overview;
  var el = document.createElement('div');
  el.className = 'hero';
  el.innerHTML =
    '<img src="' + esc(art) + '" alt="" crossorigin="anonymous">' +
    '<div class="hero-scrim"></div>' +
    '<div class="hero-in">' +
      '<div class="hero-kicker">' + esc(kicker) + '</div>' +
      '<div class="hero-t">' + esc(title) + '</div>' +
      (sub ? '<div class="hero-d">' + esc(sub) + '</div>' : '') +
      '<button class="hero-btn" tabindex="0">\u25b6 ' + (p ? 'RESUME' : 'PLAY') + '</button>' +
    '</div>';
  el.querySelector('.hero-btn').addEventListener('click', function(e){
    e.stopPropagation();
    openFile(e, f.key, false, true);
  });
  // Anywhere else on the banner opens the title rather than playing it —
  // resume is the one explicit action, everything else is "tell me more".
  el.addEventListener('click', function(e){
    if (e.target.closest('.hero-btn')) return;
    if (f.media_type === 'tv' && f.tmdb_id) {
      var eps = currentFiles.filter(function(x){ return x.media_type === 'tv' && x.tmdb_id === f.tmdb_id; });
      if (eps.length) { openShow({id: f.tmdb_id, episodes: eps}); return; }
    }
    openFile(e, f.key, false);
  });
  return el;
}

// updateHero leaves the banner's DOM (and its entrance animation) alone
// unless the picked title, its resume state, or its progress actually
// changed -- a poll that changed some other file used to tear this down and
// replay heroIn every few seconds regardless.
function heroSig(f, resuming) {
  return f ? JSON.stringify(f) + '|' + (resuming?1:0) + '|' + JSON.stringify(progressOf(f)) : '';
}
function updateHero(f, resuming) {
  var slot = wallEls.hero;
  var sig = heroSig(f, resuming);
  if (slot.dataset.rsig === sig) return;
  slot.innerHTML = '';
  slot.dataset.rsig = sig;
  if (!f) return;
  var el = renderHero(f, resuming);
  if (!el) return;
  slot.appendChild(el);
  tintDefault = f.poster_url || f.tmdb_backdrop || f.episode_still;
  withTint(tintDefault, applyTint);
}

function renderContinueCard(f) {
  var p = progressOf(f);
  var art = artFor(f, true);
  var card = document.createElement('button');
  card.className = 'cw-card';
  card.tabIndex = 0;
  stagger(card);
  var title = f.title || f.name;
  var line = title;
  if (f.media_type === 'tv' && f.season && f.episode) {
    line = title + ' \u00b7 S' + pad2(f.season) + 'E' + pad2(f.episode);
  }
  var pct = Math.max(2, Math.min(100, (p.position / p.duration) * 100));
  card.innerHTML =
    '<div class="cw-shot">' +
      (art ? '<img src="' + esc(art) + '" loading="lazy" alt="" crossorigin="anonymous" onerror="cwNoArt(this)">'
           : '<div class="cw-shot-ph">\u25b6</div>') +
      '<div class="cw-bar"><i style="width:' + pct.toFixed(1) + '%"></i></div>' +
      cardMoreHTML() +
    '</div>' +
    '<div class="cw-meta"><div class="cw-t">' + esc(line) + '</div>' +
    '<div class="cw-s">' + esc(fmtLeft(p)) + '</div></div>';
  card.addEventListener('click', function(e){ openFile(e, f.key, false, true); });
  card.addEventListener('contextmenu', function(e){ openCtxMenu(e, f); });
  bindTint(card, art);
  wireCardMore(card, f);
  return card;
}

function pad2(n) { return (n < 10 ? '0' : '') + n; }

// mediaTag is the label shown under the video -- same idea as
// showPauseCard's own title/sub split, just as one line: an episode says
// which one instead of just the show's name, a movie its year, a music
// video its artist.
function mediaTag(f) {
  var title = f.title || f.name || '';
  if (f.media_type === 'tv' && f.season && f.episode) {
    return title + '  ·  S' + pad2(f.season) + 'E' + pad2(f.episode) +
      (f.episode_title ? '  ·  ' + f.episode_title : '');
  }
  if (f.year) return title + ' (' + f.year + ')';
  if (f.artist) return f.artist + (title ? '  ·  ' + title : '');
  return title;
}

function renderPlaylistCard(pl) {
  var card = document.createElement('button');
  card.className = 'pl-card';
  card.tabIndex = 0;
  stagger(card);
  var n = pl.items.length;
  // The cover is regenerated server-side from current membership, so it is
  // busted on the playlist's updated stamp rather than cached forever.
  var cover = '/playlists/cover?id=' + encodeURIComponent(pl.id) + '&v=' + (pl.updated || 0);
  card.innerHTML =
    '<div class="pl-cover">' +
      (n ? '<img src="' + esc(cover) + '" loading="lazy" alt="" onerror="plNoCover(this)">'
         : '<div class="pl-empty">\u266a</div>') +
    '</div>' +
    '<div class="pl-meta"><div class="pl-name">' + esc(pl.name) + '</div>' +
    '<div class="pl-count">' + n + (n === 1 ? ' item' : ' items') + '</div></div>';
  card.addEventListener('click', function(){ openPlaylist(pl); });
  card.addEventListener('contextmenu', function(e){ openPlaylistCtx(e, pl); });
  return card;
}

function plNoCover(img) {
  img.outerHTML = '<div class="pl-empty">\u266a</div>';
}

function openPlaylist(pl) {
  currentPlaylist = pl;
  currentShow = null;
  currentMovieDetail = null;
  history.pushState({type:'playlist', id: pl.id}, '', '#playlist=' + pl.id);
  window.scrollTo(0, 0);
  renderGrid(currentFiles);
}

function closePlaylist() {
  currentPlaylist = null;
  backToHome();
}

// A right-click on the playlist card itself manages the playlist rather than
// its contents.
function openPlaylistCtx(e, pl) {
  e.preventDefault();
  if (!ctxMenu) return;
  ctxMenu.innerHTML =
    '<div class="ctx-head">' + esc(pl.name) + '</div>' +
    '<button class="ctx-item" data-act="open">Open</button>' +
    '<button class="ctx-item" data-act="play">Play all</button>' +
    '<button class="ctx-item" data-act="cover">Set cover image\u2026</button>' +
    '<button class="ctx-item" data-act="rename">Rename\u2026</button>' +
    '<div class="ctx-sep"></div>' +
    '<button class="ctx-item" data-act="delete" style="color:#e06c6c">Delete playlist</button>';
  ctxMenu.classList.add('open');
  var r = ctxMenu.getBoundingClientRect();
  ctxMenu.style.left = Math.max(8, Math.min(e.clientX, window.innerWidth - r.width - 8)) + 'px';
  ctxMenu.style.top = Math.max(8, Math.min(e.clientY, window.innerHeight - r.height - 8)) + 'px';

  Array.prototype.forEach.call(ctxMenu.querySelectorAll('.ctx-item'), function(b){
    b.addEventListener('click', function(){
      var act = b.dataset.act;
      closeCtxMenu();
      if (act === 'open') { openPlaylist(pl); return; }
      if (act === 'play') { playPlaylistFrom(pl, 0); return; }
      if (act === 'cover') { pickPlaylistCover(pl); return; }
      if (act === 'rename') {
        var name = prompt('Rename playlist', pl.name);
        if (!name) return;
        playlistAction({action:'rename', id:pl.id, name:name})
          .then(loadPlaylists).then(function(){ renderGrid(currentFiles); });
        return;
      }
      if (act === 'delete') {
        if (!confirm('Delete the playlist "' + pl.name + '"? The videos stay in the library.')) return;
        playlistAction({action:'delete', id:pl.id}).then(loadPlaylists).then(function(){
          if (currentPlaylist && currentPlaylist.id === pl.id) currentPlaylist = null;
          renderGrid(currentFiles);
        });
      }
    });
  });
}

// The cover upload is a bare POST of the image bytes — no form, since there is
// exactly one file and one destination.
function pickPlaylistCover(pl) {
  var inp = document.createElement('input');
  inp.type = 'file';
  inp.accept = 'image/*';
  inp.addEventListener('change', function(){
    var file = inp.files && inp.files[0];
    if (!file) return;
    fetch('/playlists/cover?id=' + encodeURIComponent(pl.id), {
      method:'POST', credentials:'include', body: file
    }).then(function(r){
      if (!r.ok) throw new Error();
      showToast('Cover updated');
      return loadPlaylists();
    }).then(function(){ renderGrid(currentFiles); })
      .catch(function(){ showToast('Could not set the cover', true); });
  });
  inp.click();
}

// ── Playlist playback ──────────────────────────────────────────────────────
// While a playlist is playing, it — not the show an episode belongs to —
// decides what comes next.
var playlistQueue = null;
var playlistPos = 0;

function playPlaylistFrom(pl, index) {
  var items = playlistItems(pl);
  if (!items.length) { showToast('That playlist is empty'); return; }
  playlistQueue = items;
  playlistPos = Math.max(0, Math.min(index, items.length - 1));
  openFile(null, playlistQueue[playlistPos].key, false, true);
}

function playlistNext() {
  if (!playlistQueue || playlistPos >= playlistQueue.length - 1) return false;
  playlistPos++;
  openFile(null, playlistQueue[playlistPos].key, false, true);
  return true;
}

function renderPlaylistView(pl) {
  var items = playlistItems(pl);
  var wrap = document.createElement('div');
  wrap.className = 'sv-wrap';
  var cover = '/playlists/cover?id=' + encodeURIComponent(pl.id) + '&v=' + (pl.updated || 0);
  wrap.innerHTML =
    '<div class="sv-backdrop">' +
      (items.length ? '<img src="' + esc(cover) + '" alt="">' : '') +
      '<div class="sv-backdrop-fade"></div>' +
    '</div>' +
    '<div class="sv-hero">' +
      '<div class="sv-hero-top">' +
        '<div class="sv-info">' +
          '<h2 class="sv-title">' + esc(pl.name) + '</h2>' +
          '<div class="sv-meta"><span>' + items.length +
            (items.length === 1 ? ' item' : ' items') + '</span></div>' +
        '</div>' +
      '</div>' +
      (items.length ? '<button class="sv-play-btn" aria-label="Play all" ' +
        'onclick="playPlaylistFrom(currentPlaylist,0)"></button>' : '') +
    '</div>';
  fileGrid.appendChild(wrap);

  if (!items.length) {
    var empty = document.createElement('div');
    empty.className = 'rail-head';
    empty.innerHTML = '<span class="rh-t" style="color:var(--muted);font-family:var(--font-sans);' +
      'font-weight:400;font-size:.9rem">Nothing here yet \u2014 right-click anything on the wall to add it.</span>';
    fileGrid.appendChild(empty);
    return;
  }

  items.forEach(function(f, i){
    var row = document.createElement('div');
    row.className = 'sv-ep-row';
    row.tabIndex = 0;
    row.addEventListener('click', function(){ playPlaylistFrom(pl, i); });
    row.addEventListener('contextmenu', function(e){ openCtxMenu(e, f); });
    var art = artFor(f, true);
    var pr = progressOf(f);
    var sub = f.media_type === 'tv' && f.season && f.episode
      ? 'S' + pad2(f.season) + 'E' + pad2(f.episode) + (f.episode_title ? '  \u00b7  ' + f.episode_title : '')
      : (f.artist ? f.artist : (f.year ? String(f.year) : ''));
    row.innerHTML =
      '<div class="sv-ep-num">' + (i + 1) + '</div>' +
      '<div class="sv-ep-shot">' +
        (art ? '<img src="' + esc(art) + '" loading="lazy" alt="" crossorigin="anonymous" onerror="epNoStill(this)">'
             : '<div class="sv-ep-no-still">\u25b6</div>') +
        (pr ? '<div class="sv-ep-prog"><i style="width:' +
              Math.max(2, Math.min(100, (pr.position / pr.duration) * 100)).toFixed(1) + '%"></i></div>' : '') +
      '</div>' +
      '<div class="sv-ep-info"><div class="sv-ep-head">' +
        '<span class="sv-ep-name">' + esc(f.title || f.name) + '</span>' +
        (f.duration ? '<span class="sv-ep-time">' + fmtDur(f.duration) + '</span>' : '') +
      '</div>' + (sub ? '<div class="sv-ep-desc">' + esc(sub) + '</div>' : '') + '</div>';
    fileGrid.appendChild(row);
  });
}

function cwNoArt(img) {
  img.outerHTML = '<div class="cw-shot-ph">\u25b6</div>';
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
  stagger(card);
  // A still pulled from the video itself, not the song's album art — that
  // way it always reflects what's actually playing, and shows up even for
  // a music video iTunes never managed to identify.
  var thumbUrl = '/local/thumbnail?path=' + encodeURIComponent(f.key);
  var stillId = !f.title && !f.artist;
  var titleText = stillId ? '⟳ identifying…' : (f.title || f.name);
  var cover = '<div class="mv2-cover"><img src="'+thumbUrl+'" loading="lazy" onerror="showNoArt(this,\'mv2-noart\')">'+cardMoreHTML()+'</div>';
  card.innerHTML = cover + '<div class="pc-info"><div class="pc-title">'+esc(titleText)+'</div><div class="pc-year">'+esc(stillId?'':(f.artist||''))+'</div></div>';
  card.addEventListener('click', function(e){ openFile(e, f.key, false); });
  card.addEventListener('contextmenu', function(e){ openCtxMenu(e, f); });
  // Same ambient wash every other card gets, sourced from the still itself
  // (see the comment above) rather than nothing -- a music video hovered on
  // the wall was the one card type that never threw any colour at all.
  bindTint(card, thumbUrl);
  wireCardMore(card, f);
  return card;
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
  reportProgress(); // flush whatever was playing before switching files
  progressKey = null;
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
  if (currentKey && currentKey !== key) previousPlayedKey = currentKey;
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
  modalFname.textContent = (fMod && fMod.title) ? mediaTag(fMod) : key.split('/').pop();
  // Give the player's ground the colour of the thing in it, so opening a
  // video does not drop the room to flat black. Opening the modal covers
  // whatever card was hovered/clicked, and browsers re-run :hover matching
  // against the new topmost element at the pointer's position even with no
  // actual mouse movement -- which fires that card's mouseleave and queues
  // tintReset() to revert to the wall's hero colour a moment later. Without
  // cancelling it here, that revert can land after this call and clobber the
  // colour with the hero's instead of the thing actually playing.
  if (fMod) {
    var openArt = fMod.poster_url || fMod.tmdb_backdrop || fMod.episode_still ||
      (fMod.is_local ? '/local/thumbnail?path=' + encodeURIComponent(fMod.key) : '');
    if (openArt) {
      clearTimeout(tintPending);
      withTint(openArt, applyTint);
      // Tracked separately from the hover-driven --tint above (which a wall
      // card hovered while minimized will keep overriding, then settle back
      // to the wall's own hero colour, not this) -- see setNowPlayingTint.
      withTint(openArt, setNowPlayingTint);
    }
  }
  if (document.activeElement && document.activeElement.tabIndex === 0) lastFocusedCard = document.activeElement;
  if (!modalInHistory) {
    history.pushState({type: 'player'}, '', location.pathname + location.search + location.hash);
    modalInHistory = true;
  }
  modal.classList.add('open');
  // Advancing to a new track (queue, playlist, next episode) re-enters here
  // without the player ever having closed -- if it was minimized, it stays
  // minimized, and the page must stay scrollable rather than snapping back
  // to locked just because a file opened.
  lockPageScroll(!modal.classList.contains('mini'));
  setTimeout(function(){ document.getElementById('modal-close').focus(); }, 60);
  var nextEp = findNextEpisode(fMod);
  currentNextKey = nextEp ? nextEp.key : null;
  if (!peekNext()) modalNext.style.display = 'none';
  showPlayerChrome();
  syncMiniControls();
  modalNext.onclick = function(){ goNext(); };

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
        lockPageScroll(false);
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
        lockPageScroll(false);
        handOffLocalFile(f);
      };
      var ltrack = document.createElement('track');
      ltrack.kind = 'subtitles'; ltrack.src = '/local/subtitle?path=' + encodeURIComponent(f.key);
      ltrack.srclang = 'en'; ltrack.label = 'English'; ltrack.default = true;
      var ccWired = false;
      var wireCC = function() {
        if (ccWired) return;
        ccWired = true;
        ltrack.track.mode = 'showing';
        showOverlayBtn(modalCC);
        modalCC.textContent = 'CC on';
        modalCC.onclick = function() {
          var on = ltrack.track.mode === 'showing';
          ltrack.track.mode = on ? 'hidden' : 'showing';
          modalCC.textContent = on ? 'CC off' : 'CC on';
        };
      };
      ltrack.addEventListener('load', wireCC);
      // Confirmed on a Hisense TV's built-in browser: a <track>'s own 'load'
      // event never fires there even though the VTT actually fetched and its
      // cues did load, so the CC toggle can't depend on that event alone —
      // fall back to checking the track's cues directly once the video has
      // enough data to know. A file with genuinely no subtitle (404, so no
      // cues ever arrive) still correctly gets no CC button either way.
      ['loadedmetadata', 'canplay'].forEach(function(ev) {
        modalVideo.addEventListener(ev, function() {
          if (!ccWired && ltrack.track && ltrack.track.cues && ltrack.track.cues.length) wireCC();
        });
      });
      modalVideo.appendChild(ltrack);
      modalVideo.src = '/local/file?path=' + encodeURIComponent(f.key);
      resumePlayback(modalVideo, f.key, {fromStart: !!f.is_music_video});
      modalVideo.play().catch(function(){});
      if (isTvLike()) requestVideoFullscreen(modalVideo);
    } else if (f && f.hls_base) {
      modalVideo.style.display = 'block';
      if (f.subtitle_url) {
        var track = document.createElement('track');
        track.kind = 'subtitles'; track.src = f.subtitle_url;
        track.srclang = 'en'; track.label = 'English'; track.default = true;
        modalVideo.appendChild(track);
      } else if (f.tmdb_id) {
        showOverlayBtn(modalCC);
        modalCC.onclick = function(){ retrySubtitle(key); };
      }
      if (!f.tmdb_id) {
        modalMeta.style.display = 'inline-block';
        modalMeta.onclick = function(){ retryMetadata(key); };
      }
      playHLS(f.hls_base + 'master.m3u8', modalVideo);
      resumePlayback(modalVideo, key);
      if (isTvLike()) requestVideoFullscreen(modalVideo);
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
          showOverlayBtn(modalCC);
          modalCC.onclick = function(){ retrySubtitle(key); };
        }
        if (!f.tmdb_id) {
          modalMeta.style.display = 'inline-block';
          modalMeta.onclick = function(){ retryMetadata(key); };
        }
        modalVideo.src = '/file?key='+encodeURIComponent(key);
        resumePlayback(modalVideo, key);
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
      lockPageScroll(false);
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
// A click on the backdrop -- the black, tinted margin outside the picture --
// minimizes in one press. Minimizing rather than closing here is deliberate:
// landing outside the video is much more often a miss-click or "get this out
// of my way for a second" than "I'm done with this" -- closing outright is
// still one press away on the close button, or a real back navigation.
modal.addEventListener('click', function(e){
  if (e.target !== modal) return;
  minimizePlayer();
});

var modalTap = document.getElementById('modal-tap');

// The modal is position:fixed and visually covers the wall either way, but
// a native (non-overlay) scrollbar for the page underneath is painted by
// the browser itself at the viewport edge, above any page content -- no
// z-index or fixed overlay hides it. Only actually removing the page's own
// scrollable overflow does, so this runs alongside every open/close and
// minimize/restore of the modal (minimize is the one state that deliberately
// wants the page scrollable again, since interacting with it is the point).
function lockPageScroll(lock) {
  document.documentElement.style.overflow = lock ? 'hidden' : '';
  document.body.style.overflow = lock ? 'hidden' : '';
}

function isChromeOn() { return !modal.classList.contains('chrome-off'); }

function setChrome(on) {
  modal.classList.toggle('chrome-off', !on);
  if (on) syncTapArea();
}

// Close, filename and Next read as one group, so they show and fade
// together: moving the mouse brings all three back, three seconds of no
// movement fades them out again -- the on-screen-controls behaviour any
// video player uses, layered on top of (not instead of) the click-to-clear
// handling in modalTap's own listener below.
var idleFadeTimer = null;
function showPlayerChrome() {
  setChrome(true);
  if (peekNext() && modalNext) showOverlayBtn(modalNext);
  clearTimeout(idleFadeTimer);
  idleFadeTimer = setTimeout(function(){
    setChrome(false);
    if (modalNext) modalNext.style.display = 'none';
  }, 3000);
}
document.addEventListener('mousemove', function(){
  if (!modal.classList.contains('open') || modal.classList.contains('mini')) return;
  showPlayerChrome();
});

// The filename/actions caption floats below whichever content box is
// actually showing (the video, or the note/image viewer) rather than
// sitting under it in normal flow -- see the #modal-filename/#modal-actions
// CSS comment for why: it's what keeps the video itself centered with equal
// margins above and below, instead of its own caption's height quietly
// eating into "the bottom" while nothing balances it at the top.
function positionCaption() {
  var box = modalVideo.style.display !== 'none' ? modalVideo
    : (modalNote && modalNote.style.display !== 'none') ? modalNote : null;
  if (!box) return;
  var r = box.getBoundingClientRect();
  if (!r.width) return;
  var midX = r.left + r.width / 2;
  modalFname.style.left = midX + 'px';
  modalFname.style.top = (r.bottom + 12) + 'px';
  var fnH = modalFname.offsetHeight || 0;
  modalActions.style.left = midX + 'px';
  modalActions.style.top = (r.bottom + 12 + fnH + (fnH ? 16 : 0)) + 'px';
}

// The tap layer is positioned from the video's measured box rather than
// wrapping it, so the video keeps its place in the modal's flex column. It
// stops 52px short of the bottom to leave the native control bar clickable.
function syncTapArea() {
  positionCaption();
  if (!modalTap) return;
  if (modalVideo.style.display === 'none' || !modal.classList.contains('open')) {
    modalTap.style.display = 'none';
    return;
  }
  var r = modalVideo.getBoundingClientRect();
  if (!r.width || !r.height) { modalTap.style.display = 'none'; return; }
  // Leave the native control bar clear; a little more of it while paused,
  // where the browser also draws its own centred play affordance. Minimized
  // is the exception -- covering the whole thing, no gap at all: a corner
  // player this small has no usable native control bar to reach anyway, and
  // a click landing in that gap fell straight through to the raw <video>,
  // toggling play/pause natively instead of restoring it -- the "janky,
  // flickers between paused and playing" bug clicking the mini player had.
  var reserve = modal.classList.contains('mini') ? 0 : Math.min(modalVideo.paused ? 64 : 52, r.height * 0.24);
  modalTap.style.display = 'block';
  modalTap.style.left = r.left + 'px';
  modalTap.style.top = r.top + 'px';
  modalTap.style.width = r.width + 'px';
  modalTap.style.height = Math.max(0, r.height - reserve) + 'px';
  positionOverlayButtons(r);
}

// Next floats over the video's right edge, vertically centred -- an icon
// rather than a labelled control now, so it reads more like part of the
// picture's own chrome than a separate element beside it.
function positionOverlayButtons(r) {
  if (!r || !r.width || !modalNext || modalNext.style.display === 'none') return;
  var x = r.right - modalNext.offsetWidth - 14;
  var y = r.top + (r.height - modalNext.offsetHeight) / 2;
  modalNext.style.left = Math.max(8, x) + 'px';
  modalNext.style.top = Math.max(8, y) + 'px';
}

// showOverlayBtn is what every "reveal the CC or Next control" call site now
// goes through: the display flip plus an immediate position against the
// video's current box, so it never appears at the fixed layer's default
// top-left corner before the next resize/loadedmetadata pass would place it.
function showOverlayBtn(el) {
  if (!el) return;
  el.style.display = 'inline-block';
  positionOverlayButtons(modalVideo.getBoundingClientRect());
}

if (modalTap) {
  // Clicking the picture toggles play/pause, the way a normal video player
  // does, and brings the chrome back with it -- but only once the picture
  // is actually clear. If the right-click menu, the chrome, or the Next
  // button is already up, that click's job is mainly to clear all of it
  // away and hand focus back to the video, not fire an extra pause on top
  // of that. A paused video is the one exception: there's nothing
  // ambiguous about a click on it with controls up, so it resumes in the
  // same motion as clearing them -- only a still-playing video holds off
  // on pausing there, since a click meant to dismiss a menu shouldn't
  // double as an accidental pause.
  modalTap.addEventListener('click', function(e){
    e.stopPropagation();
    if (modal.classList.contains('mini')) { restorePlayer(); return; }
    if (modalVideo.style.display === 'none') return;

    var menuOpen = ctxMenu && ctxMenu.classList.contains('open');
    var panelShowing = isChromeOn() || (modalNext && modalNext.style.display !== 'none');
    var wasPaused = modalVideo.paused && !modalVideo.ended;
    if (menuOpen || panelShowing) {
      closeCtxMenu();
      if (wasPaused) modalVideo.play().catch(function(){});
      clearTimeout(idleFadeTimer);
      setChrome(false);
      if (modalNext) modalNext.style.display = 'none';
      modalVideo.focus();
      return;
    }

    if (wasPaused) {
      modalVideo.play().catch(function(){});
    } else {
      modalVideo.pause();
    }
    showPlayerChrome();
  });
  modalTap.addEventListener('contextmenu', openPlayerCtxMenu);
}

// ── Minimize ─────────────────────────────────────────────────────────────
// A corner mini-player, not real (separate-window) Picture-in-Picture: the
// rest of the app stays reachable underneath because the modal itself has
// shrunk, not because anything is punching pointer-events holes through it.
// That includes scrolling the wall behind it again, so the page-scroll lock
// (see lockPageScroll) lifts here and comes back once restored.
function minimizePlayer() {
  modal.classList.add('mini');
  lockPageScroll(false);
  syncTapArea();
  syncPauseCard();
  updateNowPlayingWash();
  syncMiniControls();
}
function restorePlayer() {
  modal.classList.remove('mini');
  lockPageScroll(true);
  syncTapArea();
  syncPauseCard();
  updateNowPlayingWash();
  syncMiniControls();
}

// subtitleStateLabel turns modalCC's own text (still the source of truth --
// see openFile's local <track> wiring and retrySubtitle for the cloud path)
// into the short label the right-click menu shows, now that the button
// itself is never shown on screen (see #modal-cc-ov).
function subtitleStateLabel(t) {
  if (t === 'CC on') return 'On';
  if (t === 'CC off') return 'Off';
  if (t === 'CC fetching…') return 'Fetching…';
  if (t === 'CC retry failed') return 'Retry';
  return 'Get subtitles';
}

// openPlayerCtxMenu replaces the browser's own right-click menu on the
// video with one scoped to what this app actually offers -- reusing the
// same #ctx-menu element and open/close/positioning logic the file-card
// menu already has (see openCtxMenu), just with a different row set.
function openPlayerCtxMenu(e) {
  if (!ctxMenu) return;
  e.preventDefault();
  e.stopPropagation();
  var mini = modal.classList.contains('mini');
  // Regular menu sizing next to a 340px corner player reads as oversized.
  ctxMenu.classList.toggle('ctx-compact', mini);
  var rows = [];
  if (modalCC.style.display !== 'none') {
    rows.push('<button class="ctx-item" data-act="subtitles"' +
      (modalCC.disabled ? ' disabled style="opacity:.45"' : '') + '>' +
      '<span>Subtitles</span><span class="n">' + esc(subtitleStateLabel(modalCC.textContent)) + '</span></button>');
  }
  if (peekNext()) {
    rows.push('<button class="ctx-item" data-act="next">Next</button>');
  }
  if (rows.length) rows.push('<div class="ctx-sep"></div>');
  rows.push('<button class="ctx-item" data-act="minimize">' + (mini ? 'Restore' : 'Minimize') + '</button>');
  if (document.pictureInPictureEnabled && !modalVideo.disablePictureInPicture) {
    rows.push('<button class="ctx-item" data-act="pip">Picture-in-Picture</button>');
  }
  rows.push('<button class="ctx-item" data-act="fullscreen">Fullscreen</button>');
  ctxMenu.innerHTML = rows.join('');

  ctxMenu.classList.add('open');
  var r = ctxMenu.getBoundingClientRect();
  var x = Math.min(e.clientX, window.innerWidth - r.width - 8);
  var y = Math.min(e.clientY, window.innerHeight - r.height - 8);
  ctxMenu.style.left = Math.max(8, x) + 'px';
  ctxMenu.style.top = Math.max(8, y) + 'px';

  Array.prototype.forEach.call(ctxMenu.querySelectorAll('.ctx-item'), function(b){
    b.addEventListener('click', function(){
      var act = b.dataset.act;
      closeCtxMenu();
      if (act === 'subtitles') { modalCC.click(); return; }
      if (act === 'next') { goNext(); return; }
      if (act === 'minimize') { if (modal.classList.contains('mini')) restorePlayer(); else minimizePlayer(); return; }
      if (act === 'pip') { modalVideo.requestPictureInPicture().catch(function(){}); return; }
      if (act === 'fullscreen') { requestVideoFullscreen(modalVideo); return; }
    });
  });
}

// ── Paused ────────────────────────────────────────────────────────────────
var pauseCard = document.getElementById('pause-card');
var pauseTimer = null;

function currentPlayingFile() {
  return currentFiles.filter(function(x){ return x.key === currentKey; })[0] || null;
}

function showPauseCard() {
  if (!pauseCard || !modal.classList.contains('open')) return;
  if (modalVideo.ended || modalVideo.style.display === 'none') return;
  // Held ambient motion along with the held frame -- see the html.media-paused
  // rules by #pause-card's own CSS -- and the now-playing wash fades out
  // entirely rather than just holding, since it specifically means "playing
  // right now" (see updateNowPlayingWash).
  document.documentElement.classList.add('media-paused');
  updateNowPlayingWash();
  syncMiniControls();
  var f = currentPlayingFile();
  var title = (f && f.title) || (currentKey || '').split('/').pop();
  var sub = '';
  if (f && f.media_type === 'tv' && f.season && f.episode) {
    sub = 'S' + pad2(f.season) + 'E' + pad2(f.episode) +
          (f.episode_title ? '  \u00b7  ' + f.episode_title : '');
  } else if (f && f.year) {
    sub = String(f.year) + (f.runtime ? '  \u00b7  ' + f.runtime + ' min' : '');
  } else if (f && f.artist) {
    sub = f.artist;
  }
  pauseCard.innerHTML =
    '<div class="pause-in">' +
      '<div class="pause-kicker">Paused</div>' +
      '<div class="pause-t">' + esc(title) + '</div>' +
      (sub ? '<div class="pause-s">' + esc(sub) + '</div>' : '') +
    '</div>';
  syncPauseCard();
  // A beat's delay so scrubbing, which fires pause/play in quick succession,
  // does not strobe the plate.
  clearTimeout(pauseTimer);
  pauseTimer = setTimeout(function(){ pauseCard.classList.add('on'); }, 220);
}

function hidePauseCard() {
  clearTimeout(pauseTimer);
  if (pauseCard) pauseCard.classList.remove('on');
  document.documentElement.classList.remove('media-paused');
  updateNowPlayingWash();
  syncMiniControls();
}

// Sized to the video's own box, like the tap layer, so it never covers the
// letterboxing or the native controls.
function syncPauseCard() {
  if (!pauseCard) return;
  var r = modalVideo.getBoundingClientRect();
  if (!r.width || !r.height) return;
  pauseCard.style.left = r.left + 'px';
  pauseCard.style.top = r.top + 'px';
  pauseCard.style.width = r.width + 'px';
  pauseCard.style.height = Math.max(0, r.height - 46) + 'px';
}

modalVideo.addEventListener('pause', showPauseCard);
modalVideo.addEventListener('play', hidePauseCard);
modalVideo.addEventListener('ended', hidePauseCard);
window.addEventListener('resize', syncPauseCard);
window.addEventListener('resize', syncTapArea);
modalVideo.addEventListener('loadedmetadata', syncTapArea);
modalVideo.addEventListener('play', function(){ setTimeout(syncTapArea, 60); });

// ══ TV playback ═══════════════════════════════════════════════════════════
// There is no way to ask a browser for the panel's physical size, so this is
// a heuristic: a known TV platform in the user-agent, or a very large screen
// being driven by something other than a mouse. Set localStorage
// 'stream-tv' to '1' or '0' to force it either way.
function isTvLike() {
  var forced = lsGet('stream-tv');
  if (forced === '1') return true;
  if (forced === '0') return false;
  var ua = navigator.userAgent || '';
  if (/SmartTV|SMART-TV|Tizen|Web0S|WebOS|NetCast|BRAVIA|HbbTV|AndroidTV|Android TV|GoogleTV|AppleTV|CrKey|Roku|VIDAA|AFT[BMST]/i.test(ua)) return true;
  var coarse = window.matchMedia('(pointer: coarse)').matches ||
               window.matchMedia('(hover: none)').matches;
  var big = Math.max(screen.width || 0, screen.height || 0) >= 1920;
  return coarse && big;
}
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
  if (searchOverlay.classList.contains('open')) return;

  // ── Modal navigation ───────────────────────────────────────────────────────
  if (modal.classList.contains('open')) {
    if (e.key === 'Escape' && !isChromeOn() && modalVideo.style.display !== 'none') {
      e.preventDefault();
      setChrome(true);
      return;
    }
    if (e.key === 'ArrowLeft' || e.key === 'ArrowRight') {
      e.preventDefault();
      var modalControls = ['modal-close','modal-quality','modal-dl','modal-del','modal-copy','modal-meta','modal-cc-ov','modal-next-ov']
        .map(function(id){ return document.getElementById(id); })
        .filter(function(el){ return el && el.offsetParent !== null && !el.disabled; });
      var mi = modalControls.indexOf(document.activeElement);
      if (mi === -1) { if (modalControls[0]) modalControls[0].focus(); return; }
      var mn = e.key === 'ArrowRight' ? modalControls[mi+1] : modalControls[mi-1];
      if (mn) mn.focus();
    }
    if (e.key === 'Enter' && document.activeElement && document.activeElement !== document.body) {
      e.preventDefault();
      document.activeElement.click();
    }
    return;
  }

  var dirs = {ArrowLeft: 'left', ArrowRight: 'right', ArrowUp: 'up', ArrowDown: 'down'};
  if (!dirs[e.key] && e.key !== 'Enter') return;
  if (document.activeElement && document.activeElement.tagName === 'INPUT') return;

  // ── Header navigation ──────────────────────────────────────────────────────
  var inHeader = document.activeElement && document.activeElement.closest && document.activeElement.closest('header');
  if (inHeader) {
    e.preventDefault();
    var hBtns = Array.prototype.slice.call(document.querySelectorAll('header [tabindex="0"]'));
    var hi = hBtns.indexOf(document.activeElement);
    if (e.key === 'ArrowDown') { var t = navTiles(); if (t.length) t[0].focus(); return; }
    if (e.key === 'ArrowRight') { if (hBtns[hi+1]) hBtns[hi+1].focus(); return; }
    if (e.key === 'ArrowLeft')  { if (hBtns[hi-1]) hBtns[hi-1].focus(); return; }
    if (e.key === 'Enter')      { document.activeElement.click(); return; }
    return;
  }

  // ── Grid / list navigation ─────────────────────────────────────────────────
  if (e.key === 'Enter' && document.activeElement && document.activeElement.getAttribute('tabindex') === '0') {
    e.preventDefault();
    document.activeElement.click();
    return;
  }
  if (dirs[e.key]) {
    e.preventDefault();
    var prevActive = document.activeElement;
    moveNavFocus(dirs[e.key]);
    // If nothing above the current row, climb to header
    if (e.key === 'ArrowUp' && document.activeElement === prevActive) {
      var hBtns2 = Array.prototype.slice.call(document.querySelectorAll('header [tabindex="0"]'));
      if (hBtns2.length) hBtns2[0].focus();
    }
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

// reportProgress sends the current playback position for the file modalVideo
// is tracking. Called on a throttle during playback and once more whenever
// the file changes or the modal closes, so a quick watch still saves before
// the next throttle tick would have fired.
function findNextEpisode(f) {
  if (!f || f.media_type !== 'tv') return null;
  var eps = currentFiles.filter(function(x){ return x.media_type === 'tv' && x.tmdb_id && x.tmdb_id === f.tmdb_id; });
  eps.sort(function(a,b){ return (a.season - b.season) || (a.episode - b.episode); });
  var i = eps.findIndex(function(x){ return x.key === f.key; });
  return (i !== -1 && i < eps.length - 1) ? eps[i+1] : null;
}
function findPrevEpisode(f) {
  if (!f || f.media_type !== 'tv') return null;
  var eps = currentFiles.filter(function(x){ return x.media_type === 'tv' && x.tmdb_id && x.tmdb_id === f.tmdb_id; });
  eps.sort(function(a,b){ return (a.season - b.season) || (a.episode - b.episode); });
  var i = eps.findIndex(function(x){ return x.key === f.key; });
  return i > 0 ? eps[i-1] : null;
}

// previousPlayedKey: whatever was playing immediately before the current
// thing -- set in openFile just before it switches currentKey over. Only
// one step of history, not a full stack: enough for a Previous control to
// mean something for an ad-hoc queue (which has no ordering of its own to
// look up a "previous" in) without keeping a whole playback history around.
var previousPlayedKey = null;
function peekPrev() {
  if (previousPlayedKey) return previousPlayedKey;
  if (playlistQueue && playlistPos > 0) return playlistQueue[playlistPos - 1].key;
  var prevEp = findPrevEpisode(currentPlayingFile());
  return prevEp ? prevEp.key : null;
}
function goPrev() {
  if (previousPlayedKey) { var k = previousPlayedKey; previousPlayedKey = null; openFile(null, k, false); return true; }
  if (playlistQueue && playlistPos > 0) { playlistPos--; openFile(null, playlistQueue[playlistPos].key, false, true); return true; }
  var prevEp = findPrevEpisode(currentPlayingFile());
  if (prevEp) { openFile(null, prevEp.key, false); return true; }
  return false;
}

// syncMiniControls keeps the mini player's own previous/play/next in step
// with whatever changed -- a new track, the queue, pause/play -- so they
// only ever offer to do something that's actually possible right now.
// Real glyphs (⏮⏸⏭) instead of SVG used to sit visibly off-centre in their
// circles -- those Miscellaneous Technical symbols carry font-dependent
// bearing that flexbox's align-items:center can't see past, since it centres
// the line box, not the glyph's actual ink. An SVG's viewBox has no such
// slack, so it lands dead-centre regardless of platform or font.
var MINI_PLAY_ICON  = '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M8 5v14l11-7z"/></svg>';
var MINI_PAUSE_ICON = '<svg viewBox="0 0 24 24" fill="currentColor"><path d="M6 19h4V5H6v14zm8-14v14h4V5h-4z"/></svg>';
function syncMiniControls() {
  if (!modalMiniPrev || !modalMiniPlay || !modalMiniNext) return;
  modalMiniPrev.disabled = !peekPrev();
  modalMiniNext.disabled = !peekNext();
  modalMiniPlay.innerHTML = modalVideo.paused ? MINI_PLAY_ICON : MINI_PAUSE_ICON;
}
if (modalMiniPrev) modalMiniPrev.addEventListener('click', function(e){ e.stopPropagation(); goPrev(); });
if (modalMiniNext) modalMiniNext.addEventListener('click', function(e){ e.stopPropagation(); goNext(); });
if (modalMiniPlay) modalMiniPlay.addEventListener('click', function(e){
  e.stopPropagation();
  if (modalVideo.paused) modalVideo.play().catch(function(){}); else modalVideo.pause();
});

function reportProgress() {
  if (!progressKey || !modalVideo.duration) return Promise.resolve();
  return fetch('/api/progress', {method:'POST', credentials:'include',
    headers:{'Content-Type':'application/json'},
    body: JSON.stringify({key: progressKey, position: modalVideo.currentTime, duration: modalVideo.duration})
  }).catch(function(){});
}

// After watching something the saved position has moved, the row's order has
// changed, and a finished item has been dropped server-side — none of which
// the grid knows until it is rebuilt. Rebuilding scrolls you back to the top
// unless the position is put back, so it is captured and restored.
function refreshProgressView() {
  if (currentShow || currentMovieDetail || viewMode === 'list') {
    return loadProgressMap().then(function(){
      if (currentShow || currentMovieDetail) renderGrid(currentFiles);
    });
  }
  var y = window.scrollY;
  return loadProgressMap().then(function(){
    renderGrid(currentFiles);
    window.scrollTo(0, y);
  });
}

// resumePlayback looks up any saved position for 'key' and seeks 'video'
// there once its metadata is available — immediately if it already is
// (common for local files, which load near-instantly), otherwise on
// 'loadedmetadata' (the usual case for HLS, which takes a moment to parse
// the manifest).
function resumePlayback(video, key, opts) {
  progressKey = key;
  lastProgressPost = 0;
  // A music video is three minutes long and gets rewatched, not continued.
  // Progress is still recorded — the continue-watching row and the bar on the
  // card both still work — but playback always opens at the start.
  if (opts && opts.fromStart) {
    try { video.currentTime = 0; } catch (e) {}
    return;
  }
  fetch('/api/progress?key=' + encodeURIComponent(key), {credentials:'include'})
    .then(function(r){ return r.json(); })
    .then(function(p){
      if (!(p && p.position > 5) || progressKey !== key) return;
      var seek = function(){ if (progressKey === key) video.currentTime = p.position; };
      if (video.readyState >= 1) seek();
      else video.addEventListener('loadedmetadata', seek, {once:true});
    })
    .catch(function(){});
}

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
      modalFname.textContent = updated.title ? mediaTag(updated) : key.split('/').pop();
      if (!updated.subtitle_url) {
        showOverlayBtn(modalCC);
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
  // Closing by button or backdrop must also unwind the history entry the
  // player pushed, so one back press never has to be pressed twice.
  if (modalInHistory && !closingFromPop) {
    modalInHistory = false;
    closingIntentionally = true;
    history.back();
    return;
  }
  closingIntentionally = false;
  modalInHistory = false;
  // Save the final position before currentTime resets below, then rebuild the
  // shelf from it so continue-watching shows where you actually got to.
  reportProgress().then(refreshProgressView);
  progressKey = null;
  modal.classList.remove('open');
  modal.classList.remove('chrome-off');
  modal.classList.remove('mini');
  lockPageScroll(false);
  clearTimeout(idleFadeTimer);
  if (modalTap) modalTap.style.display = 'none';
  // Leaving the player: hand the wash back to the wall's ambient tint, same
  // as closing a detail view does.
  clearTimeout(tintPending);
  if (tintDefault) withTint(tintDefault, applyTint);
  hidePauseCard();
  currentNextKey = null;
  upNextQueue = [];
  queueSessionActive = false;
  previousPlayedKey = null;
  // Left unreset here, this stuck around after the player closed: opening a
  // totally unrelated file later would still see an old, part-finished
  // playlist as "in progress" and report ITS next item as this file's next
  // -- the Next icon (and the mini player's Next button, and goNext() itself)
  // all trusted peekNext(), so all three were wrong, not just the icon.
  playlistQueue = null;
  playlistPos = 0;
  updateNowPlayingWash();
  modalNext.style.display = 'none';
  if (lastFocusedCard) { var c = lastFocusedCard; lastFocusedCard = null; setTimeout(function(){ c.focus(); }, 60); }
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

modalVideo.addEventListener('timeupdate', function() {
  if (!progressKey) return;
  var now = Date.now();
  if (now - lastProgressPost < 10000) return;
  lastProgressPost = now;
  reportProgress();
});
modalVideo.addEventListener('ended', function(){
  // goNext() carries the same priority this used to inline: anything
  // explicitly queued from the wall wins over what would have played
  // anyway; a playlist decides what follows next, and only outside one does
  // the show's own next episode. Nothing left in any of them closes it.
  if (!goNext()) closeModal();
});
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
  // Back out of the player first: it sits on top of whatever view is beneath
  // it, so one press should close it and leave that view alone. Minimized is
  // the exception -- it's a corner player specifically meant to keep playing
  // while you navigate the rest of the app, so browsing back out of a show
  // or movie page (its own history entry, same as the player's) must not
  // also kill it. But if the player minimized itself intentionally closed
  // (video ended with nothing queued, the close button while minimized),
  // this same popstate is the second half of that close finishing -- see
  // closingIntentionally -- and has to go through regardless.
  if (modal.classList.contains('open') && (!modal.classList.contains('mini') || closingIntentionally)) {
    // Captured before closeModal() runs, since closing removes 'mini' as
    // part of the same call.
    var wasMiniIntentionalClose = modal.classList.contains('mini') && closingIntentionally;
    closingFromPop = true;
    closeModal();
    closingFromPop = false;
    // A minimized player closing itself never actually left the wall --
    // it was showing underneath the whole time it was minimized, untouched
    // -- so there is no separate navigation here to also act on; doing so
    // anyway was a spurious extra render of a view that never changed,
    // which was the "closing it refreshes the screen" bug.
    if (wasMiniIntentionalClose || (st && st.type === 'player')) return;
  }
  if (!st || st.type === 'home') {
    currentShow = null;
    currentMovieDetail = null;
    currentPlaylist = null;
    playlistQueue = null;
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
  } else if (st.type === 'playlist') {
    currentPlaylist = playlists.filter(function(x){ return x.id === st.id; })[0] || null;
    currentShow = null;
    currentMovieDetail = null;
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
document.addEventListener('keydown', function(e){
  if (!searchOverlay.classList.contains('open')) return;
  if (e.key === 'Escape') { closeSearch(); return; }
  if (e.key === 'ArrowDown' || e.key === 'ArrowUp') {
    var rows = Array.prototype.slice.call(searchResults.querySelectorAll('[tabindex="0"]'));
    if (!rows.length) return;
    e.preventDefault();
    if (document.activeElement === searchInput) {
      if (e.key === 'ArrowDown') rows[0].focus();
      return;
    }
    var si = rows.indexOf(document.activeElement);
    if (si === -1) { rows[0].focus(); return; }
    if (e.key === 'ArrowUp' && si === 0) { searchInput.focus(); return; }
    var sn = e.key === 'ArrowDown' ? rows[si+1] : rows[si-1];
    if (sn) sn.focus();
  }
  if (e.key === 'Enter' && document.activeElement && searchResults.contains(document.activeElement)) {
    e.preventDefault();
    document.activeElement.click();
  }
});

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

var searchKind = 'media';

Array.prototype.forEach.call(document.querySelectorAll('.sk-btn'), function(b){
  b.addEventListener('click', function(){
    searchKind = b.dataset.kind;
    Array.prototype.forEach.call(document.querySelectorAll('.sk-btn'), function(x){
      x.classList.toggle('active', x === b);
    });
    searchInput.placeholder = searchKind === 'music'
      ? 'search youtube for a music video\u2026'
      : 'search for a movie or show\u2026';
    searchTvOpts.style.display = 'none';
    searchResults.innerHTML = '';
    var q = searchInput.value.trim();
    if (q) runSearch(q);
    searchInput.focus();
  });
});

function runSearch(q) {
  if (searchKind === 'music') { doMusicSearch(q); } else { doTMDBSearch(q); }
}

searchInput.addEventListener('input', function(){
  clearTimeout(searchTimer);
  var q = this.value.trim();
  if (!q) { searchResults.innerHTML = ''; return; }
  // yt-dlp search is a process launch, so it gets a longer settle than the
  // TMDB one rather than firing on every keystroke.
  var wait = searchKind === 'music' ? 650 : 400;
  searchTimer = setTimeout(function(){ runSearch(q); }, wait);
});

function fmtViews(n) {
  if (!n) return '';
  if (n >= 1e9) return (n/1e9).toFixed(1).replace(/\.0$/,'') + 'B views';
  if (n >= 1e6) return (n/1e6).toFixed(1).replace(/\.0$/,'') + 'M views';
  if (n >= 1e3) return Math.round(n/1e3) + 'K views';
  return n + ' views';
}

// watchMusicFetch polls the server's job state so the button shows a real
// percentage rather than sitting on "queued", and refreshes the library the
// moment the file lands instead of waiting for the next poll or a reload.
function watchMusicFetch(url, title, btn) {
  var key = 'yt:' + url;
  var misses = 0;
  (function poll(){
    fetch('/proc-status?key=' + encodeURIComponent(key), {credentials:'include'})
      .then(function(r){ return r.json(); })
      .then(function(st){
        if (st.status === 'error') {
          btn.disabled = false;
          btn.textContent = '\u2193 retry';
          showToast(st.error || 'Download failed', true);
          return;
        }
        if (st.status === 'done') {
          // The job is only cleared once the file is written, so this is the
          // completion signal — but the enrichment sweep still has to name it,
          // hence the reload rather than an optimistic insert.
          btn.textContent = '\u2713 saved';
          showToast('Saved \u2014 ' + title);
          lastLocalSig = null;   // force the next load to repaint
          loadLocalFiles();
          return;
        }
        // "processing": show whatever stage it reached.
        if (st.stage === 'merging') {
          btn.textContent = 'merging\u2026';
        } else if (st.percent > 0) {
          btn.textContent = Math.round(st.percent) + '%';
        } else {
          btn.textContent = 'starting\u2026';
        }
        misses = 0;
        setTimeout(poll, 1200);
      })
      .catch(function(){
        // A blip should not abandon a download that is still running.
        if (++misses > 5) {
          btn.disabled = false;
          btn.textContent = '\u2193 get';
          return;
        }
        setTimeout(poll, 2000);
      });
  })();
}

function doMusicSearch(q) {
  searchTvOpts.style.display = 'none';
  searchResults.innerHTML = '<div class="sr-msg">searching youtube\u2026</div>';
  fetch('/search/music?q=' + encodeURIComponent(q), {credentials:'include'})
    .then(function(r){ return r.ok ? r.json() : Promise.reject(r.status); })
    .then(function(hits){
      searchResults.innerHTML = '';
      if (!hits || !hits.length) {
        searchResults.innerHTML = '<div class="sr-msg">no results</div>';
        return;
      }
      hits.forEach(function(h){
        var row = document.createElement('div');
        row.className = 'sr-music';
        row.tabIndex = 0;
        var meta = [h.channel, h.duration ? fmtDur(Math.round(h.duration)) : '', fmtViews(h.views)]
          .filter(Boolean).map(esc).join('  \u00b7  ');
        row.innerHTML =
          (h.thumbnail ? '<img class="sr-music-thumb" src="' + esc(h.thumbnail) + '" loading="lazy" alt="">'
                       : '<div class="sr-music-thumb"></div>') +
          '<div class="sr-music-info">' +
            '<div class="sr-music-t">' + esc(h.title) + '</div>' +
            '<div class="sr-music-m">' +
              (h.official ? '<span class="sr-official">official</span>' : '') +
              '<span>' + meta + '</span>' +
            '</div>' +
          '</div>' +
          '<button class="sr-dl-btn">\u2193 get</button>';
        row.querySelector('.sr-dl-btn').addEventListener('click', function(e){
          e.stopPropagation();
          var btn = e.currentTarget;
          btn.disabled = true;
          btn.textContent = 'starting\u2026';
          fetch('/download/music', {
            method: 'POST', credentials: 'include',
            headers: {'Content-Type': 'application/json'},
            body: JSON.stringify({url: h.url, title: h.title})
          }).then(function(r){
            if (!r.ok) throw new Error('failed');
            watchMusicFetch(h.url, h.title, btn);
          }).catch(function(){
            btn.disabled = false;
            btn.textContent = '\u2193 get';
            showToast('Could not start the download', true);
          });
        });
        searchResults.appendChild(row);
      });
    })
    .catch(function(){
      searchResults.innerHTML = '<div class="sr-msg">search failed \u2014 is yt-dlp installed?</div>';
    });
}

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
        row.tabIndex = 0;
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
        row.tabIndex = 0;
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

// logoSVG is the app mark: a line-art sibling of the FileDrop logo — same
// 200x200 rounded-square field, same gold cradle and dim motion lines, with
// a play triangle as the focal object in place of that mark's file. Kept in
// sync with stream-logo.svg in the repo root.
const logoSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 200 200" width="200" height="200">
  <rect width="200" height="200" rx="36" fill="#110F0D"/>
  <rect x="52" y="128" width="96" height="20" rx="3" fill="none" stroke="#9A7D4A" stroke-width="3" stroke-linejoin="round"/>
  <line x1="52" y1="128" x2="52" y2="104" stroke="#9A7D4A" stroke-width="3" stroke-linecap="round"/>
  <line x1="148" y1="128" x2="148" y2="104" stroke="#9A7D4A" stroke-width="3" stroke-linecap="round"/>
  <path d="M82,50 L136,82 L82,114 Z" fill="#1C1915" stroke="#D4CFC8" stroke-width="2.5" stroke-linejoin="round"/>
  <line x1="56" y1="58" x2="70" y2="58" stroke="#5A554E" stroke-width="2" stroke-linecap="round"/>
  <line x1="56" y1="70" x2="70" y2="70" stroke="#5A554E" stroke-width="2" stroke-linecap="round"/>
  <line x1="56" y1="82" x2="70" y2="82" stroke="#3A3530" stroke-width="2" stroke-linecap="round"/>
</svg>`

func handleFavicon(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "image/svg+xml")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	fmt.Fprint(w, logoSVG)
}

// handleFaviconPNG rasterizes a deliberately simplified version of logoSVG:
// at 32px the 2.5px strokes and motion lines of the real mark collapse into
// mud, so this keeps only the two elements that still read at that size —
// the filled play triangle and the gold cradle bar under it.
func handleFaviconPNG(w http.ResponseWriter, r *http.Request) {
	const size = 32
	img := image.NewRGBA(image.Rect(0, 0, size, size))
	bg := color.RGBA{17, 15, 13, 255}
	gold := color.RGBA{154, 125, 74, 255}
	cream := color.RGBA{212, 207, 200, 255}
	for y := 0; y < size; y++ {
		for x := 0; x < size; x++ {
			img.Set(x, y, bg)
		}
	}
	// Play triangle: apex at (25,14), base spanning y 6..22 at x=9.
	const (
		baseX = 9
		apexX = 25
		apexY = 14
		halfH = 8
	)
	for y := apexY - halfH; y <= apexY+halfH; y++ {
		dy := y - apexY
		if dy < 0 {
			dy = -dy
		}
		edge := baseX + (apexX-baseX)*(halfH-dy)/halfH
		for x := baseX; x <= edge; x++ {
			img.Set(x, y, cream)
		}
	}
	// Gold cradle bar.
	for y := 25; y < 27; y++ {
		for x := 7; x < 25; x++ {
			img.Set(x, y, gold)
		}
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "public, max-age=86400")
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
	mux.HandleFunc("/api/progress", c.handleProgress)
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
	mux.HandleFunc("/search/music", c.localOnly(c.handleSearchMusic))
	mux.HandleFunc("/api/history", c.handleHistory)
	mux.HandleFunc("/playlists", c.handlePlaylists)
	mux.HandleFunc("/playlists/cover", c.handlePlaylistCover)
	mux.HandleFunc("/download/music", c.localOnly(c.handleDownloadMusic))
	mux.HandleFunc("/download", c.localOnly(c.handleDownload))
	mux.HandleFunc("/local/files", c.localOnly(c.handleLocalFiles))
	mux.HandleFunc("/local/file", c.localOnly(c.handleLocalFile))
	mux.HandleFunc("/local/file-token", c.localOnly(c.handleLocalFileToken))
	mux.HandleFunc("/local/subtitle", c.localOnly(c.handleLocalSubtitle))
	mux.HandleFunc("/local/xspf", c.handleLocalXSPF)
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
	// A shared auth secret makes this server validate the *same* session value
	// FileDrop issues. Combined with a matching cookie name it is one login
	// across both apps, and sessionHoursLeft reads the same expiry, so both
	// report the same hours remaining.
	secretHex := cfg.Secret
	if cfg.AuthSecret != "" {
		secretHex = cfg.AuthSecret
	}
	secret, _ := hex.DecodeString(secretHex)
	if cfg.AuthCookieName != "" {
		logger.Printf("session: sharing cookie %q with the configured auth source", cfg.AuthCookieName)
	}
	c := &serverCtx{
		passHash:    cfg.PasswordHash,
		secret:      secret,
		cookie:      cfg.AuthCookieName,
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
		watchFolder:   cfg.WatchFolder,
		sortToLibrary: cfg.SortDownloadsToLibrary,
		ytdlpPath:     cfg.YtdlpPath,
		ytdlpCookie:   cfg.YtdlpCookie,
		localFolders:  cfg.LocalLibraryFolders,
	}
	c.loginDisabled.Store(cfg.LoginDisabled)
	c.r2 = newR2Client(cfg.R2AccountID, cfg.R2AccessKey, cfg.R2SecretKey)
	loadProgressCache()
	loadHistory()
	loadPlaylists()
	// The metadata cache is what watchKey resolves TMDB ids from, and the
	// enrichment goroutine below would otherwise load it after the seed runs.
	loadLocalMetaCache()
	seedHistoryFromProgress()
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
