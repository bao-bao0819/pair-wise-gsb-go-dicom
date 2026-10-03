package dicom

import (
	"bytes"
	"fmt"

	"github.com/suyashkumar/dicom/pkg/dicomio"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// convertSequence transcodes an SQ element. Little Endian targets support
// undefined-length sequences/items and are streamed directly; Explicit VR Big
// Endian (PS3.5 Annex A.3) permits only defined-length sequences and items, so
// each item's content is buffered to determine its length.
func (tc *transcoder) convertSequence(t tag.Tag, offset int64, vl uint32) *ConversionError {
	tc.path = append(tc.path, tag.DebugString(t))
	defer tc.popPath()

	if tc.targetBigEndian {
		return tc.convertSequenceDefined(t, offset, vl)
	}
	return tc.convertSequenceUndefined(t, offset, vl)
}

// convertSequenceUndefined streams an SQ using undefined-length sequence and
// items, which both little endian target syntaxes allow.
func (tc *transcoder) convertSequenceUndefined(t tag.Tag, offset int64, vl uint32) *ConversionError {
	if err := tc.writeElementHeader(t, "SQ", tag.VLUndefinedLength); err != nil {
		return tc.failElement(err, t, offset)
	}

	var itemIndex int
	handleItem := func(it tag.Tag, itemOffset int64, itemVL uint32) *ConversionError {
		if it != tag.Item {
			return tc.failElement(fmt.Errorf("expected Item in sequence, got %s: %w",
				tag.DebugString(it), ErrorMalformedElement), it, itemOffset)
		}
		tc.path = append(tc.path, fmt.Sprintf("Item[%d]", itemIndex))
		defer tc.popPath()
		tc.scopes = append(tc.scopes, &pixelDescriptors{})
		defer func() { tc.scopes = tc.scopes[:len(tc.scopes)-1] }()

		if itemVL == tag.VLUndefinedLength {
			if err := tc.writeElementHeader(tag.Item, "NA", tag.VLUndefinedLength); err != nil {
				return tc.failElement(err, it, itemOffset)
			}
			if cerr := tc.convertItemContentUndefined(); cerr != nil {
				return cerr
			}
			if err := tc.writeElementHeader(tag.ItemDelimitationItem, "NA", 0); err != nil {
				return tc.failElement(err, tag.ItemDelimitationItem, itemOffset)
			}
			return nil
		}
		// Both little endian target syntaxes emit undefined-length items so
		// that nested SQ length changes (defined<->undefined) need no
		// recomputation.
		if err := tc.writeElementHeader(tag.Item, "NA", tag.VLUndefinedLength); err != nil {
			return tc.failElement(err, it, itemOffset)
		}
		if cerr := tc.convertItemContentBounded(itemVL); cerr != nil {
			return cerr
		}
		if err := tc.writeElementHeader(tag.ItemDelimitationItem, "NA", 0); err != nil {
			return tc.failElement(err, tag.ItemDelimitationItem, itemOffset)
		}
		return nil
	}

	if vl == tag.VLUndefinedLength {
		for {
			it, itemOffset, err := tc.readTagAt()
			if err != nil {
				return tc.failOffset(err, itemOffset, "reading sequence item tag")
			}
			_, itemVL, verr := tc.readImplicitVL()
			if verr != nil {
				return tc.failElement(verr, it, itemOffset)
			}
			if it == tag.SequenceDelimitationItem {
				if itemVL != 0 {
					return tc.failElement(fmt.Errorf("sequence delimitation item length %d is not 0: %w",
						itemVL, ErrorMalformedElement), it, itemOffset)
				}
				if err := tc.writeElementHeader(tag.SequenceDelimitationItem, "NA", 0); err != nil {
					return tc.failElement(err, it, itemOffset)
				}
				return nil
			}
			if cerr := handleItem(it, itemOffset, itemVL); cerr != nil {
				return cerr
			}
			itemIndex++
		}
	}

	if err := tc.rawReader.PushLimit(int64(vl)); err != nil {
		return tc.fail(err)
	}
	defer tc.rawReader.PopLimit()
	for !tc.rawReader.IsLimitExhausted() {
		it, itemOffset, err := tc.readTagAt()
		if err != nil {
			return tc.failOffset(err, itemOffset, "reading sequence item tag")
		}
		_, itemVL, verr := tc.readImplicitVL()
		if verr != nil {
			return tc.failElement(verr, it, itemOffset)
		}
		if it == tag.SequenceDelimitationItem {
			return tc.failElement(fmt.Errorf("unexpected sequence delimitation in defined-length sequence: %w",
				ErrorMalformedElement), it, itemOffset)
		}
		if cerr := handleItem(it, itemOffset, itemVL); cerr != nil {
			return cerr
		}
		itemIndex++
	}
	// The target SQ header was written with undefined length, so a defined
	// length source sequence still needs its Sequence Delimitation Item.
	if err := tc.writeElementHeader(tag.SequenceDelimitationItem, "NA", 0); err != nil {
		return tc.fail(err)
	}
	return nil
}

// convertSequenceDefined encodes an SQ for the Explicit VR Big Endian target,
// where sequences and items use defined lengths only. Each item is transcoded
// into a buffer so its defined length is known.
func (tc *transcoder) convertSequenceDefined(t tag.Tag, offset int64, vl uint32) *ConversionError {
	itemBuffers := [][]byte{}

	flushItem := func(itemIndex int, it tag.Tag, itemOffset int64, itemVL uint32) *ConversionError {
		if it != tag.Item {
			return tc.failElement(fmt.Errorf("expected Item in sequence, got %s: %w",
				tag.DebugString(it), ErrorMalformedElement), it, itemOffset)
		}
		buf := &bytes.Buffer{}
		saved := tc.writer
		tc.writer = dicomio.NewWriter(buf, tc.targetBO, tc.targetImp)
		tc.path = append(tc.path, fmt.Sprintf("Item[%d]", itemIndex))
		defer tc.popPath()
		tc.scopes = append(tc.scopes, &pixelDescriptors{})

		var cerr *ConversionError
		if itemVL == tag.VLUndefinedLength {
			cerr = tc.convertItemContentUndefined()
		} else {
			cerr = tc.convertItemContentBounded(itemVL)
		}
		tc.scopes = tc.scopes[:len(tc.scopes)-1]
		tc.writer = saved
		if cerr != nil {
			return cerr
		}
		itemBuffers = append(itemBuffers, buf.Bytes())
		return nil
	}

	itemIndex := 0
	if vl == tag.VLUndefinedLength {
		for {
			it, itemOffset, err := tc.readTagAt()
			if err != nil {
				return tc.failOffset(err, itemOffset, "reading sequence item tag")
			}
			_, itemVL, verr := tc.readImplicitVL()
			if verr != nil {
				return tc.failElement(verr, it, itemOffset)
			}
			if it == tag.SequenceDelimitationItem {
				if itemVL != 0 {
					return tc.failElement(fmt.Errorf("sequence delimitation item length %d is not 0: %w",
						itemVL, ErrorMalformedElement), it, itemOffset)
				}
				break
			}
			if cerr := flushItem(itemIndex, it, itemOffset, itemVL); cerr != nil {
				return cerr
			}
			itemIndex++
		}
	} else {
		if err := tc.rawReader.PushLimit(int64(vl)); err != nil {
			return tc.fail(err)
		}
		for !tc.rawReader.IsLimitExhausted() {
			it, itemOffset, err := tc.readTagAt()
			if err != nil {
				tc.rawReader.PopLimit()
				return tc.failOffset(err, itemOffset, "reading sequence item tag")
			}
			_, itemVL, verr := tc.readImplicitVL()
			if verr != nil {
				tc.rawReader.PopLimit()
				return tc.failElement(verr, it, itemOffset)
			}
			if it == tag.SequenceDelimitationItem {
				tc.rawReader.PopLimit()
				return tc.failElement(fmt.Errorf("unexpected sequence delimitation in defined-length sequence: %w",
					ErrorMalformedElement), it, itemOffset)
			}
			if cerr := flushItem(itemIndex, it, itemOffset, itemVL); cerr != nil {
				tc.rawReader.PopLimit()
				return cerr
			}
			itemIndex++
		}
		tc.rawReader.PopLimit()
	}

	var total int
	for _, b := range itemBuffers {
		total += len(b) + 8 // item tag (4) + item VL (4)
	}
	if err := tc.writeElementHeader(t, "SQ", uint32(total)); err != nil {
		return tc.failElement(err, t, offset)
	}
	for _, b := range itemBuffers {
		if err := tc.writeElementHeader(tag.Item, "NA", uint32(len(b))); err != nil {
			return tc.fail(err)
		}
		if err := tc.writer.WriteBytes(b); err != nil {
			return tc.fail(err)
		}
	}
	return nil
}

// convertItemContentUndefined transcodes elements of an undefined-length item
// until the Item Delimitation Item. The delimiter itself is consumed here but
// not written (callers decide on the target item encoding).
func (tc *transcoder) convertItemContentUndefined() *ConversionError {
	for {
		el, elOffset, err := tc.readTagAt()
		if err != nil {
			return tc.failOffset(err, elOffset, "reading item element tag")
		}
		if el == tag.ItemDelimitationItem {
			_, delimVL, verr := tc.readImplicitVL()
			if verr != nil {
				return tc.failElement(verr, el, elOffset)
			}
			if delimVL != 0 {
				return tc.failElement(fmt.Errorf("item delimitation length %d is not 0: %w",
					delimVL, ErrorMalformedElement), el, elOffset)
			}
			return nil
		}
		if cerr := tc.convertElement(el, elOffset); cerr != nil {
			return cerr
		}
	}
}

// convertItemContentBounded transcodes the bounded contents of a defined-length
// item.
func (tc *transcoder) convertItemContentBounded(itemVL uint32) *ConversionError {
	if err := tc.rawReader.PushLimit(int64(itemVL)); err != nil {
		return tc.fail(err)
	}
	defer tc.rawReader.PopLimit()
	for !tc.rawReader.IsLimitExhausted() {
		el, elOffset, err := tc.readTagAt()
		if err != nil {
			return tc.failOffset(err, elOffset, "reading item element tag")
		}
		if el == tag.ItemDelimitationItem {
			return tc.failElement(fmt.Errorf("unexpected item delimitation in defined-length item: %w",
				ErrorMalformedElement), el, elOffset)
		}
		if cerr := tc.convertElement(el, elOffset); cerr != nil {
			return cerr
		}
	}
	return nil
}
