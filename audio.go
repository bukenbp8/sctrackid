package main

import (
	"bufio"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"time"
)

const (
	coarseFactor    = 3  // adaptive mode: probe every 3rd window, bisect where results differ
	silentPeaks     = 20 // windows with fewer spectral peaks than this are not sent
	maxConsecutive  = 4  // consecutive 429s before giving up on Shazam for this run
	maxRequestDelay = 10 * time.Second
)

type sample struct {
	offset int
	sig    []byte
	n      int
	peaks  int
	alt    *sample // a second window half an interval later, for double-checking lone matches
}

// pacer spaces requests start-to-start and adapts to rate limiting:
// slow down on 429, slowly speed back up after a streak of successes.
type pacer struct {
	delay, min time.Duration
	last       time.Time
	streak     int
}

func (p *pacer) wait(ctx context.Context) {
	if d := time.Until(p.last.Add(p.delay)); d > 0 {
		sleepCtx(ctx, d)
	}
	p.last = time.Now()
}

func (p *pacer) success() {
	if p.streak++; p.streak >= 8 {
		p.delay, p.streak = max(p.min, p.delay*9/10), 0
	}
}

func (p *pacer) limited() {
	p.delay, p.streak = min(maxRequestDelay, p.delay*3/2), 0
}

// --- result cache (lets reruns and resumed runs skip Shazam entirely) ---

type shazamCache struct {
	path string
	hits map[int]*ShazamTrack // offset -> result (nil = no match)
	meta *cacheMeta           // nil until a run got through the whole set
	f    *os.File
}

// cacheMeta describes the windows of a completely decoded set, so a later run
// that finds all its answers in the cache does not have to fetch the audio.
type cacheMeta struct {
	Interval int   `json:"interval"`
	N        int   `json:"n"`      // regular windows
	Alts     []int `json:"alts"`   // offsets of the in-between windows that exist
	Silent   []int `json:"silent"` // offsets (either kind) too quiet to send
}

// windows rebuilds the window list without signatures.
func (m *cacheMeta) windows() <-chan sample {
	alts, silent := map[int]bool{}, map[int]bool{}
	for _, o := range m.Alts {
		alts[o] = true
	}
	for _, o := range m.Silent {
		silent[o] = true
	}
	mk := func(off int) sample {
		s := sample{offset: off, peaks: silentPeaks}
		if silent[off] {
			s.peaks = 0
		}
		return s
	}
	ch := make(chan sample, m.N)
	for i := 0; i < m.N; i++ {
		s := mk(m.Interval/2 + i*m.Interval)
		if a := s.offset + m.Interval/2; alts[a] {
			as := mk(a)
			s.alt = &as
		}
		ch <- s
	}
	close(ch)
	return ch
}

func (c *shazamCache) putMeta(interval int, got []sample) {
	m := &cacheMeta{Interval: interval, N: len(got), Alts: []int{}, Silent: []int{}}
	for _, s := range got {
		if s.peaks < silentPeaks {
			m.Silent = append(m.Silent, s.offset)
		}
		if s.alt != nil {
			m.Alts = append(m.Alts, s.alt.offset)
			if s.alt.peaks < silentPeaks {
				m.Silent = append(m.Silent, s.alt.offset)
			}
		}
	}
	c.meta = m
	if c.f != nil {
		b, _ := json.Marshal(map[string]any{"meta": m})
		_, _ = c.f.Write(append(b, '\n'))
	}
}

