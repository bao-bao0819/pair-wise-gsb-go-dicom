package dicom

// This file implements a standalone, read-only structural validator for
// Datasets. It answers a narrow question for ingestion tooling that needs to
// gate writes: does the in-memory Dataset agree with the data dictionary on
//
//   - the value representation (VR) of each element,
//   - the Go value shape used to carry that VR,
//   - the value multiplicity (VM) declared by the dictionary, and
//   - the representation ranges and string lengths defined in PS3.5.
//
// The validator recurses into Sequence items and reports the full
// tag/item path of every finding.
//
// Scope limitations (on purpose):
//
//   - It validates file/dataset structure only. It does not validate any
//     Information Object Definition (IOD), Type 1/Type 2/Type 3 usage,
//     conditional modules, or medical/diagnostic compliance.
//   - It never modifies the Dataset. It does not repair, re-encode, pad,
//     split, or otherwise "fix" values.
//   - Elements whose validity depends on information the dictionary cannot
//     supply on its own (unknown/non-standard tags, private tags, ambiguous
//     context-dependent VRs such as OB/OW or US/SS without a concrete VR,
//     opaque UN contents, or unrecognized VM strings) are reported as
//     SeverityUndetermined rather than silently treated as valid.

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/suyashkumar/dicom/pkg/tag"
)

// Severity classifies a structural validation finding.
type Severity int

const (
	// SeverityError is a definite structural violation against the dictionary
	// or the representation rules in DICOM PS3.5.
	SeverityError Severity = iota
	// SeverityUndetermined means the element could not be proven structurally
	// valid from the information available. Examples are unknown or private
	// tags missing from the dictionary, ambiguous VRs that require context to
	// resolve (e.g. OB/OW or US/SS), opaque UN values, or VM strings the
	// validator does not understand. Such elements must not be counted as
	// validation successes.
	SeverityUndetermined
)

func (s Severity) String() string {
	switch s {
	case SeverityError:
		return "error"
	case SeverityUndetermined:
		return "undetermined"
	default:
		return "unknown"
	}
}

// Issue codes emitted by the structural validator. They are stable strings so
// callers can branch on them programmatically.
const (
	// IssueUnknownTag: the tag is absent from the data dictionary. Applies to
	// unknown non-standard tags and to unknown private tags; both are
	// undetermined and neither counts as validation success.
	IssueUnknownTag = "UnknownTag"
	// IssueContextDependentVR: the element's VR cannot be resolved without
	// contextual information (empty/UN raw VR, or a multi-VR dictionary entry
	// presented as UN). Undetermined.
	IssueContextDependentVR = "ContextDependentVR"
	// IssueUnrecognizedVR: the two-character VR is not a VR known to DICOM
	// PS3.5. This is a definite structural error.
	IssueUnrecognizedVR = "UnrecognizedVR"
	// IssueVRNotAllowed: the concrete VR carried by the element is not among
	// the VRs the dictionary permits for its tag.
	IssueVRNotAllowed = "VRNotAllowed"
	// IssueValueTypeMismatch: the in-memory Value type cannot represent the
	// declared VR (e.g. Strings for a US element, Ints for a CS element).
	IssueValueTypeMismatch = "ValueTypeMismatch"
	// IssueValueNil: the Element has no Value at all.
	IssueValueNil = "ValueNil"
	// IssueVMMismatch: the number of values violates the dictionary VM rule.
	IssueVMMismatch = "VMMismatch"
	// IssueValueOutOfRange: an integer value lies outside the PS3.5 range of
	// its VR (US, SS, UL, SL), or an AT component is not a 16-bit quantity.
	IssueValueOutOfRange = "ValueOutOfRange"
	// IssueStringTooLong: a single value exceeds the maximum length of its
	// character VR.
	IssueStringTooLong = "StringTooLong"
	// IssueBinaryLength: the byte stream of a binary VR is not an exact
	// multiple of the VR unit width (e.g. an odd-length OW stream).
	IssueBinaryLength = "BinaryLength"
	// IssueATPair: an AT value does not hold an even number of 16-bit words.
	IssueATPair = "ATPair"
	// IssueUnrecognizedVM: the dictionary VM string uses a form the validator
	// cannot parse, so the VM could not be checked. Undetermined.
	IssueUnrecognizedVM = "UnrecognizedVM"
)

