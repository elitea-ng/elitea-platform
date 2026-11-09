package agentmedia

// A minimal EBML (WebM) reader. It checks the DocType is `webm`, reads
// Segment → Info (TimecodeScale and Duration) and Segment → Tracks →
// TrackEntry (TrackType, CodecID, Video PixelWidth and PixelHeight), and
// skips every other element, Clusters included.

import (
	"encoding/binary"
	"math"
)

// EBML element ids.
const (
	idEBML          = 0x1A45DFA3
	idDocType       = 0x4282
	idSegment       = 0x18538067
	idInfo          = 0x1549A966
	idTimecodeScale = 0x2AD7B1
	idDuration      = 0x4489
	idTracks        = 0x1654AE6B
	idTrackEntry    = 0xAE
	idTrackType     = 0x83
	idCodecID       = 0x86
	idVideo         = 0xE0
	idPixelWidth    = 0xB0
	idPixelHeight   = 0xBA
)

type element struct {
	id      uint64
	payload []byte
}

// readVint reads an EBML variable-length integer. keepMarker keeps the
// length marker bit (ids keep it; sizes drop it). unknown reports an
// all-ones size (an element of unknown size).
func readVint(data []byte, keepMarker bool) (value uint64, length int, unknown bool, ok bool) {
	if len(data) == 0 || data[0] == 0 {
		return 0, 0, false, false
	}
	length = 1
	for mask := byte(0x80); data[0]&mask == 0; mask >>= 1 {
		length++
	}
	if length > 8 || len(data) < length {
		return 0, 0, false, false
	}
	first := data[0]
	if !keepMarker {
		first &^= 0x80 >> (length - 1)
	}
	value = uint64(first)
	allOnes := first == byte(0xFF>>length)
	for i := 1; i < length; i++ {
		value = value<<8 | uint64(data[i])
		allOnes = allOnes && data[i] == 0xFF
	}
	return value, length, !keepMarker && allOnes, true
}

// elements splits data into its elements. An element of unknown size (a
// live-streamed Segment or Cluster) runs to the end of data.
func elements(data []byte) ([]element, error) {
	var out []element
	for offset := 0; offset < len(data); {
		id, idLen, _, ok := readVint(data[offset:], true)
		if !ok {
			return nil, refuse("the WebM file has a malformed element id")
		}
		size, sizeLen, unknown, ok := readVint(data[offset+idLen:], false)
		if !ok {
			return nil, refuse("the WebM file has a malformed element size")
		}
		start := offset + idLen + sizeLen
		end := len(data)
		if !unknown {
			if size > uint64(len(data)-start) {
				return nil, refuse("a WebM element runs past the end of the file")
			}
			end = start + int(size)
		}
		out = append(out, element{id: id, payload: data[start:end]})
		offset = end
		if len(out) > 8192 {
			return nil, refuse("the WebM file has too many elements")
		}
	}
	return out, nil
}

func uintOf(b []byte) uint64 {
	var v uint64
	for _, c := range b {
		v = v<<8 | uint64(c)
	}
	return v
}

func floatOf(b []byte) (float64, bool) {
	switch len(b) {
	case 4:
		return float64(math.Float32frombits(binary.BigEndian.Uint32(b))), true
	case 8:
		return math.Float64frombits(binary.BigEndian.Uint64(b)), true
	}
	return 0, false
}

func inspectWebM(data []byte) (Media, error) {
	top, err := elements(data)
	if err != nil {
		return Media{}, err
	}
	var header, segment []byte
	for _, e := range top {
		switch e.id {
		case idEBML:
			header = e.payload
		case idSegment:
			segment = e.payload
		}
	}
	if header == nil || segment == nil {
		return Media{}, refuse("the file is not a valid WebM video")
	}
	headerFields, err := elements(header)
	if err != nil {
		return Media{}, err
	}
	docType := ""
	for _, e := range headerFields {
		if e.id == idDocType {
			docType = string(e.payload)
		}
	}
	if docType != "webm" {
		return Media{}, refuse("the file is a %q EBML document, not WebM", docType)
	}
	sections, err := elements(segment)
	if err != nil {
		return Media{}, err
	}
	timecodeScale := uint64(1_000_000)
	durationTicks := -1.0
	var tracks []byte
	for _, e := range sections {
		switch e.id {
		case idInfo:
			info, err := elements(e.payload)
			if err != nil {
				return Media{}, err
			}
			for _, field := range info {
				switch field.id {
				case idTimecodeScale:
					timecodeScale = uintOf(field.payload)
				case idDuration:
					if v, ok := floatOf(field.payload); ok {
						durationTicks = v
					}
				}
			}
		case idTracks:
			tracks = e.payload
		}
	}
	if durationTicks < 0 || timecodeScale == 0 || math.IsNaN(durationTicks) || math.IsInf(durationTicks, 0) {
		return Media{}, refuse("the WebM file states no duration")
	}
	durationMS := int(math.Ceil(durationTicks * float64(timecodeScale) / 1e6))
	if err := checkDuration(durationMS); err != nil {
		return Media{}, err
	}
	if tracks == nil {
		return Media{}, refuse("the WebM file has no tracks")
	}
	entries, err := elements(tracks)
	if err != nil {
		return Media{}, err
	}
	width, height, videoTracks := 0, 0, 0
	for _, entry := range entries {
		if entry.id != idTrackEntry {
			continue
		}
		trackType, codec, w, h, err := webmTrack(entry.payload)
		if err != nil {
			return Media{}, err
		}
		switch trackType {
		case 1:
			videoTracks++
			if codec != "V_VP9" && codec != "V_AV1" {
				return Media{}, refuse("the WebM video codec is %q; only VP9 and AV1 are accepted", codec)
			}
			width, height = max(width, w), max(height, h)
		case 2:
			return Media{}, refuse("the video has an audio track; an avatar animation must be muted (remove the audio track)")
		default:
			return Media{}, refuse("the WebM file has a track of type %d; only one video track is accepted", trackType)
		}
	}
	if videoTracks != 1 {
		return Media{}, refuse("the WebM file has %d video tracks; exactly one is accepted", videoTracks)
	}
	if err := checkEdge(width, height); err != nil {
		return Media{}, err
	}
	return Media{
		Format: FormatWebM, MediaType: "video/webm", Extension: "webm",
		Width: width, Height: height, DurationMS: durationMS,
	}, nil
}

func webmTrack(entry []byte) (trackType uint64, codec string, width, height int, err error) {
	fields, err := elements(entry)
	if err != nil {
		return 0, "", 0, 0, err
	}
	for _, field := range fields {
		switch field.id {
		case idTrackType:
			trackType = uintOf(field.payload)
		case idCodecID:
			codec = string(field.payload)
		case idVideo:
			video, err := elements(field.payload)
			if err != nil {
				return 0, "", 0, 0, err
			}
			for _, v := range video {
				switch v.id {
				case idPixelWidth:
					width = int(min(uintOf(v.payload), 1<<20))
				case idPixelHeight:
					height = int(min(uintOf(v.payload), 1<<20))
				}
			}
		}
	}
	return trackType, codec, width, height, nil
}
