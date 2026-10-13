//! Integration tests for chart authoring (Part B — Requirements 4.1–5.2, 26.2).
//!
//! Validates:
//! - Adding charts of all supported types (bar, column, line, pie, doughnut, area, scatter)
//! - The chart XML contains correct chart-type elements with proper attributes
//! - Categories and series data are present in the chart XML
//! - The embedded workbook is a valid xlsx
//! - Round-trip: add chart → save → reopen → chart part is byte-preserved
//! - Data editing: update categories/series on opened decks
//! - LibreOffice load gate (env-guarded)

use std::io::Cursor;
use zavora_slide::{ChartKind, ChartSpec, Emu, Layout, Presentation};
use zavora_slide_opc::OpcPackage;

/// Helper: create a presentation with a chart on slide 0.
fn deck_with_chart(kind: ChartKind, title: Option<&str>) -> Vec<u8> {
    let mut p = Presentation::new();
    p.add_slide(Layout::Blank);
    let spec = ChartSpec {
        kind,
        categories: vec!["Q1".into(), "Q2".into(), "Q3".into(), "Q4".into()],
        series: vec![
            ("Revenue".into(), vec![100.0, 150.0, 130.0, 170.0]),
            ("Costs".into(), vec![80.0, 90.0, 85.0, 95.0]),
        ],
        title: title.map(|s| s.to_string()),
        legend_position: None,
        data_labels: false,
    };
    {
        let mut slide = p.slide_mut(0).unwrap();
        slide
            .add_chart(
                &spec,
                Emu(914400),
                Emu(914400),
                Emu(7315200),
                Emu(4572000),
                1,
            )
            .unwrap();
    }
    p.save_to_buffer().unwrap()
}

