//! Chromium's accessibility tree to the contract's raw [`Node`] tree.
//!
//! `Accessibility.getFullAXTree` returns one frame's tree as a flat list of
//! `AXNode`s (`nodeId`, `parentId`, `childIds`, `ignored`, `role`, `name`,
//! `value`, `properties`, `backendDOMNodeId`). This module turns that list
//! into [`Node`]s, which [`elitea_browser_tools::Snapshot::build`] then
//! normalises exactly as it does for the desktop driver:
//!
//! * ignored nodes are replaced by their children;
//! * Chromium's own role names become ARIA ones (`StaticText` is `text`,
//!   `Iframe` is `iframe`, a `<select>`'s `MenuListOption`s are `option`s
//!   directly under the `combobox`); inline text boxes and line breaks go;
//! * ARIA properties become [`State`]s;
//! * a field is `[secure]` when its element's `type` or `autocomplete`
//!   attribute says so ([`is_secure_field`]); Chromium's tree does not carry
//!   `autocomplete`, so the attributes come from the DOM ([`Elements`]);
//! * an element with a `tabindex` or `contenteditable` is actionable;
//! * an `iframe`'s document is a tree of its own (on its own CDP session
//!   when it is out of process), and
//!   it is grafted in place from the [`FrameTree`]s the driver read.

use std::collections::HashMap;
use std::sync::Arc;

use elitea_browser_tools::snapshot::{Node, State, is_secure_field};
use serde_json::Value;

/// Deeper nesting is cut: a page cannot make the agent recurse without
/// bound.
pub const MAX_DEPTH: usize = 256;
/// Roles whose content is their value, not their children.
const TEXT_FIELDS: &[&str] = &["textbox", "searchbox", "spinbutton"];
/// Frames inside frames, at most this deep.
pub const MAX_FRAME_DEPTH: usize = 8;

/// What the DOM says about one element.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Element {
    pub local_name: String,
    /// Attribute names in lower case.
    pub attributes: Vec<(String, String)>,
    /// The frame an `iframe` (or `frame`) element hosts.
    pub content_frame: Option<String>,
}

impl Element {
    #[must_use]
    pub fn attribute(&self, name: &str) -> Option<&str> {
        self.attributes
            .iter()
            .find(|(key, _)| key == name)
            .map(|(_, value)| value.as_str())
    }

    /// A password or payment card field (ADR-0048 decision 5).
    #[must_use]
    pub fn is_secure(&self) -> bool {
        is_secure_field(self.attribute("type"), self.attribute("autocomplete"))
    }

    /// A link, button or field by its tag or ARIA role: something an
    /// accessibility tree gives a ref. `auto` counts these to tell a page
    /// with controls from one of text alone, without a page script.
    #[must_use]
    pub fn is_control(&self) -> bool {
        let tag = match self.local_name.as_str() {
            "a" | "area" => self.attribute("href").is_some(),
            "button" | "select" | "textarea" | "summary" => true,
            "input" => !self
                .attribute("type")
                .is_some_and(|kind| kind.eq_ignore_ascii_case("hidden")),
            _ => false,
        };
        tag || self.attribute("role").is_some_and(|role| {
            matches!(
                role,
                "button"
                    | "link"
                    | "textbox"
                    | "searchbox"
                    | "combobox"
                    | "checkbox"
                    | "tab"
                    | "menuitem"
            )
        }) || self
            .attribute("tabindex")
            .and_then(|index| index.trim().parse::<i64>().ok())
            .is_some_and(|index| index >= 0)
            || self
                .attribute("contenteditable")
                .is_some_and(|value| !value.eq_ignore_ascii_case("false"))
    }

    fn is_actionable(&self) -> bool {
        self.attribute("tabindex").is_some()
            || self
                .attribute("contenteditable")
                .is_some_and(|value| !value.eq_ignore_ascii_case("false"))
    }
}

/// The page's elements by backend node id.
pub type Elements = HashMap<u64, Element>;

