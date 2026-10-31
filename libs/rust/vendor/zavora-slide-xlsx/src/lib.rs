//! Minimal embedded-workbook builder for chart data.
//!
//! Produces a valid `.xlsx` file (ZIP/OPC package) containing a single worksheet
//! with tabular data suitable for embedding as a chart data source in PowerPoint.
//!
//! This crate is intentionally minimal — it emits only the parts required for
//! PowerPoint to recognize the workbook as a valid chart data source:
//! - `[Content_Types].xml`
//! - `_rels/.rels`
//! - `xl/workbook.xml`
//! - `xl/_rels/workbook.xml.rels`
//! - `xl/worksheets/sheet1.xml`
//! - `xl/styles.xml`
//! - `xl/sharedStrings.xml`

mod error;

pub use error::XlsxError;

use std::io::{Cursor, Write};
use zip::ZipWriter;
use zip::write::SimpleFileOptions;

/// A cell value in the workbook.
#[derive(Debug, Clone)]
pub enum CellValue {
    /// A string value (stored in shared strings).
    Text(String),
    /// A numeric value.
    Number(f64),
}

/// Builder for a minimal `.xlsx` workbook containing chart data.
///
/// # Example
/// ```
/// use zavora_slide_xlsx::WorkbookBuilder;
///
/// let categories = &["Q1", "Q2", "Q3", "Q4"];
/// let series = &[
///     ("Revenue", vec![100.0, 150.0, 130.0, 170.0]),
///     ("Costs", vec![80.0, 90.0, 85.0, 95.0]),
/// ];
///
/// let xlsx_bytes = WorkbookBuilder::new()
///     .set_data(categories, series)
///     .build()
///     .unwrap();
///
/// assert!(!xlsx_bytes.is_empty());
/// ```
#[derive(Debug, Clone)]
pub struct WorkbookBuilder {
    /// Rows of cells. Row 0 is typically headers.
    rows: Vec<Vec<CellValue>>,
}

impl WorkbookBuilder {
    /// Creates a new empty workbook builder.
    pub fn new() -> Self {
        Self { rows: Vec::new() }
    }

    /// Sets a single cell value.
    ///
    /// Rows and columns are zero-indexed. The grid expands as needed.
    pub fn set_cell(&mut self, row: usize, col: usize, value: CellValue) -> &mut Self {
        // Expand rows if needed
        while self.rows.len() <= row {
            self.rows.push(Vec::new());
        }
        // Expand columns in the target row
        while self.rows[row].len() <= col {
            self.rows[row].push(CellValue::Text(String::new()));
        }
        self.rows[row][col] = value;
        self
    }

    /// Populates the workbook with chart data: categories in column A (rows 1..N)
    /// and series values in columns B, C, ... with series names in row 0.
    ///
    /// Layout matches what PowerPoint expects for an embedded chart workbook:
    /// ```text
    ///        |   B        |   C      | ...
    ///   -----+------------+----------+----
    ///   1    | Series1    | Series2  | ...
    ///   2    | cat1 val   | cat1 val | ...
    ///   3    | cat2 val   | cat2 val | ...
    /// ```
    /// Row 0 col 0 is left empty; categories go in col 0 starting at row 1.
    pub fn set_data(mut self, categories: &[&str], series: &[(&str, Vec<f64>)]) -> Self {
        self.rows.clear();

        // Header row: empty cell + series names
        let mut header = vec![CellValue::Text(String::new())];
        for (name, _) in series {
            header.push(CellValue::Text(name.to_string()));
        }
        self.rows.push(header);

        // Data rows: category + values
        for (i, cat) in categories.iter().enumerate() {
            let mut row = vec![CellValue::Text(cat.to_string())];
            for (_, values) in series {
                let val = values.get(i).copied().unwrap_or(0.0);
                row.push(CellValue::Number(val));
            }
            self.rows.push(row);
        }

        self
    }

