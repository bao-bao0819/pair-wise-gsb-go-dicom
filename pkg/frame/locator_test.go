package frame_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/suyashkumar/dicom/pkg/frame"
)

// itemBytes builds a DICOM Item (FFFE,E000) with the given payload.
func itemBytes(payload []byte) []byte {
	b := make([]byte, 8+len(payload))
	binary.LittleEndian.PutUint16(b[0:2], 0xfffe)
	binary.LittleEndian.PutUint16(b[2:4], 0xe000)
	binary.LittleEndian.PutUint32(b[4:8], uint32(len(payload)))
	copy(b[8:], payload)
	return b
}

// botItem builds the Basic Offset Table item from uint32 offsets.
func botItem(offsets ...uint32) []byte {
	payload := make([]byte, 4*len(offsets))
	for i, o := range offsets {
		binary.LittleEndian.PutUint32(payload[i*4:], o)
	}
	return itemBytes(payload)
}

// delimitationItem builds a Sequence Delimitation Item (FFFE,E0DD).
func delimitationItem() []byte {
	b := make([]byte, 8)
	binary.LittleEndian.PutUint16(b[0:2], 0xfffe)
	binary.LittleEndian.PutUint16(b[2:4], 0xe0dd)
	return b
}

func concat(parts ...[]byte) []byte {
	return bytes.Join(parts, nil)
}

// threeFrameStream builds an encapsulated Pixel Data stream with three frames
// where the second frame spans two fragments. It returns the stream bytes and
// the fragment payloads in order (f0, f1a, f1b, f2).
func threeFrameStream() (stream []byte, payloads [][]byte, botOffsets []uint32) {
	f0 := []byte{0x10, 0x11, 0x12, 0x13}
	f1a := []byte{0x20, 0x21}
	f1b := []byte{0x22, 0x23, 0x24, 0x25, 0x26, 0x27}
	f2 := []byte{0x30, 0x31, 0x32, 0x33}
	payloads = [][]byte{f0, f1a, f1b, f2}

	o1 := uint32(8 + len(f0))
	o2 := o1 + uint32(8+len(f1a)) + uint32(8+len(f1b))
	botOffsets = []uint32{0, o1, o2}

	stream = concat(
		botItem(botOffsets...),
		itemBytes(f0),
		itemBytes(f1a),
		itemBytes(f1b),
		itemBytes(f2),
		delimitationItem(),
	)
	return stream, payloads, botOffsets
}