// Issue is a single structural validation finding.
type Issue struct {
	// Severity distinguishes definite errors from undetermined elements.
	Severity Severity `json:"severity"`
	// Code is one of the Issue* constants.
	Code string `json:"code"`
	// Path is the full tag/item path to the element, e.g.
	// "(0008,1199)/item[1]/(0008,1150)". Item indexes are 1-based.
	Path string `json:"path"`
	// Tag is the offending element's tag.
	Tag tag.Tag `json:"tag"`
	// Keyword is the dictionary keyword when known, otherwise "".
	Keyword string `json:"keyword"`
	// VR is the raw VR carried by the element.
	VR string `json:"vr"`
	// Message is a human-readable description.
	Message string `json:"message"`
}

func (i Issue) String() string {
	return fmt.Sprintf("%s: %s (%s, VR=%s): %s", i.Severity, i.Path, i.Code, i.VR, i.Message)
}

// DefaultMaxIssues bounds the number of findings collected by Validate unless
// overridden with WithMaxIssues.
const DefaultMaxIssues = 100

// Report is the structural validation result for a Dataset.
type Report struct {
	// Issues holds findings in traversal order, up to the configured maximum.
	Issues []Issue `json:"issues"`
	// Truncated is true when one or more findings were dropped because the
	// maximum report size was reached.
	Truncated bool `json:"truncated"`
	// MaxIssues is the bound that applied to this report. A value <= 0 means
	// no bound was enforced.
	MaxIssues int `json:"maxIssues"`
}

// Errors returns only the definite structural violations.
func (r Report) Errors() []Issue {
	return r.filter(SeverityError)
}

// Undetermined returns the elements that could not be judged from the
// available evidence.
func (r Report) Undetermined() []Issue {
	return r.filter(SeverityUndetermined)
}

func (r Report) filter(sev Severity) []Issue {
	var out []Issue
	for _, is := range r.Issues {
		if is.Severity == sev {
			out = append(out, is)
		}
	}
	return out
}

// Valid reports whether the Dataset is structurally sound and every element
// was fully judged. It returns false if there are any errors OR any
// undetermined items: an element that cannot be proven valid is not valid.
func (r Report) Valid() bool {
	return len(r.Issues) == 0
}

// HasErrors reports whether at least one definite structural violation was
// found.
func (r Report) HasErrors() bool {
	for _, is := range r.Issues {
		if is.Severity == SeverityError {
			return true
		}
	}
	return false
}

// ValidateOption configures a structural validation run.
type ValidateOption func(*validateConfig)

type validateConfig struct {
	maxIssues int
}

// WithMaxIssues limits how many findings the report aggregates. When the bound
// is reached, traversal stops and Report.Truncated is set. A value <= 0
// removes the limit. Defaults to DefaultMaxIssues.
func WithMaxIssues(n int) ValidateOption {
	return func(c *validateConfig) {
		c.maxIssues = n
	}
}

// Validate runs the standalone structural validator over the Dataset. It is
// read-only: it never changes element values or reports success on elements it
// cannot judge. See the package-level and Severity documentation for scope.
func (d *Dataset) Validate(opts ...ValidateOption) Report {
	return ValidateDataset(d, opts...)
}

// ValidateDataset validates a Dataset structurally. A nil Dataset produces an
// empty report.
func ValidateDataset(d *Dataset, opts ...ValidateOption) Report {
	cfg := validateConfig{maxIssues: DefaultMaxIssues}
	for _, o := range opts {
		o(&cfg)
	}
	v := &validator{cfg: cfg}
	if d != nil {
		v.walk(d.Elements, "")
	}
	return Report{
		Issues:    v.issues,
		Truncated: v.truncated,
		MaxIssues: cfg.maxIssues,
	}
}

type validator struct {
	cfg       validateConfig
	issues    []Issue
	truncated bool
	stop      bool
}

