//! The platform-neutral half of a snapshot: filtering, the node budget, ref
//! binding and the store of recent snapshots. Pure, so it is unit-tested on
//! every OS; the macOS driver only converts the vendor tree into
//! [`RawNode`]s and keeps the vendor's ref entries as the handle `H`.

use std::collections::VecDeque;

use crate::{DesktopError, ElementRef, Node, Rect, SnapshotBudget, SnapshotId};

/// One node as a platform adapter read it, before filtering.
#[derive(Debug, Clone, PartialEq)]
pub struct RawNode<H> {
    pub role: String,
    pub name: Option<String>,
    pub value: Option<String>,
    pub description: Option<String>,
    /// `AXSecureTextField` or its subrole. The value is dropped whatever the
    /// adapter put in it.
    pub secure: bool,
    pub states: Vec<String>,
    pub actions: Vec<String>,
    pub bounds: Option<Rect>,
    /// What the platform needs to find the element again. `Some` exactly
    /// when the element can be acted on; only those nodes get a ref.
    pub handle: Option<H>,
    /// The adapter stopped reading below this node (its own depth or time
    /// limit), so children are missing.
    pub children_cut: bool,
    pub children: Vec<RawNode<H>>,
}

impl<H> RawNode<H> {
    /// A node with only a role, for building trees by hand.
    #[must_use]
    pub fn new(role: impl Into<String>) -> Self {
        Self {
            role: role.into(),
            name: None,
            value: None,
            description: None,
            secure: false,
            states: Vec::new(),
            actions: Vec::new(),
            bounds: None,
            handle: None,
            children_cut: false,
            children: Vec::new(),
        }
    }
}

/// What one bound ref resolves to.
#[derive(Debug, Clone, PartialEq)]
pub struct Binding<H> {
    pub handle: H,
    pub role: String,
    /// The element's name (title) as the snapshot read it: the driver
    /// refuses a denied menu item by it.
    pub name: Option<String>,
    pub secure: bool,
}

/// A filtered, budgeted tree with its refs, before it is stored.
#[derive(Debug, Clone, PartialEq)]
pub struct BuiltTree<H> {
    pub root: Node,
    pub node_count: usize,
    pub truncated: bool,
    /// Index `i` is the binding of ref `e{i + 1}`.
    pub bindings: Vec<Binding<H>>,
}

/// States that take a node and its subtree out of a snapshot.
const INVISIBLE: [&str; 2] = ["hidden", "offscreen"];

/// Filters `raw` under `budget` and numbers its refs `e1`, `e2`, ... in
/// document order, bound to `snapshot`.
///
/// - Hidden and offscreen nodes go, with their subtrees.
/// - With `actionable_only`, a node stays when it can be acted on, when it
///   carries text (a label the model needs), or when a kept node is below
///   it; an unnamed container with one kept child is replaced by the child.
///   The root always stays.
/// - Nodes deeper than `max_depth` go. Past `max_nodes`, the walk stops.
///   Either cut, or an adapter cut, sets `truncated`.
/// - A secure node never carries a value.
#[must_use]
pub fn build_tree<H: Clone>(
    raw: &RawNode<H>,
    budget: &SnapshotBudget,
    snapshot: &SnapshotId,
) -> BuiltTree<H> {
    let filtered = filter(raw, 0, budget, true);
    let mut out = BuiltTree {
        root: Node {
            element_ref: None,
            role: raw.role.clone(),
            name: None,
            value: None,
            description: None,
            secure: false,
            states: Vec::new(),
            actions: Vec::new(),
            bounds: None,
            children: Vec::new(),
        },
        node_count: 0,
        truncated: false,
        bindings: Vec::new(),
    };
    match filtered {
        Filtered::Kept(tree, cut) => {
            out.truncated = cut;
            let mut walk = Walk {
                budget,
                snapshot,
                count: 0,
                truncated: false,
                bindings: Vec::new(),
            };
            if let Some(root) = walk.emit(tree) {
                out.root = root;
            }
            out.node_count = walk.count;
            out.truncated |= walk.truncated;
            out.bindings = walk.bindings;
        }
        Filtered::Dropped(cut) => out.truncated = cut,
    }
    out
}

/// The result of filtering one subtree; the flag is "something was cut".
enum Filtered<'a, H> {
    Kept(Kept<'a, H>, bool),
    Dropped(bool),
}

/// A kept node and its kept children, still borrowing the raw tree.
struct Kept<'a, H> {
    node: &'a RawNode<H>,
    children: Vec<Kept<'a, H>>,
}

