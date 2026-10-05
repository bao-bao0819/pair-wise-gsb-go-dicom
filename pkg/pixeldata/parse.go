package pixeldata

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
)

type fileSizer interface {
	Size() int64
}

// LocateEncapsulated parses the structure of one undefined-length Pixel Data
// value beginning at pixelDataStart. pixelDataHeaderLength is 12 for explicit
// VR and 8 for implicit VR. Encapsulated Pixel Data item structure is always
// encoded Little Endian.
//
// Basic Offset Table entries are relative to the first fragment item after
// the BOT. Extended Offset Table entries are relative to the Pixel Data
// element start (so the first entry equals the Pixel Data header length).
func LocateEncapsulated(r io.ReaderAt, pixelDataStart uint64, pixelDataHeaderLength uint64, opts ...Option) (*Location, error) {
	if r == nil {
		return nil, errors.New("pixeldata: nil ReaderAt")
	}
	if pixelDataHeaderLength != 8 && pixelDataHeaderLength != 12 {
		return nil, fmt.Errorf("%w: Pixel Data header length must be 8 or 12, got %d", ErrInvalidItem, pixelDataHeaderLength)
	}

	c := config{}
	for _, opt := range opts {
		opt(&c)
	}
	if !c.hasSize {
		if sized, ok := r.(fileSizer); ok {
			if size := sized.Size(); size >= 0 {
				c.size, c.hasSize = uint64(size), true
			}
		}
	}
	if len(c.extendedOffsets) != len(c.extendedLengths) {
		return nil, fmt.Errorf("%w: extended offset count %d differs from length count %d", ErrInvalidOffsetTable, len(c.extendedOffsets), len(c.extendedLengths))
	}

	l := &Location{
		PixelDataStart:  pixelDataStart,
		ExtendedOffsets: append([]uint64(nil), c.extendedOffsets...),
		ExtendedLengths: append([]uint64(nil), c.extendedLengths...),
	}

	header := make([]byte, pixelDataHeaderLength)
	if err := readFullAt(r, header, pixelDataStart); err != nil {
		return nil, fmt.Errorf("read Pixel Data header: %w", err)
	}
	if binary.LittleEndian.Uint32(header[:4]) != pixelDataTag {
		return nil, fmt.Errorf("%w: expected Pixel Data tag, got %08x", ErrInvalidItem, binary.LittleEndian.Uint32(header[:4]))
	}
	if binary.LittleEndian.Uint32(header[pixelDataHeaderLength-4:]) != undefinedLength {
		return nil, fmt.Errorf("%w: Pixel Data value length is defined, not encapsulated", ErrInvalidItem)
	}

	pos, err := safeAddU64(pixelDataStart, pixelDataHeaderLength)
	if err != nil {
		return nil, fmt.Errorf("%w: Pixel Data start and header overflow", ErrInvalidItem)
	}

	if !c.firstIsFragment {
		basic, next, err := readBasicOffsetTable(r, pos, c)
		if err != nil {
			return nil, err
		}
		l.BasicOffsets = basic
		pos = next
	}

	l.FragmentBase = pos
	pos, err = l.scanFragments(r, pos, c)
	if err != nil {
		return nil, err
	}
	l.SequenceEnd = pos

	if err := l.buildFrames(); err != nil {
		return nil, err
	}
	if c.hasSize {
		if err := l.validateSize(c.size); err != nil {
			return nil, err
		}
	}
	return l, nil
}

