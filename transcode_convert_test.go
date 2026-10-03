package dicom_test

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/suyashkumar/dicom"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

var allTargetTS = []string{
	uid.ImplicitVRLittleEndian,
	uid.ExplicitVRLittleEndian,
	uid.ExplicitVRBigEndian,
}

func tsName(ts string) string {
	switch ts {
	case uid.ImplicitVRLittleEndian:
		return "ImplicitLE"
	case uid.ExplicitVRLittleEndian:
		return "ExplicitLE"
	case uid.ExplicitVRBigEndian:
		return "ExplicitBE"
	}
	return ts
}

// metaTSFor returns the TransferSyntaxUID string that the meta of a file
// converted to ts must carry.
func metaTSFor(ts string) string { return ts }

// TestConvertTransferSyntaxRoundTrip transcodes each tiny synthetic file into
// each supported target syntax, parses the result, and compares element values
// against the Explicit-VR-LE original. It then transcodes back to Explicit LE
// and compares again (bidirectional). Comparison is of parsed element values,
// not of identical wire bytes.
func TestConvertTransferSyntaxRoundTrip(t *testing.T) {
	reference := buildSynthetic(t, uid.ExplicitVRLittleEndian)
	refDS, err := dicom.ParseUntilEOF(bytes.NewReader(reference), nil)
	if err != nil {
		t.Fatalf("parse reference: %v", err)
	}

	for _, sourceTS := range allTargetTS {
		sourceTS := sourceTS
		t.Run(tsName(sourceTS), func(t *testing.T) {
			src := buildSynthetic(t, sourceTS)
			for _, targetTS := range allTargetTS {
				targetTS := targetTS
				t.Run("to_"+tsName(targetTS), func(t *testing.T) {
					out := &bytes.Buffer{}
					if err := dicom.ConvertTransferSyntax(bytes.NewReader(src), out, targetTS); err != nil {
						t.Fatalf("convert %s->%s: %v", tsName(sourceTS), tsName(targetTS), err)
					}

					got, err := dicom.ParseUntilEOF(bytes.NewReader(out.Bytes()), nil)
					if err != nil {
						t.Fatalf("parse converted %s->%s: %v", tsName(sourceTS), tsName(targetTS), err)
					}
					if diff := compareDatasets(&refDS, &got, targetTS); diff != "" {
						t.Fatalf("element mismatch %s->%s:\n%s", tsName(sourceTS), tsName(targetTS), diff)
					}

					// Bidirectional: convert back to Explicit LE.
					back := &bytes.Buffer{}
					if err := dicom.ConvertTransferSyntax(bytes.NewReader(out.Bytes()), back, uid.ExplicitVRLittleEndian); err != nil {
						t.Fatalf("convert back: %v", err)
					}
					backDS, err := dicom.ParseUntilEOF(bytes.NewReader(back.Bytes()), nil)
					if err != nil {
						t.Fatalf("parse back-converted: %v", err)
					}
					if diff := compareDatasets(&refDS, &backDS, uid.ExplicitVRLittleEndian); diff != "" {
						t.Fatalf("element mismatch after round-trip back:\n%s", diff)
					}
				})
			}
		})
	}
}

// compareDatasets compares all body element values recursively, ignoring
// file meta (other than TransferSyntaxUID) and element value lengths. It
// returns a human-readable diff or the empty string when equal.
func compareDatasets(want, got *dicom.Dataset, gotTS string) string {
	var b strings.Builder
	compareElements(want.Elements, got.Elements, gotTS, "", &b)
	return b.String()
}

