package main

// Web interface: a single worker processes one lookup at a time; further
// submissions wait in a FIFO queue. Progress is pushed to browsers over SSE.

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

const (
	maxJobLog     = 3000 // log lines kept per job
	maxFinished   = 50   // finished jobs kept in history
	sseBufferSize = 512
)

type jobState string

const (
	stateQueued   jobState = "queued"
	stateRunning  jobState = "running"
	stateDone     jobState = "done"
	stateFailed   jobState = "failed"
	stateCanceled jobState = "canceled"
)

type jobRequest struct {
	URL           string `json:"url"`
	Thorough      bool   `json:"thorough"`
	NoAudio       bool   `json:"no_audio"`
	NoComments    bool   `json:"no_comments"`
	No1001        bool   `json:"no_1001"`
	NoLinks       bool   `json:"no_links"`
	MinConfidence string `json:"min_confidence"`
}

type job struct {
	ID       int        `json:"id"`
	Req      jobRequest `json:"request"`
	State    jobState   `json:"state"`
	Position int        `json:"position,omitempty"` // 1-based place in the queue
	Created  time.Time  `json:"created"`
	Started  *time.Time `json:"started,omitempty"`
	Finished *time.Time `json:"finished,omitempty"`
	Title    string     `json:"title,omitempty"`
	Uploader string     `json:"uploader,omitempty"`
	Duration int        `json:"duration_sec,omitempty"`
	Progress string     `json:"progress,omitempty"`
	Error    string     `json:"error,omitempty"`
	Tracks   int        `json:"track_count"`
	File     string     `json:"file,omitempty"`

	owner  string // clientKey of the submitter
	log    []string
	result *Report
	text   string
	cancel context.CancelFunc
}

type event struct {
	Type string `json:"type"`
	ID   int    `json:"id,omitempty"`
	Text string `json:"text,omitempty"`
	Jobs []job  `json:"jobs,omitempty"`
}

type server struct {
	base  options
	guard *guard

	mu     sync.Mutex
	jobs   []*job // oldest first
	nextID int
	wake   chan struct{}

	subMu sync.Mutex
	subs  map[chan event]struct{}
}