#[test]
fn chart_part_exists_in_package() {
    let bytes = deck_with_chart(ChartKind::ClusteredBar, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    assert!(
        pkg.get_part("/ppt/charts/chart1.xml").is_some(),
        "chart part should exist"
    );
}

#[test]
fn embedded_workbook_exists_in_package() {
    let bytes = deck_with_chart(ChartKind::ClusteredBar, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    assert!(
        pkg.get_part("/ppt/embeddings/Microsoft_Excel_Worksheet1.xlsx")
            .is_some(),
        "embedded workbook should exist"
    );
}

#[test]
fn chart_xml_has_bar_chart_with_correct_bar_dir_and_grouping() {
    let bytes = deck_with_chart(ChartKind::ClusteredBar, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("<c:barChart>"),
        "should contain c:barChart"
    );
    assert!(
        chart_xml.contains("barDir val=\"bar\""),
        "clustered bar should have barDir=bar"
    );
    assert!(
        chart_xml.contains("grouping val=\"clustered\""),
        "clustered bar should have grouping=clustered"
    );
}

#[test]
fn stacked_column_has_correct_attributes() {
    let bytes = deck_with_chart(ChartKind::StackedColumn, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("barDir val=\"col\""),
        "stacked column should have barDir=col"
    );
    assert!(
        chart_xml.contains("grouping val=\"stacked\""),
        "stacked column should have grouping=stacked"
    );
}

#[test]
fn chart_xml_contains_categories_and_series_data() {
    let bytes = deck_with_chart(ChartKind::ClusteredBar, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    // Categories.
    assert!(
        chart_xml.contains("<c:v>Q1</c:v>"),
        "should contain category Q1"
    );
    assert!(
        chart_xml.contains("<c:v>Q2</c:v>"),
        "should contain category Q2"
    );
    assert!(
        chart_xml.contains("<c:v>Q3</c:v>"),
        "should contain category Q3"
    );
    assert!(
        chart_xml.contains("<c:v>Q4</c:v>"),
        "should contain category Q4"
    );

    // Series names.
    assert!(
        chart_xml.contains("<c:v>Revenue</c:v>"),
        "should contain series name Revenue"
    );
    assert!(
        chart_xml.contains("<c:v>Costs</c:v>"),
        "should contain series name Costs"
    );

    // Series values.
    assert!(
        chart_xml.contains("<c:v>100</c:v>"),
        "should contain value 100"
    );
    assert!(
        chart_xml.contains("<c:v>150</c:v>"),
        "should contain value 150"
    );
    assert!(
        chart_xml.contains("<c:v>80</c:v>"),
        "should contain value 80"
    );
    assert!(
        chart_xml.contains("<c:v>95</c:v>"),
        "should contain value 95"
    );
}

#[test]
fn chart_xml_has_external_data_reference() {
    let bytes = deck_with_chart(ChartKind::ClusteredBar, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("<c:externalData r:id=\"rId1\">"),
        "should have externalData referencing rId1"
    );
}

#[test]
fn chart_xml_has_title_when_specified() {
    let bytes = deck_with_chart(ChartKind::ClusteredColumn, Some("Sales Report"));
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("<c:title>"),
        "should contain title element"
    );
    assert!(
        chart_xml.contains("<a:t>Sales Report</a:t>"),
        "should contain title text"
    );
}

#[test]
fn embedded_workbook_is_valid_xlsx() {
    let bytes = deck_with_chart(ChartKind::ClusteredBar, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let xlsx_bytes = pkg
        .get_part("/ppt/embeddings/Microsoft_Excel_Worksheet1.xlsx")
        .unwrap();

    // Should be a valid ZIP archive.
    let cursor = Cursor::new(xlsx_bytes);
    let mut archive =
        zip::ZipArchive::new(cursor).expect("embedded workbook should be a valid ZIP");

    // Should contain the standard xlsx parts.
    assert!(archive.by_name("xl/worksheets/sheet1.xml").is_ok());
    assert!(archive.by_name("xl/sharedStrings.xml").is_ok());

    // Verify data is present in the sheet.
    let mut sheet = archive.by_name("xl/worksheets/sheet1.xml").unwrap();
    let mut sheet_xml = String::new();
    std::io::Read::read_to_string(&mut sheet, &mut sheet_xml).unwrap();
    assert!(
        sheet_xml.contains("<v>100</v>"),
        "sheet should contain value 100"
    );
    assert!(
        sheet_xml.contains("<v>150</v>"),
        "sheet should contain value 150"
    );
}

#[test]
fn slide_has_graphic_frame_referencing_chart() {
    let bytes = deck_with_chart(ChartKind::ClusteredBar, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let slide_xml = String::from_utf8_lossy(pkg.get_part("/ppt/slides/slide1.xml").unwrap());

    assert!(
        slide_xml.contains("<p:graphicFrame>"),
        "slide should contain a graphicFrame"
    );
    assert!(
        slide_xml.contains("uri=\"http://schemas.openxmlformats.org/drawingml/2006/chart\""),
        "graphicFrame should reference the chart URI"
    );
    assert!(
        slide_xml.contains("r:id=\"rId20\""),
        "graphicFrame should reference the chart relationship"
    );
}

#[test]
fn slide_to_chart_relationship_exists() {
    let bytes = deck_with_chart(ChartKind::ClusteredBar, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let rels = pkg
        .get_part_rels("/ppt/slides/slide1.xml")
        .expect("slide should have relationships");

    let chart_rel = rels
        .get_by_id("rId20")
        .expect("should have rId20 for chart");
    assert_eq!(
        chart_rel.rel_type,
        "http://schemas.openxmlformats.org/officeDocument/2006/relationships/chart"
    );
    assert!(
        chart_rel.target.contains("chart1.xml"),
        "chart rel target should point to chart1.xml"
    );
}

#[test]
fn chart_to_workbook_relationship_exists() {
    let bytes = deck_with_chart(ChartKind::ClusteredBar, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let rels = pkg
        .get_part_rels("/ppt/charts/chart1.xml")
        .expect("chart should have relationships");

    let wb_rel = rels
        .get_by_id("rId1")
        .expect("should have rId1 for workbook");
    assert_eq!(
        wb_rel.rel_type,
        "http://schemas.openxmlformats.org/officeDocument/2006/relationships/package"
    );
    assert!(
        wb_rel.target.contains("Microsoft_Excel_Worksheet1.xlsx"),
        "workbook rel target should point to the xlsx"
    );
}

#[test]
fn content_type_override_for_chart_part() {
    let bytes = deck_with_chart(ChartKind::ClusteredBar, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();

    let ct = pkg
        .content_types
        .content_type_for("/ppt/charts/chart1.xml")
        .expect("chart part should have a content type");
    assert_eq!(
        ct,
        "application/vnd.openxmlformats-officedocument.drawingml.chart+xml"
    );
}

#[test]
fn content_type_default_for_xlsx() {
    let bytes = deck_with_chart(ChartKind::ClusteredBar, None);
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();

    let ct = pkg
        .content_types
        .content_type_for("/ppt/embeddings/Microsoft_Excel_Worksheet1.xlsx")
        .expect("xlsx should have a content type via default extension");
    assert_eq!(
        ct,
        "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet"
    );
}

#[test]
fn round_trip_chart_part_is_byte_preserved() {
    // Requirement 4.6: open and re-save unmodified → chart parts byte-preserved.
    let bytes = deck_with_chart(ChartKind::ClusteredBar, Some("Test Chart"));

    // Open the saved deck.
    let p = Presentation::open_from_bytes(&bytes).unwrap();
    // Re-save without any edits.
    let bytes2 = p.save_to_buffer().unwrap();

    // Compare chart part bytes.
    let pkg1 = OpcPackage::from_reader(Cursor::new(&bytes)).unwrap();
    let pkg2 = OpcPackage::from_reader(Cursor::new(&bytes2)).unwrap();

    let chart1 = pkg1.get_part("/ppt/charts/chart1.xml").unwrap();
    let chart2 = pkg2.get_part("/ppt/charts/chart1.xml").unwrap();
    assert_eq!(
        chart1, chart2,
        "chart part should be byte-preserved on unmodified round-trip"
    );

    let wb1 = pkg1
        .get_part("/ppt/embeddings/Microsoft_Excel_Worksheet1.xlsx")
        .unwrap();
    let wb2 = pkg2
        .get_part("/ppt/embeddings/Microsoft_Excel_Worksheet1.xlsx")
        .unwrap();
    assert_eq!(
        wb1, wb2,
        "embedded workbook should be byte-preserved on unmodified round-trip"
    );
}

#[test]
fn all_nine_chart_kinds_produce_valid_packages() {
    for kind in [
        ChartKind::ClusteredBar,
        ChartKind::StackedBar,
        ChartKind::ClusteredColumn,
        ChartKind::StackedColumn,
        ChartKind::Line,
        ChartKind::Pie,
        ChartKind::Doughnut,
        ChartKind::Area,
        ChartKind::Scatter,
    ] {
        let bytes = deck_with_chart(kind, None);
        let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
        assert!(
            pkg.get_part("/ppt/charts/chart1.xml").is_some(),
            "chart part should exist for {:?}",
            kind
        );
        assert!(
            pkg.get_part("/ppt/embeddings/Microsoft_Excel_Worksheet1.xlsx")
                .is_some(),
            "workbook should exist for {:?}",
            kind
        );
    }
}

#[test]
fn line_chart_has_correct_structure() {
    let bytes = deck_with_chart(ChartKind::Line, Some("Line Chart"));
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("<c:lineChart>"),
        "should contain c:lineChart"
    );
    assert!(
        chart_xml.contains("grouping val=\"standard\""),
        "line chart should have grouping=standard"
    );
    assert!(
        chart_xml.contains("<c:catAx>"),
        "line chart should have catAx"
    );
    assert!(
        chart_xml.contains("<c:valAx>"),
        "line chart should have valAx"
    );
}

#[test]
fn pie_chart_has_correct_structure() {
    let bytes = deck_with_chart(ChartKind::Pie, Some("Pie Chart"));
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("<c:pieChart>"),
        "should contain c:pieChart"
    );
    assert!(
        chart_xml.contains("varyColors val=\"1\""),
        "pie chart should have varyColors=1"
    );
    // Pie charts have no axes.
    assert!(
        !chart_xml.contains("<c:catAx>"),
        "pie chart should NOT have catAx"
    );
    assert!(
        !chart_xml.contains("<c:valAx>"),
        "pie chart should NOT have valAx"
    );
}

#[test]
fn doughnut_chart_has_correct_structure() {
    let bytes = deck_with_chart(ChartKind::Doughnut, Some("Doughnut Chart"));
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("<c:doughnutChart>"),
        "should contain c:doughnutChart"
    );
    assert!(
        chart_xml.contains("holeSize val=\"50\""),
        "doughnut chart should have holeSize=50"
    );
    assert!(
        chart_xml.contains("varyColors val=\"1\""),
        "doughnut chart should have varyColors=1"
    );
    // Doughnut charts have no axes.
    assert!(
        !chart_xml.contains("<c:catAx>"),
        "doughnut chart should NOT have catAx"
    );
    assert!(
        !chart_xml.contains("<c:valAx>"),
        "doughnut chart should NOT have valAx"
    );
}

#[test]
fn area_chart_has_correct_structure() {
    let bytes = deck_with_chart(ChartKind::Area, Some("Area Chart"));
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("<c:areaChart>"),
        "should contain c:areaChart"
    );
    assert!(
        chart_xml.contains("grouping val=\"standard\""),
        "area chart should have grouping=standard"
    );
    assert!(
        chart_xml.contains("<c:catAx>"),
        "area chart should have catAx"
    );
    assert!(
        chart_xml.contains("<c:valAx>"),
        "area chart should have valAx"
    );
}

#[test]
fn scatter_chart_has_correct_structure() {
    let bytes = deck_with_chart(ChartKind::Scatter, Some("Scatter Chart"));
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("<c:scatterChart>"),
        "should contain c:scatterChart"
    );
    assert!(
        chart_xml.contains("scatterStyle val=\"lineMarker\""),
        "scatter chart should have scatterStyle=lineMarker"
    );
    // Scatter uses xVal/yVal instead of cat/val.
    assert!(
        chart_xml.contains("<c:xVal>"),
        "scatter chart should have xVal"
    );
    assert!(
        chart_xml.contains("<c:yVal>"),
        "scatter chart should have yVal"
    );
    // Scatter has two value axes.
    assert!(
        chart_xml.contains("<c:valAx>"),
        "scatter chart should have valAx"
    );
}

#[test]
fn doughnut_chart_data_editing() {
    let bytes = deck_with_chart(ChartKind::Doughnut, None);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["Slice A".into(), "Slice B".into()],
        series: vec![("Portions".into(), vec![60.0, 40.0])],
    };
    p.update_chart_data(0, 0, &update).unwrap();

    let saved = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(saved)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("<c:doughnutChart>"),
        "doughnutChart preserved"
    );
    assert!(
        chart_xml.contains("<c:v>Slice A</c:v>"),
        "new category Slice A"
    );
    assert!(
        chart_xml.contains("<c:v>Slice B</c:v>"),
        "new category Slice B"
    );
    assert!(chart_xml.contains("<c:v>60</c:v>"), "new value 60");
    assert!(chart_xml.contains("<c:v>40</c:v>"), "new value 40");
}

