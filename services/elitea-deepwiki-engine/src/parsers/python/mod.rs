//! The Python parser: the Python engine's `PythonParser`
//! (`python_parser.py`), which walks the stdlib `ast` module.
//!
//! The Python parser does not use tree-sitter, so this port parses with
//! tree-sitter-python and first rebuilds the tree Python 3.12's `ast`
//! would build (`lower` → `ast`); the visitors (`extract`) are then
//! the Python visitors over that tree, `ast.unparse` included (`unparse`).
//!
//! `parse_multiple_files`, as Python runs it:
//!
//! 1. every file alone (`parse_file`), in parallel: symbols, relationships
//!    with only the file's own classes known, fields, module info, then
//!    `validate_result`;
//! 2. the registries, filled from the files that produced symbols in the
//!    caller's sorted order — a name defined twice resolves to the LAST
//!    file, as Python's dict assignment;
//! 3. for each such file, relationships extracted AGAIN with every class
//!    of the repository known (a call of another file's class becomes
//!    `creates`) and NOT validated — Python quirk: duplicates, orphans and
//!    the field `defines` edges at `0:0` stay — then cross-file `target_file`
//!    for inheritance, calls and references.
//!
//! `_global_symbol_registry` is written but never read by the Python
//! parser, so it is not built here.

mod ast;
mod extract;
mod lower;
pub(crate) mod text;
mod unparse;

#[cfg(test)]
mod tests;

use super::LanguageParser;
use super::limits;
use super::model::{ParseResult, Relationship, RelationshipType, SymbolType};
use rayon::prelude::*;
use serde_json::Value;
use std::collections::{BTreeMap, HashMap, HashSet};

/// The Python [`LanguageParser`].
#[derive(Debug, Default, Clone, Copy)]
pub struct PythonParser;

impl LanguageParser for PythonParser {
    fn language(&self) -> &'static str {
        "python"
    }

    fn parse_files(&self, files: &[String]) -> BTreeMap<String, ParseResult> {
        let run = || {
            let parsed: Vec<Parsed> = files.par_iter().map(|path| parse_path(path)).collect();
            link(files, parsed)
        };
        // Lowering and the visitors recurse once per tree level; files may
        // nest up to `limits::MAX_TREE_DEPTH` levels, more than a default
        // 2 MiB thread stack holds.
        limits::on_worker_pool("Python", STACK_SIZE, run).unwrap_or_else(|error| {
            files
                .iter()
                .map(|path| (path.clone(), failed(path, error.clone())))
                .collect()
        })
    }
}

/// The stack of each parsing thread (address space; only touched pages
/// are committed).
const STACK_SIZE: usize = limits::LARGEST_PARSER_STACK;

/// One file after pass 1, with its tree kept for the re-extraction.
pub(crate) struct Parsed {
    result: ParseResult,
    module: Option<Vec<ast::Stmt>>,
}

/// Read and parse one file as `PythonParser.parse_file`.
fn parse_path(path: &str) -> Parsed {
    match read_python_text(path) {
        Ok(source) => {
            limits::with_output_budget(|| parse_source(path, &source)).unwrap_or_else(|error| {
                Parsed {
                    result: failed(path, error),
                    module: None,
                }
            })
        }
        Err(error) => Parsed {
            result: failed(path, format!("Parse error: {error}")),
            module: None,
        },
    }
}

/// `parse_file` on decoded text.
pub(crate) fn parse_source(path: &str, source: &str) -> Parsed {
    match lower::parse_module(source) {
        Ok(module) => {
            if recursion_limit_hit(&module) {
                return Parsed {
                    result: failed(path, "Parse error: maximum recursion depth exceeded"),
                    module: None,
                };
            }
            Parsed {
                result: extract::extract_file(path, source, &module),
                module: Some(module),
            }
        }
        Err(error) if error.too_deep => Parsed {
            result: failed(path, format!("Parse error: {}", error.message)),
            module: None,
        },
        Err(error) => {
            let message = match error.line {
                Some(line) => format!(
                    "Syntax error: {} ({}, line {line})",
                    error.message,
                    basename(path)
                ),
                None => format!("Syntax error: {}", error.message),
            };
            Parsed {
                result: failed(path, message),
                module: None,
            }
        }
    }
}

fn basename(path: &str) -> &str {
    path.rsplit('/').next().unwrap_or(path)
}

