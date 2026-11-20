# quillfs

**A file system that lives inside your process.** A quillfs volume is a directory tree with files you create, append to, overwrite, truncate, rename and delete, but no part of it is in the kernel: your Go program links the package and calls it directly. The data sits in memory-mapped log segments, the tree sits in memory, and a read, a `stat` or a small write involves no system call at all.

That is the whole trick, and it cuts both ways. Measured against ext4 on the same disk, through the same Go process ([numbers below](#benchmark)):

| | |
|---|---|
| **Faster, single-threaded** | Small files: create 3.4×, read 4.4×, append 4.1×, overwrite in place 2.4×. Metadata: `stat` 5.6×, delete 18×, truncate 12×, rename 29×, rename a directory 26×. |
| **About the same** | Creating a file and calling fsync on each one (0.9×). The disk decides that, not the file system. |
| **Slower** | Eight goroutines creating and reading at once (0.8×): there is one writer lock. Create-and-delete churn once reclaiming the space is counted (0.7×). |
| **Varies from run to run** | Writing and reading one large file, and fsync from eight goroutines at once. Across several full runs on the same laptop these came out above ext4, below it and level with it, so no claim is made either way. |
| **What you give up** | It cannot be mounted, and other programs cannot see the files. One process owns a volume at a time. The whole tree is held in memory, about 244 bytes per file. No hard links, no symbolic links, and permission bits are stored but not enforced. |

So quillfs is not a replacement for ext4. It is for a program that keeps a great many small files to itself — a cache, a build system's object store, a message spool, a test fixture tree — and spends its time in metadata and small reads and writes.

## Use

Needs Linux and Go 1.26 or newer.

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
- **A record is there completely or not at all.** Every record carries a checksum, written last, and recovery stops in front of the first record that fails it. One record is one `Mkdir`, `Remove`, `Rename` or `Truncate`, or one write of up to 4 MiB. So a rename is atomic, however large the directory.
- **Calls made of several records can be cut between them**, as their `os` counterparts can be cut between system calls: `WriteFile` (create, then write) can leave an empty file, a write larger than 4 MiB can stop at a 4 MiB boundary, and `MkdirAll` and `RemoveAll` can stop part of the way.
- **`Sync` is fsync.** After it returns, everything written before it survives a power cut. Calls made at the same time share one flush.
- **After a power cut**, of what was written since the last `Sync` some may be on disk and some not. Recovery keeps the longest prefix of the log that is intact and discards the rest, so the volume opens to the state after some operation at or past the last `Sync`.

## How it works

A volume is a directory of **segments**: files of 64 MiB, preallocated and mapped into memory. Every change appends a **record** to the newest segment: create, write, truncate, unlink, rename. Nothing is ever rewritten in place, so overwriting the middle of a file is just an append plus a change to that file's list of extents.

The **tree** — directories, names, sizes, and for each file where its bytes are in the log — is ordinary Go data in memory. That is why `stat`, `rename` and `readdir` cost a lock and a map lookup.

- **Opening** a volume loads the last **checkpoint**, a snapshot of the tree, and replays the records logged after it.
- **Space** comes back by **compaction**: segments that are mostly dead have their live data copied to the head of the log and are deleted. A background goroutine does it when more than half of the volume is garbage.
- **Disk space** for a segment is reserved before the segment is mapped, so that running out of disk comes back as an error from the call that needed the segment, not as a signal on a store into the mapping. No test covers this path.

The file system is about 1,800 lines of Go.

## Benchmark

`cmd/quillbench` runs the same workloads on a quillfs volume and, through Go's `os` package, on an ordinary directory of the same file system. Everything happens in one process, so the difference is the file system, not the program around it. Rates are operations per second unless marked, the median of several runs, and **ratio** is quillfs divided by the kernel file system.

Laptop (Intel i7-1355U, NVMe, Linux 6.8), measured at commit `56ce417`. 12 CPUs, go1.27.3, 20000 files, large file 256 MiB, median of 5 runs.

| Workload | ext4 | quillfs on ext4 | ratio | tmpfs | quillfs on tmpfs | ratio |
|---|---:|---:|---:|---:|---:|---:|
| create 4 KiB files | 79,517 | 271,101 | **3.4×** | 228,381 | 324,765 | **1.4×** |
| stat | 986,931 | 5,487,915 | **5.6×** | 1,115,644 | 5,384,157 | **4.8×** |
| read whole files | 121,829 | 531,870 | **4.4×** | 141,250 | 504,375 | **3.6×** |
| overwrite 1 KiB inside a file | 258,251 | 624,581 | **2.4×** | 295,429 | 784,198 | **2.7×** |
| append 1 KiB | 225,276 | 930,496 | **4.1×** | 287,936 | 1,168,069 | **4.1×** |
| truncate to 2 KiB | 149,078 | 1,809,185 | **12×** | 246,019 | 1,663,976 | **6.8×** |
| list directories, entries/s | 2,485,170 | 3,240,049 | **1.3×** | 3,163,192 | 3,390,948 | 1.1× |
| rename a file | 72,215 | 2,065,131 | **29×** | 230,731 | 2,419,811 | **10×** |
| rename a directory of files | 132,429 | 3,407,608 | **26×** | 338,404 | 3,684,558 | **11×** |
| delete | 162,580 | 2,883,025 | **18×** | 414,374 | 2,480,870 | **6.0×** |
| create + read, 8 goroutines | 152,858 | 122,209 | 0.8× | 268,344 | 152,741 | 0.6× |
| create + delete, space reclaimed | 72,949 | 54,622 | 0.7× | 133,171 | 264,071 | **2.0×** |
| create 4 KiB + fsync each | 255 | 235 | 0.9× | 216,637 | 457,325 | **2.1×** |
| create 4 KiB + fsync each, 8 goroutines | 1,685 | 1,017 | 0.6× | 481,379 | 220,637 | 0.5× |
| write a large file, then fsync, MiB/s | 1,038 | 932 | 0.9× | 4,586 | 3,124 | 0.7× |
| read a large file, MiB/s | 8,776 | 6,894 | 0.8× | 8,090 | 8,779 | 1.1× |

Reading it:

- **tmpfs** never touches a disk, so it is about as fast as a kernel file system gets. quillfs is still ahead of it on single-threaded small-file and metadata work, because what it saves is the system call, not the disk.
- **The losses are real.** One lock serializes all writers, so with eight goroutines the kernel file systems are ahead. Reclaiming space means copying live data and flushing a checkpoint, which ext4 does not need.
- **Three rows are not stable** on this machine: the large-file write, the large-file read, and fsync from eight goroutines came out on either side of ext4 from one full run to the next. Treat them as a tie.
- Numbers from one machine. Run it on yours:

```sh
go run github.com/ChadiDridi/quillfs/cmd/quillbench@latest -dirs /path/on/your/disk,/dev/shm
```

<!-- ci-benchmark -->

### A million files

```text
1000000 files of 64 bytes in 1000 directories
  create                      791548 files/s
  memory for the tree         233 MiB (244 bytes per file)
  log on disk                 192 MiB in 3 segments
  close (writes a checkpoint) 532ms, checkpoint 67 MiB
  open from the checkpoint    342ms
  open by replaying the log   563ms
```

Opening is fast either way: a checkpoint is one sequential read, and replaying the log is a scan of memory-mapped files.

## Testing

- **Against a real file system.** The same random operations — write, overwrite, append, truncate, rename, remove, mkdir — run on a volume and on a real directory, and every result has to match: the same error, the same bytes read back, the same tree at the end. Along the way the volume is closed and reopened, dropped without closing, and compacted.
- **`kill -9`.** A writer process is killed with SIGKILL at a random moment, six times in a row. Every write it reported as done must be there afterwards, complete.
- **Torn writes.** The log is cut inside an operation, at every byte for a small file and at every 61st for a 100 KiB one, with zeros and with garbage after the cut. The volume must open with every record whole or absent, and must stay usable.
- **Power cuts.** 150 times over, a random quarter of the pages written since the last `Sync` are wiped and new segment files are removed. The volume must open to the state after some operation at or past the `Sync`, and nothing that was lost may reappear after more is written.
- **Recovery equals memory.** Eight goroutines hammer a volume while it compacts; replaying the log must give back exactly the tree that was in memory.
- **Space.** Rewriting the same files sixty times must not grow the volume, and a volume of tiny files must not be compacted over and over.
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
- A damaged record in the middle of the log is treated like the end of it: recovery keeps what comes before and drops what comes after.
- No hard links, no symbolic links, no ownership, and permission bits are not enforced. Names are at most 255 bytes.
- Linux only.

## License

[Apache-2.0](LICENSE)