func serveWeb(ctx context.Context, addr string, base options, gc guardConfig) error {
	if _, err := strconv.Atoi(addr); err == nil {
		addr = "localhost:" + addr // "-serve 8080"
	}
	s := &server{base: base, guard: newGuard(ctx, gc), wake: make(chan struct{}, 1), subs: map[chan event]struct{}{}}
	go s.worker(ctx)

	static, _ := fs.Sub(webFS, "web")
	mux := http.NewServeMux()
	mux.Handle("GET /", http.FileServerFS(static))
	mux.HandleFunc("GET /api/challenge", s.guard.handleChallenge)
	mux.HandleFunc("GET /api/jobs", s.handleList)
	mux.HandleFunc("POST /api/jobs", s.handleSubmit)
	mux.HandleFunc("GET /api/jobs/{id}", s.handleGet)
	mux.HandleFunc("DELETE /api/jobs/{id}", s.handleDelete)
	mux.HandleFunc("GET /api/jobs/{id}/txt", s.handleText)
	mux.HandleFunc("GET /api/events", s.handleEvents)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{
		Handler:           mux,
		BaseContext:       func(net.Listener) context.Context { return ctx },
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       2 * time.Minute,
		MaxHeaderBytes:    16 << 10,
	}
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	fmt.Fprintf(os.Stderr, "sctrackid web interface on http://%s\n", ln.Addr())
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// --- queue ---

func (s *server) worker(ctx context.Context) {
	for {
		s.mu.Lock()
		var next *job
		for _, j := range s.jobs {
			if j.State == stateQueued {
				next = j
				break
			}
		}
		if next == nil {
			s.mu.Unlock()
			select {
			case <-ctx.Done():
				return
			case <-s.wake:
				continue
			}
		}
		jctx, cancel := context.WithCancel(ctx)
		now := time.Now()
		next.State, next.Started, next.cancel = stateRunning, &now, cancel
		s.mu.Unlock()
		s.broadcastJobs()

		s.runJob(jctx, next)
		cancel()
		if ctx.Err() != nil {
			return
		}
	}
}

func (s *server) runJob(ctx context.Context, j *job) {
	o := s.base
	o.thorough = j.Req.Thorough
	o.noAudio = j.Req.NoAudio
	o.noComments = j.Req.NoComments
	o.no1001 = j.Req.No1001
	o.noLinks = j.Req.NoLinks
	if j.Req.MinConfidence != "" {
		o.minConf = j.Req.MinConfidence
	}
	o.onResolved = func(t *Track, dur int) {
		s.mu.Lock()
		j.Title, j.Uploader, j.Duration = t.Title, t.User.Username, dur
		s.mu.Unlock()
		s.broadcastJobs()
	}

	w := &jobWriter{s: s, j: j}
	logOut.Set(io.MultiWriter(os.Stderr, w))
	fmt.Fprintf(os.Stderr, "\n== job #%d: %s\n", j.ID, j.Req.URL)
	rep, err := analyze(ctx, j.Req.URL, o)
	var file, text string
	if rep != nil {
		text = rep.Text()
		file = textFileName(rep.Track)
		if werr := os.WriteFile(file, []byte(text), 0o644); werr != nil {
			logf("! could not write text file: %v", werr)
			file = ""
		} else {
			logf("· saved tracklist to %s", file)
		}
	}
	w.flush()
	logOut.Set(os.Stderr)

	s.mu.Lock()
	now := time.Now()
	j.Finished, j.Progress, j.cancel = &now, "", nil
	j.result, j.text, j.File = rep, text, file
	if rep != nil {
		j.Tracks = len(rep.Entries)
	}
	switch {
	case ctx.Err() != nil:
		j.State = stateCanceled
	case err != nil:
		j.State, j.Error = stateFailed, err.Error()
	default:
		j.State = stateDone
	}
	s.pruneLocked()
	s.mu.Unlock()
	s.broadcastJobs()
}

func (s *server) pruneLocked() {
	finished := 0
	for _, j := range s.jobs {
		if j.Finished != nil {
			finished++
		}
	}
	kept := s.jobs[:0]
	for _, j := range s.jobs {
		if j.Finished != nil && finished > maxFinished {
			finished--
			continue
		}
		kept = append(kept, j)
	}
	s.jobs = kept
}

// snapshotLocked copies the public job fields and numbers the queue.
func (s *server) snapshotLocked() []job {
	out := make([]job, 0, len(s.jobs))
	pos := 0
	for _, j := range s.jobs {
		c := *j
		c.log, c.result, c.cancel, c.owner = nil, nil, nil, ""
		c.Position = 0
		if j.State == stateQueued {
			pos++
			c.Position = pos
		}
		out = append(out, c)
	}
	return out
}

func (s *server) find(r *http.Request) *job {
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil {
		return nil
	}
	for _, j := range s.jobs {
		if j.ID == id {
			return j
		}
	}
	return nil
}

// --- live output ---

// jobWriter turns the terminal-style output (\r + ESC[K progress lines) into
// log lines and a separate, overwritable progress status.
type jobWriter struct {
	s   *server
	j   *job
	buf []byte
}

func (w *jobWriter) Write(p []byte) (int, error) {
	for i := 0; i < len(p); i++ {
		switch c := p[i]; {
		case c == '\x1b': // skip ANSI escape sequence
			for i++; i < len(p) && !(p[i] >= '@' && p[i] <= '~' && p[i] != '['); i++ {
			}
		case c == '\r':
			w.buf = w.buf[:0]
		case c == '\n':
			w.line(string(w.buf))
			w.buf = w.buf[:0]
		default:
			w.buf = append(w.buf, c)
		}
	}
	if len(w.buf) > 0 {
		w.progress(string(w.buf))
	}
	return len(p), nil
}

func (w *jobWriter) flush() {
	if len(w.buf) > 0 {
		w.line(string(w.buf))
		w.buf = w.buf[:0]
	}
}

func (w *jobWriter) line(text string) {
	if strings.TrimSpace(text) == "" {
		return
	}
	w.s.mu.Lock()
	w.j.log = append(w.j.log, text)
	if len(w.j.log) > maxJobLog {
		w.j.log = w.j.log[len(w.j.log)-maxJobLog:]
	}
	w.s.mu.Unlock()
	w.s.publish(event{Type: "log", ID: w.j.ID, Text: text})
}

func (w *jobWriter) progress(text string) {
	w.s.mu.Lock()
	w.j.Progress = text
	w.s.mu.Unlock()
	w.s.publish(event{Type: "progress", ID: w.j.ID, Text: text})
}

func (s *server) publish(e event) {
	s.subMu.Lock()
	defer s.subMu.Unlock()
	for ch := range s.subs {
		select {
		case ch <- e:
		default: // slow client: drop; it resyncs from /api/jobs/{id}
		}
	}
}

func (s *server) broadcastJobs() {
	s.mu.Lock()
	jobs := s.snapshotLocked()
	s.mu.Unlock()
	s.publish(event{Type: "jobs", Jobs: jobs})
}

// --- HTTP handlers ---

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func httpError(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

func (s *server) handleList(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	jobs := s.snapshotLocked()
	s.mu.Unlock()
	writeJSON(w, 200, jobs)
}

// submitRequest is a jobRequest plus the anti-bot fields, which are not kept with the job.
type submitRequest struct {
	jobRequest
	Website      string `json:"website"` // honeypot: hidden from humans, bots fill it in
	PowChallenge string `json:"pow_challenge"`
	PowNonce     string `json:"pow_nonce"`
}

func (s *server) handleSubmit(w http.ResponseWriter, r *http.Request) {
	var sr submitRequest
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&sr); err != nil {
		httpError(w, 400, "invalid request")
		return
	}
	if sr.Website != "" {
		httpError(w, 400, "invalid request")
		return
	}
	req := sr.jobRequest
	req.URL = strings.TrimSpace(req.URL)
	u, err := url.Parse(req.URL)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") ||
		!(u.Host == "soundcloud.com" || strings.HasSuffix(u.Host, ".soundcloud.com")) {
		httpError(w, 400, "that is not a SoundCloud link")
		return
	}
	switch req.MinConfidence {
	case "", "low", "medium", "high":
	default:
		httpError(w, 400, "min_confidence must be low, medium or high")
		return
	}

	if msg := s.guard.checkPoW(sr.PowChallenge, sr.PowNonce); msg != "" {
		httpError(w, 403, msg)
		return
	}
	client := clientKey(r)

	s.mu.Lock()
	active, queued := 0, 0
	for _, j := range s.jobs {
		if j.State != stateQueued && j.State != stateRunning {
			continue
		}
		if j.Req.URL == req.URL { // the same link already waiting or running: don't queue it twice
			s.mu.Unlock()
			httpError(w, 409, fmt.Sprintf("already in the queue as job #%d", j.ID))
			return
		}
		if j.owner == client {
			active++
		}
		if j.State == stateQueued {
			queued++
		}
	}
	if gc := s.guard.cfg; gc.maxActivePerIP > 0 && active >= gc.maxActivePerIP {
		s.mu.Unlock()
		httpError(w, 429, fmt.Sprintf("you already have %d job(s) waiting or running — wait for one to finish", active))
		return
	}
	if gc := s.guard.cfg; gc.maxQueue > 0 && queued >= gc.maxQueue {
		s.mu.Unlock()
		httpError(w, 503, "the queue is full — try again later")
		return
	}
	if ok, retry := s.guard.allowSubmit(client); !ok {
		s.mu.Unlock()
		w.Header().Set("Retry-After", strconv.Itoa(int(retry.Seconds())+1))
		httpError(w, 429, fmt.Sprintf("hourly submission limit reached — try again in %d min", int(retry.Minutes())+1))
		return
	}
	s.nextID++
	j := &job{ID: s.nextID, Req: req, State: stateQueued, Created: time.Now(), owner: client}
	s.jobs = append(s.jobs, j)
	s.mu.Unlock()

	select {
	case s.wake <- struct{}{}:
	default:
	}
	s.broadcastJobs()
	writeJSON(w, 201, map[string]int{"id": j.ID})
}

func (s *server) handleGet(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := s.find(r)
	if j == nil {
		httpError(w, 404, "no such job")
		return
	}
	var summary job
	for _, c := range s.snapshotLocked() {
		if c.ID == j.ID {
			summary = c
		}
	}
	resp := map[string]any{"job": summary, "log": append([]string{}, j.log...)}
	if j.result != nil {
		resp["result"] = j.result.JSON()
	}
	writeJSON(w, 200, resp)
}

// DELETE cancels a running job, removes a queued one, or clears a finished one from history.
func (s *server) handleDelete(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.find(r)
	if j == nil {
		s.mu.Unlock()
		httpError(w, 404, "no such job")
		return
	}
	if j.owner != clientKey(r) {
		s.mu.Unlock()
		httpError(w, 403, "only whoever submitted this job can cancel or remove it")
		return
	}
	switch j.State {
	case stateRunning:
		if j.cancel != nil {
			j.cancel()
		}
	default:
		for i, x := range s.jobs {
			if x == j {
				s.jobs = append(s.jobs[:i], s.jobs[i+1:]...)
				break
			}
		}
	}
	s.mu.Unlock()
	s.broadcastJobs()
	w.WriteHeader(204)
}

func (s *server) handleText(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	j := s.find(r)
	var text, name string
	if j != nil && j.result != nil {
		text, name = j.text, textFileName(j.result.Track)
	}
	s.mu.Unlock()
	if text == "" {
		httpError(w, 404, "no tracklist for this job")
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(name))
	_, _ = io.WriteString(w, text)
}

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	fl, ok := w.(http.Flusher)
	if !ok {
		httpError(w, 500, "streaming unsupported")
		return
	}
	client := clientKey(r)
	if !s.guard.openStream(client) {
		httpError(w, 429, "too many open connections")
		return
	}
	defer s.guard.closeStream(client)

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")

	ch := make(chan event, sseBufferSize)
	s.subMu.Lock()
	s.subs[ch] = struct{}{}
	s.subMu.Unlock()
	defer func() {
		s.subMu.Lock()
		delete(s.subs, ch)
		s.subMu.Unlock()
	}()

	s.mu.Lock()
	initial := event{Type: "jobs", Jobs: s.snapshotLocked()}
	s.mu.Unlock()
	send := func(e event) bool {
		b, _ := json.Marshal(e)
		if _, err := fmt.Fprintf(w, "data: %s\n\n", b); err != nil {
			return false
		}
		fl.Flush()
		return true
	}
	if !send(initial) {
		return
	}
	ping := time.NewTicker(20 * time.Second)
	defer ping.Stop()
	for {
		select {
		case <-r.Context().Done():
			return
		case e := <-ch:
			if !send(e) {
				return
			}
		case <-ping.C:
			if _, err := io.WriteString(w, ": ping\n\n"); err != nil {
				return
			}
			fl.Flush()
		}
	}
}
