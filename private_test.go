package dicom

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"

	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
	"github.com/suyashkumar/dicom/pkg/vrraw"
)

// These tests build minimal DICOM datasets by hand so that private block
// ordering, transfer syntaxes, and nested items can be controlled precisely.

const (
	privateGroup = uint16(0x0009)
	creatorElem  = uint16(0x0010)
	privateTag01 = uint16(0x1001)
	privateTag02 = uint16(0x1002)

	creatorAcme = "ACME_INSTRUMENTS"
	creatorBeta = "BETA_SYSTEMS"
)

// enc builds an encoded dataset element. vr=="" produces an Implicit-VR
// element (tag + uint32 VL); otherwise an Explicit-VR element is encoded with
// the appropriate 16- or 32-bit VL form.
func enc(buf *bytes.Buffer, group, element uint16, vr string, value []byte) {
	_ = binary.Write(buf, binary.LittleEndian, group)
	_ = binary.Write(buf, binary.LittleEndian, element)
	if vr != "" {
		buf.WriteString(vr)
		switch vr {
		case "OB", "OD", "OF", "OL", "OW", "SQ", "UN", "UC", "UR", "UT":
			buf.Write([]byte{0, 0})
			_ = binary.Write(buf, binary.LittleEndian, uint32(len(value)))
		default:
			_ = binary.Write(buf, binary.LittleEndian, uint16(len(value)))
		}
	} else {
		_ = binary.Write(buf, binary.LittleEndian, uint32(len(value)))
	}
	buf.Write(value)
}

// encItemStart writes an undefined-length Item tag (fffe,e0ff). Item tags are
// always encoded Implicit: tag + uint32 VL of 0xffffffff.
func encItemStart(buf *bytes.Buffer) {
	_ = binary.Write(buf, binary.LittleEndian, uint16(tag.GroupSeqItem))
	_ = binary.Write(buf, binary.LittleEndian, uint16(0xe000))
	_ = binary.Write(buf, binary.LittleEndian, tag.VLUndefinedLength)
}

// encItemEnd writes an Item Delimitation Item (fffe,e00d, VL=0).
func encItemEnd(buf *bytes.Buffer) {
	_ = binary.Write(buf, binary.LittleEndian, uint16(tag.GroupSeqItem))
	_ = binary.Write(buf, binary.LittleEndian, uint16(0xe00d))
	_ = binary.Write(buf, binary.LittleEndian, uint32(0))
}

// encSeqEnd writes a Sequence Delimitation Item (fffe,e0dd, VL=0).
func encSeqEnd(buf *bytes.Buffer) {
	_ = binary.Write(buf, binary.LittleEndian, uint16(tag.GroupSeqItem))
	_ = binary.Write(buf, binary.LittleEndian, uint16(0xe0dd))
	_ = binary.Write(buf, binary.LittleEndian, uint32(0))
}

func usValue(vals ...uint16) []byte {
	var buf bytes.Buffer
	for _, v := range vals {
		_ = binary.Write(&buf, binary.LittleEndian, v)
	}
	return buf.Bytes()
}

// acmeDictionary returns a dictionary in which both vendors assign the same
// private tag number in the same group but to different fields/VRs.
func acmeBetaDictionary(t *testing.T) *PrivateDictionary {
	t.Helper()
	d := NewPrivateDictionary()
	if err := d.Add(creatorAcme, privateGroup, 0x01, "US", "AcmeDose"); err != nil {
		t.Fatalf("add acme: %v", err)
	}
	if err := d.Add(creatorBeta, privateGroup, 0x01, "CS", "BetaMode"); err != nil {
		t.Fatalf("add beta: %v", err)
	}
	return d
}

// buildFile wraps an encoded body with a preamble, DICM magic, and minimal
// group-2 metadata declaring the given transfer syntax.
func buildFile(t *testing.T, transferSyntax string, body []byte) []byte {
	t.Helper()
	var out bytes.Buffer
	out.Write(make([]byte, 128))
	out.WriteString("DICM")

	// Metadata is always Explicit VR Little Endian.
	var meta bytes.Buffer
	enc(&meta, tag.MediaStorageSOPClassUID.Group, tag.MediaStorageSOPClassUID.Element, "UI", padUID("1.2.840.10008.5.1.4.1.1.2"))
	enc(&meta, tag.MediaStorageSOPInstanceUID.Group, tag.MediaStorageSOPInstanceUID.Element, "UI", padUID("1.2.3.4"))
	enc(&meta, tag.TransferSyntaxUID.Group, tag.TransferSyntaxUID.Element, "UI", padUID(transferSyntax))

	enc(&out, 0x0002, 0x0000, "UL", u32(uint32(meta.Len())))
	out.Write(meta.Bytes())
	out.Write(body)
	return out.Bytes()
}

