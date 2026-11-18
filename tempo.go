package main

// Downtempo mode: a set whose tracks were pitched down (vinyl style, pitch
// and tempo together) is out of Shazam's reach, which tolerates only about 2%.
// Each window's tempo is measured, and the window is sped up to the tempo the
// user names before it is fingerprinted, which restores pitch and tempo alike.

import (
	"math"
	"math/cmplx"
	"sort"
)

const (
	onsetFFT  = 1024
	onsetHop  = 128
	onsetRate = float64(sampleRate) / onsetHop // onset envelope frames per second
	minBPM    = 60.0
	maxBPM    = 180.0
	weakBeat  = 1.5 // comb peak over the median score below which a tempo is a guess
)

// estimateTempo measures the tempo of 16 kHz mono audio from the
// autocorrelation of its spectral flux, scored over several beats so the
// result is precise to a fraction of a percent. strength is how far the best
// tempo stands out (1 = no beat at all).
func estimateTempo(samples []int16) (bpm, strength float64) {
	env := onsetEnvelope(samples)
	if len(env) < int(8*onsetRate) {
		return 0, 0
	}
	// unbiased autocorrelation
	n := len(env)
	maxLag := n / 2
	ac := make([]float64, maxLag+2)
	for l := range ac {
		var s float64
		for i := 0; i+l < n; i++ {
			s += env[i] * env[i+l]
		}
		ac[l] = s / float64(n-l)
	}
	at := func(lag float64) float64 {
		i := int(lag)
		if i+1 >= len(ac) {
			return 0
		}
		fr := lag - float64(i)
		return ac[i]*(1-fr) + ac[i+1]*fr
	}

	var scores []float64
	best, bestScore := 0.0, math.Inf(-1)
	for t := minBPM; t <= maxBPM; t += 0.05 {
		beat := 60 * onsetRate / t
		var s float64
		k := 0
		for ; k < 8 && float64(k+1)*beat <= float64(maxLag); k++ {
			s += at(float64(k+1) * beat)
		}
		if k < 3 {
			continue
		}
		s /= float64(k)
		scores = append(scores, s)
		if s > bestScore {
			best, bestScore = t, s
		}
	}
	if len(scores) == 0 {
		return 0, 0
	}
	sort.Float64s(scores)
	med := scores[len(scores)/2]
	if med <= 0 {
		med = 1e-12
	}
	if bestScore <= 0 {
		return best, 1
	}
	return best, bestScore / med
}

// onsetEnvelope is the positive spectral flux of log magnitudes, with its
// local mean removed so only the attacks remain.
func onsetEnvelope(samples []int16) []float64 {
	frames := (len(samples) - onsetFFT) / onsetHop
	if frames < 2 {
		return nil
	}
	var win [onsetFFT]float64
	for i := range win {
		win[i] = 0.5 - 0.5*math.Cos(2*math.Pi*float64(i)/onsetFFT)
	}
	buf := make([]complex128, onsetFFT)
	prev := make([]float64, onsetFFT/2)
	cur := make([]float64, onsetFFT/2)
	flux := make([]float64, frames)
	for f := 0; f < frames; f++ {
		off := f * onsetHop
		for i := range buf {
			buf[i] = complex(float64(samples[off+i])/32768*win[i], 0)
		}
		fftRadix2(buf)
		var s float64
		for k := 1; k < onsetFFT/2; k++ {
			cur[k] = math.Log1p(100 * cmplx.Abs(buf[k]))
			if f > 0 {
				s += max(0, cur[k]-prev[k])
			}
		}
		flux[f] = s
		prev, cur = cur, prev
	}
	// subtract a moving average (~0.4 s) and keep what sticks out
	const half = 25
	out := make([]float64, frames)
	var sum float64
	lo, hi := 0, 0
	for f := range flux {
		for hi < frames && hi <= f+half {
			sum += flux[hi]
			hi++
		}
		for lo < f-half {
			sum -= flux[lo]
			lo++
		}
		out[f] = max(0, flux[f]-sum/float64(hi-lo))
	}
	return out
}

// fftRadix2 is an in-place FFT for power-of-two lengths.
func fftRadix2(a []complex128) {
	n := len(a)
	for i, j := 1, 0; i < n; i++ {
		bit := n >> 1
		for ; j&bit != 0; bit >>= 1 {
			j ^= bit
		}
		j |= bit
		if i < j {
			a[i], a[j] = a[j], a[i]
		}
	}
	for size := 2; size <= n; size <<= 1 {
		w := cmplx.Exp(complex(0, -2*math.Pi/float64(size)))
		for start := 0; start < n; start += size {
			tw := complex(1, 0)
			for k := 0; k < size/2; k++ {
				t := tw * a[start+k+size/2]
				a[start+k+size/2] = a[start+k] - t
				a[start+k] += t
				tw *= w
			}
		}
	}
}