func TestScanAndLocate_BasicOffsetTable(t *testing.T) {
	stream, payloads, botOffsets := threeFrameStream()

	bot, frags, end, err := frame.ScanEncapsulatedPixelData(bytes.NewReader(stream), 0)
	if err != nil {
		t.Fatalf("ScanEncapsulatedPixelData: %v", err)
	}
	if got, want := end, uint64(len(stream)); got != want {
		t.Errorf("end offset: got %d, want %d", got, want)
	}
	if got, want := len(frags), 4; got != want {
		t.Fatalf("fragment count: got %d, want %d", got, want)
	}
	// BOT item is 8 + 12 = 20 bytes, so the first fragment starts at 20.
	if got, want := frags[0].TagOffset, uint64(20); got != want {
		t.Errorf("frags[0].TagOffset: got %d, want %d", got, want)
	}
	for i, p := range payloads {
		if got, want := frags[i].Length, uint32(len(p)); got != want {
			t.Errorf("frags[%d].Length: got %d, want %d", i, got, want)
		}
	}

	layout, err := frame.LocateFrames(frags, bot, nil, nil, 3)
	if err != nil {
		t.Fatalf("LocateFrames: %v", err)
	}
	wantSpans := []frame.FrameSpan{
		{FirstFragment: 0, FragmentCount: 1, ByteLength: 4},
		{FirstFragment: 1, FragmentCount: 2, ByteLength: 8},
		{FirstFragment: 3, FragmentCount: 1, ByteLength: 4},
	}
	if diff := cmp.Diff(wantSpans, layout.Frames); diff != "" {
		t.Errorf("spans mismatch (-want +got):\n%s", diff)
	}

	// The frame spanning multiple fragments must reassemble exactly.
	got, err := layout.ReadFrame(bytes.NewReader(stream), 1)
	if err != nil {
		t.Fatalf("ReadFrame(1): %v", err)
	}
	want := concat(payloads[1], payloads[2])
	if !bytes.Equal(got, want) {
		t.Errorf("ReadFrame(1): got %v, want %v", got, want)
	}

	got, err = layout.ReadFrame(bytes.NewReader(stream), 2)
	if err != nil {
		t.Fatalf("ReadFrame(2): %v", err)
	}
	if !bytes.Equal(got, payloads[3]) {
		t.Errorf("ReadFrame(2): got %v, want %v", got, payloads[3])
	}

	if _, err := layout.ReadFrame(bytes.NewReader(stream), 3); err == nil {
		t.Error("ReadFrame(3): expected error for out-of-range frame index")
	}

	wantBot := make([]byte, 12)
	for i, o := range botOffsets {
		binary.LittleEndian.PutUint32(wantBot[i*4:], o)
	}
	if !bytes.Equal(bot, wantBot) {
		t.Errorf("basic offset table: got %v, want %v", bot, wantBot)
	}
}
func TestLocateFrames_ExtendedOffsetTable(t *testing.T) {
	stream, payloads, botOffsets := threeFrameStream()
	bot, frags, _, err := frame.ScanEncapsulatedPixelData(bytes.NewReader(stream), 0)
	if err != nil {
		t.Fatalf("ScanEncapsulatedPixelData: %v", err)
	}

	extOffsets := []uint64{uint64(botOffsets[0]), uint64(botOffsets[1]), uint64(botOffsets[2])}
	extLengths := []uint64{4, 8, 4}

	// Consistent BOT + EOT (with lengths) is accepted.
	layout, err := frame.LocateFrames(frags, bot, extOffsets, extLengths, 3)
	if err != nil {
		t.Fatalf("LocateFrames with consistent tables: %v", err)
	}
	if got, want := len(layout.Frames), 3; got != want {
		t.Fatalf("frame count: got %d, want %d", got, want)
	}

	// EOT alone (empty BOT) is accepted.
	layout, err = frame.LocateFrames(frags, nil, extOffsets, extLengths, 3)
	if err != nil {
		t.Fatalf("LocateFrames with EOT only: %v", err)
	}
	got, err := layout.ReadFrame(bytes.NewReader(stream), 1)
	if err != nil {
		t.Fatalf("ReadFrame(1): %v", err)
	}
	if want := concat(payloads[1], payloads[2]); !bytes.Equal(got, want) {
		t.Errorf("ReadFrame(1): got %v, want %v", got, want)
	}

	// Conflicting EOT offset is a diagnostic error.
	badExt := []uint64{extOffsets[0], extOffsets[1], extOffsets[2] + 8}
	if _, err := frame.LocateFrames(frags, bot, badExt, extLengths, 3); !errors.Is(err, frame.ErrOffsetTableConflict) {
		t.Errorf("conflicting tables: got %v, want ErrOffsetTableConflict", err)
	}

	// Conflicting frame count between the tables is a diagnostic error.
	if _, err := frame.LocateFrames(frags, bot, extOffsets[:2], extLengths[:2], 0); !errors.Is(err, frame.ErrOffsetTableConflict) {
		t.Errorf("conflicting frame counts: got %v, want ErrOffsetTableConflict", err)
	}

	// Wrong extended length is rejected.
	badLengths := []uint64{4, 9, 4}
	if _, err := frame.LocateFrames(frags, bot, extOffsets, badLengths, 3); !errors.Is(err, frame.ErrExtendedLengthMismatch) {
		t.Errorf("bad extended lengths: got %v, want ErrExtendedLengthMismatch", err)
	}

	// Lengths count mismatch with offsets count is rejected.
	if _, err := frame.LocateFrames(frags, nil, extOffsets, extLengths[:2], 3); !errors.Is(err, frame.ErrInvalidOffsetTable) {
		t.Errorf("lengths/offsets count mismatch: got %v, want ErrInvalidOffsetTable", err)
	}

	// Lengths without offsets is rejected.
	if _, err := frame.LocateFrames(frags, nil, nil, extLengths, 3); !errors.Is(err, frame.ErrInvalidOffsetTable) {
		t.Errorf("lengths without offsets: got %v, want ErrInvalidOffsetTable", err)
	}
}

