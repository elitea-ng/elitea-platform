//! The types that cross the [`DesktopDriver`](crate::DesktopDriver) trait.
//!
//! Every one of them is ours. A vendor type never appears here, so a
//! re-vendor of agent-desktop changes no signature outside this crate
//! (ADR-0047 decision 1).

use serde::{Deserialize, Serialize};

/// The longest text one `type_text` or `act(SetValue)` writes, in
/// characters (the vendor's own `type_text` limit).
pub const MAX_TYPED_CHARS: usize = 10_000;

/// Validates text the driver is asked to write into an element
/// (`type_text`, `act(SetValue)`).
///
/// # Errors
/// [`DesktopError::InvalidArgs`](crate::DesktopError::InvalidArgs) when the
/// text is longer than [`MAX_TYPED_CHARS`].
pub fn validate_text_write(text: &str) -> Result<(), crate::DesktopError> {
    if text.chars().count() > MAX_TYPED_CHARS {
        return Err(crate::DesktopError::InvalidArgs(format!(
            "text exceeds {MAX_TYPED_CHARS} characters"
        )));
    }
    Ok(())
}

/// A rectangle in global screen points (top-left origin, as macOS reports
/// accessibility frames).
#[derive(Debug, Clone, Copy, PartialEq, Serialize, Deserialize)]
pub struct Rect {
    pub x: f64,
    pub y: f64,
    pub width: f64,
    pub height: f64,
}

impl Rect {
    /// Whether every field is finite and the size is not negative.
    #[must_use]
    pub fn is_valid(&self) -> bool {
        [self.x, self.y, self.width, self.height]
            .iter()
            .all(|v| v.is_finite())
            && self.width >= 0.0
            && self.height >= 0.0
    }
}

/// A point in global screen points.
#[derive(Debug, Clone, Copy, PartialEq, Serialize, Deserialize)]
pub struct Point {
    pub x: f64,
    pub y: f64,
}

/// A running application.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct AppInfo {
    pub name: String,
    pub pid: u32,
    /// The bundle id the approvals and the deny list key on (ADR-0047
    /// decision 3). `None` for a process with no bundle.
    pub bundle_id: Option<String>,
    /// Whether it is the frontmost application.
    pub focused: bool,
}

/// One top-level window.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct WindowInfo {
    /// The driver's window id. Stable while the window lives; not a title.
    pub id: String,
    pub title: String,
    pub app_name: String,
    pub pid: u32,
    pub bundle_id: Option<String>,
    pub bounds: Option<Rect>,
    pub focused: bool,
    pub minimized: bool,
}

/// Which window a snapshot, a key chord or a capture targets.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind", content = "value")]
pub enum WindowTarget {
    /// The focused window of the frontmost application.
    Focused,
    /// The focused (or only) window of the application with this bundle id.
    App(String),
    /// One window, by the id [`WindowInfo::id`] reported, of the
    /// application with this bundle id: a window of any other app is not
    /// found.
    Window { bundle_id: String, id: String },
}

impl WindowTarget {
    /// The application the target names, if it names one.
    #[must_use]
    pub fn bundle_id(&self) -> Option<&str> {
        match self {
            Self::Focused => None,
            Self::App(bundle_id) | Self::Window { bundle_id, .. } => Some(bundle_id),
        }
    }
}

/// Whether a process's bundle id (`actual`, as the OS reports it) is the
/// application a call names. An unknown bundle id never matches: a window
/// or element whose application cannot be told is not the named one's.
#[must_use]
pub fn bundle_matches(actual: Option<&str>, named: &str) -> bool {
    actual.is_some_and(|actual| actual.eq_ignore_ascii_case(named.trim()))
}

/// The limits of one snapshot.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct SnapshotBudget {
    /// The deepest level read below the window (the window is depth 0).
    pub max_depth: u8,
    /// The most nodes the snapshot returns. Past it, the snapshot is cut and
    /// marked [`Snapshot::truncated`].
    pub max_nodes: usize,
    /// Keep only visible nodes that can be acted on, plus the containers on
    /// the path to them (ADR-0047 decision 2).
    pub actionable_only: bool,
    /// The longest name or value kept, in characters; longer text is cut
    /// with an ellipsis and the node is not marked truncated.
    pub max_text_chars: usize,
}

