package dicom

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"

	"github.com/suyashkumar/dicom/pkg/dicomio"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
	"github.com/suyashkumar/dicom/pkg/vrraw"
)

var (
	// ErrorUnsupportedConversionTransferSyntax indicates that the source or
	// target transfer syntax is not one of the three supported uncompressed
	// transfer syntaxes (Explicit VR Little Endian, Implicit VR Little Endian,
	// Explicit VR Big Endian).
	ErrorUnsupportedConversionTransferSyntax = errors.New("unsupported transfer syntax for conversion (only uncompressed Explicit/Implicit VR Little Endian and Explicit VR Big Endian are supported)")
	// ErrorEncapsulatedPixelDataRejected indicates that the source PixelData is
	// encapsulated (e.g. compressed), which this converter refuses to pass
	// through without decompressing.
	ErrorEncapsulatedPixelDataRejected = errors.New("encapsulated PixelData cannot be converted; decompression is not performed")
	// ErrorUnknownVRRejected indicates that an element's VR could not be
	// determined (an unknown private element in implicit VR, or a UN VR), so the
	// converter refused to guess and copy it as raw bytes.
	ErrorUnknownVRRejected = errors.New("unable to determine VR for element; refusing to treat it as raw bytes")
	// ErrorUnsupportedVRConversion indicates that an element uses a VR this
	// converter does not know how to re-encode across byte orders.
	ErrorUnsupportedVRConversion = errors.New("unsupported VR for transfer syntax conversion")
)

// supportedConversionTransferSyntaxes maps the transfer syntax UIDs the
// converter understands to their byte order and VR encoding.
var supportedConversionTransferSyntaxes = map[string]struct {
	bo       binary.ByteOrder
	implicit bool
}{
	uid.ImplicitVRLittleEndian: {binary.LittleEndian, true},
	uid.ExplicitVRLittleEndian: {binary.LittleEndian, false},
	uid.ExplicitVRBigEndian:    {binary.BigEndian, false},
}

// ConversionError wraps a failure encountered while converting a DICOM to a
// new transfer syntax. It records the logical path of the data element being
// processed (tag tuples joined by "/", with sequence items marked "[i]") and
// the absolute byte offset of that element in the source stream (0-based).
//
// On error, some bytes may already have been written to the output stream; the
// presence of a ConversionError means the output is never a complete, valid
// DICOM.
type ConversionError struct {
	Op     string
	Path   string
	Offset int64
	Err    error
}

func (e *ConversionError) Error() string {
	loc := ""
	if e.Path != "" {
		loc = " at element " + e.Path
	}
	return fmt.Sprintf("dicom transfer syntax conversion error in %s%s (source offset %d): %v",
		e.Op, loc, e.Offset, e.Err)
}

func (e *ConversionError) Unwrap() error { return e.Err }

// ConvertTransferSyntax reads an uncompressed DICOM file stream from in and
// writes it to out re-encoded in the requested target transfer syntax.
//
// targetTransferSyntaxUID must be one of:
//   - 1.2.840.10008.1.2   (Implicit VR Little Endian)
//   - 1.2.840.10008.1.2.1 (Explicit VR Little Endian)
//   - 1.2.840.10008.1.2.2 (Explicit VR Big Endian)
//
// The input must likewise use one of those three uncompressed transfer
// syntaxes. The file metadata (group 0002) is always re-emitted as Explicit VR
// Little Endian, with its (0002,0000) FileMetaInformationGroupLength
// recomputed and (0002,0010) TransferSyntaxUID replaced by the target UID.
//
// Numeric VRs, sequence and item lengths, and native pixel samples are
// re-encoded to the target byte order. Pixel values are not decompressed or
// modified; BitsAllocated and PixelRepresentation semantics are preserved.
// Encapsulated PixelData, elements whose VR cannot be determined, and unknown
// transfer syntaxes cause an error rather than being silently copied.
//
// The conversion is streaming. On error the output may already contain bytes,
// but it must not be treated as a complete DICOM. The returned *ConversionError
// reports the failing element path and the element's original offset in in.
func ConvertTransferSyntax(in io.Reader, out io.Writer, bytesToRead int64, targetTransferSyntaxUID string) error {
	target, ok := supportedConversionTransferSyntaxes[targetTransferSyntaxUID]
	if !ok {
		return &ConversionError{
			Op:     "validating target transfer syntax",
			Offset: 0,
			Err:    fmt.Errorf("%w: %q", ErrorUnsupportedConversionTransferSyntax, targetTransferSyntaxUID),
		}
	}

	bw := bufio.NewWriter(out)
	c := &transcoder{
		reader: dicomio.NewReader(bufio.NewReader(in), binary.LittleEndian, bytesToRead),
		writer: dicomio.NewWriter(bw, binary.LittleEndian, false),
		path:   nil,
		scopes: []attrScope{{attrs: map[tag.Tag]uint64{}}},
	}
	if err := c.run(target.bo, target.implicit, targetTransferSyntaxUID); err != nil {
		// Best-effort flush so any bytes already produced are delivered to out,
		// but never claim the result is complete.
		_ = bw.Flush()
		return err
	}
	return bw.Flush()
}

