package dicom

import (
	"errors"
	"fmt"
	"strings"

	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
)

// Path-based, transactional editing API.
//
// An edit path is an ordered sequence of PathSteps: a TagStep selects a
// uniquely-tagged element within the current item (or top-level Dataset),
// and an ItemStep selects one item of the immediately preceding sequence
// element. For example, the path
//
//	TagStep{ContributingEquipmentSequence}, ItemStep{0}, TagStep{PatientName}
//
// addresses the PatientName element inside the first item of the
// ContributingEquipmentSequence, and never any other element that happens
// to share that tag anywhere else in the Dataset.
//
// Tags are never matched recursively: a TagStep only matches elements at
// the current level. If more than one element at that level carries the
// tag, the path is rejected as ambiguous instead of silently editing the
// first match. This prevents a deeply nested same-tag element (or a sibling
// duplicate) from being mistaken for the target.

var (
	// ErrorPathNotFound indicates that a tag referenced by an edit path
	// does not exist at the resolved location.
	ErrorPathNotFound = errors.New("edit path: element not found")
	// ErrorAmbiguousPath indicates that a TagStep matches more than one
	// element at the same level. The caller must disambiguate by navigating
	// through the enclosing sequence items.
	ErrorAmbiguousPath = errors.New("edit path: tag matches multiple elements at the same level")
	// ErrorIndexOutOfRange indicates that an ItemStep index (or insertion
	// index) is outside the valid range of the targeted sequence.
	ErrorIndexOutOfRange = errors.New("edit path: sequence item index out of range")
	// ErrorInvalidPath indicates that an edit path is malformed, e.g. it is
	// empty, starts with an ItemStep, places two TagSteps back-to-back
	// without an intervening ItemStep, or applies an ItemStep to an element
	// that is not a sequence.
	ErrorInvalidPath = errors.New("edit path: invalid path")
	// ErrorInvalidEdit indicates that an Edit is malformed, e.g. it has an
	// unknown Type, lacks a Path, or is missing the payload (Value/Item)
	// that its Type requires.
	ErrorInvalidEdit = errors.New("invalid edit")
)

// PathStep is one segment of an edit path. Build paths using TagStep and
// ItemStep.
type PathStep interface {
	isPathStep()
	String() string
}

// TagStep selects the uniquely-tagged element carrying Tag at the current
// level.
type TagStep struct {
	Tag tag.Tag
}

func (TagStep) isPathStep() {}

// String renders this step as its DICOM tag, e.g. "(0018,a001)".
func (s TagStep) String() string { return s.Tag.String() }

// ItemStep selects the sequence item at Index within the immediately
// preceding sequence TagStep. Indices are 0-based.
type ItemStep struct {
	Index int
}

func (ItemStep) isPathStep() {}

// String renders this step as its 0-based index, e.g. "[2]".
func (s ItemStep) String() string { return fmt.Sprintf("[%d]", s.Index) }

// EditType enumerates the supported edit operations.
type EditType int

const (
	// ReplaceValue replaces the Value of the element addressed by Path. The
	// element's tag and VR are preserved; the replacement Value's ValueType
	// must be compatible with that VR (the same compatibility enforced when
	// writing DICOMs). The value length is recomputed on write, except that
	// undefined-length pixel data keeps its undefined-length marker.
	ReplaceValue EditType = iota
	// DeleteElement removes the addressed element. A path ending in a
	// TagStep deletes that single element; a path ending in an ItemStep
	// deletes the entire sequence item at that index.
	DeleteElement
	// InsertSequenceItem inserts Item as a new item into the sequence
	// addressed by Path (which must end at a sequence element). ItemIndex
	// selects the insertion position and must be in the range
	// [0, len(items)]; ItemIndex == len(items) appends. Later edits in the
	// same batch observe the newly inserted item.
	InsertSequenceItem
)