impl Default for SnapshotBudget {
    fn default() -> Self {
        Self {
            max_depth: 12,
            max_nodes: 400,
            actionable_only: true,
            max_text_chars: 200,
        }
    }
}

/// A ref a snapshot handed out: `@<snapshot>:eN`. It names one element of
/// one snapshot, and only of that snapshot.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct ElementRef {
    snapshot: SnapshotId,
    index: u32,
}

impl ElementRef {
    #[must_use]
    pub fn new(snapshot: SnapshotId, index: u32) -> Self {
        Self { snapshot, index }
    }

    #[must_use]
    pub fn snapshot(&self) -> &SnapshotId {
        &self.snapshot
    }

    #[must_use]
    pub fn index(&self) -> u32 {
        self.index
    }

    /// Parses `@<snapshot>:eN`.
    ///
    /// # Errors
    /// [`DesktopError::InvalidArgs`](crate::DesktopError::InvalidArgs) when
    /// the text is not a ref.
    pub fn parse(text: &str) -> Result<Self, crate::DesktopError> {
        let bad = || crate::DesktopError::InvalidArgs(format!("not an element ref: {text:?}"));
        let rest = text.strip_prefix('@').ok_or_else(bad)?;
        let (snapshot, element) = rest.rsplit_once(':').ok_or_else(bad)?;
        let digits = element.strip_prefix('e').ok_or_else(bad)?;
        if digits.is_empty()
            || !digits.bytes().all(|b| b.is_ascii_digit())
            || digits.starts_with('0')
        {
            return Err(bad());
        }
        let index = digits.parse::<u32>().map_err(|_| bad())?;
        Ok(Self {
            snapshot: SnapshotId::parse(snapshot)?,
            index,
        })
    }
}

impl std::fmt::Display for ElementRef {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "@{}:e{}", self.snapshot, self.index)
    }
}

impl From<ElementRef> for String {
    fn from(value: ElementRef) -> Self {
        value.to_string()
    }
}

impl TryFrom<String> for ElementRef {
    type Error = crate::DesktopError;
    fn try_from(value: String) -> Result<Self, Self::Error> {
        Self::parse(&value)
    }
}

/// The id of one snapshot: `s` and a decimal sequence number, unique within
/// one driver.
#[derive(Debug, Clone, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(try_from = "String", into = "String")]
pub struct SnapshotId(String);

impl SnapshotId {
    #[must_use]
    pub fn from_sequence(sequence: u64) -> Self {
        Self(format!("s{sequence}"))
    }

    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }

    /// Parses `sN`.
    ///
    /// # Errors
    /// [`DesktopError::InvalidArgs`](crate::DesktopError::InvalidArgs) when
    /// the text is not a snapshot id.
    pub fn parse(text: &str) -> Result<Self, crate::DesktopError> {
        let ok = text.strip_prefix('s').is_some_and(|d| {
            !d.is_empty() && d.len() <= 20 && d.bytes().all(|b| b.is_ascii_digit())
        });
        if ok {
            Ok(Self(text.to_owned()))
        } else {
            Err(crate::DesktopError::InvalidArgs(format!(
                "not a snapshot id: {text:?}"
            )))
        }
    }
}

impl std::fmt::Display for SnapshotId {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(&self.0)
    }
}

impl From<SnapshotId> for String {
    fn from(value: SnapshotId) -> Self {
        value.0
    }
}

impl TryFrom<String> for SnapshotId {
    type Error = crate::DesktopError;
    fn try_from(value: String) -> Result<Self, Self::Error> {
        Self::parse(&value)
    }
}

