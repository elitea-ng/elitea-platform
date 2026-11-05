//! A model's tool call parsed into a typed [`BrowserCall`].
//!
//! Parsing is where the contract's argument rules live, so both drivers
//! refuse the same calls with the same codes: unknown arguments, a ref that
//! is not `eN` or `@<snapshot>:eN`, a `browser_wait` with no condition or
//! two, a key that is not a key, and (ADR-0048 decision 5) a URL whose
//! scheme is refused. A driver only ever receives a call that passed here.

use std::fmt;
use std::time::Duration;

use serde_json::{Map, Value};

use crate::error::{BrowserError, ErrorCode};
use crate::spec::{self, DEFAULT_MAX_CHARS, DEFAULT_MAX_NODES, DEFAULT_WAIT_MS, ToolKind};

/// The JSON-schema pattern of a ref argument.
pub const REF_PATTERN: &str = "^(@[A-Za-z0-9_-]{1,32}:)?e[1-9][0-9]{0,6}$";

/// The id of one snapshot. Refs are bound to it: `@<id>:eN`.
///
/// Letters, digits, `_` and `-`, 1 to 32 characters. A driver hands them
/// out with [`crate::snapshot::SnapshotSlot::next_id`] (`s1`, `s2`, …).
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct SnapshotId(String);

impl SnapshotId {
    /// `text` as a snapshot id, if it is one.
    #[must_use]
    pub fn new(text: &str) -> Option<Self> {
        let valid = (1..=32).contains(&text.len())
            && text
                .bytes()
                .all(|byte| byte.is_ascii_alphanumeric() || byte == b'_' || byte == b'-');
        valid.then(|| Self(text.to_owned()))
    }

    /// The `n`th id of a sequence: `s<n>`.
    #[must_use]
    pub fn sequence(n: u64) -> Self {
        Self(format!("s{n}"))
    }

    #[must_use]
    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl fmt::Display for SnapshotId {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.0)
    }
}

/// A ref the model passed: `eN` (the latest snapshot) or `@<snapshot>:eN`.
#[derive(Clone, Debug, Eq, Hash, PartialEq)]
pub struct ElementRef {
    /// The snapshot the ref was pinned to, if the model pinned it.
    pub snapshot: Option<SnapshotId>,
    /// `N` of `eN`, from 1.
    pub index: u32,
}

impl ElementRef {
    /// `eN` in the latest snapshot.
    #[must_use]
    pub const fn latest(index: u32) -> Self {
        Self {
            snapshot: None,
            index,
        }
    }

    /// `@<snapshot>:eN`.
    #[must_use]
    pub const fn pinned(snapshot: SnapshotId, index: u32) -> Self {
        Self {
            snapshot: Some(snapshot),
            index,
        }
    }

    /// Parse `eN` or `@<snapshot>:eN`.
    ///
    /// # Errors
    ///
    /// `invalid_argument` when `text` is neither.
    pub fn parse(text: &str) -> Result<Self, BrowserError> {
        let invalid = || {
            BrowserError::invalid(format!(
                "{text:?} is not a ref: use eN or @<snapshot>:eN from browser_snapshot"
            ))
        };
        let (snapshot, local) = match text.strip_prefix('@') {
            Some(rest) => {
                let (id, local) = rest.split_once(':').ok_or_else(invalid)?;
                (Some(SnapshotId::new(id).ok_or_else(invalid)?), local)
            }
            None => (None, text),
        };
        let digits = local.strip_prefix('e').ok_or_else(invalid)?;
        let well_formed = (1..=7).contains(&digits.len())
            && digits.bytes().all(|byte| byte.is_ascii_digit())
            && !digits.starts_with('0');
        if !well_formed {
            return Err(invalid());
        }
        let index = digits.parse().map_err(|_| invalid())?;
        Ok(Self { snapshot, index })
    }
}

impl fmt::Display for ElementRef {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match &self.snapshot {
            Some(snapshot) => write!(formatter, "@{snapshot}:e{}", self.index),
            None => write!(formatter, "e{}", self.index),
        }
    }
}

/// `browser_snapshot`'s mode.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum SnapshotMode {
    Tree,
    Text,
}

/// A page scroll direction.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Direction {
    Up,
    Down,
    Left,
    Right,
}

