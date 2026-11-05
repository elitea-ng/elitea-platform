//! The snapshot grammar (ADR-0048 decision 3).
//!
//! A driver reads the page into a tree of [`Node`]s (the CDP driver from
//! `Accessibility.getFullAXTree`, the webview driver from the DOM and ARIA
//! in its isolated world) and hands it to [`Snapshot::build`], which is the
//! same code on both hosts:
//!
//! 1. **Normalise.** Names and values have their whitespace collapsed and
//!    are cut ([`MAX_NAME_CHARS`], [`MAX_VALUE_CHARS`]); the page URL is cut
//!    to [`MAX_URL_CHARS`]. A role that is not
//!    a plain token (`[a-z][a-z0-9-]*`) becomes `generic`: roles come from
//!    the page's `role` attribute, and a page must not be able to write
//!    grammar. So that the wrappers and text runs each engine adds do not
//!    show:
//!    * unnamed `generic`, `none` and `presentation` nodes are replaced by
//!      their children, and empty `text` nodes are dropped;
//!    * adjacent `text` runs are joined with one space;
//!    * a `text` child that repeats its parent's name, or the name of the
//!      sibling just before or after it (a label beside its field), is
//!      dropped;
//!    * an unnamed node whose only child is a `text` takes that text as its
//!      name (`- paragraph "Status: idle"`).
//!
//!    States are sorted and deduplicated. A [`State::Secure`] node loses its
//!    value and all its children (so no text inside it can show, or be
//!    hoisted into its name).
//! 2. **Budget.** Nodes are kept in document order up to `max_nodes`; the
//!    rest are counted in [`Snapshot::omitted`].
//! 3. **Refs.** Every kept node with an interactive role
//!    ([`INTERACTIVE_ROLES`]) or marked [`Node::actionable`] gets `eN`,
//!    numbered from 1 in document order. Refs belong to the snapshot's id:
//!    [`Snapshot::resolve`] refuses a ref pinned to another snapshot, and
//!    [`SnapshotSlot`] refuses every ref once the page has changed.
//!
//! The rendered form, inside the untrusted frame ([`crate::frame`]):
//!
//! ```text
//! # snapshot s3
//! # url "https://example.test/sign-in"
//! # title "Sign in"
//! - heading "Sign in" [level=1]
//! - form "Sign in":
//!   - textbox "Email" [required] [ref=e1]: "ada@example.test"
//!   - textbox "Password" [secure] [ref=e2]
//!   - checkbox "Remember me" [checked] [ref=e3]
//!   - button "Sign in" [ref=e4]
//! # omitted 12
//! ```
//!
//! One line per node: `- <role>`, then the name as a JSON string when there
//! is one, then the state flags, then `[ref=eN]`, then `: <value>` (a JSON
//! string) when there is a value, or a bare `:` when the node has children
//! and no value. Children are indented two spaces. Every page-controlled
//! string is a JSON string, so no name or value can start a line of its own.
//! `# omitted N` appears only when the budget cut the tree.

use std::fmt::{self, Write as _};

use crate::call::{ElementRef, SnapshotId};
use crate::error::BrowserError;

/// Longest name kept, in characters (longer ones end in `…`).
pub const MAX_NAME_CHARS: usize = 200;
/// Longest value kept, in characters.
pub const MAX_VALUE_CHARS: usize = 500;
/// Longest page URL kept in a header, in characters (longer ones end in
/// `…`).
pub const MAX_URL_CHARS: usize = 2048;

/// The roles that get a ref.
pub const INTERACTIVE_ROLES: &[&str] = &[
    "button",
    "checkbox",
    "combobox",
    "link",
    "listbox",
    "menuitem",
    "menuitemcheckbox",
    "menuitemradio",
    "option",
    "radio",
    "searchbox",
    "slider",
    "spinbutton",
    "switch",
    "tab",
    "textbox",
    "treeitem",
];