// ConvertTransferSyntaxUntilEOF is like ConvertTransferSyntax but reads the
// input until EOF rather than requiring an explicit input byte length.
func ConvertTransferSyntaxUntilEOF(in io.Reader, out io.Writer, targetTransferSyntaxUID string) error {
	return ConvertTransferSyntax(in, out, dicomio.LimitReadUntilEOF, targetTransferSyntaxUID)
}

// elementHeader describes the parsed header of one data element.
type elementHeader struct {
	tag tag.Tag
	vr  string
	vl  uint32
	// offset is the absolute byte offset of this element's tag in the source.
	offset int64
}

// attrScope holds the attribute values relevant to native PixelData decoding
// for one nesting context: the root dataset or a single sequence item.
type attrScope struct {
	attrs          map[tag.Tag]uint64
	numberOfFrames string
}

// transcoder performs the actual element-by-element conversion.
type transcoder struct {
	reader      *dicomio.Reader
	writer      *dicomio.Writer
	srcBO       binary.ByteOrder
	dstBO       binary.ByteOrder
	srcImplicit bool
	dstImplicit bool
	swap        bool
	path        []string
	scopes      []attrScope
}

func (c *transcoder) fail(op string, h elementHeader, err error) error {
	return &ConversionError{
		Op:     op,
		Path:   c.pathString(h),
		Offset: h.offset,
		Err:    err,
	}
}

func (c *transcoder) pathString(h elementHeader) string {
	parts := append([]string{}, c.path...)
	if h.tag.Group != 0 || h.tag.Element != 0 {
		parts = append(parts, h.tag.String())
	}
	return strings.Join(parts, "/")
}

func (c *transcoder) pushScope() {
	c.scopes = append(c.scopes, attrScope{attrs: map[tag.Tag]uint64{}})
}

func (c *transcoder) popScope() {
	c.scopes = c.scopes[:len(c.scopes)-1]
}

// attrValue returns the recorded value of a pixel-describing attribute from
// the innermost scope that defines it.
func (c *transcoder) attrValue(t tag.Tag) (uint64, bool) {
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if v, ok := c.scopes[i].attrs[t]; ok {
			return v, true
		}
	}
	return 0, false
}

var errAttrNotCaptured = errors.New("attribute not captured for pixel decoding")

// pixelAttrTags are the numeric attributes needed to interpret native
// PixelData. NumberOfFrames is a string (IS) and handled separately.
var pixelAttrTags = map[tag.Tag]bool{
	tag.Rows:                true,
	tag.Columns:             true,
	tag.SamplesPerPixel:     true,
	tag.BitsAllocated:       true,
	tag.PixelRepresentation: true,
}

// captureAttr parses and records a small attribute needed to interpret native
// PixelData. The value is copied through unchanged; it is only parsed so that
// pixel sample sizes can be validated. It returns errAttrNotCaptured if the
// element is not one of the captured numeric attributes.
func (c *transcoder) captureAttr(h elementHeader) error {
	if h.tag == tag.NumberOfFrames {
		if h.vl == tag.VLUndefinedLength {
			return fmt.Errorf("NumberOfFrames has undefined length")
		}
		raw := make([]byte, h.vl)
		if _, err := io.ReadFull(c.reader, raw); err != nil {
			return err
		}
		c.scopes[len(c.scopes)-1].numberOfFrames = strings.TrimSpace(strings.Trim(string(raw), "\x00 "))
		return nil
	}
	if !pixelAttrTags[h.tag] {
		return errAttrNotCaptured
	}
	if h.vl == tag.VLUndefinedLength {
		return fmt.Errorf("attribute %s has undefined length", tag.DebugString(h.tag))
	}
	width, ok := widthForVR(h.vr)
	if !ok || width > 4 {
		return fmt.Errorf("attribute %s has unexpected VR %s", tag.DebugString(h.tag), h.vr)
	}
	if h.vl%uint32(width) != 0 {
		return fmt.Errorf("attribute %s value length %d is not a multiple of %d", tag.DebugString(h.tag), h.vl, width)
	}
	v, err := c.readUnsignedWord(width)
	if err != nil {
		return err
	}
	c.scopes[len(c.scopes)-1].attrs[h.tag] = v
	return nil
}

// readUnsignedWord reads a single unsigned integer of width 1, 2 or 4 bytes
// according to the current source byte order.
func (c *transcoder) readUnsignedWord(width int) (uint64, error) {
	switch width {
	case 1:
		v, err := c.reader.ReadUInt8()
		return uint64(v), err
	case 2:
		v, err := c.reader.ReadUInt16()
		return uint64(v), err
	case 4:
		v, err := c.reader.ReadUInt32()
		return uint64(v), err
	default:
		return 0, fmt.Errorf("internal error: unsupported attribute width %d", width)
	}
}

