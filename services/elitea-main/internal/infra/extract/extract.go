// Package extract turns one stored chat attachment into plain text plus a map
// of where each page, slide, sheet or section starts and ends in that text.
//
// # Why this package exists
//
// The native runtime can put only text in front of a model. Before this
// package, elitea-main served an attachment to the runtime only when its raw
// bytes were valid UTF-8 and at most 128 KiB, so a PDF, a DOCX or any document
// larger than 128 KiB reached the model as its file name alone. This package
// reads those formats once, in elitea-main, so every runtime gets the same text.
//
// # What it reads
//
//   - PDF through PDFium compiled to WebAssembly (go-pdfium, webassembly mode,
//     run by wazero). It needs no cgo, PDFium is BSD-3 / Apache-2 licensed, and
//     the parser runs inside a WebAssembly sandbox with a memory ceiling, so a
//     hostile file cannot corrupt the Go heap or crash the process.
//   - DOCX and PPTX with archive/zip and encoding/xml.
//   - XLSX with excelize.
//   - UTF-8 and UTF-16 text (TXT, MD, CSV, JSON, code) as it is.
//
// The format comes from the file's BYTES (magic numbers and the ZIP
// directory), never from the upload's media type or file name: the browser
// sets those, and they are wrong often enough to matter.
//
// # What it refuses, and the limits
//
// Every refusal is an *Error with a stable Reason that a worker can turn into
// an honest sentence for the model. The limits follow the attachment research
// (section D): at most 25 MiB in, at most 2,000 PDF pages, at most 2,000 ZIP
// entries, 200 MB uncompressed, a 100x compression ratio, no XML DTD or entity
// declarations, at most 1,000,000 spreadsheet cells, and at most 20 MiB of text
// out. A document that reaches a page, cell or text limit is NOT cut silently:
// the Document is marked Partial and says how many units it covers.
package extract

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf16"
	"unicode/utf8"
)

// Version is recorded with every stored extraction. A cached extraction made
// by a different version is extracted again, so a fix to an extractor reaches
// documents uploaded before it.
const Version = "elitea-extract/1"

// Format is the detected document format.
type Format string

const (
	FormatPDF  Format = "pdf"
	FormatDOCX Format = "docx"
	FormatPPTX Format = "pptx"
	FormatXLSX Format = "xlsx"
	FormatText Format = "text"
)

// UnitKind names what one entry of Document.Units is.
type UnitKind string

const (
	UnitPage    UnitKind = "page"
	UnitSlide   UnitKind = "slide"
	UnitSheet   UnitKind = "sheet"
	UnitSection UnitKind = "section"
	UnitPart    UnitKind = "part"
)

// Unit is one page, slide, sheet or section. Start and End are BYTE offsets
// into Document.Text (UTF-8), End exclusive. The separator between two units
// belongs to neither.
type Unit struct {
	Kind    UnitKind `json:"kind"`
	Label   string   `json:"label"`
	Start   int      `json:"start"`
	End     int      `json:"end"`
	HasText bool     `json:"has_text"`
}

// Reason is the stable, caller-safe cause of a refusal.
type Reason string

const (
	ReasonEmpty             Reason = "empty"
	ReasonTooLarge          Reason = "too_large"
	ReasonUnsupportedFormat Reason = "unsupported_format"
	ReasonEncrypted         Reason = "encrypted"
	ReasonMalformed         Reason = "malformed"
	ReasonUnsafeStructure   Reason = "unsafe_structure"
	ReasonNoText            Reason = "no_text"
	ReasonTimeout           Reason = "timeout"
)

// PartialReason says which limit stopped a Partial document.
type PartialReason string

const (
	PartialPageLimit PartialReason = "page_limit"
	PartialTextLimit PartialReason = "text_limit"
	PartialCellLimit PartialReason = "cell_limit"
)

// Error is a refusal. Its Reason is safe to send to a worker; the wrapped
// cause is for logs only.
type Error struct {
	Reason Reason
	cause  error
}

func (e *Error) Error() string {
	if e.cause != nil {
		return "extract: " + string(e.Reason) + ": " + e.cause.Error()
	}
	return "extract: " + string(e.Reason)
}

func (e *Error) Unwrap() error { return e.cause }

func refuse(reason Reason, cause error) *Error {
	return &Error{Reason: reason, cause: cause}
}