func openCache(track *Track, o options) *shazamCache {
	c := &shazamCache{hits: map[int]*ShazamTrack{}}
	dir, err := os.UserCacheDir()
	if err != nil {
		return c
	}
	name := fmt.Sprintf("%d-%ds", track.ID, o.sampleLen)
	if o.file != "" { // a local file may be offset differently from the stream
		if st, err := os.Stat(o.file); err == nil {
			name += fmt.Sprintf("-file%d", st.Size())
		}
	}
	c.path = filepath.Join(dir, "sctrackid", "shazam", name+".jsonl")
	if !o.noCache {
		if f, err := os.Open(c.path); err == nil {
			sc := bufio.NewScanner(f)
			sc.Buffer(nil, 4<<20) // the meta line of a long set is long
			for sc.Scan() {
				var line struct {
					O    int          `json:"o"`
					T    *ShazamTrack `json:"t"`
					Meta *cacheMeta   `json:"meta"`
				}
				if json.Unmarshal(sc.Bytes(), &line) != nil {
					continue
				}
				if line.Meta != nil {
					c.meta = line.Meta
				} else {
					c.hits[line.O] = line.T
				}
			}
			f.Close()
		}
	}
	_ = os.MkdirAll(filepath.Dir(c.path), 0o755)
	c.f, _ = os.OpenFile(c.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	return c
}

func (c *shazamCache) put(offset int, t *ShazamTrack) {
	c.hits[offset] = t
	if c.f != nil {
		b, _ := json.Marshal(map[string]any{"o": offset, "t": t})
		_, _ = c.f.Write(append(b, '\n'))
	}
}

func (c *shazamCache) close() {
	if c.f != nil {
		c.f.Close()
	}
}

// decode streams audio through ffmpeg and turns every window into a signature.
// The channel is large so decoding never stalls (SoundCloud stream URLs expire).
func decode(ctx context.Context, input string, o options) (<-chan sample, <-chan error) {
	out := make(chan sample, 8192)
	errc := make(chan error, 1)
	go func() {
		defer close(out)
		cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-loglevel", "error", "-i", input,
			"-vn", "-f", "s16le", "-ac", "1", "-ar", "16000", "-")
		cmd.Stderr = logOut
		stdout, err := cmd.StdoutPipe()
		if err != nil {
			errc <- err
			return
		}
		if err := cmd.Start(); err != nil {
			errc <- err
			return
		}
		r := bufio.NewReaderSize(stdout, 1<<20)
		pos := 0
		buf := make([]byte, o.sampleLen*sampleRate*2)
		// read the next window; short reports that the audio ended inside it
		window := func(off int) (s *sample, short bool) {
			n, rerr := io.ReadFull(r, buf)
			pos += n / 2
			if n/2 < 4*sampleRate {
				return nil, true
			}
			samples := make([]int16, n/2)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(buf[i*2:]))
			}
			sig, peaks := MakeSignature(samples)
			return &sample{offset: off, sig: sig, n: len(samples), peaks: peaks}, rerr != nil
		}
		altGap := (o.interval/2 - o.sampleLen) * sampleRate // < 0: no room for in-between windows
		for off := o.interval / 2; ; off += o.interval {
			if skip := off*sampleRate - pos; skip > 0 {
				n, _ := io.CopyN(io.Discard, r, int64(skip*2))
				pos += int(n / 2)
				if int(n/2) < skip {
					break
				}
			}
			s, short := window(off)
			if s == nil {
				break
			}
			if !short && altGap >= 0 {
				n, _ := io.CopyN(io.Discard, r, int64(altGap*2))
				pos += int(n / 2)
				if short = int(n/2) < altGap; !short {
					s.alt, short = window(off + o.interval/2)
				}
			}
			out <- *s
			if short {
				break
			}
		}
		_, _ = io.Copy(io.Discard, r)
		errc <- cmd.Wait()
	}()
	return out, errc
}

var (
	errShazamBlocked = errors.New("Shazam keeps rate limiting; stopped early (finished samples are cached, rerun later to resume)")
	errNeedAudio     = errors.New("a sample that is not cached is needed")
)

var recognize = Recognize // swapped out in tests

