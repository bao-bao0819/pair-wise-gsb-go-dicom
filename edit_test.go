package dicom

import (
	"bytes"
	"errors"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// findByPath walks a Path with the same "no descent" semantics as
// ApplyEdits: tag steps search only the current element list. It is a test
// helper for asserting where edits landed.
func findByPath(t *testing.T, d *Dataset, path Path) *Element {
	t.Helper()
	list := &d.Elements
	var seqElem *Element
	for seg, step := range path {
		if step.isTag {
			if list == nil {
				t.Fatalf("tag step %s at segment %d has no enclosing list", step.tag, seg)
			}
			idx := findElementIndex(*list, step.tag)
			if idx < 0 {
				t.Fatalf("tag %s not found at segment %d", step.tag, seg)
			}
			elem := (*list)[idx]
			if seg == len(path)-1 {
				return elem
			}
			if elem.Value == nil || elem.Value.ValueType() != Sequences {
				t.Fatalf("tag %s at segment %d is not a sequence", step.tag, seg)
			}
			seqElem = elem
			list = nil
			continue
		}
		if seqElem == nil {
			t.Fatalf("item step at segment %d has no enclosing sequence", seg)
		}
		items := seqElem.Value.GetValue().([]*SequenceItemValue)
		if step.index < 0 || step.index >= len(items) {
			t.Fatalf("item index %d out of range at segment %d (len=%d)",
				step.index, seg, len(items))
		}
		list = &items[step.index].elements
		seqElem = nil
	}
	t.Fatal("empty path")
	return nil
}

// nestedEditDataset builds a dataset with two levels of nested sequences and
// several repeated PatientName/PatientID tags across distinct branches:
//
//	root
//	  PatientName          "rootPN"
//	  PatientID            "rootID"
//	  StudyDescription     "study"
//	  AnatomicRegionSequence (SQ)
//	    item0
//	      PatientName      "outer1PN"
//	      PatientID       "outer1ID"
//	      AddOtherSequence (SQ)
//	        item0
//	          PatientName  "inner1PN"
//	          PatientID    "inner1ID"
//	    item1
//	      PatientName      "outer2PN"
//	      PatientID       "outer2ID"
func nestedEditDataset() *Dataset {
	return &Dataset{Elements: []*Element{
		mustNewElement(tag.PatientName, []string{"rootPN"}),
		mustNewElement(tag.PatientID, []string{"rootID"}),
		mustNewElement(tag.StudyDescription, []string{"study"}),
		makeSequenceElement(tag.AnatomicRegionSequence, [][]*Element{
			{
				mustNewElement(tag.PatientName, []string{"outer1PN"}),
				mustNewElement(tag.PatientID, []string{"outer1ID"}),
				makeSequenceElement(tag.AddOtherSequence, [][]*Element{
					{
						mustNewElement(tag.PatientName, []string{"inner1PN"}),
						mustNewElement(tag.PatientID, []string{"inner1ID"}),
					},
				}),
			},
			{
				mustNewElement(tag.PatientName, []string{"outer2PN"}),
				mustNewElement(tag.PatientID, []string{"outer2ID"}),
			},
		}),
	}}
}

func mustStringsValue(s string) Value {
	v, err := NewValue([]string{s})
	if err != nil {
		panic(err)
	}
	return v
}

func mustSequencesForTest() Value {
	v, err := NewValue([][]*Element{{}})
	if err != nil {
		panic(err)
	}
	return v
}

func assertEditError(t *testing.T, err error, opIndex, segment int, sentinel error) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected an edit error, got nil")
	}
	var ee *EditError
	if !errors.As(err, &ee) {
		t.Fatalf("expected *EditError, got %T: %v", err, err)
	}
	if ee.OpIndex != opIndex {
		t.Errorf("OpIndex: got %d, want %d", ee.OpIndex, opIndex)
	}
	if ee.Segment != segment {
		t.Errorf("Segment: got %d, want %d (error: %v)", ee.Segment, segment, err)
	}
	if !errors.Is(err, sentinel) {
		t.Errorf("expected error to wrap %v, got %v", sentinel, err)
	}
}