#[test]
fn area_chart_data_editing() {
    let bytes = deck_with_chart(ChartKind::Area, None);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["Mon".into(), "Tue".into(), "Wed".into()],
        series: vec![("Traffic".into(), vec![100.0, 200.0, 150.0])],
    };
    p.update_chart_data(0, 0, &update).unwrap();

    let saved = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(saved)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(chart_xml.contains("<c:areaChart>"), "areaChart preserved");
    assert!(chart_xml.contains("<c:v>Mon</c:v>"), "new category Mon");
    assert!(chart_xml.contains("<c:v>200</c:v>"), "new value 200");
}

#[test]
fn round_trip_all_chart_types_byte_preserved() {
    // Requirement 4.6: open and re-save unmodified → chart parts byte-preserved for all types.
    for kind in [
        ChartKind::ClusteredBar,
        ChartKind::Line,
        ChartKind::Pie,
        ChartKind::Doughnut,
        ChartKind::Area,
        ChartKind::Scatter,
    ] {
        let bytes = deck_with_chart(kind, Some("Round-trip Test"));
        let p = Presentation::open_from_bytes(&bytes).unwrap();
        let bytes2 = p.save_to_buffer().unwrap();

        let pkg1 = OpcPackage::from_reader(Cursor::new(&bytes)).unwrap();
        let pkg2 = OpcPackage::from_reader(Cursor::new(&bytes2)).unwrap();

        let chart1 = pkg1.get_part("/ppt/charts/chart1.xml").unwrap();
        let chart2 = pkg2.get_part("/ppt/charts/chart1.xml").unwrap();
        assert_eq!(
            chart1, chart2,
            "chart part should be byte-preserved on unmodified round-trip for {:?}",
            kind
        );
    }
}

