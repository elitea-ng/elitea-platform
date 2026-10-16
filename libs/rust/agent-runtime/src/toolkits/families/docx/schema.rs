//! The tool names, descriptions and argument schemas of the `docx` family.
//!
//! The SDK has no `docx` toolkit, so every name here is this family's own
//! (ADR-0032 P4); it follows the `pptx` family's conventions (a `file_name` in
//! the toolkit's bucket, `output_file_name` defaulting to the input, 1-based
//! numbers, lengths in inches). A paragraph is addressed by its 1-based number
//! among the body paragraphs (`list_placeholders` with `include_all` lists
//! them), a table by its 1-based number among the body tables.

use serde_json::{Value, json};

use super::super::office_common::{
    MAX_TEXT_CHARS, OfficeTool, catalogue_tools, chart_properties, file_param, object, out_param,
    text_param,
};

/// One tool of the family.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum DocxTool {
    CreateDocument,
    ReadDocument,
    AddHeading,
    AddParagraph,
    SetParagraphText,
    AddTable,
    SetTableCell,
    AddImage,
    AddChart,
    ListPlaceholders,
    ReplacePlaceholders,
    ExportPdf,
    RenderPage,
}

/// Every tool, in listing order.
pub(crate) const ALL_TOOLS: [DocxTool; 13] = [
    DocxTool::CreateDocument,
    DocxTool::ReadDocument,
    DocxTool::AddHeading,
    DocxTool::AddParagraph,
    DocxTool::SetParagraphText,
    DocxTool::AddTable,
    DocxTool::SetTableCell,
    DocxTool::AddImage,
    DocxTool::AddChart,
    DocxTool::ListPlaceholders,
    DocxTool::ReplacePlaceholders,
    DocxTool::ExportPdf,
    DocxTool::RenderPage,
];

/// The chart types a Word document can take.
pub(crate) const CHART_TYPES: &[&str] = &["bar", "column", "line", "pie", "area", "scatter"];

