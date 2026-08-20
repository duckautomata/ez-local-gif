package server

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/duckautomata/ez-local-gif/internal/probe"
	"github.com/duckautomata/ez-local-gif/internal/store"
)

// POST /api/upload accepts one file — any format ffprobe can read, stored as
// a blob — or several "file" parts that together form an image sequence:
// every part must be a still image the image2 demuxer can open (png, jpeg,
// webp, bmp or tiff — by extension, or by content when the name has no
// recognised image extension; gif and avif have no image2 codec mapping, so
// they are refused as sequence frames and upload one at a time), all frames
// must share one effective extension (the image2 pattern), and the frames
// are ordered naturally by file name ("frame2.png" before "frame10.png")
// whatever order the client sent them in. The optional "delayMs" field
// (1..60000, default 100) is the sequence's per-frame duration; it only
// seeds a sequence that is new to the store — re-uploading the same frames
// returns the stored source, whose delay a recipe overrides with the "delay"
// op.
//
// The body is streamed exactly once: every file part is staged as a temp
// file under the store's tmp dir while it is read. Nothing enters the blob
// store until the whole body has been read and validated — a single file
// then goes through store.PutBlob, a sequence through store.PutSequence —
// so a rejected or failed upload never has to delete a stored blob (a
// concurrent upload of the same bytes may have deduped onto it and still be
// using it). The staging dir always goes away with the request.

const (
	// uploadFileField is the multipart field name carrying file parts.
	uploadFileField = "file"
	// uploadDelayField is the optional per-frame delay of an image sequence.
	uploadDelayField = "delayMs"
	// DefaultSequenceDelayMS is the per-frame delay of an uploaded image
	// sequence when the client does not send one (10 fps, as the prober
	// assumes).
	DefaultSequenceDelayMS = probe.DefaultSequenceDelayMS
	// maxSequenceDelayMS bounds the delayMs field (recipe.DelayParams: 1..60000).
	maxSequenceDelayMS = 60000
	// sniffLen is how many leading bytes decide the content of a part whose
	// name has no recognised image extension.
	sniffLen = 512
	// maxDelayFieldLen bounds the delayMs field's value.
	maxDelayFieldLen = 32
	// storeTmpDir is the store's upload staging directory under its Root
	// (same filesystem as the blobs; abandoned entries are swept after an
	// hour).
	storeTmpDir = "tmp"
)

// sequenceImageExts are the extensions an image-sequence frame may be stored
// under: exactly the still-image formats whose extension ffmpeg's image2
// demuxer maps to a decoder. GIF and AVIF are deliberately absent — image2
// has no codec mapping for them, so a stored %06d.gif / %06d.avif pattern
// could never be opened by the render pipeline; they upload one at a time.
var sequenceImageExts = map[string]bool{
	"png": true, "jpg": true, "jpeg": true, "webp": true,
	"bmp": true, "tiff": true, "tif": true,
}

// uploadError is a client-side mistake found while reading the multipart
// body; the handler answers it with Status and Msg.
type uploadError struct {
	Status int
	Msg    string
}

func (e *uploadError) Error() string { return e.Msg }

func badUpload(format string, args ...any) *uploadError {
	return &uploadError{Status: http.StatusBadRequest, Msg: fmt.Sprintf(format, args...)}
}

// uploadPart is one file part of a multi-file upload.
type uploadPart struct {
	name string // client file name (last path element); orders the frames
	// seqExt is the effective sequence extension: the file's own extension
	// when it is sequence-eligible, else the sniffed image format when that
	// is; "" when the part cannot be a sequence frame.
	seqExt string
	// storeName is the name handed to store.PutSequence. It always carries
	// seqExt, so the stored pattern is one the image2 demuxer can open
	// ("frame-a" with PNG bytes is stored as if named "frame-a.png", never
	// under %06d.bin).
	storeName string
	path      string // staged file under the upload's staging dir
}

