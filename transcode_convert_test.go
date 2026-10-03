package dicom

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"testing"

	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// compareWireElems compares two parsed element trees by tag, VR and decoded
// value. This checks element values and legal wire formats rather than exact
// byte equality between transfer syntaxes.
func compareWireElems(t *testing.T, path string, want, got []*wireElem) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: element count = %d, want %d", path, len(got), len(want))
	}
	for i := range want {
		w, g := want[i], got[i]
		p := fmt.Sprintf("%s/%s", path, w.tag)
		if w.tag != g.tag {
			t.Fatalf("%s: tag = %s, want %s", p, g.tag, w.tag)
		}
		if w.vr != g.vr {
			t.Fatalf("%s: VR = %s, want %s", p, g.vr, w.vr)
		}

		if len(w.items) != len(g.items) {
			t.Fatalf("%s: item count = %d, want %d", p, len(g.items), len(w.items))
		}
		for j := range w.items {
			compareWireElems(t, fmt.Sprintf("%s[%d]", p, j), w.items[j], g.items[j])
		}

		if w.vr == "SQ" {
			continue
		}
		if w.tag == tag.TransferSyntaxUID {
			// The whole point of conversion is that this value changes; its
			// correctness is checked separately via dstFile.transferUID.
			continue
		}
		if w.tag == tag.PixelData {
			// Pixel samples are decoded separately and compared via
			// assertPixels; raw bytes legitimately differ by byte order.
			continue
		}

		if width, numeric := testVRWidth[w.vr]; numeric {
			switch width {
			case 2, 4, 8:
				if width == 4 && w.vr == "FL" || width == 8 && (w.vr == "FD" || w.vr == "OD") {
					compareFloats(t, p, w.floats, g.floats)
				} else {
					compareUint64s(t, p, w.uints, g.uints)
				}
			}
			continue
		}

		if !bytes.Equal(w.raw, g.raw) {
			t.Fatalf("%s: raw value mismatch:\n want % x\n got  % x", p, w.raw, g.raw)
		}
	}
}

func compareUint64s(t *testing.T, p string, want, got []uint64) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: numeric value count = %d, want %d", p, len(got), len(want))
	}
	for i := range want {
		if want[i] != got[i] {
			t.Fatalf("%s: value[%d] = %d, want %d", p, i, got[i], want[i])
		}
	}
}

func compareFloats(t *testing.T, p string, want, got []float64) {
	t.Helper()
	if len(want) != len(got) {
		t.Fatalf("%s: float value count = %d, want %d", p, len(got), len(want))
	}
	for i := range want {
		if math.Float64bits(want[i]) != math.Float64bits(got[i]) {
			t.Fatalf("%s: float[%d] = %v, want %v", p, i, got[i], want[i])
		}
	}
}