/// Roles that are replaced by their children when they have no name.
const TRANSPARENT_ROLES: &[&str] = &["generic", "none", "presentation"];

/// A state flag of a node, in the order a line shows them.
#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd)]
pub enum State {
    /// `[level=N]`: a heading's level (1 to 6), a tree item's depth.
    Level(u8),
    Checked,
    /// `[checked=mixed]`.
    CheckedMixed,
    Pressed,
    /// `[pressed=mixed]`.
    PressedMixed,
    Selected,
    Expanded,
    Collapsed,
    Disabled,
    Readonly,
    Required,
    Invalid,
    Focused,
    /// A password or payment card field ([`is_secure_field`]): shown with
    /// no value, and never typed into.
    Secure,
}

impl State {
    fn parse(token: &str) -> Option<Self> {
        Some(match token {
            "checked" => Self::Checked,
            "checked=mixed" => Self::CheckedMixed,
            "pressed" => Self::Pressed,
            "pressed=mixed" => Self::PressedMixed,
            "selected" => Self::Selected,
            "expanded" => Self::Expanded,
            "collapsed" => Self::Collapsed,
            "disabled" => Self::Disabled,
            "readonly" => Self::Readonly,
            "required" => Self::Required,
            "invalid" => Self::Invalid,
            "focused" => Self::Focused,
            "secure" => Self::Secure,
            _ => {
                let level = token.strip_prefix("level=")?.parse().ok()?;
                Self::Level(level)
            }
        })
    }
}

impl fmt::Display for State {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self {
            Self::Level(level) => return write!(formatter, "level={level}"),
            Self::Checked => "checked",
            Self::CheckedMixed => "checked=mixed",
            Self::Pressed => "pressed",
            Self::PressedMixed => "pressed=mixed",
            Self::Selected => "selected",
            Self::Expanded => "expanded",
            Self::Collapsed => "collapsed",
            Self::Disabled => "disabled",
            Self::Readonly => "readonly",
            Self::Required => "required",
            Self::Invalid => "invalid",
            Self::Focused => "focused",
            Self::Secure => "secure",
        })
    }
}

/// Whether a form field is secure (ADR-0048 decision 5): an
/// `input[type=password]`, or a field whose `autocomplete` names a password
/// or a payment card number or security code. Both drivers decide with
/// this, from the element's `type` and `autocomplete` attributes.
#[must_use]
pub fn is_secure_field(input_type: Option<&str>, autocomplete: Option<&str>) -> bool {
    let password = input_type.is_some_and(|kind| kind.trim().eq_ignore_ascii_case("password"));
    let secure_token = autocomplete.is_some_and(|tokens| {
        tokens.split_ascii_whitespace().any(|token| {
            ["current-password", "new-password", "cc-number", "cc-csc"]
                .iter()
                .any(|secure| token.eq_ignore_ascii_case(secure))
        })
    });
    password || secure_token
}

/// One node of the page as a driver reads it.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Node {
    pub role: String,
    pub name: String,
    pub value: Option<String>,
    pub states: Vec<State>,
    /// A ref even though the role is not interactive (an element with a
    /// click handler or a `tabindex`).
    pub actionable: bool,
    /// The driver's own handle for the element (a CDP backend node id, the
    /// isolated world's element number); never rendered.
    pub key: u64,
    /// `N` of the node's `eN`: set by [`Snapshot::build`], left `None` by a
    /// driver.
    pub ref_index: Option<u32>,
    pub children: Vec<Node>,
}

impl Node {
    #[must_use]
    pub fn new(role: &str, name: &str) -> Self {
        Self {
            role: role.to_owned(),
            name: name.to_owned(),
            ..Self::default()
        }
    }

    #[must_use]
    pub fn key(mut self, key: u64) -> Self {
        self.key = key;
        self
    }

    #[must_use]
    pub fn value(mut self, value: &str) -> Self {
        self.value = Some(value.to_owned());
        self
    }

