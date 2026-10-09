package store

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/duckautomata/ez-local-gif/internal/matte"
)

// putMatteDerived writes a complete derived sequence <clipDir>/<name> of
// frames PNG stand-ins of pngSize bytes, every mtime set to mtime.
func putMatteDerived(t *testing.T, clipDir, name string, frames, pngSize int, mtime time.Time) string {
	t.Helper()
	dir := MatteDerivedDir(clipDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	for i := 1; i <= frames; i++ {
		p := filepath.Join(dir, matte.FrameFile(i))
		if err := os.WriteFile(p, bytes.Repeat([]byte("d"), pngSize), 0o644); err != nil {
			t.Fatal(err)
		}
		os.Chtimes(p, mtime, mtime)
	}
	os.Chtimes(dir, mtime, mtime)
	return dir
}

// putMatteDerivedTmp writes a derive in progress <clipDir>/<name>.tmp
// holding one frame of pngSize bytes, every mtime set to mtime.
func putMatteDerivedTmp(t *testing.T, clipDir, name string, pngSize int, mtime time.Time) string {
	t.Helper()
	dir := MatteDerivedTmpDir(clipDir, name)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, matte.FrameFile(1))
	if err := os.WriteFile(p, bytes.Repeat([]byte("t"), pngSize), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mtime, mtime)
	os.Chtimes(dir, mtime, mtime)
	return dir
}

// putPromptMask writes one prompt-mask memo entry of size bytes as name
// under the memo dir, mtime set.
func putPromptMask(t *testing.T, st *Store, name string, size int, mtime time.Time) string {
	t.Helper()
	dir := st.MattePromptsMemoDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(dir, name)
	if err := os.WriteFile(p, bytes.Repeat([]byte("m"), size), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, mtime, mtime)
	return p
}

// pinMtime sets dir's mtime after its children were made. A child mkdir /
// rename / remove refreshes the parent's mtime (which is why a derive
// counts as a use of its clip), but NTFS applies that refresh lazily — a
// listing may show it only after a later open — so the tests pin what they
// mean, as putMatteClip does.
func pinMtime(t *testing.T, dir string, mtime time.Time) {
	t.Helper()
	if err := os.Chtimes(dir, mtime, mtime); err != nil {
		t.Fatal(err)
	}
}