// Edit is a single path-based edit operation.
//
// Field usage by Type:
//   - ReplaceValue: Path and Value are required.
//   - DeleteElement: Path is required.
//   - InsertSequenceItem: Path, Item and ItemIndex are required. Path must
//     address a sequence element; Item holds the elements forming the new
//     sequence item (at least one).
type Edit struct {
	Type      EditType
	Path      []PathStep
	Value     Value
	Item      []*Element
	ItemIndex int
}

// ApplyEdits applies a batch of edits to a deep copy of d and returns the
// resulting Dataset.
//
// Edits are applied in order to a single temporary result, so item indices
// in later edits are resolved against the state produced by earlier edits.
// The batch is transactional: if any edit fails (its path does not exist or
// is ambiguous, an item index is out of range, or a VR/value type is
// incompatible), no edit is applied and a nil Dataset is returned. The
// error identifies both the failing operation (1-based index) and the
// failing path segment (1-based index). The receiver Dataset and every one
// of its nested slices is left untouched.
//
// Values and item elements supplied in the edits are deep-copied into the
// result: reusing the same *Element or Value across multiple edits (or
// mutating it after the call) can never make one path's data appear at an
// unrelated path. Repeated object references in the input are preserved as
// independent copies, one per referenced path.
func (d *Dataset) ApplyEdits(edits []Edit) (*Dataset, error) {
	if d == nil {
		return nil, fmt.Errorf("%w: cannot apply edits to a nil dataset", ErrorInvalidEdit)
	}
	result, err := d.deepCopy()
	if err != nil {
		return nil, err
	}
	for i, edit := range edits {
		if err := applyEdit(result, edit); err != nil {
			return nil, fmt.Errorf("edit #%d: %w", i+1, err)
		}
	}
	return result, nil
}

// DeepCopy returns a fully independent copy of d, including nested
// sequence items, values, and pixel data buffers. It returns nil if d is
// nil.
func (d *Dataset) DeepCopy() (*Dataset, error) {
	if d == nil {
		return nil, nil
	}
	return d.deepCopy()
}

func (d *Dataset) deepCopy() (*Dataset, error) {
	elems, err := deepCopyElements(d.Elements)
	if err != nil {
		return nil, err
	}
	return &Dataset{Elements: elems}, nil
}

func applyEdit(d *Dataset, edit Edit) error {
	if len(edit.Path) == 0 {
		return fmt.Errorf("%w: path is empty", ErrorInvalidEdit)
	}
	switch edit.Type {
	case ReplaceValue:
		return applyReplaceValue(d, edit)
	case DeleteElement:
		return applyDeleteElement(d, edit)
	case InsertSequenceItem:
		return applyInsertSequenceItem(d, edit)
	default:
		return fmt.Errorf("%w: unknown edit type %d", ErrorInvalidEdit, edit.Type)
	}
}

func applyReplaceValue(d *Dataset, edit Edit) error {
	if edit.Value == nil {
		return fmt.Errorf("%w: ReplaceValue requires a non-nil Value", ErrorInvalidEdit)
	}
	target, perr := resolveElementPath(d, edit.Path)
	if perr != nil {
		return perr
	}
	vr, err := verifyVROrDefault(target.elem.Tag, target.elem.RawValueRepresentation, writeOptSet{})
	if err != nil {
		return &pathError{segment: len(edit.Path) - 1, steps: edit.Path, cause: err}
	}
	if err := verifyValueType(target.elem.Tag, edit.Value, vr); err != nil {
		return &pathError{
			segment: len(edit.Path) - 1,
			steps:   edit.Path,
			cause:   fmt.Errorf("replacement value incompatible with VR %q for %v: %w", vr, target.elem.Tag, fmt.Errorf("%w: %v", ErrorMismatchValueTypeAndVR, err)),
		}
	}
	newValue, err := deepCopyValue(edit.Value)
	if err != nil {
		return err
	}
	target.elem.Value = newValue
	// Keep the undefined-length marker for encapsulated pixel data; every
	// other element gets its length recomputed from the new value on write.
	if target.elem.ValueLength != tag.VLUndefinedLength {
		target.elem.ValueLength = 0
	}
	return nil
}