/// One node of a snapshot.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Node {
    /// Set when the node can be acted on.
    pub element_ref: Option<ElementRef>,
    /// The normalised role (`button`, `textfield`, `checkbox`, ...).
    pub role: String,
    pub name: Option<String>,
    /// The element's value. Always `None` when [`Node::secure`] is set.
    pub value: Option<String>,
    pub description: Option<String>,
    /// A secure text field (`AXSecureTextField`): no value is ever read, and
    /// typing into it is refused (ADR-0047 decision 5).
    pub secure: bool,
    /// State words (`focused`, `disabled`, `checked`, `expanded`, ...).
    pub states: Vec<String>,
    /// The actions the element advertises.
    pub actions: Vec<String>,
    pub bounds: Option<Rect>,
    pub children: Vec<Node>,
}

/// One window's filtered accessibility tree.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Snapshot {
    pub id: SnapshotId,
    pub window: WindowInfo,
    pub root: Node,
    /// How many nodes `root` holds.
    pub node_count: usize,
    /// The budget cut the tree: a node, a level or a subtree is missing.
    pub truncated: bool,
}

/// An action on a ref (ADR-0047 decision 2, `computer_act`).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum Action {
    Press,
    SetValue {
        value: String,
    },
    Focus,
    Select {
        option: String,
    },
    Scroll {
        direction: ScrollDirection,
        amount: u32,
    },
    Expand,
    Collapse,
    Toggle,
}

impl Action {
    /// Whether the action writes text into the element, and so is refused
    /// on a secure field.
    #[must_use]
    pub fn writes_text(&self) -> bool {
        matches!(self, Self::SetValue { .. })
    }

    /// Checks the action's arguments before anything is sent: a
    /// `SetValue` is capped as `type_text` is.
    ///
    /// # Errors
    /// [`DesktopError::InvalidArgs`](crate::DesktopError::InvalidArgs).
    pub fn validate(&self) -> Result<(), crate::DesktopError> {
        match self {
            Self::SetValue { value } => validate_text_write(value),
            _ => Ok(()),
        }
    }

    #[must_use]
    pub fn name(&self) -> &'static str {
        match self {
            Self::Press => "press",
            Self::SetValue { .. } => "set_value",
            Self::Focus => "focus",
            Self::Select { .. } => "select",
            Self::Scroll { .. } => "scroll",
            Self::Expand => "expand",
            Self::Collapse => "collapse",
            Self::Toggle => "toggle",
        }
    }
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ScrollDirection {
    Up,
    Down,
    Left,
    Right,
}

/// How an action reached the application.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Delivery {
    /// An accessibility call (`AXPress`, `AXValue`): the pointer did not move
    /// and focus was not taken.
    Semantic,
    /// Synthetic keyboard or pointer events, tagged as the driver's own
    /// (see [`DesktopDriver::is_synthetic_event`](crate::DesktopDriver::is_synthetic_event)).
    Physical,
    /// Nothing was sent: the element was already in the asked state.
    NotNeeded,
}

/// What an action did.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ActionOutcome {
    pub action: String,
    pub delivery: Delivery,
    /// The driver read the element back and saw the change.
    pub verified: bool,
}

/// A modifier of a key chord.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Modifier {
    Command,
    Control,
    Option,
    Shift,
}

/// One key chord: a key name (`a`, `return`, `escape`, `f5`, `left`) and its
/// modifiers.
#[derive(Debug, Clone, PartialEq, Eq, Serialize, Deserialize)]
pub struct KeyChord {
    pub key: String,
    pub modifiers: Vec<Modifier>,
}

