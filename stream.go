package dicom

import (
	"context"
	"errors"
	"io"

	"github.com/suyashkumar/dicom/pkg/dicomio"
	"github.com/suyashkumar/dicom/pkg/frame"
)

// Sentinel errors returned by the context-aware streaming frame APIs. Callers
// can use errors.Is to distinguish an intentional early stop from a normal
// completion or a malformed/truncated DICOM.
var (
	// ErrStreamCanceled is returned when the stream stops early because its
	// context was canceled (the caller canceled ctx or called Close). It wraps
	// the context's underlying error (context.Canceled or
	// context.DeadlineExceeded), which is also available via StreamResult.Err.
	ErrStreamCanceled = errors.New("frame stream canceled before end of dicom")
)

// FrameStreamStatus describes how a FrameStream finished.
type FrameStreamStatus int

const (
	// StreamComplete means every element of the DICOM (including the file
	// tail after PixelData) was read successfully.
	StreamComplete FrameStreamStatus = iota
	// StreamCanceled means the stream stopped early due to context
	// cancellation (ctx canceled or FrameStream.Close called).
	StreamCanceled
	// StreamError means the stream stopped because of a parse or read error,
	// such as io.ErrUnexpectedEOF from truncated PixelData.
	StreamError
)

// ReadCanceler may optionally be implemented by the io.Reader passed to the
// streaming APIs. If the reader implements it, CancelPendingRead is invoked
// once when the stream's context is canceled while a read is in flight (for
// example while waiting for the next frame's bytes).
//
// This is the supported way to unblock reads backed by a network connection
// or a device: the caller implements Read and CancelPendingRead on their
// reader so that a blocked Read returns promptly (typically with an error)
// after CancelPendingRead. This library never cancels or closes an arbitrary
// io.Reader, and it never closes the reader it was given: canceling a plain
// io.Reader whose Read blocks without honoring this interface cannot be
// guaranteed to interrupt the blocked read.
//
// CancelPendingRead must be safe to call concurrently with Read.
type ReadCanceler interface {
	CancelPendingRead()
}

// FrameHandler is the per-frame callback used by ParseFrames. It receives each
// frame as it is parsed. The frame (and all of its pixel buffers) is owned by
// the handler for the duration of the call only: do not retain the frame or
// its slices after the handler returns without copying them. Returning an
// error stops the stream immediately and the error is returned to the caller
// (wrapped, not transformed, so errors.Is works against it).
type FrameHandler func(f *frame.Frame) error

// StreamResult is the terminal outcome of a stream, returned by
// FrameStream.Close and by ParseFrames.
type StreamResult struct {
	// Status indicates how the stream finished.
	Status FrameStreamStatus
	// Metadata is the set of elements parsed up to termination. For
	// StreamComplete this is the full Dataset. For StreamCanceled and
	// StreamError it is the partial Dataset parsed so far (it always includes
	// the file metadata, which is parsed when the stream is constructed).
	Metadata Dataset
	// Err is nil for StreamComplete. For StreamCanceled it wraps
	// ErrStreamCanceled and the context's error; for StreamError it is the
	// parse/read error that stopped the stream (e.g. io.ErrUnexpectedEOF when
	// PixelData is truncated).
	Err error
}

// FrameStream is a context-aware, pull-based streaming frame reader.
//
// Frames are produced lazily as the consumer calls Next: Next only returns
// frames that were actually parsed, and pixel data is never fully decoded up
// front. Parsing runs in a bounded internal goroutine that delivers frames
// into a buffer of at most bufferedFrames frames, so a slow consumer applies
// backpressure on the parser and on the underlying reader.
//
// Ownership: every *frame.Frame returned by Next owns freshly allocated pixel
// buffers. The parser never reuses or overwrites them, so a frame returned by
// Next stays valid while later frames are read. The FrameStream owns only the
// goroutine and channels it created; it never closes the io.Reader passed to
// NewFrameStream.
//
// Every FrameStream must be finished with Close (the returned StreamResult is
// also cached and returned by subsequent Close and Wait calls).
type FrameStream struct {
	frames       chan *frame.Frame
	ctx          context.Context
	cancelRead   func()
	cancel       context.CancelFunc
	done         chan struct{}
	bufferedSize int

	mu     chan struct{} // closed once terminal is set
	result StreamResult
}

