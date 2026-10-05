package pixeldata

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"testing"
)

var threeFramePayloads = [][]byte{
	{0x10},
	{0x20, 0x21},
	{0x22},
	{0x30, 0x31, 0x32},
}

func TestLocateEncapsulated_BasicOffsetTable_MultiFragmentFrame(t *testing.T) {
	data, pixelStart := buildBasicPixelData(0, 8, threeFramePayloads)

	location, err := LocateEncapsulated(bytes.NewReader(data), pixelStart, 8, WithSize(uint64(len(data))))
	if err != nil {
		t.Fatalf("LocateEncapsulated() error = %v", err)
	}
	if location.Table != TableBasic {
		t.Fatalf("Table = %v, want TableBasic", location.Table)
	}
	if len(location.Fragments) != 4 {
		t.Fatalf("len(Fragments) = %d, want 4", len(location.Fragments))
	}
	if len(location.Frames) != 3 {
		t.Fatalf("len(Frames) = %d, want 3", len(location.Frames))
	}

	wantIntervals := [][2]int{{0, 1}, {1, 3}, {3, 4}}
	wantLengths := []uint64{1, 3, 3}
	wantEncoded := []uint64{9, 19, 11}
	for i, frame := range location.Frames {
		if got := [2]int{frame.FirstFragment, frame.LastFragment}; got != wantIntervals[i] {
			t.Errorf("frame %d fragments = %v, want %v", i, got, wantIntervals[i])
		}
		if frame.ValidLength != wantLengths[i] {
			t.Errorf("frame %d ValidLength = %d, want %d", i, frame.ValidLength, wantLengths[i])
		}
		if frame.EncodedItemLength != wantEncoded[i] {
			t.Errorf("frame %d EncodedItemLength = %d, want %d", i, frame.EncodedItemLength, wantEncoded[i])
		}
	}

	frameReader, err := location.Reader(bytes.NewReader(data), 1)
	if err != nil {
		t.Fatalf("Reader(1) error = %v", err)
	}
	frameReader.reader = &fragmentOnlyReader{data: data, allowed: location.Frames[1].Fragments}
	got := make([]byte, 3)
	n, err := frameReader.ReadAt(got, 0)
	if err != nil || n != 3 {
		t.Fatalf("ReadAt full frame = %d, %v; want 3, nil", n, err)
	}
	if !bytes.Equal(got, []byte{0x20, 0x21, 0x22}) {
		t.Fatalf("frame bytes = %v, want bytes across two fragments", got)
	}

	crossFragment := make([]byte, 2)
	if _, err := frameReader.ReadAt(crossFragment, 1); err != nil {
		t.Fatalf("ReadAt across fragment boundary: %v", err)
	}
	if !bytes.Equal(crossFragment, []byte{0x21, 0x22}) {
		t.Fatalf("cross-fragment bytes = %v", crossFragment)
	}
}

func TestLocateEncapsulated_EmptyTable_BoundariesUnknown(t *testing.T) {
	data, _ := buildBasicPixelData(0, 8, threeFramePayloads, withEmptyBasicTable())
	_, err := LocateEncapsulated(bytes.NewReader(data), 0, 8, WithSize(uint64(len(data))))
	if !errors.Is(err, ErrFrameBoundariesUnknown) {
		t.Fatalf("error = %v, want ErrFrameBoundariesUnknown", err)
	}
}

func TestLocateEncapsulated_InvalidBasicOffsets(t *testing.T) {
	cases := []struct {
		name    string
		offsets []uint32
	}{
		{name: "not an item boundary", offsets: []uint32{0, 8, 28}},
		{name: "non-monotonic", offsets: []uint32{0, 28, 28}},
		{name: "first offset not zero", offsets: []uint32{9, 19}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			data, _ := buildBasicPixelData(0, 8, threeFramePayloads, withBasicOffsets(tc.offsets))
			_, err := LocateEncapsulated(bytes.NewReader(data), 0, 8, WithSize(uint64(len(data))))
			if !errors.Is(err, ErrInvalidOffsetTable) {
				t.Fatalf("error = %v, want ErrInvalidOffsetTable", err)
			}
		})
	}
}