    #[must_use]
    pub fn state(mut self, state: State) -> Self {
        self.states.push(state);
        self
    }

    #[must_use]
    pub fn actionable(mut self) -> Self {
        self.actionable = true;
        self
    }

    #[must_use]
    pub fn children(mut self, children: Vec<Self>) -> Self {
        self.children = children;
        self
    }

    /// Whether this node is a password or payment card field.
    #[must_use]
    pub fn is_secure(&self) -> bool {
        self.states.contains(&State::Secure)
    }

    fn wants_ref(&self) -> bool {
        self.actionable || INTERACTIVE_ROLES.contains(&self.role.as_str())
    }
}

/// One snapshot of a page: its id, URL, title and node tree with refs.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Snapshot {
    pub id: SnapshotId,
    pub url: String,
    pub title: String,
    pub nodes: Vec<Node>,
    /// Nodes the budget left out.
    pub omitted: usize,
}

impl Snapshot {
    /// Normalise `roots`, cut them to `max_nodes` and give out refs (see the
    /// module docs).
    #[must_use]
    pub fn build(
        id: SnapshotId,
        url: &str,
        title: &str,
        roots: Vec<Node>,
        max_nodes: usize,
    ) -> Self {
        let normalised = normalise_all(roots);
        let total = count(&normalised);
        let mut budget = Budget {
            left: max_nodes,
            next_ref: 1,
        };
        let nodes = budget.take(normalised);
        Self {
            id,
            url: cut_url(url),
            title: collapse(title, MAX_NAME_CHARS),
            nodes,
            omitted: total.saturating_sub(max_nodes),
        }
    }

    /// The whole snapshot as the model reads it (header and tree).
    #[must_use]
    pub fn render(&self) -> String {
        let mut out = String::new();
        let _ = writeln!(out, "# snapshot {}", self.id);
        let _ = writeln!(out, "# url {}", quote(&self.url));
        let _ = writeln!(out, "# title {}", quote(&self.title));
        out.push_str(&self.render_tree());
        if self.omitted > 0 {
            let _ = writeln!(out, "# omitted {}", self.omitted);
        }
        out
    }

    /// The node lines only: what a golden file holds.
    #[must_use]
    pub fn render_tree(&self) -> String {
        let mut out = String::new();
        for node in &self.nodes {
            render_node(&mut out, node, 0);
        }
        out
    }

    /// Read a rendered snapshot back (keys are 0).
    ///
    /// # Errors
    ///
    /// A [`ParseError`] naming the first line that is not the grammar.
    pub fn parse(text: &str) -> Result<Self, ParseError> {
        let mut id = None;
        let mut url = String::new();
        let mut title = String::new();
        let mut omitted = 0;
        let mut stack: Vec<(usize, Node)> = Vec::new();
        let mut roots = Vec::new();
        for (number, line) in text.lines().enumerate() {
            let fail = |reason: &str| ParseError {
                line: number + 1,
                reason: reason.to_owned(),
            };
            if line.trim().is_empty() {
                continue;
            }
            if let Some(header) = line.strip_prefix("# ") {
                let (field, rest) = header.split_once(' ').ok_or_else(|| fail("header"))?;
                match field {
                    "snapshot" => id = Some(SnapshotId::new(rest).ok_or_else(|| fail("id"))?),
                    "url" => url = unquote_all(rest).ok_or_else(|| fail("url"))?,
                    "title" => title = unquote_all(rest).ok_or_else(|| fail("title"))?,
                    "omitted" => omitted = rest.parse().map_err(|_| fail("omitted"))?,
                    _ => return Err(fail("unknown header")),
                }
                continue;
            }
            let depth_spaces = line.len() - line.trim_start_matches(' ').len();
            if depth_spaces % 2 != 0 {
                return Err(fail("indent is not a multiple of two"));
            }
            let depth = depth_spaces / 2;
            let node = parse_line(&line[depth_spaces..]).map_err(|reason| fail(&reason))?;
            while stack.last().is_some_and(|(at, _)| *at >= depth) {
                close(&mut stack, &mut roots);
            }
            if depth != stack.len() {
                return Err(fail("a child deeper than one level below its parent"));
            }
            stack.push((depth, node));
        }
        while !stack.is_empty() {
            close(&mut stack, &mut roots);
        }
        Ok(Self {
            id: id.ok_or(ParseError {
                line: 0,
                reason: "no snapshot id".to_owned(),
            })?,
            url,
            title,
            nodes: roots,
            omitted,
        })
    }

