package quillfs

import (
	"encoding/binary"
	"errors"
	"fmt"
	"hash/crc32"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

const (
	checkpointName  = "CHECKPOINT"
	checkpointMagic = "QUILLCK1"
)

var errCorrupt = errors.New("quillfs: volume is corrupt")

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errCorrupt, fmt.Sprintf(format, args...))
}

// load opens an existing volume or starts a new one.
func (fsys *FS) load(opts *Options) error {
	// Leftovers of a segment or checkpoint that was being prepared when the
	// process died. Nothing ever depended on them.
	if stale, err := filepath.Glob(filepath.Join(fsys.dir, "*.tmp")); err == nil {
		for _, name := range stale {
			os.Remove(name)
		}
	}
	names, err := filepath.Glob(filepath.Join(fsys.dir, "*.seg"))
	if err != nil {
		return err
	}
	fsys.root = &inode{ino: rootIno, mode: fs.ModeDir | 0o755, mtime: now(), children: map[string]*inode{}}
	fsys.inodes[rootIno] = fsys.root
	if len(names) == 0 {
		fsys.segSize = defaultSegmentSize
		if opts != nil && opts.SegmentSize != 0 {
			fsys.segSize = opts.SegmentSize
		}
		if fsys.segSize < 4096 || fsys.segSize > 1<<31 {
			return fmt.Errorf("quillfs: segment size %d is outside 4 KiB to 2 GiB", fsys.segSize)
		}
		fsys.setChunk()
		return fsys.newSegment()
	}

	var seqs []uint32
	for _, name := range names {
		n, err := strconv.ParseUint(strings.TrimSuffix(filepath.Base(name), ".seg"), 10, 32)
		if err != nil {
			return corrupt("unexpected file %s", name)
		}
		seqs = append(seqs, uint32(n))
	}
	sort.Slice(seqs, func(i, j int) bool { return seqs[i] < seqs[j] })
	for i, seq := range seqs {
		path := filepath.Join(fsys.dir, segName(seq))
		f, err := os.OpenFile(path, os.O_RDWR, 0)
		if err != nil {
			return err
		}
		st, err := f.Stat()
		if err != nil {
			f.Close()
			return err
		}
		if i == 0 {
			fsys.segSize = st.Size()
			fsys.setChunk()
		}
		if st.Size() != fsys.segSize {
			f.Close()
			return corrupt("%s is %d bytes, segments are %d", path, st.Size(), fsys.segSize)
		}
		seg, err := mapSegment(f, path, seq, fsys.segSize)
		if err != nil {
			return err
		}
		fsys.segs[seq] = seg
		if string(seg.mem[:8]) != segMagic || binary.LittleEndian.Uint32(seg.mem[8:]) != seq {
			return corrupt("%s has a bad header", path)
		}
	}
	last := seqs[len(seqs)-1]
	fsys.nextSeq = last + 1

	start := logPos{seqs[0], segHeader}
	if pos, ok, err := fsys.loadCheckpoint(); err != nil {
		return err
	} else if ok {
		start = pos
	}
	fsys.synced = logPos{start.seq, 0}

	for _, seq := range seqs {
		if seq < start.seq {
			continue
		}
		seg := fsys.segs[seq]
		from := int64(segHeader)
		if seq == start.seq {
			from = start.off
		}
		end, closed, err := scan(seg, from, func(typ byte, meta, data []byte, dataPos uint32) error {
			return fsys.replay(seg, typ, meta, data, dataPos)
		})
		if err != nil {
			return err
		}
		seg.tail = end
		if seq != last {
			if !closed {
				return corrupt("the log stops at offset %d of %s, which is not the last segment", end, seg.path)
			}
			continue
		}
		fsys.head = seg
		if closed {
			return fsys.newSegment()
		}
		// What follows the last good record is a write that never finished.
		// Clear it, so that no part of it can ever be read as a record.
		if torn := seg.mem[end:min(int64(len(seg.mem)), end+pad8(int64(recHeader+1024+fsys.maxChunk)))]; !allZero(torn) {
			clear(torn)
		}
	}
	return nil
}

func allZero(b []byte) bool {
	for _, c := range b {
		if c != 0 {
			return false
		}
	}
	return true
}

func (fsys *FS) setChunk() {
	fsys.maxChunk = int(min(maxChunkSize, fsys.segSize/4))
}

