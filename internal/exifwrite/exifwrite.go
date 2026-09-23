// Package exifwrite embeds a small, fixed set of EXIF and XMP tags into image
// bytes, without shelling out to exiftool or taking on a dependency.
//
// Only what a photo library needs to file an image sensibly is written: a
// description, a capture date, a make/model pair (posterlink records the
// provider there, so the images are filterable by "camera"), the source URL as
// a user comment, and XMP keywords. Existing EXIF/XMP blocks are replaced.
//
// JPEG (APP1 segments) and PNG (an eXIf chunk plus an XMP iTXt chunk) are
// written in place. Any other format falls back to a Sidecar packet, which
// Immich, digiKam and Lightroom all read as "<image>.xmp".
package exifwrite

import (
	"bytes"
	"encoding/binary"
	"encoding/xml"
	"fmt"
	"hash/crc32"
	"slices"
	"strings"
	"time"
)

// Meta is the metadata written into an image. Empty fields are omitted.
type Meta struct {
	Title       string
	Description string
	Make        string
	Model       string
	Software    string
	Artist      string
	Copyright   string
	Source      string // source URL; stored as the EXIF UserComment
	UniqueID    string
	Keywords    []string
	Taken       time.Time // EXIF DateTimeOriginal / XMP CreateDate
}

// Apply embeds meta in data. The second return value reports whether the
// format supports embedding; when it is false data is returned unchanged and
// the caller should write a Sidecar next to the file instead.
func Apply(data []byte, m Meta) ([]byte, bool, error) {
	switch {
	case isJPEG(data):
		out, err := applyJPEG(data, m)
		return out, err == nil, err
	case isPNG(data):
		out, err := applyPNG(data, m)
		return out, err == nil, err
	default:
		return data, false, nil
	}
}

func isJPEG(d []byte) bool { return len(d) > 3 && d[0] == 0xFF && d[1] == 0xD8 && d[2] == 0xFF }

const pngSig = "\x89PNG\r\n\x1a\n"

func isPNG(d []byte) bool { return len(d) > 8 && string(d[:8]) == pngSig }

// ── EXIF (TIFF) encoding ────────────────────────────────────────────────────

// EXIF tag numbers used below; see the EXIF 2.32 specification.
const (
	tagImageDescription = 0x010E
	tagMake             = 0x010F
	tagModel            = 0x0110
	tagSoftware         = 0x0131
	tagDateTime         = 0x0132
	tagArtist           = 0x013B
	tagCopyright        = 0x8298
	tagExifIFD          = 0x8769
	tagExifVersion      = 0x9000
	tagDateTimeOriginal = 0x9003
	tagDateTimeDigitzd  = 0x9004
	tagUserComment      = 0x9286
	tagImageUniqueID    = 0xA420
	tagXPTitle          = 0x9C9B // UCS-2LE, what Windows/Explorer shows as Title
)

const (
	typASCII = 2
	typLong  = 4
	typUndef = 7
)

var be = binary.BigEndian

// entry is one IFD field. val holds the already-encoded value bytes.
type entry struct {
	tag, typ uint16
	count    uint32
	val      []byte
}

func ascii(tag uint16, s string) []entry {
	if s == "" {
		return nil
	}
	b := append([]byte(s), 0)
	return []entry{{tag: tag, typ: typASCII, count: uint32(len(b)), val: b}}
}

func undef(tag uint16, b []byte) []entry {
	if len(b) == 0 {
		return nil
	}
	return []entry{{tag: tag, typ: typUndef, count: uint32(len(b)), val: b}}
}

func long(tag uint16, v uint32) entry {
	b := make([]byte, 4)
	be.PutUint32(b, v)
	return entry{tag: tag, typ: typLong, count: 1, val: b}
}

// ucs2 encodes s as the little-endian UCS-2 an XP* tag expects, NUL-terminated.
func ucs2(tag uint16, s string) []entry {
	if s == "" {
		return nil
	}
	var b []byte
	for _, r := range s {
		if r > 0xFFFF {
			r = '?'
		}
		b = append(b, byte(r), byte(r>>8))
	}
	b = append(b, 0, 0)
	// XP tags are BYTE (type 1) sequences in practice; UNDEFINED reads the same.
	return []entry{{tag: tag, typ: 1, count: uint32(len(b)), val: b}}
}