// add records one finding. It enforces the maximum-report bound and flips the
// truncated flag, then halts further traversal once the cap is reached.
func (v *validator) add(is Issue) {
	if v.stop {
		return
	}
	if v.cfg.maxIssues > 0 && len(v.issues) >= v.cfg.maxIssues {
		v.truncated = true
		v.stop = true
		return
	}
	v.issues = append(v.issues, is)
}

// walk visits a slice of elements (a dataset or one sequence item) and
// recurses into sequence items. prefix is the path of the enclosing sequence
// element without a trailing slash ("" at dataset root).
func (v *validator) walk(elems []*Element, prefix string) {
	for _, elem := range elems {
		if v.stop {
			return
		}
		v.validateElement(elem, prefix)
	}
}

func tagPath(t tag.Tag, prefix string) string {
	if prefix == "" {
		return t.String()
	}
	return prefix + "/" + t.String()
}

func (v *validator) validateElement(elem *Element, prefix string) {
	if elem == nil {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueValueNil,
			Path:     prefix,
			Message:  "nil element in dataset",
		})
		return
	}

	path := tagPath(elem.Tag, prefix)

	// Encoding delimiters are structural artifacts consumed by the parser, not
	// dictionary data elements; they never reach a normal Dataset, but ignore
	// them defensively.
	switch elem.Tag {
	case tag.ItemDelimitationItem, tag.SequenceDelimitationItem:
		return
	}

	if elem.Value == nil {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueValueNil,
			Path:     path,
			Tag:      elem.Tag,
			VR:       elem.RawValueRepresentation,
			Message:  "element has no value",
		})
		// Without a value there is nothing else to check or recurse into.
		return
	}

	info, infoErr := tag.Find(elem.Tag)
	if infoErr != nil {
		// Unknown tag: standard-but-unregistered and private tags are both
		// unprovable, so they are undetermined rather than successes. We still
		// run the intrinsic VR representation checks when the VR is known, so
		// a definite violation is not hidden behind an unknown tag.
		v.add(Issue{
			Severity: SeverityUndetermined,
			Code:     IssueUnknownTag,
			Path:     path,
			Tag:      elem.Tag,
			VR:       elem.RawValueRepresentation,
			Message:  "tag is not defined in the data dictionary; validity cannot be established",
		})
		// Validate the intrinsic representation of the claimed VR (using an
		// empty VM, since an unknown tag has no dictionary VM to enforce), so a
		// concrete malformed value is still flagged.
		v.checkRepresentation(elem, path, "", elem.RawValueRepresentation, "", false, true)
		return
	}

	keyword := info.Keyword
	rawVR := elem.RawValueRepresentation

	switch {
	case rawVR == "":
		v.add(Issue{
			Severity: SeverityUndetermined,
			Code:     IssueContextDependentVR,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  "element carries no VR; the correct representation cannot be selected without external evidence",
		})
		v.checkRepresentation(elem, path, keyword, rawVR, info.VM, false, true)
		return
	case rawVR == tag.UnknownVR:
		v.add(Issue{
			Severity: SeverityUndetermined,
			Code:     IssueContextDependentVR,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  "element uses UN; contents are opaque and the dictionary cannot confirm the intended VR",
		})
		v.checkRepresentation(elem, path, keyword, rawVR, info.VM, false, true)
		return
	}

	allowed := false
	for _, candidate := range info.VRs {
		if candidate == rawVR {
			allowed = true
			break
		}
	}
	if !allowed {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueVRNotAllowed,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  fmt.Sprintf("VR %s is not permitted for %s; dictionary allows %s", rawVR, keyword, strings.Join(info.VRs, "/")),
		})
		// The claimed VR is invalid for this tag, so do not additionally hold
		// the value to that VR's representation rules; still recurse if the
		// value happens to be a sequence so nested findings are not hidden.
	}

	v.checkRepresentation(elem, path, keyword, rawVR, info.VM, true, allowed)
	if !allowed && elem.Value.ValueType() == Sequences {
		v.recurseSequence(elem, path)
	}
}

