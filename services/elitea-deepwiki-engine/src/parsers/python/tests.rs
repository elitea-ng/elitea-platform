//! Python extraction rules on small inline sources. Every expected value
//! here is what the Python parser returns for the same source (checked with
//! the reference engine); the whole-corpus comparison is
//! `tests/python_parser.rs`.

use super::{PythonParser, parse_source};
use crate::parsers::LanguageParser;
use crate::parsers::model::{ParseResult, RelationshipType, SymbolType};
use serde_json::json;

const FILE: &str = "/repo/pkg/mod.py";

/// Pass 1 only (`parse_file`), as a single-file parse — on a thread with
/// the parser's stack size, as `parse_files` runs it.
fn parse_one(source: &str) -> ParseResult {
    let source = source.to_owned();
    std::thread::Builder::new()
        .stack_size(super::STACK_SIZE)
        .spawn(move || parse_source(FILE, &source).result)
        .ok()
        .and_then(|handle| handle.join().ok())
        .unwrap_or_default()
}

fn rels(result: &ParseResult, rel_type: RelationshipType) -> Vec<(String, String)> {
    result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == rel_type)
        .map(|r| (r.source_symbol.clone(), r.target_symbol.clone()))
        .collect()
}

fn pair(a: &str, b: &str) -> (String, String) {
    (a.to_owned(), b.to_owned())
}

fn symbol<'r>(result: &'r ParseResult, name: &str) -> &'r crate::parsers::model::Symbol {
    let found = result.symbols.iter().find(|s| s.name == name);
    assert!(found.is_some(), "no symbol {name}");
    found.unwrap_or(&result.symbols[0])
}

#[test]
fn definitions_take_the_module_stem_as_their_outer_scope() {
    let result = parse_one(
        "class A:\n    X = 1\n    def __init__(self, a: int = 1):\n        pass\n    def m(self):\n        def inner():\n            pass\n\ndef top():\n    y = 2\n",
    );
    let a = symbol(&result, "A");
    assert_eq!(a.full_name.as_deref(), Some("mod.A"));
    assert_eq!(a.parent_symbol.as_deref(), Some("mod"));
    assert_eq!(
        symbol(&result, "__init__").symbol_type,
        SymbolType::Constructor
    );
    assert_eq!(symbol(&result, "m").symbol_type, SymbolType::Method);
    // Python quirk: a function nested in a method is "in a class" too.
    assert_eq!(symbol(&result, "inner").symbol_type, SymbolType::Method);
    assert_eq!(symbol(&result, "top").symbol_type, SymbolType::Function);
    // Class-level `X` is a constant by case; a function's `y` a variable.
    assert_eq!(symbol(&result, "X").symbol_type, SymbolType::Constant);
    assert_eq!(symbol(&result, "y").symbol_type, SymbolType::Variable);
    let param = result
        .symbols
        .iter()
        .find(|s| s.symbol_type == SymbolType::Parameter && s.name == "a");
    assert_eq!(param.and_then(|p| p.return_type.as_deref()), Some("int"));
    assert_eq!(param.map(|p| p.range.key()).as_deref(), Some("3:23-3:29"));
}

#[test]
fn a_decorated_definition_starts_at_def_and_ends_at_its_last_statement() {
    let result = parse_one(
        "@dec\nasync def f(a, *args: int, b=1, **kw) -> R:\n    x = 1  # c\n    # trailing\n\ny = 2\n",
    );
    let f = symbol(&result, "f");
    assert_eq!(f.range.key(), "2:0-3:9");
    assert!(f.is_async);
    assert_eq!(f.signature.as_deref(), Some("f(a, *args: int, **kw) -> R"));
    assert_eq!(
        f.source_text.as_deref(),
        Some("async def f(a, *args: int, b=1, **kw) -> R:\n    x = 1  # c")
    );
    assert_eq!(f.metadata["decorators"], json!(["dec"]));
    // Python quirk: keyword-only `b` is not a parameter symbol.
    let params: Vec<_> = result
        .symbols
        .iter()
        .filter(|s| s.symbol_type == SymbolType::Parameter)
        .map(|s| (s.name.as_str(), s.range.key()))
        .collect();
    assert_eq!(
        params,
        [
            ("a", "2:12-2:13".to_owned()),
            ("args", "2:16-2:25".to_owned()),
            ("kw", "2:34-2:36".to_owned())
        ]
    );
}