// ReasonOf returns the Reason of an *Error anywhere in err's chain.
func ReasonOf(err error) (Reason, bool) {
	var refusal *Error
	if errors.As(err, &refusal) {
		return refusal.Reason, true
	}
	return "", false
}

// ErrUnavailable is an extractor that cannot run at all (for example the
// PDF engine failed to start). It is a server fault, not a property of the
// file, so callers must not cache it as the file's outcome.
var ErrUnavailable = errors.New("extract: extractor unavailable")

// Document is one extraction.
type Document struct {
	Format Format
	// Text is normalised UTF-8: no NUL or other control characters except
	// newline and tab, no zero-width or bidirectional control characters,
	// and "\n" line ends.
	Text string
	// Units covers Text in order. For a Partial document it covers only the
	// units that were read; UnitCount is the source's own count.
	Units []Unit
	// UnitCount is how many pages, slides or sheets the SOURCE has.
	UnitCount int
	// LowTextUnits lists 1-based unit numbers with fewer than
	// lowTextThreshold characters of text. For a PDF that usually means a
	// scanned page with no text layer.
	LowTextUnits []int
	Partial      bool
	PartialBy    PartialReason
	// TokenEstimate is len(Text)/4 rounded up: the same bytes/4 heuristic
	// the worker's context budget uses.
	TokenEstimate    int64
	ExtractorVersion string
}

// Limits bounds one extraction. The zero value is not usable; use
// DefaultLimits.
type Limits struct {
	MaxInputBytes         int64
	MaxTextBytes          int
	MaxPages              int
	MaxUnits              int
	MaxZipEntries         int
	MaxZipUncompressed    uint64
	MaxZipRatio           uint64
	MaxXMLEntryBytes      int64
	MaxCells              int
	Timeout               time.Duration
	PDFMemoryLimitPages   uint32
	TextPartBytes         int
	LowTextUnitCharacters int
}

// DefaultLimits are the limits from the attachment research, section D.
func DefaultLimits() Limits {
	return Limits{
		MaxInputBytes: 25 << 20,
		MaxTextBytes:  20 << 20,
		MaxPages:      2_000,
		// Sections, slides, sheets and text parts. It bounds the unit map
		// that travels with the text (about 350 bytes per unit at most).
		MaxUnits:           4_000,
		MaxZipEntries:      2_000,
		MaxZipUncompressed: 200_000_000,
		MaxZipRatio:        100,
		// One XML part of an office file. Larger than any real document.xml
		// or sheet; the ZIP total still applies.
		MaxXMLEntryBytes: 64 << 20,
		MaxCells:         1_000_000,
		Timeout:          60 * time.Second,
		// 512 MiB of WebAssembly linear memory (64 KiB pages).
		PDFMemoryLimitPages:   8_192,
		TextPartBytes:         8 << 10,
		LowTextUnitCharacters: 50,
	}
}

// Extractor runs extractions under one set of limits. It is safe for
// concurrent use.
type Extractor struct {
	limits Limits
	pdf    *pdfEngine
}

// New returns an Extractor. The PDF engine starts lazily on the first PDF.
func New(limits Limits) *Extractor {
	return &Extractor{limits: limits, pdf: newPDFEngine(limits.PDFMemoryLimitPages)}
}

// Extract reads one document. The context deadline and Limits.Timeout both
// apply; the shorter one wins.
func (extractor *Extractor) Extract(ctx context.Context, data []byte) (doc Document, err error) {
	if extractor == nil {
		return Document{}, ErrUnavailable
	}
	if len(data) == 0 {
		return Document{}, refuse(ReasonEmpty, nil)
	}
	if int64(len(data)) > extractor.limits.MaxInputBytes {
		return Document{}, refuse(ReasonTooLarge, nil)
	}
	if extractor.limits.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, extractor.limits.Timeout)
		defer cancel()
	}
	// A parser bug must refuse one document, not take the process down.
	defer func() {
		if recovered := recover(); recovered != nil {
			doc = Document{}
			err = refuse(ReasonMalformed, fmt.Errorf("parser panic: %v", recovered))
		}
	}()
	builder := newTextBuilder(extractor.limits.MaxTextBytes, extractor.limits.MaxUnits)
	var format Format
	switch sniff(data) {
	case sniffPDF:
		format = FormatPDF
		err = extractor.pdf.extract(ctx, data, extractor.limits, builder)
	case sniffZIP:
		format, err = extractor.extractOOXML(ctx, data, builder)
	case sniffText:
		format = FormatText
		err = extractText(data, extractor.limits, builder)
	case sniffImage, sniffOLE, sniffBinary:
		return Document{}, refuse(ReasonUnsupportedFormat, nil)
	}
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			if errors.Is(ctxErr, context.DeadlineExceeded) {
				return Document{}, refuse(ReasonTimeout, ctxErr)
			}
			return Document{}, ctxErr
		}
		return Document{}, err
	}
	doc = builder.document(format, extractor.limits.LowTextUnitCharacters)
	if strings.TrimSpace(doc.Text) == "" {
		return Document{}, refuse(ReasonNoText, nil)
	}
	return doc, nil
}