// exifBlob builds a complete little-endian-free (big-endian, "MM") TIFF block
// holding IFD0 and an Exif sub-IFD: the payload of a JPEG APP1 Exif segment or
// a PNG eXIf chunk.
func exifBlob(m Meta) []byte {
	taken := m.Taken
	if taken.IsZero() {
		taken = time.Now()
	}
	stamp := taken.Format("2006:01:02 15:04:05")

	var ifd0 []entry
	ifd0 = append(ifd0, ascii(tagImageDescription, m.Description)...)
	ifd0 = append(ifd0, ascii(tagMake, m.Make)...)
	ifd0 = append(ifd0, ascii(tagModel, m.Model)...)
	ifd0 = append(ifd0, ascii(tagSoftware, m.Software)...)
	ifd0 = append(ifd0, ascii(tagDateTime, stamp)...)
	ifd0 = append(ifd0, ascii(tagArtist, m.Artist)...)
	ifd0 = append(ifd0, ascii(tagCopyright, m.Copyright)...)
	ifd0 = append(ifd0, ucs2(tagXPTitle, m.Title)...)

	var sub []entry
	sub = append(sub, undef(tagExifVersion, []byte("0232"))...)
	sub = append(sub, ascii(tagDateTimeOriginal, stamp)...)
	sub = append(sub, ascii(tagDateTimeDigitzd, stamp)...)
	if m.Source != "" {
		sub = append(sub, undef(tagUserComment, append([]byte("ASCII\x00\x00\x00"), m.Source...))...)
	}
	sub = append(sub, ascii(tagImageUniqueID, m.UniqueID)...)

	// The Exif sub-IFD offset has to be known before IFD0 is serialized, so
	// lay the block out first: header, IFD0, IFD0 overflow, sub-IFD, overflow.
	ifd0 = append(ifd0, long(tagExifIFD, 0))
	subOff := 8 + ifdLen(len(ifd0)) + overflowLen(ifd0)
	ifd0[len(ifd0)-1] = long(tagExifIFD, uint32(subOff))

	var buf bytes.Buffer
	buf.WriteString("MM")
	_ = binary.Write(&buf, be, uint16(42))
	_ = binary.Write(&buf, be, uint32(8))
	writeIFD(&buf, ifd0, 8+ifdLen(len(ifd0)))
	writeIFD(&buf, sub, subOff+ifdLen(len(sub)))
	return buf.Bytes()
}

// ifdLen is the on-disk size of an IFD with n entries (count, entries, next).
func ifdLen(n int) int { return 2 + 12*n + 4 }

// overflowLen is the size of the value area for entries too big to inline.
func overflowLen(entries []entry) int {
	n := 0
	for _, e := range entries {
		if len(e.val) > 4 {
			n += len(e.val) + len(e.val)%2 // values start on even offsets
		}
	}
	return n
}

// writeIFD serializes entries (sorted by tag, as TIFF requires) followed by
// their overflow values, which live at absolute offset dataOff.
func writeIFD(buf *bytes.Buffer, entries []entry, dataOff int) {
	slices.SortFunc(entries, func(a, b entry) int { return int(a.tag) - int(b.tag) })

	_ = binary.Write(buf, be, uint16(len(entries)))
	var data []byte
	off := dataOff
	for _, e := range entries {
		_ = binary.Write(buf, be, e.tag)
		_ = binary.Write(buf, be, e.typ)
		_ = binary.Write(buf, be, e.count)
		if len(e.val) <= 4 {
			inline := make([]byte, 4)
			copy(inline, e.val)
			buf.Write(inline)
			continue
		}
		_ = binary.Write(buf, be, uint32(off))
		data = append(data, e.val...)
		off += len(e.val)
		if len(e.val)%2 == 1 {
			data = append(data, 0)
			off++
		}
	}
	_ = binary.Write(buf, be, uint32(0)) // no IFD1
	buf.Write(data)
}