impl KeyChord {
    /// Parses `cmd+shift+a` style text. Modifier names: `cmd`/`command`/
    /// `meta`, `ctrl`/`control`, `alt`/`option`/`opt`, `shift`.
    ///
    /// # Errors
    /// [`DesktopError::InvalidArgs`](crate::DesktopError::InvalidArgs) for an
    /// empty chord, an unknown modifier, a repeated modifier or no key.
    pub fn parse(text: &str) -> Result<Self, crate::DesktopError> {
        let bad =
            |why: &str| crate::DesktopError::InvalidArgs(format!("key chord {text:?}: {why}"));
        let parts: Vec<&str> = text.split('+').map(str::trim).collect();
        let Some((key, mods)) = parts.split_last() else {
            return Err(bad("empty"));
        };
        if key.is_empty() {
            return Err(bad("no key"));
        }
        let mut modifiers = Vec::new();
        for m in mods {
            let modifier = match m.to_ascii_lowercase().as_str() {
                "cmd" | "command" | "meta" => Modifier::Command,
                "ctrl" | "control" => Modifier::Control,
                "alt" | "option" | "opt" => Modifier::Option,
                "shift" => Modifier::Shift,
                _ => return Err(bad("unknown modifier")),
            };
            if modifiers.contains(&modifier) {
                return Err(bad("repeated modifier"));
            }
            modifiers.push(modifier);
        }
        Ok(Self {
            key: key.to_ascii_lowercase(),
            modifiers,
        })
    }
}

/// The pointer button of a coordinate click (`computer_click_at`).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PointerButton {
    #[default]
    Left,
    Right,
}

/// The most clicks one `click_at` sends (a triple click selects a line).
pub const MAX_CLICK_COUNT: u32 = 3;

/// One coordinate click (ADR-0047 decision 2, `computer_click_at`): the
/// fallback for an application with no accessible tree. The point is in
/// global screen points, inside the window `window_id` names, and that
/// window must be the topmost one at the point.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct ClickAt {
    /// The application the window belongs to; it must be frontmost.
    pub bundle_id: String,
    /// The window, by [`WindowInfo::id`].
    pub window_id: String,
    pub point: Point,
    pub button: PointerButton,
    /// 1 to [`MAX_CLICK_COUNT`].
    pub count: u32,
}

impl ClickAt {
    /// Checks the click's own arguments before anything is read or sent.
    ///
    /// # Errors
    /// [`DesktopError::InvalidArgs`](crate::DesktopError::InvalidArgs) for a
    /// point that is not finite or a count outside 1 to [`MAX_CLICK_COUNT`].
    pub fn validate(&self) -> Result<(), crate::DesktopError> {
        if !(self.point.x.is_finite() && self.point.y.is_finite()) {
            return Err(crate::DesktopError::InvalidArgs(
                "the point is not finite".into(),
            ));
        }
        if !(1..=MAX_CLICK_COUNT).contains(&self.count) {
            return Err(crate::DesktopError::InvalidArgs(format!(
                "count must be 1 to {MAX_CLICK_COUNT}"
            )));
        }
        Ok(())
    }
}

/// Whether `point` is inside `rect` (the far edges excluded).
#[must_use]
pub fn rect_contains(rect: &Rect, point: &Point) -> bool {
    point.x >= rect.x
        && point.y >= rect.y
        && point.x < rect.x + rect.width
        && point.y < rect.y + rect.height
}

/// The window topmost at `point`, from windows listed front to back: the
/// first one, not minimized, whose bounds hold the point.
#[must_use]
pub fn topmost_at<'a>(front_to_back: &'a [WindowInfo], point: &Point) -> Option<&'a WindowInfo> {
    front_to_back
        .iter()
        .find(|w| !w.minimized && w.bounds.as_ref().is_some_and(|b| rect_contains(b, point)))
}

/// What `open_app` did.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct OpenedApp {
    pub name: String,
    pub pid: u32,
    pub bundle_id: String,
    /// The window the app presented, when it presented one in time.
    pub window: Option<WindowInfo>,
}

/// A captured image.
#[derive(Debug, Clone, PartialEq)]
pub struct Capture {
    /// PNG bytes.
    pub png: Vec<u8>,
    pub width: u32,
    pub height: u32,
    /// Device pixels per point of the display the capture came from.
    pub scale: f64,
    /// What the image shows, in global screen points.
    pub region: Rect,
}

