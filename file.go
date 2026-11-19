package quillfs

import (
	"io"
	"io/fs"
	"os"
	"sync"
	"syscall"
)

// File is an open file or directory. It follows the conventions of os.File.
type File struct {
	fsys *FS
	in   *inode
	name string
	flag int

	mu      sync.Mutex // guards the fields below
	pos     int64
	entries []fs.DirEntry // directory listing, taken at the first ReadDir
	listed  bool
	closed  bool
}

// Open opens a file or directory for reading.
func (fsys *FS) Open(name string) (*File, error) { return fsys.OpenFile(name, os.O_RDONLY, 0) }

// Create creates or truncates a file and opens it for reading and writing.
func (fsys *FS) Create(name string) (*File, error) {
	return fsys.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// OpenFile opens a file with the flags of os.OpenFile: O_RDONLY, O_WRONLY,
// O_RDWR, O_APPEND, O_CREATE, O_EXCL and O_TRUNC.
func (fsys *FS) OpenFile(name string, flag int, perm fs.FileMode) (*File, error) {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if fsys.closed {
		return nil, fs.ErrClosed
	}
	in, err := fsys.openLocked(name, flag, perm)
	if err != nil {
		return nil, pathErr("open", name, err)
	}
	in.opens++
	return &File{fsys: fsys, in: in, name: name, flag: flag}, nil
}

func (fsys *FS) openLocked(name string, flag int, perm fs.FileMode) (*inode, error) {
	writing := flag&(os.O_WRONLY|os.O_RDWR) != 0
	parts := split(name)
	if len(parts) == 0 {
		if writing {
			return nil, syscall.EISDIR
		}
		return fsys.root, nil
	}
	parent, leaf, err := fsys.walkParent(name)
	if err != nil {
		return nil, err
	}
	in := parent.children[leaf]
	switch {
	case in == nil && flag&os.O_CREATE == 0:
		return nil, syscall.ENOENT
	case in == nil:
		return fsys.create(parent, leaf, perm.Perm())
	case flag&(os.O_CREATE|os.O_EXCL) == os.O_CREATE|os.O_EXCL:
		return nil, syscall.EEXIST
	case in.isDir() && writing:
		return nil, syscall.EISDIR
	}
	if flag&os.O_TRUNC != 0 && writing && in.size > 0 {
		if err := fsys.truncate(in, 0); err != nil {
			return nil, err
		}
	}
	return in, nil
}

// truncate logs and applies a change of size.
func (fsys *FS) truncate(in *inode, size int64) error {
	mtime := now()
	m := putU64(fsys.scratch[:0], in.ino)
	m = putU64(m, uint64(size))
	m = putU64(m, uint64(mtime))
	fsys.scratch = m
	if _, _, err := fsys.append(recTrunc, m, nil); err != nil {
		return err
	}
	fsys.applyTrunc(in, size)
	in.mtime = mtime
	return nil
}

// write logs and applies one write. With appendMode the offset is the end of
// the file at the moment the write happens.
func (fsys *FS) write(in *inode, off int64, data []byte, appendMode bool) (int, error) {
	done := 0
	for done < len(data) {
		chunk := data[done:]
		if len(chunk) > fsys.maxChunk {
			chunk = chunk[:fsys.maxChunk]
		}
		at := off + int64(done)
		if appendMode {
			at = in.size
		}
		mtime := now()
		m := putU64(fsys.scratch[:0], in.ino)
		m = putU64(m, uint64(at))
		m = putU64(m, uint64(mtime))
		fsys.scratch = m
		seg, pos, err := fsys.append(recWrite, m, chunk)
		if err != nil {
			return done, err
		}
		fsys.applyWrite(in, at, seg, pos, uint32(len(chunk)))
		in.mtime = mtime
		done += len(chunk)
	}
	return done, nil
}

// read copies file contents into p, starting at off. The caller holds
// fsys.mu at least for reading.
func (fsys *FS) read(in *inode, p []byte, off int64) int {
	if off >= in.size {
		return 0
	}
	if int64(len(p)) > in.size-off {
		p = p[:in.size-off]
	}
	i := in.firstExtentEndingAfter(off)
	for done := 0; done < len(p); {
		cur := off + int64(done)
		if i < len(in.ext) && in.ext[i].off <= cur {
			e := in.ext[i]
			from := uint32(cur - e.off)
			done += copy(p[done:], fsys.segs[e.seg].mem[e.pos+from:e.pos+e.n])
			i++
			continue
		}
		// A hole: zeros up to the next extent or the end of the request.
		end := len(p)
		if i < len(in.ext) && in.ext[i].off-off < int64(end) {
			end = int(in.ext[i].off - off)
		}
		clear(p[done:end])
		done = end
	}
	return len(p)
}

func (f *File) writable() bool { return f.flag&(os.O_WRONLY|os.O_RDWR) != 0 }
func (f *File) readable() bool { return f.flag&os.O_WRONLY == 0 }

func (f *File) fail(op string, err error) error { return pathErr(op, f.name, err) }

// Name returns the name the file was opened with.
func (f *File) Name() string { return f.name }

// Read reads up to len(p) bytes from the current position.
func (f *File) Read(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	n, err := f.readAt(p, f.pos)
	f.pos += int64(n)
	if n > 0 {
		err = nil
	}
	return n, err
}

// ReadAt reads len(p) bytes starting at off. It returns io.EOF if the file
// ends first.
func (f *File) ReadAt(p []byte, off int64) (int, error) { return f.readAt(p, off) }

func (f *File) readAt(p []byte, off int64) (int, error) {
	f.fsys.mu.RLock()
	defer f.fsys.mu.RUnlock()
	switch {
	case f.closed || f.fsys.closed:
		return 0, f.fail("read", fs.ErrClosed)
	case f.in.isDir():
		return 0, f.fail("read", syscall.EISDIR)
	case !f.readable():
		return 0, f.fail("read", syscall.EBADF)
	case off < 0:
		return 0, f.fail("read", syscall.EINVAL)
	}
	n := f.fsys.read(f.in, p, off)
	if n < len(p) {
		return n, io.EOF
	}
	return n, nil
}

// Write writes p at the current position, or at the end of the file if it
// was opened with O_APPEND.
func (f *File) Write(p []byte) (int, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	appendMode := f.flag&os.O_APPEND != 0
	n, err := f.writeAt(p, f.pos, appendMode)
	if appendMode {
		f.fsys.mu.RLock()
		f.pos = f.in.size
		f.fsys.mu.RUnlock()
	} else {
		f.pos += int64(n)
	}
	return n, err
}

// WriteAt writes p at off.
func (f *File) WriteAt(p []byte, off int64) (int, error) { return f.writeAt(p, off, false) }

func (f *File) writeAt(p []byte, off int64, appendMode bool) (int, error) {
	f.fsys.mu.Lock()
	defer f.fsys.mu.Unlock()
	switch {
	case f.closed || f.fsys.closed:
		return 0, f.fail("write", fs.ErrClosed)
	case !f.writable():
		return 0, f.fail("write", syscall.EBADF)
	case off < 0:
		return 0, f.fail("write", syscall.EINVAL)
	}
	n, err := f.fsys.write(f.in, off, p, appendMode)
	if err != nil {
		err = f.fail("write", err)
	}
	return n, err
}

// Seek sets the position for the next Read or Write.
func (f *File) Seek(offset int64, whence int) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		offset += f.pos
	case io.SeekEnd:
		f.fsys.mu.RLock()
		offset += f.in.size
		f.fsys.mu.RUnlock()
	default:
		return 0, f.fail("seek", syscall.EINVAL)
	}
	if offset < 0 {
		return 0, f.fail("seek", syscall.EINVAL)
	}
	f.pos = offset
	return offset, nil
}

