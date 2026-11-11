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
	Refresh       bool   `json:"refresh,omitempty"` // ignore the library and decode again

	Retry *retryRef `json:"retry,omitempty"` // set by the server: re-identify one track of a stored set
}

type retryRef struct {
	SetID int64 `json:"set_id,string"`
	entryRef
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
	lib   *library // nil when disabled

	mu     sync.Mutex
	jobs   []*job // oldest first
	nextID int
	wake   chan struct{}

	subMu sync.Mutex
	subs  map[chan event]struct{}
}

func serveWeb(ctx context.Context, addr string, base options, gc guardConfig, dbPath string) error {
	if _, err := strconv.Atoi(addr); err == nil {
		addr = "localhost:" + addr // "-serve 8080"
	}
	g, err := newGuard(ctx, gc)
	if err != nil {
		return err
	}
	s := &server{base: base, guard: g, wake: make(chan struct{}, 1), subs: map[chan event]struct{}{}}
	if dbPath != "" {
		if s.lib, err = openLibrary(dbPath); err != nil {
			return err
		}
		defer s.lib.db.Close()
		fmt.Fprintf(os.Stderr, "library: %s\n", dbPath)
	}
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
	mux.HandleFunc("GET /api/library", s.handleLibrarySearch)
	mux.HandleFunc("GET /api/library/{id}", s.handleLibraryGet)
	mux.HandleFunc("GET /api/library/{id}/txt", s.handleLibraryText)
	mux.HandleFunc("POST /api/library/{id}/approve", s.handleApprove)
	mux.HandleFunc("POST /api/library/{id}/retry", s.handleRetry)

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
		if q := s.queuedLocked(); len(q) > 0 {
			next = q[0]
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
	o.noCache = o.noCache || j.Req.Refresh // a forced rescan asks Shazam again instead of replaying cached answers
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
	var rep *Report
	var err error
	if j.Req.Retry != nil {
		rep, err = s.retry(ctx, j, o)
	} else {
		rep, err = analyze(ctx, j.Req.URL, o)
		if rep != nil && err == nil && ctx.Err() == nil && s.lib != nil {
			if lerr := s.lib.Save(rep, j.Req.URL, optsOf(j.Req, s.base)); lerr != nil {
				logf("! could not save to library: %v", lerr)
			} else {
				defer s.publish(event{Type: "library", Text: strconv.FormatInt(rep.Track.ID, 10)})
			}
		}
	}
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

// retry listens again at one track of a stored set and swaps in what it finds.
func (s *server) retry(ctx context.Context, j *job, o options) (*Report, error) {
	rt := j.Req.Retry
	cur, err := s.lib.Get(rt.SetID, "")
	if err != nil {
		return nil, errors.New("that set is not in the archive any more")
	}
	i := findEntry(cur.Entries, rt.entryRef)
	if i < 0 {
		return nil, errNoEntry
	}
	others := append(append([]*Entry(nil), cur.Entries[:i]...), cur.Entries[i+1:]...)
	found, absorbed, rejected, err := retryEntry(ctx, j.Req.URL, cur.Entries[i], others, o)
	if err != nil {
		return nil, err
	}
	rep, err := s.lib.Update(rt.SetID, func(es []*Entry) ([]*Entry, error) {
		i := findEntry(es, rt.entryRef)
		if i < 0 || es[i].Approved {
			return nil, errNoEntry
		}
		if absorbed == nil {
			es[i] = found
			return es, nil
		}
		// A neighbour plays there: remember the rejection on it, so a rescan
		// does not bring the wrong track back, and drop the wrong entry.
		n := findEntry(es, entryRef{absorbed.Artist, absorbed.Title, absorbed.TimeSec})
		if n < 0 {
			return nil, errNoEntry
		}
		es[n].Rejected = append(es[n].Rejected, rejected...)
		return append(es[:i], es[i+1:]...), nil
	})
	if err != nil {
		return nil, err
	}
	s.libraryChanged(rep)
	return rep, nil
}

// libraryChanged brings finished jobs showing the same set up to date and
// tells browsers to reload it.
func (s *server) libraryChanged(rep *Report) {
	text := rep.Text()
	s.mu.Lock()
	for _, j := range s.jobs {
		if j.result != nil && j.Finished != nil && j.result.Track.ID == rep.Track.ID {
			j.result, j.text, j.Tracks = rep, text, len(rep.Entries)
		}
	}
	s.mu.Unlock()
	s.publish(event{Type: "library", Text: strconv.FormatInt(rep.Track.ID, 10)})
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

// queuedLocked lists the waiting jobs in the order they will run. Retries
// take seconds, so they go ahead of full lookups.
func (s *server) queuedLocked() []*job {
	var retries, rest []*job
	for _, j := range s.jobs {
		switch {
		case j.State != stateQueued:
		case j.Req.Retry != nil:
			retries = append(retries, j)
		default:
			rest = append(rest, j)
		}
	}
	return append(retries, rest...)
}

// snapshotLocked copies the public job fields and numbers the queue.
func (s *server) snapshotLocked() []job {
	pos := map[*job]int{}
	for i, j := range s.queuedLocked() {
		pos[j] = i + 1
	}
	out := make([]job, 0, len(s.jobs))
	for _, j := range s.jobs {
		c := *j
		c.log, c.result, c.cancel, c.owner = nil, nil, nil, ""
		c.Position = pos[j]
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
	req.Retry = nil
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
	if s.lib != nil && !req.Refresh {
		if id, ok := s.lib.Lookup(req.URL, optsOf(req, s.base)); ok {
			writeJSON(w, 200, map[string]string{"library_id": strconv.FormatInt(id, 10)})
			return
		}
	}
	s.enqueue(w, r, req)
}

// enqueue applies the per-client and queue limits and adds the job.
func (s *server) enqueue(w http.ResponseWriter, r *http.Request, req jobRequest) {
	client := s.guard.clientKey(r)
	var title, uploader string
	var dur int
	if req.Retry != nil { // the set is known already: label the job right away
		if rep, err := s.lib.Get(req.Retry.SetID, ""); err == nil {
			title, uploader, dur = rep.Track.Title, rep.Track.User.Username, rep.Dur
		}
	}

	s.mu.Lock()
	active, queued := 0, 0
	for _, j := range s.jobs {
		if j.State != stateQueued && j.State != stateRunning {
			continue
		}
		same := (j.Req.Retry == nil) == (req.Retry == nil) && (req.Retry == nil || *j.Req.Retry == *req.Retry)
		if j.Req.URL == req.URL && same { // the same link already waiting or running: don't queue it twice
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
	j := &job{ID: s.nextID, Req: req, State: stateQueued, Created: time.Now(), owner: client,
		Title: title, Uploader: uploader, Duration: dur}
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
	if j.owner != s.guard.clientKey(r) {
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
	client := s.guard.clientKey(r)
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

// --- library ---

func (s *server) handleLibrarySearch(w http.ResponseWriter, r *http.Request) {
	if s.lib == nil {
		writeJSON(w, 200, map[string]any{"enabled": false, "sets": []librarySet{}})
		return
	}
	q := r.URL.Query().Get("q")
	if len(q) > 200 {
		q = q[:200]
	}
	sets, err := s.lib.Search(q, 50)
	if err != nil {
		httpError(w, 500, "search failed")
		return
	}
	writeJSON(w, 200, map[string]any{"enabled": true, "sets": sets})
}

// minParam is the ?min= confidence filter, "" when absent or invalid.
func minParam(r *http.Request) string {
	min := r.URL.Query().Get("min")
	if _, ok := confRank[min]; !ok {
		min = ""
	}
	return min
}

// handleApprove marks one track of a stored set as confirmed by a user (or takes that back).
func (s *server) handleApprove(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if s.lib == nil || err != nil {
		httpError(w, 404, "no such set")
		return
	}
	var body struct {
		entryRef
		Approved bool `json:"approved"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		httpError(w, 400, "invalid request")
		return
	}
	rep, err := s.lib.Update(id, func(es []*Entry) ([]*Entry, error) {
		i := findEntry(es, body.entryRef)
		if i < 0 {
			return nil, errNoEntry
		}
		es[i].Approved = body.Approved
		return es, nil
	})
	switch {
	case errors.Is(err, errNoEntry):
		httpError(w, 409, err.Error())
		return
	case err != nil:
		httpError(w, 404, "no such set")
		return
	}
	s.libraryChanged(rep)
	if min := minParam(r); min != "" {
		c := *rep
		c.Entries = filterConfidence(rep.Entries, min)
		rep = &c
	}
	writeJSON(w, 200, map[string]any{"identified": rep.Identified, "result": rep.JSON()})
}

// handleRetry queues a job that listens again at one track of a stored set.
func (s *server) handleRetry(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if s.lib == nil || err != nil {
		httpError(w, 404, "no such set")
		return
	}
	var body struct {
		entryRef
		PowChallenge string `json:"pow_challenge"`
		PowNonce     string `json:"pow_nonce"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<16)).Decode(&body); err != nil {
		httpError(w, 400, "invalid request")
		return
	}
	rep, err := s.lib.Get(id, "")
	if err != nil {
		httpError(w, 404, "no such set")
		return
	}
	switch i := findEntry(rep.Entries, body.entryRef); {
	case i < 0:
		httpError(w, 409, errNoEntry.Error())
		return
	case rep.Entries[i].Approved:
		httpError(w, 409, "this track is confirmed — unconfirm it first")
		return
	case rep.Entries[i].TimeSec < 0:
		httpError(w, 409, "this track has no timestamp, so it cannot be listened to again")
		return
	}
	if msg := s.guard.checkPoW(body.PowChallenge, body.PowNonce); msg != "" {
		httpError(w, 403, msg)
		return
	}
	s.enqueue(w, r, jobRequest{URL: rep.Track.PermalinkURL, Retry: &retryRef{SetID: id, entryRef: body.entryRef}})
}

func (s *server) libraryReport(w http.ResponseWriter, r *http.Request) *Report {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if s.lib == nil || err != nil {
		httpError(w, 404, "no such set")
		return nil
	}
	min := minParam(r)
	rep, err := s.lib.Get(id, min)
	if err != nil {
		httpError(w, 404, "no such set")
		return nil
	}
	return rep
}

func (s *server) handleLibraryGet(w http.ResponseWriter, r *http.Request) {
	if rep := s.libraryReport(w, r); rep != nil {
		writeJSON(w, 200, map[string]any{"identified": rep.Identified, "result": rep.JSON()})
	}
}

func (s *server) handleLibraryText(w http.ResponseWriter, r *http.Request) {
	rep := s.libraryReport(w, r)
	if rep == nil {
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if r.URL.Query().Get("dl") != "" {
		w.Header().Set("Content-Disposition", "attachment; filename*=UTF-8''"+url.PathEscape(textFileName(rep.Track)))
	}
	_, _ = io.WriteString(w, rep.Text())
}
