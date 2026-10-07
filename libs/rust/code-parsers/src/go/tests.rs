//! Go extraction rules on small inline sources, mirroring the Python parser.

use super::visitor::{parse_source, strip_pointer_slice};
use super::{GoParser, link};
use crate::LanguageParser;
use crate::model::{ParseResult, RelationshipType, SymbolType};
use serde_json::json;

fn parse_one(source: &str) -> ParseResult {
    parse_source("/repo/pkg/file.go", source)
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

#[test]
fn imports_use_the_file_stem_and_the_last_path_segment() {
    let result = parse_one(
        "package x\nimport (\n  \"net/http\"\n  f \"os\"\n  _ \"embed\"\n  . \"fmt\"\n)\n",
    );
    assert_eq!(result.imports, ["net/http", "os", "embed", "fmt"]);
    let imports = rels(&result, RelationshipType::Imports);
    assert_eq!(imports[0], pair("file", "net/http"));
    let annotations: Vec<_> = result
        .relationships
        .iter()
        .map(|r| r.annotations.clone())
        .collect();
    assert_eq!(annotations[1]["alias"], json!("f"));
    assert_eq!(annotations[2]["is_blank"], json!(true));
    // The grammar's `dot` node is not the `.` Python looks for.
    assert_eq!(annotations[3]["is_dot"], json!(false));
}

#[test]
fn structs_record_fields_embedding_and_field_types() {
    let result = parse_one(
        "package x\n// Server serves.\ntype Server struct {\n\tHost, Alt string `json:\"h\"`\n\tLog *Logger\n\tItems map[string][]Item\n\t*Base\n\tio.Reader\n}\n",
    );
    let server = &result.symbols[0];
    assert_eq!(server.symbol_type, SymbolType::Struct);
    assert_eq!(server.docstring.as_deref(), Some("Server serves."));
    assert_eq!(server.signature.as_deref(), Some("type Server struct"));
    assert_eq!(server.range.start.line, 3);
    let fields: Vec<_> = result.symbols[1..]
        .iter()
        .map(|s| s.name.as_str())
        .collect();
    assert_eq!(fields, ["Host", "Log", "Items", "Base", "Reader"]);
    assert_eq!(result.symbols[1].metadata["struct_tag"], json!("json:\"h"));
    assert_eq!(
        rels(&result, RelationshipType::Aggregation),
        [pair("Server", "Logger")]
    );
    // Python quirk: the `map[K]` prefix is stripped last, so `[]Item` stays.
    assert_eq!(
        rels(&result, RelationshipType::Composition),
        [
            pair("Server", "[]Item"),
            pair("Server", "Base"),
            pair("Server", "Reader")
        ]
    );
}

#[test]
fn interfaces_record_methods_and_embedding() {
    let result = parse_one(
        "package x\ntype Store interface {\n\tio.Closer\n\tGet(id string) (Item, error)\n\tName() string\n}\n",
    );
    let get = result.symbols.iter().find(|s| s.name == "Get");
    assert_eq!(
        get.and_then(|s| s.signature.as_deref()),
        Some("Get(id string)")
    );
    assert_eq!(get.and_then(|s| s.return_type.clone()), None);
    let name = result.symbols.iter().find(|s| s.name == "Name");
    assert_eq!(name.and_then(|s| s.return_type.as_deref()), Some("string"));
    assert_eq!(
        rels(&result, RelationshipType::Inheritance),
        [pair("Store", "io.Closer")]
    );
}

#[test]
fn functions_and_methods_keep_the_python_signatures() {
    let result = parse_one(
        "package x\nfunc init() {}\nfunc init() {}\nfunc F[K comparable](a, b int, string) error { return nil }\nfunc G() (int, error) { return 0, nil }\nfunc (s *S) M(xs ...int) error { return nil }\nfunc (s *Stack[T]) Push(v T) {}\n",
    );
    let names: Vec<_> = result.symbols.iter().map(|s| s.name.as_str()).collect();
    assert_eq!(names, ["init", "init_L3", "F", "G", "M"]);
    let f = &result.symbols[2];
    assert_eq!(
        f.signature.as_deref(),
        Some("func F[K comparable](a int, b int,  string)")
    );
    assert_eq!(f.return_type, None);
    assert_eq!(result.symbols[3].return_type.as_deref(), Some("int, error"));
    let m = &result.symbols[4];
    assert_eq!(
        m.signature.as_deref(),
        Some("func (S) M(xs xs ...int) error")
    );
    assert_eq!(m.full_name.as_deref(), Some("S.M"));
    assert_eq!(m.metadata["is_pointer_receiver"], json!(true));
}

#[test]
fn bodies_record_calls_creates_and_references() {
    let result = parse_one(
        "package x\nimport \"fmt\"\nfunc F() {\n\tfmt.Println(len(xs))\n\tdefer s.Close(g())\n\tgo run()\n\t_ = Thing{}\n\t_ = []int{1}\n\t_ = v.(Foo)\n\tx.append(1)\n}\n",
    );
    let calls = rels(&result, RelationshipType::Calls);
    assert_eq!(
        calls,
        [
            pair("F", "fmt.Println"),
            pair("F", "s.Close"),
            pair("F", "run")
        ]
    );
    let deferred = &result
        .relationships
        .iter()
        .find(|r| r.target_symbol == "s.Close");
    assert_eq!(
        deferred.map(|r| r.annotations.clone()),
        Some(
            json!({"is_deferred": true, "is_method_call": true})
                .as_object()
                .cloned()
                .unwrap_or_default()
        )
    );
    assert_eq!(
        rels(&result, RelationshipType::Creates),
        [pair("F", "Thing")]
    );
    assert_eq!(
        rels(&result, RelationshipType::References),
        [pair("F", "Foo")]
    );
}

#[test]
fn iota_groups_become_enums_and_grouped_vars_are_skipped() {
    let result = parse_one(
        "package x\nconst (\n\tA Kind = iota\n\tB\n)\nvar (\n\tV = 1\n)\nvar W = 2\ntype Kind int\ntype Alias = pkg.T\n",
    );
    let names: Vec<_> = result
        .symbols
        .iter()
        .map(|s| (s.name.as_str(), s.symbol_type))
        .collect();
    assert_eq!(
        names,
        [
            ("Kind", SymbolType::Enum),
            ("A", SymbolType::Constant),
            ("B", SymbolType::Constant),
            ("W", SymbolType::Variable),
            ("Kind", SymbolType::TypeAlias),
            ("Alias", SymbolType::TypeAlias),
        ]
    );
    assert_eq!(
        rels(&result, RelationshipType::AliasOf),
        [pair("Alias", "pkg.T")]
    );
}

#[test]
fn same_file_methods_are_defined_at_line_one() {
    let result = parse_one("package x\ntype S struct{}\nfunc (s S) Run() {}\n");
    let defines: Vec<_> = result
        .relationships
        .iter()
        .filter(|r| r.annotations.get("cross_file") == Some(&json!(false)))
        .collect();
    assert_eq!(defines.len(), 1);
    assert_eq!(defines[0].target_symbol, "S.Run");
    assert_eq!(defines[0].source_range.map(|r| r.start.line), Some(1));
}

#[test]
fn cross_file_linking_matches_python() {
    let files = vec!["/r/a.go".to_owned(), "/r/b.go".to_owned()];
    let a = parse_source(
        &files[0],
        "package x\ntype Runner interface { Run() error; helper() }\ntype S struct{}\nfunc New() *S { return &S{} }\n",
    );
    let b = parse_source(
        &files[1],
        "package x\nfunc (s *S) Run() error { return nil }\nfunc main() { New(); S.Run(nil) }\n",
    );
    let out = link(&files, vec![a, b]);
    let b = &out["/r/b.go"];
    let cross = b
        .relationships
        .iter()
        .find(|r| r.annotations.get("cross_file") == Some(&json!(true)));
    assert_eq!(cross.map(|r| r.source_file.as_str()), Some("/r/a.go"));
    let new_call = b.relationships.iter().find(|r| r.target_symbol == "New");
    assert_eq!(
        new_call.and_then(|r| r.target_file.as_deref()),
        Some("/r/a.go")
    );
    let method_call = b.relationships.iter().find(|r| r.target_symbol == "S.Run");
    assert_eq!(
        method_call.and_then(|r| r.target_file.as_deref()),
        Some("/r/b.go")
    );
    // `helper` is unexported, so `S` implements `Runner` with only `Run`.
    let a = &out["/r/a.go"];
    let implementations = rels(a, RelationshipType::Implementation);
    assert_eq!(
        implementations,
        [pair("S", "Runner"), pair("S.Run", "Runner.Run")]
    );
    assert!(
        a.relationships
            .iter()
            .filter(|r| r.relationship_type == RelationshipType::Implementation)
            .all(|r| (r.confidence - 0.9).abs() < f64::EPSILON)
    );
}

#[test]
fn strip_pointer_slice_matches_python() {
    assert_eq!(strip_pointer_slice("*[]*pkg.T"), "pkg.T");
    assert_eq!(strip_pointer_slice("[4]T"), "T");
    assert_eq!(strip_pointer_slice("map[string][]*V"), "[]*V");
    assert_eq!(strip_pointer_slice("chan int"), "chan int");
}

#[test]
fn parsing_is_deterministic() {
    let dir = std::env::temp_dir().join(format!("dwgo-determinism-{}", std::process::id()));
    std::fs::create_dir_all(&dir).ok();
    let mut files = Vec::new();
    for index in 0..24 {
        let path = dir.join(format!("f{index:02}.go"));
        let source = format!(
            "package x\ntype I{index} interface {{ Run() }}\ntype Config struct{{}}\nfunc (c *Config) Run() {{ New{index}() }}\nfunc New{index}() {{}}\n"
        );
        std::fs::write(&path, source).ok();
        files.push(path.to_string_lossy().into_owned());
    }
    files.sort();
    let first = GoParser.parse_files(&files);
    let second = GoParser.parse_files(&files);
    std::fs::remove_dir_all(&dir).ok();
    assert_eq!(first.len(), 24);
    assert_eq!(
        serde_json::to_string(&first).ok(),
        serde_json::to_string(&second).ok()
    );
}
