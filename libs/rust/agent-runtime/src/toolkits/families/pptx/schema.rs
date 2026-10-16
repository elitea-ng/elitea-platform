//! The tool names, descriptions and argument schemas of the `pptx` family.
//!
//! `fill_template` and `translate_presentation` are the SDK's, byte for byte
//! in name, description and arguments (`elitea_sdk/tools/pptx/pptx_wrapper.py`:
//! saved agents and pipeline mappings were written against them, and the SDK
//! conformance gate checks the schemas). The other nineteen are this
//! family's own (ADR-0032 P3): the deterministic halves of the two SDK tools
//! and the authoring tools. Every one reads and writes artifacts of the
//! toolkit's own bucket; a slide is always addressed by its 1-based number,
//! a length in inches.

use serde_json::{Value, json};

use super::super::office_common::{
    MAX_FILE_NAME_BYTES, MAX_TEXT_CHARS, OfficeTool, catalogue_schema, catalogue_tools, file_param,
    object, out_param, text_param,
};

/// One tool of the family.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PptxTool {
    FillTemplate,
    TranslatePresentation,
    ListPlaceholders,
    ReplacePlaceholders,
    CreatePresentation,
    AddSlide,
    DeleteSlide,
    MoveSlide,
    SetTitle,
    SetText,
    AddBullets,
    AddTable,
    SetTableCell,
    AddChart,
    AddImage,
    SetNotes,
    ApplyTheme,
    RenderPreview,
    DesignCheck,
    ToMarkdown,
    ExportPdf,
}

/// Every tool, in listing order: the SDK's two first.
pub(crate) const ALL_TOOLS: [PptxTool; 21] = [
    PptxTool::FillTemplate,
    PptxTool::TranslatePresentation,
    PptxTool::ListPlaceholders,
    PptxTool::ReplacePlaceholders,
    PptxTool::CreatePresentation,
    PptxTool::AddSlide,
    PptxTool::DeleteSlide,
    PptxTool::MoveSlide,
    PptxTool::SetTitle,
    PptxTool::SetText,
    PptxTool::AddBullets,
    PptxTool::AddTable,
    PptxTool::SetTableCell,
    PptxTool::AddChart,
    PptxTool::AddImage,
    PptxTool::SetNotes,
    PptxTool::ApplyTheme,
    PptxTool::RenderPreview,
    PptxTool::DesignCheck,
    PptxTool::ToMarkdown,
    PptxTool::ExportPdf,
];

/// The SDK's `fill_template` docstring (`__doc__`, indentation included): it
/// is the description saved prompts were written against.
const FILL_TEMPLATE_DESCRIPTION: &str = "
        Fill a PPTX template with content based on the provided description.
        Uses batch processing to minimize LLM calls.

        Args:
            file_name: PPTX file name in the bucket
            output_file_name: Output PPTX file name to save in the bucket
            content_description: Detailed description of what content to put where in the template
            pdf_file_name: Optional PDF file name in the bucket that matches the PPTX template 1:1
            batch_size: Number of placeholders to process per LLM call (default: 20)

        Returns:
            Dictionary with result information
        ";

impl PptxTool {
    pub(crate) fn parse(name: &str) -> Option<Self> {
        ALL_TOOLS.into_iter().find(|tool| tool.name() == name)
    }

