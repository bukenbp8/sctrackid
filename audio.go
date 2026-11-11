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
	"sort"
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
	f    *os.File
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
			for sc.Scan() {
				var line struct {
					O int          `json:"o"`
					T *ShazamTrack `json:"t"`
				}
				if json.Unmarshal(sc.Bytes(), &line) == nil {
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
		cmd.Stderr = os.Stderr
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
		for off := o.interval / 2; ; off += o.interval {
			if skip := off*sampleRate - pos; skip > 0 {
				n, _ := io.CopyN(io.Discard, r, int64(skip*2))
				pos += int(n / 2)
				if int(n/2) < skip {
					break
				}
			}
			n, rerr := io.ReadFull(r, buf)
			pos += n / 2
			if n/2 < 4*sampleRate {
				break
			}
			samples := make([]int16, n/2)
			for i := range samples {
				samples[i] = int16(binary.LittleEndian.Uint16(buf[i*2:]))
			}
			sig, peaks := MakeSignature(samples)
			out <- sample{off, sig, len(samples), peaks}
			if rerr != nil {
				break
			}
		}
		_, _ = io.Copy(io.Discard, r)
		errc <- cmd.Wait()
	}()
	return out, errc
}

var errShazamBlocked = errors.New("Shazam keeps rate limiting; stopped early (finished samples are cached, rerun later to resume)")

func audioCandidates(ctx context.Context, sc *SoundCloud, track *Track, dur int, o options, onPause func(), onMatch func(artist, title string)) ([]Candidate, error) {
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, errors.New("ffmpeg not found in PATH; skipping fingerprinting")
	}
	cache := openCache(track, o)
	defer cache.close()

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

	var (
		got     []sample
		results = map[int]*ShazamTrack{} // window index -> result
		done    = map[int]bool{}
		client  = &http.Client{Timeout: 20 * time.Second}
		pace    = &pacer{delay: o.delay, min: o.delay}
		stats   struct{ requests, cached, silent, limited, matches int }
		blocked bool
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
			fmt.Fprintf(os.Stderr, "\r\033[K  %8s  %s\n", fmtTime(s.offset), status)
		}
		fmt.Fprintf(os.Stderr, "\r\033[K· fingerprinting at %s / %s — %d requests, %d cached, %d matches",
			fmtTime(s.offset), fmtTime(dur), stats.requests, stats.cached, stats.matches)
	}

	query := func(i int) *ShazamTrack {
		if done[i] || blocked || ctx.Err() != nil {
			return results[i]
		}
		s := got[i]
		var t *ShazamTrack
		status := "—"
		if c, ok := cache.hits[s.offset]; ok {
			t, status = c, "— (cached)"
			stats.cached++
		} else if s.peaks < silentPeaks {
			status = fmt.Sprintf("— (near-silent, %d peaks, not sent)", s.peaks)
			stats.silent++
		} else {
			var err error
			for consecutive := 1; ; consecutive++ {
				pace.wait(ctx)
				stats.requests++
				t, err = Recognize(ctx, client, s.sig, s.n)
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
				fmt.Fprintf(os.Stderr, "\r\033[K! rate limited, cooling down %s (pace now %.1fs/request)…", cool, pace.delay.Seconds())
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
		done[i], results[i] = true, t
		progress(s, status)
		return t
	}

	same := func(a, b *ShazamTrack) bool {
		return a != nil && b != nil && (a.Key == b.Key || sameTrack(a.Subtitle, a.Title, b.Subtitle, b.Title))
	}
	var refine func(a, b int)
	refine = func(a, b int) {
		if b-a <= 1 || blocked || ctx.Err() != nil || same(results[a], results[b]) {
			return // same track on both ends: assume it plays throughout
		}
		m := (a + b) / 2
		query(m)
		refine(a, m)
		refine(m, b)
	}

	step := coarseFactor
	if o.thorough {
		step = 1
	}
	prev := -1
	for c := 0; need(c); c += step {
		query(c)
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
		query(last)
		refine(prev, last)
	}
	fmt.Fprintln(os.Stderr)
	logf("· shazam: %d requests, %d cached, %d silent skipped, %d rate limits, %d windows checked of %d",
		stats.requests, stats.cached, stats.silent, stats.limited, len(done), len(got))

	idx := make([]int, 0, len(results))
	for i := range results {
		idx = append(idx, i)
	}
	sort.Ints(idx) // chronological, so merging is deterministic
	var out []Candidate
	for _, i := range idx {
		if t := results[i]; t != nil {
			out = append(out, Candidate{Artist: t.Subtitle, Title: t.Title, TimeSec: got[i].offset, Source: "shazam", URL: t.URL})
		}
	}
	switch {
	case blocked:
		return out, errShazamBlocked
	case ctx.Err() != nil:
		return out, ctx.Err()
	}
	if err := <-decodeErr; err != nil {
		return out, fmt.Errorf("ffmpeg: %w", err)
	}
	return out, nil
}