func TestApplyEdits_BatchOperations(t *testing.T) {
	original := nestedEditDataset()
	snapshot := cloneDataset(original)

	innerPNPath := Path{
		TagPathStep(tag.AnatomicRegionSequence),
		ItemPathStep(0),
		TagPathStep(tag.AddOtherSequence),
		ItemPathStep(0),
		TagPathStep(tag.PatientName),
	}
	innerSeqPath := Path{
		TagPathStep(tag.AnatomicRegionSequence),
		ItemPathStep(0),
		TagPathStep(tag.AddOtherSequence),
	}

	insertedItem := []*Element{
		mustNewElement(tag.PatientName, []string{"innerNEWPN"}),
		mustNewElement(tag.PatientID, []string{"innerNEWID"}),
	}

	result, err := original.ApplyEdits(
		// 1. Replace a deeply nested value, disambiguated by the path.
		SetValueOp{Path: innerPNPath, Value: mustStringsValue("inner1CHANGED")},
		// 2. Insert an item into the inner sequence (it had one item).
		InsertItemOp{Path: innerSeqPath, Index: 1, Item: insertedItem},
		// 3. Indices now observe the prior insert: the old item 0 is still
		//    at 0, the new item is at 1. Set value via the shifted index.
		SetValueOp{Path: Path{
			TagPathStep(tag.AnatomicRegionSequence),
			ItemPathStep(0),
			TagPathStep(tag.AddOtherSequence),
			ItemPathStep(1),
			TagPathStep(tag.PatientID),
		}, Value: mustStringsValue("innerNEWID2")},
		// 4. Insert at the front of the outer sequence and delete a leaf
		//    inside the original (now shifted) item.
		InsertItemOp{
			Path:  Path{TagPathStep(tag.AnatomicRegionSequence)},
			Index: 0,
			Item: []*Element{
				mustNewElement(tag.PatientName, []string{"outerNEWPN"}),
				mustNewElement(tag.PatientID, []string{"outerNEWID"}),
			},
		},
		DeleteElementOp{Path: Path{
			TagPathStep(tag.AnatomicRegionSequence),
			ItemPathStep(2), // original item1 shifted from index 1
			TagPathStep(tag.PatientID),
		}},
		// 5. Replace a top-level value.
		SetValueOp{Path: Path{TagPathStep(tag.PatientName)}, Value: mustStringsValue("rootPN2")},
	)
	if err != nil {
		t.Fatalf("ApplyEdits returned unexpected error: %v", err)
	}

	// The original dataset must be untouched.
	if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
		t.Fatalf("original dataset was modified by ApplyEdits (-want +got):\n%s", diff)
	}

	// Top-level replacement.
	if got := MustGetStrings(findByPath(t, result, Path{TagPathStep(tag.PatientName)}).Value); got[0] != "rootPN2" {
		t.Errorf("root PatientName: got %v, want rootPN2", got)
	}
	if got := MustGetStrings(findByPath(t, result, Path{TagPathStep(tag.PatientID)}).Value); got[0] != "rootID" {
		t.Errorf("root PatientID should be unchanged: got %v", got)
	}
	if got := MustGetStrings(findByPath(t, result, Path{TagPathStep(tag.StudyDescription)}).Value); got[0] != "study" {
		t.Errorf("StudyDescription should be unchanged: got %v", got)
	}

	// Deep replacement landed on inner1, not on any outer same-tag element.
	// After the outer front insert, original outer item0 is now index 1.
	innerPNAfterShift := Path{
		TagPathStep(tag.AnatomicRegionSequence),
		ItemPathStep(1),
		TagPathStep(tag.AddOtherSequence),
		ItemPathStep(0),
		TagPathStep(tag.PatientName),
	}
	if got := MustGetStrings(findByPath(t, result, innerPNAfterShift).Value)[0]; got != "inner1CHANGED" {
		t.Errorf("inner1 PN: got %v, want inner1CHANGED", got)
	}

	// Outer sequence: new item at front, then original item0, item1 (minus PID).
	outerSeq := findByPath(t, result, Path{TagPathStep(tag.AnatomicRegionSequence)})
	outerItems := outerSeq.Value.GetValue().([]*SequenceItemValue)
	if len(outerItems) != 3 {
		t.Fatalf("outer sequence item count: got %d, want 3", len(outerItems))
	}
	assertPN := func(itemIndex int, wantPN, wantIDOrEmpty string, expectID bool) {
		item := outerItems[itemIndex]
		pnIdx := findElementIndex(item.elements, tag.PatientName)
		if pnIdx < 0 {
			t.Fatalf("outer item %d missing PatientName", itemIndex)
		}
		if got := MustGetStrings(item.elements[pnIdx].Value)[0]; got != wantPN {
			t.Errorf("outer item %d PN: got %q, want %q", itemIndex, got, wantPN)
		}
		idIdx := findElementIndex(item.elements, tag.PatientID)
		if expectID {
			if idIdx < 0 {
				t.Fatalf("outer item %d missing PatientID", itemIndex)
			}
			if got := MustGetStrings(item.elements[idIdx].Value)[0]; got != wantIDOrEmpty {
				t.Errorf("outer item %d PID: got %q, want %q", itemIndex, got, wantIDOrEmpty)
			}
		} else if idIdx >= 0 {
			t.Errorf("outer item %d PatientID should have been deleted", itemIndex)
		}
	}
	assertPN(0, "outerNEWPN", "outerNEWID", true)
	assertPN(1, "outer1PN", "outer1ID", true)
	assertPN(2, "outer2PN", "", false)

	// Inner sequence inside original outer item0 (now index 1).
	innerSeq := findByPath(t, result, Path{
		TagPathStep(tag.AnatomicRegionSequence),
		ItemPathStep(1),
		TagPathStep(tag.AddOtherSequence),
	})
	innerItems := innerSeq.Value.GetValue().([]*SequenceItemValue)
	if len(innerItems) != 2 {
		t.Fatalf("inner sequence item count: got %d, want 2", len(innerItems))
	}
	innerPN0 := MustGetStrings(findElement(innerItems[0].elements, tag.PatientName).Value)[0]
	innerPN1 := MustGetStrings(findElement(innerItems[1].elements, tag.PatientName).Value)[0]
	innerID1 := MustGetStrings(findElement(innerItems[1].elements, tag.PatientID).Value)[0]
	if innerPN0 != "inner1CHANGED" {
		t.Errorf("inner item0 PN: got %q, want inner1CHANGED", innerPN0)
	}
	if innerPN1 != "innerNEWPN" {
		t.Errorf("inner item1 PN: got %q, want innerNEWPN", innerPN1)
	}
	if innerID1 != "innerNEWID2" {
		t.Errorf("inner item1 PID (shifted-index set): got %q, want innerNEWID2", innerID1)
	}

	// The caller's inserted slice must not be retained/shared.
	if got := insertedItem[1].Value.(*stringsValue).value[0]; got != "innerNEWID" {
		t.Errorf("caller's inserted slice unexpectedly mutated: got %q", got)
	}
}

