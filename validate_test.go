package dicom

import (
	"strings"
	"testing"

	"github.com/suyashkumar/dicom/pkg/tag"
)

func elem(tg tag.Tag, rawVR string, value Value) *Element {
	return &Element{
		Tag:                    tg,
		RawValueRepresentation: rawVR,
		ValueRepresentation:    tag.GetVRKind(tg, rawVR),
		Value:                  value,
	}
}

func strsElem(tg tag.Tag, rawVR string, ss ...string) *Element {
	v, _ := NewValue(ss)
	return elem(tg, rawVR, v)
}

func intsElem(tg tag.Tag, rawVR string, is ...int) *Element {
	v, _ := NewValue(is)
	return elem(tg, rawVR, v)
}

func bytesElem(tg tag.Tag, rawVR string, b []byte) *Element {
	v, _ := NewValue(b)
	return elem(tg, rawVR, v)
}

func issueCodes(report Report) []string {
	codes := make([]string, 0, len(report.Issues))
	for _, is := range report.Issues {
		codes = append(codes, is.Code)
	}
	return codes
}

func containsCode(codes []string, want string) bool {
	for _, c := range codes {
		if c == want {
			return true
		}
	}
	return false
}

// A single element can violate more than one rule; all independent violations
// must be reported on that element.
func TestValidate_ElementMultipleViolations(t *testing.T) {
	// PatientID (0010,0020) is LO, VM 1. CS is not a permitted VR for it, so
	// the concrete VR mismatch is a definite error.
	bad := &Element{
		Tag:                    tag.PatientID, // LO, VM 1
		RawValueRepresentation: "CS",          // CS is not permitted for PatientID
		ValueRepresentation:    tag.VRStringList,
		Value:                  mustNewValue([]string{"A", "B"}),
	}

	report := ValidateDataset(&Dataset{Elements: []*Element{bad}})
	if !report.HasErrors() {
		t.Fatalf("expected errors, got report %+v", report)
	}
	codes := issueCodes(report)
	if !containsCode(codes, IssueVRNotAllowed) {
		t.Errorf("expected %s, got %v", IssueVRNotAllowed, codes)
	}

	// A US element that is out of range AND wrong multiplicity: both findings.
	multi := intsElem(tag.Rows, "US", 1, 70000) // Rows is US, VM 1
	report = ValidateDataset(&Dataset{Elements: []*Element{multi}})
	codes = issueCodes(report)
	if !containsCode(codes, IssueVMMismatch) || !containsCode(codes, IssueValueOutOfRange) {
		t.Errorf("expected VMMismatch and ValueOutOfRange, got %v", codes)
	}
}

// Legal multi-valued elements across fixed, interval, open, and multiple VM
// forms must validate cleanly.
func TestValidate_LegalMultiplicityForms(t *testing.T) {
	d := &Dataset{Elements: []*Element{
		strsElem(tag.SpecificCharacterSet, "CS", "ISO_IR 100", "ISO_IR 101"), // 1-n
		strsElem(tag.ImageType, "CS", "ORIGINAL", "PRIMARY"),                 // 2-n
		strsElem(tag.ShutterShape, "CS", "CIRCULAR", "RECTANGULAR"),          // 1-3
		intsElem(tag.ApplicableFrameRange, "US", 1, 2, 3, 4),                 // 2-2n (4)
		intsElem(tag.Rows, "US", 512),                                        // fixed 1
	}}
	report := d.Validate()
	if !report.Valid() {
		t.Fatalf("expected valid dataset, got %+v", report.Issues)
	}
}

func TestValidate_VMMultiplesAndRanges(t *testing.T) {
	// 3 values violates 2-2n; 2 values satisfies it.
	bad := &Dataset{Elements: []*Element{
		intsElem(tag.ApplicableFrameRange, "US", 1, 2, 3),
	}}
	if r := bad.Validate(); !r.HasErrors() {
		t.Errorf("expected VM violation for 3 values against 2-2n, got %+v", r.Issues)
	}

	// 4 values violates 1-3.
	badRange := &Dataset{Elements: []*Element{
		strsElem(tag.ShutterShape, "CS", "A", "B", "C", "D"),
	}}
	if r := badRange.Validate(); !r.HasErrors() {
		t.Errorf("expected VM violation for 4 values against 1-3, got %+v", r.Issues)
	}
}