/// What `browser_scroll` scrolls.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ScrollTarget {
    /// This element into view.
    Element(ElementRef),
    /// The page, by `pixels` (`None`: one viewport).
    Page {
        direction: Direction,
        pixels: Option<u32>,
    },
}

/// A modifier of a key chord.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum Modifier {
    Control,
    Alt,
    Shift,
    Meta,
}

/// One key or chord for `browser_press`: modifiers (sorted, no repeats) and
/// a key, which is one character or a named key from [`NAMED_KEYS`].
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Key {
    pub modifiers: Vec<Modifier>,
    pub key: String,
}

/// The named keys `browser_press` takes, spelled as DOM `KeyboardEvent.key`.
pub const NAMED_KEYS: &[&str] = &[
    "Enter",
    "Tab",
    "Escape",
    "Backspace",
    "Delete",
    "Space",
    "Home",
    "End",
    "PageUp",
    "PageDown",
    "ArrowUp",
    "ArrowDown",
    "ArrowLeft",
    "ArrowRight",
    "F1",
    "F2",
    "F3",
    "F4",
    "F5",
    "F6",
    "F7",
    "F8",
    "F9",
    "F10",
    "F11",
    "F12",
];

impl Key {
    /// Parse `Enter`, `a`, `Control+A`, `Meta+Shift+K`.
    ///
    /// # Errors
    ///
    /// `invalid_argument` for anything else.
    pub fn parse(text: &str) -> Result<Self, BrowserError> {
        let invalid = || BrowserError::invalid(format!("{text:?} is not a key or chord"));
        let mut parts: Vec<&str> = text.split('+').collect();
        // `+` itself, alone or as the last key of a chord (`Shift++`).
        if text.ends_with("++") || text == "+" {
            parts.pop();
            if let Some(last) = parts.last_mut() {
                *last = "+";
            }
        }
        let key = parts.pop().ok_or_else(invalid)?;
        let mut modifiers = Vec::new();
        for part in parts {
            let modifier = match part {
                "Control" | "Ctrl" => Modifier::Control,
                "Alt" | "Option" => Modifier::Alt,
                "Shift" => Modifier::Shift,
                "Meta" | "Cmd" | "Command" => Modifier::Meta,
                _ => return Err(invalid()),
            };
            if modifiers.contains(&modifier) {
                return Err(invalid());
            }
            modifiers.push(modifier);
        }
        modifiers.sort();
        let single = key.chars().count() == 1 && !key.chars().any(char::is_control);
        if !single && !NAMED_KEYS.contains(&key) {
            return Err(invalid());
        }
        Ok(Self {
            modifiers,
            key: key.to_owned(),
        })
    }
}

/// The one condition `browser_wait` waits for.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum WaitCondition {
    Element(ElementRef),
    Text(String),
    /// A URL pattern; `*` matches any run of characters ([`url_matches`]).
    Url(String),
    NetworkIdle,
}

/// A parsed browser tool call.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum BrowserCall {
    Open {
        url: String,
    },
    Snapshot {
        mode: SnapshotMode,
        max_nodes: usize,
        max_chars: usize,
    },
    Screenshot {
        target: Option<ElementRef>,
    },
    Click {
        target: ElementRef,
        double: bool,
    },
    Type {
        target: ElementRef,
        text: String,
        clear: bool,
        submit: bool,
    },
    Select {
        target: ElementRef,
        values: Vec<String>,
    },
    Scroll(ScrollTarget),
    Press(Key),
    Back,
    Forward,
    Reload,
    Wait {
        condition: WaitCondition,
        timeout: Duration,
    },
    RequestUser {
        reason: String,
    },
}

