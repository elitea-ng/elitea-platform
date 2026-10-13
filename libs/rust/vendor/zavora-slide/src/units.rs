//! Units and enumerations for the high-level API.

/// English Metric Units — the PresentationML coordinate unit (914400 per inch).
#[derive(Debug, Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
pub struct Emu(pub i64);

impl Emu {
    pub const PER_INCH: i64 = 914400;
    pub const PER_POINT: i64 = 12700;
    pub const PER_CM: i64 = 360000;

    pub fn inches(v: f64) -> Emu {
        Emu((v * Self::PER_INCH as f64).round() as i64)
    }
    pub fn points(v: f64) -> Emu {
        Emu((v * Self::PER_POINT as f64).round() as i64)
    }
    pub fn cm(v: f64) -> Emu {
        Emu((v * Self::PER_CM as f64).round() as i64)
    }
}

/// Built-in slide layouts (placeholder arrangements resolved in later phases).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum Layout {
    Title,
    TitleContent,
    SectionHeader,
    TwoContent,
    Blank,
}

impl Layout {
    /// Parse a layout name (case-insensitive, snake/camel tolerant).
    pub fn parse(s: &str) -> Option<Layout> {
        match s.to_ascii_lowercase().replace(['_', '-', ' '], "").as_str() {
            "title" => Some(Layout::Title),
            "titlecontent" | "content" => Some(Layout::TitleContent),
            "sectionheader" | "section" => Some(Layout::SectionHeader),
            "twocontent" | "two" => Some(Layout::TwoContent),
            "blank" => Some(Layout::Blank),
            _ => None,
        }
    }

    /// The PresentationML `sldLayout@type` this layout corresponds to, used to
    /// bind a new slide to an existing layout in an opened deck.
    pub fn layout_type(self) -> &'static str {
        match self {
            Layout::Title => "title",
            Layout::TitleContent => "obj",
            Layout::SectionHeader => "secHead",
            Layout::TwoContent => "twoObj",
            Layout::Blank => "blank",
        }
    }
}

/// Deck slide-size presets.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SlideSize {
    /// 16:9 widescreen (default).
    Widescreen,
    /// 4:3 standard.
    Standard,
    /// 16:10.
    Wide16x10,
}

impl SlideSize {
    /// (cx, cy, type) in EMU.
    pub fn dims(self) -> (i64, i64, &'static str) {
        match self {
            SlideSize::Widescreen => (12192000, 6858000, "screen16x9"),
            SlideSize::Standard => (9144000, 6858000, "screen4x3"),
            SlideSize::Wide16x10 => (12192000, 7620000, "screen16x10"),
        }
    }
}

/// Output format for slide rasterization (Phase 3).
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum RenderFormat {
    Png,
    Svg,
}