func padUID(uid string) []byte {
	if len(uid)%2 == 1 {
		return []byte(uid + "\x00")
	}
	return []byte(uid)
}

func u32(v uint32) []byte {
	b := make([]byte, 4)
	binary.LittleEndian.PutUint32(b, v)
	return b
}

// privateElem fetches a top-level private element by tag, failing the test if
// it is absent.
func privateElem(t *testing.T, ds *Dataset, group, element uint16) *Element {
	t.Helper()
	e, err := ds.FindElementByTag(tag.Tag{Group: group, Element: element})
	if err != nil {
		t.Fatalf("missing element (%04x,%04x): %v", group, element, err)
	}
	return e
}

// TestPrivateDictionary_TwoVendorsSameTag verifies that the same uint32 tag
// in two tiny private blocks resolves to a different field/VR based on the
// bound creator, and that the composite key (creator + odd group + block
// offset) is what drives lookup. Both Implicit and Explicit input are covered.
func TestPrivateDictionary_TwoVendorsSameTag(t *testing.T) {
	// The two vendors interpret the same tag differently. Values differ
	// slightly in layout below per VR, but the key point is the dictionary VR
	// (US int vs CS string) is selected by the creator, not the raw tag.
	acmeUS := usValue(0x0135)
	betaCS := []byte{'5', ' '}

	buildBody := func(implicit bool, creator string) []byte {
		var body bytes.Buffer
		if implicit {
			enc(&body, privateGroup, creatorElem, "", padLO(creator))
			val := acmeUS
			if creator == creatorBeta {
				val = betaCS
			}
			enc(&body, privateGroup, privateTag01, "", val)
		} else {
			enc(&body, privateGroup, creatorElem, "LO", padLO(creator))
			// Both files claim US on the wire; under Explicit that is kept,
			// while the dictionary drives the Implicit case.
			enc(&body, privateGroup, privateTag01, "US", acmeUS)
		}
		return body.Bytes()
	}

	d := acmeBetaDictionary(t)

	t.Run("implicit acme interprets as US int", func(t *testing.T) {
		data := buildFile(t, uid.ImplicitVRLittleEndian, buildBody(true, creatorAcme))
		ds, err := Parse(bytes.NewReader(data), int64(len(data)), nil, WithPrivateDictionary(d))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		e := privateElem(t, &ds, privateGroup, privateTag01)
		if e.RawValueRepresentation != "US" {
			t.Fatalf("expected dictionary VR US, got %q", e.RawValueRepresentation)
		}
		if e.Name != "AcmeDose" {
			t.Fatalf("expected name AcmeDose, got %q", e.Name)
		}
		if got := MustGetInts(e.Value); len(got) != 1 || got[0] != 0x0135 {
			t.Fatalf("expected [309], got %v", got)
		}
	})

	t.Run("implicit beta interprets same bytes as CS string", func(t *testing.T) {
		data := buildFile(t, uid.ImplicitVRLittleEndian, buildBody(true, creatorBeta))
		ds, err := Parse(bytes.NewReader(data), int64(len(data)), nil, WithPrivateDictionary(d))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		e := privateElem(t, &ds, privateGroup, privateTag01)
		if e.RawValueRepresentation != "CS" {
			t.Fatalf("expected dictionary VR CS, got %q", e.RawValueRepresentation)
		}
		if e.Name != "BetaMode" {
			t.Fatalf("expected name BetaMode, got %q", e.Name)
		}
		if got := MustGetStrings(e.Value); len(got) != 1 || got[0] != "5" {
			t.Fatalf("expected [\"5\"], got %v", got)
		}
	})

	t.Run("explicit same tag keeps wire VR", func(t *testing.T) {
		data := buildFile(t, uid.ExplicitVRLittleEndian, buildBody(false, creatorAcme))
		ds, err := Parse(bytes.NewReader(data), int64(len(data)), nil, WithPrivateDictionary(d))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		e := privateElem(t, &ds, privateGroup, privateTag01)
		if e.RawValueRepresentation != "US" {
			t.Fatalf("expected wire VR US, got %q", e.RawValueRepresentation)
		}
		if e.Name != "AcmeDose" {
			t.Fatalf("expected known dictionary name, got %q", e.Name)
		}
	})
}

