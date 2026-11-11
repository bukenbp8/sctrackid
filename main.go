package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"
)

type options struct {
	clientID   string
	file       string
	interval   int
	sampleLen  int
	delay      time.Duration
	noAudio    bool
	noComments bool
	no1001     bool
	jsonOut    bool
	minConf    string
	verbose    bool
	thorough   bool
	noCache    bool
	noLinks    bool
	ytKey      string

	onResolved func(t *Track, dur int) // web mode: called once the SoundCloud track is known
}

func main() {
	var o options
	flag.StringVar(&o.clientID, "client-id", os.Getenv("SC_CLIENT_ID"), "SoundCloud client_id (auto-scraped if empty; env SC_CLIENT_ID)")
	flag.StringVar(&o.file, "file", "", "use a local audio file for fingerprinting instead of streaming from SoundCloud")
	flag.IntVar(&o.interval, "interval", 30, "seconds between audio samples")
	flag.IntVar(&o.sampleLen, "sample", 12, "length of each audio sample in seconds (Shazam works best with ~10-12)")
	flag.DurationVar(&o.delay, "delay", 2*time.Second, "minimum time between Shazam requests (grows automatically when rate limited)")
	flag.BoolVar(&o.thorough, "thorough", false, "sample every -interval instead of skipping stretches where the same track is playing (slower, catches more overlays)")
	flag.BoolVar(&o.noCache, "no-cache", false, "ignore cached Shazam results from earlier runs")
	flag.BoolVar(&o.noLinks, "no-links", false, "don't look up streaming links")
	flag.StringVar(&o.ytKey, "youtube-key", os.Getenv("YOUTUBE_API_KEY"), "free YouTube Data API key for direct video links on every track (env YOUTUBE_API_KEY)")
	flag.BoolVar(&o.noAudio, "no-audio", false, "skip audio fingerprinting (comments + description only)")
	flag.BoolVar(&o.noComments, "no-comments", false, "skip comment scraping")
	flag.BoolVar(&o.no1001, "no-1001", false, "skip the 1001tracklists search")
	flag.BoolVar(&o.jsonOut, "json", false, "print JSON instead of a table")
	flag.StringVar(&o.minConf, "min-confidence", "low", "hide results below this confidence: low|medium|high")
	flag.BoolVar(&o.verbose, "v", false, "verbose: log every Shazam sample")
	serve := flag.String("serve", "", "run the web interface on this address (e.g. localhost:8080) instead of a one-off lookup")
	var gc guardConfig
	flag.IntVar(&gc.powBits, "pow-bits", 18, "web: proof-of-work difficulty per submission in leading zero bits (each +1 doubles the work; 0 disables)")
	flag.IntVar(&gc.maxActivePerIP, "max-active", 2, "web: max queued+running jobs per client (0 = unlimited)")
	flag.IntVar(&gc.maxPerHourPerIP, "max-per-hour", 10, "web: max job submissions per client per hour (0 = unlimited)")
	flag.IntVar(&gc.maxQueue, "max-queue", 30, "web: max jobs waiting in the queue overall (0 = unlimited)")
	flag.IntVar(&gc.maxStreamsPerIP, "max-streams", 8, "web: max open live-update connections per client (0 = unlimited)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, "usage: %s [flags] <soundcloud-url>\n       %s -serve localhost:8080\n\nflags:\n", os.Args[0], os.Args[0])
		flag.PrintDefaults()
	}
	flag.Parse()
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if *serve != "" {
		if err := serveWeb(ctx, *serve, o, gc); err != nil {
			fmt.Fprintln(os.Stderr, "error:", err)
			os.Exit(1)
		}
		return
	}
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	if err := run(ctx, flag.Arg(0), o); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// logOut receives all progress output. The web server points it at the running
// job (only one job runs at a time, so a single global target is enough).
var logOut = &switchWriter{w: os.Stderr}

type switchWriter struct {
	mu sync.Mutex
	w  io.Writer
}

func (s *switchWriter) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.w.Write(p)
}

func (s *switchWriter) Set(w io.Writer) {
	s.mu.Lock()
	s.w = w
	s.mu.Unlock()
}

func logf(format string, a ...any) { fmt.Fprintf(logOut, format+"\n", a...) }

// Report is the outcome of analyzing one SoundCloud set.
type Report struct {
	Track      *Track
	Dur        int
	Entries    []*Entry
	Unresolved map[int]int
	TL         *TL1001Result
}

