package dicom

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/suyashkumar/dicom/pkg/dicomio"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// Sentinel errors returned by ConvertTransferSyntax. They describe why an
// uncompressed DICOM could not be transcoded. Use errors.Is to check for them;
// every failure is additionally wrapped in a *ConversionError that reports the
// offending element path and its absolute byte offset in the original input.
var (
	// ErrorUnsupportedTransferSyntax indicates the input (or requested output)
	// transfer syntax is not one of the three supported uncompressed transfer
	// syntaxes (Explicit VR Little Endian, Implicit VR Little Endian,
	// Explicit VR Big Endian). Encapsulated (e.g. JPEG) and deflated transfer
	// syntaxes are rejected; pixels are never decompressed.
	ErrorUnsupportedTransferSyntax = errors.New("unsupported or unknown transfer syntax")
	// ErrorEncapsulatedPixelData indicates the PixelData element uses
	// encapsulation (undefined length / item fragments). Encapsulated pixel
	// data is never transcoded.
	ErrorEncapsulatedPixelData = errors.New("encapsulated pixel data is not supported")
	// ErrorUndeterminableVR indicates an element was encountered whose Value
	// Representation could not be determined: a private (or unknown) element
	// under an implicit VR transfer syntax with no dictionary VR. Such elements
	// are rejected rather than silently copied as OB/UN.
	ErrorUndeterminableVR = errors.New("unable to determine VR for element")
	// ErrorMalformedElement indicates an element violates the structural rules
	// of its encoding (e.g. an odd value length for a VR that requires even
	// length, a stray delimiter, or a truncated value).
	ErrorMalformedElement = errors.New("malformed DICOM element")
	// ErrorUnsupportedBitsAllocatedForConversion indicates that native
	// PixelData declares a BitsAllocated whose sample byte width is not
	// supported by this transcoder (only 1, 8, 16 and 32 are handled).
	ErrorUnsupportedBitsAllocatedForConversion = errors.New("unsupported BitsAllocated for pixel byte order conversion")
)

// ConversionError wraps a transfer syntax conversion failure with the path of
// the offending element and its absolute byte offset in the original input
// stream. The offset points at the start of the offending element (tag) and
// includes the 128-byte preamble and DICM magic word.
type ConversionError struct {
	// Path is a human-readable path of nested sequence/item elements, in
	// outer-to-inner order joined by " > ". The offending element is the last
	// component.
	Path string
	// Offset is the absolute byte offset of the offending element's tag in the
	// original input, measured from byte 0 (so it includes the preamble).
	Offset int64
	// Err is the underlying error (typically one of the sentinel errors).
	Err error
}

func (e *ConversionError) Error() string {
	if e.Path == "" {
		return fmt.Sprintf("dicom transfer syntax conversion error at offset %d: %v", e.Offset, e.Err)
	}
	return fmt.Sprintf("dicom transfer syntax conversion error at element %s (input offset %d): %v", e.Path, e.Offset, e.Err)
}

func (e *ConversionError) Unwrap() error { return e.Err }

// ConvertTransferSyntax transcodes an uncompressed DICOM from the transfer
// syntax declared in its file meta information into targetTransferSyntaxUID,
// writing the result as a new DICOM file stream to out.
//
// targetTransferSyntaxUID must be one of:

