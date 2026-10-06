package dicom

import (
	"strings"
	"testing"

	"github.com/suyashkumar/dicom/pkg/tag"
)

func issueKinds(r *Report) []IssueKind {
	kinds := make([]IssueKind, 0, len(r.Issues))
	for _, issue := range r.Issues {
		kinds = append(kinds, issue.Kind)
	}
	return kinds
}

func hasKind(r *Report, kind IssueKind) bool {
	for _, issue := range r.Issues {
		if issue.Kind == kind {
			return true
		}
	}
	return false
}

func TestValidateDataset_ValidMultiValue(t *testing.T) {
	ds := &Dataset{Elements: []*Element{
		// ImagePositionPatient: DS, VM 3.
		mustNewElement(tag.ImagePositionPatient, []string{"1.5", "-2.5", "30"}),
		// ModalitiesInStudy: CS, VM 1-n.
		mustNewElement(tag.ModalitiesInStudy, []string{"CT", "MR"}),
		// VerticesOfThePolygonalCollimator: IS, VM 2-2n.
		mustNewElement(tag.VerticesOfThePolygonalCollimator, []string{"1", "2", "3", "4"}),
	}}
	report := ValidateDataset(ds)
	if !report.OK() {
		t.Fatalf("expected valid dataset, got issues: %v", report.Issues)
	}
}

func TestValidateDataset_MultipleViolationsOnOneElement(t *testing.T) {
	// VerticesOfThePolygonalCollimator: IS, VM 2-2n. Three values violate the
	// VM, and the 13-byte value exceeds the 12 byte IS limit.
	ds := &Dataset{Elements: []*Element{
		mustNewElement(tag.VerticesOfThePolygonalCollimator, []string{"1234567890123", "2", "3"}),
	}}
	report := ValidateDataset(ds)
	if report.OK() {
		t.Fatal("expected issues, got none")
	}
	if !hasKind(report, IssueVMViolation) {
		t.Errorf("expected a VM violation, got %v", issueKinds(report))
	}
	if !hasKind(report, IssueStringTooLong) {
		t.Errorf("expected a string length violation, got %v", issueKinds(report))
	}
	if len(report.Issues) != 2 {
		t.Errorf("expected exactly 2 issues for the element, got %d: %v", len(report.Issues), report.Issues)
	}
}

func TestValidateDataset_EmptyValue(t *testing.T) {
	// PatientName: PN, VM 1. An empty (zero-length) value is structurally
	// acceptable; presence requirements are IOD-level concerns.
	ds := &Dataset{Elements: []*Element{
		mustNewElement(tag.PatientName, []string{}),
	}}
	report := ValidateDataset(ds)
	if !report.OK() {
		t.Fatalf("expected empty value to be valid, got: %v", report.Issues)
	}
}

func TestValidateDataset_BinaryValueNotCountedAsVM(t *testing.T) {
	// DarkCurrentCounts: OB/OW, VM 1. A 4-byte value is a single binary
	// value, not four values.
	ds := &Dataset{Elements: []*Element{
		mustNewElement(tag.DarkCurrentCounts, []byte{0x1, 0x2, 0x3, 0x4}),
	}}
	report := ValidateDataset(ds)
	if !report.OK() {
		t.Fatalf("expected binary value to count as one value, got: %v", report.Issues)
	}
}

func TestValidateDataset_UnknownPrivateTag(t *testing.T) {
	ds := &Dataset{Elements: []*Element{
		mustNewPrivateElement(tag.Tag{Group: 0x0029, Element: 0x1001}, "UN", []byte{0x1}),
	}}
	report := ValidateDataset(ds)
	if report.OK() {
		t.Fatal("unknown private tag must not count as a successful validation")
	}
	if !hasKind(report, IssueUnknownTag) {
		t.Errorf("expected IssueUnknownTag, got %v", issueKinds(report))
	}
}

func TestValidateDataset_UndeterminedContextDependentVR(t *testing.T) {
	// SmallestImagePixelValue allows US or SS depending on
	// PixelRepresentation; with a UN VR there is not enough evidence.
	ds := &Dataset{Elements: []*Element{
		mustNewPrivateElement(tag.SmallestImagePixelValue, "UN", []byte{0x1, 0x2}),
	}}
	report := ValidateDataset(ds)
	if report.OK() {
		t.Fatal("expected undetermined item, got a passing report")
	}
	if !hasKind(report, IssueUndetermined) {
		t.Errorf("expected IssueUndetermined, got %v", issueKinds(report))
	}
}

func TestValidateDataset_NestedSequenceItemPath(t *testing.T) {
	inner := mustNewElement(tag.Manufacturer, []string{strings.Repeat("x", 65)})
	seq := makeSequenceElement(tag.ContentSequence, [][]*Element{{inner}})
	ds := &Dataset{Elements: []*Element{seq}}

	report := ValidateDataset(ds)
	if report.OK() {
		t.Fatal("expected issues from nested item, got none")
	}
	wantPath := tag.ContentSequence.String() + "[0]>" + tag.Manufacturer.String()
	found := false
	for _, issue := range report.Issues {
		if issue.Kind == IssueStringTooLong && issue.Path == wantPath {
			found = true
		}
	}
	if !found {
		t.Errorf("expected IssueStringTooLong at path %q, got %v", wantPath, report.Issues)
	}
}

