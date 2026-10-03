package dicom

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strings"
	"testing"

	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

func pixelDescriptors() []*synElem {
	return []*synElem{
		{tag: tag.Rows, vr: "US", build: usValues(2)},
		{tag: tag.Columns, vr: "US", build: usValues(2)},
		{tag: tag.SamplesPerPixel, vr: "US", build: usValues(1)},
		{tag: tag.PhotometricInterpretation, vr: "CS", build: func(binary.ByteOrder) []byte {
			return padSpace("MONOCHROME2")
		}},
		{tag: tag.BitsAllocated, vr: "US", build: usValues(16)},
		{tag: tag.PixelRepresentation, vr: "US", build: usValues(1)},
	}
}

func convertExpectFailure(t *testing.T, name string, input []byte, target string, wantErr error, wantPathTag tag.Tag) {
	t.Helper()
	out := &bytes.Buffer{}
	err := ConvertTransferSyntax(bytes.NewReader(input), out, int64(len(input)), target)
	if err == nil {
		t.Fatalf("%s: expected conversion error, got nil", name)
	}
	if wantErr != nil && !errors.Is(err, wantErr) {
		t.Fatalf("%s: error = %v, does not wrap %v", name, err, wantErr)
	}
	var ce *ConversionError
	if !errors.As(err, &ce) {
		t.Fatalf("%s: error is not *ConversionError: %v", name, err)
	}
	if wantPathTag != (tag.Tag{}) && !strings.Contains(ce.Path, wantPathTag.String()) {
		t.Fatalf("%s: error path %q does not contain %s; full: %v", name, ce.Path, wantPathTag, err)
	}
}

// TestRejectPrivateImplicitUnknownVR verifies implicit VR files with unknown
// private elements are refused rather than treated as OB and copied.
func TestRejectPrivateImplicitUnknownVR(t *testing.T) {
	privateTag := tag.Tag{Group: 0x0011, Element: 0x0010}
	body := []*synElem{
		{tag: tag.Modality, vr: "CS", build: func(binary.ByteOrder) []byte { return padSpace("MR") }},
		{tag: privateTag, vr: "OB", build: rawBytesVal([]byte{1, 2, 3, 4})},
	}
	input := buildSyntheticFile(tsILE, standardMeta(uid.ImplicitVRLittleEndian), body)
	convertExpectFailure(t, "private-implicit", input, uid.ExplicitVRLittleEndian,
		ErrorUnknownVRRejected, privateTag)
}

// TestRejectExplicitUN verifies UN elements are rejected even in explicit VR.
func TestRejectExplicitUN(t *testing.T) {
	unTag := tag.Tag{Group: 0x0019, Element: 0x0100}
	body := []*synElem{
		{tag: tag.Modality, vr: "CS", build: func(binary.ByteOrder) []byte { return padSpace("MR") }},
		{tag: unTag, vr: "UN", build: rawBytesVal([]byte{0xa, 0xb, 0xc, 0xd})},
	}
	input := buildSyntheticFile(tsELE, standardMeta(uid.ExplicitVRLittleEndian), body)
	convertExpectFailure(t, "explicit-un", input, uid.ImplicitVRLittleEndian,
		ErrorUnknownVRRejected, unTag)
}

// TestRejectUnknownTransferSyntax verifies an unrecognized source TS is
// rejected. JPEG Baseline (1.2.840.10008.1.2.4.50) is not supported.
func TestRejectUnknownTransferSyntax(t *testing.T) {
	input := buildSyntheticFile(tsELE, standardMeta("1.2.840.10008.1.2.4.50"),
		[]*synElem{{tag: tag.Modality, vr: "CS", build: func(binary.ByteOrder) []byte {
			return padSpace("MR")
		}}})
	convertExpectFailure(t, "unknown-source-ts", input, uid.ExplicitVRLittleEndian,
		ErrorUnsupportedConversionTransferSyntax, tag.Tag{})
}

// TestRejectBadTargetTransferSyntax verifies an unsupported target TS fails
// before any input is consumed.
func TestRejectBadTargetTransferSyntax(t *testing.T) {
	input := buildSyntheticFile(tsELE, standardMeta(uid.ExplicitVRLittleEndian), nil)
	out := &bytes.Buffer{}
	err := ConvertTransferSyntax(bytes.NewReader(input), out, int64(len(input)), "1.2.840.10008.1.2.4.50")
	if !errors.Is(err, ErrorUnsupportedConversionTransferSyntax) {
		t.Fatalf("error = %v, want ErrorUnsupportedConversionTransferSyntax", err)
	}
}

// TestRejectTruncatedInput verifies a value whose bytes run past the end of the
// stream reports an error with the element path and offset.
func TestRejectTruncatedInput(t *testing.T) {
	body := []*synElem{
		{tag: tag.Modality, vr: "CS", build: func(binary.ByteOrder) []byte { return padSpace("MR") }},
		{tag: tag.SliceVector, vr: "US", build: usValues(1, 2)},
	}
	full := buildSyntheticFile(tsELE, standardMeta(uid.ExplicitVRLittleEndian), body)
	// Truncate within the last US value (drop its final 2 bytes).
	input := full[:len(full)-2]
	convertExpectFailure(t, "truncated", input, uid.ExplicitVRLittleEndian, nil, tag.SliceVector)
}