func (c *transcoder) run(dstBO binary.ByteOrder, dstImplicit bool, targetUID string) error {
	// Preamble (128 bytes) + "DICM" magic word are byte order independent.
	preamble := make([]byte, 128+4)
	if _, err := io.ReadFull(c.reader, preamble); err != nil {
		return &ConversionError{Op: "reading preamble", Offset: c.reader.BytesRead(), Err: err}
	}
	if string(preamble[128:]) != magicWord {
		return &ConversionError{Op: "reading preamble", Offset: 0, Err: ErrorMagicWord}
	}
	if _, err := c.writer.Write(preamble); err != nil {
		return &ConversionError{Op: "writing preamble", Offset: 0, Err: err}
	}

	// File meta information is always Explicit VR Little Endian.
	c.reader.SetTransferSyntax(binary.LittleEndian, false)
	c.writer.SetTransferSyntax(binary.LittleEndian, false)

	metaElems, srcUID, err := c.readMetadata()
	if err != nil {
		return err
	}

	srcTS, ok := supportedConversionTransferSyntaxes[srcUID]
	if !ok {
		return &ConversionError{
			Op:     "resolving source transfer syntax",
			Offset: 0,
			Err:    fmt.Errorf("%w: source TransferSyntaxUID %q", ErrorUnsupportedConversionTransferSyntax, srcUID),
		}
	}

	if err := c.writeMetadata(metaElems, targetUID); err != nil {
		return err
	}

	c.srcBO, c.srcImplicit = srcTS.bo, srcTS.implicit
	c.dstBO, c.dstImplicit = dstBO, dstImplicit
	c.swap = c.srcBO != c.dstBO
	c.reader.SetTransferSyntax(c.srcBO, c.srcImplicit)
	c.writer.SetTransferSyntax(c.dstBO, c.dstImplicit)

	for {
		// Detect a clean end of stream/limit at an element boundary.
		if _, err := c.reader.Peek(1); err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return &ConversionError{
				Op:     "reading element header",
				Path:   strings.Join(c.path, "/"),
				Offset: c.reader.BytesRead(),
				Err:    err,
			}
		}
		h, err := c.readElementHeader(false)
		if err != nil {
			return &ConversionError{
				Op:     "reading element header",
				Path:   c.pathString(h),
				Offset: c.reader.BytesRead(),
				Err:    err,
			}
		}
		if h.tag.Group == tag.GroupSeqItem {
			return c.fail("reading dataset", h, fmt.Errorf("unexpected item/delimitation tag %s outside of a sequence", h.tag))
		}
		if err := c.convertElement(h); err != nil {
			return err
		}
	}
	return nil
}

// metaElement holds one group 0002 element already consumed from the input,
// along with its raw value bytes as encoded in the source (Explicit VR Little
// Endian, which never changes for group 2).
type metaElement struct {
	h         elementHeader
	value     []byte
	isTS      bool
	sourceUID string
}

// readMetadata reads the group 0002 File Meta Information, requiring the
// standard (0002,0000) FileMetaInformationGroupLength element first.
func (c *transcoder) readMetadata() ([]metaElement, string, error) {
	h, err := c.readElementHeader(false)
	if err != nil {
		return nil, "", &ConversionError{
			Op:     "reading File Meta Information",
			Offset: 0,
			Err:    fmt.Errorf("%w: %v", ErrorMetaElementGroupLength, err),
		}
	}
	if h.tag != tag.FileMetaInformationGroupLength {
		return nil, "", &ConversionError{
			Op:     "reading File Meta Information",
			Path:   h.tag.String(),
			Offset: h.offset,
			Err:    ErrorMetaElementGroupLength,
		}
	}
	if h.vl == tag.VLUndefinedLength || h.vl != 4 {
		return nil, "", c.fail("reading File Meta Information group length", h,
			fmt.Errorf("FileMetaInformationGroupLength must be a 4-byte UL, got VR=%s VL=%d", h.vr, h.vl))
	}

	metaLenVal, err := c.reader.ReadUInt32()
	if err != nil {
		return nil, "", c.fail("reading File Meta Information group length", h, err)
	}

	var elems []metaElement
	sourceUID := ""
	if err := c.reader.PushLimit(int64(metaLenVal)); err != nil {
		return nil, "", c.fail("reading File Meta Information", h, err)
	}
	for !c.reader.IsLimitExhausted() {
		mh, err := c.readElementHeader(false)
		if err != nil {
			return nil, "", &ConversionError{
				Op:     "reading File Meta Information element",
				Offset: c.reader.BytesRead(),
				Err:    err,
			}
		}
		if mh.tag.Group != tag.MetadataGroup {
			return nil, "", c.fail("reading File Meta Information", mh,
				fmt.Errorf("unexpected non-group-0002 tag %s within File Meta Information", mh.tag))
		}

		if mh.vr == vrraw.Sequence {
			return nil, "", c.fail("reading File Meta Information", mh,
				fmt.Errorf("%w: SQ inside File Meta Information", ErrorUnsupportedVRConversion))
		}
		if mh.vr == vrraw.Unknown {
			return nil, "", c.fail("reading File Meta Information", mh,
				fmt.Errorf("%w: UN VR inside File Meta Information", ErrorUnknownVRRejected))
		}

		me := metaElement{h: mh}
		if mh.vl == tag.VLUndefinedLength {
			return nil, "", c.fail("reading File Meta Information", mh,
				fmt.Errorf("undefined length is not allowed for group-0002 element with VR %s", mh.vr))
		}
		me.value = make([]byte, mh.vl)
		if _, err := io.ReadFull(c.reader, me.value); err != nil {
			return nil, "", c.fail("reading File Meta Information", mh, err)
		}

		if mh.tag == tag.TransferSyntaxUID {
			if mh.vr != vrraw.UniqueIdentifier {
				return nil, "", c.fail("reading File Meta Information", mh,
					fmt.Errorf("TransferSyntaxUID has VR %s, want UI", mh.vr))
			}
			me.isTS = true
			me.sourceUID = strings.Trim(string(me.value), "\x00 ")
			sourceUID = me.sourceUID
		}
		elems = append(elems, me)
	}
	c.reader.PopLimit()

	if sourceUID == "" {
		return nil, "", &ConversionError{
			Op:     "reading File Meta Information",
			Offset: 132,
			Err:    fmt.Errorf("TransferSyntaxUID (0002,0010) is missing from File Meta Information"),
		}
	}
	return elems, sourceUID, nil
}