func TestLocateFrames_BadOffsets(t *testing.T) {
	stream, _, botOffsets := threeFrameStream()
	goodBot, frags, _, err := frame.ScanEncapsulatedPixelData(bytes.NewReader(stream), 0)
	if err != nil {
		t.Fatalf("ScanEncapsulatedPixelData: %v", err)
	}
	o1, o2 := botOffsets[1], botOffsets[2]

	cases := []struct {
		name    string
		bot     []byte
		nFrames int
		wantErr error
	}{
		{
			name:    "non-monotonic offsets",
			bot:     botItem(0, o2, o1)[8:],
			nFrames: 3,
			wantErr: frame.ErrNonMonotonicOffsets,
		},
		{
			name:    "duplicate offsets",
			bot:     botItem(0, o1, o1)[8:],
			nFrames: 3,
			wantErr: frame.ErrNonMonotonicOffsets,
		},
		{
			name:    "offset not on item boundary",
			bot:     botItem(0, o1+1, o2)[8:],
			nFrames: 3,
			wantErr: frame.ErrOffsetNotOnItemBoundary,
		},
		{
			name:    "offset beyond last fragment",
			bot:     botItem(0, o1, o2+1000)[8:],
			nFrames: 3,
			wantErr: frame.ErrOffsetOutOfRange,
		},
		{
			name:    "first offset not zero",
			bot:     botItem(o1, o1+8, o2+8)[8:],
			nFrames: 3,
			wantErr: frame.ErrInvalidOffsetTable,
		},
		{
			name:    "bot length not multiple of 4",
			bot:     []byte{0, 0, 0, 0, 1, 0},
			nFrames: 0,
			wantErr: frame.ErrInvalidOffsetTable,
		},
		{
			name:    "frame count mismatch",
			bot:     goodBot,
			nFrames: 5,
			wantErr: frame.ErrFrameCountMismatch,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := frame.LocateFrames(frags, tc.bot, nil, nil, tc.nFrames); !errors.Is(err, tc.wantErr) {
				t.Errorf("got %v, want %v", err, tc.wantErr)
			}
		})
	}
}