func applyDeleteElement(d *Dataset, edit Edit) error {
	if last, ok := edit.Path[len(edit.Path)-1].(ItemStep); ok {
		prefix := edit.Path[:len(edit.Path)-1]
		seq, perr := resolveSequenceAt(d, prefix)
		if perr != nil {
			return perr
		}
		if last.Index < 0 || last.Index >= len(seq.items) {
			return &pathError{
				segment: len(edit.Path) - 1,
				steps:   edit.Path,
				cause:   fmt.Errorf("%w: index %d in sequence of length %d", ErrorIndexOutOfRange, last.Index, len(seq.items)),
			}
		}
		remaining := make([]*SequenceItemValue, 0, len(seq.items)-1)
		remaining = append(remaining, seq.items[:last.Index]...)
		remaining = append(remaining, seq.items[last.Index+1:]...)
		seq.set(remaining)
		return nil
	}
	target, perr := resolveElementPath(d, edit.Path)
	if perr != nil {
		return perr
	}
	remaining := make([]*Element, 0, len(target.parent)-1)
	remaining = append(remaining, target.parent[:target.index]...)
	remaining = append(remaining, target.parent[target.index+1:]...)
	target.holder.setElements(remaining)
	return nil
}

func applyInsertSequenceItem(d *Dataset, edit Edit) error {
	if len(edit.Item) == 0 {
		return fmt.Errorf("%w: InsertSequenceItem requires at least one element in Item", ErrorInvalidEdit)
	}
	seq, perr := resolveSequenceAt(d, edit.Path)
	if perr != nil {
		return perr
	}
	if edit.ItemIndex < 0 || edit.ItemIndex > len(seq.items) {
		return &pathError{
			segment: len(edit.Path) - 1,
			steps:   edit.Path,
			cause: fmt.Errorf("%w: insertion index %d for sequence of length %d (valid range is 0..%d)",
				ErrorIndexOutOfRange, edit.ItemIndex, len(seq.items), len(seq.items)),
		}
	}
	itemElems, err := deepCopyElements(edit.Item)
	if err != nil {
		return err
	}
	for _, elem := range itemElems {
		if err := normalizeInsertedElement(elem); err != nil {
			return &pathError{segment: len(edit.Path) - 1, steps: edit.Path, cause: err}
		}
	}
	newItem := &SequenceItemValue{elements: itemElems}
	updated := make([]*SequenceItemValue, 0, len(seq.items)+1)
	updated = append(updated, seq.items[:edit.ItemIndex]...)
	updated = append(updated, newItem)
	updated = append(updated, seq.items[edit.ItemIndex:]...)
	seq.set(updated)
	return nil
}

// normalizeInsertedElement validates and normalizes an element supplied as
// part of an inserted sequence item: it must carry a Value, must not be an
// item/delimitation pseudo-element, its raw VR must be valid for its tag,
// and its ValueType must match that VR.
func normalizeInsertedElement(elem *Element) error {
	if elem == nil || elem.Value == nil {
		return fmt.Errorf("%w: inserted item contains a nil element or an element without a value", ErrorInvalidEdit)
	}
	if elem.Tag.Group == tag.GroupSeqItem {
		return fmt.Errorf("%w: inserted item elements must not use item/delimitation tags (group fffe), got %v", ErrorInvalidEdit, elem.Tag)
	}
	vr, err := verifyVROrDefault(elem.Tag, elem.RawValueRepresentation, writeOptSet{})
	if err != nil {
		return err
	}
	if err := verifyValueType(elem.Tag, elem.Value, vr); err != nil {
		return fmt.Errorf("inserted element %v: %w", elem.Tag, fmt.Errorf("%w: %v", ErrorMismatchValueTypeAndVR, err))
	}
	elem.RawValueRepresentation = vr
	elem.ValueRepresentation = tag.GetVRKind(elem.Tag, vr)
	return nil
}

// elementsHolder owns an element slice (a Dataset or SequenceItemValue) and
// lets a deletion write the resized slice back to the proper owner.
type elementsHolder interface {
	setElements([]*Element)
}

