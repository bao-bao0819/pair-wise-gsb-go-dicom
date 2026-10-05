package dicom

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// gatedReader is an io.Reader + dicomio.ContextReader whose progress is fully
// controlled by the test through synchronization points (no device connections
// and no reliance on long sleeps).
//
// Reads never deliver bytes beyond the running allowance (initialBytes plus
// everything granted via Allow). When a read needs bytes beyond the allowance
// it parks, increments parkedCount, and only resumes when Allow grants more
// bytes or the context passed to ReadContext is cancelled.
type gatedReader struct {
	mu           sync.Mutex
	cond         *sync.Cond
	data         []byte
	pos          int
	gateAt       int // position before which reads are ungated (bufio fill)
	allowance    int // total releasable bytes once pos reaches gateAt
	parkedCount  int
	ctx          context.Context // context of the read currently parked
	parked       bool
	closeWatcher chan struct{}
}

// newGatedReader gates reads at gateAt. Before that position bytes flow freely
// (this absorbs bufio's initial fill). At/after gateAt, reads may deliver up to
// initialBytes total, then park until Allow grants more.
func newGatedReader(data []byte, gateAt, initialBytes int) *gatedReader {
	g := &gatedReader{
		data:         data,
		gateAt:       gateAt,
		allowance:    initialBytes,
		closeWatcher: make(chan struct{}),
	}
	g.cond = sync.NewCond(&g.mu)

	// A single watcher observes the context of the currently parked read and
	// wakes the condition when that context is cancelled.
	go func() {
		for {
			g.mu.Lock()
			for g.ctx == nil {
				g.cond.Wait()
			}
			ctx := g.ctx
			g.mu.Unlock()

			select {
			case <-ctx.Done():
				g.mu.Lock()
				g.cond.Broadcast()
				g.mu.Unlock()
			case <-g.closeWatcher:
				return
			}
		}
	}()
	return g
}

func (g *gatedReader) stop() { close(g.closeWatcher) }

func (g *gatedReader) delivered() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.pos
}

// Allow grants n more deliverable bytes and wakes a parked read.
func (g *gatedReader) Allow(n int) {
	g.mu.Lock()
	g.allowance += n
	g.mu.Unlock()
	g.cond.Broadcast()
}

// waitParked blocks until the reader has parked at least want times.
func (g *gatedReader) waitParked(t *testing.T, want int) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		for g.parkedCount < want {
			if g.ctx != nil && g.ctx.Err() != nil {
				break
			}
			g.cond.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("producer did not reach parked count %d (have %d)", want, g.parkedCount)
	}
}

// waitDelivered blocks until at least want bytes have been consumed.
func (g *gatedReader) waitDelivered(t *testing.T, want int) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		g.mu.Lock()
		defer g.mu.Unlock()
		for g.pos < want {
			g.cond.Wait()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("producer consumed %d bytes, wanted >= %d", g.delivered(), want)
	}
}

func (g *gatedReader) Read(p []byte) (int, error) {
	return g.ReadContext(context.Background(), p)
}

func (g *gatedReader) ReadContext(ctx context.Context, p []byte) (int, error) {
	g.mu.Lock()
	if g.pos >= len(g.data) {
		g.mu.Unlock()
		return 0, io.EOF
	}
	if g.pos >= g.gateAt && g.pos >= g.allowance {
		g.parkedCount++
		g.parked = true
		g.ctx = ctx
		g.cond.Broadcast() // let waitParked observe the parked state

		for g.pos >= g.gateAt && g.pos >= g.allowance && g.pos < len(g.data) && ctx.Err() == nil {
			g.cond.Wait()
		}
		g.ctx = nil
		g.parked = false
		if ctx.Err() != nil {
			g.mu.Unlock()
			return 0, ctx.Err()
		}
	}
	n := len(p)
	if g.pos >= g.gateAt {
		if avail := g.allowance - g.pos; n > avail {
			n = avail
		}
	}
	if rem := len(g.data) - g.pos; n > rem {
		n = rem
	}
	copy(p, g.data[g.pos:g.pos+n])
	g.pos += n
	g.cond.Broadcast()
	g.mu.Unlock()
	return n, nil
}