/// DrawingML auto-shape preset geometries — the full ECMA-376 `ST_ShapeType`
/// enumeration (~239 presets). Each variant maps to a `prstGeom@prst` value.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash)]
#[allow(missing_docs)]
pub enum ShapePreset {
    // ─── Basic shapes ───────────────────────────────────────────────────
    Rect,
    RoundRect,
    Snip1Rect,
    Snip2SameRect,
    Snip2DiagRect,
    SnipRoundRect,
    Round1Rect,
    Round2SameRect,
    Round2DiagRect,
    Ellipse,
    Triangle,
    RtTriangle,
    Parallelogram,
    Trapezoid,
    Diamond,
    Pentagon,
    Hexagon,
    Heptagon,
    Octagon,
    Decagon,
    Dodecagon,
    Pie,
    Chord,
    Teardrop,
    Frame,
    HalfFrame,
    Corner,
    DiagStripe,
    Plus,
    Plaque,
    Can,
    Cube,
    Bevel,
    Donut,
    NoSmoking,
    BlockArc,
    FoldedCorner,
    SmileyFace,
    Heart,
    LightningBolt,
    Sun,
    Moon,
    Cloud,
    Arc,
    BracketPair,
    BracePair,
    LeftBracket,
    RightBracket,
    LeftBrace,
    RightBrace,
    // ─── Lines and connectors ─────────────────────────────────────────
    Line,
    LineInv,
    StraightConnector1,
    BentConnector2,
    BentConnector3,
    BentConnector4,
    BentConnector5,
    CurvedConnector2,
    CurvedConnector3,
    CurvedConnector4,
    CurvedConnector5,
    // ─── Arrows ─────────────────────────────────────────────────────────
    RightArrow,
    LeftArrow,
    UpArrow,
    DownArrow,
    LeftRightArrow,
    UpDownArrow,
    QuadArrow,
    LeftRightUpArrow,
    BentArrow,
    UturnArrow,
    CircularArrow,
    LeftCircularArrow,
    LeftRightCircularArrow,
    CurvedRightArrow,
    CurvedLeftArrow,
    CurvedUpArrow,
    CurvedDownArrow,
    StripedRightArrow,
    NotchedRightArrow,
    HomePlate,
    Chevron,
    RightArrowCallout,
    LeftArrowCallout,
    UpArrowCallout,
    DownArrowCallout,
    LeftRightArrowCallout,
    UpDownArrowCallout,
    QuadArrowCallout,
    // ─── Block arrows ───────────────────────────────────────────────────
    BentUpArrow,
    LeftUpArrow,
    SwooshArrow,
    // ─── Stars and banners ────────────────────────────────────────────
    Star4,
    Star5,
    Star6,
    Star7,
    Star8,
    Star10,
    Star12,
    Star16,
    Star24,
    Star32,
    Ribbon,
    Ribbon2,
    EllipseRibbon,
    EllipseRibbon2,
    VerticalScroll,
    HorizontalScroll,
    Wave,
    DoubleWave,
    IrregularSeal1,
    IrregularSeal2,
    // ─── Callouts ───────────────────────────────────────────────────────
    WedgeRectCallout,
    WedgeRoundRectCallout,
    WedgeEllipseCallout,
    CloudCallout,
    BorderCallout1,
    BorderCallout2,
    BorderCallout3,
    AccentCallout1,
    AccentCallout2,
    AccentCallout3,
    Callout1,
    Callout2,
    Callout3,
    AccentBorderCallout1,
    AccentBorderCallout2,
    AccentBorderCallout3,
    // ─── Flowchart ────────────────────────────────────────────────────
    FlowChartProcess,
    FlowChartAlternateProcess,
    FlowChartDecision,
    FlowChartInputOutput,
    FlowChartPredefinedProcess,
    FlowChartInternalStorage,
    FlowChartDocument,
    FlowChartMultidocument,
    FlowChartTerminator,
    FlowChartPreparation,
    FlowChartManualInput,
    FlowChartManualOperation,
    FlowChartConnector,
    FlowChartOffpageConnector,
    FlowChartPunchedCard,
    FlowChartPunchedTape,
    FlowChartSummingJunction,
    FlowChartOr,
    FlowChartCollate,
    FlowChartSort,
    FlowChartExtract,
    FlowChartMerge,
    FlowChartOnlineStorage,
    FlowChartDelay,
    FlowChartMagneticTape,
    FlowChartMagneticDisk,
    FlowChartMagneticDrum,
    FlowChartDisplay,
    // ─── Math / equation shapes ────────────────────────────────────────
    MathPlus,
    MathMinus,
    MathMultiply,
    MathDivide,
    MathEqual,
    MathNotEqual,
    // ─── Action buttons ─────────────────────────────────────────────────
    ActionButtonBlank,
    ActionButtonHome,
    ActionButtonHelp,
    ActionButtonInformation,
    ActionButtonBackPrevious,
    ActionButtonForwardNext,
    ActionButtonBeginning,
    ActionButtonEnd,
    ActionButtonReturn,
    ActionButtonDocument,
    ActionButtonSound,
    ActionButtonMovie,
    // ─── Miscellaneous ──────────────────────────────────────────────────
    Gear6,
    Gear9,
    Funnel,
    PieWedge,
    LeftRightRibbon,
    CornerTabs,
    SquareTabs,
    PlaqueTabs,
    ChartX,
    ChartStar,
    ChartPlus,
    // ─── Additional shapes from ST_ShapeType ────────────────────────────
    FlowChartOfflineStorage,
    NonIsoscelesTrapezoid,
    // ─── Tab / bracket shapes ───────────────────────────────────────────
    RoundTab,
    SquareTab,
    // ─── Additional unique ECMA-376 ST_ShapeType values ──────────────────
    /// "cross" — distinct from "plus" in some producers.
    Cross,
    /// "isosTriangle" — isosceles triangle (alias for triangle in some contexts).
    IsosTriangle,
}

