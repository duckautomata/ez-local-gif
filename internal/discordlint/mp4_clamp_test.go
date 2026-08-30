package discordlint

import (
	"encoding/binary"
	"math"
	"testing"
)

// The clamp ceilings must fit an int on every platform (GOARCH=386/arm have
// 32-bit ints), so both clampInt31 and the mp4STTSFrames cap top out at
// math.MaxInt32 — never 1<<31, which overflows a 32-bit int at compile time.
func TestClampInt31(t *testing.T) {
	for _, tc := range []struct {
		in   int64
		want int
	}{
		{-1, 0},
		{0, 0},
		{12345, 12345},
		{math.MaxInt32, math.MaxInt32},
		{math.MaxInt32 + 1, math.MaxInt32},
		{math.MaxInt64, math.MaxInt32},
	} {
		if got := clampInt31(tc.in); got != tc.want {
			t.Errorf("clampInt31(%d) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// sttsBox builds an stts isoBox: version/flags, entry_count, then
// (sample_count, sample_delta) pairs.
func sttsBox(counts ...uint32) *isoBox {
	p := make([]byte, 8+8*len(counts))
	binary.BigEndian.PutUint32(p[4:], uint32(len(counts)))
	for i, c := range counts {
		binary.BigEndian.PutUint32(p[8+8*i:], c)
	}
	return &isoBox{typ: "stts", payload: p}
}

func TestMP4STTSFramesClamp(t *testing.T) {
	for _, tc := range []struct {
		name   string
		counts []uint32
		want   int
	}{
		{"sum", []uint32{2, 3}, 5},
		{"at cap", []uint32{math.MaxInt32}, math.MaxInt32},
		{"single overflow", []uint32{math.MaxUint32}, math.MaxInt32},
		{"cumulative overflow", []uint32{math.MaxInt32, 1}, math.MaxInt32},
	} {
		if got := mp4STTSFrames(sttsBox(tc.counts...)); got != tc.want {
			t.Errorf("%s: mp4STTSFrames = %d, want %d", tc.name, got, tc.want)
		}
	}
	if got := mp4STTSFrames(nil); got != 0 {
		t.Errorf("mp4STTSFrames(nil) = %d, want 0", got)
	}
}
