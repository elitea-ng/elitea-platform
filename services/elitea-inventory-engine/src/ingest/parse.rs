//! The parser stage (ADR-0027 P3c): code files → entities and relations,
//! without a model.
//!
//! What the Python pipeline did with a code file (`_symbol_to_entity`,
//! `_parser_relationship_to_dict`, the containment edges of
//! `_process_single_document`, and the relations phase that resolves them
//! by name over the whole graph) is carried over. What parses the file is
//! not: the Python engine ran `ast` / regex parsers; here the shared
//! tree-sitter parsers (`elitea-code-parsers`, the `DeepWiki` engine's) parse
//! Python, JavaScript, TypeScript, Java, C#, Go and Rust, and ports of the
//! Inventory regex parsers parse Kotlin and Swift. The symbols found differ
//! from the Python parsers' (usually more precise); the graph built from
//! them follows the Python rules exactly.
//!
//! Choices where the shared parsers' model is wider than the Python one:
//!
//! * symbol types the Python map lacked keep a name instead of `unknown`:
//!   `struct`, `trait`, `macro`, `annotation`; a constructor is a `method`;
//! * parameters and variables local to a function or block are not
//!   entities — the Python parsers reported only declarations a reader
//!   looks for — and a relation TO one is dropped: relations resolve by
//!   name over the whole graph, so it would land on an unrelated entity;
//! * a `references` relation beside a more specific one for the same pair
//!   (the shared parsers report a call as both) is dropped, or the graph's
//!   one edge per pair would end up the vaguer of the two;
//! * every edge a file contributes records the file it was found in
//!   (`discovered_in_file`), so an incremental run removes it with the
//!   file. Python left it empty on the file and containment edges.

use super::files::EntityCounts;
use super::ids::entity_id;
use crate::graph::{Citation, Graph};
use elitea_code_parsers::model::{ParseResult, RelationshipType, Scope, Symbol, SymbolType};
use serde_json::{Map, Value, json};
use std::collections::{BTreeMap, HashMap};
use std::path::Path;

/// The parser language of a path, for the extensions the Python pipeline
/// parsed (`get_parser_for_file` ∧ `_is_code_file`). `.pyi`, `.pyx` and
/// `.kts` are listed there too, but the SDK loader never selected them.
#[must_use]
pub fn language_of(path: &str) -> Option<&'static str> {
    let extension = elitea_engine_core::pystr::suffix(path).to_lowercase();
    Some(match extension.as_str() {
        ".py" | ".pyi" | ".pyx" => "python",
        ".js" | ".jsx" | ".mjs" | ".cjs" => "javascript",
        ".ts" | ".tsx" => "typescript",
        ".java" => "java",
        ".kt" | ".kts" => "kotlin",
        ".cs" => "csharp",
        ".rs" => "rust",
        ".swift" => "swift",
        ".go" => "go",
        _ => return None,
    })
}

/// `SYMBOL_TYPE_TO_ENTITY_TYPE`, widened (module docs). `None`: not an
/// entity.
fn entity_type(symbol: &Symbol) -> Option<&'static str> {
    // `scope` is the symbol's own kind of scope for a function or method,
    // and where it lives for a variable: a variable in a function or block
    // is a local.
    let local = matches!(
        symbol.symbol_type,
        SymbolType::Variable | SymbolType::Constant | SymbolType::Field | SymbolType::Property
    ) && matches!(symbol.scope, Scope::Function | Scope::Block | Scope::Local);
    if symbol.symbol_type == SymbolType::Parameter || local {
        return None;
    }
    Some(match symbol.symbol_type {
        SymbolType::Class => "class",
        SymbolType::Function => "function",
        SymbolType::Method | SymbolType::Constructor => "method",
        SymbolType::Module => "module",
        SymbolType::Interface => "interface",
        SymbolType::Constant => "constant",
        SymbolType::Variable => "variable",
        SymbolType::Property => "property",
        SymbolType::Field => "field",
        SymbolType::Enum => "enum",
        SymbolType::TypeAlias => "type_alias",
        SymbolType::Decorator => "decorator",
        SymbolType::Namespace => "namespace",
        SymbolType::Struct => "struct",
        SymbolType::Trait => "trait",
        SymbolType::Macro => "macro",
        SymbolType::Annotation => "annotation",
        SymbolType::Parameter => "parameter",
        _ => "unknown",
    })
}

