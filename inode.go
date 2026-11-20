package quillfs

import (
	"io/fs"
	"sort"
)

// inode is a file or a directory. Everything about the tree lives in memory;
// the log exists to rebuild it.
type inode struct {
	ino      uint64
	mode     fs.FileMode
	mtime    int64 // nanoseconds since the epoch
	size     int64
	parent   *inode
	name     string
	children map[string]*inode // directories only
	ext      []extent          // files only, sorted by off, never overlapping
	opens    int               // open handles
	unlinked bool              // no name any more; freed when the last handle closes
}

// extent says that n bytes of a file, starting at off, are the n bytes at pos
// in a segment. A range no extent covers is a hole and reads as zeros.
type extent struct {
	off int64
	seg uint32
	pos uint32
	n   uint32
}

func (e extent) end() int64 { return e.off + int64(e.n) }

func (in *inode) isDir() bool { return in.mode.IsDir() }

// firstExtentEndingAfter returns the index of the first extent whose end is
// beyond off.
func (in *inode) firstExtentEndingAfter(off int64) int {
	return sort.Search(len(in.ext), func(i int) bool { return in.ext[i].end() > off })
}

// punch removes the range [from, to) from the extent list, reporting every
// run of bytes that stops being live through dead.
func (in *inode) punch(from, to int64, dead func(seg uint32, n int64)) {
	i := in.firstExtentEndingAfter(from)
	if i == len(in.ext) || in.ext[i].off >= to {
		return
	}
	var keep []extent // at most two pieces survive around the range
	j := i
	for ; j < len(in.ext) && in.ext[j].off < to; j++ {
		e := in.ext[j]
		lo, hi := max(e.off, from), min(e.end(), to)
		dead(e.seg, hi-lo)
		if e.off < from {
			keep = append(keep, extent{e.off, e.seg, e.pos, uint32(from - e.off)})
		}
		if e.end() > to {
			keep = append(keep, extent{to, e.seg, e.pos + uint32(to-e.off), uint32(e.end() - to)})
		}
	}
	in.ext = append(in.ext[:i], append(keep, in.ext[j:]...)...)
}

// setExtent records that [e.off, e.end()) now lives at e's position.
func (in *inode) setExtent(e extent, dead func(seg uint32, n int64)) {
	// Appending to the end of a file is the common case.
	if n := len(in.ext); n == 0 || in.ext[n-1].end() <= e.off {
		in.ext = append(in.ext, e)
		return
	}
	in.punch(e.off, e.end(), dead)
	i := in.firstExtentEndingAfter(e.off)
	in.ext = append(in.ext, extent{})
	copy(in.ext[i+1:], in.ext[i:])
	in.ext[i] = e
}

// The apply functions change the in-memory tree. They are the single place
// where state changes, used both when an operation runs and when the log is
// replayed, so that a recovered tree is the tree that was.

func (fsys *FS) dead(seg uint32, n int64) {
	if s := fsys.segs[seg]; s != nil {
		s.live -= n
	}
}

func (fsys *FS) applyCreate(parent *inode, name string, ino uint64, mode fs.FileMode, mtime int64) *inode {
	in := &inode{ino: ino, mode: mode, mtime: mtime, parent: parent, name: name}
	if mode.IsDir() {
		in.children = map[string]*inode{}
	}
	parent.children[name] = in
	parent.mtime = mtime
	fsys.inodes[ino] = in
	if ino >= fsys.nextIno {
		fsys.nextIno = ino + 1
	}
	return in
}

func (fsys *FS) applyWrite(in *inode, off int64, seg *segment, pos, n uint32) {
	in.setExtent(extent{off, seg.seq, pos, n}, fsys.dead)
	seg.live += int64(n)
	seg.data += int64(n)
	if end := off + int64(n); end > in.size {
		in.size = end
	}
}

func (fsys *FS) applyTrunc(in *inode, size int64) {
	if size < in.size {
		in.punch(size, in.size, fsys.dead)
	}
	in.size = size
}

// release frees an inode that has neither a name nor an open handle.
func (fsys *FS) release(in *inode) {
	for _, e := range in.ext {
		fsys.dead(e.seg, int64(e.n))
	}
	in.ext = nil
	delete(fsys.inodes, in.ino)
}

func (fsys *FS) applyUnlink(parent *inode, name string, mtime int64) {
	in := parent.children[name]
	delete(parent.children, name)
	parent.mtime = mtime
	in.unlinked = true
	if in.opens == 0 {
		fsys.release(in)
	}
}

func (fsys *FS) applyRename(oldParent *inode, oldName string, newParent *inode, newName string, mtime int64) {
	in := oldParent.children[oldName]
	if old := newParent.children[newName]; old != nil && old != in {
		fsys.applyUnlink(newParent, newName, mtime)
	}
	delete(oldParent.children, oldName)
	newParent.children[newName] = in
	in.parent, in.name = newParent, newName
	oldParent.mtime, newParent.mtime = mtime, mtime
}