// TestRejectPixelDataLengthMismatch verifies a PixelData value whose length
// disagrees with rows/cols/samples/bits is rejected instead of being copied.
func TestRejectPixelDataLengthMismatch(t *testing.T) {
	descs := pixelDescriptors()
	// Declare 2x2x1x2 = 8 bytes, but provide only 4.
	descs = append(descs, &synElem{
		tag:   tag.PixelData,
		vr:    "OW",
		build: rawBytesVal([]byte{0, 1, 0, 2}),
	})
	input := buildSyntheticFile(tsELE, standardMeta(uid.ExplicitVRLittleEndian), descs)
	convertExpectFailure(t, "pixel-vl-mismatch", input, uid.ImplicitVRLittleEndian,
		ErrorMismatchPixelDataLength, tag.PixelData)
}

// TestRejectUnsupportedBitsAllocated verifies pixel data with a sample width
// that has no determined byte order is rejected explicitly.
func TestRejectUnsupportedBitsAllocated(t *testing.T) {
	descs := pixelDescriptors()
	// Replace BitsAllocated (16) with 12 and provide a 2x2x2 = 8 byte payload.
	for _, e := range descs {
		if e.tag == tag.BitsAllocated {
			e.build = usValues(12)
		}
	}
	descs = append(descs, &synElem{
		tag:   tag.PixelData,
		vr:    "OW",
		build: rawBytesVal([]byte{0, 1, 2, 3, 4, 5, 6, 7}),
	})
	input := buildSyntheticFile(tsELE, standardMeta(uid.ExplicitVRLittleEndian), descs)
	convertExpectFailure(t, "bitsallocated-12", input, uid.ImplicitVRLittleEndian,
		ErrorUnsupportedBitsAllocated, tag.PixelData)
}

// TestRejectMissingPreamble verifies a stream without the DICM magic word is
// rejected.
func TestRejectMissingPreamble(t *testing.T) {
	input := make([]byte, 200)
	out := &bytes.Buffer{}
	err := ConvertTransferSyntax(bytes.NewReader(input), out, int64(len(input)), uid.ExplicitVRLittleEndian)
	if !errors.Is(err, ErrorMagicWord) {
		t.Fatalf("error = %v, want ErrorMagicWord", err)
	}
}

// TestDefinedLengthSequence ensures defined-length sequences (with
// defined-length items) are correctly converted to the canonical
// undefined-length output and values preserved.
func TestDefinedLengthSequence(t *testing.T) {
	definedSQ := &synElem{
		tag:         tag.ReferencedSeriesSequence,
		vr:          "SQ",
		undefinedSQ: false,
		items: [][]*synElem{
			{
				{tag: tag.SeriesInstanceUID, vr: "UI", build: func(binary.ByteOrder) []byte {
					return uiBytes("1.2.3.4.5.6")
				}},
				{tag: tag.TagAngleSecondAxis, vr: "SS", build: ssValues(-7, 8)},
			},
		},
	}
	body := []*synElem{
		definedSQ,
		{tag: tag.SliceVector, vr: "US", build: usValues(42)},
	}

	for _, src := range allTS {
		input := buildSyntheticFile(src, standardMeta(src.uid), body)
		srcFile, err := parseWireFile(input)
		if err != nil {
			t.Fatalf("%s: parse source: %v", src.uid, err)
		}
		for _, dst := range allTS {
			out := &bytes.Buffer{}
			if err := ConvertTransferSyntax(bytes.NewReader(input), out, int64(len(input)), dst.uid); err != nil {
				t.Fatalf("%s -> %s: %v", src.uid, dst.uid, err)
			}
			dstFile, err := parseWireFile(out.Bytes())
			if err != nil {
				t.Fatalf("%s -> %s: output not legal wire format: %v", src.uid, dst.uid, err)
			}
			compareWireElems(t, "body", srcFile.body, dstFile.body)

			// The output sequence must be undefined-length: locate its SQ
			// header bytes and verify VL is FFFFFFFF.
			if !hasUndefinedSQHeader(out.Bytes(), dst, tag.ReferencedSeriesSequence) {
				t.Fatalf("%s -> %s: output SQ was not encoded undefined-length", src.uid, dst.uid)
			}
		}
	}
}