func TestMatteDerivedLayout(t *testing.T) {
	st := newTestStore(t)
	clip := st.MatteDir(matteKeyOf('a'))

	for _, c := range []struct{ mode, want string }{
		{"light", "stab-light"},
		{"strong", "stab-strong"},
		{"../x", "stab-x"}, // sanitised like every matte path element
		{"", "stab-invalid"},
	} {
		if got := MatteStabName(c.mode); got != c.want {
			t.Errorf("MatteStabName(%q) = %q, want %q", c.mode, got, c.want)
		}
	}
	edgeKey := "5600024376f572a557870a5eb0afb1e5961636bef4e1e22132025467d0f03333"
	for _, c := range []struct {
		edge, key string
		radius    int
		want      string
	}{
		{"birefnet-lite", edgeKey, 3, "gated-birefnet-lite-5600024376f5-r3"},
		{"isnet-anime", "abcdef", 5, "gated-isnet-anime-abcdef-r5"},
		{"", edgeKey, 3, "gated-invalid-5600024376f5-r3"},          // the recipe's "" is never a dir name
		{"birefnet-lite", "", 3, "gated-birefnet-lite-invalid-r3"}, // nor is an unknown edge key
	} {
		if got := MatteGatedName(c.edge, c.key, c.radius); got != c.want {
			t.Errorf("MatteGatedName(%q, %q, %d) = %q, want %q", c.edge, c.key, c.radius, got, c.want)
		}
	}
	// Two edge memos of one model (another size / precision / weights) name two gates.
	if a, b := MatteGatedName("birefnet-lite", "aaaa", 3), MatteGatedName("birefnet-lite", "bbbb", 3); a == b {
		t.Errorf("gates of two edge memos share a name: %q", a)
	}

	if got, w := MatteDerivedDir(clip, "stab-light"), filepath.Join(clip, "stab-light"); got != w {
		t.Errorf("MatteDerivedDir = %q, want %q", got, w)
	}
	if got, w := MatteDerivedTmpDir(clip, "stab-light"), filepath.Join(clip, "stab-light.tmp"); got != w {
		t.Errorf("MatteDerivedTmpDir = %q, want %q", got, w)
	}
	// A derived name cannot leave the clip dir.
	if got, w := MatteDerivedDir(clip, "../../x"), filepath.Join(clip, "x"); got != w {
		t.Errorf("MatteDerivedDir(traversal) = %q, want %q", got, w)
	}
	if got, w := MatteDerivedDir(clip, ""), filepath.Join(clip, "invalid"); got != w {
		t.Errorf("MatteDerivedDir(\"\") = %q, want %q", got, w)
	}

	for name, want := range map[string]bool{
		"stab-light.tmp": true,
		"gated-birefnet-lite-5600024376f5-r3.tmp": true,
		"stab-light": false,
		".tmp":       false, // no name
		"matte.json": false,
		"000001.png": false,
	} {
		if got := IsMatteDerivedTmp(name); got != want {
			t.Errorf("IsMatteDerivedTmp(%q) = %v, want %v", name, got, want)
		}
	}

	if got, w := st.MattePromptsMemoDir(), filepath.Join(st.Scratch, "matte-prompts"); got != w {
		t.Errorf("MattePromptsMemoDir = %q, want %q", got, w)
	}
	if exists(t, st.MattePromptsMemoDir()) || exists(t, filepath.Join(st.Root, "mattes")) {
		t.Error("the path helpers must not create directories")
	}
}

func TestSweepMattesDerivedDirs(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	src := putBlobAt(t, st, "source", "src.mov", now) // fresh: always alive

	// An expired clip goes with its derived dirs, however fresh those are.
	expired := putMatteClip(t, st, matteKeyOf('a'), src.Hash, 2, 10, old)
	expiredStab := putMatteDerived(t, expired, MatteStabName("light"), 2, 10, now)
	expiredGated := putMatteDerived(t, expired, MatteGatedName("birefnet-lite", "5600024376f5", 3), 2, 10, now)
	pinMtime(t, expired, old)

	// A live clip keeps its derived dirs whatever their own age; an
	// abandoned derive temp beside them is junk and goes on its own, a
	// fresh one is a derive in progress and stays.
	live := putMatteClip(t, st, matteKeyOf('b'), src.Hash, 2, 10, now)
	liveStab := putMatteDerived(t, live, MatteStabName("light"), 2, 10, old)
	liveGated := putMatteDerived(t, live, MatteGatedName("isnet-anime", "f15622d853e8", 3), 2, 10, old)
	junkTmp := putMatteDerivedTmp(t, live, MatteStabName("strong"), 10, old)
	freshTmp := putMatteDerivedTmp(t, live, MatteGatedName("birefnet-lite", "5600024376f5", 3), 10, now)
	// A derive that started long ago but is still writing frames.
	longTmp := putMatteDerivedTmp(t, live, MatteStabName("light"), 10, old)
	if err := os.WriteFile(filepath.Join(longTmp, matte.FrameFile(2)), []byte("new"), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(longTmp, old, old)

	// An expired clip with a derive in progress is in progress itself: the
	// derive is reading it right now.
	deriving := putMatteClip(t, st, matteKeyOf('c'), src.Hash, 2, 10, old)
	derivingTmp := putMatteDerivedTmp(t, deriving, MatteStabName("light"), 10, now)
	pinMtime(t, deriving, old)
	// So is one whose source is gone.
	dead := putMatteClip(t, st, matteKeyOf('d'), matteKeyOf('9'), 2, 10, now)
	deadTmp := putMatteDerivedTmp(t, dead, MatteStabName("light"), 10, now)
	pinMtime(t, dead, now)

	// Entries of a clip dir that are not derive temps are not the
	// sweeper's: a file ending in .tmp, a dir without the suffix.
	strayFile := filepath.Join(live, "notes.tmp")
	putOldFile(t, strayFile, old)
	strayDir := filepath.Join(live, "stab-light-old")
	putOldFile(t, filepath.Join(strayDir, "x"), old)
	pinMtime(t, live, now)

	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	want(t, false, "derived", expired, expiredStab, expiredGated, junkTmp)
	want(t, true, "derived", live, liveStab, liveGated, freshTmp, longTmp, deriving, derivingTmp, dead, deadTmp,
		strayFile, strayDir)
	for _, p := range []string{liveStab, liveGated} {
		if !exists(t, filepath.Join(p, matte.FrameFile(2))) {
			t.Errorf("derived frames of %s touched", filepath.Base(p))
		}
	}

	// Once the derive temps are abandoned, the expired clip and the
	// dead-source clip go by their own rules; the derived temps go with them.
	for _, p := range []string{derivingTmp, deadTmp} {
		os.Chtimes(filepath.Join(p, matte.FrameFile(1)), old, old)
		os.Chtimes(p, old, old)
	}
	pinMtime(t, deriving, old)
	pinMtime(t, dead, now)
	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatalf("Sweep #2: %v", err)
	}
	want(t, false, "derived #2", deriving, derivingTmp, dead, deadTmp)
	want(t, true, "derived #2", live, liveStab, liveGated, freshTmp, longTmp)
}

