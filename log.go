package quillfs

import (
	"encoding/binary"
	"fmt"
	"hash/crc32"
	"os"
	"path/filepath"
	"syscall"
)

// The volume is a directory of segment files. Each segment is preallocated,
// mapped into memory, and filled from front to back with records. Nothing in
// a segment is ever rewritten: changing a file appends a record, and space
// comes back by deleting whole segments once nothing in them is live.
//
// A record is:
//
//	 0  crc32c of bytes 4..end           uint32
//	 4  record length, without padding   uint32
//	 8  type                             uint8, then 3 zero bytes
//	12  length of the meta part          uint32
//	16  meta (fixed fields and names), then data
//
// Records are padded to 8 bytes. An operation returns only after its record
// is complete in the mapping. The mapping is shared with the file, so those
// bytes are in the kernel's page cache: they survive the death of the process
// exactly as a completed write(2) does.

const (
	segMagic  = "QUILLSG1"
	segHeader = 16 // magic, then the sequence number
	recHeader = 16

	recEnd    = 1 // the segment was closed; the log continues in the next one
	recCreate = 2
	recWrite  = 3
	recTrunc  = 4
	recUnlink = 5
	recRename = 6
	recReloc  = 7 // a write moved by compaction: same bytes, no new mtime
)

var castagnoli = crc32.MakeTable(crc32.Castagnoli)

type segment struct {
	seq  uint32
	path string
	f    *os.File
	mem  []byte
	tail int64 // next free byte; only meaningful for the head segment
	live int64 // bytes of file data that some extent still points to
}

func segName(seq uint32) string { return fmt.Sprintf("%08d.seg", seq) }

// newSegment creates the next segment file and makes it the head.
func (fsys *FS) newSegment() error {
	seq := fsys.nextSeq
	path := filepath.Join(fsys.dir, segName(seq))
	// The file is prepared under a temporary name and renamed into place, so
	// that a segment file either does not exist or is complete, even if the
	// process dies in the middle of this.
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	// Reserve the blocks now. Storing into a mapped hole on a full disk
	// would kill the process with SIGBUS; this way a full disk is an error.
	if err := syscall.Fallocate(int(f.Fd()), 0, 0, fsys.segSize); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	var hdr [segHeader]byte
	copy(hdr[:], segMagic)
	binary.LittleEndian.PutUint32(hdr[8:], seq)
	if _, err := f.WriteAt(hdr[:], 0); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	seg, err := mapSegment(f, path, seq, fsys.segSize)
	if err != nil {
		os.Remove(path)
		return err
	}
	seg.tail = segHeader
	fsys.segs[seq] = seg
	fsys.head = seg
	fsys.nextSeq++
	return nil
}

func mapSegment(f *os.File, path string, seq uint32, size int64) (*segment, error) {
	mem, err := syscall.Mmap(int(f.Fd()), 0, int(size), syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		f.Close()
		return nil, err
	}
	return &segment{seq: seq, path: path, f: f, mem: mem}, nil
}

func (s *segment) close() {
	syscall.Munmap(s.mem)
	s.f.Close()
}

func pad8(n int64) int64 { return (n + 7) &^ 7 }

// append writes one record to the head segment and returns the segment and
// the offset of its data part. The caller holds fsys.mu for writing.
func (fsys *FS) append(typ byte, meta, data []byte) (*segment, uint32, error) {
	n := int64(recHeader + len(meta) + len(data))
	if pad8(n)+recHeader > fsys.segSize-segHeader {
		return nil, 0, fmt.Errorf("quillfs: record of %d bytes does not fit a %d-byte segment", n, fsys.segSize)
	}
	// Always keep room for the end record that closes the segment.
	if fsys.head.tail+pad8(n)+recHeader > fsys.segSize {
		fsys.writeRecord(fsys.head, recEnd, nil, nil)
		if err := fsys.newSegment(); err != nil {
			return nil, 0, err
		}
	}
	seg := fsys.head
	pos := fsys.writeRecord(seg, typ, meta, data)
	return seg, uint32(pos + recHeader + int64(len(meta))), nil
}

func (fsys *FS) writeRecord(seg *segment, typ byte, meta, data []byte) int64 {
	pos := seg.tail
	n := recHeader + len(meta) + len(data)
	b := seg.mem[pos : pos+int64(n)]
	binary.LittleEndian.PutUint32(b[4:], uint32(n))
	b[8], b[9], b[10], b[11] = typ, 0, 0, 0
	binary.LittleEndian.PutUint32(b[12:], uint32(len(meta)))
	copy(b[recHeader:], meta)
	copy(b[recHeader+len(meta):], data)
	// The checksum goes in last: until it is there, recovery sees a torn
	// record and stops in front of it.
	binary.LittleEndian.PutUint32(b, crc32.Checksum(b[4:], castagnoli))
	seg.tail = pos + pad8(int64(n))
	return pos
}

// scan calls fn for every record of a segment from offset from. It returns
// where the valid records end and whether the segment was closed by an end
// record. A record that fails its checksum ends the scan without an error:
// whether that is a torn tail or corruption is for the caller to decide.
func scan(seg *segment, from int64, fn func(typ byte, meta, data []byte, dataPos uint32) error) (end int64, closed bool, err error) {
	pos := from
	size := int64(len(seg.mem))
	for pos+recHeader <= size {
		b := seg.mem[pos:]
		n := int64(binary.LittleEndian.Uint32(b[4:]))
		metaLen := int64(binary.LittleEndian.Uint32(b[12:]))
		if n < recHeader || pos+n > size || recHeader+metaLen > n {
			return pos, false, nil
		}
		if crc32.Checksum(b[4:n], castagnoli) != binary.LittleEndian.Uint32(b) {
			return pos, false, nil
		}
		typ := b[8]
		if typ == recEnd {
			return pos, true, nil
		}
		meta := b[recHeader : recHeader+metaLen]
		data := b[recHeader+metaLen : n]
		if err := fn(typ, meta, data, uint32(pos+recHeader+metaLen)); err != nil {
			return pos, false, err
		}
		pos += pad8(n)
	}
	return pos, false, nil
}

// Little encoders for the meta part of a record. The buffer is reused, so a
// record is built without allocating.

func putU32(b []byte, v uint32) []byte { return binary.LittleEndian.AppendUint32(b, v) }
func putU64(b []byte, v uint64) []byte { return binary.LittleEndian.AppendUint64(b, v) }
func putStr(b []byte, s string) []byte {
	b = binary.LittleEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

type decoder struct {
	b   []byte
	bad bool
}

func (d *decoder) u32() uint32 {
	if len(d.b) < 4 {
		d.bad = true
		return 0
	}
	v := binary.LittleEndian.Uint32(d.b)
	d.b = d.b[4:]
	return v
}

func (d *decoder) u64() uint64 {
	if len(d.b) < 8 {
		d.bad = true
		return 0
	}
	v := binary.LittleEndian.Uint64(d.b)
	d.b = d.b[8:]
	return v
}

func (d *decoder) str() string {
	if len(d.b) < 2 {
		d.bad = true
		return ""
	}
	n := int(binary.LittleEndian.Uint16(d.b))
	if len(d.b) < 2+n {
		d.bad = true
		return ""
	}
	s := string(d.b[2 : 2+n])
	d.b = d.b[2+n:]
	return s
}