// ── JPEG ────────────────────────────────────────────────────────────────────

var (
	exifPrefix = []byte("Exif\x00\x00")
	xmpPrefix  = []byte("http://ns.adobe.com/xap/1.0/\x00")
)

// applyJPEG rebuilds the JPEG with our Exif and XMP APP1 segments first,
// dropping any Exif/XMP APP1 segments that were already there.
func applyJPEG(data []byte, m Meta) ([]byte, error) {
	exifSeg, err := app1(exifPrefix, exifBlob(m))
	if err != nil {
		return nil, err
	}
	xmpSeg, err := app1(xmpPrefix, Sidecar(m))
	if err != nil {
		return nil, err
	}

	var out bytes.Buffer
	out.Grow(len(data) + len(exifSeg) + len(xmpSeg))
	out.Write(data[:2]) // SOI
	out.Write(exifSeg)
	out.Write(xmpSeg)

	i := 2
	for i+1 < len(data) {
		if data[i] != 0xFF {
			break // not a marker: copy the rest verbatim
		}
		marker := data[i+1]
		if marker == 0xFF { // fill byte
			i++
			continue
		}
		if marker == 0xDA || marker == 0xD9 { // SOS / EOI: copy the rest verbatim
			break
		}
		if marker == 0x01 || (marker >= 0xD0 && marker <= 0xD8) { // standalone
			out.Write(data[i : i+2])
			i += 2
			continue
		}
		if i+4 > len(data) {
			break
		}
		size := int(be.Uint16(data[i+2:]))
		if size < 2 || i+2+size > len(data) {
			break
		}
		seg := data[i : i+2+size]
		if marker != 0xE1 || (!isExifSeg(seg) && !isXMPSeg(seg)) {
			out.Write(seg)
		}
		i += 2 + size
	}
	out.Write(data[i:])
	return out.Bytes(), nil
}

// isExifSeg and isXMPSeg identify the APP1 segments we replace. seg starts at
// the marker, so the payload begins after the marker and length bytes.
func isExifSeg(seg []byte) bool { return bytes.HasPrefix(seg[4:], exifPrefix) }
func isXMPSeg(seg []byte) bool  { return bytes.HasPrefix(seg[4:], xmpPrefix) }

// app1 wraps payload in an APP1 segment introduced by prefix.
func app1(prefix, payload []byte) ([]byte, error) {
	size := 2 + len(prefix) + len(payload)
	if size > 0xFFFF {
		return nil, fmt.Errorf("exifwrite: APP1 segment too large (%d bytes)", size)
	}
	seg := make([]byte, 0, size+2)
	seg = append(seg, 0xFF, 0xE1, byte(size>>8), byte(size))
	seg = append(seg, prefix...)
	seg = append(seg, payload...)
	return seg, nil
}

// ── PNG ─────────────────────────────────────────────────────────────────────

const xmpKeyword = "XML:com.adobe.xmp"

// applyPNG inserts an eXIf chunk and an XMP iTXt chunk before IEND, dropping
// any that were already present.
func applyPNG(data []byte, m Meta) ([]byte, error) {
	var out bytes.Buffer
	out.Write(data[:8])

	inserted := false
	insert := func() {
		if inserted {
			return
		}
		out.Write(chunk("eXIf", exifBlob(m)))
		out.Write(chunk("iTXt", itxt(xmpKeyword, Sidecar(m))))
		inserted = true
	}

	for i := 8; i+8 <= len(data); {
		size := int(be.Uint32(data[i:]))
		if i+12+size > len(data) {
			return nil, fmt.Errorf("exifwrite: truncated PNG chunk at offset %d", i)
		}
		typ := string(data[i+4 : i+8])
		body := data[i : i+12+size]
		switch {
		case typ == "eXIf":
		case typ == "iTXt" && strings.HasPrefix(string(data[i+8:i+12+size]), xmpKeyword+"\x00"):
		case typ == "IEND":
			insert()
			out.Write(body)
		default:
			out.Write(body)
		}
		i += 12 + size
	}
	if !inserted {
		return nil, fmt.Errorf("exifwrite: PNG has no IEND chunk")
	}
	return out.Bytes(), nil
}