func TestSweepMattesDerivedSizeAccounting(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	fresh := putBlobAt(t, st, strings.Repeat("f", 10), "fresh.bin", now) // never evictable

	clip := putMatteClip(t, st, matteKeyOf('a'), fresh.Hash, 2, 50, now.Add(-10*time.Hour))
	stab := putMatteDerived(t, clip, MatteStabName("light"), 2, 40, old)
	junkTmp := putMatteDerivedTmp(t, clip, MatteStabName("strong"), 1000, old)
	pinMtime(t, clip, now.Add(-10*time.Hour))

	fi, err := os.Stat(st.metaPath(fresh.Hash))
	if err != nil {
		t.Fatal(err)
	}
	blobsTotal := 10 + fi.Size()
	clipTotal := dirSize(clip) - dirSize(junkTmp) // the junk temp is not the clip's weight

	// 1. A cap exactly the blob plus the clip with its derived dir: the
	// junk temp goes (age rule), the clip and its derived dir stay — the
	// temp's bytes were not counted against the cap.
	if err := st.Sweep(context.Background(), 0, blobsTotal+clipTotal); err != nil {
		t.Fatalf("Sweep #1: %v", err)
	}
	want(t, false, "#1", junkTmp)
	want(t, true, "#1", clip, stab)

	// 2. One byte less: the clip (with its derived dir) is what goes.
	if err := st.Sweep(context.Background(), 0, blobsTotal+clipTotal-1); err != nil {
		t.Fatalf("Sweep #2: %v", err)
	}
	want(t, false, "#2", clip, stab)
	if _, err := st.GetBlob(fresh.Hash); err != nil {
		t.Errorf("#2: fresh blob evicted: %v", err)
	}

	// 3. A clip with a derive in progress is never evicted by the size
	// pass, whatever the cap; once the derive has landed (rename), it is.
	clip2 := putMatteClip(t, st, matteKeyOf('b'), fresh.Hash, 2, 50, now.Add(-10*time.Hour))
	tmp2 := putMatteDerivedTmp(t, clip2, MatteStabName("light"), 10, now)
	pinMtime(t, clip2, now.Add(-10*time.Hour))
	if err := st.Sweep(context.Background(), 0, 1); err != nil {
		t.Fatalf("Sweep #3: %v", err)
	}
	want(t, true, "#3 deriving", clip2, tmp2)
	if err := os.Rename(tmp2, MatteDerivedDir(clip2, MatteStabName("light"))); err != nil {
		t.Fatal(err)
	}
	pinMtime(t, clip2, now.Add(-10*time.Hour))
	if err := st.Sweep(context.Background(), 0, 1); err != nil {
		t.Fatalf("Sweep #4: %v", err)
	}
	want(t, false, "#4 derived", clip2)

	// 4. A dead-source clip with a derive in progress survives the size
	// pass's dead-weight rule too (as the age pass's).
	dying := putBlobAt(t, st, strings.Repeat("o", 200), "old.bin", now.Add(-5*time.Hour))
	clip3 := putMatteClip(t, st, matteKeyOf('c'), dying.Hash, 1, 10, now)
	tmp3 := putMatteDerivedTmp(t, clip3, MatteStabName("light"), 10, now)
	pinMtime(t, clip3, now)
	if err := st.Sweep(context.Background(), 0, blobsTotal); err != nil {
		t.Fatalf("Sweep #5: %v", err)
	}
	if _, err := st.GetBlob(dying.Hash); err == nil {
		t.Fatal("#5: the old blob should have been evicted")
	}
	want(t, true, "#5 deriving from an evicted source", clip3, tmp3)
}

