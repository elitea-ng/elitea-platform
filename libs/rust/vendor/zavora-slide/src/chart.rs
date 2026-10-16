//! Chart authoring and editing: bar/column chart parts, embedded workbook,
//! relationships, content types, and data update (Part B — Requirements 4.1–5.2).
//!
//! Adds a chart as a `p:graphicFrame` with a chart part (`ppt/charts/chartN.xml`)
//! and an embedded `.xlsx` workbook, wired with correct relationships and content
//! types. Supports editing chart data on opened decks via the lossless DOM.

use crate::error::{Result, SlideError};
use crate::slide::SlideData;
use zavora_slide_oxml::{Document, Element, Node};

/// The kind of chart to create.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum ChartKind {
    /// `c:barChart` with `barDir="bar"` and `grouping="clustered"`.
    ClusteredBar,
    /// `c:barChart` with `barDir="bar"` and `grouping="stacked"`.
    StackedBar,
    /// `c:barChart` with `barDir="col"` and `grouping="clustered"`.
    ClusteredColumn,
    /// `c:barChart` with `barDir="col"` and `grouping="stacked"`.
    StackedColumn,
    /// `c:lineChart` with `grouping="standard"`.
    Line,
    /// `c:pieChart` with `varyColors="1"`.
    Pie,
    /// `c:doughnutChart` with `varyColors="1"` and `holeSize="50"`.
    Doughnut,
    /// `c:areaChart` with `grouping="standard"`.
    Area,
    /// `c:scatterChart` with `scatterStyle="lineMarker"`.
    Scatter,
}

impl ChartKind {
    /// The `barDir` attribute value for bar/column chart kinds.
    /// Returns `None` for non-bar chart types.
    pub fn bar_dir(&self) -> &'static str {
        match self {
            ChartKind::ClusteredBar | ChartKind::StackedBar => "bar",
            ChartKind::ClusteredColumn | ChartKind::StackedColumn => "col",
            _ => "",
        }
    }

    /// The `grouping` attribute value for this chart kind.
    /// Returns an empty string for chart types that don't use grouping.
    pub fn grouping(&self) -> &'static str {
        match self {
            ChartKind::ClusteredBar | ChartKind::ClusteredColumn => "clustered",
            ChartKind::StackedBar | ChartKind::StackedColumn => "stacked",
            ChartKind::Line | ChartKind::Area => "standard",
            _ => "",
        }
    }

    /// Whether this chart kind uses category + value axes.
    fn has_cat_val_axes(&self) -> bool {
        matches!(
            self,
            ChartKind::ClusteredBar
                | ChartKind::StackedBar
                | ChartKind::ClusteredColumn
                | ChartKind::StackedColumn
                | ChartKind::Line
                | ChartKind::Area
        )
    }

    /// Whether this chart kind uses two value axes (scatter).
    fn has_two_val_axes(&self) -> bool {
        matches!(self, ChartKind::Scatter)
    }
}

/// Specification for a chart to add to a slide.
#[derive(Debug, Clone)]
pub struct ChartSpec {
    /// The type of chart.
    pub kind: ChartKind,
    /// Category labels (x-axis).
    pub categories: Vec<String>,
    /// Named data series: `(name, values)`.
    pub series: Vec<(String, Vec<f64>)>,
    /// Optional chart title.
    pub title: Option<String>,
    /// Legend position: "b" (bottom), "t" (top), "l" (left), "r" (right), "tr" (top-right).
    /// When `Some`, emits `<c:legend>` after `</c:plotArea>`.
    pub legend_position: Option<String>,
    /// When true, emit `<c:dLbls><c:showVal val="1"/></c:dLbls>` inside each series.
    pub data_labels: bool,
}

/// A pending chart to be written during package save.
#[derive(Debug, Clone)]
pub(crate) struct ChartEntry {
    /// Chart part path (e.g. "/ppt/charts/chart1.xml").
    pub part_path: String,
    /// Embedded workbook part path (e.g. "/ppt/embeddings/Microsoft_Excel_Worksheet1.xlsx").
    pub workbook_path: String,
    /// The chart XML bytes.
    pub chart_xml: Vec<u8>,
    /// The embedded workbook bytes (.xlsx).
    pub workbook_bytes: Vec<u8>,
    /// Relationship ID linking the slide to this chart part.
    pub slide_r_id: String,
}

/// Content type for chart parts.
pub(crate) const CT_CHART: &str =
    "application/vnd.openxmlformats-officedocument.drawingml.chart+xml";

/// Relationship type for chart → embedded workbook (package relationship).
const RT_PACKAGE: &str =
    "http://schemas.openxmlformats.org/officeDocument/2006/relationships/package";

/// Add a chart to a slide's data model. The chart is materialized as parts
/// during package save.
///
/// # Arguments
/// * `slide` - The slide data to add the chart to
/// * `spec` - The chart specification
/// * `x`, `y` - Position in EMU
/// * `cx`, `cy` - Size in EMU
/// * `chart_index` - The 1-based chart index for naming parts
pub(crate) fn add_chart_to_slide(
    slide: &mut SlideData,
    spec: &ChartSpec,
    x: i64,
    y: i64,
    cx: i64,
    cy: i64,
    chart_index: usize,
) -> Result<()> {
    if spec.categories.is_empty() {
        return Err(SlideError::InvalidInput(
            "chart must have at least one category".into(),
        ));
    }
    if spec.series.is_empty() {
        return Err(SlideError::InvalidInput(
            "chart must have at least one series".into(),
        ));
    }

    let chart_part = format!("/ppt/charts/chart{chart_index}.xml");
    let workbook_part = format!("/ppt/embeddings/Microsoft_Excel_Worksheet{chart_index}.xlsx");

    // Relationship ID for slide → chart (use rId20+ to avoid collisions with
    // layout/notes/images).
    let slide_r_id = format!("rId{}", 20 + slide.charts.len());

    // Build the embedded workbook.
    let cat_refs: Vec<&str> = spec.categories.iter().map(|s| s.as_str()).collect();
    let series_refs: Vec<(&str, Vec<f64>)> = spec
        .series
        .iter()
        .map(|(name, vals)| (name.as_str(), vals.clone()))
        .collect();
    let workbook_bytes = zavora_slide_xlsx::WorkbookBuilder::new()
        .set_data(&cat_refs, &series_refs)
        .build()
        .map_err(|e| SlideError::InvalidInput(format!("xlsx build failed: {e}")))?;

    // Build the chart XML.
    let chart_xml = build_chart_xml(spec, &workbook_part);

    // Build the graphicFrame XML for the slide's spTree.
    let shape_id = slide.alloc_id();
    let graphic_frame_xml = build_graphic_frame_xml(shape_id, &slide_r_id, x, y, cx, cy);

    // Store the chart entry for package save.
    slide.charts.push(ChartEntry {
        part_path: chart_part,
        workbook_path: workbook_part,
        chart_xml: chart_xml.into_bytes(),
        workbook_bytes,
        slide_r_id,
    });

    // Store the graphic frame XML snippet for slide serialization.
    slide.chart_frames.push(graphic_frame_xml);

    // Store a chart placeholder for rendering in the Scene.
    slide
        .chart_placeholders
        .push(crate::slide::ChartPlaceholder {
            x,
            y,
            cx,
            cy,
            title: spec.title.clone(),
        });

    Ok(())
}

