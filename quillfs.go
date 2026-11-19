// Package quillfs is a file system that lives inside your process.
//
// A volume is a directory of memory-mapped log segments. Files, directories,
// renames and writes are appended to the log, and the tree itself is kept in
// memory. There is no system call on the path of a read, a stat, or a small
// write, which is where the speed comes from. In exchange, a volume belongs
// to one process at a time and cannot be mounted: programs use it through
// this package.
package quillfs

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Options configures a volume.
type Options struct {
	// SegmentSize is the size of each log segment file. It is fixed when the
	// volume is created and defaults to 64 MiB.
	SegmentSize int64
}

const (
	defaultSegmentSize = 64 << 20
	maxChunkSize       = 4 << 20
	// MaxNameLen is the longest name of a file or directory, in bytes.
	MaxNameLen = 255
)

// FS is an open volume. Its methods are safe to call from many goroutines.
type FS struct {
	dir  string
	lock *os.File

	mu       sync.RWMutex // guards everything below
	segSize  int64
	maxChunk int
	segs     map[uint32]*segment
	head     *segment
	nextSeq  uint32
	root     *inode
	inodes   map[uint64]*inode
	nextIno  uint64
	scratch  []byte // reused for the meta part of a record
	closed   bool

	syncMu sync.Mutex // serializes Sync and Compact
	synced logPos     // everything before this is on stable storage
}

// logPos is a position in the log.
type logPos struct {
	seq uint32
	off int64
}

func (p logPos) before(q logPos) bool { return p.seq < q.seq || p.seq == q.seq && p.off < q.off }

const rootIno = 1

// Open opens the volume in dir, creating it if the directory is empty or
// missing. A volume can be open in one process at a time.
func Open(dir string, opts *Options) (*FS, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "LOCK"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		lock.Close()
		return nil, fmt.Errorf("quillfs: %s is in use by another process", dir)
	}
	fsys := &FS{dir: dir, lock: lock, segs: map[uint32]*segment{}, inodes: map[uint64]*inode{}, nextIno: rootIno + 1, nextSeq: 1}
	if err := fsys.load(opts); err != nil {
		fsys.closeFiles()
		return nil, err
	}
	return fsys, nil
}

func (fsys *FS) closeFiles() {
	for _, s := range fsys.segs {
		s.close()
	}
	fsys.lock.Close()
}

// Close writes a checkpoint, so that the next Open does not have to replay
// the log, and releases the volume.
func (fsys *FS) Close() error {
	fsys.syncMu.Lock()
	defer fsys.syncMu.Unlock()
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if fsys.closed {
		return fs.ErrClosed
	}
	err := fsys.checkpoint()
	fsys.closed = true
	fsys.closeFiles()
	return err
}

// Sync forces everything written so far to stable storage. Calls made at the
// same time share one flush.
func (fsys *FS) Sync() error {
	fsys.mu.RLock()
	if fsys.closed {
		fsys.mu.RUnlock()
		return fs.ErrClosed
	}
	want := logPos{fsys.head.seq, fsys.head.tail}
	fsys.mu.RUnlock()

	fsys.syncMu.Lock()
	defer fsys.syncMu.Unlock()
	if !fsys.synced.before(want) {
		return nil // a flush that started after this call already covered it
	}
	fsys.mu.RLock()
	now := logPos{fsys.head.seq, fsys.head.tail}
	var dirty []*segment
	for seq := fsys.synced.seq; seq <= now.seq; seq++ {
		if s := fsys.segs[seq]; s != nil {
			dirty = append(dirty, s)
		}
	}
	fsys.mu.RUnlock()
	return fsys.flush(dirty, now)
}

// flush writes the given segments to stable storage. The caller holds syncMu,
// which keeps Compact from closing them meanwhile.
func (fsys *FS) flush(dirty []*segment, upTo logPos) error {
	for _, s := range dirty {
		if err := syscall.Fdatasync(int(s.f.Fd())); err != nil {
			return err
		}
	}
	fsys.synced = upTo
	return nil
}

func now() int64 { return time.Now().UnixNano() }

func pathErr(op, name string, err error) error { return &fs.PathError{Op: op, Path: name, Err: err} }

// split cleans a path into its components. The root is the empty list.
func split(name string) []string {
	clean := path.Clean("/" + name)
	if clean == "/" {
		return nil
	}
	return strings.Split(clean[1:], "/")
}

// walk resolves a path to an inode.
func (fsys *FS) walk(parts []string) (*inode, error) {
	in := fsys.root
	for _, p := range parts {
		if !in.isDir() {
			return nil, syscall.ENOTDIR
		}
		next := in.children[p]
		if next == nil {
			return nil, syscall.ENOENT
		}
		in = next
	}
	return in, nil
}