// appendExplicitElement appends one explicit-VR little-endian element.
func appendExplicitElement(buf []byte, t tag.Tag, vr string, value []byte) []byte {
	buf = binary.LittleEndian.AppendUint16(buf, t.Group)
	buf = binary.LittleEndian.AppendUint16(buf, t.Element)
	buf = append(buf, vr[0], vr[1])
	switch vr {
	case "NA", "OB", "OD", "OF", "OL", "OW", "SQ", "UN", "UC", "UR", "UT":
		buf = append(buf, 0, 0)
		buf = binary.LittleEndian.AppendUint32(buf, uint32(len(value)))
	default:
		buf = binary.LittleEndian.AppendUint16(buf, uint16(len(value)))
	}
	buf = append(buf, value...)
	if len(value)%2 == 1 {
		buf = append(buf, 0)
	}
	return buf
}

func usValue(vals ...int) []byte {
	var b []byte
	for _, v := range vals {
		b = binary.LittleEndian.AppendUint16(b, uint16(v))
	}
	return b
}

type nativeDICOMLayout struct {
	Bytes           []byte
	FrameLen        int
	PixelDataOffset int
}

func fileMetaHeader(meta []byte) []byte {
	var h []byte
	h = binary.LittleEndian.AppendUint16(h, tag.FileMetaInformationGroupLength.Group)
	h = binary.LittleEndian.AppendUint16(h, tag.FileMetaInformationGroupLength.Element)
	h = append(h, 'U', 'L')
	h = binary.LittleEndian.AppendUint16(h, 4)
	h = binary.LittleEndian.AppendUint32(h, uint32(len(meta)))
	h = append(h, meta...)
	return h
}

func transferSyntaxMeta() []byte {
	return appendExplicitElement(nil, tag.TransferSyntaxUID, "UI",
		append([]byte(uid.ExplicitVRLittleEndian), 0))
}

// buildNativeDICOM builds a minimal explicit-VR little-endian DICOM with nFrames
// uint8 frames of rows x cols. Frame f's pixels are the repeated byte 0xA0+f,
// so buffer reuse between frames is observable.
func buildNativeDICOM(t *testing.T, rows, cols, nFrames int) nativeDICOMLayout {
	t.Helper()
	meta := transferSyntaxMeta()
	header := fileMetaHeader(meta)

	var body []byte
	body = appendExplicitElement(body, tag.Rows, "US", usValue(rows))
	body = appendExplicitElement(body, tag.Columns, "US", usValue(cols))
	body = appendExplicitElement(body, tag.BitsAllocated, "US", usValue(8))
	body = appendExplicitElement(body, tag.BitsStored, "US", usValue(8))
	body = appendExplicitElement(body, tag.HighBit, "US", usValue(0))
	body = appendExplicitElement(body, tag.SamplesPerPixel, "US", usValue(1))
	body = appendExplicitElement(body, tag.PixelRepresentation, "US", usValue(0))
	if nFrames > 1 {
		is := []byte(itoa(nFrames))
		if len(is)%2 == 1 {
			is = append(is, 0)
		}
		body = appendExplicitElement(body, tag.NumberOfFrames, "IS", is)
	}

	const pixelDataHeaderLen = 12 // tag(4)+VR(2)+reserved(2)+VL(4)
	pixelDataOffset := 128 + 4 + len(header) + len(body) + pixelDataHeaderLen

	frameLen := rows * cols
	var pixels []byte
	for f := 0; f < nFrames; f++ {
		pixels = append(pixels, bytes.Repeat([]byte{byte(0xA0 + f)}, frameLen)...)
	}
	body = appendExplicitElement(body, tag.PixelData, "OW", pixels)

	out := make([]byte, 0, 128+4+len(header)+len(body))
	out = append(out, make([]byte, 128)...)
	out = append(out, "DICM"...)
	out = append(out, header...)
	out = append(out, body...)
	return nativeDICOMLayout{Bytes: out, FrameLen: frameLen, PixelDataOffset: pixelDataOffset}
}