    /// Builds the `.xlsx` file as a byte vector.
    pub fn build(&self) -> Result<Vec<u8>, XlsxError> {
        let buf = Cursor::new(Vec::new());
        let mut zip = ZipWriter::new(buf);
        let options =
            SimpleFileOptions::default().compression_method(zip::CompressionMethod::Deflated);

        // Collect shared strings
        let shared_strings = self.collect_shared_strings();

        // [Content_Types].xml
        zip.start_file("[Content_Types].xml", options)?;
        zip.write_all(self.content_types_xml().as_bytes())?;

        // _rels/.rels
        zip.start_file("_rels/.rels", options)?;
        zip.write_all(Self::root_rels_xml().as_bytes())?;

        // xl/workbook.xml
        zip.start_file("xl/workbook.xml", options)?;
        zip.write_all(Self::workbook_xml().as_bytes())?;

        // xl/_rels/workbook.xml.rels
        zip.start_file("xl/_rels/workbook.xml.rels", options)?;
        zip.write_all(Self::workbook_rels_xml().as_bytes())?;

        // xl/worksheets/sheet1.xml
        zip.start_file("xl/worksheets/sheet1.xml", options)?;
        zip.write_all(self.sheet_xml(&shared_strings).as_bytes())?;

        // xl/styles.xml
        zip.start_file("xl/styles.xml", options)?;
        zip.write_all(Self::styles_xml().as_bytes())?;

        // xl/sharedStrings.xml
        zip.start_file("xl/sharedStrings.xml", options)?;
        zip.write_all(Self::shared_strings_xml(&shared_strings).as_bytes())?;

        let cursor = zip.finish()?;
        Ok(cursor.into_inner())
    }

    /// Collects all unique strings from the grid in order.
    fn collect_shared_strings(&self) -> Vec<String> {
        let mut strings = Vec::new();
        for row in &self.rows {
            for cell in row {
                if let CellValue::Text(s) = cell
                    && !s.is_empty()
                    && !strings.contains(s)
                {
                    strings.push(s.clone());
                }
            }
        }
        strings
    }

    fn content_types_xml(&self) -> String {
        r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Types xmlns="http://schemas.openxmlformats.org/package/2006/content-types">
  <Default Extension="rels" ContentType="application/vnd.openxmlformats-package.relationships+xml"/>
  <Default Extension="xml" ContentType="application/xml"/>
  <Override PartName="/xl/workbook.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sheet.main+xml"/>
  <Override PartName="/xl/worksheets/sheet1.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.worksheet+xml"/>
  <Override PartName="/xl/styles.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.styles+xml"/>
  <Override PartName="/xl/sharedStrings.xml" ContentType="application/vnd.openxmlformats-officedocument.spreadsheetml.sharedStrings+xml"/>
</Types>"#.to_string()
    }

    fn root_rels_xml() -> String {
        r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/officeDocument" Target="xl/workbook.xml"/>
</Relationships>"#.to_string()
    }

    fn workbook_xml() -> String {
        r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<workbook xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"
          xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <sheets>
    <sheet name="Sheet1" sheetId="1" r:id="rId1"/>
  </sheets>
</workbook>"#
            .to_string()
    }

    fn workbook_rels_xml() -> String {
        r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<Relationships xmlns="http://schemas.openxmlformats.org/package/2006/relationships">
  <Relationship Id="rId1" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/worksheet" Target="worksheets/sheet1.xml"/>
  <Relationship Id="rId2" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/styles" Target="styles.xml"/>
  <Relationship Id="rId3" Type="http://schemas.openxmlformats.org/officeDocument/2006/relationships/sharedStrings" Target="sharedStrings.xml"/>
</Relationships>"#.to_string()
    }

    fn styles_xml() -> String {
        r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<styleSheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main">
  <fonts count="1">
    <font>
      <sz val="11"/>
      <name val="Calibri"/>
    </font>
  </fonts>
  <fills count="2">
    <fill><patternFill patternType="none"/></fill>
    <fill><patternFill patternType="gray125"/></fill>
  </fills>
  <borders count="1">
    <border>
      <left/><right/><top/><bottom/><diagonal/>
    </border>
  </borders>
  <cellStyleXfs count="1">
    <xf numFmtId="0" fontId="0" fillId="0" borderId="0"/>
  </cellStyleXfs>
  <cellXfs count="1">
    <xf numFmtId="0" fontId="0" fillId="0" borderId="0" xfId="0"/>
  </cellXfs>
</styleSheet>"#
            .to_string()
    }