#[test]
fn chart_with_special_characters_in_categories() {
    let mut p = Presentation::new();
    p.add_slide(Layout::Blank);
    let spec = ChartSpec {
        kind: ChartKind::ClusteredColumn,
        categories: vec!["A & B".into(), "<C>".into(), "D\"E".into()],
        series: vec![("Series 'One'".into(), vec![1.0, 2.0, 3.0])],
        title: Some("Title & <Special>".into()),
        legend_position: None,
        data_labels: false,
    };
    {
        let mut slide = p.slide_mut(0).unwrap();
        slide
            .add_chart(&spec, Emu(0), Emu(0), Emu(5000000), Emu(3000000), 1)
            .unwrap();
    }
    let bytes = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(bytes)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    // Verify XML escaping.
    assert!(chart_xml.contains("A &amp; B"));
    assert!(chart_xml.contains("&lt;C&gt;"));
    assert!(chart_xml.contains("D&quot;E"));
    assert!(chart_xml.contains("Title &amp; &lt;Special&gt;"));
}

#[test]
fn error_on_empty_categories() {
    let mut p = Presentation::new();
    p.add_slide(Layout::Blank);
    let spec = ChartSpec {
        kind: ChartKind::ClusteredBar,
        categories: vec![],
        series: vec![("S".into(), vec![1.0])],
        title: None,
        legend_position: None,
        data_labels: false,
    };
    let mut slide = p.slide_mut(0).unwrap();
    let result = slide.add_chart(&spec, Emu(0), Emu(0), Emu(5000000), Emu(3000000), 1);
    assert!(result.is_err(), "should error on empty categories");
}