func TestSweepProtectsDerivedDirs(t *testing.T) {
	st := newTestStore(t)
	now := time.Now()
	old := now.Add(-48 * time.Hour)
	src := putBlobAt(t, st, "source", "src.mov", now)

	// A hold on the clip dir covers its derived dirs and an abandoned
	// derive temp under it.
	held := putMatteClip(t, st, matteKeyOf('a'), src.Hash, 1, 10, old)
	heldStab := putMatteDerived(t, held, MatteStabName("light"), 1, 10, old)
	heldTmp := putMatteDerivedTmp(t, held, MatteStabName("strong"), 10, old)
	pinMtime(t, held, old)
	releaseClip := st.Protect(held)

	// A hold on the derived dir alone (what a consumer reading
	// <clipdir>/stab-light/%06d.png may take) keeps the clip dir: removing
	// the clip would remove the held sequence.
	viaDerived := putMatteClip(t, st, matteKeyOf('b'), src.Hash, 1, 10, old)
	viaDerivedStab := putMatteDerived(t, viaDerived, MatteStabName("light"), 1, 10, old)
	pinMtime(t, viaDerived, old)
	releaseDerived := st.Protect(viaDerivedStab)
	// Likewise for a dead-source clip.
	deadViaDerived := putMatteClip(t, st, matteKeyOf('c'), matteKeyOf('9'), 1, 10, now)
	deadStab := putMatteDerived(t, deadViaDerived, MatteStabName("light"), 1, 10, now)
	pinMtime(t, deadViaDerived, now)
	releaseDead := st.Protect(deadStab)

	// An unprotected expired clip beside them goes.
	loose := putMatteClip(t, st, matteKeyOf('d'), src.Hash, 1, 10, old)
	putMatteDerived(t, loose, MatteStabName("light"), 1, 10, old)
	pinMtime(t, loose, old)

	for _, pass := range []struct {
		name string
		ttl  time.Duration
		max  int64
	}{{"age", 24 * time.Hour, 0}, {"size", 0, 1}} {
		if err := st.Sweep(context.Background(), pass.ttl, pass.max); err != nil {
			t.Fatalf("Sweep (%s): %v", pass.name, err)
		}
		want(t, true, "protected ("+pass.name+")", held, heldStab, heldTmp, viaDerived, viaDerivedStab, deadViaDerived, deadStab)
		want(t, false, "unprotected ("+pass.name+")", loose)
	}

	releaseClip()
	releaseDerived()
	releaseDead()
	if err := st.Sweep(context.Background(), 24*time.Hour, 0); err != nil {
		t.Fatalf("Sweep (released): %v", err)
	}
	want(t, false, "released", held, heldTmp, viaDerived, deadViaDerived)
}

