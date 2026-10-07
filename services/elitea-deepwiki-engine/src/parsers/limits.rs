//! Per-file limits every parser applies to hostile input.
//!
//! * **Tree depth.** The extraction walks recurse once per tree level (as
//!   the Python visitors do). A file nested deeper than [`MAX_TREE_DEPTH`]
//!   levels fails alone with [`RECURSION_ERROR`] — the text Python's
//!   `RecursionError` gives, which the Python parsers hit far earlier (about
//!   500 nesting levels) — instead of overflowing the worker stack and
//!   aborting the process. The depth is measured without recursion, before
//!   the walk. No file of the parity corpora comes near the limit.
//! * **Output size.** Each symbol keeps its node's whole text, so nested
//!   declarations repeat the same bytes once per level: the output can grow
//!   with the square of the file. [`kept_text`] and [`charge`] count the
//!   output of the file being parsed against [`MAX_OUTPUT_BYTES`]; past it
//!   they keep nothing more, and [`with_output_budget`] fails the file with
//!   [`OUTPUT_ERROR`].
//! * **Worker pool.** [`on_worker_pool`] runs the parse on threads with the
//!   large stack the walks need. When that pool cannot start, the parse
//!   fails; it never falls back to the caller's (small) stack. The failure
//!   is also noted for the calling thread, so a caller inside
//!   [`with_pool_failures`] (the native generation) fails the whole run
//!   instead of indexing a repository whose files all failed to parse.

use std::cell::{Cell, RefCell};
use tree_sitter::Node;

/// The deepest tree a parser walks. Python's own parser refuses a file at
/// the same depth (`parsers::python::lower`), and at 64 MiB of worker stack
/// every recursive walk of this crate stays far below the stack size.
pub(crate) const MAX_TREE_DEPTH: usize = 4000;

/// The error of a file deeper than [`MAX_TREE_DEPTH`].
pub(crate) const RECURSION_ERROR: &str = "maximum recursion depth exceeded";

/// The most symbol text and relationship data one file may produce.
pub(crate) const MAX_OUTPUT_BYTES: usize = 256 << 20;

/// The error of a file whose output passes [`MAX_OUTPUT_BYTES`].
pub(crate) const OUTPUT_ERROR: &str =
    "output limit exceeded: the symbols of this file hold more than 268435456 bytes of text";

/// The depth of the tree under `root` (the root alone is depth 0), without
/// recursion.
pub(crate) fn tree_depth(root: Node<'_>) -> usize {
    let mut cursor = root.walk();
    let (mut depth, mut deepest) = (0usize, 0usize);
    loop {
        deepest = deepest.max(depth);
        if cursor.goto_first_child() {
            depth += 1;
            continue;
        }
        loop {
            if cursor.goto_next_sibling() {
                break;
            }
            if !cursor.goto_parent() {
                return deepest;
            }
            depth -= 1;
        }
    }
}

/// Whether the tree under `root` is deeper than [`MAX_TREE_DEPTH`].
pub(crate) fn too_deep(root: Node<'_>) -> bool {
    tree_depth(root) > MAX_TREE_DEPTH
}

/// The largest stack a parser thread reserves (the Python parser's).
/// Every thread of a pool reserves it in the address space `RLIMIT_AS`
/// counts, so the worker settings size the thread count against it.
pub(crate) const LARGEST_PARSER_STACK: usize = 256 << 20;

thread_local! {
    /// The first worker pool that could not start on this thread, inside
    /// [`with_pool_failures`].
    static POOL_FAILURE: RefCell<Option<String>> = const { RefCell::new(None) };
    /// The output bytes left for the file this thread parses; `None` outside
    /// [`with_output_budget`].
    static BUDGET: Cell<Option<usize>> = const { Cell::new(None) };
    /// Whether the file this thread parses went over its budget.
    static EXCEEDED: Cell<bool> = const { Cell::new(false) };
}

/// Run `parse` (one file, on this thread) with a fresh output budget.
/// `Err(OUTPUT_ERROR)` when the file went over it. Each file is parsed
/// whole on one thread (no parser calls into rayon inside a file), so a
/// thread-local budget counts exactly that file.
pub(crate) fn with_output_budget<T>(parse: impl FnOnce() -> T) -> Result<T, &'static str> {
    let saved = (BUDGET.get(), EXCEEDED.get());
    BUDGET.set(Some(MAX_OUTPUT_BYTES));
    EXCEEDED.set(false);
    let result = parse();
    let exceeded = EXCEEDED.get();
    BUDGET.set(saved.0);
    EXCEEDED.set(saved.1);
    if exceeded {
        Err(OUTPUT_ERROR)
    } else {
        Ok(result)
    }
}

/// Count `bytes` of output. `false` once the file is over its budget
/// (outside a budget, always `true`).
pub(crate) fn charge(bytes: usize) -> bool {
    match BUDGET.get() {
        None => true,
        Some(_) if EXCEEDED.get() => false,
        Some(left) => {
            if let Some(rest) = left.checked_sub(bytes) {
                BUDGET.set(Some(rest));
                true
            } else {
                EXCEEDED.set(true);
                false
            }
        }
    }
}

