package extract

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// Workbook parts, written as a spreadsheet application writes them.
const (
	testWorkbookRels = `<?xml version="1.0" encoding="UTF-8"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
<Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
</Relationships>`
	testStyles = `<?xml version="1.0" encoding="UTF-8"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
<numFmts count="1"><numFmt numFmtId="164" formatCode="yyyy\-mm\-dd&quot; at &quot;hh:mm"/></numFmts>
<cellXfs count="4"><xf numFmtId="0"/><xf numFmtId="14"/><xf numFmtId="9"/><xf numFmtId="164"/></cellXfs>
</styleSheet>`
)

func testWorkbook(sheetName string) string {
	return `<?xml version="1.0" encoding="UTF-8"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<sheets><sheet name="` + sheetName + `" sheetId="1" r:id="rId1"/></sheets></workbook>`
}

func xlsxOf(t *testing.T, sheetName string, shared []string, sheetData string) []byte {
	t.Helper()
	var strings_ strings.Builder
	strings_.WriteString(`<?xml version="1.0" encoding="UTF-8"?><sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">`)
	for _, item := range shared {
		strings_.WriteString("<si><t>" + item + "</t></si>")
	}
	strings_.WriteString("</sst>")
	return zipOf(t, map[string][]byte{
		"xl/workbook.xml":            []byte(testWorkbook(sheetName)),
		"xl/_rels/workbook.xml.rels": []byte(testWorkbookRels),
		"xl/sharedStrings.xml":       []byte(strings_.String()),
		"xl/styles.xml":              []byte(testStyles),
		"xl/worksheets/sheet1.xml": []byte(`<?xml version="1.0" encoding="UTF-8"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"><sheetData>` +
			sheetData + `</sheetData></worksheet>`),
	})
}

func docxOf(t *testing.T, body string) []byte {
	t.Helper()
	return zipOf(t, map[string][]byte{"word/document.xml": []byte(
		`<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body>` +
			body + `</w:body></w:document>`,
	)})
}

func pptxOf(t *testing.T, slideIDs []string, slides map[string]string) []byte {
	t.Helper()
	var list strings.Builder
	for index, id := range slideIDs {
		fmt.Fprintf(&list, `<p:sldId id="%d" r:id="%s"/>`, 256+index, id)
	}
	files := map[string][]byte{
		"ppt/presentation.xml": []byte(`<p:presentation xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships"><p:sldIdLst>` +
			list.String() + `</p:sldIdLst></p:presentation>`),
	}
	var rels strings.Builder
	rels.WriteString(`<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">`)
	index := 0
	for id, text := range slides {
		index++
		name := "slide" + strconv.Itoa(index) + ".xml"
		fmt.Fprintf(&rels, `<Relationship Id="%s" Target="slides/%s"/>`, id, name)
		files["ppt/slides/"+name] = []byte(`<p:sld xmlns:p="http://schemas.openxmlformats.org/presentationml/2006/main" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main"><a:p><a:r><a:t>` +
			text + `</a:t></a:r></a:p></p:sld>`)
	}
	rels.WriteString(`</Relationships>`)
	files["ppt/_rels/presentation.xml.rels"] = []byte(rels.String())
	return zipOf(t, files)
}