/// Build the chart XML (`c:chartSpace` document).
fn build_chart_xml(spec: &ChartSpec, _workbook_rel_path: &str) -> String {
    let mut xml = String::from(
        r#"<?xml version="1.0" encoding="UTF-8" standalone="yes"?>
<c:chartSpace xmlns:c="http://schemas.openxmlformats.org/drawingml/2006/chart" xmlns:a="http://schemas.openxmlformats.org/drawingml/2006/main" xmlns:r="http://schemas.openxmlformats.org/officeDocument/2006/relationships">
<c:chart>"#,
    );

    // Optional title.
    if let Some(title) = &spec.title {
        xml.push_str(&format!(
            r#"<c:title><c:tx><c:rich><a:bodyPr/><a:lstStyle/><a:p><a:r><a:t>{}</a:t></a:r></a:p></c:rich></c:tx><c:overlay val="0"/></c:title>"#,
            xml_escape(title)
        ));
    }

    xml.push_str("<c:autoTitleDeleted val=\"0\"/>");
    xml.push_str("<c:plotArea><c:layout/>");

    // Emit the chart-type-specific element.
    build_chart_type_xml(&mut xml, spec);

    // Axes (after the chart type element, inside plotArea).
    if spec.kind.has_cat_val_axes() {
        // Category axis + value axis.
        xml.push_str(
            "<c:catAx><c:axId val=\"1\"/><c:scaling><c:orientation val=\"minMax\"/></c:scaling>\
             <c:delete val=\"0\"/><c:axPos val=\"b\"/><c:crossAx val=\"2\"/></c:catAx>",
        );
        xml.push_str(
            "<c:valAx><c:axId val=\"2\"/><c:scaling><c:orientation val=\"minMax\"/></c:scaling>\
             <c:delete val=\"0\"/><c:axPos val=\"l\"/><c:crossAx val=\"1\"/></c:valAx>",
        );
    } else if spec.kind.has_two_val_axes() {
        // Scatter: two value axes.
        xml.push_str(
            "<c:valAx><c:axId val=\"1\"/><c:scaling><c:orientation val=\"minMax\"/></c:scaling>\
             <c:delete val=\"0\"/><c:axPos val=\"b\"/><c:crossAx val=\"2\"/></c:valAx>",
        );
        xml.push_str(
            "<c:valAx><c:axId val=\"2\"/><c:scaling><c:orientation val=\"minMax\"/></c:scaling>\
             <c:delete val=\"0\"/><c:axPos val=\"l\"/><c:crossAx val=\"1\"/></c:valAx>",
        );
    }
    // Pie/Doughnut: no axes.

    xml.push_str("</c:plotArea>");

    // Legend (after plotArea, before plotVisOnly).
    if let Some(pos) = &spec.legend_position {
        xml.push_str(&format!(
            "<c:legend><c:legendPos val=\"{}\"/><c:overlay val=\"0\"/></c:legend>",
            xml_escape(pos)
        ));
    }

    xml.push_str("<c:plotVisOnly val=\"1\"/><c:dispBlanksAs val=\"gap\"/>");
    xml.push_str("</c:chart>");

    // External data reference (link to embedded workbook).
    xml.push_str("<c:externalData r:id=\"rId1\"><c:autoUpdate val=\"0\"/></c:externalData>");

    xml.push_str("</c:chartSpace>");
    xml
}

/// Emit the chart-type-specific XML element (barChart, lineChart, etc.) with series.
fn build_chart_type_xml(xml: &mut String, spec: &ChartSpec) {
    match spec.kind {
        ChartKind::ClusteredBar
        | ChartKind::StackedBar
        | ChartKind::ClusteredColumn
        | ChartKind::StackedColumn => {
            xml.push_str(&format!(
                "<c:barChart><c:barDir val=\"{}\"/><c:grouping val=\"{}\"/><c:varyColors val=\"0\"/>",
                spec.kind.bar_dir(),
                spec.kind.grouping()
            ));
            build_cat_val_series(xml, spec);
            xml.push_str("<c:axId val=\"1\"/><c:axId val=\"2\"/>");
            xml.push_str("</c:barChart>");
        }
        ChartKind::Line => {
            xml.push_str("<c:lineChart><c:grouping val=\"standard\"/><c:varyColors val=\"0\"/>");
            build_cat_val_series(xml, spec);
            xml.push_str("<c:axId val=\"1\"/><c:axId val=\"2\"/>");
            xml.push_str("</c:lineChart>");
        }
        ChartKind::Pie => {
            xml.push_str("<c:pieChart><c:varyColors val=\"1\"/>");
            build_cat_val_series(xml, spec);
            xml.push_str("</c:pieChart>");
        }
        ChartKind::Doughnut => {
            xml.push_str("<c:doughnutChart><c:varyColors val=\"1\"/>");
            build_cat_val_series(xml, spec);
            xml.push_str("<c:holeSize val=\"50\"/>");
            xml.push_str("</c:doughnutChart>");
        }
        ChartKind::Area => {
            xml.push_str("<c:areaChart><c:grouping val=\"standard\"/><c:varyColors val=\"0\"/>");
            build_cat_val_series(xml, spec);
            xml.push_str("<c:axId val=\"1\"/><c:axId val=\"2\"/>");
            xml.push_str("</c:areaChart>");
        }
        ChartKind::Scatter => {
            xml.push_str(
                "<c:scatterChart><c:scatterStyle val=\"lineMarker\"/><c:varyColors val=\"0\"/>",
            );
            build_scatter_series(xml, spec);
            xml.push_str("<c:axId val=\"1\"/><c:axId val=\"2\"/>");
            xml.push_str("</c:scatterChart>");
        }
    }
}

/// Build series elements using `c:cat` + `c:val` (for bar, line, pie, doughnut, area).
fn build_cat_val_series(xml: &mut String, spec: &ChartSpec) {
    let num_cats = spec.categories.len();
    for (idx, (name, values)) in spec.series.iter().enumerate() {
        xml.push_str(&format!(
            "<c:ser><c:idx val=\"{idx}\"/><c:order val=\"{idx}\"/>"
        ));
        // Series name.
        xml.push_str(&format!(
            "<c:tx><c:strRef><c:f>Sheet1!${}$1</c:f><c:strCache><c:ptCount val=\"1\"/><c:pt idx=\"0\"><c:v>{}</c:v></c:pt></c:strCache></c:strRef></c:tx>",
            col_letter(idx + 1),
            xml_escape(name)
        ));
        // Data labels (per-series).
        if spec.data_labels {
            xml.push_str("<c:dLbls><c:showVal val=\"1\"/></c:dLbls>");
        }
        // Categories.
        xml.push_str("<c:cat><c:strRef><c:f>Sheet1!$A$2:$A$");
        xml.push_str(&(num_cats + 1).to_string());
        xml.push_str("</c:f><c:strCache>");
        xml.push_str(&format!("<c:ptCount val=\"{num_cats}\"/>"));
        for (ci, cat) in spec.categories.iter().enumerate() {
            xml.push_str(&format!(
                "<c:pt idx=\"{ci}\"><c:v>{}</c:v></c:pt>",
                xml_escape(cat)
            ));
        }
        xml.push_str("</c:strCache></c:strRef></c:cat>");
        // Values.
        let col = col_letter(idx + 1);
        xml.push_str(&format!(
            "<c:val><c:numRef><c:f>Sheet1!${col}$2:${col}${}</c:f><c:numCache>",
            num_cats + 1
        ));
        xml.push_str(&format!(
            "<c:formatCode>General</c:formatCode><c:ptCount val=\"{num_cats}\"/>"
        ));
        for (vi, val) in values.iter().enumerate() {
            xml.push_str(&format!("<c:pt idx=\"{vi}\"><c:v>{val}</c:v></c:pt>"));
        }
        xml.push_str("</c:numCache></c:numRef></c:val>");
        xml.push_str("</c:ser>");
    }
}