// writeMetadata serializes the group-0002 elements in Explicit VR Little
// Endian, replacing the TransferSyntaxUID value and recomputing
// FileMetaInformationGroupLength.
func (c *transcoder) writeMetadata(elems []metaElement, targetUID string) error {
	var body bytes.Buffer
	sub := dicomio.NewWriter(&body, binary.LittleEndian, false)
	for _, me := range elems {
		h := me.h
		value := me.value
		if me.isTS {
			value = padUID(targetUID)
			h.vl = uint32(len(value))
		}
		if err := writeElementHeader(sub, h, h.vl); err != nil {
			return &ConversionError{
				Op:     "writing File Meta Information",
				Path:   h.tag.String(),
				Offset: c.reader.BytesRead(),
				Err:    err,
			}
		}
		if _, err := sub.Write(value); err != nil {
			return &ConversionError{
				Op:     "writing File Meta Information",
				Path:   h.tag.String(),
				Offset: c.reader.BytesRead(),
				Err:    err,
			}
		}
	}

	lengthHeader := elementHeader{
		tag: tag.FileMetaInformationGroupLength,
		vr:  vrraw.UnsignedLong,
		vl:  4,
	}
	if err := writeElementHeader(c.writer, lengthHeader, 4); err != nil {
		return &ConversionError{Op: "writing File Meta Information group length", Offset: c.reader.BytesRead(), Err: err}
	}
	if err := c.writer.WriteUInt32(uint32(body.Len())); err != nil {
		return &ConversionError{Op: "writing File Meta Information group length", Offset: c.reader.BytesRead(), Err: err}
	}
	if _, err := c.writer.Write(body.Bytes()); err != nil {
		return &ConversionError{Op: "writing File Meta Information", Offset: c.reader.BytesRead(), Err: err}
	}
	return nil
}

// padUID returns the UID padded to even length with a trailing NULL byte.
func padUID(uidValue string) []byte {
	if len(uidValue)%2 == 1 {
		return []byte(uidValue + "\x00")
	}
	return []byte(uidValue)
}

// widthForVR returns the native word width in bytes for numeric VRs that must
// be byte-swapped when changing byte order.
func widthForVR(vr string) (int, bool) {
	switch vr {
	case vrraw.UnsignedShort, vrraw.SignedShort, vrraw.AttributeTag:
		return 2, true
	case vrraw.UnsignedLong, vrraw.SignedLong, vrraw.FloatingPointSingle,
		vrraw.OtherFloat, vrraw.OtherLong:
		return 4, true
	case vrraw.FloatingPointDouble, vrraw.OtherDouble, vrraw.OtherVeryLong,
		vrraw.SignedVeryLong, vrraw.UnsignedVeryLong:
		return 8, true
	}
	return 0, false
}

// explicitLongFormVRs are the VRs encoded with a 16-bit reserved field and a
// 32-bit value length in Explicit VR transfer syntaxes (PS3.5 Table 7.1-2).
var explicitLongFormVRs = map[string]bool{
	vrraw.OtherByte: true, vrraw.OtherWord: true, vrraw.OtherDouble: true,
	vrraw.OtherFloat: true, vrraw.OtherLong: true, vrraw.OtherVeryLong: true,
	vrraw.Sequence: true, vrraw.Unknown: true, vrraw.SignedVeryLong: true,
	vrraw.UnsignedVeryLong: true, vrraw.UnlimitedCharacters: true,
	vrraw.UniversalResourceIdentifier: true, vrraw.UnlimitedText: true,
}