//   - uid.ImplicitVRLittleEndian (1.2.840.10008.1.2)
//   - uid.ExplicitVRLittleEndian (1.2.840.10008.1.2.1)
//   - uid.ExplicitVRBigEndian    (1.2.840.10008.1.2.2)
//
// The input must be a DICOM Part 10 file (128-byte preamble + "DICM") whose
// meta information names one of those same three transfer syntaxes. Elements
// are re-encoded for the target syntax: multi-byte numeric VRs and word-based
// binary VRs (OW/OF/OL/OD/OV/AT) have their byte order swapped as needed;
// sequences and items are rewritten with length encodings legal for the
// target syntax (Explicit VR Big Endian always uses defined item lengths, per
// PS3.5 Annex A.3); file meta information is rewritten in Explicit VR Little
// Endian and its (0002,0000) group length and TransferSyntaxUID are updated.
//
// Pixels are never decompressed and pixel values are never reinterpreted:
// native PixelData only undergoes the byte-order swap implied by the declared
// BitsAllocated (no swap for 1/8-bit samples), and PixelRepresentation keeps
// its existing meaning (signed samples round-trip identically at the bit
// level). Encapsulated PixelData, unknown transfer syntaxes, and elements with
// undeterminable VRs cause an explicit error.
//
// Conversion is streaming: elements are written to out as they are consumed.
// If an error occurs after output has started, out may already contain a
// prefix of a well-formed-but-truncated DICOM; a non-nil error always means
// the result is incomplete and callers must discard out.
func ConvertTransferSyntax(in io.Reader, out io.Writer, targetTransferSyntaxUID string) error {
	if err := validateSupportedTransferSyntax(targetTransferSyntaxUID); err != nil {
		return err
	}
	targetBO, targetImplicit := transferSyntaxEncoding(targetTransferSyntaxUID)

	tc := &transcoder{
		rawReader:       dicomio.NewReader(bufio.NewReader(in), binary.LittleEndian, dicomio.LimitReadUntilEOF),
		out:             out,
		targetBO:        targetBO,
		targetImp:       targetImplicit,
		targetBigEndian: targetTransferSyntaxUID == uid.ExplicitVRBigEndian,
		path:            make([]string, 0, 8),
		scopes:          []*pixelDescriptors{{}},
	}

	metaElems, sourceTS, err := tc.readFileMeta()
	if err != nil {
		return err
	}
	if err := validateSupportedTransferSyntax(sourceTS); err != nil {
		return tc.failf(err, "source TransferSyntaxUID %q", sourceTS)
	}
	sourceBO, sourceImplicit := transferSyntaxEncoding(sourceTS)
	tc.sourceBO = sourceBO
	tc.sourceImp = sourceImplicit
	tc.rawReader.SetTransferSyntax(sourceBO, sourceImplicit)

	// Rewrite file meta information. Meta is always Explicit VR Little Endian;
	// the TransferSyntaxUID element is changed to the target.
	if tsElem, findErr := metaElems.FindElementByTag(tag.TransferSyntaxUID); findErr == nil {
		tsElem.Value = mustNewValue([]string{targetTransferSyntaxUID})
		tsElem.RawValueRepresentation = uidVR
		tsElem.ValueRepresentation = tag.GetVRKind(tag.TransferSyntaxUID, uidVR)
		tsElem.ValueLength = 0
	}
	headerWriter := dicomio.NewWriter(out, binary.LittleEndian, false)
	if err := writeFileHeader(headerWriter, &metaElems, metaElems.Elements, writeOptSet{}); err != nil {
		return tc.failf(err, "writing file meta information")
	}

	tc.writer = dicomio.NewWriter(out, targetBO, targetImplicit)

	for {
		t, tagOffset, readErr := tc.readTagAt()
		if readErr != nil {
			if errors.Is(readErr, io.EOF) || errors.Is(readErr, io.ErrUnexpectedEOF) {
				return nil
			}
			return tc.failOffset(readErr, tagOffset, "reading element tag")
		}
		if err := tc.convertElement(t, tagOffset); err != nil {
			return err
		}
	}
}

// transcoder carries the mutable state for a single conversion pass.
type transcoder struct {
	rawReader *dicomio.Reader
	writer    *dicomio.Writer
	out       io.Writer

	sourceBO  binary.ByteOrder
	sourceImp bool
	targetBO  binary.ByteOrder
	targetImp bool
	// targetBigEndian records whether targetBO is binary.BigEndian.
	targetBigEndian bool

	// path is the nested element path (outer-first). It is mutated while
	// descending into sequences and restored afterwards.
	path []string

	// scopes is a stack of pixel descriptor sets, one per nesting scope. The
	// bottom entry describes the top-level dataset; each sequence item pushes
	// a fresh one (items may carry their own icon image PixelData).
	scopes []*pixelDescriptors
}

func (tc *transcoder) currentScope() *pixelDescriptors {
	return tc.scopes[len(tc.scopes)-1]
}

func (tc *transcoder) fail(err error) *ConversionError {
	return tc.failOffset(err, tc.rawReader.BytesRead(), "")
}

func (tc *transcoder) failf(err error, format string, args ...any) *ConversionError {
	msg := fmt.Sprintf(format, args...)
	if msg == "" {
		return tc.fail(err)
	}
	return tc.failOffset(fmt.Errorf("%s: %w", msg, err), tc.rawReader.BytesRead(), "")
}

