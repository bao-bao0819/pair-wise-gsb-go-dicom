package dicom_test

import (
	"bytes"
	"encoding/binary"
	"math"
	"testing"

	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

var (
	itemTag                        = tag.Tag{Group: 0xFFFE, Element: 0xE000}
	itemDelimitationTag            = tag.Tag{Group: 0xFFFE, Element: 0xE00D}
	sequenceDelimitationTag        = tag.Tag{Group: 0xFFFE, Element: 0xE0DD}
	undefinedLength         uint32 = 0xFFFFFFFF
)

// longFormVRs mirrors the explicit VR encodings that use the 32-bit VL field.
var longFormVRs = map[string]bool{
	"OB": true, "OW": true, "SQ": true, "UN": true, "UT": true, "OF": true,
	"OL": true, "OD": true, "OV": true, "UC": true, "UR": true,
}

// rawBuilder emits DICOM bytes for a chosen source transfer syntax.
type rawBuilder struct {
	buf      bytes.Buffer
	bo       binary.ByteOrder
	explicit bool
}

func newRawBuilder(ts string) *rawBuilder {
	b := &rawBuilder{bo: binary.LittleEndian, explicit: ts != uid.ImplicitVRLittleEndian}
	if ts == uid.ExplicitVRBigEndian {
		b.bo = binary.BigEndian
	}
	return b
}

func (b *rawBuilder) u16(v uint16) {
	var d [2]byte
	b.bo.PutUint16(d[:], v)
	b.buf.Write(d[:])
}

func (b *rawBuilder) u32(v uint32) {
	var d [4]byte
	b.bo.PutUint32(d[:], v)
	b.buf.Write(d[:])
}

func (b *rawBuilder) theTag(t tag.Tag) {
	b.u16(t.Group)
	b.u16(t.Element)
}

// element writes a standard element with a raw (already even-padded) payload.
func (b *rawBuilder) element(t tag.Tag, vr string, value []byte) {
	b.theTag(t)
	vl := uint32(len(value))
	if b.explicit {
		b.buf.WriteString(vr)
		if longFormVRs[vr] {
			b.u16(0)
			b.u32(vl)
		} else {
			b.u16(uint16(vl))
		}
	} else {
		b.u32(vl)
	}
	b.buf.Write(value)
}

func (b *rawBuilder) u16Values(t tag.Tag, vr string, vals ...uint16) {
	data := make([]byte, 2*len(vals))
	for i, v := range vals {
		b.bo.PutUint16(data[2*i:], v)
	}
	b.element(t, vr, data)
}

func (b *rawBuilder) i16Values(t tag.Tag, vr string, vals ...int16) {
	uv := make([]uint16, len(vals))
	for i, v := range vals {
		uv[i] = uint16(v)
	}
	b.u16Values(t, vr, uv...)
}

func (b *rawBuilder) u32Values(t tag.Tag, vr string, vals ...uint32) {
	data := make([]byte, 4*len(vals))
	for i, v := range vals {
		b.bo.PutUint32(data[4*i:], v)
	}
	b.element(t, vr, data)
}

func (b *rawBuilder) text(t tag.Tag, vr, s string) {
	value := []byte(s)
	if len(value)%2 == 1 {
		if vr == "UI" {
			value = append(value, 0)
		} else {
			value = append(value, ' ')
		}
	}
	b.element(t, vr, value)
}

func (b *rawBuilder) beginUndefinedSequence(t tag.Tag) {
	b.theTag(t)
	if b.explicit {
		b.buf.WriteString("SQ")
		b.u16(0)
	}
	b.u32(undefinedLength)
}

func (b *rawBuilder) beginUndefinedItem() {
	b.theTag(itemTag)
	b.u32(undefinedLength)
}

func (b *rawBuilder) endUndefinedItem() {
	b.theTag(itemDelimitationTag)
	b.u32(0)
}

func (b *rawBuilder) endUndefinedSequence() {
	b.theTag(sequenceDelimitationTag)
	b.u32(0)
}

// withExplicitLE runs fn with the builder temporarily in Explicit VR LE, which
// is how file meta information is always encoded.
func (b *rawBuilder) withExplicitLE(fn func()) {
	savedBO, savedExp := b.bo, b.explicit
	b.bo, b.explicit = binary.LittleEndian, true
	fn()
	b.bo, b.explicit = savedBO, savedExp
}

// buildSynthetic assembles a small uncompressed DICOM in the given source
// transfer syntax. It exercises odd-length text/UI padding, 2/4/8-byte numeric
// and binary VRs, 16-bit signed native pixels, an OB stream, and a nested
// undefined-length sequence (an undefined SQ containing an undefined SQ).
func buildSynthetic(t *testing.T, ts string) []byte {
	t.Helper()
	b := newRawBuilder(ts)

	b.buf.Write(make([]byte, 128))
	b.buf.WriteString("DICM")

	// File meta: build group-2 payload first to compute (0002,0000).
	mb := newRawBuilder(uid.ExplicitVRLittleEndian)
	mb.text(tag.MediaStorageSOPClassUID, "UI", "1.2.840.10008.5.1.4.1.1.2")
	mb.text(tag.MediaStorageSOPInstanceUID, "UI", "1.2.3.4.5.6.7.8.9.0.1") // odd UI
	mb.text(tag.TransferSyntaxUID, "UI", ts)
	mb.text(tag.ImplementationVersionName, "SH", "GO_DCM")
	metaBytes := mb.buf.Bytes()

	b.withExplicitLE(func() {
		b.element(tag.FileMetaInformationGroupLength, "UL", leU32(uint32(len(metaBytes))))
	})
	b.buf.Write(metaBytes)

	// Body: odd-length text/UI padding.
	b.text(tag.SpecificCharacterSet, "CS", "ISO_IR 100") // len 10, even
	b.text(tag.Manufacturer, "LO", "ACME")               // len 4
	b.text(tag.StudyDescription, "LO", "Odd")            // len 3, space pad
	b.text(tag.SOPInstanceUID, "UI", "1.2.3")            // len 5, NUL pad
	b.text(tag.PatientName, "PN", "Doe^J")               // len 5
	b.text(tag.PatientID, "LO", "X")                     // len 1

	// Pixel descriptors, including signed PixelRepresentation.
	b.u16Values(tag.SamplesPerPixel, "US", 1)
	b.u16Values(tag.PixelRepresentation, "US", 1)
	b.u16Values(tag.Rows, "US", 2)
	b.u16Values(tag.Columns, "US", 3)
	b.u16Values(tag.BitsAllocated, "US", 16)
	b.u16Values(tag.BitsStored, "US", 16)
	b.u16Values(tag.HighBit, "US", 15)

	// 2-byte numeric VRs plus AT tags. PixelPaddingValue's first dictionary VR
	// is US; keep it positive so implicit/explicit agree.
	b.u16Values(tag.PixelPaddingValue, "US", 100)
	b.u16Values(tag.DataElement, "AT", 0x0010, 0x0010)
	b.u16Values(tag.FrameIncrementPointer, "AT", 0x0028, 0x0010)
	// A purely-SS tag carrying a negative value.
	b.i16Values(tag.TagAngleSecondAxis, "SS", -1234)
	// A purely-UL 4-byte element.
	b.u32Values(tag.TriggerSamplePosition, "UL", 42)

	// FL (4) and FD (8).
	var f1 [4]byte
	b.bo.PutUint32(f1[:], math.Float32bits(1.0))
	var f2 [8]byte
	b.bo.PutUint64(f2[:], math.Float64bits(2.5))
	b.element(tag.FloatPixelPaddingValue, "FL", f1[:])
	b.element(tag.DoubleFloatPixelPaddingValue, "FD", f2[:])

	// An OB byte stream with a known payload for verbatim comparison.
	b.element(tag.MakerNote, "OB", []byte{0x01, 0x02, 0x03, 0x04})

	// Nested undefined-length sequences.
	b.beginUndefinedSequence(tag.ReferencedStudySequence)
	b.beginUndefinedItem()
	b.text(tag.ReferencedSOPClassUID, "UI", "1.2.840.10008.5.1.4.1.1.2")
	b.beginUndefinedSequence(tag.ReferencedSeriesSequence)
	b.beginUndefinedItem()
	b.text(tag.ReferencedSOPInstanceUID, "UI", "9.9.9.9.9.9.9.9")
	b.u16Values(tag.FunctionalGroupPointer, "AT", 0x0028, 0x0010)
	b.text(tag.CodeMeaning, "LO", "Nest")
	b.endUndefinedItem()
	b.endUndefinedSequence()
	b.endUndefinedItem()
	b.endUndefinedSequence()

	// Native pixel data: 2x3 16-bit signed samples, negatives included.
	b.i16Values(tag.PixelData, "OW", 0, 1, -1, 32767, -32768, 100)

	return b.buf.Bytes()
}

func leU32(v uint32) []byte {
	var d [4]byte
	binary.LittleEndian.PutUint32(d[:], v)
	return d[:]
}
