//! A Figma node tree reduced to the text a search can use: the SDK's
//! `extract_text_by_role` (`tools/figma/toon_tools.py`), without the
//! model-written parts.
//!
//! Every `TEXT` node under a frame is classified by its role from its font
//! size and weight, its fill and the names on its path: a heading, a button
//! label, a form label, an error, a placeholder or body text.

use serde_json::Value;

/// Safety bound on how deep either walk descends. The SDK stops at depth 10,
/// which dropped real text of deeply nested designs; this only guards
/// against a pathological tree.
pub(crate) const MAX_TREE_DEPTH: usize = 256;
/// How many nodes either text walk visits in one frame, whatever the byte
/// cap on a response is.
pub(crate) const MAX_FRAME_NODES: usize = 200_000;
const BUTTON_NAMES: [&str; 7] = ["button", "btn", "cta", "action", "link", "tab", "chip"];
const INPUT_NAMES: [&str; 5] = ["input", "textfield", "textarea", "dropdown", "select"];
const HEADING_NAMES: [&str; 6] = ["heading", "title", "header", "h1", "h2", "h3"];
const FORM_NAMES: [&str; 3] = ["form", "input", "field"];

/// A frame's text by role, in document order, duplicates (ignoring case)
/// removed.
#[derive(Debug, Default, PartialEq, Eq)]
pub(crate) struct Roles {
    pub(crate) headings: Vec<String>,
    pub(crate) buttons: Vec<String>,
    pub(crate) labels: Vec<String>,
    pub(crate) body: Vec<String>,
    pub(crate) errors: Vec<String>,
    pub(crate) placeholders: Vec<String>,
}

impl Roles {
    pub(crate) fn is_empty(&self) -> bool {
        self.headings.is_empty()
            && self.buttons.is_empty()
            && self.labels.is_empty()
            && self.body.is_empty()
            && self.errors.is_empty()
            && self.placeholders.is_empty()
    }
}

#[derive(Clone, Copy, PartialEq, Eq)]
enum Mode {
    Normal,
    /// Inside a button-like node: all text is a button label.
    Button,
    /// Inside an input-like node: its text is a field label or a hint.
    Input,
}

#[derive(Clone, Copy)]
enum Role {
    Error,
    Placeholder,
    Heading,
    Label,
    Body,
}

/// The text of `node` and everything below it, by role. At most `max_nodes`
/// nodes are visited, to a depth of [`MAX_TREE_DEPTH`]. `node` is the frame
/// being indexed: its own name never switches the mode, only the names of
/// containers nested in it do.
pub(crate) fn extract(node: &Value, max_nodes: usize) -> Roles {
    let mut roles = Roles::default();
    walk(node, max_nodes, &mut roles);
    for list in [
        &mut roles.headings,
        &mut roles.buttons,
        &mut roles.labels,
        &mut roles.body,
        &mut roles.errors,
        &mut roles.placeholders,
    ] {
        let mut seen = std::collections::HashSet::new();
        list.retain(|text| seen.insert(text.to_lowercase()));
    }
    roles
}

/// The `characters` of every `TEXT` node under `node`, in order, within the
/// same depth and node bounds as [`extract`].
pub(crate) fn texts(node: &Value, max_nodes: usize, out: &mut Vec<String>) {
    let mut stack = vec![(node, 0_usize)];
    let mut visited = 0_usize;
    while let Some((node, depth)) = stack.pop() {
        if hidden(node) {
            continue;
        }
        visited += 1;
        if visited > max_nodes {
            return;
        }
        if node_type(node) == "TEXT"
            && let Some(text) = node.get("characters").and_then(Value::as_str)
        {
            out.push(text.to_owned());
        }
        if depth < MAX_TREE_DEPTH {
            stack.extend(children(node).iter().rev().map(|child| (child, depth + 1)));
        }
    }
}

/// A node Figma marks hidden (`visible: false`); it and everything below it
/// is not on the canvas.
pub(crate) fn hidden(node: &Value) -> bool {
    node.get("visible").and_then(Value::as_bool) == Some(false)
}

pub(crate) fn node_type(node: &Value) -> String {
    node.get("type")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .to_ascii_uppercase()
}

pub(crate) fn children(node: &Value) -> &[Value] {
    node.get("children")
        .and_then(Value::as_array)
        .map_or(&[], Vec::as_slice)
}

