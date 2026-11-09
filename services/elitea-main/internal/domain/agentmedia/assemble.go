package agentmedia

// Loops without a video model (ADR-0050 decision 6, "Generation", Q5 C):
// /llm/v1/images/edits makes 4–8 pose frames of the still, and the server
// assembles them here into an animated WebP that loops forever, then runs
// the result through Inspect like any upload.

import (
	"bytes"
	"image"
	"image/draw"
	_ "image/jpeg" // frame decoder
	"image/png"

	"github.com/HugoSmits86/nativewebp"
	xdraw "golang.org/x/image/draw"
	_ "golang.org/x/image/webp"
)

// The frame bounds of one assembled loop.
const (
	MinLoopFrames = 2
	MaxLoopFrames = 8
	// LoopEdge is the edge of an assembled loop: frames are scaled into a
	// square of this size, so a loop stays well inside MaxBytes.
	LoopEdge = 256
)

// AssembleLoop decodes each frame (PNG, JPEG or WebP), scales it into a
// LoopEdge square, and encodes an animated WebP that loops forever with
// frameMS per frame. The result has passed Inspect.
func AssembleLoop(frames [][]byte, frameMS int) ([]byte, Media, error) {
	if len(frames) < MinLoopFrames || len(frames) > MaxLoopFrames {
		return nil, Media{}, refuse("a loop takes %d to %d frames, got %d", MinLoopFrames, MaxLoopFrames, len(frames))
	}
	if frameMS <= 0 || frameMS*len(frames) > MaxDurationMS {
		return nil, Media{}, refuse("the loop would be %d ms long; at most %d ms", frameMS*len(frames), MaxDurationMS)
	}
	images := make([]image.Image, 0, len(frames))
	durations := make([]uint, 0, len(frames))
	disposals := make([]uint, 0, len(frames))
	for index, data := range frames {
		if len(data) == 0 || len(data) > 8*MaxBytes {
			return nil, Media{}, refuse("frame %d is empty or too large", index+1)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 {
			return nil, Media{}, refuse("frame %d is not a PNG, JPEG or WebP image of at most 4096 px", index+1)
		}
		decoded, _, err := image.Decode(bytes.NewReader(data))
		if err != nil {
			return nil, Media{}, refuse("frame %d is not a valid image", index+1)
		}
		images = append(images, fitSquare(decoded, LoopEdge))
		durations = append(durations, uint(frameMS))
		disposals = append(disposals, 1)
	}
	var out bytes.Buffer
	animation := &nativewebp.Animation{Images: images, Durations: durations, Disposals: disposals, LoopCount: 0}
	if err := nativewebp.EncodeAll(&out, animation, nil); err != nil {
		return nil, Media{}, refuse("the loop could not be encoded")
	}
	media, err := Inspect(out.Bytes())
	if err != nil {
		return nil, Media{}, err
	}
	return out.Bytes(), media, nil
}

// StillEdge is the edge a generated still is scaled into.
const StillEdge = MaxEdge

// FitStill decodes a generated image (PNG, JPEG or WebP), scales it into a
// StillEdge square and encodes it as PNG. Providers answer 1024 px, above
// the validator's edge. The result has passed Inspect.
func FitStill(data []byte) ([]byte, Media, error) {
	if len(data) == 0 || len(data) > 8*MaxBytes {
		return nil, Media{}, refuse("the generated image is empty or too large")
	}
	cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || cfg.Width > 4096 || cfg.Height > 4096 {
		return nil, Media{}, refuse("the generated image is not a PNG, JPEG or WebP of at most 4096 px")
	}
	decoded, _, err := image.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, Media{}, refuse("the generated image is not a valid image")
	}
	var out bytes.Buffer
	if err := png.Encode(&out, fitSquare(decoded, StillEdge)); err != nil {
		return nil, Media{}, refuse("the still could not be encoded")
	}
	media, err := Inspect(out.Bytes())
	if err != nil {
		return nil, Media{}, err
	}
	return out.Bytes(), media, nil
}

// fitSquare scales img to fit an edge×edge square, centred on a transparent
// canvas.
func fitSquare(img image.Image, edge int) image.Image {
	bounds := img.Bounds()
	width, height := bounds.Dx(), bounds.Dy()
	scaledW, scaledH := edge, edge
	if width > height {
		scaledH = max(1, height*edge/width)
	} else if height > width {
		scaledW = max(1, width*edge/height)
	}
	canvas := image.NewNRGBA(image.Rect(0, 0, edge, edge))
	draw.Draw(canvas, canvas.Bounds(), image.Transparent, image.Point{}, draw.Src)
	offsetX, offsetY := (edge-scaledW)/2, (edge-scaledH)/2
	target := image.Rect(offsetX, offsetY, offsetX+scaledW, offsetY+scaledH)
	xdraw.CatmullRom.Scale(canvas, target, img, bounds, xdraw.Over, nil)
	return canvas
}