func TestSweepMattePrompts(t *testing.T) {
	st := newTestStore(t)
	ctx := context.Background()
	now := time.Now()

	// No memo yet: nothing to do, nothing created.
	if err := st.SweepMattePrompts(ctx); err != nil {
		t.Fatalf("SweepMattePrompts without a memo: %v", err)
	}
	if exists(t, st.MattePromptsMemoDir()) {
		t.Fatal("SweepMattePrompts created the memo dir")
	}

	// Entries e0..e9, e0 the oldest by mtime; e2 was hit just now (jobs
	// touches a memo on every read), so it ranks newest.
	var entries []string
	for i := range 10 {
		entries = append(entries, putPromptMask(t, st, "e"+strconv.Itoa(i)+".png", 10, now.Add(time.Duration(i-20)*time.Minute)))
	}
	os.Chtimes(entries[2], now, now)
	// A write in progress and one abandoned, beside a subdir and a
	// dot-less temp: never counted, the abandoned one removed.
	liveTemp := putPromptMask(t, st, ".memo-live", 10, now)
	junkTemp := putPromptMask(t, st, ".memo-junk", 10, now.Add(-2*time.Hour))
	sub := filepath.Join(st.MattePromptsMemoDir(), "sub")
	putOldFile(t, filepath.Join(sub, "x.png"), now.Add(-72*time.Hour))

	// Count bound 7 of 10: the three oldest unprotected go — e0, e1 and,
	// e2 being hit and e3 protected, e4.
	releaseE3 := st.Protect(entries[3])
	if err := st.sweepMattePrompts(ctx, now, 7, 0); err != nil {
		t.Fatalf("sweepMattePrompts (count): %v", err)
	}
	want(t, false, "count", entries[0], entries[1], entries[4], junkTemp)
	want(t, true, "count", entries[2], entries[3], entries[5], entries[6], entries[7], entries[8], entries[9], liveTemp, sub)
	releaseE3()

	// Byte bound 45 of 70: e3, e5 and e6 go (the temps and the subdir do
	// not count).
	if err := st.sweepMattePrompts(ctx, now, -1, 45); err != nil {
		t.Fatalf("sweepMattePrompts (bytes): %v", err)
	}
	want(t, false, "bytes", entries[3], entries[5], entries[6])
	want(t, true, "bytes", entries[2], entries[7], entries[8], entries[9], liveTemp)

	// Within both bounds: untouched.
	if err := st.sweepMattePrompts(ctx, now, 4, 40); err != nil {
		t.Fatalf("sweepMattePrompts (within): %v", err)
	}
	want(t, true, "within", entries[2], entries[7], entries[8], entries[9], liveTemp)

	// The exported bounds, through Sweep: one entry over the count goes.
	for i := range MattePromptsMaxEntries - 3 {
		putPromptMask(t, st, "f"+strconv.Itoa(i)+".png", 1, now.Add(time.Duration(i)*time.Second))
	}
	if err := st.Sweep(ctx, 0, 0); err != nil {
		t.Fatalf("Sweep: %v", err)
	}
	want(t, false, "Sweep", entries[7]) // the oldest of the 257
	want(t, true, "Sweep", entries[2], entries[8], entries[9], liveTemp)
	left, err := os.ReadDir(st.MattePromptsMemoDir())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range left {
		if e.Type().IsRegular() && !strings.HasPrefix(e.Name(), ".") {
			n++
		}
	}
	if n != MattePromptsMaxEntries {
		t.Errorf("Sweep left %d entries, want %d", n, MattePromptsMaxEntries)
	}

	// A cancelled sweep stops.
	cctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := st.sweepMattePrompts(cctx, now, 0, 0); err == nil {
		t.Error("sweepMattePrompts ignored a cancelled ctx")
	}
}