// newUploadPart derives a part's sequence facts from its client name and
// leading bytes.
func newUploadPart(fileName string, head []byte) uploadPart {
	name := lastPathElement(fileName)
	p := uploadPart{name: name, storeName: name}
	if ext := store.SanitizeExt(name); sequenceImageExts[ext] {
		p.seqExt = ext
	} else if sniffed := sniffImage(head); sequenceImageExts[sniffed] {
		p.seqExt = sniffed
		p.storeName = name + "." + sniffed
	}
	return p
}

// upload is the parsed multipart body of one POST /api/upload. Every file
// part is staged under stage; nothing is in the blob store yet.
type upload struct {
	hasFirst  bool       // a "file" part arrived
	firstPart uploadPart // the first "file" part, staged like the rest
	firstSize int64      // bytes of the first part

	stage string       // temp dir holding the staged parts; "" until the first part
	rest  []uploadPart // later file parts, in upload order

	delayMS int
}

// close removes the staging dir (best effort; the sweeper also reaps
// abandoned tmp entries after an hour).
func (u *upload) close() {
	if u.stage == "" {
		return
	}
	if err := os.RemoveAll(u.stage); err != nil {
		log.Printf("server: remove upload staging %s: %v", u.stage, err)
	}
	u.stage = ""
}

// isSequence reports whether the body carried more than one file part.
func (u *upload) isSequence() bool { return len(u.rest) > 0 }

// readUpload consumes the multipart body. A returned *uploadError is the
// client's fault (answer with its status); any other error is a body read or
// staging failure for uploadReadError. The upload is returned even on error
// so the caller can clean up what was staged so far.
func (s *Server) readUpload(mr *multipart.Reader) (*upload, error) {
	up := &upload{delayMS: DefaultSequenceDelayMS}
	for {
		part, err := mr.NextPart()
		if errors.Is(err, io.EOF) {
			return up, nil
		}
		if err != nil {
			return up, err
		}
		switch part.FormName() {
		case uploadDelayField:
			if err := up.readDelay(part); err != nil {
				return up, err
			}
		case uploadFileField:
			if err := s.readFilePart(up, part); err != nil {
				return up, err
			}
		}
		// NextPart discards whatever is left of this part.
	}
}

// readDelay parses the delayMs field.
func (u *upload) readDelay(part *multipart.Part) error {
	raw, err := io.ReadAll(io.LimitReader(part, maxDelayFieldLen+1))
	if err != nil {
		return err
	}
	ms, err := strconv.Atoi(strings.TrimSpace(string(raw)))
	if err != nil || ms < 1 || ms > maxSequenceDelayMS {
		return badUpload("%s must be a whole number of milliseconds from 1 to %d", uploadDelayField, maxSequenceDelayMS)
	}
	u.delayMS = ms
	return nil
}

// readFilePart stages every file part, enforcing the image-sequence rules as
// soon as a second part shows up — before its bytes are streamed, so a stray
// video is refused cheaply.
func (s *Server) readFilePart(up *upload, part *multipart.Part) error {
	if !up.hasFirst {
		return s.readFirstPart(up, part)
	}
	if !up.isSequence() {
		// The second file part turns this into a sequence upload: the first
		// part is now held to the same rules.
		if err := checkSequencePart(up.firstPart, up.firstSize == 0, 1); err != nil {
			return err
		}
	}
	if len(up.rest)+2 > store.MaxSequenceFrames {
		return badUpload("too many files: an image sequence may have at most %d frames", store.MaxSequenceFrames)
	}
	head, err := readHead(part)
	if err != nil {
		return err
	}
	p := newUploadPart(part.FileName(), head)
	if err := checkSequencePart(p, len(head) == 0, len(up.rest)+2); err != nil {
		return err
	}
	if p.seqExt != up.firstPart.seqExt {
		return badUpload("%s (%s is .%s, %s is .%s)", store.ErrMixedSequence.Error(), up.firstPart.name, up.firstPart.seqExt, p.name, p.seqExt)
	}
	if err := up.ensureStage(s.st); err != nil {
		return err
	}
	p.path = filepath.Join(up.stage, fmt.Sprintf("%06d", len(up.rest)+2))
	if _, err := writeStaged(p.path, head, part); err != nil {
		return err
	}
	up.rest = append(up.rest, p)
	return nil
}