// speedUp plays samples f times faster (pitch rises with the tempo), with a
// windowed-sinc low-pass so nothing folds back below the Nyquist frequency.
// At most maxOut samples are produced.
func speedUp(x []int16, f float64, maxOut int) []int16 {
	n := min(maxOut, int(float64(len(x))/f))
	if n <= 0 {
		return nil
	}
	fc := min(1, 1/f)                // cutoff relative to the input's Nyquist frequency
	halfW := int(math.Ceil(16 / fc)) // taps on each side
	const over = 256                 // kernel table steps per input sample
	kernel := make([]float64, halfW*over+2)
	for i := range kernel {
		d := float64(i) / over
		v := fc
		if d > 0 {
			v = math.Sin(math.Pi*fc*d) / (math.Pi * d)
		}
		kernel[i] = v * (0.5 + 0.5*math.Cos(math.Pi*d/float64(halfW)))
	}
	kern := func(d float64) float64 {
		p := math.Abs(d) * over
		i := int(p)
		if i+1 >= len(kernel) {
			return 0
		}
		fr := p - float64(i)
		return kernel[i]*(1-fr) + kernel[i+1]*fr
	}
	out := make([]int16, n)
	for j := range out {
		t := float64(j) * f
		c := int(t)
		var s float64
		for k := max(0, c-halfW+1); k <= c+halfW && k < len(x); k++ {
			s += float64(x[k]) * kern(t-float64(k))
		}
		out[j] = int16(max(-32768, min(32767, math.Round(s))))
	}
	return out
}

// tempoTracker follows the set's tempo from window to window. A window with no
// clear beat (a breakdown, an ambient stretch) or a sudden jump uses the
// tempo of the windows before it.
type tempoTracker struct {
	target float64
	recent []float64
}

func (tt *tempoTracker) median() float64 {
	if len(tt.recent) == 0 {
		return 0
	}
	s := append([]float64(nil), tt.recent...)
	sort.Float64s(s)
	return s[len(s)/2]
}

// fold moves a tempo by octaves so the speed-up lies in [0.8, 1.6): a beat
// tracker easily locks on to double or half the tempo.
func (tt *tempoTracker) fold(bpm float64) float64 {
	for bpm > 0 && tt.target/bpm < 0.8 {
		bpm /= 2
	}
	for bpm > 0 && tt.target/bpm >= 1.6 {
		bpm *= 2
	}
	return bpm
}

// window returns the set tempo to assume for one window.
func (tt *tempoTracker) window(samples []int16) float64 {
	bpm, strength := estimateTempo(samples)
	bpm = tt.fold(bpm)
	med := tt.median()
	if bpm <= 0 || strength < weakBeat {
		if med > 0 {
			return med
		}
		return bpm // nothing better known yet
	}
	tt.recent = append(tt.recent, bpm)
	if len(tt.recent) > 5 {
		tt.recent = tt.recent[1:]
	}
	if len(tt.recent) >= 3 && math.Abs(bpm/med-1) > 0.06 {
		return med // a lone jump is more likely a misread than the DJ
	}
	return bpm
}

// prepare turns a decoded window into the audio Shazam gets: unchanged
// normally, sped up to the target tempo in downtempo mode. It returns the
// samples, the speed-up applied (1 = none) and the measured set tempo.
func (tt *tempoTracker) prepare(samples []int16, sampleLen int) ([]int16, float64, float64) {
	maxOut := sampleLen * sampleRate
	if tt == nil || tt.target <= 0 {
		return samples[:min(len(samples), maxOut)], 1, 0
	}
	bpm := tt.window(samples)
	if bpm <= 0 {
		return samples[:min(len(samples), maxOut)], 1, 0
	}
	f := tt.target / bpm
	return speedUp(samples, f, maxOut), f, bpm
}

// readLen is how many seconds of the set each window takes. In downtempo mode
// it is longer, so that once sped up there is still a full sample's worth.
func readLen(o options) int {
	if o.downtempo <= 0 {
		return o.sampleLen
	}
	return max(o.sampleLen, min(o.interval/2, o.sampleLen*2))
}

// logTempo reports the tempo found across the set and the speed-up applied.
func logTempo(got []sample, target float64) {
	var bpms []float64
	for _, s := range got {
		if s.bpm > 0 {
			bpms = append(bpms, s.bpm)
		}
	}
	if len(bpms) == 0 {
		if len(got) > 0 && got[0].sig != nil {
			logf("! downtempo: no beat found anywhere, samples were sent unchanged")
		}
		return
	}
	sort.Float64s(bpms)
	med := bpms[len(bpms)/2]
	logf("· downtempo: set tempo %.1f–%.1f BPM (median %.1f), samples sped up to %g BPM (×%.2f)",
		bpms[0], bpms[len(bpms)-1], med, target, target/med)
}