    pub(crate) const fn name(self) -> &'static str {
        match self {
            Self::FillTemplate => "fill_template",
            Self::TranslatePresentation => "translate_presentation",
            Self::ListPlaceholders => "list_placeholders",
            Self::ReplacePlaceholders => "replace_placeholders",
            Self::CreatePresentation => "create_presentation",
            Self::AddSlide => "add_slide",
            Self::DeleteSlide => "delete_slide",
            Self::MoveSlide => "move_slide",
            Self::SetTitle => "set_title",
            Self::SetText => "set_text",
            Self::AddBullets => "add_bullets",
            Self::AddTable => "add_table",
            Self::SetTableCell => "set_table_cell",
            Self::AddChart => "add_chart",
            Self::AddImage => "add_image",
            Self::SetNotes => "set_notes",
            Self::ApplyTheme => "apply_theme",
            Self::RenderPreview => "render_preview",
            Self::DesignCheck => "design_check",
            Self::ToMarkdown => "to_markdown",
            Self::ExportPdf => "export_pdf",
        }
    }

    /// Whether the tool leaves every artifact as it was.
    pub(crate) const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::ListPlaceholders | Self::DesignCheck | Self::ToMarkdown
        )
    }

    /// Whether the tool calls the chat model.
    pub(crate) const fn calls_model(self) -> bool {
        matches!(self, Self::FillTemplate | Self::TranslatePresentation)
    }

    pub(crate) const fn description(self) -> &'static str {
        match self {
            Self::FillTemplate => FILL_TEMPLATE_DESCRIPTION,
            Self::TranslatePresentation => {
                "Translate text in a PowerPoint presentation to another language"
            }
            Self::ListPlaceholders => {
                "List the placeholders of a PPTX file: every text frame and table cell whose text contains `{{` or `[PLACEHOLDER]`, with its slide, shape and text. Use it to see what fill_template would fill, or to drive replace_placeholders with your own content. Set include_all to list every text frame and table cell instead."
            }
            Self::ReplacePlaceholders => {
                "Replace placeholder text in a PPTX file with content you provide, without calling a model. Each key of `replacements` is either a placeholder's whole text as list_placeholders reports it (the frame is rewritten, keeping the first run's formatting) or any other text, which is replaced wherever it occurs. Saves to output_file_name, or over file_name."
            }
            Self::CreatePresentation => {
                "Create a new PPTX file in the bucket with one slide: a title slide when title or subtitle is given, otherwise a blank one. Refuses to replace an existing file unless overwrite is true. Add slides with add_slide."
            }
            Self::AddSlide => {
                "Add a slide to a PPTX file at the end, or at `position`. Layouts: title, title_content, section_header, two_content, blank. Optionally sets the title, bullets and speaker notes. Returns the new slide's number."
            }
            Self::DeleteSlide => {
                "Delete one slide from a PPTX file. Slide numbers after it move down by one."
            }
            Self::MoveSlide => "Move a slide of a PPTX file to another position (both 1-based).",
            Self::SetTitle => "Set the title of a slide of a PPTX file.",
            Self::SetText => {
                "Put text on a slide of a PPTX file: with `shape` (a number from list_placeholders include_all, or a shape name) the text of that existing shape is replaced; without it a new text box is added at the given position (inches)."
            }
            Self::AddBullets => {
                "Put a bulleted list on a slide of a PPTX file: it replaces the bullets of the slide's content placeholder, or becomes a text box on a slide that has none. Each item is a string, or an object with text, level (0-8, indentation) and bold."
            }
            Self::AddTable => {
                "Add a table to a slide of a PPTX file from rows of cell text. The first row is the header. Position and size are in inches; by default the table spans the slide width below the title."
            }
            Self::SetTableCell => {
                "Set the text of one cell of an existing table on a slide of a PPTX file. `table` is the table's number on the slide (1 for the first table); row and column are 1-based."
            }
            Self::AddChart => {
                "Add a native chart to a slide of a PPTX file: bar, column, line, pie, doughnut, area, scatter, or stacked bar/column. Give the category labels and one or more named series of numbers, one value per category. Previews (render_preview) do not draw charts; PowerPoint does."
            }
            Self::AddImage => {
                "Add a picture from the bucket (PNG, JPEG or GIF) to a slide of a PPTX file. The picture is fitted into the box you give (inches) keeping its proportions; by default it is centred below the title."
            }
            Self::SetNotes => {
                "Set the speaker notes of a slide of a PPTX file. Adding notes to a slide that has none makes the document library rewrite the whole file; if that would lose tables, charts or pictures the edit is refused and nothing is saved."
            }
            Self::ApplyTheme => {
                "Replace the theme of a PPTX file with one built from the accent colour, heading and body fonts and named theme colours (hex RRGGBB) you give; fields you leave out take the library defaults, not the old theme's. If saving would lose tables, charts or pictures the edit is refused and nothing is saved."
            }
            Self::RenderPreview => {
                "Render one slide of a PPTX file to a PNG in the bucket and return its path. The preview is approximate: charts are drawn as grey placeholders (their data is returned beside the path), SVG pictures are not drawn, and fonts that are not bundled render with a substitute. Use design_check for layout problems."
            }
            Self::DesignCheck => {
                "Check a PPTX file for common layout problems using the file's own geometry and text: text that will not fit its box, empty placeholders, missing titles, text under 12 pt, low contrast between an explicit text colour and an explicit fill, shapes off the slide, overlapping shapes, and slides that are very dense. A heuristic: shapes that inherit their size or font from the layout are not measured, and nothing is rendered."
            }
            Self::ToMarkdown => {
                "Read a PPTX file as Markdown: slide titles, text, tables, speaker notes and chart data. Long decks are cut at a size limit and say so."
            }
            Self::ExportPdf => {
                "Export a PPTX file to a PDF in the bucket, one page per slide, and return its path. The export is approximate in the same ways as render_preview."
            }
        }
    }

    #[allow(clippy::too_many_lines)]
    pub(crate) fn parameters_schema(self) -> Value {
        match self {
            Self::FillTemplate => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    (
                        "output_file_name",
                        file_param("Output PPTX file name to save in the bucket"),
                    ),
                    (
                        "content_description",
                        json!({"type": "string", "maxLength": 64_000, "description": "Detailed description of what content to put where in the template"}),
                    ),
                    (
                        "pdf_file_name",
                        json!({"type": "string", "maxLength": MAX_FILE_NAME_BYTES, "description": "Optional PDF file name in the bucket that matches the PPTX template 1:1"}),
                    ),
                    (
                        "batch_size",
                        json!({"type": "integer", "minimum": 1, "maximum": 100, "description": "Number of placeholders to process per LLM call (default: 20)"}),
                    ),
                ],
                &["file_name", "output_file_name", "content_description"],
            ),
            Self::TranslatePresentation => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    (
                        "output_file_name",
                        file_param("Output PPTX file name to save in the bucket"),
                    ),
                    (
                        "target_language",
                        json!({"type": "string", "minLength": 1, "maxLength": 64, "description": "Target language code (e.g., 'es' for Spanish, 'ua' for Ukrainian)"}),
                    ),
                ],
                &["file_name", "output_file_name", "target_language"],
            ),
            Self::ListPlaceholders => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    (
                        "include_all",
                        json!({"type": "boolean", "description": "List every text frame and table cell, not only those that contain a placeholder marker."}),
                    ),
                ],
                &["file_name"],
            ),
            Self::ReplacePlaceholders => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    (
                        "replacements",
                        json!({"type": "object", "additionalProperties": {"type": "string", "maxLength": MAX_TEXT_CHARS}, "description": "Map from the text to find to its replacement. A key equal to a placeholder's whole text (as list_placeholders reports it) replaces that whole frame or cell."}),
                    ),
                    (
                        "output_file_name",
                        out_param(
                            "Output PPTX file name. Defaults to file_name (the file is overwritten).",
                        ),
                    ),
                ],
                &["file_name", "replacements"],
            ),
            Self::CreatePresentation => object(
                &[
                    (
                        "file_name",
                        file_param("Name of the PPTX file to create in the bucket"),
                    ),
                    (
                        "title",
                        text_param(
                            "Title for a first title slide. Leave out for an empty deck.",
                            1_000,
                        ),
                    ),
                    (
                        "subtitle",
                        text_param("Subtitle for the title slide.", 1_000),
                    ),
                    (
                        "overwrite",
                        json!({"type": "boolean", "description": "Replace the file if it exists (default false)."}),
                    ),
                ],
                &["file_name"],
            ),
            Self::AddSlide => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    (
                        "layout",
                        json!({"type": "string", "enum": ["title", "title_content", "section_header", "two_content", "blank"], "description": "Slide layout (default title_content)."}),
                    ),
                    (
                        "position",
                        slide_param("Where to insert the slide, 1-based. Default: at the end."),
                    ),
                    ("title", text_param("Slide title.", 1_000)),
                    (
                        "bullets",
                        json!({"type": "array", "maxItems": 100, "items": bullet_schema(), "description": "Bulleted items to add."}),
                    ),
                    ("notes", text_param("Speaker notes.", MAX_TEXT_CHARS)),
                ],
                &["file_name"],
            ),
            Self::DeleteSlide => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    ("slide", slide_param("Slide number, 1-based.")),
                ],
                &["file_name", "slide"],
            ),
            Self::MoveSlide => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    ("slide", slide_param("Slide number to move, 1-based.")),
                    (
                        "to_slide",
                        slide_param("The position it should end up at, 1-based."),
                    ),
                ],
                &["file_name", "slide", "to_slide"],
            ),
            Self::SetTitle => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    ("slide", slide_param("Slide number, 1-based.")),
                    (
                        "title",
                        json!({"type": "string", "maxLength": 1_000, "description": "The new title."}),
                    ),
                ],
                &["file_name", "slide", "title"],
            ),
            Self::SetText => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    ("slide", slide_param("Slide number, 1-based.")),
                    (
                        "text",
                        json!({"type": "string", "maxLength": MAX_TEXT_CHARS, "description": "The text."}),
                    ),
                    (
                        "shape",
                        json!({"type": ["string", "integer"], "description": "An existing text shape to replace the text of: its number from list_placeholders include_all, or its name. Leave out to add a new text box."}),
                    ),
                    ("x", inches("Left edge of a new text box, inches.")),
                    ("y", inches("Top edge of a new text box, inches.")),
                    ("width", inches("Width of a new text box, inches.")),
                    ("height", inches("Height of a new text box, inches.")),
                    (
                        "output_file_name",
                        out_param("Output PPTX file name. Defaults to file_name."),
                    ),
                ],
                &["file_name", "slide", "text"],
            ),
            Self::AddBullets => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    ("slide", slide_param("Slide number, 1-based.")),
                    (
                        "items",
                        json!({"type": "array", "minItems": 1, "maxItems": 200, "items": bullet_schema(), "description": "The bullets: strings, or objects {text, level, bold}."}),
                    ),
                    (
                        "output_file_name",
                        out_param("Output PPTX file name. Defaults to file_name."),
                    ),
                ],
                &["file_name", "slide", "items"],
            ),
            Self::AddTable => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    ("slide", slide_param("Slide number, 1-based.")),
                    (
                        "rows",
                        json!({"type": "array", "minItems": 1, "maxItems": 100, "items": {"type": "array", "minItems": 1, "maxItems": 30, "items": {"type": "string", "maxLength": 2_000}}, "description": "Rows of cell text, the first row being the header. Every row has the same number of cells."}),
                    ),
                    ("x", inches("Left edge, inches.")),
                    ("y", inches("Top edge, inches.")),
                    ("width", inches("Width, inches.")),
                    ("height", inches("Height, inches.")),
                    (
                        "output_file_name",
                        out_param("Output PPTX file name. Defaults to file_name."),
                    ),
                ],
                &["file_name", "slide", "rows"],
            ),
            Self::SetTableCell => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    ("slide", slide_param("Slide number, 1-based.")),
                    (
                        "table",
                        json!({"type": "integer", "minimum": 1, "description": "The table's number on the slide (default 1)."}),
                    ),
                    (
                        "row",
                        json!({"type": "integer", "minimum": 1, "description": "Row, 1-based."}),
                    ),
                    (
                        "column",
                        json!({"type": "integer", "minimum": 1, "description": "Column, 1-based."}),
                    ),
                    (
                        "text",
                        json!({"type": "string", "maxLength": 2_000, "description": "The cell text."}),
                    ),
                    (
                        "output_file_name",
                        out_param("Output PPTX file name. Defaults to file_name."),
                    ),
                ],
                &["file_name", "slide", "row", "column", "text"],
            ),
            Self::AddChart => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    ("slide", slide_param("Slide number, 1-based.")),
                    (
                        "chart_type",
                        json!({"type": "string", "enum": ["bar", "stacked_bar", "column", "stacked_column", "line", "pie", "doughnut", "area", "scatter"], "description": "The chart type (default column)."}),
                    ),
                    (
                        "categories",
                        json!({"type": "array", "minItems": 1, "maxItems": 500, "items": {"type": "string", "maxLength": 200}, "description": "Category labels."}),
                    ),
                    (
                        "series",
                        json!({"type": "array", "minItems": 1, "maxItems": 20, "items": {"type": "object", "properties": {"name": {"type": "string", "maxLength": 200}, "values": {"type": "array", "items": {"type": "number"}, "maxItems": 500}}, "required": ["name", "values"], "additionalProperties": false}, "description": "The series: a name and one number per category."}),
                    ),
                    ("title", text_param("Chart title.", 500)),
                    (
                        "legend_position",
                        json!({"type": "string", "enum": ["top", "bottom", "left", "right"], "description": "Where the legend goes. Omit for none."}),
                    ),
                    (
                        "data_labels",
                        json!({"type": "boolean", "description": "Show the value on each point."}),
                    ),
                    ("x", inches("Left edge, inches.")),
                    ("y", inches("Top edge, inches.")),
                    ("width", inches("Width, inches.")),
                    ("height", inches("Height, inches.")),
                    (
                        "output_file_name",
                        out_param("Output PPTX file name. Defaults to file_name."),
                    ),
                ],
                &["file_name", "slide", "categories", "series"],
            ),
            Self::AddImage => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    ("slide", slide_param("Slide number, 1-based.")),
                    (
                        "image_file_name",
                        file_param("The picture's file name in the bucket (PNG, JPEG or GIF)."),
                    ),
                    ("x", inches("Left edge of the box, inches.")),
                    ("y", inches("Top edge of the box, inches.")),
                    ("width", inches("Width of the box, inches.")),
                    ("height", inches("Height of the box, inches.")),
                    (
                        "output_file_name",
                        out_param("Output PPTX file name. Defaults to file_name."),
                    ),
                ],
                &["file_name", "slide", "image_file_name"],
            ),
            Self::SetNotes => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    ("slide", slide_param("Slide number, 1-based.")),
                    (
                        "notes",
                        json!({"type": "string", "maxLength": MAX_TEXT_CHARS, "description": "The speaker notes."}),
                    ),
                    (
                        "output_file_name",
                        out_param("Output PPTX file name. Defaults to file_name."),
                    ),
                ],
                &["file_name", "slide", "notes"],
            ),
            Self::ApplyTheme => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    (
                        "accent",
                        json!({"type": "string", "maxLength": 9, "description": "Accent colour, hex RRGGBB."}),
                    ),
                    ("heading_font", text_param("Heading font family.", 100)),
                    ("body_font", text_param("Body font family.", 100)),
                    (
                        "colors",
                        json!({"type": "object", "additionalProperties": {"type": "string", "maxLength": 9}, "description": "Named theme colours to set, e.g. {\"dk2\": \"1F3864\"}."}),
                    ),
                    (
                        "output_file_name",
                        out_param("Output PPTX file name. Defaults to file_name."),
                    ),
                ],
                &["file_name"],
            ),
            Self::RenderPreview => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    ("slide", slide_param("Slide number, 1-based (default 1).")),
                    (
                        "output_file_name",
                        out_param(
                            "PNG file name to save in the bucket. Defaults to <file>-slide-<n>.png.",
                        ),
                    ),
                ],
                &["file_name"],
            ),
            Self::DesignCheck => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    (
                        "slide",
                        slide_param("Check only this slide, 1-based. Default: every slide."),
                    ),
                ],
                &["file_name"],
            ),
            Self::ToMarkdown => object(
                &[("file_name", file_param("PPTX file name in the bucket"))],
                &["file_name"],
            ),
            Self::ExportPdf => object(
                &[
                    ("file_name", file_param("PPTX file name in the bucket")),
                    (
                        "output_file_name",
                        out_param(
                            "PDF file name to save in the bucket. Defaults to the PPTX name with .pdf.",
                        ),
                    ),
                ],
                &["file_name"],
            ),
        }
    }
}

