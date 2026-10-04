// Package private provides support for caller-supplied dictionaries of DICOM
// private data elements.
//
// The same private tag (a uint32 group/element pair) does not identify the same
// field on equipment from two different vendors. A private element is only
// meaningful relative to a Private Creator string that was previously reserved
// in a Private Creator data element (gggg,00xx) within the same dataset or
// sequence item. See PS3.5 Section 7.8.1.
//
// Because of this, entries in a Dictionary are keyed by the normalized Private
// Creator string, the odd group number, and the element number within the
// private block (the low byte of the element field), never by the raw uint32
// tag alone.
//
// A Scope tracks the Private Creator reservations in effect while parsing a
// single dataset or single sequence item. Each dataset and each sequence item
// maintains its own Scope; child items never inherit their parent's
// reservations.
package private

import (
	"errors"
	"fmt"
	"strings"

	"github.com/suyashkumar/dicom/pkg/tag"
)

// Key uniquely identifies a private data element definition: the Private
// Creator string it belongs to, the (odd) group number, and the element number
// within the creator's reserved private block (the low byte of the element
// field, in the range 0x00-0xFF).
type Key struct {
	Creator string
	Group   uint16
	Offset  byte
}

// Definition describes a single private data element known to the caller.
type Definition struct {
	// VR is the two-character DICOM value representation used to interpret
	// the element, e.g. "US", "LO" or "FD".
	VR string
	// Name is an optional human-readable name.
	Name string
	// Keyword is an optional machine-readable identifier.
	Keyword string
}

// Dictionary is a caller-supplied collection of private data element
// definitions. It is safe for concurrent read-only use; callers should build
// it once before parsing.
type Dictionary struct {
	defs map[Key]Definition
}

// NewDictionary returns a Dictionary containing the provided definitions.
// Creator values are normalized (see NormalizeCreator) when stored.
func NewDictionary(defs map[Key]Definition) *Dictionary {
	d := &Dictionary{defs: make(map[Key]Definition, len(defs))}
	for k, def := range defs {
		k.Creator = NormalizeCreator(k.Creator)
		d.defs[k] = def
	}
	return d
}

// Add registers a single definition, overwriting any previous definition for
// the same key. The creator is normalized.
func (d *Dictionary) Add(k Key, def Definition) {
	k.Creator = NormalizeCreator(k.Creator)
	if d.defs == nil {
		d.defs = make(map[Key]Definition)
	}
	d.defs[k] = def
}

// Lookup returns the definition registered for the given key. The creator is
// normalized before lookup.
func (d *Dictionary) Lookup(k Key) (Definition, bool) {
	if d == nil || d.defs == nil {
		return Definition{}, false
	}
	k.Creator = NormalizeCreator(k.Creator)
	def, ok := d.defs[k]
	return def, ok
}

// NormalizeCreator returns the canonical form of a Private Creator value.
// Private Creator elements use the LO VR and may be padded with trailing
// spaces (or, in non-compliant files, NUL bytes); such padding is not
// significant (PS3.5 7.8.1). Normalization trims trailing spaces and NULs so
// that, e.g., "ACME" and "ACME  " resolve to the same dictionary entry
// regardless of how the creator was encoded on the wire.
func NormalizeCreator(creator string) string {
	return strings.TrimRight(creator, " \x00")
}

// IsCreator reports whether t is a Private Creator reservation data element,
// i.e. an element in an odd group with element number 0x0010-0x00FF.
func IsCreator(t tag.Tag) bool {
	return tag.IsPrivate(t.Group) && t.Element >= 0x0010 && t.Element <= 0x00FF
}

// Parts splits a private data element tag (element number 0x1000-0xFFFF in an
// odd group) into the reserved block number (high byte) and the element number
// within the private block (low byte). It returns an error for non-private
// tags, Private Creator elements, or other element numbers that do not refer
// to a private data element.
func Parts(t tag.Tag) (block byte, offset byte, err error) {
	if !tag.IsPrivate(t.Group) {
		return 0, 0, fmt.Errorf("tag %s is not in a private (odd) group", t.String())
	}
	if t.Element < 0x0100 {
		return 0, 0, fmt.Errorf("tag %s is not a private data element (element below 0x0100)", t.String())
	}
	return byte(t.Element >> 8), byte(t.Element), nil
}

// Scope holds the Private Creator reservations in effect for a single dataset
// or sequence item. Scopes are intentionally not shared: a nested sequence
// item gets a fresh Scope and does not inherit its parent's reservations.
type Scope struct {
	dict     *Dictionary
	creators map[byte]string
}

// NewScope returns a fresh, empty Scope backed by the provided dictionary
// (which may be nil).
func NewScope(dict *Dictionary) *Scope {
	return &Scope{dict: dict, creators: make(map[byte]string)}
}

// SetCreator binds the given private creator block (the low byte of a Private
// Creator element such as (gggg,0010)) to the normalized creator value. An
// empty (all-padding) creator value does not bind a slot, and ok is false.
// The same slot may be rebound to a different creator later within the same
// scope.
func (s *Scope) SetCreator(slot byte, creator string) (ok bool) {
	creator = NormalizeCreator(creator)
	if creator == "" {
		return false
	}
	if s.creators == nil {
		s.creators = make(map[byte]string)
	}
	s.creators[slot] = creator
	return true
}

// Creator returns the normalized creator bound to the given slot, if any.
func (s *Scope) Creator(slot byte) (string, bool) {
	if s == nil {
		return "", false
	}
	c, ok := s.creators[slot]
	return c, ok
}

// Lookup resolves a private data element tag against the scope's creator
// reservations and the backing dictionary. It returns the definition and the
// normalized creator of the reservation it resolved through. It returns
// ok=false (without error) when the tag is not a private data element, its
// block has no creator reservation, or no dictionary entry exists for the
// reserved creator/group/offset triple.
func (s *Scope) Lookup(t tag.Tag) (def Definition, creator string, ok bool) {
	if s == nil || s.dict == nil {
		return Definition{}, "", false
	}
	block, offset, err := Parts(t)
	if err != nil {
		return Definition{}, "", false
	}
	creator, found := s.creators[block]
	if !found {
		return Definition{}, "", false
	}
	def, found = s.dict.Lookup(Key{Creator: creator, Group: t.Group, Offset: offset})
	if !found {
		return Definition{}, creator, false
	}
	return def, creator, true
}

// ResolveCreator returns the normalized Private Creator associated with a
// private data element's reserved block in this scope, if the block has been
// reserved. Unlike Lookup it does not require a dictionary entry.
func (s *Scope) ResolveCreator(t tag.Tag) (creator string, reserved bool) {
	if s == nil {
		return "", false
	}
	block, _, err := Parts(t)
	if err != nil {
		return "", false
	}
	creator, reserved = s.creators[block]
	return creator, reserved
}

// ErrNotPrivateDataElement indicates that a tag could not be interpreted as a
// private data element.
var ErrNotPrivateDataElement = errors.New("tag is not a private data element")