// buildEncapsulatedDICOM wraps already-encoded encapsulated PixelData items in
// a minimal explicit-VR DICOM using an undefined PixelData length.
func buildEncapsulatedDICOM(t *testing.T, pixelDataItems []byte) []byte {
	t.Helper()
	header := fileMetaHeader(transferSyntaxMeta())

	var pd []byte
	pd = binary.LittleEndian.AppendUint16(pd, tag.PixelData.Group)
	pd = binary.LittleEndian.AppendUint16(pd, tag.PixelData.Element)
	pd = append(pd, 'O', 'W', 0, 0)
	pd = binary.LittleEndian.AppendUint32(pd, tag.VLUndefinedLength)
	pd = append(pd, pixelDataItems...)

	out := make([]byte, 0, 128+4+len(header)+len(pd))
	out = append(out, make([]byte, 128)...)
	out = append(out, "DICM"...)
	out = append(out, header...)
	out = append(out, pd...)
	return out
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func nativeFrameBytes(f *frame.Frame) []byte {
	nf, err := f.GetNativeFrame()
	if err != nil {
		return nil
	}
	raw, ok := nf.RawDataSlice().([]uint8)
	if !ok {
		return nil
	}
	return []byte(raw)
}

// TestParseFrames_FullDrain verifies genuine streaming (frames are produced on
// demand, not after decoding all pixels), bounded buffering/back-pressure, and
// that a fully drained stream reports a complete, error-free summary.
func TestParseFrames_FullDrain(t *testing.T) {
	const rows, cols, nFrames = 100, 100, 3
	layout := buildNativeDICOM(t, rows, cols, nFrames)

	gr := newGatedReader(layout.Bytes, layout.PixelDataOffset, layout.PixelDataOffset)
	defer gr.stop()

	ctx := context.Background()
	fs, metadata, err := ParseFrames(ctx, gr, int64(len(layout.Bytes)),
		ParseOptions{MaxBufferedFrames: 0})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}

	tsElem, err := metadata.FindElementByTag(tag.TransferSyntaxUID)
	if err != nil {
		t.Fatalf("metadata missing TransferSyntaxUID: %v", err)
	}
	if MustGetStrings(tsElem.Value)[0] != uid.ExplicitVRLittleEndian {
		t.Fatalf("unexpected transfer syntax %q", MustGetStrings(tsElem.Value)[0])
	}

	var received [][]byte
	for i := 0; i < nFrames; i++ {
		if i == 0 {
			// Before the first frame the producer must park waiting for its
			// bytes: pixels are not all decoded up front.
			gr.waitParked(t, 1)
		} else {
			// With an unbuffered frame channel the producer can only read
			// frame i after we consumed frame i-1.
			gr.waitDelivered(t, layout.PixelDataOffset+i*layout.FrameLen)
		}
		gr.Allow(layout.FrameLen)

		f, err := fs.Next(ctx)
		if err != nil {
			t.Fatalf("Next frame %d: %v", i, err)
		}
		b := nativeFrameBytes(f)
		if len(b) != layout.FrameLen {
			t.Fatalf("frame %d len = %d, want %d", i, len(b), layout.FrameLen)
		}
		want := byte(0xA0 + i)
		for _, v := range b {
			if v != want {
				t.Fatalf("frame %d byte = %#x, want %#x", i, v, want)
			}
		}
		received = append(received, append([]byte(nil), b...))
	}

	if terminal, err := fs.Next(ctx); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Next = %v, want io.EOF", err)
	} else if terminal != nil {
		t.Fatal("terminal Next returned a non-nil frame")
	}
	summary := fs.Result()
	if !summary.Complete || summary.Err != nil || summary.FramesDelivered != nFrames {
		t.Fatalf("summary = %+v, want complete with %d frames", summary, nFrames)
	}

	// Delivered frame buffers must not be overwritten by later reads.
	for i, b := range received {
		want := byte(0xA0 + i)
		for _, v := range b {
			if v != want {
				t.Fatalf("delivered frame %d overwritten: got %#x want %#x", i, v, want)
			}
		}
	}
}

// TestParseFrames_BufferNotReused pulls all frames from an ordinary reader and
// checks the first frame's bytes survive the subsequent reads.
func TestParseFrames_BufferNotReused(t *testing.T) {
	const rows, cols, nFrames = 20, 20, 4
	layout := buildNativeDICOM(t, rows, cols, nFrames)

	fs, _, err := ParseFrames(context.Background(), bytes.NewReader(layout.Bytes),
		int64(len(layout.Bytes)), ParseOptions{MaxBufferedFrames: 0})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}

	first, err := fs.Next(context.Background())
	if err != nil {
		t.Fatalf("first Next: %v", err)
	}
	firstBytes := nativeFrameBytes(first)

	for i := 1; i < nFrames; i++ {
		if _, err := fs.Next(context.Background()); err != nil {
			t.Fatalf("Next %d: %v", i, err)
		}
	}
	if _, err := fs.Next(context.Background()); !errors.Is(err, io.EOF) {
		t.Fatalf("terminal Next: %v", err)
	}
	for _, v := range firstBytes {
		if v != 0xA0 {
			t.Fatalf("first frame buffer mutated by later reads: %#x", v)
		}
	}
	if s := fs.Result(); !s.Complete {
		t.Fatalf("summary = %+v, want complete", s)
	}
}