/// Build series elements using `c:xVal` + `c:yVal` (for scatter charts).
fn build_scatter_series(xml: &mut String, spec: &ChartSpec) {
    let num_cats = spec.categories.len();
    for (idx, (name, values)) in spec.series.iter().enumerate() {
        xml.push_str(&format!(
            "<c:ser><c:idx val=\"{idx}\"/><c:order val=\"{idx}\"/>"
        ));
        // Series name.
        xml.push_str(&format!(
            "<c:tx><c:strRef><c:f>Sheet1!${}$1</c:f><c:strCache><c:ptCount val=\"1\"/><c:pt idx=\"0\"><c:v>{}</c:v></c:pt></c:strCache></c:strRef></c:tx>",
            col_letter(idx + 1),
            xml_escape(name)
        ));
        // Data labels (per-series).
        if spec.data_labels {
            xml.push_str("<c:dLbls><c:showVal val=\"1\"/></c:dLbls>");
        }
        // xVal — categories as numeric references (scatter uses numeric x-axis).
        xml.push_str("<c:xVal><c:strRef><c:f>Sheet1!$A$2:$A$");
        xml.push_str(&(num_cats + 1).to_string());
        xml.push_str("</c:f><c:strCache>");
        xml.push_str(&format!("<c:ptCount val=\"{num_cats}\"/>"));
        for (ci, cat) in spec.categories.iter().enumerate() {
            xml.push_str(&format!(
                "<c:pt idx=\"{ci}\"><c:v>{}</c:v></c:pt>",
                xml_escape(cat)
            ));
        }
        xml.push_str("</c:strCache></c:strRef></c:xVal>");
        // yVal — numeric values.
        let col = col_letter(idx + 1);
        xml.push_str(&format!(
            "<c:yVal><c:numRef><c:f>Sheet1!${col}$2:${col}${}</c:f><c:numCache>",
            num_cats + 1
        ));
        xml.push_str(&format!(
            "<c:formatCode>General</c:formatCode><c:ptCount val=\"{num_cats}\"/>"
        ));
        for (vi, val) in values.iter().enumerate() {
            xml.push_str(&format!("<c:pt idx=\"{vi}\"><c:v>{val}</c:v></c:pt>"));
        }
        xml.push_str("</c:numCache></c:numRef></c:yVal>");
        xml.push_str("</c:ser>");
    }
}

/// Build the `p:graphicFrame` XML for embedding a chart in the slide's spTree.
fn build_graphic_frame_xml(shape_id: u32, r_id: &str, x: i64, y: i64, cx: i64, cy: i64) -> String {
    format!(
        "<p:graphicFrame>\
         <p:nvGraphicFramePr>\
         <p:cNvPr id=\"{shape_id}\" name=\"Chart {shape_id}\"/>\
         <p:cNvGraphicFramePr><a:graphicFrameLocks noGrp=\"1\"/></p:cNvGraphicFramePr>\
         <p:nvPr/>\
         </p:nvGraphicFramePr>\
         <p:xfrm><a:off x=\"{x}\" y=\"{y}\"/><a:ext cx=\"{cx}\" cy=\"{cy}\"/></p:xfrm>\
         <a:graphic>\
         <a:graphicData uri=\"http://schemas.openxmlformats.org/drawingml/2006/chart\">\
         <c:chart xmlns:c=\"http://schemas.openxmlformats.org/drawingml/2006/chart\" \
         xmlns:r=\"http://schemas.openxmlformats.org/officeDocument/2006/relationships\" \
         r:id=\"{r_id}\"/>\
         </a:graphicData>\
         </a:graphic>\
         </p:graphicFrame>"
    )
}

/// Write chart-related parts, relationships, and content types into the package
/// during save. Called from `build_package`.
pub(crate) fn write_chart_parts(
    pkg: &mut zavora_slide_opc::OpcPackage,
    slide_part: &str,
    charts: &[ChartEntry],
) {
    for chart in charts {
        // Chart part.
        pkg.set_part(&chart.part_path, chart.chart_xml.clone());
        pkg.content_types.add_override(&chart.part_path, CT_CHART);

        // Embedded workbook part.
        pkg.set_part(&chart.workbook_path, chart.workbook_bytes.clone());
        // xlsx default content type.
        pkg.content_types.add_default(
            "xlsx",
            "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet",
        );

        // Slide → chart relationship.
        let slide_rels = pkg.get_or_create_part_rels(slide_part);
        let chart_target = relative_path(slide_part, &chart.part_path);
        slide_rels.add_with_id(
            &chart.slide_r_id,
            zavora_slide_opc::rel_types::CHART,
            &chart_target,
        );

        // Chart → workbook relationship (package type).
        let chart_rels = pkg.get_or_create_part_rels(&chart.part_path);
        let wb_target = relative_path(&chart.part_path, &chart.workbook_path);
        chart_rels.add_with_id("rId1", RT_PACKAGE, &wb_target);
    }
}

/// Specification for updating a chart's data on an opened deck.
///
/// Replaces the chart's categories and series values while preserving all other
/// chart XML (styling, effects, formatting) via the lossless DOM.
#[derive(Debug, Clone)]
pub struct ChartDataUpdate {
    /// New category labels (x-axis).
    pub categories: Vec<String>,
    /// New named data series: `(name, values)`.
    pub series: Vec<(String, Vec<f64>)>,
}

