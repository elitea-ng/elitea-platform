//! The code chunker: `parse_code_files_for_db`.
//!
//! A file in a language with a grammar is cut into its methods and
//! functions (a doc comment travels with the method it precedes); each
//! method is split at `chunk_size` characters with `chunk_overlap`, along
//! its syntax where the splitter can (`text-splitter`'s `CodeSplitter`, as
//! the Inventory engine chunks a file). A file with no method is chunked
//! whole. Code outside methods (imports, constants, class headers) is not a
//! chunk, as in the SDK. A language without a grammar here (and any
//! unmapped extension) is split into 256-token windows with overlap 30 in
//! the `gpt2` encoding.

use crate::tokens::{self, Encoding};
use crate::{Chunk, ChunkError};
use text_splitter::{ChunkConfig, CodeSplitter, TextSplitter};
use tree_sitter::{Node, Parser};

/// SDK defaults (`parse_code_files_for_db`).
pub(crate) const CHUNK_SIZE: usize = 1024;
pub(crate) const CHUNK_OVERLAP: usize = 128;
pub(crate) const UNKNOWN_CHUNK_SIZE: usize = 256;
pub(crate) const UNKNOWN_CHUNK_OVERLAP: usize = 30;

/// What the code chunker is told.
pub(crate) struct Code {
    pub chunk_size: usize,
    pub chunk_overlap: usize,
    pub unknown_chunk_size: usize,
    pub unknown_chunk_overlap: usize,
}

/// A language the SDK names, and the grammar this crate parses it with.
struct Language {
    /// The SDK's `Language.value`, the `language` metadata.
    label: &'static str,
    /// The `elitea_code_parsers::grammar_for` name, if there is a grammar.
    grammar: Option<&'static str>,
}

/// The SDK's `get_programming_language`, plus Swift (which has a grammar).
/// `.c` borrows the C++ grammar: its function nodes are the same. Ruby and
/// Haskell have no grammar here and take the unknown-language split.
fn language_of(extension: &str) -> Option<Language> {
    let (label, grammar) = match extension {
        ".py" => ("python", Some("python")),
        ".js" | ".jsx" | ".mjs" | ".cjs" => ("javascript", Some("javascript")),
        ".ts" | ".tsx" => ("typescript", Some("typescript")),
        ".java" => ("java", Some("java")),
        ".kt" | ".kts" => ("kotlin", Some("kotlin")),
        ".rs" => ("rust", Some("rust")),
        ".go" => ("go", Some("go")),
        ".cpp" => ("cpp", Some("cpp")),
        ".c" => ("c", Some("cpp")),
        ".cs" => ("c_sharp", Some("csharp")),
        ".swift" => ("swift", Some("swift")),
        ".hs" => ("haskell", None),
        ".rb" => ("ruby", None),
        _ => return None,
    };
    Some(Language { label, grammar })
}

/// Node kinds that are a method or function, per grammar. A node of one of
/// these kinds is taken whole; the walk does not look inside it.
fn method_kinds(grammar: &str) -> &'static [&'static str] {
    match grammar {
        "python" | "cpp" => &["function_definition"],
        "javascript" | "typescript" => &["function_declaration", "method_definition"],
        "java" | "csharp" => &["method_declaration", "constructor_declaration"],
        "kotlin" => &["function_declaration"],
        "rust" => &["function_item"],
        "go" => &["function_declaration", "method_declaration"],
        "swift" => &["function_declaration", "init_declaration"],
        _ => &[],
    }
}

fn is_comment(kind: &str) -> bool {
    matches!(
        kind,
        "comment" | "line_comment" | "block_comment" | "multiline_comment"
    )
}

/// A method's name: its `name` field, else the identifier inside its
/// declarator chain (C and C++).
fn name_of(node: Node<'_>, source: &[u8]) -> Option<String> {
    let mut at = node;
    loop {
        if let Some(name) = at.child_by_field_name("name") {
            return name.utf8_text(source).ok().map(str::to_owned);
        }
        match at.child_by_field_name("declarator") {
            Some(next) => at = next,
            None => break,
        }
    }
    (at.kind().ends_with("identifier"))
        .then(|| at.utf8_text(source).ok().map(str::to_owned))
        .flatten()
}

/// One method found in a file: its name and the text to chunk.
struct Method {
    name: Option<String>,
    text: String,
}