// failElement reports err against the element identified by t at offset.
func (tc *transcoder) failElement(err error, t tag.Tag, offset int64) *ConversionError {
	return &ConversionError{Path: tc.elementPath(t), Offset: offset, Err: err}
}

// failOffset reports err at an explicit offset with an optional detail message.
func (tc *transcoder) failOffset(err error, offset int64, detail string) *ConversionError {
	path := strings.Join(tc.path, " > ")
	if detail != "" {
		err = fmt.Errorf("%s: %w", detail, err)
	}
	return &ConversionError{Path: path, Offset: offset, Err: err}
}

func (tc *transcoder) elementPath(t tag.Tag) string {
	seg := tag.DebugString(t)
	if len(tc.path) == 0 {
		return seg
	}
	return strings.Join(tc.path, " > ") + " > " + seg
}

// readTagAt reads the next (group, element) tag using the current source
// encoding and returns the absolute offset the tag started at.
func (tc *transcoder) readTagAt() (tag.Tag, int64, error) {
	offset := tc.rawReader.BytesRead()
	group, err := tc.rawReader.ReadUInt16()
	if err != nil {
		return tag.Tag{}, offset, err
	}
	element, err := tc.rawReader.ReadUInt16()
	if err != nil {
		return tag.Tag{}, offset, err
	}
	return tag.Tag{Group: group, Element: element}, offset, nil
}

func validateSupportedTransferSyntax(ts string) error {
	switch ts {
	case uid.ImplicitVRLittleEndian, uid.ExplicitVRLittleEndian, uid.ExplicitVRBigEndian:
		return nil
	default:
		return fmt.Errorf("%q: %w", ts, ErrorUnsupportedTransferSyntax)
	}
}

func transferSyntaxEncoding(ts string) (binary.ByteOrder, bool) {
	if ts == uid.ImplicitVRLittleEndian {
		return binary.LittleEndian, true
	}
	if ts == uid.ExplicitVRBigEndian {
		return binary.BigEndian, false
	}
	return binary.LittleEndian, false
}

const uidVR = "UI"

// readFileMeta consumes the 128-byte preamble and DICM magic word, then parses
// the group 2 file meta elements (always Explicit VR Little Endian).
func (tc *transcoder) readFileMeta() (Dataset, string, error) {
	preamble := make([]byte, 128)
	if _, err := io.ReadFull(tc.rawReader, preamble); err != nil {
		return Dataset{}, "", tc.failOffset(ErrorMalformedElement, 0, "missing 128-byte DICOM preamble")
	}
	magic := make([]byte, 4)
	if _, err := io.ReadFull(tc.rawReader, magic); err != nil {
		return Dataset{}, "", tc.failf(ErrorMalformedElement, "missing DICM magic word")
	}
	if string(magic) != magicWord {
		return Dataset{}, "", tc.failOffset(
			fmt.Errorf("expected %q magic word: %w", magicWord, ErrorMalformedElement), 128, "")
	}

	// File meta information is always encoded Explicit VR Little Endian.
	tc.rawReader.SetTransferSyntax(binary.LittleEndian, false)

	metaElems := []*Element{}
	var metaLen int64 = -1

	first, firstOffset, err := tc.readTagAt()
	if err != nil {
		return Dataset{}, "", tc.failOffset(ErrorMalformedElement, tc.rawReader.BytesRead(), "file meta information is empty")
	}
	if first == tag.FileMetaInformationGroupLength {
		vr, vl, verr := tc.readExplicitVRVL(first)
		if verr != nil {
			return Dataset{}, "", tc.failElement(verr, first, firstOffset)
		}
		val, verr := tc.readSimpleValue(first, vr, vl)
		if verr != nil {
			return Dataset{}, "", tc.failElement(verr, first, firstOffset)
		}
		ints := MustGetInts(val)
		if len(ints) != 1 {
			return Dataset{}, "", tc.failElement(ErrorMalformedElement, first, firstOffset)
		}
		metaElems = append(metaElems, tc.newElement(first, vr, vl, val))
		metaLen = int64(ints[0])
	} else {
		// No (0002,0000) element: tolerate by scanning group 2 until the first
		// non-group-2 tag, mirroring the parser's missing group-length mode.
		if first.Group != tag.MetadataGroup {
			return Dataset{}, "", tc.failElement(
				fmt.Errorf("file meta information must start at group 2, got %s: %w", first.String(), ErrorMalformedElement),
				first, firstOffset)
		}
		elem, cerr := tc.readWholeElement(first, firstOffset)
		if cerr != nil {
			return Dataset{}, "", cerr
		}
		metaElems = append(metaElems, elem)
	}

	if metaLen >= 0 {
		if err := tc.rawReader.PushLimit(metaLen); err != nil {
			return Dataset{}, "", tc.fail(err)
		}
	}

	for {
		if metaLen >= 0 {
			if tc.rawReader.IsLimitExhausted() {
				break
			}
		} else {
			peek, perr := tc.rawReader.Peek(2)
			if perr != nil {
				return Dataset{}, "", tc.failf(ErrorMalformedElement, "scanning file meta group")
			}
			if binary.LittleEndian.Uint16(peek) != tag.MetadataGroup {
				break
			}
		}
		t, off, err := tc.readTagAt()
		if err != nil {
			return Dataset{}, "", tc.failOffset(err, off, "reading file meta element tag")
		}
		elem, cerr := tc.readWholeElement(t, off)
		if cerr != nil {
			return Dataset{}, "", cerr
		}
		metaElems = append(metaElems, elem)
	}

	if metaLen >= 0 {
		tc.rawReader.PopLimit()
	}

	meta := Dataset{Elements: metaElems}
	tsElem, err := meta.FindElementByTag(tag.TransferSyntaxUID)
	if err != nil {
		return Dataset{}, "", tc.failf(ErrorUnsupportedTransferSyntax, "file meta information has no TransferSyntaxUID")
	}
	tsStrs := MustGetStrings(tsElem.Value)
	if len(tsStrs) != 1 {
		return Dataset{}, "", tc.failf(ErrorUnsupportedTransferSyntax, "malformed TransferSyntaxUID")
	}
	return meta, tsStrs[0], nil
}

