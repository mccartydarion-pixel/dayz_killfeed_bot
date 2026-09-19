package logoimage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"strings"
	"testing"
)

func pngBytes(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), uint8(y), 128, 255})
		}
	}
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func jpegBytes(t testing.TB, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			img.Set(x, y, color.RGBA{uint8(x), 200, uint8(y), 255})
		}
	}
	var b bytes.Buffer
	if err := jpeg.Encode(&b, img, nil); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func chunk(fourcc string, payload []byte) []byte {
	out := []byte(fourcc)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(payload)))
	out = append(out, payload...)
	if len(payload)%2 == 1 {
		out = append(out, 0)
	}
	return out
}

func riff(chunks ...[]byte) []byte {
	body := []byte("WEBP")
	for _, c := range chunks {
		body = append(body, c...)
	}
	out := []byte("RIFF")
	out = binary.LittleEndian.AppendUint32(out, uint32(len(body)))
	return append(out, body...)
}

func vp8lChunk(w, h int) []byte {
	bits := uint32(w-1) | uint32(h-1)<<14
	p := []byte{0x2F}
	p = binary.LittleEndian.AppendUint32(p, bits)
	p = append(p, make([]byte, 8)...) // opaque image data; not decoded
	return chunk("VP8L", p)
}

func vp8Chunk(w, h int) []byte {
	p := []byte{0x10, 0x00, 0x00, 0x9D, 0x01, 0x2A}
	p = binary.LittleEndian.AppendUint16(p, uint16(w))
	p = binary.LittleEndian.AppendUint16(p, uint16(h))
	p = append(p, make([]byte, 10)...)
	return chunk("VP8 ", p)
}

func vp8xChunk(flags byte, w, h int) []byte {
	p := []byte{flags, 0, 0, 0}
	for _, v := range []int{w - 1, h - 1} {
		p = append(p, byte(v), byte(v>>8), byte(v>>16))
	}
	return chunk("VP8X", p)
}

func TestAcceptsRealImages(t *testing.T) {
	cases := []struct {
		name    string
		data    []byte
		want    Info
		wantErr error
	}{
		{"png", pngBytes(t, 256, 200), Info{PNG, "png", 256, 200}, nil},
		{"png at the minimum", pngBytes(t, MinDimension, MinDimension), Info{PNG, "png", 128, 128}, nil},
		{"png at the maximum", pngBytes(t, MaxDimension, 128), Info{PNG, "png", 2048, 128}, nil},
		{"jpeg", jpegBytes(t, 300, 300), Info{JPEG, "jpg", 300, 300}, nil},
		{"webp lossless", riff(vp8lChunk(512, 256)), Info{WebP, "webp", 512, 256}, nil},
		{"webp lossy", riff(vp8Chunk(400, 400)), Info{WebP, "webp", 400, 400}, nil},
		{"webp extended", riff(vp8xChunk(0, 640, 480), vp8lChunk(640, 480)), Info{WebP, "webp", 640, 480}, nil},
	}
	for _, c := range cases {
		got, err := Inspect(c.data)
		if !errors.Is(err, c.wantErr) || got != c.want {
			t.Errorf("%s: got %+v, %v; want %+v, %v", c.name, got, err, c.want, c.wantErr)
		}
	}
	if ExtFor(PNG) != "png" || ExtFor(JPEG) != "jpg" || ExtFor(WebP) != "webp" || ExtFor("image/svg+xml") != "" {
		t.Fatal("ExtFor")
	}
}

func TestRejectsUnsupportedAndDisguisedFiles(t *testing.T) {
	svg := []byte(`<?xml version="1.0"?><svg xmlns="http://www.w3.org/2000/svg" width="256" height="256"><script>alert(1)</script></svg>`)
	html := []byte("<!DOCTYPE html><html><body><script>alert(1)</script></body></html>")
	cases := map[string][]byte{
		"svg":                 svg,
		"svg with bom":        append([]byte("\xEF\xBB\xBF"), svg...),
		"html":                html,
		"html renamed .png":   html,
		"text renamed .jpg":   []byte(strings.Repeat("just some text pretending to be a picture\n", 50)),
		"gif":                 append([]byte("GIF89a"), make([]byte, 400)...),
		"pdf":                 append([]byte("%PDF-1.7\n"), make([]byte, 400)...),
		"zip":                 append([]byte("PK\x03\x04"), make([]byte, 400)...),
		"exe":                 append([]byte("MZ"), make([]byte, 400)...),
		"bmp":                 append([]byte("BM"), make([]byte, 400)...),
		"png inside a script": append([]byte("<script>"), pngBytes(t, 200, 200)...),
		"riff but not webp":   append([]byte("RIFF\x10\x00\x00\x00WAVEfmt "), make([]byte, 20)...),
	}
	for name, data := range cases {
		if _, err := Inspect(data); !errors.Is(err, ErrUnsupported) {
			t.Errorf("%s: want ErrUnsupported, got %v", name, err)
		}
	}
}

func TestRejectsEmptyAndOversized(t *testing.T) {
	if _, err := Inspect(nil); !errors.Is(err, ErrEmpty) {
		t.Fatalf("nil: %v", err)
	}
	if _, err := Inspect([]byte{}); !errors.Is(err, ErrEmpty) {
		t.Fatalf("empty: %v", err)
	}
	big := append(pngBytes(t, 200, 200), make([]byte, MaxBytes)...)
	if _, err := Inspect(big); !errors.Is(err, ErrTooLarge) {
		t.Fatalf("oversized: %v", err)
	}
	// Exactly at the limit is judged on its content, not its size (this is not a valid image).
	if _, err := Inspect(append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, MaxBytes-8)...)); errors.Is(err, ErrTooLarge) {
		t.Fatal("the size limit is inclusive")
	}
}