    /// The tree of a golden file (no header), under `id`.
    ///
    /// # Errors
    ///
    /// As [`Self::parse`].
    pub fn parse_tree(id: &SnapshotId, tree: &str) -> Result<Self, ParseError> {
        Self::parse(&format!("# snapshot {id}\n{tree}"))
    }

    /// How many refs this snapshot gave out.
    #[must_use]
    pub fn ref_count(&self) -> usize {
        let mut refs = 0;
        walk(&self.nodes, &mut |node| {
            if node.ref_index.is_some() {
                refs += 1;
            }
        });
        refs
    }

    /// The node `target` names.
    ///
    /// # Errors
    ///
    /// `stale_ref` when the ref is pinned to another snapshot or this
    /// snapshot gave no such ref.
    pub fn resolve(&self, target: &ElementRef) -> Result<&Node, BrowserError> {
        if let Some(pinned) = &target.snapshot
            && *pinned != self.id
        {
            return Err(BrowserError::stale_ref(format!(
                "{target} belongs to snapshot {pinned}; the latest is {}: snapshot again",
                self.id
            )));
        }
        find_ref(&self.nodes, target.index).ok_or_else(|| {
            BrowserError::stale_ref(format!(
                "snapshot {} has no {target}: snapshot again",
                self.id
            ))
        })
    }

    /// The first node in document order with this role and name.
    #[must_use]
    pub fn find(&self, role: &str, name: &str) -> Option<&Node> {
        find_node(&self.nodes, &|node| node.role == role && node.name == name)
    }
}

/// A line of a rendered snapshot that is not the grammar.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ParseError {
    /// 1-based; 0 for the snapshot as a whole.
    pub line: usize,
    pub reason: String,
}

impl fmt::Display for ParseError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "line {}: {}", self.line, self.reason)
    }
}

impl std::error::Error for ParseError {}

/// A pane's latest snapshot, the one its refs resolve against.
///
/// A driver keeps one per page, gives out ids with [`Self::next_id`],
/// [`Self::install`]s each snapshot it returns, and calls
/// [`Self::invalidate`] whenever the page changes under it (a navigation, a
/// reload, a DOM mutation it observes, the person's input), so a ref taken
/// before the change fails with `stale_ref`.
#[derive(Debug, Default)]
pub struct SnapshotSlot {
    issued: u64,
    current: Option<Snapshot>,
}

impl SnapshotSlot {
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// The id for the next snapshot: `s1`, `s2`, …
    pub fn next_id(&mut self) -> SnapshotId {
        self.issued += 1;
        SnapshotId::sequence(self.issued)
    }

    pub fn install(&mut self, snapshot: Snapshot) {
        self.current = Some(snapshot);
    }

    /// The page changed: every ref given out so far is stale.
    pub fn invalidate(&mut self) {
        self.current = None;
    }

    #[must_use]
    pub fn current(&self) -> Option<&Snapshot> {
        self.current.as_ref()
    }

    /// The node `target` names in the latest snapshot.
    ///
    /// # Errors
    ///
    /// `stale_ref` when there is no current snapshot or it has no such ref.
    pub fn resolve(&self, target: &ElementRef) -> Result<&Node, BrowserError> {
        self.current
            .as_ref()
            .ok_or_else(|| {
                BrowserError::stale_ref(format!(
                    "{target} is from before the page changed: snapshot again"
                ))
            })?
            .resolve(target)
    }