// syntheticBody builds the dataset used across the round-trip tests: numeric
// VRs of every width, odd-length text padding (both NUL for UI and space for
// text VRs), a nested undefined-length sequence containing a defined-length
// nested sequence, and 16-bit signed native pixels.
func syntheticBody() []*synElem {
	pixels := []int16{-1, -2, -256, -32768, 255, 32767}

	nested := &synElem{
		tag:         tag.ReferencedImageSequence,
		vr:          "SQ",
		undefinedSQ: false,
		items: [][]*synElem{
			{
				{tag: tag.ReferencedSOPClassUID, vr: "UI", build: func(binary.ByteOrder) []byte {
					return uiBytes("1.2.840.10008.5.1.4.1.1.2") // 27 chars odd -> NUL
				}},
				{tag: tag.ReferencedSOPInstanceUID, vr: "UI", build: func(binary.ByteOrder) []byte {
					return uiBytes("1.2.3.7")
				}},
			},
		},
	}

	outer := &synElem{
		tag:          tag.ReferencedSeriesSequence,
		vr:           "SQ",
		undefinedSQ:  true,
		undefinedItm: true,
		items: [][]*synElem{
			{
				{tag: tag.SeriesInstanceUID, vr: "UI", build: func(binary.ByteOrder) []byte {
					return uiBytes("1.2.840.113619.2.55.3.604688119.1")
				}},
				{tag: tag.Modality, vr: "CS", build: func(binary.ByteOrder) []byte {
					return padSpace("MR")
				}},
				nested,
				{tag: tag.TagAngleSecondAxis, vr: "SS", build: ssValues(-12, 345)},
			},
			{
				{tag: tag.SeriesInstanceUID, vr: "UI", build: func(binary.ByteOrder) []byte {
					return uiBytes("1.2.840.113619.2.55.3.604688119.2")
				}},
			},
		},
	}

	return []*synElem{
		{tag: tag.PatientName, vr: "PN", build: func(binary.ByteOrder) []byte {
			return append([]byte("Patient^A"), 0) // 9 chars odd -> NUL padding to 10
		}},
		{tag: tag.PatientID, vr: "LO", build: func(binary.ByteOrder) []byte {
			return padSpace("P123") // 4 chars even
		}},
		{tag: tag.Modality, vr: "CS", build: func(binary.ByteOrder) []byte {
			return padSpace("C") // odd -> space padding
		}},
		{tag: tag.StudyInstanceUID, vr: "UI", build: func(binary.ByteOrder) []byte {
			return uiBytes("1.2.840.113619.2.55.3.604688119.999")
		}},
		{tag: tag.InstanceNumber, vr: "IS", build: func(binary.ByteOrder) []byte {
			return padSpace("7")
		}},
		{tag: tag.SliceVector, vr: "US", build: usValues(0x0102, 0x0304)},
		{tag: tag.SimpleFrameList, vr: "UL", build: ulValues(0x01020304, 0x05060708)},
		{tag: tag.DimensionIndexPointer, vr: "AT", build: atValues(tag.Tag{Group: 0x0008, Element: 0x1115})},
		{tag: tag.ExaminedBodyThickness, vr: "FL", build: flValues(1.5)},
		{tag: tag.TimeRange, vr: "FD", build: fdValues(-3.25)},
		{tag: tag.TagAngleSecondAxis, vr: "SS", build: ssValues(-1000, 1000)},
		outer,
		{tag: tag.Rows, vr: "US", build: usValues(2)},
		{tag: tag.Columns, vr: "US", build: usValues(3)},
		{tag: tag.SamplesPerPixel, vr: "US", build: usValues(1)},
		{tag: tag.PhotometricInterpretation, vr: "CS", build: func(binary.ByteOrder) []byte {
			return padSpace("MONOCHROME2")
		}},
		{tag: tag.BitsAllocated, vr: "US", build: usValues(16)},
		{tag: tag.BitsStored, vr: "US", build: usValues(16)},
		{tag: tag.HighBit, vr: "US", build: usValues(15)},
		{tag: tag.PixelRepresentation, vr: "US", build: usValues(1)},
		{tag: tag.PixelData, vr: "OW", build: ssValues(pixels...)},
	}
}

// TestConvertTransferSyntaxRoundTrip builds a small synthetic file under each
// source transfer syntax, converts it to each target transfer syntax, reparses
// the output, and compares element values and structure against the reparsed
// source. It also checks wire-format invariants independent of byte equality.
func TestConvertTransferSyntaxRoundTrip(t *testing.T) {
	for _, src := range allTS {
		src := src
		t.Run(src.uid, func(t *testing.T) {
			input := buildSyntheticFile(src, standardMeta(src.uid), syntheticBody())

			srcFile, err := parseWireFile(input)
			if err != nil {
				t.Fatalf("failed to parse synthetic source file: %v", err)
			}

			for _, dst := range allTS {
				dst := dst
				t.Run(dst.uid, func(t *testing.T) {
					out := &bytes.Buffer{}
					if err := ConvertTransferSyntax(bytes.NewReader(input), out, int64(len(input)), dst.uid); err != nil {
						t.Fatalf("conversion %s -> %s failed: %v", src.uid, dst.uid, err)
					}

					dstFile, err := parseWireFile(out.Bytes())
					if err != nil {
						t.Fatalf("output is not a legal DICOM wire format: %v", err)
					}

					if dstFile.transferUID != dst.uid {
						t.Fatalf("TransferSyntaxUID = %q, want %q", dstFile.transferUID, dst.uid)
					}

					// The declared meta group length must cover exactly all
					// group-0002 bytes that follow it.
					declared := binary.LittleEndian.Uint32(out.Bytes()[132+8 : 132+12])
					if declared != dstFile.metaGroupLen {
						t.Fatalf("FileMetaInformationGroupLength = %d, parsed meta length %d", declared, dstFile.metaGroupLen)
					}
					if 132+8+int(declared) > len(out.Bytes()) {
						t.Fatalf("declared meta length %d exceeds file size", declared)
					}

					compareWireElems(t, "meta", srcFile.meta, dstFile.meta)
					compareWireElems(t, "body", srcFile.body, dstFile.body)

					// 16-bit signed pixels must be preserved exactly.
					assertPixels(t, dstFile, []int16{-1, -2, -256, -32768, 255, 32767})

					// Verify the actual on-wire pixel byte order matches the
					// target transfer syntax (sample -32768 = 0x8000).
					assertPixelWireOrder(t, out.Bytes(), dst, dst.bo)
				})
			}
		})
	}
}

