package main

// Streaming links, YouTube preferred. With a (free) YouTube Data API key every
// track gets a verified video link. Without one, a few plain YouTube searches
// are tried, stopping at the first "unusual traffic" page. Anything without a
// verified video gets a YouTube search link. Bandcamp and Spotify search URLs
// are built locally (their search pages sit behind bot checks, so they are not fetched).

import (
	"context"
	"encoding/json"
	"errors"
	"html"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"
)

type Links struct {
	YouTube       string `json:"youtube,omitempty"`
	YouTubeSearch string `json:"youtube_search,omitempty"`
	YouTubeTitle  string `json:"youtube_title,omitempty"`
	BandcampQuery string `json:"bandcamp_search,omitempty"`
	SpotifyQuery  string `json:"spotify_search,omitempty"`
	YTChecked     bool   `json:"yt_checked,omitempty"`
}

// Best is the single link shown in the table: YouTube video, else a YouTube search.
func (l *Links) Best() string {
	switch {
	case l == nil:
		return ""
	case l.YouTube != "":
		return l.YouTube
	case l.YouTubeSearch != "":
		return l.YouTubeSearch
	}
	return l.BandcampQuery
}

var errYTSorry = errors.New("youtube unusual-traffic page")

var (
	ytVideoRe       = regexp.MustCompile(`"videoRenderer":\{"videoId":"([\w-]{11})"`)
	ytTitleRe       = regexp.MustCompile(`"title":\{"runs":\[\{"text":"((?:[^"\\]|\\.)*)"`)
	ytOwnerRe       = regexp.MustCompile(`"ownerText":\{"runs":\[\{"text":"((?:[^"\\]|\\.)*)"`)
	bracketRe       = regexp.MustCompile(`[(\[][^)\]]*[)\]]`)
	mixedJunkRe     = regexp.MustCompile(`(?i)\s*\[mixed\]`)
	ytScrapeSpacing = 2 * time.Second
	ytAPISpacing    = 200 * time.Millisecond
)

type LinkResolver struct {
	mu       sync.Mutex
	client   *http.Client
	cache    map[string]*Links
	path     string
	last     time.Time
	apiKey   string
	ytBroken bool // stop asking YouTube after a refusal (CAPTCHA page, quota used up)
	queue    chan [2]string
	stop     chan struct{}
	wg       sync.WaitGroup
	ctx      context.Context
}

func linkKey(artist, title string) string {
	return strings.ToLower(strings.Join(strings.Fields(artist), " ") + "|" + strings.Join(strings.Fields(title), " "))
}

func NewLinkResolver(ctx context.Context, apiKey string) *LinkResolver {
	r := &LinkResolver{cache: map[string]*Links{}, apiKey: apiKey,
		queue: make(chan [2]string, 1024), stop: make(chan struct{}), ctx: ctx}
	r.client = &http.Client{Timeout: 20 * time.Second, CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if strings.Contains(req.URL.Path, "/sorry") || strings.HasPrefix(req.URL.Host, "consent.") {
			return errYTSorry // a bot check: never follow it
		}
		return nil
	}}
	if dir, err := os.UserCacheDir(); err == nil {
		r.path = filepath.Join(dir, "sctrackid", "links.json")
		if b, err := os.ReadFile(r.path); err == nil {
			_ = json.Unmarshal(b, &r.cache)
		}
	}
	r.wg.Add(1)
	go func() { // background prefetcher: fills the cache while Shazam is pacing
		defer r.wg.Done()
		for {
			select {
			case <-r.stop:
				return
			case q := <-r.queue:
				r.Lookup(q[0], q[1])
			}
		}
	}()
	return r
}

// Prefetch queues a lookup without blocking.
func (r *LinkResolver) Prefetch(artist, title string) {
	select {
	case r.queue <- [2]string{artist, title}:
	default:
	}
}

// StopPrefetch stops the background worker, dropping anything still queued.
func (r *LinkResolver) StopPrefetch() {
	close(r.stop)
	r.wg.Wait()
}