func chunk(typ string, body []byte) []byte {
	out := make([]byte, 0, len(body)+12)
	out = be.AppendUint32(out, uint32(len(body)))
	out = append(out, typ...)
	out = append(out, body...)
	return be.AppendUint32(out, crc32.ChecksumIEEE(out[4:]))
}

// itxt builds an uncompressed iTXt chunk body.
func itxt(keyword string, text []byte) []byte {
	out := make([]byte, 0, len(keyword)+len(text)+5)
	out = append(out, keyword...)
	out = append(out, 0) // keyword terminator
	out = append(out, 0) // not compressed
	out = append(out, 0) // compression method
	out = append(out, 0) // empty language tag
	out = append(out, 0) // empty translated keyword
	return append(out, text...)
}

// ── XMP ─────────────────────────────────────────────────────────────────────

// Sidecar returns a standalone XMP packet for meta. It is embedded in JPEG and
// PNG output and is what gets written as an "<image>.xmp" sidecar for formats
// that cannot carry metadata in-band.
func Sidecar(m Meta) []byte {
	taken := m.Taken
	if taken.IsZero() {
		taken = time.Now()
	}
	stamp := taken.Format(time.RFC3339)

	var b strings.Builder
	b.WriteString(`<?xpacket begin="" id="W5M0MpCehiHzreSzNTczkc9d"?>` + "\n")
	b.WriteString(`<x:xmpmeta xmlns:x="adobe:ns:meta/" x:xmptk="posterlink">` + "\n")
	b.WriteString(` <rdf:RDF xmlns:rdf="http://www.w3.org/1999/02/22-rdf-syntax-ns#">` + "\n")
	b.WriteString(`  <rdf:Description rdf:about=""` + "\n")
	b.WriteString(`    xmlns:dc="http://purl.org/dc/elements/1.1/"` + "\n")
	b.WriteString(`    xmlns:xmp="http://ns.adobe.com/xap/1.0/"` + "\n")
	b.WriteString(`    xmlns:tiff="http://ns.adobe.com/tiff/1.0/"` + "\n")
	b.WriteString(`    xmlns:photoshop="http://ns.adobe.com/photoshop/1.0/">` + "\n")
	altText(&b, "dc:title", m.Title)
	altText(&b, "dc:description", m.Description)
	if len(m.Keywords) > 0 {
		b.WriteString("   <dc:subject><rdf:Bag>")
		for _, k := range m.Keywords {
			if k == "" {
				continue
			}
			b.WriteString("<rdf:li>" + esc(k) + "</rdf:li>")
		}
		b.WriteString("</rdf:Bag></dc:subject>\n")
	}
	simple(&b, "dc:source", m.Source)
	simple(&b, "tiff:Make", m.Make)
	simple(&b, "tiff:Model", m.Model)
	simple(&b, "xmp:CreatorTool", m.Software)
	simple(&b, "xmp:CreateDate", stamp)
	simple(&b, "xmp:ModifyDate", stamp)
	simple(&b, "photoshop:DateCreated", stamp)
	b.WriteString(`  </rdf:Description>` + "\n")
	b.WriteString(` </rdf:RDF>` + "\n")
	b.WriteString(`</x:xmpmeta>` + "\n")
	b.WriteString(`<?xpacket end="w"?>`)
	return []byte(b.String())
}

func simple(b *strings.Builder, tag, val string) {
	if val == "" {
		return
	}
	b.WriteString("   <" + tag + ">" + esc(val) + "</" + tag + ">\n")
}

func altText(b *strings.Builder, tag, val string) {
	if val == "" {
		return
	}
	b.WriteString("   <" + tag + `><rdf:Alt><rdf:li xml:lang="x-default">` + esc(val) +
		"</rdf:li></rdf:Alt></" + tag + ">\n")
}

func esc(s string) string {
	var b strings.Builder
	_ = xml.EscapeText(&b, []byte(s))
	return b.String()
}