/// What to capture.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum CaptureTarget {
    /// One window of the app with this bundle id, by [`WindowInfo::id`]:
    /// the window alone, without its shadow, so the image maps 1:1 onto
    /// the window's frame.
    Window { bundle_id: String, id: String },
    /// A region of one window, in points from the window's top-left
    /// corner: the window is captured alone and cropped, so no other
    /// window that overlaps the region shows in the image.
    WindowRegion {
        bundle_id: String,
        id: String,
        region: Rect,
    },
}

/// The pixel box (x, y, width, height) of `region` (window points from
/// its top-left corner) in a capture of the whole window that is
/// `image_width` x `image_height` pixels for a frame of `frame_width` x
/// `frame_height` points. The capture has no shadow, so pixels map onto
/// the frame linearly.
///
/// # Errors
/// [`DesktopError::InvalidArgs`](crate::DesktopError::InvalidArgs) for an
/// empty frame or image, or a region not inside the frame.
pub fn region_pixels(
    frame_width: f64,
    frame_height: f64,
    region: &Rect,
    image_width: u32,
    image_height: u32,
) -> Result<(u32, u32, u32, u32), crate::DesktopError> {
    let bad = |why: &str| crate::DesktopError::InvalidArgs(why.to_owned());
    if !(frame_width.is_finite() && frame_height.is_finite())
        || frame_width <= 0.0
        || frame_height <= 0.0
        || image_width == 0
        || image_height == 0
    {
        return Err(bad("the window has no frame"));
    }
    if !region.is_valid()
        || region.x < 0.0
        || region.y < 0.0
        || region.width <= 0.0
        || region.height <= 0.0
        || region.x + region.width > frame_width
        || region.y + region.height > frame_height
    {
        return Err(bad("the region is outside the window"));
    }
    let sx = f64::from(image_width) / frame_width;
    let sy = f64::from(image_height) / frame_height;
    // Inside the frame, so every value is in 0..=image size.
    #[allow(clippy::cast_possible_truncation, clippy::cast_sign_loss)]
    let px = |v: f64, max: u32| -> u32 { (v.round().max(0.0) as u32).min(max) };
    let x = px(region.x * sx, image_width - 1);
    let y = px(region.y * sy, image_height - 1);
    let w = px(region.width * sx, image_width - x).max(1);
    let h = px(region.height * sy, image_height - y).max(1);
    Ok((x, y, w, h))
}

/// A condition [`DesktopDriver::wait`](crate::DesktopDriver::wait) waits for.
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum WaitCondition {
    /// A window of the app with this bundle id whose title contains the
    /// text (any window when `title_contains` is `None`).
    Window {
        bundle_id: String,
        title_contains: Option<String>,
    },
    /// The element a ref names has (or, `present: false`, lacks) the state.
    RefState {
        /// The application the ref's snapshot was taken of.
        bundle_id: String,
        element_ref: ElementRef,
        state: String,
        present: bool,
    },
    /// A node whose name or value contains the text appears in the window.
    Text { target: WindowTarget, text: String },
    /// Two consecutive snapshots of the window are the same.
    Stable { target: WindowTarget },
}