func TestRejectsBadDimensions(t *testing.T) {
	for name, data := range map[string][]byte{
		"png too small":   pngBytes(t, 64, 64),
		"png thin":        pngBytes(t, 4000, 100),
		"png too wide":    pngBytes(t, 2049, 256),
		"png too tall":    pngBytes(t, 256, 2049),
		"jpeg too small":  jpegBytes(t, 100, 100),
		"jpeg too large":  jpegBytes(t, 2100, 300),
		"webp too small":  riff(vp8lChunk(100, 100)),
		"webp too large":  riff(vp8lChunk(4096, 4096)),
		"webp x too wide": riff(vp8xChunk(0, 16384, 300), vp8lChunk(16384, 300)),
	} {
		if _, err := Inspect(data); !errors.Is(err, ErrDimensions) {
			t.Errorf("%s: want ErrDimensions, got %v", name, err)
		}
	}
}

// A decompression bomb: a tiny, valid-looking PNG whose header claims a huge canvas. It must be
// rejected from the header, before anything is allocated for the pixels.
func TestHeaderClaimingHugeCanvasIsRejectedBeforeDecoding(t *testing.T) {
	data := pngBytes(t, 200, 200)
	// IHDR is the first chunk: 8 signature + 4 length + 4 type, then width, height, ...
	binary.BigEndian.PutUint32(data[16:20], 60000)
	binary.BigEndian.PutUint32(data[20:24], 60000)
	// Fix the IHDR CRC so the header is otherwise well formed.
	crc := crc32.ChecksumIEEE(data[12:29])
	binary.BigEndian.PutUint32(data[29:33], crc)
	if _, err := Inspect(data); !errors.Is(err, ErrDimensions) {
		t.Fatalf("a 60000x60000 header must be ErrDimensions, got %v", err)
	}
	// Same for JPEG: patch the SOF0 dimensions.
	j := jpegBytes(t, 200, 200)
	i := bytes.Index(j, []byte{0xFF, 0xC0})
	if i < 0 {
		t.Fatal("no SOF0 marker")
	}
	binary.BigEndian.PutUint16(j[i+5:i+7], 60000)
	binary.BigEndian.PutUint16(j[i+7:i+9], 60000)
	if _, err := Inspect(j); !errors.Is(err, ErrDimensions) {
		t.Fatalf("a 60000x60000 JPEG header must be ErrDimensions, got %v", err)
	}
}

func TestRejectsCorruptImages(t *testing.T) {
	p := pngBytes(t, 256, 256)
	j := jpegBytes(t, 256, 256)
	flip := func(b []byte, at int) []byte {
		c := append([]byte(nil), b...)
		c[at] ^= 0xFF
		return c
	}
	cases := map[string][]byte{
		"png truncated":         p[:len(p)/2],
		"png header only":       p[:40],
		"png flipped IDAT byte": flip(p, len(p)/2),
		"png signature only":    []byte("\x89PNG\r\n\x1a\n"),
		"jpeg truncated":        j[:len(j)/2],
		"jpeg header only":      j[:30],
		"jpeg marker only":      {0xFF, 0xD8, 0xFF},
		"webp truncated":        riff(vp8lChunk(256, 256))[:20],
		"webp riff size lies":   append([]byte("RIFF\xFF\xFF\xFF\x7FWEBP"), vp8lChunk(256, 256)...),
		"webp no image chunk":   riff(chunk("EXIF", []byte("hello"))),
		"webp bad vp8 start":    riff(chunk("VP8 ", []byte{0x10, 0, 0, 1, 2, 3, 0, 1, 0, 1, 0, 0, 0, 0, 0, 0})),
		"webp bad vp8l magic":   riff(chunk("VP8L", []byte{0x00, 1, 2, 3, 4, 5, 6, 7})),
		"webp chunk overruns":   append(append([]byte("RIFF"), 0x1E, 0, 0, 0), []byte("WEBPVP8L\xFF\xFF\x00\x00xxxxxxxxxx")...),
	}
	for name, data := range cases {
		if _, err := Inspect(data); !errors.Is(err, ErrCorrupt) {
			t.Errorf("%s: want ErrCorrupt, got %v", name, err)
		}
	}
	// A mismatched VP8X canvas vs the image chunk is corrupt too.
	if _, err := Inspect(riff(vp8xChunk(0, 640, 480), vp8lChunk(300, 300))); !errors.Is(err, ErrCorrupt) {
		t.Fatalf("canvas mismatch: %v", err)
	}
}

func TestRejectsAnimatedWebP(t *testing.T) {
	anim := riff(vp8xChunk(0x02, 256, 256), chunk("ANIM", make([]byte, 6)), chunk("ANMF", make([]byte, 30)))
	if _, err := Inspect(anim); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("animated webp: %v", err)
	}
	if _, err := Inspect(riff(vp8xChunk(0x02, 256, 256), vp8lChunk(256, 256))); !errors.Is(err, ErrUnsupported) {
		t.Fatalf("animation flag: %v", err)
	}
}

func TestInspectNeverPanicsOnGarbage(t *testing.T) {
	seeds := [][]byte{pngBytes(t, 200, 200), jpegBytes(t, 200, 200), riff(vp8lChunk(200, 200))}
	for _, s := range seeds {
		for cut := 0; cut < len(s) && cut < 300; cut++ {
			_, _ = Inspect(s[:cut])
		}
		for i := 0; i < 200 && i < len(s); i++ {
			c := append([]byte(nil), s...)
			c[i] ^= 0x5A
			_, _ = Inspect(c)
		}
	}
}