/// Update a chart's data in an opened deck's source package.
///
/// Locates the chart part via the slide's relationships, parses the chart XML
/// using the lossless DOM, updates the `c:cat`/`c:val` (or `c:xVal`/`c:yVal`)
/// cache values in the series elements, and rebuilds the embedded workbook.
/// All other chart XML (styling, effects, formatting) is preserved byte-for-byte
/// via the DOM.
///
/// # Arguments
/// * `pkg` - The OPC package (source) to edit in place
/// * `slide_part` - The slide part path (e.g. "/ppt/slides/slide1.xml")
/// * `chart_idx` - 0-based index of the chart relationship on this slide
/// * `update` - The new data to apply
pub(crate) fn update_chart_data(
    pkg: &mut zavora_slide_opc::OpcPackage,
    slide_part: &str,
    chart_idx: usize,
    update: &ChartDataUpdate,
) -> Result<()> {
    if update.categories.is_empty() {
        return Err(SlideError::InvalidInput(
            "chart update must have at least one category".into(),
        ));
    }
    if update.series.is_empty() {
        return Err(SlideError::InvalidInput(
            "chart update must have at least one series".into(),
        ));
    }

    // 1. Locate the chart part via the slide's relationships.
    let chart_rels: Vec<(String, String)> = pkg
        .get_part_rels(slide_part)
        .map(|rels| {
            rels.get_all_by_type(zavora_slide_opc::rel_types::CHART)
                .into_iter()
                .map(|r| (r.id.clone(), r.target.clone()))
                .collect()
        })
        .unwrap_or_default();

    let (_chart_rid, chart_target) = chart_rels.get(chart_idx).ok_or_else(|| {
        SlideError::NotFound(format!("chart index {chart_idx} on slide {slide_part}"))
    })?;

    let chart_part_path = normalize_chart_path(&zavora_slide_opc::OpcPackage::resolve_rel_target(
        slide_part,
        chart_target,
    ));

    // 2. Parse the chart XML via the lossless DOM.
    let chart_bytes = pkg
        .get_part(&chart_part_path)
        .ok_or_else(|| SlideError::NotFound(format!("chart part {chart_part_path}")))?
        .to_vec();

    let mut doc = Document::parse(&chart_bytes)
        .map_err(|e| SlideError::Unsupported(format!("chart XML parse: {e}")))?;

    // 3. Update the series data in the DOM.
    update_series_in_dom(&mut doc, update)?;

    // 4. Rebuild the embedded workbook with the new data.
    let workbook_part_path = find_workbook_part(pkg, &chart_part_path)?;
    let cat_refs: Vec<&str> = update.categories.iter().map(|s| s.as_str()).collect();
    let series_refs: Vec<(&str, Vec<f64>)> = update
        .series
        .iter()
        .map(|(name, vals)| (name.as_str(), vals.clone()))
        .collect();
    let workbook_bytes = zavora_slide_xlsx::WorkbookBuilder::new()
        .set_data(&cat_refs, &series_refs)
        .build()
        .map_err(|e| SlideError::InvalidInput(format!("xlsx rebuild failed: {e}")))?;

    // 5. Write updated chart XML and workbook back to the package.
    pkg.set_part(&chart_part_path, doc.to_bytes());
    pkg.set_part(&workbook_part_path, workbook_bytes);

    Ok(())
}

/// Find the embedded workbook part path from the chart's relationships.
fn find_workbook_part(pkg: &zavora_slide_opc::OpcPackage, chart_part_path: &str) -> Result<String> {
    let chart_rels = pkg.get_part_rels(chart_part_path).ok_or_else(|| {
        SlideError::NotFound(format!("chart relationships for {chart_part_path}"))
    })?;

    let wb_rel = chart_rels
        .get_by_type(RT_PACKAGE)
        .ok_or_else(|| SlideError::NotFound("embedded workbook relationship in chart".into()))?;

    Ok(normalize_chart_path(
        &zavora_slide_opc::OpcPackage::resolve_rel_target(chart_part_path, &wb_rel.target),
    ))
}

/// Update the series elements in the chart DOM with new categories and values.
///
/// Handles both cat/val charts (bar, line, pie, area) and xVal/yVal charts (scatter).
/// Preserves all other elements (styling, effects, formatting) in the DOM.
fn update_series_in_dom(doc: &mut Document, update: &ChartDataUpdate) -> Result<()> {
    let root = doc
        .root_mut()
        .ok_or_else(|| SlideError::Unsupported("chart XML has no root element".into()))?;

    // Find c:chart > c:plotArea > chart-type element > c:ser elements.
    // We need to traverse: chartSpace > chart > plotArea > (barChart|lineChart|...) > ser
    let chart_el = find_child_mut(root, b"chart")
        .ok_or_else(|| SlideError::Unsupported("no c:chart element found".into()))?;
    let plot_area = find_child_mut(chart_el, b"plotArea")
        .ok_or_else(|| SlideError::Unsupported("no c:plotArea element found".into()))?;

    // Find the chart type element (barChart, lineChart, pieChart, etc.)
    let chart_type_el = find_chart_type_element_mut(plot_area)
        .ok_or_else(|| SlideError::Unsupported("no chart type element found in plotArea".into()))?;

    // Determine if this is a scatter chart (uses xVal/yVal instead of cat/val).
    let is_scatter = chart_type_el.local_name() == b"scatterChart";

    // Iterate over c:ser elements and update their data.
    let ser_elements: Vec<usize> = chart_type_el
        .children
        .iter()
        .enumerate()
        .filter_map(|(i, n)| match n {
            Node::Element(e) if e.local_name() == b"ser" => Some(i),
            _ => None,
        })
        .collect();

    for (ser_idx, &child_idx) in ser_elements.iter().enumerate() {
        if ser_idx >= update.series.len() {
            break;
        }
        let (ref name, ref values) = update.series[ser_idx];
        let ser_el = match &mut chart_type_el.children[child_idx] {
            Node::Element(e) => e,
            _ => continue,
        };

        // Update series name (c:tx > c:strRef > c:strCache > c:pt > c:v).
        update_series_name(ser_el, name);

        // Update categories and values.
        if is_scatter {
            update_str_cache_in(ser_el, b"xVal", &update.categories);
            update_num_cache_in(ser_el, b"yVal", values);
        } else {
            update_str_cache_in(ser_el, b"cat", &update.categories);
            update_num_cache_in(ser_el, b"val", values);
        }

        // Update the formula references for the new data range.
        let num_cats = update.categories.len();
        let col = col_letter(ser_idx + 1);
        if is_scatter {
            update_formula_ref(ser_el, b"xVal", &format!("Sheet1!$A$2:$A${}", num_cats + 1));
            update_formula_ref(
                ser_el,
                b"yVal",
                &format!("Sheet1!${col}$2:${col}${}", num_cats + 1),
            );
        } else {
            update_formula_ref(ser_el, b"cat", &format!("Sheet1!$A$2:$A${}", num_cats + 1));
            update_formula_ref(
                ser_el,
                b"val",
                &format!("Sheet1!${col}$2:${col}${}", num_cats + 1),
            );
        }

        // Update series name formula reference.
        update_tx_formula(ser_el, &format!("Sheet1!${}$1", col));
    }

    // If there are more series in the update than in the existing chart, add new ones.
    if update.series.len() > ser_elements.len() {
        let num_cats = update.categories.len();
        for ser_idx in ser_elements.len()..update.series.len() {
            let (ref name, ref values) = update.series[ser_idx];
            let col = col_letter(ser_idx + 1);
            let new_ser_xml = if is_scatter {
                build_single_scatter_ser_xml(
                    ser_idx,
                    name,
                    &update.categories,
                    values,
                    &col,
                    num_cats,
                )
            } else {
                build_single_cat_val_ser_xml(
                    ser_idx,
                    name,
                    &update.categories,
                    values,
                    &col,
                    num_cats,
                )
            };
            let frag = Document::parse(new_ser_xml.as_bytes())
                .map_err(|e| SlideError::Unsupported(format!("ser fragment parse: {e}")))?;
            for node in frag.nodes {
                if matches!(&node, Node::Element(_)) {
                    chart_type_el.insert_child_at(chart_type_el.children.len(), node);
                    break;
                }
            }
        }
    }

    // If there are fewer series in the update than in the existing chart, remove extras.
    if update.series.len() < ser_elements.len() {
        // Remove from the end to avoid index shifting.
        for &child_idx in ser_elements[update.series.len()..].iter().rev() {
            chart_type_el.children.remove(child_idx);
        }
    }

    Ok(())
}

