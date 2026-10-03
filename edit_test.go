package dicom

import (
	"bytes"
	"errors"
	"strings"
	"testing"

	"github.com/suyashkumar/dicom/pkg/frame"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// nestedEditDataset builds a dataset with two levels of nesting and many
// elements that share the PatientName tag:
//
//	PatientID                       (top level)
//	PatientName                     (top level)
//	AnatomicRegionSequence:
//	  item0:
//	    PatientName                 ("item0-name")
//	    AnatomicRegionSequence:
//	      item0: PatientName ("inner0-name"), PatientID ("inner0-id")
//	      item1: PatientName ("inner1-name"), PatientID ("inner1-id")
//	  item1:
//	    PatientName                 ("item1-name")
//	    AnatomicRegionSequence:
//	      item0: PatientName ("innerA-name"), PatientID ("innerA-id")
func nestedEditDataset() *Dataset {
	return &Dataset{Elements: []*Element{
		mustNewElement(tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.1.2"}),
		mustNewElement(tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.5.6.7"}),
		mustNewElement(tag.TransferSyntaxUID, []string{uid.ImplicitVRLittleEndian}),
		mustNewElement(tag.PatientID, []string{"PAT-0001"}),
		mustNewElement(tag.PatientName, []string{"top-name"}),
		mustNewElement(tag.Modality, []string{"MR"}),
		makeSequenceElement(tag.AnatomicRegionSequence, [][]*Element{
			{
				mustNewElement(tag.PatientName, []string{"item0-name"}),
				mustNewElement(tag.PatientID, []string{"item0-id"}),
				makeSequenceElement(tag.AnatomicRegionSequence, [][]*Element{
					{
						mustNewElement(tag.PatientName, []string{"inner0-name"}),
						mustNewElement(tag.PatientID, []string{"inner0-id"}),
					},
					{
						mustNewElement(tag.PatientName, []string{"inner1-name"}),
						mustNewElement(tag.PatientID, []string{"inner1-id"}),
					},
				}),
			},
			{
				mustNewElement(tag.PatientName, []string{"item1-name"}),
				mustNewElement(tag.PatientID, []string{"item1-id"}),
				makeSequenceElement(tag.AnatomicRegionSequence, [][]*Element{
					{
						mustNewElement(tag.PatientName, []string{"innerA-name"}),
						mustNewElement(tag.PatientID, []string{"innerA-id"}),
					},
				}),
			},
		}),
	}}
}

func arsPath() []PathStep {
	return []PathStep{
		TagStep{Tag: tag.AnatomicRegionSequence},
	}
}

func arsItemPath(outer int) []PathStep {
	return []PathStep{
		TagStep{Tag: tag.AnatomicRegionSequence},
		ItemStep{Index: outer},
		TagStep{Tag: tag.PatientName},
	}
}

func innerPath(outer, inner int, tg tag.Tag) []PathStep {
	return []PathStep{
		TagStep{Tag: tag.AnatomicRegionSequence},
		ItemStep{Index: outer},
		TagStep{Tag: tag.AnatomicRegionSequence},
		ItemStep{Index: inner},
		TagStep{Tag: tg},
	}
}

func innerSequencePath(outer int) []PathStep {
	return []PathStep{
		TagStep{Tag: tag.AnatomicRegionSequence},
		ItemStep{Index: outer},
		TagStep{Tag: tag.AnatomicRegionSequence},
	}
}

func outerSequencePath() []PathStep {
	return []PathStep{TagStep{Tag: tag.AnatomicRegionSequence}}
}

func stringAt(t *testing.T, d *Dataset, path []PathStep) string {
	t.Helper()
	target, perr := resolveElementPath(d, path)
	if perr != nil {
		t.Fatalf("resolveElementPath(%v) returned unexpected error: %v", path, perr)
	}
	values, ok := target.elem.Value.GetValue().([]string)
	if !ok || len(values) != 1 {
		t.Fatalf("expected single string at %v, got %#v", path, target.elem.Value.GetValue())
	}
	return values[0]
}

func seqItemCountAt(t *testing.T, d *Dataset, path []PathStep) int {
	t.Helper()
	target, perr := resolveElementPath(d, path)
	if perr != nil {
		t.Fatalf("resolveElementPath(%v) returned unexpected error: %v", path, perr)
	}
	if target.elem.Value.ValueType() != Sequences {
		t.Fatalf("expected sequence at %v, got ValueType %v", path, target.elem.Value.ValueType())
	}
	return len(target.elem.Value.GetValue().([]*SequenceItemValue))
}

func TestApplyEdits_NestedSequencesReplaceInsertDelete(t *testing.T) {
	original := nestedEditDataset()

	innerZero := innerPath(0, 0, tag.PatientName)
	innerOne := innerPath(0, 1, tag.PatientName)

	edits := []Edit{
		// 1. Replace a doubly-nested value. The other same-tag elements must
		// not change.
		{
			Type:  ReplaceValue,
			Path:  innerZero,
			Value: mustNewElement(tag.PatientName, []string{"inner0-CHANGED"}).Value,
		},
		// 2. Insert into the inner sequence of outer item 1 (which only has
		// one item before this edit). Insert at position 1 appends.
		{
			Type:      InsertSequenceItem,
			Path:      innerSequencePath(1),
			ItemIndex: 1,
			Item: []*Element{
				mustNewElement(tag.PatientName, []string{"innerB-name"}),
				mustNewElement(tag.PatientID, []string{"innerB-id"}),
			},
		},
		// 3. After the insert, outer-1 now has two inner items. Insert again
		// at position 0 so the newly inserted "innerB" shifts to index 2.
		{
			Type:      InsertSequenceItem,
			Path:      innerSequencePath(1),
			ItemIndex: 0,
			Item: []*Element{
				mustNewElement(tag.PatientName, []string{"innerNew-name"}),
			},
		},
		// 4. Delete the inner1-name element from outer 0, inner 1.
		{
			Type: DeleteElement,
			Path: innerOne,
		},
		// 5. Delete an entire outer item. Item indices here are relative to
		// the post-insert state above (outer sequence length still 2).
		{
			Type: DeleteElement,
			Path: append(arsPath(), ItemStep{Index: 1}),
		},
	}

	result, err := original.ApplyEdits(edits)
	if err != nil {
		t.Fatalf("ApplyEdits returned unexpected error: %v", err)
	}

	if got := stringAt(t, result, innerZero); got != "inner0-CHANGED" {
		t.Errorf("inner0 patient name = %q, want %q", got, "inner0-CHANGED")
	}

	// Untouched same-tag elements retain their values.
	if got := stringAt(t, result, arsItemPath(0)); got != "item0-name" {
		t.Errorf("outer0 item0 PatientName = %q, want %q", got, "item0-name")
	}
	if got := stringAt(t, result, []PathStep{TagStep{Tag: tag.PatientName}}); got != "top-name" {
		t.Errorf("top-level PatientName = %q, want %q", got, "top-name")
	}
	if got := stringAt(t, result, innerPath(0, 1, tag.PatientID)); got != "inner1-id" {
		t.Errorf("inner1 patient id = %q, want %q (sibling of deleted name must survive)", got, "inner1-id")
	}

	// inner0 still has two items; its inner1 PatientName element was
	// deleted but the item remains (only PatientID inside).
	if got := seqItemCountAt(t, result, innerSequencePath(0)); got != 2 {
		t.Errorf("outer0 inner sequence length = %d, want 2", got)
	}
	if got := countTagInItem(t, result, innerPath(0, 1, tag.PatientID), tag.PatientName); got != 0 {
		t.Errorf("PatientName count in outer0/inner1 after delete = %d, want 0", got)
	}

	// Outer item 1 was deleted entirely, taking its two inserted inner
	// items with it.
	if got := seqItemCountAt(t, result, outerSequencePath()); got != 1 {
		t.Errorf("outer sequence length = %d, want 1", got)
	}

	// The original dataset and all of its slices are untouched.
	if got := stringAt(t, original, innerZero); got != "inner0-name" {
		t.Errorf("original dataset mutated: inner0 patient name = %q, want %q", got, "inner0-name")
	}
	if got := seqItemCountAt(t, original, innerSequencePath(1)); got != 1 {
		t.Errorf("original dataset mutated: outer1 inner sequence length = %d, want 1", got)
	}
	if got := seqItemCountAt(t, original, outerSequencePath()); got != 2 {
		t.Errorf("original dataset mutated: outer sequence length = %d, want 2", got)
	}
}

func countTagInItem(t *testing.T, d *Dataset, pathToSibling []PathStep, tg tag.Tag) int {
	t.Helper()
	// Resolve the element addressed by pathToSibling, then use the item
	// holder recorded during resolution. A simpler and sufficient approach
	// here is to resolve the sequence that contains the final item and
	// count within that item.
	// For ..., TagStep(sequence), ItemStep(item), TagStep(target), the
	// enclosing sequence is addressed by [:len-2] and the item index is at
	// len-2.
	itemStep := pathToSibling[len(pathToSibling)-2].(ItemStep)
	seq, perr := resolveSequenceAt(d, pathToSibling[:len(pathToSibling)-2])
	if perr != nil {
		t.Fatalf("resolveSequenceAt: %v", perr)
	}
	count := 0
	for _, elem := range seq.items[itemStep.Index].elements {
		if elem.Tag == tg {
			count++
		}
	}
	return count
}

func TestApplyEdits_FailureRollsBackWholeBatch(t *testing.T) {
	snapshot := mustDeepCopyDataset(t, nestedEditDataset())

	cases := []struct {
		name         string
		edits        []Edit
		wantOp       int
		wantSeg      int
		wantSentinel error
		wantText     string
	}{
		{
			name: "missing tag at second level",
			edits: []Edit{
				{Type: ReplaceValue, Path: arsItemPath(0), Value: mustNewElement(tag.PatientName, []string{"x"}).Value},
				{
					Type:  ReplaceValue,
					Path:  []PathStep{TagStep{Tag: tag.AnatomicRegionSequence}, ItemStep{Index: 0}, TagStep{Tag: tag.Modality}},
					Value: mustNewElement(tag.Modality, []string{"CT"}).Value,
				},
			},
			wantOp:       2,
			wantSeg:      3,
			wantSentinel: ErrorPathNotFound,
			wantText:     tag.Modality.String(),
		},
		{
			name: "item index out of range",
			edits: []Edit{
				{Type: ReplaceValue, Path: arsItemPath(0), Value: mustNewElement(tag.PatientName, []string{"x"}).Value},
				{
					Type:      InsertSequenceItem,
					Path:      innerSequencePath(0),
					ItemIndex: 5,
					Item:      []*Element{mustNewElement(tag.PatientName, []string{"x"})},
				},
			},
			wantOp:       2,
			wantSeg:      3,
			wantSentinel: ErrorIndexOutOfRange,
		},
		{
			name: "delete item out of range",
			edits: []Edit{
				{Type: DeleteElement, Path: append(outerSequencePath(), ItemStep{Index: 9})},
			},
			wantOp:       1,
			wantSeg:      2,
			wantSentinel: ErrorIndexOutOfRange,
		},
		{
			name: "value type incompatible with VR",
			edits: []Edit{
				{Type: ReplaceValue, Path: []PathStep{TagStep{Tag: tag.Modality}}, Value: mustNewElement(tag.Rows, []int{42}).Value},
			},
			wantOp:       1,
			wantSeg:      1,
			wantSentinel: ErrorMismatchValueTypeAndVR,
		},
		{
			name: "insert into a non-sequence element",
			edits: []Edit{
				{
					Type:      InsertSequenceItem,
					Path:      []PathStep{TagStep{Tag: tag.PatientName}},
					ItemIndex: 0,
					Item:      []*Element{mustNewElement(tag.PatientID, []string{"x"})},
				},
			},
			wantOp:       1,
			wantSeg:      1,
			wantSentinel: ErrorInvalidPath,
		},
		{
			name: "inserted element has wrong VR for its tag",
			edits: []Edit{
				{
					Type:      InsertSequenceItem,
					Path:      outerSequencePath(),
					ItemIndex: 0,
					Item:      []*Element{mustNewElement(tag.Rows, []string{"not-ints"})},
				},
			},
			wantOp:       1,
			wantSeg:      1,
			wantSentinel: ErrorMismatchValueTypeAndVR,
		},
		{
			name: "index applied to a non-sequence",
			edits: []Edit{
				{
					Type:  ReplaceValue,
					Path:  []PathStep{TagStep{Tag: tag.PatientName}, ItemStep{Index: 0}, TagStep{Tag: tag.PatientID}},
					Value: mustNewElement(tag.PatientID, []string{"x"}).Value,
				},
			},
			wantOp:       1,
			wantSeg:      2,
			wantSentinel: ErrorInvalidPath,
		},
		{
			name: "empty path",
			edits: []Edit{
				{Type: ReplaceValue, Path: nil, Value: mustNewElement(tag.Modality, []string{"CT"}).Value},
			},
			wantOp:   1,
			wantText: "empty",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := nestedEditDataset()
			result, err := d.ApplyEdits(tc.edits)
			if err == nil {
				t.Fatalf("ApplyEdits unexpectedly succeeded; result=%v", result)
			}
			if result != nil {
				t.Fatalf("ApplyEdits returned a non-nil result on failure")
			}
			if tc.wantSentinel != nil && !errors.Is(err, tc.wantSentinel) {
				t.Errorf("error %q does not wrap %v", err, tc.wantSentinel)
			}
			if tc.wantText != "" && !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("error %q does not mention %q", err, tc.wantText)
			}
			if !strings.Contains(err.Error(), "edit #") ||
				(tc.wantSeg != 0 && !strings.Contains(err.Error(), "segment")) {
				t.Errorf("error %q does not identify operation and segment", err)
			}
			if tc.wantOp != 0 && !strings.Contains(err.Error(), "edit #"+itoa(tc.wantOp)) {
				t.Errorf("error %q does not identify failing operation #%d", err, tc.wantOp)
			}
			if tc.wantSeg != 0 && !strings.Contains(err.Error(), "segment #"+itoa(tc.wantSeg)) {
				t.Errorf("error %q does not identify failing segment #%d", err, tc.wantSeg)
			}
			if !d.Equals(snapshot) {
				t.Errorf("dataset was modified by a failed batch:\n%v", d)
			}
		})
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

func mustDeepCopyDataset(t *testing.T, d *Dataset) *Dataset {
	t.Helper()
	copied, err := d.DeepCopy()
	if err != nil {
		t.Fatalf("DeepCopy: %v", err)
	}
	return copied
}

func TestApplyEdits_TopLevelDuplicateTagIsAmbiguous(t *testing.T) {
	d := &Dataset{Elements: []*Element{
		mustNewElement(tag.PatientName, []string{"first"}),
		mustNewElement(tag.PatientName, []string{"second"}),
	}}
	_, err := d.ApplyEdits([]Edit{
		{Type: ReplaceValue, Path: []PathStep{TagStep{Tag: tag.PatientName}}, Value: mustNewElement(tag.PatientName, []string{"x"}).Value},
	})
	if !errors.Is(err, ErrorAmbiguousPath) {
		t.Fatalf("expected ErrorAmbiguousPath, got: %v", err)
	}
	if !strings.Contains(err.Error(), "edit #1") || !strings.Contains(err.Error(), "segment #1") {
		t.Fatalf("error %q does not locate op/segment", err)
	}
	if got := MustGetStrings(d.Elements[0].Value)[0]; got != "first" {
		t.Errorf("original dataset mutated: %q", got)
	}
}

func TestApplyEdits_RepeatedInputReferencesAreIndependentCopies(t *testing.T) {
	original := nestedEditDataset()

	// The same *Element instance is used in two inserted items at distinct
	// paths. Mutating the result at one path must not affect the other.
	shared := mustNewElement(tag.PatientName, []string{"shared"})
	result, err := original.ApplyEdits([]Edit{
		{Type: InsertSequenceItem, Path: innerSequencePath(0), ItemIndex: 0, Item: []*Element{shared}},
		{Type: InsertSequenceItem, Path: innerSequencePath(1), ItemIndex: 0, Item: []*Element{shared}},
	})
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}

	pathFirst := innerPath(0, 0, tag.PatientName)
	pathSecond := innerPath(1, 0, tag.PatientName)
	if got := stringAt(t, result, pathFirst); got != "shared" {
		t.Fatalf("first inserted name = %q, want %q", got, "shared")
	}
	if got := stringAt(t, result, pathSecond); got != "shared" {
		t.Fatalf("second inserted name = %q, want %q", got, "shared")
	}

	// Replace at the first path; the second path and the caller's element
	// must be unaffected.
	result2, err := result.ApplyEdits([]Edit{
		{Type: ReplaceValue, Path: pathFirst, Value: mustNewElement(tag.PatientName, []string{"only-first"}).Value},
	})
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	if got := stringAt(t, result2, pathFirst); got != "only-first" {
		t.Errorf("first name = %q, want %q", got, "only-first")
	}
	if got := stringAt(t, result2, pathSecond); got != "shared" {
		t.Errorf("second name leaked change: %q, want %q", got, "shared")
	}
	if got := MustGetStrings(shared.Value)[0]; got != "shared" {
		t.Errorf("caller-supplied element mutated: %q", got)
	}
}

func TestApplyEdits_SubsequentIndicesReflectPriorOperations(t *testing.T) {
	original := nestedEditDataset()
	// Inner sequence of outer item 0 starts with two items. Insert at 0
	// twice, then delete index 2 (which is what used to be index 0) and
	// resolve the survivors by their post-edit indices.
	newInner := func(name string) []*Element {
		return []*Element{mustNewElement(tag.PatientName, []string{name})}
	}
	result, err := original.ApplyEdits([]Edit{
		{Type: InsertSequenceItem, Path: innerSequencePath(0), ItemIndex: 0, Item: newInner("a")},
		{Type: InsertSequenceItem, Path: innerSequencePath(0), ItemIndex: 0, Item: newInner("b")},
		{Type: DeleteElement, Path: append(innerSequencePath(0), ItemStep{Index: 2})},
	})
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}
	// After inserts: [b, a, inner0, inner1]. Delete index 2 removes
	// inner0, leaving [b, a, inner1].
	wantOrder := []string{"b", "a", "inner1-name"}
	for i, want := range wantOrder {
		if got := stringAt(t, result, innerPath(0, i, tag.PatientName)); got != want {
			t.Errorf("inner item %d name = %q, want %q", i, got, want)
		}
	}
}

func TestApplyEdits_WriteParseRoundTrip(t *testing.T) {
	original := nestedEditDataset()
	result, err := original.ApplyEdits([]Edit{
		{Type: ReplaceValue, Path: innerPath(0, 0, tag.PatientName), Value: mustNewElement(tag.PatientName, []string{"roundtrip-CHANGED"}).Value},
		{Type: InsertSequenceItem, Path: innerSequencePath(1), ItemIndex: 1, Item: []*Element{
			mustNewElement(tag.PatientName, []string{"roundtrip-new"}),
		}},
		{Type: DeleteElement, Path: innerPath(0, 1, tag.PatientName)},
	})
	if err != nil {
		t.Fatalf("ApplyEdits: %v", err)
	}

	buf := &bytes.Buffer{}
	if err := Write(buf, *result); err != nil {
		t.Fatalf("Write: %v", err)
	}
	parsed, err := ParseUntilEOF(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatalf("ParseUntilEOF: %v", err)
	}

	checks := []struct {
		path []PathStep
		want string
	}{
		{[]PathStep{TagStep{Tag: tag.PatientID}}, "PAT-0001"},
		{[]PathStep{TagStep{Tag: tag.PatientName}}, "top-name"},
		{[]PathStep{TagStep{Tag: tag.Modality}}, "MR"},
		{arsItemPath(0), "item0-name"},
		{innerPath(0, 0, tag.PatientName), "roundtrip-CHANGED"},
		{innerPath(0, 0, tag.PatientID), "inner0-id"},
		{innerPath(0, 1, tag.PatientID), "inner1-id"},
	}
	for _, c := range checks {
		if got := stringAt(t, &parsed, c.path); got != c.want {
			t.Errorf("at %v got %q, want %q", c.path, got, c.want)
		}
	}
	// Deleted element stays gone; inserted item survives with its value.
	if got := countTagInItem(t, &parsed, innerPath(0, 1, tag.PatientID), tag.PatientName); got != 0 {
		t.Errorf("deleted PatientName count = %d, want 0", got)
	}
	if got := stringAt(t, &parsed, innerPath(1, 1, tag.PatientName)); got != "roundtrip-new" {
		t.Errorf("inserted inner item name = %q, want %q", got, "roundtrip-new")
	}
	if got := seqItemCountAt(t, &parsed, innerSequencePath(1)); got != 2 {
		t.Errorf("outer1 inner item count = %d, want 2", got)
	}
}

func TestDeepCopy_IsolatesPixelDataAndSlices(t *testing.T) {
	original := &Dataset{Elements: []*Element{
		mustNewElement(tag.PatientID, []string{"P-1"}),
		{
			Tag:                    tag.PixelData,
			RawValueRepresentation: "OW",
			ValueRepresentation:    tag.VRPixelData,
			ValueLength:            tag.VLUndefinedLength,
			Value: &pixelDataValue{PixelDataInfo: PixelDataInfo{
				IsEncapsulated: true,
				Frames: []*frame.Frame{{
					Encapsulated:     true,
					EncapsulatedData: frame.EncapsulatedFrame{Data: []byte{0xAA, 0xBB, 0xCC}},
				}},
			}},
		},
	}}

	copied, err := original.DeepCopy()
	if err != nil {
		t.Fatalf("DeepCopy: %v", err)
	}

	origInfo := MustGetPixelDataInfo(original.Elements[1].Value)
	copyInfo := MustGetPixelDataInfo(copied.Elements[1].Value)
	copyInfo.Frames[0].EncapsulatedData.Data[0] = 0x00

	if origInfo.Frames[0].EncapsulatedData.Data[0] != 0xAA {
		t.Errorf("original pixel data mutated through the copy: %v", origInfo.Frames[0].EncapsulatedData.Data)
	}

	// Top-level slices are independent too.
	copied.Elements[0] = mustNewElement(tag.PatientID, []string{"CHANGED"})
	if got := MustGetStrings(original.Elements[0].Value)[0]; got != "P-1" {
		t.Errorf("original element slice mutated through the copy: %q", got)
	}
}

func TestApplyEdits_NilReceiverAndNilDatasetSafe(t *testing.T) {
	var d *Dataset
	if _, err := d.ApplyEdits(nil); err == nil {
		t.Fatalf("expected error for nil receiver, got nil")
	}
	copied, err := d.DeepCopy()
	if err != nil || copied != nil {
		t.Fatalf("DeepCopy on nil = (%v, %v), want (nil, nil)", copied, err)
	}
}
