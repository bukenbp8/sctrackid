package main

// Retrying one result: when a listed track is wrong, the audio around its
// timestamp is fingerprinted again at shifted offsets and the entry is
// replaced by whatever else Shazam hears there. If what it hears is mostly a
// neighbouring track that is already listed, the wrong entry is dropped.

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http"
	"os/exec"
	"strconv"
	"time"
)

// Window starts relative to the entry's timestamp. They are staggered against
// the regular sampling grid so Shazam gets audio it has not judged before.
var retryShifts = []int{6, -9, 19, 33, 47, 62}

// retryEntry returns either a replacement for old, or (absorbed) the entry of
// others that turned out to be playing there, in which case old should go.
// Either way the result carries old's name in Rejected.
func retryEntry(ctx context.Context, link string, old *Entry, others []*Entry, o options) (repl *Entry, absorbed *Entry, rejected []TrackName, err error) {
	if old.TimeSec < 0 {
		return nil, nil, nil, errors.New("this track has no timestamp, so there is no audio position to listen at again")
	}
	if _, err := exec.LookPath("ffmpeg"); err != nil {
		return nil, nil, nil, errors.New("ffmpeg not found in PATH")
	}
	input := o.file
	dur := 0
	if input == "" {
		sc, track, err := resolveTrack(ctx, link, o)
		if err != nil {
			return nil, nil, nil, err
		}
		dur = int(max(track.FullDuration, track.Duration) / 1000)
		if input, _, err = sc.StreamURL(ctx, track); err != nil {
			return nil, nil, nil, err
		}
	}

	from := max(0, old.TimeSec+retryShifts[1])
	length := retryShifts[len(retryShifts)-1] + o.sampleLen + old.TimeSec - from
	logf("· retry: listening again around %s (rejected: %s - %s)", fmtTime(old.TimeSec), old.Artist, old.Title)
	cmd := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-loglevel", "error",
		"-ss", strconv.Itoa(from), "-t", strconv.Itoa(length), "-i", input,
		"-vn", "-f", "s16le", "-ac", "1", "-ar", "16000", "-")
	cmd.Stderr = logOut
	pcm, err := cmd.Output()
	if len(pcm) < 4*sampleRate*2 {
		if err == nil {
			err = errors.New("no audio at that position")
		}
		return nil, nil, nil, fmt.Errorf("ffmpeg: %w", err)
	}

	rejected = append(append([]TrackName(nil), old.Rejected...), TrackName{old.Artist, old.Title})
	type vote struct {
		t     *ShazamTrack
		n     int
		first int
	}
	var votes []*vote
	var best *vote
	heardSame, limited := 0, 0
	listed := map[*Entry]int{} // windows in which a track already in the list was heard
	var neighbour *Entry
	client := &http.Client{Timeout: 20 * time.Second}

windows:
	for _, shift := range retryShifts {
		at := old.TimeSec + shift
		if at < from || (dur > 0 && at+4 > dur) {
			continue
		}
		lo := (at - from) * sampleRate * 2
		hi := min(len(pcm), lo+o.sampleLen*sampleRate*2)
		if hi-lo < 4*sampleRate*2 {
			continue
		}
		samples := make([]int16, (hi-lo)/2)
		for i := range samples {
			samples[i] = int16(binary.LittleEndian.Uint16(pcm[lo+i*2:]))
		}
		sig, peaks := MakeSignature(samples)
		if peaks < silentPeaks {
			logf("  %8s  — (near-silent, not sent)", fmtTime(at))
			continue
		}
		var t *ShazamTrack
		for {
			sleepCtx(ctx, o.delay)
			if ctx.Err() != nil {
				return nil, nil, nil, ctx.Err()
			}
			t, err = Recognize(ctx, client, sig, len(samples))
			var rl rateLimitedError
			if !errors.As(err, &rl) {
				break
			}
			if limited++; limited > 2 {
				logf("! shazam keeps rate limiting; stopping the retry early")
				break windows
			}
			cool := rl.RetryAfter
			if cool == 0 {
				cool = 15 * time.Second
			}
			logf("! rate limited, cooling down %s…", cool)
			sleepCtx(ctx, cool)
		}
		switch {
		case err != nil:
			logf("  %8s  error: %v", fmtTime(at), err)
			continue
		case t == nil:
			logf("  %8s  —", fmtTime(at))
			continue
		}
		logf("  %8s  %s - %s", fmtTime(at), t.Subtitle, t.Title)
		for _, r := range rejected {
			if sameTrack(r.Artist, r.Title, t.Subtitle, t.Title) {
				heardSame++
				continue windows
			}
		}
		for _, e := range others {
			if sameTrack(e.Artist, e.Title, t.Subtitle, t.Title) {
				if listed[e]++; neighbour == nil || listed[e] > listed[neighbour] {
					neighbour = e
				}
				continue windows
			}
		}
		var v *vote
		for _, x := range votes {
			if x.t.Key == t.Key || sameTrack(x.t.Subtitle, x.t.Title, t.Subtitle, t.Title) {
				v = x
			}
		}
		if v == nil {
			v = &vote{t: t, first: len(votes)}
			votes = append(votes, v)
		}
		v.n++
		if best == nil || v.n > best.n {
			best = v
		}
		if best.n >= 2 { // heard twice: good enough, spare Shazam the rest
			break
		}
	}

	// A listed neighbour heard at least twice, and more often than anything
	// new, is what is really playing: the wrong entry was a stray hit.
	if neighbour != nil && listed[neighbour] >= 2 && (best == nil || listed[neighbour] > best.n) {
		logf("· retry: %s - %s is what plays there and is already listed; dropped the wrong entry", neighbour.Artist, neighbour.Title)
		return nil, neighbour, rejected, nil
	}
	if best == nil {
		switch {
		case limited > 2:
			return nil, nil, nil, errShazamBlocked
		case heardSame > 0:
			return nil, nil, nil, fmt.Errorf("no other match around %s — Shazam still hears %s - %s", fmtTime(old.TimeSec), old.Artist, old.Title)
		case neighbour != nil:
			return nil, nil, nil, fmt.Errorf("no other match around %s — only a glimpse of %s - %s, which is already in the list", fmtTime(old.TimeSec), neighbour.Artist, neighbour.Title)
		}
		return nil, nil, nil, fmt.Errorf("no other match around %s — Shazam recognises nothing there", fmtTime(old.TimeSec))
	}

	e := &Entry{Artist: best.t.Subtitle, Title: best.t.Title, TimeSec: old.TimeSec,
		Sources: map[string]int{"shazam": best.n}, ShazamURL: best.t.URL, Rejected: rejected}
	e.Confidence = confidence(e.Sources)
	if !o.noLinks {
		lr := NewLinkResolver(ctx, o.ytKey)
		lr.StopPrefetch()
		e.Links = lr.Lookup(e.Artist, e.Title)
		lr.Save()
	}
	logf("· retry: replaced with %s - %s", e.Artist, e.Title)
	return e, nil, rejected, nil
}