#[test]
fn annotations_are_written_as_get_type_annotation_writes_them() {
    let result = parse_one(
        "def f(a: Dict[str, int], b: Optional[Dict[str, Any]], c: Callable[[int], None], d: X | None, e: \"Fwd\", g: Annotated[str, Field(description=\"it's\")]) -> tuple[int, ...]:\n    pass\n",
    );
    let f = symbol(&result, "f");
    assert_eq!(
        f.parameter_types,
        [
            "Dict[(str, int)]",
            "Optional[Dict[(str, Any)]]",
            "Callable[([int], None)]",
            "X | None",
            "Fwd",
            "Annotated[(str, Field(description=\"it's\"))]",
        ]
    );
    assert_eq!(f.return_type.as_deref(), Some("tuple[(int, ...)]"));
}

#[test]
fn docstrings_are_decoded_and_cleaned() {
    let result = parse_one(
        "def f():\n    \"\"\"  First.\n\n        body\n\ttab\n    \"\"\"\n\ndef g():\n    r'raw \\n' 'next'\n\ndef h():\n    f'not {doc}'\n",
    );
    assert_eq!(
        symbol(&result, "f").docstring.as_deref(),
        Some("First.\n\nbody\ntab")
    );
    assert_eq!(
        symbol(&result, "g").docstring.as_deref(),
        Some("raw \\nnext")
    );
    assert_eq!(symbol(&result, "h").docstring, None);
}

#[test]
fn calls_of_known_classes_are_creates() {
    let result = parse_one(
        "class Repo:\n    pass\n\ndef build():\n    r = Repo()\n    helper(r)\n    return mod.Repo()\n",
    );
    assert_eq!(
        rels(&result, RelationshipType::Creates),
        [pair("build", "Repo"), pair("build", "Repo")]
    );
    assert_eq!(
        rels(&result, RelationshipType::Calls),
        [pair("build", "helper")]
    );
    let created = result
        .relationships
        .iter()
        .filter(|r| r.relationship_type == RelationshipType::Creates)
        .nth(1)
        .map(|r| r.annotations.clone());
    assert_eq!(
        created.and_then(|a| a.get("module_prefix").cloned()),
        Some(json!("mod"))
    );
}

#[test]
fn a_nested_class_clears_the_current_symbol() {
    // Python quirk: after `Inner`, `Outer`'s body records nothing.
    let result = parse_one(
        "class Outer:\n    a = first()\n    class Inner:\n        pass\n    b = second()\n",
    );
    assert_eq!(
        rels(&result, RelationshipType::Calls),
        [pair("Outer", "first")]
    );
}

#[test]
fn fields_come_from_init_and_class_annotations() {
    let result = parse_one(
        "class C:\n    db: Database\n    cache: Optional[Cache]\n    n: int = 1\n    def __init__(self, x):\n        self.client = api.Client()\n        self.x = x\n        self.opt: Optional[Store] = None\n        self.client = Other()\n",
    );
    let fields: Vec<_> = result
        .symbols
        .iter()
        .filter(|s| s.symbol_type == SymbolType::Field)
        .map(|s| s.name.as_str())
        .collect();
    assert_eq!(fields, ["client", "opt", "db", "cache", "n"]);
    assert_eq!(
        rels(&result, RelationshipType::Composition),
        [pair("mod.C.client", "Client"), pair("mod.C.db", "Database")]
    );
    assert_eq!(
        rels(&result, RelationshipType::Aggregation),
        [pair("mod.C.opt", "Store"), pair("mod.C.cache", "Cache")]
    );
}

#[test]
fn imports_and_exports_follow_the_module_info_rules() {
    let result = parse_one(
        "import a.b as ab, c\nfrom . import x\nfrom ..p.q import y as z\nfrom ..p.q import *\ndef f():\n    import late\n__all__ = ['f', \"g\"]\n",
    );
    assert_eq!(result.imports, ["a.b", "c", ".x", "p.q.y", "p.q.*", "late"]);
    assert_eq!(result.exports, ["f", "g"]);
    assert_eq!(
        rels(&result, RelationshipType::Imports),
        [
            pair("mod", "ab"),
            pair("mod", "c"),
            pair("mod", "x"),
            pair("mod", "z"),
            pair("mod", "*"),
            pair("mod", "late"),
        ]
    );
    assert_eq!(result.dependencies, ["", "a", "c", "late", "p"]);
}

#[test]
fn syntax_errors_leave_only_the_error() {
    let result = parse_one("print 'x'\n");
    assert!(result.symbols.is_empty());
    assert_eq!(
        result.errors,
        [
            "Syntax error: Missing parentheses in call to 'print'. Did you mean print(...)? (mod.py, line 1)"
        ]
    );
    let result = parse_one("\u{feff}x = 1\n");
    assert_eq!(
        result.errors,
        ["Syntax error: invalid non-printable character U+FEFF (mod.py, line 1)"]
    );
    let result = parse_one("x = (\n");
    assert_eq!(
        result.errors,
        ["Syntax error: '(' was never closed (mod.py, line 1)"]
    );
}

