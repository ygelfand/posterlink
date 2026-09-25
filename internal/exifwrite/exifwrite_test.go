package exifwrite

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func testMeta() Meta {
	return Meta{
		Title:       "Blade Runner 2049",
		Description: "tmdb / discover/movie?with_crew=137427",
		Make:        "posterlink",
		Model:       "tmdb",
		Software:    "posterlink dev",
		Source:      "https://image.tmdb.org/t/p/original/abc123.jpg",
		UniqueID:    "0123456789abcdef",
		Keywords:    []string{"posterlink", "tmdb"},
		Taken:       time.Date(2026, 9, 21, 10, 30, 0, 0, time.UTC),
	}
}

func testImage() image.Image {
	img := image.NewRGBA(image.Rect(0, 0, 60, 90))
	for y := range 90 {
		for x := range 60 {
			img.Set(x, y, color.RGBA{uint8(x * 4), uint8(y * 2), 90, 255})
		}
	}
	return img
}

func dump(t *testing.T, name string, data []byte) {
	dir := os.Getenv("EXIF_DUMP")
	if dir == "" {
		return
	}
	if err := os.WriteFile(filepath.Join(dir, name), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestApplyJPEG(t *testing.T) {
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, testImage(), nil); err != nil {
		t.Fatal(err)
	}
	orig := buf.Bytes()

	out, ok, err := Apply(orig, testMeta())
	if err != nil || !ok {
		t.Fatalf("Apply: ok=%v err=%v", ok, err)
	}
	dump(t, "out.jpg", out)

	if _, err := jpeg.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("output is not decodable JPEG: %v", err)
	}

	exif, xmp := jpegSegments(t, out)
	if len(exif) != 1 {
		t.Fatalf("got %d Exif APP1 segments, want 1", len(exif))
	}
	if len(xmp) != 1 {
		t.Fatalf("got %d XMP APP1 segments, want 1", len(xmp))
	}
	for _, want := range []string{"posterlink", "tmdb", "2026:09:21 10:30:00", "abc123.jpg"} {
		if !bytes.Contains(exif[0], []byte(want)) {
			t.Errorf("Exif segment missing %q", want)
		}
	}
	if !strings.Contains(string(xmp[0]), "2026-09-21T10:30:00Z") {
		t.Error("XMP segment missing CreateDate")
	}

	again, ok, err := Apply(out, testMeta())
	if err != nil || !ok {
		t.Fatalf("second Apply: ok=%v err=%v", ok, err)
	}
	if len(again) != len(out) {
		t.Errorf("second Apply changed size: %d -> %d", len(out), len(again))
	}
	exif2, xmp2 := jpegSegments(t, again)
	if len(exif2) != 1 || len(xmp2) != 1 {
		t.Errorf("after re-apply: %d Exif, %d XMP segments; want 1 and 1", len(exif2), len(xmp2))
	}
}

func jpegSegments(t *testing.T, data []byte) (exif, xmp [][]byte) {
	t.Helper()
	if !isJPEG(data) {
		t.Fatal("not a JPEG")
	}
	for i := 2; i+4 <= len(data); {
		if data[i] != 0xFF {
			break
		}
		marker := data[i+1]
		if marker == 0xDA || marker == 0xD9 {
			break
		}
		size := int(binary.BigEndian.Uint16(data[i+2:]))
		body := data[i+4 : i+2+size]
		if marker == 0xE1 {
			switch {
			case bytes.HasPrefix(body, exifPrefix):
				exif = append(exif, body[len(exifPrefix):])
			case bytes.HasPrefix(body, xmpPrefix):
				xmp = append(xmp, body[len(xmpPrefix):])
			}
		}
		i += 2 + size
	}
	return exif, xmp
}

func TestApplyPNG(t *testing.T) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, testImage()); err != nil {
		t.Fatal(err)
	}

	out, ok, err := Apply(buf.Bytes(), testMeta())
	if err != nil || !ok {
		t.Fatalf("Apply: ok=%v err=%v", ok, err)
	}
	dump(t, "out.png", out)

	if _, err := png.Decode(bytes.NewReader(out)); err != nil {
		t.Fatalf("output is not decodable PNG: %v", err)
	}
	if n := bytes.Count(out, []byte("eXIf")); n != 1 {
		t.Errorf("got %d eXIf chunks, want 1", n)
	}
	if n := bytes.Count(out, []byte(xmpKeyword)); n != 1 {
		t.Errorf("got %d XMP iTXt chunks, want 1", n)
	}

	again, _, err := Apply(out, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	if len(again) != len(out) {
		t.Errorf("second Apply changed size: %d -> %d", len(out), len(again))
	}
}

