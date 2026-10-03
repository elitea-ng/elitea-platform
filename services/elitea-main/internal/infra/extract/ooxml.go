package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"path"
	"sort"
	"strconv"
	"strings"
)

// extractOOXML guards the ZIP container, then picks DOCX, PPTX or XLSX from
// the parts the package actually contains.
func (extractor *Extractor) extractOOXML(
	ctx context.Context,
	data []byte,
	builder *textBuilder,
) (Format, error) {
	archive, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		return "", refuse(ReasonMalformed, err)
	}
	if err := guardZip(archive, extractor.limits); err != nil {
		return "", err
	}
	entries := make(map[string]*zip.File, len(archive.File))
	for _, file := range archive.File {
		entries[file.Name] = file
	}
	reader := &zipParts{entries: entries, limits: extractor.limits}
	switch {
	case entries["word/document.xml"] != nil:
		return FormatDOCX, extractDOCX(ctx, reader, builder)
	case entries["ppt/presentation.xml"] != nil:
		return FormatPPTX, extractPPTX(ctx, reader, builder)
	case entries["xl/workbook.xml"] != nil:
		return FormatXLSX, extractXLSX(ctx, reader, extractor.limits, builder)
	}
	return "", refuse(ReasonUnsupportedFormat, errors.New("zip is not an office document"))
}

// guardZip applies the ZIP-bomb limits to the DECLARED sizes. zipParts.read
// applies them again to the bytes actually inflated, because a declared size
// is only a claim.
func guardZip(archive *zip.Reader, limits Limits) error {
	if len(archive.File) > limits.MaxZipEntries {
		return refuse(ReasonUnsafeStructure, fmt.Errorf("%d zip entries", len(archive.File)))
	}
	var total uint64
	for _, file := range archive.File {
		total += file.UncompressedSize64
		if total > limits.MaxZipUncompressed {
			return refuse(ReasonUnsafeStructure, errors.New("zip uncompressed size over limit"))
		}
		if file.UncompressedSize64 == 0 {
			continue
		}
		if file.CompressedSize64 == 0 ||
			file.UncompressedSize64/file.CompressedSize64 > limits.MaxZipRatio {
			return refuse(ReasonUnsafeStructure, fmt.Errorf("zip entry %q compression ratio over limit", file.Name))
		}
	}
	return nil
}

type zipParts struct {
	entries  map[string]*zip.File
	limits   Limits
	inflated uint64
}

// read inflates one part, bounded by its declared size, the per-part XML
// limit and the archive total.
func (parts *zipParts) read(name string) ([]byte, error) {
	file := parts.entries[name]
	if file == nil {
		return nil, nil
	}
	limit := int64(file.UncompressedSize64)
	if limit > parts.limits.MaxXMLEntryBytes {
		return nil, refuse(ReasonUnsafeStructure, fmt.Errorf("zip entry %q over part limit", name))
	}
	handle, err := file.Open()
	if err != nil {
		return nil, refuse(ReasonMalformed, err)
	}
	defer func() { _ = handle.Close() }()
	content, err := io.ReadAll(io.LimitReader(handle, limit+1))
	if err != nil {
		return nil, refuse(ReasonMalformed, err)
	}
	if int64(len(content)) > limit {
		return nil, refuse(ReasonUnsafeStructure, fmt.Errorf("zip entry %q inflates past its declared size", name))
	}
	parts.inflated += uint64(len(content))
	if parts.inflated > parts.limits.MaxZipUncompressed {
		return nil, refuse(ReasonUnsafeStructure, errors.New("zip inflated size over limit"))
	}
	return content, nil
}

// readXML reads one XML part and refuses any document type declaration.
// encoding/xml never expands a DTD's entities, but a file that declares them
// is either a billion-laughs attempt or not an Office file, and the next
// parser to touch these bytes may not be as careful.
func (parts *zipParts) readXML(name string) ([]byte, error) {
	content, err := parts.read(name)
	if err != nil || content == nil {
		return content, err
	}
	if err := rejectDTD(content); err != nil {
		return nil, err
	}
	return content, nil
}

func rejectDTD(content []byte) error {
	upper := bytes.ToUpper(content)
	if bytes.Contains(upper, []byte("<!DOCTYPE")) || bytes.Contains(upper, []byte("<!ENTITY")) {
		return refuse(ReasonUnsafeStructure, errors.New("xml declares a dtd or entity"))
	}
	return nil
}