func (tc *transcoder) newElement(t tag.Tag, vr string, vl uint32, val Value) *Element {
	return &Element{
		Tag:                    t,
		ValueRepresentation:    tag.GetVRKind(t, vr),
		RawValueRepresentation: vr,
		ValueLength:            vl,
		Value:                  val,
	}
}

// readWholeElement fully parses an element (tag already read) into the
// in-memory Element representation. It is used for file meta elements, which
// are always Explicit VR Little Endian and which must be re-emitted through
// the standard header writer.
func (tc *transcoder) readWholeElement(t tag.Tag, offset int64) (*Element, *ConversionError) {
	vr, vl, err := tc.readExplicitVRVL(t)
	if err != nil {
		return nil, tc.failElement(err, t, offset)
	}
	val, err := tc.readSimpleValue(t, vr, vl)
	if err != nil {
		return nil, tc.failElement(err, t, offset)
	}
	return tc.newElement(t, vr, vl, val), nil
}

// explicitLongFormVRs are the VRs that use the 32-bit value length field with
// two reserved zero bytes in explicit VR transfer syntaxes.
var explicitLongFormVRs = map[string]bool{
	"NA": true, "OB": true, "OD": true, "OF": true, "OL": true, "OV": true,
	"OW": true, "SQ": true, "UN": true, "UC": true, "UR": true, "UT": true,
}

// readExplicitVRVL reads the 2-byte VR and its value length in an explicit VR
// encoding. Item/delimiter tags (group FFFE) are always encoded implicit-style.
func (tc *transcoder) readExplicitVRVL(t tag.Tag) (string, uint32, error) {
	if t.Group == tag.GroupSeqItem {
		return tc.readImplicitVL()
	}
	vrBytes := make([]byte, 2)
	if _, err := io.ReadFull(tc.rawReader, vrBytes); err != nil {
		return "", 0, err
	}
	vr := string(vrBytes)
	if !isSupportedExplicitVR(vr) {
		return "", 0, fmt.Errorf("VR %q for %s: %w", vr, tag.DebugString(t), ErrorUnsupportedVR)
	}
	if explicitLongFormVRs[vr] {
		reserved := make([]byte, 2)
		if _, err := io.ReadFull(tc.rawReader, reserved); err != nil {
			return "", 0, err
		}
		vl, err := tc.rawReader.ReadUInt32()
		if err != nil {
			return "", 0, err
		}
		if vl == tag.VLUndefinedLength && (vr == "UC" || vr == "UR" || vr == "UT") {
			return "", 0, fmt.Errorf("%s VR=%s may not use undefined length: %w", tag.DebugString(t), vr, ErrorMalformedElement)
		}
		return vr, vl, nil
	}
	vl16, err := tc.rawReader.ReadUInt16()
	if err != nil {
		return "", 0, err
	}
	vl := uint32(vl16)
	if vl == 0xffff {
		vl = tag.VLUndefinedLength
	}
	return vr, vl, nil
}

