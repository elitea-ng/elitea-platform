package extract

import (
	"archive/zip"
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The fixtures in testdata/ are real files written by reportlab, pypdf,
// python-docx, openpyxl and python-pptx (testdata/make_fixtures.py). They are
// small on purpose and each carries a FIXTURETOKEN* string that exists only
// inside the document body.

func fixture(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join("testdata", name))
	require.NoError(t, err)
	return data
}

func requireReason(t *testing.T, err error, want Reason) {
	t.Helper()
	require.Error(t, err)
	reason, ok := ReasonOf(err)
	require.Truef(t, ok, "error %v carries no reason", err)
	require.Equal(t, want, reason, "error: %v", err)
}

func unitText(doc Document, index int) string {
	unit := doc.Units[index]
	return doc.Text[unit.Start:unit.End]
}

func TestExtractFixtures(t *testing.T) {
	t.Parallel()
	extractor := New(DefaultLimits())
	cases := []struct {
		name      string
		file      string
		format    Format
		kind      UnitKind
		unitCount int
		labels    []string
		contains  []string
		lowText   []int
	}{
		{
			name: "pdf with an image-only page", file: "report.pdf", format: FormatPDF, kind: UnitPage,
			unitCount: 4, labels: []string{"1", "2", "3", "4"},
			contains: []string{"FIXTURETOKENPDF1", "FIXTURETOKENPDF2", "Fourth page closes"},
			// Page 3 is a filled rectangle with no text layer.
			lowText: []int{3},
		},
		{
			name: "docx sections from headings", file: "notes.docx", format: FormatDOCX, kind: UnitSection,
			unitCount: 2, labels: []string{"Introduction", "Details"},
			contains: []string{"FIXTURETOKENDOCX", "name\tvalue\nalpha\t42"},
		},
		{
			name: "xlsx one unit per sheet", file: "sheet.xlsx", format: FormatXLSX, kind: UnitSheet,
			unitCount: 3, labels: []string{"Sales", "Empty", "Totals"},
			contains: []string{"region\tamount", "south\tFIXTURETOKENXLSX", "total\t240"},
		},
		{
			name: "pptx slides in presentation order", file: "deck.pptx", format: FormatPPTX, kind: UnitSlide,
			unitCount: 2, labels: []string{"1", "2"},
			contains: []string{"Opening slide", "FIXTURETOKENPPTX1", "FIXTURETOKENPPTX2"},
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			doc, err := extractor.Extract(context.Background(), fixture(t, testCase.file))
			require.NoError(t, err)
			assert.Equal(t, testCase.format, doc.Format)
			assert.Equal(t, Version, doc.ExtractorVersion)
			assert.False(t, doc.Partial)
			assert.Equal(t, testCase.unitCount, doc.UnitCount)
			require.Len(t, doc.Units, len(testCase.labels))
			for index, label := range testCase.labels {
				assert.Equal(t, testCase.kind, doc.Units[index].Kind)
				assert.Equal(t, label, doc.Units[index].Label)
			}
			for _, needle := range testCase.contains {
				assert.Contains(t, doc.Text, needle)
			}
			if testCase.lowText != nil {
				assert.Equal(t, testCase.lowText, doc.LowTextUnits)
			}
			assert.Equal(t, (int64(len(doc.Text))+3)/4, doc.TokenEstimate)
			// Units are ordered, disjoint, and inside the text.
			previousEnd := 0
			for _, unit := range doc.Units {
				assert.GreaterOrEqual(t, unit.Start, previousEnd)
				assert.LessOrEqual(t, unit.End, len(doc.Text))
				assert.LessOrEqual(t, unit.Start, unit.End)
				previousEnd = unit.End
			}
		})
	}
}

func TestExtractPDFPageMapPointsAtThePage(t *testing.T) {
	t.Parallel()
	doc, err := New(DefaultLimits()).Extract(context.Background(), fixture(t, "report.pdf"))
	require.NoError(t, err)
	assert.Contains(t, unitText(doc, 0), "FIXTURETOKENPDF1")
	assert.NotContains(t, unitText(doc, 0), "FIXTURETOKENPDF2")
	assert.Contains(t, unitText(doc, 1), "FIXTURETOKENPDF2")
	assert.False(t, doc.Units[2].HasText)
}

func TestExtractPDFPageLimitIsPartialNotSilent(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits()
	limits.MaxPages = 2
	doc, err := New(limits).Extract(context.Background(), fixture(t, "report.pdf"))
	require.NoError(t, err)
	assert.True(t, doc.Partial)
	assert.Equal(t, PartialPageLimit, doc.PartialBy)
	assert.Equal(t, 4, doc.UnitCount, "the source page count survives the cut")
	assert.Len(t, doc.Units, 2)
	assert.NotContains(t, doc.Text, "Fourth page")
}

