# sctrackid

## Web interface

    sctrackid -serve localhost:8080

or with Docker:

    docker compose up -d        # http://localhost:8080

The compose file keeps everything in the `sctrackid-data` volume: the library database, the Shazam and link caches, and the saved `.txt` tracklists. Optionally set `SC_CLIENT_ID`, `YOUTUBE_API_KEY` and `TRUSTED_PROXIES` in your environment or in a `.env` file next to the compose file.

### Archive

Every finished lookup is saved to a SQLite database (`-db`, default `sctrackid.db`; env `SCTRACKID_DB`; empty disables it).

- If someone submits a set that was already decoded, the result comes back instantly instead of being queued. This only happens when the stored run used at least the options asked for. For example, a run that skipped audio is never served for a request that wants audio. Tick "force rescan" to decode a set again.
- The archive panel searches all decoded sets by set title, uploader, artist or track name, and shows where in the set a matching track plays.
- A new run only replaces a stored result if it is at least as complete.

### Single matches and repeat runs

A track that Shazam hears in only one sample is as often a stray match as a real one. Each of these is listened to again half an interval before and after. If it is heard again it counts as two matches (high confidence); if not, it stays in the list as low confidence, so `min confidence: medium` hides it. This needs `-sample` to be at most half of `-interval` (the defaults, 12 and 30, are fine).

Shazam answers are cached per set. Once a set has been decoded completely, a later run that finds all its answers in the cache (a "force rescan", or the same set with other options) does not download the audio again.

### Track times and versions

Shazam also says how far into the recording a sample is. From that the list shows when a track started in the set, not just the first sample that heard it. A computed start more than two minutes before that first sample is not used, since the DJ probably dropped the track in halfway. Samples cached by an older version carry no position, so those tracks keep the old timing until they are fetched again (`-no-cache`).

Shazam often names one playing track as different releases of the same song from one sample to the next ("Nein, Mann! (X Remix)", then "Nein Mann (Y Remix)"). Entries by the same artist with the same title outside the brackets, within eight minutes of each other, are folded into one, named after the version heard most. Two versions that both come from the description or the comments are left alone.

### Confirming and retrying tracks

Each track of a decoded set has two buttons in the "verify" column:

- **✓** marks the track as confirmed by a user (click again to take it back). Confirmed tracks are labelled "confirmed" in the list and the `.txt` export, and are shown whatever the confidence filter is.
- **↻** is for a wrong track. It queues a short job that listens again around that timestamp, at offsets the first run did not use, and replaces the entry with whatever else Shazam hears there. If it mostly hears a neighbouring track that is already listed, the wrong entry is removed instead. The rejected track stays noted on the corrected entry and is not offered again. If nothing else is found the job fails with the reason and the entry stays as it was.

Both are stored in the archive, so they need `-db`. They survive a rescan: confirmed and retried entries are kept, and a rejected track does not return to its slot. A retry needs a proof of work and counts against the job limits like any submission, but it runs ahead of the full lookups waiting in the queue; confirming is open to every visitor.

Bot/spam protection is built in and needs no setup:

- every submission needs a proof of work (the browser solves it in the background; `-pow-bits`, default 18, `0` disables)
- a hidden honeypot field rejects form-filling bots
- per-client limits: `-max-active` (queued + running jobs, default 2), `-max-per-hour` (default 10), `-max-streams` (live-update connections, default 8)
- `-max-queue` caps the whole queue (default 30)
- only the client that submitted a job can cancel or remove it

Clients are told apart by IP address (IPv6 by /64). Behind a reverse proxy, the proxy must set `X-Forwarded-For` (or `X-Real-IP`), otherwise every visitor looks like the same client. These headers are only trusted on connections from localhost or from addresses listed in `-trusted-proxies` (env `TRUSTED_PROXIES`). With Docker, a proxy on the host reaches the container from the Docker network, e.g. `172.16.0.0/12`.