// Empty value fields are structurally legal for presence; presence/Type
// requirements are an IOD concern this validator does not assess.
func TestValidate_EmptyValuesAreStructural(t *testing.T) {
	d := &Dataset{Elements: []*Element{
		strsElem(tag.PatientID, "LO", ""),
		intsElem(tag.Rows, "US"),
		bytesElem(tag.OverlayData, "OW", nil),
	}}
	report := d.Validate()
	for _, is := range report.Issues {
		if is.Code == IssueVMMismatch {
			t.Errorf("empty value must not be a VM violation, got %s", is)
		}
	}
}

// UN on a standard tag is undetermined (opaque/context dependent), not a pass;
// an unrecognized two-character VR is a definite error.
func TestValidate_UnknownAndUnrecognizedVR(t *testing.T) {
	// PatientID with UN: undetermined, and carried as bytes here.
	un := bytesElem(tag.PatientID, "UN", []byte{0x41})
	report := ValidateDataset(&Dataset{Elements: []*Element{un}})
	if report.Valid() {
		t.Fatalf("UN element must not validate as success")
	}
	if len(report.Undetermined()) == 0 {
		t.Errorf("expected an undetermined finding for UN, got %+v", report.Issues)
	}

	// A made-up VR "XY" on a known tag is a structural error.
	badVR := strsElem(tag.PatientID, "XY", "ABC")
	report = ValidateDataset(&Dataset{Elements: []*Element{badVR}})
	if !report.HasErrors() {
		t.Errorf("expected error for unrecognized VR, got %+v", report.Issues)
	}
	if !containsCode(issueCodes(report), IssueUnrecognizedVR) {
		t.Errorf("expected %s", IssueUnrecognizedVR)
	}
}

// Context-dependent VRs: when the element supplies a concrete allowed VR, the
// element can be judged; OB/OW presented concretely is checked intrinsically.
func TestValidate_ContextDependentVRResolved(t *testing.T) {
	// OverlayData allows OB or OW; an odd-length OW stream is a structural
	// error even though the tag is multi-VR.
	d := &Dataset{Elements: []*Element{
		bytesElem(tag.OverlayData, "OW", []byte{0x00, 0x01, 0x02}),
	}}
	report := d.Validate()
	if !containsCode(issueCodes(report), IssueBinaryLength) {
		t.Fatalf("expected odd OW stream to be flagged, got %+v", report.Issues)
	}

	// Even-length OW is fine (VM must NOT be inferred from byte length).
	ok := &Dataset{Elements: []*Element{
		bytesElem(tag.OverlayData, "OW", []byte{0x00, 0x01, 0x02, 0x03, 0x04, 0x05}),
	}}
	if r := ok.Validate(); !r.Valid() {
		t.Errorf("binary VM must not be derived from byte length; got %+v", r.Issues)
	}

	// SmallestImagePixelValue allows US/SS; choosing SS with an out-of-SS
	// value is an error, proving the concrete VR's range is applied.
	usss := &Dataset{Elements: []*Element{
		intsElem(tag.SmallestImagePixelValue, "SS", 40000),
	}}
	if r := usss.Validate(); !containsCode(issueCodes(r), IssueValueOutOfRange) {
		t.Errorf("expected SS range check, got %+v", r.Issues)
	}
}

// Unknown private tags are undetermined rather than successful, even when the
// bytes look tidy.
func TestValidate_UnknownPrivateTag(t *testing.T) {
	private := bytesElem(tag.Tag{Group: 0x0009, Element: 0x1001}, "UN", []byte{0x01})
	report := ValidateDataset(&Dataset{Elements: []*Element{private}})
	if report.Valid() {
		t.Fatalf("unknown private tag must not count as valid")
	}
	if !containsCode(issueCodes(report), IssueUnknownTag) {
		t.Errorf("expected %s, got %v", IssueUnknownTag, issueCodes(report))
	}
	for _, is := range report.Issues {
		if is.Severity != SeverityUndetermined {
			t.Errorf("expected undetermined only for tidy unknown private tag, got %s", is)
		}
	}
}

