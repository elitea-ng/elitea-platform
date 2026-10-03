package extract

import (
	"context"
	"encoding/xml"
	"errors"
	"math"
	"path"
	"strconv"
	"strings"
	"time"
)

// extractXLSX reads every worksheet as tab-separated rows. One sheet is one
// unit, labelled with the sheet name. A workbook past the cell limit is marked
// Partial at the sheet where the limit was reached.
//
// THE READER IS OUR OWN, and the reason is amplification. A cell names a
// shared string by index, so one 32 KiB string that 20,000 cells name is a
// 640 MB sheet inside a 240 KB file. A general spreadsheet library copies
// the string for every cell, and it builds a whole row before a caller can
// count it. Here each shared string is held ONCE, a cell is appended only
// while the sheet is under the text that is left, and the row stops at that
// point. So memory follows the file size and the text limit, not the
// expansion.
//
// Every part goes through zipParts.readXML (size limits, DTD refusal) and
// tokens (directive refusal), the same as DOCX and PPTX.
func extractXLSX(
	ctx context.Context,
	parts *zipParts,
	limits Limits,
	builder *textBuilder,
) error {
	book, err := readWorkbook(parts)
	if err != nil {
		return err
	}
	shared, err := readSharedStrings(ctx, parts)
	if err != nil {
		return err
	}
	styles, err := readCellStyles(parts)
	if err != nil {
		return err
	}
	builder.unitCount = len(book.sheets)
	cells := 0
	for index, sheet := range book.sheets {
		if err := ctx.Err(); err != nil {
			return err
		}
		reader := sheetReader{
			shared:   shared,
			styles:   styles,
			date1904: book.date1904,
			budget:   builder.remaining(),
			maxCells: limits.MaxCells,
			cells:    &cells,
		}
		text, err := reader.read(ctx, parts, sheet.part)
		if err != nil {
			return err
		}
		label := shortLabel(sheet.name)
		if label == "" {
			label = "Sheet " + strconv.Itoa(index+1)
		}
		if !builder.add(UnitSheet, label, text) {
			return nil
		}
		if reader.overText {
			builder.stop(PartialTextLimit)
			return nil
		}
		if reader.overCells {
			builder.stop(PartialCellLimit)
			return nil
		}
	}
	return nil
}

type workbookSheet struct {
	name string
	// part is the worksheet's ZIP entry name, or "" when the relationship
	// does not resolve (a chartsheet, a dangling id): the sheet is then
	// listed with no text.
	part string
}

type workbook struct {
	sheets   []workbookSheet
	date1904 bool
}

// readWorkbook lists the sheets in workbook order, resolving each r:id
// through xl/_rels/workbook.xml.rels.
func readWorkbook(parts *zipParts) (workbook, error) {
	content, err := parts.readXML("xl/workbook.xml")
	if err != nil {
		return workbook{}, err
	}
	relationships, err := parts.readXML("xl/_rels/workbook.xml.rels")
	if err != nil {
		return workbook{}, err
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
			if id == "" || target == "" {
				return nil
			}
			if strings.HasPrefix(target, "/") {
				targets[id] = path.Clean(strings.TrimPrefix(target, "/"))
			} else {
				targets[id] = path.Clean(path.Join("xl", target))
			}
			return nil
		})
		if err != nil {
			return workbook{}, err
		}
	}
	var book workbook
	err = tokens(content, func(token xml.Token) error {
		element, ok := token.(xml.StartElement)
		if !ok {
			return nil
		}
		switch element.Name.Local {
		case "workbookPr":
			for _, attribute := range element.Attr {
				if attribute.Name.Local == "date1904" {
					book.date1904 = attribute.Value == "1" || strings.EqualFold(attribute.Value, "true")
				}
			}
		case "sheet":
			var sheet workbookSheet
			for _, attribute := range element.Attr {
				switch {
				case attribute.Name.Local == "name":
					sheet.name = attribute.Value
				case attribute.Name.Local == "id" && attribute.Name.Space != "":
					if target := targets[attribute.Value]; parts.entries[target] != nil &&
						strings.HasPrefix(target, "xl/worksheets/") {
						sheet.part = target
					}
				}
			}
			book.sheets = append(book.sheets, sheet)
		}
		return nil
	})
	if err != nil {
		return workbook{}, err
	}
	return book, nil
}