// padLO right-pads an LO value with spaces to an even length, mirroring how a
// Private Creator is normally encoded.
func padLO(s string) []byte {
	for len(s)%2 == 1 {
		s += " "
	}
	return []byte(s)
}

// TestPrivateCreatorScope_NestedItems verifies that every sequence item gets
// its own Private Creator scope: child items do not inherit the parent
// dataset's (or a sibling item's) bindings, and the same slot can be bound to
// a different creator in different items within one file.
func TestPrivateCreatorScope_NestedItems(t *testing.T) {
	d := acmeBetaDictionary(t)
	shared := []byte{0x35, 0x01}

	var body bytes.Buffer

	// Parent dataset binds slot 0x0010 to ACME and defines 0009,1001.
	enc(&body, privateGroup, creatorElem, "LO", padLO(creatorAcme))
	enc(&body, privateGroup, privateTag01, "US", shared)

	// A standard SQ carrying two items and one nested (inner) item.
	_ = binary.Write(&body, binary.LittleEndian, tag.AnatomicRegionSequence.Group)
	_ = binary.Write(&body, binary.LittleEndian, tag.AnatomicRegionSequence.Element)
	body.WriteString("SQ")
	body.Write([]byte{0, 0})
	_ = binary.Write(&body, binary.LittleEndian, tag.VLUndefinedLength)

	// Item 1 binds the same slot to BETA.
	encItemStart(&body)
	enc(&body, privateGroup, creatorElem, "LO", padLO(creatorBeta))
	enc(&body, privateGroup, privateTag01, "CS", shared)

	// Nested item inside Item 1 binds nothing; 0009,1001 must be opaque
	// because it does not inherit Item 1's BETA binding.
	innerSQ := tag.Tag{Group: 0x0040, Element: 0x0275} // RequestAttributesSequence (SQ)
	_ = binary.Write(&body, binary.LittleEndian, innerSQ.Group)
	_ = binary.Write(&body, binary.LittleEndian, innerSQ.Element)
	body.WriteString("SQ")
	body.Write([]byte{0, 0})
	_ = binary.Write(&body, binary.LittleEndian, tag.VLUndefinedLength)
	encItemStart(&body)
	enc(&body, privateGroup, privateTag01, "US", shared) // no creator in this item
	encItemEnd(&body)
	encSeqEnd(&body)

	encItemEnd(&body) // end Item 1

	// Item 2 binds slot 0x0010 back to ACME (same slot, different creator
	// than the previous item).
	encItemStart(&body)
	enc(&body, privateGroup, creatorElem, "LO", padLO(creatorAcme))
	enc(&body, privateGroup, privateTag01, "US", shared)
	encItemEnd(&body)

	encSeqEnd(&body)

	data := buildFile(t, uid.ExplicitVRLittleEndian, body.Bytes())
	ds, err := Parse(bytes.NewReader(data), int64(len(data)), nil, WithPrivateDictionary(d))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// Parent: ACME US.
	parent := privateElem(t, &ds, privateGroup, privateTag01)
	if parent.RawValueRepresentation != "US" || parent.Name != "AcmeDose" {
		t.Fatalf("parent: got vr=%q name=%q", parent.RawValueRepresentation, parent.Name)
	}

	sq, err := ds.FindElementByTag(tag.AnatomicRegionSequence)
	if err != nil {
		t.Fatalf("find outer sq: %v", err)
	}
	outerItems := sq.Value.GetValue().([]*SequenceItemValue)
	if len(outerItems) != 2 {
		t.Fatalf("expected 2 outer items, got %d", len(outerItems))
	}

	// Item 1: BETA CS.
	item1Elem := findItemElem(t, outerItems[0].elements, privateGroup, privateTag01)
	if item1Elem.RawValueRepresentation != "CS" || item1Elem.Name != "BetaMode" {
		t.Fatalf("item1: got vr=%q name=%q", item1Elem.RawValueRepresentation, item1Elem.Name)
	}

	// Nested item: no inherited binding -> opaque UN bytes.
	innerSqElem := findItemElem(t, outerItems[0].elements, innerSQ.Group, innerSQ.Element)
	innerItems := innerSqElem.Value.GetValue().([]*SequenceItemValue)
	if len(innerItems) != 1 {
		t.Fatalf("expected 1 inner item, got %d", len(innerItems))
	}
	nested := findItemElem(t, innerItems[0].elements, privateGroup, privateTag01)
	if nested.RawValueRepresentation != vrraw.Unknown {
		t.Fatalf("nested: expected opaque UN, got vr=%q", nested.RawValueRepresentation)
	}
	if nested.Name != "" {
		t.Fatalf("nested: expected no fabricated name, got %q", nested.Name)
	}
	if !bytes.Equal(MustGetBytes(nested.Value), shared) {
		t.Fatalf("nested: raw bytes not preserved")
	}

	// Item 2: rebound to ACME US.
	item2Elem := findItemElem(t, outerItems[1].elements, privateGroup, privateTag01)
	if item2Elem.RawValueRepresentation != "US" || item2Elem.Name != "AcmeDose" {
		t.Fatalf("item2: got vr=%q name=%q", item2Elem.RawValueRepresentation, item2Elem.Name)
	}
}

