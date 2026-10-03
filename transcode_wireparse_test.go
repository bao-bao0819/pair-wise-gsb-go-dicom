package dicom

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"io"
	"math"

	"github.com/suyashkumar/dicom/pkg/tag"
	"github.com/suyashkumar/dicom/pkg/uid"
)

// wireElem is a parsed DICOM element kept in byte-order independent form so
// files produced under different transfer syntaxes can be compared by value.
type wireElem struct {
	tag    tag.Tag
	vr     string
	raw    []byte
	ints   []int64
	uints  []uint64
	floats []float64
	items  [][]*wireElem
}

type wireFile struct {
	meta         []*wireElem
	body         []*wireElem
	metaGroupLen uint32
	transferUID  string
}

// testVRWidth is the native word width for numeric VRs in test files.
var testVRWidth = map[string]int{
	"US": 2, "SS": 2, "AT": 2, "UL": 4, "SL": 4, "FL": 4,
	"OF": 4, "OL": 4, "FD": 8, "OD": 8, "OV": 8, "SV": 8, "UV": 8,
}

type wireParser struct {
	buf      []byte
	pos      int
	bo       binary.ByteOrder
	implicit bool
	limits   []int // absolute end offsets
}

func (p *wireParser) end() int {
	if len(p.limits) > 0 {
		return p.limits[len(p.limits)-1]
	}
	return len(p.buf)
}

func (p *wireParser) atEnd() bool { return p.pos >= p.end() }

func (p *wireParser) need(n int) error {
	if p.pos+n > p.end() {
		return io.ErrUnexpectedEOF
	}
	return nil
}

func (p *wireParser) u16() (uint16, error) {
	if err := p.need(2); err != nil {
		return 0, err
	}
	v := p.bo.Uint16(p.buf[p.pos:])
	p.pos += 2
	return v, nil
}

func (p *wireParser) u32() (uint32, error) {
	if err := p.need(4); err != nil {
		return 0, err
	}
	v := p.bo.Uint32(p.buf[p.pos:])
	p.pos += 4
	return v, nil
}

func (p *wireParser) bytes(n int) ([]byte, error) {
	if err := p.need(n); err != nil {
		return nil, err
	}
	v := p.buf[p.pos : p.pos+n]
	p.pos += n
	return v, nil
}

func (p *wireParser) pushLimit(n int) { p.limits = append(p.limits, p.pos+n) }
func (p *wireParser) popLimit()       { p.limits = p.limits[:len(p.limits)-1] }

func (p *wireParser) readTag() (tag.Tag, error) {
	g, err := p.u16()
	if err != nil {
		return tag.Tag{}, err
	}
	e, err := p.u16()
	if err != nil {
		return tag.Tag{}, err
	}
	return tag.Tag{Group: g, Element: e}, nil
}

// readHeader parses an element/item header. Item control tags (group FFFE)
// always use implicit (32-bit) VL.
func (p *wireParser) readHeader() (tag.Tag, string, uint32, error) {
	t, err := p.readTag()
	if err != nil {
		return t, "", 0, err
	}

	if t == tag.Item || t == tag.ItemDelimitationItem || t == tag.SequenceDelimitationItem {
		vl, err := p.u32()
		return t, "NA", vl, err
	}

	if p.implicit {
		vl, err := p.u32()
		return t, implicitTestVR(t), vl, err
	}

	vrb, err := p.bytes(2)
	if err != nil {
		return t, "", 0, err
	}
	vr := string(vrb)
	var vl uint32
	if longFormVR[vr] {
		if _, err := p.bytes(2); err != nil {
			return t, vr, 0, err
		}
		vl, err = p.u32()
	} else {
		vl16, err := p.u16()
		if err != nil {
			return t, vr, 0, err
		}
		vl = uint32(vl16)
		if vl == 0xffff {
			vl = tag.VLUndefinedLength
		}
	}
	return t, vr, vl, err
}

// implicitTestVR mirrors the converter's implicit VR resolution for tags used
// in tests.
func implicitTestVR(t tag.Tag) string {
	if t == tag.PixelData || t == tag.OverlayData {
		return "OW"
	}
	info, err := tag.Find(t)
	if err != nil {
		return "UN"
	}
	return info.VRs[0]
}

// parseDataset parses ordinary elements until the current limit is reached.
// An ItemDelimitationItem or SequenceDelimitationItem terminates the parse and
// is reported via consumedDelim so the caller can distinguish the cases.
func (p *wireParser) parseDataset() (elems []*wireElem, delim tag.Tag, err error) {
	for !p.atEnd() {
		t, vr, vl, err := p.readHeader()
		if err != nil {
			return nil, tag.Tag{}, err
		}
		if t == tag.ItemDelimitationItem || t == tag.SequenceDelimitationItem {
			return elems, t, nil
		}

		e := &wireElem{tag: t, vr: vr}
		if vr == "SQ" {
			items, err := p.parseSequence(vl)
			if err != nil {
				return nil, tag.Tag{}, err
			}
			e.items = items
			elems = append(elems, e)
			continue
		}

		raw, err := p.bytes(int(vl))
		if err != nil {
			return nil, tag.Tag{}, err
		}
		// Native PixelData is a stream of samples; decode its 16-bit words so
		// converted pixel values can be compared even though the element is OW.
		if t == tag.PixelData {
			decodePixelData(e, raw, p.bo)
		} else {
			decodeValue(e, raw, p.bo)
		}
		elems = append(elems, e)
	}
	return elems, tag.Tag{}, nil
}