    fn shared_strings_xml(strings: &[String]) -> String {
        let mut xml = format!(
            r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<sst xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main" count="{}" uniqueCount="{}">"#,
            strings.len(),
            strings.len()
        );
        for s in strings {
            xml.push_str(&format!("\n  <si><t>{}</t></si>", xml_escape(s)));
        }
        xml.push_str("\n</sst>");
        xml
    }

    /// Generates the sheet XML with cell references (A1, B1, etc.).
    fn sheet_xml(&self, shared_strings: &[String]) -> String {
        let mut xml = String::from(
            r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<worksheet xmlns="http://schemas.openxmlformats.org/spreadsheetml/2006/main"
           xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
  <sheetData>"#,
        );

        for (row_idx, row) in self.rows.iter().enumerate() {
            let row_num = row_idx + 1; // Excel rows are 1-based
            xml.push_str(&format!("\n    <row r=\"{}\">", row_num));
            for (col_idx, cell) in row.iter().enumerate() {
                let col_letter = col_index_to_letter(col_idx);
                let cell_ref = format!("{}{}", col_letter, row_num);
                match cell {
                    CellValue::Text(s) if s.is_empty() => {
                        // Skip empty cells
                    }
                    CellValue::Text(s) => {
                        // Shared string reference
                        let ssi = shared_strings.iter().position(|x| x == s).unwrap_or(0);
                        xml.push_str(&format!(
                            "\n      <c r=\"{}\" t=\"s\"><v>{}</v></c>",
                            cell_ref, ssi
                        ));
                    }
                    CellValue::Number(n) => {
                        xml.push_str(&format!("\n      <c r=\"{}\"><v>{}</v></c>", cell_ref, n));
                    }
                }
            }
            xml.push_str("\n    </row>");
        }

        xml.push_str("\n  </sheetData>\n</worksheet>");
        xml
    }
}

impl Default for WorkbookBuilder {
    fn default() -> Self {
        Self::new()
    }
}

/// Converts a zero-based column index to an Excel column letter (A, B, ..., Z, AA, ...).
fn col_index_to_letter(idx: usize) -> String {
    let mut result = String::new();
    let mut n = idx;
    loop {
        result.insert(0, (b'A' + (n % 26) as u8) as char);
        if n < 26 {
            break;
        }
        n = n / 26 - 1;
    }
    result
}