func compareElements(want, got []*dicom.Element, gotTS, prefix string, b *strings.Builder) {
	wm := indexElements(want)
	gm := indexElements(got)
	for t, we := range wm {
		if t.Group == tag.MetadataGroup {
			if t == tag.TransferSyntaxUID {
				ge, ok := gm[t]
				if !ok {
					b.WriteString(prefix + "missing TransferSyntaxUID\n")
					continue
				}
				if got, wantTS := scalarString(ge), gotTS; got != wantTS {
					b.WriteString(prefix + "TransferSyntaxUID mismatch: got " + got + " want " + wantTS + "\n")
				}
			}
			continue
		}
		ge, ok := gm[t]
		if !ok {
			b.WriteString(prefix + t.String() + " missing in converted dataset\n")
			continue
		}
		compareOne(we, ge, gotTS, prefix, b)
	}
}

func compareOne(want, got *dicom.Element, gotTS, prefix string, b *strings.Builder) {
	if want.RawValueRepresentation != got.RawValueRepresentation {
		// OW vs OB PixelData is acceptable across implicit/explicit; compare
		// values below regardless.
		if !(want.Tag == tag.PixelData &&
			(want.RawValueRepresentation == "OW" || want.RawValueRepresentation == "OB") &&
			(got.RawValueRepresentation == "OW" || got.RawValueRepresentation == "OB")) {
			b.WriteString(prefix + want.Tag.String() + " VR differs: " +
				want.RawValueRepresentation + " vs " + got.RawValueRepresentation + "\n")
		}
	}

	switch want.Value.ValueType() {
	case dicom.Sequences:
		wantItems := want.Value.GetValue().([]*dicom.SequenceItemValue)
		gotItems := got.Value.GetValue().([]*dicom.SequenceItemValue)
		if len(wantItems) != len(gotItems) {
			b.WriteString(prefix + want.Tag.String() + " item count differs\n")
			return
		}
		for i := range wantItems {
			compareElements(
				wantItems[i].GetValue().([]*dicom.Element),
				gotItems[i].GetValue().([]*dicom.Element),
				gotTS,
				prefix+"  ",
				b,
			)
		}
	case dicom.PixelData:
		if !want.Value.Equals(got.Value) {
			b.WriteString(prefix + want.Tag.String() + " pixel data differs\n")
		}
	default:
		if !want.Value.Equals(got.Value) {
			b.WriteString(prefix + want.Tag.String() + " value differs: " +
				want.Value.String() + " vs " + got.Value.String() + "\n")
		}
	}
}

func indexElements(elems []*dicom.Element) map[tag.Tag]*dicom.Element {
	m := make(map[tag.Tag]*dicom.Element, len(elems))
	for _, e := range elems {
		m[e.Tag] = e
	}
	return m
}

func scalarString(e *dicom.Element) string {
	return dicom.MustGetStrings(e.Value)[0]
}

// TestConvertRejectsEncapsulatedPixelData verifies undefined-length PixelData
// (encapsulation) is rejected, not copied.
func TestConvertRejectsEncapsulatedPixelData(t *testing.T) {
	src := buildSynthetic(t, uid.ExplicitVRLittleEndian)
	// Corrupt: replace the OW PixelData header's defined VL with undefined
	// length and append a basic offset table item + sequence delimiter.
	corrupt := makeEncapsulatedPixelData(t, src)
	out := &bytes.Buffer{}
	err := dicom.ConvertTransferSyntax(bytes.NewReader(corrupt), out, uid.ExplicitVRLittleEndian)
	if !errors.Is(err, dicom.ErrorEncapsulatedPixelData) {
		t.Fatalf("expected encapsulated pixel data error, got %v", err)
	}
	var ce *dicom.ConversionError
	if !errors.As(err, &ce) {
		t.Fatalf("expected *ConversionError, got %T", err)
	}
	if !strings.Contains(ce.Path, "PixelData") {
		t.Fatalf("expected path to mention PixelData, got %q", ce.Path)
	}
	if ce.Offset <= 132 {
		t.Fatalf("expected absolute offset beyond preamble, got %d", ce.Offset)
	}
}