// readSharedStrings returns each <si> as one string: its <t> runs joined,
// without the phonetic guide (<rPh>) that only East Asian input methods use.
// The strings are held once and referred to by every cell that names them.
func readSharedStrings(ctx context.Context, parts *zipParts) ([]string, error) {
	content, err := parts.readXML("xl/sharedStrings.xml")
	if err != nil || content == nil {
		return nil, err
	}
	var (
		shared   []string
		item     strings.Builder
		inItem   bool
		inText   bool
		phonetic int
	)
	err = tokens(content, func(token xml.Token) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "si":
				inItem = true
				item.Reset()
			case "rPh":
				phonetic++
			case "t":
				inText = inItem && phonetic == 0
			}
		case xml.EndElement:
			switch element.Name.Local {
			case "si":
				// Made one-line once here, not once per cell that names it.
				shared = append(shared, oneLine(item.String()))
				inItem = false
			case "rPh":
				if phonetic > 0 {
					phonetic--
				}
			case "t":
				inText = false
			}
		case xml.CharData:
			if inText {
				item.Write(element)
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return shared, nil
}

// cellStyle is what a cell's number format means for its text.
type cellStyle uint8

const (
	styleGeneral cellStyle = iota
	styleDate
	styleDateTime
	styleTime
	stylePercent
)

// readCellStyles maps each cellXfs index to how its number format reads.
// Only dates, times and percentages change the text: a date is stored as a
// serial number, and "45292" is not a date to a reader.
func readCellStyles(parts *zipParts) ([]cellStyle, error) {
	content, err := parts.readXML("xl/styles.xml")
	if err != nil || content == nil {
		return nil, err
	}
	custom := map[int]string{}
	var formatIDs []int
	inCellXfs := false
	err = tokens(content, func(token xml.Token) error {
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "numFmt":
				var id int
				var code string
				for _, attribute := range element.Attr {
					switch attribute.Name.Local {
					case "numFmtId":
						id, _ = strconv.Atoi(attribute.Value)
					case "formatCode":
						code = attribute.Value
					}
				}
				custom[id] = code
			case "cellXfs":
				inCellXfs = true
			case "xf":
				if inCellXfs {
					id := 0
					for _, attribute := range element.Attr {
						if attribute.Name.Local == "numFmtId" {
							id, _ = strconv.Atoi(attribute.Value)
						}
					}
					formatIDs = append(formatIDs, id)
				}
			}
		case xml.EndElement:
			if element.Name.Local == "cellXfs" {
				inCellXfs = false
			}
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	styles := make([]cellStyle, len(formatIDs))
	for index, id := range formatIDs {
		if code, found := custom[id]; found {
			styles[index] = formatCodeStyle(code)
		} else {
			styles[index] = builtinFormatStyle(id)
		}
	}
	return styles, nil
}

// builtinFormatStyle classifies the ECMA-376 built-in number formats.
func builtinFormatStyle(id int) cellStyle {
	switch {
	case id == 9 || id == 10:
		return stylePercent
	case id >= 14 && id <= 17, id >= 27 && id <= 31, id >= 34 && id <= 36, id >= 50 && id <= 58:
		return styleDate
	case id == 22:
		return styleDateTime
	case id >= 18 && id <= 21, id == 32, id == 33, id >= 45 && id <= 47:
		return styleTime
	}
	return styleGeneral
}

// formatCodeStyle classifies a custom format code. Quoted text, escaped
// characters and [bracketed] colour or locale sections are not format
// tokens, so they are removed before the test.
func formatCodeStyle(code string) cellStyle {
	var plain strings.Builder
	quoted, bracketed := false, false
	for index := 0; index < len(code); index++ {
		character := code[index]
		switch {
		case quoted:
			quoted = character != '"'
		case bracketed:
			bracketed = character != ']'
		case character == '"':
			quoted = true
		case character == '[':
			bracketed = true
		case character == '\\' || character == '_' || character == '*':
			index++
		default:
			plain.WriteByte(character)
		}
	}
	tokens := strings.ToLower(plain.String())
	// Only the first section (positive numbers) decides.
	if section, _, found := strings.Cut(tokens, ";"); found {
		tokens = section
	}
	date := strings.ContainsAny(tokens, "yd")
	clock := strings.ContainsAny(tokens, "hs")
	switch {
	case date && clock:
		return styleDateTime
	case date:
		return styleDate
	case clock:
		return styleTime
	case strings.Contains(tokens, "%"):
		return stylePercent
	}
	return styleGeneral
}

// sheetReader writes one worksheet as tab-separated rows, under a byte
// budget and the workbook's cell limit.
type sheetReader struct {
	shared   []string
	styles   []cellStyle
	date1904 bool
	// budget is the text the builder still accepts. Past it the sheet
	// stops, and overText is set.
	budget   int
	maxCells int
	cells    *int

	text      strings.Builder
	overText  bool
	overCells bool
}

// errSheetFull ends the token walk once the sheet has stopped.
var errSheetFull = errors.New("sheet full")

func (reader *sheetReader) read(ctx context.Context, parts *zipParts, part string) (string, error) {
	if part == "" {
		return "", nil
	}
	content, err := parts.readXML(part)
	if err != nil || content == nil {
		return "", err
	}
	var (
		row          strings.Builder
		tabsWritten  int
		rowNumber    int
		column       int
		cellType     string
		cellStyleID  int
		cellColumn   int
		value        strings.Builder
		inValue      bool
		inInline     bool
		inCell       bool
		pendingBlank int
	)
	endRow := func() error {
		line := strings.TrimRight(row.String(), "\t")
		row.Reset()
		tabsWritten = 0
		if line == "" {
			pendingBlank++
			return nil
		}
		// A row with no text is a blank line, so a row's line in the text
		// matches its row number in the sheet.
		breaks := pendingBlank
		if reader.text.Len() > 0 {
			breaks++
		}
		pendingBlank = 0
		if !reader.write(strings.Repeat("\n", min(breaks, reader.room()+1))) {
			return errSheetFull
		}
		if !reader.write(line) {
			return errSheetFull
		}
		return nil
	}
	err = tokens(content, func(token xml.Token) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		switch element := token.(type) {
		case xml.StartElement:
			switch element.Name.Local {
			case "row":
				number := attributeInt(element.Attr, "r")
				if number <= rowNumber {
					number = rowNumber + 1
				}
				// Rows the sheet skips are blank lines.
				pendingBlank += min(number-rowNumber-1, maxBlankRows)
				rowNumber = number
				column = 0
			case "c":
				inCell = true
				cellType, cellStyleID, cellColumn = "", 0, column
				for _, attribute := range element.Attr {
					switch attribute.Name.Local {
					case "t":
						cellType = attribute.Value
					case "s":
						cellStyleID, _ = strconv.Atoi(attribute.Value)
					case "r":
						if index := columnIndex(attribute.Value); index >= 0 {
							cellColumn = index
						}
					}
				}
				value.Reset()
			case "v":
				inValue = inCell
			case "is":
				inInline = inCell
			case "t":
				inValue = inInline
			}
		case xml.EndElement:
			switch element.Name.Local {
			case "v":
				inValue = false
			case "t":
				if inInline {
					inValue = false
				}
			case "is":
				inInline = false
			case "c":
				inCell = false
				text := reader.cellText(cellType, cellStyleID, value.String())
				if text == "" {
					column = cellColumn + 1
					return nil
				}
				if *reader.cells >= reader.maxCells {
					reader.overCells = true
					return errSheetFull
				}
				*reader.cells++
				// Column c is the field after c tabs. The row is cut at the
				// budget as it grows, so a row never holds more than the
				// sheet can still accept.
				room := reader.room() - row.Len() - 1
				if room <= 0 {
					reader.overText = true
					return errSheetFull
				}
				tabs := min(max(cellColumn-tabsWritten, 0), room)
				row.WriteString(strings.Repeat("\t", tabs))
				tabsWritten += tabs
				room -= tabs
				column = cellColumn + 1
				if room < len(text) {
					row.WriteString(cutText(text, room))
					reader.overText = true
					if endErr := endRow(); endErr != nil {
						return endErr
					}
					return errSheetFull
				}
				row.WriteString(text)
			case "row":
				return endRow()
			}
		case xml.CharData:
			if inValue && value.Len() <= reader.budget {
				value.Write(element)
			}
		}
		return nil
	})
	if err != nil && !errors.Is(err, errSheetFull) {
		return "", err
	}
	if row.Len() > 0 {
		_ = endRow()
	}
	return reader.text.String(), nil
}

// room is how many more bytes this sheet accepts.
func (reader *sheetReader) room() int {
	return max(reader.budget-reader.text.Len(), 0)
}

// write appends text or, when it does not fit, the part of it that does,
// and reports whether the whole text fitted.
func (reader *sheetReader) write(text string) bool {
	if room := reader.room(); len(text) > room {
		reader.text.WriteString(cutText(text, room))
		reader.overText = true
		return false
	}
	reader.text.WriteString(text)
	return true
}

// cellText is one cell's text. A shared string is referred to, not copied:
// the copy happens when it is written, under the budget.
func (reader *sheetReader) cellText(cellType string, styleID int, raw string) string {
	switch cellType {
	case "s":
		index, err := strconv.Atoi(strings.TrimSpace(raw))
		if err != nil || index < 0 || index >= len(reader.shared) {
			return ""
		}
		return reader.shared[index]
	case "inlineStr", "str", "e", "d":
		return oneLine(raw)
	case "b":
		switch strings.TrimSpace(raw) {
		case "1":
			return "TRUE"
		case "0":
			return "FALSE"
		}
		return raw
	}
	raw = strings.TrimSpace(raw)
	number, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return oneLine(raw)
	}
	style := styleGeneral
	if styleID >= 0 && styleID < len(reader.styles) {
		style = reader.styles[styleID]
	}
	switch style {
	case styleDate, styleDateTime, styleTime:
		if formatted, ok := serialTime(number, reader.date1904, style); ok {
			return formatted
		}
	case stylePercent:
		return formatNumber(number*100) + "%"
	}
	return formatNumber(number)
}