#[test]
fn error_on_empty_series() {
    let mut p = Presentation::new();
    p.add_slide(Layout::Blank);
    let spec = ChartSpec {
        kind: ChartKind::ClusteredBar,
        categories: vec!["A".into()],
        series: vec![],
        title: None,
        legend_position: None,
        data_labels: false,
    };
    let mut slide = p.slide_mut(0).unwrap();
    let result = slide.add_chart(&spec, Emu(0), Emu(0), Emu(5000000), Emu(3000000), 1);
    assert!(result.is_err(), "should error on empty series");
}

// ============================================================================
// Chart data editing tests (Task 12.5 — Requirements 5.1, 5.2)
// ============================================================================

use zavora_slide::ChartDataUpdate;

/// Helper: create a deck with a chart, save, reopen, and return the bytes.
fn deck_with_chart_opened(kind: ChartKind) -> Vec<u8> {
    deck_with_chart(kind, Some("Original Title"))
}

#[test]
fn update_chart_data_replaces_categories_and_values() {
    // Requirement 5.1: replace categories/series values, updating chart XML and workbook.
    let bytes = deck_with_chart_opened(ChartKind::ClusteredBar);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["Jan".into(), "Feb".into(), "Mar".into()],
        series: vec![
            ("Sales".into(), vec![10.0, 20.0, 30.0]),
            ("Profit".into(), vec![5.0, 10.0, 15.0]),
        ],
    };
    p.update_chart_data(0, 0, &update).unwrap();

    let saved = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(saved)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    // New categories present.
    assert!(
        chart_xml.contains("<c:v>Jan</c:v>"),
        "should contain new category Jan"
    );
    assert!(
        chart_xml.contains("<c:v>Feb</c:v>"),
        "should contain new category Feb"
    );
    assert!(
        chart_xml.contains("<c:v>Mar</c:v>"),
        "should contain new category Mar"
    );

    // Old categories gone.
    assert!(
        !chart_xml.contains("<c:v>Q1</c:v>"),
        "old category Q1 should be gone"
    );
    assert!(
        !chart_xml.contains("<c:v>Q4</c:v>"),
        "old category Q4 should be gone"
    );

    // New series names.
    assert!(
        chart_xml.contains("<c:v>Sales</c:v>"),
        "should contain new series name Sales"
    );
    assert!(
        chart_xml.contains("<c:v>Profit</c:v>"),
        "should contain new series name Profit"
    );

    // New values.
    assert!(
        chart_xml.contains("<c:v>10</c:v>"),
        "should contain value 10"
    );
    assert!(
        chart_xml.contains("<c:v>20</c:v>"),
        "should contain value 20"
    );
    assert!(
        chart_xml.contains("<c:v>30</c:v>"),
        "should contain value 30"
    );
    assert!(
        chart_xml.contains("<c:v>15</c:v>"),
        "should contain value 15"
    );
}