// TestParseFrames_CancelWhileWaitingForConsumer parks the producer delivering
// frame 1 into the unbuffered channel (slow consumer) and cancels. The parser
// must stop promptly with a recognizable result and frame 1 is not delivered.
func TestParseFrames_CancelWhileWaitingForConsumer(t *testing.T) {
	const rows, cols, nFrames = 100, 100, 3
	layout := buildNativeDICOM(t, rows, cols, nFrames)

	gr := newGatedReader(layout.Bytes, layout.PixelDataOffset, layout.PixelDataOffset)
	defer gr.stop()

	ctx, cancel := context.WithCancel(context.Background())
	fs, _, err := ParseFramesUntilEOF(ctx, gr,
		ParseOptions{MaxBufferedFrames: 0})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}

	// Consume frame 0, then release frame 1. The producer decodes frame 1 and
	// blocks delivering it to the absent (slow) consumer.
	gr.waitParked(t, 1)
	gr.Allow(layout.FrameLen)
	f0, err := fs.Next(context.Background())
	if err != nil {
		t.Fatalf("frame 0: %v", err)
	}
	if len(nativeFrameBytes(f0)) != layout.FrameLen {
		t.Fatal("frame 0 has wrong length")
	}
	gr.Allow(layout.FrameLen)
	gr.waitDelivered(t, layout.PixelDataOffset+2*layout.FrameLen)

	cancel()

	done := make(chan error, 1)
	go func() {
		_, e := fs.Next(context.Background())
		done <- e
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStreamStopped) || !errors.Is(err, context.Canceled) {
			t.Fatalf("Next after cancel = %v, want stop+canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Next did not return after cancelling a producer waiting for the consumer")
	}

	summary := fs.Result()
	if summary.Complete || summary.FramesDelivered != 1 {
		t.Fatalf("summary = %+v, want incomplete with 1 delivered frame", summary)
	}
}

// buildSequenceDICOM builds a DICOM with a long fixed-length sequence before
// PixelData, exercising cancellation checkpoints while iterating a sequence.
func buildSequenceDICOM(t *testing.T, items int) []byte {
	t.Helper()
	header := fileMetaHeader(transferSyntaxMeta())

	itemPayload := appendExplicitElement(nil, tag.CodeValue, "SH", []byte("ABCD"))
	var item []byte
	item = binary.LittleEndian.AppendUint16(item, 0xfffe)
	item = binary.LittleEndian.AppendUint16(item, 0xe000)
	item = binary.LittleEndian.AppendUint32(item, uint32(len(itemPayload)))
	item = append(item, itemPayload...)

	var sq []byte
	for i := 0; i < items; i++ {
		sq = append(sq, item...)
	}

	var body []byte
	body = appendExplicitElement(body, tag.ReferencedSeriesSequence, "SQ", sq)
	body = appendExplicitElement(body, tag.Rows, "US", usValue(1))
	body = appendExplicitElement(body, tag.Columns, "US", usValue(1))
	body = appendExplicitElement(body, tag.BitsAllocated, "US", usValue(8))
	body = appendExplicitElement(body, tag.SamplesPerPixel, "US", usValue(1))
	body = appendExplicitElement(body, tag.PixelData, "OW", []byte{0x01})

	out := make([]byte, 0, 128+4+len(header)+len(body))
	out = append(out, make([]byte, 128)...)
	out = append(out, "DICM"...)
	out = append(out, header...)
	out = append(out, body...)
	return out
}

