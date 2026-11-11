package main

import (
	"regexp"
	"sort"
	"strings"
)

type Entry struct {
	Artist     string         `json:"artist"`
	Title      string         `json:"title"`
	TimeSec    int            `json:"time_sec"` // -1 unknown
	Sources    map[string]int `json:"sources"`
	ShazamURL  string         `json:"shazam_url,omitempty"`
	Confidence string         `json:"confidence"`
	Links      *Links         `json:"links,omitempty"`
	Unverified bool           `json:"unverified,omitempty"` // a single Shazam hit that a second listen did not confirm
	Approved   bool           `json:"approved,omitempty"`   // a user confirmed this is the right track
	Rejected   []TrackName    `json:"rejected,omitempty"`   // what was listed here before a retry replaced it

	order    int
	times    map[string][]int
	variants map[string]int       // spelling -> score
	urls     map[string]string    // spelling -> shazam link
	starts   map[string][]startAt // spelling -> computed starts of the recording in the set
	weak     bool
}

// TrackName is a bare artist/title pair.
type TrackName struct {
	Artist string `json:"artist"`
	Title  string `json:"title"`
}

var (
	noiseBracketRe = regexp.MustCompile(`(?i)[(\[][^)\]]*(?:original|extended|radio|club|album)\s*(?:mix|edit|version)?[^)\]]*[)\]]`)
	featRe         = regexp.MustCompile(`(?i)\b(?:feat|ft|featuring|vs|x)\b\.?`)
	nonAlnumRe     = regexp.MustCompile(`[^\p{L}\p{N}]+`)
)

func tokens(s string) map[string]bool {
	s = strings.ToLower(noiseBracketRe.ReplaceAllString(s, " "))
	s = featRe.ReplaceAllString(s, " ")
	s = nonAlnumRe.ReplaceAllString(s, " ")
	out := map[string]bool{}
	for _, f := range strings.Fields(s) {
		if f != "the" && f != "and" && f != "a" {
			out[f] = true
		}
	}
	return out
}

func overlap(a, b map[string]bool) (inter, union int) {
	for k := range a {
		if b[k] {
			inter++
		}
	}
	return inter, len(a) + len(b) - inter
}

// sameTrack decides whether two artist/title pairs refer to the same track.
func sameTrack(a1, t1, a2, t2 string) bool {
	ti, tu := overlap(tokens(t1), tokens(t2))
	if tu == 0 || float64(ti)/float64(tu) < 0.5 {
		return false
	}
	ai, au := overlap(tokens(a1), tokens(a2))
	if au > 0 && ai == 0 {
		return false
	}
	all1, all2 := tokens(a1+" "+t1), tokens(a2+" "+t2)
	i, u := overlap(all1, all2)
	return u > 0 && float64(i)/float64(u) >= 0.5
}

var sourceRank = map[string]int{"shazam": 3, "description": 2, "comment": 1}

const (
	versionSpan = 480 // seconds within which two versions of one song count as the same play
	maxLead     = 120 // a computed start further than this before the first sample that heard the track is not believed
	startSlack  = 6   // samples of one play agree on the start within this many seconds
)

// sameSong reports whether two tracks are versions of one song: the same
// artist and the same title once everything in brackets is left out
// ("Nein, Mann! (X Remix)" and "Nein Mann (Y Remix)").
func sameSong(a1, t1, a2, t2 string) bool {
	b1, b2 := tokens(bracketRe.ReplaceAllString(t1, " ")), tokens(bracketRe.ReplaceAllString(t2, " "))
	if len(b1) == 0 || len(b1) != len(b2) {
		return false
	}
	if i, _ := overlap(b1, b2); i != len(b1) {
		return false
	}
	ai, au := overlap(tokens(a1), tokens(a2))
	return au > 0 && float64(ai)/float64(au) >= 0.5
}

// agreedStart picks the start that most samples of a track agree on (the
// earliest such group on a tie). DJs loop and cut, so samples of one play
// often disagree; when no two agree, the earliest sample is taken at its word.
func agreedStart(starts []startAt) (int, bool) {
	if len(starts) == 0 {
		return 0, false
	}
	first := starts[0]
	st := make([]int, len(starts))
	for i, s := range starts {
		st[i] = s.start
		if s.sample < first.sample {
			first = s
		}
	}
	sort.Ints(st)
	bestLo, bestN := 0, 1
	for lo := range st {
		n := 0
		for hi := lo; hi < len(st) && st[hi]-st[lo] <= startSlack; hi++ {
			n++
		}
		if n > bestN {
			bestLo, bestN = lo, n
		}
	}
	if bestN < 2 {
		return first.start, true
	}
	return st[bestLo+bestN/2], true
}

// startAt is where a recording began in the set, as computed from one sample.
type startAt struct{ sample, start int }

func shazamOnly(e *Entry) bool {
	return e.Sources["shazam"] > 0 && e.Sources["description"] == 0 && e.Sources["comment"] == 0
}