func TestLocateFrames_EmptyTables(t *testing.T) {
	stream, payloads, _ := threeFrameStream()
	_, frags, _, err := frame.ScanEncapsulatedPixelData(bytes.NewReader(stream), 0)
	if err != nil {
		t.Fatalf("ScanEncapsulatedPixelData: %v", err)
	}

	// Empty BOT, unknown frame count: boundaries cannot be determined, and
	// each fragment must not be treated as its own frame.
	if _, err := frame.LocateFrames(frags, nil, nil, nil, 0); !errors.Is(err, frame.ErrBoundariesUnknown) {
		t.Errorf("empty tables, unknown frames: got %v, want ErrBoundariesUnknown", err)
	}
	// Empty BOT, more than one frame: boundaries cannot be determined.
	if _, err := frame.LocateFrames(frags, []byte{}, nil, nil, 3); !errors.Is(err, frame.ErrBoundariesUnknown) {
		t.Errorf("empty tables, 3 frames: got %v, want ErrBoundariesUnknown", err)
	}
	// Empty BOT, exactly one frame: all fragments belong to that frame.
	layout, err := frame.LocateFrames(frags, nil, nil, nil, 1)
	if err != nil {
		t.Fatalf("empty tables, 1 frame: %v", err)
	}
	if got, want := len(layout.Frames), 1; got != want {
		t.Fatalf("frame count: got %d, want %d", got, want)
	}
	span := layout.Frames[0]
	if span.FirstFragment != 0 || span.FragmentCount != 4 {
		t.Errorf("span: got {%d, %d}, want {0, 4}", span.FirstFragment, span.FragmentCount)
	}
	got, err := layout.ReadFrame(bytes.NewReader(stream), 0)
	if err != nil {
		t.Fatalf("ReadFrame(0): %v", err)
	}
	if want := concat(payloads...); !bytes.Equal(got, want) {
		t.Errorf("ReadFrame(0): got %v, want %v", got, want)
	}
}
func TestScan_TruncatedItem(t *testing.T) {
	// Fragment item claims 100 bytes of payload but only 4 are present.
	truncated := concat(
		botItem(0),
		[]byte{0xfe, 0xff, 0x00, 0xe0, 0x64, 0x00, 0x00, 0x00},
		[]byte{0x01, 0x02, 0x03, 0x04},
	)
	if _, _, _, err := frame.ScanEncapsulatedPixelData(bytes.NewReader(truncated), 0); !errors.Is(err, frame.ErrTruncatedItem) {
		t.Errorf("truncated item value: got %v, want ErrTruncatedItem", err)
	}

	// Item header itself is cut short.
	cutHeader := concat(botItem(0), []byte{0xfe, 0xff, 0x00})
	if _, _, _, err := frame.ScanEncapsulatedPixelData(bytes.NewReader(cutHeader), 0); !errors.Is(err, frame.ErrTruncatedItem) {
		t.Errorf("truncated item header: got %v, want ErrTruncatedItem", err)
	}

	// Truncated Basic Offset Table value.
	cutBot := []byte{0xfe, 0xff, 0x00, 0xe0, 0x08, 0x00, 0x00, 0x00, 0x00, 0x00}
	if _, _, _, err := frame.ScanEncapsulatedPixelData(bytes.NewReader(cutBot), 0); !errors.Is(err, frame.ErrTruncatedItem) {
		t.Errorf("truncated basic offset table: got %v, want ErrTruncatedItem", err)
	}
}

func TestScan_MalformedStructure(t *testing.T) {
	// Undefined-length item is not allowed in encapsulated Pixel Data.
	undef := concat(
		botItem(),
		[]byte{0xfe, 0xff, 0x00, 0xe0, 0xff, 0xff, 0xff, 0xff},
	)
	if _, _, _, err := frame.ScanEncapsulatedPixelData(bytes.NewReader(undef), 0); !errors.Is(err, frame.ErrUndefinedLengthItem) {
		t.Errorf("undefined-length item: got %v, want ErrUndefinedLengthItem", err)
	}

	// Unexpected tag where an Item is required.
	badTag := concat(
		botItem(),
		[]byte{0x10, 0x00, 0x10, 0x00, 0x00, 0x00, 0x00, 0x00},
	)
	if _, _, _, err := frame.ScanEncapsulatedPixelData(bytes.NewReader(badTag), 0); !errors.Is(err, frame.ErrExpectedItem) {
		t.Errorf("unexpected tag: got %v, want ErrExpectedItem", err)
	}
}

// sparseRegion places data at an absolute offset in a sparseReaderAt.
type sparseRegion struct {
	off  uint64
	data []byte
}

// sparseReaderAt is an io.ReaderAt backed by sparse regions; holes read as
// zero bytes. It allows tests to use logical offsets beyond uint32 without
// creating large files.
type sparseReaderAt struct {
	regions []sparseRegion
}

