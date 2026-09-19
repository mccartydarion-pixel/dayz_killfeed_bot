// Package logoimage validates uploaded faction logos from their actual bytes (never the
// filename or the declared Content-Type). Only PNG, JPEG and WebP are accepted; SVG, GIF,
// HTML, PDF and every other format are refused. Dimensions are read from the header and
// checked BEFORE any pixel decoding, so a decompression-bomb (a tiny file that claims a huge
// canvas) is rejected without allocating for it; PNG and JPEG are then fully decoded within
// the (small) accepted size to prove they are not corrupt. WebP has no decoder in the Go
// standard library, so it is validated structurally: RIFF container, exact chunk framing, a
// well-formed VP8 / VP8L / VP8X image header with in-range dimensions; animated WebP is refused.
//
// Nothing is re-encoded: the original safe image is stored as uploaded (image metadata such
// as EXIF is therefore preserved - see docs/FACTIONS.md). Future thumbnailing can add
// derived assets.
package logoimage

import (
	"bytes"
	"encoding/binary"
	"errors"
	"image/jpeg"
	"image/png"
)

// Limits.
const (
	// MaxBytes is the largest accepted image (also the upload body limit for the file part).
	MaxBytes = 5 << 20
	// MinDimension / MaxDimension bound each side in pixels.
	MinDimension = 128
	MaxDimension = 2048
)

// Accepted content types.
const (
	PNG  = "image/png"
	JPEG = "image/jpeg"
	WebP = "image/webp"
)

// Typed rejection reasons.
var (
	ErrEmpty       = errors.New("the file is empty")
	ErrTooLarge    = errors.New("the image is too large")
	ErrUnsupported = errors.New("unsupported image type: only PNG, JPEG and WebP are accepted")
	ErrCorrupt     = errors.New("the image is corrupt or not a valid image")
	ErrDimensions  = errors.New("the image dimensions are not allowed")
)

// Info describes an accepted image.
type Info struct {
	ContentType string
	Ext         string // file extension without the dot: png, jpg, webp
	Width       int
	Height      int
}

// ExtFor returns the extension for an accepted content type ("" if unknown).
func ExtFor(contentType string) string {
	switch contentType {
	case PNG:
		return "png"
	case JPEG:
		return "jpg"
	case WebP:
		return "webp"
	}
	return ""
}

// Inspect validates data and reports its real type and dimensions.
func Inspect(data []byte) (Info, error) {
	if len(data) == 0 {
		return Info{}, ErrEmpty
	}
	if len(data) > MaxBytes {
		return Info{}, ErrTooLarge
	}
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return inspectPNG(data)
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return inspectJPEG(data)
	case len(data) >= 12 && string(data[0:4]) == "RIFF" && string(data[8:12]) == "WEBP":
		return inspectWebP(data)
	}
	return Info{}, ErrUnsupported
}

func checkDimensions(w, h int) error {
	if w < MinDimension || h < MinDimension || w > MaxDimension || h > MaxDimension {
		return ErrDimensions
	}
	return nil
}

func inspectPNG(data []byte) (Info, error) {
	cfg, err := png.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Info{}, ErrCorrupt
	}
	if err := checkDimensions(cfg.Width, cfg.Height); err != nil {
		return Info{}, err
	}
	// Dimensions are bounded, so a full decode is bounded too; it proves the pixel data is intact.
	if _, err := png.Decode(bytes.NewReader(data)); err != nil {
		return Info{}, ErrCorrupt
	}
	return Info{ContentType: PNG, Ext: "png", Width: cfg.Width, Height: cfg.Height}, nil
}

func inspectJPEG(data []byte) (Info, error) {
	cfg, err := jpeg.DecodeConfig(bytes.NewReader(data))
	if err != nil {
		return Info{}, ErrCorrupt
	}
	if err := checkDimensions(cfg.Width, cfg.Height); err != nil {
		return Info{}, err
	}
	if _, err := jpeg.Decode(bytes.NewReader(data)); err != nil {
		return Info{}, ErrCorrupt
	}
	return Info{ContentType: JPEG, Ext: "jpg", Width: cfg.Width, Height: cfg.Height}, nil
}

// inspectWebP walks the RIFF container.
func inspectWebP(data []byte) (Info, error) {
	riffSize := int(binary.LittleEndian.Uint32(data[4:8]))
	// The RIFF size covers everything after the first 8 bytes; a trailing pad byte is tolerated.
	if riffSize+8 != len(data) && riffSize+9 != len(data) {
		return Info{}, ErrCorrupt
	}
	body := data[12 : riffSize+8]
	var w, h int
	haveImage, extended := false, false
	for pos := 0; pos < len(body); {
		if pos+8 > len(body) {
			return Info{}, ErrCorrupt
		}
		fourcc := string(body[pos : pos+4])
		size := int(binary.LittleEndian.Uint32(body[pos+4 : pos+8]))
		start, end := pos+8, pos+8+size
		if size < 0 || end > len(body) {
			return Info{}, ErrCorrupt
		}
		payload := body[start:end]
		switch fourcc {
		case "VP8X":
			if pos != 0 || size < 10 {
				return Info{}, ErrCorrupt
			}
			if payload[0]&0x02 != 0 { // animation flag
				return Info{}, ErrUnsupported
			}
			extended = true
			// Canvas width/height are stored as 24-bit little-endian "minus one".
			w = int(payload[4]) | int(payload[5])<<8 | int(payload[6])<<16
			h = int(payload[7]) | int(payload[8])<<8 | int(payload[9])<<16
			w, h = w+1, h+1
		case "ANIM", "ANMF":
			return Info{}, ErrUnsupported
		case "VP8 ":
			if haveImage {
				return Info{}, ErrCorrupt
			}
			// 3-byte frame tag (bit 0 = 0 for a key frame), start code 9D 01 2A, 14-bit sizes.
			if size < 10 || payload[0]&1 != 0 || payload[3] != 0x9D || payload[4] != 0x01 || payload[5] != 0x2A {
				return Info{}, ErrCorrupt
			}
			iw := int(binary.LittleEndian.Uint16(payload[6:8])) & 0x3FFF
			ih := int(binary.LittleEndian.Uint16(payload[8:10])) & 0x3FFF
			if !extended {
				w, h = iw, ih
			} else if iw != w || ih != h {
				return Info{}, ErrCorrupt
			}
			haveImage = true
		case "VP8L":
			if haveImage {
				return Info{}, ErrCorrupt
			}
			if size < 5 || payload[0] != 0x2F {
				return Info{}, ErrCorrupt
			}
			bits := binary.LittleEndian.Uint32(payload[1:5])
			iw, ih := int(bits&0x3FFF)+1, int((bits>>14)&0x3FFF)+1
			if !extended {
				w, h = iw, ih
			} else if iw != w || ih != h {
				return Info{}, ErrCorrupt
			}
			haveImage = true
		}
		pos = end + (size & 1) // chunks are padded to an even length
	}
	if !haveImage || w == 0 || h == 0 {
		return Info{}, ErrCorrupt
	}
	if err := checkDimensions(w, h); err != nil {
		return Info{}, err
	}
	return Info{ContentType: WebP, Ext: "webp", Width: w, Height: h}, nil
}
