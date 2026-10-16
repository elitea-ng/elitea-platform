//! High-level PowerPoint (.pptx) presentation API.
//!
//! ```no_run
//! use zavora_slide::{Presentation, Layout};
//! let mut p = Presentation::new();
//! p.add_slide(Layout::TitleContent);
//! p.save("deck.pptx").unwrap();
//! ```

pub mod chart;
mod core_properties;
pub mod design;
mod error;
pub mod extraction;
mod presentation;
pub mod qa;
mod slide;
mod template;
mod theme;
mod units;

pub use chart::{ChartDataUpdate, ChartKind, ChartSpec};
pub use design::{
    FontPairing, LayoutPattern, Palette, PatternParams, apply_design_theme, apply_layout_pattern,
    font_pairing_by_id, font_pairings, palette_by_id, palettes,
};
pub use error::{Result, SlideError};
pub use presentation::Presentation;
pub use slide::{
    Bullet, Fill, ImageFormat, ImageSrc, MediaEntry, ShapeInfo, Slide, SlideData, SlideRef, Table,
    TableId,
};
pub use theme::ThemeSpec;
pub use units::{Emu, Layout, RenderFormat, ShapePreset, SlideSize};

pub use qa::{design_lint, design_lint_with_config};
mod from_dom;

pub use zavora_slide_layout::{Color, Item, Scene, TextLine};
pub use zavora_slide_oxml::Align;
pub use zavora_slide_oxml::RunFormat;
pub use zavora_slide_oxml::{AutoFit, BulletKind, SpacingValue};

pub use core_properties::CoreProperties;
pub use extraction::{DeckOutline, OutlineElement, SlideOutline, to_markdown, to_outline};
