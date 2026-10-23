// Package ui serves BigBadRescue's window: a local web page talking to a
// small HTTP API on 127.0.0.1.
package ui

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/FemoTheDon/bigbadrescue/internal/convert"
	"github.com/FemoTheDon/bigbadrescue/internal/evernote"
	"github.com/FemoTheDon/bigbadrescue/internal/pace"
	"github.com/FemoTheDon/bigbadrescue/internal/rescue"
	"github.com/FemoTheDon/bigbadrescue/internal/store"
	"github.com/FemoTheDon/bigbadrescue/internal/version"
)

//go:embed web
var webFS embed.FS

// Options configure Run.
type Options struct {
	Port     int    // the window's port; Evernote's sign-in returns here, so it is fixed
	Open     bool   // open the window
	Stay     bool   // keep running after the window closes
	FFmpeg   string // path to ffmpeg ("" = not found)
	FFmpegOK string // version line, or "" when FFmpegEr is set
	FFmpegEr error
	Endpoint string // Evernote MCP server
	NoAuth   bool   // the server needs no sign-in (demo)
	DataDir  string // settings, memory and cache
	Demo     bool
}

type server struct {
	opts     Options
	st       *store.Store
	ev       *evernote.Client
	lim      *pace.Limiter
	eng      *rescue.Engine
	quit     chan struct{}
	quitOnce sync.Once
	clients  sync.Map // chan sseMsg -> struct{}
	nClient  int
	cmu      sync.Mutex
	lastGone time.Time
	ctx      context.Context
}

type sseMsg struct {
	event string
	data  []byte
}

// Run starts the server and blocks until the app quits.
func Run(opts Options) error {
	if alreadyRunning(opts.Port) {
		url := fmt.Sprintf("http://127.0.0.1:%d/", opts.Port)
		log.Printf("%s is already running at %s", version.App, url)
		if opts.Open {
			return openWindow(url)
		}
		return nil
	}
	ln, err := net.Listen("tcp", fmt.Sprintf("127.0.0.1:%d", opts.Port))
	if err != nil {
		return fmt.Errorf("port %d is in use by another program; Evernote's sign-in needs it (%w)", opts.Port, err)
	}
	st, err := store.Open(filepath.Join(opts.DataDir, "memory.json"))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	s := &server{opts: opts, st: st, quit: make(chan struct{}), ctx: ctx}
	set := st.Settings()
	if opts.Demo {
		set.BackupDir = filepath.Join(opts.DataDir, "Backups")
		_ = st.SetSettings(set)
	}
	s.lim = pace.New(time.Duration(set.IntervalMs)*time.Millisecond, func(ps pace.Status) {
		st.SetPause(ps.PausedUntil, ps.Reason)
		s.broadcast("pace", ps)
	})
	if until, reason := st.Pause(); !until.IsZero() {
		s.lim.Restore(until, reason)
	}
	s.ev, err = evernote.New(evernote.Config{
		Endpoint:          opts.Endpoint,
		NoAuth:            opts.NoAuth,
		RedirectURL:       fmt.Sprintf("http://127.0.0.1:%d/oauth/callback", opts.Port),
		OpenBrowser:       openBrowser,
		Tokens:            evernote.TokenStore{Service: "BigBadRescue", File: filepath.Join(opts.DataDir, "signin.json")},
		ClientID:          set.ClientID,
		ClientMetadataURL: set.ClientMetadataURL,
		Limiter:           s.lim,
		OnState:           func(cs evernote.State) { s.broadcast("conn", cs) },
	})
	if err != nil {
		return err
	}
	s.eng = rescue.New(s.ev, st, &convert.Converter{FFmpeg: opts.FFmpeg}, filepath.Join(opts.DataDir, "cache"), s.broadcast)
	go s.eng.Run(ctx)
	go s.tickPace(ctx)
	if s.ev.HasSavedSignIn() {
		go func() { _ = s.ev.Connect(ctx) }()
	}

	srv := &http.Server{Handler: s.routes()}
	go func() { _ = srv.Serve(ln) }()
	url := fmt.Sprintf("http://%s/", ln.Addr().String())
	log.Printf("%s is running at %s", version.Title(), url)
	if opts.Open {
		if err := openWindow(url); err != nil {
			log.Printf("open %s in your browser (%v)", url, err)
		}
	}
	if !opts.Stay {
		go s.watchWindow()
	}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	go func() { <-sig; s.shutdown() }()
	<-s.quit
	cancel()
	sctx, scancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer scancel()
	_ = srv.Shutdown(sctx)
	return nil
}

