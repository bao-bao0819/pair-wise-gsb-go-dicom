package dicom

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// gateReader serves bytes before gate freely (so the DICOM header/metadata can
// be parsed during construction), then requires one token per byte. This gives
// tests exact, deterministic control over how far the parser reads, without
// sleeps or devices: it is the synchronization point that lets tests stop the
// parser at precise positions (waiting for the next frame's bytes, blocked in
// Emit on a slow consumer, or at the file tail). It also implements
// ReadCanceler.
// gateReader serves bytes before gate freely (so the DICOM header/metadata can
// be parsed during construction), then requires the reader to consume one
// buffered token per byte. Tests enqueue exactly the number of bytes they want
// to release, which gives deterministic byte-level control with no handshake
// races, and lets a blocked read be interrupted via ReadCanceler. It is the
// synchronization point for stopping at "reading next frame", "blocked handing
// a frame to a slow consumer", and "file tail", without sleeps or devices.
type gateReader struct {
	mu        sync.Mutex
	data      []byte
	pos       int
	gate      int
	tokens    chan struct{} // one token permits one byte past the gate
	disarmed  chan struct{} // closed: serve remaining bytes without tokens
	canceled  chan struct{} // closed: blocked reads return errReadCanceled
	cancels   int
	progress  chan struct{} // non-blocking ping after each post-gate byte
	heldMu    sync.Mutex
	heldToken chan struct{} // if non-nil, a Read is parked selecting on it
	heldArmed chan struct{} // closed once a Read is selecting on heldToken
	heldAfter int           // only block a Read when consumed pos >= heldAfter
}

var errReadCanceled = errors.New("read unblocked by CancelPendingRead")

func newGateReader(data []byte, gateAt int) *gateReader {
	return &gateReader{
		data:     data,
		gate:     gateAt,
		tokens:   make(chan struct{}, len(data)),
		disarmed: make(chan struct{}),
		canceled: make(chan struct{}),
		progress: make(chan struct{}, 1),
	}
}

func (g *gateReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	g.mu.Lock()
	pos, gate := g.pos, g.gate
	g.mu.Unlock()

	if pos < gate {
		// Prefix bytes: serve up to the gate in this call.
		g.mu.Lock()
		n := gate - g.pos
		if n > len(p) {
			n = len(p)
		}
		if g.pos+n > len(g.data) {
			n = len(g.data) - g.pos
		}
		cnt := copy(p[:n], g.data[g.pos:g.pos+n])
		g.pos += cnt
		g.mu.Unlock()
		return cnt, nil
	}

	// Past the gate: one byte per token.
	g.heldMu.Lock()
	held := g.heldToken
	armed := g.heldArmed
	after := g.heldAfter
	g.heldMu.Unlock()
	if held != nil && armed != nil && pos >= after {
		// Signal that we are about to block, then wait for the held token.
		close(armed)
		select {
		case <-held:
		case <-g.disarmed:
		case <-g.canceled:
			return 0, errReadCanceled
		}
	}
	select {
	case <-g.tokens:
	case <-g.disarmed:
	case <-g.canceled:
		return 0, errReadCanceled
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.pos >= len(g.data) {
		return 0, io.EOF
	}
	p[0] = g.data[g.pos]
	g.pos++
	select {
	case g.progress <- struct{}{}:
	default:
	}
	return 1, nil
}

// CancelPendingRead unblocks a token-waiting read. Safe to call repeatedly.
func (g *gateReader) CancelPendingRead() {
	g.mu.Lock()
	g.cancels++
	g.mu.Unlock()
	select {
	case <-g.canceled:
	default:
		close(g.canceled)
	}
}

func (g *gateReader) cancelCount() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.cancels
}

func (g *gateReader) bytesConsumed() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pos
}

// enqueue releases n bytes by adding n tokens (non-blocking; channel is large).
func (g *gateReader) enqueue(n int) {
	for i := 0; i < n; i++ {
		g.tokens <- struct{}{}
	}
}

// drainTokens removes any released-but-unconsumed tokens, so tests can advance
// to a precise byte boundary without letting the parser read past it.
func (g *gateReader) drainTokens() {
	for {
		select {
		case <-g.tokens:
		default:
			return
		}
	}
}