// NewFrameStream parses the DICOM header and metadata from in immediately and
// returns a FrameStream for pulling frames on demand. bytesToRead bounds the
// parse (use the file size, or a non-positive value to read until EOF).
//
// The returned metadata Dataset contains the header elements parsed during
// construction; more elements accumulate in the terminal StreamResult.Metadata.
//
// bufferedFrames is the maximum number of parsed-but-unconsumed frames the
// internal buffer may hold (0 parses strictly on demand). A slow consumer past
// that limit blocks the parser and the underlying reader (backpressure).
//
// ctx governs the whole stream: canceling it (or calling Close) stops the
// parser as soon as it reaches a cancellation checkpoint (while waiting to
// hand a frame to a slow consumer, before reading the next frame, inside a
// sequence, or at the file tail). If in implements ReadCanceler, its
// CancelPendingRead is invoked to unblock an in-flight Read; otherwise a
// blocked Read is not forcibly interrupted.
func NewFrameStream(ctx context.Context, in io.Reader, bytesToRead int64, bufferedFrames int, opts ...ParseOption) (*FrameStream, Dataset, error) {
	if bufferedFrames < 0 {
		return nil, Dataset{}, errors.New("bufferedFrames must be >= 0")
	}
	limit := bytesToRead
	if bytesToRead < 0 {
		limit = dicomio.LimitReadUntilEOF
	}
	ctx, cancel := context.WithCancel(ctx)
	s := &FrameStream{
		frames:       make(chan *frame.Frame, bufferedFrames),
		ctx:          ctx,
		cancel:       cancel,
		done:         make(chan struct{}),
		bufferedSize: bufferedFrames,
		mu:           make(chan struct{}),
	}
	if rc, ok := in.(ReadCanceler); ok {
		s.cancelRead = rc.CancelPendingRead
	}

	// For the whole stream lifetime, unblock an in-flight Read on a reader
	// that implements ReadCanceler as soon as ctx is canceled. This supports
	// callers whose Read is backed by a cancellable connection/device. The
	// reader itself is never closed.
	go s.cancelWatch(ctx, s.done)

	p, err := newParserInternal(in, limit, nil, s, opts...)
	if err != nil {
		cancel()
		s.finish(StreamError, Dataset{}, err)
		return nil, Dataset{}, err
	}
	go s.run(ctx, p)
	return s, p.GetMetadata(), nil
}

// Emit is the bridge between the parser goroutine and the pull consumer:
// sending blocks (backpressure) until the consumer pulls, the buffer has room,
// or the stream is terminated (Close/ctx cancellation). Each emitted frame
// keeps its own buffers; Emit never mutates or reuses f.
func (s *FrameStream) Emit(f *frame.Frame) error {
	select {
	case s.frames <- f:
		return nil
	case <-s.ctx.Done():
		return context.Cause(s.ctx)
	case <-s.done:
		// Already terminated (e.g. Close raced ahead); treat as canceled.
		return ErrStreamCanceled
	}
}

// cancelWatch invokes CancelPendingRead after ctx is canceled, until stop is
// closed. CancelPendingRead is idempotent from the caller's perspective; Close
// may invoke it again harmlessly.
func (s *FrameStream) cancelWatch(ctx context.Context, stop <-chan struct{}) {
	if s.cancelRead == nil {
		return
	}
	select {
	case <-ctx.Done():
		s.cancelRead()
	case <-stop:
	}
}

// run drives the parser until completion, cancellation, or an error.
func (s *FrameStream) run(ctx context.Context, p *Parser) {
	// Any ctx cancellation while inside the parser surfaces either directly
	// (see checkpoints) or via the Emit select.
	p.reader.checkCancel = func() error {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			return nil
		}
	}

	var runErr error
	for runErr == nil && !p.reader.rawReader.IsLimitExhausted() {
		if err := p.reader.checkpoint(); err != nil {
			runErr = err
			break
		}
		_, runErr = p.Next()
		if errors.Is(runErr, io.EOF) {
			runErr = nil
			break
		}
	}

	// Capture whether the caller actually canceled before we cancel our own
	// derived context below; otherwise a genuine parse error (e.g. truncated
	// pixels) would be misclassified as a cancellation.
	callerCanceled := ctx.Err() != nil
	s.cancel()
	close(s.frames)

	status := StreamComplete
	if runErr != nil {
		canceled := errors.Is(runErr, context.Canceled) ||
			errors.Is(runErr, context.DeadlineExceeded) ||
			errors.Is(runErr, ErrStreamCanceled)
		// A canceled ctx can also surface as the underlying reader's error when
		// the reader was unblocked via ReadCanceler.CancelPendingRead.
		if !canceled && callerCanceled {
			canceled = true
		}
		if canceled {
			status = StreamCanceled
			runErr = errors.Join(ErrStreamCanceled, ctx.Err(), runErr)
		} else {
			status = StreamError
		}
	}
	s.finish(status, p.dataset, runErr)
}