/// `PARSER_REL_TYPE_TO_STRING`; anything else is `references`.
fn relation_type(kind: RelationshipType) -> &'static str {
    match kind {
        RelationshipType::Imports => "imports",
        RelationshipType::Exports => "exports",
        RelationshipType::Calls => "calls",
        RelationshipType::Returns => "returns",
        RelationshipType::Inheritance => "extends",
        RelationshipType::Implementation => "implements",
        RelationshipType::Composition | RelationshipType::Contains => "contains",
        RelationshipType::Aggregation => "uses",
        RelationshipType::Defines => "defines",
        RelationshipType::Decorates => "decorates",
        RelationshipType::Annotates => "annotates",
        _ => "references",
    }
}

/// One entity a file contributes.
#[derive(Debug, Clone, PartialEq)]
pub struct ParsedEntity {
    pub id: String,
    pub name: String,
    pub entity_type: String,
    pub citation: Citation,
    pub properties: Map<String, Value>,
}

/// One relation a file contributes, resolved when every file is in the
/// graph: by id where the file already knows both ends, else by name.
#[derive(Debug, Clone, PartialEq)]
pub struct PendingRelation {
    pub source_id: Option<String>,
    pub target_id: Option<String>,
    pub source_symbol: String,
    pub target_symbol: String,
    pub relation_type: String,
    pub discovered_in_file: String,
    pub confidence: f64,
    pub is_cross_file: bool,
}

/// What parsing one file gave.
#[derive(Debug, Clone, Default, PartialEq)]
pub struct FileExtraction {
    pub entities: Vec<ParsedEntity>,
    pub relations: Vec<PendingRelation>,
    /// The parse failed (the file still gets its file node).
    pub error: Option<String>,
}

/// `_symbol_to_entity`.
fn symbol_entity(
    symbol: &Symbol,
    kind: &str,
    path: &str,
    source_toolkit: &str,
    content_hash: &str,
) -> ParsedEntity {
    let exported = symbol
        .metadata
        .get("is_exported")
        .and_then(Value::as_bool)
        .unwrap_or_else(|| symbol.visibility.as_deref() == Some("public"));
    let mut properties = Map::new();
    properties.insert(
        "description".to_owned(),
        json!(symbol.docstring.clone().unwrap_or_default()),
    );
    let optional = [
        ("parent_symbol", symbol.parent_symbol.clone()),
        ("full_name", Some(symbol.qualified_name())),
        ("visibility", symbol.visibility.clone()),
    ];
    for (key, value) in optional {
        if let Some(value) = value {
            properties.insert(key.to_owned(), json!(value));
        }
    }
    properties.insert("is_static".to_owned(), json!(symbol.is_static));
    properties.insert("is_async".to_owned(), json!(symbol.is_async));
    properties.insert("is_exported".to_owned(), json!(exported));
    for (key, value) in [
        ("signature", symbol.signature.clone()),
        ("return_type", symbol.return_type.clone()),
    ] {
        if let Some(value) = value {
            properties.insert(key.to_owned(), json!(value));
        }
    }
    for (key, value) in &symbol.metadata {
        if !value.is_null() {
            properties.insert(key.clone(), value.clone());
        }
    }
    ParsedEntity {
        id: entity_id(kind, &symbol.name, Some(path)),
        name: symbol.name.clone(),
        entity_type: kind.to_owned(),
        citation: Citation {
            file_path: path.to_owned(),
            line_start: Some(i64::from(symbol.range.start.line)),
            line_end: Some(i64::from(symbol.range.end.line)),
            source_toolkit: Some(source_toolkit.to_owned()),
            doc_id: Some(format!("{source_toolkit}://{path}")),
            content_hash: Some(content_hash.to_owned()),
        },
        properties,
    }
}

