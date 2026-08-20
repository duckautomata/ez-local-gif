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

func FuzzLintAPNG(f *testing.F) {
	for _, name := range []string{"ff_rgba.apng", "ff_plays1.apng", "ff_indexed.apng", "ff_still.png", "ff_still_opaque.png"} {
		f.Add(readFixture(f, name))
	}
	f.Add(goodSynthAPNG().bytes())
	hidden := goodSynthAPNG()
	hidden.hideDefault = true
	f.Add(hidden.bytes())
	f.Add(synthAPNG{w: 8, h: 8, colorType: pngIndexed, plte: 4, trns: 2}.bytes())
	f.Add(append([]byte(nil), pngSignature...))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, target := range []Target{TargetNone, TargetEmote, TargetSticker, TargetAttachment} {
			r, err := LintAPNG(data, target)
			if err != nil {
				continue
			}
			if r.Bytes != int64(len(data)) || len(r.Checks) == 0 || r.Frames < 0 || r.DurationMS < 0 || r.MinDelayMS < 0 {
				t.Fatalf("report: %+v", r)
			}
			if (r.Format != "apng" && r.Format != "png") || (r.Format == "png" && r.Frames != 1) {
				t.Fatalf("format/frames: %+v", r)
			}
			for _, c := range r.Checks {
				if c.Fixed {
					t.Fatalf("no fixer, yet %s reports Fixed", c.Rule)
				}
			}
			if r.OK != checkList(r.Checks).allOK() {
				t.Fatalf("OK disagrees with the checks: %+v", r)
			}
		}
	})
}

func FuzzLintStatic(f *testing.F) {
	for _, name := range []string{"ff_still.png", "ff_still.jpg", "ff_still.webp", "ff_still_alpha.webp", "ff_still_alpha.avif", "ff_still_opaque.avif", "ff_rgba.apng"} {
		f.Add(readFixture(f, name))
	}
	f.Add([]byte{0xFF, 0xD8, 0xFF, 0xC0, 0x00, 0x0B, 8, 0, 4, 0, 4, 1, 0xFF, 0xDA})
	f.Add([]byte("\x00\x00\x00\x0cftypavif"))
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, format := range []string{"png", "jpeg", "webp", "avif"} {
			for _, target := range []Target{TargetNone, TargetEmote, TargetSticker} {
				r, err := LintStatic(format, data, target)
				if err != nil {
					t.Fatalf("%s: unexpected error %v", format, err)
				}
				if r.Bytes != int64(len(data)) || r.Format != format || r.Frames != 1 || !r.LoopForever || len(r.Checks) == 0 || r.Width < 0 || r.Height < 0 {
					t.Fatalf("%s: report %+v", format, r)
				}
			}
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