// Sequence items are validated recursively with a full tag/item path.
func TestValidate_NestedSequencePath(t *testing.T) {
	innerBad := intsElem(tag.Rows, "US", 1, 2) // VM 1 violation inside item 2
	innerSequence := makeSequenceElement(tag.ConceptNameCodeSequence, [][]*Element{
		{
			strsElem(tag.CodeValue, "SH", "12345"), // legal
		},
		{
			innerBad,
			strsElem(tag.CodeValue, "SH", "this-code-value-is-clearly-too-long-for-sh"), // too long
		},
	})

	outer := makeSequenceElement(tag.ReferencedSOPSequence, [][]*Element{
		{
			strsElem(tag.ReferencedSOPClassUID, "UI", "1.2.840.10008.5.1.4.1.1.2"),
			innerSequence,
		},
	})

	report := ValidateDataset(&Dataset{Elements: []*Element{outer}})
	if !report.HasErrors() {
		t.Fatalf("expected nested violations, got %+v", report.Issues)
	}

	var sawVM, sawLen bool
	for _, is := range report.Issues {
		if is.Code == IssueVMMismatch && strings.Contains(is.Path, "(0028,0010)") {
			sawVM = true
			want := "(0008,1199)/item[1]/(0040,a043)/item[2]/(0028,0010)"
			if is.Path != want {
				t.Errorf("path = %q, want %q", is.Path, want)
			}
		}
		if is.Code == IssueStringTooLong && strings.Contains(is.Path, "(0008,0100)") {
			sawLen = true
			want := "(0008,1199)/item[1]/(0040,a043)/item[2]/(0008,0100)"
			if is.Path != want {
				t.Errorf("path = %q, want %q", is.Path, want)
			}
		}
	}
	if !sawVM || !sawLen {
		t.Errorf("expected nested VM and string-length findings with paths; got %+v", report.Issues)
	}
}

func TestValidate_MaxIssuesTruncation(t *testing.T) {
	var elems []*Element
	for i := 0; i < 10; i++ {
		elems = append(elems, intsElem(tag.Rows, "US", 1, 2)) // each: VM violation
	}
	report := ValidateDataset(&Dataset{Elements: elems}, WithMaxIssues(3))
	if len(report.Issues) != 3 {
		t.Fatalf("expected 3 reported issues, got %d", len(report.Issues))
	}
	if !report.Truncated {
		t.Errorf("expected Truncated=true")
	}

	// Unlimited report collects everything.
	full := ValidateDataset(&Dataset{Elements: elems}, WithMaxIssues(0))
	if full.Truncated || len(full.Issues) != 10 {
		t.Errorf("expected 10 issues and no truncation, got %d (truncated=%v)", len(full.Issues), full.Truncated)
	}
}

// Validation is read-only: it must never mutate element values.
func TestValidate_DoesNotMutate(t *testing.T) {
	long := strings.Repeat("A", 20)
	d := &Dataset{Elements: []*Element{
		strsElem(tag.CodeValue, "SH", long), // exceeds SH max 16
	}}
	before := d.Elements[0].Value.GetValue().([]string)[0]
	_ = d.Validate()
	after := d.Elements[0].Value.GetValue().([]string)[0]
	if before != after || len(after) != 20 {
		t.Errorf("validator mutated the value: before=%q after=%q", before, after)
	}
}

func TestValidate_ValueShapeMismatch(t *testing.T) {
	// A US element carrying strings is a value-type mismatch regardless of VM.
	bad := strsElem(tag.Rows, "US", "512")
	report := ValidateDataset(&Dataset{Elements: []*Element{bad}})
	if !containsCode(issueCodes(report), IssueValueTypeMismatch) {
		t.Fatalf("expected ValueTypeMismatch, got %v", issueCodes(report))
	}

	// A valid, clean dataset.
	clean := &Dataset{Elements: []*Element{
		strsElem(tag.PatientID, "LO", "PAT001"),
		intsElem(tag.Rows, "US", 1024),
	}}
	if r := clean.Validate(); !r.Valid() {
		t.Errorf("expected clean dataset to validate, got %+v", r.Issues)
	}
}