// readFirstPart stages the first file part like any later one, keeping its
// leading bytes' verdict and size: whether it becomes a blob of its own
// (single-file upload) or frame one of a sequence is only known once the
// whole body has been read, and it must not enter the shared blob store
// before then.
func (s *Server) readFirstPart(up *upload, part *multipart.Part) error {
	if err := up.ensureStage(s.st); err != nil {
		return err
	}
	head, err := readHead(part)
	if err != nil {
		return err
	}
	p := newUploadPart(part.FileName(), head)
	p.path = filepath.Join(up.stage, "000001")
	n, err := writeStaged(p.path, head, part)
	if err != nil {
		return err
	}
	up.hasFirst = true
	up.firstPart = p
	up.firstSize = n
	return nil
}

// storeFirstPart moves the staged single file into the blob store. It runs
// only once the whole body has been read and validated, so a rejected or
// failed upload never has to remove anything from the store.
func (s *Server) storeFirstPart(up *upload) (*store.Blob, error) {
	f, err := os.Open(up.firstPart.path)
	if err != nil {
		return nil, fmt.Errorf("open staged upload: %w", err)
	}
	defer f.Close()
	return s.st.PutBlob(f, up.firstPart.name)
}

// ensureStage creates the staging dir on first use.
func (u *upload) ensureStage(st *store.Store) error {
	if u.stage != "" {
		return nil
	}
	dir, err := os.MkdirTemp(filepath.Join(st.Root, storeTmpDir), "upload-seq-*")
	if err != nil {
		return fmt.Errorf("create upload staging dir: %w", err)
	}
	u.stage = dir
	return nil
}

// readHead reads up to sniffLen leading bytes of r (fewer at EOF).
func readHead(r io.Reader) ([]byte, error) {
	head := make([]byte, sniffLen)
	n, err := io.ReadFull(r, head)
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return nil, err
	}
	return head[:n], nil
}

// writeStaged writes head followed by the rest of part to path and returns
// the byte count.
func writeStaged(path string, head []byte, part io.Reader) (int64, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0o644)
	if err != nil {
		return 0, fmt.Errorf("create upload staging file: %w", err)
	}
	n, err := io.Copy(f, io.MultiReader(bytes.NewReader(head), part))
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return n, err
}

// checkSequencePart enforces the per-frame rules of a multi-file upload on
// the i-th (1-based) file part.
func checkSequencePart(p uploadPart, empty bool, i int) error {
	if empty {
		return badUpload("file %d (%s) is empty", i, p.name)
	}
	if p.seqExt == "" {
		return badUpload("several files are uploaded as an image sequence, but file %d (%s) is not an image the sequence pipeline can read (png, jpeg, webp, bmp, tiff); upload videos and animations (gif, avif) one at a time", i, p.name)
	}
	return nil
}

// sequenceParts opens every frame of the upload in natural name order for
// store.PutSequence. closeFn must be called once the store has consumed them
// (before the staging dir is removed: an open file cannot be deleted on
// Windows). The store sees each part's storeName, so the stored pattern
// always carries a readable image extension.
func (u *upload) sequenceParts() (parts []store.SequencePart, closeFn func(), err error) {
	all := make([]uploadPart, 0, 1+len(u.rest))
	all = append(all, u.firstPart)
	all = append(all, u.rest...)
	slices.SortStableFunc(all, func(a, b uploadPart) int { return naturalCompare(a.name, b.name) })

	files := make([]*os.File, 0, len(all))
	closeFn = func() {
		for _, f := range files {
			f.Close()
		}
	}
	for _, p := range all {
		f, err := os.Open(p.path)
		if err != nil {
			closeFn()
			return nil, nil, fmt.Errorf("open staged frame %s: %w", p.name, err)
		}
		files = append(files, f)
		parts = append(parts, store.SequencePart{Name: p.storeName, R: f})
	}
	return parts, closeFn, nil
}

