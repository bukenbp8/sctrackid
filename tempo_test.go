package main

import (
	"math"
	"math/rand"
	"testing"
)

// beats makes a kick-and-hat pattern at bpm with some noise.
func beats(bpm float64, secs int) []int16 {
	n := secs * sampleRate
	out := make([]float64, n)
	rng := rand.New(rand.NewSource(1))
	beat := 60 / bpm * sampleRate
	for b := 0.0; b < float64(n); b += beat / 2 {
		start := int(b)
		kick := int(b/(beat/2))%2 == 0
		for i := 0; i < 3000 && start+i < n; i++ {
			t := float64(i) / sampleRate
			if kick {
				out[start+i] += 12000 * math.Exp(-t*25) * math.Sin(2*math.Pi*(55+120*math.Exp(-t*40))*t)
			} else {
				out[start+i] += 3000 * math.Exp(-t*60) * (rng.Float64()*2 - 1)
			}
		}
	}
	s := make([]int16, n)
	for i, v := range out {
		s[i] = int16(max(-32768, min(32767, v+400*(rng.Float64()*2-1))))
	}
	return s
}

func TestEstimateTempo(t *testing.T) {
	for _, bpm := range []float64{72, 90, 97.5, 118, 124, 140} {
		got, strength := estimateTempo(beats(bpm, 15))
		tt := &tempoTracker{target: bpm * 1.3}
		got = tt.fold(got)
		if math.Abs(got/bpm-1) > 0.005 || strength < weakBeat {
			t.Errorf("%.1f BPM: got %.2f (strength %.2f)", bpm, got, strength)
		}
	}
	if _, strength := estimateTempo(make([]int16, 15*sampleRate)); strength >= weakBeat {
		t.Errorf("silence has a beat of strength %.2f", strength)
	}
}

func TestSpeedUp(t *testing.T) {
	in := make([]int16, 4*sampleRate)
	for i := range in {
		in[i] = int16(10000 * math.Sin(2*math.Pi*750*float64(i)/sampleRate))
	}
	out := speedUp(in, 4.0/3, 10*sampleRate)
	if want := 3 * sampleRate; len(out) != want {
		t.Fatalf("length %d, want %d", len(out), want)
	}
	crossings := 0
	for i := 1; i < len(out); i++ {
		if (out[i-1] < 0) != (out[i] < 0) {
			crossings++
		}
	}
	if hz := float64(crossings) / 2 / 3; math.Abs(hz-1000) > 5 {
		t.Errorf("750 Hz sped up by 4/3 is %.1f Hz, want 1000", hz)
	}
	if got := speedUp(in, 1.5, sampleRate); len(got) != sampleRate {
		t.Errorf("maxOut not respected: %d", len(got))
	}
}

func TestTrackerFallsBackOnWeakWindows(t *testing.T) {
	tt := &tempoTracker{target: 120}
	for i := 0; i < 3; i++ {
		if got := tt.window(beats(90, 15)); math.Abs(got-90) > 0.5 {
			t.Fatalf("window %d: %.2f", i, got)
		}
	}
	if got := tt.window(make([]int16, 15*sampleRate)); math.Abs(got-90) > 0.5 {
		t.Errorf("silent window: %.2f, want the running 90", got)
	}
	_, f, _ := tt.prepare(beats(90, 15), 12)
	if math.Abs(f-120.0/90) > 0.01 {
		t.Errorf("speed-up %.3f, want %.3f", f, 120.0/90)
	}
}

func TestStartInWithSpeed(t *testing.T) {
	// set at 90, original at 120: 60 s into the recording is 80 s into the set
	tr := &ShazamTrack{Matched: true, Offset: 60, Speed: 120.0 / 90}
	if got, ok := tr.startIn(500); !ok || got != 420 {
		t.Errorf("start %d, want 420", got)
	}
}

func TestCoversDowntempo(t *testing.T) {
	plain := scanOpts{MinConf: "low"}
	dt := scanOpts{MinConf: "low", Downtempo: 120}
	if plain.covers(dt) || dt.covers(plain) {
		t.Error("runs at different tempos must not stand in for each other")
	}
	if !dt.covers(dt) {
		t.Error("same tempo must cover")
	}
	if !dt.covers(scanOpts{MinConf: "low", NoAudio: true}) {
		t.Error("an audio run covers a no-audio request")
	}
}
