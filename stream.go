package dicom

import (
	"context"
	"errors"
	"io"
	"sync/atomic"

	"github.com/suyashkumar/dicom/pkg/dicomio"
	"github.com/suyashkumar/dicom/pkg/frame"
)

// Frame streaming sentinel results.
var (
	// ErrStreamStopped indicates that a frame stream ended early because it was
	// explicitly stopped (FrameStream.Close) or its parse context was cancelled.
	// It always wraps the underlying context error, so errors.Is(err,
	// context.Canceled) (or context.DeadlineExceeded) remains true.
	ErrStreamStopped = errors.New("frame stream stopped before the DICOM was fully read")
)

// StreamSummary describes how a frame stream finished. It is produced for both
// the pull-based (FrameStream) and callback-based (ParseFramesWithCallback)
// APIs.
type StreamSummary struct {
	// FramesDelivered is the number of frames handed to the consumer (returned
	// by a successful FrameStream.Next or passed to the callback). A frame that
	// was decoded but never consumed because the stream was stopped is not
	// counted.
	FramesDelivered int

	// Complete reports whether the entire DICOM was parsed and every frame was
	// consumed. It is true only when the parser reached the end of the input
	// cleanly. An early stop, a parse error or a truncated pixel data element
	// all leave Complete false.
	Complete bool

	// Err is the terminal result of the stream:
	//   - nil when Complete is true,
	//   - an error wrapping ErrStreamStopped and the context error when stopped
	//     early,
	//   - the parse error otherwise (including io.ErrUnexpectedEOF and the
	//     wrapped read error for truncated pixel data).
	Err error
}

// ParseOptions groups tuning parameters for the context-aware frame streaming
// APIs. The zero value is valid.
type ParseOptions struct {
	// MaxBufferedFrames bounds the number of frames the parser may produce
	// ahead of the consumer. When this many unconsumed frames are queued, the
	// parser blocks on the next frame until the consumer catches up or the
	// context is cancelled, which applies back-pressure. 0 means no frames are
	// buffered ahead: each frame must be consumed before the parser continues.
	MaxBufferedFrames int

	// ParseOptions forwards ordinary parser options (for example
	// AllowMismatchPixelDataLength). SkipPixelData and
	// SkipProcessingPixelDataValue are not meaningful while streaming frames and
	// should not be used.
	Options []ParseOption
}

// FrameStream is a pull-based, context-aware iterator over the frames of a
// DICOM. Frames are decoded lazily as they are requested; pixels are never all
// decoded up front before the first frame is delivered.
//
// A FrameStream must be closed by calling Close (or by fully draining it until
// Next returns io.EOF or another terminal error). Closing stops the background
// parser when it is still running. Closing a FrameStream never closes the
// io.Reader passed to ParseFrames; that Reader is owned by the caller and may
// continue to be used afterwards.
//
// FrameStream is not safe for concurrent use by multiple goroutines.
type FrameStream struct {
	ctx       context.Context
	cancel    context.CancelFunc
	out       chan *frame.Frame
	ack       chan struct{}
	metadata  Dataset
	consumed  atomic.Int64
	closed    bool
	finishErr error

	done    chan struct{}
	summary *StreamSummary
}

// ParseFrames creates a context-aware, pull-based frame stream over the DICOM
// read from in, which has bytesToRead bytes left (use
// ParseFramesUntilEOF to read until EOF). It reads and returns the DICOM
// metadata up front (the file header), then decodes pixel-data frames lazily in
// the background as they are pulled with Next.
// Metadata is available immediately from FrameStream.Metadata. Frames are
// delivered to Next in order. The parse context governs the lifetime of the
// background parser; cancelling it (or calling Close) makes the parser stop as
// soon as it reaches a cancellation point.
//
// Cancellation points include: while blocked waiting for the consumer to accept
// a frame, between frames (native and encapsulated), while reading the next
// frame from a caller-provided ContextReader, while iterating inside a
// sequence, and at the tail of the file. A plain io.Reader that blocks in Read
// cannot be interrupted by the context; implement dicomio.ContextReader to
// opt into interruptible reads.
//
// ParseFrames does not read or close beyond what it needs and never closes in.
func ParseFrames(ctx context.Context, in io.Reader, bytesToRead int64, opts ParseOptions) (*FrameStream, Dataset, error) {
	return newFrameStream(ctx, in, bytesToRead, opts)
}

