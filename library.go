package main

// The library is a SQLite database of finished lookups. A set that was
// already decoded (with at least the options now asked for) is answered
// instantly from it, and everything in it can be searched by set title,
// uploader, or any identified track.

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const librarySchema = `
PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;
CREATE TABLE IF NOT EXISTS sets (
	id            INTEGER PRIMARY KEY, -- SoundCloud track id
	url           TEXT NOT NULL,
	title         TEXT NOT NULL,
	uploader      TEXT NOT NULL,
	duration_sec  INTEGER NOT NULL,
	track_count   INTEGER NOT NULL,
	opts          TEXT NOT NULL,       -- JSON scanOpts the result was made with
	report        TEXT NOT NULL,       -- JSON storedReport
	identified_at INTEGER NOT NULL     -- unix seconds
);
CREATE INDEX IF NOT EXISTS sets_identified ON sets(identified_at DESC);
CREATE TABLE IF NOT EXISTS set_urls (     -- every link that led to a set, normalised
	url    TEXT PRIMARY KEY,
	set_id INTEGER NOT NULL REFERENCES sets(id) ON DELETE CASCADE
);
CREATE TABLE IF NOT EXISTS set_tracks (
	set_id     INTEGER NOT NULL REFERENCES sets(id) ON DELETE CASCADE,
	pos        INTEGER NOT NULL,
	artist     TEXT NOT NULL,
	title      TEXT NOT NULL,
	time_sec   INTEGER NOT NULL,
	confidence TEXT NOT NULL,
	PRIMARY KEY (set_id, pos)
);
`

type library struct{ db *sql.DB }

func openLibrary(path string) (*library, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?_pragma=busy_timeout(5000)")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite has a single writer; one connection avoids lock contention
	if _, err := db.Exec(librarySchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("library %s: %w", path, err)
	}
	return &library{db}, nil
}

// scanOpts are the options that change what a lookup finds.
type scanOpts struct {
	Thorough   bool   `json:"thorough"`
	NoAudio    bool   `json:"no_audio"`
	NoComments bool   `json:"no_comments"`
	No1001     bool   `json:"no_1001"`
	NoLinks    bool   `json:"no_links"`
	MinConf    string `json:"min_confidence"`
}

func optsOf(r jobRequest, base options) scanOpts {
	o := scanOpts{r.Thorough, r.NoAudio, r.NoComments, r.No1001, r.NoLinks, r.MinConfidence}
	if o.MinConf == "" {
		o.MinConf = base.minConf
	}
	return o
}

var confRank = map[string]int{"low": 0, "medium": 1, "high": 2}

// covers reports whether a result made with `have` contains everything a
// lookup with `want` would find (results are filtered down on read).
func (have scanOpts) covers(want scanOpts) bool {
	return (!have.NoAudio || want.NoAudio) &&
		(have.Thorough || !want.Thorough || want.NoAudio) &&
		(!have.NoComments || want.NoComments) &&
		(!have.No1001 || want.No1001) &&
		(!have.NoLinks || want.NoLinks) &&
		confRank[strings.ToLower(have.MinConf)] <= confRank[strings.ToLower(want.MinConf)]
}

// normalizeURL maps the ways of writing a SoundCloud link onto one key.
func normalizeURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil {
		return strings.ToLower(raw)
	}
	host := strings.ToLower(u.Host)
	for _, p := range []string{"www.", "m."} {
		host = strings.TrimPrefix(host, p)
	}
	return "https://" + host + strings.ToLower(strings.TrimRight(u.Path, "/"))
}

// storedReport is the persisted form of a Report.
type storedReport struct {
	Track      *Track        `json:"track"`
	Dur        int           `json:"dur"`
	Entries    []*Entry      `json:"entries"`
	Unresolved map[int]int   `json:"unresolved"`
	TL         *TL1001Result `json:"tl"`
	Identified time.Time     `json:"identified"`
}

// Lookup finds a stored result for a link that covers the wanted options.
func (l *library) Lookup(link string, want scanOpts) (int64, bool) {
	var id int64
	var raw string
	err := l.db.QueryRow(`SELECT s.id, s.opts FROM set_urls u JOIN sets s ON s.id = u.set_id WHERE u.url = ?`,
		normalizeURL(link)).Scan(&id, &raw)
	if err != nil {
		return 0, false
	}
	var have scanOpts
	if json.Unmarshal([]byte(raw), &have) != nil || !have.covers(want) {
		return 0, false
	}
	return id, true
}