// checkRepresentation validates the in-memory value shape, VM, numeric ranges,
// string lengths and binary word widths for the element's claimed VR. It then
// recurses into any sequence items.
//
// knownTag is false for dictionary-missing tags, for which dictionary VM and
// tag-specific allowances are not enforced. enforceRepresentation controls
// whether the claimed VR's intrinsic representation rules are applied (false
// when a known tag carries a VR its dictionary entry forbids).
func (v *validator) checkRepresentation(elem *Element, path, keyword, rawVR, dictVM string, knownTag, enforceRepresentation bool) {
	vt := elem.Value.ValueType()

	// PixelData carries an opaque PixelDataInfo: its frame count and encoding
	// are governed by image attributes and the negotiated transfer syntax, not
	// by a simple VM. Only verify the value shape; do not infer VM from bytes.
	if elem.Tag == tag.PixelData {
		if vt != PixelData {
			v.add(Issue{
				Severity: SeverityError,
				Code:     IssueValueTypeMismatch,
				Path:     path,
				Tag:      elem.Tag,
				Keyword:  keyword,
				VR:       rawVR,
				Message:  fmt.Sprintf("PixelData element must hold a PixelData value, got %v", vt),
			})
		}
		return
	}

	spec, vrKnown := vrSpecs[rawVR]
	if rawVR == "" || rawVR == tag.UnknownVR {
		// Opaque/unknown VR: verify only the value shape (bytes or an inferred
		// sequence) and recurse if it is a sequence. Never derive a VM from the
		// byte length.
		v.checkOpaque(elem, path, keyword, rawVR)
		v.recurseSequence(elem, path)
		return
	}
	if !vrKnown {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueUnrecognizedVR,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  fmt.Sprintf("%q is not a VR defined in DICOM PS3.5", rawVR),
		})
		return
	}
	if !enforceRepresentation {
		return
	}

	switch spec.kind {
	case vrTextList:
		v.checkTextList(elem, path, keyword, rawVR, dictVM, knownTag, spec, vt)
	case vrSingleText:
		v.checkSingleText(elem, path, keyword, rawVR, dictVM, knownTag, spec, vt)
	case vrBinary:
		v.checkBinary(elem, path, keyword, rawVR, vt, spec)
	case vrNumeric:
		v.checkNumeric(elem, path, keyword, rawVR, dictVM, knownTag, spec, vt)
	case vrFloat:
		v.checkFloat(elem, path, keyword, rawVR, dictVM, knownTag, vt)
	case vrAttribute:
		v.checkAttributeTag(elem, path, keyword, rawVR, dictVM, knownTag, vt)
	case vrSequence:
		if vt != Sequences {
			v.add(Issue{
				Severity: SeverityError,
				Code:     IssueValueTypeMismatch,
				Path:     path,
				Tag:      elem.Tag,
				Keyword:  keyword,
				VR:       rawVR,
				Message:  fmt.Sprintf("SQ element must hold a sequence value, got %v", vt),
			})
			return
		}
		// A sequence is a single value of VM 1 regardless of how many items it
		// contains; VM 1 was guaranteed by reaching here (checked against the
		// dictionary VM string below for completeness).
		if knownTag {
			v.checkVM(dictVM, 1, elem, path, keyword, rawVR)
		}
		v.recurseSequence(elem, path)
	}
}

func (v *validator) checkOpaque(elem *Element, path, keyword, rawVR string) {
	switch elem.Value.ValueType() {
	case Bytes, Sequences:
		// Acceptable opaque representations: an octet stream or an
		// undefined-length UN that parsed as a sequence.
	default:
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueValueTypeMismatch,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  fmt.Sprintf("opaque VR %s must be carried as bytes or a sequence, got %v", rawVR, elem.Value.ValueType()),
		})
	}
}

func (v *validator) recurseSequence(elem *Element, path string) {
	if elem.Value.ValueType() != Sequences {
		return
	}
	items := elem.Value.GetValue().([]*SequenceItemValue)
	for idx, item := range items {
		if v.stop {
			return
		}
		itemPrefix := fmt.Sprintf("%s/item[%d]", path, idx+1)
		v.walk(item.elements, itemPrefix)
	}
}

