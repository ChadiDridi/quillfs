// Command quillbench runs the same file workloads on a quillfs volume and on
// ordinary directories, through Go's os package, and prints the rates side by
// side. Everything runs in this one process, so the comparison is between
// file systems, not between programs.
//
//	quillbench -dirs /path/on/ext4,/dev/shm
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/ChadiDridi/quillfs"
)

// target is a file system under test.
type target interface {
	MkdirAll(name string) error
	WriteFile(name string, data []byte) error
	ReadFile(name string) ([]byte, error)
	Stat(name string) error
	Remove(name string) error
	Rename(from, to string) error
	ReadDir(name string) (int, error)
	Open(name string, flag int) (handle, error)
	Reclaim() // give back the space of deleted files, where that is a separate step
	Close()
}

type handle interface {
	io.WriterAt
	io.Writer
	io.ReaderAt
	Truncate(size int64) error
	Sync() error
	Close() error
}

type osTarget struct{ root string }

func (t osTarget) p(name string) string                 { return t.root + "/" + name }
func (t osTarget) MkdirAll(name string) error           { return os.MkdirAll(t.p(name), 0o755) }
func (t osTarget) Stat(name string) error               { _, err := os.Stat(t.p(name)); return err }
func (t osTarget) Remove(name string) error             { return os.Remove(t.p(name)) }
func (t osTarget) Rename(from, to string) error         { return os.Rename(t.p(from), t.p(to)) }
func (t osTarget) Reclaim()                             {}
func (t osTarget) Close()                               { os.RemoveAll(t.root) }
func (t osTarget) ReadFile(name string) ([]byte, error) { return os.ReadFile(t.p(name)) }
func (t osTarget) WriteFile(name string, data []byte) error {
	return os.WriteFile(t.p(name), data, 0o644)
}
func (t osTarget) ReadDir(name string) (int, error) {
	entries, err := os.ReadDir(t.p(name))
	return len(entries), err
}
func (t osTarget) Open(name string, flag int) (handle, error) {
	return os.OpenFile(t.p(name), flag, 0o644)
}

type quillTarget struct {
	fsys *quillfs.FS
	dir  string
}

func (t quillTarget) MkdirAll(name string) error           { return t.fsys.MkdirAll(name, 0o755) }
func (t quillTarget) Stat(name string) error               { _, err := t.fsys.Stat(name); return err }
func (t quillTarget) Remove(name string) error             { return t.fsys.Remove(name) }
func (t quillTarget) Rename(from, to string) error         { return t.fsys.Rename(from, to) }
func (t quillTarget) Reclaim()                             { t.fsys.Compact() }
func (t quillTarget) Close()                               { t.fsys.Close(); os.RemoveAll(t.dir) }
func (t quillTarget) ReadFile(name string) ([]byte, error) { return t.fsys.ReadFile(name) }
func (t quillTarget) WriteFile(name string, data []byte) error {
	return t.fsys.WriteFile(name, data, 0o644)
}
func (t quillTarget) ReadDir(name string) (int, error) {
	entries, err := t.fsys.ReadDir(name)
	return len(entries), err
}
func (t quillTarget) Open(name string, flag int) (handle, error) {
	return t.fsys.OpenFile(name, flag, 0o644)
}

var (
	files   = flag.Int("files", 20000, "small files per workload")
	largeMB = flag.Int("large", 256, "size of the large file in MiB")
	runs    = flag.Int("runs", 5, "repetitions; the median is reported")
	dirs    = flag.String("dirs", os.TempDir(), "comma-separated directories to test in, one per file system")
	scale   = flag.Int("scale", 0, "instead of the workloads, create this many files and report memory and open times")
)

func must(err error) {
	if err != nil {
		fmt.Fprintln(os.Stderr, "quillbench:", err)
		os.Exit(1)
	}
}

// rate times fn and returns n operations per second.
func rate(n int, fn func()) float64 {
	start := time.Now()
	fn()
	return float64(n) / time.Since(start).Seconds()
}

type result struct {
	workload string
	unit     string
	value    float64
}