type sniffResult int

const (
	sniffBinary sniffResult = iota
	sniffPDF
	sniffZIP
	sniffOLE
	sniffImage
	sniffText
)

func sniff(data []byte) sniffResult {
	head := data
	if len(head) > 1024 {
		head = head[:1024]
	}
	switch {
	case bytes.HasPrefix(data, []byte("%PDF-")):
		return sniffPDF
	// The PDF specification lets the header start anywhere in the first
	// 1024 bytes, and real files carry junk before it. A TEXT file that only
	// mentions "%PDF-" stays text.
	case bytes.Contains(head, []byte("%PDF-")) && !looksLikeText(data):
		return sniffPDF
	case bytes.HasPrefix(data, []byte("PK\x03\x04")):
		return sniffZIP
	// Compound File Binary: legacy .doc/.xls/.ppt, and also an ENCRYPTED
	// OOXML file, which is a CFB container around the encrypted package.
	case bytes.HasPrefix(data, []byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}):
		return sniffOLE
	case bytes.HasPrefix(data, []byte("\x89PNG\r\n\x1a\n")),
		bytes.HasPrefix(data, []byte{0xFF, 0xD8, 0xFF}),
		bytes.HasPrefix(data, []byte("GIF87a")), bytes.HasPrefix(data, []byte("GIF89a")),
		len(data) >= 12 && bytes.Equal(data[:4], []byte("RIFF")) && bytes.Equal(data[8:12], []byte("WEBP")):
		return sniffImage
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}), bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return sniffText
	case looksLikeText(data):
		return sniffText
	}
	return sniffBinary
}

// looksLikeText accepts valid UTF-8 with no NUL byte. A NUL in the first
// 8 KiB is the classic binary signal; UTF-16 without a BOM is not accepted.
func looksLikeText(data []byte) bool {
	probe := data
	if len(probe) > 8<<10 {
		probe = probe[:8<<10]
	}
	if bytes.IndexByte(probe, 0) >= 0 {
		return false
	}
	return utf8.Valid(data)
}

// textBuilder accumulates units into one normalised text. It stops at the
// text limit and records that, rather than dropping content silently.
type textBuilder struct {
	text      strings.Builder
	units     []Unit
	maxBytes  int
	maxUnits  int
	unitCount int
	partial   bool
	partialBy PartialReason
	// separator joins two units. A blank line for pages, slides, sheets and
	// sections; nothing for the parts of a text file, which are contiguous
	// slices of the same text.
	separator string
}

func newTextBuilder(maxBytes, maxUnits int) *textBuilder {
	return &textBuilder{maxBytes: maxBytes, maxUnits: maxUnits, separator: "\n\n"}
}

// full reports whether the text limit already stopped the build.
func (builder *textBuilder) full() bool {
	return builder.partial && builder.partialBy == PartialTextLimit
}

// stop marks the document partial for the given reason. The first reason
// wins.
func (builder *textBuilder) stop(reason PartialReason) {
	if !builder.partial {
		builder.partial = true
		builder.partialBy = reason
	}
}

// add appends one unit. It returns false when the text limit is reached; the
// unit is then not added.
func (builder *textBuilder) add(kind UnitKind, label, raw string) bool {
	if builder.full() {
		return false
	}
	if builder.maxUnits > 0 && len(builder.units) >= builder.maxUnits {
		builder.stop(PartialPageLimit)
		return false
	}
	body := normalize(raw)
	separator := ""
	if builder.text.Len() > 0 {
		separator = builder.separator
	}
	if builder.text.Len()+len(separator)+len(body) > builder.maxBytes {
		builder.stop(PartialTextLimit)
		return false
	}
	builder.text.WriteString(separator)
	start := builder.text.Len()
	builder.text.WriteString(body)
	builder.units = append(builder.units, Unit{
		Kind:    kind,
		Label:   label,
		Start:   start,
		End:     builder.text.Len(),
		HasText: strings.TrimSpace(body) != "",
	})
	return true
}