// waitConsumed blocks until the reader has consumed targetPos bytes or fails on
// timeout. It is a synchronization point for "parser reached this position".
func (g *gateReader) waitConsumed(t *testing.T, targetPos int, what string) {
	t.Helper()
	for {
		if g.bytesConsumed() >= targetPos {
			return
		}
		select {
		case <-g.progress:
		case <-time.After(5 * time.Second):
			t.Fatalf("timed out waiting for parser to reach %s (pos=%d target=%d)", what, g.bytesConsumed(), targetPos)
		}
	}
}

// disarm serves all remaining bytes without tokens (used at the file tail).
func (g *gateReader) disarmGate() {
	select {
	case <-g.disarmed:
	default:
		close(g.disarmed)
	}
}

// armHeldRead arms a "held" token: the very next Read past the gate will block
// until releaseHeld is called (or the reader is canceled/disarmed). It returns
// a channel closed once that Read is actually parked inside Read, giving tests
// a precise "a read is in flight" synchronization point.
func (g *gateReader) armHeldRead(after int) <-chan struct{} {
	armed := make(chan struct{})
	held := make(chan struct{})
	g.heldMu.Lock()
	g.heldToken = held
	g.heldArmed = armed
	g.heldAfter = after
	g.heldMu.Unlock()
	return armed
}

// releaseHeld releases a held read to consume a normal token byte.
func (g *gateReader) releaseHeld() {
	g.heldMu.Lock()
	held := g.heldToken
	g.heldToken = nil
	g.heldArmed = nil
	g.heldMu.Unlock()
	if held != nil {
		close(held)
	}
}