/// Find a direct child element by local name (mutable).
fn find_child_mut<'a>(el: &'a mut Element, local: &[u8]) -> Option<&'a mut Element> {
    el.children.iter_mut().find_map(|n| match n {
        Node::Element(e) if e.local_name() == local => Some(e),
        _ => None,
    })
}

/// Known chart type element local names.
const CHART_TYPE_NAMES: &[&[u8]] = &[
    b"barChart",
    b"lineChart",
    b"pieChart",
    b"doughnutChart",
    b"areaChart",
    b"scatterChart",
    b"bar3DChart",
    b"line3DChart",
    b"pie3DChart",
    b"area3DChart",
    b"bubbleChart",
    b"radarChart",
    b"stockChart",
    b"surfaceChart",
    b"ofPieChart",
];

/// Find the chart type element inside plotArea.
fn find_chart_type_element_mut(plot_area: &mut Element) -> Option<&mut Element> {
    plot_area.children.iter_mut().find_map(|n| match n {
        Node::Element(e) if CHART_TYPE_NAMES.iter().any(|name| e.local_name() == *name) => Some(e),
        _ => None,
    })
}

/// Update the series name in c:tx > c:strRef > c:strCache > c:pt[0] > c:v.
fn update_series_name(ser_el: &mut Element, name: &str) {
    if let Some(tx) = find_child_mut(ser_el, b"tx")
        && let Some(str_ref) = find_child_mut(tx, b"strRef")
        && let Some(str_cache) = find_child_mut(str_ref, b"strCache")
        && let Some(pt) = find_child_mut(str_cache, b"pt")
        && let Some(v) = find_child_mut(pt, b"v")
    {
        v.set_text_content(&xml_escape(name));
    }
}

/// Update the string cache inside a container element (c:cat or c:xVal).
/// Replaces the c:strCache children (ptCount + pt elements) with new data.
fn update_str_cache_in(ser_el: &mut Element, container_local: &[u8], categories: &[String]) {
    let container = match find_child_mut(ser_el, container_local) {
        Some(c) => c,
        None => return,
    };
    // Find strRef > strCache (or numRef > numCache for numeric categories).
    let cache = if let Some(str_ref) = find_child_mut(container, b"strRef") {
        find_child_mut(str_ref, b"strCache")
    } else if let Some(num_ref) = find_child_mut(container, b"numRef") {
        find_child_mut(num_ref, b"numCache")
    } else {
        None
    };

    let cache = match cache {
        Some(c) => c,
        None => return,
    };

    // Rebuild cache children: ptCount + pt elements.
    rebuild_str_cache_children(cache, categories);
}

/// Update the numeric cache inside a container element (c:val or c:yVal).
fn update_num_cache_in(ser_el: &mut Element, container_local: &[u8], values: &[f64]) {
    let container = match find_child_mut(ser_el, container_local) {
        Some(c) => c,
        None => return,
    };
    let num_ref = match find_child_mut(container, b"numRef") {
        Some(r) => r,
        None => return,
    };
    let num_cache = match find_child_mut(num_ref, b"numCache") {
        Some(c) => c,
        None => return,
    };

    rebuild_num_cache_children(num_cache, values);
}

/// Rebuild a strCache element's children with new category data.
fn rebuild_str_cache_children(cache: &mut Element, categories: &[String]) {
    // Build new XML fragment for the cache content.
    let mut xml = String::new();
    xml.push_str(&format!("<c:ptCount val=\"{}\"/>", categories.len()));
    for (i, cat) in categories.iter().enumerate() {
        xml.push_str(&format!(
            "<c:pt idx=\"{i}\"><c:v>{}</c:v></c:pt>",
            xml_escape(cat)
        ));
    }

    // Parse and replace children, preserving formatCode if present.
    let wrapper = format!(
        "<wrapper xmlns:c=\"http://schemas.openxmlformats.org/drawingml/2006/chart\">{xml}</wrapper>"
    );
    let frag = match Document::parse(wrapper.as_bytes()) {
        Ok(d) => d,
        Err(_) => return,
    };

    // Keep any formatCode element that exists.
    let format_code: Option<Node> = cache.children.iter().find_map(|n| match n {
        Node::Element(e) if e.local_name() == b"formatCode" => Some(n.clone()),
        _ => None,
    });

    cache.children.clear();
    cache.self_closing = false;
    if let Some(fc) = format_code {
        cache.children.push(fc);
    }
    if let Some(root) = frag.root() {
        for child in &root.children {
            cache.children.push(child.clone());
        }
    }
}

/// Rebuild a numCache element's children with new numeric data.
fn rebuild_num_cache_children(cache: &mut Element, values: &[f64]) {
    let mut xml = String::new();
    xml.push_str(&format!("<c:ptCount val=\"{}\"/>", values.len()));
    for (i, val) in values.iter().enumerate() {
        xml.push_str(&format!("<c:pt idx=\"{i}\"><c:v>{val}</c:v></c:pt>"));
    }

    let wrapper = format!(
        "<wrapper xmlns:c=\"http://schemas.openxmlformats.org/drawingml/2006/chart\">{xml}</wrapper>"
    );
    let frag = match Document::parse(wrapper.as_bytes()) {
        Ok(d) => d,
        Err(_) => return,
    };

    // Keep formatCode if present.
    let format_code: Option<Node> = cache.children.iter().find_map(|n| match n {
        Node::Element(e) if e.local_name() == b"formatCode" => Some(n.clone()),
        _ => None,
    });

    cache.children.clear();
    cache.self_closing = false;
    if let Some(fc) = format_code {
        cache.children.push(fc);
    }
    // Always add a formatCode for numeric caches if one wasn't preserved.
    if !cache
        .children
        .iter()
        .any(|n| matches!(n, Node::Element(e) if e.local_name() == b"formatCode"))
    {
        let fc_xml = "<wrapper xmlns:c=\"http://schemas.openxmlformats.org/drawingml/2006/chart\"><c:formatCode>General</c:formatCode></wrapper>";
        if let Ok(fc_doc) = Document::parse(fc_xml.as_bytes())
            && let Some(fc_root) = fc_doc.root()
        {
            for child in &fc_root.children {
                if matches!(child, Node::Element(e) if e.local_name() == b"formatCode") {
                    cache.children.insert(0, child.clone());
                    break;
                }
            }
        }
    }
    if let Some(root) = frag.root() {
        for child in &root.children {
            cache.children.push(child.clone());
        }
    }
}

/// Update the formula reference (c:f) inside a container (c:cat/c:val/c:xVal/c:yVal).
fn update_formula_ref(ser_el: &mut Element, container_local: &[u8], formula: &str) {
    let container = match find_child_mut(ser_el, container_local) {
        Some(c) => c,
        None => return,
    };
    let ref_el = if let Some(r) = find_child_mut(container, b"strRef") {
        Some(r)
    } else {
        find_child_mut(container, b"numRef")
    };
    let ref_el = match ref_el {
        Some(r) => r,
        None => return,
    };
    if let Some(f_el) = find_child_mut(ref_el, b"f") {
        f_el.set_text_content(formula);
    }
}