func (tc *transcoder) readImplicitVL() (string, uint32, error) {
	// Item tags carry no VR bytes; the VR reported ("NA") is only used to pick
	// the implicit VL path below.
	vl, err := tc.rawReader.ReadUInt32()
	return "NA", vl, err
}

func isSupportedExplicitVR(vr string) bool {
	if len(vr) != 2 {
		return false
	}
	_, ok := vrValueUnit[vr]
	return ok
}

// vrValueUnit maps each supported VR to the size in bytes of one value (one
// word) for the purpose of byte-order conversion and value-length validation.
// A unit of 1 means the value is a byte/character stream that is copied
// verbatim. SQ and UN are special-cased elsewhere.
var vrValueUnit = map[string]int{
	"AE": 1, "AS": 1, "CS": 1, "DA": 1, "DS": 1, "DT": 1, "IS": 1,
	"LO": 1, "LT": 1, "PN": 1, "SH": 1, "ST": 1, "TM": 1, "UC": 1,
	"UI": 1, "UR": 1, "UT": 1,
	"OB": 1,
	"OW": 2,
	"OF": 4,
	"OL": 4,
	"OD": 8,
	"OV": 8,
	"UN": 1,
	"US": 2,
	"SS": 2,
	"UL": 4,
	"SL": 4,
	"AT": 2,
	"FL": 4,
	"FD": 8,
	"UV": 8,
	"SV": 8,
	"SQ": 0,
	"NA": 1, // items are handled structurally
}

// determineSourceVR resolves the VR of element t under the source encoding.
// Under explicit VR the caller has already read it; under implicit VR the
// dictionary is consulted, and private/unknown elements are rejected rather
// than being silently assumed to be OB.
func (tc *transcoder) determineSourceVR(t tag.Tag) (string, error) {
	if t.Group == tag.GroupSeqItem {
		return "NA", nil
	}
	if t == tag.PixelData || t == tag.OverlayData {
		return "OW", nil
	}
	info, err := tag.Find(t)
	if err != nil {
		if tag.IsPrivate(t.Group) {
			return "", fmt.Errorf("%s private element under implicit VR transfer syntax: %w",
				tag.DebugString(t), ErrorUndeterminableVR)
		}
		return "", fmt.Errorf("%s unknown element under implicit VR transfer syntax: %w",
			tag.DebugString(t), ErrorUndeterminableVR)
	}
	return info.VRs[0], nil
}