/// Read every element of a `DOM.getDocument` result (`depth: -1`,
/// `pierce: true`: frames' documents and shadow roots included).
#[must_use]
pub fn elements_of(document: &Value) -> Elements {
    let mut elements = Elements::new();
    let mut stack: Vec<(&Value, usize)> = vec![(document, 0)];
    while let Some((node, depth)) = stack.pop() {
        if depth > MAX_DEPTH * 4 {
            continue;
        }
        // nodeType 1 is an element.
        if node["nodeType"] == 1
            && let Some(id) = node["backendNodeId"].as_u64()
        {
            let attributes = node["attributes"]
                .as_array()
                .map(|flat| {
                    flat.chunks(2)
                        .filter_map(|pair| match pair {
                            [name, value] => Some((
                                name.as_str()?.to_ascii_lowercase(),
                                value.as_str()?.to_owned(),
                            )),
                            _ => None,
                        })
                        .collect()
                })
                .unwrap_or_default();
            elements.insert(
                id,
                Element {
                    local_name: node["localName"].as_str().unwrap_or_default().to_owned(),
                    attributes,
                    content_frame: node["frameId"].as_str().map(str::to_owned),
                },
            );
        }
        for key in ["children", "shadowRoots"] {
            if let Some(children) = node[key].as_array() {
                stack.extend(children.iter().map(|child| (child, depth + 1)));
            }
        }
        if let Some(content) = node.get("contentDocument") {
            stack.push((content, depth + 1));
        }
    }
    elements
}

/// One frame's tree as the driver read it.
#[derive(Clone, Debug, Default)]
pub struct FrameTree {
    /// The flattened CDP session the frame's renderer answers on.
    pub session: String,
    /// `Accessibility.getFullAXTree`'s `nodes`.
    pub nodes: Vec<Value>,
    /// The session's elements ([`elements_of`]).
    pub elements: Arc<Elements>,
}

impl FrameTree {
    /// The frames this frame's tree embeds (its `iframe` nodes' documents).
    #[must_use]
    pub fn hosted_frames(&self) -> Vec<String> {
        self.nodes
            .iter()
            .filter(|node| {
                node["ignored"] != true
                    && matches!(
                        node["role"]["value"].as_str(),
                        Some("Iframe" | "IframePresentational")
                    )
            })
            .filter_map(|node| node["backendDOMNodeId"].as_u64())
            .filter_map(|id| self.elements.get(&id)?.content_frame.clone())
            .collect()
    }
}

/// How the driver finds an element again: the session and frame it lives
/// in and its backend node id there (backend ids are per renderer, so the
/// session is part of the handle).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Handle {
    pub session: String,
    pub frame_id: String,
    pub backend_id: u64,
}

/// A converted tree. Each keyed [`Node::key`] is `1 +` its index in
/// `handles`; 0 is a node with no element.
#[derive(Debug, Default)]
pub struct Converted {
    pub roots: Vec<Node>,
    pub handles: Vec<Handle>,
    /// A grafted frame's id to the index in `handles` of the `iframe`
    /// element that hosts it (for its offset in the viewport).
    pub hosts: HashMap<String, usize>,
}

/// Convert `main_frame`'s tree, grafting every frame in `frames` it embeds.
#[must_use]
pub fn convert<S: std::hash::BuildHasher>(
    main_frame: &str,
    frames: &HashMap<String, FrameTree, S>,
) -> Converted {
    let mut out = Converted::default();
    out.roots = convert_frame(main_frame, frames, 0, &mut out);
    out
}

fn convert_frame<S: std::hash::BuildHasher>(
    frame_id: &str,
    frames: &HashMap<String, FrameTree, S>,
    frame_depth: usize,
    out: &mut Converted,
) -> Vec<Node> {
    let Some(tree) = frames.get(frame_id) else {
        return Vec::new();
    };
    let by_id: HashMap<&str, &Value> = tree
        .nodes
        .iter()
        .filter_map(|node| Some((node["nodeId"].as_str()?, node)))
        .collect();
    let Some(root) = tree
        .nodes
        .iter()
        .find(|node| node.get("parentId").is_none_or(Value::is_null))
    else {
        return Vec::new();
    };
    let mut walk = Walk {
        frame_id,
        tree,
        by_id: &by_id,
        frames,
        frame_depth,
        out,
    };
    // The root web area is the document itself: its children are the roots.
    walk.children(root, 0)
}

struct Walk<'a, 'b, S> {
    frame_id: &'a str,
    tree: &'a FrameTree,
    by_id: &'a HashMap<&'a str, &'a Value>,
    frames: &'a HashMap<String, FrameTree, S>,
    frame_depth: usize,
    out: &'b mut Converted,
}

impl<S: std::hash::BuildHasher> Walk<'_, '_, S> {
    fn children(&mut self, node: &Value, depth: usize) -> Vec<Node> {
        let mut out = Vec::new();
        if depth >= MAX_DEPTH {
            return out;
        }
        for child in node["childIds"].as_array().into_iter().flatten() {
            if let Some(child) = child.as_str().and_then(|id| self.by_id.get(id)) {
                out.extend(self.node(child, depth + 1));
            }
        }
        out
    }