#[test]
fn update_chart_data_preserves_chart_structure() {
    // Requirement 5.2: unmodeled chart features (styling, effects) preserved.
    let bytes = deck_with_chart_opened(ChartKind::ClusteredBar);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["X".into(), "Y".into()],
        series: vec![("S1".into(), vec![1.0, 2.0])],
    };
    p.update_chart_data(0, 0, &update).unwrap();

    let saved = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(saved)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    // Chart structure preserved.
    assert!(
        chart_xml.contains("<c:barChart>"),
        "barChart element preserved"
    );
    assert!(chart_xml.contains("barDir val=\"bar\""), "barDir preserved");
    assert!(
        chart_xml.contains("grouping val=\"clustered\""),
        "grouping preserved"
    );
    assert!(chart_xml.contains("<c:catAx>"), "catAx preserved");
    assert!(chart_xml.contains("<c:valAx>"), "valAx preserved");
    assert!(
        chart_xml.contains("<c:externalData"),
        "externalData preserved"
    );

    // Title preserved.
    assert!(
        chart_xml.contains("Original Title"),
        "title preserved on edit"
    );
}

#[test]
fn update_chart_data_updates_workbook_consistently() {
    // Requirement 5.1: workbook updated consistently with chart XML.
    let bytes = deck_with_chart_opened(ChartKind::ClusteredBar);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["Alpha".into(), "Beta".into()],
        series: vec![("Revenue".into(), vec![500.0, 600.0])],
    };
    p.update_chart_data(0, 0, &update).unwrap();

    let saved = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(saved)).unwrap();
    let xlsx_bytes = pkg
        .get_part("/ppt/embeddings/Microsoft_Excel_Worksheet1.xlsx")
        .unwrap();

    // Verify the workbook contains the new data.
    let cursor = Cursor::new(xlsx_bytes);
    let mut archive = zip::ZipArchive::new(cursor).expect("workbook should be valid ZIP");
    let mut sheet = archive.by_name("xl/worksheets/sheet1.xml").unwrap();
    let mut sheet_xml = String::new();
    std::io::Read::read_to_string(&mut sheet, &mut sheet_xml).unwrap();

    assert!(
        sheet_xml.contains("<v>500</v>"),
        "workbook should contain value 500"
    );
    assert!(
        sheet_xml.contains("<v>600</v>"),
        "workbook should contain value 600"
    );
}