func alreadyRunning(port int) bool {
	c := http.Client{Timeout: 700 * time.Millisecond}
	resp, err := c.Get(fmt.Sprintf("http://127.0.0.1:%d/api/hello", port))
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
	return strings.Contains(string(b), version.App)
}

// watchWindow quits the app a little while after the last window closes,
// unless a job is running or waiting. Jobs that are paused for Evernote's
// rate limit keep the app open too.
func (s *server) watchWindow() {
	started := time.Now()
	for {
		select {
		case <-s.quit:
			return
		case <-time.After(2 * time.Second):
		}
		s.cmu.Lock()
		n, gone := s.nClient, s.lastGone
		s.cmu.Unlock()
		if n > 0 || s.eng.Busy() {
			continue
		}
		if gone.IsZero() {
			if time.Since(started) > 60*time.Second {
				log.Print("no window opened; quitting")
				s.shutdown()
				return
			}
			continue
		}
		if time.Since(gone) > 12*time.Second {
			log.Print("window closed; quitting")
			s.shutdown()
			return
		}
	}
}

// tickPace keeps the footer's call count current between pause changes.
func (s *server) tickPace(ctx context.Context) {
	var last pace.Status
	t := time.NewTicker(2 * time.Second)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if ps := s.lim.Status(); ps.CallsLastHour != last.CallsLastHour || ps.State != last.State {
				last = ps
				s.broadcast("pace", ps)
			}
		}
	}
}

func (s *server) shutdown() { s.quitOnce.Do(func() { close(s.quit) }) }

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	sub, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", http.FileServer(http.FS(sub)))
	mux.HandleFunc("GET /api/hello", func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, version.Title())
	})
	mux.HandleFunc("GET /oauth/callback", s.oauthCallback)
	mux.HandleFunc("GET /api/state", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.stateMap()) })
	mux.HandleFunc("GET /api/events", s.events)
	mux.HandleFunc("POST /api/connect", s.connect)
	mux.HandleFunc("POST /api/signout", s.signOut)
	mux.HandleFunc("GET /api/notebooks", s.notebooks)
	mux.HandleFunc("POST /api/search", s.search)
	mux.HandleFunc("POST /api/scan", s.scan)
	mux.HandleFunc("GET /api/note", s.note)
	mux.HandleFunc("GET /api/audio", s.audio)
	mux.HandleFunc("GET /api/review/audio", s.reviewAudio)
	mux.HandleFunc("GET /api/items", func(w http.ResponseWriter, r *http.Request) { writeJSON(w, 200, s.st.All()) })
	mux.HandleFunc("POST /api/review", s.review)
	mux.HandleFunc("POST /api/convert", s.enqueue("convert"))
	mux.HandleFunc("POST /api/cleanup", s.enqueue("cleanup"))
	mux.HandleFunc("POST /api/stop", func(w http.ResponseWriter, r *http.Request) {
		s.eng.Stop()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("POST /api/resume", func(w http.ResponseWriter, r *http.Request) {
		s.lim.Resume()
		writeJSON(w, 200, map[string]bool{"ok": true})
	})
	mux.HandleFunc("PUT /api/settings", s.putSettings)
	mux.HandleFunc("POST /api/pick/backup", s.pickBackup)
	mux.HandleFunc("POST /api/reveal", s.reveal)
	mux.HandleFunc("POST /api/quit", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, 200, map[string]bool{"ok": true})
		go func() { time.Sleep(200 * time.Millisecond); s.shutdown() }()
	})
	return localOnly(mux)
}