/// Escapes XML special characters.
fn xml_escape(s: &str) -> String {
    s.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
        .replace('"', "&quot;")
        .replace('\'', "&apos;")
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn col_letter_a() {
        assert_eq!(col_index_to_letter(0), "A");
    }

    #[test]
    fn col_letter_z() {
        assert_eq!(col_index_to_letter(25), "Z");
    }

    #[test]
    fn col_letter_aa() {
        assert_eq!(col_index_to_letter(26), "AA");
    }

    #[test]
    fn col_letter_az() {
        assert_eq!(col_index_to_letter(51), "AZ");
    }

    #[test]
    fn builder_produces_nonempty_bytes() {
        let bytes = WorkbookBuilder::new()
            .set_data(&["Q1", "Q2"], &[("Sales", vec![10.0, 20.0])])
            .build()
            .unwrap();
        assert!(!bytes.is_empty());
    }

    #[test]
    fn output_is_valid_zip() {
        let bytes = WorkbookBuilder::new()
            .set_data(&["A", "B", "C"], &[("Series1", vec![1.0, 2.0, 3.0])])
            .build()
            .unwrap();

        let cursor = Cursor::new(bytes);
        let archive = zip::ZipArchive::new(cursor).expect("should be a valid ZIP");
        assert!(!archive.is_empty());
    }

    #[test]
    fn output_contains_required_parts() {
        let bytes = WorkbookBuilder::new()
            .set_data(&["X"], &[("Y", vec![42.0])])
            .build()
            .unwrap();

        let cursor = Cursor::new(bytes);
        let mut archive = zip::ZipArchive::new(cursor).unwrap();

        let required_parts = [
            "[Content_Types].xml",
            "_rels/.rels",
            "xl/workbook.xml",
            "xl/_rels/workbook.xml.rels",
            "xl/worksheets/sheet1.xml",
            "xl/styles.xml",
            "xl/sharedStrings.xml",
        ];

        for part in &required_parts {
            assert!(
                archive.by_name(part).is_ok(),
                "missing required part: {}",
                part
            );
        }
    }

    #[test]
    fn cell_data_present_in_sheet() {
        let bytes = WorkbookBuilder::new()
            .set_data(&["Jan", "Feb"], &[("Revenue", vec![100.0, 200.0])])
            .build()
            .unwrap();

        let cursor = Cursor::new(bytes);
        let mut archive = zip::ZipArchive::new(cursor).unwrap();

        // Read sheet XML
        let mut sheet = archive.by_name("xl/worksheets/sheet1.xml").unwrap();
        let mut sheet_xml = String::new();
        std::io::Read::read_to_string(&mut sheet, &mut sheet_xml).unwrap();

        // Numeric values should appear in the sheet
        assert!(sheet_xml.contains("<v>100</v>"), "should contain value 100");
        assert!(sheet_xml.contains("<v>200</v>"), "should contain value 200");

        // Read shared strings to verify category text
        drop(sheet);
        let mut ss = archive.by_name("xl/sharedStrings.xml").unwrap();
        let mut ss_xml = String::new();
        std::io::Read::read_to_string(&mut ss, &mut ss_xml).unwrap();

        assert!(ss_xml.contains("<t>Jan</t>"), "should contain category Jan");
        assert!(ss_xml.contains("<t>Feb</t>"), "should contain category Feb");
        assert!(
            ss_xml.contains("<t>Revenue</t>"),
            "should contain series name Revenue"
        );
    }

    #[test]
    fn set_cell_individual() {
        let mut builder = WorkbookBuilder::new();
        builder.set_cell(0, 0, CellValue::Text("Header".to_string()));
        builder.set_cell(1, 0, CellValue::Number(2.5));

        let bytes = builder.build().unwrap();
        let cursor = Cursor::new(bytes);
        let mut archive = zip::ZipArchive::new(cursor).unwrap();

        let mut sheet = archive.by_name("xl/worksheets/sheet1.xml").unwrap();
        let mut sheet_xml = String::new();
        std::io::Read::read_to_string(&mut sheet, &mut sheet_xml).unwrap();

        assert!(sheet_xml.contains("<v>2.5</v>"));
    }

    #[test]
    fn xml_escape_special_chars() {
        let bytes = WorkbookBuilder::new()
            .set_data(&["A & B", "<C>"], &[("D\"E", vec![1.0, 2.0])])
            .build()
            .unwrap();

        let cursor = Cursor::new(bytes);
        let mut archive = zip::ZipArchive::new(cursor).unwrap();

        let mut ss = archive.by_name("xl/sharedStrings.xml").unwrap();
        let mut ss_xml = String::new();
        std::io::Read::read_to_string(&mut ss, &mut ss_xml).unwrap();

        assert!(ss_xml.contains("A &amp; B"));
        assert!(ss_xml.contains("&lt;C&gt;"));
        assert!(ss_xml.contains("D&quot;E"));
    }

    #[test]
    fn empty_workbook_builds() {
        let bytes = WorkbookBuilder::new().build().unwrap();
        assert!(!bytes.is_empty());

        let cursor = Cursor::new(bytes);
        let archive = zip::ZipArchive::new(cursor).expect("empty workbook should be valid ZIP");
        assert!(!archive.is_empty());
    }
}