// nFrames native 8-bit frames of frameSize bytes each, followed by a trailing
// element. It returns the encoded bytes and the byte offset of the PixelData
// tag.
func buildMultiFrameNativeDICOM(t *testing.T, nFrames, frameSize int) ([]byte, int) {
	t.Helper()
	frames := make([]*frame.Frame, 0, nFrames)
	for i := 0; i < nFrames; i++ {
		raw := make([]uint8, frameSize)
		for j := range raw {
			raw[j] = uint8((i+1)*10 + (j % 10))
		}
		frames = append(frames, &frame.Frame{
			Encapsulated: false,
			NativeData: &frame.NativeFrame[uint8]{
				InternalBitsPerSample:   8,
				InternalRows:            1,
				InternalCols:            frameSize,
				InternalSamplesPerPixel: 1,
				RawData:                 raw,
			},
		})
	}

	elems := []*Element{
		mustNewElement(tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.1.2"}),
		mustNewElement(tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.5.6.7"}),
		mustNewElement(tag.TransferSyntaxUID, []string{uid.ExplicitVRLittleEndian}),
		mustNewElement(tag.Rows, []int{1}),
		mustNewElement(tag.Columns, []int{frameSize}),
		mustNewElement(tag.BitsAllocated, []int{8}),
		mustNewElement(tag.NumberOfFrames, []string{fmt.Sprintf("%d", nFrames)}),
		mustNewElement(tag.SamplesPerPixel, []int{1}),
		mustNewElement(tag.PixelData, PixelDataInfo{IsEncapsulated: false, Frames: frames}),
		// Trailing element so the file tail is non-empty after PixelData.
		mustNewElement(tag.FloatingPointValue, []float64{3.14}),
	}

	var buf bytes.Buffer
	if err := Write(&buf, Dataset{Elements: elems}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	data := buf.Bytes()

	off := findTagOffset(data, tag.PixelData)
	if off < 0 {
		t.Fatal("could not locate PixelData tag in encoded DICOM")
	}
	return data, off
}

// findTagOffset returns the byte offset of the 4-byte explicit-VR encoding of
// t in data, or -1. It searches for the exact tag bytes (group/element are
// encoded little-endian).
func findTagOffset(data []byte, t tag.Tag) int {
	needle := []byte{
		byte(t.Group), byte(t.Group >> 8),
		byte(t.Element), byte(t.Element >> 8),
	}
	idx := bytes.Index(data, needle)
	if idx < 0 {
		return -1
	}
	return idx
}

// newStreamForTest constructs a FrameStream over a gateReader using a 16-byte
// (minimal) internal buffer so each released byte is observed immediately.
func newStreamForTest(t *testing.T, ctx context.Context, r io.Reader, size int64) *FrameStream {
	t.Helper()
	s, meta, err := NewFrameStream(ctx, r, size, 0, withReadBufferSize(256))
	if err != nil {
		t.Fatalf("NewFrameStream: %v", err)
	}
	if len(meta.Elements) == 0 {
		t.Fatal("expected file metadata to be parsed and returned up front")
	}
	return s
}

const (
	testFrames    = 4
	testFrameSize = 26
)

// assertFrameValue checks every pixel of an 8-bit native frame against the
// buildMultiFrameNativeDICOM pattern (frame idx is 1-based).
func assertFrameValue(t *testing.T, f *frame.Frame, frameIdx int) {
	t.Helper()
	nf, err := f.GetNativeFrame()
	if err != nil {
		t.Fatalf("frame %d GetNativeFrame: %v", frameIdx, err)
	}
	raw, ok := nf.RawDataSlice().([]uint8)
	if !ok {
		t.Fatalf("frame %d: expected []uint8, got %T", frameIdx, nf.RawDataSlice())
	}
	if len(raw) != testFrameSize {
		t.Fatalf("frame %d: expected %d bytes, got %d", frameIdx, testFrameSize, len(raw))
	}
	for j, v := range raw {
		want := uint8((frameIdx+1)*10 + (j % 10))
		if v != want {
			t.Fatalf("frame %d byte %d: got %d want %d", frameIdx, j, v, want)
		}
	}
}

const pixelHeaderSize = 12 // tag(4) + VR(2) + reserved(2) + VL(4) for explicit VR OB/OW

// nativeFrameOf returns the typed native frame payload for ownership checks.
func nativeRaw(f *frame.Frame) []uint8 {
	return f.NativeData.(*frame.NativeFrame[uint8]).RawData
}

// pullFrame feeds exactly the tokens needed to let the parser consume through
// frame index fi (0-based) past pixelOff, then pulls that frame from the
// stream. It feeds tokens concurrently with the pull so the parser blocks on
// the consumer (backpressure) once the frame is ready, and stops feeding as
// soon as the frame is in hand.
func pullFrame(t *testing.T, g *gateReader, s *FrameStream, pixelOff, fi, frameSize int) *frame.Frame {
	t.Helper()
	target := pixelOff + pixelHeaderSize + (fi+1)*frameSize
	// Release exactly the bytes up to and including this frame, no more. Any
	// unused tokens from a prior stage are drained first so boundaries stay
	// precise.
	g.drainTokens()
	g.enqueue(target - g.bytesConsumed())
	f, err := s.Next()
	if err != nil {
		t.Fatalf("frame %d Next: %v", fi, err)
	}
	g.waitConsumed(t, target, "frame boundary")
	return f
}

// feedTo starts feeding tokens up to targetPos and returns a stop func.
func feedTo(g *gateReader, t *testing.T, targetPos int) func() {
	g.drainTokens()
	g.enqueue(targetPos - g.bytesConsumed())
	g.waitConsumed(t, targetPos, "feed boundary")
	// Nothing to stop: tokens were released exactly to the boundary.
	return func() { g.drainTokens() }
}

// TestFrameStream_FullRead verifies incremental delivery with a synchronizing
// reader and slow consumer: pixels are not decoded up front; each delivered
// frame keeps its own bytes through later reads; completion is StreamComplete
// including the file tail.
func TestFrameStream_FullRead(t *testing.T) {
	data, pixelOff := buildMultiFrameNativeDICOM(t, testFrames, testFrameSize)

	g := newGateReader(data, pixelOff)
	s := newStreamForTest(t, context.Background(), g, int64(len(data)))

	// Nothing past the gate can be read yet, so no frame exists: the first pull
	// blocks until the consumer drives bytes.
	var got []*frame.Frame
	for i := 0; i < testFrames; i++ {
		f := pullFrame(t, g, s, pixelOff, i, testFrameSize)
		assertFrameValue(t, f, i)
		got = append(got, f)
	}

	// Retain frame 0 bytes, then let the file tail be read: frame 0 must not be
	// overwritten by subsequent reads.
	frame0Copy := append([]uint8(nil), nativeRaw(got[0])...)
	g.disarmGate()

	if _, err := s.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("expected io.EOF, got %v", err)
	}
	res := s.Wait()
	if res.Status != StreamComplete || res.Err != nil {
		t.Fatalf("expected complete, got status=%d err=%v", res.Status, res.Err)
	}
	if _, err := res.Metadata.FindElementByTag(tag.FloatingPointValue); err != nil {
		t.Fatalf("expected trailing element in completed metadata: %v", err)
	}
	if !bytes.Equal(nativeRaw(got[0]), frame0Copy) {
		t.Fatal("last delivered frame bytes were overwritten by a later read")
	}
	if g.bytesConsumed() != len(data) {
		t.Fatalf("expected all %d bytes consumed, got %d", len(data), g.bytesConsumed())
	}
}

// TestFrameStream_CancelWaitingForConsumer stops after taking the first frame
// while the parser is blocked handing the next frame to a slow consumer
// (backpressure). The parser must exit promptly, not read further, and report
// StreamCanceled distinct from completion.
func TestFrameStream_CancelWaitingForConsumer(t *testing.T) {
	data, pixelOff := buildMultiFrameNativeDICOM(t, testFrames, testFrameSize)
	g := newGateReader(data, pixelOff)
	s := newStreamForTest(t, context.Background(), g, int64(len(data)))

	f0 := pullFrame(t, g, s, pixelOff, 0, testFrameSize)

	// Feed frame 1 but never pull it: parser blocks delivering frame 1 to the
	// slow consumer.
	stopFeed := feedTo(g, t, pixelOff+pixelHeaderSize+2*testFrameSize)
	defer stopFeed()
	target := pixelOff + pixelHeaderSize + 2*testFrameSize
	deadline := time.Now().Add(5 * time.Second)
	for g.bytesConsumed() < target {
		if time.Now().After(deadline) {
			t.Fatalf("parser did not reach frame 1; pos=%d", g.bytesConsumed())
		}
	}

	consumedAtCancel := g.bytesConsumed()
	res := s.Close()
	if res.Status != StreamCanceled {
		t.Fatalf("expected StreamCanceled, got %d err=%v", res.Status, res.Err)
	}
	if !errors.Is(res.Err, ErrStreamCanceled) || !errors.Is(res.Err, context.Canceled) {
		t.Fatalf("expected ErrStreamCanceled wrapping context.Canceled, got %v", res.Err)
	}
	if g.bytesConsumed() != consumedAtCancel {
		t.Fatalf("parser read further after cancel: %d (was %d)", g.bytesConsumed(), consumedAtCancel)
	}
	assertFrameValue(t, f0, 0)
	if _, err := res.Metadata.FindElementByTag(tag.FloatingPointValue); err == nil {
		t.Fatal("partial (canceled) metadata unexpectedly contains file-tail element")
	}
}

// TestFrameStream_CancelWhileReadingNextFrame cancels while the parser is
// blocked waiting for the next frame's bytes. The caller-provided ReadCanceler
// is invoked to unblock the read; the read error remains traceable via the
// returned error chain.
func TestFrameStream_CancelWhileReadingNextFrame(t *testing.T) {
	data, pixelOff := buildMultiFrameNativeDICOM(t, testFrames, testFrameSize)
	g := newGateReader(data, pixelOff)
	ctx, cancel := context.WithCancel(context.Background())
	s := newStreamForTest(t, ctx, g, int64(len(data)))
	defer s.Close()

	// Arm a held read before frame 0 so it deterministically catches frame 1's
	// first byte-read: frame 0 consumes ordinary buffered tokens, and the next
	// gate-past Read (frame 1) blocks in Read until released/canceled.
	frame0End := pixelOff + pixelHeaderSize + 1*testFrameSize
	readParked := g.armHeldRead(frame0End)
	pullFrame(t, g, s, pixelOff, 0, testFrameSize)

	// Release the boundary precisely, then wait for frame 1's read to block.
	g.drainTokens()
	g.enqueue(1) // so releasing the held read lets it consume exactly one byte
	cancelsBefore := g.cancelCount()
	select {
	case <-readParked:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for next-frame read to block")
	}
	cancel()

	f, err := s.Next()
	if f != nil {
		t.Fatalf("expected no frame after cancel, got %v", f)
	}
	if !errors.Is(err, ErrStreamCanceled) || !errors.Is(err, context.Canceled) {
		t.Fatalf("expected canceled error, got %v", err)
	}
	if !errors.Is(err, errReadCanceled) {
		t.Fatalf("underlying read error must remain traceable, got %v", err)
	}
	if res := s.Close(); res.Status != StreamCanceled {
		t.Fatalf("expected StreamCanceled, got %d", res.Status)
	}
	if g.cancelCount() == cancelsBefore {
		t.Fatal("expected CancelPendingRead to be invoked")
	}
}

// TestFrameStream_CancelAtFileTail cancels after all frames are delivered but
// before the trailing element is read.
func TestFrameStream_CancelAtFileTail(t *testing.T) {
	data, pixelOff := buildMultiFrameNativeDICOM(t, testFrames, testFrameSize)
	g := newGateReader(data, pixelOff)
	ctx, cancel := context.WithCancel(context.Background())
	s := newStreamForTest(t, ctx, g, int64(len(data)))
	defer s.Close()

	for i := 0; i < testFrames; i++ {
		pullFrame(t, g, s, pixelOff, i, testFrameSize)
	}
	// The parser is blocked before the trailing element; cancel there.
	cancel()
	if _, err := s.Next(); !errors.Is(err, ErrStreamCanceled) {
		t.Fatalf("expected ErrStreamCanceled at file tail, got %v", err)
	}
	res := s.Close()
	if res.Status != StreamCanceled {
		t.Fatalf("expected StreamCanceled, got %d", res.Status)
	}
	if _, err := res.Metadata.FindElementByTag(tag.FloatingPointValue); err == nil {
		t.Fatal("trailing element should not be read before cancel")
	}
}

// TestFrameStream_TruncatedNativePixels cuts PixelData midway and claims the
// original length, so the parser hits io.ErrUnexpectedEOF mid-frame. The error
// must remain traceable from the terminal result and be StreamError.
func TestFrameStream_TruncatedNativePixels(t *testing.T) {
	data, pixelOff := buildMultiFrameNativeDICOM(t, testFrames, testFrameSize)
	pixelEnd := pixelOff + pixelHeaderSize + testFrames*testFrameSize
	// End the physical stream 3 bytes before the last frame completes and tell
	// the parser the full length still exists. ReadFull exhausts the reader
	// mid-last-frame and returns io.ErrUnexpectedEOF.
	truncated := data[:pixelEnd-3]
	// A hard limit equal to the physical size returns a plain io.EOF mid-frame;
	// the parser converts that to io.ErrUnexpectedEOF because it is still inside
	// a declared PixelData element.
	s, _, err := NewFrameStream(context.Background(), bytes.NewReader(truncated), int64(len(truncated)), 1)
	if err != nil {
		t.Fatal(err)
	}
	var sawFrame bool
	for {
		_, err := s.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("unexpected clean EOF on truncated DICOM")
		}
		if err != nil {
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
			}
			break
		}
		sawFrame = true
	}
	if !sawFrame {
		t.Fatal("expected at least one frame before the truncation error")
	}
	res := s.Close()
	if res.Status != StreamError || !errors.Is(res.Err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected StreamError retaining io.ErrUnexpectedEOF, got %d %v", res.Status, res.Err)
	}
}