// finish records the terminal result exactly once and signals completion.
func (s *FrameStream) finish(status FrameStreamStatus, metadata Dataset, err error) {
	select {
	case <-s.mu:
		return // already finished
	default:
	}
	s.result = StreamResult{Status: status, Metadata: metadata, Err: err}
	close(s.mu)
	close(s.done)
}

// Next returns the next parsed frame, or nil with a terminal error when the
// stream has finished. A normal end of DICOM returns (nil, io.EOF); an early
// cancel returns an error wrapping ErrStreamCanceled; truncated data returns
// the underlying read error (e.g. io.ErrUnexpectedEOF).
//
// The returned frame's buffers are owned by the caller and remain valid after
// subsequent Next calls.
func (s *FrameStream) Next() (*frame.Frame, error) {
	select {
	case f, ok := <-s.frames:
		if !ok {
			return nil, s.terminalErr()
		}
		return f, nil
	case <-s.done:
		return nil, s.terminalErr()
	}
}

// terminalErr maps the final stream status to the error returned by Next.
func (s *FrameStream) terminalErr() error {
	<-s.mu
	switch s.result.Status {
	case StreamComplete:
		return io.EOF
	default:
		return s.result.Err
	}
}

// Wait blocks until the stream reaches its terminal state and returns the
// StreamResult. Wait does not cancel the stream, so it blocks until all frames
// are consumed and the parser finishes naturally, or until the stream's
// context is canceled elsewhere.
func (s *FrameStream) Wait() StreamResult {
	<-s.done
	<-s.mu
	return s.result
}

// Close stops the stream early (if it has not already finished), releases the
// goroutine and channels owned by the FrameStream, and returns the terminal
// StreamResult. It is idempotent. Close never closes the underlying io.Reader.
//
// Frames still buffered and not yet consumed are discarded; frames already
// returned by Next are unaffected.
func (s *FrameStream) Close() StreamResult {
	s.cancel()
	if s.cancelRead != nil {
		// Unblock a read possibly in flight.
		s.cancelRead()
	}
	<-s.done
	<-s.mu
	return s.result
}

// BufferedFrames returns the maximum number of parsed-but-unconsumed frames the
// stream buffers (0 means frames are parsed strictly on demand).
func (s *FrameStream) BufferedFrames() int { return s.bufferedSize }

// ParseFrames is a callback-style streaming convenience: it parses the DICOM
// header, then invokes onFrame for each frame as it is parsed, applying
// backpressure while onFrame runs. It returns when the DICOM is fully read, ctx
// is canceled, onFrame returns an error, or a parse/read error occurs.
//
// onFrame must not retain frame buffers past its return without copying. The
// returned StreamResult distinguishes full reads (StreamComplete) from early
// stops (StreamCanceled) and surfaces truncated-data errors (StreamError). The
// caller's reader is never closed.
func ParseFrames(ctx context.Context, in io.Reader, bytesToRead int64, opts []ParseOption, onFrame FrameHandler) StreamResult {
	s, _, err := NewFrameStream(ctx, in, bytesToRead, 0, opts...)
	if err != nil {
		return StreamResult{Status: StreamError, Err: err}
	}
	defer s.Close()

	for {
		f, err := s.Next()
		if errors.Is(err, io.EOF) {
			return s.Wait()
		}
		if err != nil {
			return s.Close()
		}
		if hErr := onFrame(f); hErr != nil {
			res := s.Close()
			// If the caller's context was independently canceled, that takes
			// precedence; otherwise the handler error is the reason the stream
			// stopped and must be reported verbatim (Close tears the parser
			// down via cancellation).
			if ctx.Err() != nil {
				return res
			}
			return StreamResult{Status: StreamError, Metadata: res.Metadata, Err: hErr}
		}
	}
}