// hasUndefinedSQHeader scans the body for the sequence tag under dst's byte
// order and checks that its VL field is FFFFFFFF.
func hasUndefinedSQHeader(data []byte, dst tsDesc, t tag.Tag) bool {
	need := make([]byte, 8)
	dst.bo.PutUint16(need[0:2], t.Group)
	dst.bo.PutUint16(need[2:4], t.Element)
	// Body begins after preamble (128+4) and metadata; scan whole file for the
	// tag followed by an SQ marker (explicit: "SQ"+reserved+VL; implicit: VL).
	idx := bytes.Index(data, need[:4])
	for idx >= 0 {
		rest := data[idx+4:]
		if dst.implicit && len(rest) >= 4 && dst.bo.Uint32(rest[:4]) == tag.VLUndefinedLength {
			return true
		}
		if !dst.implicit && len(rest) >= 8 && string(rest[:2]) == "SQ" &&
			dst.bo.Uint32(rest[4:8]) == tag.VLUndefinedLength {
			return true
		}
		next := bytes.Index(data[idx+4:], need[:4])
		if next < 0 {
			return false
		}
		idx = idx + 4 + next
	}
	return false
}

// TestLibraryParsesConvertedOutput ensures little-endian outputs remain
// compatible with the library's canonical parser.
func TestLibraryParsesConvertedOutput(t *testing.T) {
	src := tsEBE
	input := buildSyntheticFile(src, standardMeta(src.uid), syntheticBody())
	out := &bytes.Buffer{}
	if err := ConvertTransferSyntax(bytes.NewReader(input), out, int64(len(input)), uid.ExplicitVRLittleEndian); err != nil {
		t.Fatalf("conversion failed: %v", err)
	}
	ds, err := ParseUntilEOF(bytes.NewReader(out.Bytes()), nil)
	if err != nil {
		t.Fatalf("library failed to parse converted LE output: %v", err)
	}
	if _, err := ds.FindElementByTag(tag.PixelData); err != nil {
		t.Fatalf("PixelData missing from parsed output: %v", err)
	}
	tsElem, err := ds.FindElementByTag(tag.TransferSyntaxUID)
	if err != nil {
		t.Fatalf("TransferSyntaxUID missing: %v", err)
	}
	if got := MustGetStrings(tsElem.Value)[0]; got != uid.ExplicitVRLittleEndian {
		t.Fatalf("parsed TransferSyntaxUID = %q", got)
	}
}

// TestPartialOutputOnErrorDoesNotClaimCompletion ensures an error leaves the
// output with only the bytes written before the failure.
func TestPartialOutputOnErrorDoesNotClaimCompletion(t *testing.T) {
	body := []*synElem{
		{tag: tag.Modality, vr: "CS", build: func(binary.ByteOrder) []byte { return padSpace("MR") }},
		{tag: tag.SliceVector, vr: "US", build: usValues(1, 2)},
	}
	full := buildSyntheticFile(tsELE, standardMeta(uid.ExplicitVRLittleEndian), body)
	input := full[:len(full)-2]
	out := &bytes.Buffer{}
	err := ConvertTransferSyntax(bytes.NewReader(input), out, int64(len(input)), uid.ExplicitVRLittleEndian)
	if err == nil {
		t.Fatal("expected an error")
	}
	// Preamble, metadata and the first body element must already be present,
	// but the output must be shorter than a complete file.
	if out.Len() >= len(full) {
		t.Fatalf("partial output length %d >= complete file length %d", out.Len(), len(full))
	}
	if out.Len() <= 132 {
		t.Fatalf("expected some body bytes before failure, got %d", out.Len())
	}
}

// TestOBPixelDataSwapsBySampleWidth verifies that PixelData encoded with VR=OB
// but 16-bit BitsAllocated is still byte-swapped per sample (per PS3.5
// Annex E), not copied as a byte stream.
func TestOBPixelDataSwapsBySampleWidth(t *testing.T) {
	descs := []*synElem{
		{tag: tag.Rows, vr: "US", build: usValues(1)},
		{tag: tag.Columns, vr: "US", build: usValues(2)},
		{tag: tag.SamplesPerPixel, vr: "US", build: usValues(1)},
		{tag: tag.PhotometricInterpretation, vr: "CS", build: func(binary.ByteOrder) []byte {
			return padSpace("MONOCHROME2")
		}},
		{tag: tag.BitsAllocated, vr: "US", build: usValues(16)},
		{tag: tag.BitsStored, vr: "US", build: usValues(16)},
		{tag: tag.HighBit, vr: "US", build: usValues(15)},
		{tag: tag.PixelRepresentation, vr: "US", build: usValues(0)},
		// VR=OB explicitly, even though the samples are 16-bit words.
		{tag: tag.PixelData, vr: "OB", build: ssValues(0x1122, 0x3344)},
	}

	for _, src := range []tsDesc{tsELE, tsEBE} {
		input := buildSyntheticFile(src, standardMeta(src.uid), descs)
		out := &bytes.Buffer{}
		if err := ConvertTransferSyntax(bytes.NewReader(input), out, int64(len(input)), uid.ExplicitVRBigEndian); err != nil {
			t.Fatalf("%s: conversion failed: %v", src.uid, err)
		}
		wf, err := parseWireFile(out.Bytes())
		if err != nil {
			t.Fatalf("%s: output illegal: %v", src.uid, err)
		}
		assertPixels(t, wf, []int16{0x1122, 0x3344})
	}
}