// finish derives the displayed name, time and confidence from what was collected.
func (e *Entry) finish() {
	// Display name: the best-ranked / most frequent spelling.
	best, bestScore := "", -1
	for v, s := range e.variants {
		if s > bestScore || s == bestScore && v < best {
			best, bestScore = v, s
		}
	}
	parts := strings.SplitN(best, "\x00", 2)
	e.Artist, e.Title = parts[0], parts[1]
	if u := e.urls[best]; u != "" {
		e.ShazamURL = u
	}

	// Time: description > shazam > median of comments.
	switch {
	case len(e.times["description"]) > 0:
		e.TimeSec = e.times["description"][0]
	case len(e.times["shazam"]) > 0:
		// The first sample that heard the track, or better: where the track
		// started, worked out from how far into the recording the samples were.
		first := minInt(e.times["shazam"])
		e.TimeSec = first
		// A start far ahead of the first sample means the track was dropped
		// in halfway (or the match is a stray): the first sample is the
		// better guess then.
		if st, ok := agreedStart(e.starts[best]); ok && st >= first-maxLead {
			e.TimeSec = min(max(st, 0), first+startSlack)
		}
	case len(e.times["comment"]) > 0:
		ts := append([]int(nil), e.times["comment"]...)
		sort.Ints(ts)
		e.TimeSec = ts[len(ts)/2]
	}

	e.Confidence, e.Unverified = confidence(e.Sources), false
	if e.weak && e.Confidence == "medium" { // nothing but one unconfirmed Shazam hit
		e.Confidence, e.Unverified = "low", true
	}
}

// absorb folds another version of the same song into e.
func (e *Entry) absorb(b *Entry) {
	for k, n := range b.Sources {
		e.Sources[k] += n
	}
	for k, ts := range b.times {
		e.times[k] = append(e.times[k], ts...)
	}
	for v, n := range b.variants {
		e.variants[v] += n
	}
	for v, u := range b.urls {
		e.urls[v] = u
	}
	for v, st := range b.starts {
		e.starts[v] = append(e.starts[v], st...)
	}
	e.weak = e.weak && b.weak
	e.order = min(e.order, b.order)
}

func Merge(cands []Candidate) []*Entry {
	var entries []*Entry
	for _, c := range cands {
		var e *Entry
		for _, x := range entries {
			if sameTrack(x.Artist, x.Title, c.Artist, c.Title) {
				e = x
				break
			}
		}
		if e == nil {
			e = &Entry{Artist: c.Artist, Title: c.Title, TimeSec: -1, Sources: map[string]int{},
				times: map[string][]int{}, variants: map[string]int{}, urls: map[string]string{},
				starts: map[string][]startAt{}, order: c.Order}
			entries = append(entries, e)
		}
		v := c.Artist + "\x00" + c.Title
		e.Sources[c.Source]++
		e.weak = e.weak || c.Weak
		if c.URL != "" && e.urls[v] == "" {
			e.urls[v] = c.URL
		}
		e.variants[v] += sourceRank[c.Source]*100 + 1
		if c.TimeSec >= 0 {
			e.times[c.Source] = append(e.times[c.Source], c.TimeSec)
		}
		if c.HasStart {
			e.starts[v] = append(e.starts[v], startAt{c.TimeSec, c.Start})
		}
	}
	for _, e := range entries {
		e.finish()
	}
	sortEntries(entries)

	// Shazam often names one playing track as different releases of the same
	// song from one sample to the next. Fold those into one entry, unless
	// both come from a written tracklist or comments (then it is deliberate).
	for i := 0; i < len(entries); i++ {
		a := entries[i]
		for j := i + 1; j < len(entries); j++ {
			b := entries[j]
			if a.TimeSec < 0 || b.TimeSec < 0 || b.TimeSec-a.TimeSec > versionSpan {
				break
			}
			if (shazamOnly(a) || shazamOnly(b)) && sameSong(a.Artist, a.Title, b.Artist, b.Title) {
				a.absorb(b)
				a.finish()
				entries = append(entries[:j], entries[j+1:]...)
				j--
			}
		}
	}
	sortEntries(entries)
	return entries
}

// sortEntries orders by time, entries without one last.
func sortEntries(entries []*Entry) {
	sort.SliceStable(entries, func(i, j int) bool {
		a, b := entries[i], entries[j]
		if (a.TimeSec < 0) != (b.TimeSec < 0) {
			return a.TimeSec >= 0
		}
		if a.TimeSec != b.TimeSec {
			return a.TimeSec < b.TimeSec
		}
		return a.order < b.order
	})
}

// keepUserEdits carries what users did to an earlier result over to a fresh
// one: confirmed tracks and retried slots survive a rescan, and a track that
// was rejected for a slot does not come back there.
func keepUserEdits(old, fresh []*Entry) []*Entry {
	var kept []*Entry
	for _, e := range old {
		if e.Approved || len(e.Rejected) > 0 {
			kept = append(kept, e)
		}
	}
	if len(kept) == 0 {
		return fresh
	}
	out := append([]*Entry(nil), kept...)
next:
	for _, n := range fresh {
		for _, k := range kept {
			if sameTrack(k.Artist, k.Title, n.Artist, n.Title) {
				continue next
			}
			near := k.TimeSec < 0 || n.TimeSec < 0 || max(k.TimeSec-n.TimeSec, n.TimeSec-k.TimeSec) <= 180
			for _, r := range k.Rejected {
				if near && sameTrack(r.Artist, r.Title, n.Artist, n.Title) {
					continue next
				}
			}
		}
		out = append(out, n)
	}
	sortEntries(out)
	return out
}

func confidence(src map[string]int) string {
	sh, de, co := src["shazam"], src["description"], src["comment"]
	kinds := 0
	for _, n := range src {
		if n > 0 {
			kinds++
		}
	}
	switch {
	case de > 0 || kinds >= 2 || sh >= 2 || co >= 3:
		return "high"
	case sh == 1 || co == 2:
		return "medium"
	default:
		return "low"
	}
}

func minInt(xs []int) int {
	m := xs[0]
	for _, x := range xs[1:] {
		m = min(m, x)
	}
	return m
}