func findItemElem(t *testing.T, elems []*Element, group, element uint16) *Element {
	t.Helper()
	want := tag.Tag{Group: group, Element: element}
	for _, e := range elems {
		if e.Tag == want {
			return e
		}
	}
	t.Fatalf("item missing element %s", want.String())
	return nil
}

// conflictBody encodes an Explicit-VR private element whose on-wire VR (CS)
// disagrees with the dictionary VR (US).
func conflictBody() []byte {
	var body bytes.Buffer
	enc(&body, privateGroup, creatorElem, "LO", padLO(creatorAcme))
	enc(&body, privateGroup, privateTag01, "CS", []byte{'5', ' '})
	return body.Bytes()
}

// TestPrivateVRConflict_Lenient verifies that by default the on-wire VR is
// retained and the conflict is reported to the diagnostic handler.
func TestPrivateVRConflict_Lenient(t *testing.T) {
	d := acmeBetaDictionary(t)
	data := buildFile(t, uid.ExplicitVRLittleEndian, conflictBody())

	var reported []PrivateVRConflict
	ds, err := Parse(bytes.NewReader(data), int64(len(data)), nil,
		WithPrivateDictionary(d),
		PrivateVRConflictHandler(func(c PrivateVRConflict) { reported = append(reported, c) }),
	)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(reported) != 1 {
		t.Fatalf("expected 1 conflict report, got %d", len(reported))
	}
	c := reported[0]
	if c.WireVR != "CS" || c.DictionaryVR != "US" || c.Creator != creatorAcme {
		t.Fatalf("unexpected conflict: %+v", c)
	}
	e := privateElem(t, &ds, privateGroup, privateTag01)
	if e.RawValueRepresentation != "CS" {
		t.Fatalf("expected wire VR CS retained, got %q", e.RawValueRepresentation)
	}
	if got := MustGetStrings(e.Value); len(got) != 1 || got[0] != "5" {
		t.Fatalf("expected [\"5\"], got %v", got)
	}
}

// TestPrivateVRConflict_Reject verifies that strict configuration turns the
// conflict into a parse error carrying PrivateVRConflict.
func TestPrivateVRConflict_Reject(t *testing.T) {
	d := acmeBetaDictionary(t)
	data := buildFile(t, uid.ExplicitVRLittleEndian, conflictBody())

	_, err := Parse(bytes.NewReader(data), int64(len(data)), nil,
		WithPrivateDictionary(d),
		RejectPrivateVRConflicts(),
	)
	if err == nil {
		t.Fatal("expected parse error, got nil")
	}
	var conflict PrivateVRConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("expected PrivateVRConflict in error chain, got %v", err)
	}
	if conflict.WireVR != "CS" || conflict.DictionaryVR != "US" {
		t.Fatalf("unexpected conflict: %+v", conflict)
	}
}

// TestPrivateCreator_PaddingNormalization verifies that a creator padded with
// trailing spaces (or NULs) still matches a dictionary key supplied without
// padding.
func TestPrivateCreator_PaddingNormalization(t *testing.T) {
	d := acmeBetaDictionary(t)
	if got := NormalizePrivateCreator(creatorAcme + "  \x00"); got != creatorAcme {
		t.Fatalf("normalize: got %q", got)
	}

	var body bytes.Buffer
	enc(&body, privateGroup, creatorElem, "LO", []byte(creatorAcme+"  \x00"))
	enc(&body, privateGroup, privateTag01, "US", usValue(0x0135))
	data := buildFile(t, uid.ExplicitVRLittleEndian, body.Bytes())

	ds, err := Parse(bytes.NewReader(data), int64(len(data)), nil, WithPrivateDictionary(d))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	e := privateElem(t, &ds, privateGroup, privateTag01)
	if e.Name != "AcmeDose" || e.RawValueRepresentation != "US" {
		t.Fatalf("padded creator failed to resolve: vr=%q name=%q", e.RawValueRepresentation, e.Name)
	}
}