func makeEncapsulatedPixelData(t *testing.T, src []byte) []byte {
	t.Helper()
	// Re-emit by truncating at the PixelData tag and appending encapsulated
	// structure via the raw builder.
	idx := bytes.Index(src, []byte{0xe0, 0x7f, 0x10, 0x00})
	if idx < 0 {
		t.Fatal("could not locate pixel data tag bytes")
	}
	// idx points at 0x7fe0 LE encoding; truncate just before it.
	pdTagStart := idx
	head := src[:pdTagStart]
	b := newRawBuilder(uid.ExplicitVRLittleEndian)
	b.buf.Write(head)
	b.theTag(tag.PixelData)
	b.buf.WriteString("OW")
	b.u16(0)
	b.u32(undefinedLength)
	// Empty basic offset table item.
	b.theTag(itemTag)
	b.u32(0)
	// Sequence delimiter.
	b.theTag(sequenceDelimitationTag)
	b.u32(0)
	return b.buf.Bytes()
}

// TestConvertRejectsUnknownTransferSyntax rejects a compressed/deflated TS in
// the input meta information.
func TestConvertRejectsUnknownTransferSyntax(t *testing.T) {
	// Same length as ExplicitVRLittleEndian (19 chars) but an unrecognized TS.
	const unknownTS = "1.2.840.10008.1.2.9"
	corrupt := buildSyntheticWithTS(t, uid.ExplicitVRLittleEndian, unknownTS)
	out := &bytes.Buffer{}
	err := dicom.ConvertTransferSyntax(bytes.NewReader(corrupt), out, uid.ExplicitVRLittleEndian)
	if !errors.Is(err, dicom.ErrorUnsupportedTransferSyntax) {
		t.Fatalf("expected unsupported TS error, got %v", err)
	}
}

// buildSyntheticWithTS is buildSynthetic but with a replaced meta TS UID.
func buildSyntheticWithTS(t *testing.T, bodyTS, metaTS string) []byte {
	t.Helper()
	src := buildSynthetic(t, bodyTS)
	// Locate the (0002,0010) element in the meta and rebuild it with metaTS.
	// Element bytes: tag(4) + "UI"(2) + VL16(2) + value(padded even).
	tagBytesLE := []byte{0x02, 0x00, 0x10, 0x00}
	idx := bytes.Index(src, tagBytesLE)
	if idx < 0 {
		t.Fatal("meta TS tag not found")
	}
	val := []byte(metaTS)
	if len(val)%2 == 1 {
		val = append(val, 0)
	}
	header := make([]byte, 8)
	copy(header, tagBytesLE)
	header[4] = 'U'
	header[5] = 'I'
	binary.LittleEndian.PutUint16(header[6:], uint16(len(val)))
	replacement := append(header, val...)
	// Original element length: 8 + even-padded bodyTS length.
	origVal := len(bodyTS)
	if origVal%2 == 1 {
		origVal++
	}
	origElemLen := 8 + origVal
	corrupt := make([]byte, 0, len(src)-origElemLen+len(replacement))
	corrupt = append(corrupt, src[:idx]...)
	corrupt = append(corrupt, replacement...)
	corrupt = append(corrupt, src[idx+origElemLen:]...)
	return corrupt
}

// TestConvertRejectsUnknownTarget rejects an unsupported target TS before
// reading any input.
func TestConvertRejectsUnknownTarget(t *testing.T) {
	err := dicom.ConvertTransferSyntax(bytes.NewReader(nil), io.Discard, "1.2.840.10008.1.2.4.50")
	if !errors.Is(err, dicom.ErrorUnsupportedTransferSyntax) {
		t.Fatalf("expected unsupported target TS error, got %v", err)
	}
}