#[test]
fn update_chart_data_line_chart() {
    let bytes = deck_with_chart(ChartKind::Line, None);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["A".into(), "B".into()],
        series: vec![("Trend".into(), vec![7.0, 14.0])],
    };
    p.update_chart_data(0, 0, &update).unwrap();

    let saved = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(saved)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(chart_xml.contains("<c:lineChart>"), "lineChart preserved");
    assert!(chart_xml.contains("<c:v>A</c:v>"), "new category A");
    assert!(chart_xml.contains("<c:v>B</c:v>"), "new category B");
    assert!(chart_xml.contains("<c:v>7</c:v>"), "new value 7");
    assert!(chart_xml.contains("<c:v>14</c:v>"), "new value 14");
    assert!(chart_xml.contains("<c:v>Trend</c:v>"), "new series name");
}

#[test]
fn update_chart_data_scatter_chart() {
    let bytes = deck_with_chart(ChartKind::Scatter, None);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["10".into(), "20".into()],
        series: vec![("Points".into(), vec![3.5, 7.0])],
    };
    p.update_chart_data(0, 0, &update).unwrap();

    let saved = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(saved)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("<c:scatterChart>"),
        "scatterChart preserved"
    );
    // Scatter uses xVal/yVal.
    assert!(chart_xml.contains("<c:xVal>"), "xVal present");
    assert!(chart_xml.contains("<c:yVal>"), "yVal present");
    assert!(chart_xml.contains("<c:v>10</c:v>"), "new xVal category 10");
    assert!(chart_xml.contains("<c:v>20</c:v>"), "new xVal category 20");
    assert!(chart_xml.contains("<c:v>3.5</c:v>"), "new yVal 3.5");
    assert!(chart_xml.contains("<c:v>7</c:v>"), "new yVal 7");
}

#[test]
fn update_chart_data_pie_chart() {
    let bytes = deck_with_chart(ChartKind::Pie, None);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["Red".into(), "Blue".into(), "Green".into()],
        series: vec![("Share".into(), vec![50.0, 30.0, 20.0])],
    };
    p.update_chart_data(0, 0, &update).unwrap();

    let saved = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(saved)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(chart_xml.contains("<c:pieChart>"), "pieChart preserved");
    assert!(chart_xml.contains("<c:v>Red</c:v>"), "new category Red");
    assert!(chart_xml.contains("<c:v>Blue</c:v>"), "new category Blue");
    assert!(chart_xml.contains("<c:v>50</c:v>"), "new value 50");
    assert!(chart_xml.contains("<c:v>30</c:v>"), "new value 30");
}

#[test]
fn update_chart_data_error_on_new_deck() {
    // update_chart_data requires an opened deck (source package).
    let mut p = Presentation::new();
    p.add_slide(Layout::Blank);
    let update = ChartDataUpdate {
        categories: vec!["A".into()],
        series: vec![("S".into(), vec![1.0])],
    };
    let result = p.update_chart_data(0, 0, &update);
    assert!(result.is_err(), "should error on a new (non-opened) deck");
}

#[test]
fn update_chart_data_error_on_invalid_slide_index() {
    let bytes = deck_with_chart_opened(ChartKind::ClusteredBar);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["A".into()],
        series: vec![("S".into(), vec![1.0])],
    };
    let result = p.update_chart_data(99, 0, &update);
    assert!(result.is_err(), "should error on invalid slide index");
}

#[test]
fn update_chart_data_error_on_invalid_chart_index() {
    let bytes = deck_with_chart_opened(ChartKind::ClusteredBar);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["A".into()],
        series: vec![("S".into(), vec![1.0])],
    };
    let result = p.update_chart_data(0, 5, &update);
    assert!(result.is_err(), "should error on invalid chart index");
}

#[test]
fn update_chart_data_error_on_empty_categories() {
    let bytes = deck_with_chart_opened(ChartKind::ClusteredBar);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec![],
        series: vec![("S".into(), vec![1.0])],
    };
    let result = p.update_chart_data(0, 0, &update);
    assert!(result.is_err(), "should error on empty categories");
}

