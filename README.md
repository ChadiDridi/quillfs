# quillfs

**A file system that lives inside your process.** A quillfs volume is a directory tree with files you create, append to, overwrite, truncate, rename and delete, but no part of it is in the kernel: your Go program links the package and calls it directly. The data sits in memory-mapped log segments, the tree sits in memory, and a read, a `stat` or a small write involves no system call at all.

That is the whole trick, and it cuts both ways. Measured against ext4 on the same disk, through the same Go process ([numbers below](#benchmark)):

| | |
|---|---|
| **Faster, single-threaded** | Small files: create 3.3×, read 4.0×, append 4.1×, overwrite in place 2.2×. Metadata: `stat` 5.6×, delete 16×, truncate 16×, rename 32×, rename a directory 26×. Reading a large file: 1.4×. |
| **About the same** | Creating a file and calling fsync on each one (1.0×). The disk decides that, not the file system. |
| **Slower** | Eight goroutines creating and reading at once (0.6×): there is one writer lock, and ext4 scales where quillfs does not. Writing a large file (0.7×). Create-and-delete churn once reclaiming the space is counted (0.7×). Parallel fsync (0.5×). |
| **What you give up** | It cannot be mounted, and other programs cannot see the files. One process owns a volume at a time. The whole tree is held in memory, about 244 bytes per file. No hard links, no symbolic links, and permission bits are stored but not enforced. |

So quillfs is not a replacement for ext4. It is for a program that keeps a great many small files to itself — a cache, a build system's object store, a message spool, a test fixture tree — and spends its time in metadata and small reads and writes.

## Use

```sh
go get github.com/ChadiDridi/quillfs
```

```go
fsys, err := quillfs.Open("/var/lib/myapp/store", nil) // a directory; created if missing
if err != nil {
	log.Fatal(err)
}
defer fsys.Close()

fsys.MkdirAll("notes/2026", 0o755)
fsys.WriteFile("notes/2026/todo.txt", []byte("write the docs\n"), 0o644)

f, _ := fsys.OpenFile("notes/2026/todo.txt", os.O_WRONLY|os.O_APPEND, 0)
f.Write([]byte("ship it\n"))
f.Sync() // on stable storage from here on
f.Close()

fsys.Rename("notes/2026", "notes/done") // atomic, however large the directory
data, _ := fsys.ReadFile("notes/done/todo.txt")
```

The API follows the `os` package: `Open`, `Create`, `OpenFile` with the usual flags, `ReadFile`, `WriteFile`, `Mkdir`, `MkdirAll`, `Remove`, `RemoveAll`, `Rename`, `Stat`, `ReadDir`, `Truncate`, and a `File` with `Read`, `ReadAt`, `Write`, `WriteAt`, `Seek`, `Truncate`, `Sync`, `Stat`, `ReadDir` and `Close`. `fsys.IOFS()` gives a read-only `io/fs.FS` for everything that takes one. All of it is safe to call from many goroutines.

## What a crash does

quillfs gives the guarantees you get from a kernel file system without calling fsync, and it is tested against them:

- **A call that returned is not lost if your process dies.** A write returns only when its bytes are in the kernel's page cache, through the shared mapping or `pwrite`. `kill -9` cannot take them back, exactly as with `write(2)`.
- **A call that did not return either happened completely or not at all.** Every record carries a checksum, written last. Recovery stops in front of the first record that fails it.
- **`Sync` is fsync.** After it returns, everything written before it survives a power loss. Calls made at the same time share one flush.
- **`Rename` is atomic**, as are all operations on the tree: each is one record.

As with any file system, what was written but not synced can be lost if the machine loses power.

## How it works

A volume is a directory of **segments**: files of 64 MiB, preallocated and mapped into memory. Every change appends a **record** to the newest segment: create, write, truncate, unlink, rename. Nothing is ever rewritten in place, so overwriting the middle of a file is just an append plus a change to that file's list of extents.

The **tree** — directories, names, sizes, and for each file where its bytes are in the log — is ordinary Go data in memory. That is why `stat`, `rename` and `readdir` cost a lock and a map lookup.

- **Opening** a volume loads the last **checkpoint**, a snapshot of the tree, and replays the records logged after it.
- **Space** comes back by **compaction**: segments that are mostly dead have their live data copied to the head of the log and are deleted. A background goroutine does it when more than half of the volume is garbage.
- **A full disk** is an error, not a crash: a segment's blocks are reserved before anything is stored into its mapping.

The file system is about 1,800 lines of Go.

## Benchmark

`cmd/quillbench` runs the same workloads on a quillfs volume and, through Go's `os` package, on an ordinary directory of the same file system. Everything happens in one process, so the difference is the file system, not the program around it. Rates are operations per second unless marked, the median of several runs, and **ratio** is quillfs divided by the kernel file system.

Laptop (Intel i7-1355U, NVMe, Linux 6.8), measured at commit `9ff0d73`. 12 CPUs, go1.27.3, 20000 files, large file 256 MiB, median of 5 runs.

| Workload | ext4 | quillfs on ext4 | ratio | tmpfs | quillfs on tmpfs | ratio |
|---|---:|---:|---:|---:|---:|---:|
| create 4 KiB files | 80,346 | 267,140 | **3.3×** | 233,062 | 357,013 | **1.5×** |
| stat | 889,704 | 4,997,741 | **5.6×** | 960,753 | 5,293,506 | **5.5×** |
| read whole files | 104,055 | 417,847 | **4.0×** | 118,558 | 410,639 | **3.5×** |
| overwrite 1 KiB inside a file | 268,780 | 584,477 | **2.2×** | 297,565 | 621,212 | **2.1×** |
| append 1 KiB | 226,353 | 921,353 | **4.1×** | 298,099 | 1,108,411 | **3.7×** |
| truncate to 2 KiB | 141,021 | 2,206,030 | **16×** | 258,076 | 2,064,605 | **8.0×** |
| list directories, entries/s | 2,767,368 | 4,776,174 | **1.7×** | 2,511,933 | 4,161,071 | **1.7×** |
| rename a file | 72,133 | 2,284,249 | **32×** | 247,900 | 1,789,540 | **7.2×** |
| rename a directory of files | 129,976 | 3,428,591 | **26×** | 359,558 | 3,648,943 | **10×** |
| delete | 169,879 | 2,749,734 | **16×** | 419,555 | 2,304,825 | **5.5×** |
| create + read, 8 goroutines | 160,948 | 95,364 | 0.6× | 279,907 | 110,384 | 0.4× |
| create + delete, space reclaimed | 75,176 | 52,928 | 0.7× | 145,757 | 293,342 | **2.0×** |
| create 4 KiB + fsync each | 224 | 234 | 1.0× | 158,015 | 475,982 | **3.0×** |
| create 4 KiB + fsync each, 8 goroutines | 1,740 | 947 | 0.5× | 484,579 | 260,015 | 0.5× |
| write a large file, then fsync, MiB/s | 1,177 | 786 | 0.7× | 4,613 | 3,206 | 0.7× |
| read a large file, MiB/s | 5,235 | 7,230 | **1.4×** | 8,499 | 8,817 | 1.0× |

Reading it:

- **tmpfs** never touches a disk, so it is about as fast as a kernel file system gets. quillfs is still ahead of it on single-threaded small-file and metadata work, because what it saves is the system call, not the disk.
- **The losses are real.** One lock serializes all writers, so with eight goroutines the kernel file systems are ahead. Large writes pay for a checksum and for the log's segment boundaries. Reclaiming space means copying live data and flushing a checkpoint, which ext4 does not need.
- Numbers from one machine. Run it on yours:

```sh
go run github.com/ChadiDridi/quillfs/cmd/quillbench@latest -dirs /path/on/your/disk,/dev/shm
```

<!-- ci-benchmark -->

### A million files

```text
1000000 files of 64 bytes in 1000 directories
  create                      818560 files/s
  memory for the tree         233 MiB (244 bytes per file)
  log on disk                 192 MiB in 3 segments
  close (writes a checkpoint) 518ms, checkpoint 67 MiB
  open from the checkpoint    402ms
  open by replaying the log   535ms
```

Opening is fast either way: a checkpoint is one sequential read, and replaying the log is a scan of memory-mapped files.

## Testing

- **Against a real file system.** The same random operations — write, overwrite, append, truncate, rename, remove, mkdir — run on a volume and on a real directory, and every result has to match: the same error, the same bytes read back, the same tree at the end. Along the way the volume is closed and reopened, dropped without closing, and compacted.
- **`kill -9`.** A writer process is killed with SIGKILL at a random moment, six times in a row. Every write it reported as done must be there afterwards, complete.
- **Torn writes.** The log is cut at every byte of an operation, with zeros and with garbage after the cut. The volume must open to a state where that operation happened fully or not at all, and must stay usable.
- **Recovery equals memory.** Eight goroutines hammer a volume while it compacts; replaying the log must give back exactly the tree that was in memory.
- **Space.** Rewriting the same files sixty times must not grow the volume.
- **`testing/fstest.TestFS`** on the `io/fs` view, and the race detector on everything.

```sh
go test -race ./...
```

## Limits

- One process at a time; not mountable. Other programs reach the files only through your program.
- The tree is in memory: about 244 bytes per file, so ten million files take about 2.4 GB.
- One writer lock. Readers run in parallel with each other, not with a writer.
- A file's extent list grows with random overwrites and is not merged.
- Compaction copies live data and holds the writer lock while it does. The flush that follows does not block writers.
- No hard links, no symbolic links, no ownership, and permission bits are not enforced. Names are at most 255 bytes.
- Linux only.

## License

[Apache-2.0](LICENSE)
