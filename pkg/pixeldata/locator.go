// Package pixeldata locates and reads encapsulated Pixel Data without
// interpreting compressed payloads such as JPEG.
package pixeldata

import (
	"errors"
	"fmt"
	"io"
)

const (
	itemTag                 uint32 = 0xfffee000
	itemDelimitationTag     uint32 = 0xfffee00d
	sequenceDelimitationTag uint32 = 0xfffee0dd
	pixelDataTag            uint32 = 0x7fe00010
	itemHeaderLength               = 8
	undefinedLength         uint32 = 0xffffffff
)

var (
	// ErrFrameBoundariesUnknown means fragment boundaries alone do not identify frames.
	ErrFrameBoundariesUnknown = errors.New("pixeldata: frame boundaries are unknown")
	// ErrConflictingOffsetTables means BOT and EOT both describe different frames.
	ErrConflictingOffsetTables = errors.New("pixeldata: basic and extended offset tables conflict")
	// ErrInvalidOffsetTable means an offset or extended length violates DICOM structure.
	ErrInvalidOffsetTable = errors.New("pixeldata: invalid offset table")
	// ErrInvalidItem means the Pixel Data item structure is malformed.
	ErrInvalidItem = errors.New("pixeldata: invalid encapsulated Pixel Data item")
	// ErrFrameNotFound means the requested frame index does not exist.
	ErrFrameNotFound = errors.New("pixeldata: frame not found")
)

// Table identifies the structure that determined frame boundaries.
type Table int

const (
	TableUnknown Table = iota
	TableBasic
	TableExtended
	TableBoth
	TableStructure
)

// Fragment is a validated encapsulated Pixel Data item payload range.
type Fragment struct {
	Index       int
	ItemStart   uint64
	DataStart   uint64
	DataEnd     uint64
	ValueLength uint64
}

// Frame is one compressed frame located from DICOM item/table structure.
type Frame struct {
	Index             int
	Fragments         []Fragment
	FirstFragment     int
	LastFragment      int
	DataStart         uint64
	DataEnd           uint64
	ValidLength       uint64
	EncodedItemLength uint64
}

// Location contains the validated frame and fragment layout.
type Location struct {
	PixelDataStart  uint64
	FragmentBase    uint64
	SequenceEnd     uint64
	Table           Table
	Fragments       []Fragment
	Frames          []Frame
	BasicOffsets    []uint32
	ExtendedOffsets []uint64
	ExtendedLengths []uint64
}

// Reader returns a constrained ReaderAt for frame i. It never reads item tags,
// item lengths, BOT bytes, or bytes outside the frame's validated fragments.
func (l *Location) Reader(r io.ReaderAt, i int) (*FrameReader, error) {
	if r == nil {
		return nil, errors.New("pixeldata: nil ReaderAt")
	}
	if i < 0 || i >= len(l.Frames) {
		return nil, fmt.Errorf("read frame %d: %w", i, ErrFrameNotFound)
	}
	return &FrameReader{reader: r, frame: l.Frames[i]}, nil
}

// FrameReader reads a located frame. Its coordinate space is relative to the
// frame's compressed payload, with fragment headers removed.
type FrameReader struct {
	reader io.ReaderAt
	frame  Frame
}

// Len returns the number of valid compressed bytes in the frame.
func (r *FrameReader) Len() uint64 { return r.frame.ValidLength }

// ReadAt implements io.ReaderAt at frame-relative offset.
func (r *FrameReader) ReadAt(p []byte, off int64) (int, error) {
	if off < 0 {
		return 0, fmt.Errorf("read frame %d at negative offset %d", r.frame.Index, off)
	}
	relative := uint64(off)
	if relative >= r.frame.ValidLength {
		if len(p) == 0 {
			return 0, nil
		}
		return 0, io.EOF
	}

	frameOffset := relative
	limit := r.frame.ValidLength - frameOffset
	want := uint64(len(p))
	short := want > limit
	if short {
		want = limit
	}

	var n uint64
	for want > 0 {
		frag, fragOff, ok := r.resolve(frameOffset)
		if !ok {
			return int(n), io.ErrUnexpectedEOF
		}
		nThis := frag.DataEnd - frag.DataStart - fragOff
		if nThis > want {
			nThis = want
		}
		got, err := r.reader.ReadAt(p[n:uint64(n)+nThis], int64(frag.DataStart+fragOff))
		n += uint64(got)
		if err != nil {
			return int(n), err
		}
		if uint64(got) != nThis {
			return int(n), io.ErrUnexpectedEOF
		}
		frameOffset += uint64(got)
		want -= uint64(got)
	}
	if short {
		return int(n), io.EOF
	}
	return int(n), nil
}

func (r *FrameReader) resolve(off uint64) (Fragment, uint64, bool) {
	var before uint64
	for _, frag := range r.frame.Fragments {
		size := frag.DataEnd - frag.DataStart
		end, err := safeAddU64(before, size)
		if err != nil || off < end {
			return frag, off - before, true
		}
		before = end
	}
	return Fragment{}, 0, false
}

type config struct {
	size            uint64
	hasSize         bool
	extendedOffsets []uint64
	extendedLengths []uint64
	firstIsFragment bool
}

// Option configures encapsulated Pixel Data location.
type Option func(*config)

// WithSize sets the logical size used to validate offsets and ranges on sparse
// ReaderAt implementations.
func WithSize(size uint64) Option {
	return func(c *config) { c.size = size; c.hasSize = true }
}

// WithExtendedOffsetTables supplies EOT (7fe0,0001) and EOT lengths
// (7fe0,0002), which are encoded immediately before Pixel Data.
func WithExtendedOffsetTables(offsets, lengths []uint64) Option {
	return func(c *config) {
		c.extendedOffsets = offsets
		c.extendedLengths = lengths
	}
}

// WithoutBasicOffsetTable indicates that the first Pixel Data item is a
// fragment. This is required by DICOM when an Extended Offset Table is used.
func WithoutBasicOffsetTable() Option {
	return func(c *config) { c.firstIsFragment = true }
}