#[test]
fn update_chart_data_error_on_empty_series() {
    let bytes = deck_with_chart_opened(ChartKind::ClusteredBar);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["A".into()],
        series: vec![],
    };
    let result = p.update_chart_data(0, 0, &update);
    assert!(result.is_err(), "should error on empty series");
}

#[test]
fn update_chart_data_fewer_series_removes_extras() {
    // Original has 2 series; update with 1 → extra series removed.
    let bytes = deck_with_chart_opened(ChartKind::ClusteredBar);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["X".into()],
        series: vec![("Only".into(), vec![42.0])],
    };
    p.update_chart_data(0, 0, &update).unwrap();

    let saved = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(saved)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(
        chart_xml.contains("<c:v>Only</c:v>"),
        "single series name present"
    );
    // Old series names should be gone.
    assert!(
        !chart_xml.contains("<c:v>Revenue</c:v>"),
        "old series Revenue removed"
    );
    assert!(
        !chart_xml.contains("<c:v>Costs</c:v>"),
        "old series Costs removed"
    );
}

#[test]
fn update_chart_data_more_series_adds_new_ones() {
    // Original has 2 series; update with 3 → new series added.
    let bytes = deck_with_chart_opened(ChartKind::ClusteredBar);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["A".into(), "B".into()],
        series: vec![
            ("S1".into(), vec![1.0, 2.0]),
            ("S2".into(), vec![3.0, 4.0]),
            ("S3".into(), vec![5.0, 6.0]),
        ],
    };
    p.update_chart_data(0, 0, &update).unwrap();

    let saved = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(saved)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    assert!(chart_xml.contains("<c:v>S1</c:v>"), "series S1 present");
    assert!(chart_xml.contains("<c:v>S2</c:v>"), "series S2 present");
    assert!(chart_xml.contains("<c:v>S3</c:v>"), "series S3 present");
    assert!(chart_xml.contains("<c:v>5</c:v>"), "value 5 present");
    assert!(chart_xml.contains("<c:v>6</c:v>"), "value 6 present");
}

#[test]
fn update_chart_data_formula_refs_updated() {
    let bytes = deck_with_chart_opened(ChartKind::ClusteredBar);
    let mut p = Presentation::open_from_bytes(&bytes).unwrap();

    let update = ChartDataUpdate {
        categories: vec!["One".into(), "Two".into(), "Three".into()],
        series: vec![("Data".into(), vec![10.0, 20.0, 30.0])],
    };
    p.update_chart_data(0, 0, &update).unwrap();

    let saved = p.save_to_buffer().unwrap();
    let pkg = OpcPackage::from_reader(Cursor::new(saved)).unwrap();
    let chart_xml = String::from_utf8_lossy(pkg.get_part("/ppt/charts/chart1.xml").unwrap());

    // Formula references should reflect 3 categories (rows 2-4).
    assert!(
        chart_xml.contains("Sheet1!$A$2:$A$4"),
        "category formula should reference A2:A4 for 3 categories"
    );
    assert!(
        chart_xml.contains("Sheet1!$B$2:$B$4"),
        "value formula should reference B2:B4 for series 1"
    );
}

// ============================================================================
// LibreOffice load gate (env-guarded) — Requirement 26.3
// ============================================================================

mod test_util;

#[test]
fn libreoffice_load_gate_all_chart_types() {
    // Env-guarded: only runs when ZAVORA_LIBREOFFICE_GATE=1.
    for kind in [
        ChartKind::ClusteredBar,
        ChartKind::StackedBar,
        ChartKind::ClusteredColumn,
        ChartKind::StackedColumn,
        ChartKind::Line,
        ChartKind::Pie,
        ChartKind::Doughnut,
        ChartKind::Area,
        ChartKind::Scatter,
    ] {
        let bytes = deck_with_chart(kind, Some("LO Gate Test"));
        test_util::libreoffice_load_gate(&bytes)
            .unwrap_or_else(|e| panic!("LibreOffice load gate failed for {:?}: {e}", kind));
    }
}
