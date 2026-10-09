package store

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/matte"
)

// matteKeyOf makes a 64-hex-character stand-in clip key from one digit.
func matteKeyOf(c byte) string { return strings.Repeat(string(c), 64) }

// putMatteClip writes a memoised clip sequence for key made from src: frames
// PNG stand-ins of pngSize bytes and a manifest, every mtime set to mtime.
func putMatteClip(t *testing.T, st *Store, key, src string, frames, pngSize int, mtime time.Time) string {
	t.Helper()
	dir := st.MatteDir(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= frames; i++ {
		p := filepath.Join(dir, matte.FrameFile(i))
		if err := os.WriteFile(p, bytes.Repeat([]byte("p"), pngSize), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
	}
	man := filepath.Join(dir, matte.ManifestName)
	err := matte.WriteManifest(man, &matte.Manifest{
		Key: key, Src: src, Model: "isnet-anime", Weights: strings.Repeat("0", 64), Proc: "1",
		Precision: "fp16", Size: 1024, FPS: "25", Frames: frames, Device: "cuda",
	})
	if err != nil {
		t.Fatal(err)
	}
	os.Chtimes(man, mtime, mtime)
	os.Chtimes(dir, mtime, mtime)
	return dir
}

// putMatteTmp creates a pass dir for key holding one frame, every mtime set
// to mtime.
func putMatteTmp(t *testing.T, st *Store, key string, mtime time.Time) string {
	t.Helper()
	dir := st.MatteTmpDir(key)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, matte.FrameFile(1))
	if err := os.WriteFile(p, []byte("partial"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mtime, mtime)
	os.Chtimes(dir, mtime, mtime)
	return dir
}

// testFrameDir is the frames-store dir the tests file under for model.
func testFrameDir(st *Store, model string) string {
	return st.MatteFrameDir(model, 1024, "fp16", "0123abcd", "1")
}

// putMatteFrame files one frames-store PNG stand-in of size bytes as name
// under testFrameDir(model), mtime set.
func putMatteFrame(t *testing.T, st *Store, model, name string, size int, mtime time.Time) string {
	t.Helper()
	dir := testFrameDir(st, model)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, bytes.Repeat([]byte("f"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mtime, mtime)
	return p
}

// putBlobAt uploads content as name and backdates payload and meta to mtime.
func putBlobAt(t *testing.T, st *Store, content, name string, mtime time.Time) *Blob {
	t.Helper()
	b, err := st.PutBlob(strings.NewReader(content), name)
	if err != nil {
		t.Fatal(err)
	}
	os.Chtimes(b.Path, mtime, mtime)
	os.Chtimes(st.metaPath(b.Hash), mtime, mtime)
	return b
}

// putOldFile writes a file at path and backdates it (the dir of a dot-dir or
// junk clip too, when it is a dir's only file).
func putOldFile(t *testing.T, path string, mtime time.Time) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(path, mtime, mtime)
	os.Chtimes(filepath.Dir(path), mtime, mtime)
}

func exists(t *testing.T, path string) bool {
	t.Helper()
	_, err := os.Stat(path)
	if err == nil {
		return true
	}
	if errors.Is(err, os.ErrNotExist) {
		return false
	}
	t.Fatalf("stat %s: %v", path, err)
	return false
}

// want asserts the presence (true) or absence (false) of every path.
func want(t *testing.T, present bool, what string, paths ...string) {
	t.Helper()
	for _, p := range paths {
		if got := exists(t, p); got != present {
			if present {
				t.Errorf("%s: %s was deleted", what, filepath.Base(p))
			} else {
				t.Errorf("%s: %s survived", what, filepath.Base(p))
			}
		}
	}
}

func TestMatteDirLayout(t *testing.T) {
	st := newTestStore(t)
	key := matteKeyOf('a')
	mattes := filepath.Join(st.Root, "mattes")

	if got, w := st.MatteDir(key), filepath.Join(mattes, key); got != w {
		t.Errorf("MatteDir = %q, want %q", got, w)
	}
	// Sanitised like a scratch id: a traversal attempt cannot leave mattes.
	if got, w := st.MatteDir("../x/../"+key), filepath.Join(mattes, "x"+key); got != w {
		t.Errorf("MatteDir(traversal) = %q, want %q", got, w)
	}
	if got, w := st.MatteDir(""), filepath.Join(mattes, "invalid"); got != w {
		t.Errorf("MatteDir(\"\") = %q, want %q", got, w)
	}

	if got, w := st.MatteFrameDir("isnet-anime", 1024, "fp16", "0123abcd", "1"),
		filepath.Join(mattes, "frames", "isnet-anime", "1024", "fp16", "0123abcd-1"); got != w {
		t.Errorf("MatteFrameDir = %q, want %q", got, w)
	}
	if got, w := st.MatteFrameDir("", 512, "", "", ""),
		filepath.Join(mattes, "frames", "invalid", "512", "invalid", "invalid-invalid"); got != w {
		t.Errorf("MatteFrameDir(empty) = %q, want %q", got, w)
	}

	tmp1, tmp2 := st.MatteTmpDir(key), st.MatteTmpDir(key)
	prefix := filepath.Join(mattes, ".tmp-"+key+"-")
	for _, p := range []string{tmp1, tmp2} {
		if !strings.HasPrefix(p, prefix) || len(p) != len(prefix)+2*matteTmpRandBytes {
			t.Errorf("MatteTmpDir = %q, want %q + %d hex", p, prefix, 2*matteTmpRandBytes)
		}
		if exists(t, p) {
			t.Errorf("MatteTmpDir created %s", p)
		}
	}
	if tmp1 == tmp2 {
		t.Errorf("MatteTmpDir repeated %q", tmp1)
	}
	if exists(t, mattes) {
		t.Error("the path helpers must not create the mattes dir")
	}
}

func TestProtect(t *testing.T) {
	st := newTestStore(t)
	dir := st.MatteDir(matteKeyOf('a'))
	child := filepath.Join(dir, matte.FrameFile(1))
	sibling := dir + "b" // shares the prefix without a separator
	parent := filepath.Dir(dir)

	if st.isProtected(dir) {
		t.Fatal("protected before Protect")
	}
	r1 := st.Protect(dir)
	r2 := st.Protect(dir + string(filepath.Separator)) // cleans to the same dir
	for _, p := range []string{dir, child} {
		if !st.isProtected(p) {
			t.Errorf("%s not protected", p)
		}
	}
	for _, p := range []string{sibling, parent} {
		if st.isProtected(p) {
			t.Errorf("%s protected although outside the dir", p)
		}
	}
	r1()
	r1() // idempotent: must not release the second hold
	if !st.isProtected(child) {
		t.Fatal("the second hold was released by the first release")
	}
	r2()
	if st.isProtected(dir) {
		t.Fatal("still protected after every hold was released")
	}
	r2() // over-release is harmless
	if len(st.protected) != 0 {
		t.Errorf("protected set not empty: %v", st.protected)
	}
}

func TestTouchMatte(t *testing.T) {
	st := newTestStore(t)
	if err := st.TouchMatte(matteKeyOf('z')); err != nil {
		t.Errorf("TouchMatte of a missing memo: %v", err)
	}
	if err := st.TouchMatteFrame(filepath.Join(st.Root, "mattes", "frames", "nope.png")); err != nil {
		t.Errorf("TouchMatteFrame of a missing file: %v", err)
	}
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	src := putBlobAt(t, st, "source", "src.mov", now)
	clip := putMatteClip(t, st, matteKeyOf('a'), src.Hash, 1, 10, old)
	frame := putMatteFrame(t, st, "isnet-anime", "aaaa.png", 10, old)

	if err := st.TouchMatte(matteKeyOf('a')); err != nil {
		t.Fatal(err)
	}
	if err := st.TouchMatteFrame(frame); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{clip, filepath.Join(clip, matte.ManifestName), frame} {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if fi.ModTime().Before(now.Add(-time.Minute)) {
			t.Errorf("%s not touched: mtime %v", filepath.Base(p), fi.ModTime())
		}
	}
	// The frames inside a clip dir are never touched (the dir is the unit).
	fi, _ := os.Stat(filepath.Join(clip, matte.FrameFile(1)))
	if !fi.ModTime().Before(now.Add(-time.Hour)) {
		t.Error("TouchMatte touched the clip's frames")
	}
}

func TestSweepMattesTTL(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	src := putBlobAt(t, st, "source", "src.mov", now) // fresh: always alive

	oldClip := putMatteClip(t, st, matteKeyOf('a'), src.Hash, 2, 10, old)
	newClip := putMatteClip(t, st, matteKeyOf('b'), src.Hash, 2, 10, now)
	touchedClip := putMatteClip(t, st, matteKeyOf('c'), src.Hash, 2, 10, old)
	if err := st.TouchMatte(matteKeyOf('c')); err != nil {
		t.Fatal(err)
	}
	// A clip aged only through its manifest (a touch that reached the file
	// but not the dir) counts as touched too.
	manTouched := putMatteClip(t, st, matteKeyOf('d'), src.Hash, 2, 10, old)
	os.Chtimes(filepath.Join(manTouched, matte.ManifestName), now, now)

	oldTmp := putMatteTmp(t, st, matteKeyOf('e'), old)
	newTmp := putMatteTmp(t, st, matteKeyOf('f'), now)
	// A pass that started long ago but is still receiving frames.
	liveTmp := putMatteTmp(t, st, matteKeyOf('g'), old)
	if err := os.WriteFile(filepath.Join(liveTmp, matte.FrameFile(2)), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(liveTmp, old, old)

	oldFrame := putMatteFrame(t, st, "isnet-anime", "aaaa.png", 10, old)
	newFrame := putMatteFrame(t, st, "isnet-anime", "bbbb.png", 10, now)
	touchedFrame := putMatteFrame(t, st, "isnet-anime", "cccc.png", 10, old)
	if err := st.TouchMatteFrame(touchedFrame); err != nil {
		t.Fatal(err)
	}
	// Write temps beside the frames: junk after the grace, in progress before.
	oldTemp := putMatteFrame(t, st, "isnet-anime", ".dddd.png.tmp", 10, now.Add(-2*time.Hour))
	newTemp := putMatteFrame(t, st, "isnet-anime", ".eeee.png.tmp", 10, now)

	// The facts file and its write temp are not the sweeper's, nor is an
	// unknown dot-dir.
	facts := filepath.Join(st.Root, "mattes", matte.FactsName)
	putOldFile(t, facts, old)
	factsTmp := filepath.Join(st.Root, "mattes", ".models.json.abc")
	putOldFile(t, factsTmp, old)
	unknownDir := filepath.Join(st.Root, "mattes", ".unknown")
	putOldFile(t, filepath.Join(unknownDir, "x"), old)

	// A clip dir without a readable manifest: junk when old, in progress
	// when fresh (the results rule).
	junkClip := st.MatteDir(matteKeyOf('h'))
	putOldFile(t, filepath.Join(junkClip, matte.FrameFile(1)), old)
	progClip := st.MatteDir(matteKeyOf('i'))
	putOldFile(t, filepath.Join(progClip, matte.FrameFile(1)), now)
	// A manifest of another version is unreadable too.
	badClip := st.MatteDir(matteKeyOf('j'))
	putOldFile(t, filepath.Join(badClip, matte.FrameFile(1)), old)
	putOldFile(t, filepath.Join(badClip, matte.ManifestName), old)

	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	want(t, false, "TTL", oldClip, oldTmp, oldFrame, oldTemp, junkClip, badClip)
	want(t, true, "TTL", newClip, touchedClip, manTouched, newTmp, liveTmp, newFrame, touchedFrame, newTemp,
		facts, factsTmp, unknownDir, progClip)
	// The age pass touched nothing else.
	if _, err := st.GetBlob(src.Hash); err != nil {
		t.Errorf("source blob swept: %v", err)
	}

	// ttl disabled: only junk goes.
	oldClip2 := putMatteClip(t, st, matteKeyOf('k'), src.Hash, 1, 10, old)
	oldTmp2 := putMatteTmp(t, st, matteKeyOf('l'), old)
	oldFrame2 := putMatteFrame(t, st, "isnet-anime", "ffff.png", 10, old)
	if err := st.Sweep(context.Background(), 0, 0); err != nil {
		t.Fatalf("Sweep (ttl 0): %v", err)
	}
	want(t, false, "ttl 0", oldTmp2)
	want(t, true, "ttl 0", oldClip2, oldFrame2)
}

func TestSweepMattesDeadSource(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	live := putBlobAt(t, st, "live", "live.mov", now)
	dying := putBlobAt(t, st, "dying", "dying.mov", old) // expires under a 24 h TTL

	alive := putMatteClip(t, st, matteKeyOf('a'), live.Hash, 1, 10, now)
	alive2 := putMatteClip(t, st, matteKeyOf('b'), live.Hash, 1, 10, now) // shares the source
	orphan := putMatteClip(t, st, matteKeyOf('c'), matteKeyOf('9'), 1, 10, now)
	dead := putMatteClip(t, st, matteKeyOf('d'), dying.Hash, 1, 10, now)
	bogus := putMatteClip(t, st, matteKeyOf('e'), "not-a-hash", 1, 10, now)

	// Whatever the TTL says, a clip whose source is gone goes; one whose
	// source is merely old stays while the source does.
	if err := st.Sweep(context.Background(), 0, 0); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	want(t, false, "dead source", orphan, bogus)
	want(t, true, "dead source", alive, alive2, dead)

	// The source expires in this sweep's blob pass: its clip is dead weight
	// in the same sweep.
	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := st.GetBlob(dying.Hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("dying blob: %v", err)
	}
	want(t, false, "expired source", dead)
	want(t, true, "expired source", alive, alive2)
}

func TestSweepMattesSizeOrder(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	fresh := putBlobAt(t, st, strings.Repeat("f", 10), "fresh.bin", now) // never evictable
	oldBlob := putBlobAt(t, st, strings.Repeat("o", 200), "old.bin", now.Add(-5*time.Hour))
	res := matteKeyOf('1')
	putResult(t, st, res, 100, now.Add(-3*time.Hour)) // 102 bytes
	clip := putMatteClip(t, st, matteKeyOf('a'), fresh.Hash, 2, 50, now.Add(-10*time.Hour))
	frame := putMatteFrame(t, st, "isnet-anime", "aaaa.png", 60, now.Add(-11*time.Hour)) // the oldest thing on the disk
	tmp := putMatteTmp(t, st, matteKeyOf('b'), now)                                      // in progress

	fileSize := func(p string) int64 {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	blobsTotal := 210 + fileSize(st.metaPath(fresh.Hash)) + fileSize(st.metaPath(oldBlob.Hash))
	mattesTotal := dirSize(clip) + 60 + dirSize(tmp)
	total := 102 + blobsTotal + mattesTotal

	// 1. A cap the result alone meets: nothing older in the other classes goes.
	if err := st.Sweep(context.Background(), 0, total-102); err != nil {
		t.Fatalf("Sweep #1: %v", err)
	}
	if st.HasResult(res) {
		t.Error("#1: result not evicted first")
	}
	if _, err := st.GetBlob(oldBlob.Hash); err != nil {
		t.Errorf("#1: old blob evicted before the result sufficed: %v", err)
	}
	want(t, true, "#1 mattes last", clip, frame, tmp)

	// 2. A cap the old blob alone meets: mattes still untouched.
	if err := st.Sweep(context.Background(), 0, blobsTotal+mattesTotal-1); err != nil {
		t.Fatalf("Sweep #2: %v", err)
	}
	if _, err := st.GetBlob(oldBlob.Hash); !errors.Is(err, ErrNotFound) {
		t.Errorf("#2: old blob should be evicted: %v", err)
	}
	if _, err := st.GetBlob(fresh.Hash); err != nil {
		t.Errorf("#2: fresh blob evicted: %v", err)
	}
	want(t, true, "#2 mattes last", clip, frame, tmp)

	// 3. Over the cap by the frame's size: the oldest matte goes, the clip stays.
	total3 := 10 + fileSize(st.metaPath(fresh.Hash)) + mattesTotal
	if err := st.Sweep(context.Background(), 0, total3-60); err != nil {
		t.Fatalf("Sweep #3: %v", err)
	}
	want(t, false, "#3 oldest matte", frame)
	want(t, true, "#3 oldest matte", clip, tmp)

	// 4. A cap nothing meets: the clip goes; the in-progress pass and the
	// fresh blob stay.
	if err := st.Sweep(context.Background(), 0, 1); err != nil {
		t.Fatalf("Sweep #4: %v", err)
	}
	want(t, false, "#4", clip)
	want(t, true, "#4", tmp)
	if _, err := st.GetBlob(fresh.Hash); err != nil {
		t.Errorf("#4: fresh blob evicted: %v", err)
	}
}

func TestSweepMattesDeadAfterSizeEviction(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	fresh := putBlobAt(t, st, strings.Repeat("f", 10), "fresh.bin", now)
	oldBlob := putBlobAt(t, st, strings.Repeat("o", 200), "old.bin", now.Add(-5*time.Hour))
	doomed := putMatteClip(t, st, matteKeyOf('a'), oldBlob.Hash, 1, 10, now) // fresh matte of a blob about to go
	other := putMatteClip(t, st, matteKeyOf('b'), fresh.Hash, 1, 10, now)

	fileSize := func(p string) int64 {
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		return fi.Size()
	}
	total := 210 + fileSize(st.metaPath(fresh.Hash)) + fileSize(st.metaPath(oldBlob.Hash)) + dirSize(doomed) + dirSize(other)
	// The blob pass evicts the old blob and meets the cap; its clip is dead
	// weight in the same sweep, the other clip untouched.
	if err := st.Sweep(context.Background(), 0, total-1); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	if _, err := st.GetBlob(oldBlob.Hash); !errors.Is(err, ErrNotFound) {
		t.Fatalf("old blob: %v", err)
	}
	want(t, false, "dead after eviction", doomed)
	want(t, true, "dead after eviction", other)
}

func TestSweepProtects(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	old := now.Add(-48 * time.Hour)

	resOld := st.ResultDir(matteKeyOf('1'))
	putResult(t, st, matteKeyOf('1'), 10, old)
	resJunk := st.ResultDir(matteKeyOf('2'))
	putOldFile(t, filepath.Join(resJunk, "out.gif"), old)
	blobOld := putBlobAt(t, st, "old", "old.gif", old)
	src := putBlobAt(t, st, "source", "src.mov", now)
	clipOld := putMatteClip(t, st, matteKeyOf('a'), src.Hash, 1, 10, old)
	clipDead := putMatteClip(t, st, matteKeyOf('b'), matteKeyOf('9'), 1, 10, now)
	tmpOld := putMatteTmp(t, st, matteKeyOf('c'), old)
	frameOld := putMatteFrame(t, st, "isnet-anime", "aaaa.png", 10, old)
	frameOther := putMatteFrame(t, st, "birefnet-lite", "bbbb.png", 10, old)
	uploadTmp := filepath.Join(st.Root, "tmp", "upload-x.part")
	putOldFile(t, uploadTmp, old)

	held := []string{resOld, resJunk, blobOld.Path, clipOld, clipDead, tmpOld,
		filepath.Join(st.Root, "mattes", "frames", "isnet-anime"), // the whole model subtree
		uploadTmp}
	var releases []func()
	for _, p := range held {
		releases = append(releases, st.Protect(p))
	}

	// Age pass: every protected path survives its rule; the unprotected
	// frame of the other model goes.
	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	want(t, true, "protected (age)", resOld, resJunk, blobOld.Path, clipOld, clipDead, tmpOld, frameOld, uploadTmp)
	want(t, false, "unprotected (age)", frameOther)

	// Size pass: likewise.
	if err := st.Sweep(context.Background(), 0, 1); err != nil {
		t.Fatalf("Sweep (size): %v", err)
	}
	want(t, true, "protected (size)", resOld, resJunk, blobOld.Path, clipOld, clipDead, tmpOld, frameOld, uploadTmp)

	for _, r := range releases {
		r()
	}
	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatalf("Sweep (released): %v", err)
	}
	want(t, false, "released", resOld, resJunk, blobOld.Path, clipOld, clipDead, tmpOld, frameOld, uploadTmp)
	if _, err := st.GetBlob(src.Hash); err != nil {
		t.Errorf("fresh source blob swept: %v", err)
	}
}

func TestSweepMattesAbsentOrUnreadable(t *testing.T) {
	// A plain install never writes the memo: the sweep is unaffected and
	// does not create the dir.
	st := newTestStore(t)
	if err := st.Sweep(context.Background(), 24*time.Hour, 1); err != nil {
		t.Fatalf("Sweep without mattes: %v", err)
	}
	if exists(t, filepath.Join(st.Root, "mattes")) {
		t.Error("Sweep created the mattes dir")
	}

	// An unreadable mattes dir does not stop the other classes from being
	// swept. Linux reports it (ENOTDIR); Windows lists a file's "children"
	// as path-not-found, which is the plain-install case above.
	st2 := newTestStore(t)
	old := time.Now().Add(-48 * time.Hour)
	if err := os.WriteFile(filepath.Join(st2.Root, "mattes"), []byte("not a dir"), 0o644); err != nil {
		t.Fatal(err)
	}
	putResult(t, st2, matteKeyOf('1'), 10, old)
	err := st2.Sweep(context.Background(), 24*time.Hour, 0)
	if runtime.GOOS != "windows" && (err == nil || !strings.Contains(err.Error(), "sweep mattes")) {
		t.Errorf("Sweep with a file at mattes: err = %v, want a sweep mattes error", err)
	}
	if st2.HasResult(matteKeyOf('1')) {
		t.Error("old result survived because the mattes dir was unreadable")
	}
}

func TestSweepPrunesEmptyMatteFrameDirs(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	old := now.Add(-2 * time.Hour)

	// An empty leaf older than the grace goes; the parent it leaves behind
	// carries the removal's mtime and waits for a later sweep.
	emptyOld := st.MatteFrameDir("isnet-anime", 1024, "fp16", "old00000", "1")
	if err := os.MkdirAll(emptyOld, 0o755); err != nil {
		t.Fatal(err)
	}
	for p := emptyOld; strings.HasPrefix(p, filepath.Join(st.Root, "mattes", "frames")+string(filepath.Separator)); p = filepath.Dir(p) {
		os.Chtimes(p, old, old)
	}
	// A fresh empty leaf is a pass about to file its first frame.
	emptyNew := st.MatteFrameDir("isnet-anime", 512, "fp32", "new00000", "1")
	if err := os.MkdirAll(emptyNew, 0o755); err != nil {
		t.Fatal(err)
	}
	// A leaf with a live frame stays, and so do its parents.
	frame := putMatteFrame(t, st, "birefnet-lite", "aaaa.png", 10, now)
	// A protected empty old leaf stays.
	emptyHeld := st.MatteFrameDir("isnet-anime", 1024, "fp32", "held0000", "1")
	if err := os.MkdirAll(emptyHeld, 0o755); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(emptyHeld, old, old)
	release := st.Protect(emptyHeld)
	defer release()

	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	want(t, false, "prune", emptyOld)
	want(t, true, "prune", filepath.Dir(emptyOld), emptyNew, frame, testFrameDir(st, "birefnet-lite"), emptyHeld)
}