func TestParseFrames_CancelInsideSequence(t *testing.T) {
	data := buildSequenceDICOM(t, 50000)
	ctx, cancel := context.WithCancel(context.Background())

	fs, metadata, err := ParseFrames(ctx, bytes.NewReader(data), int64(len(data)),
		ParseOptions{MaxBufferedFrames: 0})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}
	if _, err := metadata.FindElementByTag(tag.TransferSyntaxUID); err != nil {
		t.Fatalf("metadata: %v", err)
	}

	cancel()

	done := make(chan error, 1)
	go func() {
		_, e := fs.Next(context.Background())
		done <- e
	}()
	select {
	case err := <-done:
		if !errors.Is(err, ErrStreamStopped) || !errors.Is(err, context.Canceled) {
			t.Fatalf("Next = %v, want stop+canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("cancellation did not take effect while iterating a sequence")
	}
	if s := fs.Result(); s.Complete {
		t.Fatal("summary.Complete = true after cancellation inside sequence")
	}
}

func TestParseFrames_CancelAtFileTail(t *testing.T) {
	const rows, cols, nFrames = 100, 100, 1
	layout := buildNativeDICOM(t, rows, cols, nFrames)

	// Append a sizable element after PixelData so the "file tail" is larger than
	// bufio's prefill during the final frame read, forcing a distinct blocking
	// read for the tail.
	tail := appendExplicitElement(nil, tag.PatientID, "LO", bytes.Repeat([]byte{'Z'}, 8192))
	data := append(append([]byte(nil), layout.Bytes...), tail...)
	frameEnd := layout.PixelDataOffset + layout.FrameLen
	gr := newGatedReader(data, frameEnd, frameEnd)
	defer gr.stop()
	ctx, cancel := context.WithCancel(context.Background())

	fs, _, err := ParseFramesUntilEOF(ctx, gr,
		ParseOptions{MaxBufferedFrames: 0})
	if err != nil {
		t.Fatalf("ParseFramesUntilEOF: %v", err)
	}

	if _, err := fs.Next(context.Background()); err != nil {
		t.Fatalf("frame 0: %v", err)
	}
	gr.waitParked(t, 1)
	cancel()
	if _, err := fs.Next(context.Background()); !errors.Is(err, context.Canceled) ||
		!errors.Is(err, ErrStreamStopped) {
		t.Fatalf("terminal Next = %v, want stop+canceled", err)
	}
	if s := fs.Result(); s.Complete {
		t.Fatal("summary.Complete = true after cancelling at file tail")
	}
}

func TestParseFrames_TruncatedNativePixels(t *testing.T) {
	const rows, cols, nFrames = 40, 40, 2
	layout := buildNativeDICOM(t, rows, cols, nFrames)

	// Declared length covers 2 frames; cut the stream mid-frame-1.
	truncated := layout.Bytes[:layout.PixelDataOffset+layout.FrameLen+3]

	fs, _, err := ParseFrames(context.Background(), bytes.NewReader(truncated),
		int64(len(truncated)), ParseOptions{MaxBufferedFrames: 0})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}

	f0, err := fs.Next(context.Background())
	if err != nil {
		t.Fatalf("frame 0: %v", err)
	}
	if len(nativeFrameBytes(f0)) != layout.FrameLen {
		t.Fatalf("frame 0 len = %d", len(nativeFrameBytes(f0)))
	}

	_, terr := fs.Next(context.Background())
	if !errors.Is(terr, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated frame error = %v, want io.ErrUnexpectedEOF", terr)
	}
	if s := fs.Result(); s.Complete || s.FramesDelivered != 1 {
		t.Fatalf("summary = %+v, want incomplete with 1 delivered frame", s)
	}
}

func TestParseFrames_TruncatedEncapsulated(t *testing.T) {
	var b []byte
	// Empty basic offset table item.
	b = binary.LittleEndian.AppendUint16(b, 0xfffe)
	b = binary.LittleEndian.AppendUint16(b, 0xe000)
	b = binary.LittleEndian.AppendUint32(b, 0)
	// Data item declares 100 bytes but only 4 follow; no sequence delimiter.
	b = binary.LittleEndian.AppendUint16(b, 0xfffe)
	b = binary.LittleEndian.AppendUint16(b, 0xe000)
	b = binary.LittleEndian.AppendUint32(b, 100)
	b = append(b, 1, 2, 3, 4)

	data := buildEncapsulatedDICOM(t, b)
	fs, _, err := ParseFrames(context.Background(), bytes.NewReader(data), int64(len(data)),
		ParseOptions{MaxBufferedFrames: 0})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}
	_, terr := fs.Next(context.Background())
	if terr == nil {
		t.Fatal("expected error for truncated encapsulated pixels, got nil")
	}
	if !errors.Is(terr, io.ErrUnexpectedEOF) {
		t.Fatalf("error = %v, want io.ErrUnexpectedEOF (traceable truncation)", terr)
	}
	if errors.Is(terr, io.EOF) {
		t.Fatal("truncation must be io.ErrUnexpectedEOF, not bare io.EOF (clean completion)")
	}
	if s := fs.Result(); s.Complete {
		t.Fatal("encapsulated truncation must not be reported complete")
	}
}

