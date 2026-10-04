package dicom

import (
	"bytes"
	"errors"
	"testing"

	"github.com/suyashkumar/dicom/pkg/private"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// buildImplicitPrivateFile constructs a minimal Implicit-VR LE DICOM with two
// known private blocks at the top level, an unknown-creator private block, and
// a standard sequence whose two items each bind the same slot (0x10) to a
// different creator. This checks per-item creator scope isolation.
func buildImplicitPrivateFile(t *testing.T) []byte {
	t.Helper()
	b := &privateBuilder{}
	b.meta(uid.ImplicitVRLittleEndian)

	// Top-level private blocks (implicit VR).
	b.implicitElem(creatorAcmeTag, loBytes(testCreatorAcme))
	b.implicitElem(acmeDataTag, usBytes(0x1234))

	// Trailing-NUL (instead of space) padding: normalization must still bind.
	// Use space padding on the wire here so the value round-trips byte-for-byte
	// (the writer always pads LO with spaces); NUL-padding binding is covered
	// by TestNormalizeCreator and the dictionary scope unit tests.
	b.implicitElem(creatorOtherTag, loBytes(testCreatorOther))
	b.implicitElem(otherDataTag, loBytes("HELLO"))

	// Unknown creator: its data element has no dictionary entry and must stay
	// raw bytes (UN), never a fabricated field name/typed value.
	b.implicitElem(creatorMysteryTag, loBytes(testCreatorMystery))
	b.implicitElem(mysteryDataTag, []byte{0x01, 0x02, 0x03, 0x04})

	// Sequence with two items; each item gets its OWN private scope.
	b.implicitSQHeader(sequenceTag)

	// Item 1: slot 0x10 -> ACME, so (0009,1010) resolves as US.
	b.beginItem()
	b.implicitElem(creatorAcmeTag, loBytes(testCreatorAcme))
	b.implicitElem(acmeDataTag, usBytes(0x0064)) // 100
	b.endItem()

	// Item 2: same slot 0x10 -> OTHER, so (0009,1010) does NOT resolve
	// through ACME; OTHER defines offset 0x20, which item 2 uses as LO.
	b.beginItem()
	// Rebind slot 0x10 (used by ACME in item 1) to a creator with no
	// dictionary entry for offset 0x10, and reserve OTHER in its own slot
	// 0x11. Child items start with empty slots regardless of item 1.
	b.implicitElem(creatorAcmeTag, loBytes(testCreatorMystery))
	b.implicitElem(creatorOtherTag, loBytes(testCreatorOther))
	// Same numeric tag as item 1's US element, but under a different creator:
	// it must be retained as raw bytes, not reinterpreted as item 1's field.
	b.implicitElem(acmeDataTag, []byte{0xab, 0xcd})
	b.implicitElem(otherDataTag, loBytes("ITEM2"))
	b.endItem()

	b.endSequence()
	return b.Bytes()
}

func itemElements(t *testing.T, ds *Dataset, seqTag tag.Tag, idx int) []*Element {
	t.Helper()
	seq, err := ds.FindElementByTag(seqTag)
	if err != nil {
		t.Fatalf("finding sequence: %v", err)
	}
	items := seq.Value.GetValue().([]*SequenceItemValue)
	if idx >= len(items) {
		t.Fatalf("wanted item %d, sequence only has %d", idx, len(items))
	}
	return items[idx].GetValue().([]*Element)
}

func findIn(elems []*Element, t tag.Tag) *Element {
	for _, e := range elems {
		if e.Tag == t {
			return e
		}
	}
	return nil
}

func TestPrivate_ImplicitScopedInterpretation(t *testing.T) {
	data := buildImplicitPrivateFile(t)
	ds, err := ParseUntilEOF(bytes.NewReader(data), nil, WithPrivateDictionary(testPrivateDictionary()))
	if err != nil {
		t.Fatalf("ParseUntilEOF: %v", err)
	}

	// ACME block interpreted as US.
	acme, err := ds.FindElementByTag(acmeDataTag)
	if err != nil {
		t.Fatalf("find acme data: %v", err)
	}
	if acme.RawValueRepresentation != "US" || acme.Value.ValueType() != Ints {
		t.Fatalf("acme element VR=%s type=%v, want US/Ints", acme.RawValueRepresentation, acme.Value.ValueType())
	}
	if got := MustGetInts(acme.Value); len(got) != 1 || got[0] != 0x1234 {
		t.Errorf("acme value = %v, want [4660]", got)
	}

	// OTHER block interpreted as LO.
	other, err := ds.FindElementByTag(otherDataTag)
	if err != nil {
		t.Fatalf("find other data: %v", err)
	}
	if other.RawValueRepresentation != "LO" || other.Value.ValueType() != Strings {
		t.Fatalf("other element VR=%s type=%v, want LO/Strings", other.RawValueRepresentation, other.Value.ValueType())
	}
	if got := MustGetStrings(other.Value); len(got) != 1 || got[0] != "HELLO" {
		t.Errorf("other value = %v, want [HELLO]", got)
	}

	// Unknown creator remains raw bytes under UN.
	mystery, err := ds.FindElementByTag(mysteryDataTag)
	if err != nil {
		t.Fatalf("find mystery data: %v", err)
	}
	if mystery.RawValueRepresentation != tag.UnknownVR || mystery.Value.ValueType() != Bytes {
		t.Fatalf("mystery element VR=%s type=%v, want UN/Bytes", mystery.RawValueRepresentation, mystery.Value.ValueType())
	}
	if got := MustGetBytes(mystery.Value); !bytes.Equal(got, []byte{0x01, 0x02, 0x03, 0x04}) {
		t.Errorf("mystery raw bytes = %v", got)
	}

	// Private creators are LO strings with normalized values.
	creatorAcme, err := ds.FindElementByTag(creatorAcmeTag)
	if err != nil {
		t.Fatalf("find creator acme: %v", err)
	}
	if creatorAcme.RawValueRepresentation != "LO" {
		t.Errorf("creator VR=%s, want LO", creatorAcme.RawValueRepresentation)
	}
	if got := MustGetStrings(creatorAcme.Value)[0]; got != testCreatorAcme {
		t.Errorf("creator value = %q, want %q", got, testCreatorAcme)
	}

	// Sequence item isolation.
	item1 := itemElements(t, &ds, sequenceTag, 0)
	item2 := itemElements(t, &ds, sequenceTag, 1)

	e1 := findIn(item1, acmeDataTag)
	if e1 == nil || e1.RawValueRepresentation != "US" || MustGetInts(e1.Value)[0] != 100 {
		t.Errorf("item1 acme slot not US=100: %+v", e1)
	}

	// Item 2 rebound slot 0x10 to MYSTERY: (0009,1010) must NOT be US; it has
	// no dictionary entry under MYSTERY and stays raw UN bytes.
	e2Tag := findIn(item2, acmeDataTag)
	if e2Tag == nil {
		t.Fatalf("expected raw (0009,1010) element in item2")
	}
	if e2Tag.RawValueRepresentation != tag.UnknownVR {
		t.Errorf("item2 (0009,1010) VR=%s, want UN (slot rebound to OTHER)", e2Tag.RawValueRepresentation)
	}

	e2Other := findIn(item2, otherDataTag)
	if e2Other == nil || e2Other.RawValueRepresentation != "LO" || MustGetStrings(e2Other.Value)[0] != "ITEM2" {
		t.Errorf("item2 other block not LO=ITEM2: %+v", e2Other)
	}

	// The parent-scope bindings must be unaffected by the child items.
	if got := MustGetInts(acme.Value)[0]; got != 0x1234 {
		t.Errorf("top-level acme value changed after parsing items: %v", got)
	}
}

// Without a dictionary, all private data elements remain UN raw bytes, but
// private creator elements are still LO.
// A creator padded with a trailing NUL must still bind and resolve stably.
func TestPrivate_NULPaddedCreatorBinds(t *testing.T) {
	b := &privateBuilder{}
	b.meta(uid.ImplicitVRLittleEndian)
	b.implicitElem(creatorAcmeTag, append([]byte(testCreatorAcme), 0x00))
	b.implicitElem(acmeDataTag, usBytes(7))

	ds, err := ParseUntilEOF(bytes.NewReader(b.Bytes()), nil, WithPrivateDictionary(testPrivateDictionary()))
	if err != nil {
		t.Fatalf("ParseUntilEOF: %v", err)
	}
	acme, err := ds.FindElementByTag(acmeDataTag)
	if err != nil {
		t.Fatalf("find acme: %v", err)
	}
	if acme.RawValueRepresentation != "US" || MustGetInts(acme.Value)[0] != 7 {
		t.Errorf("NUL-padded creator did not bind/resolves: %+v", acme)
	}
	creator, err := ds.FindElementByTag(creatorAcmeTag)
	if err != nil {
		t.Fatalf("find creator: %v", err)
	}
	if got := MustGetStrings(creator.Value)[0]; got != testCreatorAcme {
		t.Errorf("creator normalized value = %q, want %q", got, testCreatorAcme)
	}
}

func TestPrivate_NoDictionaryFallsBackToRaw(t *testing.T) {
	data := buildImplicitPrivateFile(t)
	ds, err := ParseUntilEOF(bytes.NewReader(data), nil)
	if err != nil {
		t.Fatalf("ParseUntilEOF: %v", err)
	}
	acme, err := ds.FindElementByTag(acmeDataTag)
	if err != nil {
		t.Fatalf("find acme data: %v", err)
	}
	if acme.RawValueRepresentation != tag.UnknownVR || acme.Value.ValueType() != Bytes {
		t.Errorf("no-dict acme VR=%s type=%v, want UN/Bytes", acme.RawValueRepresentation, acme.Value.ValueType())
	}
	creator, err := ds.FindElementByTag(creatorOtherTag)
	if err != nil {
		t.Fatalf("find creator: %v", err)
	}
	if creator.RawValueRepresentation != "LO" {
		t.Errorf("creator VR=%s, want LO even without dictionary", creator.RawValueRepresentation)
	}
}

func buildExplicitPrivateFile(t *testing.T, conflictVR string) []byte {
	t.Helper()
	b := &privateBuilder{}
	b.meta(uid.ExplicitVRLittleEndian)

	b.explicitElem(creatorAcmeTag, "LO", loBytes(testCreatorAcme))
	// On-wire VR may agree or disagree with the dictionary (US).
	b.explicitElem(acmeDataTag, conflictVR, usBytes(0x1234))

	b.explicitElem(creatorOtherTag, "LO", loBytes(testCreatorOther))
	b.explicitElem(otherDataTag, "LO", loBytes("HELLO"))
	return b.Bytes()
}

func TestPrivate_ExplicitConflictKeepsWireVRByDefault(t *testing.T) {
	data := buildExplicitPrivateFile(t, "SS") // dictionary says US, wire says SS
	p, err := NewParser(bytes.NewReader(data), int64(len(data)), nil, WithPrivateDictionary(testPrivateDictionary()))
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	var ds Dataset
	for p.reader.moreToRead() {
		elem, err := p.Next()
		if err != nil {
			t.Fatalf("Next: %v", err)
		}
		ds.Elements = append(ds.Elements, elem)
	}

	acme, err := ds.FindElementByTag(acmeDataTag)
	if err != nil {
		t.Fatalf("find acme: %v", err)
	}
	if acme.RawValueRepresentation != "SS" {
		t.Errorf("expected on-wire SS retained, got %s", acme.RawValueRepresentation)
	}

	conflicts := p.PrivateVRConflicts()
	if len(conflicts) != 1 {
		t.Fatalf("expected 1 conflict diagnostic, got %d", len(conflicts))
	}
	c := conflicts[0]
	if c.Tag != acmeDataTag || c.WireVR != "SS" || c.DictionaryVR != "US" ||
		c.DictionaryKey.Creator != testCreatorAcme || c.DictionaryKey.Offset != 0x10 {
		t.Errorf("unexpected conflict: %+v", c)
	}
}

func TestPrivate_ExplicitConflictRejectsWhenConfigured(t *testing.T) {
	data := buildExplicitPrivateFile(t, "SS")
	_, err := ParseUntilEOF(bytes.NewReader(data), nil,
		WithPrivateDictionary(testPrivateDictionary()), RejectOnPrivateVRConflict())
	if err == nil {
		t.Fatal("expected parse error on VR conflict, got nil")
	}
	var conflict *PrivateVRConflict
	if !errors.As(err, &conflict) {
		t.Fatalf("expected *PrivateVRConflict, got %v", err)
	}
	if conflict.WireVR != "SS" || conflict.DictionaryVR != "US" {
		t.Errorf("unexpected conflict: %+v", conflict)
	}
}

func TestPrivate_ExplicitAgreementNoConflict(t *testing.T) {
	data := buildExplicitPrivateFile(t, "US")
	p, err := NewParser(bytes.NewReader(data), int64(len(data)), nil, WithPrivateDictionary(testPrivateDictionary()))
	if err != nil {
		t.Fatalf("NewParser: %v", err)
	}
	for p.reader.moreToRead() {
		if _, err := p.Next(); err != nil {
			t.Fatalf("Next: %v", err)
		}
	}
	if got := len(p.PrivateVRConflicts()); got != 0 {
		t.Errorf("expected no conflicts, got %d", got)
	}
}

// TestPrivate_SameSlotDifferentCreatorAcrossItems verifies that the same slot
// number (0x10), bound to different creators in two sequence items, makes the
// same numeric private tag resolve differently per item.
func TestPrivate_SameSlotDifferentCreatorAcrossItems(t *testing.T) {
	dict := private.NewDictionary(map[private.Key]private.Definition{
		{Creator: "VENDORA", Group: testPrivateGroup, Offset: 0x40}: {VR: "US"},
		{Creator: "VENDORB", Group: testPrivateGroup, Offset: 0x40}: {VR: "LO"},
	})
	sharedTag := tag.Tag{Group: testPrivateGroup, Element: 0x1040}

	b := &privateBuilder{}
	b.meta(uid.ImplicitVRLittleEndian)
	b.implicitSQHeader(sequenceTag)

	b.beginItem()
	b.implicitElem(creatorAcmeTag, loBytes("VENDORA"))
	b.implicitElem(sharedTag, usBytes(42))
	b.endItem()

	b.beginItem()
	b.implicitElem(creatorAcmeTag, loBytes("VENDORB"))
	b.implicitElem(sharedTag, loBytes("FORTYTWO"))
	b.endItem()

	b.endSequence()

	ds, err := ParseUntilEOF(bytes.NewReader(b.Bytes()), nil, WithPrivateDictionary(dict))
	if err != nil {
		t.Fatalf("ParseUntilEOF: %v", err)
	}

	item1 := findIn(itemElements(t, &ds, sequenceTag, 0), sharedTag)
	item2 := findIn(itemElements(t, &ds, sequenceTag, 1), sharedTag)
	if item1.RawValueRepresentation != "US" || MustGetInts(item1.Value)[0] != 42 {
		t.Errorf("item1 shared tag = VR %s value %v, want US [42]", item1.RawValueRepresentation, item1.Value)
	}
	if item2.RawValueRepresentation != "LO" || MustGetStrings(item2.Value)[0] != "FORTYTWO" {
		t.Errorf("item2 shared tag = VR %s value %v, want LO [FORTYTWO]", item2.RawValueRepresentation, item2.Value)
	}
	if item1.Tag != item2.Tag {
		t.Errorf("numeric private tags must be preserved identically: %v vs %v", item1.Tag, item2.Tag)
	}
}

// TestPrivate_RoundTrip writes a parsed dataset (implicit and explicit) back
// out and re-parses. Private tag numbers must be preserved exactly, and the
// values must round-trip.
func TestPrivate_RoundTrip(t *testing.T) {
	cases := []struct {
		name  string
		ts    string
		build func(t *testing.T) []byte
	}{
		{"implicit", uid.ImplicitVRLittleEndian, buildImplicitPrivateFile},
		{"explicit", uid.ExplicitVRLittleEndian, func(t *testing.T) []byte {
			return buildExplicitPrivateFile(t, "US")
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			in := tc.build(t)
			ds, err := ParseUntilEOF(bytes.NewReader(in), nil, WithPrivateDictionary(testPrivateDictionary()))
			if err != nil {
				t.Fatalf("parse: %v", err)
			}

			var out bytes.Buffer
			if err := Write(&out, ds); err != nil {
				t.Fatalf("write: %v", err)
			}

			reparsed, err := ParseUntilEOF(bytes.NewReader(out.Bytes()), nil, WithPrivateDictionary(testPrivateDictionary()))
			if err != nil {
				t.Fatalf("reparse: %v", err)
			}

			if !ds.Equals(&reparsed) {
				for i := range ds.Elements {
					if !ds.Elements[i].Equals(reparsed.Elements[i]) {
						t.Logf("differing element %d:\nbefore=%+v\nafter=%+v", i, ds.Elements[i], reparsed.Elements[i])
					}
				}
				t.Errorf("round-trip dataset mismatch.\nbefore:\n%v\nafter:\n%v", ds.String(), reparsed.String())
			}

			// Private tag numbers must be unchanged.
			for _, want := range []tag.Tag{acmeDataTag, otherDataTag, creatorAcmeTag, creatorOtherTag} {
				if _, err := reparsed.FindElementByTag(want); err != nil {
					t.Errorf("tag %v missing after round-trip", want)
				}
			}
		})
	}
}