// workloads runs the whole set once on one target.
func workloads(t target) []result {
	var out []result
	add := func(name, unit string, v float64) { out = append(out, result{name, unit, v}) }
	n := *files
	names := make([]string, n)
	for i := range names {
		names[i] = fmt.Sprintf("d%02d/f%06d", i%64, i)
	}
	for d := 0; d < 64; d++ {
		must(t.MkdirAll(fmt.Sprintf("d%02d", d)))
	}
	small, kib := make([]byte, 4096), make([]byte, 1024)

	add("create 4 KiB files", "files/s", rate(n, func() {
		for _, name := range names {
			must(t.WriteFile(name, small))
		}
	}))
	add("stat", "files/s", rate(n, func() {
		for _, name := range names {
			must(t.Stat(name))
		}
	}))
	add("read whole files", "files/s", rate(n, func() {
		for _, name := range names {
			_, err := t.ReadFile(name)
			must(err)
		}
	}))
	add("overwrite 1 KiB inside a file", "files/s", rate(n, func() {
		for _, name := range names {
			h, err := t.Open(name, os.O_WRONLY)
			must(err)
			_, err = h.WriteAt(kib, 1500)
			must(err)
			must(h.Close())
		}
	}))
	add("append 1 KiB", "files/s", rate(n, func() {
		for _, name := range names {
			h, err := t.Open(name, os.O_WRONLY|os.O_APPEND)
			must(err)
			_, err = h.Write(kib)
			must(err)
			must(h.Close())
		}
	}))
	add("truncate to 2 KiB", "files/s", rate(n, func() {
		for _, name := range names {
			h, err := t.Open(name, os.O_WRONLY)
			must(err)
			must(h.Truncate(2048))
			must(h.Close())
		}
	}))
	add("list directories", "entries/s", rate(n, func() {
		for d := 0; d < 64; d++ {
			_, err := t.ReadDir(fmt.Sprintf("d%02d", d))
			must(err)
		}
	}))
	add("rename a file", "files/s", rate(n, func() {
		for _, name := range names {
			must(t.Rename(name, name+".r"))
		}
	}))
	const dirMoves = 2000
	add("rename a directory of files", "renames/s", rate(dirMoves, func() {
		for i := 0; i < dirMoves; i += 2 {
			must(t.Rename("d00", "moved"))
			must(t.Rename("moved", "d00"))
		}
	}))
	add("delete", "files/s", rate(n, func() {
		for _, name := range names {
			must(t.Remove(name + ".r"))
		}
	}))

	// Eight goroutines, each creating and reading back its own files.
	const workers = 8
	per := n / workers
	add("create + read, 8 goroutines", "files/s", rate(per*workers, func() {
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < per; i++ {
					name := fmt.Sprintf("d%02d/p%06d", w, i)
					must(t.WriteFile(name, small))
					_, err := t.ReadFile(name)
					must(err)
				}
			}(w)
		}
		wg.Wait()
	}))
	for w := 0; w < workers; w++ {
		for i := 0; i < per; i++ {
			must(t.Remove(fmt.Sprintf("d%02d/p%06d", w, i)))
		}
	}

	// Churn: the same files created and deleted over and over, with the space
	// of each round reclaimed before the next. A log that is only fast while
	// it is empty would show here.
	const rounds = 10
	churn := n / 4
	add("create + delete, space reclaimed", "files/s", rate(rounds*churn, func() {
		for r := 0; r < rounds; r++ {
			for i := 0; i < churn; i++ {
				must(t.WriteFile(names[i], small))
			}
			for i := 0; i < churn; i++ {
				must(t.Remove(names[i]))
			}
			t.Reclaim()
		}
	}))

	// Durability: every file is on stable storage before the next one starts.
	const durable = 300
	add("create 4 KiB + fsync each", "files/s", rate(durable, func() {
		for i := 0; i < durable; i++ {
			h, err := t.Open(fmt.Sprintf("d01/s%04d", i), os.O_WRONLY|os.O_CREATE)
			must(err)
			_, err = h.Write(small)
			must(err)
			must(h.Sync())
			must(h.Close())
		}
	}))
	add("create 4 KiB + fsync each, 8 goroutines", "files/s", rate(durable*workers, func() {
		var wg sync.WaitGroup
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func(w int) {
				defer wg.Done()
				for i := 0; i < durable; i++ {
					h, err := t.Open(fmt.Sprintf("d%02d/t%04d", w+2, i), os.O_WRONLY|os.O_CREATE)
					must(err)
					_, err = h.Write(small)
					must(err)
					must(h.Sync())
					must(h.Close())
				}
			}(w)
		}
		wg.Wait()
	}))

	// One large file, written and read in 1 MiB pieces.
	mib := make([]byte, 1<<20)
	add("write a large file, then fsync", "MiB/s", rate(*largeMB, func() {
		h, err := t.Open("large", os.O_WRONLY|os.O_CREATE)
		must(err)
		for i := 0; i < *largeMB; i++ {
			_, err := h.Write(mib)
			must(err)
		}
		must(h.Sync())
		must(h.Close())
	}))
	add("read a large file", "MiB/s", rate(*largeMB, func() {
		h, err := t.Open("large", os.O_RDONLY)
		must(err)
		for i := 0; i < *largeMB; i++ {
			_, err := h.ReadAt(mib, int64(i)<<20)
			must(err)
		}
		must(h.Close())
	}))
	must(t.Remove("large"))
	return out
}

func median(v []float64) float64 {
	sort.Float64s(v)
	return v[len(v)/2]
}

