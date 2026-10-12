//! The TypeScript / TSX parser: a port of the Python engine's
//! `parsers/typescript_enhanced_parser.py` (`TypeScriptEnhancedParser`).
//!
//! A file whose path ends in `.tsx` (case-sensitive, as Python's
//! `endswith`) is parsed with the TSX grammar and gets the React-component
//! metadata; every other file with the TypeScript grammar.
//!
//! [`TypeScriptParser::parse_files`] is `parse_multiple_files`:
//!
//! 1. every file is parsed on its own (`parse_file`) — in parallel here;
//! 2. the cross-file registries (`_global_class_locations`, interface,
//!    function, type-alias and the general symbol registry) are filled from
//!    the files that produced symbols, in the caller's order, a later file
//!    overwriting an earlier one;
//! 3. `inheritance`, `implementation`, `calls`, `references`, `creates` and
//!    `imports` relationships whose target name is registered to ANOTHER
//!    file get that file as `target_file`.
//!
//! The Python parser fills no `imports`, `exports`, `dependencies`,
//! `module_docstring`, `docstring`, `comments` or `signature`; neither does
//! this one.

pub(crate) mod ast;
mod relations;
mod source;
mod symbols;
#[cfg(test)]
mod tests;

use super::LanguageParser;
use super::limits;
use super::model::{ParseResult, RelationshipType, SymbolType};
use crate::input::Sources;
use rayon::prelude::*;
use source::Source;
use std::collections::{BTreeMap, HashMap};

/// The TypeScript language parser.
#[derive(Debug, Default, Clone, Copy)]
pub struct TypeScriptParser;

/// Worker stack for the recursive walks. Python ran them under its own
/// recursion limit; a deeply nested file needs more than rayon's default.
const WORKER_STACK: usize = 64 * 1024 * 1024;

impl LanguageParser for TypeScriptParser {
    fn language(&self) -> &'static str {
        "typescript"
    }

    fn parse_sources(
        &self,
        files: &[String],
        sources: Sources<'_>,
    ) -> BTreeMap<String, ParseResult> {
        let parse_all =
            || -> Vec<ParseResult> { files.par_iter().map(|f| parse_path(f, sources)).collect() };
        let mut results = match limits::on_worker_pool("TypeScript", WORKER_STACK, parse_all) {
            Ok(results) => results,
            Err(error) => {
                return files
                    .iter()
                    .map(|f| {
                        (f.clone(), {
                            let mut result = ParseResult::new(f, "typescript");
                            result.errors.push(error.clone());
                            result
                        })
                    })
                    .collect();
            }
        };
        resolve_cross_file(&mut results);
        files.iter().cloned().zip(results).collect()
    }
}

/// Read and parse one file; a read failure is an `errors` entry, as the
/// Python `parse_file`'s exception handler records it.
fn parse_path(path: &str, sources: Sources<'_>) -> ParseResult {
    match sources.read(path) {
        Ok(bytes) => {
            let source = Source::decode(&bytes);
            limits::with_output_budget(|| parse_file(path, &source)).unwrap_or_else(|error| {
                let mut result = ParseResult::new(path, "typescript");
                result.errors.push(error.to_owned());
                result
            })
        }
        Err(error) => {
            let mut result = ParseResult::new(path, "typescript");
            result.errors.push(format!("Parse error: {error}"));
            result
        }
    }
}

/// `TypeScriptEnhancedParser.parse_file` for already-decoded text.
fn parse_file(path: &str, source: &Source) -> ParseResult {
    let mut result = ParseResult::new(path, "typescript");
    let language = if ast::is_tsx(path) {
        tree_sitter_typescript::LANGUAGE_TSX
    } else {
        tree_sitter_typescript::LANGUAGE_TYPESCRIPT
    };
    let mut parser = tree_sitter::Parser::new();
    if parser.set_language(&language.into()).is_err() {
        result
            .errors
            .push("Tree-sitter parser not available".to_owned());
        return result;
    }
    let Some(tree) = parser.parse(source.bytes(), None) else {
        result
            .errors
            .push("Failed to parse TypeScript file".to_owned());
        return result;
    };
    let root = tree.root_node();
    if limits::too_deep(root) {
        result
            .errors
            .push(format!("Parse error: {}", limits::RECURSION_ERROR));
        return result;
    }

    let mut symbol_walk = symbols::SymbolExtractor::new(source, path);
    symbol_walk.visit(root);
    let symbols = std::mem::take(&mut symbol_walk.symbols);
    let field_relationships = std::mem::take(&mut symbol_walk.relationships);

    let mut relationship_walk = relations::RelationshipExtractor::new(source, path, &symbols);
    relationship_walk.visit(root);
    let mut relationships = std::mem::take(&mut relationship_walk.relationships);
    relationships.extend(relations::defines_relationships(&symbols, path));
    relationships.extend(field_relationships);

    result.symbols = symbols;
    result.relationships = relationships;
    result.validate();
    result
}

/// Passes 2 and 3 of `parse_multiple_files`. `results` is in the caller's
/// order; the registries are filled in that order so the last file to
/// register a name wins, as in Python.
fn resolve_cross_file(results: &mut [ParseResult]) {
    #[derive(Default)]
    struct Registries<'r> {
        class: HashMap<&'r str, usize>,
        interface: HashMap<&'r str, usize>,
        function: HashMap<&'r str, usize>,
        type_alias: HashMap<&'r str, usize>,
        symbol: HashMap<&'r str, usize>,
    }

    let mut registries = Registries::default();
    for (index, result) in results.iter().enumerate() {
        for symbol in &result.symbols {
            let registry = match symbol.symbol_type {
                SymbolType::Class => Some(&mut registries.class),
                SymbolType::Interface => Some(&mut registries.interface),
                SymbolType::Function | SymbolType::Method => Some(&mut registries.function),
                SymbolType::TypeAlias => Some(&mut registries.type_alias),
                SymbolType::Constant => None,
                _ => continue,
            };
            let full = symbol.full_name.as_deref().filter(|f| !f.is_empty());
            if let Some(registry) = registry {
                registry.insert(symbol.name.as_str(), index);
                if let Some(full) = full {
                    registry.insert(full, index);
                }
            }
            if let Some(full) = full {
                registries.symbol.insert(full, index);
            }
            registries.symbol.insert(symbol.name.as_str(), index);
        }
    }

    let lookup = |target: &str| -> Option<usize> {
        registries
            .class
            .get(target)
            .or_else(|| registries.interface.get(target))
            .or_else(|| registries.function.get(target))
            .or_else(|| registries.type_alias.get(target))
            .or_else(|| registries.symbol.get(target))
            .copied()
    };
    let resolved: Vec<Vec<Option<usize>>> = results
        .iter()
        .map(|result| {
            if result.symbols.is_empty() {
                return Vec::new();
            }
            result
                .relationships
                .iter()
                .map(|rel| {
                    matches!(
                        rel.relationship_type,
                        RelationshipType::Inheritance
                            | RelationshipType::Implementation
                            | RelationshipType::Calls
                            | RelationshipType::References
                            | RelationshipType::Creates
                            | RelationshipType::Imports
                    )
                    .then(|| lookup(&rel.target_symbol))
                    .flatten()
                })
                .collect()
        })
        .collect();
    let paths: Vec<String> = results.iter().map(|r| r.file_path.clone()).collect();
    for (result, targets) in results.iter_mut().zip(resolved) {
        for (rel, target) in result.relationships.iter_mut().zip(targets) {
            if let Some(target) = target
                && paths[target] != rel.source_file
            {
                rel.target_file = Some(paths[target].clone());
            }
        }
    }
}
