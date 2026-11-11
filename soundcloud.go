package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"
)

const browserUA = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/130.0.0.0 Safari/537.36"

type SoundCloud struct {
	HTTP     *http.Client
	ClientID string
}

type Transcoding struct {
	URL     string `json:"url"`
	Preset  string `json:"preset"`
	Snipped bool   `json:"snipped"`
	Format  struct {
		Protocol string `json:"protocol"`
		MimeType string `json:"mime_type"`
	} `json:"format"`
}

type Track struct {
	ID                 int64  `json:"id"`
	Kind               string `json:"kind"`
	Title              string `json:"title"`
	Description        string `json:"description"`
	Duration           int64  `json:"duration"`      // ms (may be a preview length)
	FullDuration       int64  `json:"full_duration"` // ms
	PermalinkURL       string `json:"permalink_url"`
	CommentCount       int    `json:"comment_count"`
	Policy             string `json:"policy"`
	TrackAuthorization string `json:"track_authorization"`
	User               struct {
		Username string `json:"username"`
	} `json:"user"`
	Media struct {
		Transcodings []Transcoding `json:"transcodings"`
	} `json:"media"`
}

type Comment struct {
	ID        int64  `json:"id"`
	Body      string `json:"body"`
	Timestamp *int64 `json:"timestamp"` // ms into the track, nil if not anchored
	User      struct {
		Username string `json:"username"`
	} `json:"user"`
}

var errUnauthorized = errors.New("unauthorized")

// get retries transient failures (network errors, 5xx) up to 3 attempts.
func (s *SoundCloud) get(ctx context.Context, u string) ([]byte, error) {
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			sleepCtx(ctx, time.Duration(2<<attempt)*time.Second)
		}
		body, retry, err := s.getOnce(ctx, u)
		if err == nil || !retry || ctx.Err() != nil {
			return body, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (s *SoundCloud) getOnce(ctx context.Context, u string) ([]byte, bool, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("User-Agent", browserUA)
	req.Header.Set("Accept", "application/json, text/html;q=0.9, */*;q=0.8")
	resp, err := s.HTTP.Do(req)
	if err != nil {
		return nil, true, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, true, err
	}
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		return nil, false, fmt.Errorf("GET %s: HTTP %d: %w", redactClientID(u), resp.StatusCode, errUnauthorized)
	case resp.StatusCode != http.StatusOK:
		return nil, resp.StatusCode >= 500, fmt.Errorf("GET %s: HTTP %d", redactClientID(u), resp.StatusCode)
	}
	return body, false, nil
}

func redactClientID(u string) string {
	u = regexp.MustCompile(`client_id=[^&]+`).ReplaceAllString(u, "client_id=…")
	return regexp.MustCompile(`track_authorization=[^&]+`).ReplaceAllString(u, "track_authorization=…")
}

var (
	scriptSrcRe = regexp.MustCompile(`<script[^>]+src="(https://a-v2\.sndcdn\.com/assets/[^"]+\.js)"`)
	clientIDRe  = regexp.MustCompile(`client_id\s*[:=]\s*"?([a-zA-Z0-9]{32})\b`)
)

// FetchClientID scrapes the public web-player client_id from SoundCloud's JS bundles.
func (s *SoundCloud) FetchClientID(ctx context.Context) error {
	html, err := s.get(ctx, "https://soundcloud.com/")
	if err != nil {
		return err
	}
	scripts := scriptSrcRe.FindAllStringSubmatch(string(html), -1)
	// The client_id normally lives in one of the last bundles.
	for i := len(scripts) - 1; i >= 0; i-- {
		js, err := s.get(ctx, scripts[i][1])
		if err != nil {
			continue
		}
		if m := clientIDRe.FindSubmatch(js); m != nil {
			s.ClientID = string(m[1])
			saveCachedClientID(s.ClientID)
			return nil
		}
	}
	return errors.New("could not find a SoundCloud client_id (pass one with -client-id)")
}

func clientIDCachePath() string {
	dir, err := os.UserCacheDir()
	if err != nil {
		return ""
	}
	return filepath.Join(dir, "sctrackid", "client_id")
}

func loadCachedClientID() string {
	if p := clientIDCachePath(); p != "" {
		if b, err := os.ReadFile(p); err == nil {
			return strings.TrimSpace(string(b))
		}
	}
	return ""
}