// walkParent resolves the directory that holds the last component of a path.
func (fsys *FS) walkParent(name string) (parent *inode, leaf string, err error) {
	parts := split(name)
	if len(parts) == 0 {
		return nil, "", syscall.EINVAL // the root has no parent
	}
	leaf = parts[len(parts)-1]
	if len(leaf) > MaxNameLen {
		return nil, "", syscall.ENAMETOOLONG
	}
	parent, err = fsys.walk(parts[:len(parts)-1])
	if err != nil {
		return nil, "", err
	}
	if !parent.isDir() {
		return nil, "", syscall.ENOTDIR
	}
	return parent, leaf, nil
}

// create logs and applies the creation of a file or directory.
func (fsys *FS) create(parent *inode, name string, mode fs.FileMode) (*inode, error) {
	ino, mtime := fsys.nextIno, now()
	m := putU64(fsys.scratch[:0], ino)
	m = putU64(m, parent.ino)
	m = putU32(m, uint32(mode))
	m = putU64(m, uint64(mtime))
	m = putStr(m, name)
	fsys.scratch = m
	if _, _, err := fsys.append(recCreate, m, nil); err != nil {
		return nil, err
	}
	return fsys.applyCreate(parent, name, ino, mode, mtime), nil
}

// Mkdir creates a directory.
func (fsys *FS) Mkdir(name string, perm fs.FileMode) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if fsys.closed {
		return fs.ErrClosed
	}
	parent, leaf, err := fsys.walkParent(name)
	if err != nil {
		if len(split(name)) == 0 {
			err = syscall.EEXIST // the root always exists
		}
		return pathErr("mkdir", name, err)
	}
	if parent.children[leaf] != nil {
		return pathErr("mkdir", name, syscall.EEXIST)
	}
	if _, err := fsys.create(parent, leaf, fs.ModeDir|perm.Perm()); err != nil {
		return pathErr("mkdir", name, err)
	}
	return nil
}

// MkdirAll creates a directory and any parents that are missing.
func (fsys *FS) MkdirAll(name string, perm fs.FileMode) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if fsys.closed {
		return fs.ErrClosed
	}
	in := fsys.root
	for _, p := range split(name) {
		if len(p) > MaxNameLen {
			return pathErr("mkdir", name, syscall.ENAMETOOLONG)
		}
		next := in.children[p]
		if next == nil {
			var err error
			if next, err = fsys.create(in, p, fs.ModeDir|perm.Perm()); err != nil {
				return pathErr("mkdir", name, err)
			}
		} else if !next.isDir() {
			return pathErr("mkdir", name, syscall.ENOTDIR)
		}
		in = next
	}
	return nil
}

func (fsys *FS) unlink(parent *inode, name string) error {
	mtime := now()
	m := putU64(fsys.scratch[:0], parent.ino)
	m = putU64(m, uint64(mtime))
	m = putStr(m, name)
	fsys.scratch = m
	if _, _, err := fsys.append(recUnlink, m, nil); err != nil {
		return err
	}
	fsys.applyUnlink(parent, name, mtime)
	return nil
}

// Remove removes a file or an empty directory.
func (fsys *FS) Remove(name string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if fsys.closed {
		return fs.ErrClosed
	}
	parent, leaf, err := fsys.walkParent(name)
	if err != nil {
		return pathErr("remove", name, err)
	}
	in := parent.children[leaf]
	if in == nil {
		return pathErr("remove", name, syscall.ENOENT)
	}
	if in.isDir() && len(in.children) > 0 {
		return pathErr("remove", name, syscall.ENOTEMPTY)
	}
	if err := fsys.unlink(parent, leaf); err != nil {
		return pathErr("remove", name, err)
	}
	return nil
}

// RemoveAll removes a path and everything below it. A missing path is not an
// error.
func (fsys *FS) RemoveAll(name string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if fsys.closed {
		return fs.ErrClosed
	}
	parent, leaf, err := fsys.walkParent(name)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		return pathErr("removeall", name, err)
	}
	in := parent.children[leaf]
	if in == nil {
		return nil
	}
	var rm func(parent *inode, name string, in *inode) error
	rm = func(parent *inode, name string, in *inode) error {
		for childName, child := range in.children {
			if err := rm(in, childName, child); err != nil {
				return err
			}
		}
		return fsys.unlink(parent, name)
	}
	if err := rm(parent, leaf, in); err != nil {
		return pathErr("removeall", name, err)
	}
	return nil
}