// A short unit is short, not scanned. Only a PDF page is flagged.
func TestLowTextIsOnlyForPDFPages(t *testing.T) {
	t.Parallel()
	extractor := New(DefaultLimits())

	t.Run("a short text file", func(t *testing.T) {
		t.Parallel()
		doc, err := extractor.Extract(context.Background(), []byte("The invoice total is 42 EUR."))
		require.NoError(t, err)
		assert.Empty(t, doc.LowTextUnits)
	})

	t.Run("a title-only slide", func(t *testing.T) {
		t.Parallel()
		doc, err := extractor.Extract(context.Background(), pptxOf(t, []string{"rId1"}, map[string]string{"rId1": "Q3 results"}))
		require.NoError(t, err)
		require.Len(t, doc.Units, 1)
		assert.Empty(t, doc.LowTextUnits)
	})

	t.Run("a heading-only docx section and a small sheet", func(t *testing.T) {
		t.Parallel()
		for _, data := range [][]byte{
			docxOf(t, `<w:p><w:pPr><w:pStyle w:val="Heading1"/></w:pPr><w:r><w:t>Scope</w:t></w:r></w:p>`),
			xlsxOf(t, "Sheet1", []string{"total"}, `<row r="1"><c r="A1" t="s"><v>0</v></c><c r="B1"><v>240</v></c></row>`),
			fixture(t, "sheet.xlsx"),
			fixture(t, "notes.docx"),
			fixture(t, "deck.pptx"),
		} {
			doc, err := extractor.Extract(context.Background(), data)
			require.NoError(t, err)
			assert.Empty(t, doc.LowTextUnits, "format %s", doc.Format)
		}
	})

	t.Run("an image-only pdf page is still flagged", func(t *testing.T) {
		t.Parallel()
		doc, err := extractor.Extract(context.Background(), fixture(t, "report.pdf"))
		require.NoError(t, err)
		assert.Equal(t, []int{3}, doc.LowTextUnits)
	})
}

// A first unit larger than the text limit is served cut and partial, never
// refused as a file with no text.
func TestFirstUnitOverTheTextLimitIsPartial(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits()
	limits.MaxTextBytes = 1_000

	t.Run("one docx section", func(t *testing.T) {
		t.Parallel()
		var body strings.Builder
		for index := range 200 {
			fmt.Fprintf(&body, `<w:p><w:r><w:t>paragraph %d with words</w:t></w:r></w:p>`, index)
		}
		doc, err := New(limits).Extract(context.Background(), docxOf(t, body.String()))
		require.NoError(t, err)
		assert.True(t, doc.Partial)
		assert.Equal(t, PartialTextLimit, doc.PartialBy)
		assert.LessOrEqual(t, len(doc.Text), limits.MaxTextBytes)
		assert.Contains(t, doc.Text, "paragraph 0 with words")
		require.Len(t, doc.Units, 1)
	})

	t.Run("one xlsx sheet", func(t *testing.T) {
		t.Parallel()
		var rows strings.Builder
		for index := range 500 {
			fmt.Fprintf(&rows, `<row r="%d"><c r="A%d" t="s"><v>0</v></c></row>`, index+1, index+1)
		}
		doc, err := New(limits).Extract(context.Background(), xlsxOf(t, "Data", []string{"a cell value"}, rows.String()))
		require.NoError(t, err)
		assert.True(t, doc.Partial)
		assert.Equal(t, PartialTextLimit, doc.PartialBy)
		assert.LessOrEqual(t, len(doc.Text), limits.MaxTextBytes)
		assert.True(t, strings.HasPrefix(doc.Text, "a cell value\na cell value\n"))
	})

	t.Run("one text line with no line end", func(t *testing.T) {
		t.Parallel()
		textLimits := limits
		textLimits.TextPartBytes = 4_000
		doc, err := New(textLimits).Extract(context.Background(), []byte(strings.Repeat("é", 1_500)))
		require.NoError(t, err)
		assert.Equal(t, PartialTextLimit, doc.PartialBy)
		assert.LessOrEqual(t, len(doc.Text), limits.MaxTextBytes)
		assert.True(t, strings.HasPrefix(doc.Text, "é"))
	})
}