// lastPathElement returns the part of name after the last '/' or '\' (a
// client may send a relative path for folder uploads).
func lastPathElement(name string) string {
	if i := strings.LastIndexAny(name, `/\`); i >= 0 {
		return name[i+1:]
	}
	return name
}

// ---- content sniffing ----------------------------------------------------------

// sniffImage returns the extension of the image format whose signature
// opens head ("png", "jpg", "gif", "webp", "bmp", "tiff", "avif"), or "".
// It covers the formats a sequence may consist of plus gif/avif — which it
// must recognise so that extension-less GIF/AVIF content is refused as a
// sequence frame rather than stored under a pattern image2 cannot open;
// net/http's sniffer lacks TIFF and AVIF.
func sniffImage(head []byte) string {
	switch {
	case bytes.HasPrefix(head, []byte("\x89PNG\r\n\x1a\n")):
		return "png"
	case bytes.HasPrefix(head, []byte{0xFF, 0xD8, 0xFF}):
		return "jpg"
	case bytes.HasPrefix(head, []byte("GIF87a")), bytes.HasPrefix(head, []byte("GIF89a")):
		return "gif"
	case len(head) >= 12 && bytes.Equal(head[:4], []byte("RIFF")) && bytes.Equal(head[8:12], []byte("WEBP")):
		return "webp"
	case bytes.HasPrefix(head, []byte("BM")):
		return "bmp"
	case bytes.HasPrefix(head, []byte("II*\x00")), bytes.HasPrefix(head, []byte("MM\x00*")):
		return "tiff"
	case isAVIF(head):
		return "avif"
	}
	return ""
}

// isAVIF reports whether head opens with an ISOBMFF ftyp box whose major or
// compatible brands include avif/avis.
func isAVIF(head []byte) bool {
	if len(head) < 12 || !bytes.Equal(head[4:8], []byte("ftyp")) {
		return false
	}
	end := int(binary.BigEndian.Uint32(head[:4]))
	if end < 16 || end > len(head) {
		end = len(head)
	}
	for off := 8; off+4 <= end; off += 4 {
		switch string(head[off : off+4]) {
		case "avif", "avis":
			return true
		}
	}
	return false
}

// ---- natural ordering ---------------------------------------------------------

// naturalCompare orders file names the way a person reads them: where both
// names have a run of ASCII digits, the runs compare by value ("frame2" <
// "frame10"); everywhere else runes compare case-insensitively ("img.png" <
// "img1.png" < "imga.png", as '.' < '1' < 'a'). Ties — "01" vs "1", "A" vs
// "a" — are broken by the raw bytes so the order is total and stable.
func naturalCompare(a, b string) int {
	i, j := 0, 0
	for i < len(a) && j < len(b) {
		if isASCIIDigit(a[i]) && isASCIIDigit(b[j]) {
			ai, bj := digitRun(a, i), digitRun(b, j)
			if c := compareNumbers(a[i:ai], b[j:bj]); c != 0 {
				return c
			}
			i, j = ai, bj
			continue
		}
		ra, na := utf8.DecodeRuneInString(a[i:])
		rb, nb := utf8.DecodeRuneInString(b[j:])
		if la, lb := unicode.ToLower(ra), unicode.ToLower(rb); la != lb {
			if la < lb {
				return -1
			}
			return 1
		}
		i += na
		j += nb
	}
	switch {
	case i < len(a):
		return 1
	case j < len(b):
		return -1
	}
	return strings.Compare(a, b)
}

func isASCIIDigit(c byte) bool { return c >= '0' && c <= '9' }

// digitRun returns the index just past the run of digits starting at i.
func digitRun(s string, i int) int {
	for i < len(s) && isASCIIDigit(s[i]) {
		i++
	}
	return i
}

// compareNumbers compares two digit strings by value (leading zeros ignored,
// arbitrary length).
func compareNumbers(a, b string) int {
	a, b = strings.TrimLeft(a, "0"), strings.TrimLeft(b, "0")
	if len(a) != len(b) {
		if len(a) < len(b) {
			return -1
		}
		return 1
	}
	return strings.Compare(a, b)
}