func TestExtractRefusals(t *testing.T) {
	t.Parallel()
	report := fixture(t, "report.pdf")
	cases := []struct {
		name   string
		data   []byte
		limits func(*Limits)
		want   Reason
	}{
		{name: "empty", data: nil, want: ReasonEmpty},
		{name: "encrypted pdf", data: fixture(t, "encrypted.pdf"), want: ReasonEncrypted},
		{name: "malformed pdf", data: []byte("%PDF-1.7\n1 0 obj << /Type /Catalog >>\ngarbage without xref"), want: ReasonMalformed},
		{name: "truncated pdf", data: report[:len(report)/3], want: ReasonMalformed},
		{name: "png image", data: append([]byte("\x89PNG\r\n\x1a\n"), make([]byte, 64)...), want: ReasonUnsupportedFormat},
		{name: "legacy office or encrypted ooxml", data: append([]byte{0xD0, 0xCF, 0x11, 0xE0, 0xA1, 0xB1, 0x1A, 0xE1}, make([]byte, 64)...), want: ReasonUnsupportedFormat},
		{name: "arbitrary binary", data: []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0x00, 0x7F}, want: ReasonUnsupportedFormat},
		{name: "zip that is not an office file", data: zipOf(t, map[string][]byte{"readme.txt": []byte("hello")}), want: ReasonUnsupportedFormat},
		{name: "whitespace only", data: []byte(" \n\t \n"), want: ReasonNoText},
		{
			name: "oversized input", data: bytes.Repeat([]byte("a"), 2048),
			limits: func(limits *Limits) { limits.MaxInputBytes = 1024 }, want: ReasonTooLarge,
		},
	}
	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			t.Parallel()
			limits := DefaultLimits()
			if testCase.limits != nil {
				testCase.limits(&limits)
			}
			assert.NotPanics(t, func() {
				_, err := New(limits).Extract(context.Background(), testCase.data)
				requireReason(t, err, testCase.want)
			})
		})
	}
}

func zipOf(t *testing.T, files map[string][]byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for name, content := range files {
		entry, err := writer.Create(name)
		require.NoError(t, err)
		_, err = entry.Write(content)
		require.NoError(t, err)
	}
	require.NoError(t, writer.Close())
	return buffer.Bytes()
}

func TestExtractZipBombs(t *testing.T) {
	t.Parallel()
	extractor := New(DefaultLimits())

	t.Run("compression ratio", func(t *testing.T) {
		t.Parallel()
		// 16 MiB of one byte deflates to a few KiB: far past 100x.
		bomb := zipOf(t, map[string][]byte{
			"[Content_Types].xml": []byte("<Types/>"),
			"word/document.xml":   bytes.Repeat([]byte{'a'}, 16<<20),
		})
		_, err := extractor.Extract(context.Background(), bomb)
		requireReason(t, err, ReasonUnsafeStructure)
	})

	t.Run("entry count", func(t *testing.T) {
		t.Parallel()
		files := map[string][]byte{"word/document.xml": []byte("<w:document/>")}
		for index := range 2_001 {
			files["parts/p"+strconv.Itoa(index)+".xml"] = []byte("x")
		}
		_, err := extractor.Extract(context.Background(), zipOf(t, files))
		requireReason(t, err, ReasonUnsafeStructure)
	})

	t.Run("total uncompressed size", func(t *testing.T) {
		t.Parallel()
		limits := DefaultLimits()
		limits.MaxZipUncompressed = 1 << 10
		files := map[string][]byte{"word/document.xml": []byte(strings.Repeat("<w:p/>", 400))}
		_, err := New(limits).Extract(context.Background(), zipOf(t, files))
		requireReason(t, err, ReasonUnsafeStructure)
	})
}

func TestExtractRejectsXMLEntityBomb(t *testing.T) {
	t.Parallel()
	laughs := `<?xml version="1.0"?>
<!DOCTYPE lolz [
 <!ENTITY lol "lol">
 <!ENTITY lol2 "&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;&lol;">
 <!ENTITY lol3 "&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;&lol2;">
]>
<w:document xmlns:w="http://schemas.openxmlformats.org/wordprocessingml/2006/main"><w:body><w:p><w:r><w:t>&lol3;</w:t></w:r></w:p></w:body></w:document>`
	for name, part := range map[string]string{
		"docx": "word/document.xml",
		"pptx": "ppt/presentation.xml",
		"xlsx": "xl/workbook.xml",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			data := zipOf(t, map[string][]byte{part: []byte(laughs)})
			_, err := New(DefaultLimits()).Extract(context.Background(), data)
			requireReason(t, err, ReasonUnsafeStructure)
		})
	}
}