impl BrowserCall {
    /// Parse the call of tool `name` with the model's `arguments`.
    ///
    /// # Errors
    ///
    /// `unknown_tool` for a name not in [`spec::TOOLS`]; `invalid_argument`
    /// for arguments outside the tool's schema; `scheme_refused` for a
    /// `browser_open` URL [`check_url`] refuses.
    pub fn parse(name: &str, arguments: &Value) -> Result<Self, BrowserError> {
        let spec = spec::find(name).ok_or_else(|| {
            BrowserError::new(
                ErrorCode::UnknownTool,
                format!("{name:?} is not a browser tool"),
            )
        })?;
        let empty = Map::new();
        let args = match arguments {
            Value::Object(args) => args,
            Value::Null => &empty,
            _ => return Err(BrowserError::invalid("arguments must be a JSON object")),
        };
        let allowed = spec.properties();
        if let Some(unknown) = args.keys().find(|key| !allowed.contains(key)) {
            return Err(BrowserError::invalid(format!(
                "{name} has no argument {unknown:?}"
            )));
        }
        let args = Args(args);
        Ok(match name {
            spec::BROWSER_OPEN => {
                let url = args.string("url")?.ok_or_else(|| missing("url"))?;
                check_url(&url)?;
                Self::Open { url }
            }
            spec::BROWSER_SNAPSHOT => Self::Snapshot {
                mode: match args.string("mode")?.as_deref() {
                    None | Some("tree") => SnapshotMode::Tree,
                    Some("text") => SnapshotMode::Text,
                    Some(_) => return Err(BrowserError::invalid("mode is tree or text")),
                },
                max_nodes: args.bounded("max_nodes", 1, spec::MAX_NODES, DEFAULT_MAX_NODES)?,
                max_chars: args.bounded("max_chars", 1, spec::MAX_CHARS, DEFAULT_MAX_CHARS)?,
            },
            spec::BROWSER_SCREENSHOT => Self::Screenshot {
                target: args.element("ref")?,
            },
            spec::BROWSER_CLICK => Self::Click {
                target: args.element("ref")?.ok_or_else(|| missing("ref"))?,
                double: args.flag("double")?,
            },
            spec::BROWSER_TYPE => {
                let text = args.string("text")?.ok_or_else(|| missing("text"))?;
                if text.chars().count() as u64 > spec::MAX_TYPE_CHARS {
                    return Err(BrowserError::invalid(format!(
                        "text is longer than {} characters",
                        spec::MAX_TYPE_CHARS
                    )));
                }
                Self::Type {
                    target: args.element("ref")?.ok_or_else(|| missing("ref"))?,
                    text,
                    clear: args.flag("clear")?,
                    submit: args.flag("submit")?,
                }
            }
            spec::BROWSER_SELECT => Self::Select {
                target: args.element("ref")?.ok_or_else(|| missing("ref"))?,
                values: args.strings("values")?,
            },
            spec::BROWSER_SCROLL => Self::Scroll(args.scroll()?),
            spec::BROWSER_PRESS => Self::Press(Key::parse(
                &args.string("key")?.ok_or_else(|| missing("key"))?,
            )?),
            spec::BROWSER_BACK => Self::Back,
            spec::BROWSER_FORWARD => Self::Forward,
            spec::BROWSER_RELOAD => Self::Reload,
            spec::BROWSER_WAIT => args.wait()?,
            spec::BROWSER_REQUEST_USER => {
                let reason = args.nonempty("reason")?.ok_or_else(|| missing("reason"))?;
                if reason.chars().count() > 500 {
                    return Err(BrowserError::invalid(
                        "reason is longer than 500 characters",
                    ));
                }
                Self::RequestUser { reason }
            }
            _ => {
                return Err(BrowserError::new(
                    ErrorCode::UnknownTool,
                    format!("{name:?} is not a browser tool"),
                ));
            }
        })
    }

    /// The tool's name.
    #[must_use]
    pub const fn name(&self) -> &'static str {
        match self {
            Self::Open { .. } => spec::BROWSER_OPEN,
            Self::Snapshot { .. } => spec::BROWSER_SNAPSHOT,
            Self::Screenshot { .. } => spec::BROWSER_SCREENSHOT,
            Self::Click { .. } => spec::BROWSER_CLICK,
            Self::Type { .. } => spec::BROWSER_TYPE,
            Self::Select { .. } => spec::BROWSER_SELECT,
            Self::Scroll(_) => spec::BROWSER_SCROLL,
            Self::Press(_) => spec::BROWSER_PRESS,
            Self::Back => spec::BROWSER_BACK,
            Self::Forward => spec::BROWSER_FORWARD,
            Self::Reload => spec::BROWSER_RELOAD,
            Self::Wait { .. } => spec::BROWSER_WAIT,
            Self::RequestUser { .. } => spec::BROWSER_REQUEST_USER,
        }
    }

    /// The tool's kind.
    #[must_use]
    pub fn kind(&self) -> ToolKind {
        spec::find(self.name()).map_or(ToolKind::BrowserAct, |spec| spec.kind)
    }
}