/// A symbol's `source_text` of `len` bytes, built by `make` only when the
/// file's budget holds it; `None` (the file then fails) when it does not.
pub(crate) fn kept_text(len: usize, make: impl FnOnce() -> String) -> Option<String> {
    charge(len).then(make)
}

/// [`kept_text`] for text already borrowed from the source.
pub(crate) fn kept_str(text: &str) -> Option<String> {
    kept_text(text.len(), || text.to_owned())
}

/// [`kept_text`] for text already built.
pub(crate) fn kept(text: String) -> Option<String> {
    charge(text.len()).then_some(text)
}

/// Run `job` on a rayon pool whose threads have `stack` bytes of stack.
/// `Err` (the text for every file's `errors`) when the pool cannot start.
pub(crate) fn on_worker_pool<T: Send>(
    language: &str,
    stack: usize,
    job: impl FnOnce() -> T + Send,
) -> Result<T, String> {
    rayon::ThreadPoolBuilder::new()
        .stack_size(stack)
        .thread_name(move |i| format!("parser-{i}"))
        .build()
        .map(|pool| pool.install(job))
        .map_err(|error| {
            let text = format!(
                "Parse error: the {language} parser could not start its worker threads: {error}"
            );
            POOL_FAILURE.with_borrow_mut(|failure| {
                if failure.is_none() {
                    *failure = Some(text.clone());
                }
            });
            text
        })
}