fn filter<'a, H>(
    node: &'a RawNode<H>,
    depth: u8,
    budget: &SnapshotBudget,
    is_root: bool,
) -> Filtered<'a, H> {
    if !is_root && node.states.iter().any(|s| INVISIBLE.contains(&s.as_str())) {
        return Filtered::Dropped(false);
    }
    let mut cut = node.children_cut;
    let mut children = Vec::new();
    if depth >= budget.max_depth {
        cut |= !node.children.is_empty();
    } else {
        for child in &node.children {
            match filter(child, depth.saturating_add(1), budget, false) {
                Filtered::Kept(kept, child_cut) => {
                    cut |= child_cut;
                    children.push(kept);
                }
                Filtered::Dropped(child_cut) => cut |= child_cut,
            }
        }
    }
    let keep = is_root
        || !budget.actionable_only
        || node.handle.is_some()
        || has_text(node)
        || !children.is_empty();
    if !keep {
        return Filtered::Dropped(cut);
    }
    // An unnamed, inert wrapper of exactly one kept node adds nothing.
    if budget.actionable_only
        && !is_root
        && node.handle.is_none()
        && !has_text(node)
        && children.len() == 1
        && let Some(only) = children.pop()
    {
        return Filtered::Kept(only, cut);
    }
    Filtered::Kept(Kept { node, children }, cut)
}

fn has_text<H>(node: &RawNode<H>) -> bool {
    let present = |s: &Option<String>| s.as_deref().is_some_and(|s| !s.trim().is_empty());
    present(&node.name) || (!node.secure && present(&node.value))
}

struct Walk<'b, H> {
    budget: &'b SnapshotBudget,
    snapshot: &'b SnapshotId,
    count: usize,
    truncated: bool,
    bindings: Vec<Binding<H>>,
}

impl<H: Clone> Walk<'_, H> {
    fn emit(&mut self, kept: Kept<'_, H>) -> Option<Node> {
        if self.count >= self.budget.max_nodes {
            self.truncated = true;
            return None;
        }
        self.count += 1;
        let raw = kept.node;
        let element_ref = raw.handle.as_ref().and_then(|handle| {
            let index = u32::try_from(self.bindings.len() + 1).ok()?;
            self.bindings.push(Binding {
                handle: handle.clone(),
                role: raw.role.clone(),
                name: raw.name.clone(),
                secure: raw.secure,
            });
            Some(ElementRef::new(self.snapshot.clone(), index))
        });
        let limit = self.budget.max_text_chars;
        let mut node = Node {
            element_ref,
            role: raw.role.clone(),
            name: raw.name.as_deref().map(|s| clip(s, limit)),
            value: if raw.secure {
                None
            } else {
                raw.value.as_deref().map(|s| clip(s, limit))
            },
            description: raw.description.as_deref().map(|s| clip(s, limit)),
            secure: raw.secure,
            states: raw.states.clone(),
            actions: raw.actions.clone(),
            bounds: raw.bounds.filter(Rect::is_valid),
            children: Vec::new(),
        };
        if node.secure && !node.states.iter().any(|s| s == "secure") {
            node.states.push("secure".to_owned());
        }
        for child in kept.children {
            match self.emit(child) {
                Some(child) => node.children.push(child),
                None => break,
            }
        }
        Some(node)
    }
}

/// `text` cut to `limit` characters, with an ellipsis when cut.
#[must_use]
pub fn clip(text: &str, limit: usize) -> String {
    match text.char_indices().nth(limit) {
        Some((at, _)) => format!("{}…", &text[..at]),
        None => text.to_owned(),
    }
}

/// One stored snapshot: its bindings and whatever the platform keeps beside
/// them (on macOS, the vendor's ref entries and the window).
struct Stored<H, P> {
    id: SnapshotId,
    bindings: Vec<Binding<H>>,
    payload: P,
}

/// The snapshots a driver still answers refs for. The oldest goes once
/// `capacity` are held; a ref into it is then stale.
pub struct RefRegistry<H, P> {
    capacity: usize,
    next: u64,
    held: VecDeque<Stored<H, P>>,
}

impl<H, P> RefRegistry<H, P> {
    /// A registry that keeps the last `capacity` snapshots (at least one).
    #[must_use]
    pub fn new(capacity: usize) -> Self {
        Self {
            capacity: capacity.max(1),
            next: 1,
            held: VecDeque::new(),
        }
    }

    /// The id the next snapshot gets. Ids are never reused.
    pub fn next_id(&mut self) -> SnapshotId {
        let id = SnapshotId::from_sequence(self.next);
        self.next = self.next.saturating_add(1);
        id
    }

    /// Stores a snapshot under `id`, evicting the oldest past capacity.
    pub fn insert(&mut self, id: SnapshotId, bindings: Vec<Binding<H>>, payload: P) {
        self.held.retain(|s| s.id != id);
        self.held.push_back(Stored {
            id,
            bindings,
            payload,
        });
        while self.held.len() > self.capacity {
            self.held.pop_front();
        }
    }