/// The words of a layer name: split on anything that is not a letter or
/// digit and at camelCase boundaries, lower-cased, plus each pair of
/// neighbouring words joined (`Text Field` and `TextField` both give
/// `textfield`). "Tablet" is not "tab" and "Selection" is not "select".
fn name_words(name: &str) -> Vec<String> {
    let mut words: Vec<String> = Vec::new();
    let mut current = String::new();
    let mut previous_lower = false;
    for character in name.chars() {
        if !character.is_alphanumeric() {
            if !current.is_empty() {
                words.push(std::mem::take(&mut current));
            }
            previous_lower = false;
            continue;
        }
        if character.is_uppercase() && previous_lower && !current.is_empty() {
            words.push(std::mem::take(&mut current));
        }
        previous_lower = character.is_lowercase();
        current.extend(character.to_lowercase());
    }
    if !current.is_empty() {
        words.push(current);
    }
    let pairs: Vec<String> = words
        .windows(2)
        .map(|pair| format!("{}{}", pair[0], pair[1]))
        .collect();
    words.extend(pairs);
    words
}

fn has_any(name: &str, words: &[&str]) -> bool {
    let found = name_words(name);
    words
        .iter()
        .any(|word| found.iter().any(|candidate| candidate == word))
}

struct Frame<'a> {
    node: &'a Value,
    depth: usize,
    mode: Mode,
    in_error: bool,
    in_form: bool,
}

fn walk(root: &Value, max_nodes: usize, roles: &mut Roles) {
    let mut stack = vec![Frame {
        node: root,
        depth: 0,
        mode: Mode::Normal,
        in_error: false,
        in_form: false,
    }];
    let mut visited = 0_usize;
    while let Some(Frame {
        node,
        depth,
        mode,
        in_error,
        in_form,
    }) = stack.pop()
    {
        if hidden(node) {
            continue;
        }
        visited += 1;
        if visited > max_nodes {
            return;
        }
        let kind = node_type(node);
        let raw_name = node.get("name").and_then(Value::as_str).unwrap_or_default();
        let name = raw_name.to_lowercase();
        // The frame being indexed (depth 0) is named for what it is
        // ("Checkout form", "Error handling"): its own name classifies
        // nothing, only the names of what is nested in it do.
        let in_error = in_error || (depth > 0 && name.contains("error"));
        let in_form = in_form || (depth > 0 && has_any(raw_name, &FORM_NAMES));
        if kind == "TEXT"
            && let Some(text) = text_of(node)
        {
            let role = classify(node, &name, raw_name, in_error, in_form);
            place(roles, mode, role, text);
        }
        if depth >= MAX_TREE_DEPTH {
            continue;
        }
        // The frame being indexed (depth 0) never sets the mode: only the
        // containers nested in it do.
        let container = depth > 0 && matches!(kind.as_str(), "INSTANCE" | "COMPONENT" | "FRAME");
        let child_mode = match mode {
            Mode::Normal if container && has_any(raw_name, &BUTTON_NAMES) => Mode::Button,
            Mode::Normal if container && has_any(raw_name, &INPUT_NAMES) => Mode::Input,
            other => other,
        };
        stack.extend(children(node).iter().rev().map(|child| Frame {
            node: child,
            depth: depth + 1,
            mode: child_mode,
            in_error,
            in_form,
        }));
    }
}

/// The trimmed `characters`, unless there are none, one character that is
/// no punctuation worth keeping (icons, separators), or template filler.
fn text_of(node: &Value) -> Option<String> {
    let text = node.get("characters").and_then(Value::as_str)?.trim();
    if text.is_empty() {
        return None;
    }
    if text.chars().count() <= 1 && !matches!(text, "?" | "!") {
        return None;
    }
    if text.to_lowercase().contains("lorem ipsum") {
        return None;
    }
    Some(text.to_owned())
}

fn number(value: Option<&Value>, default: f64) -> f64 {
    value.and_then(Value::as_f64).unwrap_or(default)
}