fn missing(name: &str) -> BrowserError {
    BrowserError::invalid(format!("{name} is required"))
}

struct Args<'a>(&'a Map<String, Value>);

impl Args<'_> {
    fn scroll(&self) -> Result<ScrollTarget, BrowserError> {
        let element = self.element("ref")?;
        let direction = self.string("direction")?;
        let pixels = match self.0.get("pixels") {
            None => None,
            Some(_) => Some(self.bounded("pixels", 1, 10_000, 1)?),
        };
        match (element, direction) {
            (Some(element), None) if pixels.is_none() => Ok(ScrollTarget::Element(element)),
            (None, Some(direction)) => Ok(ScrollTarget::Page {
                direction: match direction.as_str() {
                    "up" => Direction::Up,
                    "down" => Direction::Down,
                    "left" => Direction::Left,
                    "right" => Direction::Right,
                    _ => {
                        return Err(BrowserError::invalid(
                            "direction is up, down, left or right",
                        ));
                    }
                },
                pixels: pixels.and_then(|pixels| u32::try_from(pixels).ok()),
            }),
            _ => Err(BrowserError::invalid(
                "give ref, or direction (with optional pixels), not both",
            )),
        }
    }

    fn wait(&self) -> Result<BrowserCall, BrowserError> {
        let mut conditions = Vec::new();
        if let Some(element) = self.element("ref")? {
            conditions.push(WaitCondition::Element(element));
        }
        if let Some(text) = self.nonempty("text")? {
            conditions.push(WaitCondition::Text(text));
        }
        if let Some(url) = self.nonempty("url")? {
            conditions.push(WaitCondition::Url(url));
        }
        if self.flag("network_idle")? {
            conditions.push(WaitCondition::NetworkIdle);
        }
        if conditions.len() != 1 {
            return Err(BrowserError::invalid(
                "browser_wait takes exactly one of ref, text, url or network_idle",
            ));
        }
        let timeout = self.bounded("timeout_ms", 1, spec::MAX_WAIT_MS, DEFAULT_WAIT_MS)?;
        Ok(BrowserCall::Wait {
            condition: conditions.remove(0),
            timeout: Duration::from_millis(timeout as u64),
        })
    }

    fn string(&self, name: &str) -> Result<Option<String>, BrowserError> {
        match self.0.get(name) {
            None | Some(Value::Null) => Ok(None),
            Some(Value::String(text)) => Ok(Some(text.clone())),
            Some(_) => Err(BrowserError::invalid(format!("{name} must be a string"))),
        }
    }

    fn nonempty(&self, name: &str) -> Result<Option<String>, BrowserError> {
        match self.string(name)? {
            Some(text) if text.trim().is_empty() => {
                Err(BrowserError::invalid(format!("{name} is empty")))
            }
            other => Ok(other),
        }
    }

    fn flag(&self, name: &str) -> Result<bool, BrowserError> {
        match self.0.get(name) {
            None | Some(Value::Null) => Ok(false),
            Some(Value::Bool(flag)) => Ok(*flag),
            Some(_) => Err(BrowserError::invalid(format!("{name} must be a boolean"))),
        }
    }

    fn bounded(&self, name: &str, min: u64, max: u64, default: u64) -> Result<usize, BrowserError> {
        let value = match self.0.get(name) {
            None | Some(Value::Null) => default,
            Some(value) => value
                .as_u64()
                .filter(|value| (min..=max).contains(value))
                .ok_or_else(|| {
                    BrowserError::invalid(format!("{name} must be an integer from {min} to {max}"))
                })?,
        };
        usize::try_from(value).map_err(|_| BrowserError::invalid(format!("{name} is too large")))
    }

    fn element(&self, name: &str) -> Result<Option<ElementRef>, BrowserError> {
        self.string(name)?
            .map(|text| ElementRef::parse(&text))
            .transpose()
    }

    fn strings(&self, name: &str) -> Result<Vec<String>, BrowserError> {
        let invalid = || BrowserError::invalid(format!("{name} must be 1 to 100 strings"));
        let items = self
            .0
            .get(name)
            .and_then(Value::as_array)
            .ok_or_else(invalid)?;
        if items.is_empty() || items.len() > 100 {
            return Err(invalid());
        }
        items
            .iter()
            .map(|item| item.as_str().map(str::to_owned).ok_or_else(invalid))
            .collect()
    }
}

