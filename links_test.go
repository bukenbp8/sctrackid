package main

import "testing"

func TestPickAPIResult(t *testing.T) {
	body := []byte(`{"items":[
	 {"id":{"videoId":"aaaaaaaaaaa"},"snippet":{"title":"Honey Dijon Boiler Room Melbourne DJ Set","channelTitle":"Boiler Room"}},
	 {"id":{"videoId":"bbbbbbbbbbb"},"snippet":{"title":"Fly Away (Original Mix)","channelTitle":"Damian Lazarus - Topic"}},
	 {"id":{"videoId":"ccccccccccc"},"snippet":{"title":"Damian Lazarus &amp; The Ancient Moons - Fly Away (Dennis Ferrer Remix)","channelTitle":"Crosstown Rebels"}}]}`)
	id, vt, st := pickAPIResult(body, "Damian Lazarus & The Ancient Moons", "Fly Away (Dennis Ferrer Remix)")
	if st != ytOK || id != "ccccccccccc" {
		t.Fatalf("got %q %q %v, want the Dennis Ferrer remix", id, vt, st)
	}
	if vt != "Damian Lazarus & The Ancient Moons - Fly Away (Dennis Ferrer Remix)" {
		t.Errorf("title not unescaped: %q", vt)
	}
}

func TestVideoScoreRejectsWrongTrack(t *testing.T) {
	if s := videoScore("Mike Dehnert", "Black Smoke", "Black Sabbath - Smoke on the Water", "Rock Hits"); s != 0 {
		t.Errorf("wrong artist should score 0, got %v", s)
	}
	if s := videoScore("Twism & DJ Queen B", "Those Days", "Twism   Those Days Baldaccini & Bedo Mashup", "someone"); s != 0 {
		t.Errorf("mashup should score 0, got %v", s)
	}
	if s := videoScore("Mondragon", "Vertigo", "Vertigo", "Mondragon - Topic"); s == 0 {
		t.Error("Topic channel upload should match")
	}
}

func TestBestPrefersYouTube(t *testing.T) {
	l := &Links{YouTubeSearch: "yt-search", BandcampQuery: "bc"}
	if l.Best() != "yt-search" {
		t.Errorf("got %q", l.Best())
	}
	l.YouTube = "yt"
	if l.Best() != "yt" {
		t.Errorf("got %q", l.Best())
	}
}