func audioCandidates(ctx context.Context, sc *SoundCloud, track *Track, dur int, o options, onPause func(), onMatch func(artist, title string)) ([]Candidate, error) {
	cache := openCache(track, o)
	defer cache.close()

	// A complete earlier run recorded which windows the set has. If everything
	// this run asks Shazam is already cached, no audio is needed at all.
	if m := cache.meta; m != nil && m.Interval == o.interval {
		out, _, err := scanWindows(ctx, m.windows(), cache, dur, o, onPause, onMatch)
		if !errors.Is(err, errNeedAudio) {
			if err == nil {
				logf("· audio: every sample was already cached, nothing downloaded")
			}
			return out, err
		}
		logf("· some samples are not cached yet, fetching the audio")
	}

	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, errors.New("ffmpeg not found in PATH; skipping fingerprinting")
	}
	input := o.file
	if input == "" {
		u, snipped, err := sc.StreamURL(ctx, track)
		if err != nil {
			return nil, err
		}
		if snipped || track.Policy == "SNIP" {
			logf("! only a 30s preview is streamable (Go+ track); use -file with your own copy for full coverage")
		}
		input = u
	}
	stream, decodeErr := decode(ctx, input, o)
	out, got, err := scanWindows(ctx, stream, cache, dur, o, onPause, onMatch)
	if err != nil {
		return out, err
	}
	if err := <-decodeErr; err != nil {
		return out, fmt.Errorf("ffmpeg: %w", err)
	}
	cache.putMeta(o.interval, got)
	return out, nil
}

