//! The Kotlin parser: a tree-sitter visitor (`tree-sitter-kotlin-ng`
//! 1.1.0) that keeps the output model of the Inventory engine's former
//! regex `KotlinParser` (`elitea_inventory/engine/inventory/parsers/
//! kotlin_parser.py`), of which this module was first a transcription.
//!
//! What the model keeps from the regex parser:
//!
//! * symbol kinds: `class` (classes, objects, companions; `enum` for an
//!   `enum class`), `interface`, `function` (members too), `variable`,
//!   `type_alias`; every symbol `global`, its `full_name` `<package>.<name>`
//!   (or the bare name without a package), `is_exported` in the metadata
//!   and always `false`, and the regex metadata keys (`visibility`,
//!   `modifiers`, `is_data_class`, `is_sealed`, `is_fun_interface`,
//!   `is_companion`, `is_suspend`, `is_inline`, `extension_receiver`,
//!   `mutable`, `type`, `aliased_type`);
//! * relationships: `imports` (alias / wildcard annotations), supertypes
//!   (`inheritance` for a supertype written with a constructor call or any
//!   supertype of an interface, else `implementation`), `composition` for a
//!   `by` delegate, `decorates` for annotations, `calls` for bare calls and
//!   `aggregation` (the Python `USES`, which the Inventory ingestion maps to
//!   the same `uses` relation) for constructor calls; file-level edges come
//!   from the file stem, structural ones from the type's bare name.
//!
//! What the syntax tree changes (deliberately):
//!
//! * nothing inside a comment or a string is a symbol or an edge;
//! * declarations may span lines (a class header, a parameter list, an
//!   annotation on its own line), and ranges are the declaration node's;
//! * a member carries `parent_symbol` (its type's `full_name`), so the
//!   Inventory derives `contains` edges; locals inside a function body,
//!   getter or lambda are not symbols;
//! * all of a declaration's modifiers are kept (the regex kept the last);
//! * an `import a.b.*` is ONE `imports` edge to `a.b` with `wildcard`;
//! * a constructor call is only a `uses` edge (the regex also made it a
//!   `calls` edge), and supertype constructor calls, annotation arguments
//!   and enum entries are not calls;
//! * an object's constructor-call supertype is `inheritance`.
//!
//! Each file is parsed alone: the regex parser had no cross-file registry
//! either. `validate_result` is not called, so duplicates are kept.

mod visitor;

#[cfg(test)]
mod tests;

use super::LanguageParser;
use crate::visit_support;
use std::collections::BTreeMap;

/// The Kotlin [`LanguageParser`].
#[derive(Debug, Default, Clone, Copy)]
pub struct KotlinParser;

impl LanguageParser for KotlinParser {
    fn language(&self) -> &'static str {
        "kotlin"
    }

    fn parse_files(&self, files: &[String]) -> BTreeMap<String, crate::model::ParseResult> {
        let grammar = tree_sitter_kotlin_ng::LANGUAGE.into();
        visit_support::parse_files(files, "kotlin", &grammar, visitor::visit)
    }
}

/// Parse one source text (for the unit tests).
#[cfg(test)]
pub(crate) fn parse_source(file_path: &str, content: &str) -> crate::model::ParseResult {
    let grammar = tree_sitter_kotlin_ng::LANGUAGE.into();
    visit_support::parse_text(file_path, "kotlin", &grammar, content, visitor::visit)
}
