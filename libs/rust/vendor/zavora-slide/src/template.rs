//! Minimal, valid OOXML part templates for a blank 16:9 presentation.
//!
//! These are real PowerPoint-authored parts (theme, slide master, "blank" slide
//! layout, blank slide, and the standard auxiliary parts), trimmed to a single
//! layout. Using genuine Microsoft output — including the full standard part set
//! every real .pptx ships (presProps, viewProps, tableStyles, docProps) and a
//! schema-required `notesSz` — guarantees PowerPoint opens the deck without
//! repair. Hand-written minimal containers tripped PowerPoint's stricter (vs.
//! python-pptx) schema validation.

pub const THEME_XML: &str = include_str!("parts/theme1.xml");
pub const SLIDE_MASTER_XML: &str = include_str!("parts/slideMaster1.xml");
pub const SLIDE_LAYOUT_XML: &str = include_str!("parts/slideLayout1.xml");
pub const PRES_PROPS_XML: &str = include_str!("parts/presProps.xml");
pub const VIEW_PROPS_XML: &str = include_str!("parts/viewProps.xml");
pub const TABLE_STYLES_XML: &str = include_str!("parts/tableStyles.xml");
pub const CORE_XML: &str = include_str!("parts/core.xml");
pub const APP_XML: &str = include_str!("parts/app.xml");
pub const NOTES_MASTER_XML: &str = include_str!("parts/notesMaster1.xml");

// Content types.
pub const CT_SLIDE: &str = "application/vnd.openxmlformats-officedocument.presentationml.slide+xml";
pub const CT_SLIDE_MASTER: &str =
    "application/vnd.openxmlformats-officedocument.presentationml.slideMaster+xml";
pub const CT_SLIDE_LAYOUT: &str =
    "application/vnd.openxmlformats-officedocument.presentationml.slideLayout+xml";
pub const CT_THEME: &str = "application/vnd.openxmlformats-officedocument.theme+xml";
pub const CT_PRES_PROPS: &str =
    "application/vnd.openxmlformats-officedocument.presentationml.presProps+xml";
pub const CT_VIEW_PROPS: &str =
    "application/vnd.openxmlformats-officedocument.presentationml.viewProps+xml";
pub const CT_TABLE_STYLES: &str =
    "application/vnd.openxmlformats-officedocument.presentationml.tableStyles+xml";
pub const CT_CORE: &str = "application/vnd.openxmlformats-package.core-properties+xml";
pub const CT_APP: &str = "application/vnd.openxmlformats-officedocument.extended-properties+xml";
pub const CT_NOTES_SLIDE: &str =
    "application/vnd.openxmlformats-officedocument.presentationml.notesSlide+xml";
pub const CT_NOTES_MASTER: &str =
    "application/vnd.openxmlformats-officedocument.presentationml.notesMaster+xml";

// Relationship type URIs not in the opc rel_types module.
pub const RT_PRES_PROPS: &str =
    "http://schemas.openxmlformats.org/officeDocument/2006/relationships/presProps";
pub const RT_VIEW_PROPS: &str =
    "http://schemas.openxmlformats.org/officeDocument/2006/relationships/viewProps";
pub const RT_TABLE_STYLES: &str =
    "http://schemas.openxmlformats.org/officeDocument/2006/relationships/tableStyles";
pub const RT_CORE: &str =
    "http://schemas.openxmlformats.org/package/2006/relationships/metadata/core-properties";
pub const RT_EXTENDED: &str =
    "http://schemas.openxmlformats.org/officeDocument/2006/relationships/extended-properties";
pub const RT_NOTES_SLIDE: &str =
    "http://schemas.openxmlformats.org/officeDocument/2006/relationships/notesSlide";
pub const RT_NOTES_MASTER: &str =
    "http://schemas.openxmlformats.org/officeDocument/2006/relationships/notesMaster";
pub const RT_SLIDE: &str =
    "http://schemas.openxmlformats.org/officeDocument/2006/relationships/slide";