// localOnly refuses requests from other web pages, so a site open in your
// browser can't drive the app. The OAuth callback is a plain top-level
// navigation from Evernote's sign-in page, which sends no Origin header.
func localOnly(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, _ := net.SplitHostPort(r.Host)
		if host != "127.0.0.1" && host != "localhost" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if o := r.Header.Get("Origin"); o != "" && !strings.HasPrefix(o, "http://127.0.0.1:") && !strings.HasPrefix(o, "http://localhost:") {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		w.Header().Set("Cache-Control", "no-store")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, err error) {
	var rl *evernote.RateLimitError
	if errors.As(err, &rl) {
		code = http.StatusTooManyRequests
	}
	writeJSON(w, code, map[string]string{"error": err.Error()})
}

func readJSON(r *http.Request, v any) error {
	if r.ContentLength == 0 {
		return nil
	}
	return json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(v)
}

func (s *server) stateMap() map[string]any {
	ff := map[string]any{"path": s.opts.FFmpeg, "version": s.opts.FFmpegOK}
	if s.opts.FFmpegEr != nil {
		ff["error"] = s.opts.FFmpegEr.Error()
	}
	return map[string]any{
		"app": version.App, "company": version.Company, "version": version.Version,
		"formats":  convert.Formats,
		"presets":  []convert.Preset{convert.Voice, convert.Music},
		"ffmpeg":   ff,
		"settings": s.st.Settings(),
		"conn":     s.ev.State(),
		"pace":     s.lim.Status(),
		"queue":    s.eng.Queue(),
		"demo":     s.opts.Demo,
		"canPick":  runtime.GOOS == "darwin" || hasZenity(),
		"endpoint": s.opts.Endpoint,
	}
}

// events streams changes to the window (Server-Sent Events).
func (s *server) events(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming unsupported", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	ch := make(chan sseMsg, 512)
	s.clients.Store(ch, struct{}{})
	s.cmu.Lock()
	s.nClient++
	s.cmu.Unlock()
	defer func() {
		s.clients.Delete(ch)
		s.cmu.Lock()
		s.nClient--
		s.lastGone = time.Now()
		s.cmu.Unlock()
	}()
	b, _ := json.Marshal(s.stateMap())
	fmt.Fprintf(w, "event: state\ndata: %s\n\n", b)
	fl.Flush()
	ping := time.NewTicker(15 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.quit:
			fmt.Fprint(w, "event: bye\ndata: {}\n\n")
			fl.Flush()
			return
		case <-ping.C:
			fmt.Fprint(w, ": ping\n\n")
			fl.Flush()
		case m := <-ch:
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", m.event, m.data)
			fl.Flush()
		}
	}
}

func (s *server) broadcast(event string, v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	s.clients.Range(func(k, _ any) bool {
		select {
		case k.(chan sseMsg) <- sseMsg{event, b}:
		default:
		}
		return true
	})
}

func (s *server) connect(w http.ResponseWriter, r *http.Request) {
	// Signing in can take minutes in the browser, so it runs in the
	// background and reports through the "conn" event.
	go func() {
		ctx, cancel := context.WithTimeout(s.ctx, 15*time.Minute)
		defer cancel()
		if err := s.ev.Connect(ctx); err != nil {
			log.Printf("connect: %v", err)
		}
	}()
	writeJSON(w, 200, map[string]bool{"ok": true})
}

func (s *server) signOut(w http.ResponseWriter, r *http.Request) {
	s.eng.Stop()
	if err := s.ev.SignOut(); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}

var callbackPage = `<!doctype html><meta charset="utf-8"><title>BigBadRescue</title>
<style>body{font:16px -apple-system,system-ui,sans-serif;display:grid;place-items:center;height:90vh;background:#f6f3ee;color:#2b2420}
main{text-align:center;max-width:30em}h1{font-size:1.4em}@media(prefers-color-scheme:dark){body{background:#17130f;color:#f3ebe2}}</style>
<main><h1>%s</h1><p>%s</p></main>`

func (s *server) oauthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	if msg := s.ev.Callback(q.Get("code"), q.Get("state"), q.Get("iss"), q.Get("error"), q.Get("error_description")); msg != "" {
		fmt.Fprintf(w, callbackPage, "Sign-in didn't complete", html.EscapeString(msg)+". Go back to BigBadRescue and press Connect to try again.")
		return
	}
	fmt.Fprintf(w, callbackPage, "You're signed in", "You can close this tab and go back to BigBadRescue.")
}

func (s *server) notebooks(w http.ResponseWriter, r *http.Request) {
	nbs, err := s.ev.Notebooks(r.Context())
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, nbs)
}

func (s *server) search(w http.ResponseWriter, r *http.Request) {
	var f rescue.Filter
	if err := readJSON(r, &f); err != nil {
		writeErr(w, 400, err)
		return
	}
	rows, err := s.eng.Search(r.Context(), f)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, map[string]any{"rows": rows, "capped": len(rows) >= rescue.MaxResults})
}