// TestParseFrames_CallbackEarlyStop exercises the callback-style API: the
// handler stops after the first two frames, the parser exits immediately
// (StreamCanceled), and a normal full callback run reports StreamComplete.
func TestParseFrames_CallbackEarlyStop(t *testing.T) {
	data, _ := buildMultiFrameNativeDICOM(t, testFrames, testFrameSize)

	t.Run("early_stop", func(t *testing.T) {
		var seen int
		var last *frame.Frame
		res := ParseFrames(context.Background(), bytes.NewReader(data), int64(len(data)), nil,
			func(f *frame.Frame) error {
				seen++
				last = f
				if seen == 2 {
					return errConsumerDone
				}
				return nil
			})
		if seen != 2 {
			t.Fatalf("expected handler to see 2 frames, got %d", seen)
		}
		// A handler error is a recognizable early-stop (StreamError), not a
		// completion, and must remain traceable via errors.Is.
		if res.Status == StreamComplete {
			t.Fatal("handler error must not look like a complete read")
		}
		if !errors.Is(res.Err, errConsumerDone) {
			t.Fatalf("expected handler error traceable, got %v", res.Err)
		}
		assertFrameValue(t, last, 1)
	})

	t.Run("context_cancel", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		var seen int
		res := ParseFrames(ctx, bytes.NewReader(data), int64(len(data)), nil,
			func(f *frame.Frame) error {
				seen++
				cancel()
				return nil
			})
		if res.Status != StreamCanceled || !errors.Is(res.Err, ErrStreamCanceled) {
			t.Fatalf("expected StreamCanceled, got %d %v", res.Status, res.Err)
		}
		if seen < 1 {
			t.Fatal("expected at least one callback before cancel")
		}
	})

	t.Run("full_read", func(t *testing.T) {
		var seen int
		res := ParseFrames(context.Background(), bytes.NewReader(data), int64(len(data)), nil,
			func(f *frame.Frame) error {
				seen++
				return nil
			})
		if res.Status != StreamComplete || res.Err != nil {
			t.Fatalf("expected StreamComplete, got %d %v", res.Status, res.Err)
		}
		if seen != testFrames {
			t.Fatalf("expected %d frames, got %d", testFrames, seen)
		}
	})
}