// ParseFramesUntilEOF is ParseFrames with an unbounded read length.
func ParseFramesUntilEOF(ctx context.Context, in io.Reader, opts ParseOptions) (*FrameStream, Dataset, error) {
	return newFrameStream(ctx, in, dicomio.LimitReadUntilEOF, opts)
}

func newFrameStream(ctx context.Context, in io.Reader, bytesToRead int64, opts ParseOptions) (*FrameStream, Dataset, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithCancel(ctx)

	fs := &FrameStream{
		ctx:    ctx,
		cancel: cancel,
		out:    make(chan *frame.Frame, opts.MaxBufferedFrames),
		ack:    make(chan struct{}),
		done:   make(chan struct{}),
	}

	// Owned by this stream: the frame sink hands decoded frames to the stream
	// and respects cancellation/back-pressure. It is the only channel the
	// stream closes; the caller's Reader is never closed.
	sink := func(f *frame.Frame) error { return fs.deliver(f) }

	allOpts := make([]ParseOption, 0, len(opts.Options)+2)
	allOpts = append(allOpts, opts.Options...)
	allOpts = append(allOpts, withFrameSink(sink), withParseContext(ctx))

	p, err := NewParser(in, bytesToRead, nil, allOpts...)
	if err != nil {
		cancel()
		return nil, Dataset{}, err
	}
	fs.metadata = p.GetMetadata()

	go fs.run(p)
	return fs, fs.metadata, nil
}

