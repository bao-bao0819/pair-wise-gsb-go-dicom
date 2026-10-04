package dicom

import (
	"errors"
	"fmt"
	"strings"

	"github.com/suyashkumar/dicom/pkg/tag"
)

var (
	// ErrorInvalidPrivateGroup indicates that a private dictionary operation
	// was attempted using an even (non-private) group number. Private elements
	// always live in odd-numbered groups.
	ErrorInvalidPrivateGroup = errors.New("private tags must use an odd group number")
	// ErrorInvalidPrivateVR indicates that an entry was added to a private
	// dictionary with a VR that is not exactly two characters long.
	ErrorInvalidPrivateVR = errors.New("private dictionary VR must be exactly two characters long")
)

// PrivateTagKey uniquely identifies a private data element. It is intentionally
// not just the raw uint32 tag: the same private tag number can name a different
// field in data produced by different vendors, so the resolved Private Creator
// string and the containing odd group are required in addition to the element's
// intra-block offset (the low byte of the element number).
type PrivateTagKey struct {
	// Creator is the normalized Private Creator string, e.g. "MY_COMPANY".
	Creator string
	// Group is the containing odd group number, e.g. 0x0009.
	Group uint16
	// Offset is the element's intra-block offset, i.e. the low byte of the
	// (gggg,xxyy) element number (yy), in the range 0x00-0xff.
	Offset uint8
}

// PrivateTagEntry describes a single element known via a caller-supplied
// private dictionary.
type PrivateTagEntry struct {
	// VR is the Value Representation for this element, e.g. "US" or "CS".
	VR string
	// Name is an optional human-readable/keyword identifier, e.g. "AcmeDose".
	// It is used only for display; the wire tag is never renumbered.
	Name string
}

// PrivateDictionary is a caller-supplied mapping of private data elements.
// Keys are the resolved Private Creator string, the containing odd group, and
// the element's intra-block offset. The dictionary only affects how bytes are
// interpreted; it never reassigns private tag numbers in the underlying file.
type PrivateDictionary struct {
	entries map[PrivateTagKey]PrivateTagEntry
}

// NewPrivateDictionary returns an empty PrivateDictionary.
func NewPrivateDictionary() *PrivateDictionary {
	return &PrivateDictionary{entries: map[PrivateTagKey]PrivateTagEntry{}}
}

// Add registers (or replaces) an entry in the private dictionary. The creator
// string is normalized the same way creator values read from files are, so
// dictionary lookups and file-observed creators compare stably regardless of
// trailing padding.
func (d *PrivateDictionary) Add(creator string, group uint16, offset uint8, vr string, name string) error {
	if !tag.IsPrivate(group) {
		return fmt.Errorf("group 0x%04x is not private: %w", group, ErrorInvalidPrivateGroup)
	}
	if len(vr) != 2 {
		return fmt.Errorf("vr %q is invalid: %w", vr, ErrorInvalidPrivateVR)
	}
	key := PrivateTagKey{
		Creator: NormalizePrivateCreator(creator),
		Group:   group,
		Offset:  offset,
	}
	d.entries[key] = PrivateTagEntry{VR: strings.ToUpper(vr), Name: name}
	return nil
}

// lookup returns the entry for the given key and whether it was found.
func (d *PrivateDictionary) lookup(key PrivateTagKey) (PrivateTagEntry, bool) {
	if d == nil {
		return PrivateTagEntry{}, false
	}
	entry, ok := d.entries[key]
	return entry, ok
}

// NormalizePrivateCreator canonicalizes a Private Creator string so that
// values read from the file and values supplied in a caller dictionary compare
// stably. Private Creator elements are VR=LO strings padded with trailing
// spaces (0x20) and sometimes NUL (0x00); that padding is stripped. Interior
// and leading whitespace is preserved, since it may be significant.
func NormalizePrivateCreator(creator string) string {
	return strings.TrimRight(creator, " \x00")
}

// privateDataElement reports whether t is a Private Data Element, i.e. an
// element in an odd group whose element number is 0x1000 or higher. The block
// number (high byte) is resolved against the Private Creators bound in the
// current scope; the intra-block offset (low byte) identifies the field.
func isPrivateDataElement(t tag.Tag) bool {
	return tag.IsPrivate(t.Group) && t.Element >= 0x1000
}

// privateCreatorElement reports whether t occupies a Private Creator Data
// Element position: an element in an odd group whose element number is
// 0x0010-0x00ff (PS3.5 7.8.1). Such elements carry a creator string with
// VR=LO. The VR is checked separately where transfer syntax matters, so that
// non-compliant Explicit data reusing a reserved low element number is left
// untouched.
func isPrivateCreatorElement(t tag.Tag) bool {
	return tag.IsPrivate(t.Group) && t.Element >= 0x0010 && t.Element <= 0x00ff
}

// privateScope holds the Private Creator bindings for a single dataset or
// sequence item. Each dataset / sequence item maintains its own scope; child
// items never inherit bindings from the enclosing item. Re-binding the same
// slot to a different creator replaces the previous binding, which is allowed
// and is common when different items in one file carry data from different
// vendors.
type privateScope struct {
	creators map[privateSlot]string
}

// privateSlot identifies a Private Creator reservation: the odd group plus
// the element number (0x0010-0x00ff) at which the creator string lives.
type privateSlot struct {
	group   uint16
	element uint16
}

func newPrivateScope() *privateScope {
	return &privateScope{creators: map[privateSlot]string{}}
}

// bind records that the creator string at the given slot claims the
// corresponding private block. An empty creator clears the binding.
func (s *privateScope) bind(t tag.Tag, creator string) {
	creator = NormalizePrivateCreator(creator)
	slot := privateSlot{group: t.Group, element: t.Element}
	if creator == "" {
		delete(s.creators, slot)
		return
	}
	s.creators[slot] = creator
}

// resolve finds the normalized creator for the private data element's block,
// returning the creator, its dictionary key (group + intra-block offset), and
// whether a binding exists in this scope.
func (s *privateScope) resolve(t tag.Tag) (string, PrivateTagKey, bool) {
	block := (t.Element & 0xff00) >> 8
	// A private data element (gggg,xxyy) is reserved by the Private Creator
	// carried at (gggg,00xx). The block number xx is exactly the element
	// number of its creator, so block 0x10 -> creator element 0x0010.
	slot := privateSlot{group: t.Group, element: block}
	creator, ok := s.creators[slot]
	if !ok {
		return "", PrivateTagKey{}, false
	}
	return creator, PrivateTagKey{
		Creator: creator,
		Group:   t.Group,
		Offset:  uint8(t.Element & 0x00ff),
	}, true
}
