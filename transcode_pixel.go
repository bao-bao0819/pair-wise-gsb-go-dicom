package dicom

import (
	"encoding/binary"
	"fmt"
	"strconv"
	"strings"

	"github.com/suyashkumar/dicom/pkg/tag"
)

// pixelDescriptors holds already-parsed elements in one dataset scope that are
// needed to interpret a native PixelData value. Each sequence item gets a fresh
// scope, since an item may carry its own icon image.
type pixelDescriptors struct {
	rows            int
	cols            int
	samplesPerPixel int
	bitsAllocated   int
	pixelRepresent  int
	numberOfFrames  int
	haveRows        bool
	haveCols        bool
	haveSamples     bool
	haveBits        bool
	havePixelRep    bool
	haveFrames      bool
}

func (d *pixelDescriptors) set(t tag.Tag, n int) {
	switch t {
	case tag.Rows:
		d.rows, d.haveRows = n, true
	case tag.Columns:
		d.cols, d.haveCols = n, true
	case tag.SamplesPerPixel:
		d.samplesPerPixel, d.haveSamples = n, true
	case tag.BitsAllocated:
		d.bitsAllocated, d.haveBits = n, true
	case tag.PixelRepresentation:
		d.pixelRepresent, d.havePixelRep = n, true
	case tag.NumberOfFrames:
		d.numberOfFrames, d.haveFrames = n, true
	}
}

// isPixelDescriptor reports whether t carries information needed to interpret
// a native PixelData value in the same scope.
func isPixelDescriptor(t tag.Tag) bool {
	switch t {
	case tag.Rows, tag.Columns, tag.SamplesPerPixel, tag.BitsAllocated,
		tag.PixelRepresentation, tag.NumberOfFrames:
		return true
	}
	return false
}

// parseDescriptor parses the already-buffered value of a descriptor element.
// US descriptors are a single 16-bit integer; NumberOfFrames is IS text.
func parseDescriptor(t tag.Tag, vr string, data []byte, bo binary.ByteOrder) (int, error) {
	if t == tag.NumberOfFrames {
		s := strings.Trim(string(data), " \000")
		n, err := strconv.Atoi(s)
		if err != nil {
			return 0, err
		}
		return n, nil
	}
	if len(data) < 2 {
		return 0, fmt.Errorf("descriptor %s value too short", tag.DebugString(t))
	}
	return int(bo.Uint16(data[:2])), nil
}

// convertPixelData handles (7FE0,0010). Encapsulated (undefined length) data
// is explicitly rejected. Native data has only the byte order swap implied by
// BitsAllocated applied; pixel values (including signed PixelRepresentation=1
// samples) are never otherwise altered.
func (tc *transcoder) convertPixelData(t tag.Tag, offset int64, vr string, vl uint32) *ConversionError {
	tc.path = append(tc.path, tag.DebugString(t))
	defer tc.popPath()

	if vl == tag.VLUndefinedLength {
		return tc.failElement(ErrorEncapsulatedPixelData, t, offset)
	}

	d := tc.currentScope()
	if !d.haveBits {
		return tc.failElement(fmt.Errorf("PixelData without BitsAllocated descriptor: %w", ErrorMalformedElement), t, offset)
	}
	bits := d.bitsAllocated
	// Respect the existing meaning of PixelRepresentation: it is recorded but
	// never reinterpreted. Validate only its coding.
	if d.havePixelRep && d.pixelRepresent != 0 && d.pixelRepresent != 1 {
		return tc.failElement(fmt.Errorf("PixelRepresentation=%d is not 0 or 1: %w",
			d.pixelRepresent, ErrorMalformedElement), t, offset)
	}

	var unit int
	switch bits {
	case 1, 8:
		unit = 1
	case 16:
		unit = 2
	case 32:
		unit = 4
	default:
		return tc.failElement(fmt.Errorf("BitsAllocated=%d: %w", bits, ErrorUnsupportedBitsAllocatedForConversion), t, offset)
	}

	if vl%2 != 0 {
		return tc.failElement(fmt.Errorf("native PixelData has odd value length %d: %w", vl, ErrorMalformedElement), t, offset)
	}

	// When geometry is known, verify the declared length matches the native
	// frame layout (one trailing padding byte is legal for 1-bit data).
	if d.haveRows && d.haveCols && d.haveSamples {
		frames := 1
		if d.haveFrames {
			frames = d.numberOfFrames
		}
		samples := d.rows * d.cols * d.samplesPerPixel * frames
		var expected uint32
		if bits == 1 {
			expected = uint32(samples / 8)
		} else {
			expected = uint32(samples * bits / 8)
		}
		if expected != vl && expected != vl-1 {
			return tc.failElement(fmt.Errorf("native PixelData VL=%d does not match expected %d bytes from Rows/Columns/SamplesPerPixel/BitsAllocated: %w",
				vl, expected, ErrorMismatchPixelDataLength), t, offset)
		}
	}

	if err := tc.writeElementHeader(t, vr, vl); err != nil {
		return tc.failElement(err, t, offset)
	}
	if err := tc.copyValueBytes(vl, unit); err != nil {
		return tc.failElement(err, t, offset)
	}
	return nil
}