// deliver is invoked by the parser goroutine for each decoded frame. It
// applies back-pressure when the bounded buffer is full and returns promptly
// with the context error when the stream is stopped.
func (s *FrameStream) deliver(f *frame.Frame) error {
	select {
	case s.out <- f:
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
	// The frame is not "delivered" until the consumer accepts it. Wait for that
	// ack (guarded by the same context) so FramesDelivered never counts a frame
	// still sitting in the bounded buffer.
	select {
	case <-s.ack:
		return nil
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

// run executes the parser to completion and records the terminal summary. It
// is the single producer closing the owned output channel.
func (s *FrameStream) run(p *Parser) {
	var summary StreamSummary

	parseErr := parseAll(p)

	switch {
	case parseErr == nil || errors.Is(parseErr, ErrorEndOfDICOM):
		summary = StreamSummary{FramesDelivered: int(s.consumed.Load()), Complete: true, Err: nil}
	case errors.Is(parseErr, context.Canceled) || errors.Is(parseErr, context.DeadlineExceeded):
		summary = StreamSummary{
			FramesDelivered: int(s.consumed.Load()),
			Complete:        false,
			Err:             fmtStopError(s.ctx.Err(), parseErr),
		}
	default:
		summary = StreamSummary{FramesDelivered: int(s.consumed.Load()), Complete: false, Err: parseErr}
	}

	// Set the summary before closing out so a consumer woken by the close always
	// observes it.
	s.summary = &summary
	if summary.Complete {
		s.finishErr = io.EOF
	} else {
		s.finishErr = summary.Err
	}
	close(s.out)
	close(s.done)
}

func parseAll(p *Parser) error {
	for !p.reader.rawReader.IsLimitExhausted() {
		if err := p.reader.checkContext(); err != nil {
			return err
		}
		_, err := p.Next()
		if err != nil {
			// Only a bare io.EOF (natural end) marks completion. An
			// io.ErrUnexpectedEOF also satisfies errors.Is(err, io.EOF) but
			// represents truncation and must be reported.
			if err == io.EOF {
				return nil
			}
			if errors.Is(err, ErrorEndOfDICOM) {
				return ErrorEndOfDICOM
			}
			return err
		}
	}
	return nil
}

func fmtStopError(ctxErr, parseErr error) error {
	switch {
	case ctxErr != nil:
		return errors.Join(ErrStreamStopped, ctxErr)
	default:
		return errors.Join(ErrStreamStopped, parseErr)
	}
}

// Next returns the next decoded frame. The returned frame and its byte buffers
// are owned by the caller after this call. The parser never reuses or mutates
// a delivered frame's buffer when decoding subsequent frames, so retaining the
// frame is safe.
//
// Next blocks while waiting for the next frame to be produced, while the parser
// reads it, or while the parser is blocked by back-pressure.
//
// The terminal results are distinguishable:
//   - io.EOF: the DICOM was fully read (complete),
//   - an error wrapping ErrStreamStopped (and context.Canceled): stopped early,
//   - any other error: a parse/read error, including io.ErrUnexpectedEOF for
//     truncated pixel data. The underlying read error is wrapped and can be
//     inspected with errors.Is/errors.As.
//
// ctx applies to this single call only; cancelling it aborts the wait but does
// not stop the background parser. Use Close or the context passed to
// ParseFrames to stop the stream.
func (s *FrameStream) Next(ctx context.Context) (*frame.Frame, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	// If the stream-level context is already cancelled, do not hand out a
	// residual buffered/in-flight frame; surface the terminal result.
	if err := s.ctx.Err(); err != nil {
		return nil, errors.Join(ErrStreamStopped, err)
	}
	select {
	case f, ok := <-s.out:
		if !ok {
			return nil, s.finishErr
		}
		s.consumed.Add(1)
		select {
		case s.ack <- struct{}{}:
		case <-s.ctx.Done():
			return nil, errors.Join(ErrStreamStopped, s.ctx.Err())
		case <-ctx.Done():
			return nil, errors.Join(ErrStreamStopped, ctx.Err())
		}
		return f, nil
	case <-ctx.Done():
		return nil, errors.Join(ErrStreamStopped, ctx.Err())
	}
}

// Close stops the background parser if it is still running and releases the
// resources created by this FrameStream. It is idempotent and safe to call
// after the stream has finished. It never closes the io.Reader supplied to
// ParseFrames.
func (s *FrameStream) Close() error {
	if s.closed {
		return nil
	}
	s.closed = true
	s.cancel()
	<-s.done
	return nil
}

// Result blocks until the stream has fully finished (the parser reached EOF,
// hit an error, or was stopped) and returns its summary. It does not consume
// frames; use it after draining with Next or after Close.
func (s *FrameStream) Result() StreamSummary {
	<-s.done
	return *s.summary
}

// Metadata returns the metadata elements read during parser initialization.
func (s *FrameStream) Metadata() Dataset { return s.metadata }

// ParseFramesWithCallback is the callback-based variant of ParseFrames. It
// reads the metadata up front, then decodes and invokes onFrame for every
// frame, stopping as soon as onFrame returns a non-nil error or the context is
// cancelled. The callback receives frames in order; each frame's buffers are
// owned by the callback for the duration of the call and are never reused by
// later frames.
//
// Returning an error from onFrame stops the stream early; that error is wrapped
// into the returned summary's Err. The returned summary distinguishes a complete
// read from an early stop or parse error. The caller's Reader is never closed.
func ParseFramesWithCallback(ctx context.Context, in io.Reader, bytesToRead int64, opts ParseOptions, onFrame func(*frame.Frame) error) (Dataset, StreamSummary, error) {
	if onFrame == nil {
		onFrame = func(*frame.Frame) error { return nil }
	}
	s, metadata, err := newFrameStream(ctx, in, bytesToRead, opts)
	if err != nil {
		return Dataset{}, StreamSummary{}, err
	}

	var callbackErr error
	pullCtx := context.Background()
	for {
		f, nerr := s.Next(pullCtx)
		if nerr != nil {
			// Terminal or stop error from the stream.
			s.Close()
			break
		}
		if cerr := onFrame(f); cerr != nil {
			callbackErr = cerr
			s.Close()
			break
		}
	}

	summary := s.Result()
	if callbackErr != nil {
		summary.Complete = false
		summary.Err = errors.Join(callbackErr, ErrStreamStopped)
	}
	return metadata, summary, nil
}