var errConsumerDone = errors.New("consumer took enough frames")

// TestNewFrameStream_HeaderReadError ensures a reader that errors while the
// header/metadata is read returns the error directly and does not hang.
func TestNewFrameStream_HeaderReadError(t *testing.T) {
	s, _, err := NewFrameStream(context.Background(), errReader{}, 16, 0)
	if s != nil {
		t.Fatalf("expected nil stream, got %v", s)
	}
	if !errors.Is(err, errReaderBoom) {
		t.Fatalf("expected errReaderBoom, got %v", err)
	}
}

type errReader struct{}

var errReaderBoom = errors.New("header read boom")

func (errReader) Read(p []byte) (int, error) { return 0, errReaderBoom }

// buildEncapsulatedDICOM builds an explicit-VR little-endian DICOM with
// nFrames encapsulated frames carrying payloadLen bytes each.
func buildEncapsulatedDICOM(t *testing.T, nFrames, payloadLen int) []byte {
	t.Helper()
	frames := make([]*frame.Frame, 0, nFrames)
	for i := 0; i < nFrames; i++ {
		payload := make([]byte, payloadLen)
		for j := range payload {
			payload[j] = byte(0xA0 + i + (j % 10))
		}
		frames = append(frames, &frame.Frame{
			Encapsulated:     true,
			EncapsulatedData: frame.EncapsulatedFrame{Data: payload},
		})
	}
	elems := []*Element{
		mustNewElement(tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.1.2"}),
		mustNewElement(tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.5.6.7"}),
		mustNewElement(tag.TransferSyntaxUID, []string{uid.ExplicitVRLittleEndian}),
		mustNewElement(tag.BitsAllocated, []int{8}),
		setUndefinedLength(&Element{
			Tag:                    tag.PixelData,
			ValueRepresentation:    tag.VRPixelData,
			RawValueRepresentation: "OB",
			Value: mustNewValue(PixelDataInfo{
				IsEncapsulated: true,
				Frames:         frames,
			}),
		}),
		mustNewElement(tag.FloatingPointValue, []float64{2.71}),
	}
	var buf bytes.Buffer
	if err := Write(&buf, Dataset{Elements: elems}); err != nil {
		t.Fatalf("Write: %v", err)
	}
	return buf.Bytes()
}

// TestFrameStream_TruncatedEncapsulatedItem declares an encapsulated frame
// item longer than the bytes physically present; readRawItem must surface
// io.ErrUnexpectedEOF, retained as StreamError rather than a clean finish.
func TestFrameStream_TruncatedEncapsulatedItem(t *testing.T) {
	const nFrames, payloadLen = 3, 24
	data := buildEncapsulatedDICOM(t, nFrames, payloadLen)
	// Cut 5 bytes from the end. The last structure before the trailing element
	// is the sequence-delimitation item (8 bytes) preceded by frame payloads;
	// instead, cut deep enough to fall inside the last frame item.
	// Locate the last frame payload marker is cumbersome; cut in the middle of
	// the encapsulated region and present it with until-EOF so the declared
	// item length runs past physical EOF.
	pixelOff := findTagOffset(data, tag.PixelData)
	if pixelOff < 0 {
		t.Fatal("no PixelData")
	}
	// Item header after PixelData header(12) + empty offset-table item(8).
	// Truncate inside the final frame item by removing the trailing element and
	// delimitation plus 6 payload bytes.
	truncLen := len(data) - 8 /*trailer float elem*/ - 8 /*sequence delim*/ - 6
	truncated := data[:truncLen]

	s, _, err := NewFrameStream(context.Background(), bytes.NewReader(truncated), int64(len(truncated)), 1)
	if err != nil {
		t.Fatal(err)
	}
	var saw int
	for {
		_, err := s.Next()
		if errors.Is(err, io.EOF) {
			t.Fatal("unexpected clean EOF on truncated encapsulated item")
		}
		if err != nil {
			if !errors.Is(err, io.ErrUnexpectedEOF) {
				t.Fatalf("expected io.ErrUnexpectedEOF, got %v", err)
			}
			break
		}
		saw++
	}
	if saw == 0 {
		t.Fatal("expected at least one encapsulated frame before truncation")
	}
	res := s.Close()
	if res.Status != StreamError || !errors.Is(res.Err, io.ErrUnexpectedEOF) {
		t.Fatalf("expected StreamError retaining io.ErrUnexpectedEOF, got %d %v", res.Status, res.Err)
	}
}