fn classify(node: &Value, name: &str, raw_name: &str, in_error: bool, in_form: bool) -> Role {
    let style = node.get("style");
    let font_size = number(style.and_then(|style| style.get("fontSize")), 14.0);
    let font_weight = number(style.and_then(|style| style.get("fontWeight")), 400.0);
    let solid_colors: Vec<(f64, f64, f64)> = node
        .get("fills")
        .and_then(Value::as_array)
        .map(|fills| {
            fills
                .iter()
                .filter(|fill| fill.get("type").and_then(Value::as_str) == Some("SOLID"))
                .map(|fill| {
                    let color = fill.get("color");
                    let channel = |name: &str| number(color.and_then(|color| color.get(name)), 0.0);
                    (channel("r"), channel("g"), channel("b"))
                })
                .collect()
        })
        .unwrap_or_default();
    let is_red = solid_colors
        .iter()
        .any(|(r, g, b)| *r > 0.7 && *g < 0.3 && *b < 0.3);
    let is_gray = solid_colors
        .iter()
        .any(|(r, g, _)| (r - g).abs() < 0.1 && *r > 0.4 && *r < 0.7);
    // Priority: error > placeholder > heading > label > body.
    if is_red || in_error {
        Role::Error
    } else if is_gray || name.contains("placeholder") || name.contains("hint") {
        Role::Placeholder
    } else if font_size >= 18.0 || font_weight >= 600.0 || has_any(raw_name, &HEADING_NAMES) {
        Role::Heading
    } else if font_size <= 12.0 || name.contains("label") || name.contains("caption") || in_form {
        Role::Label
    } else {
        Role::Body
    }
}

