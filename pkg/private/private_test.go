package private

import (
	"testing"

	"github.com/suyashkumar/dicom/pkg/tag"
)

func TestNormalizeCreator(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"ACME", "ACME"},
		{"ACME  ", "ACME"},
		{"ACME\x00", "ACME"},
		{"ACME \x00 ", "ACME"},
		{" VENDOR ", " VENDOR"}, // leading spaces are significant for LO
	}
	for _, tc := range cases {
		if got := NormalizeCreator(tc.in); got != tc.want {
			t.Errorf("NormalizeCreator(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestIsCreator(t *testing.T) {
	if !IsCreator(tag.Tag{Group: 0x0009, Element: 0x0010}) {
		t.Error("(0009,0010) should be a creator")
	}
	if !IsCreator(tag.Tag{Group: 0x0009, Element: 0x00ff}) {
		t.Error("(0009,00ff) should be a creator")
	}
	if IsCreator(tag.Tag{Group: 0x0009, Element: 0x1000}) {
		t.Error("(0009,1000) should not be a creator")
	}
	if IsCreator(tag.Tag{Group: 0x0008, Element: 0x0010}) {
		t.Error("even-group element should not be a creator")
	}
}

func TestParts(t *testing.T) {
	block, offset, err := Parts(tag.Tag{Group: 0x0009, Element: 0x1010})
	if err != nil || block != 0x10 || offset != 0x10 {
		t.Errorf("Parts((0009,1010)) = (%02x,%02x,%v), want (10,10,nil)", block, offset, err)
	}
	if _, _, err := Parts(tag.Tag{Group: 0x0008, Element: 0x1010}); err == nil {
		t.Error("expected error for even group")
	}
	if _, _, err := Parts(tag.Tag{Group: 0x0009, Element: 0x0010}); err == nil {
		t.Error("expected error for creator element")
	}
}

func TestDictionaryKeyedByCreatorGroupOffset(t *testing.T) {
	d := NewDictionary(map[Key]Definition{
		{Creator: "ACME", Group: 0x0009, Offset: 0x10}: {VR: "US"},
	})
	// The same block+offset under a different creator must not resolve.
	if _, ok := d.Lookup(Key{Creator: "OTHER", Group: 0x0009, Offset: 0x10}); ok {
		t.Error("different creator unexpectedly resolved")
	}
	// Different group, same offset, same creator must not resolve.
	if _, ok := d.Lookup(Key{Creator: "ACME", Group: 0x0011, Offset: 0x10}); ok {
		t.Error("different group unexpectedly resolved")
	}
	// Different intra-block offset must not resolve.
	if _, ok := d.Lookup(Key{Creator: "ACME", Group: 0x0009, Offset: 0x11}); ok {
		t.Error("different offset unexpectedly resolved")
	}
	// Padding in the queried creator normalizes away.
	if _, ok := d.Lookup(Key{Creator: "ACME  ", Group: 0x0009, Offset: 0x10}); !ok {
		t.Error("padded creator failed to resolve")
	}
}

func TestScopeReservationAndLookup(t *testing.T) {
	d := NewDictionary(map[Key]Definition{
		{Creator: "ACME", Group: 0x0009, Offset: 0x10}: {VR: "US", Keyword: "AcmeThing"},
	})
	s := NewScope(d)
	if s.SetCreator(0x10, "ACME  ") != true {
		t.Fatal("expected slot binding")
	}

	def, creator, ok := s.Lookup(tag.Tag{Group: 0x0009, Element: 0x1010})
	if !ok {
		t.Fatal("expected (0009,1010) to resolve")
	}
	if def.VR != "US" || creator != "ACME" {
		t.Errorf("lookup = (%+v, %q), want US/ACME", def, creator)
	}

	// Unknown creator reservation: reserved but absent from the dictionary.
	s.SetCreator(0x11, "VENDOR-X")
	if _, _, ok := s.Lookup(tag.Tag{Group: 0x0009, Element: 0x1110}); ok {
		t.Error("unknown creator unexpectedly resolved")
	}
	if creator, reserved := s.ResolveCreator(tag.Tag{Group: 0x0009, Element: 0x1110}); !reserved || creator != "VENDOR-X" {
		t.Errorf("ResolveCreator = (%q,%v), want VENDOR-X/true", creator, reserved)
	}

	// Unreserved block.
	if _, reserved := s.ResolveCreator(tag.Tag{Group: 0x0009, Element: 0x1210}); reserved {
		t.Error("unreserved block unexpectedly reported reserved")
	}

	// Empty creator does not bind.
	if s.SetCreator(0x12, "  \x00") {
		t.Error("empty creator unexpectedly bound a slot")
	}

	// The same slot may be rebound to a different creator.
	s.SetCreator(0x10, "REPLACEMENT")
	if creator, _ := s.Creator(0x10); creator != "REPLACEMENT" {
		t.Errorf("rebind failed, got %q", creator)
	}
}

func TestScopeIsolation(t *testing.T) {
	d := NewDictionary(map[Key]Definition{
		{Creator: "ACME", Group: 0x0009, Offset: 0x10}: {VR: "US"},
	})
	parent := NewScope(d)
	parent.SetCreator(0x10, "ACME")

	// A fresh child scope must not inherit the parent's reservation.
	child := NewScope(d)
	if _, _, ok := child.Lookup(tag.Tag{Group: 0x0009, Element: 0x1010}); ok {
		t.Error("child scope unexpectedly inherited parent creator reservation")
	}
}
