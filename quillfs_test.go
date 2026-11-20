package quillfs

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"testing"
	"testing/fstest"
	"time"
)

func open(t testing.TB, dir string, segSize int64) *FS {
	t.Helper()
	fsys, err := Open(dir, &Options{SegmentSize: segSize})
	if err != nil {
		t.Fatal(err)
	}
	return fsys
}

// abandon drops a volume the way a killed process does: no checkpoint, no
// flush, nothing but what is already in the mapped segments.
func (fsys *FS) abandon() {
	fsys.stopMaintenance()
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	fsys.closed = true
	fsys.closeFiles()
}

// tree returns every path of a volume with its contents ("dir" for a
// directory).
func tree(t testing.TB, fsys *FS) map[string]string {
	t.Helper()
	out := map[string]string{}
	var walk func(dir string)
	walk = func(dir string) {
		entries, err := fsys.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			p := dir + "/" + e.Name()
			if e.IsDir() {
				out[p] = "dir"
				walk(p)
				continue
			}
			data, err := fsys.ReadFile(p)
			if err != nil {
				t.Fatal(err)
			}
			out[p] = "file:" + string(data)
		}
	}
	walk("")
	return out
}

// osTree is tree for a real directory.
func osTree(t testing.TB, root string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == root {
			return err
		}
		rel := "/" + filepath.ToSlash(p[len(root)+1:])
		if d.IsDir() {
			out[rel] = "dir"
			return nil
		}
		data, err := os.ReadFile(p)
		out[rel] = "file:" + string(data)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func sameTree(t testing.TB, what string, got, want map[string]string) {
	t.Helper()
	for p, w := range want {
		if g, ok := got[p]; !ok {
			t.Errorf("%s: %s is missing", what, p)
		} else if g != w {
			t.Errorf("%s: %s differs: got %d bytes, want %d", what, p, len(g), len(w))
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s: unexpected %s", what, p)
		}
	}
}

func errno(err error) syscall.Errno {
	var e syscall.Errno
	if err == nil || !errors.As(err, &e) {
		return 0
	}
	return e
}

// TestAgainstRealFileSystem runs the same random operations on a volume and
// on a real directory and requires the same outcome every time: the same
// error, the same data read back, and in the end the same tree. Along the
// way the volume is closed and reopened, dropped without closing, and
// compacted, none of which may change anything.
func TestAgainstRealFileSystem(t *testing.T) {
	for seed := int64(1); seed <= 4; seed++ {
		t.Run(fmt.Sprint("seed", seed), func(t *testing.T) {
			rng := rand.New(rand.NewSource(seed))
			dir, real := t.TempDir(), t.TempDir()
			fsys := open(t, dir, 64<<10)
			defer func() { fsys.Close() }()
			name := func() string {
				parts := []string{"a", "b", "c", "d"}
				p := parts[rng.Intn(4)]
				for rng.Intn(3) == 0 {
					p += "/" + parts[rng.Intn(4)]
				}
				return p
			}
			blob := func() []byte {
				b := make([]byte, rng.Intn(9000))
				rng.Read(b)
				return b
			}
			for i := 0; i < 3000; i++ {
				var got, want error
				what := ""
				switch op := rng.Intn(100); {
				case op < 12:
					n := name()
					what = "mkdir " + n
					got, want = fsys.Mkdir(n, 0o755), os.Mkdir(filepath.Join(real, n), 0o755)
				case op < 32:
					n, data := name(), blob()
					what = "writefile " + n
					got, want = fsys.WriteFile(n, data, 0o644), os.WriteFile(filepath.Join(real, n), data, 0o644)
				case op < 44: // overwrite part of a file, possibly past its end
					n, data, off := name(), blob(), int64(rng.Intn(12000))
					what = fmt.Sprintf("writeat %s +%d", n, off)
					f, err := fsys.OpenFile(n, os.O_WRONLY, 0)
					rf, rerr := os.OpenFile(filepath.Join(real, n), os.O_WRONLY, 0)
					got, want = err, rerr
					if err == nil && rerr == nil {
						_, got = f.WriteAt(data, off)
						_, want = rf.WriteAt(data, off)
					}
					if err == nil {
						f.Close()
					}
					if rerr == nil {
						rf.Close()
					}
				case op < 52: // append
					n, data := name(), blob()
					what = "append " + n
					f, err := fsys.OpenFile(n, os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
					rf, rerr := os.OpenFile(filepath.Join(real, n), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644)
					got, want = err, rerr
					if err == nil && rerr == nil {
						_, got = f.Write(data)
						_, want = rf.Write(data)
					}
					if err == nil {
						f.Close()
					}
					if rerr == nil {
						rf.Close()
					}
				case op < 60:
					n, size := name(), int64(rng.Intn(15000))
					what = fmt.Sprintf("truncate %s %d", n, size)
					got, want = fsys.Truncate(n, size), os.Truncate(filepath.Join(real, n), size)
				case op < 72:
					n := name()
					what = "remove " + n
					got, want = fsys.Remove(n), os.Remove(filepath.Join(real, n))
				case op < 86:
					a, b := name(), name()
					what = "rename " + a + " " + b
					// rename(2) itself: os.Rename adds a check of its own
					// that refuses any existing directory as the target.
					got, want = fsys.Rename(a, b), syscall.Rename(filepath.Join(real, a), filepath.Join(real, b))
				case op < 92:
					n := name()
					what = "readfile " + n
					g, gerr := fsys.ReadFile(n)
					w, werr := os.ReadFile(filepath.Join(real, n))
					got, want = gerr, werr
					if gerr == nil && werr == nil && !bytes.Equal(g, w) {
						t.Fatalf("step %d: %s: contents differ (%d and %d bytes)", i, what, len(g), len(w))
					}
				case op < 95:
					n := name()
					what = "stat " + n
					g, gerr := fsys.Stat(n)
					w, werr := os.Stat(filepath.Join(real, n))
					got, want = gerr, werr
					if gerr == nil && werr == nil && (g.IsDir() != w.IsDir() || !g.IsDir() && g.Size() != w.Size()) {
						t.Fatalf("step %d: %s: got dir=%v size=%d, want dir=%v size=%d", i, what, g.IsDir(), g.Size(), w.IsDir(), w.Size())
					}
				case op < 97:
					what = "close and reopen"
					if err := fsys.Close(); err != nil {
						t.Fatal(err)
					}
					fsys = open(t, dir, 0)
				case op < 99:
					what = "die and reopen"
					fsys.abandon()
					fsys = open(t, dir, 0)
				default:
					what = "compact"
					_, got = fsys.Compact()
				}
				if errno(got) != errno(want) || (got == nil) != (want == nil) {
					t.Fatalf("step %d: %s: got %v, a real file system says %v", i, what, got, want)
				}
			}
			sameTree(t, "at the end", tree(t, fsys), osTree(t, real))
		})
	}
}

func TestIOFS(t *testing.T) {
	fsys := open(t, t.TempDir(), 64<<10)
	defer fsys.Close()
	fsys.MkdirAll("docs/deep/er", 0o755)
	fsys.Mkdir("empty", 0o755)
	big := bytes.Repeat([]byte("0123456789abcdef"), 20000) // spans several segments
	for name, data := range map[string][]byte{"hello.txt": []byte("hello"), "docs/a.txt": big, "docs/deep/er/b": nil} {
		if err := fsys.WriteFile(name, data, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := fstest.TestFS(fsys.IOFS(), "hello.txt", "docs/a.txt", "docs/deep/er/b", "empty"); err != nil {
		t.Fatal(err)
	}
}

func TestHandles(t *testing.T) {
	fsys := open(t, t.TempDir(), 64<<10)
	defer fsys.Close()
	f, err := fsys.Create("f")
	if err != nil {
		t.Fatal(err)
	}
	f.Write([]byte("hello world"))
	if _, err := f.WriteAt([]byte("HOLE"), 100); err != nil {
		t.Fatal(err)
	}
	f.Seek(0, io.SeekStart)
	got, _ := io.ReadAll(f)
	want := append([]byte("hello world"), make([]byte, 89)...)
	want = append(want, "HOLE"...)
	if !bytes.Equal(got, want) {
		t.Fatalf("a write past the end must leave zeros in between: got %q", got)
	}
	// Removed while open: the handle keeps working, the name is gone.
	if err := fsys.Remove("f"); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Stat("f"); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("stat after remove = %v", err)
	}
	buf := make([]byte, 5)
	if _, err := f.ReadAt(buf, 0); err != nil || string(buf) != "hello" {
		t.Fatalf("read through an open handle after remove = %q, %v", buf, err)
	}
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if u := fsys.Usage(); u.Live != 0 || u.Files != 0 {
		t.Fatalf("after the last close nothing should be live: %+v", u)
	}
	// Flags.
	if _, err := fsys.OpenFile("new", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.OpenFile("new", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); !errors.Is(err, fs.ErrExist) {
		t.Fatalf("O_EXCL on an existing file = %v", err)
	}
	r, _ := fsys.Open("new")
	if _, err := r.Write([]byte("x")); errno(err) != syscall.EBADF {
		t.Fatalf("write through a read-only handle = %v", err)
	}
	// A second process cannot open the volume.
	if _, err := Open(fsys.dir, nil); err == nil {
		t.Fatal("opening a volume twice must fail")
	}
}

// TestTornTail cuts the log inside the last operation, as a crash in the
// middle of it would, and requires that the volume opens to a state in which
// each record of that operation is there completely or not at all. A small
// file is cut at every byte. A large one, which is written with pwrite, is
// cut at every 61st.
func TestTornTail(t *testing.T) {
	t.Run("4KiB", func(t *testing.T) { tornTail(t, 64<<10, 3000, 1) })
	t.Run("100KiB", func(t *testing.T) { tornTail(t, 512<<10, 100<<10, 61) })
}

func tornTail(t *testing.T, segSize int64, size int, step int64) {
	dir := t.TempDir()
	fsys := open(t, dir, segSize)
	fsys.MkdirAll("d", 0o755)
	fsys.WriteFile("d/kept", bytes.Repeat([]byte("k"), 5000), 0o644)
	before := tree(t, fsys)
	start := fsys.head.tail
	last := bytes.Repeat([]byte("L"), size)
	fsys.WriteFile("d/last", last, 0o644)
	end := fsys.head.tail
	seg := fsys.head.path
	fsys.abandon()
	whole, err := os.ReadFile(seg)
	if err != nil {
		t.Fatal(err)
	}
	outcomes := map[string]int{}
	for cut := start; cut <= end; cut += step {
		for _, damage := range []string{"zeros", "garbage"} {
			img := append([]byte{}, whole...)
			for i := cut; i < end; i++ {
				img[i] = 0
				if damage == "garbage" {
					img[i] = byte(i*7 + 1)
				}
			}
			if err := os.WriteFile(seg, img, 0o600); err != nil {
				t.Fatal(err)
			}
			re, err := Open(dir, nil)
			if err != nil {
				t.Fatalf("cut at %d (%s): %v", cut, damage, err)
			}
			got := tree(t, re)
			content, exists := got["/d/last"]
			delete(got, "/d/last")
			sameTree(t, fmt.Sprintf("cut at %d", cut), got, before)
			switch {
			case !exists:
				outcomes["absent"]++
			case content == "file:":
				outcomes["created, not written"]++
			case content == "file:"+string(last):
				outcomes["complete"]++
			default:
				t.Fatalf("cut at %d (%s): d/last holds %d bytes, a torn write", cut, damage, len(content)-5)
			}
			// The volume must be usable afterwards, and stay consistent.
			if err := re.WriteFile("d/after", []byte("ok"), 0o644); err != nil {
				t.Fatal(err)
			}
			re.abandon()
			re2, err := Open(dir, nil)
			if err != nil {
				t.Fatalf("cut at %d: reopening after a write: %v", cut, err)
			}
			if data, err := re2.ReadFile("d/after"); err != nil || string(data) != "ok" {
				t.Fatalf("cut at %d: write after recovery lost: %q, %v", cut, data, err)
			}
			re2.abandon()
		}
	}
	if outcomes["absent"] == 0 || outcomes["created, not written"] == 0 {
		t.Errorf("expected cuts that leave the file absent and cuts that leave it empty, got %v", outcomes)
	}
	t.Logf("%d cut points: %v", (end-start)/step+1, outcomes)
}

// TestPowerLoss imitates what a power cut leaves on disk: of everything
// written since the last Sync, some pages made it and some did not, and
// segment files created since then may be missing or empty. The volume has
// to open anyway, to the state after some operation at or past the Sync, and
// what did not survive must never come back, however much is written
// afterwards.
func TestPowerLoss(t *testing.T) {
	const segSize, page = 64 << 10, 4096
	dir := t.TempDir()
	fsys, err := Open(dir, &Options{SegmentSize: segSize, NoAutoCompact: true})
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(7))
	var states []map[string]string // the tree after each operation
	// Every operation here is a single record, so the states a recovery may
	// land on are exactly the states between operations.
	op := func(i int) {
		name := fmt.Sprintf("d%d/f%d", i%3, rng.Intn(12))
		switch rng.Intn(8) {
		case 0:
			fsys.Remove(name)
		case 1:
			fsys.Rename(name, fmt.Sprintf("d%d/g%d", rng.Intn(3), rng.Intn(12)))
		case 2:
			fsys.Truncate(name, int64(rng.Intn(4000)))
		default:
			f, err := fsys.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0)
			if err != nil { // not there yet: creating it is this operation
				if f, err = fsys.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644); err == nil {
					f.Close()
				}
				break
			}
			data := make([]byte, 500+rng.Intn(6000))
			rng.Read(data)
			f.Write(data)
			f.Close()
		}
		states = append(states, tree(t, fsys))
	}
	for d := 0; d < 3; d++ {
		fsys.Mkdir(fmt.Sprintf("d%d", d), 0o755)
	}
	for i := 0; i < 40; i++ {
		op(i)
	}
	if err := fsys.Sync(); err != nil {
		t.Fatal(err)
	}
	syncedState := len(states) - 1
	syncedSeq, syncedOff := fsys.head.seq, fsys.head.tail
	for i := 40; i < 260; i++ { // several segments past the sync
		op(i)
	}
	if fsys.head.seq < syncedSeq+3 {
		t.Fatalf("the unsynced part spans only %d segments, the test needs more", fsys.head.seq-syncedSeq)
	}
	fsys.abandon()
	pristine := filepath.Join(t.TempDir(), "pristine")
	copyDir(t, dir, pristine)

	matches := func(got map[string]string) int {
		for j := len(states) - 1; j >= syncedState; j-- {
			if len(got) == len(states[j]) {
				same := true
				for p, c := range states[j] {
					if got[p] != c {
						same = false
						break
					}
				}
				if same {
					return j
				}
			}
		}
		return -1
	}
	recoveredAt := map[int]int{}
	for trial := 0; trial < 150; trial++ {
		work := filepath.Join(t.TempDir(), "vol")
		copyDir(t, pristine, work)
		segs, _ := filepath.Glob(filepath.Join(work, "*.seg"))
		sort.Strings(segs)
		for _, path := range segs {
			seq64, _ := strconv.ParseUint(filepath.Base(path)[:8], 10, 32)
			seq := uint32(seq64)
			if seq < syncedSeq {
				continue
			}
			// A segment created after the sync may have no directory entry.
			if seq > syncedSeq && rng.Intn(6) == 0 {
				os.Remove(path)
				continue
			}
			img, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			first := int64(0)
			if seq == syncedSeq {
				first = (syncedOff + page - 1) / page // the page holding the synced tail is on disk
			}
			for pg := first; pg < segSize/page; pg++ {
				if rng.Intn(4) == 0 { // this page never reached the disk
					clear(img[pg*page : (pg+1)*page])
				}
			}
			if err := os.WriteFile(path, img, 0o600); err != nil {
				t.Fatal(err)
			}
		}

		re, err := Open(work, &Options{NoAutoCompact: true})
		if err != nil {
			t.Fatalf("trial %d: the volume does not open after a power cut: %v", trial, err)
		}
		got := tree(t, re)
		at := matches(got)
		if at < 0 {
			t.Fatalf("trial %d: the recovered tree is not the state after any operation since the sync", trial)
		}
		recoveredAt[at-syncedState]++
		// Write well past anything recovery could have cleared, then come
		// back: nothing that was lost may reappear.
		for i := 0; i < 40; i++ {
			name := fmt.Sprintf("after/f%d", i)
			re.MkdirAll("after", 0o755)
			data := bytes.Repeat([]byte{byte(i)}, 3000+i*50)
			if err := re.WriteFile(name, data, 0o644); err != nil {
				t.Fatal(err)
			}
			got["/after"] = "dir"
			got["/"+name] = "file:" + string(data)
		}
		re.abandon()
		re2, err := Open(work, &Options{NoAutoCompact: true})
		if err != nil {
			t.Fatalf("trial %d: reopening after writing on: %v", trial, err)
		}
		sameTree(t, fmt.Sprintf("trial %d, after writing on and reopening", trial), tree(t, re2), got)
		re2.abandon()
		if t.Failed() {
			return
		}
	}
	if len(recoveredAt) < 3 {
		t.Errorf("expected recoveries at several different points, got %v (operations past the sync -> trials)", recoveredAt)
	}
	t.Logf("150 power cuts; operations past the sync that survived -> trials: %v", recoveredAt)
}

func copyDir(t *testing.T, from, to string) {
	t.Helper()
	if err := os.MkdirAll(to, 0o700); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(from)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "LOCK" {
			continue
		}
		data, err := os.ReadFile(filepath.Join(from, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(to, e.Name()), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

// TestTinyFilesAreNotCompactedForever fills a volume with files so small
// that record headers take most of the space. Such segments are not garbage,
// and compaction must not keep copying them.
func TestTinyFilesAreNotCompactedForever(t *testing.T) {
	fsys := open(t, t.TempDir(), 64<<10)
	defer fsys.Close()
	const files = 60000
	for i := 0; i < files; i++ {
		if err := fsys.WriteFile(fmt.Sprintf("d%02d/f%05d", i%50, i), []byte("0123456789abcdef"), 0o644); err != nil {
			if i < 50 { // the directories do not exist yet
				fsys.MkdirAll(fmt.Sprintf("d%02d", i%50), 0o755)
				i--
				continue
			}
			t.Fatal(err)
		}
	}
	if _, err := fsys.Compact(); err != nil {
		t.Fatal(err)
	}
	fsys.mu.RLock()
	moved := fsys.relocated
	fsys.mu.RUnlock()
	if live := int64(files * 16); moved > live/10 {
		t.Errorf("compaction copied %d bytes of live data; the volume only holds %d, all of it live", moved, live)
	}
	if u := fsys.Usage(); u.Live != files*16 {
		t.Errorf("live = %d, want %d", u.Live, files*16)
	}
}

func TestCompactReclaimsSpace(t *testing.T) {
	dir := t.TempDir()
	// No background compaction: this test wants to see what one call does.
	fsys, err := Open(dir, &Options{SegmentSize: 256 << 10, NoAutoCompact: true})
	if err != nil {
		t.Fatal(err)
	}
	payload := func(i int) []byte { return bytes.Repeat([]byte{byte(i)}, 10000) }
	for i := 0; i < 300; i++ {
		fsys.WriteFile(fmt.Sprintf("f%03d", i), payload(i), 0o644)
	}
	for i := 0; i < 300; i++ {
		if i%10 != 0 {
			fsys.Remove(fmt.Sprintf("f%03d", i))
		}
	}
	before := fsys.Usage()
	removed, err := fsys.Compact()
	if err != nil {
		t.Fatal(err)
	}
	after := fsys.Usage()
	t.Logf("before: %d segments, %d live bytes; removed %d; after: %d segments", before.Segments, before.Live, removed, after.Segments)
	if after.Live != before.Live || after.Live != 30*10000 {
		t.Errorf("live bytes changed: %d -> %d", before.Live, after.Live)
	}
	if after.Segments*2 > before.Segments {
		t.Errorf("expected at least half of %d segments to go, %d are left", before.Segments, after.Segments)
	}
	onDisk, _ := filepath.Glob(filepath.Join(dir, "*.seg"))
	if len(onDisk) != after.Segments {
		t.Errorf("%d segment files on disk, usage says %d", len(onDisk), after.Segments)
	}
	check := func(fsys *FS, when string) {
		for i := 0; i < 300; i += 10 {
			if data, err := fsys.ReadFile(fmt.Sprintf("f%03d", i)); err != nil || !bytes.Equal(data, payload(i)) {
				t.Fatalf("%s: f%03d: %d bytes, %v", when, i, len(data), err)
			}
		}
	}
	check(fsys, "after compact")
	// Churn on: the volume must not grow without bound.
	for round := 0; round < 20; round++ {
		for i := 0; i < 300; i++ {
			if i%10 != 0 {
				fsys.WriteFile(fmt.Sprintf("f%03d", i), payload(i), 0o644)
			}
		}
		for i := 0; i < 300; i++ {
			if i%10 != 0 {
				fsys.Remove(fmt.Sprintf("f%03d", i))
			}
		}
		if _, err := fsys.Compact(); err != nil {
			t.Fatal(err)
		}
	}
	if u := fsys.Usage(); u.Segments > after.Segments+3 {
		t.Errorf("after 20 rounds of churn the volume has %d segments, it started this at %d", u.Segments, after.Segments)
	}
	fsys.abandon()
	re := open(t, dir, 0)
	defer re.Close()
	check(re, "after reopening without a clean close")
}

// TestAutoCompact overwrites the same files many times without ever calling
// Compact. The background goroutine has to keep the volume from growing with
// everything that was ever written.
func TestAutoCompact(t *testing.T) {
	fsys := open(t, t.TempDir(), 256<<10)
	defer fsys.Close()
	const rounds, files, size = 60, 100, 10000
	for r := 0; r < rounds; r++ {
		for i := 0; i < files; i++ {
			if err := fsys.WriteFile(fmt.Sprintf("f%03d", i), bytes.Repeat([]byte{byte(r)}, size), 0o644); err != nil {
				t.Fatal(err)
			}
		}
	}
	written := int64(rounds * files * size)
	deadline := time.Now().Add(5 * time.Second)
	for fsys.Usage().Disk > 8*files*size && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	u := fsys.Usage()
	t.Logf("wrote %d MiB in total, %d MiB are live, the volume takes %d MiB", written>>20, u.Live>>20, u.Disk>>20)
	if u.Disk > 8*files*size {
		t.Errorf("the volume takes %d bytes for %d live bytes", u.Disk, u.Live)
	}
	for i := 0; i < files; i++ {
		if data, err := fsys.ReadFile(fmt.Sprintf("f%03d", i)); err != nil || !bytes.Equal(data, bytes.Repeat([]byte{rounds - 1}, size)) {
			t.Fatalf("f%03d: %d bytes, %v", i, len(data), err)
		}
	}
}

// TestRecoveredTreeIsTheTreeThatWas hammers a volume from several goroutines,
// then drops it without a clean close and requires that replaying the log
// gives back exactly the tree that was in memory.
func TestRecoveredTreeIsTheTreeThatWas(t *testing.T) {
	dir := t.TempDir()
	fsys := open(t, dir, 128<<10)
	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			rng := rand.New(rand.NewSource(int64(w)))
			base := fmt.Sprintf("w%d", w%4) // writers share directories in pairs
			fsys.MkdirAll(base+"/sub", 0o755)
			for i := 0; i < 400; i++ {
				name := fmt.Sprintf("%s/f%d", base, rng.Intn(20))
				switch rng.Intn(6) {
				case 0, 1:
					data := make([]byte, rng.Intn(6000))
					rng.Read(data)
					fsys.WriteFile(name, data, 0o644)
				case 2:
					fsys.Remove(name)
				case 3:
					fsys.Rename(name, fmt.Sprintf("%s/sub/g%d", base, rng.Intn(20)))
				case 4:
					fsys.ReadFile(name)
				default:
					if f, err := fsys.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0); err == nil {
						f.Write([]byte("tail"))
						f.Close()
					}
				}
				if i%150 == 149 {
					fsys.Compact()
				}
			}
		}(w)
	}
	wg.Wait()
	want := tree(t, fsys)
	fsys.abandon()
	re := open(t, dir, 0)
	sameTree(t, "after replaying the log", tree(t, re), want)
	if err := re.Close(); err != nil {
		t.Fatal(err)
	}
	re = open(t, dir, 0)
	defer re.Close()
	sameTree(t, "after loading the checkpoint", tree(t, re), want)
}

// TestKill9 runs a writer in another process and kills it with SIGKILL at a
// random moment, several times over. Every write the writer reported as done
// must be there afterwards, complete.
func TestKill9(t *testing.T) {
	if dir := os.Getenv("QUILLFS_CRASH_WRITER"); dir != "" {
		crashWriter(dir)
		return
	}
	dir := t.TempDir()
	acked := map[int]bool{}
	next := 0
	for round := 0; round < 6; round++ {
		cmd := exec.Command(os.Args[0], "-test.run", "^TestKill9$")
		cmd.Env = append(os.Environ(), "QUILLFS_CRASH_WRITER="+dir, "QUILLFS_CRASH_START="+strconv.Itoa(next))
		out, err := cmd.StdoutPipe()
		if err != nil {
			t.Fatal(err)
		}
		cmd.Stderr = os.Stderr
		if err := cmd.Start(); err != nil {
			t.Fatal(err)
		}
		lines := make(chan int)
		go func() {
			defer close(lines)
			sc := bufio.NewScanner(out)
			for sc.Scan() {
				if n, err := strconv.Atoi(sc.Text()); err == nil {
					lines <- n
				}
			}
		}()
		kill := time.After(time.Duration(40+rand.Intn(120)) * time.Millisecond)
	reading:
		for {
			select {
			case n, ok := <-lines:
				if !ok {
					break reading
				}
				acked[n] = true
				next = max(next, n+1)
			case <-kill:
				cmd.Process.Signal(syscall.SIGKILL)
				kill = nil
			}
		}
		cmd.Wait()

		fsys, err := Open(dir, nil)
		if err != nil {
			t.Fatalf("round %d: opening after SIGKILL: %v", round, err)
		}
		for n := range acked {
			data, err := fsys.ReadFile(crashName(n))
			if err != nil || !bytes.Equal(data, crashPayload(n)) {
				t.Fatalf("round %d: write %d was reported done but reads back as %d bytes, %v", round, n, len(data), err)
			}
		}
		// Whatever else is there must be made of whole records: empty,
		// complete, or cut where one record of a large write ends.
		entries, _ := fsys.ReadDir("")
		for _, e := range entries {
			n, _ := strconv.Atoi(e.Name()[1:])
			info, _ := e.Info()
			if size, full := info.Size(), int64(len(crashPayload(n))); size != 0 && size != full && size%(64<<10) != 0 {
				t.Fatalf("round %d: %s has %d of %d bytes, a torn record", round, e.Name(), size, full)
			}
		}
		fsys.abandon() // do not checkpoint: the next round replays the whole log
	}
	if len(acked) < 100 {
		t.Fatalf("only %d writes were acknowledged; the writer did not get going", len(acked))
	}
	t.Logf("%d acknowledged writes survived 6 kills", len(acked))
}

func crashName(n int) string { return fmt.Sprintf("f%07d", n) }

// crashPayload is small for most writes and 150 KiB for every 40th, which is
// large enough to be written with pwrite, in several records.
func crashPayload(n int) []byte {
	size := 100 + n%5000
	if n%40 == 39 {
		size = 150 << 10
	}
	return bytes.Repeat([]byte{byte('a' + n%26)}, size)
}

func crashWriter(dir string) {
	fsys, err := Open(dir, &Options{SegmentSize: 256 << 10})
	if err != nil {
		fmt.Fprintln(os.Stderr, "writer:", err)
		os.Exit(1)
	}
	start, _ := strconv.Atoi(os.Getenv("QUILLFS_CRASH_START"))
	w := bufio.NewWriter(os.Stdout)
	for n := start; ; n++ {
		if err := fsys.WriteFile(crashName(n), crashPayload(n), 0o644); err != nil {
			fmt.Fprintln(os.Stderr, "writer:", err)
			os.Exit(1)
		}
		// Only now, after the call returned, is the write acknowledged.
		fmt.Fprintln(w, n)
		w.Flush()
	}
}

func TestSyncAndReopenKeepsEverything(t *testing.T) {
	dir := t.TempDir()
	fsys := open(t, dir, 64<<10)
	var names []string
	for i := 0; i < 50; i++ {
		name := fmt.Sprintf("dir%d/file%d", i%5, i)
		fsys.MkdirAll(filepath.Dir(name), 0o755)
		fsys.WriteFile(name, bytes.Repeat([]byte{byte(i)}, i*300), 0o644)
		names = append(names, name)
	}
	if err := fsys.Sync(); err != nil {
		t.Fatal(err)
	}
	want := tree(t, fsys)
	if err := fsys.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := fsys.Stat("dir0"); !errors.Is(err, fs.ErrClosed) {
		t.Fatalf("use after close = %v", err)
	}
	re := open(t, dir, 0)
	defer re.Close()
	sameTree(t, "after close and open", tree(t, re), want)
	sort.Strings(names)
	if _, err := re.ReadFile(names[0]); err != nil {
		t.Fatal(err)
	}
}