    /// The binding and the snapshot payload of `element_ref`.
    ///
    /// # Errors
    /// [`DesktopError::StaleRef`] when the snapshot is not held (evicted,
    /// from another driver, or never taken) or has no such element.
    pub fn resolve(&self, element_ref: &ElementRef) -> Result<(&Binding<H>, &P), DesktopError> {
        let stored = self
            .held
            .iter()
            .find(|s| &s.id == element_ref.snapshot())
            .ok_or_else(|| {
                DesktopError::stale(
                    element_ref,
                    "the snapshot is no longer held; take a new one",
                )
            })?;
        let index = usize::try_from(element_ref.index())
            .ok()
            .and_then(|i| i.checked_sub(1))
            .ok_or_else(|| DesktopError::stale(element_ref, "no such element"))?;
        let binding = stored
            .bindings
            .get(index)
            .ok_or_else(|| DesktopError::stale(element_ref, "no such element in that snapshot"))?;
        Ok((binding, &stored.payload))
    }

    /// Drops every snapshot, so every ref handed out so far is stale.
    pub fn clear(&mut self) {
        self.held.clear();
    }

    /// How many snapshots are held.
    #[must_use]
    pub fn len(&self) -> usize {
        self.held.len()
    }

    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.held.is_empty()
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn button(name: &str, handle: u32) -> RawNode<u32> {
        let mut n = RawNode::new("button");
        n.name = Some(name.to_owned());
        n.handle = Some(handle);
        n
    }

    fn group(children: Vec<RawNode<u32>>) -> RawNode<u32> {
        let mut n = RawNode::new("group");
        n.children = children;
        n
    }

    fn sid() -> SnapshotId {
        SnapshotId::from_sequence(7)
    }

    fn refs(node: &Node, out: &mut Vec<String>) {
        if let Some(r) = &node.element_ref {
            out.push(r.to_string());
        }
        for c in &node.children {
            refs(c, out);
        }
    }

    #[test]
    fn refs_are_numbered_in_document_order_and_bound_to_the_snapshot() {
        let raw = group(vec![
            button("A", 10),
            group(vec![button("B", 20)]),
            button("C", 30),
        ]);
        let built = build_tree(&raw, &SnapshotBudget::default(), &sid());
        let mut got = Vec::new();
        refs(&built.root, &mut got);
        assert_eq!(got, ["@s7:e1", "@s7:e2", "@s7:e3"]);
        let handles: Vec<u32> = built.bindings.iter().map(|b| b.handle).collect();
        assert_eq!(handles, [10, 20, 30]);
        assert!(!built.truncated);
    }

    #[test]
    fn the_node_budget_cuts_the_tree_and_marks_it_truncated() {
        let raw = group((0..10).map(|i| button(&format!("b{i}"), i)).collect());
        let budget = SnapshotBudget {
            max_nodes: 4,
            ..SnapshotBudget::default()
        };
        let built = build_tree(&raw, &budget, &sid());
        assert_eq!(built.node_count, 4);
        assert_eq!(built.root.children.len(), 3);
        assert_eq!(built.bindings.len(), 3);
        assert!(built.truncated);
    }

    #[test]
    fn a_tree_exactly_at_the_budget_is_not_truncated() {
        let raw = group(vec![button("a", 1), button("b", 2), button("c", 3)]);
        let budget = SnapshotBudget {
            max_nodes: 4,
            ..SnapshotBudget::default()
        };
        let built = build_tree(&raw, &budget, &sid());
        assert_eq!(built.node_count, 4);
        assert!(!built.truncated);
    }

    #[test]
    fn the_depth_budget_cuts_and_marks_truncated() {
        let raw = group(vec![group(vec![group(vec![button("deep", 1)])])]);
        let budget = SnapshotBudget {
            max_depth: 2,
            actionable_only: false,
            ..SnapshotBudget::default()
        };
        let built = build_tree(&raw, &budget, &sid());
        assert!(built.truncated);
        assert_eq!(built.bindings.len(), 0);
    }

    #[test]
    fn an_adapter_cut_is_reported_as_truncated() {
        let mut inner = group(vec![button("x", 1)]);
        inner.children_cut = true;
        let built = build_tree(&group(vec![inner]), &SnapshotBudget::default(), &sid());
        assert!(built.truncated);
    }

    #[test]
    fn hidden_and_offscreen_subtrees_are_dropped() {
        let mut hidden = group(vec![button("secret", 1)]);
        hidden.states.push("hidden".into());
        let mut off = button("far", 2);
        off.states.push("offscreen".into());
        let raw = group(vec![hidden, off, button("seen", 3)]);
        let built = build_tree(&raw, &SnapshotBudget::default(), &sid());
        assert_eq!(built.root.children.len(), 1);
        assert_eq!(built.root.children[0].name.as_deref(), Some("seen"));
        assert_eq!(built.bindings.len(), 1);
        assert_eq!(built.bindings[0].handle, 3);
    }

