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
    /// One window, by the id [`WindowInfo::id`] reported.
    Window(String),
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
    /// One window, by [`WindowInfo::id`].
    Window { id: String },
    /// A region in global screen points, inside one display.
    Region { rect: Rect },
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