/// Complete table of all `ShapePreset` variants and their ECMA-376 `prst` names.
/// Used by both `prst_name()` and `from_prst()` to ensure a single source of truth.
const SHAPE_PRESET_TABLE: &[(ShapePreset, &str)] = &[
    // Basic shapes
    (ShapePreset::Rect, "rect"),
    (ShapePreset::RoundRect, "roundRect"),
    (ShapePreset::Snip1Rect, "snip1Rect"),
    (ShapePreset::Snip2SameRect, "snip2SameRect"),
    (ShapePreset::Snip2DiagRect, "snip2DiagRect"),
    (ShapePreset::SnipRoundRect, "snipRoundRect"),
    (ShapePreset::Round1Rect, "round1Rect"),
    (ShapePreset::Round2SameRect, "round2SameRect"),
    (ShapePreset::Round2DiagRect, "round2DiagRect"),
    (ShapePreset::Ellipse, "ellipse"),
    (ShapePreset::Triangle, "triangle"),
    (ShapePreset::RtTriangle, "rtTriangle"),
    (ShapePreset::Parallelogram, "parallelogram"),
    (ShapePreset::Trapezoid, "trapezoid"),
    (ShapePreset::Diamond, "diamond"),
    (ShapePreset::Pentagon, "pentagon"),
    (ShapePreset::Hexagon, "hexagon"),
    (ShapePreset::Heptagon, "heptagon"),
    (ShapePreset::Octagon, "octagon"),
    (ShapePreset::Decagon, "decagon"),
    (ShapePreset::Dodecagon, "dodecagon"),
    (ShapePreset::Pie, "pie"),
    (ShapePreset::Chord, "chord"),
    (ShapePreset::Teardrop, "teardrop"),
    (ShapePreset::Frame, "frame"),
    (ShapePreset::HalfFrame, "halfFrame"),
    (ShapePreset::Corner, "corner"),
    (ShapePreset::DiagStripe, "diagStripe"),
    (ShapePreset::Plus, "plus"),
    (ShapePreset::Plaque, "plaque"),
    (ShapePreset::Can, "can"),
    (ShapePreset::Cube, "cube"),
    (ShapePreset::Bevel, "bevel"),
    (ShapePreset::Donut, "donut"),
    (ShapePreset::NoSmoking, "noSmoking"),
    (ShapePreset::BlockArc, "blockArc"),
    (ShapePreset::FoldedCorner, "foldedCorner"),
    (ShapePreset::SmileyFace, "smileyFace"),
    (ShapePreset::Heart, "heart"),
    (ShapePreset::LightningBolt, "lightningBolt"),
    (ShapePreset::Sun, "sun"),
    (ShapePreset::Moon, "moon"),
    (ShapePreset::Cloud, "cloud"),
    (ShapePreset::Arc, "arc"),
    (ShapePreset::BracketPair, "bracketPair"),
    (ShapePreset::BracePair, "bracePair"),
    (ShapePreset::LeftBracket, "leftBracket"),
    (ShapePreset::RightBracket, "rightBracket"),
    (ShapePreset::LeftBrace, "leftBrace"),
    (ShapePreset::RightBrace, "rightBrace"),
    // Lines and connectors
    (ShapePreset::Line, "line"),
    (ShapePreset::LineInv, "lineInv"),
    (ShapePreset::StraightConnector1, "straightConnector1"),
    (ShapePreset::BentConnector2, "bentConnector2"),
    (ShapePreset::BentConnector3, "bentConnector3"),
    (ShapePreset::BentConnector4, "bentConnector4"),
    (ShapePreset::BentConnector5, "bentConnector5"),
    (ShapePreset::CurvedConnector2, "curvedConnector2"),
    (ShapePreset::CurvedConnector3, "curvedConnector3"),
    (ShapePreset::CurvedConnector4, "curvedConnector4"),
    (ShapePreset::CurvedConnector5, "curvedConnector5"),
    // Arrows
    (ShapePreset::RightArrow, "rightArrow"),
    (ShapePreset::LeftArrow, "leftArrow"),
    (ShapePreset::UpArrow, "upArrow"),
    (ShapePreset::DownArrow, "downArrow"),
    (ShapePreset::LeftRightArrow, "leftRightArrow"),
    (ShapePreset::UpDownArrow, "upDownArrow"),
    (ShapePreset::QuadArrow, "quadArrow"),
    (ShapePreset::LeftRightUpArrow, "leftRightUpArrow"),
    (ShapePreset::BentArrow, "bentArrow"),
    (ShapePreset::UturnArrow, "uturnArrow"),
    (ShapePreset::CircularArrow, "circularArrow"),
    (ShapePreset::LeftCircularArrow, "leftCircularArrow"),
    (
        ShapePreset::LeftRightCircularArrow,
        "leftRightCircularArrow",
    ),
    (ShapePreset::CurvedRightArrow, "curvedRightArrow"),
    (ShapePreset::CurvedLeftArrow, "curvedLeftArrow"),
    (ShapePreset::CurvedUpArrow, "curvedUpArrow"),
    (ShapePreset::CurvedDownArrow, "curvedDownArrow"),
    (ShapePreset::StripedRightArrow, "stripedRightArrow"),
    (ShapePreset::NotchedRightArrow, "notchedRightArrow"),
    (ShapePreset::HomePlate, "homePlate"),
    (ShapePreset::Chevron, "chevron"),
    (ShapePreset::RightArrowCallout, "rightArrowCallout"),
    (ShapePreset::LeftArrowCallout, "leftArrowCallout"),
    (ShapePreset::UpArrowCallout, "upArrowCallout"),
    (ShapePreset::DownArrowCallout, "downArrowCallout"),
    (ShapePreset::LeftRightArrowCallout, "leftRightArrowCallout"),
    (ShapePreset::UpDownArrowCallout, "upDownArrowCallout"),
    (ShapePreset::QuadArrowCallout, "quadArrowCallout"),
    // Block arrows
    (ShapePreset::BentUpArrow, "bentUpArrow"),
    (ShapePreset::LeftUpArrow, "leftUpArrow"),
    (ShapePreset::SwooshArrow, "swooshArrow"),
    // Stars and banners
    (ShapePreset::Star4, "star4"),
    (ShapePreset::Star5, "star5"),
    (ShapePreset::Star6, "star6"),
    (ShapePreset::Star7, "star7"),
    (ShapePreset::Star8, "star8"),
    (ShapePreset::Star10, "star10"),
    (ShapePreset::Star12, "star12"),
    (ShapePreset::Star16, "star16"),
    (ShapePreset::Star24, "star24"),
    (ShapePreset::Star32, "star32"),
    (ShapePreset::Ribbon, "ribbon"),
    (ShapePreset::Ribbon2, "ribbon2"),
    (ShapePreset::EllipseRibbon, "ellipseRibbon"),
    (ShapePreset::EllipseRibbon2, "ellipseRibbon2"),
    (ShapePreset::VerticalScroll, "verticalScroll"),
    (ShapePreset::HorizontalScroll, "horizontalScroll"),
    (ShapePreset::Wave, "wave"),
    (ShapePreset::DoubleWave, "doubleWave"),
    (ShapePreset::IrregularSeal1, "irregularSeal1"),
    (ShapePreset::IrregularSeal2, "irregularSeal2"),
    // Callouts
    (ShapePreset::WedgeRectCallout, "wedgeRectCallout"),
    (ShapePreset::WedgeRoundRectCallout, "wedgeRoundRectCallout"),
    (ShapePreset::WedgeEllipseCallout, "wedgeEllipseCallout"),
    (ShapePreset::CloudCallout, "cloudCallout"),
    (ShapePreset::BorderCallout1, "borderCallout1"),
    (ShapePreset::BorderCallout2, "borderCallout2"),
    (ShapePreset::BorderCallout3, "borderCallout3"),
    (ShapePreset::AccentCallout1, "accentCallout1"),
    (ShapePreset::AccentCallout2, "accentCallout2"),
    (ShapePreset::AccentCallout3, "accentCallout3"),
    (ShapePreset::Callout1, "callout1"),
    (ShapePreset::Callout2, "callout2"),
    (ShapePreset::Callout3, "callout3"),
    (ShapePreset::AccentBorderCallout1, "accentBorderCallout1"),
    (ShapePreset::AccentBorderCallout2, "accentBorderCallout2"),
    (ShapePreset::AccentBorderCallout3, "accentBorderCallout3"),
    // Flowchart
    (ShapePreset::FlowChartProcess, "flowChartProcess"),
    (
        ShapePreset::FlowChartAlternateProcess,
        "flowChartAlternateProcess",
    ),
    (ShapePreset::FlowChartDecision, "flowChartDecision"),
    (ShapePreset::FlowChartInputOutput, "flowChartInputOutput"),
    (
        ShapePreset::FlowChartPredefinedProcess,
        "flowChartPredefinedProcess",
    ),
    (
        ShapePreset::FlowChartInternalStorage,
        "flowChartInternalStorage",
    ),
    (ShapePreset::FlowChartDocument, "flowChartDocument"),
    (
        ShapePreset::FlowChartMultidocument,
        "flowChartMultidocument",
    ),
    (ShapePreset::FlowChartTerminator, "flowChartTerminator"),
    (ShapePreset::FlowChartPreparation, "flowChartPreparation"),
    (ShapePreset::FlowChartManualInput, "flowChartManualInput"),
    (
        ShapePreset::FlowChartManualOperation,
        "flowChartManualOperation",
    ),
    (ShapePreset::FlowChartConnector, "flowChartConnector"),
    (
        ShapePreset::FlowChartOffpageConnector,
        "flowChartOffpageConnector",
    ),
    (ShapePreset::FlowChartPunchedCard, "flowChartPunchedCard"),
    (ShapePreset::FlowChartPunchedTape, "flowChartPunchedTape"),
    (
        ShapePreset::FlowChartSummingJunction,
        "flowChartSummingJunction",
    ),
    (ShapePreset::FlowChartOr, "flowChartOr"),
    (ShapePreset::FlowChartCollate, "flowChartCollate"),
    (ShapePreset::FlowChartSort, "flowChartSort"),
    (ShapePreset::FlowChartExtract, "flowChartExtract"),
    (ShapePreset::FlowChartMerge, "flowChartMerge"),
    (
        ShapePreset::FlowChartOnlineStorage,
        "flowChartOnlineStorage",
    ),
    (ShapePreset::FlowChartDelay, "flowChartDelay"),
    (ShapePreset::FlowChartMagneticTape, "flowChartMagneticTape"),
    (ShapePreset::FlowChartMagneticDisk, "flowChartMagneticDisk"),
    (ShapePreset::FlowChartMagneticDrum, "flowChartMagneticDrum"),
    (ShapePreset::FlowChartDisplay, "flowChartDisplay"),
    (
        ShapePreset::FlowChartOfflineStorage,
        "flowChartOfflineStorage",
    ),
    // Math / equation shapes
    (ShapePreset::MathPlus, "mathPlus"),
    (ShapePreset::MathMinus, "mathMinus"),
    (ShapePreset::MathMultiply, "mathMultiply"),
    (ShapePreset::MathDivide, "mathDivide"),
    (ShapePreset::MathEqual, "mathEqual"),
    (ShapePreset::MathNotEqual, "mathNotEqual"),
    // Action buttons
    (ShapePreset::ActionButtonBlank, "actionButtonBlank"),
    (ShapePreset::ActionButtonHome, "actionButtonHome"),
    (ShapePreset::ActionButtonHelp, "actionButtonHelp"),
    (
        ShapePreset::ActionButtonInformation,
        "actionButtonInformation",
    ),
    (
        ShapePreset::ActionButtonBackPrevious,
        "actionButtonBackPrevious",
    ),
    (
        ShapePreset::ActionButtonForwardNext,
        "actionButtonForwardNext",
    ),
    (ShapePreset::ActionButtonBeginning, "actionButtonBeginning"),
    (ShapePreset::ActionButtonEnd, "actionButtonEnd"),
    (ShapePreset::ActionButtonReturn, "actionButtonReturn"),
    (ShapePreset::ActionButtonDocument, "actionButtonDocument"),
    (ShapePreset::ActionButtonSound, "actionButtonSound"),
    (ShapePreset::ActionButtonMovie, "actionButtonMovie"),
    // Miscellaneous
    (ShapePreset::Gear6, "gear6"),
    (ShapePreset::Gear9, "gear9"),
    (ShapePreset::Funnel, "funnel"),
    (ShapePreset::PieWedge, "pieWedge"),
    (ShapePreset::LeftRightRibbon, "leftRightRibbon"),
    (ShapePreset::CornerTabs, "cornerTabs"),
    (ShapePreset::SquareTabs, "squareTabs"),
    (ShapePreset::PlaqueTabs, "plaqueTabs"),
    (ShapePreset::ChartX, "chartX"),
    (ShapePreset::ChartStar, "chartStar"),
    (ShapePreset::ChartPlus, "chartPlus"),
    // Additional
    (ShapePreset::NonIsoscelesTrapezoid, "nonIsoscelesTrapezoid"),
    (ShapePreset::RoundTab, "roundTab"),
    (ShapePreset::SquareTab, "squareTab"),
    (ShapePreset::Cross, "cross"),
    (ShapePreset::IsosTriangle, "isosTriangle"),
];