// Save stores a finished report. An existing entry is only replaced by a
// result at least as complete, so a quick "skip audio" run never overwrites
// a full one.
func (l *library) Save(rep *Report, submitted string, o scanOpts) error {
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	id := rep.Track.ID
	replace := true
	var raw string
	if err := tx.QueryRow(`SELECT opts FROM sets WHERE id = ?`, id).Scan(&raw); err == nil {
		var have scanOpts
		replace = json.Unmarshal([]byte(raw), &have) != nil || o.covers(have)
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}

	if replace {
		t := *rep.Track // keep only what the report needs
		t.Description, t.TrackAuthorization, t.Media.Transcodings = "", "", nil
		sr := storedReport{&t, rep.Dur, rep.Entries, rep.Unresolved, rep.TL, rep.Identified}
		repJSON, err := json.Marshal(sr)
		if err != nil {
			return err
		}
		optJSON, _ := json.Marshal(o)
		if _, err := tx.Exec(`INSERT INTO sets (id, url, title, uploader, duration_sec, track_count, opts, report, identified_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT(id) DO UPDATE SET url=excluded.url, title=excluded.title, uploader=excluded.uploader,
				duration_sec=excluded.duration_sec, track_count=excluded.track_count, opts=excluded.opts,
				report=excluded.report, identified_at=excluded.identified_at`,
			id, t.PermalinkURL, t.Title, t.User.Username, rep.Dur, len(rep.Entries), optJSON, repJSON, rep.Identified.Unix()); err != nil {
			return err
		}
		if _, err := tx.Exec(`DELETE FROM set_tracks WHERE set_id = ?`, id); err != nil {
			return err
		}
		for i, e := range rep.Entries {
			if _, err := tx.Exec(`INSERT INTO set_tracks (set_id, pos, artist, title, time_sec, confidence) VALUES (?, ?, ?, ?, ?, ?)`,
				id, i, e.Artist, e.Title, e.TimeSec, e.Confidence); err != nil {
				return err
			}
		}
	}
	for _, u := range []string{submitted, rep.Track.PermalinkURL} {
		if _, err := tx.Exec(`INSERT INTO set_urls (url, set_id) VALUES (?, ?) ON CONFLICT(url) DO UPDATE SET set_id = excluded.set_id`,
			normalizeURL(u), id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// Get loads a stored report, keeping only entries at or above minConf.
func (l *library) Get(id int64, minConf string) (*Report, error) {
	var raw string
	if err := l.db.QueryRow(`SELECT report FROM sets WHERE id = ?`, id).Scan(&raw); err != nil {
		return nil, err
	}
	var sr storedReport
	if err := json.Unmarshal([]byte(raw), &sr); err != nil {
		return nil, err
	}
	entries := sr.Entries
	if minConf != "" {
		entries = filterConfidence(entries, minConf)
	}
	return &Report{sr.Track, sr.Dur, entries, sr.Unresolved, sr.TL, sr.Identified}, nil
}

type libraryMatch struct {
	Artist  string `json:"artist"`
	Title   string `json:"title"`
	TimeSec int    `json:"time_sec"`
}

type librarySet struct {
	ID         int64          `json:"id,string"` // SoundCloud ids exceed JS's safe integers in theory
	URL        string         `json:"url"`
	Title      string         `json:"title"`
	Uploader   string         `json:"uploader"`
	Duration   int            `json:"duration_sec"`
	Tracks     int            `json:"track_count"`
	Identified time.Time      `json:"identified"`
	Matches    []libraryMatch `json:"matches,omitempty"` // tracks matching the search
}

func likeEscape(s string) string {
	return "%" + strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(s) + "%"
}

// Search returns sets whose title, uploader or identified tracks contain
// every search term, newest first. An empty query lists the latest sets.
func (l *library) Search(q string, limit int) ([]librarySet, error) {
	terms := strings.Fields(q)
	if len(terms) > 8 {
		terms = terms[:8]
	}
	var where []string
	var args []any
	for _, t := range terms {
		p := likeEscape(t)
		where = append(where, `(s.title LIKE ? ESCAPE '\' OR s.uploader LIKE ? ESCAPE '\' OR EXISTS (
			SELECT 1 FROM set_tracks t WHERE t.set_id = s.id AND (t.artist || ' ' || t.title) LIKE ? ESCAPE '\'))`)
		args = append(args, p, p, p)
	}
	query := `SELECT s.id, s.url, s.title, s.uploader, s.duration_sec, s.track_count, s.identified_at FROM sets s`
	if len(where) > 0 {
		query += " WHERE " + strings.Join(where, " AND ")
	}
	query += " ORDER BY s.identified_at DESC LIMIT ?"
	args = append(args, limit)

	rows, err := l.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	out := []librarySet{}
	for rows.Next() {
		var s librarySet
		var at int64
		if err := rows.Scan(&s.ID, &s.URL, &s.Title, &s.Uploader, &s.Duration, &s.Tracks, &at); err != nil {
			rows.Close()
			return nil, err
		}
		s.Identified = time.Unix(at, 0)
		out = append(out, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Show which tracks matched, so searching for a track points at the right spot.
	if len(terms) > 0 {
		var conds []string
		var targs []any
		for _, t := range terms {
			conds = append(conds, `(artist || ' ' || title) LIKE ? ESCAPE '\'`)
			targs = append(targs, likeEscape(t))
		}
		tq := `SELECT artist, title, time_sec FROM set_tracks WHERE set_id = ? AND (` + strings.Join(conds, " OR ") + `) ORDER BY pos LIMIT 5`
		for i := range out {
			rows, err := l.db.Query(tq, append([]any{out[i].ID}, targs...)...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var m libraryMatch
				if rows.Scan(&m.Artist, &m.Title, &m.TimeSec) == nil {
					out[i].Matches = append(out[i].Matches, m)
				}
			}
			rows.Close()
		}
	}
	return out, nil
}