// writeElementHeader emits a data element (or item) tag, VR and value length in
// the writer's current transfer syntax.
func writeElementHeader(w *dicomio.Writer, h elementHeader, vl uint32) error {
	if err := w.WriteUInt16(h.tag.Group); err != nil {
		return err
	}
	if err := w.WriteUInt16(h.tag.Element); err != nil {
		return err
	}

	if h.tag == tag.Item || h.tag == tag.ItemDelimitationItem || h.tag == tag.SequenceDelimitationItem {
		// Item and delimitation item headers are always implicit.
		return w.WriteUInt32(vl)
	}

	_, implicit := w.GetTransferSyntax()
	if implicit {
		return w.WriteUInt32(vl)
	}

	if len(h.vr) != 2 {
		return fmt.Errorf("internal error: cannot write header with VR %q for tag %s", h.vr, h.tag)
	}
	if err := w.WriteString(h.vr); err != nil {
		return err
	}
	if explicitLongFormVRs[h.vr] {
		if err := w.WriteZeros(2); err != nil {
			return err
		}
		return w.WriteUInt32(vl)
	}
	if vl != tag.VLUndefinedLength && vl > 0xffff {
		return fmt.Errorf("value length %d does not fit in the 16-bit explicit-VR length field for VR %s", vl, h.vr)
	}
	return w.WriteUInt16(uint16(vl))
}

// readElementHeader parses the tag, VR and value length of the next element.
// itemHeader must be true when reading directly inside a sequence (where item
// and delimitation headers are always implicit VR).
func (c *transcoder) readElementHeader(itemHeader bool) (elementHeader, error) {
	var h elementHeader
	h.offset = c.reader.BytesRead()
	group, err := c.reader.ReadUInt16()
	if err != nil {
		return h, err
	}
	element, err := c.reader.ReadUInt16()
	if err != nil {
		return h, err
	}
	h.tag = tag.Tag{Group: group, Element: element}

	// Tags other than group FFFE control the implicit/explicit and VR rules.
	if h.tag == tag.Item || h.tag == tag.ItemDelimitationItem || h.tag == tag.SequenceDelimitationItem {
		h.vr = "NA"
		h.vl, err = c.readVLImplicit()
		return h, err
	}

	implicit := c.srcImplicit || itemHeader
	if implicit {
		vr, err := c.implicitVR(h.tag)
		if err != nil {
			return h, err
		}
		h.vr = vr
		h.vl, err = c.readVLImplicit()
		return h, err
	}

	vr, err := c.reader.ReadString(2)
	if err != nil {
		return h, err
	}
	if !knownVR(vr) {
		return h, fmt.Errorf("%w: invalid VR bytes %q in explicit VR encoding for tag %s",
			ErrorUnsupportedVRConversion, vr, tag.DebugString(h.tag))
	}
	h.vr = vr
	h.vl, err = c.readVLExplicit(vr)
	return h, err
}

// implicitVR resolves the VR of an element under an implicit VR transfer
// syntax from the data dictionary. It refuses to guess when the tag is unknown
// (this includes private creators and private data elements).
func (c *transcoder) implicitVR(t tag.Tag) (string, error) {
	info, err := tag.Find(t)
	if err != nil {
		if tag.IsPrivate(t.Group) {
			return "", fmt.Errorf("%w: private element %s has no determinable VR in implicit VR encoding",
				ErrorUnknownVRRejected, t)
		}
		return "", fmt.Errorf("%w: unknown tag %s has no determinable VR in implicit VR encoding",
			ErrorUnknownVRRejected, t)
	}
	switch t {
	case tag.PixelData, tag.OverlayData:
		// OW takes priority for these tags (PS3.5 8.1.2/8.2).
		return vrraw.OtherWord, nil
	}
	return info.VRs[0], nil
}

func (c *transcoder) readVLImplicit() (uint32, error) {
	return c.reader.ReadUInt32()
}

func (c *transcoder) readVLExplicit(vr string) (uint32, error) {
	if explicitLongFormVRs[vr] {
		if _, err := io.CopyN(io.Discard, c.reader, 2); err != nil {
			return 0, err
		}
		vl, err := c.reader.ReadUInt32()
		if err != nil {
			return 0, err
		}
		if vl == tag.VLUndefinedLength &&
			(vr == vrraw.UnlimitedCharacters ||
				vr == vrraw.UniversalResourceIdentifier ||
				vr == vrraw.UnlimitedText) {
			return 0, fmt.Errorf("%s may not have an Undefined Length", vr)
		}
		return vl, nil
	}
	vl16, err := c.reader.ReadUInt16()
	if err != nil {
		return 0, err
	}
	if uint32(vl16) == 0xffff {
		return tag.VLUndefinedLength, nil
	}
	return uint32(vl16), nil
}

// knownVR reports whether the two VR bytes name a VR this converter supports.
func knownVR(vr string) bool {
	if len(vr) != 2 {
		return false
	}
	_, numeric := widthForVR(vr)
	switch vr {
	case vrraw.ApplicationEntity, vrraw.AgeString, vrraw.CodeString, vrraw.Date,
		vrraw.DecimalString, vrraw.DateTime, vrraw.IntegerString, vrraw.LongString,
		vrraw.LongText, vrraw.PersonName, vrraw.ShortString, vrraw.ShortText,
		vrraw.Time, vrraw.UniqueIdentifier, vrraw.UnlimitedCharacters, vrraw.Unknown,
		vrraw.UniversalResourceIdentifier, vrraw.UnlimitedText,
		vrraw.OtherByte, vrraw.OtherWord, vrraw.Sequence:
		return true
	}
	return numeric
}