func TestApplyUnsupportedFormat(t *testing.T) {
	in := []byte("RIFF....WEBPVP8 ")
	out, ok, err := Apply(in, testMeta())
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Error("Apply reported success for a format it cannot write")
	}
	if !bytes.Equal(in, out) {
		t.Error("Apply modified unsupported input")
	}
}

func TestExifBlobLayout(t *testing.T) {
	blob := exifBlob(testMeta())
	if string(blob[:2]) != "MM" {
		t.Fatalf("bad byte order marker %q", blob[:2])
	}
	if got := binary.BigEndian.Uint16(blob[2:]); got != 42 {
		t.Fatalf("bad TIFF magic %d", got)
	}
	ifd0 := int(binary.BigEndian.Uint32(blob[4:]))
	if ifd0 != 8 {
		t.Fatalf("IFD0 offset = %d, want 8", ifd0)
	}

	sub := walkIFD(t, blob, ifd0)
	if sub == 0 {
		t.Fatal("no Exif sub-IFD pointer in IFD0")
	}
	walkIFD(t, blob, sub)
}

func walkIFD(t *testing.T, blob []byte, off int) int {
	t.Helper()
	if off+2 > len(blob) {
		t.Fatalf("IFD at %d is past the end of the blob (%d bytes)", off, len(blob))
	}
	n := int(binary.BigEndian.Uint16(blob[off:]))
	sizes := map[uint16]int{1: 1, 2: 1, 3: 2, 4: 4, 7: 1}
	subIFD := 0
	last := uint16(0)
	for i := range n {
		e := blob[off+2+12*i:]
		tag := binary.BigEndian.Uint16(e)
		typ := binary.BigEndian.Uint16(e[2:])
		count := int(binary.BigEndian.Uint32(e[4:]))
		if tag < last {
			t.Errorf("entry %d: tag %#x out of order (after %#x)", i, tag, last)
		}
		last = tag
		unit, ok := sizes[typ]
		if !ok {
			t.Fatalf("entry %#x: unexpected type %d", tag, typ)
		}
		if n := count * unit; n > 4 {
			at := int(binary.BigEndian.Uint32(e[8:]))
			if at%2 != 0 {
				t.Errorf("entry %#x: value offset %d is not even", tag, at)
			}
			if at+n > len(blob) {
				t.Errorf("entry %#x: value [%d,%d) runs past the blob (%d bytes)", tag, at, at+n, len(blob))
			}
		}
		if tag == tagExifIFD {
			subIFD = int(binary.BigEndian.Uint32(e[8:]))
		}
	}
	if next := binary.BigEndian.Uint32(blob[off+2+12*n:]); next != 0 {
		t.Errorf("IFD at %d chains to %d, want 0", off, next)
	}
	return subIFD
}

func TestExifStringsAreLatin1(t *testing.T) {
	m := testMeta()
	m.Description = "Amélie — Jean-Pierre Jeunet (2001)"
	blob := exifBlob(m)

	if bytes.Contains(blob, []byte("Amélie")) {
		t.Error("UTF-8 leaked into an EXIF string")
	}
	if !bytes.Contains(blob, []byte("Am\xe9lie")) {
		t.Error("é was not encoded as Latin-1 0xE9")
	}
	if !bytes.Contains(blob, []byte("Jean-Pierre Jeunet (2001)")) {
		t.Error("ASCII tail did not survive")
	}

	if got := string(latin1("千と千尋")); got != "" {
		t.Errorf("unrepresentable runes = %q, want them dropped", got)
	}
	if got := string(Sidecar(m)); !strings.Contains(got, "Amélie — Jean-Pierre Jeunet") {
		t.Error("XMP lost UTF-8")
	}
}

func TestSidecarEscapesAndOmits(t *testing.T) {
	m := testMeta()
	m.Description = `A & B <c> "d"`
	m.Copyright = ""
	got := string(Sidecar(m))
	if strings.Contains(got, "A & B <c>") {
		t.Error("description was not XML-escaped")
	}
	if !strings.Contains(got, "A &amp; B &lt;c&gt;") {
		t.Errorf("escaped description missing:\n%s", got)
	}
	if strings.Contains(got, "dc:rights") {
		t.Error("empty field emitted")
	}
	if !strings.Contains(got, "<rdf:li>tmdb</rdf:li>") {
		t.Error("keywords missing")
	}
}
