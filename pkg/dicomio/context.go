package dicomio

import (
	"context"
	"io"
)

// ContextReader is implemented by io.Readers that are able to interrupt an
// in-progress (potentially blocking) Read when a context is cancelled. The
// library cannot cancel an arbitrary blocking io.Reader on its own: a plain
// io.Read has no way to be preempted. Callers whose underlying source may block
// indefinitely (for example a network socket or a device handle) can pass a
// Reader that also implements ContextReader, and context-aware entry points
// (such as dicom.ParseFrames) route their reads through ReadContext using the
// caller-supplied context.
//
// ReadContext follows the same contract as io.Reader but should return as soon
// as the context is cancelled, typically with the context's error
// (context.Canceled or context.DeadlineExceeded), even if no bytes were
// produced.
type ContextReader interface {
	ReadContext(ctx context.Context, p []byte) (n int, err error)
}

// cancellableReader bridges a ContextReader into the ordinary io.Reader
// interface while forwarding a context that can be swapped at runtime. This is
// what sits underneath the buffered reader so that blocking reads on a
// caller-provided ContextReader honor cancellation.
type cancellableReader struct {
	cr  ContextReader
	ctx func() context.Context
}

// MaybeCancellable wraps in with a cancellableReader when in implements
// ContextReader and a non-nil context provider is supplied. Otherwise it
// returns in unchanged. The provided ctx function is consulted on every read,
// so the effective context can be updated after construction.
func MaybeCancellable(in io.Reader, ctx func() context.Context) io.Reader {
	if ctx == nil {
		return in
	}
	cr, ok := in.(ContextReader)
	if !ok {
		return in
	}
	return &cancellableReader{cr: cr, ctx: ctx}
}

func (c *cancellableReader) Read(p []byte) (int, error) {
	return c.cr.ReadContext(c.ctx(), p)
}
