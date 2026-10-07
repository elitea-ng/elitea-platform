//! The tree-sitter helpers the Python parser builds every rule from.
//!
//! Python's `node.children` lists every child, named or anonymous (keywords,
//! punctuation, comments, `ERROR` and zero-width `MISSING` nodes), and
//! `_find_child_by_type` returns the first child whose type is in a list —
//! the first in CHILD order, not in list order. These helpers keep both.

use super::source::Source;
use crate::model::Range;
use tree_sitter::Node;

/// Every child, in order.
pub(super) fn children(node: Node<'_>) -> Vec<Node<'_>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).collect()
}

/// `_find_child_by_type`: the first child whose kind is one of `kinds`.
pub(super) fn find_child<'t>(node: Node<'t>, kinds: &[&str]) -> Option<Node<'t>> {
    let mut cursor = node.walk();
    node.children(&mut cursor)
        .find(|child| kinds.contains(&child.kind()))
}

/// Whether any child has `kind`.
pub(super) fn has_child(node: Node<'_>, kind: &str) -> bool {
    find_child(node, &[kind]).is_some()
}

/// `_node_to_range`: 1-based lines, 0-based byte columns.
pub(super) fn range(node: Node<'_>) -> Range {
    let start = node.start_position();
    let end = node.end_position();
    Range::new(
        to_u32(start.row + 1),
        to_u32(start.column),
        to_u32(end.row + 1),
        to_u32(end.column),
    )
}

fn to_u32(value: usize) -> u32 {
    u32::try_from(value).unwrap_or(u32::MAX)
}

/// `_get_node_text`, with the Python byte-offset quirk ([`Source::slice`]).
pub(super) fn text<'s>(source: &'s Source, node: Node<'_>) -> &'s str {
    source.slice(node.start_byte(), node.end_byte())
}

/// `_extract_return_type` / the inline type-annotation loops: the text of the
/// first non-`:` child of the node's first `type_annotation` child.
pub(super) fn annotation_text(source: &Source, node: Node<'_>) -> Option<String> {
    let annotation = find_child(node, &["type_annotation"])?;
    first_type_in_annotation(annotation).map(|t| text(source, t).to_owned())
}

/// The first child of a `type_annotation` that is not the `:`.
pub(super) fn first_type_in_annotation(annotation: Node<'_>) -> Option<Node<'_>> {
    let mut cursor = annotation.walk();
    annotation
        .children(&mut cursor)
        .find(|child| child.kind() != ":")
}

/// `_contains_jsx`: whether the subtree holds a JSX element.
pub(super) fn contains_jsx(node: Node<'_>) -> bool {
    if matches!(
        node.kind(),
        "jsx_element" | "jsx_fragment" | "jsx_self_closing_element"
    ) {
        return true;
    }
    let mut cursor = node.walk();
    node.children(&mut cursor).any(contains_jsx)
}

/// Python's `str[0].isupper()` on a non-empty string.
pub(super) fn starts_upper(text: &str) -> bool {
    text.chars().next().is_some_and(char::is_uppercase)
}

/// Python's `str[0].islower()` on a non-empty string.
pub(super) fn starts_lower(text: &str) -> bool {
    text.chars().next().is_some_and(char::is_lowercase)
}

/// `Path(file_path).stem`.
pub(super) fn stem(file_path: &str) -> String {
    std::path::Path::new(file_path)
        .file_stem()
        .map(|s| s.to_string_lossy().into_owned())
        .unwrap_or_default()
}

/// Python's `file_path.endswith('.tsx')`, which picks the grammar and the
/// React metadata. Case-sensitive on purpose: `X.TSX` is parsed as plain
/// TypeScript by the Python parser too.
#[allow(clippy::case_sensitive_file_extension_comparisons)]
pub(super) fn is_tsx(file_path: &str) -> bool {
    file_path.ends_with(".tsx")
}