    #[test]
    fn actionable_only_drops_inert_nodes_and_collapses_wrappers() {
        let mut label = RawNode::new("statictext");
        label.name = Some("Subject".into());
        let inert = RawNode::new("image");
        let raw = group(vec![
            group(vec![group(vec![button("Send", 1)])]),
            label,
            inert,
        ]);
        let built = build_tree(&raw, &SnapshotBudget::default(), &sid());
        let roles: Vec<&str> = built
            .root
            .children
            .iter()
            .map(|n| n.role.as_str())
            .collect();
        assert_eq!(roles, ["button", "statictext"]);
    }

    #[test]
    fn without_actionable_only_every_visible_node_stays() {
        let raw = group(vec![group(vec![RawNode::new("image")])]);
        let budget = SnapshotBudget {
            actionable_only: false,
            ..SnapshotBudget::default()
        };
        let built = build_tree(&raw, &budget, &sid());
        assert_eq!(built.node_count, 3);
    }

    #[test]
    fn a_secure_field_has_no_value_and_is_marked_secure() {
        let mut field = RawNode::new("textfield");
        field.secure = true;
        field.value = Some("hunter2".into());
        field.handle = Some(9);
        let built = build_tree(&group(vec![field]), &SnapshotBudget::default(), &sid());
        let node = &built.root.children[0];
        assert!(node.secure);
        assert_eq!(node.value, None);
        assert!(node.states.iter().any(|s| s == "secure"));
        assert!(built.bindings[0].secure);
        assert!(!format!("{built:?}").contains("hunter2"));
    }

    #[test]
    fn a_secure_value_does_not_count_as_text_that_keeps_a_node() {
        let mut field = RawNode::<u32>::new("textfield");
        field.secure = true;
        field.value = Some("pw".into());
        let built = build_tree(&group(vec![field]), &SnapshotBudget::default(), &sid());
        assert_eq!(built.root.children.len(), 0);
    }

    #[test]
    fn long_text_is_clipped_on_a_char_boundary() {
        assert_eq!(clip("héllo wörld", 5), "héllo…");
        assert_eq!(clip("short", 10), "short");
    }

    #[test]
    fn invalid_bounds_are_dropped() {
        let mut b = button("x", 1);
        b.bounds = Some(Rect {
            x: f64::NAN,
            y: 0.0,
            width: 1.0,
            height: 1.0,
        });
        let built = build_tree(&group(vec![b]), &SnapshotBudget::default(), &sid());
        assert_eq!(built.root.children[0].bounds, None);
    }

    #[test]
    fn a_ref_resolves_only_in_its_own_snapshot() {
        let mut reg: RefRegistry<u32, &str> = RefRegistry::new(4);
        let first = reg.next_id();
        reg.insert(
            first.clone(),
            vec![Binding {
                handle: 11,
                role: "button".into(),
                name: None,
                secure: false,
            }],
            "w1",
        );
        let second = reg.next_id();
        reg.insert(second.clone(), Vec::new(), "w2");
        let (b, p) = reg.resolve(&ElementRef::new(first.clone(), 1)).unwrap();
        assert_eq!((b.handle, *p), (11, "w1"));
        let err = reg.resolve(&ElementRef::new(second, 1)).unwrap_err();
        assert_eq!(err.code(), "stale_ref");
        let err = reg.resolve(&ElementRef::new(first, 2)).unwrap_err();
        assert_eq!(err.code(), "stale_ref");
    }

    #[test]
    fn an_evicted_or_unknown_snapshot_is_stale() {
        let mut reg: RefRegistry<u32, ()> = RefRegistry::new(2);
        let ids: Vec<SnapshotId> = (0..3)
            .map(|_| {
                let id = reg.next_id();
                reg.insert(
                    id.clone(),
                    vec![Binding {
                        handle: 1,
                        role: "button".into(),
                        name: None,
                        secure: false,
                    }],
                    (),
                );
                id
            })
            .collect();
        assert_eq!(reg.len(), 2);
        assert!(matches!(
            reg.resolve(&ElementRef::new(ids[0].clone(), 1)),
            Err(DesktopError::StaleRef { .. })
        ));
        assert!(reg.resolve(&ElementRef::new(ids[2].clone(), 1)).is_ok());
        assert!(matches!(
            reg.resolve(&ElementRef::new(SnapshotId::from_sequence(99), 1)),
            Err(DesktopError::StaleRef { .. })
        ));
        reg.clear();
        assert!(reg.resolve(&ElementRef::new(ids[2].clone(), 1)).is_err());
    }

    #[test]
    fn snapshot_ids_are_never_reused() {
        let mut reg: RefRegistry<u32, ()> = RefRegistry::new(1);
        let a = reg.next_id();
        let b = reg.next_id();
        assert_ne!(a, b);
    }
}
