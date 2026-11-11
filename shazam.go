package main

// Audio fingerprinting compatible with Shazam's public (unofficial, keyless)
// recognition endpoint. The signature algorithm is a port of the one used by
// the open-source SongRec project (github.com/marin-m/SongRec).

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"hash/crc32"
	"io"
	"math"
	"math/cmplx"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const sampleRate = 16000

type freqPeak struct {
	pass uint32
	mag  uint16
	bin  uint16
}

var hanning [2048]float64

func init() {
	for i := range hanning {
		hanning[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i+1)/2049)
	}
}

type sigGen struct {
	ring          [2048]int16
	ringIdx       int
	fftOut        [256][1025]float32
	fftIdx        int
	spread        [256][1025]float32
	spreadIdx     int
	spreadWritten int
	peaks         [4][]freqPeak
	buf           [2048]complex128
}

// MakeSignature builds a Shazam signature (binary) from 16 kHz mono samples,
// and reports how many spectral peaks it contains (near-silence has almost none).
func MakeSignature(samples []int16) ([]byte, int) {
	g := &sigGen{}
	for i := 0; i+128 <= len(samples); i += 128 {
		g.doFFT(samples[i : i+128])
		g.doSpread()
		if g.spreadWritten >= 46 {
			g.doPeaks()
		}
	}
	peaks := 0
	for _, p := range g.peaks {
		peaks += len(p)
	}
	return g.encode(len(samples)), peaks
}

func (g *sigGen) doFFT(chunk []int16) {
	copy(g.ring[g.ringIdx:g.ringIdx+128], chunk)
	g.ringIdx = (g.ringIdx + 128) & 2047
	for i := 0; i < 2048; i++ {
		g.buf[i] = complex(float64(g.ring[(i+g.ringIdx)&2047])*hanning[i], 0)
	}
	fft2048(g.buf[:])
	out := &g.fftOut[g.fftIdx]
	for i := 0; i <= 1024; i++ {
		v := real(g.buf[i])*real(g.buf[i]) + imag(g.buf[i])*imag(g.buf[i])
		out[i] = float32(math.Max(v/(1<<17), 1e-10))
	}
	g.fftIdx = (g.fftIdx + 1) & 255
}

func (g *sigGen) doSpread() {
	s := g.fftOut[(g.fftIdx-1)&255] // array copy
	for p := 0; p < 1023; p++ {
		s[p] = max(s[p], s[p+1], s[p+2])
	}
	for p := 0; p < 1025; p++ {
		mx := s[p]
		for _, f := range [...]int{1, 3, 6} {
			fo := &g.spread[(g.spreadIdx-f)&255]
			mx = max(mx, fo[p])
			fo[p] = mx
		}
	}
	g.spread[g.spreadIdx] = s
	g.spreadIdx = (g.spreadIdx + 1) & 255
	g.spreadWritten++
}

var (
	freqNeighbors = [...]int{-10, -7, -4, -3, 1, 2, 5, 8}
	timeNeighbors = [...]int{-53, -45, 165, 172, 179, 186, 193, 200, 214, 221, 228, 235, 242, 249}
)

func (g *sigGen) doPeaks() {
	f46 := &g.fftOut[(g.fftIdx-46)&255]
	f49 := &g.spread[(g.spreadIdx-49)&255]
	logMag := func(v float32) float64 { return math.Log(math.Max(1.0/64, float64(v)))*1477.3 + 6144 }

	for bin := 10; bin < 1015; bin++ {
		v := f46[bin]
		if v < 1.0/64 || v < f49[bin-1] {
			continue
		}
		var mx float32
		for _, o := range freqNeighbors {
			mx = max(mx, f49[bin+o])
		}
		if v <= mx {
			continue
		}
		for _, o := range timeNeighbors {
			mx = max(mx, g.spread[(g.spreadIdx+o)&255][bin-1])
		}
		if v <= mx {
			continue
		}
		mag := logMag(v)
		before, after := logMag(f46[bin-1]), logMag(f46[bin+1])
		var1 := mag*2 - before - after
		var2 := 0.0
		if var1 != 0 {
			var2 = (after - before) * 32 / var1
		}
		corrBin := float64(bin)*64 + var2
		hz := corrBin * (sampleRate / 2.0 / 1024 / 64)
		var band int
		switch {
		case hz >= 250 && hz < 520:
			band = 0
		case hz >= 520 && hz < 1450:
			band = 1
		case hz >= 1450 && hz < 3500:
			band = 2
		case hz >= 3500 && hz <= 5500:
			band = 3
		default:
			continue
		}
		g.peaks[band] = append(g.peaks[band], freqPeak{
			pass: uint32(g.spreadWritten - 46),
			mag:  uint16(mag),
			bin:  uint16(corrBin),
		})
	}
}

