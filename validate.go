package dicom

import (
	"fmt"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/suyashkumar/dicom/pkg/tag"
)

// defaultMaxIssues bounds how many issues a Report collects by default before
// the report is truncated.
const defaultMaxIssues = 100

// IssueKind categorizes a single structural validation finding.
type IssueKind int

const (
	// IssueUnknownTag indicates the element's tag is not present in the DICOM
	// data dictionary (including unknown private tags). Unknown tags never
	// count as a successful validation.
	IssueUnknownTag IssueKind = iota
	// IssueVRMismatch indicates the element's value representation does not
	// match any of the VRs the data dictionary allows for its tag.
	IssueVRMismatch
	// IssueValueTypeMismatch indicates the Go value type stored by the
	// element is inconsistent with its value representation (e.g. strings
	// stored for a binary VR).
	IssueValueTypeMismatch
	// IssueVMViolation indicates the number of values violates the value
	// multiplicity (VM) rule defined for the tag (fixed counts, ranges,
	// "n", and multiple forms like "2-2n" are supported).
	IssueVMViolation
	// IssueStringTooLong indicates a string value exceeds the length limit
	// of its VR (character-based or byte-based, depending on the VR).
	IssueStringTooLong
	// IssueUndetermined indicates the element could not be conclusively
	// validated, e.g. a context-dependent VR with insufficient evidence or
	// an unknown ("UN") value representation.
	IssueUndetermined
)

// String returns a human-readable name for the IssueKind.
func (k IssueKind) String() string {
	switch k {
	case IssueUnknownTag:
		return "UnknownTag"
	case IssueVRMismatch:
		return "VRMismatch"
	case IssueValueTypeMismatch:
		return "ValueTypeMismatch"
	case IssueVMViolation:
		return "VMViolation"
	case IssueStringTooLong:
		return "StringTooLong"
	case IssueUndetermined:
		return "Undetermined"
	default:
		return "UnknownIssueKind"
	}
}

// Issue describes a single structural validation finding, located by a full
// tag/item path such as "(0040,a730)[1]>(0008,0100)".
type Issue struct {
	// Kind categorizes the finding.
	Kind IssueKind
	// Path is the full tag path of the offending element, including sequence
	// item indices for elements nested inside sequences.
	Path string
	// Tag is the tag of the offending element.
	Tag tag.Tag
	// Message holds human-readable detail about the finding.
	Message string
}

// String returns a human-readable representation of the Issue.
func (i Issue) String() string {
	return fmt.Sprintf("%s: %s: %s", i.Kind, i.Path, i.Message)
}

// Report aggregates the findings of a structural Dataset validation.
//
// A Report only describes the structural consistency of a Dataset (value
// types, value multiplicities, VR string length limits, and dictionary
// conformance of tags). It does NOT assert conformance to any DICOM IOD,
// nor any form of clinical or diagnostic compliance.
type Report struct {
	// Issues holds the collected findings, up to the configured maximum.
	Issues []Issue
	// Truncated is true if more issues were found than the configured
	// maximum, in which case Issues holds only the first ones encountered.
	Truncated bool
}

// OK returns true only if no issues of any kind (including undetermined
// items and unknown tags) were found.
func (r *Report) OK() bool {
	return r == nil || (len(r.Issues) == 0 && !r.Truncated)
}

// Error returns a single-line summary of the Report.
func (r *Report) Error() string {
	if r.OK() {
		return "dataset validation passed"
	}
	s := fmt.Sprintf("dataset validation found %d issue(s)", len(r.Issues))
	if r.Truncated {
		s += " (report truncated)"
	}
	return s
}

// ValidateOption configures ValidateDataset.
type ValidateOption func(*validateConfig)

type validateConfig struct {
	maxIssues int
}

// WithMaxIssues limits the number of issues collected in the Report. Once
// the limit is reached, no further issues are recorded and Report.Truncated
// is set to true. A value <= 0 selects the default limit.
func WithMaxIssues(n int) ValidateOption {
	return func(c *validateConfig) {
		c.maxIssues = n
	}
}

// ValidateDataset performs a structural validation of the given Dataset and
// returns a Report of all findings (up to the configured maximum).
//
// For every element (recursing into sequence items, reporting full tag/item
// paths) it checks:
//   - the tag exists in the DICOM data dictionary (unknown private tags are
//     reported and never count as a successful validation),
//   - the element's VR is consistent with the dictionary entry,
//   - the stored Go value type matches the VR,
//   - the number of values satisfies the dictionary VM rule,
//   - string values respect the character/byte length limit of their VR.
//
// Elements whose VR depends on context that cannot be determined (e.g. a
// "US or SS" tag seen with a "UN" VR) are reported as undetermined items.
//
// The validation is purely structural: it does not verify conformance to any
// DICOM IOD or any clinical/diagnostic requirements, and it never modifies
// the Dataset.
func ValidateDataset(d *Dataset, opts ...ValidateOption) *Report {
	cfg := validateConfig{maxIssues: defaultMaxIssues}
	for _, opt := range opts {
		opt(&cfg)
	}
	if cfg.maxIssues <= 0 {
		cfg.maxIssues = defaultMaxIssues
	}
	v := &validator{cfg: cfg}
	if d != nil {
		v.validateElements(d.Elements, "")
	}
	return &Report{Issues: v.issues, Truncated: v.truncated}
}