func TestLocateEncapsulated_TruncatedItem(t *testing.T) {
	data, _ := buildBasicPixelData(0, 8, threeFramePayloads)
	truncated := len(data) - 2
	_, err := LocateEncapsulated(bytes.NewReader(data[:truncated]), 0, 8, WithSize(uint64(truncated)))
	if !errors.Is(err, ErrInvalidItem) {
		t.Fatalf("error = %v, want ErrInvalidItem", err)
	}
}

func TestLocateEncapsulated_ExtendedOffsetsConflict(t *testing.T) {
	data, pixelStart := buildBasicPixelData(0, 8, threeFramePayloads)
	firstFragment := pixelStart + uint64(8+8+len([]uint32{0, 9, 19})*4)
	offsets := []uint64{uint64(firstFragment), firstFragment + 9, firstFragment + 28}

	_, err := LocateEncapsulated(bytes.NewReader(data), pixelStart, 8,
		WithSize(uint64(len(data))),
		WithExtendedOffsetTables(offsets, []uint64{9, 20, 11}),
	)
	if !errors.Is(err, ErrConflictingOffsetTables) {
		t.Fatalf("error = %v, want ErrConflictingOffsetTables", err)
	}
}

func TestLocateEncapsulated_ExtendedSparseOffsetsOverUint32(t *testing.T) {
	pixelStart := uint64(5_000_000_000)
	reader := newSparseExtendedPixelData(pixelStart, 12, threeFramePayloads)
	offsets := []uint64{12, 21, 40}
	lengths := []uint64{9, 19, 11}

	location, err := LocateEncapsulated(reader, pixelStart, 12,
		WithSize(reader.size),
		WithExtendedOffsetTables(offsets, lengths),
		WithoutBasicOffsetTable(),
	)
	if err != nil {
		t.Fatalf("LocateEncapsulated() error = %v", err)
	}
	if location.Table != TableExtended {
		t.Fatalf("Table = %v, want TableExtended", location.Table)
	}
	if location.Frames[0].DataStart <= math.MaxUint32 {
		t.Fatalf("first data offset = %d, want a sparse logical offset above uint32", location.Frames[0].DataStart)
	}
	if location.Frames[1].ValidLength != 3 {
		t.Fatalf("multi-fragment frame ValidLength = %d, want 3", location.Frames[1].ValidLength)
	}

	frameReader, err := location.Reader(reader, 1)
	if err != nil {
		t.Fatalf("Reader(1): %v", err)
	}
	buf := make([]byte, 3)
	if _, err := frameReader.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt from sparse ReaderAt: %v", err)
	}
	if !bytes.Equal(buf, []byte{0x20, 0x21, 0x22}) {
		t.Fatalf("sparse frame bytes = %v", buf)
	}
}

func TestLocateEncapsulated_ExtendedDataset(t *testing.T) {
	data := buildExtendedDataset(threeFramePayloads)
	reader := bytes.NewReader(data)

	location, err := LocateEncapsulated(reader, 72, 12,
		WithSize(uint64(len(data))),
		WithExtendedOffsetTables([]uint64{12, 21, 40}, []uint64{9, 19, 11}),
		WithoutBasicOffsetTable(),
	)
	if err != nil {
		t.Fatalf("LocateFrames() error = %v", err)
	}
	if location.PixelDataStart != 72 {
		t.Fatalf("PixelDataStart = %d, want 72", location.PixelDataStart)
	}
	if len(location.Frames) != 3 || location.Frames[1].LastFragment-location.Frames[1].FirstFragment != 2 {
		t.Fatalf("unexpected frames or multi-fragment interval: %+v", location.Frames)
	}

	frameReader, err := location.Reader(reader, 2)
	if err != nil {
		t.Fatalf("Reader(2): %v", err)
	}
	buf := make([]byte, 3)
	if _, err := frameReader.ReadAt(buf, 0); err != nil {
		t.Fatalf("ReadAt last frame: %v", err)
	}
	if !bytes.Equal(buf, []byte{0x30, 0x31, 0x32}) {
		t.Fatalf("last frame = %v", buf)
	}
}

