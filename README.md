# sctrackid

## Web interface

    sctrackid -serve localhost:8080

Bot/spam protection is built in and needs no setup:

- every submission needs a proof of work (the browser solves it in the background; `-pow-bits`, default 18, `0` disables)
- a hidden honeypot field rejects form-filling bots
- per-client limits: `-max-active` (queued + running jobs, default 2), `-max-per-hour` (default 10), `-max-streams` (live-update connections, default 8)
- `-max-queue` caps the whole queue (default 30)
- only the client that submitted a job can cancel or remove it

Clients are told apart by IP address (IPv6 by /64). Behind a reverse proxy on the same machine, the proxy must set `X-Forwarded-For` (or `X-Real-IP`), otherwise every visitor looks like the same client. These headers are only trusted on connections from localhost.