/// The macOS privacy grants the driver depends on (ADR-0047 decision 7).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
pub struct Permissions {
    pub accessibility: Grant,
    pub screen_recording: Grant,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum Grant {
    Granted,
    Denied,
    Unknown,
    /// The platform needs no grant for this.
    NotRequired,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn text_writes_are_capped_in_characters() {
        assert_eq!(validate_text_write(&"a".repeat(MAX_TYPED_CHARS)), Ok(()));
        // Multi-byte characters count once each.
        assert_eq!(validate_text_write(&"é".repeat(MAX_TYPED_CHARS)), Ok(()));
        assert_eq!(
            validate_text_write(&"a".repeat(MAX_TYPED_CHARS + 1))
                .unwrap_err()
                .code(),
            "invalid_args"
        );
    }

    fn window(id: &str, x: f64, minimized: bool) -> WindowInfo {
        WindowInfo {
            id: id.into(),
            title: String::new(),
            app_name: String::new(),
            pid: 1,
            bundle_id: None,
            bounds: Some(Rect {
                x,
                y: 0.0,
                width: 100.0,
                height: 100.0,
            }),
            focused: false,
            minimized,
        }
    }

    #[test]
    fn the_topmost_window_is_the_first_that_holds_the_point() {
        let windows = [
            window("front", 50.0, false),
            window("hidden", 0.0, true),
            window("back", 0.0, false),
        ];
        let p = |x| Point { x, y: 10.0 };
        assert_eq!(topmost_at(&windows, &p(60.0)).unwrap().id, "front");
        // A minimized window covers nothing.
        assert_eq!(topmost_at(&windows, &p(10.0)).unwrap().id, "back");
        // The far edge is outside.
        assert!(topmost_at(&windows, &p(150.0)).is_none());
    }

    #[test]
    fn a_click_needs_a_finite_point_and_one_to_three_clicks() {
        let click = |x: f64, count| ClickAt {
            bundle_id: "com.apple.TextEdit".into(),
            window_id: "w".into(),
            point: Point { x, y: 1.0 },
            button: PointerButton::Left,
            count,
        };
        assert_eq!(click(1.0, 1).validate(), Ok(()));
        assert_eq!(click(1.0, 3).validate(), Ok(()));
        for bad in [click(f64::NAN, 1), click(1.0, 0), click(1.0, 4)] {
            assert_eq!(bad.validate().unwrap_err().code(), "invalid_args");
        }
    }

    #[test]
    fn an_unknown_bundle_is_never_the_named_one() {
        assert!(bundle_matches(
            Some("com.apple.TextEdit"),
            "com.apple.textedit"
        ));
        assert!(!bundle_matches(
            Some("com.apple.Notes"),
            "com.apple.TextEdit"
        ));
        assert!(!bundle_matches(None, "com.apple.TextEdit"));
        assert_eq!(
            WindowTarget::Window {
                bundle_id: "a.b".into(),
                id: "1".into()
            }
            .bundle_id(),
            Some("a.b")
        );
        assert_eq!(WindowTarget::Focused.bundle_id(), None);
    }

    #[test]
    fn a_region_maps_onto_a_shadowless_window_capture() {
        let r = |x, y, width, height| Rect {
            x,
            y,
            width,
            height,
        };
        // A 400 x 300 point window captured at 2x: 800 x 600 pixels.
        assert_eq!(
            region_pixels(400.0, 300.0, &r(100.0, 50.0, 40.0, 20.0), 800, 600),
            Ok((200, 100, 80, 40))
        );
        // The whole frame is the whole image.
        assert_eq!(
            region_pixels(400.0, 300.0, &r(0.0, 0.0, 400.0, 300.0), 800, 600),
            Ok((0, 0, 800, 600))
        );
        // At 1x the pixels are the points.
        assert_eq!(
            region_pixels(400.0, 300.0, &r(10.0, 10.0, 5.0, 5.0), 400, 300),
            Ok((10, 10, 5, 5))
        );
        for bad in [
            r(390.0, 0.0, 20.0, 10.0),
            r(-1.0, 0.0, 10.0, 10.0),
            r(0.0, 0.0, 0.0, 10.0),
        ] {
            assert_eq!(
                region_pixels(400.0, 300.0, &bad, 800, 600)
                    .unwrap_err()
                    .code(),
                "invalid_args",
                "{bad:?}"
            );
        }
        assert!(region_pixels(0.0, 300.0, &r(0.0, 0.0, 1.0, 1.0), 800, 600).is_err());
    }

    #[test]
    fn set_value_is_capped_like_type_text() {
        let long = Action::SetValue {
            value: "a".repeat(MAX_TYPED_CHARS + 1),
        };
        assert_eq!(long.validate().unwrap_err().code(), "invalid_args");
        let ok = Action::SetValue {
            value: "hello".into(),
        };
        assert_eq!(ok.validate(), Ok(()));
        assert_eq!(Action::Press.validate(), Ok(()));
    }
}