// validator accumulates issues up to a configured maximum.
type validator struct {
	cfg       validateConfig
	issues    []Issue
	truncated bool
}

// add records an issue unless the report is already full, in which case it
// marks the report as truncated.
func (v *validator) add(kind IssueKind, path string, t tag.Tag, msg string) {
	if len(v.issues) >= v.cfg.maxIssues {
		v.truncated = true
		return
	}
	v.issues = append(v.issues, Issue{Kind: kind, Path: path, Tag: t, Message: msg})
}

func (v *validator) validateElements(elems []*Element, parentPath string) {
	for _, elem := range elems {
		if elem == nil {
			continue
		}
		path := elem.Tag.String()
		if parentPath != "" {
			path = parentPath + ">" + path
		}
		v.validateElement(elem, path)
		if elem.Value != nil && elem.Value.ValueType() == Sequences {
			for i, item := range elem.Value.GetValue().([]*SequenceItemValue) {
				if item == nil {
					continue
				}
				itemPath := fmt.Sprintf("%s[%d]", path, i)
				v.validateElements(item.elements, itemPath)
			}
		}
	}
}

func (v *validator) validateElement(elem *Element, path string) {
	t := elem.Tag
	rawVR := elem.RawValueRepresentation

	info, err := tag.Find(t)
	if err != nil {
		v.add(IssueUnknownTag, path, t,
			fmt.Sprintf("tag not found in data dictionary (private=%t)", tag.IsPrivate(t.Group)))
		// Without a dictionary entry the expected VR and VM are unknown;
		// still check what the element's own VR tells us below.
	}

	// Resolve the effective VR used for type and length checks.
	effectiveVR := rawVR
	if err == nil {
		if containsString(info.VRs, rawVR) {
			effectiveVR = rawVR
		} else if len(info.VRs) == 1 && info.VRs[0] != "NA" {
			v.add(IssueVRMismatch, path, t,
				fmt.Sprintf("VR %q does not match dictionary VR %q", rawVR, info.VRs[0]))
		} else {
			// The dictionary allows several VRs and the element's VR is not
			// one of them (or is unknown): the correct VR depends on context
			// we do not have, so this element is undetermined.
			v.add(IssueUndetermined, path, t,
				fmt.Sprintf("VR %q does not match any dictionary VR %v; context-dependent VR cannot be resolved",
					rawVR, info.VRs))
			effectiveVR = ""
		}
	}

	if effectiveVR != "" {
		v.checkValueType(elem, path, effectiveVR)
		v.checkStringLengths(elem, path, effectiveVR)
	}

	if err == nil {
		v.checkVM(elem, path, info.VM)
	}
}

// checkValueType verifies that the Go value stored by the element matches the
// kind of value implied by its VR.
func (v *validator) checkValueType(elem *Element, path, vr string) {
	if elem.Value == nil {
		return
	}
	kind := tag.GetVRKind(elem.Tag, vr)
	if kind == tag.VRUnknown {
		v.add(IssueUndetermined, path, elem.Tag,
			fmt.Sprintf("VR %q is unknown; value type cannot be verified", vr))
		return
	}
	want, ok := expectedValueType(kind)
	if !ok {
		return
	}
	if got := elem.Value.ValueType(); got != want {
		v.add(IssueValueTypeMismatch, path, elem.Tag,
			fmt.Sprintf("VR %q expects value type %v, but element stores %v", vr, want, got))
	}
}

// expectedValueType maps a VRKind to the ValueType an element value must
// have. Returns ok=false for kinds that carry no structural expectation.
func expectedValueType(kind tag.VRKind) (ValueType, bool) {
	switch kind {
	case tag.VRStringList, tag.VRString, tag.VRDate:
		return Strings, true
	case tag.VRBytes:
		return Bytes, true
	case tag.VRUInt16List, tag.VRUInt32List, tag.VRInt16List, tag.VRInt32List, tag.VRTagList:
		return Ints, true
	case tag.VRFloat32List, tag.VRFloat64List:
		return Floats, true
	case tag.VRSequence:
		return Sequences, true
	case tag.VRPixelData:
		return PixelData, true
	case tag.VRItem:
		return SequenceItem, true
	default:
		return 0, false
	}
}

// checkVM verifies the number of values against the dictionary VM rule.
func (v *validator) checkVM(elem *Element, path, vm string) {
	if elem.Value == nil {
		return
	}
	rule, err := parseVMRule(vm)
	if err != nil {
		v.add(IssueUndetermined, path, elem.Tag,
			fmt.Sprintf("unable to parse dictionary VM %q: %s", vm, err))
		return
	}
	count := valueCount(elem.Value)
	if !rule.allows(count) {
		v.add(IssueVMViolation, path, elem.Tag,
			fmt.Sprintf("value count %d violates VM %q", count, vm))
	}
}