// replay applies one record of the log to the in-memory tree.
func (fsys *FS) replay(seg *segment, typ byte, meta, data []byte, dataPos uint32) error {
	d := decoder{b: meta}
	switch typ {
	case recCreate:
		ino, parent, mode, mtime, name := d.u64(), fsys.inodes[d.u64()], fs.FileMode(d.u32()), int64(d.u64()), d.str()
		if d.bad || parent == nil || !parent.isDir() || parent.children[name] != nil {
			return corrupt("bad create record in %s", seg.path)
		}
		fsys.applyCreate(parent, name, ino, mode, mtime)
	case recWrite, recReloc:
		in, off, mtime := fsys.inodes[d.u64()], int64(d.u64()), int64(d.u64())
		if d.bad {
			return corrupt("bad write record in %s", seg.path)
		}
		// A file removed while open can still be written to; such a write is
		// in the log but its file is gone.
		if in == nil || in.isDir() {
			return nil
		}
		fsys.applyWrite(in, off, seg, dataPos, uint32(len(data)))
		if typ == recWrite {
			in.mtime = mtime
		}
	case recTrunc:
		in, size, mtime := fsys.inodes[d.u64()], int64(d.u64()), int64(d.u64())
		if d.bad {
			return corrupt("bad truncate record in %s", seg.path)
		}
		if in == nil || in.isDir() {
			return nil
		}
		fsys.applyTrunc(in, size)
		in.mtime = mtime
	case recUnlink:
		parent, mtime, name := fsys.inodes[d.u64()], int64(d.u64()), d.str()
		if d.bad || parent == nil || parent.children[name] == nil {
			return corrupt("bad unlink record in %s", seg.path)
		}
		fsys.applyUnlink(parent, name, mtime)
	case recRename:
		oldParent, newParent, mtime := fsys.inodes[d.u64()], fsys.inodes[d.u64()], int64(d.u64())
		oldName, newName := d.str(), d.str()
		if d.bad || oldParent == nil || newParent == nil || oldParent.children[oldName] == nil {
			return corrupt("bad rename record in %s", seg.path)
		}
		fsys.applyRename(oldParent, oldName, newParent, newName, mtime)
	default:
		return corrupt("unknown record type %d in %s", typ, seg.path)
	}
	return nil
}

// A checkpoint is the whole tree written to disk, so that the next Open
// starts from it and replays only what was logged afterwards. It is made in
// two steps, so that writers wait only for the first:
//
//	snapshot  serializes the tree in memory, under the lock
//	commit    flushes the log and writes the file, without the lock
type snapshot struct {
	pos   logPos     // the log position the tree corresponds to
	image []byte     // the checkpoint file
	dirty []*segment // segments to flush before the file may exist
	dead  []uint32   // segments nothing in the image points to
}

// snapshot serializes the tree. The caller holds syncMu and mu.
func (fsys *FS) snapshot() *snapshot {
	snap := &snapshot{pos: logPos{fsys.head.seq, fsys.head.tail}}
	for seq := fsys.synced.seq; seq <= snap.pos.seq; seq++ {
		if s := fsys.segs[seq]; s != nil {
			snap.dirty = append(snap.dirty, s)
		}
	}
	// A segment before the snapshot is needed only for the file data in it.
	// Which ones are empty has to be decided now: a segment that empties
	// later is still pointed to by this image.
	for seq, seg := range fsys.segs {
		if seq < snap.pos.seq && seg.live == 0 {
			snap.dead = append(snap.dead, seq)
		}
	}
	buf := append(make([]byte, 0, 64+96*len(fsys.inodes)), checkpointMagic...)
	buf = putU64(buf, uint64(fsys.segSize))
	buf = putU64(buf, fsys.nextIno)
	buf = putU32(buf, snap.pos.seq)
	buf = putU64(buf, uint64(snap.pos.off))
	buf = putU64(buf, uint64(fsys.root.mtime))
	// Parents before children, so that loading can attach each inode as it
	// is read.
	var dump func(in *inode)
	dump = func(in *inode) {
		for _, child := range in.children {
			buf = append(buf, 1)
			buf = putU64(buf, child.ino)
			buf = putU64(buf, in.ino)
			buf = putU32(buf, uint32(child.mode))
			buf = putU64(buf, uint64(child.mtime))
			buf = putU64(buf, uint64(child.size))
			buf = putStr(buf, child.name)
			buf = putU32(buf, uint32(len(child.ext)))
			for _, e := range child.ext {
				buf = putU64(buf, uint64(e.off))
				buf = putU32(buf, e.seg)
				buf = putU32(buf, e.pos)
				buf = putU32(buf, e.n)
			}
			dump(child)
		}
	}
	dump(fsys.root)
	buf = append(buf, 0)
	snap.image = binary.LittleEndian.AppendUint32(buf, crc32.Checksum(buf, castagnoli))
	return snap
}

// commit makes a snapshot the volume's checkpoint. The caller holds syncMu
// but not mu: operations go on meanwhile, and land in the log after the
// snapshot's position, where the next Open will find them.
func (fsys *FS) commit(snap *snapshot) error {
	// The image points into the log. Everything it points to must be on disk
	// before the image is.
	if err := fsys.flush(snap.dirty, snap.pos); err != nil {
		return err
	}
	tmp := filepath.Join(fsys.dir, checkpointName+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	defer os.Remove(tmp)
	if _, err := f.Write(snap.image); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(fsys.dir, checkpointName)); err != nil {
		return err
	}
	return syncDir(fsys.dir)
}

// drop deletes the segments a committed snapshot found empty. The caller
// holds syncMu and mu.
func (fsys *FS) drop(snap *snapshot) {
	for _, seq := range snap.dead {
		if seg := fsys.segs[seq]; seg != nil {
			seg.close()
			os.Remove(seg.path)
			delete(fsys.segs, seq)
		}
	}
}