func TestPDFPartialReasonIsWhatStoppedTheRead(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits()
	limits.MaxPages = 5
	page := func(int) (string, error) { return strings.Repeat("p", 40), nil }

	t.Run("text limit before the page limit", func(t *testing.T) {
		t.Parallel()
		builder := newTextBuilder(100, limits.MaxUnits)
		require.NoError(t, readPages(context.Background(), 20, limits, builder, page))
		doc := builder.document(FormatPDF, limits.LowTextUnitCharacters)
		assert.Equal(t, PartialTextLimit, doc.PartialBy)
		assert.Equal(t, 20, doc.UnitCount)
	})

	t.Run("page limit when every allowed page fits", func(t *testing.T) {
		t.Parallel()
		builder := newTextBuilder(1<<20, limits.MaxUnits)
		require.NoError(t, readPages(context.Background(), 20, limits, builder, page))
		doc := builder.document(FormatPDF, limits.LowTextUnitCharacters)
		assert.Equal(t, PartialPageLimit, doc.PartialBy)
		assert.Len(t, doc.Units, 5)
	})

	t.Run("no partial when every page is read", func(t *testing.T) {
		t.Parallel()
		builder := newTextBuilder(1<<20, limits.MaxUnits)
		require.NoError(t, readPages(context.Background(), 3, limits, builder, page))
		assert.False(t, builder.partial)
	})
}

func TestDOCXTabStopDefinitionsAreNotText(t *testing.T) {
	t.Parallel()
	body := `<w:p><w:pPr><w:tabs><w:tab w:val="left" w:pos="720"/><w:tab w:val="right" w:pos="9000"/></w:tabs></w:pPr>` +
		`<w:r><w:t>Name</w:t></w:r><w:r><w:tab/><w:t>Value</w:t></w:r></w:p>` +
		`<w:tbl><w:tr><w:tc><w:p><w:pPr><w:tabs><w:tab w:val="left" w:pos="720"/></w:tabs></w:pPr><w:r><w:t>a</w:t></w:r></w:p></w:tc>` +
		`<w:tc><w:p><w:r><w:t>b</w:t></w:r></w:p></w:tc></w:tr></w:tbl>`
	doc, err := New(DefaultLimits()).Extract(context.Background(), docxOf(t, body))
	require.NoError(t, err)
	assert.Equal(t, "Name\tValue\na\tb", doc.Text)
}

func TestPPTXRepeatedSlideIsReadOnce(t *testing.T) {
	t.Parallel()
	ids := make([]string, 50)
	for index := range ids {
		ids[index] = "rId1"
	}
	ids = append(ids, "rId2")
	doc, err := New(DefaultLimits()).Extract(context.Background(), pptxOf(t, ids, map[string]string{
		"rId1": "only once", "rId2": "second",
	}))
	require.NoError(t, err)
	assert.Equal(t, 2, doc.UnitCount)
	assert.Equal(t, 1, strings.Count(doc.Text, "only once"))
}

func TestXLSXCellValues(t *testing.T) {
	t.Parallel()
	sheet := `<row r="1"><c r="A1" t="s"><v>0</v></c><c r="C1" t="inlineStr"><is><t>inline</t></is></c></row>` +
		`<row r="3"><c r="A3" s="1"><v>45292</v></c><c r="B3" s="2"><v>0.25</v></c><c r="C3" s="3"><v>45292.5</v></c>` +
		`<c r="D3" t="b"><v>1</v></c><c r="E3"><v>0.30000000000000004</v></c></row>`
	doc, err := New(DefaultLimits()).Extract(context.Background(), xlsxOf(t, "Mixed", []string{"line one\nline two"}, sheet))
	require.NoError(t, err)
	assert.Equal(t, "line one line two\t\tinline\n\n2024-01-01\t25%\t2024-01-01 12:00:00\tTRUE\t0.3", doc.Text)
}

func TestXLSXSheetNameIsNormalisedAndBounded(t *testing.T) {
	t.Parallel()
	name := "Q3‮" + strings.Repeat("long name ", 100)
	doc, err := New(DefaultLimits()).Extract(context.Background(), xlsxOf(t, name, []string{"x"}, `<row r="1"><c r="A1" t="s"><v>0</v></c></row>`))
	require.NoError(t, err)
	require.Len(t, doc.Units, 1)
	label := doc.Units[0].Label
	assert.NotContains(t, label, "‮")
	assert.LessOrEqual(t, len([]rune(label)), 81)
	assert.Less(t, len(label), 512, "the worker refuses a label over 512 bytes")
}