// TestPrivateElement_UnknownCreatorIsRaw verifies that a private data element
// with no matching creator is kept as opaque bytes with no fabricated VR/name,
// and that supplying no dictionary at all behaves the same way.
func TestPrivateElement_UnknownCreatorIsRaw(t *testing.T) {
	raw := []byte{0xde, 0xad, 0xbe, 0xef}
	mk := func(t *testing.T) []byte {
		t.Helper()
		var body bytes.Buffer
		enc(&body, privateGroup, privateTag02, "US", raw)
		return buildFile(t, uid.ExplicitVRLittleEndian, body.Bytes())
	}

	t.Run("with dictionary but unbound creator", func(t *testing.T) {
		data := mk(t)
		ds, err := Parse(bytes.NewReader(data), int64(len(data)), nil, WithPrivateDictionary(acmeBetaDictionary(t)))
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		e := privateElem(t, &ds, privateGroup, privateTag02)
		if e.RawValueRepresentation != vrraw.Unknown || e.Name != "" {
			t.Fatalf("expected UN/unnamed, got vr=%q name=%q", e.RawValueRepresentation, e.Name)
		}
		if !bytes.Equal(MustGetBytes(e.Value), raw) {
			t.Fatalf("raw bytes mismatch")
		}
	})

	t.Run("no dictionary at all", func(t *testing.T) {
		data := mk(t)
		ds, err := Parse(bytes.NewReader(data), int64(len(data)), nil)
		if err != nil {
			t.Fatalf("parse: %v", err)
		}
		e := privateElem(t, &ds, privateGroup, privateTag02)
		if e.RawValueRepresentation != vrraw.Unknown || e.Name != "" {
			t.Fatalf("expected UN/unnamed, got vr=%q name=%q", e.RawValueRepresentation, e.Name)
		}
		if !bytes.Equal(MustGetBytes(e.Value), raw) {
			t.Fatalf("raw bytes mismatch")
		}
	})
}