func TestValidateDataset_VMAndValueTypeMismatch(t *testing.T) {
	// Rows: US, VM 1. Storing strings is a value type mismatch.
	badType := &Element{
		Tag:                    tag.Rows,
		ValueRepresentation:    tag.GetVRKind(tag.Rows, "US"),
		RawValueRepresentation: "US",
		Value:                  mustNewValue([]string{"not-an-int"}),
	}
	// FileMetaInformationGroupLength: UL, VM 1. Two values violate the VM.
	badVM := mustNewElement(tag.FileMetaInformationGroupLength, []int{1, 2})
	ds := &Dataset{Elements: []*Element{badType, badVM}}

	report := ValidateDataset(ds)
	if !hasKind(report, IssueValueTypeMismatch) {
		t.Errorf("expected IssueValueTypeMismatch, got %v", issueKinds(report))
	}
	if !hasKind(report, IssueVMViolation) {
		t.Errorf("expected IssueVMViolation, got %v", issueKinds(report))
	}
}

func TestValidateDataset_VRMismatch(t *testing.T) {
	// PatientName is PN; claiming it is AS is a VR mismatch.
	elem := &Element{
		Tag:                    tag.PatientName,
		ValueRepresentation:    tag.GetVRKind(tag.PatientName, "AS"),
		RawValueRepresentation: "AS",
		Value:                  mustNewValue([]string{"Doe^John"}),
	}
	report := ValidateDataset(&Dataset{Elements: []*Element{elem}})
	if !hasKind(report, IssueVRMismatch) {
		t.Errorf("expected IssueVRMismatch, got %v", issueKinds(report))
	}
}

func TestValidateDataset_CharacterBasedLengthLimit(t *testing.T) {
	// LO is limited to 64 characters, not bytes: 65 multi-byte runes exceed
	// the character limit even though the byte length is larger.
	ds := &Dataset{Elements: []*Element{
		mustNewElement(tag.Manufacturer, []string{strings.Repeat("é", 65)}),
	}}
	report := ValidateDataset(ds)
	if !hasKind(report, IssueStringTooLong) {
		t.Errorf("expected IssueStringTooLong for 65 characters, got %v", issueKinds(report))
	}

	// 64 multi-byte characters are within the character limit.
	ds = &Dataset{Elements: []*Element{
		mustNewElement(tag.Manufacturer, []string{strings.Repeat("é", 64)}),
	}}
	if report := ValidateDataset(ds); !report.OK() {
		t.Errorf("expected 64 characters to be valid, got %v", report.Issues)
	}
}

func TestValidateDataset_Truncation(t *testing.T) {
	var elems []*Element
	for i := 0; i < 5; i++ {
		// Each element produces one VM violation (VM 1, two values).
		elems = append(elems, mustNewElement(tag.Rows, []int{1, 2}))
	}
	report := ValidateDataset(&Dataset{Elements: elems}, WithMaxIssues(2))
	if len(report.Issues) != 2 {
		t.Errorf("expected 2 issues with max 2, got %d", len(report.Issues))
	}
	if !report.Truncated {
		t.Error("expected Truncated to be true")
	}
	if report.OK() {
		t.Error("truncated report must not be OK")
	}

	report = ValidateDataset(&Dataset{Elements: elems})
	if len(report.Issues) != 5 || report.Truncated {
		t.Errorf("expected 5 issues and no truncation, got %d issues, truncated=%t", len(report.Issues), report.Truncated)
	}
}

func TestValidateDataset_DoesNotModifyDataset(t *testing.T) {
	elem := mustNewElement(tag.Rows, []int{1, 2})
	ds := &Dataset{Elements: []*Element{elem}}
	_ = ValidateDataset(ds)
	got := MustGetInts(ds.Elements[0].Value)
	if len(got) != 2 || got[0] != 1 || got[1] != 2 {
		t.Errorf("validation modified the dataset: %v", got)
	}
}

func TestParseVMRule(t *testing.T) {
	cases := []struct {
		vm      string
		allowed []int
		denied  []int
	}{
		{"1", []int{0, 1}, []int{2, 5}},
		{"3", []int{0, 3}, []int{1, 2, 4}},
		{"1-2", []int{0, 1, 2}, []int{3}},
		{"1-n", []int{0, 1, 2, 100}, []int{}},
		{"2-2n", []int{0, 2, 4, 6}, []int{1, 3, 5}},
		{"3-3n", []int{0, 3, 6}, []int{1, 4, 5}},
	}
	for _, c := range cases {
		rule, err := parseVMRule(c.vm)
		if err != nil {
			t.Fatalf("parseVMRule(%q) unexpected error: %v", c.vm, err)
		}
		for _, n := range c.allowed {
			if !rule.allows(n) {
				t.Errorf("parseVMRule(%q).allows(%d) = false, want true", c.vm, n)
			}
		}
		for _, n := range c.denied {
			if rule.allows(n) {
				t.Errorf("parseVMRule(%q).allows(%d) = true, want false", c.vm, n)
			}
		}
	}
	for _, invalid := range []string{"", "n", "1-", "a-b", "3-2", "1-0n"} {
		if _, err := parseVMRule(invalid); err == nil {
			t.Errorf("parseVMRule(%q) expected error, got nil", invalid)
		}
	}
}