impl PptxTool {
    /// The argument schema as the toolkit editor's catalogue carries it.
    pub(crate) fn catalogue_schema(self) -> Value {
        catalogue_schema(self.name(), self.parameters_schema())
    }
}

impl OfficeTool for PptxTool {
    const FAMILY: &'static str = "pptx";
    const ALL: &'static [Self] = &ALL_TOOLS;

    fn name(self) -> &'static str {
        PptxTool::name(self)
    }

    fn description(self) -> &'static str {
        PptxTool::description(self)
    }
}

/// The Rust-only tools' catalogue document (the file elitea-main embeds as
/// `rust_only_toolkit_tools.json`): every tool of the family the SDK's
/// `pptx` toolkit does not declare.
pub(crate) fn rust_only_catalogue_tools() -> serde_json::Map<String, Value> {
    catalogue_tools::<PptxTool>(
        |t| matches!(t, PptxTool::FillTemplate | PptxTool::TranslatePresentation),
        PptxTool::parameters_schema,
    )
}

fn slide_param(description: &str) -> Value {
    json!({"type": "integer", "minimum": 1, "description": description})
}

fn inches(description: &str) -> Value {
    json!({"type": "number", "minimum": 0, "maximum": 400, "description": description})
}

fn bullet_schema() -> Value {
    json!({"anyOf": [
        {"type": "string", "maxLength": 2_000},
        {"type": "object", "properties": {
            "text": {"type": "string", "maxLength": 2_000},
            "level": {"type": "integer", "minimum": 0, "maximum": 8},
            "bold": {"type": "boolean"}
        }, "required": ["text"], "additionalProperties": false}
    ]})
}

/// Every tool this family serves, with whether it needs the run's file store
/// (`materialize::omitted_selection` reads it for the run's notice).
pub(crate) fn tool_catalog() -> Vec<(&'static str, bool)> {
    ALL_TOOLS.iter().map(|tool| (tool.name(), true)).collect()
}