func saveCachedClientID(id string) {
	if p := clientIDCachePath(); p != "" {
		_ = os.MkdirAll(filepath.Dir(p), 0o755)
		_ = os.WriteFile(p, []byte(id+"\n"), 0o644)
	}
}

func (s *SoundCloud) withClientID(u string) string {
	sep := "?"
	if strings.Contains(u, "?") {
		sep = "&"
	}
	return u + sep + "client_id=" + url.QueryEscape(s.ClientID)
}

// canonicalURL follows short links (on.soundcloud.com) and strips tracking params.
func (s *SoundCloud) canonicalURL(ctx context.Context, raw string) (string, error) {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return "", err
	}
	if u.Host == "on.soundcloud.com" {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
		req.Header.Set("User-Agent", browserUA)
		resp, err := s.HTTP.Do(req)
		if err != nil {
			return "", err
		}
		resp.Body.Close()
		u = resp.Request.URL
	}
	u.Host = strings.TrimPrefix(strings.TrimPrefix(u.Host, "m."), "www.")
	u.Scheme = "https"
	u.RawQuery = ""
	u.Fragment = ""
	return u.String(), nil
}

func (s *SoundCloud) Resolve(ctx context.Context, trackURL string) (*Track, error) {
	canon, err := s.canonicalURL(ctx, trackURL)
	if err != nil {
		return nil, err
	}
	body, err := s.get(ctx, s.withClientID("https://api-v2.soundcloud.com/resolve?url="+url.QueryEscape(canon)))
	if err != nil {
		return nil, err
	}
	var t Track
	if err := json.Unmarshal(body, &t); err != nil {
		return nil, err
	}
	if t.Kind != "track" {
		return nil, fmt.Errorf("URL resolves to a %q, expected a single track", t.Kind)
	}
	return &t, nil
}

func (s *SoundCloud) Comments(ctx context.Context, trackID int64) ([]Comment, error) {
	var all []Comment
	next := fmt.Sprintf("https://api-v2.soundcloud.com/tracks/%d/comments?threaded=0&filter_replies=0&limit=200&offset=0", trackID)
	for next != "" {
		body, err := s.get(ctx, s.withClientID(next))
		if err != nil {
			return all, err
		}
		var page struct {
			Collection []Comment `json:"collection"`
			NextHref   *string   `json:"next_href"`
		}
		if err := json.Unmarshal(body, &page); err != nil {
			return all, err
		}
		all = append(all, page.Collection...)
		next = ""
		if page.NextHref != nil && len(page.Collection) > 0 {
			next = *page.NextHref
		}
	}
	return all, nil
}

// StreamURL returns a URL ffmpeg can read (progressive MP3 or an HLS playlist).
func (s *SoundCloud) StreamURL(ctx context.Context, t *Track) (string, bool, error) {
	rank := func(tc Transcoding) int {
		p, m := tc.Format.Protocol, tc.Format.MimeType
		switch {
		case strings.Contains(p, "encrypted"):
			return -1
		case p == "progressive":
			return 5
		case p == "hls" && strings.HasPrefix(m, "audio/mpeg") && !strings.Contains(m, "url"):
			return 4
		case p == "hls" && strings.Contains(m, "mp4a"):
			return 3
		case p == "hls" && strings.Contains(m, "ogg"):
			return 2
		case p == "hls":
			return 1 // e.g. adaptive "abr" playlists; often not directly fetchable
		}
		return -1
	}
	var cands []Transcoding
	for _, tc := range t.Media.Transcodings {
		if rank(tc) >= 0 {
			cands = append(cands, tc)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return rank(cands[i]) > rank(cands[j]) })
	if len(cands) == 0 {
		return "", false, errors.New("no unencrypted stream available for this track")
	}
	var lastErr error
	for _, tc := range cands {
		u := s.withClientID(tc.URL)
		if t.TrackAuthorization != "" {
			u += "&track_authorization=" + url.QueryEscape(t.TrackAuthorization)
		}
		body, err := s.get(ctx, u)
		if err != nil {
			lastErr = err
			continue
		}
		var out struct {
			URL string `json:"url"`
		}
		if err := json.Unmarshal(body, &out); err != nil || out.URL == "" {
			lastErr = fmt.Errorf("bad stream response for %s", tc.Preset)
			continue
		}
		return out.URL, tc.Snipped, nil
	}
	return "", false, lastErr
}
