package main

// Bot/spam protection for the web interface. Everything runs in-process,
// with no third-party CAPTCHA:
//   - proof of work: a browser must find a nonce for a server-signed,
//     single-use challenge before it can submit a job (costs a second or two
//     of CPU per submission, which makes scripted flooding expensive)
//   - a honeypot form field that real users never see or fill in
//   - per-client limits on active jobs, submissions per hour and live event
//     streams, plus a global cap on the queue length
//   - only the client that submitted a job can cancel or remove it

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"math/bits"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

type guardConfig struct {
	powBits         int // leading zero bits required in the proof of work; 0 disables it
	maxActivePerIP  int // queued + running jobs per client; 0 = unlimited
	maxPerHourPerIP int // job submissions per client per hour; 0 = unlimited
	maxQueue        int // queued jobs in total; 0 = unlimited
	maxStreamsPerIP int // concurrent live-update connections per client; 0 = unlimited
}

const challengeTTL = 10 * time.Minute

type guard struct {
	cfg guardConfig
	key []byte

	mu      sync.Mutex
	used    map[string]time.Time   // spent challenges -> their expiry
	submits map[string][]time.Time // client -> recent submission times
	streams map[string]int         // client -> open event streams
}

func newGuard(ctx context.Context, cfg guardConfig) *guard {
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		panic(err)
	}
	g := &guard{
		cfg:     cfg,
		key:     key,
		used:    map[string]time.Time{},
		submits: map[string][]time.Time{},
		streams: map[string]int{},
	}
	go g.janitor(ctx)
	return g
}

func (g *guard) janitor(ctx context.Context) {
	t := time.NewTicker(5 * time.Minute)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		now := time.Now()
		g.mu.Lock()
		for c, exp := range g.used {
			if now.After(exp) {
				delete(g.used, c)
			}
		}
		for ip := range g.submits {
			g.recentLocked(ip, now)
		}
		g.mu.Unlock()
	}
}

// clientKey identifies the client behind a request. X-Forwarded-For /
// X-Real-IP are only honoured when the connection comes from loopback, i.e. a
// reverse proxy on the same machine; anyone else could forge them. IPv6
// clients are grouped by /64 because devices rotate addresses within it.
func clientKey(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	if ip != nil && ip.IsLoopback() {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			if p := net.ParseIP(strings.TrimSpace(parts[len(parts)-1])); p != nil {
				ip = p
			}
		} else if p := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); p != nil {
			ip = p
		}
	}
	if ip == nil {
		return host
	}
	if ip.To4() == nil {
		return ip.Mask(net.CIDRMask(64, 128)).String() + "/64"
	}
	return ip.String()
}

// --- proof of work ---

func (g *guard) sign(payload string) string {
	m := hmac.New(sha256.New, g.key)
	m.Write([]byte(payload))
	return hex.EncodeToString(m.Sum(nil))[:32]
}

// newChallenge returns "<expiry>.<random>.<mac>".
func (g *guard) newChallenge() string {
	r := make([]byte, 12)
	_, _ = rand.Read(r)
	payload := strconv.FormatInt(time.Now().Add(challengeTTL).Unix(), 10) + "." + hex.EncodeToString(r)
	return payload + "." + g.sign(payload)
}

// checkPoW verifies that sha256(challenge + ":" + nonce) starts with
// powBits zero bits, that the challenge is genuine and unexpired, and spends it.
func (g *guard) checkPoW(challenge, nonce string) string {
	if g.cfg.powBits <= 0 {
		return ""
	}
	parts := strings.Split(challenge, ".")
	if len(parts) != 3 || len(nonce) == 0 || len(nonce) > 32 {
		return "missing or malformed proof of work — reload the page"
	}
	payload := parts[0] + "." + parts[1]
	if !hmac.Equal([]byte(parts[2]), []byte(g.sign(payload))) {
		return "invalid proof of work — reload the page"
	}
	exp, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil || time.Now().Unix() > exp {
		return "proof of work expired — try again"
	}
	sum := sha256.Sum256([]byte(challenge + ":" + nonce))
	if leadingZeroBits(sum[:]) < g.cfg.powBits {
		return "proof of work does not check out"
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if _, spent := g.used[challenge]; spent {
		return "proof of work already used — try again"
	}
	g.used[challenge] = time.Unix(exp, 0)
	return ""
}

func leadingZeroBits(b []byte) int {
	n := 0
	for _, c := range b {
		if c != 0 {
			return n + bits.LeadingZeros8(c)
		}
		n += 8
	}
	return n
}

func (g *guard) handleChallenge(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, map[string]any{"challenge": g.newChallenge(), "bits": g.cfg.powBits})
}

// --- rate limits ---

func (g *guard) recentLocked(ip string, now time.Time) []time.Time {
	ts := g.submits[ip]
	i := 0
	for i < len(ts) && now.Sub(ts[i]) > time.Hour {
		i++
	}
	ts = ts[i:]
	if len(ts) == 0 {
		delete(g.submits, ip)
		return nil
	}
	g.submits[ip] = ts
	return ts
}

// allowSubmit checks the hourly limit and, if allowed, records the submission.
func (g *guard) allowSubmit(ip string) (ok bool, retry time.Duration) {
	if g.cfg.maxPerHourPerIP <= 0 {
		return true, 0
	}
	now := time.Now()
	g.mu.Lock()
	defer g.mu.Unlock()
	ts := g.recentLocked(ip, now)
	if len(ts) >= g.cfg.maxPerHourPerIP {
		return false, ts[0].Add(time.Hour).Sub(now)
	}
	g.submits[ip] = append(ts, now)
	return true, 0
}

func (g *guard) openStream(ip string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cfg.maxStreamsPerIP > 0 && g.streams[ip] >= g.cfg.maxStreamsPerIP {
		return false
	}
	g.streams[ip]++
	return true
}

func (g *guard) closeStream(ip string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.streams[ip]--; g.streams[ip] <= 0 {
		delete(g.streams, ip)
	}
}
