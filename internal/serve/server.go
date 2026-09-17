package serve

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/noamsto/hookyard/internal/manifest"
	"github.com/noamsto/hookyard/internal/vocab"
)

// Options is the server's configuration.
type Options struct {
	StateDir string
	Port     int
	Out      io.Writer // banner sink; nil -> os.Stdout is set by Run
	Now      func() time.Time
}

// Run binds, prints the banner, seeds, then serves until ctx is done.
func Run(ctx context.Context, opts Options) error {
	if opts.Out == nil {
		opts.Out = io.Discard // Run chooses the zero, not the caller; the sentinel is handled below
	}
	out := opts.Out
	if out == io.Discard {
		// Only the test path sets Out=nil; the real path uses os.Stdout.
		// This is here so the function signature does not import "os".
		out = nil
	}

	if opts.Port < 0 {
		return fmt.Errorf("serve: port %d is negative", opts.Port)
	}

	addr := net.JoinHostPort(listenHost, strconv.Itoa(opts.Port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("serve: %w", err)
	}
	defer func() { _ = ln.Close() }()

	actualAddr := ln.Addr().String()
	host, port, _ := net.SplitHostPort(actualAddr)
	if out != nil {
		_, _ = fmt.Fprintf(out, "hookyard serve \u2014 http://%s:%s\n", host, port)
	}

	absDir, err := filepath.Abs(opts.StateDir)
	if err != nil {
		return fmt.Errorf("cannot resolve a state directory: %w", err)
	}
	if absDir == "" {
		return fmt.Errorf("cannot resolve a state directory: empty path")
	}
	if out != nil {
		_, _ = fmt.Fprintf(out, "reading %s\n", absDir)
	}

	now := opts.Now
	if now == nil {
		now = time.Now
	}

	hub := NewHub(absDir, now)
	if err := hub.Seed(); err != nil {
		return fmt.Errorf("serve: seed: %w", err)
	}

	srv := &http.Server{
		Handler: middleware(&serveMux{
			stateDir: absDir,
			hub:      hub,
			port:     port,
		}),
		BaseContext: func(_ net.Listener) context.Context { return ctx },
	}

	go func() {
		<-ctx.Done()
		_ = srv.Shutdown(context.Background())
	}()

	hubErr := make(chan error, 1)
	go func() { hubErr <- hub.Run(ctx) }()

	serveErr := srv.Serve(ln)
	if serveErr != nil && serveErr != http.ErrServerClosed {
		return serveErr
	}
	return <-hubErr
}

// serveMux owns the routing state that a plain http.ServeMux would hold and
// that middleware needs to reach (the port for Host checking).
type serveMux struct {
	stateDir string
	hub      *Hub
	port     string
}

func middleware(h *serveMux) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/", h.handleIndex)
	mux.HandleFunc("/static/", h.handleStatic)
	mux.HandleFunc("/api/days", h.handleDays)
	mux.HandleFunc("/api/events", h.handleEvents)
	mux.HandleFunc("/api/stats", h.handleStats)
	mux.HandleFunc("/api/stream", h.handleStream)
	mux.HandleFunc("/api/table", h.handleTable)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Method guard: non-GET → 405.
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", http.MethodGet)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}

		// Host guard: DNS rebinding defense (SPEC 4.9).
		allowed := make(map[string]bool)
		allowed[listenHost+":"+h.port] = true
		allowed["localhost:"+h.port] = true
		allowed["[::1]:"+h.port] = true
		if !allowed[strings.ToLower(r.Host)] {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}

		// No CORS and no cache (SPEC 4.9).
		w.Header().Set("Cache-Control", "no-store")

		mux.ServeHTTP(w, r)
	})
}