// scaleTest fills a volume with many small files and reports what it costs
// to hold them and to come back to them.
func scaleTest(dir string, n int) {
	vol := filepath.Join(dir, fmt.Sprintf("quillbench-%d.vol", os.Getpid()))
	os.RemoveAll(vol)
	defer os.RemoveAll(vol)
	fsys, err := quillfs.Open(vol, nil)
	must(err)
	for d := 0; d < 1000; d++ {
		must(fsys.MkdirAll(fmt.Sprintf("d%03d", d), 0o755))
	}
	payload := make([]byte, 64)
	var before runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	create := rate(n, func() {
		for i := 0; i < n; i++ {
			must(fsys.WriteFile(fmt.Sprintf("d%03d/f%07d", i%1000, i), payload, 0o644))
		}
	})
	var after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&after)
	u := fsys.Usage()
	start := time.Now()
	must(fsys.Close())
	closeTime := time.Since(start)
	ck, err := os.Stat(filepath.Join(vol, "CHECKPOINT"))
	must(err)

	start = time.Now()
	fsys, err = quillfs.Open(vol, nil)
	must(err)
	fromCheckpoint := time.Since(start)
	_, err = fsys.ReadFile(fmt.Sprintf("d%03d/f%07d", (n-1)%1000, n-1))
	must(err)
	must(fsys.Close())

	// Without the checkpoint, Open has to replay the whole log, which is what
	// happens after a crash that no checkpoint preceded.
	must(os.Remove(filepath.Join(vol, "CHECKPOINT")))
	start = time.Now()
	fsys, err = quillfs.Open(vol, nil)
	must(err)
	fromLog := time.Since(start)
	if got := fsys.Usage().Files; got != u.Files {
		must(fmt.Errorf("replay found %d files, expected %d", got, u.Files))
	}
	must(fsys.Close())

	fmt.Printf("%d files of %d bytes in 1000 directories\n", n, len(payload))
	fmt.Printf("  create                      %.0f files/s\n", create)
	fmt.Printf("  memory for the tree         %.0f MiB (%d bytes per file)\n", float64(after.HeapAlloc-before.HeapAlloc)/(1<<20), (after.HeapAlloc-before.HeapAlloc)/uint64(n))
	fmt.Printf("  log on disk                 %d MiB in %d segments\n", u.Disk>>20, u.Segments)
	fmt.Printf("  close (writes a checkpoint) %v, checkpoint %d MiB\n", closeTime.Round(time.Millisecond), ck.Size()>>20)
	fmt.Printf("  open from the checkpoint    %v\n", fromCheckpoint.Round(time.Millisecond))
	fmt.Printf("  open by replaying the log   %v\n", fromLog.Round(time.Millisecond))
}

func main() {
	flag.Parse()
	if *scale > 0 {
		scaleTest(strings.Split(*dirs, ",")[0], *scale)
		return
	}
	type column struct {
		name string
		open func() target
	}
	var columns []column
	for _, dir := range strings.Split(*dirs, ",") {
		dir := dir
		fstype := fsType(dir)
		columns = append(columns,
			column{"quillfs on " + fstype, func() target {
				d := filepath.Join(dir, fmt.Sprintf("quillbench-%d.vol", os.Getpid()))
				os.RemoveAll(d)
				fsys, err := quillfs.Open(d, nil)
				must(err)
				return quillTarget{fsys, d}
			}},
			column{fstype, func() target {
				d := filepath.Join(dir, fmt.Sprintf("quillbench-%d.dir", os.Getpid()))
				os.RemoveAll(d)
				must(os.MkdirAll(d, 0o755))
				return osTarget{d}
			}})
	}
	samples := make([]map[string][]float64, len(columns))
	var order []result
	for run := 0; run < *runs; run++ {
		for c, col := range columns {
			t := col.open()
			res := workloads(t)
			t.Close()
			if samples[c] == nil {
				samples[c] = map[string][]float64{}
			}
			for _, r := range res {
				samples[c][r.workload] = append(samples[c][r.workload], r.value)
			}
			order = res
		}
	}
	fmt.Printf("%d CPUs, %s, %d files, large file %d MiB, median of %d runs\n\n", runtime.NumCPU(), runtime.Version(), *files, *largeMB, *runs)
	fmt.Printf("%-42s", "workload")
	for _, col := range columns {
		fmt.Printf(" %18s", col.name)
	}
	fmt.Println()
	for _, r := range order {
		fmt.Printf("%-42s", r.workload+" ("+r.unit+")")
		for c := range columns {
			fmt.Printf(" %18.0f", median(samples[c][r.workload]))
		}
		fmt.Println()
	}
}

// fsType names the file system a directory is on, from /proc/mounts.
func fsType(dir string) string {
	abs, err := filepath.Abs(dir)
	if err != nil {
		return dir
	}
	data, err := os.ReadFile("/proc/mounts")
	if err != nil {
		return dir
	}
	best, typ := "", dir
	for _, line := range strings.Split(string(data), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		if mp := f[1]; (abs == mp || strings.HasPrefix(abs, strings.TrimSuffix(mp, "/")+"/")) && len(mp) > len(best) {
			best, typ = mp, f[2]
		}
	}
	return typ
}