type datasetHolder struct{ d *Dataset }

func (h datasetHolder) setElements(e []*Element) { h.d.Elements = e }

type itemHolder struct{ item *SequenceItemValue }

func (h itemHolder) setElements(e []*Element) { h.item.elements = e }

// elementTarget identifies a resolved element plus everything needed to
// delete it from its parent.
type elementTarget struct {
	elem   *Element
	parent []*Element
	index  int
	holder elementsHolder
}

// sequenceTarget identifies a resolved sequence value and provides a setter
// for its (possibly resized) item slice.
type sequenceTarget struct {
	items []*SequenceItemValue
	set   func([]*SequenceItemValue)
}

// resolveElementPath walks steps, which must end in a TagStep, and returns
// the addressed element.
func resolveElementPath(d *Dataset, steps []PathStep) (*elementTarget, *pathError) {
	if _, trailingItem := steps[len(steps)-1].(ItemStep); trailingItem {
		return nil, &pathError{
			segment: len(steps) - 1,
			steps:   steps,
			cause:   fmt.Errorf("%w: this operation targets an element; path must end with a tag", ErrorInvalidPath),
		}
	}
	return walkElementPath(d.Elements, steps, 0, datasetHolder{d: d})
}

// resolveSequenceAt walks steps, which must end in a TagStep that addresses
// a sequence element, and returns a handle on that sequence.
func resolveSequenceAt(d *Dataset, steps []PathStep) (sequenceTarget, *pathError) {
	target, perr := walkElementPath(d.Elements, steps, 0, datasetHolder{d: d})
	if perr != nil {
		return sequenceTarget{}, perr
	}
	if target.elem.Value == nil || target.elem.Value.ValueType() != Sequences {
		return sequenceTarget{}, &pathError{
			segment: len(steps) - 1,
			steps:   steps,
			cause:   fmt.Errorf("%w: %v is not a sequence", ErrorInvalidPath, target.elem.Tag),
		}
	}
	items := target.elem.Value.GetValue().([]*SequenceItemValue)
	return sequenceTarget{
		items: items,
		set: func(updated []*SequenceItemValue) {
			target.elem.Value = &sequencesValue{value: updated}
		},
	}, nil
}

// walkElementPath is the recursive core of path resolution. holder owns
// elems, allowing a deletion at the final step to write the resized slice
// back to the right Dataset or sequence item.
func walkElementPath(elems []*Element, steps []PathStep, seg int, holder elementsHolder) (*elementTarget, *pathError) {
	tagStep, ok := steps[seg].(TagStep)
	if !ok {
		var detail string
		if itemStep, isItem := steps[seg].(ItemStep); isItem {
			detail = fmt.Sprintf("got item index %d", itemStep.Index)
		} else {
			detail = fmt.Sprintf("got unsupported path step %T", steps[seg])
		}
		return nil, &pathError{
			segment: seg,
			steps:   steps,
			cause:   fmt.Errorf("%w: expected a tag at segment #%d, %s", ErrorInvalidPath, seg+1, detail),
		}
	}
	elem, index, perr := findUniqueTagged(elems, tagStep.Tag, seg, steps)
	if perr != nil {
		return nil, perr
	}

	if seg == len(steps)-1 {
		return &elementTarget{elem: elem, parent: elems, index: index, holder: holder}, nil
	}

	// The next step must descend into a sequence item; two consecutive tag
	// steps (without an intervening item index) are invalid.
	itemStep, ok := steps[seg+1].(ItemStep)
	if !ok {
		return nil, &pathError{
			segment: seg + 1,
			steps:   steps,
			cause:   fmt.Errorf("%w: tag %v must be followed by an item index, not another tag", ErrorInvalidPath, tagStep.Tag),
		}
	}
	if elem.Value == nil || elem.Value.ValueType() != Sequences {
		return nil, &pathError{
			segment: seg + 1,
			steps:   steps,
			cause:   fmt.Errorf("%w: item index applied to non-sequence element %v", ErrorInvalidPath, tagStep.Tag),
		}
	}
	items := elem.Value.GetValue().([]*SequenceItemValue)
	if itemStep.Index < 0 || itemStep.Index >= len(items) {
		return nil, &pathError{
			segment: seg + 1,
			steps:   steps,
			cause:   fmt.Errorf("%w: index %d in sequence %v of length %d", ErrorIndexOutOfRange, itemStep.Index, tagStep.Tag, len(items)),
		}
	}
	return walkElementPath(items[itemStep.Index].elements, steps, seg+2, itemHolder{item: items[itemStep.Index]})
}