/// One file's parse → its entities and relations. `path` is the file's
/// path in the source (the one cited), `result` the parser's output.
#[must_use]
pub fn extract(
    path: &str,
    result: &ParseResult,
    source_toolkit: &str,
    content_hash: &str,
) -> FileExtraction {
    let mut extraction = FileExtraction {
        error: result.errors.first().cloned(),
        ..FileExtraction::default()
    };
    let mut by_qualified_name: HashMap<String, String> = HashMap::new();
    let mut kept: Vec<&Symbol> = Vec::new();
    for symbol in &result.symbols {
        let Some(kind) = entity_type(symbol) else {
            continue;
        };
        let entity = symbol_entity(symbol, kind, path, source_toolkit, content_hash);
        by_qualified_name.insert(symbol.qualified_name(), entity.id.clone());
        extraction.entities.push(entity);
        kept.push(symbol);
    }
    // A reference to a parameter or a local resolves by NAME over the whole
    // graph, so it would land on an unrelated entity of that name; and a
    // `references` edge beside a more specific one for the same pair would
    // replace it (one edge per pair). Both are left out.
    let kept_names: std::collections::HashSet<&str> =
        kept.iter().map(|symbol| symbol.name.as_str()).collect();
    let locals: std::collections::HashSet<&str> = result
        .symbols
        .iter()
        .filter(|symbol| {
            entity_type(symbol).is_none() && !kept_names.contains(symbol.name.as_str())
        })
        .map(|symbol| symbol.name.as_str())
        .collect();
    let specific: std::collections::HashSet<(&str, &str)> = result
        .relationships
        .iter()
        .filter(|r| relation_type(r.relationship_type) != "references")
        .map(|r| (r.source_symbol.as_str(), r.target_symbol.as_str()))
        .collect();
    for relationship in &result.relationships {
        let generic = relation_type(relationship.relationship_type) == "references";
        if locals.contains(relationship.target_symbol.as_str())
            || (generic
                && specific.contains(&(
                    relationship.source_symbol.as_str(),
                    relationship.target_symbol.as_str(),
                )))
        {
            continue;
        }
        extraction.relations.push(PendingRelation {
            source_id: None,
            target_id: None,
            source_symbol: relationship.source_symbol.clone(),
            target_symbol: relationship.target_symbol.clone(),
            relation_type: relation_type(relationship.relationship_type).to_owned(),
            discovered_in_file: path.to_owned(),
            confidence: relationship.confidence,
            is_cross_file: relationship
                .target_file
                .as_deref()
                .is_some_and(|target| target != relationship.source_file),
        });
    }
    // Containment from `parent_symbol`, by the qualified names this file
    // declared.
    for symbol in kept {
        let Some(parent) = symbol.parent_symbol.as_deref() else {
            continue;
        };
        if let (Some(child_id), Some(parent_id)) = (
            by_qualified_name.get(&symbol.qualified_name()),
            by_qualified_name.get(parent),
        ) {
            extraction.relations.push(PendingRelation {
                source_id: Some(parent_id.clone()),
                target_id: Some(child_id.clone()),
                source_symbol: String::new(),
                target_symbol: String::new(),
                relation_type: "contains".to_owned(),
                discovered_in_file: path.to_owned(),
                confidence: 1.0,
                is_cross_file: false,
            });
        }
    }
    extraction
}

/// The counts the file node's properties carry.
#[must_use]
pub fn counts(entities: &[ParsedEntity]) -> EntityCounts {
    EntityCounts {
        entities: entities.len(),
        code: entities
            .iter()
            .filter(|e| {
                matches!(
                    e.entity_type.as_str(),
                    "class" | "function" | "method" | "module" | "interface"
                )
            })
            .count(),
        facts: entities.iter().filter(|e| e.entity_type == "fact").count(),
    }
}

/// The edges from a file node to what the file declares: `implements` for
/// a feature, requirement, user story, epic or capability, else `contains`.
#[must_use]
pub fn file_edges(file_id: &str, path: &str, entities: &[ParsedEntity]) -> Vec<PendingRelation> {
    entities
        .iter()
        .map(|entity| PendingRelation {
            source_id: Some(file_id.to_owned()),
            target_id: Some(entity.id.clone()),
            source_symbol: String::new(),
            target_symbol: String::new(),
            relation_type: if matches!(
                entity.entity_type.to_lowercase().as_str(),
                "feature" | "requirement" | "user_story" | "epic" | "capability"
            ) {
                "implements"
            } else {
                "contains"
            }
            .to_owned(),
            discovered_in_file: path.to_owned(),
            confidence: 1.0,
            is_cross_file: false,
        })
        .collect()
}

/// `_ENTITY_TYPE_PRIORITY`.
fn priority(entity_type: &str) -> i32 {
    match entity_type.to_lowercase().as_str() {
        "class" | "interface" | "struct" | "enum" | "trait" => 10,
        "function" | "method" => 9,
        "module" | "component" | "service" => 8,
        "constant" => 7,
        "variable" | "property" => 6,
        "feature" | "requirement" | "test" | "rule" | "workflow" => 5,
        "source_file" | "document_file" | "config_file" => 4,
        "fact" => 2,
        "import" => 1,
        _ => 3,
    }
}