func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}

// loadCheckpoint rebuilds the tree from the checkpoint file, if there is one,
// and returns the log position to replay from.
func (fsys *FS) loadCheckpoint() (logPos, bool, error) {
	data, err := os.ReadFile(filepath.Join(fsys.dir, checkpointName))
	if errors.Is(err, fs.ErrNotExist) {
		return logPos{}, false, nil
	}
	if err != nil {
		return logPos{}, false, err
	}
	if len(data) < len(checkpointMagic)+4 || string(data[:len(checkpointMagic)]) != checkpointMagic ||
		crc32.Checksum(data[:len(data)-4], castagnoli) != binary.LittleEndian.Uint32(data[len(data)-4:]) {
		return logPos{}, false, corrupt("the checkpoint fails its checksum")
	}
	d := decoder{b: data[len(checkpointMagic) : len(data)-4]}
	if segSize := int64(d.u64()); segSize != fsys.segSize {
		return logPos{}, false, corrupt("the checkpoint is for %d-byte segments, the volume has %d", segSize, fsys.segSize)
	}
	fsys.nextIno = d.u64()
	pos := logPos{d.u32(), int64(d.u64())}
	fsys.root.mtime = int64(d.u64())
	for {
		if len(d.b) < 1 {
			return logPos{}, false, corrupt("the checkpoint ends early")
		}
		more := d.b[0]
		d.b = d.b[1:]
		if more == 0 {
			break
		}
		ino, parent, mode, mtime, size, name := d.u64(), fsys.inodes[d.u64()], fs.FileMode(d.u32()), int64(d.u64()), int64(d.u64()), d.str()
		if d.bad || parent == nil {
			return logPos{}, false, corrupt("bad inode in the checkpoint")
		}
		parentMtime := parent.mtime
		in := fsys.applyCreate(parent, name, ino, mode, mtime)
		parent.mtime = parentMtime // loading a child is not a change to its directory
		in.size = size
		n := int(d.u32())
		if n > 0 {
			in.ext = make([]extent, n)
		}
		for i := range in.ext {
			e := extent{off: int64(d.u64()), seg: d.u32(), pos: d.u32(), n: d.u32()}
			seg := fsys.segs[e.seg]
			if d.bad || seg == nil || int64(e.pos)+int64(e.n) > fsys.segSize {
				return logPos{}, false, corrupt("the checkpoint points outside the log")
			}
			seg.live += int64(e.n)
			in.ext[i] = e
		}
	}
	if d.bad {
		return logPos{}, false, corrupt("the checkpoint ends early")
	}
	return pos, true, nil
}

// Checkpoint writes the tree to disk now. Close does it too; calling it in
// between bounds how much of the log a recovery has to replay.
func (fsys *FS) Checkpoint() error {
	fsys.syncMu.Lock()
	defer fsys.syncMu.Unlock()
	fsys.mu.Lock()
	if fsys.closed {
		fsys.mu.Unlock()
		return fs.ErrClosed
	}
	snap := fsys.snapshot()
	fsys.mu.Unlock()
	if err := fsys.commit(snap); err != nil {
		return err
	}
	fsys.mu.Lock()
	fsys.drop(snap)
	fsys.mu.Unlock()
	return nil
}

// Compact gives disk space back. Segments in which less than half of the
// bytes are still live have their live data copied to the head of the log;
// a checkpoint then makes every emptied segment deletable. It returns the
// number of segment files removed. Unless Options.NoAutoCompact is set, a
// background goroutine calls it when more than half of the volume is
// garbage.
func (fsys *FS) Compact() (removed int, err error) {
	fsys.syncMu.Lock()
	defer fsys.syncMu.Unlock()
	fsys.mu.Lock()
	if fsys.closed {
		fsys.mu.Unlock()
		return 0, fs.ErrClosed
	}
	victims := map[uint32]bool{}
	for seq, seg := range fsys.segs {
		if seg != fsys.head && seg.live > 0 && seg.live < fsys.segSize/2 {
			victims[seq] = true
		}
	}
	for _, in := range fsys.inodes {
		for i := range in.ext {
			e := &in.ext[i]
			if !victims[e.seg] {
				continue
			}
			old := fsys.segs[e.seg]
			m := putU64(fsys.scratch[:0], in.ino)
			m = putU64(m, uint64(e.off))
			m = putU64(m, 0)
			fsys.scratch = m
			seg, pos, err := fsys.append(recReloc, m, old.mem[e.pos:e.pos+e.n])
			if err != nil {
				fsys.mu.Unlock()
				return 0, err
			}
			old.live -= int64(e.n)
			seg.live += int64(e.n)
			e.seg, e.pos = seg.seq, pos
		}
	}
	snap := fsys.snapshot()
	fsys.mu.Unlock()
	if err := fsys.commit(snap); err != nil {
		return 0, err
	}
	fsys.mu.Lock()
	before := len(fsys.segs)
	fsys.drop(snap)
	removed = before - len(fsys.segs)
	fsys.mu.Unlock()
	return removed, nil
}