func TestLocateEncapsulated_ExtendedOffsetOverflow(t *testing.T) {
	reader := newSparseExtendedPixelData(0, 12, threeFramePayloads)
	_, err := LocateEncapsulated(reader, 0, 12,
		WithSize(reader.size),
		WithExtendedOffsetTables([]uint64{math.MaxUint64, math.MaxUint64}, []uint64{9, 11}),
		WithoutBasicOffsetTable(),
	)
	if !errors.Is(err, ErrInvalidOffsetTable) {
		t.Fatalf("error = %v, want ErrInvalidOffsetTable", err)
	}
}

func TestLocateEncapsulated_ExtendedLengthDoesNotCoverItem(t *testing.T) {
	data, _ := buildBasicPixelData(0, 8, threeFramePayloads)
	_, err := LocateEncapsulated(bytes.NewReader(data), 0, 8,
		WithSize(uint64(len(data))),
		WithExtendedOffsetTables([]uint64{24, 33, 52}, []uint64{8, 19, 11}),
		WithoutBasicOffsetTable(),
	)
	if !errors.Is(err, ErrInvalidOffsetTable) {
		t.Fatalf("error = %v, want ErrInvalidOffsetTable", err)
	}
}

func TestLocateEncapsulated_BasicOffsetBeyondStream(t *testing.T) {
	data, _ := buildBasicPixelData(0, 8, threeFramePayloads, withBasicOffsets([]uint32{0, 9, 1000}))
	_, err := LocateEncapsulated(bytes.NewReader(data), 0, 8, WithSize(uint64(len(data))))
	if !errors.Is(err, ErrInvalidOffsetTable) {
		t.Fatalf("error = %v, want ErrInvalidOffsetTable", err)
	}
}

type buildOption struct {
	empty   bool
	offsets []uint32
}

type builderOption func(*buildOption)

func withEmptyBasicTable() builderOption {
	return func(o *buildOption) { o.empty = true }
}

func withBasicOffsets(offsets []uint32) builderOption {
	return func(o *buildOption) { o.offsets = offsets }
}

func buildBasicPixelData(pixelStart, headerLength uint64, payloads [][]byte, opts ...builderOption) ([]byte, uint64) {
	cfg := buildOption{offsets: basicOffsets(payloads)}
	for _, opt := range opts {
		opt(&cfg)
	}

	offsets := cfg.offsets
	if cfg.empty {
		offsets = nil
	}

	var b bytes.Buffer
	writeAt := func(off uint64, value []byte) {
		if uint64(b.Len()) < off {
			b.Write(make([]byte, off-uint64(b.Len())))
		}
		b.Write(value)
	}

	header := make([]byte, headerLength)
	binary.LittleEndian.PutUint32(header[:4], pixelDataTag)
	binary.LittleEndian.PutUint32(header[headerLength-4:], undefinedLength)
	writeAt(pixelStart, header)

	tableStart := pixelStart + headerLength
	table := make([]byte, 4*len(offsets))
	for i, offset := range offsets {
		binary.LittleEndian.PutUint32(table[i*4:], offset)
	}
	writeItem(tableStart, table, &b)

	cursor := tableStart + 8 + uint64(len(table))
	for _, payload := range payloads {
		writeItem(cursor, payload, &b)
		cursor += 8 + uint64(len(payload))
	}
	writeTag(cursor, sequenceDelimitationTag, 0, &b)
	return b.Bytes(), pixelStart
}