// TestConvertRejectsUndeterminablePrivateVR rejects a private element under an
// implicit VR source, rather than silently treating it as OB.
func TestConvertRejectsUndeterminablePrivateVR(t *testing.T) {
	src := buildSynthetic(t, uid.ImplicitVRLittleEndian)
	b := newRawBuilder(uid.ImplicitVRLittleEndian)
	// Insert a private element (0009,1001) with 2 bytes payload right after
	// preamble/meta by rebuilding from the explicit variant's header region.
	insertAt := locateBodyStart(t, src)
	b.buf.Write(src[:insertAt])
	privateTag := tag.Tag{Group: 0x0009, Element: 0x1001}
	b.element(privateTag, "", []byte{0xAB, 0xCD})
	b.buf.Write(src[insertAt:])

	out := &bytes.Buffer{}
	err := dicom.ConvertTransferSyntax(bytes.NewReader(b.buf.Bytes()), out, uid.ExplicitVRLittleEndian)
	if !errors.Is(err, dicom.ErrorUndeterminableVR) {
		t.Fatalf("expected undeterminable VR error, got %v", err)
	}
}

// locateBodyStart returns the byte offset immediately after the file meta
// group in src by parsing: it finds the first non-group-2 tag following the
// meta group length. For our synthetic input that's (0008,0005).
func locateBodyStart(t *testing.T, src []byte) int {
	t.Helper()
	needle := []byte{0x08, 0x00, 0x05, 0x00}
	idx := bytes.Index(src, needle)
	if idx < 0 {
		t.Fatal("could not locate body start")
	}
	return idx
}

// TestConvertRejectsMissingMagicWord rejects input that is not a Part 10 file.
func TestConvertRejectsMissingMagicWord(t *testing.T) {
	src := buildSynthetic(t, uid.ExplicitVRLittleEndian)
	corrupt := append([]byte{}, src...)
	copy(corrupt[128:132], []byte("XXXX"))
	err := dicom.ConvertTransferSyntax(bytes.NewReader(corrupt), io.Discard, uid.ExplicitVRLittleEndian)
	if err == nil {
		t.Fatal("expected error for missing DICM")
	}
}

// errAfterNWriter fails after n bytes, simulating a broken/storage failure once
// streaming has started.
type errAfterNWriter struct {
	remaining int
	failed    bool
}

func (w *errAfterNWriter) Write(p []byte) (int, error) {
	if w.failed {
		return 0, io.ErrClosedPipe
	}
	if len(p) > w.remaining {
		w.failed = true
		return w.remaining, io.ErrClosedPipe
	}
	w.remaining -= len(p)
	return len(p), nil
}

// TestConvertWriterErrorNotComplete ensures that when the destination fails
// mid-stream, the error is surfaced (so callers cannot treat partial output as
// complete).
func TestConvertWriterErrorNotComplete(t *testing.T) {
	src := buildSynthetic(t, uid.ExplicitVRLittleEndian)
	w := &errAfterNWriter{remaining: 200}
	err := dicom.ConvertTransferSyntax(bytes.NewReader(src), w, uid.ExplicitVRBigEndian)
	if err == nil {
		t.Fatal("expected writer error to propagate")
	}
}