// convertElement reads one element value and emits it in the target transfer
// syntax, dispatching on sequences, pixel data and primitive VRs.
func (c *transcoder) convertElement(h elementHeader) error {
	// Record attributes needed to interpret native PixelData. Their bytes are
	// consumed and re-encoded normally; the captured value only validates pixel
	// geometry.
	captured := false
	if h.vl != tag.VLUndefinedLength {
		if err := c.captureAttr(h); err == nil {
			captured = true
		} else if !errors.Is(err, errAttrNotCaptured) {
			return c.fail("reading pixel descriptor attribute", h, err)
		}
	}

	if h.tag == tag.PixelData {
		if captured {
			return c.fail("converting PixelData", h, fmt.Errorf("internal error: PixelData incorrectly captured as an attribute"))
		}
		return c.convertPixelData(h)
	}

	if h.vr == vrraw.Sequence {
		if captured {
			return c.fail("converting sequence", h, fmt.Errorf("internal error: sequence incorrectly captured as an attribute"))
		}
		return c.convertSequence(h)
	}

	if h.vr == vrraw.Unknown {
		// UN is never rewritten implicitly as bytes: its content encoding is
		// unknown across byte orders, and the requirement is to refuse rather
		// than silently copy.
		return c.fail("converting element", h,
			fmt.Errorf("%w: UN VR element cannot be safely transcoded", ErrorUnknownVRRejected))
	}

	if !captured {
		if err := c.transcodePrimitive(h); err != nil {
			return err
		}
	} else {
		// The bytes were already consumed by captureAttr; emit the header and
		// re-encode the captured value from the source byte order.
		if err := writeElementHeader(c.writer, h, h.vl); err != nil {
			return c.fail("writing element header", h, err)
		}
		if err := c.emitCapturedAttr(h); err != nil {
			return err
		}
	}
	return nil
}

// emitCapturedAttr re-encodes a previously captured numeric descriptor value
// (rows, columns, etc.) into the target byte order.
func (c *transcoder) emitCapturedAttr(h elementHeader) error {
	if h.tag == tag.NumberOfFrames {
		// IS is a decimal string: re-pad to the original (even) length.
		value := c.scopes[len(c.scopes)-1].numberOfFrames
		data := []byte(value)
		for len(data) < int(h.vl) {
			data = append(data, ' ')
		}
		if len(data) != int(h.vl) {
			return c.fail("writing attribute", h,
				fmt.Errorf("re-encoded NumberOfFrames length %d differs from source length %d", len(data), h.vl))
		}
		if _, err := c.writer.Write(data); err != nil {
			return c.fail("writing attribute", h, err)
		}
		return nil
	}
	v, ok := c.attrValue(h.tag)
	if !ok {
		return c.fail("writing attribute", h, fmt.Errorf("captured value missing for %s", tag.DebugString(h.tag)))
	}
	width, _ := widthForVR(h.vr)
	switch width {
	case 2:
		if err := c.writer.WriteUInt16(uint16(v)); err != nil {
			return c.fail("writing attribute", h, err)
		}
	case 4:
		if err := c.writer.WriteUInt32(uint32(v)); err != nil {
			return c.fail("writing attribute", h, err)
		}
	default:
		return c.fail("writing attribute", h, fmt.Errorf("unsupported width %d for attribute %s", width, tag.DebugString(h.tag)))
	}
	return nil
}

// transcodePrimitive handles all non-sequence, non-pixel, non-UN elements.
func (c *transcoder) transcodePrimitive(h elementHeader) error {
	if h.vl == tag.VLUndefinedLength {
		// Defined-length encoding is the only supported form outside sequences
		// and encapsulated pixel data (which is separately rejected).
		return c.fail("converting element", h,
			fmt.Errorf("undefined length is not valid for VR %s outside a sequence", h.vr))
	}
	if h.vl%2 != 0 {
		return c.fail("converting element", h, fmt.Errorf("%w: odd value length %d", ErrorExpectedEvenLength, h.vl))
	}

	if err := writeElementHeader(c.writer, h, h.vl); err != nil {
		return c.fail("writing element header", h, err)
	}

	width, numeric := widthForVR(h.vr)
	if !numeric {
		// Text and byte-stream VRs (AE, AS, CS, DA, DS, DT, IS, LO, LT, PN,
		// SH, ST, TM, UI, UC, UR, UT, OB): value bytes carry no numeric byte
		// order. Copy verbatim, preserving existing padding.
		if err := c.copyValue(int64(h.vl)); err != nil {
			return c.fail("converting element value", h, err)
		}
		return nil
	}

	if err := c.transcodeWords(h, width, int64(h.vl)); err != nil {
		return err
	}
	return nil
}

// copyValue streams n bytes unchanged from the input to the output.
func (c *transcoder) copyValue(n int64) error {
	_, err := io.CopyN(c.writer, c.reader, n)
	return err
}