/// Update the series name formula (c:tx > c:strRef > c:f).
fn update_tx_formula(ser_el: &mut Element, formula: &str) {
    if let Some(tx) = find_child_mut(ser_el, b"tx")
        && let Some(str_ref) = find_child_mut(tx, b"strRef")
        && let Some(f_el) = find_child_mut(str_ref, b"f")
    {
        f_el.set_text_content(formula);
    }
}

/// Build a single c:ser XML fragment for cat/val charts.
fn build_single_cat_val_ser_xml(
    idx: usize,
    name: &str,
    categories: &[String],
    values: &[f64],
    col: &str,
    num_cats: usize,
) -> String {
    let mut xml = format!(
        "<c:ser xmlns:c=\"http://schemas.openxmlformats.org/drawingml/2006/chart\"><c:idx val=\"{idx}\"/><c:order val=\"{idx}\"/>"
    );
    xml.push_str(&format!(
        "<c:tx><c:strRef><c:f>Sheet1!${col}$1</c:f><c:strCache><c:ptCount val=\"1\"/><c:pt idx=\"0\"><c:v>{}</c:v></c:pt></c:strCache></c:strRef></c:tx>",
        xml_escape(name)
    ));
    xml.push_str("<c:cat><c:strRef><c:f>Sheet1!$A$2:$A$");
    xml.push_str(&(num_cats + 1).to_string());
    xml.push_str("</c:f><c:strCache>");
    xml.push_str(&format!("<c:ptCount val=\"{num_cats}\"/>"));
    for (ci, cat) in categories.iter().enumerate() {
        xml.push_str(&format!(
            "<c:pt idx=\"{ci}\"><c:v>{}</c:v></c:pt>",
            xml_escape(cat)
        ));
    }
    xml.push_str("</c:strCache></c:strRef></c:cat>");
    xml.push_str(&format!(
        "<c:val><c:numRef><c:f>Sheet1!${col}$2:${col}${}</c:f><c:numCache>",
        num_cats + 1
    ));
    xml.push_str(&format!(
        "<c:formatCode>General</c:formatCode><c:ptCount val=\"{num_cats}\"/>"
    ));
    for (vi, val) in values.iter().enumerate() {
        xml.push_str(&format!("<c:pt idx=\"{vi}\"><c:v>{val}</c:v></c:pt>"));
    }
    xml.push_str("</c:numCache></c:numRef></c:val>");
    xml.push_str("</c:ser>");
    xml
}

/// Build a single c:ser XML fragment for scatter charts.
fn build_single_scatter_ser_xml(
    idx: usize,
    name: &str,
    categories: &[String],
    values: &[f64],
    col: &str,
    num_cats: usize,
) -> String {
    let mut xml = format!(
        "<c:ser xmlns:c=\"http://schemas.openxmlformats.org/drawingml/2006/chart\"><c:idx val=\"{idx}\"/><c:order val=\"{idx}\"/>"
    );
    xml.push_str(&format!(
        "<c:tx><c:strRef><c:f>Sheet1!${col}$1</c:f><c:strCache><c:ptCount val=\"1\"/><c:pt idx=\"0\"><c:v>{}</c:v></c:pt></c:strCache></c:strRef></c:tx>",
        xml_escape(name)
    ));
    xml.push_str("<c:xVal><c:strRef><c:f>Sheet1!$A$2:$A$");
    xml.push_str(&(num_cats + 1).to_string());
    xml.push_str("</c:f><c:strCache>");
    xml.push_str(&format!("<c:ptCount val=\"{num_cats}\"/>"));
    for (ci, cat) in categories.iter().enumerate() {
        xml.push_str(&format!(
            "<c:pt idx=\"{ci}\"><c:v>{}</c:v></c:pt>",
            xml_escape(cat)
        ));
    }
    xml.push_str("</c:strCache></c:strRef></c:xVal>");
    xml.push_str(&format!(
        "<c:yVal><c:numRef><c:f>Sheet1!${col}$2:${col}${}</c:f><c:numCache>",
        num_cats + 1
    ));
    xml.push_str(&format!(
        "<c:formatCode>General</c:formatCode><c:ptCount val=\"{num_cats}\"/>"
    ));
    for (vi, val) in values.iter().enumerate() {
        xml.push_str(&format!("<c:pt idx=\"{vi}\"><c:v>{val}</c:v></c:pt>"));
    }
    xml.push_str("</c:numCache></c:numRef></c:yVal>");
    xml.push_str("</c:ser>");
    xml
}

/// Normalize a part path by resolving `..` segments.
/// E.g. `/ppt/slides/../charts/chart1.xml` → `/ppt/charts/chart1.xml`.
fn normalize_chart_path(path: &str) -> String {
    let mut segments: Vec<&str> = Vec::new();
    for seg in path.split('/') {
        if seg == ".." {
            segments.pop();
        } else if seg != "." {
            segments.push(seg);
        }
    }
    segments.join("/")
}

/// Compute a relative path from `from_part` to `to_part`.
/// E.g. from "/ppt/slides/slide1.xml" to "/ppt/charts/chart1.xml" → "../charts/chart1.xml"
fn relative_path(from_part: &str, to_part: &str) -> String {
    let from_dir = from_part.rsplit_once('/').map(|(d, _)| d).unwrap_or("");
    let to_dir = to_part.rsplit_once('/').map(|(d, _)| d).unwrap_or("");
    let to_file = to_part.rsplit_once('/').map(|(_, f)| f).unwrap_or(to_part);

    // Split into segments (skip leading empty from the leading /).
    let from_segs: Vec<&str> = from_dir.split('/').filter(|s| !s.is_empty()).collect();
    let to_segs: Vec<&str> = to_dir.split('/').filter(|s| !s.is_empty()).collect();

    // Find common prefix length.
    let common = from_segs
        .iter()
        .zip(to_segs.iter())
        .take_while(|(a, b)| a == b)
        .count();

    let ups = from_segs.len() - common;
    let mut result = String::new();
    for _ in 0..ups {
        result.push_str("../");
    }
    for seg in &to_segs[common..] {
        result.push_str(seg);
        result.push('/');
    }
    result.push_str(to_file);
    result
}

