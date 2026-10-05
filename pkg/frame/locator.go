package frame

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"

	"github.com/suyashkumar/dicom/pkg/tag"
)

// itemHeaderLen is the length in bytes of a DICOM Item header (4 byte tag +
// 4 byte VL), as used by encapsulated Pixel Data fragments.
const itemHeaderLen = 8

var (
	// ErrBoundariesUnknown indicates that neither a Basic Offset Table nor an
	// Extended Offset Table was available, and the frame boundaries could not
	// be uniquely determined from the known structure. Callers must not fall
	// back to treating each fragment as its own frame.
	ErrBoundariesUnknown = errors.New("frame boundaries unknown")
	// ErrOffsetTableConflict indicates that both a Basic Offset Table and an
	// Extended Offset Table were present, but they disagree.
	ErrOffsetTableConflict = errors.New("basic offset table and extended offset table conflict")
	// ErrNonMonotonicOffsets indicates that frame offsets are not strictly
	// increasing.
	ErrNonMonotonicOffsets = errors.New("frame offsets are not strictly increasing")
	// ErrOffsetNotOnItemBoundary indicates that a frame offset does not land
	// exactly on the Item Tag of a fragment.
	ErrOffsetNotOnItemBoundary = errors.New("frame offset does not land on a fragment item boundary")
	// ErrOffsetOutOfRange indicates that a frame offset points beyond the
	// available fragments.
	// ErrOffsetOutOfRange indicates that a frame offset points beyond the
	// available fragments.
	ErrOffsetOutOfRange = errors.New("frame offset out of range")
	// ErrInvalidOffsetTable indicates a malformed Basic or Extended Offset
	// Table.
	ErrInvalidOffsetTable = errors.New("invalid offset table")
	// ErrFrameCountMismatch indicates the offset tables describe a different
	// number of frames than NumberOfFrames.
	ErrFrameCountMismatch = errors.New("frame count mismatch")
	// ErrExtendedLengthMismatch indicates the Extended Offset Table Lengths do
	// not match the fragment extents.
	ErrExtendedLengthMismatch = errors.New("extended offset table lengths do not match fragment extents")
	// ErrTruncatedItem indicates an Item whose header or value is cut short.
	ErrTruncatedItem = errors.New("truncated item")
	// ErrExpectedItem indicates a tag other than Item (FFFE,E000) or
	// SequenceDelimitationItem (FFFE,E0DD) was found while scanning
	// encapsulated Pixel Data.
	ErrExpectedItem = errors.New("expected item tag in encapsulated pixel data")
	// ErrUndefinedLengthItem indicates an Item with undefined length, which is
	// not permitted inside encapsulated Pixel Data.
	ErrUndefinedLengthItem = errors.New("undefined-length item in encapsulated pixel data")
	// ErrOffsetOverflow indicates that offset arithmetic would overflow.
	ErrOffsetOverflow = errors.New("offset arithmetic overflow")
)

// Fragment describes a single Item (fragment) of an encapsulated Pixel Data
// element in purely structural terms. No compressed content is decoded.
type Fragment struct {
	// TagOffset is the absolute offset of this fragment's Item Tag (FFFE,E000)
	// in the source.
	TagOffset uint64
	// ValueOffset is the absolute offset of this fragment's value bytes.
	ValueOffset uint64
	// Length is the fragment value length in bytes.
	Length uint32
}

// FrameSpan identifies the fragments composing a single frame and the total
// number of valid (compressed) bytes in it.
type FrameSpan struct {
	// FirstFragment is the index (into Layout.Fragments) of the first
	// fragment of this frame.
	FirstFragment int
	// FragmentCount is the number of consecutive fragments in this frame.
	FragmentCount int
	// ByteLength is the total number of valid compressed bytes in this frame
	// (the sum of its fragment value lengths).
	ByteLength uint64
}

// Layout is the validated structural map of an encapsulated Pixel Data
// element: which fragments exist, and which fragments make up each frame.
type Layout struct {
	// Fragments holds all fragments of the Pixel Data element in file order.
	Fragments []Fragment
	// Frames holds one FrameSpan per frame.
	Frames []FrameSpan
}