func (r *LinkResolver) Save() {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.path != "" {
		if b, err := json.Marshal(r.cache); err == nil {
			_ = os.MkdirAll(filepath.Dir(r.path), 0o755)
			_ = os.WriteFile(r.path, b, 0o644)
		}
	}
}

func (r *LinkResolver) Lookup(artist, title string) *Links {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := linkKey(artist, title)
	// Search URLs are always rebuilt locally; only the looked-up video is cached.
	query := strings.TrimSpace(mixedJunkRe.ReplaceAllString(artist+" "+title, ""))
	l := &Links{
		YouTubeSearch: "https://www.youtube.com/results?search_query=" + url.QueryEscape(query),
		BandcampQuery: "https://bandcamp.com/search?item_type=t&q=" + url.QueryEscape(query),
		SpotifyQuery:  "https://open.spotify.com/search/" + url.PathEscape(query),
	}
	if c, ok := r.cache[key]; ok && (c.YTChecked || r.ytBroken) {
		l.YouTube, l.YouTubeTitle, l.YTChecked = c.YouTube, c.YouTubeTitle, c.YTChecked
		r.cache[key] = l
		return l
	}
	if idWordRe.MatchString(strings.TrimSpace(bracketRe.ReplaceAllString(title, ""))) {
		l.YTChecked = true // unreleased "ID": nothing to look up
	} else if !r.ytBroken && r.ctx.Err() == nil {
		search, spacing := r.searchYouTube, ytScrapeSpacing
		if r.apiKey != "" {
			search, spacing = r.searchYouTubeAPI, ytAPISpacing
		}
		var id, vt string
		var status ytStatus
		for attempt := 0; attempt < 2; attempt++ { // one retry for network hiccups
			if d := time.Until(r.last.Add(spacing)); d > 0 {
				sleepCtx(r.ctx, d)
			}
			r.last = time.Now()
			if id, vt, status = search(query, artist, title); status != ytTransient || r.ctx.Err() != nil {
				break
			}
		}
		switch status {
		case ytRefused:
			r.ytBroken = true
			if r.apiKey != "" {
				logf("\r\033[K! YouTube API refused (quota used up or bad key); using YouTube search links for the rest")
			} else {
				logf("\r\033[K! YouTube showed its unusual-traffic check; stopped searching. Remaining tracks get YouTube search links.\n  For direct links on every track, set a free YouTube Data API key: -youtube-key or YOUTUBE_API_KEY")
			}
		case ytTransient:
			// leave YTChecked false so a rerun tries again
		default:
			l.YTChecked = true
			if id != "" {
				l.YouTube, l.YouTubeTitle = "https://youtu.be/"+id, vt
			}
		}
	}
	r.cache[key] = l
	return l
}

type ytStatus int

const (
	ytOK ytStatus = iota
	ytTransient
	ytRefused
)

