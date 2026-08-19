package ffrun

import (
	"bufio"
	"bytes"
	"io"
	"strconv"
	"strings"
)

// maxProgressLine bounds a single unterminated line held by progressWriter
// so a misbehaving producer cannot grow memory without bound.
const maxProgressLine = 64 << 10

// ParseProgress reads `ffmpeg -progress` output (key=value lines, one block
// per stats period terminated by "progress=continue" or "progress=end") from
// r and calls fn once per block with the running state. It returns r's read
// error, if any. Exposed for callers that drive ffmpeg themselves; RunFFmpeg
// uses the same parser.
func ParseProgress(r io.Reader, fn func(Progress)) error {
	p := &progressParser{emit: fn}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 4096), maxProgressLine)
	for sc.Scan() {
		p.line(sc.Text())
	}
	return sc.Err()
}

// progressParser turns key=value lines into Progress blocks. Fields carry
// over between blocks (ffmpeg re-prints every key each block, but a value
// of "N/A" leaves the previous number in place).
type progressParser struct {
	cur  Progress
	emit func(Progress)

	// per-block time candidates, resolved when the block ends
	us, ms      int64
	hasUS       bool
	hasMS       bool
	outTime     string
	pendingTime bool
}

// line consumes one line of progress output.
func (p *progressParser) line(s string) {
	key, val, ok := strings.Cut(s, "=")
	if !ok {
		return
	}
	key = strings.TrimSpace(key)
	val = strings.TrimSpace(val)
	switch key {
	case "frame":
		if n, err := strconv.Atoi(val); err == nil && n >= 0 {
			p.cur.Frame = n
		}
	case "fps":
		if f, err := strconv.ParseFloat(val, 64); err == nil && f >= 0 {
			p.cur.FPS = f
		}
	case "out_time_us":
		if n, err := strconv.ParseInt(val, 10, 64); err == nil && n >= 0 {
			p.us, p.hasUS, p.pendingTime = n, true, true
		}
	case "out_time_ms":
		if n, err := strconv.ParseInt(val, 10, 64); err == nil && n >= 0 {
			p.ms, p.hasMS, p.pendingTime = n, true, true
		}
	case "out_time":
		if val != "" && val != "N/A" {
			p.outTime, p.pendingTime = val, true
		}
	case "speed":
		if val != "" {
			p.cur.Speed = val
		}
	case "progress":
		p.resolveTime()
		p.cur.Done = val == "end"
		if p.emit != nil {
			p.emit(p.cur)
		}
	}
}

// resolveTime picks the output timestamp for the block that just ended.
// out_time_us is authoritative. Historically ffmpeg's out_time_ms has been
// microseconds despite its name (kept for compatibility when out_time_us
// was added), so it is treated as microseconds unless the human-readable
// out_time in the same block proves it really is milliseconds.
func (p *progressParser) resolveTime() {
	defer func() {
		p.hasUS, p.hasMS, p.outTime, p.pendingTime = false, false, "", false
	}()
	if !p.pendingTime {
		return
	}
	switch {
	case p.hasUS:
		p.cur.OutTimeMS = p.us / 1000
	case p.hasMS:
		if ref, ok := parseOutTime(p.outTime); ok && ref > 0 && looksLikeMillis(p.ms, ref) {
			p.cur.OutTimeMS = p.ms
			return
		}
		p.cur.OutTimeMS = p.ms / 1000
	default:
		if ref, ok := parseOutTime(p.outTime); ok {
			p.cur.OutTimeMS = ref / 1000
		}
	}
}

// looksLikeMillis reports whether ms agrees with the reference timestamp
// (in microseconds) when read as milliseconds rather than microseconds.
func looksLikeMillis(ms, refUS int64) bool {
	asMillis := absDiff(ms*1000, refUS)
	asMicros := absDiff(ms, refUS)
	return asMillis < asMicros
}

func absDiff(a, b int64) int64 {
	if a > b {
		return a - b
	}
	return b - a
}

// parseOutTime parses ffmpeg's out_time value ("HH:MM:SS.micro") into
// microseconds. Negative or malformed values return ok == false.
func parseOutTime(s string) (us int64, ok bool) {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "-") {
		return 0, false
	}
	parts := strings.Split(s, ":")
	if len(parts) != 3 {
		return 0, false
	}
	h, err1 := strconv.ParseInt(parts[0], 10, 64)
	m, err2 := strconv.ParseInt(parts[1], 10, 64)
	sec, err3 := strconv.ParseFloat(parts[2], 64)
	if err1 != nil || err2 != nil || err3 != nil || h < 0 || m < 0 || sec < 0 {
		return 0, false
	}
	total := float64(h*3600+m*60)*1e6 + sec*1e6
	return int64(total + 0.5), true
}

// progressWriter is an io.Writer that splits incoming bytes into lines and
// feeds them to a progressParser. exec.Cmd copies stdout into it from its
// own goroutine, and Wait does not return until that copy is finished, so
// every complete block is delivered before RunFFmpeg returns.
type progressWriter struct {
	parser  *progressParser
	pending []byte
}

func newProgressWriter(fn func(Progress)) *progressWriter {
	return &progressWriter{parser: &progressParser{emit: fn}}
}

func (w *progressWriter) Write(p []byte) (int, error) {
	w.pending = append(w.pending, p...)
	for {
		i := bytes.IndexByte(w.pending, '\n')
		if i < 0 {
			break
		}
		w.parser.line(string(w.pending[:i]))
		w.pending = w.pending[i+1:]
	}
	if len(w.pending) > maxProgressLine {
		w.pending = w.pending[len(w.pending)-maxProgressLine:]
	}
	if len(w.pending) == 0 {
		w.pending = nil // release the backing array once a block is fully consumed
	}
	return len(p), nil
}
