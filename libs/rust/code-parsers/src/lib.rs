//! Language parsers: source files in, [`model::ParseResult`]s out.
//!
//! Each language is one [`LanguageParser`]. The graph builder hands it ALL of
//! that language's files at once, as the Python builder calls
//! `parse_multiple_files`: cross-file resolution (inheritance, imports,
//! calls into another file) needs every file's symbols before any file's
//! relationships are final.
//!
//! The contract every implementation keeps, because the graph gate depends
//! on it:
//!
//! * the result map is keyed by the path string the caller passed;
//! * output is deterministic — files may be parsed in parallel, but any
//!   cross-file registry is filled and read in the caller's (sorted) order;
//! * `symbol_type` / `rel_type` values are [`model`]'s wire values, and the
//!   extraction semantics are the Python visitor's for that language.

pub mod cpp;
pub mod csharp;
pub mod go;
pub mod java;
pub mod javascript;
pub mod kotlin;
mod limits;
pub mod model;
pub mod python;
mod regex_support;
pub mod rust_lang;
pub mod swift;
pub mod typescript;

pub use limits::{LARGEST_PARSER_STACK, with_pool_failures};

use model::ParseResult;
use std::collections::BTreeMap;

/// One language's parser.
pub trait LanguageParser: Send + Sync {
    /// The language name the graph uses (`"python"`, `"typescript"`, …).
    fn language(&self) -> &'static str;

    /// Parse every file of this language in the repository.
    ///
    /// `files` are absolute paths in sorted order. A file that cannot be read
    /// or parsed yields a result with `errors` set rather than being dropped,
    /// as the Python parsers do.
    fn parse_files(&self, files: &[String]) -> BTreeMap<String, ParseResult>;
}

/// The parser for `language`, if this engine has one.
///
/// One arm per ported language; a language without an arm is not parsed
/// (the Python builder logs the same for C and PHP).
#[must_use]
pub fn parser_for(language: &str) -> Option<Box<dyn LanguageParser>> {
    match language {
        "cpp" => Some(Box::new(cpp::CppParser)),
        "csharp" => Some(Box::new(csharp::CSharpParser)),
        "go" => Some(Box::new(go::GoParser)),
        "java" => Some(Box::new(java::JavaParser)),
        "javascript" => Some(Box::new(javascript::JavaScriptParser)),
        "kotlin" => Some(Box::new(kotlin::KotlinParser)),
        "python" => Some(Box::new(python::PythonParser)),
        "rust" => Some(Box::new(rust_lang::RustParser)),
        "swift" => Some(Box::new(swift::SwiftParser)),
        "typescript" => Some(Box::new(typescript::TypeScriptParser)),
        _ => None,
    }
}

/// The tree-sitter grammar this crate parses `language` with (`path` picks
/// TSX over TypeScript), for a caller that needs only the syntax tree, such
/// as a syntax-aware chunker. Kotlin and Swift are regex ports and have
/// none.
#[must_use]
pub fn grammar_for(language: &str, path: &str) -> Option<tree_sitter::Language> {
    Some(match language {
        "cpp" => tree_sitter_cpp::LANGUAGE.into(),
        "csharp" => tree_sitter_c_sharp::LANGUAGE.into(),
        "go" => tree_sitter_go::LANGUAGE.into(),
        "java" => tree_sitter_java::LANGUAGE.into(),
        "javascript" => tree_sitter_javascript::LANGUAGE.into(),
        "python" => tree_sitter_python::LANGUAGE.into(),
        "rust" => tree_sitter_rust::LANGUAGE.into(),
        "typescript" if typescript::ast::is_tsx(path) => {
            tree_sitter_typescript::LANGUAGE_TSX.into()
        }
        "typescript" => tree_sitter_typescript::LANGUAGE_TYPESCRIPT.into(),
        _ => return None,
    })
}
