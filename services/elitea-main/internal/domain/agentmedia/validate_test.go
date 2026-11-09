package agentmedia

import (
	"bytes"
	"encoding/binary"
	"image"
	"image/color"
	"image/jpeg"
	"image/png"
	"math"
	"strings"
	"testing"

	"github.com/HugoSmits86/nativewebp"
)

func solid(width, height int, c color.Color) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, width, height))
	for y := 0; y < height; y++ {
		for x := 0; x < width; x++ {
			img.Set(x, y, c)
		}
	}
	return img
}

func pngOf(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := png.Encode(&buf, solid(width, height, color.NRGBA{R: 200, A: 255})); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func jpegOf(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := jpeg.Encode(&buf, solid(width, height, color.NRGBA{G: 200, A: 255}), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func webpStill(t *testing.T, width, height int) []byte {
	t.Helper()
	var buf bytes.Buffer
	if err := nativewebp.Encode(&buf, solid(width, height, color.NRGBA{B: 200, A: 255}), nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func webpLoop(t *testing.T, edge, frames, frameMS int, loops uint16) []byte {
	t.Helper()
	animation := &nativewebp.Animation{LoopCount: loops}
	for i := 0; i < frames; i++ {
		animation.Images = append(animation.Images, solid(edge, edge, color.NRGBA{R: uint8(40 * i), A: 255}))
		animation.Durations = append(animation.Durations, uint(frameMS))
		animation.Disposals = append(animation.Disposals, 1)
	}
	var buf bytes.Buffer
	if err := nativewebp.EncodeAll(&buf, animation, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// mp4Box builds one ISO BMFF box.
func mp4Box(kind string, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	out := make([]byte, 8, 8+len(body))
	binary.BigEndian.PutUint32(out, uint32(8+len(body)))
	copy(out[4:], kind)
	return append(out, body...)
}

func mp4Track(handler, codec string, width, height int) []byte {
	tkhd := make([]byte, 84) // version 0 tkhd: width/height are the last 8 bytes
	binary.BigEndian.PutUint32(tkhd[76:], uint32(width)<<16)
	binary.BigEndian.PutUint32(tkhd[80:], uint32(height)<<16)
	hdlr := make([]byte, 24)
	copy(hdlr[8:], handler)
	stsd := make([]byte, 16)
	binary.BigEndian.PutUint32(stsd[4:], 1)
	binary.BigEndian.PutUint32(stsd[8:], 8)
	copy(stsd[12:], codec)
	return mp4Box("trak", mp4Box("tkhd", tkhd),
		mp4Box("mdia", mp4Box("hdlr", hdlr), mp4Box("minf", mp4Box("stbl", mp4Box("stsd", stsd)))))
}

func mp4Of(durationMS int, tracks ...[]byte) []byte {
	mvhd := make([]byte, 100)
	binary.BigEndian.PutUint32(mvhd[12:], 1000)
	binary.BigEndian.PutUint32(mvhd[16:], uint32(durationMS))
	moov := mp4Box("moov", append([][]byte{mp4Box("mvhd", mvhd)}, tracks...)...)
	return append(mp4Box("ftyp", []byte("isom\x00\x00\x02\x00isomavc1")), moov...)
}

// ebml builds one EBML element with an 8-byte size.
func ebml(id uint64, payload ...[]byte) []byte {
	body := bytes.Join(payload, nil)
	var idBytes []byte
	for v := id; v > 0; v >>= 8 {
		idBytes = append([]byte{byte(v)}, idBytes...)
	}
	size := make([]byte, 8)
	binary.BigEndian.PutUint64(size, uint64(len(body)))
	size[0] = 0x01
	return append(append(idBytes, size...), body...)
}

func ebmlUint(id, value uint64) []byte {
	b := make([]byte, 8)
	binary.BigEndian.PutUint64(b, value)
	return ebml(id, b)
}

func webmTrackEntry(trackType uint64, codec string, width, height uint64) []byte {
	parts := [][]byte{ebmlUint(idTrackType, trackType), ebml(idCodecID, []byte(codec))}
	if trackType == 1 {
		parts = append(parts, ebml(idVideo, ebmlUint(idPixelWidth, width), ebmlUint(idPixelHeight, height)))
	}
	return ebml(idTrackEntry, parts...)
}

func webmOf(docType string, durationMS float64, entries ...[]byte) []byte {
	duration := make([]byte, 8)
	binary.BigEndian.PutUint64(duration, math.Float64bits(durationMS))
	info := ebml(idInfo, ebmlUint(idTimecodeScale, 1_000_000), ebml(idDuration, duration))
	return append(ebml(idEBML, ebml(idDocType, []byte(docType))), ebml(idSegment, info, ebml(idTracks, entries...))...)
}

func TestInspectAccepts(t *testing.T) {
	cases := map[string]struct {
		data     []byte
		format   Format
		duration int
	}{
		"png":           {pngOf(t, 64, 64), FormatPNG, 0},
		"jpeg":          {jpegOf(t, 512, 300), FormatJPEG, 0},
		"webp still":    {webpStill(t, 128, 128), FormatWebP, 0},
		"svg":           {[]byte(`<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 4 4"><circle r="2"/></svg>`), FormatSVG, 0},
		"animated webp": {webpLoop(t, 64, 4, 250, 0), FormatAnimatedWebP, 1000},
		"mp4 h264":      {mp4Of(3000, mp4Track("vide", "avc1", 320, 320)), FormatMP4, 3000},
		"webm vp9":      {webmOf("webm", 2500, webmTrackEntry(1, "V_VP9", 256, 256)), FormatWebM, 2500},
		"webm av1":      {webmOf("webm", 4000, webmTrackEntry(1, "V_AV1", 512, 288)), FormatWebM, 4000},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			media, err := Inspect(tc.data)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if media.Format != tc.format || media.DurationMS != tc.duration {
				t.Fatalf("got %+v", media)
			}
		})
	}
}

func TestInspectRefuses(t *testing.T) {
	pngBytes := pngOf(t, 32, 32)
	apng := append([]byte{}, pngBytes[:33]...) // signature + IHDR
	apng = append(apng, 0, 0, 0, 8, 'a', 'c', 'T', 'L', 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0)
	apng = append(apng, pngBytes[33:]...)
	oversizeBytes := append(pngOf(t, 8, 8), bytes.Repeat([]byte{0}, MaxBytes)...)

	cases := map[string]struct {
		data []byte
		want string
	}{
		"empty":                   {nil, "empty"},
		"oversize bytes":          {oversizeBytes, "at most 2048 KiB"},
		"oversize png edge":       {pngOf(t, 513, 64), "at most 512 px"},
		"oversize webp loop edge": {webpLoop(t, 600, 2, 100, 0), "at most 512 px"},
		"oversize mp4 edge":       {mp4Of(1000, mp4Track("vide", "avc1", 1280, 720)), "at most 512 px"},
		"too long webp":           {webpLoop(t, 32, 5, 1000, 0), "at most 4 s"},
		"too long mp4":            {mp4Of(4500, mp4Track("vide", "avc1", 256, 256)), "at most 4 s"},
		"too long webm":           {webmOf("webm", 6000, webmTrackEntry(1, "V_VP9", 256, 256)), "at most 4 s"},
		"mp4 with audio":          {mp4Of(2000, mp4Track("vide", "avc1", 256, 256), mp4Track("soun", "mp4a", 0, 0)), "audio track"},
		"webm with audio":         {webmOf("webm", 2000, webmTrackEntry(1, "V_VP9", 256, 256), webmTrackEntry(2, "A_OPUS", 0, 0)), "audio track"},
		"webp plays once":         {webpLoop(t, 32, 3, 100, 1), "must loop forever"},
		"mp4 wrong codec":         {mp4Of(2000, mp4Track("vide", "hvc1", 256, 256)), "only H.264"},
		"webm wrong codec":        {webmOf("webm", 2000, webmTrackEntry(1, "V_VP8", 256, 256)), "only VP9 and AV1"},
		"matroska not webm":       {webmOf("matroska", 2000, webmTrackEntry(1, "V_VP9", 256, 256)), "not WebM"},
		"mp4 no video":            {mp4Of(2000), "0 video tracks"},
		"gif":                     {[]byte("GIF89a\x01\x00\x01\x00"), "GIF is not accepted"},
		"animated png":            {apng, "animated PNG"},
		"html named png":          {[]byte("<html><script>alert(1)</script></html>"), "not <svg>"},
		"exe disguised as png":    {[]byte("MZ\x90\x00\x03\x00\x00\x00"), "not a PNG, JPEG"},
		"truncated png":           {pngBytes[:40], "not a valid PNG"},
		"png magic then junk":     {append([]byte("\x89PNG\r\n\x1a\n"), bytes.Repeat([]byte{7}, 64)...), "not a valid PNG"},
		"webp riff size lies":     {append(webpStill(t, 8, 8), 0, 0), "RIFF size"},
		"svg script":              {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><script>alert(1)</script></svg>`), "<script>"},
		"svg onload":              {[]byte(`<svg xmlns="http://www.w3.org/2000/svg" onload="alert(1)"/>`), "event handler"},
		"svg external href":       {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><image href="https://evil.example/x.png"/></svg>`), "references"},
		"svg foreignObject":       {[]byte(`<svg xmlns="http://www.w3.org/2000/svg"><foreignObject/></svg>`), "foreignObject"},
		"mp4 box past end":        {append(mp4Box("ftyp", []byte("isom")), 0, 0, 1, 0, 'm', 'o', 'o', 'v'), "past the end"},
		"fragmented mp4":          {append(mp4Of(1000, mp4Track("vide", "avc1", 64, 64)), mp4Box("moof")...), "fragmented"},
		"webm without duration":   {append(ebml(idEBML, ebml(idDocType, []byte("webm"))), ebml(idSegment, ebml(idTracks, webmTrackEntry(1, "V_VP9", 64, 64)))...), "no duration"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Inspect(tc.data)
			if err == nil {
				t.Fatal("accepted")
			}
			if !IsRefusal(err) {
				t.Fatalf("not a refusal: %v", err)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("reason %q does not name %q", err.Error(), tc.want)
			}
		})
	}
}

func TestAssembleLoop(t *testing.T) {
	frames := [][]byte{pngOf(t, 600, 400), jpegOf(t, 300, 300), webpStill(t, 128, 256), pngOf(t, 64, 64)}
	data, media, err := AssembleLoop(frames, 250)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if media.Format != FormatAnimatedWebP || media.DurationMS != 1000 || media.Width != LoopEdge || media.Height != LoopEdge {
		t.Fatalf("got %+v", media)
	}
	if _, err := Inspect(data); err != nil {
		t.Fatalf("the assembled loop fails the validator: %v", err)
	}
	if _, _, err := AssembleLoop(frames[:1], 250); err == nil {
		t.Fatal("one frame accepted")
	}
	if _, _, err := AssembleLoop(frames, 1200); err == nil {
		t.Fatal("a loop over 4 s accepted")
	}
	if _, _, err := AssembleLoop([][]byte{frames[0], []byte("not an image")}, 250); err == nil {
		t.Fatal("a non-image frame accepted")
	}
}
