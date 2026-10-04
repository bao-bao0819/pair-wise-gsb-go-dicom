package dicom

import (
	"fmt"

	"github.com/suyashkumar/dicom/pkg/tag"
)

// PrivateVRConflict describes a disagreement between the VR declared on the
// wire (Explicit VR transfer syntax) and the VR configured in the
// caller-supplied private dictionary for the same element.
type PrivateVRConflict struct {
	// Tag is the on-wire private tag. It is never rewritten.
	Tag tag.Tag
	// Creator is the normalized Private Creator bound to the element's block.
	Creator string
	// WireVR is the VR encoded in the file.
	WireVR string
	// DictionaryVR is the VR configured in the caller's private dictionary.
	DictionaryVR string
}

func (c PrivateVRConflict) Error() string {
	return fmt.Sprintf("private VR conflict for %s (creator %q): wire VR=%s but dictionary VR=%s",
		c.Tag.String(), c.Creator, c.WireVR, c.DictionaryVR)
}
