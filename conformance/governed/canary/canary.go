// Package canary finds secret canary values in evidence: NATS captures,
// database dumps, container logs and audit exports (issue #1124, item 3).
//
// A canary is a unique, unguessable value seeded as a credential. It must
// never appear anywhere but the secret store and the provider that receives
// it, so ANY occurrence in the evidence is a leak. The scanner looks for each
// canary in the encodings a leak is actually written in:
//
//	raw            a log line, a JSON string, a protobuf string field
//	hex            pg_dump's bytea output (\x...), lower and upper case
//	base64         JSON-encoded bytes, a JWT segment, a Basic header; the
//	               standard and URL alphabets, at all three byte alignments
//
// A hit names the canary's LABEL, the file, the encoding and the offset,
// never the value, so the report itself cannot leak.
package canary

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

// Prefix starts every generated canary, so a value is recognisable as test
// material wherever it turns up.
const Prefix = "gvcanary"

// MinLength refuses a canary short enough to collide with ordinary bytes.
const MinLength = 24

var labelPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,47}$`)

// Canary is one seeded value and the label a report names it by.
type Canary struct {
	Label string `json:"label"`
	Value string `json:"value"`
}

// New returns a fresh canary for label: Prefix, the label and 128 random
// bits in hex.
func New(label string) (Canary, error) {
	if !labelPattern.MatchString(label) {
		return Canary{}, fmt.Errorf("canary: label %q must match %s", label, labelPattern)
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return Canary{}, err
	}
	return Canary{Label: label, Value: Prefix + "-" + label + "-" + hex.EncodeToString(raw)}, nil
}

// Hit is one occurrence of a canary.
type Hit struct {
	Label    string `json:"label"`
	File     string `json:"file"`
	Encoding string `json:"encoding"`
	Offset   int64  `json:"offset"`
}

func (h Hit) String() string {
	return fmt.Sprintf("%s: canary %q (%s) at byte %d", h.File, h.Label, h.Encoding, h.Offset)
}

type needle struct {
	label    string
	encoding string
	bytes    []byte
}

// Scanner holds the needles for a set of canaries.
type Scanner struct {
	needles []needle
	longest int
}

// NewScanner builds a scanner. An empty set is an error: a scan that looks
// for nothing finds nothing, and that must never read as "no leak".
func NewScanner(canaries []Canary) (*Scanner, error) {
	if len(canaries) == 0 {
		return nil, errors.New("canary: no canaries to look for")
	}
	seen := map[string]bool{}
	s := &Scanner{}
	for _, c := range canaries {
		if len(c.Value) < MinLength {
			return nil, fmt.Errorf("canary %q: value shorter than %d bytes", c.Label, MinLength)
		}
		if seen[c.Value] {
			return nil, fmt.Errorf("canary %q: duplicate value", c.Label)
		}
		seen[c.Value] = true
		for encoding, body := range encodings([]byte(c.Value)) {
			s.needles = append(s.needles, needle{label: c.Label, encoding: encoding, bytes: body})
			if len(body) > s.longest {
				s.longest = len(body)
			}
		}
	}
	sort.Slice(s.needles, func(i, j int) bool {
		if s.needles[i].label != s.needles[j].label {
			return s.needles[i].label < s.needles[j].label
		}
		return s.needles[i].encoding < s.needles[j].encoding
	})
	return s, nil
}

// encodings is every form of value the scanner looks for.
func encodings(value []byte) map[string][]byte {
	out := map[string][]byte{
		"raw":       value,
		"hex":       []byte(hex.EncodeToString(value)),
		"hex-upper": []byte(strings.ToUpper(hex.EncodeToString(value))),
	}
	for name, alphabet := range map[string]*base64.Encoding{"base64": base64.StdEncoding, "base64url": base64.URLEncoding} {
		for shift := 0; shift < 3; shift++ {
			out[fmt.Sprintf("%s+%d", name, shift)] = []byte(alignedBase64(alphabet, value, shift))
		}
	}
	return out
}

// alignedBase64 is the part of base64(prefix || value || suffix) that depends
// on value alone, where prefix is shift bytes long: the encoding of the whole
// 3-byte groups that hold value bytes only. Whatever surrounds value, those
// characters appear verbatim in the encoding.
func alignedBase64(alphabet *base64.Encoding, value []byte, shift int) string {
	lead := (3 - shift) % 3 // value bytes that share a group with the prefix
	if lead > len(value) {
		return ""
	}
	body := value[lead:]
	body = body[:len(body)/3*3]
	return strings.TrimRight(alphabet.EncodeToString(body), "=")
}

const chunkSize = 1 << 20

// ScanReader reports every needle in r. name labels the hits.
func (s *Scanner) ScanReader(name string, r io.Reader) ([]Hit, int64, error) {
	var hits []Hit
	overlap := s.longest - 1
	buffer := make([]byte, 0, chunkSize+overlap)
	var base int64 // offset of buffer[0] in the stream
	var total int64
	chunk := make([]byte, chunkSize)
	reported := map[string]bool{}
	for {
		n, err := r.Read(chunk)
		total += int64(n)
		buffer = append(buffer, chunk[:n]...)
		for _, nd := range s.needles {
			if len(nd.bytes) == 0 {
				continue
			}
			from := 0
			for {
				index := bytes.Index(buffer[from:], nd.bytes)
				if index < 0 {
					break
				}
				offset := base + int64(from+index)
				key := fmt.Sprintf("%s/%s/%d", nd.label, nd.encoding, offset)
				if !reported[key] {
					reported[key] = true
					hits = append(hits, Hit{Label: nd.label, File: name, Encoding: nd.encoding, Offset: offset})
				}
				from += index + 1
			}
		}
		if len(buffer) > overlap {
			drop := len(buffer) - overlap
			base += int64(drop)
			buffer = append(buffer[:0], buffer[drop:]...)
		}
		if errors.Is(err, io.EOF) {
			return hits, total, nil
		}
		if err != nil {
			return hits, total, err
		}
	}
}

// Report is the outcome of a directory scan.
type Report struct {
	Files int      `json:"files"`
	Bytes int64    `json:"bytes"`
	Hits  []Hit    `json:"hits"`
	Read  []string `json:"read"`
}

// ScanDir scans every regular file under root. skip, when it answers true
// for a slash-separated path relative to root, leaves that file out (the
// canary list itself).
func (s *Scanner) ScanDir(root string, skip func(rel string) bool) (Report, error) {
	var report Report
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		rel, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		rel = filepath.ToSlash(rel)
		if skip != nil && skip(rel) {
			return nil
		}
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer func() { _ = file.Close() }()
		hits, n, err := s.ScanReader(rel, file)
		if err != nil {
			return fmt.Errorf("%s: %w", rel, err)
		}
		report.Files++
		report.Bytes += n
		report.Hits = append(report.Hits, hits...)
		report.Read = append(report.Read, rel)
		return nil
	})
	return report, err
}
