package main

import (
	"path/filepath"
	"testing"
	"time"
)

func testReport(id int64, entries ...*Entry) *Report {
	t := &Track{ID: id, Title: "Spätsauna b2b", PermalinkURL: "https://soundcloud.com/dj/spaetsauna"}
	t.User.Username = "dj"
	return &Report{Track: t, Dur: 3600, Entries: entries, Unresolved: map[int]int{120: 2}, Identified: time.Now()}
}

func TestLibrary(t *testing.T) {
	lib, err := openLibrary(filepath.Join(t.TempDir(), "lib.db"))
	if err != nil {
		t.Fatal(err)
	}
	full := scanOpts{MinConf: "low"}
	rep := testReport(42,
		&Entry{Artist: "Ben Klock", Title: "Subzero", TimeSec: 60, Confidence: "high", Sources: map[string]int{"shazam": 2}},
		&Entry{Artist: "Someone", Title: "Maybe", TimeSec: 300, Confidence: "low"},
	)
	if err := lib.Save(rep, "https://m.soundcloud.com/DJ/spaetsauna/?si=abc", full); err != nil {
		t.Fatal(err)
	}

	for _, u := range []string{"https://soundcloud.com/dj/spaetsauna", "http://www.soundcloud.com/dj/spaetsauna/", "https://m.soundcloud.com/dj/spaetsauna?utm=x"} {
		if id, ok := lib.Lookup(u, scanOpts{NoAudio: true, MinConf: "high"}); !ok || id != 42 {
			t.Errorf("Lookup(%s) = %d, %v", u, id, ok)
		}
	}
	if _, ok := lib.Lookup("https://soundcloud.com/dj/other", full); ok {
		t.Error("unknown link matched")
	}

	got, err := lib.Get(42, "high")
	if err != nil || len(got.Entries) != 1 || got.Entries[0].Sources["shazam"] != 2 || got.Unresolved[120] != 2 {
		t.Fatalf("Get = %+v, %v", got, err)
	}

	// A skimpier run must neither be served for a full request nor replace the full result.
	if err := lib.Save(testReport(42), "https://soundcloud.com/dj/spaetsauna", scanOpts{NoAudio: true, MinConf: "low"}); err != nil {
		t.Fatal(err)
	}
	if got, _ := lib.Get(42, ""); len(got.Entries) != 2 {
		t.Errorf("full result was overwritten by a no-audio run")
	}
	if _, ok := lib.Lookup("https://soundcloud.com/dj/spaetsauna", scanOpts{Thorough: true, MinConf: "low"}); ok {
		t.Error("non-thorough result served for a thorough request")
	}

	for q, want := range map[string]int{"klock subzero": 1, "spätsauna": 1, "DJ": 1, "klock nothing": 0, "100%": 0} {
		sets, err := lib.Search(q, 10)
		if err != nil || len(sets) != want {
			t.Errorf("Search(%q) = %d sets, %v; want %d", q, len(sets), err, want)
		}
	}
	if sets, _ := lib.Search("subzero", 10); len(sets) != 1 || len(sets[0].Matches) != 1 || sets[0].Matches[0].TimeSec != 60 {
		t.Errorf("track match not reported: %+v", sets)
	}
}

func TestUserEdits(t *testing.T) {
	lib, err := openLibrary(filepath.Join(t.TempDir(), "lib.db"))
	if err != nil {
		t.Fatal(err)
	}
	full := scanOpts{MinConf: "low"}
	link := "https://soundcloud.com/dj/spaetsauna"
	entries := func() []*Entry {
		return []*Entry{
			{Artist: "Ben Klock", Title: "Subzero", TimeSec: 60, Confidence: "low", Sources: map[string]int{"comment": 1}},
			{Artist: "Wrong Guy", Title: "Wrong Tune", TimeSec: 300, Confidence: "medium", Sources: map[string]int{"shazam": 1}},
			{Artist: "Third", Title: "Thing", TimeSec: 600, Confidence: "high", Sources: map[string]int{"shazam": 2}},
		}
	}
	if err := lib.Save(testReport(42, entries()...), link, full); err != nil {
		t.Fatal(err)
	}

	// confirm the first track, replace the second as a retry would
	rep, err := lib.Update(42, func(es []*Entry) ([]*Entry, error) {
		i := findEntry(es, entryRef{"Ben Klock", "Subzero", 60})
		j := findEntry(es, entryRef{"Wrong Guy", "Wrong Tune", 300})
		if i != 0 || j != 1 {
			t.Fatalf("findEntry = %d, %d", i, j)
		}
		es[i].Approved = true
		es[j] = &Entry{Artist: "Right Guy", Title: "Right Tune", TimeSec: 300, Confidence: "medium",
			Sources: map[string]int{"shazam": 1}, Rejected: []TrackName{{"Wrong Guy", "Wrong Tune"}}}
		return es, nil
	})
	if err != nil || !rep.Entries[0].Approved || rep.Entries[1].Artist != "Right Guy" {
		t.Fatalf("Update = %+v, %v", rep, err)
	}
	if _, err := lib.Update(42, func(es []*Entry) ([]*Entry, error) { return nil, errNoEntry }); err != errNoEntry {
		t.Errorf("failed update returned %v", err)
	}

	// a confirmed low-confidence track passes the confidence filter, and search sees the new track
	if got, _ := lib.Get(42, "high"); len(got.Entries) != 2 || !got.Entries[0].Approved {
		t.Errorf("filtered Get = %d entries", len(got.Entries))
	}
	if sets, _ := lib.Search("right tune", 10); len(sets) != 1 {
		t.Error("replaced track not searchable")
	}
	if sets, _ := lib.Search("wrong tune", 10); len(sets) != 0 {
		t.Error("rejected track still searchable")
	}

	// a rescan brings the wrong track back; the user's edits must win
	if err := lib.Save(testReport(42, entries()...), link, full); err != nil {
		t.Fatal(err)
	}
	got, _ := lib.Get(42, "")
	if len(got.Entries) != 3 || !got.Entries[0].Approved || got.Entries[1].Artist != "Right Guy" ||
		len(got.Entries[1].Rejected) != 1 || got.Entries[2].Artist != "Third" {
		for _, e := range got.Entries {
			t.Logf("%+v", e)
		}
		t.Error("user edits lost on rescan")
	}
}