fn failed(path: &str, error: impl Into<String>) -> ParseResult {
    let mut result = ParseResult::new(path, "python");
    result.errors.push(error.into());
    result
}

/// Read a file as `open(path, 'r', encoding='utf-8').read()`: STRICT UTF-8
/// (Python quirk: unlike the tree-sitter parsers there is no
/// `errors='replace'`, so a bad byte fails the file) and universal newlines
/// (`\r\n` and a lone `\r` become `\n`, which moves byte columns).
fn read_python_text(path: &str) -> Result<String, String> {
    let bytes = std::fs::read(path).map_err(|error| match error.kind() {
        std::io::ErrorKind::NotFound => {
            format!("[Errno 2] No such file or directory: '{path}'")
        }
        _ => error.to_string(),
    })?;
    let text = match String::from_utf8(bytes) {
        Ok(text) => text,
        Err(error) => {
            let utf8 = error.utf8_error();
            let position = utf8.valid_up_to();
            let byte = error.as_bytes().get(position).copied().unwrap_or(0);
            let reason = match utf8.error_len() {
                None => "unexpected end of data",
                Some(_) if (0x80..0xc2).contains(&byte) || byte >= 0xf5 => "invalid start byte",
                Some(_) => "invalid continuation byte",
            };
            return Err(format!(
                "'utf-8' codec can't decode byte 0x{byte:02x} in position {position}: {reason}"
            ));
        }
    };
    if text.contains('\r') {
        Ok(text.replace("\r\n", "\n").replace('\r', "\n"))
    } else {
        Ok(text)
    }
}

/// Python's visitors recurse, and Python raises `RecursionError` past
/// 1000 frames; `parse_file` reports it as a parse error with no symbols.
/// The deepest frame stack each visitor reaches is modelled here node by
/// node, calibrated on the reference run (`x = 1 + … + 1` passes with 492
/// terms, `return a.b…b` with 326 attributes).
fn recursion_limit_hit(module: &[ast::Stmt]) -> bool {
    let symbols = module
        .iter()
        .map(|s| frames(ast::Node::Stmt(s), Visitor::Symbols))
        .max()
        .unwrap_or(0)
        + 3;
    let relationships = module
        .iter()
        .map(|s| frames(ast::Node::Stmt(s), Visitor::Relationships))
        .max()
        .unwrap_or(0)
        + 2;
    symbols.max(relationships) > MAX_FRAMES
}

/// The visitor whose frames [`frames`] counts.
#[derive(Clone, Copy, PartialEq, Eq)]
enum Visitor {
    Symbols,
    Relationships,
}

/// The most frames visiting `node` stacks: `visit` + `generic_visit` (2),
/// one more for a `visit_X` that calls `generic_visit` (3), `_visit_function`
/// one more again (4); a context or operator object is itself visited (2).
fn frames(node: ast::Node<'_>, visitor: Visitor) -> usize {
    use ast::{ExprKind, Node, StmtKind};
    let (own, leaf) = match node {
        Node::Stmt(stmt) => match (&stmt.kind, visitor) {
            (StmtKind::FunctionDef(_), _) => (4, 0),
            (StmtKind::ClassDef(_), _)
            | (StmtKind::Assign { .. } | StmtKind::AnnAssign { .. }, Visitor::Symbols)
            | (StmtKind::Import(_) | StmtKind::ImportFrom { .. }, Visitor::Relationships) => (3, 2),
            (StmtKind::Import(_) | StmtKind::ImportFrom { .. } | StmtKind::AugAssign { .. }, _) => {
                (2, 2)
            }
            _ => (2, 0),
        },
        Node::Expr(expr) => {
            let own = match (&expr.kind, visitor) {
                (
                    ExprKind::Call { .. } | ExprKind::Attribute { .. } | ExprKind::Name { .. },
                    Visitor::Relationships,
                ) => 3,
                _ => 2,
            };
            let leaf = match &expr.kind {
                ExprKind::Name { .. }
                | ExprKind::Attribute { .. }
                | ExprKind::Subscript { .. }
                | ExprKind::Starred { .. }
                | ExprKind::List { .. }
                | ExprKind::Tuple { .. }
                | ExprKind::BoolOp { .. }
                | ExprKind::BinOp { .. }
                | ExprKind::UnaryOp { .. }
                | ExprKind::Compare { .. } => 2,
                _ => 0,
            };
            (own, leaf)
        }
        _ => (2, 0),
    };
    let mut deepest = leaf;
    ast::for_each_child(node, &mut |child| {
        deepest = deepest.max(frames(child, visitor));
    });
    own + deepest
}