func run(ctx context.Context, link string, o options) error {
	rep, err := analyze(ctx, link, o)
	if err != nil {
		return err
	}
	if path, err := writeTextFile(rep); err != nil {
		logf("! could not write text file: %v", err)
	} else {
		logf("· saved tracklist to %s", path)
	}
	if o.jsonOut {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(rep.JSON())
	}
	printTable(os.Stdout, rep.Entries, rep.Unresolved, rep.TL)
	return nil
}

func analyze(ctx context.Context, link string, o options) (*Report, error) {
	sc := &SoundCloud{HTTP: &http.Client{Timeout: 30 * time.Second}, ClientID: o.clientID}
	cached := false
	if sc.ClientID == "" {
		sc.ClientID, cached = loadCachedClientID(), true
	}
	if sc.ClientID == "" {
		cached = false
		logf("· fetching SoundCloud client_id…")
		if err := sc.FetchClientID(ctx); err != nil {
			return nil, err
		}
	}
	track, err := sc.Resolve(ctx, link)
	if errors.Is(err, errUnauthorized) && cached { // cached client_id went stale
		logf("· cached client_id expired, fetching a new one…")
		if err := sc.FetchClientID(ctx); err != nil {
			return nil, err
		}
		track, err = sc.Resolve(ctx, link)
	}
	if err != nil {
		return nil, err
	}
	dur := int(max(track.FullDuration, track.Duration) / 1000)
	logf("· %s — %s [%s, %d comments]", track.User.Username, track.Title, fmtTime(dur), track.CommentCount)
	if o.onResolved != nil {
		o.onResolved(track, dur)
	}

	// Streaming links are looked up in the background while Shazam paces itself.
	var links *LinkResolver
	prefetch := func(artist, title string) {}
	if !o.noLinks {
		links = NewLinkResolver(ctx, o.ytKey)
		prefetch = links.Prefetch
	}

	var cands []Candidate
	desc := ParseDescription(track.Description)
	for _, c := range desc {
		prefetch(c.Artist, c.Title)
	}
	if len(desc) > 0 {
		logf("· description: found a %d-line tracklist", len(desc))
	}
	cands = append(cands, desc...)

	var asks []int
	if !o.noComments {
		comments, err := sc.Comments(ctx, track.ID)
		if err != nil {
			logf("! comments: %v (continuing with %d)", err, len(comments))
		}
		cc, a := ParseComments(comments)
		asks = a
		logf("· comments: %d fetched, %d track mentions, %d ID requests", len(comments), len(cc), len(a))
		cands = append(cands, cc...)
	}

	// 1001tracklists: kicked off during the first Shazam rate-limit pause,
	// or after fingerprinting if Shazam never paused.
	var tl *TL1001
	onPause := func() {}
	if !o.no1001 {
		tl = NewTL1001(track, dur)
		onPause = func() { tl.Start(ctx, "shazam pause") }
	}

	if !o.noAudio {
		ac, err := audioCandidates(ctx, sc, track, dur, o, onPause, prefetch)
		if err != nil {
			logf("! audio: %v", err)
		}
		cands = append(cands, ac...)
	}

	var tlRes *TL1001Result
	if tl != nil {
		tl.Wait(ctx)
		if tl.Err != nil {
			logf("! 1001tracklists: %v", tl.Err)
		} else if o.verbose {
			logf("· 1001tracklists lookup ran during: %s", tl.StartedBy)
		}
		tlRes = tl.Result
	}

	entries := filterConfidence(Merge(cands), o.minConf)
	if links != nil {
		links.StopPrefetch()
		logf("· looking up streaming links for %d tracks…", len(entries))
		found := 0
		for _, e := range entries {
			if e.Links = links.Lookup(e.Artist, e.Title); e.Links.YouTube != "" {
				found++
			}
		}
		links.Save()
		logf("· links: %d direct YouTube videos, %d YouTube search links", found, len(entries)-found)
	}
	return &Report{track, dur, entries, unresolvedAsks(asks, entries), tlRes}, nil
}