// valueCount returns the cardinality of an in-memory value expressed in the
// DICOM VM sense. An empty value field (zero strings/ints/floats/bytes, or an
// empty sequence) counts as zero values; an empty sequence itself is still a
// single SQ value but has zero items (handled by the caller as count 1 for
// SQ). Crucially, binary byte streams are NOT converted to a VM by dividing
// their byte length: a binary VR is one octet-stream value and its internal
// word width is validated separately.
func valueCount(v Value) (count int, empty bool) {
	switch v.ValueType() {
	case Strings:
		s := v.GetValue().([]string)
		// The reader represents a genuinely empty value field as a single "".
		if len(s) == 1 && s[0] == "" {
			return 0, true
		}
		return len(s), len(s) == 0
	case Ints:
		s := v.GetValue().([]int)
		return len(s), len(s) == 0
	case Floats:
		s := v.GetValue().([]float64)
		return len(s), len(s) == 0
	case Bytes:
		s := v.GetValue().([]byte)
		// Binary VRs carry exactly one octet-stream value; VM must never be
		// inferred from len(s).
		if len(s) == 0 {
			return 0, true
		}
		return 1, false
	case Sequences:
		return 1, false
	case PixelData:
		return 1, false
	default:
		return 0, true
	}
}

// effectiveLength measures a character value after stripping insignificant
// trailing padding. Per PS3.5, character VRs are space padded and UI is NUL
// padded; leading characters stay significant. Lengths are counted as Unicode
// code points (runes) after decoding, matching the character-based limits in
// the PS3.5 VR table. UT/ST/LT limits in the standard are expressed in bytes
// for legacy character sets, but the rune count is the decoded character count
// and is what these VRs constrain semantically.
func effectiveLength(s string) int {
	return len([]rune(strings.TrimRight(s, " \x00")))
}

func (v *validator) checkTextList(elem *Element, path, keyword, rawVR, dictVM string, knownTag bool, spec vrSpec, vt ValueType) {
	if vt != Strings {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueValueTypeMismatch,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  fmt.Sprintf("%s element must hold a list of strings, got %v", rawVR, vt),
		})
		return
	}
	strs := elem.Value.GetValue().([]string)
	count, empty := valueCount(elem.Value)
	if empty {
		// An empty value field is structurally legal: presence requirements
		// (Type 1/2) are an IOD concern, which this validator does not assess.
		return
	}
	if knownTag {
		v.checkVM(dictVM, count, elem, path, keyword, rawVR)
	}
	if spec.maxChars > 0 {
		for idx, s := range strs {
			// PN encodes up to five ^-separated component groups within one
			// value; the per-value VR limit applies to each component.
			limit := spec.maxChars
			parts := []string{s}
			if rawVR == "PN" {
				parts = strings.Split(s, "^")
			}
			for _, part := range parts {

				if n := effectiveLength(part); n > limit {
					v.add(Issue{
						Severity: SeverityError,
						Code:     IssueStringTooLong,
						Path:     path,
						Tag:      elem.Tag,
						Keyword:  keyword,
						VR:       rawVR,
						Message: fmt.Sprintf(
							"value #%d is %d characters long, exceeding the %s maximum of %d characters (trailing padding stripped)",
							idx+1, n, rawVR, limit),
					})
					break
				}
			}
		}
	}
}