// addUint64 adds a and b, returning ErrOffsetOverflow on overflow.
func addUint64(a, b uint64) (uint64, error) {
	if a > math.MaxUint64-b {
		return 0, fmt.Errorf("%d + %d overflows uint64: %w", a, b, ErrOffsetOverflow)
	}
	return a + b, nil
}

// uint64ToInt64 converts v to an int64 usable with io.ReaderAt, returning
// ErrOffsetOverflow if v is not representable.
func uint64ToInt64(v uint64) (int64, error) {
	if v > math.MaxInt64 {
		return 0, fmt.Errorf("offset %d exceeds maximum addressable offset: %w", v, ErrOffsetOverflow)
	}
	return int64(v), nil
}

// ScanEncapsulatedPixelData scans the Item structure of an encapsulated
// (undefined length) Pixel Data value from ra, starting at start (the offset
// of the first Item, i.e. the Basic Offset Table item). It returns the raw
// Basic Offset Table value bytes, the structural description of every
// fragment, and the offset just past the Sequence Delimitation Item. Fragment
// payload bytes are not read (only their extent is validated), and no
// compressed content is decoded.
func ScanEncapsulatedPixelData(ra io.ReaderAt, start uint64) (basicOffsetTable []byte, fragments []Fragment, end uint64, err error) {
	pos := start
	first := true
	var hdr [itemHeaderLen]byte
	for {
		pos64, err := uint64ToInt64(pos)
		if err != nil {
			return nil, nil, 0, err
		}
		if _, err := ra.ReadAt(hdr[:], pos64); err != nil {
			return nil, nil, 0, fmt.Errorf("error reading item header at offset %d: %w", pos, errors.Join(ErrTruncatedItem, err))
		}
		t := tag.Tag{
			Group:   binary.LittleEndian.Uint16(hdr[0:2]),
			Element: binary.LittleEndian.Uint16(hdr[2:4]),
		}
		vl := binary.LittleEndian.Uint32(hdr[4:8])
		valueOffset, err := addUint64(pos, itemHeaderLen)
		if err != nil {
			return nil, nil, 0, err
		}
		if t == tag.SequenceDelimitationItem {
			return basicOffsetTable, fragments, valueOffset, nil
		}
		if t != tag.Item {
			return nil, nil, 0, fmt.Errorf("found tag %v at offset %d: %w", t, pos, ErrExpectedItem)
		}
		if vl == tag.VLUndefinedLength {
			return nil, nil, 0, fmt.Errorf("item at offset %d: %w", pos, ErrUndefinedLengthItem)
		}
		valueEnd, err := addUint64(valueOffset, uint64(vl))
		if err != nil {
			return nil, nil, 0, err
		}
		if first {
			// The first Item is the Basic Offset Table; it is small by
			// construction (an array of uint32 offsets), so read it fully.
			bot := make([]byte, vl)
			bot64, err := uint64ToInt64(valueOffset)
			if err != nil {
				return nil, nil, 0, err
			}
			if _, err := ra.ReadAt(bot, bot64); err != nil {
				return nil, nil, 0, fmt.Errorf("error reading basic offset table at offset %d: %w", valueOffset, errors.Join(ErrTruncatedItem, err))
			}
			basicOffsetTable = bot
			first = false
		} else {
			if vl > 0 {
				// Probe the last byte of the fragment to verify the item is
				// not truncated, without reading the (potentially large)
				// compressed payload.
				probe64, err := uint64ToInt64(valueEnd - 1)
				if err != nil {
					return nil, nil, 0, err
				}
				var probe [1]byte
				if _, err := ra.ReadAt(probe[:], probe64); err != nil {
					return nil, nil, 0, fmt.Errorf("item at offset %d (vl=%d) is truncated: %w", pos, vl, errors.Join(ErrTruncatedItem, err))
				}
			}
			fragments = append(fragments, Fragment{TagOffset: pos, ValueOffset: valueOffset, Length: vl})
		}
		pos = valueEnd
	}
}

