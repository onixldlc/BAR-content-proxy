package main

import (
	"io"
	"os"
	"sync"
)

// partialFile is a file being filled by one or more concurrent range
// workers, readable while it is still being written.
//
// Readers only ever see the contiguous completed prefix, so a client
// streaming the response never observes a hole punched by a later part
// that finished early.
type partialFile struct {
	f       *os.File
	total   int64
	unknown bool // upstream did not tell us the length

	mu    sync.Mutex
	cond  *sync.Cond
	sizes []int64
	prog  []int64
	err   error
	done  bool

	refs   int
	closed bool
}

func newPartialFile(f *os.File, total int64, sizes []int64) *partialFile {
	pf := &partialFile{
		f:       f,
		total:   total,
		unknown: total < 0,
		sizes:   sizes,
		prog:    make([]int64, len(sizes)),
		// The downloader itself holds the first reference. Without it a
		// transfer that completes before a late client attaches would
		// close the file out from under that client's reader.
		refs: 1,
	}
	pf.cond = sync.NewCond(&pf.mu)
	return pf
}

func (pf *partialFile) prefixLocked() int64 {
	if pf.unknown {
		return pf.prog[0]
	}
	var p int64
	for i := range pf.sizes {
		p += pf.prog[i]
		if pf.prog[i] < pf.sizes[i] {
			break
		}
	}
	return p
}

func (pf *partialFile) advance(idx int, n int64) {
	pf.mu.Lock()
	pf.prog[idx] += n
	pf.cond.Broadcast()
	pf.mu.Unlock()
}

// reset rolls a part back to zero so a retry re-reads it from the start.
func (pf *partialFile) reset(idx int) {
	pf.mu.Lock()
	pf.prog[idx] = 0
	pf.cond.Broadcast()
	pf.mu.Unlock()
}

func (pf *partialFile) finish(err error) {
	pf.mu.Lock()
	pf.err = err
	pf.done = true
	pf.cond.Broadcast()
	pf.maybeClose()
	pf.mu.Unlock()
}

func (pf *partialFile) maybeClose() {
	if pf.closed || pf.refs > 0 || !pf.done {
		return
	}
	pf.closed = true
	pf.f.Close()
}

// partWriter feeds one part's bytes into the file at an absolute offset.
type partWriter struct {
	pf  *partialFile
	idx int
	off int64
}

func (w *partWriter) Write(p []byte) (int, error) {
	n, err := w.pf.f.WriteAt(p, w.off)
	w.off += int64(n)
	if n > 0 {
		w.pf.advance(w.idx, int64(n))
	}
	return n, err
}

// release drops the downloader's own reference, once the download is both
// finished and no longer reachable by new clients.
func (pf *partialFile) release() {
	pf.mu.Lock()
	pf.refs--
	pf.maybeClose()
	pf.mu.Unlock()
}

// NewReader hands out an independent sequential view of the file.
func (pf *partialFile) NewReader() *pfReader {
	pf.mu.Lock()
	pf.refs++
	pf.mu.Unlock()
	return &pfReader{pf: pf}
}

type pfReader struct {
	pf     *partialFile
	pos    int64
	closed bool
}

func (r *pfReader) Read(p []byte) (int, error) {
	pf := r.pf
	pf.mu.Lock()
	var avail int64
	for {
		avail = pf.prefixLocked() - r.pos
		if avail > 0 {
			break
		}
		if pf.err != nil {
			err := pf.err
			pf.mu.Unlock()
			return 0, err
		}
		if pf.done {
			pf.mu.Unlock()
			if pf.unknown || r.pos >= pf.total {
				return 0, io.EOF
			}
			return 0, io.ErrUnexpectedEOF
		}
		pf.cond.Wait()
	}
	pf.mu.Unlock()

	if int64(len(p)) > avail {
		p = p[:avail]
	}
	n, err := pf.f.ReadAt(p, r.pos)
	r.pos += int64(n)
	if err == io.EOF && n > 0 {
		err = nil
	}
	return n, err
}

func (r *pfReader) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	pf := r.pf
	pf.mu.Lock()
	pf.refs--
	pf.maybeClose()
	pf.mu.Unlock()
	return nil
}
