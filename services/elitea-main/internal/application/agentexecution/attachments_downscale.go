package agentexecution

// Downscaling an attached image so the model can see it (client contract 1.1).
//
// The inline cap (maxInlineAttachmentImageBytes, 48 KiB) is sized by the
// worker's input bundle, and a phone photo is megabytes: before this file such
// a photo reached the model as a filename plus "this image could not be
// embedded". Now an image in one of the inline formats that is over the cap —
// or over what the turn's budget has left — is decoded, fitted into a smaller
// long edge and re-encoded as JPEG until it fits, down to a floor of 256
// pixels. Only the chunk handed to the model changes; the stored object is
// untouched, so a download still answers the original.
//
// THREE GUARDS, because the bytes are a user upload decoded inside the API
// process:
//
//   - the source read is capped (maxInlineAttachmentSourceBytes), so a large
//     object is refused before it is read;
//   - image.DecodeConfig reads only the header, and a header that claims more
//     than maxInlineAttachmentSourcePixels is refused before any pixel buffer
//     is allocated — a decompression bomb (a tiny PNG declaring 50 000 x
//     50 000 pixels) costs a header parse, not gigabytes;
//   - at most inlineDownscaleConcurrency decodes run at once in the process,
//     so a burst of turns with photos bounds memory instead of multiplying it.

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	_ "image/gif" // decoder registration
	"image/jpeg"
	_ "image/png" // decoder registration

	"golang.org/x/image/draw"
	_ "golang.org/x/image/webp" // decoder registration
)

const (
	// maxInlineAttachmentSourceBytes bounds the stored object read for a
	// possible downscale. The upload's own image ceiling defaults to 3 MiB
	// (ARTIFACT_ATTACHMENT_MAX_IMAGE_MB); a deployment that raised it past
	// this keeps the pre-downscale behaviour for the larger files.
	maxInlineAttachmentSourceBytes = 16 << 20
	// maxInlineAttachmentSourcePixels bounds the decoded image: 40 MP covers
	// every phone camera's default output with room to spare.
	maxInlineAttachmentSourcePixels = 40_000_000
	// minInlineAttachmentEdge is the floor: below it a picture is not worth
	// showing, and the file is announced and read instead.
	minInlineAttachmentEdge = 256
	// inlineDownscaleConcurrency bounds simultaneous decodes in the process.
	inlineDownscaleConcurrency = 2
)

// inlineDownscaleEdges are the long edges tried, largest first. The first is
// also the one intermediate the smaller sizes are resampled from, so the
// expensive pass over the full-size source happens once.
var inlineDownscaleEdges = []int{1024, 768, 640, 512, 384, 320, minInlineAttachmentEdge}

// inlineDownscaleQualities are the JPEG qualities tried at each edge.
var inlineDownscaleQualities = []int{80, 65, 50}

var inlineDownscaleSlots = make(chan struct{}, inlineDownscaleConcurrency)

var (
	errInlineImageUndecodable = errors.New("image cannot be decoded")
	errInlineImageTooManyPx   = errors.New("image has too many pixels")
	errInlineImageDoesNotFit  = errors.New("image does not fit at the smallest size")
)

// downscaledInlineImage is a re-encoded JPEG and the sizes it came from.
type downscaledInlineImage struct {
	jpeg                              []byte
	sourceWidth, sourceHeight         int
	downscaledWidth, downscaledHeight int
}

// downscaleInlineImage fits content into at most limit bytes of JPEG.
func downscaleInlineImage(ctx context.Context, content []byte, limit int) (downscaledInlineImage, error) {
	config, _, err := image.DecodeConfig(bytes.NewReader(content))
	if err != nil || config.Width <= 0 || config.Height <= 0 {
		return downscaledInlineImage{}, errInlineImageUndecodable
	}
	if int64(config.Width)*int64(config.Height) > maxInlineAttachmentSourcePixels {
		return downscaledInlineImage{}, errInlineImageTooManyPx
	}
	select {
	case inlineDownscaleSlots <- struct{}{}:
	case <-ctx.Done():
		return downscaledInlineImage{}, ctx.Err()
	}
	defer func() { <-inlineDownscaleSlots }()

	source, _, err := image.Decode(bytes.NewReader(content))
	if err != nil {
		return downscaledInlineImage{}, errInlineImageUndecodable
	}
	bounds := source.Bounds()
	longEdge := max(bounds.Dx(), bounds.Dy())

	// The one pass over the full-size source: fit it into the largest edge
	// tried (or keep its own size when it is already smaller), over white so
	// a transparent PNG does not turn black in a JPEG.
	firstEdge := min(longEdge, inlineDownscaleEdges[0])
	intermediate := fitImage(source, firstEdge)
	for _, edge := range inlineDownscaleEdges {
		if edge > firstEdge {
			continue
		}
		if err := ctx.Err(); err != nil {
			return downscaledInlineImage{}, err
		}
		candidate := intermediate
		if edge < firstEdge {
			candidate = fitImage(intermediate, edge)
		}
		for _, quality := range inlineDownscaleQualities {
			var encoded bytes.Buffer
			if err := jpeg.Encode(&encoded, candidate, &jpeg.Options{Quality: quality}); err != nil {
				return downscaledInlineImage{}, errInlineImageUndecodable
			}
			if encoded.Len() <= limit {
				size := candidate.Bounds()
				return downscaledInlineImage{
					jpeg:        encoded.Bytes(),
					sourceWidth: bounds.Dx(), sourceHeight: bounds.Dy(),
					downscaledWidth: size.Dx(), downscaledHeight: size.Dy(),
				}, nil
			}
		}
	}
	return downscaledInlineImage{}, errInlineImageDoesNotFit
}

// fitImage scales src so its long edge is edge (never up), onto an opaque
// white RGBA canvas, with Catmull-Rom resampling.
func fitImage(src image.Image, edge int) *image.RGBA {
	bounds := src.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	if long := max(width, height); long > edge {
		width = max(1, width*edge/long)
		height = max(1, height*edge/long)
	}
	canvas := image.NewRGBA(image.Rect(0, 0, width, height))
	draw.Draw(canvas, canvas.Bounds(), &image.Uniform{C: color.White}, image.Point{}, draw.Src)
	draw.CatmullRom.Scale(canvas, canvas.Bounds(), src, bounds, draw.Over, nil)
	return canvas
}