func sleepCtx(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

func fmtTime(sec int) string {
	if sec < 0 {
		return "?"
	}
	if sec >= 3600 {
		return fmt.Sprintf("%d:%02d:%02d", sec/3600, sec/60%60, sec%60)
	}
	return fmt.Sprintf("%d:%02d", sec/60, sec%60)
}

func filterConfidence(es []*Entry, minConf string) []*Entry {
	rank := map[string]int{"low": 0, "medium": 1, "high": 2}
	var out []*Entry
	for _, e := range es {
		if rank[e.Confidence] >= rank[strings.ToLower(minConf)] {
			out = append(out, e)
		}
	}
	return out
}

// unresolvedAsks clusters "ID?" comments and keeps those with no identified track nearby.
func unresolvedAsks(asks []int, entries []*Entry) map[int]int {
	sort.Ints(asks)
	out := map[int]int{}
	clusterStart := -1000
	for _, t := range asks {
		covered := false
		for _, e := range entries {
			if e.TimeSec >= 0 && e.TimeSec <= t+30 && t-e.TimeSec <= 240 {
				covered = true
				break
			}
		}
		if covered {
			continue
		}
		if t-clusterStart > 60 {
			clusterStart = t
		}
		out[clusterStart]++
	}
	return out
}

func sourceSummary(src map[string]int) string {
	var parts []string
	for _, k := range []string{"description", "shazam", "comment"} {
		if n := src[k]; n > 0 {
			name := map[string]string{"description": "desc", "shazam": "shazam", "comment": "comments"}[k]
			if n > 1 {
				parts = append(parts, fmt.Sprintf("%s×%d", name, n))
			} else {
				parts = append(parts, name)
			}
		}
	}
	return strings.Join(parts, ", ")
}

func printTable(w io.Writer, entries []*Entry, unresolved map[int]int, tl *TL1001Result) {
	fmt.Fprintln(w)
	if len(entries) == 0 {
		fmt.Fprintln(w, "No tracks identified.")
	} else {
		tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "#\tTIME\tTRACK\tCONF\tSOURCES\tLINK")
		for i, e := range entries {
			fmt.Fprintf(tw, "%d\t%s\t%s - %s\t%s\t%s\t%s\n", i+1, fmtTime(e.TimeSec), e.Artist, e.Title, e.Confidence, sourceSummary(e.Sources), e.Links.Best())
		}
		tw.Flush()
	}
	if len(unresolved) > 0 {
		var ts []int
		for t := range unresolved {
			ts = append(ts, t)
		}
		sort.Ints(ts)
		var parts []string
		for _, t := range ts {
			parts = append(parts, fmt.Sprintf("%s (×%d)", fmtTime(t), unresolved[t]))
		}
		fmt.Fprintf(w, "\nStill unidentified (listeners asked for an ID): %s\n", strings.Join(parts, ", "))
	}
	if tl != nil {
		var meta []string
		for _, m := range []string{tl.Date, tl.PlayTime} {
			if m != "" {
				meta = append(meta, m)
			}
		}
		if tl.IDed != "" {
			meta = append(meta, tl.IDed+" IDed")
		}
		fmt.Fprintf(w, "\n1001tracklists match: %s (%s)\n  %s\n", tl.Title, strings.Join(meta, ", "), tl.URL)
	}
}

var unsafeFileChars = regexp.MustCompile(`[/\\:*?"<>|\x00-\x1f]+`)

// textFileName is "<set title>.txt", made safe for the file system.
func textFileName(t *Track) string {
	name := strings.Join(strings.Fields(unsafeFileChars.ReplaceAllString(t.Title, " ")), " ")
	name = strings.Trim(name, ". ")
	if r := []rune(name); len(r) > 150 {
		name = strings.TrimSpace(string(r[:150]))
	}
	if name == "" {
		name = fmt.Sprintf("soundcloud-%d", t.ID)
	}
	return name + ".txt"
}

func (r *Report) Text() string {
	t := r.Track
	var b strings.Builder
	fmt.Fprintf(&b, "%s\n", t.Title)
	fmt.Fprintf(&b, "by %s · %s\n%s\n", t.User.Username, fmtTime(r.Dur), t.PermalinkURL)
	fmt.Fprintf(&b, "identified %s with sctrackid\n", time.Now().Format("2006-01-02 15:04"))
	printTable(&b, r.Entries, r.Unresolved, r.TL)
	return b.String()
}

// writeTextFile saves the tracklist as "<set title>.txt" in the current directory.
func writeTextFile(r *Report) (string, error) {
	path := textFileName(r.Track)
	return path, os.WriteFile(path, []byte(r.Text()), 0o644)
}

func (r *Report) JSON() map[string]any {
	type ask struct {
		TimeSec int `json:"time_sec"`
		Count   int `json:"count"`
	}
	asks := []ask{}
	for ts, n := range r.Unresolved {
		asks = append(asks, ask{ts, n})
	}
	sort.Slice(asks, func(i, j int) bool { return asks[i].TimeSec < asks[j].TimeSec })
	entries := r.Entries
	if entries == nil {
		entries = []*Entry{}
	}
	return map[string]any{
		"url":                r.Track.PermalinkURL,
		"title":              r.Track.Title,
		"uploader":           r.Track.User.Username,
		"duration_sec":       r.Dur,
		"tracks":             entries,
		"unresolved_id_asks": asks,
		"tracklists1001":     r.TL,
	}
}