// LocateFrames computes the structural frame boundaries of an encapsulated
// Pixel Data element from its fragments and offset tables, without decoding
// any compressed content.
//
// basicOffsetTable is the raw value of the Basic Offset Table item (may be
// empty). extOffsets and extLengths are the parsed values of the Extended
// Offset Table (7FE0,0001) and Extended Offset Table Lengths (7FE0,0002)
// elements, if present. numberOfFrames is the value of NumberOfFrames
// (0028,0008), or 0 if unknown.
//
// Per DICOM PS3.5 A.4 and PS3.3 C.7.6.3.1.8, offsets in both tables are byte
// offsets to the first byte of the Item Tag of the first fragment of each
// frame, measured from the first byte of the first Item Tag following the
// Basic Offset Table item (i.e. fragments[0].TagOffset).
//
// If both tables are present and non-empty but disagree, an error wrapping
// ErrOffsetTableConflict is returned. If neither table is usable and the
// boundaries cannot be uniquely determined (more than one potential frame),
// an error wrapping ErrBoundariesUnknown is returned.
func LocateFrames(fragments []Fragment, basicOffsetTable []byte, extOffsets, extLengths []uint64, numberOfFrames int) (*Layout, error) {
	if len(extLengths) > 0 && len(extOffsets) == 0 {
		return nil, fmt.Errorf("extended offset table lengths present without extended offset table: %w", ErrInvalidOffsetTable)
	}
	if len(extOffsets) > 0 && len(extLengths) > 0 && len(extOffsets) != len(extLengths) {
		return nil, fmt.Errorf("extended offset table has %d offsets but %d lengths: %w", len(extOffsets), len(extLengths), ErrInvalidOffsetTable)
	}

	botOffsets, err := parseBasicOffsetTable(basicOffsetTable)
	if err != nil {
		return nil, err
	}

	var offsets []uint64
	switch {
	case len(botOffsets) > 0 && len(extOffsets) > 0:
		if len(botOffsets) != len(extOffsets) {
			return nil, fmt.Errorf("basic offset table describes %d frames, extended offset table describes %d: %w", len(botOffsets), len(extOffsets), ErrOffsetTableConflict)
		}
		for i := range botOffsets {
			if botOffsets[i] != extOffsets[i] {
				return nil, fmt.Errorf("frame %d: basic offset table offset %d != extended offset table offset %d: %w", i, botOffsets[i], extOffsets[i], ErrOffsetTableConflict)
			}
		}
		offsets = botOffsets
	case len(botOffsets) > 0:
		offsets = botOffsets
	case len(extOffsets) > 0:
		offsets = extOffsets
	default:
		// No usable offset table. Boundaries are only uniquely determined if
		// there is exactly one frame (all fragments belong to it) or no
		// fragments at all.
		if len(fragments) == 0 {
			if numberOfFrames > 0 {
				return nil, fmt.Errorf("NumberOfFrames=%d but no fragments present: %w", numberOfFrames, ErrFrameCountMismatch)
			}
			return &Layout{}, nil
		}
		if numberOfFrames == 1 {
			return buildLayout(fragments, []int{0}, nil)
		}
		return nil, fmt.Errorf("empty offset tables with %d fragments and NumberOfFrames=%d: %w", len(fragments), numberOfFrames, ErrBoundariesUnknown)
	}

	if numberOfFrames > 0 && numberOfFrames != len(offsets) {
		return nil, fmt.Errorf("offset table describes %d frames but NumberOfFrames=%d: %w", len(offsets), numberOfFrames, ErrFrameCountMismatch)
	}

	bounds, err := validateOffsets(fragments, offsets)
	if err != nil {
		return nil, err
	}
	return buildLayout(fragments, bounds, extLengths)
}

// parseBasicOffsetTable parses the raw Basic Offset Table value into uint64
// offsets. An empty table yields no offsets.
func parseBasicOffsetTable(bot []byte) ([]uint64, error) {
	if len(bot) == 0 {
		return nil, nil
	}
	if len(bot)%4 != 0 {
		return nil, fmt.Errorf("basic offset table length %d is not a multiple of 4: %w", len(bot), ErrInvalidOffsetTable)
	}
	offsets := make([]uint64, len(bot)/4)
	for i := range offsets {
		offsets[i] = uint64(binary.LittleEndian.Uint32(bot[i*4:]))
	}
	return offsets, nil
}