// Rename moves a file or directory, replacing the destination if it is a
// file or an empty directory. It is atomic: after a crash the tree holds
// either the old name or the new one.
func (fsys *FS) Rename(oldName, newName string) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if fsys.closed {
		return fs.ErrClosed
	}
	fail := func(err error) error { return &os.LinkError{Op: "rename", Old: oldName, New: newName, Err: err} }
	// Both directories are resolved before the source is looked up, which is
	// the order the kernel reports errors in.
	oldParent, oldLeaf, err := fsys.walkParent(oldName)
	if err != nil {
		return fail(err)
	}
	newParent, newLeaf, err := fsys.walkParent(newName)
	if err != nil {
		return fail(err)
	}
	in := oldParent.children[oldLeaf]
	if in == nil {
		return fail(syscall.ENOENT)
	}
	old := newParent.children[newLeaf]
	if old == in {
		return nil
	}
	// A directory cannot move into itself or its own subtree.
	if in.isDir() {
		for p := newParent; p != nil; p = p.parent {
			if p == in {
				return fail(syscall.EINVAL)
			}
		}
	}
	if old != nil {
		// The target is a directory that holds the source: it is not empty.
		for p := oldParent; p != nil; p = p.parent {
			if p == old {
				return fail(syscall.ENOTEMPTY)
			}
		}
		switch {
		case in.isDir() && !old.isDir():
			return fail(syscall.ENOTDIR)
		case !in.isDir() && old.isDir():
			return fail(syscall.EISDIR)
		case old.isDir() && len(old.children) > 0:
			return fail(syscall.ENOTEMPTY)
		}
	}
	mtime := now()
	m := putU64(fsys.scratch[:0], oldParent.ino)
	m = putU64(m, newParent.ino)
	m = putU64(m, uint64(mtime))
	m = putStr(m, oldLeaf)
	m = putStr(m, newLeaf)
	fsys.scratch = m
	if _, _, err := fsys.append(recRename, m, nil); err != nil {
		return fail(err)
	}
	fsys.applyRename(oldParent, oldLeaf, newParent, newLeaf, mtime)
	return nil
}

// Stat describes a file or directory.
func (fsys *FS) Stat(name string) (fs.FileInfo, error) {
	fsys.mu.RLock()
	defer fsys.mu.RUnlock()
	if fsys.closed {
		return nil, fs.ErrClosed
	}
	in, err := fsys.walk(split(name))
	if err != nil {
		return nil, pathErr("stat", name, err)
	}
	return in.info(), nil
}

// ReadDir lists a directory, sorted by name.
func (fsys *FS) ReadDir(name string) ([]fs.DirEntry, error) {
	fsys.mu.RLock()
	defer fsys.mu.RUnlock()
	if fsys.closed {
		return nil, fs.ErrClosed
	}
	in, err := fsys.walk(split(name))
	if err != nil {
		return nil, pathErr("readdir", name, err)
	}
	if !in.isDir() {
		return nil, pathErr("readdir", name, syscall.ENOTDIR)
	}
	return in.entries(), nil
}

func (in *inode) entries() []fs.DirEntry {
	list := make([]fs.DirEntry, 0, len(in.children))
	for _, child := range in.children {
		list = append(list, fs.FileInfoToDirEntry(child.info()))
	}
	sort.Slice(list, func(i, j int) bool { return list[i].Name() < list[j].Name() })
	return list
}

// fileInfo is a snapshot of an inode's metadata.
type fileInfo struct {
	name  string
	size  int64
	mode  fs.FileMode
	mtime int64
}

func (in *inode) info() fileInfo {
	name := in.name
	if in.parent == nil {
		name = "."
	}
	return fileInfo{name: name, size: in.size, mode: in.mode, mtime: in.mtime}
}

func (i fileInfo) Name() string       { return i.name }
func (i fileInfo) Size() int64        { return i.size }
func (i fileInfo) Mode() fs.FileMode  { return i.mode }
func (i fileInfo) ModTime() time.Time { return time.Unix(0, i.mtime) }
func (i fileInfo) IsDir() bool        { return i.mode.IsDir() }
func (i fileInfo) Sys() any           { return nil }

// Usage describes the space a volume takes.
type Usage struct {
	Files    int   // files and directories, without the root
	Segments int   // segment files on disk
	Disk     int64 // bytes reserved by those segments
	Live     int64 // bytes of file contents that are still reachable
}

// Usage reports how much space the volume takes and how much of it is live.
func (fsys *FS) Usage() Usage {
	fsys.mu.RLock()
	defer fsys.mu.RUnlock()
	u := Usage{Files: len(fsys.inodes) - 1, Segments: len(fsys.segs), Disk: int64(len(fsys.segs)) * fsys.segSize}
	for _, s := range fsys.segs {
		u.Live += s.live
	}
	return u
}