/// Run `work` on this thread and return the first worker pool failure
/// [`on_worker_pool`] met during it, if any.
pub(crate) fn with_pool_failures<T>(work: impl FnOnce() -> T) -> (T, Option<String>) {
    let saved = POOL_FAILURE.take();
    let result = work();
    let failure = POOL_FAILURE.replace(saved);
    (result, failure)
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn depth_is_measured_without_recursion() {
        // 100k nested parentheses: a recursive depth count would need far
        // more than the 2 MiB stack of a test thread.
        let source = format!("x = {}1{};", "(".repeat(100_000), ")".repeat(100_000));
        let mut parser = tree_sitter::Parser::new();
        let language: tree_sitter::Language = tree_sitter_javascript::LANGUAGE.into();
        assert!(parser.set_language(&language).is_ok());
        let tree = parser.parse(&source, None);
        let Some(tree) = tree else {
            unreachable!("tree-sitter returned no tree")
        };
        assert!(tree_depth(tree.root_node()) > 100_000);
        assert!(too_deep(tree.root_node()));
    }

    #[test]
    fn output_budget_fails_the_file_once_spent() {
        let ok = with_output_budget(|| kept_text(10, || "0123456789".to_owned()));
        assert_eq!(ok, Ok(Some("0123456789".to_owned())));
        let over = with_output_budget(|| {
            assert!(charge(MAX_OUTPUT_BYTES));
            kept_text(1, || unreachable!("over budget: nothing is built"))
        });
        assert_eq!(over, Err(OUTPUT_ERROR));
        // Outside a budget nothing is counted.
        assert!(charge(usize::MAX));
    }

    /// One file per language whose call `f(f(…f(1)…))` nests `levels`
    /// deep, in a statement position the extraction walks.
    fn nested_calls(language: &str, levels: usize) -> (&'static str, String) {
        let calls = format!("{}1{}", "f(".repeat(levels), ")".repeat(levels));
        match language {
            "cpp" => ("a.cpp", format!("void m() {{ {calls}; }}\n")),
            "csharp" => ("a.cs", format!("class A {{ void M() {{ {calls}; }} }}\n")),
            "go" => ("a.go", format!("package a\nfunc m() {{ {calls} }}\n")),
            "java" => ("A.java", format!("class A {{ void m() {{ {calls}; }} }}\n")),
            "javascript" => ("a.js", format!("function m() {{ {calls}; }}\n")),
            "rust" => ("a.rs", format!("fn m() {{ {calls}; }}\n")),
            "typescript" => ("a.ts", format!("function m(): void {{ {calls}; }}\n")),
            _ => unreachable!("no crafted file for {language}"),
        }
    }

    fn parse_one(language: &str, levels: usize) -> Vec<String> {
        let dir = std::env::temp_dir().join(format!(
            "dw-limits-{language}-{levels}-{}",
            std::process::id()
        ));
        assert!(std::fs::create_dir_all(&dir).is_ok());
        let (name, text) = nested_calls(language, levels);
        let path = dir.join(name);
        assert!(std::fs::write(&path, text).is_ok());
        let path = path.to_string_lossy().into_owned();
        let Some(parser) = crate::parsers::parser_for(language) else {
            unreachable!("no parser for {language}")
        };
        let results = parser.parse_files(std::slice::from_ref(&path));
        let _ = std::fs::remove_dir_all(&dir);
        results
            .get(&path)
            .map(|r| r.errors.clone())
            .unwrap_or_default()
    }

    const LANGUAGES: [&str; 7] = [
        "cpp",
        "csharp",
        "go",
        "java",
        "javascript",
        "rust",
        "typescript",
    ];

    /// A file nested 100k calls deep fails alone with Python's recursion
    /// text; before the depth limit, the recursive walks overflowed the
    /// 64 MiB worker stack and aborted the process.
    #[test]
    fn a_deeply_nested_file_fails_alone() {
        for language in LANGUAGES {
            let errors = parse_one(language, 100_000);
            assert!(
                errors.iter().any(|e| e.ends_with(RECURSION_ERROR)),
                "{language}: {errors:?}"
            );
        }
    }

    /// A file just under the depth limit still parses: the worker stack
    /// holds every walk at that depth.
    #[test]
    fn a_file_under_the_depth_limit_parses() {
        // Tree levels per nested call in each grammar.
        let per_call = [
            ("cpp", 2),
            ("csharp", 3),
            ("go", 2),
            ("java", 2),
            ("javascript", 2),
            ("rust", 2),
            ("typescript", 2),
        ];
        let failed: Vec<String> = per_call
            .into_iter()
            .filter_map(|(language, levels)| {
                let errors = parse_one(language, MAX_TREE_DEPTH / levels - 20);
                (!errors.is_empty()).then(|| format!("{language}: {errors:?}"))
            })
            .collect();
        assert!(failed.is_empty(), "{failed:?}");
    }

    fn parse_text(language: &str, name: &str, text: &str) -> (usize, Vec<String>) {
        let dir =
            std::env::temp_dir().join(format!("dw-limits-out-{language}-{}", std::process::id()));
        assert!(std::fs::create_dir_all(&dir).is_ok());
        let path = dir.join(name);
        assert!(std::fs::write(&path, text).is_ok());
        let path = path.to_string_lossy().into_owned();
        let Some(parser) = crate::parsers::parser_for(language) else {
            unreachable!("no parser for {language}")
        };
        let results = parser.parse_files(std::slice::from_ref(&path));
        let _ = std::fs::remove_dir_all(&dir);
        results
            .get(&path)
            .map(|r| (r.symbols.len(), r.errors.clone()))
            .unwrap_or_default()
    }

    /// `inner` inside `levels` nested `{head}{i}{tail}` … `}` blocks.
    fn wrap(head: &str, tail: &str, levels: usize, inner: &str) -> String {
        use std::fmt::Write;
        let mut text = String::new();
        for i in 0..levels {
            let _ = write!(text, "{head}{i}{tail}");
        }
        text.push_str(inner);
        text.push_str(&" }".repeat(levels));
        text
    }

    /// 1100 nested declarations around 300 KB of text: each symbol keeps
    /// its whole text, so the output would be about 330 MB from a 300 KB
    /// file. The file fails instead.
    #[test]
    fn output_that_grows_with_the_square_of_the_file_fails_it() {
        let payload = "x".repeat(300_000);
        let levels = 1100;
        let js = wrap(
            "function f",
            "() { ",
            levels,
            &format!("const s = \"{payload}\";"),
        );
        let java = wrap(
            "class C",
            " { ",
            levels,
            &format!("String s = \"{payload}\";"),
        );
        for (language, name, text) in [("javascript", "a.js", js), ("java", "A.java", java)] {
            let (symbols, errors) = parse_text(language, name, &text);
            assert_eq!(symbols, 0, "{language}");
            assert_eq!(errors, vec![OUTPUT_ERROR.to_owned()], "{language}");
        }
        // The same nesting around a short text parses.
        let small = wrap("function f", "() { ", levels, "const s = 1;");
        let (symbols, errors) = parse_text("javascript", "a.js", &small);
        assert!(errors.is_empty(), "{errors:?}");
        assert!(symbols > levels);
    }

    #[test]
    fn a_pool_that_cannot_start_is_noted_for_the_caller() {
        let (result, failure) = with_pool_failures(|| {
            let first = on_worker_pool("first", usize::MAX, || 1);
            let _ = on_worker_pool("second", usize::MAX, || 1);
            first
        });
        assert!(result.is_err());
        assert!(
            failure.is_some_and(|f| f.contains("the first parser could not start")),
            "the first failure is kept"
        );
        // A pool that starts notes nothing.
        let (_, failure) = with_pool_failures(|| on_worker_pool("ok", 1 << 20, || 1));
        assert_eq!(failure, None);
    }

    #[test]
    fn a_pool_that_cannot_start_is_an_error() {
        // No platform gives a thread a stack of the whole address space.
        let result = on_worker_pool("test", usize::MAX, || 1);
        assert!(result.is_err_and(|e| e.contains("could not start its worker threads")));
        assert_eq!(on_worker_pool("test", 1 << 20, || 1), Ok(1));
    }
}