/// The scheme hard limit (ADR-0048 decision 5): only absolute `http` and
/// `https` URLs with a host, and `about:blank`.
///
/// The host is not judged here: the domain policy and the egress allowlist
/// are the driver's (`egress_refused`).
///
/// # Errors
///
/// `scheme_refused` for any other scheme; `invalid_argument` for a URL with
/// whitespace, control characters or no host.
pub fn check_url(url: &str) -> Result<(), BrowserError> {
    if url.is_empty() || url.chars().any(|ch| ch.is_whitespace() || ch.is_control()) {
        return Err(BrowserError::invalid(
            "the URL is empty or holds whitespace or control characters",
        ));
    }
    if url.eq_ignore_ascii_case("about:blank") {
        return Ok(());
    }
    let refused = || {
        BrowserError::new(
            ErrorCode::SchemeRefused,
            "only http and https pages (and about:blank) can be opened",
        )
    };
    let (scheme, rest) = url.split_once(':').ok_or_else(refused)?;
    if !(scheme.eq_ignore_ascii_case("https") || scheme.eq_ignore_ascii_case("http")) {
        return Err(refused());
    }
    let authority = rest
        .strip_prefix("//")
        .ok_or_else(|| BrowserError::invalid("the URL has no host"))?;
    let host = authority
        .split(['/', '?', '#'])
        .next()
        .unwrap_or_default()
        .rsplit('@')
        .next()
        .unwrap_or_default();
    if host.is_empty() || host.starts_with(':') {
        return Err(BrowserError::invalid("the URL has no host"));
    }
    Ok(())
}

/// Whether `url` matches a `browser_wait` URL pattern: `*` matches any run
/// of characters, everything else matches itself, and the whole URL must
/// match.
#[must_use]
pub fn url_matches(pattern: &str, url: &str) -> bool {
    let mut parts = pattern.split('*');
    let first = parts.next().unwrap_or_default();
    let Some(mut rest) = url.strip_prefix(first) else {
        return false;
    };
    let parts: Vec<&str> = parts.collect();
    let Some((last, middle)) = parts.split_last() else {
        return rest.is_empty();
    };
    for part in middle {
        match rest.find(part) {
            Some(at) => rest = &rest[at + part.len()..],
            None => return false,
        }
    }
    rest.len() >= last.len() && rest.ends_with(last)
}

#[cfg(test)]
mod tests {
    use serde_json::json;

    use super::*;

    #[test]
    fn refs() {
        assert_eq!(ElementRef::parse("e3").unwrap(), ElementRef::latest(3));
        let pinned = ElementRef::parse("@s12:e40").unwrap();
        assert_eq!(pinned.snapshot.as_ref().unwrap().as_str(), "s12");
        assert_eq!(pinned.to_string(), "@s12:e40");
        for bad in [
            "",
            "e",
            "e0",
            "e01",
            "3",
            "@:e1",
            "@s1e1",
            "@s 1:e1",
            "e12345678",
            "E1",
        ] {
            assert_eq!(
                ElementRef::parse(bad).unwrap_err().code(),
                ErrorCode::InvalidArgument,
                "{bad}"
            );
        }
    }

    #[test]
    fn schemes() {
        for good in [
            "https://example.test/",
            "HTTP://example.test:8080/a?b#c",
            "about:blank",
            "https://user@host.test",
        ] {
            check_url(good).unwrap();
        }
        for refused in [
            "file:///etc/passwd",
            "javascript:alert(1)",
            "data:text/html,<p>",
            "chrome://settings",
            "about:config",
            "ftp://example.test",
            "example.test",
            "blob:https://example.test/x",
        ] {
            assert_eq!(
                check_url(refused).unwrap_err().code(),
                ErrorCode::SchemeRefused,
                "{refused}"
            );
        }
        for invalid in [
            "https:example.test",
            "https://",
            "https:///path",
            "https://a b",
            "",
        ] {
            assert_eq!(
                check_url(invalid).unwrap_err().code(),
                ErrorCode::InvalidArgument,
                "{invalid}"
            );
        }
    }