func findElement(elems []*Element, tg tag.Tag) *Element {
	idx := findElementIndex(elems, tg)
	if idx < 0 {
		return nil
	}
	return elems[idx]
}

func TestApplyEdits_FailureRollback(t *testing.T) {
	innerPNPath := Path{
		TagPathStep(tag.AnatomicRegionSequence),
		ItemPathStep(0),
		TagPathStep(tag.AddOtherSequence),
		ItemPathStep(0),
		TagPathStep(tag.PatientName),
	}

	// Two tag steps in a row (missing item step) is an invalid path, never
	// a descent into the first same-tag nested element.
	t.Run("no_descent_without_item_step", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		_, err := original.ApplyEdits(
			SetValueOp{Path: Path{TagPathStep(tag.PatientName)}, Value: mustStringsValue("x")},
			// op #2 targets PatientName inside the outer sequence but omits
			// the item step; it must not descend into it.
			SetValueOp{
				Path: Path{
					TagPathStep(tag.AnatomicRegionSequence),
					TagPathStep(tag.PatientName),
				},
				Value: mustStringsValue("y"),
			},
		)
		assertEditError(t, err, 1, 1, ErrorEditInvalidPath)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})

	// A tag that only exists nested deeper must not be matched from a list
	// where it is absent.
	t.Run("absent_tag_not_matched_from_nested_namesake", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		_, err := original.ApplyEdits(
			// AddOtherSequence only exists inside outer item0; from the root
			// it must not be found.
			SetValueOp{Path: Path{TagPathStep(tag.AddOtherSequence)}, Value: mustSequencesForTest()},
		)
		assertEditError(t, err, 0, 0, ErrorEditPathNotFound)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})

	t.Run("missing_deep_segment", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		_, err := original.ApplyEdits(
			SetValueOp{Path: innerPNPath, Value: mustStringsValue("changed")},
			DeleteElementOp{Path: Path{
				TagPathStep(tag.AnatomicRegionSequence),
				ItemPathStep(0),
				TagPathStep(tag.AddOtherSequence),
				ItemPathStep(0),
				TagPathStep(tag.Rows), // not present in the inner item
			}},
		)
		assertEditError(t, err, 1, 4, ErrorEditPathNotFound)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})

	t.Run("item_index_out_of_range", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		_, err := original.ApplyEdits(
			SetValueOp{
				Path: Path{
					TagPathStep(tag.AnatomicRegionSequence),
					ItemPathStep(5), // outer sequence has 2 items
					TagPathStep(tag.PatientName),
				},
				Value: mustStringsValue("x"),
			},
		)
		assertEditError(t, err, 0, 1, ErrorEditIndexOutOfRange)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})

	t.Run("traverse_non_sequence", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		_, err := original.ApplyEdits(
			SetValueOp{
				Path: Path{
					TagPathStep(tag.PatientName), // a PN, not an SQ
					ItemPathStep(0),
					TagPathStep(tag.PatientName),
				},
				Value: mustStringsValue("x"),
			},
		)
		assertEditError(t, err, 0, 0, ErrorEditInvalidPath)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})

	t.Run("path_ends_on_item_step", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		_, err := original.ApplyEdits(
			SetValueOp{
				Path: Path{
					TagPathStep(tag.AnatomicRegionSequence),
					ItemPathStep(0),
				},
				Value: mustStringsValue("x"),
			},
		)
		assertEditError(t, err, 0, 1, ErrorEditInvalidPath)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})

	t.Run("value_type_mismatch", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		intsVal, nerr := NewValue([]int{123})
		if nerr != nil {
			t.Fatal(nerr)
		}
		// PatientName is VR PN (Strings); an Ints value must be rejected.
		_, err := original.ApplyEdits(
			SetValueOp{Path: innerPNPath, Value: intsVal},
		)
		assertEditError(t, err, 0, 4, ErrorEditInvalidValue)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})

	t.Run("insert_index_out_of_range", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		_, err := original.ApplyEdits(
			InsertItemOp{
				Path:  Path{TagPathStep(tag.AnatomicRegionSequence)},
				Index: 9, // 2 items exist; append max is index 2
				Item: []*Element{
					mustNewElement(tag.PatientName, []string{"z"}),
				},
			},
		)
		assertEditError(t, err, 0, 0, ErrorEditIndexOutOfRange)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})

	t.Run("insert_target_not_sequence", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		_, err := original.ApplyEdits(
			InsertItemOp{
				Path:  Path{TagPathStep(tag.PatientName)},
				Index: 0,
				Item: []*Element{
					mustNewElement(tag.PatientName, []string{"z"}),
				},
			},
		)
		assertEditError(t, err, 0, 0, ErrorEditInvalidPath)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})

	t.Run("inserted_child_vr_mismatch", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		badElem := &Element{
			Tag:                    tag.PatientName,
			RawValueRepresentation: "PN",
			ValueRepresentation:    tag.VRString,
			Value:                  mustNewValue([]int{1}),
		}
		_, err := original.ApplyEdits(
			InsertItemOp{
				Path:  Path{TagPathStep(tag.AnatomicRegionSequence)},
				Index: 0,
				Item:  []*Element{badElem},
			},
		)
		assertEditError(t, err, 0, 0, ErrorEditInvalidValue)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})

	t.Run("later_op_failure_rolls_back_earlier_ops", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		_, err := original.ApplyEdits(
			SetValueOp{Path: innerPNPath, Value: mustStringsValue("changed")},
			InsertItemOp{
				Path:  Path{TagPathStep(tag.AnatomicRegionSequence)},
				Index: 0,
				Item: []*Element{
					mustNewElement(tag.PatientName, []string{"new"}),
				},
			},
			// op #3 references an item beyond the post-insert bounds.
			SetValueOp{
				Path: Path{
					TagPathStep(tag.AnatomicRegionSequence),
					ItemPathStep(99),
					TagPathStep(tag.PatientName),
				},
				Value: mustStringsValue("x"),
			},
		)
		assertEditError(t, err, 2, 1, ErrorEditIndexOutOfRange)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})

	t.Run("empty_path", func(t *testing.T) {
		original := nestedEditDataset()
		snapshot := cloneDataset(original)
		_, err := original.ApplyEdits(DeleteElementOp{Path: Path{}})
		assertEditError(t, err, 0, 0, ErrorEditInvalidPath)
		if diff := cmp.Diff(snapshot, original, cmp.AllowUnexported(allValues...)); diff != "" {
			t.Fatalf("dataset changed on failure (-want +got):\n%s", diff)
		}
	})
}

