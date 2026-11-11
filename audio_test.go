package main

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// A cache-only scan: 9 windows 30s apart with in-between windows.
func cachedScan(t *testing.T, hits map[int]*ShazamTrack) ([]Candidate, error) {
	t.Helper()
	recognize = func(context.Context, *http.Client, []byte, int) (*ShazamTrack, error) {
		t.Fatal("Shazam was asked although the scan should run from the cache")
		return nil, nil
	}
	defer func() { recognize = Recognize }()
	m := &cacheMeta{Interval: 30, N: 9, Silent: []int{}}
	for i := 0; i < 8; i++ {
		m.Alts = append(m.Alts, 30+i*30)
	}
	c := &shazamCache{hits: hits, meta: m}
	out, _, err := scanWindows(context.Background(), m.windows(), c, 270, options{interval: 30, sampleLen: 12, thorough: true},
		func() {}, func(string, string) {})
	return out, err
}

func TestVerifyLoneMatches(t *testing.T) {
	a := &ShazamTrack{Key: "a", Subtitle: "Artist A", Title: "Long One"}
	stray := &ShazamTrack{Key: "s", Subtitle: "Nobody", Title: "Stray Hit"}
	short := &ShazamTrack{Key: "b", Subtitle: "Artist B", Title: "Quick Mix"}
	hits := map[int]*ShazamTrack{}
	for i := 0; i < 9; i++ { // regular windows at 15, 45, …, 255
		hits[15+i*30] = a
	}
	hits[105] = stray // a single odd window, with A before and after
	hits[90], hits[120] = a, a
	hits[225] = short               // heard once on the grid …
	hits[210], hits[240] = a, short // … and again in between
	for _, off := range []int{30, 60, 150, 180, 270} {
		hits[off] = nil
	}

	out, err := cachedScan(t, hits)
	if err != nil {
		t.Fatal(err)
	}
	es := Merge(out)
	got := map[string]*Entry{}
	for _, e := range es {
		got[e.Title] = e
	}
	if e := got["Stray Hit"]; e == nil || e.Confidence != "low" || !e.Unverified {
		t.Errorf("unconfirmed single hit: %+v", e)
	}
	if e := got["Quick Mix"]; e == nil || e.Confidence != "high" || e.Unverified || e.Sources["shazam"] != 2 {
		t.Errorf("confirmed single hit: %+v", e)
	}
	if e := got["Long One"]; e == nil || e.Confidence != "high" || e.TimeSec != 15 {
		t.Errorf("long track: %+v", e)
	}

	// an answer that is not in the cache means the audio has to be fetched
	delete(hits, 240)
	if _, err := cachedScan(t, hits); !errors.Is(err, errNeedAudio) {
		t.Errorf("missing sample: err = %v", err)
	}
}