func TestParseFramesWithCallback_StopEarly(t *testing.T) {
	const rows, cols, nFrames = 20, 20, 3
	layout := buildNativeDICOM(t, rows, cols, nFrames)

	ctx, cancel := context.WithCancel(context.Background())
	stopErr := errors.New("consumer seen enough")

	_, summary, err := ParseFramesWithCallback(ctx, bytes.NewReader(layout.Bytes),
		int64(len(layout.Bytes)), ParseOptions{MaxBufferedFrames: 0},
		func(f *frame.Frame) error {
			cancel()
			return stopErr
		})
	if err != nil {
		t.Fatalf("setup error: %v", err)
	}
	if summary.Complete || summary.FramesDelivered != 1 {
		t.Fatalf("summary = %+v, want incomplete with 1 delivered frame", summary)
	}
	if !errors.Is(summary.Err, stopErr) || !errors.Is(summary.Err, ErrStreamStopped) {
		t.Fatalf("summary.Err = %v, want wrapping consumer error and ErrStreamStopped", summary.Err)
	}
}

func TestParseFramesWithCallback_Complete(t *testing.T) {
	const rows, cols, nFrames = 20, 20, 3
	layout := buildNativeDICOM(t, rows, cols, nFrames)

	var got int
	metadata, summary, err := ParseFramesWithCallback(context.Background(), bytes.NewReader(layout.Bytes),
		int64(len(layout.Bytes)), ParseOptions{MaxBufferedFrames: 4},
		func(f *frame.Frame) error {
			got++
			return nil
		})
	if err != nil {
		t.Fatalf("ParseFramesWithCallback: %v", err)
	}
	if got != nFrames || !summary.Complete || summary.Err != nil {
		t.Fatalf("got=%d summary=%+v", got, summary)
	}
	if _, err := metadata.FindElementByTag(tag.TransferSyntaxUID); err != nil {
		t.Fatalf("metadata: %v", err)
	}
}

// TestParseFrames_DoesNotCloseReader verifies the caller's Reader is still
// usable after the stream is closed early; the library only closes resources it
// created.
func TestParseFrames_DoesNotCloseReader(t *testing.T) {
	const rows, cols, nFrames = 20, 20, 3
	layout := buildNativeDICOM(t, rows, cols, nFrames)

	r := bytes.NewReader(layout.Bytes)
	ctx, cancel := context.WithCancel(context.Background())
	fs, _, err := ParseFrames(ctx, r, int64(len(layout.Bytes)),
		ParseOptions{MaxBufferedFrames: 0})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}
	cancel()
	if err := fs.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The caller-owned Reader is untouched: seeking/reading still works.
	if _, err := r.Seek(0, io.SeekStart); err != nil {
		t.Fatalf("caller Reader was closed or unusable after stream Close: %v", err)
	}
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		t.Fatalf("caller Reader unreadable after stream Close: %v", err)
	}
}

// TestParseFrames_RealFile_MatchesEagerParse streams the multi-frame test DICOM
// through the new API and confirms delivered frames match an eager Parse, with
// a clean completion summary.
func TestParseFrames_RealFile_MatchesEagerParse(t *testing.T) {
	const path = "testdata/5.dcm"

	// Eager reference.
	eagerFrames := make(chan *frame.Frame, 64)
	eagerDS, err := ParseFile(path, eagerFrames)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	var eagerPixel PixelDataInfo
	for _, e := range eagerDS.Elements {
		if e.Tag == tag.PixelData {
			eagerPixel = MustGetPixelDataInfo(e.Value)
		}
	}

	// Streaming pull API.
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	info, _ := f.Stat()

	fs, _, err := ParseFrames(context.Background(), f, info.Size(), ParseOptions{MaxBufferedFrames: 2})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}
	var streamed []*frame.Frame
	for {
		fr, nerr := fs.Next(context.Background())
		if errors.Is(nerr, io.EOF) {
			break
		}
		if nerr != nil {
			t.Fatalf("Next: %v", nerr)
		}
		streamed = append(streamed, fr)
	}
	summary := fs.Result()
	if !summary.Complete || summary.Err != nil {
		t.Fatalf("summary = %+v, want complete", summary)
	}
	if len(streamed) != len(eagerPixel.Frames) {
		t.Fatalf("streamed %d frames, eager %d", len(streamed), len(eagerPixel.Frames))
	}
	for i := range streamed {
		if !streamed[i].Equals(eagerPixel.Frames[i]) {
			t.Fatalf("frame %d differs between streaming and eager parse", i)
		}
	}
}