// convertElement transcodes a single element whose tag has already been read
// (at offset) from the source stream. limitVL, when non-nil, bounds the
// element's value (used inside defined-length sequences/items); it is passed
// through to the value handlers rather than pushed here because structural
// elements manage limits themselves.
func (tc *transcoder) convertElement(t tag.Tag, offset int64) *ConversionError {
	// Group length elements (gggg,0000) describe byte counts of the encoding
	// that follows them; counts change during transcoding, so they are dropped
	// rather than copied with a stale length. The file meta group length is
	// recomputed separately by the header writer.
	if t.Element == 0x0000 && t != tag.FileMetaInformationGroupLength {
		_, glVL, err := tc.readSourceVRVL(t)
		if err != nil {
			return tc.failElement(err, t, offset)
		}
		if glVL == tag.VLUndefinedLength {
			return tc.failElement(ErrorMalformedElement, t, offset)
		}
		if err := tc.discardBytes(int64(glVL)); err != nil {
			return tc.failElement(err, t, offset)
		}
		return nil
	}

	if t.Group == tag.GroupSeqItem {
		return tc.failElement(fmt.Errorf("unexpected structural tag %s outside sequence/item: %w",
			tag.DebugString(t), ErrorMalformedElement), t, offset)
	}

	vr, vl, err := tc.readSourceVRVL(t)
	if err != nil {
		return tc.failElement(err, t, offset)
	}

	if t == tag.PixelData {
		return tc.convertPixelData(t, offset, vr, vl)
	}

	if vr == "SQ" {
		return tc.convertSequence(t, offset, vl)
	}

	unit := vrValueUnit[vr]
	if vl != tag.VLUndefinedLength {
		if unit > 1 && vl%uint32(unit) != 0 {
			return tc.failElement(fmt.Errorf("VR=%s value length %d is not a multiple of %d bytes: %w",
				vr, vl, unit, ErrorMalformedElement), t, offset)
		}
		// Text/byte VRs may carry padding and must have even length.
		if vl%2 != 0 {
			return tc.failElement(fmt.Errorf("VR=%s has odd value length %d: %w",
				vr, vl, ErrorMalformedElement), t, offset)
		}
	} else {
		// Defined-length encoding is the only legal form for everything except
		// SQ and the item-encoded OB/OW/UN byte streams.
		if vr != "OB" && vr != "OW" && vr != "UN" {
			return tc.failElement(fmt.Errorf("VR=%s may not use undefined length: %w",
				vr, ErrorMalformedElement), t, offset)
		}
	}

	tc.path = append(tc.path, tag.DebugString(t))
	defer tc.popPath()

	// Descriptor elements are tiny and needed in-memory to interpret a later
	// PixelData: buffer, parse, then transcode.
	if isPixelDescriptor(t) {
		data := make([]byte, vl)
		if _, err := io.ReadFull(tc.rawReader, data); err != nil {
			return tc.failElement(err, t, offset)
		}
		n, perr := parseDescriptor(t, vr, data, tc.sourceBO)
		if perr != nil {
			return tc.failElement(perr, t, offset)
		}
		tc.currentScope().set(t, n)
		if err := tc.writeElementHeader(t, vr, vl); err != nil {
			return tc.failElement(err, t, offset)
		}
		if err := tc.writeBufferedValue(data, unit); err != nil {
			return tc.failElement(err, t, offset)
		}
		return nil
	}

	if cerr := tc.writeElementHeader(t, vr, vl); cerr != nil {
		return tc.failElement(cerr, t, offset)
	}

	if vl == tag.VLUndefinedLength {
		if err := tc.copyUndefinedBytes(vr); err != nil {
			return tc.failElement(err, t, offset)
		}
		return nil
	}
	if err := tc.copyValueBytes(vl, unit); err != nil {
		return tc.failElement(err, t, offset)
	}
	return nil
}

func (tc *transcoder) popPath() {
	tc.path = tc.path[:len(tc.path)-1]
}

// readSourceVRVL reads VR (when present) and VL for element t in the source
// encoding.
func (tc *transcoder) readSourceVRVL(t tag.Tag) (string, uint32, error) {
	if t.Group == tag.GroupSeqItem {
		return tc.readImplicitVL()
	}
	if tc.sourceImp {
		vr, err := tc.determineSourceVR(t)
		if err != nil {
			return "", 0, err
		}
		vl, err := tc.rawReader.ReadUInt32()
		if err != nil {
			return "", 0, err
		}
		return vr, vl, nil
	}
	return tc.readExplicitVRVL(t)
}

// writeElementHeader writes the target-syntax tag + VR (if explicit) + VL.
// Structural item/delimiter tags (group FFFE) are always written implicit
// style with the target byte order.
func (tc *transcoder) writeElementHeader(t tag.Tag, vr string, vl uint32) error {
	if err := tc.writer.WriteUInt16(t.Group); err != nil {
		return err
	}
	if err := tc.writer.WriteUInt16(t.Element); err != nil {
		return err
	}
	if t.Group == tag.GroupSeqItem {
		return tc.writer.WriteUInt32(vl)
	}
	if tc.targetImp {
		return tc.writer.WriteUInt32(vl)
	}
	if err := tc.writer.WriteString(vr); err != nil {
		return err
	}
	if explicitLongFormVRs[vr] {
		if err := tc.writer.WriteZeros(2); err != nil {
			return err
		}
		return tc.writer.WriteUInt32(vl)
	}
	if vl == tag.VLUndefinedLength {
		return fmt.Errorf("internal: short-form VR %s cannot carry undefined length", vr)
	}
	return tc.writer.WriteUInt16(uint16(vl))
}