func (g *sigGen) encode(numSamples int) []byte {
	var b bytes.Buffer
	w := func(v any) { _ = binary.Write(&b, binary.LittleEndian, v) }
	w(uint32(0xcafe2580)) // magic1
	w(uint32(0))          // crc32 (filled below)
	w(uint32(0))          // size minus header (filled below)
	w(uint32(0x94119c00)) // magic2
	w([3]uint32{})
	w(uint32(3 << 27)) // sample rate id for 16 kHz
	w([2]uint32{})
	w(uint32(numSamples + int(sampleRate*0.24)))
	w(uint32((15 << 19) + 0x40000))
	w(uint32(0x40000000))
	w(uint32(0)) // size minus header again (filled below)

	for band, peaks := range g.peaks {
		if len(peaks) == 0 {
			continue
		}
		var pb bytes.Buffer
		pw := func(v any) { _ = binary.Write(&pb, binary.LittleEndian, v) }
		var last uint32
		for _, p := range peaks {
			if p.pass-last >= 255 {
				pw(uint8(0xff))
				pw(p.pass)
				last = p.pass
			}
			pw(uint8(p.pass - last))
			pw(p.mag)
			pw(p.bin)
			last = p.pass
		}
		w(uint32(0x60030040 + band))
		w(uint32(pb.Len()))
		b.Write(pb.Bytes())
		for i := 0; i < (4-pb.Len()%4)%4; i++ {
			b.WriteByte(0)
		}
	}
	out := b.Bytes()
	size := uint32(len(out) - 48)
	binary.LittleEndian.PutUint32(out[8:], size)
	binary.LittleEndian.PutUint32(out[52:], size)
	binary.LittleEndian.PutUint32(out[4:], crc32.ChecksumIEEE(out[8:]))
	return out
}

// --- FFT (iterative radix-2, size 2048) ---

var (
	fftTwiddle [1024]complex128
	fftRev     [2048]int
)

func init() {
	for i := range fftTwiddle {
		fftTwiddle[i] = cmplx.Exp(complex(0, -2*math.Pi*float64(i)/2048))
	}
	for i := range fftRev {
		r := 0
		for b := 0; b < 11; b++ {
			if i&(1<<b) != 0 {
				r |= 1 << (10 - b)
			}
		}
		fftRev[i] = r
	}
}

func fft2048(a []complex128) {
	for i, r := range fftRev {
		if i < r {
			a[i], a[r] = a[r], a[i]
		}
	}
	for size := 2; size <= 2048; size <<= 1 {
		half, step := size/2, 2048/size
		for start := 0; start < 2048; start += size {
			for k := 0; k < half; k++ {
				t := fftTwiddle[k*step] * a[start+k+half]
				a[start+k+half] = a[start+k] - t
				a[start+k] += t
			}
		}
	}
}

// --- Recognition request ---

type ShazamTrack struct {
	Key      string `json:"key"`
	Title    string `json:"title"`
	Subtitle string `json:"subtitle"` // artist
	URL      string `json:"url"`
}

func uuid4() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	return strings.ToUpper(fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:]))
}

type rateLimitedError struct{ RetryAfter time.Duration }

func (rateLimitedError) Error() string { return "shazam rate limited (HTTP 429)" }

// Recognize sends a signature to Shazam. Returns (nil, nil) when nothing matched.
func Recognize(ctx context.Context, client *http.Client, sig []byte, numSamples int) (*ShazamTrack, error) {
	now := time.Now().UnixMilli()
	payload := map[string]any{
		"geolocation": map[string]any{"altitude": 300, "latitude": 45, "longitude": 2},
		"signature": map[string]any{
			"samplems":  numSamples * 1000 / sampleRate,
			"timestamp": now,
			"uri":       "data:audio/vnd.shazam.sig;base64," + base64.StdEncoding.EncodeToString(sig),
		},
		"timestamp": now,
		"timezone":  "Europe/Paris",
	}
	body, _ := json.Marshal(payload)
	u := fmt.Sprintf("https://amp.shazam.com/discovery/v5/en/US/android/-/tag/%s/%s?sync=true&webv3=true&sampling=true&connected=&shazamapiversion=v3&sharehub=true&video=v3", uuid4(), uuid4())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Language", "en_US")
	req.Header.Set("User-Agent", "Dalvik/2.1.0 (Linux; U; Android 5.0.2; VS980 4G Build/LRX22G)")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == http.StatusTooManyRequests {
		var ra time.Duration
		if n, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && n > 0 {
			ra = time.Duration(n) * time.Second
		}
		return nil, rateLimitedError{RetryAfter: ra}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("shazam HTTP %d", resp.StatusCode)
	}
	var out struct {
		Track *ShazamTrack `json:"track"`
	}
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, err
	}
	return out.Track, nil
}