// Truncate changes the size of the file. Growing it leaves a hole that reads
// as zeros.
func (f *File) Truncate(size int64) error {
	f.fsys.mu.Lock()
	defer f.fsys.mu.Unlock()
	switch {
	case f.closed || f.fsys.closed:
		return f.fail("truncate", fs.ErrClosed)
	case !f.writable():
		return f.fail("truncate", syscall.EBADF)
	case size < 0:
		return f.fail("truncate", syscall.EINVAL)
	}
	if err := f.fsys.truncate(f.in, size); err != nil {
		return f.fail("truncate", err)
	}
	return nil
}

// Sync forces the file, and everything else written so far, to stable
// storage.
func (f *File) Sync() error { return f.fsys.Sync() }

// Stat describes the file.
func (f *File) Stat() (fs.FileInfo, error) {
	f.fsys.mu.RLock()
	defer f.fsys.mu.RUnlock()
	if f.closed {
		return nil, f.fail("stat", fs.ErrClosed)
	}
	return f.in.info(), nil
}

// ReadDir lists the directory, sorted by name, with the conventions of
// os.File.ReadDir: n <= 0 returns everything, n > 0 returns at most n entries
// and io.EOF once there are no more.
func (f *File) ReadDir(n int) ([]fs.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.listed {
		f.fsys.mu.RLock()
		if !f.in.isDir() {
			f.fsys.mu.RUnlock()
			return nil, f.fail("readdir", syscall.ENOTDIR)
		}
		f.entries, f.listed = f.in.entries(), true
		f.fsys.mu.RUnlock()
	}
	if n <= 0 {
		list := f.entries
		f.entries = nil
		return list, nil
	}
	if len(f.entries) == 0 {
		return nil, io.EOF
	}
	n = min(n, len(f.entries))
	list := f.entries[:n]
	f.entries = f.entries[n:]
	return list, nil
}