// searchYouTube returns the best verified video ("" if none matched).
func (r *LinkResolver) searchYouTube(query, artist, title string) (id, videoTitle string, st ytStatus) {
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet,
		"https://www.youtube.com/results?search_query="+url.QueryEscape(query), nil)
	if err != nil {
		return "", "", ytTransient
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept-Language", "en-US,en;q=0.9")
	resp, err := r.client.Do(req)
	if errors.Is(err, errYTSorry) {
		return "", "", ytRefused
	}
	if err != nil {
		return "", "", ytTransient
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode == http.StatusForbidden {
		return "", "", ytRefused
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil || resp.StatusCode != http.StatusOK {
		return "", "", ytTransient
	}
	page := string(body)
	if !strings.Contains(page, "ytInitialData") {
		return "", "", ytRefused
	}

	locs := ytVideoRe.FindAllStringSubmatchIndex(page, 8)
	bestScore := 0.0
	for i, loc := range locs {
		end := len(page)
		if i+1 < len(locs) {
			end = locs[i+1][0]
		}
		chunk := page[loc[0]:min(end, loc[0]+20000)]
		vt, owner := jsonStr(ytTitleRe, chunk), jsonStr(ytOwnerRe, chunk)
		if s := videoScore(artist, title, vt, owner); s > bestScore {
			bestScore, id, videoTitle = s, page[loc[2]:loc[3]], vt
		}
	}
	return id, videoTitle, ytOK
}

// searchYouTubeAPI uses the YouTube Data API v3 (free quota: 10,000 units/day, 100 per search).
func (r *LinkResolver) searchYouTubeAPI(query, artist, title string) (id, videoTitle string, st ytStatus) {
	u := "https://www.googleapis.com/youtube/v3/search?part=snippet&type=video&maxResults=5&q=" +
		url.QueryEscape(query) + "&key=" + url.QueryEscape(r.apiKey)
	req, err := http.NewRequestWithContext(r.ctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", ytTransient
	}
	resp, err := r.client.Do(req)
	if err != nil {
		return "", "", ytTransient
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	switch {
	case resp.StatusCode == http.StatusForbidden || resp.StatusCode == http.StatusBadRequest || resp.StatusCode == http.StatusTooManyRequests:
		return "", "", ytRefused
	case err != nil || resp.StatusCode != http.StatusOK:
		return "", "", ytTransient
	}
	return pickAPIResult(body, artist, title)
}

func pickAPIResult(body []byte, artist, title string) (id, videoTitle string, st ytStatus) {
	var res struct {
		Items []struct {
			ID struct {
				VideoID string `json:"videoId"`
			} `json:"id"`
			Snippet struct {
				Title        string `json:"title"`
				ChannelTitle string `json:"channelTitle"`
			} `json:"snippet"`
		} `json:"items"`
	}
	if err := json.Unmarshal(body, &res); err != nil {
		return "", "", ytTransient
	}
	best := 0.0
	for _, it := range res.Items {
		vt := html.UnescapeString(it.Snippet.Title) // the API HTML-escapes titles
		if s := videoScore(artist, title, vt, it.Snippet.ChannelTitle); s > best && it.ID.VideoID != "" {
			best, id, videoTitle = s, it.ID.VideoID, vt
		}
	}
	return id, videoTitle, ytOK
}

func jsonStr(re *regexp.Regexp, s string) string {
	m := re.FindStringSubmatch(s)
	if m == nil {
		return ""
	}
	var out string
	if json.Unmarshal([]byte(`"`+m[1]+`"`), &out) != nil {
		return m[1]
	}
	return out
}

// videoScore is 0 when the video doesn't look like the track; higher is better.
func videoScore(artist, title, videoTitle, owner string) float64 {
	hay := tokens(videoTitle + " " + strings.TrimSuffix(owner, " - Topic"))
	core := tokens(bracketRe.ReplaceAllString(title, " "))
	if len(core) == 0 {
		return 0
	}
	hit := 0
	for t := range core {
		if hay[t] {
			hit++
		}
	}
	coreRatio := float64(hit) / float64(len(core))
	if coreRatio < 0.6 {
		return 0
	}
	artistHit := false
	for t := range tokens(artist) {
		if hay[t] {
			artistHit = true
			break
		}
	}
	if !artistHit {
		return 0
	}
	score := coreRatio
	// Prefer the right version: reward matching remix/edit words from the brackets.
	extra := tokens(strings.Join(bracketRe.FindAllString(title, -1), " "))
	vt := tokens(videoTitle)
	for t := range extra {
		if vt[t] {
			score += 0.2
		}
	}
	// Reject other versions unless the track itself is one.
	low, want := strings.ToLower(videoTitle), strings.ToLower(title)
	for _, alt := range []string{"mashup", "bootleg", "cover", "sped up", "slowed", "nightcore", "karaoke", "instrumental"} {
		if strings.Contains(low, alt) && !strings.Contains(want, alt) {
			return 0
		}
	}
	// Reject obvious non-track uploads (whole sets, tutorials...).
	for _, bad := range []string{"full set", "dj set", "boiler room", "live set", "mix 20", "megamix", "tutorial", "reaction"} {
		if strings.Contains(low, bad) {
			return 0
		}
	}
	return score
}
