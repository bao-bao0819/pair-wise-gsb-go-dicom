package pixeldata

import "fmt"

type frameSpan struct {
	start uint64
	end   uint64
}

func (l *Location) buildFrames() error {
	hasBasic := len(l.BasicOffsets) > 0
	hasExtended := len(l.ExtendedOffsets) > 0

	switch {
	case hasBasic && hasExtended:
		l.Table = TableBoth
		if err := l.validateTableAgreement(); err != nil {
			return err
		}
		return l.framesFromExtended(l.ExtendedOffsets, l.ExtendedLengths)
	case hasBasic:
		l.Table = TableBasic
		return l.framesFromBasic(l.BasicOffsets)
	case hasExtended:
		l.Table = TableExtended
		return l.framesFromExtended(l.ExtendedOffsets, l.ExtendedLengths)
	default:
		l.Table = TableStructure
		if len(l.Fragments) != 1 {
			return fmt.Errorf("%w: empty offset table leaves %d fragments without unique frame boundaries", ErrFrameBoundariesUnknown, len(l.Fragments))
		}
		frag := l.Fragments[0]
		frameEnd, err := subtractU64(l.SequenceEnd, itemHeaderLength)
		if err != nil {
			return fmt.Errorf("%w: sequence end underflow: %v", ErrInvalidItem, err)
		}
		frame, err := l.makeFrame(0, 0, 1, frag.ItemStart, frameEnd)
		if err != nil {
			return err
		}
		l.Frames = []Frame{frame}
		return l.validateFrameStructure(0, 0, 1)
	}
}

func (l *Location) framesFromBasic(offsets []uint32) error {
	spans, firstFragment, err := l.basicSpans(offsets)
	if err != nil {
		return err
	}

	l.Frames = make([]Frame, len(spans))
	for i, span := range spans {
		lastFragment := len(l.Fragments)
		if i+1 < len(spans) {
			lastFragment, err = l.fragmentIndexAt(spans[i+1].start)
			if err != nil {
				return err
			}
		}
		l.Frames[i], err = l.makeFrame(i, firstFragment[i], lastFragment, span.start, span.end)
		if err != nil {
			return err
		}
		if err := l.validateFrameStructure(i, firstFragment[i], lastFragment); err != nil {
			return err
		}
	}
	if firstFragment[0] != 0 {
		return fmt.Errorf("%w: first frame starts at fragment %d, not fragment 0", ErrInvalidOffsetTable, firstFragment[0])
	}
	if l.Frames[len(l.Frames)-1].LastFragment != len(l.Fragments) {
		return fmt.Errorf("%w: %d fragments are not assigned to a frame", ErrInvalidOffsetTable, len(l.Fragments)-l.Frames[len(l.Frames)-1].LastFragment)
	}
	return nil
}

func (l *Location) basicSpans(offsets []uint32) ([]frameSpan, []int, error) {
	spans := make([]frameSpan, 0, len(offsets))
	fragmentIndexes := make([]int, 0, len(offsets))
	var previous uint64
	for i, rawOffset := range offsets {
		absolute, err := safeAddU64(l.FragmentBase, uint64(rawOffset))
		if err != nil {
			return nil, nil, fmt.Errorf("%w: basic offset %d overflows: %v", ErrInvalidOffsetTable, rawOffset, err)
		}
		if i == 0 && rawOffset != 0 {
			return nil, nil, fmt.Errorf("%w: first basic offset must be 0, got %d", ErrInvalidOffsetTable, rawOffset)
		}
		if i > 0 && absolute <= previous {
			return nil, nil, fmt.Errorf("%w: basic offset %d at index %d is not strictly increasing", ErrInvalidOffsetTable, rawOffset, i)
		}
		fragmentIndex, err := l.fragmentIndexAt(absolute)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: basic offset %d (absolute %d): %v", ErrInvalidOffsetTable, rawOffset, absolute, err)
		}
		if i > 0 && fragmentIndex <= fragmentIndexes[i-1] {
			return nil, nil, fmt.Errorf("%w: basic offset %d does not start a later fragment", ErrInvalidOffsetTable, rawOffset)
		}
		spans = append(spans, frameSpan{start: absolute})
		fragmentIndexes = append(fragmentIndexes, fragmentIndex)
		previous = absolute
	}

	for i := range spans {
		if i+1 < len(spans) {
			spans[i].end = spans[i+1].start
		} else {
			delimiterStart, err := subtractU64(l.SequenceEnd, itemHeaderLength)
			if err != nil {
				return nil, nil, err
			}
			spans[i].end = delimiterStart
		}
	}

	for i, span := range spans {
		last := len(l.Fragments)
		if i+1 < len(spans) {
			var err error
			last, err = l.fragmentIndexAt(spans[i+1].start)
			if err != nil {
				return nil, nil, err
			}
		}
		if err := l.validateEncodedLength(span.start, span.end, fragmentIndexes[i], last); err != nil {
			return nil, nil, fmt.Errorf("%w: basic table frame %d: %v", ErrInvalidOffsetTable, i, err)
		}
	}
	return spans, fragmentIndexes, nil
}