    #[test]
    fn keys() {
        assert_eq!(Key::parse("Enter").unwrap().key, "Enter");
        let chord = Key::parse("Shift+Meta+K").unwrap();
        assert_eq!(chord.modifiers, [Modifier::Shift, Modifier::Meta]);
        assert_eq!(Key::parse("Control++").unwrap().key, "+");
        assert_eq!(Key::parse("+").unwrap().key, "+");
        for bad in ["", "Enterr", "Shift+", "Hyper+a", "Shift+Shift+a", "ab"] {
            assert!(Key::parse(bad).is_err(), "{bad}");
        }
    }

    #[test]
    fn calls() {
        let call = BrowserCall::parse(
            "browser_type",
            &json!({"ref": "@s2:e5", "text": "hi", "submit": true}),
        )
        .unwrap();
        assert_eq!(call.kind(), ToolKind::BrowserAct);
        assert!(matches!(
            call,
            BrowserCall::Type {
                submit: true,
                clear: false,
                ..
            }
        ));

        let snapshot = BrowserCall::parse("browser_snapshot", &Value::Null).unwrap();
        assert_eq!(
            snapshot,
            BrowserCall::Snapshot {
                mode: SnapshotMode::Tree,
                max_nodes: 500,
                max_chars: 20_000
            }
        );

        let refused = BrowserCall::parse("browser_open", &json!({"url": "file:///x"}));
        assert_eq!(refused.unwrap_err().code(), ErrorCode::SchemeRefused);
        let unknown = BrowserCall::parse("browser_evaluate", &json!({}));
        assert_eq!(unknown.unwrap_err().code(), ErrorCode::UnknownTool);
        let extra = BrowserCall::parse("browser_back", &json!({"script": "x"}));
        assert_eq!(extra.unwrap_err().code(), ErrorCode::InvalidArgument);
        let two = BrowserCall::parse("browser_wait", &json!({"text": "a", "network_idle": true}));
        assert_eq!(two.unwrap_err().code(), ErrorCode::InvalidArgument);
        let none = BrowserCall::parse("browser_wait", &json!({}));
        assert_eq!(none.unwrap_err().code(), ErrorCode::InvalidArgument);
        let both = BrowserCall::parse("browser_scroll", &json!({"ref": "e1", "direction": "up"}));
        assert_eq!(both.unwrap_err().code(), ErrorCode::InvalidArgument);
        let page = BrowserCall::parse(
            "browser_scroll",
            &json!({"direction": "down", "pixels": 300}),
        )
        .unwrap();
        assert_eq!(
            page,
            BrowserCall::Scroll(ScrollTarget::Page {
                direction: Direction::Down,
                pixels: Some(300)
            })
        );
    }

    #[test]
    fn every_tool_parses_its_minimal_arguments() {
        let minimal = |name: &str| match name {
            "browser_open" => json!({"url": "https://example.test"}),
            "browser_click" | "browser_screenshot" => json!({"ref": "e1"}),
            "browser_type" => json!({"ref": "e1", "text": "x"}),
            "browser_select" => json!({"ref": "e1", "values": ["a"]}),
            "browser_scroll" => json!({"direction": "down"}),
            "browser_press" => json!({"key": "Enter"}),
            "browser_wait" => json!({"network_idle": true}),
            "browser_request_user" => json!({"reason": "sign in"}),
            _ => json!({}),
        };
        for spec in spec::TOOLS {
            let call = BrowserCall::parse(spec.name, &minimal(spec.name)).unwrap();
            assert_eq!(call.name(), spec.name);
            assert_eq!(call.kind(), spec.kind);
        }
    }

    #[test]
    fn url_patterns() {
        assert!(url_matches("https://a.test/*", "https://a.test/x/y"));
        assert!(url_matches("*/done", "https://a.test/done"));
        assert!(url_matches("https://a.test/", "https://a.test/"));
        assert!(!url_matches("https://a.test/", "https://a.test/x"));
        assert!(url_matches("*a*b*", "xxaxxbxx"));
        assert!(!url_matches("*ab*ba", "aba"));
    }
}