// findUniqueTagged returns the single element carrying tg at the current
// level. It never falls back to a recursive/deep search: a missing tag or a
// same-tag duplicate at this level is an error.
func findUniqueTagged(elems []*Element, tg tag.Tag, seg int, steps []PathStep) (*Element, int, *pathError) {
	var found *Element
	foundIndex := -1
	matches := 0
	for i, elem := range elems {
		if elem.Tag == tg {
			found = elem
			foundIndex = i
			matches++
		}
	}
	if matches == 0 {
		return nil, -1, &pathError{
			segment: seg,
			steps:   steps,
			cause:   fmt.Errorf("%w: no element %v at this level", ErrorPathNotFound, tg),
		}
	}
	if matches > 1 {
		return nil, -1, &pathError{
			segment: seg,
			steps:   steps,
			cause:   fmt.Errorf("%w: tag %v matches %d elements at this level; navigate via the enclosing sequence item indices", ErrorAmbiguousPath, tg, matches),
		}
	}
	return found, foundIndex, nil
}

// pathError locates a failed edit both within its path (segment, 0-based)
// and (wrapped by the caller) within the batch.
type pathError struct {
	segment int
	steps   []PathStep
	cause   error
}

func (e *pathError) Error() string {
	var marked []string
	for i, step := range e.steps {
		if i == e.segment {
			marked = append(marked, ">>"+step.String()+"<<")
			continue
		}
		marked = append(marked, step.String())
	}
	return fmt.Sprintf("path %s: segment #%d (%s): %v",
		strings.Join(marked, " "), e.segment+1, e.steps[e.segment], e.cause)
}

func (e *pathError) Unwrap() error { return e.cause }

// deepCopyElements returns an independent copy of elems, recursively
// copying sequence contents and all value buffers.
func deepCopyElements(elems []*Element) ([]*Element, error) {
	if elems == nil {
		return nil, nil
	}
	out := make([]*Element, len(elems))
	for i, elem := range elems {
		copied, err := deepCopyElement(elem)
		if err != nil {
			return nil, err
		}
		out[i] = copied
	}
	return out, nil
}

func deepCopyElement(elem *Element) (*Element, error) {
	if elem == nil {
		return nil, nil
	}
	copied := &Element{
		Tag:                    elem.Tag,
		ValueRepresentation:    elem.ValueRepresentation,
		RawValueRepresentation: elem.RawValueRepresentation,
		ValueLength:            elem.ValueLength,
	}
	if elem.Value != nil {
		value, err := deepCopyValue(elem.Value)
		if err != nil {
			return nil, err
		}
		copied.Value = value
	}
	return copied, nil
}

func deepCopyValue(v Value) (Value, error) {
	if v == nil {
		return nil, nil
	}
	switch value := v.GetValue().(type) {
	case []string:
		return &stringsValue{value: append([]string(nil), value...)}, nil
	case []byte:
		return &bytesValue{value: append([]byte(nil), value...)}, nil
	case []int:
		return &intsValue{value: append([]int(nil), value...)}, nil
	case []float64:
		return &floatsValue{value: append([]float64(nil), value...)}, nil
	case PixelDataInfo:
		info, err := deepCopyPixelDataInfo(value)
		if err != nil {
			return nil, err
		}
		return &pixelDataValue{PixelDataInfo: info}, nil
	case []*SequenceItemValue:
		items := make([]*SequenceItemValue, len(value))
		for i, item := range value {
			elems, err := deepCopyElements(item.elements)
			if err != nil {
				return nil, err
			}
			items[i] = &SequenceItemValue{elements: elems}
		}
		return &sequencesValue{value: items}, nil
	case []*Element:
		elems, err := deepCopyElements(value)
		if err != nil {
			return nil, err
		}
		return &SequenceItemValue{elements: elems}, nil
	default:
		return nil, fmt.Errorf("%w: cannot deep-copy value of type %T (ValueType %v)", ErrorUnexpectedValueType, v.GetValue(), v.ValueType())
	}
}