func (l *Location) framesFromExtended(offsets, encodedLengths []uint64) error {
	if len(offsets) == 0 {
		return fmt.Errorf("%w: extended offset table is empty", ErrInvalidOffsetTable)
	}

	l.Frames = make([]Frame, len(offsets))
	firstFragment := -1
	for i, rawOffset := range offsets {
		offset, err := safeAddU64(l.PixelDataStart, rawOffset)
		if err != nil {
			return fmt.Errorf("%w: extended offset %d overflows from Pixel Data start %d: %v", ErrInvalidOffsetTable, rawOffset, l.PixelDataStart, err)
		}
		if i == 0 && offset != l.FragmentBase {
			return fmt.Errorf("%w: first extended offset resolves to %d, but first fragment item is %d", ErrInvalidOffsetTable, offset, l.FragmentBase)
		}
		if i > 0 && rawOffset <= offsets[i-1] {
			return fmt.Errorf("%w: extended offset %d at index %d is not strictly increasing", ErrInvalidOffsetTable, rawOffset, i)
		}
		index, err := l.fragmentIndexAt(offset)
		if err != nil {
			return fmt.Errorf("%w: extended offset %d (absolute %d): %v", ErrInvalidOffsetTable, rawOffset, offset, err)
		}
		if i > 0 && index <= firstFragment {
			return fmt.Errorf("%w: extended offset %d does not start a later fragment", ErrInvalidOffsetTable, rawOffset)
		}
		firstFragment = index

		end, err := safeAddU64(offset, encodedLengths[i])
		if err != nil {
			return fmt.Errorf("%w: extended frame %d offset and length overflow: %v", ErrInvalidOffsetTable, i, err)
		}
		if encodedLengths[i] < itemHeaderLength {
			return fmt.Errorf("%w: extended frame %d length %d is shorter than an item header", ErrInvalidOffsetTable, i, encodedLengths[i])
		}
		if i+1 < len(offsets) {
			nextStart, err := safeAddU64(l.PixelDataStart, offsets[i+1])
			if err != nil {
				return fmt.Errorf("%w: next extended offset overflows: %v", ErrInvalidOffsetTable, err)
			}
			if end != nextStart {
				return fmt.Errorf("%w: frame %d ends at %d but next frame starts at %d", ErrInvalidOffsetTable, i, end, nextStart)
			}
		}
		if i == len(offsets)-1 {
			delimiterStart, err := subtractU64(l.SequenceEnd, itemHeaderLength)
			if err != nil {
				return err
			}
			if end != delimiterStart {
				return fmt.Errorf("%w: final frame ends at %d but delimiter starts at %d", ErrInvalidOffsetTable, end, delimiterStart)
			}
		}

		last := len(l.Fragments)
		if i+1 < len(offsets) {
			nextStart, nextErr := safeAddU64(l.PixelDataStart, offsets[i+1])
			if nextErr != nil {
				return nextErr
			}
			last, err = l.fragmentIndexAt(nextStart)
			if err != nil {
				return err
			}
		}
		l.Frames[i], err = l.makeFrame(i, index, last, offset, end)
		if err != nil {
			return err
		}
		if err := l.validateFrameStructure(i, index, last); err != nil {
			return err
		}
		if l.Frames[i].EncodedItemLength != encodedLengths[i] {
			return fmt.Errorf("%w: extended frame %d length is %d but items span %d", ErrInvalidOffsetTable, i, encodedLengths[i], l.Frames[i].EncodedItemLength)
		}
	}
	if l.Frames[0].FirstFragment != 0 {
		return fmt.Errorf("%w: first extended frame does not include fragment 0", ErrInvalidOffsetTable)
	}
	if l.Frames[len(l.Frames)-1].LastFragment != len(l.Fragments) {
		return fmt.Errorf("%w: extended table does not account for all fragments", ErrInvalidOffsetTable)
	}
	return nil
}