    /// As [`Self::resolve`], for a call that types or presses keys into the
    /// node.
    ///
    /// # Errors
    ///
    /// As [`Self::resolve`], and `secure_field_refused` for a secure node.
    pub fn resolve_for_input(&self, target: &ElementRef) -> Result<&Node, BrowserError> {
        let node = self.resolve(target)?;
        if node.is_secure() {
            return Err(BrowserError::secure_field());
        }
        Ok(node)
    }
}

struct Budget {
    left: usize,
    next_ref: u32,
}

impl Budget {
    fn take(&mut self, nodes: Vec<Node>) -> Vec<Node> {
        let mut kept = Vec::new();
        for mut node in nodes {
            if self.left == 0 {
                break;
            }
            self.left -= 1;
            if node.wants_ref() {
                node.ref_index = Some(self.next_ref);
                self.next_ref += 1;
            }
            let children = std::mem::take(&mut node.children);
            node.children = self.take(children);
            kept.push(node);
        }
        kept
    }
}

fn normalise_all(nodes: Vec<Node>) -> Vec<Node> {
    let mut flat = Vec::new();
    for node in nodes {
        normalise(node, &mut flat);
    }
    // Adjacent text runs (split by inline elements) are one text.
    let mut out: Vec<Node> = Vec::with_capacity(flat.len());
    for node in flat {
        match out.last_mut() {
            Some(last) if is_plain_text(last) && is_plain_text(&node) => {
                last.name = collapse(&format!("{} {}", last.name, node.name), MAX_NAME_CHARS);
            }
            _ => out.push(node),
        }
    }
    out
}

fn is_plain_text(node: &Node) -> bool {
    node.role == "text" && node.children.is_empty() && !node.actionable
}

fn normalise(mut node: Node, out: &mut Vec<Node>) {
    node.role = node.role.trim().to_ascii_lowercase();
    if !is_role_token(&node.role) {
        "generic".clone_into(&mut node.role);
    }
    node.name = collapse(&node.name, MAX_NAME_CHARS);
    node.states.sort_unstable();
    node.states.dedup();
    node.ref_index = None;
    node.value = if node.is_secure() {
        None
    } else {
        node.value
            .take()
            .map(|value| collapse(&value, MAX_VALUE_CHARS))
            .filter(|value| !value.is_empty())
    };
    // A secure node keeps no children either: an engine may expose the
    // field's content as a text child, and the hoist below would make it
    // the name.
    let secure = node.is_secure();
    let raw_children = std::mem::take(&mut node.children);
    let mut children = if secure {
        Vec::new()
    } else {
        prune_text(&node.name, normalise_all(raw_children))
    };
    let hoists = !secure
        && node.name.is_empty()
        && (node.actionable || !TRANSPARENT_ROLES.contains(&node.role.as_str()))
        && node.role != "text"
        && matches!(children.as_slice(), [only] if only.role == "text" && only.children.is_empty());
    if hoists && let Some(text) = children.pop() {
        node.name = text.name;
    }
    let transparent = node.name.is_empty()
        && node.value.is_none()
        && !node.actionable
        && node.states.is_empty()
        && TRANSPARENT_ROLES.contains(&node.role.as_str());
    if transparent {
        out.extend(children);
    } else if node.role == "text" && node.name.is_empty() {
        // An empty text node says nothing.
    } else {
        node.children = children;
        out.push(node);
    }
}

