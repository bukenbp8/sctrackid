package main

import (
	"regexp"
	"strconv"
	"strings"
	"unicode"
)

// Candidate is one "this track plays at roughly this time" claim from any source.
type Candidate struct {
	Artist, Title string
	TimeSec       int // -1 if unknown
	Source        string
	Order         int    // position in a description tracklist, for untimed lists
	URL           string // shazam link, if any
	Weak          bool   // shazam: heard once, and a second listen did not confirm it
}

var (
	tsRe         = regexp.MustCompile(`(?:^|[^\d:])((?:\d{1,2}:)?\d{1,2}:\d{2})(?:[^\d:]|$)`)
	urlRe        = regexp.MustCompile(`https?://\S+|www\.\S+`)
	mentionRe    = regexp.MustCompile(`@[\w.\-]+`)
	numberingRe  = regexp.MustCompile(`^\s*(?:#?\d{1,3}\s*[.):\]]|\[\d{1,3}\]|\d{1,3}\s+-\s+)\s*`)
	dashRe       = regexp.MustCompile(`\s+[-–—]+\s+|\s*[–—]\s*`)
	leadNoiseRe  = regexp.MustCompile(`(?i)^(?:track ?id|tracklist|track|tune|song|id|it'?s|its|this is|that'?s|thats|i think it'?s|pretty sure it'?s|i think|is|=+|-+>|>+|:)[\s:,\-]*`)
	trailNoiseRe = regexp.MustCompile(`(?i)[\s,;:.!?\-]*(?:\bon\s+\w+\s+records?\b.*|\[(?:free|out now)[^\]]*\]|\(out now[^)]*\))$`)
	idWordRe     = regexp.MustCompile(`(?i)^(?:id|\?+|unknown|unreleased|tba)$`)
	spamRe       = regexp.MustCompile(`(?i)check (?:out|my)|follow (?:me|us)|subscribe|\b(?:my|our) (?:mix|set|page|channel|music)|link in|set (?:of the|ever)|best set|what a set|thank you|thanks`)
	askRe        = regexp.MustCompile(`(?i)\b(?:id\s*\?|id pls|id please|track\s*id|track\s*\?|tune\s*\?|song\s*\?|name of (?:the|this) (?:track|song|tune)|what(?:'s| is) (?:this|the) (?:track|song|tune)|anyone know (?:this|the) (?:track|song|tune))`)
)

func parseTimestamp(s string) int {
	parts := strings.Split(s, ":")
	total := 0
	for _, p := range parts {
		n, _ := strconv.Atoi(p)
		total = total*60 + n
	}
	return total
}

func cleanPart(s string) string {
	s = strings.TrimFunc(s, func(r rune) bool {
		return unicode.IsSpace(r) || unicode.IsPunct(r) && !strings.ContainsRune("()[]&'$!", r) || unicode.IsSymbol(r) || r > 0x2000 && !unicode.IsLetter(r) && !unicode.IsDigit(r)
	})
	return strings.Join(strings.Fields(s), " ")
}

// extractTrack tries to pull "Artist - Title" out of a free-text line.
func extractTrack(line string) (artist, title string, ok bool) {
	line = urlRe.ReplaceAllString(line, " ")
	line = mentionRe.ReplaceAllString(line, " ")
	line = tsRe.ReplaceAllStringFunc(line, func(m string) string {
		// keep surrounding chars that the regex consumed, drop the timestamp
		return " "
	})
	line = numberingRe.ReplaceAllString(line, "")
	line = strings.TrimSpace(line)
	for i := 0; i < 3; i++ { // strip stacked prefixes like "ID: it's ..."
		line = strings.TrimSpace(leadNoiseRe.ReplaceAllString(line, ""))
	}

	loc := dashRe.FindStringIndex(line)
	if loc == nil {
		return "", "", false
	}
	artist = cleanPart(line[:loc[0]])
	title = cleanPart(trailNoiseRe.ReplaceAllString(line[loc[1]:], ""))
	// A second dash usually means "Artist - Title - Label" or chatter; keep the first two parts.
	if l2 := dashRe.FindStringIndex(title); l2 != nil {
		title = cleanPart(title[:l2[0]])
	}

	aw, tw := len(strings.Fields(artist)), len(strings.Fields(title))
	if aw == 0 || tw == 0 || aw > 6 || tw > 12 || len(artist) < 2 || len(title) < 2 {
		return "", "", false
	}
	if idWordRe.MatchString(artist) && idWordRe.MatchString(title) {
		return "", "", false // "ID - ID"
	}
	if spamRe.MatchString(artist + " " + title) {
		return "", "", false
	}
	if !hasLetter(artist) || !hasLetter(title) {
		return "", "", false
	}
	return artist, title, true
}

func hasLetter(s string) bool {
	for _, r := range s {
		if unicode.IsLetter(r) {
			return true
		}
	}
	return false
}

// ParseDescription finds a tracklist in the track description.
func ParseDescription(desc string) []Candidate {
	var out []Candidate
	for i, line := range strings.Split(desc, "\n") {
		a, t, ok := extractTrack(line)
		if !ok {
			continue
		}
		ts := -1
		if m := tsRe.FindStringSubmatch(line); m != nil {
			ts = parseTimestamp(m[1])
		}
		out = append(out, Candidate{Artist: a, Title: t, TimeSec: ts, Source: "description", Order: i})
	}
	// A single dash line in a description is usually not a tracklist ("Recorded live - Berlin").
	if len(out) < 3 {
		return nil
	}
	return out
}

// ParseComments extracts track candidates and "ID?" requests from comments.
func ParseComments(comments []Comment) (cands []Candidate, asks []int) {
	for _, c := range comments {
		ts := -1
		if c.Timestamp != nil {
			ts = int(*c.Timestamp / 1000)
		}
		found := false
		for _, line := range strings.Split(c.Body, "\n") {
			a, t, ok := extractTrack(line)
			if !ok {
				continue
			}
			lts := ts
			if m := tsRe.FindStringSubmatch(line); m != nil {
				lts = parseTimestamp(m[1]) // "45:12 Artist - Title" beats the comment anchor
			}
			cands = append(cands, Candidate{Artist: a, Title: t, TimeSec: lts, Source: "comment"})
			found = true
		}
		if !found && ts >= 0 && askRe.MatchString(c.Body) {
			asks = append(asks, ts)
		}
	}
	return cands, asks
}