// oneLine keeps a cell on its row: a line break inside a cell becomes a
// space, and a tab becomes a space, so the tab-separated layout holds.
func oneLine(text string) string {
	if !strings.ContainsAny(text, "\t\r\n") {
		return text
	}
	return strings.NewReplacer("\r\n", " ", "\r", " ", "\n", " ", "\t", " ").Replace(text)
}

// formatNumber prints at most 15 significant digits, as a spreadsheet does,
// so 0.1+0.2 reads as 0.3.
func formatNumber(number float64) string {
	rounded, err := strconv.ParseFloat(strconv.FormatFloat(number, 'g', 15, 64), 64)
	if err != nil {
		rounded = number
	}
	return strconv.FormatFloat(rounded, 'f', -1, 64)
}

// maxDateSerial is 9999-12-31 in the 1900 date system.
const maxDateSerial = 2_958_465

// serialTime turns a date serial into ISO text: 2006-01-02, 15:04:05, or
// both.
func serialTime(serial float64, date1904 bool, style cellStyle) (string, bool) {
	if serial < 0 || serial > maxDateSerial || math.IsNaN(serial) {
		return "", false
	}
	base := time.Date(1899, time.December, 30, 0, 0, 0, 0, time.UTC)
	if date1904 {
		base = time.Date(1904, time.January, 1, 0, 0, 0, 0, time.UTC)
	} else if serial < 60 {
		// The 1900 system counts a 29 February 1900 that never existed;
		// serials before it are one day early from a 30 December base.
		serial++
	}
	days := math.Floor(serial)
	seconds := math.Round((serial - days) * 86_400)
	moment := base.AddDate(0, 0, int(days)).Add(time.Duration(seconds) * time.Second)
	switch style {
	case styleTime:
		return moment.Format("15:04:05"), true
	case styleDateTime:
		return moment.Format("2006-01-02 15:04:05"), true
	}
	return moment.Format("2006-01-02"), true
}

// columnIndex is the 0-based column of a cell reference such as "AB12", or
// -1 when it has no column letters.
func columnIndex(reference string) int {
	index := 0
	letters := 0
	for _, character := range reference {
		upper := character &^ 0x20
		if upper < 'A' || upper > 'Z' {
			break
		}
		index = index*26 + int(upper-'A'+1)
		letters++
		if letters > 3 {
			return -1
		}
	}
	if letters == 0 {
		return -1
	}
	return index - 1
}

func attributeInt(attributes []xml.Attr, name string) int {
	for _, attribute := range attributes {
		if attribute.Name.Local == name {
			value, err := strconv.Atoi(attribute.Value)
			if err == nil {
				return value
			}
		}
	}
	return 0
}

// maxBlankRows bounds the blank lines one gap between rows becomes. A row
// numbered 1,048,576 after row 1 is one far-away row, not a million lines.
const maxBlankRows = 1_000