// scanWindows asks Shazam about the windows of a set: a coarse pass refined
// where results differ, then a second listen for every track heard only once.
// Windows without a signature (rebuilt from the cache) can only be answered
// from the cache; needing more than that returns errNeedAudio. On success the
// returned windows are the complete set.
func scanWindows(ctx context.Context, stream <-chan sample, cache *shazamCache, dur int, o options, onPause func(), onMatch func(artist, title string)) ([]Candidate, []sample, error) {
	var (
		got     []sample
		results = map[int]*ShazamTrack{} // window offset -> result
		done    = map[int]bool{}
		client  = &http.Client{Timeout: 20 * time.Second}
		pace    = &pacer{delay: o.delay, min: o.delay}
		stats   struct{ requests, cached, silent, limited, matches int }
		blocked bool
		starved bool
	)
	need := func(i int) bool {
		for len(got) <= i {
			s, ok := <-stream
			if !ok {
				return false
			}
			got = append(got, s)
		}
		return true
	}
	progress := func(s sample, status string) {
		if o.verbose {
			fmt.Fprintf(logOut, "\r\033[K  %8s  %s\n", fmtTime(s.offset), status)
		}
		fmt.Fprintf(logOut, "\r\033[K· fingerprinting at %s / %s — %d requests, %d cached, %d matches",
			fmtTime(s.offset), fmtTime(dur), stats.requests, stats.cached, stats.matches)
	}

	query := func(s sample) *ShazamTrack {
		if done[s.offset] || blocked || ctx.Err() != nil {
			return results[s.offset]
		}
		var t *ShazamTrack
		status := "—"
		if c, ok := cache.hits[s.offset]; ok {
			t, status = c, "— (cached)"
			stats.cached++
		} else if s.peaks < silentPeaks {
			status = fmt.Sprintf("— (near-silent, %d peaks, not sent)", s.peaks)
			stats.silent++
		} else if s.sig == nil {
			starved, blocked = true, true
			return nil
		} else {
			var err error
			for consecutive := 1; ; consecutive++ {
				pace.wait(ctx)
				stats.requests++
				t, err = recognize(ctx, client, s.sig, s.n)
				var rl rateLimitedError
				if !errors.As(err, &rl) {
					break
				}
				stats.limited++
				pace.limited()
				onPause() // use the idle time for the 1001tracklists lookup
				if consecutive >= maxConsecutive {
					blocked = true
					return nil
				}
				cool := rl.RetryAfter
				if cool == 0 {
					cool = time.Duration(15<<(consecutive-1)) * time.Second
				}
				fmt.Fprintf(logOut, "\r\033[K! rate limited, cooling down %s (pace now %.1fs/request)…", cool, pace.delay.Seconds())
				sleepCtx(ctx, cool)
				if ctx.Err() != nil {
					return nil
				}
			}
			if err != nil {
				progress(s, "error: "+err.Error())
				return nil // not cached, not marked done: a rerun will retry it
			}
			pace.success()
			cache.put(s.offset, t)
		}
		if t != nil {
			status = t.Subtitle + " - " + t.Title
			stats.matches++
			onMatch(t.Subtitle, t.Title)
		}
		done[s.offset], results[s.offset] = true, t
		progress(s, status)
		return t
	}

	same := func(a, b *ShazamTrack) bool {
		return a != nil && b != nil && (a.Key == b.Key || sameTrack(a.Subtitle, a.Title, b.Subtitle, b.Title))
	}
	var refine func(a, b int)
	refine = func(a, b int) {
		if b-a <= 1 || blocked || ctx.Err() != nil || same(results[got[a].offset], results[got[b].offset]) {
			return // same track on both ends: assume it plays throughout
		}
		m := (a + b) / 2
		query(got[m])
		refine(a, m)
		refine(m, b)
	}

	step := coarseFactor
	if o.thorough {
		step = 1
	}
	prev := -1
	for c := 0; need(c); c += step {
		query(got[c])
		if prev >= 0 {
			refine(prev, c)
		}
		prev = c
		if blocked || ctx.Err() != nil {
			break
		}
	}
	if !blocked && ctx.Err() == nil && len(got)-1 > prev && prev >= 0 {
		last := len(got) - 1
		query(got[last])
		refine(prev, last)
	}

	// Matches in chronological order, so merging is deterministic.
	type hit struct {
		at     int // window index
		t      *ShazamTrack
		extra  []sample // in-between windows that heard it too
		extraT []*ShazamTrack
		weak   bool
	}
	var hits []*hit
	for i, s := range got {
		if t := results[s.offset]; t != nil {
			hits = append(hits, &hit{at: i, t: t})
		}
	}

	// A track heard in a single window is as often a stray match as a real
	// one. Listen again half an interval before and after: heard again, it
	// counts twice; not heard, it is kept but marked as unconfirmed.
	var verify struct{ checked, confirmed, weak int }
	for _, h := range hits {
		lone := true
		for _, x := range hits {
			if x == h {
				continue
			}
			near := max(got[x.at].offset-got[h.at].offset, got[h.at].offset-got[x.at].offset) <= versionSpan
			if same(x.t, h.t) || near && sameSong(x.t.Subtitle, x.t.Title, h.t.Subtitle, h.t.Title) {
				lone = false
				break
			}
		}
		if !lone || blocked || ctx.Err() != nil {
			continue
		}
		var checks []sample
		if a := got[h.at].alt; a != nil {
			checks = append(checks, *a)
		}
		if h.at > 0 && got[h.at-1].alt != nil {
			checks = append(checks, *got[h.at-1].alt)
		}
		if len(checks) == 0 {
			continue
		}
		verify.checked++
		conclusive := true
		for _, a := range checks {
			if r := query(a); same(r, h.t) {
				h.extra, h.extraT = append(h.extra, a), append(h.extraT, r)
				break
			}
			conclusive = conclusive && done[a.offset]
		}
		switch {
		case len(h.extra) > 0:
			verify.confirmed++
		case conclusive:
			h.weak = true
			verify.weak++
		}
	}

	fmt.Fprint(logOut, "\r\033[K")
	if starved {
		return nil, nil, errNeedAudio
	}
	logf("· shazam: %d requests, %d cached, %d silent skipped, %d rate limits, %d samples checked in %d windows",
		stats.requests, stats.cached, stats.silent, stats.limited, len(done), len(got))
	if verify.checked > 0 {
		logf("· single matches listened to again: %d confirmed, %d not (kept as low confidence)", verify.confirmed, verify.weak)
	}

	var out []Candidate
	for _, h := range hits {
		c := Candidate{Artist: h.t.Subtitle, Title: h.t.Title, TimeSec: got[h.at].offset, Source: "shazam", URL: h.t.URL, Weak: h.weak}
		c.Start, c.HasStart = h.t.startIn(c.TimeSec)
		out = append(out, c)
		for i, a := range h.extra {
			c.TimeSec = a.offset
			c.Start, c.HasStart = h.extraT[i].startIn(c.TimeSec)
			out = append(out, c)
		}
	}
	switch {
	case blocked:
		return out, nil, errShazamBlocked
	case ctx.Err() != nil:
		return out, nil, ctx.Err()
	}
	return out, got, nil
}
