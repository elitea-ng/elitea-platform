//! The Swift parser: a tree-sitter visitor (`tree-sitter-swift` 0.7.4)
//! that keeps the output model of the Inventory engine's former regex
//! `SwiftParser` (`elitea_inventory/engine/inventory/parsers/
//! swift_parser.py`), of which this module was first a transcription.
//!
//! What the model keeps from the regex parser:
//!
//! * symbol kinds: `class` (classes; structs, actors and extensions too,
//!   flagged `is_struct` / `is_actor` / `is_extension`, an extension named
//!   `<Type>_extension`), `interface` (protocols), `enum`, `function`
//!   (methods too), `variable`, `type_alias`; every symbol `global`, its
//!   `full_name` `<file stem>.<name>`, the visibility (default `internal`)
//!   both as the field and in the metadata, `is_exported` in the metadata
//!   and always `false`;
//! * relationships: `imports` (with the import `kind`), a class's first
//!   supertype `inheritance` (unless it starts with `Any`) and the rest
//!   `implementation`, struct / enum conformances `implementation`,
//!   protocol refinements `inheritance`, an extension `inheritance` to its
//!   type (`extension`) plus the type's `implementation` of each protocol
//!   (`via_extension`), `decorates` for upper-case attributes, `calls` for
//!   method calls (`a.f(…)`) and `aggregation` (the Python `USES`, which the
//!   Inventory ingestion maps to the same `uses` relation) for initializer
//!   calls (`Type(…)`); file-level edges come from the file stem,
//!   structural ones from the type's bare name.
//!
//! What the syntax tree changes (deliberately):
//!
//! * nothing inside a comment or a string is a symbol or an edge;
//! * declarations may span lines, and ranges are the declaration node's;
//! * a member carries `parent_symbol` (its type's `full_name`), so the
//!   Inventory derives `contains` edges; locals inside a function body,
//!   accessor or closure are not symbols;
//! * a property needs no type annotation to be a symbol (`type` is set
//!   when it has one), `private(set)` and protocol requirements parse, and
//!   `type` is the annotation alone (no trailing `{ get }` or body);
//! * `async` after the parameter list sets `is_async`;
//! * an actor's conformances are `implementation` edges (the regex made
//!   none), and a subscript (`a[i]`) is not a call;
//! * an import is one edge per `import` line (the regex pattern could
//!   swallow the next line), and `/** … */` doc blocks count as docs too.
//!
//! Each file is parsed alone: the regex parser had no cross-file registry
//! either. `validate_result` is not called, so duplicates are kept.

mod visitor;

#[cfg(test)]
mod tests;

use super::LanguageParser;
use crate::visit_support;
use std::collections::BTreeMap;

/// The Swift [`LanguageParser`].
#[derive(Debug, Default, Clone, Copy)]
pub struct SwiftParser;

impl LanguageParser for SwiftParser {
    fn language(&self) -> &'static str {
        "swift"
    }

    fn parse_files(&self, files: &[String]) -> BTreeMap<String, crate::model::ParseResult> {
        let grammar = tree_sitter_swift::LANGUAGE.into();
        visit_support::parse_files(files, "swift", &grammar, visitor::visit)
    }
}

/// Parse one source text (for the unit tests).
#[cfg(test)]
pub(crate) fn parse_source(file_path: &str, content: &str) -> crate::model::ParseResult {
    let grammar = tree_sitter_swift::LANGUAGE.into();
    visit_support::parse_text(file_path, "swift", &grammar, content, visitor::visit)
}
