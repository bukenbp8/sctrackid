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
	Approved   bool           `json:"approved,omitempty"` // a user confirmed this is the right track
	Rejected   []TrackName    `json:"rejected,omitempty"` // what was listed here before a retry replaced it

	order    int
	times    map[string][]int
	variants map[string]int
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
				times: map[string][]int{}, variants: map[string]int{}, order: c.Order}
			entries = append(entries, e)
		}
		e.Sources[c.Source]++
		if c.URL != "" && e.ShazamURL == "" {
			e.ShazamURL = c.URL
		}
		e.variants[c.Artist+"\x00"+c.Title] += sourceRank[c.Source]*100 + 1
		if c.TimeSec >= 0 {
			e.times[c.Source] = append(e.times[c.Source], c.TimeSec)
		}
	}

	for _, e := range entries {
		// Display name: the best-ranked / most frequent spelling.
		best, bestScore := "", -1
		for v, s := range e.variants {
			if s > bestScore || s == bestScore && v < best {
				best, bestScore = v, s
			}
		}
		parts := strings.SplitN(best, "\x00", 2)
		e.Artist, e.Title = parts[0], parts[1]

		// Time: description > shazam (first hit) > median of comments.
		switch {
		case len(e.times["description"]) > 0:
			e.TimeSec = e.times["description"][0]
		case len(e.times["shazam"]) > 0:
			e.TimeSec = minInt(e.times["shazam"])
		case len(e.times["comment"]) > 0:
			ts := append([]int(nil), e.times["comment"]...)
			sort.Ints(ts)
			e.TimeSec = ts[len(ts)/2]
		}
		e.Confidence = confidence(e.Sources)
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
