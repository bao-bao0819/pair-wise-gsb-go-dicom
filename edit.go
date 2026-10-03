package dicom

import (
	"errors"
	"fmt"

	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// PathStep is one segment of a Path: either a tag selecting an element within
// the current element list, or an item index selecting a sequence item within
// the current sequence element. Create PathStep values with TagPathStep or
// ItemPathStep.
type PathStep struct {
	tag   tag.Tag
	index int
	isTag bool
}

// TagPathStep returns a PathStep selecting the first element with tg within
// the current list. It never searches into nested sequences: when several
// elements share the same tag, earlier ItemPathStep segments in the Path
// disambiguate them, so a deeply nested element that happens to share the tag
// is never mistaken for the target.
func TagPathStep(tg tag.Tag) PathStep {
	return PathStep{tag: tg, isTag: true}
}

// ItemPathStep returns a PathStep selecting the zero-based item at index
// within the current sequence element.
func ItemPathStep(index int) PathStep {
	return PathStep{index: index}
}

func (s PathStep) String() string {
	if s.isTag {
		return s.tag.String()
	}
	return fmt.Sprintf("[%d]", s.index)
}

// Path is an ordered list of PathStep values addressing a specific element
// within the Dataset. Tag and item steps strictly alternate, starting and
// ending with a tag step: a tag step selects an element in the current list
// (initially the Dataset root); an item step selects an item within the
// sequence element addressed by the preceding tag step.
type Path []PathStep

func (p Path) String() string {
	if len(p) == 0 {
		return "<empty path>"
	}
	out := p[0].String()
	for _, step := range p[1:] {
		if step.isTag {
			out += "/" + step.String()
		} else {
			out += step.String()
		}
	}
	return out
}

// EditOp is a single operation applied by Dataset.ApplyEdits. Concrete
// implementations are SetValueOp, DeleteElementOp, and InsertItemOp.
type EditOp interface {
	editPath() Path
	opName() string
}

// SetValueOp replaces the Value of the element addressed by Path. The target
// must exist, Path must end on a tag step, and Value must be compatible with
// the element's VR (the same compatibility Write enforces), otherwise the
// whole batch fails.
type SetValueOp struct {
	Path  Path
	Value Value
}

func (op SetValueOp) editPath() Path { return op.Path }
func (op SetValueOp) opName() string { return "SetValue" }

// DeleteElementOp removes the element addressed by Path from its containing
// element list (the Dataset root or a sequence item). Path must end on a tag
// step.
type DeleteElementOp struct {
	Path Path
}

func (op DeleteElementOp) editPath() Path { return op.Path }
func (op DeleteElementOp) opName() string { return "DeleteElement" }

// InsertItemOp inserts Item into the sequence element addressed by Path,
// which must end on the sequence's tag step. Index is zero-based and may
// equal the current item count to append, but must not exceed it. Every
// element in Item must be non-nil and carry a Value compatible with its VR.
// Inserted elements are deep copied, so later mutation of the caller's slices
// never affects the resulting Dataset.
type InsertItemOp struct {
	Path  Path
	Index int
	Item  []*Element
}

func (op InsertItemOp) editPath() Path { return op.Path }
func (op InsertItemOp) opName() string { return "InsertItem" }

// Sentinel errors surfaced through *EditError (use errors.Is to inspect).
var (
	// ErrorEditPathNotFound indicates an element addressed by a Path does
	// not exist at the expected location.
	ErrorEditPathNotFound = errors.New("edit path not found")
	// ErrorEditIndexOutOfRange indicates a sequence item index is out of
	// range for the current sequence state.
	ErrorEditIndexOutOfRange = errors.New("item index out of range")
	// ErrorEditInvalidPath indicates a Path is malformed (for example empty,
	// ending on an item step, or traversing a non-sequence element).
	ErrorEditInvalidPath = errors.New("invalid edit path")
	// ErrorEditInvalidValue indicates a nil value, a ValueType incompatible
	// with the element's VR, or an invalid element inside an inserted item.
	ErrorEditInvalidValue = errors.New("invalid edit value")
)

// EditError describes a failed edit operation. OpIndex is the zero-based
// position of the failing operation within the batch, and Segment is the
// zero-based path segment at which the failure was detected.
type EditError struct {
	OpIndex int
	Segment int
	Path    Path
	Op      string
	Err     error
}

func (e *EditError) Error() string {
	return fmt.Sprintf("edit operation #%d (%s) failed at path segment %d of %q: %v",
		e.OpIndex+1, e.Op, e.Segment+1, e.Path, e.Err)
}

// Unwrap exposes the underlying sentinel error.
func (e *EditError) Unwrap() error { return e.Err }

func editError(opIndex, segment int, op EditOp, sentinel error, format string, args ...any) error {
	return &EditError{
		OpIndex: opIndex,
		Segment: segment,
		Path:    append(Path(nil), op.editPath()...),
		Op:      op.opName(),
		Err:     fmt.Errorf("%w: %s", sentinel, fmt.Sprintf(format, args...)),
	}
}

// ApplyEdits applies a batch of edit operations and returns a new, modified
// Dataset. Operations run in order against one evolving working copy, so
// later item indices observe the state produced by earlier inserts and
// deletes.
//
// The receiver Dataset is never modified, including every element and slice
// nested within its sequences. If any operation fails (a missing path
// segment, a VR/ValueType mismatch, or an item index out of range), the
// whole batch fails: ApplyEdits returns a nil *Dataset and an *EditError
// identifying the failing operation number and path segment, and the
// original Dataset is left unchanged.
//
// Inputs that repeat the same *Element or item pointer are fully deep
// copied, so each occurrence becomes independent: editing one never alters
// an unrelated path.
func (d *Dataset) ApplyEdits(ops ...EditOp) (*Dataset, error) {
	working := cloneDataset(d)
	for i, op := range ops {
		if err := applyEdit(working, i, op); err != nil {
			return nil, err
		}
	}
	return working, nil
}

func cloneDataset(d *Dataset) *Dataset {
	if d == nil {
		return &Dataset{}
	}
	return &Dataset{Elements: cloneElementSlice(d.Elements)}
}

func cloneElementSlice(src []*Element) []*Element {
	if src == nil {
		return nil
	}
	dst := make([]*Element, len(src))
	for i, elem := range src {
		dst[i] = cloneElement(elem)
	}
	return dst
}

func cloneElement(elem *Element) *Element {
	if elem == nil {
		return nil
	}
	clone := *elem
	clone.Value = cloneValue(elem.Value)
	return &clone
}

func cloneValue(v Value) Value {
	if v == nil {
		return nil
	}
	switch v.ValueType() {
	case Strings:
		src := v.GetValue().([]string)
		return &stringsValue{value: append([]string(nil), src...)}
	case Bytes:
		src := v.GetValue().([]byte)
		return &bytesValue{value: append([]byte(nil), src...)}
	case Ints:
		src := v.GetValue().([]int)
		return &intsValue{value: append([]int(nil), src...)}
	case Floats:
		src := v.GetValue().([]float64)
		return &floatsValue{value: append([]float64(nil), src...)}
	case Sequences:
		src := v.GetValue().([]*SequenceItemValue)
		dst := make([]*SequenceItemValue, len(src))
		for i, item := range src {
			dst[i] = cloneSequenceItem(item)
		}
		return &sequencesValue{value: dst}
	case SequenceItem:
		return &SequenceItemValue{elements: cloneElementSlice(v.GetValue().([]*Element))}
	case PixelData:
		src := v.GetValue().(PixelDataInfo)
		dst := src
		if src.Offsets != nil {
			dst.Offsets = append([]uint32(nil), src.Offsets...)
		}
		if src.UnprocessedValueData != nil {
			dst.UnprocessedValueData = append([]byte(nil), src.UnprocessedValueData...)
		}
		if src.Frames != nil {
			dst.Frames = append([]*frame.Frame(nil), src.Frames...)
		}
		return &pixelDataValue{PixelDataInfo: dst}
	default:
		// Unrecognized Value implementations cannot be copied by the
		// library; they are shared rather than mutated, preserving data.
		return v
	}
}

func cloneSequenceItem(item *SequenceItemValue) *SequenceItemValue {
	if item == nil {
		return nil
	}
	return &SequenceItemValue{elements: cloneElementSlice(item.elements)}
}

func applyEdit(d *Dataset, opIndex int, op EditOp) error {
	path := op.editPath()
	if len(path) == 0 {
		return editError(opIndex, 0, op, ErrorEditInvalidPath, "path is empty")
	}

	// indices records, for every traversed sequence item, the element
	// index of its sequence tag in the grandparent element list followed
	// by the chosen item index. The current element list is always
	// re-derived from d via these indices, so pointer replacements made by
	// earlier operations are observed by later ones.
	var indices []int
	// awaitingItem is true immediately after a non-terminal sequence tag
	// step: the next step must be an item step, never another tag (which
	// could accidentally match a same-tag sibling instead of descending).
	var awaitingItem bool

	for seg, step := range path {
		isLast := seg == len(path)-1
		curList := currentElementList(d, indices)

		if step.isTag {
			if awaitingItem {
				return editError(opIndex, seg, op, ErrorEditInvalidPath,
					"tag step %s must be preceded by an item step after sequence tag", step.tag)
			}
			idx := findElementIndex(curList, step.tag)
			if idx < 0 {
				return editError(opIndex, seg, op, ErrorEditPathNotFound,
					"no element with tag %s in the current element list", step.tag)
			}
			elem := curList[idx]

			if isLast {
				return applyAtTag(d, indices, opIndex, seg, op, idx, elem)
			}
			if elem.Value == nil || elem.Value.ValueType() != Sequences {
				return editError(opIndex, seg, op, ErrorEditInvalidPath,
					"element with tag %s is not a sequence", step.tag)
			}
			indices = append(indices, idx)
			awaitingItem = true
			continue
		}

		// An item step must directly follow a sequence tag step; the
		// pending sequence element index was just appended (even length:
		// indices is a sequence of seqIdx,itemIdx pairs).
		if !awaitingItem || len(indices)%2 != 1 {
			return editError(opIndex, seg, op, ErrorEditInvalidPath,
				"item step [%d] is not nested within a sequence tag step", step.index)
		}
		seqElem := currentSequenceElement(d, indices)
		items := seqElem.Value.GetValue().([]*SequenceItemValue)
		if step.index < 0 || step.index >= len(items) {
			return editError(opIndex, seg, op, ErrorEditIndexOutOfRange,
				"item index %d is out of range for sequence %s with %d item(s)",
				step.index, seqElem.Tag, len(items))
		}
		if isLast {
			return editError(opIndex, seg, op, ErrorEditInvalidPath,
				"operation %s must end on a tag step, not on a sequence item", op.opName())
		}
		indices = append(indices, step.index)
		awaitingItem = false
	}
	return nil
}

// currentElementList returns the element list addressed by the pairs in
// indices: an even-length indices selects the element list of the last
// sequence item; an odd length selects the parent list of the pending
// sequence element (used during a tag step that starts a new pair).
func currentElementList(d *Dataset, indices []int) []*Element {
	list := d.Elements
	for i := 0; i+1 < len(indices); i += 2 {
		seqElem := list[indices[i]]
		items := seqElem.Value.GetValue().([]*SequenceItemValue)
		list = items[indices[i+1]].elements
	}
	return list
}

func currentSequenceElement(d *Dataset, indices []int) *Element {
	list := d.Elements
	for i := 0; i+1 < len(indices); i += 2 {
		seqElem := list[indices[i]]
		items := seqElem.Value.GetValue().([]*SequenceItemValue)
		list = items[indices[i+1]].elements
	}
	return list[indices[len(indices)-1]]
}

// setCurrentElementList writes a possibly reallocated element list back to
// its owning item (or the Dataset root when indices is empty).
func setCurrentElementList(d *Dataset, indices []int, updated []*Element) {
	if len(indices) == 0 {
		d.Elements = updated
		return
	}
	list := d.Elements
	for i := 0; i+2 < len(indices); i += 2 {
		seqElem := list[indices[i]]
		items := seqElem.Value.GetValue().([]*SequenceItemValue)
		list = items[indices[i+1]].elements
	}
	lastItemIdx := indices[len(indices)-1]
	lastSeqIdx := indices[len(indices)-2]
	parentItems := list[lastSeqIdx].Value.GetValue().([]*SequenceItemValue)
	parentItems[lastItemIdx].elements = updated
}

// replaceCurrentSequenceElement replaces the sequence element selected by
// the pending (odd-length) indices within its parent element list.
func replaceCurrentSequenceElement(d *Dataset, indices []int, newSeq *Element) {
	list := d.Elements
	seqElemIdx := indices[len(indices)-1]
	for i := 0; i+1 < len(indices); i += 2 {
		seqElem := list[indices[i]]
		items := seqElem.Value.GetValue().([]*SequenceItemValue)
		list = items[indices[i+1]].elements
	}
	list[seqElemIdx] = newSeq
}

func applyAtTag(d *Dataset, indices []int, opIndex, seg int, op EditOp, elemIndex int, elem *Element) error {
	curList := currentElementList(d, indices)
	switch target := op.(type) {
	case SetValueOp:
		if target.Value == nil {
			return editError(opIndex, seg, op, ErrorEditInvalidValue, "SetValue value must not be nil")
		}
		if err := verifyElementWritable(elem, target.Value); err != nil {
			return editError(opIndex, seg, op, ErrorEditInvalidValue,
				"replacement value for tag %s is incompatible with VR %q: %v",
				elem.Tag, elem.RawValueRepresentation, err)
		}
		newElem := cloneElement(elem)
		newElem.Value = cloneValue(target.Value)
		curList[elemIndex] = newElem
		return nil

	case DeleteElementOp:
		updated := make([]*Element, 0, max(len(curList)-1, 0))
		updated = append(updated, curList[:elemIndex]...)
		updated = append(updated, curList[elemIndex+1:]...)
		setCurrentElementList(d, indices, updated)
		return nil

	case InsertItemOp:
		if elem.Value == nil || elem.Value.ValueType() != Sequences {
			return editError(opIndex, seg, op, ErrorEditInvalidPath,
				"InsertItem target tag %s is not a sequence", elem.Tag)
		}
		items := elem.Value.GetValue().([]*SequenceItemValue)
		if target.Index < 0 || target.Index > len(items) {
			return editError(opIndex, seg, op, ErrorEditIndexOutOfRange,
				"insert index %d is out of range for sequence %s with %d item(s) (valid range 0..%d)",
				target.Index, elem.Tag, len(items), len(items))
		}
		newItemElements := cloneElementSlice(target.Item)
		if len(newItemElements) == 0 {
			return editError(opIndex, seg, op, ErrorEditInvalidValue,
				"inserted item for sequence %s must contain at least one element", elem.Tag)
		}
		for _, child := range newItemElements {
			if child == nil {
				return editError(opIndex, seg, op, ErrorEditInvalidValue,
					"inserted item for sequence %s contains a nil element", elem.Tag)
			}
			if child.Value == nil {
				return editError(opIndex, seg, op, ErrorEditInvalidValue,
					"inserted element with tag %s has a nil value", child.Tag)
			}
			if err := verifyElementWritable(child, child.Value); err != nil {
				return editError(opIndex, seg, op, ErrorEditInvalidValue,
					"inserted element with tag %s is incompatible with VR %q: %v",
					child.Tag, child.RawValueRepresentation, err)
			}
		}

		grown := make([]*SequenceItemValue, 0, len(items)+1)
		grown = append(grown, items[:target.Index]...)
		grown = append(grown, &SequenceItemValue{elements: newItemElements})
		grown = append(grown, items[target.Index:]...)

		newSeqElem := cloneElement(elem)
		newSeqElem.Value = &sequencesValue{value: grown}
		if len(indices)%2 == 1 {
			// indices already ends in this sequence's element index.
			replaceCurrentSequenceElement(d, indices, newSeqElem)
		} else {
			curList[elemIndex] = newSeqElem
		}
		return nil

	default:
		return editError(opIndex, seg, op, ErrorEditInvalidPath, "unknown edit operation %T", op)
	}
}

func findElementIndex(elems []*Element, tg tag.Tag) int {
	for i, elem := range elems {
		if elem != nil && elem.Tag == tg {
			return i
		}
	}
	return -1
}

// verifyElementWritable checks value against the element tag's VR using the
// same verification Write applies by default.
func verifyElementWritable(elem *Element, value Value) error {
	if value == nil {
		return errors.New("value is nil")
	}
	vr, err := verifyVROrDefault(elem.Tag, elem.RawValueRepresentation, writeOptSet{})
	if err != nil {
		return err
	}
	return verifyValueType(elem.Tag, value, vr)
}