/// Convert a 0-based column index to an Excel column letter.
fn col_letter(idx: usize) -> String {
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

/// Escape XML special characters.
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
    fn chart_kind_bar_dir() {
        assert_eq!(ChartKind::ClusteredBar.bar_dir(), "bar");
        assert_eq!(ChartKind::StackedBar.bar_dir(), "bar");
        assert_eq!(ChartKind::ClusteredColumn.bar_dir(), "col");
        assert_eq!(ChartKind::StackedColumn.bar_dir(), "col");
    }

    #[test]
    fn chart_kind_grouping() {
        assert_eq!(ChartKind::ClusteredBar.grouping(), "clustered");
        assert_eq!(ChartKind::StackedBar.grouping(), "stacked");
        assert_eq!(ChartKind::ClusteredColumn.grouping(), "clustered");
        assert_eq!(ChartKind::StackedColumn.grouping(), "stacked");
        assert_eq!(ChartKind::Line.grouping(), "standard");
        assert_eq!(ChartKind::Area.grouping(), "standard");
    }

    #[test]
    fn relative_path_sibling_dirs() {
        assert_eq!(
            relative_path("/ppt/slides/slide1.xml", "/ppt/charts/chart1.xml"),
            "../charts/chart1.xml"
        );
    }

    #[test]
    fn relative_path_deeper() {
        assert_eq!(
            relative_path("/ppt/charts/chart1.xml", "/ppt/embeddings/wb1.xlsx"),
            "../embeddings/wb1.xlsx"
        );
    }

    #[test]
    fn col_letter_values() {
        assert_eq!(col_letter(0), "A");
        assert_eq!(col_letter(1), "B");
        assert_eq!(col_letter(25), "Z");
        assert_eq!(col_letter(26), "AA");
    }

    #[test]
    fn build_chart_xml_contains_bar_chart() {
        let spec = ChartSpec {
            kind: ChartKind::ClusteredBar,
            categories: vec!["Q1".into(), "Q2".into()],
            series: vec![("Revenue".into(), vec![100.0, 200.0])],
            title: None,
            legend_position: None,
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(xml.contains("<c:barChart>"));
        assert!(xml.contains("barDir val=\"bar\""));
        assert!(xml.contains("grouping val=\"clustered\""));
        assert!(xml.contains("<c:v>Q1</c:v>"));
        assert!(xml.contains("<c:v>Q2</c:v>"));
        assert!(xml.contains("<c:v>100</c:v>"));
        assert!(xml.contains("<c:v>200</c:v>"));
        assert!(xml.contains("<c:v>Revenue</c:v>"));
        assert!(xml.contains("<c:externalData r:id=\"rId1\">"));
        // Has catAx + valAx
        assert!(xml.contains("<c:catAx>"));
        assert!(xml.contains("<c:valAx>"));
    }

    #[test]
    fn build_chart_xml_with_title() {
        let spec = ChartSpec {
            kind: ChartKind::StackedColumn,
            categories: vec!["A".into()],
            series: vec![("S1".into(), vec![1.0])],
            title: Some("My Chart".into()),
            legend_position: None,
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(xml.contains("<c:title>"));
        assert!(xml.contains("<a:t>My Chart</a:t>"));
        assert!(xml.contains("barDir val=\"col\""));
        assert!(xml.contains("grouping val=\"stacked\""));
    }

    #[test]
    fn build_chart_xml_line_chart() {
        let spec = ChartSpec {
            kind: ChartKind::Line,
            categories: vec!["Jan".into(), "Feb".into(), "Mar".into()],
            series: vec![("Sales".into(), vec![10.0, 20.0, 30.0])],
            title: None,
            legend_position: None,
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(xml.contains("<c:lineChart>"));
        assert!(xml.contains("grouping val=\"standard\""));
        assert!(xml.contains("<c:v>Jan</c:v>"));
        assert!(xml.contains("<c:v>30</c:v>"));
        assert!(xml.contains("<c:cat>"));
        assert!(xml.contains("<c:val>"));
        // Has catAx + valAx
        assert!(xml.contains("<c:catAx>"));
        assert!(xml.contains("<c:valAx>"));
        // Does NOT contain barChart
        assert!(!xml.contains("<c:barChart>"));
    }

    #[test]
    fn build_chart_xml_pie_chart() {
        let spec = ChartSpec {
            kind: ChartKind::Pie,
            categories: vec!["A".into(), "B".into(), "C".into()],
            series: vec![("Share".into(), vec![40.0, 35.0, 25.0])],
            title: None,
            legend_position: None,
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(xml.contains("<c:pieChart>"));
        assert!(xml.contains("varyColors val=\"1\""));
        assert!(xml.contains("<c:v>A</c:v>"));
        assert!(xml.contains("<c:v>40</c:v>"));
        // Pie has NO axes
        assert!(!xml.contains("<c:catAx>"));
        assert!(!xml.contains("<c:valAx>"));
        // Does NOT contain barChart or lineChart
        assert!(!xml.contains("<c:barChart>"));
        assert!(!xml.contains("<c:lineChart>"));
    }

    #[test]
    fn build_chart_xml_doughnut_chart() {
        let spec = ChartSpec {
            kind: ChartKind::Doughnut,
            categories: vec!["X".into(), "Y".into()],
            series: vec![("Pct".into(), vec![60.0, 40.0])],
            title: None,
            legend_position: None,
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(xml.contains("<c:doughnutChart>"));
        assert!(xml.contains("varyColors val=\"1\""));
        assert!(xml.contains("holeSize val=\"50\""));
        assert!(xml.contains("<c:v>X</c:v>"));
        assert!(xml.contains("<c:v>60</c:v>"));
        // Doughnut has NO axes
        assert!(!xml.contains("<c:catAx>"));
        assert!(!xml.contains("<c:valAx>"));
    }

    #[test]
    fn build_chart_xml_area_chart() {
        let spec = ChartSpec {
            kind: ChartKind::Area,
            categories: vec!["2020".into(), "2021".into(), "2022".into()],
            series: vec![("Growth".into(), vec![5.0, 8.0, 12.0])],
            title: None,
            legend_position: None,
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(xml.contains("<c:areaChart>"));
        assert!(xml.contains("grouping val=\"standard\""));
        assert!(xml.contains("<c:v>2020</c:v>"));
        assert!(xml.contains("<c:v>12</c:v>"));
        assert!(xml.contains("<c:cat>"));
        assert!(xml.contains("<c:val>"));
        // Has catAx + valAx
        assert!(xml.contains("<c:catAx>"));
        assert!(xml.contains("<c:valAx>"));
        // Does NOT contain barChart
        assert!(!xml.contains("<c:barChart>"));
    }

    #[test]
    fn build_chart_xml_scatter_chart() {
        let spec = ChartSpec {
            kind: ChartKind::Scatter,
            categories: vec!["1".into(), "2".into(), "3".into()],
            series: vec![("Points".into(), vec![1.5, 3.0, 2.5])],
            title: None,
            legend_position: None,
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(xml.contains("<c:scatterChart>"));
        assert!(xml.contains("scatterStyle val=\"lineMarker\""));
        // Scatter uses xVal/yVal instead of cat/val
        assert!(xml.contains("<c:xVal>"));
        assert!(xml.contains("<c:yVal>"));
        assert!(!xml.contains("<c:cat>"));
        assert!(!xml.contains("<c:val>"));
        // Scatter has two valAx (no catAx)
        assert!(!xml.contains("<c:catAx>"));
        // Count valAx occurrences — should be 2 in plotArea + 2 in scatterChart axId refs
        let val_ax_count = xml.matches("<c:valAx>").count();
        assert_eq!(val_ax_count, 2, "scatter should have two value axes");
    }

    #[test]
    fn graphic_frame_xml_structure() {
        let xml = build_graphic_frame_xml(5, "rId20", 100, 200, 5000000, 3000000);
        assert!(xml.contains("id=\"5\""));
        assert!(xml.contains("r:id=\"rId20\""));
        assert!(xml.contains("x=\"100\""));
        assert!(xml.contains("y=\"200\""));
        assert!(xml.contains("cx=\"5000000\""));
        assert!(xml.contains("cy=\"3000000\""));
        assert!(xml.contains("uri=\"http://schemas.openxmlformats.org/drawingml/2006/chart\""));
    }

    #[test]
    fn build_chart_xml_with_legend_position_bottom() {
        let spec = ChartSpec {
            kind: ChartKind::ClusteredBar,
            categories: vec!["A".into()],
            series: vec![("S1".into(), vec![1.0])],
            title: None,
            legend_position: Some("b".into()),
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(xml.contains("<c:legend>"));
        assert!(xml.contains("<c:legendPos val=\"b\"/>"));
        assert!(xml.contains("<c:overlay val=\"0\"/>"));
        assert!(xml.contains("</c:legend>"));
        // Legend should appear after </c:plotArea> and before <c:plotVisOnly>
        let plot_area_end = xml.find("</c:plotArea>").unwrap();
        let legend_start = xml.find("<c:legend>").unwrap();
        let plot_vis = xml.find("<c:plotVisOnly").unwrap();
        assert!(
            legend_start > plot_area_end,
            "legend should be after plotArea"
        );
        assert!(
            legend_start < plot_vis,
            "legend should be before plotVisOnly"
        );
    }

    #[test]
    fn build_chart_xml_with_legend_position_top_right() {
        let spec = ChartSpec {
            kind: ChartKind::Line,
            categories: vec!["X".into()],
            series: vec![("S".into(), vec![5.0])],
            title: None,
            legend_position: Some("tr".into()),
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(xml.contains("<c:legendPos val=\"tr\"/>"));
    }

    #[test]
    fn build_chart_xml_no_legend_when_none() {
        let spec = ChartSpec {
            kind: ChartKind::ClusteredBar,
            categories: vec!["A".into()],
            series: vec![("S1".into(), vec![1.0])],
            title: None,
            legend_position: None,
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(!xml.contains("<c:legend>"));
        assert!(!xml.contains("<c:legendPos"));
    }

    #[test]
    fn build_chart_xml_with_data_labels() {
        let spec = ChartSpec {
            kind: ChartKind::ClusteredColumn,
            categories: vec!["Q1".into(), "Q2".into()],
            series: vec![
                ("Revenue".into(), vec![100.0, 200.0]),
                ("Costs".into(), vec![50.0, 75.0]),
            ],
            title: None,
            legend_position: None,
            data_labels: true,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        // Each series should have data labels.
        let dlbls_count = xml
            .matches("<c:dLbls><c:showVal val=\"1\"/></c:dLbls>")
            .count();
        assert_eq!(dlbls_count, 2, "each series should have data labels");
    }

    #[test]
    fn build_chart_xml_no_data_labels_when_false() {
        let spec = ChartSpec {
            kind: ChartKind::ClusteredColumn,
            categories: vec!["A".into()],
            series: vec![("S1".into(), vec![1.0])],
            title: None,
            legend_position: None,
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(!xml.contains("<c:dLbls>"));
    }

    #[test]
    fn build_chart_xml_scatter_with_data_labels() {
        let spec = ChartSpec {
            kind: ChartKind::Scatter,
            categories: vec!["1".into(), "2".into()],
            series: vec![("Pts".into(), vec![3.0, 4.0])],
            title: None,
            legend_position: None,
            data_labels: true,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        assert!(xml.contains("<c:dLbls><c:showVal val=\"1\"/></c:dLbls>"));
    }

    #[test]
    fn build_chart_xml_legend_and_data_labels_together() {
        let spec = ChartSpec {
            kind: ChartKind::Pie,
            categories: vec!["A".into(), "B".into()],
            series: vec![("Share".into(), vec![60.0, 40.0])],
            title: Some("Pie Chart".into()),
            legend_position: Some("r".into()),
            data_labels: true,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        // Title present.
        assert!(xml.contains("<c:title>"));
        assert!(xml.contains("<a:t>Pie Chart</a:t>"));
        // Legend present with position "r".
        assert!(xml.contains("<c:legendPos val=\"r\"/>"));
        // Data labels present in series.
        assert!(xml.contains("<c:dLbls><c:showVal val=\"1\"/></c:dLbls>"));
    }

    #[test]
    fn normalize_chart_path_resolves_dotdot() {
        assert_eq!(
            normalize_chart_path("/ppt/slides/../charts/chart1.xml"),
            "/ppt/charts/chart1.xml"
        );
        assert_eq!(
            normalize_chart_path("/ppt/charts/../embeddings/wb1.xlsx"),
            "/ppt/embeddings/wb1.xlsx"
        );
    }

    #[test]
    fn normalize_chart_path_no_dotdot_unchanged() {
        assert_eq!(
            normalize_chart_path("/ppt/charts/chart1.xml"),
            "/ppt/charts/chart1.xml"
        );
    }

    #[test]
    fn update_series_in_dom_replaces_data() {
        // Build a chart XML, parse it, update, and verify.
        let spec = ChartSpec {
            kind: ChartKind::ClusteredBar,
            categories: vec!["Q1".into(), "Q2".into()],
            series: vec![("Revenue".into(), vec![100.0, 200.0])],
            title: Some("Test".into()),
            legend_position: None,
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        let mut doc = Document::parse(xml.as_bytes()).unwrap();

        let update = ChartDataUpdate {
            categories: vec!["Jan".into(), "Feb".into(), "Mar".into()],
            series: vec![("Sales".into(), vec![10.0, 20.0, 30.0])],
        };
        update_series_in_dom(&mut doc, &update).unwrap();

        let result = String::from_utf8(doc.to_bytes()).unwrap();
        // New data present.
        assert!(result.contains("<c:v>Jan</c:v>"));
        assert!(result.contains("<c:v>Feb</c:v>"));
        assert!(result.contains("<c:v>Mar</c:v>"));
        assert!(result.contains("<c:v>10</c:v>"));
        assert!(result.contains("<c:v>20</c:v>"));
        assert!(result.contains("<c:v>30</c:v>"));
        assert!(result.contains("<c:v>Sales</c:v>"));
        // Old data gone.
        assert!(!result.contains("<c:v>Q1</c:v>"));
        assert!(!result.contains("<c:v>Q2</c:v>"));
        assert!(!result.contains("<c:v>100</c:v>"));
        // Structure preserved.
        assert!(result.contains("<c:barChart>"));
        assert!(result.contains("<c:title>"));
        assert!(result.contains("Test"));
    }

    #[test]
    fn update_series_in_dom_scatter_uses_xval_yval() {
        let spec = ChartSpec {
            kind: ChartKind::Scatter,
            categories: vec!["1".into(), "2".into()],
            series: vec![("Pts".into(), vec![3.0, 4.0])],
            title: None,
            legend_position: None,
            data_labels: false,
        };
        let xml = build_chart_xml(&spec, "/ppt/embeddings/wb1.xlsx");
        let mut doc = Document::parse(xml.as_bytes()).unwrap();

        let update = ChartDataUpdate {
            categories: vec!["10".into(), "20".into(), "30".into()],
            series: vec![("New".into(), vec![5.0, 6.0, 7.0])],
        };
        update_series_in_dom(&mut doc, &update).unwrap();

        let result = String::from_utf8(doc.to_bytes()).unwrap();
        assert!(result.contains("<c:v>10</c:v>"));
        assert!(result.contains("<c:v>20</c:v>"));
        assert!(result.contains("<c:v>30</c:v>"));
        assert!(result.contains("<c:v>5</c:v>"));
        assert!(result.contains("<c:v>6</c:v>"));
        assert!(result.contains("<c:v>7</c:v>"));
        assert!(result.contains("<c:scatterChart>"));
    }
}