func TestValidate_NilAndNilValue(t *testing.T) {
	if r := ValidateDataset(nil); !r.Valid() {
		t.Errorf("nil dataset should produce empty report, got %+v", r.Issues)
	}
	r := ValidateDataset(&Dataset{Elements: []*Element{nil, &Element{
		Tag:                    tag.Rows,
		RawValueRepresentation: "US",
		Value:                  nil,
	}}})
	if !r.HasErrors() {
		t.Errorf("expected errors for nil element and nil value, got %+v", r.Issues)
	}
}

func floatsElem(tg tag.Tag, fs ...float64) *Element {
	v, _ := NewValue(fs)
	return elem(tg, "FD", v)
}

// Float VR multiplicity is checked against the dictionary; the float domain
// is the full IEEE-754 range so there is no range bound.
func TestValidate_FloatVM(t *testing.T) {
	// TimeRange (0018,1115?) is FD, VM 2.
	bad := &Dataset{Elements: []*Element{floatsElem(tag.TimeRange, 1.0)}}
	if r := bad.Validate(); !containsCode(issueCodes(r), IssueVMMismatch) {
		t.Errorf("expected FD VM violation, got %+v", r.Issues)
	}
	good := &Dataset{Elements: []*Element{floatsElem(tag.TimeRange, 1.0, 2.0)}}
	if r := good.Validate(); !r.Valid() {
		t.Errorf("expected valid FD pair, got %+v", r.Issues)
	}
}

// AT values are ordered pairs of 16-bit words; VM is counted in tags (words/2).
func TestValidate_AttributeTag(t *testing.T) {
	// DataElement is AT, VM 1: two words = one tag, valid.
	good := &Dataset{Elements: []*Element{intsElem(tag.DataElement, "AT", 0x0008, 0x0016)}}
	if r := good.Validate(); !r.Valid() {
		t.Errorf("expected valid AT value, got %+v", r.Issues)
	}

	// Three words: odd pairing is an error.
	odd := &Dataset{Elements: []*Element{intsElem(tag.DataElement, "AT", 0x0008, 0x0016, 0x7fe0)}}
	if r := odd.Validate(); !containsCode(issueCodes(r), IssueATPair) {
		t.Errorf("expected ATPair error, got %+v", r.Issues)
	}

	// Four words = two tags violates VM 1; one component is also out of range.
	twoTags := &Dataset{Elements: []*Element{intsElem(tag.DataElement, "AT", 0x0008, 0x0016, 0x7fe0, 0x10000)}}
	codes := issueCodes(twoTags.Validate())
	if !containsCode(codes, IssueVMMismatch) || !containsCode(codes, IssueValueOutOfRange) {
		t.Errorf("expected AT VM violation and range error, got %v", codes)
	}
}

// Single-valued text VRs (LT) are VM 1 and bounded per the VR table; embedded
// backslashes do not create extra VM values.
func TestValidate_SingleTextLength(t *testing.T) {
	short := strings.Repeat("x", 10240)
	ok := &Dataset{Elements: []*Element{strsElem(tag.ExtendedCodeMeaning, "LT", short)}}
	if r := ok.Validate(); !r.Valid() {
		t.Errorf("expected LT at exactly max length to pass, got %+v", r.Issues)
	}

	long := strings.Repeat("x", 10241)
	bad := &Dataset{Elements: []*Element{strsElem(tag.ExtendedCodeMeaning, "LT", long)}}
	if r := bad.Validate(); !containsCode(issueCodes(r), IssueStringTooLong) {
		t.Errorf("expected StringTooLong for LT, got %+v", r.Issues)
	}
}

// A dictionary VM like "1-n or 1" (LUTData) accepts any non-empty value.
func TestValidate_AlternativeVM(t *testing.T) {
	d := &Dataset{Elements: []*Element{
		intsElem(tag.LUTData, "US", 1, 2, 3),
	}}
	if r := d.Validate(); !r.Valid() {
		t.Errorf("expected 1-n or 1 to accept 3 values, got %+v", r.Issues)
	}
}