/// `_build_entity_by_name_ids` over every node: lowercase name,
/// `type:name` and lowercase full name → id, the highest-priority type
/// winning (a later node of equal priority replaces an earlier one).
#[must_use]
pub fn name_index(graph: &Graph) -> HashMap<String, String> {
    let mut index = HashMap::new();
    let mut ranks: HashMap<String, i32> = HashMap::new();
    for (id, node) in graph.nodes() {
        let Some(name) = node
            .get("name")
            .and_then(Value::as_str)
            .filter(|n| !n.is_empty())
        else {
            continue;
        };
        if id.is_empty() {
            continue;
        }
        let name_lower = name.to_lowercase();
        let kind = node
            .get("type")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .to_lowercase();
        let rank = priority(&kind);
        index.insert(format!("{kind}:{name_lower}"), id.to_owned());
        if rank >= *ranks.get(&name_lower).unwrap_or(&-1) {
            index.insert(name_lower.clone(), id.to_owned());
            ranks.insert(name_lower, rank);
        }
        let full_name = node
            .get("properties")
            .and_then(|p| p.get("full_name"))
            .and_then(Value::as_str)
            .filter(|f| !f.is_empty())
            .or_else(|| node.get("full_name").and_then(Value::as_str));
        if let Some(full) = full_name.filter(|f| !f.is_empty() && *f != name) {
            let full_lower = full.to_lowercase();
            if rank >= *ranks.get(&full_lower).unwrap_or(&-1) {
                index.insert(full_lower.clone(), id.to_owned());
                ranks.insert(full_lower, rank);
            }
        }
    }
    index
}

/// The relations phase: resolve each relation and add it, marked as
/// found by `origin` (`parser`, `llm`).
/// Returns how many were added.
pub fn add_relations(
    graph: &mut Graph,
    relations: &[PendingRelation],
    source_toolkit: &str,
    origin: &str,
) -> usize {
    let index = name_index(graph);
    let mut added = 0;
    for relation in relations {
        let source_name = relation.source_symbol.to_lowercase();
        let target_name = relation.target_symbol.to_lowercase();
        let lookup = |key: &str| index.get(key).cloned();
        let (source_id, target_id) =
            if matches!(relation.relation_type.as_str(), "extends" | "implements") {
                (
                    relation.source_id.clone().or_else(|| {
                        lookup(&format!("class:{source_name}")).or_else(|| lookup(&source_name))
                    }),
                    relation.target_id.clone().or_else(|| {
                        lookup(&format!("class:{target_name}"))
                            .or_else(|| lookup(&format!("interface:{target_name}")))
                            .or_else(|| lookup(&target_name))
                    }),
                )
            } else {
                (
                    relation.source_id.clone().or_else(|| lookup(&source_name)),
                    relation.target_id.clone().or_else(|| lookup(&target_name)),
                )
            };
        let (Some(source_id), Some(target_id)) = (source_id, target_id) else {
            continue;
        };
        let mut properties = Map::new();
        properties.insert("source_toolkit".to_owned(), json!(source_toolkit));
        properties.insert("confidence".to_owned(), json!(relation.confidence));
        properties.insert("source".to_owned(), json!(origin));
        properties.insert(
            "discovered_in_file".to_owned(),
            json!(relation.discovered_in_file),
        );
        if relation.is_cross_file {
            properties.insert("is_cross_file".to_owned(), json!(true));
        }
        if graph.add_relation(
            &source_id,
            &target_id,
            &relation.relation_type,
            Some(&properties),
        ) {
            added += 1;
        }
    }
    added
}

/// Parse `files` (paths relative to `root`) with every parser that has
/// some of them, one language at a time. Returns each file's parse by its
/// relative path; a file of a language without a parser is absent.
#[must_use]
pub fn parse_tree(root: &Path, files: &[&str]) -> BTreeMap<String, ParseResult> {
    let mut by_language: BTreeMap<&str, Vec<&str>> = BTreeMap::new();
    for path in files {
        if let Some(language) = language_of(path) {
            by_language.entry(language).or_default().push(path);
        }
    }
    let prefix = format!("{}/", root.display());
    let mut parsed = BTreeMap::new();
    for (language, paths) in by_language {
        let Some(parser) = elitea_code_parsers::parser_for(language) else {
            continue;
        };
        let absolute: Vec<String> = paths
            .iter()
            .map(|path| root.join(path).display().to_string())
            .collect();
        for (absolute_path, result) in parser.parse_files(&absolute) {
            let relative = absolute_path
                .strip_prefix(&prefix)
                .unwrap_or(&absolute_path)
                .to_owned();
            parsed.insert(relative, result);
        }
    }
    parsed
}