func TestApplyEdits_NoAliasing(t *testing.T) {
	t.Run("shared_input_element_pointer_is_decoupled", func(t *testing.T) {
		// The same *Element pointer is used in two unrelated inserted
		// items. Editing through one path must not touch the other.
		original := nestedEditDataset()
		shared := mustNewElement(tag.PatientName, []string{"shared"})

		result, err := original.ApplyEdits(
			InsertItemOp{
				Path:  Path{TagPathStep(tag.AnatomicRegionSequence)},
				Index: 0,
				Item:  []*Element{shared},
			},
			InsertItemOp{
				Path: Path{
					TagPathStep(tag.AnatomicRegionSequence),
					ItemPathStep(1),
					TagPathStep(tag.AddOtherSequence),
				},
				Index: 0,
				Item:  []*Element{shared},
			},
		)
		if err != nil {
			t.Fatalf("ApplyEdits error: %v", err)
		}

		// Change the value in the first insertion only.
		result2, err := result.ApplyEdits(
			SetValueOp{
				Path: Path{
					TagPathStep(tag.AnatomicRegionSequence),
					ItemPathStep(0),
					TagPathStep(tag.PatientName),
				},
				Value: mustStringsValue("firstOnly"),
			},
		)
		if err != nil {
			t.Fatalf("second ApplyEdits error: %v", err)
		}

		second := findByPath(t, result2, Path{
			TagPathStep(tag.AnatomicRegionSequence),
			ItemPathStep(1),
			TagPathStep(tag.AddOtherSequence),
			ItemPathStep(0),
			TagPathStep(tag.PatientName),
		})
		if got := MustGetStrings(second.Value)[0]; got != "shared" {
			t.Errorf("unrelated shared element changed: got %q, want %q", got, "shared")
		}
		first := findByPath(t, result2, Path{
			TagPathStep(tag.AnatomicRegionSequence),
			ItemPathStep(0),
			TagPathStep(tag.PatientName),
		})
		if got := MustGetStrings(first.Value)[0]; got != "firstOnly" {
			t.Errorf("target element not changed: got %q, want %q", got, "firstOnly")
		}

		// The caller's element pointer is untouched.
		if got := MustGetStrings(shared.Value)[0]; got != "shared" {
			t.Errorf("caller element mutated: got %q", got)
		}
	})

	t.Run("set_value_slice_is_decoupled", func(t *testing.T) {
		original := nestedEditDataset()
		val := []string{"fromSlice"}
		newValue, err := NewValue(val)
		if err != nil {
			t.Fatal(err)
		}
		result, err := original.ApplyEdits(
			SetValueOp{Path: Path{TagPathStep(tag.PatientName)}, Value: newValue},
		)
		if err != nil {
			t.Fatalf("ApplyEdits error: %v", err)
		}
		// Mutate the caller's backing slice.
		val[0] = "mutated"
		got := MustGetStrings(findByPath(t, result, Path{TagPathStep(tag.PatientName)}).Value)[0]
		if got != "fromSlice" {
			t.Errorf("result value aliases caller slice: got %q, want fromSlice", got)
		}
	})
}

