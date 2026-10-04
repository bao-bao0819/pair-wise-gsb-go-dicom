package dicom

import (
	"bytes"
	"encoding/binary"

	"github.com/suyashkumar/dicom/pkg/private"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// This file holds tiny hand-encoded DICOM builders used by the private tag
// tests. They produce valid DICOM byte streams without depending on the
// library's writer, so the parse behavior under test is isolated.

// privateBuilder accumulates little-endian DICOM bytes.
type privateBuilder struct{ bytes.Buffer }

func (b *privateBuilder) u16(v uint16) { _ = binary.Write(b, binary.LittleEndian, v) }
func (b *privateBuilder) u32(v uint32) { _ = binary.Write(b, binary.LittleEndian, v) }

func (b *privateBuilder) tagPair(t tag.Tag) {
	b.u16(t.Group)
	b.u16(t.Element)
}

// padToEven appends a single space pad for an odd-length value field.
func (b *privateBuilder) padToEven(n int) {
	if n%2 == 1 {
		b.WriteByte(' ')
	}
}

// meta writes a minimal file meta header, always explicit-VR little-endian.
// tsUID selects the transfer syntax used for the body that follows.
func (b *privateBuilder) meta(tsUID string) {
	b.Write(make([]byte, 128))
	b.WriteString("DICM")

	// File Meta Information Group Length (0002,0000): computed over the
	// remaining group-2 elements.
	meta := &privateBuilder{}
	meta.ui(tag.MediaStorageSOPClassUID, "1.2.840.10008.5.1.4.1.1.2")
	meta.ui(tag.MediaStorageSOPInstanceUID, "1.2.3.4.5.6.7")
	meta.ui(tag.TransferSyntaxUID, tsUID)

	b.tagPair(tag.FileMetaInformationGroupLength)
	b.WriteString("UL")
	b.u16(uint16(4))
	b.u32(uint32(meta.Len()))
	b.Write(meta.Bytes())
}

func (b *privateBuilder) ui(t tag.Tag, value string) {
	b.tagPair(t)
	b.WriteString("UI")
	// UI pads odd lengths with NUL.
	padded := value
	if len(value)%2 == 1 {
		padded += "\x00"
	}
	b.u16(uint16(len(padded)))
	b.WriteString(padded)
}

// explicitElem writes a data element in explicit-VR LE. Only the VRs used in
// tests are handled.
func (b *privateBuilder) explicitElem(t tag.Tag, vr string, value []byte) {
	b.tagPair(t)
	b.WriteString(vr)
	switch vr {
	case "US", "SS", "LO", "UI", "CS", "DS", "IS":
		b.u16(uint16(len(value)))
	default: // OB, OW, UN, SQ and friends: reserved bytes + 32-bit VL
		b.u16(0)
		b.u32(uint32(len(value)))
	}
	b.Write(value)
}

// implicitElem writes a data element in implicit-VR LE.
func (b *privateBuilder) implicitElem(t tag.Tag, value []byte) {
	b.tagPair(t)
	b.u32(uint32(len(value)))
	b.Write(value)
}

// beginItem writes an undefined-length Item start.
func (b *privateBuilder) beginItem() {
	b.tagPair(tag.Item)
	b.u32(tag.VLUndefinedLength)
}

func (b *privateBuilder) endItem() {
	b.tagPair(tag.ItemDelimitationItem)
	b.u32(0)
}

func (b *privateBuilder) endSequence() {
	b.tagPair(tag.SequenceDelimitationItem)
	b.u32(0)
}

// loBytes returns an even-padded LO value.
func loBytes(s string) []byte {
	if len(s)%2 == 1 {
		s += " "
	}
	return []byte(s)
}

// usBytes returns the little-endian bytes of a single US value.
func usBytes(v uint16) []byte {
	var buf bytes.Buffer
	_ = binary.Write(&buf, binary.LittleEndian, v)
	return buf.Bytes()
}

// sequenceTag is the standard SQ tag reused in tests to carry private blocks.
var sequenceTag = tag.AnatomicRegionsInStudyCodeSequence

// explicitSQHeader / implicitSQHeader start an undefined-length sequence.
func (b *privateBuilder) explicitSQHeader(t tag.Tag) {
	b.tagPair(t)
	b.WriteString("SQ")
	b.u16(0)
	b.u32(tag.VLUndefinedLength)
}

func (b *privateBuilder) implicitSQHeader(t tag.Tag) {
	b.tagPair(t)
	b.u32(tag.VLUndefinedLength)
}

const (
	testCreatorAcme    = "ACME CORP"
	testCreatorOther   = "OTHERINC"
	testCreatorMystery = "MYSTERY"

	testPrivateGroup uint16 = 0x0009
)

// Private Creator slots.
// Data elements using each block.
var (
	creatorAcmeTag    = tag.Tag{Group: testPrivateGroup, Element: 0x0010}
	creatorOtherTag   = tag.Tag{Group: testPrivateGroup, Element: 0x0011}
	creatorMysteryTag = tag.Tag{Group: testPrivateGroup, Element: 0x0012}

	acmeDataTag    = tag.Tag{Group: testPrivateGroup, Element: 0x1010} // block 0x10, offset 0x10
	otherDataTag   = tag.Tag{Group: testPrivateGroup, Element: 0x1120} // block 0x11, offset 0x20
	mysteryDataTag = tag.Tag{Group: testPrivateGroup, Element: 0x1230} // block 0x12, offset 0x30
)

// testPrivateDictionary defines the two known vendors. The mystery creator is
// intentionally absent to exercise raw fallback.
func testPrivateDictionary() *private.Dictionary {
	return private.NewDictionary(map[private.Key]private.Definition{
		{Creator: testCreatorAcme, Group: testPrivateGroup, Offset: 0x10}: {
			VR: "US", Name: "ACME Block Thing", Keyword: "AcmeBlockThing",
		},
		{Creator: testCreatorOther, Group: testPrivateGroup, Offset: 0x20}: {
			VR: "LO", Name: "Other Block Label", Keyword: "OtherBlockLabel",
		},
	})
}

var _ = uid.ImplicitVRLittleEndian
