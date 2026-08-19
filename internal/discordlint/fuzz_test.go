package discordlint

import (
	"bytes"
	"testing"
)

// The fuzz targets guard the "no panics on bad input" promise. `go test`
// runs only the seed corpus; `go test -fuzz=FuzzLintGIF -fuzztime=30s`
// explores mutations.

func FuzzLintGIF(f *testing.F) {
	for _, name := range []string{"ff_alpha.gif", "ff_opaque.gif", "ff_transdiff.gif"} {
		f.Add(readFixture(f, name))
	}
	f.Add(encodeFx(f, opaqueFrame0Anim()))
	f.Add(encodeFx(f, alphaAnim()))
	f.Add([]byte("GIF89a"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, target := range []Target{TargetNone, TargetEmote, TargetSticker} {
			r, out, err := LintGIF(data, target, true)
			if err != nil {
				continue
			}
			if int64(len(out)) != r.Bytes {
				t.Fatalf("Bytes %d != len(out) %d", r.Bytes, len(out))
			}
			// Whatever the fixer produced must parse again and lint without
			// error, and a second fix pass must be a no-op.
			r2, out2, err := LintGIF(out, target, true)
			if err != nil {
				t.Fatalf("fixed bytes do not parse: %v", err)
			}
			if !bytes.Equal(out2, out) {
				t.Fatalf("second fix pass changed the bytes")
			}
			for _, c := range r2.Checks {
				if c.Fixed {
					t.Fatalf("second pass still fixed %s: %s", c.Rule, c.Detail)
				}
			}
			LintGIF(data, target, false)
		}
	})
}

func FuzzLintWebP(f *testing.F) {
	for _, name := range []string{"ff_lossy_alpha.webp", "ff_lossless_alpha.webp", "ff_still.webp", "ff_still_alpha.webp", "ff_loop1.webp"} {
		f.Add(readFixture(f, name))
	}
	f.Add(goodAnim())
	f.Add([]byte("RIFF\x04\x00\x00\x00WEBP"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, target := range []Target{TargetNone, TargetEmote, TargetSticker} {
			r, err := LintWebP(data, target)
			if err != nil {
				continue
			}
			if r.Bytes != int64(len(data)) || len(r.Checks) == 0 {
				t.Fatalf("report: %+v", r)
			}
		}
	})
}