// checkSingleText handles VRs that contain exactly one text value and that may
// legitimately contain the backslash character (LT, ST, UT, UR). The library
// still stores these in a []string via the generic reader; we treat the slice
// as VM 1 when non-empty and measure the rejoined whole value.
func (v *validator) checkSingleText(elem *Element, path, keyword, rawVR, dictVM string, knownTag bool, spec vrSpec, vt ValueType) {
	if vt != Strings {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueValueTypeMismatch,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  fmt.Sprintf("%s element must hold a string, got %v", rawVR, vt),
		})
		return
	}
	strs := elem.Value.GetValue().([]string)
	if len(strs) == 1 && strs[0] == "" {
		return
	}
	if knownTag {
		v.checkVM(dictVM, 1, elem, path, keyword, rawVR)
	}
	if spec.maxChars > 0 {
		whole := strings.Join(strs, `\`)
		if n := effectiveLength(whole); n > spec.maxChars {
			v.add(Issue{
				Severity: SeverityError,
				Code:     IssueStringTooLong,
				Path:     path,
				Tag:      elem.Tag,
				Keyword:  keyword,
				VR:       rawVR,
				Message: fmt.Sprintf(
					"value is %d characters long, exceeding the %s maximum of %d characters (trailing padding stripped)",
					n, rawVR, spec.maxChars),
			})
		}
	}
}

func (v *validator) checkBinary(elem *Element, path, keyword, rawVR string, vt ValueType, spec vrSpec) {
	if vt != Bytes {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueValueTypeMismatch,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  fmt.Sprintf("%s element must hold a byte stream, got %v", rawVR, vt),
		})
		return
	}
	data := elem.Value.GetValue().([]byte)
	if len(data) == 0 {
		return
	}
	// The stream is one binary value. Its VM must not be computed from
	// len(data)/unit; instead only word-width alignment is enforced.
	if spec.unitBytes > 1 && len(data)%spec.unitBytes != 0 {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueBinaryLength,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message: fmt.Sprintf(
				"%s value is %d bytes, which is not an exact multiple of its %d-byte unit width",
				rawVR, len(data), spec.unitBytes),
		})
	}
}

func (v *validator) checkNumeric(elem *Element, path, keyword, rawVR, dictVM string, knownTag bool, spec vrSpec, vt ValueType) {
	if vt != Ints {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueValueTypeMismatch,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  fmt.Sprintf("%s element must hold a list of integers, got %v", rawVR, vt),
		})
		return
	}
	vals := elem.Value.GetValue().([]int)
	count, empty := valueCount(elem.Value)
	if empty {
		return
	}
	if knownTag {
		v.checkVM(dictVM, count, elem, path, keyword, rawVR)
	}
	if spec.hasRange {
		for idx, n := range vals {
			if int64(n) < spec.min || int64(n) > spec.max {
				v.add(Issue{
					Severity: SeverityError,
					Code:     IssueValueOutOfRange,
					Path:     path,
					Tag:      elem.Tag,
					Keyword:  keyword,
					VR:       rawVR,
					Message: fmt.Sprintf(
						"value #%d (%d) is outside the %s range [%d, %d]",
						idx+1, n, rawVR, spec.min, spec.max),
				})
			}
		}
	}
}

func (v *validator) checkFloat(elem *Element, path, keyword, rawVR, dictVM string, knownTag bool, vt ValueType) {
	if vt != Floats {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueValueTypeMismatch,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  fmt.Sprintf("%s element must hold a list of floats, got %v", rawVR, vt),
		})
		return
	}
	count, empty := valueCount(elem.Value)
	if empty {
		return
	}
	// FL/FD accept the full finite/infinite IEEE-754 range, so there is no
	// value-range bound to check; only multiplicity is structural.
	if knownTag {
		v.checkVM(dictVM, count, elem, path, keyword, rawVR)
	}
}

func (v *validator) checkAttributeTag(elem *Element, path, keyword, rawVR, dictVM string, knownTag bool, vt ValueType) {
	if vt != Ints {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueValueTypeMismatch,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  fmt.Sprintf("AT element must hold a list of 16-bit tag words, got %v", vt),
		})
		return
	}
	vals := elem.Value.GetValue().([]int)
	_, empty := valueCount(elem.Value)
	if empty {
		return
	}
	if len(vals)%2 != 0 {
		v.add(Issue{
			Severity: SeverityError,
			Code:     IssueATPair,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message: fmt.Sprintf(
				"AT value contains %d 16-bit words; each tag requires an even number of words",
				len(vals)),
		})
	}
	for idx, n := range vals {
		if n < 0 || n > 0xffff {
			v.add(Issue{
				Severity: SeverityError,
				Code:     IssueValueOutOfRange,
				Path:     path,
				Tag:      elem.Tag,
				Keyword:  keyword,
				VR:       rawVR,
				Message: fmt.Sprintf(
					"AT word #%d (%d) is outside the 16-bit unsigned range [0, 65535]", idx+1, n),
			})
		}
	}
	// One tag is two words. Only check VM when pairing is intact; otherwise the
	// pair error already describes the structural fault.
	if knownTag && len(vals)%2 == 0 {
		v.checkVM(dictVM, len(vals)/2, elem, path, keyword, rawVR)
	}
}

// checkVM compares the observed value count against a dictionary VM string.
// Supported forms cover the rules used across the standard:

//   - fixed number:            "1", "2", "3", ...
//   - bounded interval:        "1-2", "2-4", "1-99", ...
//   - open interval:           "1-n", "2-n", "6-n", ...
//   - multiples with offset:   "2-2n", "3-3n", ... (k, 2k, 3k, ...)
//   - alternatives:            "1-n or 1"
//
// A VM string in an unrecognized form yields an undetermined finding instead
// of a false pass.
func (v *validator) checkVM(dictVM string, count int, elem *Element, path, keyword, rawVR string) {
	rules, ok := parseVM(dictVM)
	if !ok {
		v.add(Issue{
			Severity: SeverityUndetermined,
			Code:     IssueUnrecognizedVM,
			Path:     path,
			Tag:      elem.Tag,
			Keyword:  keyword,
			VR:       rawVR,
			Message:  fmt.Sprintf("dictionary VM %q is in a form the validator does not understand; multiplicity not checked", dictVM),
		})
		return
	}
	for _, rule := range rules {
		if rule.matches(count) {
			return
		}
	}
	v.add(Issue{
		Severity: SeverityError,
		Code:     IssueVMMismatch,
		Path:     path,
		Tag:      elem.Tag,
		Keyword:  keyword,
		VR:       rawVR,
		Message:  fmt.Sprintf("value multiplicity is %d, but the dictionary declares VM %s", count, dictVM),
	})
}

type vmRule struct {
	kind   vmKind
	lo, hi int // bounded/fixed: inclusive range; hi == -1 means open
	mult   int // k-kn forms: count must be a positive multiple of k
}

type vmKind int

const (
	vmFixed vmKind = iota
	vmRange
	vmOpen
	vmMultiple
)

func (r vmRule) matches(count int) bool {
	switch r.kind {
	case vmFixed:
		return count == r.lo
	case vmRange:
		return count >= r.lo && count <= r.hi
	case vmOpen:
		return count >= r.lo
	case vmMultiple:
		return count >= r.mult && count%r.mult == 0
	}
	return false
}

// parseVM parses a dictionary VM expression into one or more alternative
// rules ("A or B" accepts either).
func parseVM(vm string) ([]vmRule, bool) {
	var rules []vmRule
	for _, alt := range strings.Split(vm, " or ") {
		alt = strings.TrimSpace(alt)
		rule, ok := parseVMSingle(alt)
		if !ok {
			return nil, false
		}
		rules = append(rules, rule)
	}
	if len(rules) == 0 {
		return nil, false
	}
	return rules, true
}

func parseVMSingle(s string) (vmRule, bool) {
	if s == "" {
		return vmRule{}, false
	}
	if strings.Contains(s, "-") {
		// Open interval "k-n" must be tested before the multiples form, since
		// both end in "n".
		if strings.HasSuffix(s, "-n") {
			lo, err := strconv.Atoi(strings.TrimSpace(strings.TrimSuffix(s, "-n")))
			if err != nil || lo <= 0 {
				return vmRule{}, false
			}
			return vmRule{kind: vmOpen, lo: lo}, true
		}

		// Multiples with an offset, e.g. "2-2n", "3-3n": a positive multiple
		// of k (k, 2k, 3k, ...).
		if strings.HasSuffix(s, "n") {
			parts := strings.Split(strings.TrimSuffix(s, "n"), "-")
			if len(parts) == 2 {
				lo, err1 := strconv.Atoi(strings.TrimSpace(parts[0]))
				mult, err2 := strconv.Atoi(strings.TrimSpace(parts[1]))
				if err1 == nil && err2 == nil && mult > 0 && lo == mult {
					return vmRule{kind: vmMultiple, lo: lo, mult: mult}, true
				}
			}
		}

		// Bounded interval "lo-hi".
		parts := strings.Split(s, "-")
		if len(parts) != 2 {
			return vmRule{}, false
		}
		lo, err := strconv.Atoi(strings.TrimSpace(parts[0]))
		if err != nil || lo <= 0 {
			return vmRule{}, false
		}
		hi, err := strconv.Atoi(strings.TrimSpace(parts[1]))
		if err != nil || hi < lo {
			return vmRule{}, false
		}
		return vmRule{kind: vmRange, lo: lo, hi: hi}, true
	}
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil || n <= 0 {
		return vmRule{}, false
	}
	return vmRule{kind: vmFixed, lo: n}, true
}

// vrKindInternal classifies how the validator treats a known VR's value.
type vrKindInternal int

const (
	vrTextList   vrKindInternal = iota // backslash-delimited string values
	vrSingleText                       // one text value (LT, ST, UT, UR)
	vrBinary                           // octet stream
	vrNumeric                          // list of binary integers (US, SS, UL, SL)
	vrFloat                            // list of binary floats (FL, FD)
	vrAttribute                        // AT: ordered pairs of 16-bit words
	vrSequence                         // SQ
)

// vrSpec captures the PS3.5 representation constraints needed for structural
// validation.
type vrSpec struct {
	kind      vrKindInternal
	maxChars  int   // per-value character limit for character VRs; 0 = unbounded
	unitBytes int   // word width for binary VRs
	hasRange  bool  // numeric VRs with a bounded integer domain
	min, max  int64 // inclusive numeric range
}

// vrSpecs covers every VR defined in DICOM PS3.5 Table 6.2-1, including
// retired VRs still encountered in files.
var vrSpecs = map[string]vrSpec{
	// Character VRs, multi-valued. Lengths are character counts per value
	// after stripping insignificant trailing padding (UI strips trailing NUL,
	// the others strip trailing spaces).
	"AE": {kind: vrTextList, maxChars: 16},
	"AS": {kind: vrTextList, maxChars: 4},
	"CS": {kind: vrTextList, maxChars: 16},
	"DA": {kind: vrTextList, maxChars: 8},
	"DS": {kind: vrTextList, maxChars: 16},
	"DT": {kind: vrTextList, maxChars: 26},
	"IS": {kind: vrTextList, maxChars: 12},
	"LO": {kind: vrTextList, maxChars: 64},
	"PN": {kind: vrTextList, maxChars: 64},
	"SH": {kind: vrTextList, maxChars: 16},
	"TM": {kind: vrTextList, maxChars: 14},
	"UI": {kind: vrTextList, maxChars: 64},
	"UC": {kind: vrTextList, maxChars: 0}, // unbounded characters

	// Character VRs, single-valued.
	"LT": {kind: vrSingleText, maxChars: 10240},
	"ST": {kind: vrSingleText, maxChars: 1024},
	"UR": {kind: vrSingleText, maxChars: 0},
	"UT": {kind: vrSingleText, maxChars: 0},

	// Binary VRs: one octet-stream value. Only word-width alignment is
	// checked; VM is never derived from the byte length.
	"OB": {kind: vrBinary, unitBytes: 1},
	"OD": {kind: vrBinary, unitBytes: 8},
	"OF": {kind: vrBinary, unitBytes: 4},
	"OL": {kind: vrBinary, unitBytes: 4},
	"OV": {kind: vrBinary, unitBytes: 8},
	"OW": {kind: vrBinary, unitBytes: 2},

	// Binary numeric VRs, including their PS3.5 value ranges.
	"US": {kind: vrNumeric, hasRange: true, min: 0, max: 1<<16 - 1},
	"SS": {kind: vrNumeric, hasRange: true, min: -(1 << 15), max: 1<<15 - 1},
	"UL": {kind: vrNumeric, hasRange: true, min: 0, max: 1<<32 - 1},
	"SL": {kind: vrNumeric, hasRange: true, min: -(1 << 31), max: 1<<31 - 1},
	"SV": {kind: vrNumeric, hasRange: true, min: -(1 << 63), max: 1<<63 - 1},
	"UV": {kind: vrNumeric, hasRange: true, min: 0, max: 1<<63 - 1},

	// Binary floating point VRs: full IEEE-754 range, so no domain bound.
	"FL": {kind: vrFloat},
	"FD": {kind: vrFloat},

	// Attribute tag.
	"AT": {kind: vrAttribute},

	// Sequence.
	"SQ": {kind: vrSequence},
}