// transcodeWords streams a fixed-width numeric value field, swapping each word
// when the source and target byte orders differ.
func (c *transcoder) transcodeWords(h elementHeader, width int, n int64) error {
	if n%int64(width) != 0 {
		return c.fail("converting element value", h,
			fmt.Errorf("value length %d is not a multiple of %d for VR %s", n, width, h.vr))
	}
	if !c.swap {
		if err := c.copyValue(n); err != nil {
			return c.fail("converting element value", h, err)
		}
		return nil
	}

	buf := make([]byte, 32*1024)
	remaining := n
	for remaining > 0 {
		chunk := int64(len(buf))
		if chunk > remaining {
			chunk = remaining
		}
		if chunk%int64(width) != 0 {
			return c.fail("converting element value", h,
				fmt.Errorf("internal error: chunk length %d not aligned to word width %d", chunk, width))
		}
		if _, err := io.ReadFull(c.reader, buf[:chunk]); err != nil {
			return c.fail("converting element value", h, err)
		}
		swapWords(buf[:chunk], width)
		if _, err := c.writer.Write(buf[:chunk]); err != nil {
			return c.fail("converting element value", h, err)
		}
		remaining -= chunk
	}
	return nil
}

// swapWords reverses the bytes within every width-byte word in place.
func swapWords(data []byte, width int) {
	// Byte reversal within a word is independent of the destination order:
	// reversing each word converts between the two endiannesses.
	for i := 0; i+width <= len(data); i += width {
		for a, b := i, i+width-1; a < b; a, b = a+1, b-1 {
			data[a], data[b] = data[b], data[a]
		}
	}
}

// convertSequence re-encodes an SQ element. Both defined- and undefined-length
// source sequences are accepted; output always uses the undefined-length form
// with per-item delimitation items, per PS3.5 7.5.
func (c *transcoder) convertSequence(h elementHeader) error {
	definedLength := h.vl != tag.VLUndefinedLength
	if definedLength {
		if err := c.reader.PushLimit(int64(h.vl)); err != nil {
			return c.fail("reading sequence", h, err)
		}
		defer c.reader.PopLimit()
	}

	outHeader := h
	outHeader.vl = tag.VLUndefinedLength
	if err := writeElementHeader(c.writer, outHeader, tag.VLUndefinedLength); err != nil {
		return c.fail("writing sequence header", h, err)
	}

	itemIndex := 0
	for {
		if c.reader.IsLimitExhausted() {
			if !definedLength {
				return c.fail("reading sequence", h, ErrorExpectedSequenceDelimitation)
			}
			// Defined-length source sequences end when the limit is spent;
			// close the emitted undefined-length sequence.
			if err := writeElementHeader(c.writer, elementHeader{tag: tag.SequenceDelimitationItem, vr: "NA", vl: 0}, 0); err != nil {
				return c.fail("writing sequence delimitation", h, err)
			}
			return nil
		}
		ih, err := c.readElementHeader(true)
		if err != nil {
			return c.fail("reading sequence item header", elementHeader{}, err)
		}

		switch ih.tag {
		case tag.SequenceDelimitationItem:
			if definedLength {
				return c.fail("reading sequence", ih, fmt.Errorf("sequence delimitation item found in a defined-length sequence"))
			}
			if err := writeElementHeader(c.writer, elementHeader{tag: tag.SequenceDelimitationItem, vr: "NA", vl: 0}, 0); err != nil {
				return c.fail("writing sequence delimitation", ih, err)
			}
			return nil
		case tag.Item:
			// handled below
		default:
			return c.fail("reading sequence", ih, fmt.Errorf("expected item (fffe,e000), found %s", ih.tag))
		}

		c.path = append(c.path, fmt.Sprintf("%s[%d]", h.tag, itemIndex))
		err = c.convertItem(ih)
		c.path = c.path[:len(c.path)-1]
		if err != nil {
			return err
		}
		itemIndex++
	}
}

// convertItem converts a single sequence item and its nested dataset.
func (c *transcoder) convertItem(h elementHeader) error {
	definedLength := h.vl != tag.VLUndefinedLength
	if definedLength {
		if err := c.reader.PushLimit(int64(h.vl)); err != nil {
			return c.fail("reading sequence item", h, err)
		}
		defer c.reader.PopLimit()
	}

	// Items are always emitted undefined-length with an item delimitation item.
	if err := writeElementHeader(c.writer, elementHeader{tag: tag.Item, vr: "NA", vl: tag.VLUndefinedLength}, tag.VLUndefinedLength); err != nil {
		return c.fail("writing sequence item header", h, err)
	}

	c.pushScope()
	defer c.popScope()
	for {
		var child elementHeader
		var err error
		if c.reader.IsLimitExhausted() {
			if !definedLength {
				return c.fail("reading sequence item", h, ErrorExpectedSequenceDelimitation)
			}
			err = io.EOF
		} else {
			child, err = c.readElementHeader(false)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return c.fail("reading nested element", elementHeader{}, err)
		}

		if !definedLength && child.tag == tag.ItemDelimitationItem {
			if err := writeElementHeader(c.writer, elementHeader{tag: tag.ItemDelimitationItem, vr: "NA", vl: 0}, 0); err != nil {
				return c.fail("writing item delimitation", child, err)
			}
			return nil
		}

		if child.tag == tag.Item || child.tag == tag.SequenceDelimitationItem || child.tag == tag.ItemDelimitationItem {
			return c.fail("reading nested element", child, fmt.Errorf("unexpected item control tag %s inside a sequence item", child.tag))
		}
		if err := c.convertElement(child); err != nil {
			return err
		}
	}
	// Defined-length source items had no item delimitation; the emitted item is
	// undefined-length, so append one.
	if err := writeElementHeader(c.writer, elementHeader{tag: tag.ItemDelimitationItem, vr: "NA", vl: 0}, 0); err != nil {
		return c.fail("writing item delimitation", h, err)
	}
	return nil
}