// collectPNPID performs a depth-first traversal (sequences first expanded in
// item order) and returns every PatientName then PatientID string value.
func collectPNPID(elems []*Element) []string {
	var out []string
	var walk func([]*Element)
	walk = func(list []*Element) {
		for _, e := range list {
			if e.Tag == tag.PatientName {
				out = append(out, MustGetStrings(e.Value)...)
			}
			if e.Tag == tag.PatientID {
				out = append(out, MustGetStrings(e.Value)...)
			}
			if e.Value != nil && e.Value.ValueType() == Sequences {
				for _, item := range e.Value.GetValue().([]*SequenceItemValue) {
					walk(item.elements)
				}
			}
		}
	}
	walk(elems)
	return out
}

func TestApplyEdits_WriteParseRoundTrip(t *testing.T) {
	original := nestedEditDataset()
	// Add file-meta + transfer syntax so Write produces a complete DICOM.
	original.Elements = append([]*Element{
		mustNewElement(tag.MediaStorageSOPClassUID, []string{"1.2.840.10008.5.1.4.1.1.1.2"}),
		mustNewElement(tag.MediaStorageSOPInstanceUID, []string{"1.2.3.4.5.6.7"}),
		mustNewElement(tag.TransferSyntaxUID, []string{uid.ExplicitVRLittleEndian}),
	}, original.Elements...)

	result, err := original.ApplyEdits(
		SetValueOp{Path: Path{TagPathStep(tag.PatientName)}, Value: mustStringsValue("rootPN2")},
		InsertItemOp{
			Path: Path{
				TagPathStep(tag.AnatomicRegionSequence),
				ItemPathStep(0),
				TagPathStep(tag.AddOtherSequence),
			},
			Index: 1,
			Item: []*Element{
				mustNewElement(tag.PatientName, []string{"innerNEWPN"}),
				mustNewElement(tag.PatientID, []string{"innerNEWID"}),
			},
		},
		DeleteElementOp{Path: Path{
			TagPathStep(tag.AnatomicRegionSequence),
			ItemPathStep(1),
			TagPathStep(tag.PatientID),
		}},
	)
	if err != nil {
		t.Fatalf("ApplyEdits error: %v", err)
	}

	buf := &bytes.Buffer{}
	if err := Write(buf, *result); err != nil {
		t.Fatalf("Write error: %v", err)
	}
	parsed, err := ParseUntilEOF(bytes.NewReader(buf.Bytes()), nil)
	if err != nil {
		t.Fatalf("ParseUntilEOF error: %v", err)
	}

	got := collectPNPID(parsed.Elements)
	want := []string{
		// Depth-first: each node emits its own PN/PID before children.
		"rootPN2", "rootID",
		// outer item0 (unchanged) and its inner sequence (item0 original,
		// item1 inserted)
		"outer1PN", "outer1ID",
		"inner1PN", "inner1ID",
		"innerNEWPN", "innerNEWID",
		// outer item1 (PID deleted)
		"outer2PN",
	}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("roundtrip PN/PID values mismatch (-want +got):\n%s", diff)
	}

	// Unmodified non-name elements survive the round trip.
	studyElem, err := parsed.FindElementByTag(tag.StudyDescription)
	if err != nil {
		t.Fatalf("StudyDescription lost after roundtrip: %v", err)
	}
	if got := MustGetStrings(studyElem.Value)[0]; got != "study" {
		t.Errorf("StudyDescription: got %q, want study", got)
	}

	// All modified state also matches before/after serialization at the
	// structural level for the touched region.
	if diff := cmp.Diff(
		collectPNPID(result.Elements),
		collectPNPID(parsed.Elements),
	); diff != "" {
		t.Errorf("pre/post serialize values differ (-before +after):\n%s", diff)
	}
}