/// `children` without the text nodes that only repeat a name: the parent's
/// own (a heading's or a button's text), or that of the sibling just before
/// or after (a label's text beside the field it names).
fn prune_text(parent_name: &str, children: Vec<Node>) -> Vec<Node> {
    let repeats = |at: usize| {
        let node = &children[at];
        if node.role != "text" || !node.children.is_empty() {
            return false;
        }
        let names_sibling = |sibling: Option<&Node>| {
            sibling.is_some_and(|sibling| sibling.role != "text" && sibling.name == node.name)
        };
        node.name == parent_name
            || names_sibling(at.checked_sub(1).and_then(|before| children.get(before)))
            || names_sibling(children.get(at + 1))
    };
    let keep: Vec<bool> = (0..children.len()).map(|at| !repeats(at)).collect();
    children
        .into_iter()
        .zip(keep)
        .filter_map(|(node, keep)| keep.then_some(node))
        .collect()
}

fn is_role_token(role: &str) -> bool {
    let mut bytes = role.bytes();
    bytes.next().is_some_and(|first| first.is_ascii_lowercase())
        && bytes.all(|byte| byte.is_ascii_lowercase() || byte.is_ascii_digit() || byte == b'-')
        && role.len() <= 32
}

/// Whitespace runs as one space, trimmed, cut to `max` characters.
pub(crate) fn collapse(text: &str, max: usize) -> String {
    let collapsed = text.split_whitespace().collect::<Vec<_>>().join(" ");
    if collapsed.chars().count() <= max {
        return collapsed;
    }
    let mut cut: String = collapsed.chars().take(max - 1).collect();
    cut.push('…');
    cut
}

/// A page URL cut to [`MAX_URL_CHARS`], ending in `…` when cut. The page
/// controls its URL (`history.pushState`), so it is bounded like a name.
pub(crate) fn cut_url(url: &str) -> String {
    match url.char_indices().nth(MAX_URL_CHARS - 1) {
        Some((at, _)) if url[at..].chars().nth(1).is_some() => {
            let mut cut = url[..at].to_owned();
            cut.push('…');
            cut
        }
        _ => url.to_owned(),
    }
}

fn count(nodes: &[Node]) -> usize {
    nodes.iter().map(|node| 1 + count(&node.children)).sum()
}

fn walk(nodes: &[Node], visit: &mut impl FnMut(&Node)) {
    for node in nodes {
        visit(node);
        walk(&node.children, visit);
    }
}

fn find_ref(nodes: &[Node], index: u32) -> Option<&Node> {
    find_node(nodes, &|node| node.ref_index == Some(index))
}

fn find_node<'a>(nodes: &'a [Node], matches: &dyn Fn(&Node) -> bool) -> Option<&'a Node> {
    for node in nodes {
        if matches(node) {
            return Some(node);
        }
        if let Some(found) = find_node(&node.children, matches) {
            return Some(found);
        }
    }
    None
}

fn quote(text: &str) -> String {
    serde_json::Value::String(text.to_owned()).to_string()
}

fn render_node(out: &mut String, node: &Node, depth: usize) {
    for _ in 0..depth {
        out.push_str("  ");
    }
    out.push_str("- ");
    out.push_str(&node.role);
    if !node.name.is_empty() {
        out.push(' ');
        out.push_str(&quote(&node.name));
    }
    for state in &node.states {
        let _ = write!(out, " [{state}]");
    }
    if let Some(index) = node.ref_index {
        let _ = write!(out, " [ref=e{index}]");
    }
    match &node.value {
        Some(value) => {
            out.push_str(": ");
            out.push_str(&quote(value));
        }
        None if !node.children.is_empty() => out.push(':'),
        None => {}
    }
    out.push('\n');
    for child in &node.children {
        render_node(out, child, depth + 1);
    }
}

fn close(stack: &mut Vec<(usize, Node)>, roots: &mut Vec<Node>) {
    if let Some((_, node)) = stack.pop() {
        match stack.last_mut() {
            Some((_, parent)) => parent.children.push(node),
            None => roots.push(node),
        }
    }
}

/// The JSON string at the start of `text` and what follows it.
fn take_quoted(text: &str) -> Option<(String, &str)> {
    if !text.starts_with('"') {
        return None;
    }
    let mut escaped = false;
    for (at, ch) in text.char_indices().skip(1) {
        match ch {
            _ if escaped => escaped = false,
            '\\' => escaped = true,
            '"' => {
                let value = serde_json::from_str(&text[..=at]).ok()?;
                return Some((value, &text[at + 1..]));
            }
            _ => {}
        }
    }
    None
}

