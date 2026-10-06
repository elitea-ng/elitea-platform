package codeplatform

import (
	"bytes"
	"encoding/binary"
	"strings"
	"testing"
)

func frame(text string, payload []byte) []byte {
	out := make([]byte, 8+len(text)+len(payload))
	binary.BigEndian.PutUint32(out[:4], uint32(len(text)))
	binary.BigEndian.PutUint32(out[4:8], uint32(len(payload)))
	copy(out[8:], text)
	copy(out[8+len(text):], payload)
	return out
}
func TestCodeFrameBindsExactBytesAndBinary(t *testing.T) {
	raw := `{"revision":1,"sequence":1,"operation":"artifact_write_chunk","resource":{"kind":"artifact_transfer","id":"` + strings.Repeat("a", 64) + `"},"arguments":{"offset":0}}`
	binary := []byte{0, 255, 128, 1}
	first, err := DecodeRequest(frame(raw, binary))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(first.Payload, binary) {
		t.Fatal("binary changed")
	}
	spaced, err := DecodeRequest(frame(strings.Replace(raw, `"offset":0`, `"offset": 0`, 1), binary))
	if err != nil {
		t.Fatal(err)
	}
	if first.CallDigest([32]byte{1}, [32]byte{2}, [32]byte{3}) == spaced.CallDigest([32]byte{1}, [32]byte{2}, [32]byte{3}) {
		t.Fatal("exact byte identity discarded")
	}
	if first.CallDigest([32]byte{1}, [32]byte{2}, [32]byte{3}) == first.CallDigest([32]byte{1}, [32]byte{2}, [32]byte{4}) {
		t.Fatal("policy identity discarded")
	}
}
func TestCodeFrameRefusesCallerAuthorityAndWrongResources(t *testing.T) {
	valid := `{"revision":1,"sequence":1,"operation":"secret_read","resource":{"kind":"secret","scope":"personal","name":"same"},"arguments":{}}`
	cases := []string{
		strings.Replace(valid, `"revision":1`, `"revision":1,"actor":"2"`, 1),
		strings.Replace(valid, `"scope":"personal"`, `"scope":"personal","project":"9"`, 1),
		strings.Replace(valid, `"name":"same"`, `"name":"same","name":"other"`, 1),
		strings.Replace(valid, `"name":"same"`, `"name":"same\n"`, 1),
		strings.Replace(valid, `"sequence":1`, `"sequence":0`, 1),
		strings.Replace(valid, `"arguments":{}`, `"arguments":{"default":"x"}`, 1),
		strings.Replace(valid, `"operation":"secret_read"`, `"operation":"http_fetch"`, 1),
	}
	for _, text := range cases {
		if _, err := DecodeRequest(frame(text, nil)); err == nil {
			t.Fatalf("accepted %s", text)
		}
	}
	if _, err := DecodeRequest(frame(valid, []byte{1})); err == nil {
		t.Fatal("accepted binary secret request")
	}
}
func TestCodeFrameBoundsBeforeCloneAndSerialization(t *testing.T) {
	header := make([]byte, 8)
	binary.BigEndian.PutUint32(header[:4], MaxRequestHeader+1)
	for _, raw := range [][]byte{header, make([]byte, 8+MaxRequestHeader+MaxChunk+1)} {
		if _, err := DecodeRequest(raw); err == nil {
			t.Fatal("accepted oversized frame")
		}
	}
	if validateJSON([]byte(strings.Repeat("[", 35)+"0"+strings.Repeat("]", 35))) == nil {
		t.Fatal("accepted deep JSON")
	}
	for _, raw := range []string{`{"a":1,"a":2}`, `1e999`, `{} {}`} {
		if validateJSON([]byte(raw)) == nil {
			t.Fatalf("accepted %s", raw)
		}
	}
}