// decodePixelData decodes native PixelData as 16-bit samples (the only
// PixelData width used in these tests).
func decodePixelData(e *wireElem, raw []byte, bo binary.ByteOrder) {
	e.raw = raw
	for i := 0; i+2 <= len(raw); i += 2 {
		u := bo.Uint16(raw[i : i+2])
		e.uints = append(e.uints, uint64(u))
		e.ints = append(e.ints, int64(int16(u)))
	}
}

func (p *wireParser) parseSequence(sqVL uint32) ([][]*wireElem, error) {
	var items [][]*wireElem
	defined := sqVL != tag.VLUndefinedLength
	if defined {
		p.pushLimit(int(sqVL))
		defer p.popLimit()
	}
	for !p.atEnd() {
		t, _, itemVL, err := p.readHeader()
		if err != nil {
			return nil, err
		}
		if t != tag.Item {
			if !defined && t == tag.SequenceDelimitationItem {
				return items, nil
			}
			return nil, fmt.Errorf("expected item (fffe,e000), got %s", t)
		}

		if itemVL != tag.VLUndefinedLength {
			p.pushLimit(int(itemVL))
			sub, delim, err := p.parseDataset()
			p.popLimit()
			if err != nil {
				return nil, err
			}
			if delim != (tag.Tag{}) {
				return nil, fmt.Errorf("unexpected delimitation %s inside defined-length item", delim)
			}
			items = append(items, sub)
		} else {
			sub, delim, err := p.parseDataset()
			if err != nil {
				return nil, err
			}
			if delim != tag.ItemDelimitationItem {
				return nil, fmt.Errorf("undefined-length item missing item delimitation, got %v", delim)
			}
			items = append(items, sub)
		}
	}
	if defined {
		return items, nil
	}
	return nil, fmt.Errorf("undefined-length sequence missing sequence delimitation")
}

// decodeValue populates byte-order independent numeric fields from raw bytes.
func decodeValue(e *wireElem, raw []byte, bo binary.ByteOrder) {
	e.raw = raw
	width, numeric := testVRWidth[e.vr]
	if !numeric || len(raw)%width != 0 {
		return
	}
	for i := 0; i+width <= len(raw); i += width {
		switch width {
		case 2:
			u := bo.Uint16(raw[i : i+2])
			e.uints = append(e.uints, uint64(u))
			e.ints = append(e.ints, int64(int16(u)))
		case 4:
			u := bo.Uint32(raw[i : i+4])
			e.uints = append(e.uints, uint64(u))
			e.ints = append(e.ints, int64(int32(u)))
			e.floats = append(e.floats, float64(math.Float32frombits(u)))
		case 8:
			u := bo.Uint64(raw[i : i+8])
			e.uints = append(e.uints, u)
			e.ints = append(e.ints, int64(u))
			e.floats = append(e.floats, math.Float64frombits(u))
		}
	}
}

// parseWireFile enforces the preamble and group-0002 Explicit VR Little
// Endian metadata, then parses the body in its declared transfer syntax.
func parseWireFile(data []byte) (*wireFile, error) {
	if len(data) < 132 || string(data[128:132]) != magicWord {
		return nil, fmt.Errorf("missing DICM preamble")
	}

	mp := &wireParser{buf: data[132:], bo: binary.LittleEndian, implicit: false}
	t, vr, vl, err := mp.readHeader()
	if err != nil {
		return nil, fmt.Errorf("reading meta group length: %w", err)
	}
	if t != tag.FileMetaInformationGroupLength || vr != "UL" || vl != 4 {
		return nil, fmt.Errorf("first meta element is %s/%s/%d, want (0002,0000) UL 4", t, vr, vl)
	}
	metaLen := mp.bo.Uint32(mp.buf[mp.pos:])
	mp.pos += 4

	mp.pushLimit(int(metaLen))
	metaElems, delim, err := mp.parseDataset()
	mp.popLimit()
	if err != nil {
		return nil, fmt.Errorf("parsing meta elements: %w", err)
	}
	if delim != (tag.Tag{}) {
		return nil, fmt.Errorf("unexpected delimitation inside metadata: %s", delim)
	}

	wf := &wireFile{meta: metaElems, metaGroupLen: metaLen}
	for _, e := range metaElems {
		if e.tag == tag.TransferSyntaxUID {
			wf.transferUID = string(bytes.TrimRight(e.raw, "\x00 "))
		}
	}

	bodyTS, ok := map[string]tsDesc{
		uid.ImplicitVRLittleEndian: tsILE,
		uid.ExplicitVRLittleEndian: tsELE,
		uid.ExplicitVRBigEndian:    tsEBE,
	}[wf.transferUID]
	if !ok {
		return nil, fmt.Errorf("unknown transfer syntax uid %q", wf.transferUID)
	}

	bp := &wireParser{buf: mp.buf[mp.pos:], bo: bodyTS.bo, implicit: bodyTS.implicit}
	bodyElems, delim, err := bp.parseDataset()
	if err != nil {
		return nil, fmt.Errorf("parsing body: %w", err)
	}
	if delim != (tag.Tag{}) {
		return nil, fmt.Errorf("unexpected delimitation at body top level: %s", delim)
	}
	wf.body = bodyElems
	return wf, nil
}
