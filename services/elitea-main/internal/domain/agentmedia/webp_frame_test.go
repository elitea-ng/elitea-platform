package agentmedia

import (
	"bytes"
	"strings"
	"testing"
)

// patchFirstANMF rewrites one 24-bit field of the first ANMF frame header.
func patchFirstANMF(t *testing.T, data []byte, field int, value int) []byte {
	t.Helper()
	out := append([]byte(nil), data...)
	at := bytes.Index(out, []byte("ANMF"))
	if at < 0 {
		t.Fatal("no ANMF chunk")
	}
	p := at + 8 + field
	out[p], out[p+1], out[p+2] = byte(value), byte(value>>8), byte(value>>16)
	return out
}

// A frame's bitstream is sized from its own header, so a bitstream that
// claims more than its ANMF frame (here the frame says 8x8, the bitstream
// 32x32) is refused before it is decoded; so is a frame outside the canvas.
func TestAnimatedWebPFrameMustMatchItsHeaderAndCanvas(t *testing.T) {
	loop := webpLoop(t, 32, 2, 100, 0)
	if _, err := Inspect(loop); err != nil {
		t.Fatalf("baseline loop refused: %v", err)
	}
	cases := map[string]struct {
		data []byte
		want string
	}{
		"bitstream larger than frame": {patchFirstANMF(t, patchFirstANMF(t, loop, 6, 7), 9, 7), "not the size its frame header names"},
		"frame outside the canvas":    {patchFirstANMF(t, loop, 0, 8), "inside the canvas"},
		"frame wider than the canvas": {patchFirstANMF(t, loop, 6, 1000), "inside the canvas"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			_, err := Inspect(tc.data)
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want %q", err, tc.want)
			}
		})
	}
}