/// Nodes that belong to the method that follows them: attributes and
/// decorators written as siblings of the method (Rust `#[…]`, a decorator
/// run outside a Python `decorated_definition`, annotations and attribute
/// lists where a grammar makes them siblings). Java, C#, Kotlin and
/// TypeScript method annotations are children of the method and already in
/// its text.
fn is_attribute(kind: &str) -> bool {
    matches!(
        kind,
        "attribute_item"
            | "inner_attribute_item"
            | "decorator"
            | "decorated_definition"
            | "annotation"
            | "marker_annotation"
            | "attribute_list"
            | "attribute"
    )
}

/// The node a method's chunk starts at: its export wrapper (`export
/// function`, `export default function`) or, in Python, its
/// `decorated_definition`, so the export keyword and the decorators are part
/// of the method.
fn head_of(node: Node<'_>) -> Node<'_> {
    match node.parent() {
        Some(parent) if matches!(parent.kind(), "export_statement" | "decorated_definition") => {
            parent
        }
        _ => node,
    }
}

fn collect(node: Node<'_>, source: &[u8], kinds: &[&str], python: bool, out: &mut Vec<Method>) {
    if kinds.contains(&node.kind()) {
        let head = head_of(node);
        let Ok(body) = head.utf8_text(source) else {
            return;
        };
        let mut text = body.to_owned();
        if !python {
            // The run of comments and attributes right above the method (or
            // above its export wrapper), in any order.
            let mut above = Vec::new();
            let mut sibling = head.prev_named_sibling();
            while let Some(previous) =
                sibling.filter(|s| is_comment(s.kind()) || is_attribute(s.kind()))
            {
                if let Ok(piece) = previous.utf8_text(source) {
                    above.push(piece.trim_end().to_owned());
                }
                sibling = previous.prev_named_sibling();
            }
            if !above.is_empty() {
                above.reverse();
                text = format!("{}\n{text}", above.join("\n").trim());
            }
        }
        out.push(Method {
            name: name_of(node, source),
            text,
        });
        return;
    }
    let mut cursor = node.walk();
    for child in node.children(&mut cursor) {
        collect(child, source, kinds, python, out);
    }
}

fn splitter_config(size: usize, overlap: usize) -> ChunkConfig<text_splitter::Characters> {
    let size = size.max(1);
    ChunkConfig::new(size)
        .with_overlap(tokens::sane_overlap(size, overlap))
        .unwrap_or_else(|_| ChunkConfig::new(size))
}

/// The splitter of one file, built once: along the syntax when the grammar
/// allows, else by text.
enum Splitter {
    Code(CodeSplitter<text_splitter::Characters>),
    Text(TextSplitter<text_splitter::Characters>),
}

impl Splitter {
    fn new(grammar: &tree_sitter::Language, settings: &Code) -> Self {
        let config = || splitter_config(settings.chunk_size, settings.chunk_overlap);
        match CodeSplitter::new(grammar.clone(), config()) {
            Ok(splitter) => Self::Code(splitter),
            Err(_) => Self::Text(TextSplitter::new(config())),
        }
    }

    /// `piece` split at the character size.
    fn split(&self, piece: &str) -> Vec<String> {
        let parts: Vec<String> = match self {
            Self::Code(splitter) => splitter.chunks(piece).map(str::to_owned).collect(),
            Self::Text(splitter) => splitter.chunks(piece).map(str::to_owned).collect(),
        };
        if parts.is_empty() {
            vec![piece.to_owned()]
        } else {
            parts
        }
    }
}

/// Chunk a source file. `extension` is `.py`, `.rs`, …
pub(crate) fn code(text: &str, extension: &str, settings: &Code) -> Result<Vec<Chunk>, ChunkError> {
    if text.trim().is_empty() {
        return Ok(Vec::new());
    }
    let language = language_of(extension);
    let label = language.as_ref().map_or("unknown", |l| l.label);
    let grammar = language.as_ref().and_then(|l| l.grammar).and_then(|name| {
        elitea_code_parsers::grammar_for(name, &format!("file{extension}")).map(|g| (name, g))
    });

    let Some((grammar_name, grammar)) = grammar else {
        let parts = tokens::split(
            text,
            Encoding::Gpt2,
            settings.unknown_chunk_size,
            settings.unknown_chunk_overlap,
        )?;
        return Ok(parts
            .into_iter()
            .enumerate()
            .map(|(i, part)| Chunk::new(part, i + 1, "code", "text").with("language", label))
            .collect());
    };

    let mut methods = Vec::new();
    let mut parser = Parser::new();
    if parser.set_language(&grammar).is_ok()
        && let Some(tree) = parser.parse(text, None)
    {
        collect(
            tree.root_node(),
            text.as_bytes(),
            method_kinds(grammar_name),
            grammar_name == "python",
            &mut methods,
        );
    }
    if methods.is_empty() {
        // The whole file, as the SDK's fallback node.
        methods.push(Method {
            name: None,
            text: text.to_owned(),
        });
    }

    let splitter = Splitter::new(&grammar, settings);
    let mut chunks = Vec::new();
    for method in methods {
        let name = method.name.as_deref().unwrap_or("unknown");
        for part in splitter.split(&method.text) {
            let id = chunks.len() + 1;
            chunks.push(Chunk::new(part, id, "code", name).with("language", label));
        }
    }
    Ok(chunks)
}

#[cfg(test)]
mod tests {
    #![allow(clippy::format_collect)]
    use super::*;

    fn defaults() -> Code {
        Code {
            chunk_size: CHUNK_SIZE,
            chunk_overlap: CHUNK_OVERLAP,
            unknown_chunk_size: UNKNOWN_CHUNK_SIZE,
            unknown_chunk_overlap: UNKNOWN_CHUNK_OVERLAP,
        }
    }

    fn names(chunks: &[Chunk]) -> Vec<&str> {
        chunks
            .iter()
            .filter_map(|c| c.metadata["method_name"].as_str())
            .collect()
    }

    #[test]
    fn rust_functions_and_methods_are_chunks_with_their_doc_comments() {
        let source = "use std::fmt;\n\nconst N: u32 = 1;\n\n/// Adds.\n/// Twice.\nfn add(a: u32, b: u32) -> u32 {\n    a + b\n}\n\nstruct S;\nimpl S {\n    fn method(&self) -> u32 {\n        N\n    }\n}\n";
        let chunks = code(source, ".rs", &defaults()).unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(names(&chunks), ["add", "method"]);
        assert!(
            chunks[0].text.starts_with("/// Adds.\n/// Twice.\nfn add"),
            "{:?}",
            chunks[0].text
        );
        assert!(!chunks.iter().any(|c| c.text.contains("use std::fmt")));
        assert_eq!(chunks[0].metadata["language"], "rust");
        assert_eq!(chunks[1].chunk_id, 2);
        assert_eq!(chunks[0].chunk_type, "code");
    }

    #[test]
    fn python_methods_inside_classes_and_decorated_functions_are_found() {
        let source = "import os\n\nclass A:\n    def m(self):\n        \"\"\"Doc.\"\"\"\n        return 1\n\n@cache\ndef top(x):\n    return x\n";
        let chunks = code(source, ".py", &defaults()).unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(names(&chunks), ["m", "top"]);
        assert!(chunks[0].text.contains("\"\"\"Doc.\"\"\""));
        assert!(
            chunks[1].text.starts_with("@cache\ndef top"),
            "{:?}",
            chunks[1].text
        );
        assert_eq!(chunks[0].metadata["language"], "python");
    }

    #[test]
    fn exported_functions_start_at_the_export_with_the_doc_comment_above_it() {
        let source = "/** Greets.\n * @param n name\n */\nexport function greet(n: string): string {\n  return n;\n}\n\n/** The default. */\nexport default function main(): void {\n  greet('a');\n}\n\n// Plain.\nfunction local(): void {}\n";
        for extension in [".ts", ".js"] {
            let chunks = code(source, extension, &defaults()).unwrap_or_else(|e| panic!("{e}"));
            assert_eq!(names(&chunks), ["greet", "main", "local"], "{extension}");
            assert!(
                chunks[0]
                    .text
                    .starts_with("/** Greets.\n * @param n name\n */\nexport function greet"),
                "{:?}",
                chunks[0].text
            );
            assert!(
                chunks[1]
                    .text
                    .starts_with("/** The default. */\nexport default function main"),
                "{:?}",
                chunks[1].text
            );
            assert!(chunks[2].text.starts_with("// Plain.\nfunction local"));
        }
    }

    #[test]
    fn rust_attributes_travel_with_their_function_and_its_docs() {
        let source = "/// Adds.\n#[inline]\npub fn add(a: u32, b: u32) -> u32 {\n    a + b\n}\n\n#[test]\nfn adds() {\n    assert_eq!(add(1, 2), 3);\n}\n\n/// Subtracts.\n#[inline]\n#[must_use]\n/// More.\nfn sub() {}\n";
        let chunks = code(source, ".rs", &defaults()).unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(names(&chunks), ["add", "adds", "sub"]);
        assert!(
            chunks[0]
                .text
                .starts_with("/// Adds.\n#[inline]\npub fn add"),
            "{:?}",
            chunks[0].text
        );
        assert!(
            chunks[1].text.starts_with("#[test]\nfn adds"),
            "{:?}",
            chunks[1].text
        );
        assert!(
            chunks[2]
                .text
                .starts_with("/// Subtracts.\n#[inline]\n#[must_use]\n/// More.\nfn sub"),
            "{:?}",
            chunks[2].text
        );
    }

    #[test]
    fn java_and_csharp_annotations_stay_with_the_method() {
        let java = "class A {\n    /** Runs. */\n    @Override\n    public void run() {}\n}\n";
        let chunks = code(java, ".java", &defaults()).unwrap_or_else(|e| panic!("{e}"));
        assert!(
            chunks[0].text.starts_with("/** Runs. */\n@Override"),
            "{:?}",
            chunks[0].text
        );
        let csharp = "class A {\n    /// <summary>Runs.</summary>\n    [Obsolete]\n    public void Run() {}\n}\n";
        let chunks = code(csharp, ".cs", &defaults()).unwrap_or_else(|e| panic!("{e}"));
        assert!(
            chunks[0].text.contains("[Obsolete]"),
            "{:?}",
            chunks[0].text
        );
        assert!(chunks[0].text.contains("<summary>"), "{:?}", chunks[0].text);
    }

    #[test]
    fn typescript_functions_and_class_methods_are_found() {
        let source = "import x from 'y';\n\n// Greets.\nfunction greet(n: string): string {\n  return 'hi ' + n;\n}\n\nclass K {\n  run(): void {\n    greet('a');\n  }\n}\n";
        let chunks = code(source, ".ts", &defaults()).unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(names(&chunks), ["greet", "run"]);
        assert!(chunks[0].text.starts_with("// Greets.\nfunction greet"));
    }

    #[test]
    fn a_file_with_no_method_is_one_whole_chunk_named_unknown() {
        let chunks = code("X = 1\nY = 2\n", ".py", &defaults()).unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(chunks.len(), 1);
        assert_eq!(chunks[0].text, "X = 1\nY = 2");
        assert_eq!(names(&chunks), ["unknown"]);
    }

    #[test]
    fn an_oversized_method_is_split_at_the_character_size_with_overlap() {
        let body: String = (0..120)
            .map(|n| format!("    total += value_{n};\n"))
            .collect();
        let source = format!("fn big() {{\n    let mut total = 0;\n{body}    total\n}}\n");
        let chunks = code(&source, ".rs", &defaults()).unwrap_or_else(|e| panic!("{e}"));
        assert!(chunks.len() > 2, "{}", chunks.len());
        for chunk in &chunks {
            assert!(
                chunk.text.chars().count() <= CHUNK_SIZE,
                "{}",
                chunk.text.len()
            );
            assert_eq!(chunk.metadata["method_name"], "big");
        }
        assert_eq!(
            chunks.iter().map(|c| c.chunk_id).collect::<Vec<_>>(),
            (1..=chunks.len()).collect::<Vec<_>>()
        );
    }

    #[test]
    fn an_unknown_language_is_split_into_gpt2_token_windows() {
        let source: String = (0..300)
            .map(|n| format!("SELECT col{n} FROM t{n};\n"))
            .collect();
        let chunks = code(&source, ".sql", &defaults()).unwrap_or_else(|e| panic!("{e}"));
        assert!(chunks.len() > 2);
        assert_eq!(chunks[0].metadata["language"], "unknown");
        assert_eq!(chunks[0].metadata["method_name"], "text");
        // A mapped language without a grammar here takes the same path but
        // keeps its label.
        let ruby = code("def a\n  1\nend\n", ".rb", &defaults()).unwrap_or_else(|e| panic!("{e}"));
        assert_eq!(ruby[0].metadata["language"], "ruby");
        assert_eq!(ruby[0].metadata["method_name"], "text");
        assert!(
            code("  \n", ".rs", &defaults())
                .unwrap_or_default()
                .is_empty()
        );
    }
}
