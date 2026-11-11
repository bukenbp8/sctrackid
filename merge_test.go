package main

import "testing"

func shz(artist, title string, at int) Candidate {
	return Candidate{Artist: artist, Title: title, TimeSec: at, Source: "shazam"}
}

func TestMergeVersions(t *testing.T) {
	es := Merge([]Candidate{
		shz("Laserkraft 3D", "Nein, Mann! (Tocadisco's TOCACABANA Remix)", 795),
		shz("Laserkraft 3D", "Nein Mann (Tristate Remix)", 825),
		shz("Laserkraft 3D", "Nein Mann (Tristate Remix)", 855),
		shz("Someone Else", "Nein Mann", 885),                                // other artist
		shz("Laserkraft 3D", "Nein Mann (Some Other Long Dub Rework)", 3000), // far away: played again
		{Artist: "DJ A", Title: "Song (Alpha Beta Gamma Remix)", TimeSec: 4000, Source: "description"},
		{Artist: "DJ A", Title: "Song (Delta Epsilon Zeta Rework)", TimeSec: 4200, Source: "description"}, // listed on purpose
	})
	if len(es) != 5 {
		for _, e := range es {
			t.Logf("%d %s - %s %v", e.TimeSec, e.Artist, e.Title, e.Sources)
		}
		t.Fatalf("%d entries, want 5", len(es))
	}
	if e := es[0]; e.Title != "Nein Mann (Tristate Remix)" || e.TimeSec != 795 || e.Sources["shazam"] != 3 || e.Confidence != "high" {
		t.Errorf("folded entry: %+v", e)
	}
}

func TestStartFromMatchPosition(t *testing.T) {
	at := func(title string, sample, start int) Candidate {
		c := shz("A", title, sample)
		c.Start, c.HasStart = start, true
		return c
	}
	es := Merge([]Candidate{
		at("Steady", 495, 452), at("Steady", 525, 453), at("Steady", 555, 451), // agree: started ~7:32
		at("Jumped In", 1000, 700),                           // would be 5 min early: not believed
		at("Chopped", 2000, 1900), at("Chopped", 2030, 1700), // looped or cut: the earliest sample decides
		shz("A", "Old Cache", 3000), shz("A", "Old Cache", 3030), // no position data
	})
	want := map[string]struct {
		time int
		conf string
	}{"Steady": {452, "high"}, "Jumped In": {1000, "medium"}, "Chopped": {1900, "high"}, "Old Cache": {3000, "high"}}
	for _, e := range es {
		if w := want[e.Title]; e.TimeSec != w.time || e.Confidence != w.conf {
			t.Errorf("%s: time %d conf %s, want %d %s", e.Title, e.TimeSec, e.Confidence, w.time, w.conf)
		}
	}
}