    fn node(&mut self, ax: &Value, depth: usize) -> Vec<Node> {
        let raw_role = ax["role"]["value"].as_str().unwrap_or_default();
        if ax["ignored"] == true {
            return self.children(ax, depth);
        }
        let Some(role) = map_role(raw_role) else {
            return Vec::new();
        };
        let backend_id = ax["backendDOMNodeId"].as_u64().unwrap_or_default();
        let element = self.tree.elements.get(&backend_id);
        let mut node = Node::new(role, ax["name"]["value"].as_str().unwrap_or_default());
        if raw_role == "LabelText" {
            // A label's own name repeats the field it names; its text child
            // carries it, and the field holds it.
            node.name.clear();
        }
        if backend_id != 0 {
            self.out.handles.push(Handle {
                session: self.tree.session.clone(),
                frame_id: self.frame_id.to_owned(),
                backend_id,
            });
            node.key = self.out.handles.len() as u64;
        }
        node.value = value_of(&ax["value"]["value"]).filter(|_| role != "text");
        for property in ax["properties"].as_array().into_iter().flatten() {
            if let Some(state) = state_of(
                property["name"].as_str().unwrap_or_default(),
                &property["value"]["value"],
            ) {
                node.states.push(state);
            }
        }
        if let Some(element) = element {
            if element.local_name == "select" {
                // A native select reports `expanded: false` while its popup
                // is closed, which is not a disclosure state.
                node.states.retain(|state| *state != State::Collapsed);
            }
            if element.is_secure() {
                node.states.push(State::Secure);
                node.value = None;
            }
            node.actionable = element.is_actionable();
        }
        if node.is_secure() {
            return vec![node];
        }
        if TEXT_FIELDS.contains(&role) {
            // Chromium exposes a text field's content as a subtree of text
            // runs (inside the field's inner editor): the value shows it.
            return vec![node];
        }
        let mut children = self.children(ax, depth);
        let hosted = element.and_then(|element| element.content_frame.as_deref());
        if role == "iframe"
            && children.is_empty()
            && self.frame_depth < MAX_FRAME_DEPTH
            && let Some(hosted) = hosted
        {
            if node.key != 0 {
                let host = usize::try_from(node.key - 1).unwrap_or_default();
                self.out.hosts.insert(hosted.to_owned(), host);
            }
            children = convert_frame(hosted, self.frames, self.frame_depth + 1, self.out);
        }
        node.children = children;
        vec![node]
    }
}

/// A Chromium role as the contract's role; `None` drops the node and its
/// subtree (text boxes that only repeat their `StaticText`).
fn map_role(role: &str) -> Option<&str> {
    Some(match role {
        "InlineTextBox" | "LineBreak" => return None,
        "StaticText" => "text",
        "Iframe" | "IframePresentational" => "iframe",
        // A <select>'s popup: its options belong directly to the combobox.
        "MenuListPopup" | "RootWebArea" | "WebArea" | "LabelText" | "" => "generic",
        "MenuListOption" => "option",
        other => other,
    })
}

fn value_of(value: &Value) -> Option<String> {
    match value {
        Value::String(text) if !text.is_empty() => Some(text.clone()),
        Value::Number(number) => Some(number.to_string()),
        _ => None,
    }
}

fn truthy(value: &Value) -> bool {
    match value {
        Value::Bool(flag) => *flag,
        Value::String(text) => text == "true",
        _ => false,
    }
}