// TestParseFrames_EncapsulatedComplete verifies a well-formed encapsulated
// PixelData (terminated by a Sequence Delimitation Item) streams all frames and
// reports completion.
func TestParseFrames_EncapsulatedComplete(t *testing.T) {
	frames := [][]byte{{1, 2, 3, 4}, {5, 6, 7, 8, 9}, {10}}
	var items []byte
	// Empty basic offset table.
	items = binary.LittleEndian.AppendUint16(items, 0xfffe)
	items = binary.LittleEndian.AppendUint16(items, 0xe000)
	items = binary.LittleEndian.AppendUint32(items, 0)
	for _, fr := range frames {
		items = binary.LittleEndian.AppendUint16(items, 0xfffe)
		items = binary.LittleEndian.AppendUint16(items, 0xe000)
		items = binary.LittleEndian.AppendUint32(items, uint32(len(fr)))
		items = append(items, fr...)
	}
	// Sequence Delimitation Item (fffe,e0dd), length 0.
	items = binary.LittleEndian.AppendUint16(items, 0xfffe)
	items = binary.LittleEndian.AppendUint16(items, 0xe0dd)
	items = binary.LittleEndian.AppendUint32(items, 0)

	data := buildEncapsulatedDICOM(t, items)
	fs, _, err := ParseFrames(context.Background(), bytes.NewReader(data), int64(len(data)),
		ParseOptions{MaxBufferedFrames: 1})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}

	var got [][]byte
	for {
		fr, err := fs.Next(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		enc, err := fr.GetEncapsulatedFrame()
		if err != nil {
			t.Fatalf("GetEncapsulatedFrame: %v", err)
		}
		got = append(got, append([]byte(nil), enc.Data...))
	}
	if len(got) != len(frames) {
		t.Fatalf("got %d frames, want %d", len(got), len(frames))
	}
	for i := range frames {
		if !bytes.Equal(got[i], frames[i]) {
			t.Fatalf("frame %d = %v, want %v", i, got[i], frames[i])
		}
	}
	if s := fs.Result(); !s.Complete || s.FramesDelivered != len(frames) {
		t.Fatalf("summary = %+v, want complete with %d frames", s, len(frames))
	}
}

// TestParseFrames_CancelWhileReadingNextFrame parks the producer inside a
// blocking ReadContext for frame 1. Cancelling interrupts the caller-provided
// cancellable read and surfaces a recognizable stop result.
func TestParseFrames_CancelWhileReadingNextFrame(t *testing.T) {
	const rows, cols, nFrames = 100, 100, 3
	layout := buildNativeDICOM(t, rows, cols, nFrames)

	gr := newGatedReader(layout.Bytes, layout.PixelDataOffset, layout.PixelDataOffset)
	defer gr.stop()

	ctx, cancel := context.WithCancel(context.Background())
	fs, _, err := ParseFrames(ctx, gr, int64(len(layout.Bytes)),
		ParseOptions{MaxBufferedFrames: 8})
	if err != nil {
		t.Fatalf("ParseFrames: %v", err)
	}

	gr.waitParked(t, 1)
	gr.Allow(layout.FrameLen)
	f0, err := fs.Next(context.Background())
	if err != nil {
		t.Fatalf("frame 0: %v", err)
	}
	if len(nativeFrameBytes(f0)) != layout.FrameLen {
		t.Fatal("frame 0 has wrong length")
	}

	// Producer now blocks inside ReadContext waiting for frame 1 bytes.
	gr.waitParked(t, 2)
	cancel()

	if _, err := fs.Next(context.Background()); true {
		if !errors.Is(err, ErrStreamStopped) || !errors.Is(err, context.Canceled) {
			t.Fatalf("terminal Next = %v, want stop+canceled", err)
		}
	}
	summary := fs.Result()
	if summary.Complete || summary.FramesDelivered != 1 {
		t.Fatalf("summary = %+v, want incomplete with 1 delivered frame", summary)
	}
}