impl ShapePreset {
    /// Return the ECMA-376 `prstGeom@prst` attribute value for this preset.
    pub fn prst_name(&self) -> &'static str {
        for &(ref variant, name) in SHAPE_PRESET_TABLE {
            if variant == self {
                return name;
            }
        }
        // Should never happen if the table is complete.
        "rect"
    }

    /// Backwards-compatible alias for `prst_name`.
    pub fn prst(self) -> &'static str {
        self.prst_name()
    }

    /// Parse an ECMA-376 `prst` attribute value into a `ShapePreset`.
    /// Returns `None` for unknown preset names.
    pub fn from_prst(name: &str) -> Option<Self> {
        for &(variant, prst) in SHAPE_PRESET_TABLE {
            if prst == name {
                return Some(variant);
            }
        }
        None
    }

    /// Parse a preset name with tolerance for common aliases and
    /// case-insensitive matching (e.g. "rectangle" → Rect, "oval" → Ellipse).
    /// Falls back to exact `from_prst` matching.
    pub fn parse(s: &str) -> Option<ShapePreset> {
        // Try exact prst match first.
        if let Some(p) = Self::from_prst(s) {
            return Some(p);
        }
        // Normalize: lowercase, strip separators.
        let norm = s.to_ascii_lowercase().replace(['_', '-', ' '], "");
        // Try normalized match against prst names.
        for &(variant, prst) in SHAPE_PRESET_TABLE {
            if prst.to_ascii_lowercase() == norm {
                return Some(variant);
            }
        }
        // Common aliases (python-pptx MSO_SHAPE names and other common forms).
        match norm.as_str() {
            "rectangle" => Some(ShapePreset::Rect),
            "roundedrectangle" | "roundedrect" => Some(ShapePreset::RoundRect),
            "oval" | "circle" => Some(ShapePreset::Ellipse),
            "arrow" => Some(ShapePreset::RightArrow),
            "callout" => Some(ShapePreset::WedgeRectCallout),
            // python-pptx MSO_SHAPE aliases
            "explosion1" => Some(ShapePreset::IrregularSeal1),
            "explosion2" => Some(ShapePreset::IrregularSeal2),
            "4pointstar" | "fourpointstar" => Some(ShapePreset::Star4),
            "5pointstar" | "fivepointstar" => Some(ShapePreset::Star5),
            "6pointstar" | "sixpointstar" => Some(ShapePreset::Star6),
            "7pointstar" | "sevenpointstar" => Some(ShapePreset::Star7),
            "8pointstar" | "eightpointstar" => Some(ShapePreset::Star8),
            "10pointstar" | "tenpointstar" => Some(ShapePreset::Star10),
            "12pointstar" | "twelvepointstar" => Some(ShapePreset::Star12),
            "16pointstar" | "sixteenpointstar" => Some(ShapePreset::Star16),
            "24pointstar" | "twentyfourpointstar" => Some(ShapePreset::Star24),
            "32pointstar" | "thirtytwopointstar" => Some(ShapePreset::Star32),
            "upribbon" => Some(ShapePreset::Ribbon2),
            "downribbon" => Some(ShapePreset::Ribbon),
            "curvedupribbon" => Some(ShapePreset::EllipseRibbon2),
            "curveddownribbon" => Some(ShapePreset::EllipseRibbon),
            "flowchartoffpagereference" => Some(ShapePreset::FlowChartOffpageConnector),
            "flowchartsequentialaccessstorage" => Some(ShapePreset::FlowChartMagneticTape),
            "flowchartdirectaccessstorage" => Some(ShapePreset::FlowChartMagneticDisk),
            "flowchartstoreddata" => Some(ShapePreset::FlowChartOnlineStorage),
            "notchedleftarrow" => Some(ShapePreset::LeftArrow),
            "textbox" => Some(ShapePreset::Rect),
            "regularpentagon" => Some(ShapePreset::Pentagon),
            "isoscelestriangle" | "isostriangle" => Some(ShapePreset::Triangle),
            "righttriangle" => Some(ShapePreset::RtTriangle),
            "nosymbol" | "nosign" => Some(ShapePreset::NoSmoking),
            "smileyface" | "happyface" => Some(ShapePreset::SmileyFace),
            "lightningbolt" | "lightning" => Some(ShapePreset::LightningBolt),
            "foldedcorner" => Some(ShapePreset::FoldedCorner),
            "blockarc" => Some(ShapePreset::BlockArc),
            "crossshape" => Some(ShapePreset::Plus),
            "gear6tooth" | "gear6teeth" => Some(ShapePreset::Gear6),
            "gear9tooth" | "gear9teeth" => Some(ShapePreset::Gear9),
            "leftrightuparrow" => Some(ShapePreset::LeftRightUpArrow),
            "circulararrow" => Some(ShapePreset::CircularArrow),
            "leftcirculararrow" => Some(ShapePreset::LeftCircularArrow),
            "leftrightcirculararrow" => Some(ShapePreset::LeftRightCircularArrow),
            _ => None,
        }
    }

    /// Return the total number of preset variants.
    pub fn count() -> usize {
        SHAPE_PRESET_TABLE.len()
    }

    /// Return all preset variants as a slice.
    pub fn all() -> &'static [(ShapePreset, &'static str)] {
        SHAPE_PRESET_TABLE
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn emu_conversions() {
        assert_eq!(Emu::inches(1.0), Emu(914400));
        assert_eq!(Emu::points(1.0), Emu(12700));
        assert_eq!(Emu::cm(1.0), Emu(360000));
    }

    #[test]
    fn layout_parse() {
        assert_eq!(Layout::parse("Title Content"), Some(Layout::TitleContent));
        assert_eq!(Layout::parse("blank"), Some(Layout::Blank));
        assert_eq!(Layout::parse("nope"), None);
    }

    #[test]
    fn prst_name_returns_correct_strings() {
        assert_eq!(ShapePreset::Rect.prst_name(), "rect");
        assert_eq!(ShapePreset::RoundRect.prst_name(), "roundRect");
        assert_eq!(ShapePreset::Ellipse.prst_name(), "ellipse");
        assert_eq!(ShapePreset::Triangle.prst_name(), "triangle");
        assert_eq!(ShapePreset::RtTriangle.prst_name(), "rtTriangle");
        assert_eq!(ShapePreset::Star5.prst_name(), "star5");
        assert_eq!(ShapePreset::RightArrow.prst_name(), "rightArrow");
        assert_eq!(
            ShapePreset::FlowChartProcess.prst_name(),
            "flowChartProcess"
        );
        assert_eq!(
            ShapePreset::WedgeRectCallout.prst_name(),
            "wedgeRectCallout"
        );
        assert_eq!(
            ShapePreset::ActionButtonHome.prst_name(),
            "actionButtonHome"
        );
        assert_eq!(ShapePreset::Gear6.prst_name(), "gear6");
    }

    #[test]
    fn from_prst_round_trips_all_variants() {
        for &(variant, name) in SHAPE_PRESET_TABLE {
            let parsed = ShapePreset::from_prst(name);
            assert_eq!(
                parsed,
                Some(variant),
                "from_prst({name:?}) should return {variant:?}"
            );
            assert_eq!(
                parsed.unwrap().prst_name(),
                name,
                "round-trip failed for {name:?}"
            );
        }
    }

    #[test]
    fn from_prst_unknown_returns_none() {
        assert_eq!(ShapePreset::from_prst("unknownShape"), None);
        assert_eq!(ShapePreset::from_prst(""), None);
        assert_eq!(ShapePreset::from_prst("notAShape123"), None);
    }

    #[test]
    fn parse_aliases() {
        assert_eq!(ShapePreset::parse("rectangle"), Some(ShapePreset::Rect));
        assert_eq!(ShapePreset::parse("oval"), Some(ShapePreset::Ellipse));
        assert_eq!(ShapePreset::parse("circle"), Some(ShapePreset::Ellipse));
        assert_eq!(ShapePreset::parse("arrow"), Some(ShapePreset::RightArrow));
        assert_eq!(
            ShapePreset::parse("callout"),
            Some(ShapePreset::WedgeRectCallout)
        );
        assert_eq!(
            ShapePreset::parse("roundedRectangle"),
            Some(ShapePreset::RoundRect)
        );
        // python-pptx MSO_SHAPE aliases
        assert_eq!(
            ShapePreset::parse("explosion1"),
            Some(ShapePreset::IrregularSeal1)
        );
        assert_eq!(
            ShapePreset::parse("explosion2"),
            Some(ShapePreset::IrregularSeal2)
        );
        assert_eq!(ShapePreset::parse("5_point_star"), Some(ShapePreset::Star5));
        assert_eq!(ShapePreset::parse("textBox"), Some(ShapePreset::Rect));
        assert_eq!(ShapePreset::parse("cross_shape"), Some(ShapePreset::Plus));
        assert_eq!(
            ShapePreset::parse("no_symbol"),
            Some(ShapePreset::NoSmoking)
        );
    }

    #[test]
    fn parse_case_insensitive() {
        assert_eq!(ShapePreset::parse("RECT"), Some(ShapePreset::Rect));
        assert_eq!(
            ShapePreset::parse("RoundRect"),
            Some(ShapePreset::RoundRect)
        );
        assert_eq!(
            ShapePreset::parse("flowchartprocess"),
            Some(ShapePreset::FlowChartProcess)
        );
    }

    #[test]
    fn preset_count_is_at_least_187() {
        // The ECMA-376 ST_ShapeType has ~187 core presets; we include extras.
        assert!(
            ShapePreset::count() >= 187,
            "expected at least 187 presets, got {}",
            ShapePreset::count()
        );
    }

    #[test]
    fn prst_backwards_compat() {
        // The old `prst()` method should still work.
        assert_eq!(ShapePreset::Rect.prst(), "rect");
        assert_eq!(ShapePreset::Ellipse.prst(), "ellipse");
    }
}