fn state_of(name: &str, value: &Value) -> Option<State> {
    Some(match name {
        "level" => State::Level(u8::try_from(value.as_u64()?).ok()?),
        "checked" => match value.as_str() {
            Some("mixed") => State::CheckedMixed,
            _ if truthy(value) => State::Checked,
            _ => return None,
        },
        "pressed" => match value.as_str() {
            Some("mixed") => State::PressedMixed,
            _ if truthy(value) => State::Pressed,
            _ => return None,
        },
        "selected" if truthy(value) => State::Selected,
        "expanded" => {
            if truthy(value) {
                State::Expanded
            } else {
                State::Collapsed
            }
        }
        "disabled" if truthy(value) => State::Disabled,
        "readonly" if truthy(value) => State::Readonly,
        "required" if truthy(value) => State::Required,
        "invalid" => match value.as_str() {
            Some("false") | None => return None,
            Some(_) => State::Invalid,
        },
        "focused" if truthy(value) => State::Focused,
        _ => return None,
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use serde_json::json;

    fn ax(id: &str, parent: Option<&str>, role: &str, name: &str, children: &[&str]) -> Value {
        let mut node = json!({
            "nodeId": id,
            "ignored": false,
            "role": {"type": "role", "value": role},
            "name": {"type": "computedString", "value": name},
            "childIds": children,
            "backendDOMNodeId": id.parse::<u64>().unwrap_or_default(),
        });
        if let Some(parent) = parent {
            node["parentId"] = json!(parent);
        }
        node
    }

    fn one_frame(nodes: Vec<Value>, elements: Elements) -> HashMap<String, FrameTree> {
        HashMap::from([(
            "F1".to_owned(),
            FrameTree {
                session: "S1".to_owned(),
                nodes,
                elements: Arc::new(elements),
            },
        )])
    }

    #[test]
    fn roles_states_and_ignored_nodes() {
        let mut heading = ax("2", Some("1"), "heading", "Title", &["3"]);
        heading["properties"] =
            json!([{"name": "level", "value": {"type": "integer", "value": 1}}]);
        let mut ignored = ax("4", Some("1"), "generic", "", &["5"]);
        ignored["ignored"] = json!(true);
        let mut button = ax("5", Some("4"), "button", "More", &[]);
        button["properties"] = json!([
            {"name": "expanded", "value": {"type": "booleanOrUndefined", "value": false}},
            {"name": "focusable", "value": {"type": "boolean", "value": true}},
        ]);
        let nodes = vec![
            ax("1", None, "RootWebArea", "Page", &["2", "4"]),
            heading,
            ax("3", Some("2"), "StaticText", "Title", &["6"]),
            ignored,
            button,
            ax("6", Some("3"), "InlineTextBox", "Title", &[]),
        ];
        let converted = convert("F1", &one_frame(nodes, Elements::new()));
        assert_eq!(converted.roots.len(), 2);
        assert_eq!(converted.roots[0].role, "heading");
        assert_eq!(converted.roots[0].states, vec![State::Level(1)]);
        assert_eq!(converted.roots[0].children[0].role, "text");
        assert_eq!(converted.roots[0].children[0].children, Vec::new());
        let button = &converted.roots[1];
        assert_eq!(button.role, "button");
        assert_eq!(button.states, vec![State::Collapsed]);
        let handle = &converted.handles[usize::try_from(button.key).unwrap() - 1];
        assert_eq!(
            handle,
            &Handle {
                session: "S1".to_owned(),
                frame_id: "F1".to_owned(),
                backend_id: 5
            }
        );
    }

    #[test]
    fn a_text_field_shows_its_content_as_its_value_only() {
        let mut field = ax("2", Some("1"), "textbox", "Name", &["3"]);
        field["value"] = json!({"type": "string", "value": "Ada"});
        let nodes = vec![
            ax("1", None, "RootWebArea", "", &["2"]),
            field,
            ax("3", Some("2"), "StaticText", "Ada", &[]),
        ];
        let converted = convert("F1", &one_frame(nodes, Elements::new()));
        assert_eq!(converted.roots[0].value.as_deref(), Some("Ada"));
        assert_eq!(converted.roots[0].children, Vec::new());
    }

    #[test]
    fn a_secure_field_keeps_no_value_and_no_children() {
        let mut field = ax("2", Some("1"), "textbox", "Card", &["3"]);
        field["value"] = json!({"type": "string", "value": "4111111111111111"});
        let nodes = vec![
            ax("1", None, "RootWebArea", "", &["2"]),
            field,
            ax("3", Some("2"), "StaticText", "4111111111111111", &[]),
        ];
        let mut elements = Elements::new();
        elements.insert(
            2,
            Element {
                local_name: "input".to_owned(),
                attributes: vec![("autocomplete".to_owned(), "cc-number".to_owned())],
                content_frame: None,
            },
        );
        let converted = convert("F1", &one_frame(nodes, elements));
        let field = &converted.roots[0];
        assert!(field.is_secure());
        assert_eq!(field.value, None);
        assert_eq!(field.children, Vec::new());
    }

    #[test]
    fn select_options_and_iframes_are_grafted() {
        let mut combo = ax("2", Some("1"), "combobox", "Country", &["3"]);
        combo["value"] = json!({"type": "string", "value": "France"});
        let mut option = ax("5", Some("3"), "MenuListOption", "France", &[]);
        option["properties"] =
            json!([{"name": "selected", "value": {"type": "booleanOrUndefined", "value": true}}]);
        let nodes = vec![
            ax("1", None, "RootWebArea", "", &["2", "6"]),
            combo,
            ax("3", Some("2"), "MenuListPopup", "", &["4", "5"]),
            ax("4", Some("3"), "MenuListOption", "Canada", &[]),
            option,
            ax("6", Some("1"), "Iframe", "Inner", &[]),
        ];
        let mut elements = Elements::new();
        elements.insert(
            6,
            Element {
                local_name: "iframe".to_owned(),
                attributes: Vec::new(),
                content_frame: Some("F2".to_owned()),
            },
        );
        let mut frames = one_frame(nodes, elements);
        assert_eq!(frames["F1"].hosted_frames(), vec!["F2".to_owned()]);
        frames.insert(
            "F2".to_owned(),
            FrameTree {
                session: "S2".to_owned(),
                nodes: vec![
                    ax("10", None, "RootWebArea", "", &["5"]),
                    ax("5", Some("10"), "button", "Go", &[]),
                ],
                elements: Arc::new(Elements::new()),
            },
        );
        let converted = convert("F1", &frames);
        let combo = &converted.roots[0];
        assert_eq!(combo.value.as_deref(), Some("France"));
        // The popup is a generic wrapper Snapshot::build removes.
        assert_eq!(combo.children[0].role, "generic");
        assert_eq!(combo.children[0].children[1].role, "option");
        assert_eq!(combo.children[0].children[1].states, vec![State::Selected]);
        let frame = &converted.roots[1];
        assert_eq!(frame.role, "iframe");
        let inner = &frame.children[0];
        assert_eq!(inner.name, "Go");
        // The same backend id in another renderer is another handle.
        let handle = &converted.handles[usize::try_from(inner.key).unwrap() - 1];
        assert_eq!(handle.session, "S2");
        assert_eq!(handle.backend_id, 5);
        assert_ne!(inner.key, combo.children[0].children[1].key);
        let host = converted.hosts["F2"];
        assert_eq!(converted.handles[host].backend_id, 6);
    }

    #[test]
    fn controls_are_links_buttons_and_fields() {
        let element = |tag: &str, attributes: &[(&str, &str)]| Element {
            local_name: tag.to_owned(),
            attributes: attributes
                .iter()
                .map(|(name, value)| ((*name).to_owned(), (*value).to_owned()))
                .collect(),
            content_frame: None,
        };
        assert!(element("a", &[("href", "/x")]).is_control());
        assert!(!element("a", &[]).is_control(), "an anchor without href");
        assert!(element("button", &[]).is_control());
        assert!(element("input", &[("type", "text")]).is_control());
        assert!(!element("input", &[("type", "HIDDEN")]).is_control());
        assert!(element("div", &[("role", "link")]).is_control());
        assert!(element("span", &[("tabindex", "0")]).is_control());
        // A focus target only for scripts (skip links land on it), not a
        // control: Chrome's tree gives it no ref.
        assert!(!element("main", &[("tabindex", "-1")]).is_control());
        assert!(!element("div", &[("tabindex", "x")]).is_control());
        assert!(!element("p", &[]).is_control());
        assert!(!element("div", &[("role", "presentation")]).is_control());
    }

    #[test]
    fn dom_elements_are_read_through_frames_and_shadow_roots() {
        let document = json!({
            "nodeType": 9, "backendNodeId": 1, "children": [{
                "nodeType": 1, "backendNodeId": 2, "localName": "input",
                "attributes": ["TYPE", "password", "id", "pw"],
                "shadowRoots": [{"nodeType": 11, "backendNodeId": 3, "children": [
                    {"nodeType": 1, "backendNodeId": 4, "localName": "div", "attributes": ["tabindex", "0"]}
                ]}]
            }, {
                "nodeType": 1, "backendNodeId": 5, "localName": "iframe", "frameId": "F2",
                "attributes": [],
                "contentDocument": {"nodeType": 9, "backendNodeId": 6, "children": [
                    {"nodeType": 1, "backendNodeId": 7, "localName": "input", "attributes": ["autocomplete", "new-password"]}
                ]}
            }]
        });
        let elements = elements_of(&document);
        assert!(elements[&2].is_secure());
        assert!(elements[&4].is_actionable());
        assert_eq!(elements[&5].content_frame.as_deref(), Some("F2"));
        assert!(elements[&7].is_secure());
        assert!(!elements.contains_key(&1));
    }
}