func deepCopyPixelDataInfo(info PixelDataInfo) (PixelDataInfo, error) {
	copied := PixelDataInfo{
		IntentionallySkipped:     info.IntentionallySkipped,
		ParseErr:                 info.ParseErr,
		IsEncapsulated:           info.IsEncapsulated,
		Offsets:                  append([]uint32(nil), info.Offsets...),
		IntentionallyUnprocessed: info.IntentionallyUnprocessed,
		UnprocessedValueData:     append([]byte(nil), info.UnprocessedValueData...),
	}
	if info.Frames != nil {
		copied.Frames = make([]*frame.Frame, len(info.Frames))
		for i, fr := range info.Frames {
			frameCopy, err := deepCopyFrame(fr)
			if err != nil {
				return PixelDataInfo{}, err
			}
			copied.Frames[i] = frameCopy
		}
	}
	return copied, nil
}

func deepCopyFrame(fr *frame.Frame) (*frame.Frame, error) {
	if fr == nil {
		return nil, nil
	}
	copied := &frame.Frame{Encapsulated: fr.Encapsulated}
	copied.EncapsulatedData.Data = append([]byte(nil), fr.EncapsulatedData.Data...)
	if !fr.Encapsulated && fr.NativeData != nil {
		nativeCopy, err := deepCopyNativeFrame(fr.NativeData)
		if err != nil {
			return nil, err
		}
		copied.NativeData = nativeCopy
	}
	return copied, nil
}

func deepCopyNativeFrame(native frame.INativeFrame) (frame.INativeFrame, error) {
	switch nf := native.(type) {
	case *frame.NativeFrame[int]:
		return &frame.NativeFrame[int]{
			RawData:                 append([]int(nil), nf.RawData...),
			InternalSamplesPerPixel: nf.InternalSamplesPerPixel,
			InternalRows:            nf.InternalRows,
			InternalCols:            nf.InternalCols,
			InternalBitsPerSample:   nf.InternalBitsPerSample,
		}, nil
	case *frame.NativeFrame[uint8]:
		return &frame.NativeFrame[uint8]{
			RawData:                 append([]uint8(nil), nf.RawData...),
			InternalSamplesPerPixel: nf.InternalSamplesPerPixel,
			InternalRows:            nf.InternalRows,
			InternalCols:            nf.InternalCols,
			InternalBitsPerSample:   nf.InternalBitsPerSample,
		}, nil
	case *frame.NativeFrame[uint16]:
		return &frame.NativeFrame[uint16]{
			RawData:                 append([]uint16(nil), nf.RawData...),
			InternalSamplesPerPixel: nf.InternalSamplesPerPixel,
			InternalRows:            nf.InternalRows,
			InternalCols:            nf.InternalCols,
			InternalBitsPerSample:   nf.InternalBitsPerSample,
		}, nil
	case *frame.NativeFrame[uint32]:
		return &frame.NativeFrame[uint32]{
			RawData:                 append([]uint32(nil), nf.RawData...),
			InternalSamplesPerPixel: nf.InternalSamplesPerPixel,
			InternalRows:            nf.InternalRows,
			InternalCols:            nf.InternalCols,
			InternalBitsPerSample:   nf.InternalBitsPerSample,
		}, nil
	default:
		return nil, fmt.Errorf("%w: unsupported native frame type %T", ErrorUnexpectedValueType, native)
	}
}