#[test]
fn python_recursion_limit_is_reproduced() {
    let chain = |n: usize| format!("x = {}\n", vec!["1"; n].join(" + "));
    assert!(parse_one(&chain(492)).errors.is_empty());
    assert_eq!(
        parse_one(&chain(493)).errors,
        ["Parse error: maximum recursion depth exceeded"]
    );
}

#[test]
fn reextraction_links_classes_and_functions_across_files() {
    let dir = std::env::temp_dir().join(format!("dwpy-test-{}", std::process::id()));
    let _ = std::fs::create_dir_all(&dir);
    let base = dir.join("base.py");
    let child = dir.join("child.py");
    let written = std::fs::write(&base, "class Base:\n    pass\n\ndef helper():\n    pass\n")
        .and_then(|()| {
            std::fs::write(
                &child,
                "from base import Base, helper\n\nclass Child(Base):\n    def run(self):\n        helper()\n        return Base()\n",
            )
        });
    assert!(written.is_ok());
    let files = vec![
        base.to_string_lossy().into_owned(),
        child.to_string_lossy().into_owned(),
    ];
    let results = PythonParser.parse_files(&files);
    let _ = std::fs::remove_dir_all(&dir);
    let child = &results[&files[1]];
    let inheritance = child
        .relationships
        .iter()
        .find(|r| r.relationship_type == RelationshipType::Inheritance);
    assert_eq!(
        inheritance.and_then(|r| r.target_file.clone()),
        Some(files[0].clone())
    );
    assert_eq!(
        inheritance.and_then(|r| r.annotations.get("import_path").cloned()),
        Some(json!("base.Base"))
    );
    // `Base()` is another file's class: `creates` only after re-extraction.
    assert_eq!(
        rels(child, RelationshipType::Creates),
        [pair("run", "Base")]
    );
    let call = child
        .relationships
        .iter()
        .find(|r| r.relationship_type == RelationshipType::Calls);
    assert_eq!(
        call.and_then(|r| r.target_file.clone()),
        Some(files[0].clone())
    );
}

#[test]
fn tokenizer_limits_are_reproduced() {
    let brackets = |n: usize| format!("x = {}{}\n", "[".repeat(n), "]".repeat(n));
    assert!(parse_one(&brackets(200)).errors.is_empty());
    assert_eq!(
        parse_one(&brackets(201)).errors,
        ["Syntax error: too many nested parentheses (mod.py, line 1)"]
    );
    let indented = |n: usize| {
        let mut text = String::new();
        for i in 0..n {
            text.push_str(&"    ".repeat(i));
            text.push_str("def f");
            text.push_str(&i.to_string());
            text.push_str("():\n");
        }
        text.push_str(&"    ".repeat(n));
        text.push_str("pass\n");
        text
    };
    assert!(parse_one(&indented(98)).errors.is_empty());
    assert_eq!(
        parse_one(&indented(100)).errors,
        ["Syntax error: too many levels of indentation (mod.py, line 101)"]
    );
}

#[test]
fn attribute_chains_hit_the_limit_sooner() {
    // `visit_Attribute` and `visit_Name` cost the relationship visitor a
    // frame more per level than `generic_visit` alone.
    let chain = |n: usize| format!("def f():\n    return a{}\n", ".b".repeat(n));
    assert!(parse_one(&chain(326)).errors.is_empty());
    assert_eq!(
        parse_one(&chain(327)).errors,
        ["Parse error: maximum recursion depth exceeded"]
    );
}

#[test]
fn very_deep_files_fail_without_exhausting_the_stack() {
    let dir = std::env::temp_dir().join(format!("dwpy-deep-{}", std::process::id()));
    let _ = std::fs::create_dir_all(&dir);
    let path = dir.join("deep.py");
    let deep = format!("x = {}\n", vec!["a"; 3000].join(" + "));
    let deeper = format!("x = {}\n", vec!["a"; 20000].join(" + "));
    let mut errors = Vec::new();
    for source in [deep, deeper] {
        assert!(std::fs::write(&path, source).is_ok());
        let file = path.to_string_lossy().into_owned();
        let results = PythonParser.parse_files(std::slice::from_ref(&file));
        errors.push(results[&file].errors.clone());
    }
    let _ = std::fs::remove_dir_all(&dir);
    for error in errors {
        assert_eq!(error, ["Parse error: maximum recursion depth exceeded"]);
    }
}
