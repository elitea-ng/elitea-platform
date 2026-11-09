package agentmedia

import (
	"bytes"

	"golang.org/x/image/webp"
)

// riffChunk is one chunk of a WebP RIFF container.
type riffChunk struct {
	kind    string
	payload []byte
}

// riffChunks splits a WebP file into its top-level chunks. The RIFF size
// must match the file, and every chunk must fit inside it.
func riffChunks(data []byte) ([]riffChunk, error) {
	if len(data) < 12 {
		return nil, refuse("the file is not a valid WebP image")
	}
	size := int(leUint32(data[4:8]))
	if size+8 != len(data) {
		return nil, refuse("the WebP file's RIFF size does not match the file")
	}
	var chunks []riffChunk
	for offset := 12; offset < len(data); {
		if offset+8 > len(data) {
			return nil, refuse("the WebP file ends inside a chunk header")
		}
		length := int(leUint32(data[offset+4 : offset+8]))
		end := offset + 8 + length
		if length < 0 || end > len(data) {
			return nil, refuse("a WebP chunk runs past the end of the file")
		}
		chunks = append(chunks, riffChunk{kind: string(data[offset : offset+4]), payload: data[offset+8 : end]})
		offset = end + length%2 // chunks are padded to an even size
	}
	return chunks, nil
}

// inspectAnimatedWebP reads the VP8X canvas, the ANIM loop count and every
// ANMF frame: each frame's duration adds up to the loop's length, and each
// frame's bitstream is decoded, so a file whose frames are not images is
// refused.
func inspectAnimatedWebP(chunks []riffChunk) (Media, error) {
	header := chunks[0].payload
	width := leUint24(header[4:7]) + 1
	height := leUint24(header[7:10]) + 1
	if err := checkEdge(width, height); err != nil {
		return Media{}, err
	}
	loopSeen := false
	frames, durationMS := 0, 0
	for _, chunk := range chunks[1:] {
		switch chunk.kind {
		case "ANIM":
			if len(chunk.payload) < 6 {
				return Media{}, refuse("the WebP animation header is truncated")
			}
			if loops := leUint16(chunk.payload[4:6]); loops != 0 {
				return Media{}, refuse("the animation plays %d time(s) and stops; an avatar animation must loop forever", loops)
			}
			loopSeen = true
		case "ANMF":
			if len(chunk.payload) < 16 {
				return Media{}, refuse("a WebP animation frame is truncated")
			}
			frames++
			if frames > maxFrames {
				return Media{}, refuse("the animation has more than %d frames", maxFrames)
			}
			durationMS += leUint24(chunk.payload[12:15])
			if err := decodeFrame(chunk.payload[16:]); err != nil {
				return Media{}, err
			}
		}
	}
	if !loopSeen {
		return Media{}, refuse("the WebP animation has no ANIM header")
	}
	if frames == 0 {
		return Media{}, refuse("the WebP animation has no frames")
	}
	if err := checkDuration(durationMS); err != nil {
		return Media{}, err
	}
	return Media{
		Format: FormatAnimatedWebP, MediaType: "image/webp", Extension: "webp",
		Width: width, Height: height, DurationMS: durationMS,
	}, nil
}

// decodeFrame wraps one frame's image chunks (ALPH, VP8 or VP8L) as a
// still WebP and decodes it with the standard decoder.
func decodeFrame(frameData []byte) error {
	var body bytes.Buffer
	body.WriteString("WEBP")
	var bitstream []byte
	for offset := 0; offset+8 <= len(frameData); {
		kind := string(frameData[offset : offset+4])
		length := int(leUint32(frameData[offset+4 : offset+8]))
		end := offset + 8 + length
		if length < 0 || end > len(frameData) {
			return refuse("a WebP animation frame's image runs past the frame")
		}
		// An ALPH chunk is skipped: the colour data is what proves the frame
		// is an image, and an alpha plane cannot change its size.
		if kind == "VP8 " || kind == "VP8L" {
			bitstream = frameData[offset:end]
		}
		offset = end + length%2
	}
	if bitstream == nil {
		return refuse("a WebP animation frame carries no image")
	}
	body.Write(bitstream)
	if len(bitstream)%2 == 1 {
		body.WriteByte(0)
	}
	var file bytes.Buffer
	file.WriteString("RIFF")
	size := uint32(body.Len())
	file.Write([]byte{byte(size), byte(size >> 8), byte(size >> 16), byte(size >> 24)})
	file.Write(body.Bytes())
	if _, err := webp.Decode(bytes.NewReader(file.Bytes())); err != nil {
		return refuse("a WebP animation frame is not a valid image")
	}
	return nil
}