func TestExtractTextFormats(t *testing.T) {
	t.Parallel()
	extractor := New(DefaultLimits())

	t.Run("utf-8 markdown is kept as is", func(t *testing.T) {
		t.Parallel()
		doc, err := extractor.Extract(context.Background(), []byte("# Title\n\nbody with FIXTURETOKENMD\n"))
		require.NoError(t, err)
		assert.Equal(t, FormatText, doc.Format)
		assert.Equal(t, "# Title\n\nbody with FIXTURETOKENMD\n", doc.Text)
		require.Len(t, doc.Units, 1)
		assert.Equal(t, UnitPart, doc.Units[0].Kind)
	})

	t.Run("a text file that mentions %PDF- stays text", func(t *testing.T) {
		t.Parallel()
		doc, err := extractor.Extract(context.Background(), []byte("notes: the header is %PDF-1.7 in every file"))
		require.NoError(t, err)
		assert.Equal(t, FormatText, doc.Format)
	})

	t.Run("utf-16 with a byte order mark", func(t *testing.T) {
		t.Parallel()
		data := []byte{0xFF, 0xFE}
		for _, character := range "csv,é\n" {
			data = append(data, byte(character), byte(character>>8))
		}
		doc, err := extractor.Extract(context.Background(), data)
		require.NoError(t, err)
		assert.Equal(t, "csv,é\n", doc.Text)
	})

	t.Run("long text splits at line ends", func(t *testing.T) {
		t.Parallel()
		limits := DefaultLimits()
		limits.TextPartBytes = 64
		line := strings.Repeat("x", 30) + "\n"
		doc, err := New(limits).Extract(context.Background(), []byte(strings.Repeat(line, 10)))
		require.NoError(t, err)
		require.Len(t, doc.Units, 5)
		for index := range doc.Units {
			assert.Equal(t, strconv.Itoa(index+1), doc.Units[index].Label)
			assert.True(t, strings.HasSuffix(unitText(doc, index), "\n"))
		}
	})
}

func TestExtractTextLimitIsPartialNotSilent(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits()
	limits.TextPartBytes = 32
	limits.MaxTextBytes = 100
	line := strings.Repeat("y", 20) + "\n"
	doc, err := New(limits).Extract(context.Background(), []byte(strings.Repeat(line, 20)))
	require.NoError(t, err)
	assert.True(t, doc.Partial)
	assert.Equal(t, PartialTextLimit, doc.PartialBy)
	assert.LessOrEqual(t, len(doc.Text), 100)
	assert.Greater(t, doc.UnitCount, len(doc.Units), "the missing parts are counted")
}

func TestExtractXLSXCellLimitIsPartial(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits()
	limits.MaxCells = 3
	doc, err := New(limits).Extract(context.Background(), fixture(t, "sheet.xlsx"))
	require.NoError(t, err)
	assert.True(t, doc.Partial)
	assert.Equal(t, PartialCellLimit, doc.PartialBy)
	assert.Equal(t, 3, doc.UnitCount)
	assert.NotContains(t, doc.Text, "FIXTURETOKENXLSX")
}

func TestExtractDeadline(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	_, err := New(DefaultLimits()).Extract(ctx, fixture(t, "report.pdf"))
	require.Error(t, err)
	reason, ok := ReasonOf(err)
	if ok {
		assert.Equal(t, ReasonTimeout, reason)
	} else {
		assert.True(t, errors.Is(err, context.DeadlineExceeded), "error: %v", err)
	}
}

func TestNormalizeRemovesHiddenAndControlCharacters(t *testing.T) {
	t.Parallel()
	raw := "a\x00b\r\nc\rd\u200be\u202ef\u2066g\ufeffh\x1bi\tj\xff"
	assert.Equal(t, "ab\nc\nde"+"fghi\tj\uFFFD", normalize(raw))
}

func TestExtractUnitLimitIsPartialNotSilent(t *testing.T) {
	t.Parallel()
	limits := DefaultLimits()
	limits.MaxUnits = 1
	doc, err := New(limits).Extract(context.Background(), fixture(t, "deck.pptx"))
	require.NoError(t, err)
	assert.True(t, doc.Partial)
	assert.Equal(t, PartialPageLimit, doc.PartialBy)
	assert.Equal(t, 2, doc.UnitCount)
	require.Len(t, doc.Units, 1)
	assert.NotContains(t, doc.Text, "FIXTURETOKENPPTX2")
}