fn place(roles: &mut Roles, mode: Mode, role: Role, text: String) {
    match mode {
        Mode::Button => roles.buttons.push(text),
        Mode::Input => match role {
            Role::Placeholder => roles.placeholders.push(text),
            Role::Error => roles.errors.push(text),
            _ => roles.labels.push(text),
        },
        Mode::Normal => match role {
            Role::Error => roles.errors.push(text),
            Role::Placeholder => roles.placeholders.push(text),
            Role::Heading => roles.headings.push(text),
            Role::Label => roles.labels.push(text),
            Role::Body => roles.body.push(text),
        },
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn text(name: &str, characters: &str, size: f64, weight: f64) -> Value {
        json!({"type": "TEXT", "name": name, "characters": characters,
               "style": {"fontSize": size, "fontWeight": weight}, "fills": [{"type": "SOLID", "color": {"r": 0.1, "g": 0.1, "b": 0.1}}]})
    }

    #[test]
    fn text_is_classified_by_its_role() {
        let frame = json!({"type": "FRAME", "name": "Login", "children": [
            text("t", "Welcome back", 24.0, 700.0),
            text("t", "Sign in to continue", 14.0, 400.0),
            {"type": "INSTANCE", "name": "Primary Button", "children": [text("t", "Sign In", 16.0, 600.0)]},
            {"type": "FRAME", "name": "Email input", "children": [
                text("label", "Email", 12.0, 400.0),
                text("placeholder text", "you@example.com", 14.0, 400.0),
                text("t", "Bad address", 14.0, 400.0),
            ]},
            {"type": "FRAME", "name": "Error banner", "children": [text("t", "Wrong password", 14.0, 400.0)]},
            text("t", "x", 14.0, 400.0),
            text("t", "Lorem ipsum dolor", 14.0, 400.0),
            text("t", "welcome back", 24.0, 700.0),
            {"type": "TEXT", "name": "t", "characters": "Hint grey", "style": {"fontSize": 14.0},
             "fills": [{"type": "SOLID", "color": {"r": 0.5, "g": 0.5, "b": 0.5}}]},
            {"type": "TEXT", "name": "t", "characters": "Required!", "style": {"fontSize": 14.0},
             "fills": [{"type": "SOLID", "color": {"r": 0.9, "g": 0.1, "b": 0.1}}]},
        ]});
        let roles = extract(&frame, NODES);
        assert_eq!(
            roles.headings,
            vec!["Welcome back"],
            "case-insensitive dedupe"
        );
        assert_eq!(roles.body, vec!["Sign in to continue"]);
        assert_eq!(roles.buttons, vec!["Sign In"]);
        assert_eq!(roles.labels, vec!["Email", "Bad address"]);
        assert_eq!(roles.placeholders, vec!["you@example.com", "Hint grey"]);
        assert_eq!(
            roles.errors,
            vec!["Wrong password", "Required!"],
            "a red fill or an error path is an error"
        );
        assert!(!roles.is_empty());
        assert!(extract(&json!({"type": "FRAME", "children": []}), NODES).is_empty());
    }

    const NODES: usize = 1_000_000;

    #[test]
    fn deep_text_is_extracted_and_both_walks_share_one_bound() {
        let nest = |levels: usize| {
            let mut node = text("t", "deep", 14.0, 400.0);
            for _ in 0..levels {
                node = json!({"type": "FRAME", "name": "f", "children": [node]});
            }
            node
        };
        // Fifteen levels (past the SDK's depth of ten) are read.
        assert_eq!(extract(&nest(15), NODES).body, vec!["deep"]);
        let mut found = Vec::new();
        texts(&nest(15), NODES, &mut found);
        assert_eq!(found, vec!["deep"]);
        // Past the one safety bound neither walk goes.
        assert!(extract(&nest(MAX_TREE_DEPTH + 1), NODES).is_empty());
        let mut found = Vec::new();
        texts(&nest(MAX_TREE_DEPTH + 1), NODES, &mut found);
        assert_eq!(found, [] as [String; 0]);
        // The node cap stops a walk.
        let wide = json!({"type": "FRAME", "children": [text("a", "one", 14.0, 400.0), text("b", "two", 14.0, 400.0)]});
        assert_eq!(extract(&wide, 2).body, vec!["one"]);
        let tree = json!({"type": "FRAME", "children": [text("a", "one", 14.0, 400.0), {"type": "GROUP", "children": [text("b", "two", 14.0, 400.0)]}]});
        let mut found = Vec::new();
        texts(&tree, NODES, &mut found);
        assert_eq!(found, vec!["one", "two"]);
    }

    #[test]
    fn hidden_layers_are_not_indexed_and_the_frames_own_name_sets_no_flags() {
        let hidden = json!({"type": "TEXT", "name": "t", "characters": "Old copy", "visible": false,
                            "style": {"fontSize": 14.0}});
        let group = json!({"type": "GROUP", "name": "g", "visible": false, "children": [text("t", "Inside hidden", 14.0, 400.0)]});
        let frame = json!({"type": "FRAME", "name": "Home", "children": [
            hidden.clone(), group.clone(), text("t", "Shown", 14.0, 400.0)]});
        assert_eq!(extract(&frame, NODES).body, vec!["Shown"]);
        let mut found = Vec::new();
        texts(&frame, NODES, &mut found);
        assert_eq!(found, vec!["Shown"]);

        for (name, text_name, expected_errors, expected_labels) in
            [("Checkout form", "t", 0, 0), ("Error handling", "t", 0, 0)]
        {
            let roles = extract(
                &json!({"type": "FRAME", "name": name, "children": [text(text_name, "Pay now", 14.0, 400.0)]}),
                NODES,
            );
            assert_eq!(roles.errors.len(), expected_errors, "{name}");
            assert_eq!(roles.labels.len(), expected_labels, "{name}");
            assert_eq!(roles.body, vec!["Pay now"], "{name}");
        }
        // A nested container still counts.
        let nested = extract(
            &json!({"type": "FRAME", "name": "Home", "children": [
                {"type": "FRAME", "name": "Error banner", "children": [text("t", "Oops", 14.0, 400.0)]},
                {"type": "FRAME", "name": "Signup form", "children": [text("t", "Name", 14.0, 400.0)]}]}),
            NODES,
        );
        assert_eq!(nested.errors, vec!["Oops"]);
        assert_eq!(nested.labels, vec!["Name"]);
    }

    #[test]
    fn the_mode_follows_whole_words_of_nested_containers_only() {
        let mode_of = |frame_name: &str, nested: &str| {
            extract(
                &json!({"type": "FRAME", "name": frame_name, "children": [
                    {"type": "FRAME", "name": nested, "children": [text("t", "Go on", 14.0, 400.0)]}]}),
                NODES,
            )
        };
        assert_eq!(mode_of("Home", "Primary Button").buttons, vec!["Go on"]);
        assert_eq!(mode_of("Home", "PrimaryButton").buttons, vec!["Go on"]);
        assert_eq!(mode_of("Home", "tab-bar").buttons, vec!["Go on"]);
        assert_eq!(mode_of("Home", "Text Field").labels, vec!["Go on"]);
        assert_eq!(mode_of("Home", "textfield").labels, vec!["Go on"]);
        // Substrings are not words.
        for nested in [
            "Tablet - Home",
            "Table view",
            "Selection screen",
            "Actions list",
            "Linkedin",
        ] {
            let roles = mode_of("Home", nested);
            assert!(
                roles.buttons.is_empty() && roles.labels.is_empty(),
                "{nested}"
            );
            assert_eq!(roles.body, vec!["Go on"], "{nested}");
        }
        // The frame being indexed never sets the mode, whatever its name.
        for frame_name in ["Button", "Tab", "Select dropdown", "Selection screen"] {
            let roles = extract(
                &json!({"type": "FRAME", "name": frame_name, "children": [text("t", "Go on", 14.0, 400.0)]}),
                NODES,
            );
            assert_eq!(roles.body, vec!["Go on"], "{frame_name}");
        }
    }
}