// assertPixelWireOrder locates the PixelData element on the wire and checks
// that its first 16-bit sample is encoded in the expected byte order.
func assertPixelWireOrder(t *testing.T, data []byte, d tsDesc, bo binary.ByteOrder) {
	t.Helper()
	tagBytes := make([]byte, 4)
	bo.PutUint16(tagBytes[0:2], tag.PixelData.Group)
	bo.PutUint16(tagBytes[2:4], tag.PixelData.Element)
	idx := bytes.Index(data, tagBytes)
	if idx < 0 {
		t.Fatalf("PixelData tag bytes not found in output")
	}
	var valueOff int
	if d.implicit {
		valueOff = idx + 8
	} else {
		valueOff = idx + 12 // tag(4) + VR(2) + reserved(2) + VL(4)
	}
	want := []byte{0xff, 0xff} // first pixel is -1
	if got := data[valueOff : valueOff+2]; !bytes.Equal(got, want) {
		t.Fatalf("first pixel wire bytes = % x, want % x (byte order invariant for -1)", got, want)
	}
	// The 4th sample is -32768: check its byte order explicitly.
	sample4 := data[valueOff+6 : valueOff+8]
	le := []byte{0x00, 0x80}
	be := []byte{0x80, 0x00}
	if bo == binary.LittleEndian && !bytes.Equal(sample4, le) {
		t.Fatalf("LE output sample(-32768) = % x, want % x", sample4, le)
	}
	if bo == binary.BigEndian && !bytes.Equal(sample4, be) {
		t.Fatalf("BE output sample(-32768) = % x, want % x", sample4, be)
	}
}

func assertPixels(t *testing.T, wf *wireFile, want []int16) {
	t.Helper()
	for _, e := range wf.body {
		if e.tag != tag.PixelData {
			continue
		}
		if len(e.ints) != len(want) {
			t.Fatalf("pixel sample count = %d, want %d", len(e.ints), len(want))
		}
		for i, w := range want {
			if int16(e.ints[i]) != w {
				t.Fatalf("pixel[%d] = %d, want %d", i, int16(e.ints[i]), w)
			}
		}
		return
	}
	t.Fatal("PixelData element not found in converted body")
}

// TestRejectEncapsulatedPixelData ensures undefined-length (encapsulated)
// PixelData is refused with an explicit error rather than copied.
func TestRejectEncapsulatedPixelData(t *testing.T) {
	encap := encapsulatedBody()
	for _, src := range allTS {
		input := buildSyntheticFile(src, standardMeta(src.uid), encap)
		out := &bytes.Buffer{}
		err := ConvertTransferSyntax(bytes.NewReader(input), out, int64(len(input)), uid.ExplicitVRLittleEndian)
		if err == nil {
			t.Fatalf("%s: expected error for encapsulated PixelData", src.uid)
		}
		if !errors.Is(err, ErrorEncapsulatedPixelDataRejected) {
			t.Fatalf("%s: error = %v, want ErrorEncapsulatedPixelDataRejected", src.uid, err)
		}
		var ce *ConversionError
		if !errors.As(err, &ce) {
			t.Fatalf("%s: error is not *ConversionError: %v", src.uid, err)
		}
		if ce.Offset <= 0 || ce.Path == "" {
			t.Fatalf("%s: conversion error missing path/offset: %+v", src.uid, ce)
		}
	}
}

func encapsulatedBody() []*synElem {
	// Construct an undefined-length PixelData with a basic offset table and one
	// frame item, using a dedicated custom element type via encode callback is
	// not supported by synElem, so hand-build via a raw builder element.
	return []*synElem{
		{tag: tag.Rows, vr: "US", build: usValues(2)},
		{tag: tag.Columns, vr: "US", build: usValues(2)},
		{tag: tag.SamplesPerPixel, vr: "US", build: usValues(1)},
		{tag: tag.PhotometricInterpretation, vr: "CS", build: func(binary.ByteOrder) []byte {
			return padSpace("MONOCHROME2")
		}},
		{tag: tag.BitsAllocated, vr: "US", build: usValues(16)},
		{tag: tag.PixelRepresentation, vr: "US", build: usValues(0)},
		{tag: tag.PixelData, vr: "OW", encapsulated: true},
	}
}