func (l *Location) validateTableAgreement() error {
	spans, indexes, err := l.basicSpans(l.BasicOffsets)
	if err != nil {
		return err
	}
	if len(spans) != len(l.ExtendedOffsets) {
		return fmt.Errorf("%w: basic table has %d frames but extended table has %d", ErrConflictingOffsetTables, len(spans), len(l.ExtendedOffsets))
	}
	for i, span := range spans {
		extendedStart, err := safeAddU64(l.PixelDataStart, l.ExtendedOffsets[i])
		if err != nil {
			return fmt.Errorf("%w: frame %d extended offset overflows", ErrConflictingOffsetTables, i)
		}
		if span.start != extendedStart {
			return fmt.Errorf("%w: frame %d starts at %d in BOT but absolute %d in EOT", ErrConflictingOffsetTables, i, span.start, extendedStart)
		}
		encodedLength, err := subtractU64(span.end, span.start)
		if err != nil {
			return err
		}
		if encodedLength != l.ExtendedLengths[i] {
			return fmt.Errorf("%w: frame %d spans %d bytes in BOT but EOT length is %d", ErrConflictingOffsetTables, i, encodedLength, l.ExtendedLengths[i])
		}
		expectedFragment, err := l.fragmentIndexAt(extendedStart)
		if err != nil {
			return fmt.Errorf("%w: frame %d extended start does not hit an item boundary", ErrConflictingOffsetTables, i)
		}
		if indexes[i] != expectedFragment {
			return fmt.Errorf("%w: frame %d starts at BOT fragment %d but EOT fragment %d", ErrConflictingOffsetTables, i, indexes[i], expectedFragment)
		}
	}
	return nil
}

func (l *Location) fragmentIndexAt(itemStart uint64) (int, error) {
	for i, fragment := range l.Fragments {
		if fragment.ItemStart == itemStart {
			return i, nil
		}
	}
	return -1, fmt.Errorf("offset does not point to an item boundary")
}

func (l *Location) validateEncodedLength(start, end uint64, first, last int) error {
	if first < 0 || last <= first || last > len(l.Fragments) {
		return fmt.Errorf("invalid fragment interval [%d,%d)", first, last)
	}
	if l.Fragments[first].ItemStart != start {
		return fmt.Errorf("start does not match fragment %d", first)
	}
	var total uint64
	for _, fragment := range l.Fragments[first:last] {
		span, err := subtractU64(fragment.DataEnd, fragment.ItemStart)
		if err != nil {
			return err
		}
		total, err = safeAddU64(total, span)
		if err != nil {
			return err
		}
	}
	lastEnd := l.Fragments[last-1].DataEnd
	if lastEnd != end {
		return fmt.Errorf("items end at %d but frame boundary is %d", lastEnd, end)
	}
	return nil
}

func (l *Location) validateFrameStructure(frameIndex, first, last int) error {
	err := l.validateEncodedLength(l.Fragments[first].ItemStart, l.Fragments[last-1].DataEnd, first, last)
	if err != nil {
		return fmt.Errorf("%w: frame %d: %v", ErrInvalidOffsetTable, frameIndex, err)
	}
	return nil
}

func (l *Location) makeFrame(index, first, last int, start, end uint64) (Frame, error) {
	fragments := append([]Fragment(nil), l.Fragments[first:last]...)
	var validLength uint64
	for _, fragment := range fragments {
		payloadLength, err := subtractU64(fragment.DataEnd, fragment.DataStart)
		if err != nil {
			return Frame{}, err
		}
		validLength, err = safeAddU64(validLength, payloadLength)
		if err != nil {
			return Frame{}, err
		}
	}
	encodedLength, err := subtractU64(end, start)
	if err != nil {
		return Frame{}, err
	}
	return Frame{
		Index:             index,
		Fragments:         fragments,
		FirstFragment:     first,
		LastFragment:      last,
		DataStart:         fragments[0].DataStart,
		DataEnd:           fragments[len(fragments)-1].DataEnd,
		ValidLength:       validLength,
		EncodedItemLength: encodedLength,
	}, nil
}

func subtractU64(a, b uint64) (uint64, error) {
	if a < b {
		return 0, fmt.Errorf("%w: unsigned subtraction underflow %d-%d", ErrInvalidItem, a, b)
	}
	return a - b, nil
}