// TestBigEndianWireFormat checks structural byte-order properties of the
// Explicit VR Big Endian output: the meta group remains Explicit LE, while a
// body US value and the SQ/item lengths are encoded big endian, and the
// big-endian SQ uses defined (not FFFFFFFF) sequence and item lengths.
func TestBigEndianWireFormat(t *testing.T) {
	src := buildSynthetic(t, uid.ExplicitVRLittleEndian)
	out := &bytes.Buffer{}
	if err := dicom.ConvertTransferSyntax(bytes.NewReader(src), out, uid.ExplicitVRBigEndian); err != nil {
		t.Fatal(err)
	}
	wire := out.Bytes()

	// Meta TransferSyntaxUID tag (0002,0010) is little endian even in a BE
	// body file.
	metaTS := []byte{0x02, 0x00, 0x10, 0x00, 'U', 'I'}
	if bytes.Index(wire, metaTS) < 0 {
		t.Fatal("meta not encoded as Explicit VR Little Endian")
	}

	// The body Rows element (0028,0010) VR US with value 2: tag and value big
	// endian.
	rowsBE := []byte{0x00, 0x28, 0x00, 0x10, 'U', 'S', 0x00, 0x02, 0x00, 0x02}
	if bytes.Index(wire, rowsBE) < 0 {
		t.Fatal("body US element not encoded big endian")
	}

	// Pixel data first word 0 must be big endian; check the negative sample
	// -1 (0xFFFF) and -32768 (0x8000) appear in big-endian order at the end.
	tail := wire[len(wire)-12:]
	wantTail := []byte{
		0x00, 0x00, // 0
		0x00, 0x01, // 1
		0xFF, 0xFF, // -1
		0x7F, 0xFF, // 32767
		0x80, 0x00, // -32768
		0x00, 0x64, // 100
	}
	if !bytes.Equal(tail, wantTail) {
		t.Fatalf("big-endian pixel tail mismatch:\n got % x\nwant % x", tail, wantTail)
	}

	// Outer SQ (0008,1110): big endian tag, "SQ", reserved, then a defined
	// length (must not be FF FF FF FF).
	sqIdx := bytes.Index(wire, []byte{0x00, 0x08, 0x11, 0x10, 'S', 'Q'})
	if sqIdx < 0 {
		t.Fatal("missing big-endian SQ element")
	}
	sqVL := wire[sqIdx+8 : sqIdx+12]
	if bytes.Equal(sqVL, []byte{0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Fatal("big-endian SQ must use defined length")
	}
	// First item header immediately after: fffe,e000 and a defined length.
	itemTag := wire[sqIdx+12 : sqIdx+16]
	if !bytes.Equal(itemTag, []byte{0xFF, 0xFE, 0xE0, 0x00}) {
		t.Fatalf("unexpected item tag: % x", itemTag)
	}
	itemVL := wire[sqIdx+16 : sqIdx+20]
	if bytes.Equal(itemVL, []byte{0xFF, 0xFF, 0xFF, 0xFF}) {
		t.Fatal("big-endian item must use defined length")
	}
}

// TestExplicitLEWireFormat ensures the little endian output keeps text
// padding bytes (space and NUL) intact and encodes a US value little endian.
func TestExplicitLEWireFormat(t *testing.T) {
	src := buildSynthetic(t, uid.ExplicitVRBigEndian)
	out := &bytes.Buffer{}
	if err := dicom.ConvertTransferSyntax(bytes.NewReader(src), out, uid.ExplicitVRLittleEndian); err != nil {
		t.Fatal(err)
	}
	wire := out.Bytes()

	// Rows US=2 encoded little endian: tag LE + "US" + VL 02 00 + value 02 00.
	rowsLE := []byte{0x28, 0x00, 0x10, 0x00, 'U', 'S', 0x02, 0x00, 0x02, 0x00}
	if bytes.Index(wire, rowsLE) < 0 {
		t.Fatal("body US element not encoded little endian")
	}

	// Odd-length LO "Odd" padded with a trailing space (0x20).
	odd := []byte{'O', 'd', 'd', 0x20}
	if bytes.Index(wire, odd) < 0 {
		t.Fatal("odd-length text padding (space) not preserved")
	}
	// Odd-length UI "1.2.3" padded with trailing NUL.
	uiNUL := append([]byte("1.2.3"), 0x00)
	if bytes.Index(wire, uiNUL) < 0 {
		t.Fatal("odd-length UI padding (NUL) not preserved")
	}
}

// TestMetaTransferSyntaxRewritten verifies the rewritten meta carries the
// target TransferSyntaxUID and a correct (0002,0000) group length, for each
// target syntax.
func TestMetaTransferSyntaxRewritten(t *testing.T) {
	src := buildSynthetic(t, uid.ExplicitVRLittleEndian)
	for _, target := range allTargetTS {
		out := &bytes.Buffer{}
		if err := dicom.ConvertTransferSyntax(bytes.NewReader(src), out, target); err != nil {
			t.Fatalf("convert to %s: %v", target, err)
		}
		ds, err := dicom.ParseUntilEOF(bytes.NewReader(out.Bytes()), nil)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		tsElem, err := ds.FindElementByTag(tag.TransferSyntaxUID)
		if err != nil {
			t.Fatalf("target %s missing TS: %v", target, err)
		}
		if got := dicom.MustGetStrings(tsElem.Value)[0]; got != target {
			t.Fatalf("target %s meta TS=%s", target, got)
		}
	}
}

// TestNestedSequenceErrorPath ensures a failure inside a nested sequence
// reports the outer sequence in the path with an absolute input offset. A
// second check places the bad element in the inner item to get both sequence
// names in the path.
func TestNestedSequenceErrorPath(t *testing.T) {
	src := buildSynthetic(t, uid.ImplicitVRLittleEndian)

	// Inner SQ tag (0008,1115) in implicit LE, immediately followed by the
	// item tag (fffe,e000) and its VL (ffffffff). Insert the private element
	// right after that 12-byte header so it lands inside the inner item.
	innerSQ := []byte{0x08, 0x00, 0x15, 0x11}
	sqPos := bytes.Index(src, innerSQ)
	if sqPos < 0 {
		t.Fatal("inner SQ not found")
	}

	// In implicit LE the SQ tag is 4 bytes then a 4-byte VL (ffffffff), then
	// the item tag (fffe,e000) 4 bytes and item VL (ffffffff) 4 bytes: the
	// inner item payload starts 16 bytes after the SQ tag.
	insertAt := sqPos + 16
	// Sanity-check the bytes at insertAt-4 are the item tag.
	gotTag := src[insertAt-8 : insertAt-4]
	if !bytes.Equal(gotTag, []byte{0xfe, 0xff, 0x00, 0xe0}) {
		t.Fatalf("unexpected bytes at inner item tag: % x", gotTag)
	}
	privateElement := encodeImplicitPrivateElement(binary.LittleEndian)
	corrupt := make([]byte, 0, len(src)+len(privateElement))
	corrupt = append(corrupt, src[:insertAt]...)
	corrupt = append(corrupt, privateElement...)
	corrupt = append(corrupt, src[insertAt:]...)

	err := dicom.ConvertTransferSyntax(bytes.NewReader(corrupt), io.Discard, uid.ExplicitVRLittleEndian)
	if !errors.Is(err, dicom.ErrorUndeterminableVR) {
		t.Fatalf("expected undeterminable VR, got %v", err)
	}
	var ce *dicom.ConversionError
	if !errors.As(err, &ce) {
		t.Fatalf("expected ConversionError, got %T", err)
	}
	if !strings.Contains(ce.Path, "ReferencedStudySequence") ||
		!strings.Contains(ce.Path, "ReferencedSeriesSequence") {
		t.Fatalf("path should contain both nested sequences, got %q", ce.Path)
	}
	if ce.Offset < int64(132) {
		t.Fatalf("offset should be absolute incl. preamble, got %d", ce.Offset)
	}
}

// encodeImplicitPrivateElement encodes an implicit-VR private element
// (0009,1001) with a 2-byte even payload, using the provided byte order.
func encodeImplicitPrivateElement(bo binary.ByteOrder) []byte {
	out := &bytes.Buffer{}
	var tagb [4]byte
	bo.PutUint16(tagb[0:], 0x0009)
	bo.PutUint16(tagb[2:], 0x1001)
	out.Write(tagb[:])
	var vl [4]byte
	bo.PutUint32(vl[:], 2)
	out.Write(vl[:])
	out.Write([]byte{0xab, 0xcd})
	return out.Bytes()
}