func basicOffsets(payloads [][]byte) []uint32 {
	return []uint32{0, 9, 28}
}

func writeItem(off uint64, payload []byte, b *bytes.Buffer) {
	writeTag(off, itemTag, uint32(len(payload)), b)
	writeAtBuffer(off+8, payload, b)
}

func writeTag(off uint64, tagValue uint32, length uint32, b *bytes.Buffer) {
	value := make([]byte, 8)
	binary.LittleEndian.PutUint32(value[:4], tagValue)
	binary.LittleEndian.PutUint32(value[4:], length)
	writeAtBuffer(off, value, b)
}

func writeAtBuffer(off uint64, value []byte, b *bytes.Buffer) {
	if uint64(b.Len()) < off {
		b.Write(make([]byte, off-uint64(b.Len())))
	}
	b.Write(value)
}

type sparseReader struct {
	blocks map[uint64][]byte
	size   uint64
}

type fragmentOnlyReader struct {
	data    []byte
	allowed []Fragment
}

func (r *fragmentOnlyReader) ReadAt(p []byte, off int64) (int, error) {
	start := uint64(off)
	end := start + uint64(len(p))
	for _, fragment := range r.allowed {
		if start >= fragment.DataStart && end <= fragment.DataEnd {
			copy(p, r.data[start:end])
			return len(p), nil
		}
	}
	return 0, fmt.Errorf("ReadAt [%d,%d) is outside validated fragment payloads", start, end)
}

func buildExtendedDataset(payloads [][]byte) []byte {
	var b bytes.Buffer
	writeAtBuffer(0, make([]byte, 72), &b)

	header := make([]byte, 12)
	binary.LittleEndian.PutUint32(header[:4], pixelDataTag)
	copy(header[4:6], []byte("OB"))
	binary.LittleEndian.PutUint32(header[8:], undefinedLength)
	b.Write(header)

	cursor := uint64(72 + 12)
	for _, payload := range payloads {
		writeItem(cursor, payload, &b)
		cursor += 8 + uint64(len(payload))
	}
	writeTag(cursor, sequenceDelimitationTag, 0, &b)
	return b.Bytes()
}

func newSparseExtendedPixelData(pixelStart, headerLength uint64, payloads [][]byte) *sparseReader {
	r := &sparseReader{blocks: make(map[uint64][]byte)}

	header := make([]byte, headerLength)
	binary.LittleEndian.PutUint32(header[:4], pixelDataTag)
	binary.LittleEndian.PutUint32(header[headerLength-4:], undefinedLength)
	r.set(pixelStart, header)

	cursor := pixelStart + headerLength
	for _, payload := range payloads {
		tagBytes := make([]byte, 8)
		binary.LittleEndian.PutUint32(tagBytes[:4], itemTag)
		binary.LittleEndian.PutUint32(tagBytes[4:], uint32(len(payload)))
		r.set(cursor, tagBytes)
		r.set(cursor+8, payload)
		cursor += 8 + uint64(len(payload))
	}
	delim := make([]byte, 8)
	binary.LittleEndian.PutUint32(delim[:4], sequenceDelimitationTag)
	r.set(cursor, delim)
	r.size = cursor + 8
	return r
}

func (r *sparseReader) set(off uint64, value []byte) {
	r.blocks[off] = append([]byte(nil), value...)
}

func (r *sparseReader) Size() int64 { return int64(r.size) }

func (r *sparseReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("negative offset %d", off)
	}
	start := uint64(off)
	for i := range p {
		want := start + uint64(i)
		var value byte
		found := false
		for blockStart, block := range r.blocks {
			if want >= blockStart && want < blockStart+uint64(len(block)) {
				value = block[want-blockStart]
				found = true
				break
			}
		}
		if !found {
			return i, io.ErrUnexpectedEOF
		}
		p[i] = value
	}
	return len(p), nil
}