// valueCount returns the number of values held by a Value. Binary values
// (byte streams, pixel data) count as a single value regardless of their
// underlying byte length, and a sequence counts as a single value regardless
// of its item count.
func valueCount(val Value) int {
	switch val.ValueType() {
	case Strings:
		return len(val.GetValue().([]string))
	case Ints:
		return len(val.GetValue().([]int))
	case Floats:
		return len(val.GetValue().([]float64))
	case Bytes:
		if len(val.GetValue().([]byte)) == 0 {
			return 0
		}
		return 1
	case Sequences:
		if len(val.GetValue().([]*SequenceItemValue)) == 0 {
			return 0
		}
		return 1
	case SequenceItem, PixelData:
		return 1
	default:
		return 1
	}
}

// vmRule is a parsed DICOM value multiplicity rule. It supports the fixed
// ("3"), range ("1-2"), open-ended ("1-n"), and multiple ("2-2n") forms.
type vmRule struct {
	min  int
	max  int // -1 means unbounded
	step int // required spacing between allowed counts; 1 for "n" forms
}

// parseVMRule parses VM expressions of the form "N", "A-B", "A-n", and
// "A-An" (e.g. "1", "1-2", "1-n", "2-2n").
func parseVMRule(vm string) (vmRule, error) {
	vm = strings.TrimSpace(vm)
	if vm == "" {
		return vmRule{}, fmt.Errorf("empty VM expression")
	}
	parts := strings.SplitN(vm, "-", 2)
	min, err := strconv.Atoi(parts[0])
	if err != nil || min < 0 {
		return vmRule{}, fmt.Errorf("invalid VM expression %q", vm)
	}
	if len(parts) == 1 {
		return vmRule{min: min, max: min, step: 1}, nil
	}
	upper := parts[1]
	if strings.HasSuffix(upper, "n") {
		step := 1
		if prefix := strings.TrimSuffix(upper, "n"); prefix != "" {
			step, err = strconv.Atoi(prefix)
			if err != nil || step <= 0 {
				return vmRule{}, fmt.Errorf("invalid VM expression %q", vm)
			}
		}
		return vmRule{min: min, max: -1, step: step}, nil
	}
	max, err := strconv.Atoi(upper)
	if err != nil || max < min {
		return vmRule{}, fmt.Errorf("invalid VM expression %q", vm)
	}
	return vmRule{min: min, max: max, step: 1}, nil
}

// allows reports whether the given value count satisfies the rule. A count of
// zero (an empty value) is always allowed: presence requirements are a matter
// of IOD validation, which this structural check does not cover.
func (r vmRule) allows(count int) bool {
	if count == 0 {
		return true
	}
	if count < r.min {
		return false
	}
	if r.max >= 0 {
		return count <= r.max
	}
	return (count-r.min)%r.step == 0
}

// vrStringLimit describes the length limit of a string VR. Limits are either
// character-based (for VRs that may use multi-byte character repertoires) or
// byte-based.
type vrStringLimit struct {
	max      int
	inChars  bool
	perGroup bool // limit applies per PN component group (separated by '=')
}

// vrStringLimits holds the maximum lengths of string VRs as defined in DICOM
// PS3.5 Table 6.2-1. VRs absent from this table (UC, UR, UT) are unlimited.
var vrStringLimits = map[string]vrStringLimit{
	"AE": {max: 16},
	"AS": {max: 4},
	"CS": {max: 16},
	"DA": {max: 8},
	"DS": {max: 16},
	"DT": {max: 26},
	"IS": {max: 12},
	"TM": {max: 14},
	"UI": {max: 64},
	"LO": {max: 64, inChars: true},
	"LT": {max: 10240, inChars: true},
	"PN": {max: 64, inChars: true, perGroup: true},
	"SH": {max: 16, inChars: true},
	"ST": {max: 1024, inChars: true},
}

// checkStringLengths verifies that string values respect the length limit of
// the VR. Character-based VRs are measured in characters (runes); byte-based
// VRs are measured in bytes.
func (v *validator) checkStringLengths(elem *Element, path, vr string) {
	if elem.Value == nil || elem.Value.ValueType() != Strings {
		return
	}
	limit, ok := vrStringLimits[vr]
	if !ok {
		return
	}
	for idx, s := range elem.Value.GetValue().([]string) {
		if limit.perGroup {
			for _, group := range strings.Split(s, "=") {
				v.checkOneString(elem, path, vr, limit, idx, group)
			}
			continue
		}
		v.checkOneString(elem, path, vr, limit, idx, s)
	}
}

func (v *validator) checkOneString(elem *Element, path, vr string, limit vrStringLimit, idx int, s string) {
	n := len(s)
	unit := "bytes"
	if limit.inChars {
		n = utf8.RuneCountInString(s)
		unit = "characters"
	}
	if n > limit.max {
		v.add(IssueStringTooLong, path, elem.Tag,
			fmt.Sprintf("value %d has %d %s, exceeding the %d %s limit of VR %s",
				idx, n, unit, limit.max, unit, vr))
	}
}

func containsString(list []string, s string) bool {
	for _, item := range list {
		if item == s {
			return true
		}
	}
	return false
}