impl DocxTool {
    pub(crate) const fn name(self) -> &'static str {
        match self {
            Self::CreateDocument => "create_document",
            Self::ReadDocument => "read_document",
            Self::AddHeading => "add_heading",
            Self::AddParagraph => "add_paragraph",
            Self::SetParagraphText => "set_paragraph_text",
            Self::AddTable => "add_table",
            Self::SetTableCell => "set_table_cell",
            Self::AddImage => "add_image",
            Self::AddChart => "add_chart",
            Self::ListPlaceholders => "list_placeholders",
            Self::ReplacePlaceholders => "replace_placeholders",
            Self::ExportPdf => "export_pdf",
            Self::RenderPage => "render_page",
        }
    }

    /// Whether the tool leaves every artifact as it was.
    pub(crate) const fn is_read_only(self) -> bool {
        matches!(self, Self::ReadDocument | Self::ListPlaceholders)
    }

    pub(crate) const fn description(self) -> &'static str {
        match self {
            Self::CreateDocument => {
                "Create a new DOCX file in the bucket: empty, or with a title (as a level 1 heading) when title is given. Refuses to replace an existing file unless overwrite is true. Add content with add_heading, add_paragraph, add_table, add_image and add_chart."
            }
            Self::ReadDocument => {
                "Read a DOCX file as Markdown (headings, text, lists, tables) or as plain text. Headers, footers, text boxes and pictures are not included, and a long document is cut at a size limit and says so. Use list_placeholders with include_all to get the paragraph and table numbers the editing tools take."
            }
            Self::AddHeading => {
                "Add a heading (level 1 to 9) at the end of a DOCX file. A line break in the text becomes a line break in the heading."
            }
            Self::AddParagraph => {
                "Add a paragraph of text at the end of a DOCX file. A line break (\\n) in the text becomes a line break inside the paragraph, not a new paragraph; call the tool again for a new paragraph."
            }
            Self::SetParagraphText => {
                "Replace the text of one paragraph of a DOCX file. `paragraph` is the paragraph's number among the body paragraphs, counted from 1 (tables do not count; list_placeholders with include_all shows the numbers). The first run's formatting is kept; the formatting of the paragraph's other runs, and any hyperlinks or fields in it, are replaced. A paragraph that holds a picture or a chart is refused."
            }
            Self::AddTable => {
                "Add a table at the end of a DOCX file from rows of cell text. The first row is the header. Every row has the same number of cells. Returns the table's number."
            }
            Self::SetTableCell => {
                "Set the text of one cell of a table of a DOCX file. `table` is the table's number among the body tables (1 for the first); row and column are 1-based."
            }
            Self::AddImage => {
                "Add a picture from the bucket (PNG, JPEG or GIF) at the end of a DOCX file. Give width and/or height in inches: with one the other follows the picture's proportions, with both the picture is fitted into the box, with neither it is 6 inches wide (or narrower if it is tall)."
            }
            Self::AddChart => {
                "Add a native chart at the end of a DOCX file: bar, column, line, pie, area or scatter. Give the category labels and one or more named series of numbers, one value per category. Word draws the chart; page renderings (render_page, export_pdf) may show it as a placeholder or leave it out."
            }
            Self::ListPlaceholders => {
                "List the placeholders of a DOCX file: every body paragraph and table cell whose text contains `{{` or `[PLACEHOLDER]`, with its number and text. Set include_all to list every non-empty paragraph and cell instead (the way to find a paragraph's number). Headers, footers and text boxes are not listed, but replace_placeholders does replace in them."
            }
            Self::ReplacePlaceholders => {
                "Replace text in a DOCX file with content you provide, without calling a model. Each key of `replacements` is searched for in one pass over the document as it is now (at each place the longest key wins, and a replacement is never searched again): in the body, tables, headers, footers and text boxes, also when the text is split across runs. A line break (\\n) in a replacement becomes a line break. Keys may not contain line breaks or tabs, and two keys may not overlap each other (the end of one being the start of the other). Saves to output_file_name, or over file_name."
            }
            Self::ExportPdf => {
                "Export a DOCX file to a PDF in the bucket and return its path. The export is approximate: fonts that are not bundled render with a substitute, and charts may be drawn as placeholders."
            }
            Self::RenderPage => {
                "Render one page of a DOCX file to a PNG in the bucket and return its path. The rendering is approximate in the same ways as export_pdf. A page past the end is an error."
            }
        }
    }

    #[allow(clippy::too_many_lines)]
    pub(crate) fn parameters_schema(self) -> Value {
        let file = || ("file_name", file_param("DOCX file name in the bucket"));
        let output = || {
            (
                "output_file_name",
                out_param(
                    "Output DOCX file name. Defaults to file_name (the file is edited in place).",
                ),
            )
        };
        match self {
            Self::CreateDocument => object(
                &[
                    (
                        "file_name",
                        file_param("Name of the DOCX file to create in the bucket"),
                    ),
                    (
                        "title",
                        text_param("Title, added as a level 1 heading.", 1_000),
                    ),
                    (
                        "overwrite",
                        json!({"type": "boolean", "description": "Replace the file if it exists (default false)."}),
                    ),
                ],
                &["file_name"],
            ),
            Self::ReadDocument => object(
                &[
                    file(),
                    (
                        "format",
                        json!({"type": "string", "enum": ["markdown", "text"], "description": "markdown (default) keeps headings, lists and tables; text is the plain text."}),
                    ),
                ],
                &["file_name"],
            ),
            Self::AddHeading => object(
                &[
                    file(),
                    ("text", text_param("The heading text.", 1_000)),
                    (
                        "level",
                        json!({"type": "integer", "minimum": 1, "maximum": 9, "description": "Heading level, 1 (largest) to 9 (default 1)."}),
                    ),
                    output(),
                ],
                &["file_name", "text"],
            ),
            Self::AddParagraph => object(
                &[
                    file(),
                    ("text", text_param("The paragraph text.", MAX_TEXT_CHARS)),
                    output(),
                ],
                &["file_name", "text"],
            ),
            Self::SetParagraphText => object(
                &[
                    file(),
                    (
                        "paragraph",
                        json!({"type": "integer", "minimum": 1, "description": "The paragraph's number among the body paragraphs, from 1."}),
                    ),
                    ("text", text_param("The new text.", MAX_TEXT_CHARS)),
                    output(),
                ],
                &["file_name", "paragraph", "text"],
            ),
            Self::AddTable => object(
                &[
                    file(),
                    (
                        "rows",
                        json!({"type": "array", "minItems": 1, "maxItems": 200, "items": {"type": "array", "minItems": 1, "maxItems": 30, "items": {"type": "string", "maxLength": 2_000}}, "description": "Rows of cell text, the first row being the header. Every row has the same number of cells."}),
                    ),
                    output(),
                ],
                &["file_name", "rows"],
            ),
            Self::SetTableCell => object(
                &[
                    file(),
                    (
                        "table",
                        json!({"type": "integer", "minimum": 1, "description": "The table's number among the body tables (default 1)."}),
                    ),
                    (
                        "row",
                        json!({"type": "integer", "minimum": 1, "description": "Row, 1-based."}),
                    ),
                    (
                        "column",
                        json!({"type": "integer", "minimum": 1, "description": "Column, 1-based."}),
                    ),
                    ("text", text_param("The cell text.", 2_000)),
                    output(),
                ],
                &["file_name", "row", "column", "text"],
            ),
            Self::AddImage => object(
                &[
                    file(),
                    (
                        "image_file_name",
                        file_param("The picture's file name in the bucket (PNG, JPEG or GIF)."),
                    ),
                    (
                        "width",
                        json!({"type": "number", "exclusiveMinimum": 0, "maximum": 20, "description": "Width, inches."}),
                    ),
                    (
                        "height",
                        json!({"type": "number", "exclusiveMinimum": 0, "maximum": 20, "description": "Height, inches."}),
                    ),
                    output(),
                ],
                &["file_name", "image_file_name"],
            ),
            Self::AddChart => {
                let mut props = vec![file()];
                props.extend(chart_properties(CHART_TYPES));
                props.push((
                    "width",
                    json!({"type": "number", "exclusiveMinimum": 0, "maximum": 20, "description": "Width, inches (default 6)."}),
                ));
                props.push((
                    "height",
                    json!({"type": "number", "exclusiveMinimum": 0, "maximum": 20, "description": "Height, inches (default 3.5)."}),
                ));
                props.push(output());
                object(&props, &["file_name", "categories", "series"])
            }
            Self::ListPlaceholders => object(
                &[
                    file(),
                    (
                        "include_all",
                        json!({"type": "boolean", "description": "List every non-empty paragraph and table cell, not only those that contain a placeholder marker."}),
                    ),
                ],
                &["file_name"],
            ),
            Self::ReplacePlaceholders => object(
                &[
                    file(),
                    (
                        "replacements",
                        json!({"type": "object", "additionalProperties": {"type": "string", "maxLength": MAX_TEXT_CHARS}, "description": "Map from the text to find to its replacement (at most 100 entries)."}),
                    ),
                    output(),
                ],
                &["file_name", "replacements"],
            ),
            Self::ExportPdf => object(
                &[
                    file(),
                    (
                        "output_file_name",
                        out_param(
                            "PDF file name to save in the bucket. Defaults to the DOCX name with .pdf.",
                        ),
                    ),
                ],
                &["file_name"],
            ),
            Self::RenderPage => object(
                &[
                    file(),
                    (
                        "page",
                        json!({"type": "integer", "minimum": 1, "description": "Page number, 1-based (default 1)."}),
                    ),
                    (
                        "dpi",
                        json!({"type": "integer", "minimum": 36, "maximum": 300, "description": "Resolution (default 100)."}),
                    ),
                    (
                        "output_file_name",
                        out_param(
                            "PNG file name to save in the bucket. Defaults to <file>-page-<n>.png.",
                        ),
                    ),
                ],
                &["file_name"],
            ),
        }
    }

    /// The argument schema as the toolkit editor's catalogue carries it.
    pub(crate) fn catalogue_schema(self) -> Value {
        super::super::office_common::catalogue_schema(self.name(), self.parameters_schema())
    }
}

impl OfficeTool for DocxTool {
    const FAMILY: &'static str = "docx";
    const ALL: &'static [Self] = &ALL_TOOLS;

    fn name(self) -> &'static str {
        DocxTool::name(self)
    }

    fn description(self) -> &'static str {
        DocxTool::description(self)
    }
}

/// This family's catalogue entry for `rust_only_toolkit_tools.json`: the type
/// has no SDK counterpart, so the entry carries the type and all its tools.
pub(crate) fn type_entry() -> Value {
    super::super::office_common::type_entry(
        "docx",
        "DOCX",
        "Bucket name where DOCX files are stored",
        &["document", "word", "office automation"],
        catalogue_tools::<DocxTool>(|_| false, DocxTool::parameters_schema),
    )
}

/// Every tool this family serves, with whether it needs the run's file store
/// (`materialize::omitted_selection` reads it for the run's notice).
pub(crate) fn tool_catalog() -> Vec<(&'static str, bool)> {
    ALL_TOOLS.iter().map(|tool| (tool.name(), true)).collect()
}
