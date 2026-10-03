package extract

import (
	"bytes"
	"context"
	"strings"

	"github.com/xuri/excelize/v2"
)

// extractXLSX reads every worksheet as tab-separated rows. One sheet is one
// unit, labelled with the sheet name. A workbook past the cell limit is marked
// Partial at the sheet where the limit was reached.
//
// Every XML part is checked for a DTD BEFORE excelize parses anything, so the
// entity rule does not depend on how a third-party parser is configured.
func extractXLSX(
	ctx context.Context,
	data []byte,
	parts *zipParts,
	limits Limits,
	builder *textBuilder,
) error {
	for name := range parts.entries {
		if !strings.HasSuffix(name, ".xml") && !strings.HasSuffix(name, ".rels") {
			continue
		}
		if _, err := parts.readXML(name); err != nil {
			return err
		}
	}
	workbook, err := excelize.OpenReader(bytes.NewReader(data), excelize.Options{
		UnzipSizeLimit:    int64(limits.MaxZipUncompressed),
		UnzipXMLSizeLimit: limits.MaxXMLEntryBytes,
	})
	if err != nil {
		return refuse(ReasonMalformed, err)
	}
	defer func() { _ = workbook.Close() }()
	sheets := workbook.GetSheetList()
	builder.unitCount = len(sheets)
	cells := 0
	for _, sheet := range sheets {
		if err := ctx.Err(); err != nil {
			return err
		}
		text, stopped, err := sheetText(ctx, workbook, sheet, limits.MaxCells, &cells)
		if err != nil {
			return err
		}
		if !builder.add(UnitSheet, sheet, text) {
			return nil
		}
		if stopped {
			builder.stop(PartialCellLimit)
			return nil
		}
	}
	return nil
}

// sheetText returns one sheet as tab-separated rows. stopped is true when the
// workbook's cell limit was reached inside this sheet.
func sheetText(
	ctx context.Context,
	workbook *excelize.File,
	sheet string,
	maxCells int,
	cells *int,
) (string, bool, error) {
	rows, err := workbook.Rows(sheet)
	if err != nil {
		return "", false, refuse(ReasonMalformed, err)
	}
	defer func() { _ = rows.Close() }()
	var text strings.Builder
	for rows.Next() {
		if err := ctx.Err(); err != nil {
			return "", false, err
		}
		columns, err := rows.Columns()
		if err != nil {
			return "", false, refuse(ReasonMalformed, err)
		}
		// Trailing empty cells carry no information.
		for len(columns) > 0 && columns[len(columns)-1] == "" {
			columns = columns[:len(columns)-1]
		}
		if *cells+len(columns) > maxCells {
			return text.String(), true, nil
		}
		*cells += len(columns)
		text.WriteString(strings.Join(columns, "\t"))
		text.WriteByte('\n')
	}
	if err := rows.Error(); err != nil {
		return "", false, refuse(ReasonMalformed, err)
	}
	return strings.TrimRight(text.String(), "\n"), false, nil
}