// validateOffsets checks that offsets are strictly increasing, start at zero,
// and each lands exactly on a fragment Item Tag boundary. It returns the
// fragment index at which each frame starts.
func validateOffsets(fragments []Fragment, offsets []uint64) ([]int, error) {
	if len(fragments) == 0 {
		return nil, fmt.Errorf("offset table describes %d frames but no fragments are present: %w", len(offsets), ErrOffsetOutOfRange)
	}
	if offsets[0] != 0 {
		return nil, fmt.Errorf("first frame offset is %d, want 0: %w", offsets[0], ErrInvalidOffsetTable)
	}
	boundary := make(map[uint64]int, len(fragments))
	for i, f := range fragments {
		boundary[f.TagOffset] = i
	}
	base := fragments[0].TagOffset
	bounds := make([]int, len(offsets))
	prev := uint64(0)
	for i, off := range offsets {
		if i > 0 && off <= prev {
			return nil, fmt.Errorf("frame %d offset %d is not greater than previous offset %d: %w", i, off, prev, ErrNonMonotonicOffsets)
		}
		abs, err := addUint64(base, off)
		if err != nil {
			return nil, fmt.Errorf("frame %d: %w", i, err)
		}
		idx, ok := boundary[abs]
		if !ok {
			if abs > fragments[len(fragments)-1].TagOffset {
				return nil, fmt.Errorf("frame %d offset %d (absolute %d) is beyond the last fragment: %w", i, off, abs, ErrOffsetOutOfRange)
			}
			return nil, fmt.Errorf("frame %d offset %d (absolute %d): %w", i, off, abs, ErrOffsetNotOnItemBoundary)
		}
		bounds[i] = idx
		prev = off
	}
	return bounds, nil
}

// buildLayout builds a Layout from validated frame boundary fragment indices,
// verifying extended lengths when provided.
func buildLayout(fragments []Fragment, bounds []int, extLengths []uint64) (*Layout, error) {
	layout := &Layout{Fragments: fragments, Frames: make([]FrameSpan, len(bounds))}
	for i, start := range bounds {
		end := len(fragments)
		if i+1 < len(bounds) {
			end = bounds[i+1]
		}
		var total uint64
		for _, f := range fragments[start:end] {
			var err error
			total, err = addUint64(total, uint64(f.Length))
			if err != nil {
				return nil, fmt.Errorf("frame %d byte length: %w", i, err)
			}
		}
		if len(extLengths) > 0 && extLengths[i] != total {
			return nil, fmt.Errorf("frame %d: extended offset table length is %d but fragments total %d bytes: %w", i, extLengths[i], total, ErrExtendedLengthMismatch)
		}
		layout.Frames[i] = FrameSpan{FirstFragment: start, FragmentCount: end - start, ByteLength: total}
	}
	return layout, nil
}

// ReadFrame reads the compressed bytes of the frame at frameIdx from ra.
// Reads are strictly limited to the validated fragment ranges of that frame;
// no decompression is performed.
func (l *Layout) ReadFrame(ra io.ReaderAt, frameIdx int) ([]byte, error) {
	if frameIdx < 0 || frameIdx >= len(l.Frames) {
		return nil, fmt.Errorf("frame index %d out of range [0, %d)", frameIdx, len(l.Frames))
	}
	span := l.Frames[frameIdx]
	if span.ByteLength > uint64(math.MaxInt) {
		return nil, fmt.Errorf("frame %d length %d exceeds addressable memory: %w", frameIdx, span.ByteLength, ErrOffsetOverflow)
	}
	data := make([]byte, span.ByteLength)
	pos := uint64(0)
	for _, f := range l.Fragments[span.FirstFragment : span.FirstFragment+span.FragmentCount] {
		valueEnd, err := addUint64(f.ValueOffset, uint64(f.Length))
		if err != nil {
			return nil, err
		}
		if _, err := uint64ToInt64(valueEnd); err != nil {
			return nil, err
		}
		start64, err := uint64ToInt64(f.ValueOffset)
		if err != nil {
			return nil, err
		}
		if _, err := ra.ReadAt(data[pos:pos+uint64(f.Length)], start64); err != nil {
			return nil, fmt.Errorf("error reading frame %d fragment at offset %d (len=%d): %w", frameIdx, f.ValueOffset, f.Length, err)
		}
		pos += uint64(f.Length)
	}
	return data, nil
}
