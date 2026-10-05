package agentexecution

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"hash/crc32"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math/rand"
	"strings"
	"testing"
)

// noisyPhoto is a photo-like image: smooth gradients plus noise, which JPEG
// cannot compress to nothing, so it is megabytes at full size like a real
// camera picture.
func noisyPhoto(t *testing.T, width, height int) []byte {
	t.Helper()
	random := rand.New(rand.NewSource(42))
	picture := image.NewRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			noise := uint8(random.Intn(48))
			picture.Set(x, y, color.RGBA{
				R: uint8(x*255/width) ^ noise, G: uint8(y*255/height) ^ noise, B: uint8((x + y) % 256), A: 255,
			})
		}
	}
	var encoded bytes.Buffer
	if err := jpeg.Encode(&encoded, picture, &jpeg.Options{Quality: 92}); err != nil {
		t.Fatal(err)
	}
	return encoded.Bytes()
}

func embeddedImageOf(t *testing.T, attachment CurrentTurnAttachment) (header string, data []byte, mediaType string) {
	t.Helper()
	chunks := imageChunksOf(t, attachment.Content)
	header, _ = chunks[0]["text"].(string)
	if len(chunks) != 2 {
		return header, nil, ""
	}
	imageURL, _ := chunks[1]["image_url"].(map[string]any)
	url, _ := imageURL["url"].(string)
	prefix, payload, ok := strings.Cut(url, ";base64,")
	if !ok {
		t.Fatalf("url %q is not a base64 data URL", url)
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	return header, decoded, strings.TrimPrefix(prefix, "data:")
}

// A phone photo is megabytes: before client contract 1.1 it reached the model
// as "this image could not be embedded". It is now fitted into the cap.
func TestALargePhotoIsDownscaledToFitTheInlineCap(t *testing.T) {
	photo := noisyPhoto(t, 2400, 1800)
	if len(photo) < 1<<20 {
		t.Fatalf("fixture is %d bytes; it must be a multi-megabyte photo to prove anything", len(photo))
	}
	reader := &stubAttachmentImageReader{content: photo}
	attachments, err := currentTurnAttachmentsWithImages(
		context.Background(), 42, reader,
		"ee92ccbd-3312-4c72-b20b-fddf224e7c0e", testConversationUUID,
		[]CurrentTurnAttachmentRef{{Bucket: "chat-attachments", Name: testConversationUUID + "/photo.jpg"}},
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	header, data, mediaType := embeddedImageOf(t, attachments[0])
	if data == nil {
		t.Fatalf("the photo was not embedded: %q", header)
	}
	if len(data) > maxInlineAttachmentImageBytes || mediaType != "image/jpeg" {
		t.Fatalf("embedded %d bytes of %s, want a JPEG within %d", len(data), mediaType, maxInlineAttachmentImageBytes)
	}
	config, format, err := image.DecodeConfig(bytes.NewReader(data))
	if err != nil || format != "jpeg" || max(config.Width, config.Height) < minInlineAttachmentEdge ||
		config.Width*1800 != config.Height*2400 && abs(config.Width*1800-config.Height*2400) > 2400 {
		t.Fatalf("embedded image %dx%d %s (%v): wrong size or aspect", config.Width, config.Height, format, err)
	}
	if !strings.Contains(header, "downscaled from 2400x1800") {
		t.Fatalf("the model must be told the picture was downscaled: %q", header)
	}
	if strings.Contains(header, "could not be embedded") {
		t.Fatalf("an embedded image must not also be refused: %q", header)
	}
}

func abs(value int) int {
	if value < 0 {
		return -value
	}
	return value
}

// Two photos when the turn's budget fits only one: the first is embedded,
// the second is announced and read.
func TestTwoPhotosWhereTheBudgetFitsOne(t *testing.T) {
	photo := noisyPhoto(t, 1600, 1200)
	reader := &stubAttachmentImageReader{content: photo}
	attachments, err := currentTurnAttachmentsWithImages(
		context.Background(), 42, reader,
		"ee92ccbd-3312-4c72-b20b-fddf224e7c0e", testConversationUUID,
		[]CurrentTurnAttachmentRef{
			{Bucket: "chat-attachments", Name: testConversationUUID + "/one.jpg"},
			{Bucket: "chat-attachments", Name: testConversationUUID + "/two.jpg"},
		},
	)
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	_, first, _ := embeddedImageOf(t, attachments[0])
	if first == nil {
		t.Fatal("the first photo must be embedded")
	}
	used := base64.StdEncoding.EncodedLen(len(first))
	remaining := maxInlineAttachmentImageTurnBytes - used
	header, second, _ := embeddedImageOf(t, attachments[1])
	switch {
	case second == nil:
		if !strings.Contains(header, "not shown to you as an image") {
			t.Fatalf("the refused photo must say so: %q", header)
		}
	case base64.StdEncoding.EncodedLen(len(second)) > remaining:
		t.Fatalf("the second photo (%d encoded) overran the %d left in the turn", base64.StdEncoding.EncodedLen(len(second)), remaining)
	}
	total := used
	if second != nil {
		total += base64.StdEncoding.EncodedLen(len(second))
	}
	if total > maxInlineAttachmentImageTurnBytes {
		t.Fatalf("the turn embedded %d encoded bytes, over its %d", total, maxInlineAttachmentImageTurnBytes)
	}
}

// pngBomb is a tiny PNG whose header declares an enormous image.
func pngBomb(t *testing.T, width, height uint32) []byte {
	t.Helper()
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, image.NewGray(image.Rect(0, 0, 1, 1))); err != nil {
		t.Fatal(err)
	}
	data := encoded.Bytes()
	// IHDR starts after the 8-byte signature: length(4) "IHDR"(4) then
	// width(4) height(4); its CRC covers type+data (17 bytes).
	binary.BigEndian.PutUint32(data[16:20], width)
	binary.BigEndian.PutUint32(data[20:24], height)
	binary.BigEndian.PutUint32(data[29:33], crc32.ChecksumIEEE(data[12:29]))
	// Pad past the inline cap so the downscale path is taken.
	return append(data, make([]byte, maxInlineAttachmentImageBytes)...)
}

// A decompression bomb costs a header parse: it is refused on its declared
// size before any pixel buffer is allocated, and the turn goes on.
func TestADecompressionBombIsAnnouncedNotDecoded(t *testing.T) {
	reader := &stubAttachmentImageReader{content: pngBomb(t, 60_000, 60_000)}
	attachments, err := currentTurnAttachmentsWithImages(
		context.Background(), 42, reader,
		"ee92ccbd-3312-4c72-b20b-fddf224e7c0e", testConversationUUID,
		[]CurrentTurnAttachmentRef{{Bucket: "chat-attachments", Name: testConversationUUID + "/bomb.png"}},
	)
	if err != nil {
		t.Fatalf("a bomb must never fail the turn: %v", err)
	}
	header, data, _ := embeddedImageOf(t, attachments[0])
	if data != nil {
		t.Fatal("a bomb was embedded")
	}
	if !strings.Contains(header, "could not be embedded") {
		t.Fatalf("header=%q", header)
	}
	if _, err := downscaleInlineImage(context.Background(), reader.content, maxInlineAttachmentImageBytes); err != errInlineImageTooManyPx {
		t.Fatalf("downscale of a bomb = %v, want errInlineImageTooManyPx", err)
	}
}

// A transparent PNG is flattened onto white, not black.
func TestATransparentImageIsFlattenedOntoWhite(t *testing.T) {
	picture := image.NewNRGBA(image.Rect(0, 0, 800, 600))
	var encoded bytes.Buffer
	if err := png.Encode(&encoded, picture); err != nil { // fully transparent
		t.Fatal(err)
	}
	downscaled, err := downscaleInlineImage(context.Background(), encoded.Bytes(), maxInlineAttachmentImageBytes)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := jpeg.Decode(bytes.NewReader(downscaled.jpeg))
	if err != nil {
		t.Fatal(err)
	}
	r, g, b, _ := decoded.At(10, 10).RGBA()
	if r>>8 < 240 || g>>8 < 240 || b>>8 < 240 {
		t.Fatalf("a transparent pixel became %d,%d,%d, want white", r>>8, g>>8, b>>8)
	}
}