// convertPixelData converts the native PixelData (7fe0,0010) value.
// Encapsulated (undefined-length) pixel data is refused. For native data, only
// the already-determined per-sample byte order is changed; pixel values are
// otherwise untouched and BitsAllocated/PixelRepresentation semantics are
// preserved.
func (c *transcoder) convertPixelData(h elementHeader) error {
	if h.vl == tag.VLUndefinedLength {
		return c.fail("converting PixelData", h, ErrorEncapsulatedPixelDataRejected)
	}

	rows, ok := c.attrValue(tag.Rows)
	if !ok {
		return c.fail("converting PixelData", h, fmt.Errorf("Rows (0028,0010) is required to validate native PixelData"))
	}
	cols, ok := c.attrValue(tag.Columns)
	if !ok {
		return c.fail("converting PixelData", h, fmt.Errorf("Columns (0028,0011) is required to validate native PixelData"))
	}
	samples := uint64(1)
	if v, found := c.attrValue(tag.SamplesPerPixel); found {
		samples = v
	}
	bitsAllocated, ok := c.attrValue(tag.BitsAllocated)
	if !ok {
		return c.fail("converting PixelData", h, fmt.Errorf("BitsAllocated (0028,0100) is required to validate native PixelData"))
	}
	// PixelRepresentation is intentionally only validated for presence of a
	// meaningful value; it is never transformed. Its presence is not required
	// to compute byte layout, so its absence does not block conversion.

	var sampleWidth int64
	switch bitsAllocated {
	case 1:
		// Packed bits are stored MSB-first within each byte and involve no
		// 16-bit word swapping.
		sampleWidth = 1
	case 8, 16, 32:
		sampleWidth = int64(bitsAllocated) / 8
	default:
		return c.fail("converting PixelData", h,
			fmt.Errorf("%w: BitsAllocated=%d has no determined sample byte order", ErrorUnsupportedBitsAllocated, bitsAllocated))
	}

	nFrames := uint64(1)
	var nf string
	for i := len(c.scopes) - 1; i >= 0; i-- {
		if c.scopes[i].numberOfFrames != "" {
			nf = c.scopes[i].numberOfFrames
			break
		}
	}
	if nf != "" {
		parsed, err := strconv.ParseUint(strings.TrimSpace(nf), 10, 64)
		if err != nil {
			return c.fail("converting PixelData", h, fmt.Errorf("invalid NumberOfFrames %q: %w", nf, err))
		}
		nFrames = parsed
	}

	pixelsPerFrame := rows * cols
	var expected uint64
	if bitsAllocated == 1 {
		totalBits := pixelsPerFrame * samples * nFrames
		expected = totalBits / 8
		if totalBits%8 != 0 {
			return c.fail("converting PixelData", h,
				fmt.Errorf("number of 1-bit samples (%d) is not a multiple of 8", totalBits))
		}
	} else {
		expected = pixelsPerFrame * samples * nFrames * uint64(sampleWidth)
	}

	padding := uint32(0)
	switch {
	case expected == uint64(h.vl):
	case expected+1 == uint64(h.vl) && h.vl%2 == 0:
		padding = 1
	default:
		return c.fail("converting PixelData", h,
			fmt.Errorf("%w: value length %d does not match expected %d bytes (rows=%d cols=%d samples=%d bitsAllocated=%d frames=%d)",
				ErrorMismatchPixelDataLength, h.vl, expected, rows, cols, samples, bitsAllocated, nFrames))
	}

	if err := writeElementHeader(c.writer, h, h.vl); err != nil {
		return c.fail("writing PixelData header", h, err)
	}

	if bitsAllocated == 1 || !c.swap {
		if err := c.copyValue(int64(h.vl)); err != nil {
			return c.fail("converting PixelData value", h, err)
		}
		return nil
	}

	if err := c.transcodeWords(h, int(sampleWidth), int64(h.vl)-int64(padding)); err != nil {
		return err
	}
	if padding > 0 {
		if err := c.copyValue(int64(padding)); err != nil {
			return c.fail("converting PixelData padding", h, err)
		}
	}
	return nil
}