func (s sparseReaderAt) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset %d", off)
	}
	start := uint64(off)
	for i := range p {
		p[i] = 0 // holes read as zero bytes
		pos := start + uint64(i)
		for _, r := range s.regions {
			if pos >= r.off && pos < r.off+uint64(len(r.data)) {
				p[i] = r.data[pos-r.off]
				break
			}
		}
	}
	return len(p), nil
}

func TestLocateFrames_LargeOffsetsSparseReader(t *testing.T) {
	// Two frames; the first fragment is logically larger than uint32, so the
	// second frame starts at a logical offset only an Extended Offset Table
	// (uint64 offsets) can describe. The sparse ReaderAt models the huge
	// first fragment without allocating it.
	f1 := []byte{0x42, 0x42, 0x42, 0x42, 0x42, 0x42}

	bot := itemBytes(nil)        // empty Basic Offset Table at offset 0, 8 bytes.
	frag0Off := uint64(len(bot)) // 8
	// Largest defined uint32 VL; pushes the next fragment's logical offset
	// (8 + frag0Len) past the uint32 range.
	frag0Len := uint64(0xFFFFFFFE)
	frag0Header := make([]byte, 8)
	binary.LittleEndian.PutUint16(frag0Header[0:2], 0xfffe)
	binary.LittleEndian.PutUint16(frag0Header[2:4], 0xe000)
	binary.LittleEndian.PutUint32(frag0Header[4:8], uint32(frag0Len))
	frag1Off := frag0Off + 8 + frag0Len
	frag1 := itemBytes(f1)
	delimOff := frag1Off + uint64(len(frag1))

	// Logical offsets are relative to the first fragment's Item Tag.
	extOffsets := []uint64{0, frag1Off - frag0Off}
	if extOffsets[1] <= uint64(^uint32(0)) {
		t.Fatalf("test setup: logical offset %d does not exceed uint32", extOffsets[1])
	}
	extLengths := []uint64{frag0Len, uint64(len(f1))}

	ra := sparseReaderAt{regions: []sparseRegion{
		{off: 0, data: bot},
		{off: frag0Off, data: frag0Header},
		{off: frag1Off, data: frag1},
		{off: delimOff, data: delimitationItem()},
	}}

	gotBot, frags, end, err := frame.ScanEncapsulatedPixelData(ra, 0)
	if err != nil {
		t.Fatalf("ScanEncapsulatedPixelData: %v", err)
	}
	if len(gotBot) != 0 {
		t.Errorf("basic offset table: got %v, want empty", gotBot)
	}
	if got, want := end, delimOff+8; got != want {
		t.Errorf("end offset: got %d, want %d", got, want)
	}
	if got, want := len(frags), 2; got != want {
		t.Fatalf("fragment count: got %d, want %d", got, want)
	}
	if got := frags[1].TagOffset; got != frag1Off {
		t.Errorf("frags[1].TagOffset: got %d, want %d", got, frag1Off)
	}
	if got, want := frags[0].Length, uint32(frag0Len); got != want {
		t.Errorf("frags[0].Length: got %d, want %d", got, want)
	}

	layout, err := frame.LocateFrames(frags, gotBot, extOffsets, extLengths, 2)
	if err != nil {
		t.Fatalf("LocateFrames: %v", err)
	}
	wantSpans := []frame.FrameSpan{
		{FirstFragment: 0, FragmentCount: 1, ByteLength: frag0Len},
		{FirstFragment: 1, FragmentCount: 1, ByteLength: uint64(len(f1))},
	}
	if diff := cmp.Diff(wantSpans, layout.Frames); diff != "" {
		t.Errorf("spans mismatch (-want +got):\n%s", diff)
	}

	got, err := layout.ReadFrame(ra, 1)
	if err != nil {
		t.Fatalf("ReadFrame(1): %v", err)
	}
	if !bytes.Equal(got, f1) {
		t.Errorf("ReadFrame(1): got %v, want %v", got, f1)
	}
}
