// Package agentmedia validates the media of an agent's avatar pack (ADR-0050
// decision 6, issue 1619) and normalizes the pack itself.
//
// THE TYPE COMES FROM THE BYTES. The file name and the declared Content-Type
// are never read: Go's multipart writer labels every part
// application/octet-stream, a browser labels by extension, and either can be
// chosen by the uploader. Inspect reads the magic bytes, then DECODES the
// file with the reader of that format, so a file that only starts like a PNG
// is refused.
//
// What is accepted:
//
//   - Stills: PNG, JPEG, WebP (one frame) and SVG. SVG is accepted for the
//     `still` slot only, and only when internal/domain/svgcheck finds no
//     script, handler, external reference or DOCTYPE. An animated PNG is
//     refused (it is not one of the animation formats).
//   - Animations: animated WebP, MP4 (H.264) and WebM (VP9 or AV1). At most
//     MaxDurationMS long, at most MaxEdge pixels on the longer side, with no
//     audio track. An animated WebP must loop forever (loop count 0). A video
//     has no loop flag of its own: the client plays it with `loop`, and the
//     pack must name a poster for it (pack.go).
//   - Every file: at most MaxBytes.
//
// Every refusal is an *Error, which the HTTP layer answers as 400 with the
// code `avatar_media_invalid` and the reason.
//
// The MP4 and WebM readers are minimal ISO BMFF and EBML walkers written for
// these checks. They read the boxes and elements that state the duration,
// the frame size, the tracks and the codecs, and skip the rest. There is no
// ffmpeg in the image and no cgo.
package agentmedia

import (
	"bytes"
	"errors"
	"fmt"
	"image"
	"image/jpeg"
	"image/png"
	"io"
	"strings"

	"golang.org/x/image/webp"

	"github.com/elitea-ng/elitea-platform/services/elitea-main/internal/domain/svgcheck"
)

// The limits of one media file (ADR-0050 decision 6, "Formats").
const (
	MaxBytes      = 2 << 20
	MaxEdge       = 512
	MaxDurationMS = 4_000
	// maxFrames bounds the frames of one animated WebP, so a file of many
	// tiny frames cannot cost a decode per frame without end.
	maxFrames = 240
)

// Code is the refusal code of a file that fails validation.
const Code = "avatar_media_invalid"

// Error is a refused file. Reason names the rule; it never echoes the file.
type Error struct{ Reason string }

func (e *Error) Error() string { return e.Reason }

func refuse(format string, args ...any) error {
	return &Error{Reason: fmt.Sprintf(format, args...)}
}

// IsRefusal reports whether err is a validation refusal (not an I/O failure).
func IsRefusal(err error) bool {
	var refused *Error
	return errors.As(err, &refused)
}

// Format is the detected kind of a file.
type Format string

const (
	FormatPNG          Format = "png"
	FormatJPEG         Format = "jpeg"
	FormatWebP         Format = "webp"
	FormatAnimatedWebP Format = "webp_animated"
	FormatSVG          Format = "svg"
	FormatMP4          Format = "mp4"
	FormatWebM         Format = "webm"
)

// Media describes one validated file.
type Media struct {
	Format    Format
	MediaType string
	// Extension is the one the stored object takes, without the dot.
	Extension string
	// Width and Height are the pixel size; 0 for an SVG.
	Width  int
	Height int
	// DurationMS is the length of one loop; 0 for a still.
	DurationMS int
	Bytes      int
}

// Animated reports whether the file moves.
func (m Media) Animated() bool {
	return m.Format == FormatAnimatedWebP || m.Format == FormatMP4 || m.Format == FormatWebM
}

// Video reports whether the file is a video container (it needs a poster).
func (m Media) Video() bool { return m.Format == FormatMP4 || m.Format == FormatWebM }

// Inspect detects the file's type from its bytes and validates it.
func Inspect(data []byte) (Media, error) {
	if len(data) == 0 {
		return Media{}, refuse("the file is empty")
	}
	if len(data) > MaxBytes {
		return Media{}, refuse("the file is %d KiB; an avatar file may be at most %d KiB", (len(data)+1023)/1024, MaxBytes/1024)
	}
	var (
		media Media
		err   error
	)
	switch detect(data) {
	case FormatPNG:
		media, err = inspectPNG(data)
	case FormatJPEG:
		media, err = inspectRaster(data, FormatJPEG, "image/jpeg", "jpg", jpeg.DecodeConfig, jpeg.Decode)
	case FormatWebP:
		media, err = inspectWebP(data)
	case FormatSVG:
		media, err = inspectSVG(data)
	case FormatMP4:
		media, err = inspectMP4(data)
	case FormatWebM:
		media, err = inspectWebM(data)
	default:
		if bytes.HasPrefix(data, []byte("GIF8")) {
			return Media{}, refuse("GIF is not accepted; use an animated WebP")
		}
		return Media{}, refuse("the file is not a PNG, JPEG, WebP or SVG image, nor an MP4 or WebM video")
	}
	if err != nil {
		return Media{}, err
	}
	media.Bytes = len(data)
	return media, nil
}