// Close closes the file. If it was removed while open, its data is released
// now.
func (f *File) Close() error {
	f.fsys.mu.Lock()
	defer f.fsys.mu.Unlock()
	if f.closed {
		return f.fail("close", fs.ErrClosed)
	}
	f.closed = true
	f.in.opens--
	if f.in.unlinked && f.in.opens == 0 {
		f.fsys.release(f.in)
	}
	return nil
}

// ReadFile returns the contents of a file.
func (fsys *FS) ReadFile(name string) ([]byte, error) {
	fsys.mu.RLock()
	defer fsys.mu.RUnlock()
	if fsys.closed {
		return nil, fs.ErrClosed
	}
	in, err := fsys.walk(split(name))
	if err != nil {
		return nil, pathErr("open", name, err)
	}
	if in.isDir() {
		return nil, pathErr("read", name, syscall.EISDIR)
	}
	buf := make([]byte, in.size)
	fsys.read(in, buf, 0)
	return buf, nil
}

// WriteFile writes data to a file, creating it or replacing its contents.
func (fsys *FS) WriteFile(name string, data []byte, perm fs.FileMode) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if fsys.closed {
		return fs.ErrClosed
	}
	in, err := fsys.openLocked(name, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err == nil {
		_, err = fsys.write(in, 0, data, false)
	}
	if err != nil {
		return pathErr("open", name, err)
	}
	return nil
}

// Truncate changes the size of a file.
func (fsys *FS) Truncate(name string, size int64) error {
	fsys.mu.Lock()
	defer fsys.mu.Unlock()
	if fsys.closed {
		return fs.ErrClosed
	}
	in, err := fsys.walk(split(name))
	switch {
	case err != nil:
	case in.isDir():
		err = syscall.EISDIR
	case size < 0:
		err = syscall.EINVAL
	default:
		err = fsys.truncate(in, size)
	}
	if err != nil {
		return pathErr("truncate", name, err)
	}
	return nil
}

// ioFS adapts a volume to io/fs.
type ioFS struct{ fsys *FS }

// IOFS returns a read-only view of the volume as an io/fs.FS.
func (fsys *FS) IOFS() fs.FS { return ioFS{fsys} }

func (v ioFS) Open(name string) (fs.File, error) {
	if !fs.ValidPath(name) {
		return nil, pathErr("open", name, fs.ErrInvalid)
	}
	f, err := v.fsys.Open(name)
	if err != nil {
		return nil, err
	}
	return f, nil
}