func (h *serveMux) handleIndex(w http.ResponseWriter, _ *http.Request) {
	content, err := indexHTML()
	if err != nil {
		http.Error(w, "embedded page not found", http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(content)
}

func (h *serveMux) handleStatic(w http.ResponseWriter, r *http.Request) {
	sub, err := staticFS()
	if err != nil {
		http.Error(w, "static assets not embedded", http.StatusInternalServerError)
		return
	}
	http.StripPrefix("/static/", http.FileServer(http.FS(sub))).ServeHTTP(w, r)
}

func (h *serveMux) handleDays(w http.ResponseWriter, _ *http.Request) {
	days, err := Days(h.stateDir)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	today := DayString(h.hub.Now())
	writeJSON(w, DaysResponse{Days: days, Today: today})
}

func (h *serveMux) handleEvents(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	day := q.Get("day")
	if day == "" {
		day = h.hub.Day()
	}

	limitStr := q.Get("limit")
	limit := 0
	if limitStr != "" {
		if n, err := strconv.Atoi(limitStr); err == nil {
			limit = n
		}
	}

	// before is the "load older" cursor (SPEC 4.4): the scan window ends
	// there instead of at the file size, which is what the UI and the
	// documented contract both name it.
	beforeStr := q.Get("before")
	var before int64
	if beforeStr != "" {
		before, _ = strconv.ParseInt(beforeStr, 10, 64)
	}

	f := ParseFilter(q)
	resp, err := ScanDay(h.stateDir, day, before, limit, f)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, resp)
}

func (h *serveMux) handleStats(w http.ResponseWriter, r *http.Request) {
	day := r.URL.Query().Get("day")
	if day == "" {
		day = h.hub.Day()
	}

	// Live day: served from the in-memory accumulator for freshness
	// (SPEC 4.5). Any other day: a one-shot pass over the file.
	if snap, ok := h.hub.Snapshot(day); ok {
		writeJSON(w, snap)
		return
	}

	snap, err := StatsForDay(h.stateDir, day)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	writeJSON(w, snap)
}

func (h *serveMux) handleStream(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")

	ctx := r.Context()
	f := ParseFilter(r.URL.Query())
	sub := h.hub.Subscribe(f, 256)
	defer h.hub.Unsubscribe(sub)

	var wErr error
	write := func(event, id string, data any) {
		if wErr != nil {
			return
		}
		if event != "" {
			_, wErr = fmt.Fprintf(w, "event: %s\n", event)
		}
		if id != "" {
			_, wErr = fmt.Fprintf(w, "id: %s\n", id)
		}
		if wErr == nil && data != nil {
			b, err := json.Marshal(data)
			if err != nil {
				wErr = err
				return
			}
			_, wErr = fmt.Fprintf(w, "data: %s\n", b)
		}
		if wErr == nil {
			_, wErr = fmt.Fprint(w, "\n")
		}
		if wErr == nil {
			flusher.Flush()
		}
	}

	keepaliveTicker := time.NewTicker(25 * time.Second)
	defer keepaliveTicker.Stop()

	for {
		select {
		case f, ok := <-sub.C:
			if !ok {
				return
			}
			write(f.Event, f.ID, f.Data)
		case <-keepaliveTicker.C:
			if wErr != nil {
				return
			}
			_, wErr = fmt.Fprint(w, ": keepalive\n")
			flusher.Flush()
		case <-ctx.Done():
			return
		}
	}
}

func (h *serveMux) handleTable(w http.ResponseWriter, _ *http.Request) {
	absPath := filepath.Join(h.stateDir, "table.json")
	handlers, readErr := manifest.ReadTable(absPath)

	// Vocabularies are compiled-in; they must be filled on every response,
	// error path included — dropping them when table.json is missing would
	// leave two of the five filters with no options (SPEC 4.7).
	engineStrings := make([]string, len(vocab.Engines))
	for i, e := range vocab.Engines {
		engineStrings[i] = string(e)
	}

	resp := TableResponse{
		Handlers: handlers,
		Engines:  engineStrings,
		Events:   vocab.CanonicalEvents,
		Path:     absPath,
	}
	if readErr != nil {
		resp.Error = readErr.Error()
		resp.Handlers = nil
	}
	writeJSON(w, resp)
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	b, err := json.Marshal(v)
	if err != nil {
		http.Error(w, "could not marshal response", http.StatusInternalServerError)
		return
	}
	_, _ = w.Write(b)
}
