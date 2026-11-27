// Package ipc holds godeep's inter-process plumbing: byte pipes (shell
// pipelines, a Proc's stdin/stdout) and typed request/reply ports.
//
// Nothing here is global: a Pipe or a Port is a value handed to the Procs
// that use it, and a Namespace of named ports is itself a capability.
package ipc

import (
	"errors"
	"io"
	"sync"
)

// ErrClosedPipe is returned by Write once the read end is closed (the
// reader exited: `yes | head 3`).
var ErrClosedPipe = io.ErrClosedPipe

// PipeSize is how many chunks a pipe buffers before Write blocks.
const PipeSize = 16

// Pipe returns the two ends of a buffered byte pipe. Writes are copied
// (the caller may reuse its buffer), and a Write blocks once PipeSize
// chunks are waiting, until the reader catches up or closes its end.
//
// Neither end takes a context: a Proc blocked on one is woken by closing
// it, which is what proc.Spec.Ends does when the Proc dies.
func Pipe() (*PipeReader, *PipeWriter) {
	p := &pipe{
		data:  make(chan []byte, PipeSize),
		wdone: make(chan struct{}),
		rdone: make(chan struct{}),
	}
	return &PipeReader{p}, &PipeWriter{p}
}

type pipe struct {
	data chan []byte

	wonce, ronce sync.Once
	wdone, rdone chan struct{} // closed by the writer's / reader's Close
	werr, rerr   error         // set before the matching close

	rmu  sync.Mutex // serializes readers over left
	left []byte     // the rest of a chunk a Read didn't take
}

// PipeReader is a pipe's read end.
type PipeReader struct{ p *pipe }

// Read returns buffered bytes, blocks while the pipe is empty, and returns
// io.EOF (or the writer's CloseWithError error) once the write end is
// closed and everything written has been read.
func (r *PipeReader) Read(b []byte) (int, error) {
	p := r.p
	p.rmu.Lock()
	defer p.rmu.Unlock()
	if len(b) == 0 {
		return 0, nil
	}
	select {
	case <-p.rdone:
		return 0, ErrClosedPipe
	default:
	}
	if len(p.left) == 0 {
		select {
		case c := <-p.data:
			p.left = c
		case <-p.rdone:
			return 0, ErrClosedPipe
		case <-p.wdone:
			// Closed: drain what is still buffered first.
			select {
			case c := <-p.data:
				p.left = c
			default:
				return 0, p.werr
			}
		}
	}
	n := copy(b, p.left)
	p.left = p.left[n:]
	return n, nil
}

// Close closes the read end: pending and later Writes fail with
// ErrClosedPipe. It is idempotent.
func (r *PipeReader) Close() error { return r.CloseWithError(nil) }

// CloseWithError closes the read end; writers get err (ErrClosedPipe if nil).
func (r *PipeReader) CloseWithError(err error) error {
	if err == nil {
		err = ErrClosedPipe
	}
	r.p.ronce.Do(func() {
		r.p.rerr = err
		close(r.p.rdone)
	})
	return nil
}

// PipeWriter is a pipe's write end.
type PipeWriter struct{ p *pipe }

// Write copies b into the pipe. It blocks while the pipe is full and fails
// once either end is closed.
func (w *PipeWriter) Write(b []byte) (int, error) {
	p := w.p
	select {
	case <-p.wdone:
		return 0, ErrClosedPipe
	default:
	}
	if len(b) == 0 {
		return 0, nil
	}
	c := append([]byte(nil), b...)
	select {
	case p.data <- c:
		return len(b), nil
	case <-p.rdone:
		return 0, p.rerr
	case <-p.wdone:
		return 0, ErrClosedPipe
	}
}

// Close closes the write end: the reader gets io.EOF after the buffered
// bytes. It is idempotent.
func (w *PipeWriter) Close() error { return w.CloseWithError(nil) }

// CloseWithError closes the write end; the reader gets err (io.EOF if nil)
// after the buffered bytes.
func (w *PipeWriter) CloseWithError(err error) error {
	if err == nil {
		err = io.EOF
	}
	w.p.wonce.Do(func() {
		w.p.werr = err
		close(w.p.wdone)
	})
	return nil
}

// IsClosedPipe reports whether err says the other end went away: the
// normal way for an upstream stage of a pipeline to end.
func IsClosedPipe(err error) bool { return errors.Is(err, ErrClosedPipe) }