func TestApplyEdits_DeleteAllThenInsert(t *testing.T) {
	// A sequence with a single item whose element list holds one tag;
	// deleting that leaf and inserting a fresh item into the (now
	// unaffected) outer sequence must both succeed in one batch.
	d := &Dataset{Elements: []*Element{
		makeSequenceElement(tag.AnatomicRegionSequence, [][]*Element{
			{mustNewElement(tag.PatientName, []string{"onlyPN"})},
		}),
	}}
	result, err := d.ApplyEdits(
		DeleteElementOp{Path: Path{
			TagPathStep(tag.AnatomicRegionSequence),
			ItemPathStep(0),
			TagPathStep(tag.PatientName),
		}},
		// After op1 the outer sequence still has one (now empty) item;
		// appending at index 1 is valid.
		InsertItemOp{
			Path:  Path{TagPathStep(tag.AnatomicRegionSequence)},
			Index: 1,
			Item: []*Element{
				mustNewElement(tag.PatientName, []string{"second"}),
			},
		},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seq := findByPath(t, result, Path{TagPathStep(tag.AnatomicRegionSequence)})
	items := seq.Value.GetValue().([]*SequenceItemValue)
	if len(items) != 2 {
		t.Fatalf("item count: got %d, want 2", len(items))
	}
	if len(items[0].elements) != 0 {
		t.Errorf("first item should be empty, got %d elements", len(items[0].elements))
	}
	if got := MustGetStrings(findElement(items[1].elements, tag.PatientName).Value)[0]; got != "second" {
		t.Errorf("second item PN: got %q, want second", got)
	}
}

func TestPath_String(t *testing.T) {
	p := Path{
		TagPathStep(tag.AnatomicRegionSequence),
		ItemPathStep(2),
		TagPathStep(tag.PatientName),
	}
	if got, want := p.String(), "(0008,2218)[2]/(0010,0010)"; got != want {
		t.Errorf("Path.String(): got %q, want %q", got, want)
	}
}
