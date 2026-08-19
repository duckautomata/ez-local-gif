package discordlint

import (
	"bytes"
	"compress/lzw"
	"errors"
	"fmt"
	"io"
)

// Pixel-index analysis. The fixer needs to know which palette indices a
// frame's pixel data actually uses (is the transparent index really used?
// is frame 0 entirely transparent? which index is free for frame 0?). The
// LZW stream is decoded with compress/lzw — the same decoder image/gif is
// built on — one frame at a time, so a damaged later frame does not stop
// frame 0 from being analysed. Pixel order (interlaced or not) is
// irrelevant for index usage, so no de-interlacing is done.

// maxAnalysedPixels caps the work spent per frame; larger frames are
// treated as "could not decode" (analysis then falls back to assuming the
// transparent index is used).
const maxAnalysedPixels = 64 << 20

// gifPixels holds the decoded index usage of one frame.
type gifPixels struct {
	used   [256]bool // index → appears in the pixel data
	pixels int       // number of decoded pixels (width*height)
}

// usesOnly reports whether every pixel is idx (an entirely transparent frame
// when idx is the transparent index). An empty frame counts as "only".
func (p *gifPixels) usesOnly(idx byte) bool {
	for i, u := range p.used {
		if u && i != int(idx) {
			return false
		}
	}
	return true
}

// decodeIndices decodes the LZW data of img and tallies index usage.
func decodeIndices(img *gifImage) (*gifPixels, error) {
	n := int(img.width) * int(img.height)
	if n == 0 {
		return &gifPixels{}, nil
	}
	if n > maxAnalysedPixels {
		return nil, fmt.Errorf("frame of %dx%d exceeds analysis cap", img.width, img.height)
	}
	if img.minCodeSize < 2 || img.minCodeSize > 8 {
		return nil, fmt.Errorf("LZW minimum code size %d out of range", img.minCodeSize)
	}
	joined := bytes.Join(img.data, nil)
	lr := lzw.NewReader(bytes.NewReader(joined), lzw.LSB, int(img.minCodeSize))
	defer lr.Close()

	p := &gifPixels{}
	buf := make([]byte, 32<<10)
	for p.pixels < n {
		want := min(len(buf), n-p.pixels)
		got, err := io.ReadFull(lr, buf[:want])
		for _, v := range buf[:got] {
			p.used[v] = true
		}
		p.pixels += got
		if err != nil {
			// giflib and image/gif both accept a stream that ends without an
			// explicit end-of-information code once every pixel has been
			// produced; anything short of a full frame is a decode failure.
			if p.pixels == n && (errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF)) {
				break
			}
			return nil, fmt.Errorf("LZW data: %w (%d of %d pixels)", err, p.pixels, n)
		}
	}
	return p, nil
}

// pixelCache memoises per-frame index analysis by frame index (image order
// never changes during fixing).
type pixelCache struct {
	res map[int]pixelResult
}

type pixelResult struct {
	pix *gifPixels
	err error
}

func (c *pixelCache) get(f *gifFrame) (*gifPixels, error) {
	if c.res == nil {
		c.res = make(map[int]pixelResult)
	}
	if r, ok := c.res[f.index]; ok {
		return r.pix, r.err
	}
	pix, err := decodeIndices(f.image)
	c.res[f.index] = pixelResult{pix, err}
	return pix, err
}

// usesIndex reports whether frame f's pixel data contains idx. known is
// false when the frame could not be decoded (used is then true: assume the
// worst).
func (c *pixelCache) usesIndex(f *gifFrame, idx byte) (used, known bool) {
	pix, err := c.get(f)
	if err != nil {
		return true, false
	}
	return pix.used[idx], true
}