func newDecoder(content []byte) *xml.Decoder {
	decoder := xml.NewDecoder(bytes.NewReader(content))
	decoder.Strict = true
	return decoder
}

// tokens walks every XML token and refuses any directive, so a DTD that
// slipped past the byte check still stops the parse.
func tokens(content []byte, visit func(xml.Token) error) error {
	decoder := newDecoder(content)
	for {
		token, err := decoder.Token()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return refuse(ReasonMalformed, err)
		}
		if _, directive := token.(xml.Directive); directive {
			return refuse(ReasonUnsafeStructure, errors.New("xml directive"))
		}
		if err := visit(token); err != nil {
			return err
		}
	}
}

// extractDOCX reads word/document.xml. Each heading paragraph (a paragraph
// style named "Heading..." or "Title") starts a new section unit, so the
// worker can show an outline and read one section at a time.
func extractDOCX(ctx context.Context, parts *zipParts, builder *textBuilder) error {
	content, err := parts.readXML("word/document.xml")
	if err != nil {
		return err
	}
	type section struct {
		label string
		body  strings.Builder
	}
	sections := []*section{{label: "Start"}}
	var paragraph, row strings.Builder
	heading := false
	inText := false
	// cellDepth > 0 inside a table cell. A cell's paragraphs join with a
	// space, cells with a tab, and a table row ends the line, so a table
	// reads as tab-separated rows.
	cellDepth := 0
	// tabsDepth > 0 inside <w:tabs>, the paragraph's tab-stop DEFINITIONS.
	// A <w:tab> there is a stop position, not a tab character; only a
	// <w:tab> in a run is text.
	tabsDepth := 0
	// collected bounds what the parse holds. The text limit applies when the
	// sections are added; past it nothing more is kept, so a document part
	// near its size limit does not also become a second copy in memory.
	budget := builder.remaining()
	collected := 0
	uncollected := 0
	over := func() bool { return collected > budget }
	flush := func() {
		text := paragraph.String()
		paragraph.Reset()
		if over() {
			// Still count the sections past the limit, so the worker can
			// say how many it did not read.
			if heading && cellDepth == 0 && strings.TrimSpace(text) != "" {
				uncollected++
			}
			heading = false
			return
		}
		collected += len(text) + 1
		if cellDepth > 0 {
			if row.Len() > 0 && !strings.HasSuffix(row.String(), "\t") && text != "" {
				row.WriteByte(' ')
			}
			row.WriteString(text)
			heading = false
			return
		}
		if heading && strings.TrimSpace(text) != "" {
			sections = append(sections, &section{label: shortLabel(text)})
		}
		current := sections[len(sections)-1]
		current.body.WriteString(text)
		current.body.WriteByte('\n')
		heading = false
	}
	err = tokens(content, func(token xml.Token) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "t":
				inText = true
			case "tc":
				cellDepth++
			case "tabs":
				tabsDepth++
			case "tab":
				if tabsDepth == 0 {
					paragraph.WriteByte('\t')
				}
			case "br", "cr":
				paragraph.WriteByte('\n')
			case "pStyle":
				for _, attribute := range element.Attr {
					if attribute.Name.Local == "val" {
						value := strings.ToLower(attribute.Value)
						heading = strings.HasPrefix(value, "heading") || value == "title"
					}
				}
			}
		case xml.EndElement:
			switch element.Name.Local {
			case "t":
				inText = false
			case "tabs":
				if tabsDepth > 0 {
					tabsDepth--
				}
			case "p":
				flush()
			case "tc":
				if cellDepth > 0 {
					cellDepth--
				}
				row.WriteByte('\t')
			case "tr":
				if !over() {
					current := sections[len(sections)-1]
					current.body.WriteString(strings.TrimRight(row.String(), "\t"))
					current.body.WriteByte('\n')
				}
				row.Reset()
			}
		case xml.CharData:
			if inText && !over() {
				paragraph.Write(element)
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if paragraph.Len() > 0 {
		flush()
	}
	if strings.TrimSpace(sections[0].body.String()) == "" && len(sections) > 1 {
		sections = sections[1:]
	}
	builder.unitCount = len(sections) + uncollected
	for _, current := range sections {
		if !builder.add(UnitSection, current.label, strings.TrimRight(current.body.String(), "\n")) {
			return nil
		}
	}
	return nil
}

// extractPPTX reads the slides in PRESENTATION order (presentation.xml's
// sldIdLst through its relationships), falling back to the slide number in
// the part name when the order cannot be read.
func extractPPTX(ctx context.Context, parts *zipParts, builder *textBuilder) error {
	slides, err := slideOrder(parts)
	if err != nil {
		return err
	}
	builder.unitCount = len(slides)
	for index, name := range slides {
		if err := ctx.Err(); err != nil {
			return err
		}
		content, err := parts.readXML(name)
		if err != nil {
			return err
		}
		var text strings.Builder
		inText := false
		err = tokens(content, func(token xml.Token) error {
			switch element := token.(type) {
			case xml.StartElement:
				switch element.Name.Local {
				case "t":
					inText = true
				case "br":
					text.WriteByte('\n')
				}
			case xml.EndElement:
				switch element.Name.Local {
				case "t":
					inText = false
				case "p":
					text.WriteByte('\n')
				}
			case xml.CharData:
				if inText {
					text.Write(element)
				}
			}
			return nil
		})
		if err != nil {
			return err
		}
		if !builder.add(UnitSlide, strconv.Itoa(index+1), strings.TrimRight(text.String(), "\n")) {
			return nil
		}
	}
	return nil
}

func slideOrder(parts *zipParts) ([]string, error) {
	presentation, err := parts.readXML("ppt/presentation.xml")
	if err != nil {
		return nil, err
	}
	relationships, err := parts.readXML("ppt/_rels/presentation.xml.rels")
	if err != nil {
		return nil, err
	}
	targets := map[string]string{}
	if relationships != nil {
		err = tokens(relationships, func(token xml.Token) error {
			element, ok := token.(xml.StartElement)
			if !ok || element.Name.Local != "Relationship" {
				return nil
			}
			var id, target string
			for _, attribute := range element.Attr {
				switch attribute.Name.Local {
				case "Id":
					id = attribute.Value
				case "Target":
					target = attribute.Value
				}
			}
			if id != "" && target != "" {
				targets[id] = path.Clean(path.Join("ppt", target))
			}
			return nil
		})
		if err != nil {
			return nil, err
		}
	}
	var ordered []string
	err = tokens(presentation, func(token xml.Token) error {
		element, ok := token.(xml.StartElement)
		if !ok || element.Name.Local != "sldId" {
			return nil
		}
		for _, attribute := range element.Attr {
			if attribute.Name.Local == "id" && attribute.Name.Space != "" {
				if target, found := targets[attribute.Value]; found && parts.entries[target] != nil {
					ordered = append(ordered, target)
				}
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if len(ordered) > 0 {
		return distinct(ordered), nil
	}
	for name := range parts.entries {
		if slideNumber(name) > 0 {
			ordered = append(ordered, name)
		}
	}
	sort.Slice(ordered, func(left, right int) bool {
		return slideNumber(ordered[left]) < slideNumber(ordered[right])
	})
	return ordered, nil
}

// distinct keeps the first occurrence of each slide part. A real deck lists
// each slide once; a crafted sldIdLst that names one large slide thousands
// of times would otherwise make every repeat a new read of the same part.
func distinct(names []string) []string {
	seen := make(map[string]struct{}, len(names))
	out := names[:0]
	for _, name := range names {
		if _, repeated := seen[name]; repeated {
			continue
		}
		seen[name] = struct{}{}
		out = append(out, name)
	}
	return out
}

func slideNumber(name string) int {
	const prefix, suffix = "ppt/slides/slide", ".xml"
	if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, suffix) {
		return 0
	}
	number, err := strconv.Atoi(strings.TrimSuffix(strings.TrimPrefix(name, prefix), suffix))
	if err != nil || number <= 0 {
		return 0
	}
	return number
}

// shortLabel makes a heading usable as a one-line outline label.
func shortLabel(text string) string {
	label := strings.Join(strings.Fields(normalize(text)), " ")
	const maxRunes = 80
	if runes := []rune(label); len(runes) > maxRunes {
		label = string(runes[:maxRunes]) + "\u2026"
	}
	return label
}
