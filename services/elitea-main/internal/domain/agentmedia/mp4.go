package agentmedia

// A minimal ISO BMFF (MP4) reader. It walks moov → mvhd (the duration) and
// every trak → tkhd (the frame size) and mdia → hdlr (the track type) and
// minf → stbl → stsd (the codec). It refuses a sound track, a track that is
// neither video nor sound metadata, a codec other than H.264, and a
// fragmented file whose duration is not in mvhd.

import "encoding/binary"

type box struct {
	kind    string
	payload []byte
}

// boxes splits data into its boxes. A size of 1 is a 64-bit size; a size
// of 0 runs to the end.
func boxes(data []byte) ([]box, error) {
	var out []box
	for offset := 0; offset < len(data); {
		if offset+8 > len(data) {
			return nil, refuse("the MP4 file ends inside a box header")
		}
		size := uint64(binary.BigEndian.Uint32(data[offset:]))
		kind := string(data[offset+4 : offset+8])
		header := uint64(8)
		switch size {
		case 0:
			size = uint64(len(data) - offset)
		case 1:
			if offset+16 > len(data) {
				return nil, refuse("the MP4 file ends inside a box header")
			}
			size = binary.BigEndian.Uint64(data[offset+8:])
			header = 16
		}
		if size < header || size > uint64(len(data)-offset) {
			return nil, refuse("an MP4 box runs past the end of the file")
		}
		out = append(out, box{kind: kind, payload: data[offset+int(header) : offset+int(size)]})
		offset += int(size)
		if len(out) > 4096 {
			return nil, refuse("the MP4 file has too many boxes")
		}
	}
	return out, nil
}

func child(parent []byte, kind string) ([]byte, bool, error) {
	children, err := boxes(parent)
	if err != nil {
		return nil, false, err
	}
	for _, b := range children {
		if b.kind == kind {
			return b.payload, true, nil
		}
	}
	return nil, false, nil
}

func inspectMP4(data []byte) (Media, error) {
	top, err := boxes(data)
	if err != nil {
		return Media{}, err
	}
	var moov []byte
	for _, b := range top {
		switch b.kind {
		case "moov":
			moov = b.payload
		case "moof":
			return Media{}, refuse("a fragmented MP4 is not accepted; export a plain (faststart) MP4")
		}
	}
	if moov == nil {
		return Media{}, refuse("the MP4 file has no movie header (moov)")
	}
	mvhd, ok, err := child(moov, "mvhd")
	if err != nil {
		return Media{}, err
	}
	if !ok {
		return Media{}, refuse("the MP4 file has no movie header (mvhd)")
	}
	durationMS, err := mvhdDuration(mvhd)
	if err != nil {
		return Media{}, err
	}
	if err := checkDuration(durationMS); err != nil {
		return Media{}, err
	}
	children, err := boxes(moov)
	if err != nil {
		return Media{}, err
	}
	width, height, videoTracks := 0, 0, 0
	for _, trak := range children {
		if trak.kind != "trak" {
			continue
		}
		handler, w, h, codec, err := inspectTrack(trak.payload)
		if err != nil {
			return Media{}, err
		}
		switch handler {
		case "vide":
			videoTracks++
			if codec != "avc1" && codec != "avc3" {
				return Media{}, refuse("the MP4 video codec is %q; only H.264 (avc1) is accepted", codec)
			}
			width, height = max(width, w), max(height, h)
		case "soun":
			return Media{}, refuse("the video has an audio track; an avatar animation must be muted (remove the audio track)")
		default:
			return Media{}, refuse("the MP4 file has a %q track; only one video track is accepted", handler)
		}
	}
	if videoTracks != 1 {
		return Media{}, refuse("the MP4 file has %d video tracks; exactly one is accepted", videoTracks)
	}
	if err := checkEdge(width, height); err != nil {
		return Media{}, err
	}
	return Media{
		Format: FormatMP4, MediaType: "video/mp4", Extension: "mp4",
		Width: width, Height: height, DurationMS: durationMS,
	}, nil
}

func mvhdDuration(mvhd []byte) (int, error) {
	if len(mvhd) < 4 {
		return 0, refuse("the MP4 movie header is truncated")
	}
	var timescale, duration uint64
	switch mvhd[0] {
	case 0:
		if len(mvhd) < 20 {
			return 0, refuse("the MP4 movie header is truncated")
		}
		timescale = uint64(binary.BigEndian.Uint32(mvhd[12:]))
		duration = uint64(binary.BigEndian.Uint32(mvhd[16:]))
	case 1:
		if len(mvhd) < 32 {
			return 0, refuse("the MP4 movie header is truncated")
		}
		timescale = uint64(binary.BigEndian.Uint32(mvhd[20:]))
		duration = binary.BigEndian.Uint64(mvhd[24:])
	default:
		return 0, refuse("the MP4 movie header has an unknown version")
	}
	if timescale == 0 {
		return 0, refuse("the MP4 movie header states no timescale")
	}
	if duration > 1<<40 {
		return 0, refuse("the animation is too long")
	}
	return int(duration * 1000 / timescale), nil
}

// inspectTrack answers the track's handler type, its tkhd frame size and
// the four-character code of its first sample entry.
func inspectTrack(trak []byte) (handler string, width, height int, codec string, err error) {
	if tkhd, ok, e := child(trak, "tkhd"); e != nil {
		return "", 0, 0, "", e
	} else if ok {
		width, height = tkhdSize(tkhd)
	}
	mdia, ok, err := child(trak, "mdia")
	if err != nil || !ok {
		return "", 0, 0, "", firstErr(err, refuse("an MP4 track has no media box"))
	}
	hdlr, ok, err := child(mdia, "hdlr")
	if err != nil || !ok || len(hdlr) < 12 {
		return "", 0, 0, "", firstErr(err, refuse("an MP4 track has no handler"))
	}
	handler = string(hdlr[8:12])
	minf, ok, err := child(mdia, "minf")
	if err != nil || !ok {
		return handler, width, height, "", err
	}
	stbl, ok, err := child(minf, "stbl")
	if err != nil || !ok {
		return handler, width, height, "", err
	}
	stsd, ok, err := child(stbl, "stsd")
	if err != nil || !ok || len(stsd) < 16 {
		return handler, width, height, "", err
	}
	// version+flags (4), entry count (4), then the first entry's size (4)
	// and format (4).
	codec = string(stsd[12:16])
	return handler, width, height, codec, nil
}

// tkhdSize reads the 16.16 fixed-point width and height at the end of tkhd.
func tkhdSize(tkhd []byte) (int, int) {
	if len(tkhd) < 8 {
		return 0, 0
	}
	end := len(tkhd)
	return int(binary.BigEndian.Uint32(tkhd[end-8:]) >> 16), int(binary.BigEndian.Uint32(tkhd[end-4:]) >> 16)
}

func firstErr(errs ...error) error {
	for _, err := range errs {
		if err != nil {
			return err
		}
	}
	return nil
}