func (s *server) scan(w http.ResponseWriter, r *http.Request) {
	var f rescue.Filter
	if err := readJSON(r, &f); err != nil {
		writeErr(w, 400, err)
		return
	}
	counts, err := s.eng.Scan(r.Context(), f)
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, counts)
}

func (s *server) note(w http.ResponseWriter, r *http.Request) {
	d, err := s.eng.NoteDetail(r.Context(), r.URL.Query().Get("id"))
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	writeJSON(w, 200, d)
}

func (s *server) audio(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := s.eng.Preview(r.Context(), q.Get("note"), q.Get("hash"), q.Get("mime"))
	if err != nil {
		writeErr(w, 502, err)
		return
	}
	serveAudio(w, r, p)
}

func (s *server) reviewAudio(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	p, err := s.eng.ReviewFile(r.Context(), q.Get("key"), q.Get("which"))
	if err != nil {
		writeErr(w, 404, err)
		return
	}
	serveAudio(w, r, p)
}

func serveAudio(w http.ResponseWriter, r *http.Request, p string) {
	switch strings.ToLower(filepath.Ext(p)) {
	case ".mp3":
		w.Header().Set("Content-Type", "audio/mpeg")
	case ".m4a":
		w.Header().Set("Content-Type", "audio/mp4")
	case ".wav":
		w.Header().Set("Content-Type", "audio/wav")
	}
	http.ServeFile(w, r, p)
}

func (s *server) review(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Keys []string `json:"keys"`
		OK   bool     `json:"ok"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	writeJSON(w, 200, s.eng.Review(req.Keys, req.OK))
}

func (s *server) enqueue(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Notes []store.QueuedJob `json:"notes"`
		}
		if err := readJSON(r, &req); err != nil {
			writeErr(w, 400, err)
			return
		}
		if s.opts.FFmpegEr != nil && kind == "convert" {
			writeErr(w, 409, errors.New("ffmpeg isn't available, so nothing can be converted"))
			return
		}
		n := s.eng.Enqueue(kind, req.Notes)
		writeJSON(w, 200, map[string]int{"queued": n})
	}
}

func (s *server) putSettings(w http.ResponseWriter, r *http.Request) {
	st := s.st.Settings()
	if err := readJSON(r, &st); err != nil {
		writeErr(w, 400, err)
		return
	}
	if st.BackupDir == "" {
		st.BackupDir = store.DefaultSettings().BackupDir
	}
	st.BackupDir = expandHome(st.BackupDir)
	if !filepath.IsAbs(st.BackupDir) {
		writeErr(w, 400, errors.New("the backup folder must be a full path, like /Users/you/Music/BigBadRescue Backups"))
		return
	}
	st.IntervalMs = min(max(st.IntervalMs, 300), 30000)
	st.Preset = convert.PresetByName(st.Preset).Name
	if err := s.st.SetSettings(st); err != nil {
		writeErr(w, 500, err)
		return
	}
	s.lim.SetInterval(time.Duration(st.IntervalMs) * time.Millisecond)
	s.broadcast("settings", st)
	writeJSON(w, 200, st)
}

func expandHome(p string) string {
	p = strings.TrimSpace(p)
	if p == "~" || strings.HasPrefix(p, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(p, "~"))
		}
	}
	return p
}

func (s *server) pickBackup(w http.ResponseWriter, r *http.Request) {
	st := s.st.Settings()
	start := st.BackupDir
	if _, err := os.Stat(start); err != nil {
		start, _ = os.UserHomeDir()
	}
	dir, err := pickFolder("Choose where BigBadRescue keeps backups of your original recordings", start)
	if errors.Is(err, errCancelled) {
		writeJSON(w, 200, map[string]bool{"cancelled": true})
		return
	} else if err != nil {
		writeErr(w, 500, err)
		return
	}
	st.BackupDir = dir
	_ = s.st.SetSettings(st)
	s.broadcast("settings", st)
	writeJSON(w, 200, st)
}

func (s *server) reveal(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Path string `json:"path"`
	}
	if err := readJSON(r, &req); err != nil {
		writeErr(w, 400, err)
		return
	}
	p := req.Path
	if p == "" {
		p = s.st.Settings().BackupDir
		_ = os.MkdirAll(p, 0o755)
		if err := reveal(p, false); err != nil {
			writeErr(w, 500, err)
			return
		}
	} else if err := reveal(p, true); err != nil {
		writeErr(w, 500, err)
		return
	}
	writeJSON(w, 200, map[string]bool{"ok": true})
}