/// The frame budget left to the visitors under the reference run.
const MAX_FRAMES: usize = 991;

/// `_global_class_locations` and `_global_function_locations`: name →
/// index of the defining file (last file in sorted order wins).
#[derive(Default)]
struct Registries {
    classes: HashMap<String, usize>,
    functions: HashMap<String, usize>,
}

/// Passes 2 and 3 over per-file results in `files`' order.
pub(crate) fn link(files: &[String], mut parsed: Vec<Parsed>) -> BTreeMap<String, ParseResult> {
    let mut registries = Registries::default();
    for (index, file) in parsed.iter().enumerate() {
        if file.result.symbols.is_empty() {
            continue;
        }
        for symbol in &file.result.symbols {
            match symbol.symbol_type {
                SymbolType::Class => {
                    registries.classes.insert(symbol.name.clone(), index);
                }
                SymbolType::Function => {
                    registries.functions.insert(symbol.name.clone(), index);
                }
                _ => {}
            }
        }
    }
    let global_classes: HashSet<String> = registries.classes.keys().cloned().collect();
    parsed.par_iter_mut().enumerate().for_each(|(index, file)| {
        if file.result.symbols.is_empty() {
            return;
        }
        let Some(module) = file.module.take() else {
            return;
        };
        reextract(&files[index], &module, &mut file.result, &global_classes);
        enhance(files, index, &mut file.result, &registries);
    });
    files
        .iter()
        .cloned()
        .zip(parsed.into_iter().map(|p| p.result))
        .collect()
}

/// The re-extraction of `parse_multiple_files`: new relationships replace
/// the validated ones; field symbols are added when their full name is new.
fn reextract(
    path: &str,
    module: &[ast::Stmt],
    result: &mut ParseResult,
    global_classes: &HashSet<String>,
) {
    let (relationships, fields) =
        extract::extract_relationships(path, module, &result.symbols, global_classes);
    let mut known: HashSet<String> = result
        .symbols
        .iter()
        .filter_map(|s| s.full_name.clone().filter(|f| !f.is_empty()))
        .collect();
    for field in fields {
        if let Some(full) = field.full_name.clone().filter(|f| !f.is_empty())
            && known.insert(full)
        {
            result.symbols.push(field);
        }
    }
    result.relationships = relationships;
}

/// The imports map of `_enhance_cross_file_relationships`: the last dotted
/// part (or the whole name) of every `imports` target → the target.
fn imports_map(relationships: &[Relationship]) -> HashMap<String, String> {
    let mut imports = HashMap::new();
    for relationship in relationships {
        if relationship.relationship_type != RelationshipType::Imports {
            continue;
        }
        let target = &relationship.target_symbol;
        let key = target.rsplit('.').next().unwrap_or(target);
        imports.insert(key.to_owned(), target.clone());
    }
    imports
}

/// `_enhance_cross_file_relationships` / `_enhance_relationship`.
///
/// Python quirks kept: inheritance resolves only a base that is also an
/// imported name, and gets a `target_file` even when that is this file; a
/// call or reference whose target is a class of THIS file is never tried
/// as a function; `creates` is never resolved.
fn enhance(files: &[String], index: usize, result: &mut ParseResult, registries: &Registries) {
    let imports = imports_map(&result.relationships);
    let source_file = &files[index];
    for relationship in &mut result.relationships {
        let target = relationship.target_symbol.as_str();
        match relationship.relationship_type {
            RelationshipType::Inheritance => {
                if let (Some(import_path), Some(&file)) =
                    (imports.get(target), registries.classes.get(target))
                {
                    relationship.target_file = Some(files[file].clone());
                    relationship
                        .annotations
                        .insert("cross_file".to_owned(), Value::Bool(true));
                    relationship
                        .annotations
                        .insert("import_path".to_owned(), Value::String(import_path.clone()));
                }
            }
            RelationshipType::Calls | RelationshipType::References => {
                let file = if let Some(&file) = registries.classes.get(target) {
                    Some(file)
                } else {
                    registries.functions.get(target).copied()
                };
                if let Some(file) = file
                    && &files[file] != source_file
                {
                    relationship.target_file = Some(files[file].clone());
                    relationship
                        .annotations
                        .insert("cross_file".to_owned(), Value::Bool(true));
                }
            }
            _ => {}
        }
    }
}