func readBasicOffsetTable(r io.ReaderAt, itemStart uint64, c config) ([]uint32, uint64, error) {
	var hdr [itemHeaderLength]byte
	if err := readFullAt(r, hdr[:], itemStart); err != nil {
		return nil, 0, fmt.Errorf("read basic offset table item: %w", err)
	}
	if binary.LittleEndian.Uint32(hdr[:4]) != itemTag {
		return nil, 0, fmt.Errorf("%w: expected basic offset table item at %d, got %08x", ErrInvalidItem, itemStart, binary.LittleEndian.Uint32(hdr[:4]))
	}
	tableLength64 := binary.LittleEndian.Uint32(hdr[4:])
	tableLength := uint64(tableLength64)
	if tableLength%4 != 0 {
		return nil, 0, fmt.Errorf("%w: basic offset table value length %d is not a multiple of four", ErrInvalidOffsetTable, tableLength)
	}
	dataStart, err := safeAddU64(itemStart, itemHeaderLength)
	if err != nil {
		return nil, 0, err
	}
	dataEnd, err := safeAddU64(dataStart, tableLength)
	if err != nil {
		return nil, 0, err
	}
	if c.hasSize && dataEnd > c.size {
		return nil, 0, fmt.Errorf("%w: basic offset table ends at %d beyond size %d", ErrInvalidItem, dataEnd, c.size)
	}

	buf := make([]byte, tableLength)
	if err := readFullAt(r, buf, dataStart); err != nil {
		return nil, 0, fmt.Errorf("read basic offset table: %w", err)
	}
	entries := make([]uint32, tableLength/4)
	for i := range entries {
		entries[i] = binary.LittleEndian.Uint32(buf[i*4:])
	}
	return entries, dataEnd, nil
}

func (l *Location) scanFragments(r io.ReaderAt, pos uint64, c config) (uint64, error) {
	var hdr [itemHeaderLength]byte
	for {
		if err := readFullAt(r, hdr[:], pos); err != nil {
			return 0, fmt.Errorf("%w: cannot read item header at %d: %v", ErrInvalidItem, pos, err)
		}
		item := binary.LittleEndian.Uint32(hdr[:4])
		length := binary.LittleEndian.Uint32(hdr[4:])

		if item == sequenceDelimitationTag {
			if length != 0 {
				return 0, fmt.Errorf("%w: sequence delimitation length is %d, expected 0", ErrInvalidItem, length)
			}
			end, err := safeAddU64(pos, itemHeaderLength)
			if err != nil {
				return 0, err
			}
			return end, nil
		}
		if item != itemTag {
			return 0, fmt.Errorf("%w: expected fragment item at %d, got %08x", ErrInvalidItem, pos, item)
		}
		if length == undefinedLength {
			return 0, fmt.Errorf("%w: fragment at %d has undefined length", ErrInvalidItem, pos)
		}

		dataStart, err := safeAddU64(pos, itemHeaderLength)
		if err != nil {
			return 0, err
		}
		dataEnd, err := safeAddU64(dataStart, uint64(length))
		if err != nil {
			return 0, err
		}
		if c.hasSize && dataEnd > c.size {
			return 0, fmt.Errorf("%w: fragment at %d ends at %d beyond size %d", ErrInvalidItem, pos, dataEnd, c.size)
		}

		l.Fragments = append(l.Fragments, Fragment{
			Index:       len(l.Fragments),
			ItemStart:   pos,
			DataStart:   dataStart,
			DataEnd:     dataEnd,
			ValueLength: uint64(length),
		})
		pos = dataEnd
	}
}

func readFullAt(r io.ReaderAt, buf []byte, off uint64) error {
	if off > math.MaxInt64 {
		return fmt.Errorf("%w: offset %d exceeds int64", ErrInvalidItem, off)
	}
	end, err := safeAddU64(off, uint64(len(buf)))
	if err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidItem, err)
	}
	if end > math.MaxInt64 {
		return fmt.Errorf("%w: read ending at %d exceeds int64", ErrInvalidItem, end)
	}
	n, err := r.ReadAt(buf, int64(off))
	if err == nil && n != len(buf) {
		return io.ErrUnexpectedEOF
	}
	return err
}

func safeAddU64(a, b uint64) (uint64, error) {
	if a > math.MaxUint64-b {
		return 0, fmt.Errorf("%w: integer overflow adding %d and %d", ErrInvalidItem, a, b)
	}
	return a + b, nil
}

func (l *Location) validateSize(size uint64) error {
	if l.SequenceEnd > size {
		return fmt.Errorf("%w: Pixel Data ends at %d beyond logical size %d", ErrInvalidItem, l.SequenceEnd, size)
	}
	return nil
}