#[cfg(test)]
mod tests {
    use super::*;
    use elitea_code_parsers::model::{Range, Relationship};

    fn symbol(name: &str, kind: SymbolType, scope: Scope, parent: Option<&str>) -> Symbol {
        let mut symbol = Symbol::new(name, kind, scope, Range::new(3, 0, 9, 1), "/abs/src/a.py");
        symbol.parent_symbol = parent.map(str::to_owned);
        symbol
    }

    #[test]
    fn languages_follow_the_python_routing() {
        assert_eq!(language_of("a/b.py"), Some("python"));
        assert_eq!(language_of("a/b.TSX"), Some("typescript"));
        assert_eq!(language_of("a/b.mjs"), Some("javascript"));
        assert_eq!(language_of("a/b.kt"), Some("kotlin"));
        assert_eq!(
            language_of("a/b.cpp"),
            None,
            "C++ was not parsed by Inventory"
        );
        assert_eq!(language_of("a/b.md"), None);
    }

    #[test]
    fn symbols_become_entities_with_containment() {
        let mut result = ParseResult::new("/abs/src/a.py", "python");
        result
            .symbols
            .push(symbol("Users", SymbolType::Class, Scope::Global, None));
        let mut method = symbol("create", SymbolType::Method, Scope::Class, Some("Users"));
        method.is_async = true;
        method.docstring = Some("Create one.".to_owned());
        result.symbols.push(method);
        result.symbols.push(symbol(
            "name",
            SymbolType::Parameter,
            Scope::Function,
            Some("Users.create"),
        ));
        result.symbols.push(symbol(
            "tmp",
            SymbolType::Variable,
            Scope::Function,
            Some("Users.create"),
        ));
        result.relationships.push(Relationship::new(
            "Users",
            "Base",
            RelationshipType::Inheritance,
            "/abs/src/a.py",
        ));
        let extraction = extract("src/a.py", &result, "repo", "h");
        let names: Vec<&str> = extraction
            .entities
            .iter()
            .map(|e| e.name.as_str())
            .collect();
        assert_eq!(names, ["Users", "create"], "no parameters, no locals");
        let create = &extraction.entities[1];
        assert_eq!(create.entity_type, "method");
        assert_eq!(create.properties["full_name"], json!("Users.create"));
        assert_eq!(create.properties["description"], json!("Create one."));
        assert_eq!(create.properties["is_async"], json!(true));
        assert_eq!(create.citation.line_start, Some(3));
        assert_eq!(create.citation.content_hash.as_deref(), Some("h"));
        assert_eq!(create.id, entity_id("method", "create", Some("src/a.py")));
        let kinds: Vec<&str> = extraction
            .relations
            .iter()
            .map(|r| r.relation_type.as_str())
            .collect();
        assert_eq!(kinds, ["extends", "contains"]);
        assert_eq!(
            extraction.relations[1].source_id.as_deref(),
            Some(extraction.entities[0].id.as_str())
        );
    }

    #[test]
    fn names_resolve_by_type_priority() {
        let mut graph = Graph::new();
        graph.add_entity("imp", "Users", "import", None, None);
        graph.add_entity("cls", "Users", "class", None, None);
        graph.add_entity("fact", "users", "fact", None, None);
        let full: Map<String, Value> = [("full_name".to_owned(), json!("app.Users"))]
            .into_iter()
            .collect();
        graph.add_entity("mod", "Users2", "module", None, Some(&full));
        let index = name_index(&graph);
        assert_eq!(index.get("users").map(String::as_str), Some("cls"));
        assert_eq!(index.get("import:users").map(String::as_str), Some("imp"));
        assert_eq!(index.get("app.users").map(String::as_str), Some("mod"));

        graph.add_entity("base", "Base", "interface", None, None);
        let pending = [PendingRelation {
            source_id: None,
            target_id: None,
            source_symbol: "users".to_owned(),
            target_symbol: "BASE".to_owned(),
            relation_type: "extends".to_owned(),
            discovered_in_file: "a.py".to_owned(),
            confidence: 1.0,
            is_cross_file: true,
        }];
        assert_eq!(add_relations(&mut graph, &pending, "repo", "parser"), 1);
        let (source, target, edge) = graph.edges().next().unwrap_or_else(|| panic!("an edge"));
        assert_eq!((source, target), ("cls", "base"));
        assert_eq!(edge["is_cross_file"], json!(true));
        assert_eq!(edge["discovered_in_file"], json!("a.py"));
    }
}
