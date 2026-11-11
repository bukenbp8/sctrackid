package main

// 1001tracklists lookup. Only the public search endpoint is used (at most two
// requests per run). Tracklist pages themselves sit behind a Cloudflare bot
// check, so they are not fetched; the matched link is reported instead.

import (
	"context"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const tl1001Base = "https://www.1001tracklists.com"

type TL1001Result struct {
	URL      string  `json:"url"`
	Title    string  `json:"title"`
	Date     string  `json:"date,omitempty"`
	IDed     string  `json:"ided,omitempty"` // "identified/total"
	PlayTime string  `json:"play_time,omitempty"`
	Score    float64 `json:"score"`
}

var (
	tlItemSplit = regexp.MustCompile(`class="bItm action oItm"`)
	tlHrefRe    = regexp.MustCompile(`href="(/tracklist/[^"]+\.html)"`)
	tlTitleRe   = regexp.MustCompile(`class="bTitle"[^>]*>\s*<a [^>]*>([^<]+)</a>`)
	tlIDedRe    = regexp.MustCompile(`title="IDed tracks / total tracks">\s*<i[^>]*></i>\s*(\d+\s*/\s*\d+)`)
	tlTimeRe    = regexp.MustCompile(`title="play time">\s*<i[^>]*></i>\s*([^<]+?)\s*<`)
	tlDateRe    = regexp.MustCompile(`title="tracklist date">\s*<i[^>]*></i>\s*([^<]+?)\s*<`)
	tlDurRe     = regexp.MustCompile(`(?:(\d+)\s*h)?\s*(?:(\d+)\s*m)?`)
	genericWord = map[string]bool{"dj": true, "set": true, "live": true, "mix": true, "at": true, "free": true,
		"download": true, "full": true, "recorded": true, "podcast": true, "episode": true, "ep": true}
)

// TL1001 runs the lookup at most once, in the background, whenever Start is first called.
type TL1001 struct {
	once   sync.Once
	done   chan struct{}
	track  *Track
	durSec int
	client *http.Client

	Result    *TL1001Result
	Err       error
	StartedBy string // "shazam pause" or "end of run"
}

func NewTL1001(track *Track, durSec int) *TL1001 {
	return &TL1001{done: make(chan struct{}), track: track, durSec: durSec,
		client: &http.Client{Timeout: 20 * time.Second}}
}

func (t *TL1001) Start(ctx context.Context, by string) {
	t.once.Do(func() {
		t.StartedBy = by
		go func() {
			defer close(t.done)
			t.Result, t.Err = t.find(ctx)
		}()
	})
}

// Wait starts the lookup if nothing triggered it yet, then waits for it.
func (t *TL1001) Wait(ctx context.Context) {
	t.Start(ctx, "end of run")
	select {
	case <-t.done:
	case <-ctx.Done():
	}
}

func setTokens(title string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range strings.Fields(nonAlnumRe.ReplaceAllString(strings.ToLower(title), " ")) {
		if !genericWord[f] && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	return out
}

func (t *TL1001) find(ctx context.Context) (*TL1001Result, error) {
	toks := setTokens(t.track.Title)
	if len(toks) == 0 {
		return nil, errors.New("track title has no searchable words")
	}
	queries := []string{strings.Join(toks[:min(len(toks), 8)], " ")}
	if len(toks) > 4 {
		queries = append(queries, strings.Join(toks[:4], " ")) // broader fallback
	}
	var best *TL1001Result
	for i, q := range queries {
		if i > 0 {
			sleepCtx(ctx, 3*time.Second) // be gentle
		}
		results, err := t.search(ctx, q)
		if err != nil {
			return nil, err
		}
		for _, r := range results {
			r.Score = t.score(toks, r)
			if best == nil || r.Score > best.Score {
				rr := r
				best = &rr
			}
		}
		if best != nil && best.Score >= 0.75 {
			return best, nil
		}
	}
	return nil, nil // nothing convincing
}

func (t *TL1001) search(ctx context.Context, q string) ([]TL1001Result, error) {
	form := url.Values{"main_search": {q}, "search_selection": {"9"}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tl1001Base+"/search/result.php", strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Referer", tl1001Base+"/")
	resp, err := t.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK || strings.Contains(string(body), "challenges.cloudflare.com") {
		return nil, errors.New("1001tracklists search is unavailable right now (blocked or bot check); not retrying")
	}
	var out []TL1001Result
	for _, chunk := range tlItemSplit.Split(string(body), -1)[1:] {
		href := tlHrefRe.FindStringSubmatch(chunk)
		title := tlTitleRe.FindStringSubmatch(chunk)
		if href == nil || title == nil {
			continue
		}
		r := TL1001Result{URL: tl1001Base + href[1], Title: html.UnescapeString(strings.TrimSpace(title[1]))}
		if m := tlIDedRe.FindStringSubmatch(chunk); m != nil {
			r.IDed = strings.ReplaceAll(m[1], " ", "")
		}
		if m := tlTimeRe.FindStringSubmatch(chunk); m != nil {
			r.PlayTime = m[1]
		}
		if m := tlDateRe.FindStringSubmatch(chunk); m != nil {
			r.Date = m[1]
		}
		out = append(out, r)
	}
	return out, nil
}

// score: share of the SoundCloud title's words found in the result title,
// adjusted by whether the play time matches the set length.
func (t *TL1001) score(scToks []string, r TL1001Result) float64 {
	rt := map[string]bool{}
	for _, w := range setTokens(r.Title + " " + r.Date) {
		rt[w] = true
	}
	hit := 0
	for _, w := range scToks {
		if rt[w] {
			hit++
		}
	}
	s := float64(hit) / float64(len(scToks))
	if m := tlDurRe.FindStringSubmatch(r.PlayTime); m != nil && (m[1] != "" || m[2] != "") && t.durSec > 0 {
		h, _ := strconv.Atoi(m[1])
		mi, _ := strconv.Atoi(m[2])
		diff := float64(abs(h*3600+mi*60-t.durSec)) / float64(t.durSec)
		switch {
		case diff <= 0.08:
			s += 0.3
		case diff > 0.25:
			s -= 0.3
		}
	}
	return s
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}
