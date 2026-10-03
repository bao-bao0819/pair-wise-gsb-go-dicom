package dicom

import (
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"strings"

	"github.com/suyashkumar/dicom/pkg/dicomio"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// discardBytes reads and drops n bytes from the source.
func (tc *transcoder) discardBytes(n int64) error {
	_, err := io.CopyN(io.Discard, tc.rawReader, n)
	return err
}

// copyValueBytes streams vl value bytes to the output, swapping the bytes
// within each unit-byte word when the source and target byte orders differ.
func (tc *transcoder) copyValueBytes(vl uint32, unit int) error {
	if unit <= 1 || tc.sourceBO == tc.targetBO {
		return copyN(tc.writer, tc.rawReader, int64(vl), 1, false)
	}
	return copyN(tc.writer, tc.rawReader, int64(vl), unit, true)
}

// writeBufferedValue transcribes an already-read value buffer to the target
// byte order using the same word rules as copyValueBytes.
func (tc *transcoder) writeBufferedValue(data []byte, unit int) error {
	if unit <= 1 || tc.sourceBO == tc.targetBO {
		return tc.writer.WriteBytes(data)
	}
	out := make([]byte, len(data))
	for i := 0; i < len(data); i += unit {
		for j := 0; j < unit; j++ {
			out[i+j] = data[i+unit-1-j]
		}
	}
	return tc.writer.WriteBytes(out)
}

// copyN copies n bytes from r to w in fixed chunks. When swap is true, bytes
// within each unit-sized word are reversed on the way out (endian conversion).
func copyN(w *dicomio.Writer, r *dicomio.Reader, n int64, unit int, swap bool) error {
	const chunkWords = 1 << 14
	buf := make([]byte, chunkWords*unit)
	out := make([]byte, chunkWords*unit)
	remaining := n
	for remaining > 0 {
		take := int64(len(buf))
		if remaining < take {
			take = remaining
		}
		if _, err := io.ReadFull(r, buf[:take]); err != nil {
			return err
		}
		if swap {
			if take%int64(unit) != 0 {
				return fmt.Errorf("internal: swapped copy of %d bytes not a multiple of unit %d", take, unit)
			}
			for i := int64(0); i < take; i += int64(unit) {
				for j := 0; j < unit; j++ {
					out[i+int64(j)] = buf[i+int64(unit-1-j)]
				}
			}
		}
		var toWrite []byte
		if swap {
			toWrite = out[:take]
		} else {
			toWrite = buf[:take]
		}
		if err := w.WriteBytes(toWrite); err != nil {
			return err
		}
		remaining -= take
	}
	return nil
}

// copyUndefinedBytes transcodes an OB/OW/UN value of undefined length: a
// sequence of Items terminated by a Sequence Delimitation Item. OW items have
// their words byte-swapped; OB/UN item payloads are byte-order independent and
// are copied verbatim (UN contents are unknown octets and must never be
// reinterpreted).
func (tc *transcoder) copyUndefinedBytes(vr string) error {
	unit := vrValueUnit[vr]
	for {
		it, _, err := tc.readTagAt()
		if err != nil {
			return err
		}
		_, itemVL, err := tc.readImplicitVL()
		if err != nil {
			return err
		}
		switch it {
		case tag.SequenceDelimitationItem:
			if itemVL != 0 {
				return fmt.Errorf("sequence delimitation item length %d is not 0: %w", itemVL, ErrorMalformedElement)
			}
			return tc.writeElementHeader(tag.SequenceDelimitationItem, "NA", 0)
		case tag.Item:
			if itemVL == tag.VLUndefinedLength {
				return fmt.Errorf("nested undefined-length item inside %s byte stream: %w", vr, ErrorMalformedElement)
			}
			if unit > 1 && itemVL%2 != 0 {
				return ErrorOWRequiresEvenVL
			}
			if err := tc.writeElementHeader(tag.Item, "NA", itemVL); err != nil {
				return err
			}
			if err := tc.copyValueBytes(itemVL, unit); err != nil {
				return err
			}
		default:
			return fmt.Errorf("expected Item or Sequence Delimitation in %s byte stream, got %s: %w",
				vr, tag.DebugString(it), ErrorMalformedElement)
		}
	}
}

// readSimpleValue fully parses a value into the in-memory Value representation
// using the byte order currently set on tc.rawReader (Explicit VR Little
// Endian while reading file meta; the source body encoding afterwards).
func (tc *transcoder) readSimpleValue(t tag.Tag, vr string, vl uint32) (Value, error) {
	if vl == tag.VLUndefinedLength {
		return nil, fmt.Errorf("%s undefined length not expected here: %w", tag.DebugString(t), ErrorMalformedElement)
	}
	data := make([]byte, vl)
	if _, err := io.ReadFull(tc.rawReader, data); err != nil {
		return nil, err
	}
	switch vr {
	case "OB", "UN":
		return &bytesValue{value: data}, nil
	case "OW":
		if vl%2 != 0 {
			return nil, ErrorOWRequiresEvenVL
		}
		return &bytesValue{value: data}, nil
	}
	switch vrValueUnit[vr] {
	case 2, 4, 8:
		return tc.parseNumericValue(t, vr, data, tc.rawReader.ByteOrder())
	default:
		str := string(data)
		str = strings.Trim(str, " \000")
		return &stringsValue{value: strings.Split(str, "\\")}, nil
	}
}

// parseNumericValue parses a fully buffered numeric/AT value according to vr,
// honoring the source byte order.
func (tc *transcoder) parseNumericValue(t tag.Tag, vr string, data []byte, bo binary.ByteOrder) (Value, error) {
	ints := []int{}
	floats := []float64{}
	for off := 0; off < len(data); {
		switch vr {
		case "US", "AT":
			ints = append(ints, int(bo.Uint16(data[off:off+2])))
			off += 2
		case "SS":
			ints = append(ints, int(int16(bo.Uint16(data[off:off+2]))))
			off += 2
		case "UL", "SL":
			v := bo.Uint32(data[off : off+4])
			if vr == "SL" {
				ints = append(ints, int(int32(v)))
			} else {
				ints = append(ints, int(v))
			}
			off += 4
		case "FL":
			floats = append(floats, float64(math.Float32frombits(bo.Uint32(data[off:off+4]))))
			off += 4
		case "FD":
			floats = append(floats, math.Float64frombits(bo.Uint64(data[off:off+8])))
			off += 8
		case "SV", "UV", "OV":
			// These 64-bit VRs are streamed byte-swapped in the body; only very
			// small synthetic meta-style values would reach here, so reject
			// rather than narrow into an int.
			return nil, fmt.Errorf("VR=%s in file meta is unsupported: %w", vr, ErrorUnsupportedVR)
		case "OF", "OL", "OD":
			return nil, fmt.Errorf("VR=%s in file meta is unsupported: %w", vr, ErrorUnsupportedVR)
		default:
			return nil, fmt.Errorf("unexpected numeric VR=%s for %s: %w", vr, tag.DebugString(t), ErrorUnsupportedVR)
		}
	}
	if len(floats) > 0 {
		return &floatsValue{value: floats}, nil
	}
	return &intsValue{value: ints}, nil
}