// TestPrivateElement_UnknownCreatorVLForm verifies that an unbound private
// element encoded on the wire with a short-form VR (16-bit VL) is still held
// as opaque UN bytes without corrupting the stream position, since the VL must
// be read using the on-wire VR even though interpretation falls back to UN.
func TestPrivateElement_UnknownCreatorVLForm(t *testing.T) {
	var body bytes.Buffer
	enc(&body, privateGroup, privateTag01, "US", usValue(0x0135)) // unbound, short VR form
	enc(&body, tag.PatientName.Group, tag.PatientName.Element, "PN", []byte("Doe^John "))
	data := buildFile(t, uid.ExplicitVRLittleEndian, body.Bytes())

	ds, err := Parse(bytes.NewReader(data), int64(len(data)), nil, WithPrivateDictionary(acmeBetaDictionary(t)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	priv := privateElem(t, &ds, privateGroup, privateTag01)
	if priv.RawValueRepresentation != vrraw.Unknown {
		t.Fatalf("expected UN, got %q", priv.RawValueRepresentation)
	}
	if !bytes.Equal(MustGetBytes(priv.Value), usValue(0x0135)) {
		t.Fatalf("opaque bytes mismatch")
	}
	// Stream must remain aligned so the following standard element parses.
	pn, err := ds.FindElementByTag(tag.PatientName)
	if err != nil {
		t.Fatalf("patient name not parsed (stream misaligned): %v", err)
	}
	if got := MustGetStrings(pn.Value); len(got) != 1 || got[0] != "Doe^John" {
		t.Fatalf("unexpected patient name: %v", got)
	}
}

// TestPrivateRoundTrip verifies that the dictionary affects interpretation
// only: writing a parsed dataset back out preserves the original private tag
// numbers (no renumbering) and the creator/data bytes. It runs under both
// Implicit and Explicit transfer syntaxes.
func TestPrivateRoundTrip(t *testing.T) {
	d := acmeBetaDictionary(t)

	for _, tc := range []struct {
		name     string
		ts       string
		implicit bool
	}{
		{name: "explicit", ts: uid.ExplicitVRLittleEndian, implicit: false},
		{name: "implicit", ts: uid.ImplicitVRLittleEndian, implicit: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var body bytes.Buffer
			if tc.implicit {
				enc(&body, privateGroup, creatorElem, "", []byte(creatorAcme))
				enc(&body, privateGroup, privateTag01, "", usValue(0x0135))
				// Unbound creator for a second block -> opaque bytes.
				enc(&body, privateGroup, privateTag02, "", []byte{1, 2, 3, 4})
			} else {
				enc(&body, privateGroup, creatorElem, "LO", []byte(creatorAcme))
				enc(&body, privateGroup, privateTag01, "US", usValue(0x0135))
				enc(&body, privateGroup, privateTag02, "UN", []byte{1, 2, 3, 4})
			}
			original := buildFile(t, tc.ts, body.Bytes())

			ds, err := Parse(bytes.NewReader(original), int64(len(original)), nil, WithPrivateDictionary(d))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			// Sanity: the known element was interpreted via the dictionary.
			known := privateElem(t, &ds, privateGroup, privateTag01)
			if known.Name != "AcmeDose" || known.RawValueRepresentation != "US" {
				t.Fatalf("known: vr=%q name=%q", known.RawValueRepresentation, known.Name)
			}

			var out bytes.Buffer
			if err := Write(&out, ds); err != nil {
				t.Fatalf("write: %v", err)
			}

			// Reparse the output without a dictionary, then confirm the wire
			// tag numbers and values are byte-for-byte intact.
			reparsed, err := Parse(bytes.NewReader(out.Bytes()), int64(out.Len()), nil)
			if err != nil {
				t.Fatalf("reparse: %v", err)
			}

			creatorOut := privateElem(t, &reparsed, privateGroup, creatorElem)
			if got := MustGetStrings(creatorOut.Value); len(got) != 1 ||
				NormalizePrivateCreator(got[0]) != creatorAcme {
				t.Fatalf("creator changed: %v", got)
			}

			// 0009,1001 must remain 0009,1001 (not renumbered), bytes intact.
			tag1 := privateElem(t, &reparsed, privateGroup, privateTag01)
			if tag1.Tag != (tag.Tag{Group: privateGroup, Element: privateTag01}) {
				t.Fatalf("tag number changed: %s", tag1.Tag)
			}
			// Reparsed without a dictionary, the element is opaque UN bytes,
			// but those bytes must equal the originally encoded US value.
			if !bytes.Equal(MustGetBytes(tag1.Value), usValue(0x0135)) {
				t.Fatalf("known value bytes changed: vr=%s value=%v", tag1.RawValueRepresentation, tag1.Value)
			}

			// 0009,1002 remains an opaque unknown block with the same bytes.
			tag2 := privateElem(t, &reparsed, privateGroup, privateTag02)
			if tag2.Tag != (tag.Tag{Group: privateGroup, Element: privateTag02}) {
				t.Fatalf("tag2 number changed: %s", tag2.Tag)
			}
			if !bytes.Equal(MustGetBytes(tag2.Value), []byte{1, 2, 3, 4}) {
				t.Fatalf("unknown bytes changed: %v", tag2.Value)
			}
		})
	}
}

// TestPrivateDictionary_Validation covers the composite-key construction rules.
func TestPrivateDictionary_Validation(t *testing.T) {
	d := NewPrivateDictionary()
	if err := d.Add(creatorAcme, 0x0008, 0x01, "US", "X"); !errors.Is(err, ErrorInvalidPrivateGroup) {
		t.Fatalf("expected ErrorInvalidPrivateGroup, got %v", err)
	}
	if err := d.Add(creatorAcme, privateGroup, 0x01, "USA", "X"); !errors.Is(err, ErrorInvalidPrivateVR) {
		t.Fatalf("expected ErrorInvalidPrivateVR, got %v", err)
	}
	if err := d.Add(" "+creatorAcme+" ", privateGroup, 0x01, "us", "X"); err != nil {
		t.Fatalf("valid add failed: %v", err)
	}
	// VR is upper-cased; only trailing (not leading) padding is trimmed.
	e, ok := d.lookup(PrivateTagKey{Creator: " " + creatorAcme, Group: privateGroup, Offset: 0x01})
	if !ok {
		t.Fatal("normalized key lookup failed")
	}
	if e.VR != "US" {
		t.Fatalf("expected uppercase VR, got %q", e.VR)
	}
}
