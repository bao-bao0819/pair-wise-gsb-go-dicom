package dicom

import (
	"bytes"
	"encoding/binary"
	"io"

	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// Test-only transfer syntax descriptors for building/parsing synthetic files.
type tsDesc struct {
	uid      string
	bo       binary.ByteOrder
	implicit bool
}

var (
	tsILE = tsDesc{uid.ImplicitVRLittleEndian, binary.LittleEndian, true}
	tsELE = tsDesc{uid.ExplicitVRLittleEndian, binary.LittleEndian, false}
	tsEBE = tsDesc{uid.ExplicitVRBigEndian, binary.BigEndian, false}
	allTS = []tsDesc{tsILE, tsELE, tsEBE}
)

// synElem is a synthetic DICOM element for test files.
type synElem struct {
	tag tag.Tag
	vr  string
	// build returns the primitive value encoded for the file's byte order.
	build func(bo binary.ByteOrder) []byte
	// items is set for SQ elements.
	items        [][]*synElem
	undefinedSQ  bool
	undefinedItm bool
	// encapsulated marks PixelData as undefined-length with a basic offset
	// table item and a single frame item.
	encapsulated bool
}

func uiBytes(v string) []byte {
	if len(v)%2 == 1 {
		return append([]byte(v), 0)
	}
	return []byte(v)
}

func padSpace(v string) []byte {
	b := []byte(v)
	if len(b)%2 == 1 {
		b = append(b, ' ')
	}
	return b
}

func usValues(vs ...uint16) func(binary.ByteOrder) []byte {
	return func(bo binary.ByteOrder) []byte {
		buf := &bytes.Buffer{}
		for _, v := range vs {
			_ = binary.Write(buf, bo, v)
		}
		return buf.Bytes()
	}
}

func ulValues(vs ...uint32) func(binary.ByteOrder) []byte {
	return func(bo binary.ByteOrder) []byte {
		buf := &bytes.Buffer{}
		for _, v := range vs {
			_ = binary.Write(buf, bo, v)
		}
		return buf.Bytes()
	}
}

func ssValues(vs ...int16) func(binary.ByteOrder) []byte {
	return func(bo binary.ByteOrder) []byte {
		buf := &bytes.Buffer{}
		for _, v := range vs {
			_ = binary.Write(buf, bo, v)
		}
		return buf.Bytes()
	}
}

func flValues(vs ...float32) func(binary.ByteOrder) []byte {
	return func(bo binary.ByteOrder) []byte {
		buf := &bytes.Buffer{}
		for _, v := range vs {
			_ = binary.Write(buf, bo, v)
		}
		return buf.Bytes()
	}
}

func fdValues(vs ...float64) func(binary.ByteOrder) []byte {
	return func(bo binary.ByteOrder) []byte {
		buf := &bytes.Buffer{}
		for _, v := range vs {
			_ = binary.Write(buf, bo, v)
		}
		return buf.Bytes()
	}
}

func atValues(vs ...tag.Tag) func(binary.ByteOrder) []byte {
	return func(bo binary.ByteOrder) []byte {
		buf := &bytes.Buffer{}
		for _, v := range vs {
			_ = binary.Write(buf, bo, v.Group)
			_ = binary.Write(buf, bo, v.Element)
		}
		return buf.Bytes()
	}
}

func rawBytesVal(v []byte) func(binary.ByteOrder) []byte {
	return func(binary.ByteOrder) []byte { return v }
}

// longFormVR mirrors the explicit-VR VRs that use a 32-bit value length.
var longFormVR = map[string]bool{
	"OB": true, "OW": true, "OD": true, "OF": true, "OL": true, "OV": true,
	"SQ": true, "UN": true, "SV": true, "UV": true, "UC": true, "UR": true, "UT": true,
}

// writeSynHeader writes a data element header in the given transfer syntax.
func writeSynHeader(w io.Writer, ts tsDesc, t tag.Tag, vr string, vl uint32, forceImplicit bool) {
	_ = binary.Write(w, ts.bo, t.Group)
	_ = binary.Write(w, ts.bo, t.Element)
	implicit := ts.implicit || forceImplicit
	if implicit {
		_ = binary.Write(w, ts.bo, vl)
		return
	}
	_, _ = w.Write([]byte(vr))
	if longFormVR[vr] {
		_, _ = w.Write([]byte{0, 0})
		_ = binary.Write(w, ts.bo, vl)
	} else if vl == tag.VLUndefinedLength {
		_ = binary.Write(w, ts.bo, uint16(0xffff))
	} else {
		_ = binary.Write(w, ts.bo, uint16(vl))
	}
}

// writeSynDataset writes a list of synthetic elements (recursively handling
// SQ items) to w.
func writeSynDataset(w io.Writer, ts tsDesc, elems []*synElem) {
	for _, e := range elems {
		writeSynElem(w, ts, e)
	}
}

func writeSynElem(w io.Writer, ts tsDesc, e *synElem) {
	if e.vr == "SQ" {
		writeSynSequence(w, ts, e)
		return
	}
	if e.encapsulated {
		writeEncapsulatedPixelData(w, ts)
		return
	}
	value := e.build(ts.bo)
	writeSynHeader(w, ts, e.tag, e.vr, uint32(len(value)), false)
	_, _ = w.Write(value)
}

// writeEncapsulatedPixelData writes an undefined-length PixelData element with
// an empty basic offset table item and one tiny frame item.
func writeEncapsulatedPixelData(w io.Writer, ts tsDesc) {
	writeSynHeader(w, ts, tag.PixelData, "OW", tag.VLUndefinedLength, false)
	// Empty basic offset table item.
	writeSynHeader(w, ts, tag.Item, "NA", 0, true)
	// One frame item containing a few bytes (content is irrelevant since the
	// converter must refuse before inspecting it).
	frame := []byte{0x01, 0x02, 0x03, 0x04}
	writeSynHeader(w, ts, tag.Item, "NA", uint32(len(frame)), true)
	_, _ = w.Write(frame)
	_ = binary.Write(w, ts.bo, tag.SequenceDelimitationItem.Group)
	_ = binary.Write(w, ts.bo, tag.SequenceDelimitationItem.Element)
	_ = binary.Write(w, ts.bo, uint32(0))
}

func writeSynSequence(w io.Writer, ts tsDesc, e *synElem) {
	if !e.undefinedSQ {
		itemBufs := make([][]byte, len(e.items))
		total := 0
		for i, item := range e.items {
			buf := &bytes.Buffer{}
			writeSynItem(buf, ts, item, e.undefinedItm)
			itemBufs[i] = buf.Bytes()
			total += len(itemBufs[i])
		}
		writeSynHeader(w, ts, e.tag, "SQ", uint32(total), false)
		for _, b := range itemBufs {
			_, _ = w.Write(b)
		}
		return
	}

	writeSynHeader(w, ts, e.tag, "SQ", tag.VLUndefinedLength, false)
	for _, item := range e.items {
		writeSynItem(w, ts, item, e.undefinedItm)
	}
	_ = binary.Write(w, ts.bo, tag.SequenceDelimitationItem.Group)
	_ = binary.Write(w, ts.bo, tag.SequenceDelimitationItem.Element)
	_ = binary.Write(w, ts.bo, uint32(0))
}

func writeSynItem(w io.Writer, ts tsDesc, item []*synElem, undefinedItem bool) {
	content := &bytes.Buffer{}
	writeSynDataset(content, ts, item)
	if undefinedItem {
		writeSynHeader(w, ts, tag.Item, "NA", tag.VLUndefinedLength, true)
		_, _ = w.Write(content.Bytes())
		_ = binary.Write(w, ts.bo, tag.ItemDelimitationItem.Group)
		_ = binary.Write(w, ts.bo, tag.ItemDelimitationItem.Element)
		_ = binary.Write(w, ts.bo, uint32(0))
		return
	}
	writeSynHeader(w, ts, tag.Item, "NA", uint32(content.Len()), true)
	_, _ = w.Write(content.Bytes())
}

// buildSyntheticFile creates a complete DICOM file stream with group-0002
// metadata (always explicit LE) and a body encoded in bodyTS.
func buildSyntheticFile(bodyTS tsDesc, meta []*synElem, body []*synElem) []byte {
	out := &bytes.Buffer{}
	_, _ = out.Write(make([]byte, 128))
	_, _ = out.Write([]byte(magicWord))

	metaBody := &bytes.Buffer{}
	writeSynDataset(metaBody, tsELE, meta)
	writeSynHeader(out, tsELE, tag.FileMetaInformationGroupLength, "UL", 4, false)
	var lenBytes [4]byte
	binary.LittleEndian.PutUint32(lenBytes[:], uint32(metaBody.Len()))
	_, _ = out.Write(lenBytes[:])
	_, _ = out.Write(metaBody.Bytes())

	writeSynDataset(out, bodyTS, body)
	return out.Bytes()
}

// standardMeta returns a minimal complete group-0002 metadata set carrying
// the given TransferSyntaxUID.
func standardMeta(tsUID string) []*synElem {
	return []*synElem{
		{tag: tag.FileMetaInformationVersion, vr: "OB", build: rawBytesVal([]byte{0x00, 0x01})},
		{tag: tag.MediaStorageSOPClassUID, vr: "UI", build: func(binary.ByteOrder) []byte {
			return uiBytes("1.2.840.10008.5.1.4.1.1.2")
		}},
		{tag: tag.MediaStorageSOPInstanceUID, vr: "UI", build: func(binary.ByteOrder) []byte {
			return uiBytes("1.2.3.4.5")
		}},
		{tag: tag.TransferSyntaxUID, vr: "UI", build: func(binary.ByteOrder) []byte {
			return uiBytes(tsUID)
		}},
		{tag: tag.ImplementationClassUID, vr: "UI", build: func(binary.ByteOrder) []byte {
			return uiBytes("1.2.826.0.1.3680043.9.7133.1")
		}},
	}
}