fn unquote_all(text: &str) -> Option<String> {
    match take_quoted(text)? {
        (value, "") => Some(value),
        _ => None,
    }
}

fn parse_line(line: &str) -> Result<Node, String> {
    let rest = line
        .strip_prefix("- ")
        .ok_or("a node line starts with \"- \"")?;
    let role_end = rest.find([' ', ':']).unwrap_or(rest.len());
    let mut node = Node::new(&rest[..role_end], "");
    if !is_role_token(&node.role) {
        return Err(format!("{:?} is not a role", node.role));
    }
    let mut rest = &rest[role_end..];
    if let Some(after) = rest.strip_prefix(' ')
        && after.starts_with('"')
    {
        let (name, after) = take_quoted(after).ok_or("an unterminated name")?;
        node.name = name;
        rest = after;
    }
    while let Some(after) = rest.strip_prefix(" [") {
        let (token, after) = after.split_once(']').ok_or("an unterminated flag")?;
        if let Some(index) = token.strip_prefix("ref=e") {
            node.ref_index = Some(index.parse().map_err(|_| format!("ref {token:?}"))?);
        } else {
            node.states
                .push(State::parse(token).ok_or_else(|| format!("unknown flag {token:?}"))?);
        }
        rest = after;
    }
    if let Some(value) = rest.strip_prefix(": ") {
        node.value = Some(unquote_all(value).ok_or("a value is one JSON string")?);
    } else if !(rest.is_empty() || rest == ":") {
        return Err(format!("unexpected {rest:?}"));
    }
    Ok(node)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn id() -> SnapshotId {
        SnapshotId::sequence(1)
    }

    #[test]
    fn normalises_and_numbers_refs() {
        let roots = vec![
            Node::new("generic", "").children(vec![
                Node::new("heading", "  Sign\n in ").state(State::Level(1)),
                Node::new("text", ""),
                Node::new("textbox", "Email")
                    .value("ada")
                    .state(State::Required)
                    .state(State::Required),
                Node::new("textbox", "Password")
                    .value("hunter2")
                    .state(State::Secure),
                Node::new("generic", "Card").actionable(),
            ]),
            Node::new("button\" [ref=e9]", "x"),
        ];
        let snapshot = Snapshot::build(id(), "https://a.test/", "T", roots, 100);
        assert_eq!(
            snapshot.render_tree(),
            "- heading \"Sign in\" [level=1]\n\
             - textbox \"Email\" [required] [ref=e1]: \"ada\"\n\
             - textbox \"Password\" [secure] [ref=e2]\n\
             - generic \"Card\" [ref=e3]\n\
             - generic \"x\"\n"
        );
        assert!(!snapshot.render().contains("hunter2"));
        assert_eq!(snapshot.ref_count(), 3);
    }

    #[test]
    fn budget_counts_what_it_leaves_out() {
        let roots = (0..10)
            .map(|n| Node::new("button", &n.to_string()))
            .collect();
        let snapshot = Snapshot::build(id(), "u", "t", roots, 4);
        assert_eq!(snapshot.ref_count(), 4);
        assert_eq!(snapshot.omitted, 6);
        assert!(snapshot.render().ends_with("# omitted 6\n"));
    }

    #[test]
    fn round_trips() {
        let roots = vec![Node::new("form", "F").children(vec![
            Node::new("textbox", "a \"quoted\" name").value("line\\two"),
            Node::new("list", "").children(vec![Node::new("listitem", "x")]),
        ])];
        let snapshot = Snapshot::build(id(), "https://a.test/?q=\"", "t", roots, 100);
        let parsed = Snapshot::parse(&snapshot.render()).unwrap();
        assert_eq!(parsed, snapshot);
    }

    #[test]
    fn refs_are_bound_to_their_snapshot() {
        let mut slot = SnapshotSlot::new();
        let first = slot.next_id();
        let tree = vec![
            Node::new("button", "Go"),
            Node::new("textbox", "PIN").state(State::Secure),
        ];
        slot.install(Snapshot::build(first.clone(), "u", "t", tree.clone(), 10));
        assert_eq!(slot.resolve(&ElementRef::latest(1)).unwrap().name, "Go");
        assert_eq!(
            slot.resolve_for_input(&ElementRef::latest(2))
                .unwrap_err()
                .code(),
            crate::ErrorCode::SecureFieldRefused
        );
        let second = slot.next_id();
        slot.install(Snapshot::build(second, "u", "t", tree, 10));
        let old = ElementRef::pinned(first, 1);
        assert_eq!(
            slot.resolve(&old).unwrap_err().code(),
            crate::ErrorCode::StaleRef
        );
        assert_eq!(
            slot.resolve(&ElementRef::latest(3)).unwrap_err().code(),
            crate::ErrorCode::StaleRef
        );
        slot.invalidate();
        assert_eq!(
            slot.resolve(&ElementRef::latest(1)).unwrap_err().code(),
            crate::ErrorCode::StaleRef
        );
    }

    #[test]
    fn secure_fields() {
        assert!(is_secure_field(Some("PASSWORD"), None));
        assert!(is_secure_field(Some("text"), Some("billing cc-number")));
        assert!(is_secure_field(None, Some("new-password")));
        assert!(is_secure_field(Some("tel"), Some("cc-csc")));
        assert!(!is_secure_field(Some("text"), Some("email")));
        assert!(!is_secure_field(Some("text"), Some("cc-name")));
    }

    #[test]
    fn secure_nodes_keep_no_children() {
        let roots = vec![
            Node::new("textbox", "")
                .state(State::Secure)
                .children(vec![Node::new("text", "hunter2")]),
            Node::new("textbox", "PIN")
                .state(State::Secure)
                .children(vec![
                    Node::new("text", "4242"),
                    Node::new("generic", "").children(vec![Node::new("text", "s3cret")]),
                ]),
        ];
        let snapshot = Snapshot::build(id(), "u", "t", roots, 100);
        let rendered = snapshot.render();
        assert!(!rendered.contains("hunter2"), "{rendered}");
        assert!(!rendered.contains("4242"), "{rendered}");
        assert!(!rendered.contains("s3cret"), "{rendered}");
        assert_eq!(
            snapshot.render_tree(),
            "- textbox [secure] [ref=e1]\n\
             - textbox \"PIN\" [secure] [ref=e2]\n"
        );
    }

    #[test]
    fn urls_are_cut() {
        let short = "https://a.test/".to_owned();
        assert_eq!(cut_url(&short), short);
        let exact = "x".repeat(MAX_URL_CHARS);
        assert_eq!(cut_url(&exact), exact);
        let long = format!("https://a.test/{}", "é".repeat(MAX_URL_CHARS));
        let cut = cut_url(&long);
        assert_eq!(cut.chars().count(), MAX_URL_CHARS);
        assert!(cut.ends_with('…'));
        assert!(long.starts_with(cut.trim_end_matches('…')));
        let snapshot = Snapshot::build(id(), &long, "t", Vec::new(), 10);
        assert_eq!(snapshot.url, cut);
    }

    #[test]
    fn parse_refuses_bad_lines() {
        for bad in [
            "- Button \"x\"",
            "- button \"x",
            "- button [wobbly]",
            "- button \"x\" trailing",
            "-button",
            "   - button",
        ] {
            assert!(
                Snapshot::parse_tree(&id(), bad).is_err(),
                "{bad} should not parse"
            );
        }
        assert!(Snapshot::parse_tree(&id(), "- list:\n    - listitem").is_err());
    }
}