// detect reads the magic bytes. It answers "" for an unknown file.
func detect(data []byte) Format {
	switch {
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")):
		return FormatPNG
	case bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}):
		return FormatJPEG
	case len(data) >= 12 && bytes.HasPrefix(data, []byte("RIFF")) && string(data[8:12]) == "WEBP":
		return FormatWebP
	case len(data) >= 12 && string(data[4:8]) == "ftyp":
		return FormatMP4
	case bytes.HasPrefix(data, []byte{0x1A, 0x45, 0xDF, 0xA3}):
		return FormatWebM
	case looksLikeXML(data):
		return FormatSVG
	}
	return ""
}

func looksLikeXML(data []byte) bool {
	text := bytes.TrimPrefix(data, []byte("\xEF\xBB\xBF"))
	text = bytes.TrimLeft(text, " \t\r\n")
	return bytes.HasPrefix(text, []byte("<"))
}

func checkEdge(width, height int) error {
	if width <= 0 || height <= 0 {
		return refuse("the file states no frame size")
	}
	if width > MaxEdge || height > MaxEdge {
		return refuse("the file is %dx%d px; an avatar file may be at most %d px on its longer side", width, height, MaxEdge)
	}
	return nil
}

func checkDuration(ms int) error {
	if ms <= 0 {
		return refuse("the animation states no duration")
	}
	if ms > MaxDurationMS {
		return refuse("the animation is %.1f s long; an avatar animation may be at most %d s", float64(ms)/1000, MaxDurationMS/1000)
	}
	return nil
}

// inspectRaster checks the header's size first (so a decompression bomb is
// refused before it is decoded), then decodes the whole image.
func inspectRaster(data []byte, format Format, mediaType, extension string,
	decodeConfig func(io.Reader) (image.Config, error), decode func(io.Reader) (image.Image, error),
) (Media, error) {
	cfg, err := decodeConfig(bytes.NewReader(data))
	if err != nil {
		return Media{}, refuse("the file is not a valid %s image", strings.ToUpper(extension))
	}
	if err := checkEdge(cfg.Width, cfg.Height); err != nil {
		return Media{}, err
	}
	if _, err := decode(bytes.NewReader(data)); err != nil {
		return Media{}, refuse("the file is not a valid %s image", strings.ToUpper(extension))
	}
	return Media{Format: format, MediaType: mediaType, Extension: extension, Width: cfg.Width, Height: cfg.Height}, nil
}

func inspectPNG(data []byte) (Media, error) {
	// An animated PNG carries an acTL chunk before the first IDAT.
	for offset := 8; offset+8 <= len(data); {
		length := int(beUint32(data[offset:]))
		kind := string(data[offset+4 : offset+8])
		if kind == "acTL" {
			return Media{}, refuse("an animated PNG is not accepted; use an animated WebP")
		}
		if kind == "IDAT" || length < 0 || offset+12+length > len(data) {
			break
		}
		offset += 12 + length
	}
	return inspectRaster(data, FormatPNG, "image/png", "png", png.DecodeConfig, png.Decode)
}

func inspectSVG(data []byte) (Media, error) {
	if err := svgcheck.Check(data); err != nil {
		return Media{}, refuse("%s", err.Error())
	}
	return Media{Format: FormatSVG, MediaType: "image/svg+xml", Extension: "svg"}, nil
}

// inspectWebP tells a still from an animation by the VP8X animation flag.
func inspectWebP(data []byte) (Media, error) {
	chunks, err := riffChunks(data)
	if err != nil {
		return Media{}, err
	}
	if len(chunks) > 0 && chunks[0].kind == "VP8X" && len(chunks[0].payload) >= 10 && chunks[0].payload[0]&0x02 != 0 {
		return inspectAnimatedWebP(chunks)
	}
	return inspectRaster(data, FormatWebP, "image/webp", "webp", webp.DecodeConfig, webp.Decode)
}

func beUint32(b []byte) uint32 {
	return uint32(b[0])<<24 | uint32(b[1])<<16 | uint32(b[2])<<8 | uint32(b[3])
}

func leUint16(b []byte) uint16 { return uint16(b[0]) | uint16(b[1])<<8 }

func leUint24(b []byte) int { return int(b[0]) | int(b[1])<<8 | int(b[2])<<16 }

func leUint32(b []byte) uint32 {
	return uint32(b[0]) | uint32(b[1])<<8 | uint32(b[2])<<16 | uint32(b[3])<<24
}