// One shared string named by many cells must not expand in memory. Before
// the reader held each shared string once and wrote under the budget, a
// 240 KB workbook made the extractor allocate gigabytes.
//
// Not parallel: it reads process-wide allocation counters.
func TestXLSXSharedStringAmplificationIsBounded(t *testing.T) {
	random := make([]byte, 16<<10)
	_, err := rand.Read(random)
	require.NoError(t, err)
	long := hex.EncodeToString(random) // 32 KiB that does not compress
	const cells = 20_000
	var sheet strings.Builder
	for row := range cells / 10 {
		fmt.Fprintf(&sheet, `<row r="%d">`, row+1)
		for column := range 10 {
			fmt.Fprintf(&sheet, `<c r="%c%d" t="s"><v>0</v></c>`, 'A'+column, row+1)
		}
		sheet.WriteString(`</row>`)
	}
	data := xlsxOf(t, "Bomb", []string{long}, sheet.String())
	require.Less(t, len(data), 1<<20, "the file itself is small")

	limits := DefaultLimits()
	limits.MaxTextBytes = 2 << 20
	extractor := New(limits)
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	doc, err := extractor.Extract(context.Background(), data)
	runtime.ReadMemStats(&after)
	require.NoError(t, err)
	assert.True(t, doc.Partial)
	assert.Equal(t, PartialTextLimit, doc.PartialBy)
	assert.LessOrEqual(t, len(doc.Text), limits.MaxTextBytes)
	assert.Greater(t, len(doc.Text), limits.MaxTextBytes/2, "the sheet is served up to the limit")
	// The expansion is 20,000 x 32 KiB = 640 MiB. The extraction may hold
	// the file, the parts and a few copies of the text, nothing more.
	allocated := after.TotalAlloc - before.TotalAlloc
	assert.Less(t, allocated, uint64(12*limits.MaxTextBytes+8*len(data)),
		"allocated %d bytes for a %d-byte text limit", allocated, limits.MaxTextBytes)
}

func TestPeakMemoryBytesCountsOnePDFAtATime(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits()
	one := PeakMemoryBytes(limits, 1)
	two := PeakMemoryBytes(limits, 2)
	assert.Greater(t, two, one)
	assert.Less(t, two, 2*one, "a second extraction is not a second PDF engine")
	assert.Equal(t, 1, PDFEngineConcurrency)
}

func TestColumnIndex(t *testing.T) {
	t.Parallel()
	for reference, want := range map[string]int{"A1": 0, "B7": 1, "Z3": 25, "AA1": 26, "XFD1": 16_383, "1": -1, "ABCD1": -1} {
		assert.Equal(t, want, columnIndex(reference), reference)
	}
}

func TestFormatCodeStyle(t *testing.T) {
	t.Parallel()
	assert.Equal(t, styleDate, formatCodeStyle(`dd/mm/yyyy`))
	assert.Equal(t, styleDateTime, formatCodeStyle(`yyyy-mm-dd hh:mm`))
	assert.Equal(t, styleTime, formatCodeStyle(`[h]:mm:ss`))
	assert.Equal(t, stylePercent, formatCodeStyle(`0.00%`))
	assert.Equal(t, styleGeneral, formatCodeStyle(`#,##0 "days"`), "quoted text is not a token")
	assert.Equal(t, styleGeneral, formatCodeStyle(`[Red]0.00`))
}

var errPage = errors.New("broken page")

func TestBrokenPageIsAPageWithoutText(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits()
	builder := newTextBuilder(1<<20, limits.MaxUnits)
	require.NoError(t, readPages(context.Background(), 2, limits, builder, func(index int) (string, error) {
		if index == 0 {
			return "", errPage
		}
		return strings.Repeat("second page text ", 5), nil
	}))
	doc := builder.document(FormatPDF, limits.LowTextUnitCharacters)
	require.Len(t, doc.Units, 2)
	assert.Equal(t, []int{1}, doc.LowTextUnits)
}