func (builder *textBuilder) document(format Format, lowText int) Document {
	text := builder.text.String()
	count := builder.unitCount
	if count < len(builder.units) {
		count = len(builder.units)
	}
	var low []int
	for index, unit := range builder.units {
		if utf8.RuneCountInString(strings.TrimSpace(text[unit.Start:unit.End])) < lowText {
			low = append(low, index+1)
		}
	}
	return Document{
		Format:           format,
		Text:             text,
		Units:            builder.units,
		UnitCount:        count,
		LowTextUnits:     low,
		Partial:          builder.partial,
		PartialBy:        builder.partialBy,
		TokenEstimate:    (int64(len(text)) + 3) / 4,
		ExtractorVersion: Version,
	}
}

// normalize makes extracted text safe to store and to show a model:
//
//   - invalid UTF-8 becomes U+FFFD;
//   - "\r\n" and "\r" become "\n";
//   - NUL and other C0/C1 control characters except "\n" and "\t" are
//     removed (PostgreSQL text cannot hold NUL at all);
//   - zero-width and bidirectional control characters are removed. They are
//     the usual way to hide an instruction from the human who reads a file
//     while the model still reads it.
func normalize(raw string) string {
	raw = strings.ToValidUTF8(raw, "\uFFFD")
	raw = strings.ReplaceAll(raw, "\r\n", "\n")
	var out strings.Builder
	out.Grow(len(raw))
	for _, character := range raw {
		switch {
		case character == '\r':
			out.WriteByte('\n')
		case character == '\n' || character == '\t':
			out.WriteRune(character)
		case unicode.IsControl(character):
		case hiddenCharacter(character):
		default:
			out.WriteRune(character)
		}
	}
	return out.String()
}

func hiddenCharacter(character rune) bool {
	switch {
	case character >= 0x200B && character <= 0x200F, // zero-width, LRM, RLM
		character >= 0x202A && character <= 0x202E, // bidi embeddings and overrides
		character >= 0x2060 && character <= 0x2064, // word joiner and invisible operators
		character >= 0x2066 && character <= 0x2069, // bidi isolates
		character == 0x061C,                        // Arabic letter mark
		character == 0xFEFF:                        // BOM / zero-width no-break space
		return true
	}
	return false
}

// extractText splits a text file into parts of about TextPartBytes at line
// boundaries, so that the worker can page through a long text file in the
// same way as through a PDF.
func extractText(data []byte, limits Limits, builder *textBuilder) error {
	text, err := decodeText(data)
	if err != nil {
		return err
	}
	builder.separator = ""
	part := 0
	for len(text) > 0 {
		cut := len(text)
		if cut > limits.TextPartBytes {
			cut = limits.TextPartBytes
			if newline := strings.LastIndexByte(text[:cut], '\n'); newline > 0 {
				cut = newline + 1
			} else {
				for cut > 0 && !utf8.RuneStart(text[cut]) {
					cut--
				}
			}
		}
		part++
		builder.unitCount = part
		if !builder.add(UnitPart, strconv.Itoa(part), text[:cut]) {
			// Count the remaining parts so the worker can say how much is
			// missing.
			rest := len(text) - cut
			builder.unitCount = part + (rest+limits.TextPartBytes-1)/limits.TextPartBytes
			return nil
		}
		text = text[cut:]
	}
	return nil
}

func decodeText(data []byte) (string, error) {
	switch {
	case bytes.HasPrefix(data, []byte{0xEF, 0xBB, 0xBF}):
		return string(data[3:]), nil
	case bytes.HasPrefix(data, []byte{0xFF, 0xFE}):
		return decodeUTF16(data[2:], false)
	case bytes.HasPrefix(data, []byte{0xFE, 0xFF}):
		return decodeUTF16(data[2:], true)
	}
	return string(data), nil
}

func decodeUTF16(data []byte, bigEndian bool) (string, error) {
	if len(data)%2 != 0 {
		return "", refuse(ReasonMalformed, errors.New("odd UTF-16 length"))
	}
	units := make([]uint16, len(data)/2)
	for index := range units {
		high, low := data[2*index], data[2*index+1]
		if bigEndian {
			units[index] = uint16(high)<<8 | uint16(low)
		} else {
			units[index] = uint16(low)<<8 | uint16(high)
		}
	}
	return string(utf16.Decode(units)), nil
}
