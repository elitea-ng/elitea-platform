//! What a language parser extracts from a file.
//!
//! A transcription of the Python engine's `parsers/base_parser.py`: the
//! symbol and relationship vocabularies are the graph's `symbol_type` and
//! `rel_type` values, so every string here is a wire value and must not be
//! renamed. Each parser fills these; the graph builder turns them into
//! nodes and edges.

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

macro_rules! wire_enum {
    ($(#[$meta:meta])* $name:ident { $($variant:ident = $wire:literal),+ $(,)? }) => {
        $(#[$meta])*
        #[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, PartialOrd, Ord, Serialize, Deserialize)]
        pub enum $name {
            $(#[serde(rename = $wire)] $variant),+
        }

        impl $name {
            /// Every value, in declaration order.
            pub const ALL: &'static [Self] = &[$(Self::$variant),+];

            /// The value written into the graph.
            #[must_use]
            pub fn as_str(self) -> &'static str {
                match self { $(Self::$variant => $wire),+ }
            }

            /// The value for a wire string.
            #[must_use]
            pub fn parse(text: &str) -> Option<Self> {
                match text { $($wire => Some(Self::$variant),)+ _ => None }
            }
        }

        impl std::fmt::Display for $name {
            fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
                f.write_str(self.as_str())
            }
        }
    };
}

wire_enum! {
    /// `RelationshipType` — the `rel_type` of a structural edge.
    RelationshipType {
        Imports = "imports",
        Calls = "calls",
        Inheritance = "inheritance",
        Implementation = "implementation",
        Composition = "composition",
        Aggregation = "aggregation",
        Defines = "defines",
        Contains = "contains",
        Decorates = "decorates",
        Annotates = "annotates",
        Assigns = "assigns",
        Returns = "returns",
        Parameter = "parameter",
        Exports = "exports",
        References = "references",
        Specializes = "specializes",
        Instantiates = "instantiates",
        Declares = "declares",
        DefinesBody = "defines_body",
        Overrides = "overrides",
        Hides = "hides",
        UsesType = "uses_type",
        Creates = "creates",
        Reads = "reads",
        Writes = "writes",
        Captures = "captures",
        AliasOf = "alias_of",
        Consumes = "consumes",
        TriggeredBy = "triggered_by",
    }
}

wire_enum! {
    /// `SymbolType` — the `symbol_type` of a code node.
    SymbolType {
        Function = "function",
        Method = "method",
        Constructor = "constructor",
        Class = "class",
        Interface = "interface",
        Variable = "variable",
        Constant = "constant",
        Property = "property",
        Field = "field",
        Parameter = "parameter",
        Module = "module",
        Namespace = "namespace",
        Enum = "enum",
        Struct = "struct",
        Trait = "trait",
        TypeAlias = "type_alias",
        Annotation = "annotation",
        Decorator = "decorator",
        Macro = "macro",
        Contract = "contract",
        SqlSchema = "sql_schema",
        SqlTable = "sql_table",
        SqlView = "sql_view",
        SqlColumn = "sql_column",
        SqlIndex = "sql_index",
        SqlFunction = "sql_function",
        SqlTrigger = "sql_trigger",
    }
}

wire_enum! {
    /// `Scope` — the unified scoping model.
    Scope {
        Global = "global",
        Class = "class",
        Function = "function",
        Block = "block",
        Local = "local",
    }
}

/// Deserialise `null` as the type's default (an empty map).
fn null_as_default<'de, D, T>(deserializer: D) -> Result<T, D::Error>
where
    D: serde::Deserializer<'de>,
    T: Default + Deserialize<'de>,
{
    Option::<T>::deserialize(deserializer).map(Option::unwrap_or_default)
}

/// A 1-based line and 0-based column, as the Python parsers record them.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default, Serialize, Deserialize)]
pub struct Position {
    pub line: u32,
    pub column: u32,
}

/// A source range.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Hash, Default, Serialize, Deserialize)]
pub struct Range {
    pub start: Position,
    pub end: Position,
}

impl Range {
    #[must_use]
    pub fn new(start_line: u32, start_column: u32, end_line: u32, end_column: u32) -> Self {
        Self {
            start: Position {
                line: start_line,
                column: start_column,
            },
            end: Position {
                line: end_line,
                column: end_column,
            },
        }
    }

    /// Python's `str(Range)`: `"l:c-l:c"`.
    #[must_use]
    pub fn key(&self) -> String {
        format!(
            "{}:{}-{}:{}",
            self.start.line, self.start.column, self.end.line, self.end.column
        )
    }
}

/// One extracted symbol (`Symbol`).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Symbol {
    pub name: String,
    pub symbol_type: SymbolType,
    pub scope: Scope,
    pub range: Range,
    pub file_path: String,
    pub parent_symbol: Option<String>,
    pub full_name: Option<String>,
    pub visibility: Option<String>,
    pub is_static: bool,
    pub is_abstract: bool,
    pub is_async: bool,
    pub docstring: Option<String>,
    pub comments: Vec<String>,
    pub return_type: Option<String>,
    pub parameter_types: Vec<String>,
    pub source_text: Option<String>,
    pub signature: Option<String>,
    /// Free-form, language-specific. Keys are the Python parsers' keys.
    /// A Python `None` (the C# parser passes it for "no metadata") reads as
    /// empty: the graph builder treats both alike.
    #[serde(default, deserialize_with = "null_as_default")]
    pub metadata: Map<String, Value>,
}

impl Symbol {
    /// A symbol with every optional field empty.
    #[must_use]
    pub fn new(
        name: impl Into<String>,
        symbol_type: SymbolType,
        scope: Scope,
        range: Range,
        file_path: impl Into<String>,
    ) -> Self {
        Self {
            name: name.into(),
            symbol_type,
            scope,
            range,
            file_path: file_path.into(),
            parent_symbol: None,
            full_name: None,
            visibility: None,
            is_static: false,
            is_abstract: false,
            is_async: false,
            docstring: None,
            comments: Vec::new(),
            return_type: None,
            parameter_types: Vec::new(),
            source_text: None,
            signature: None,
            metadata: Map::new(),
        }
    }

    /// `get_qualified_name()`: the full name, else `parent.name`, else `name`.
    #[must_use]
    pub fn qualified_name(&self) -> String {
        if let Some(full) = self.full_name.as_ref().filter(|f| !f.is_empty()) {
            return full.clone();
        }
        match self.parent_symbol.as_ref().filter(|p| !p.is_empty()) {
            Some(parent) => format!("{parent}.{}", self.name),
            None => self.name.clone(),
        }
    }
}

/// One extracted relationship (`Relationship`).
#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
pub struct Relationship {
    pub source_symbol: String,
    pub target_symbol: String,
    pub relationship_type: RelationshipType,
    pub source_file: String,
    pub target_file: Option<String>,
    pub source_range: Option<Range>,
    pub confidence: f64,
    pub weight: f64,
    pub is_direct: bool,
    pub context: Option<String>,
    /// A Python `None` (C# and C++ pass it) reads as empty, as for
    /// [`Symbol::metadata`].
    #[serde(default, deserialize_with = "null_as_default")]
    pub annotations: Map<String, Value>,
}

impl Relationship {
    /// A relationship with the Python defaults (confidence and weight 1).
    #[must_use]
    pub fn new(
        source_symbol: impl Into<String>,
        target_symbol: impl Into<String>,
        relationship_type: RelationshipType,
        source_file: impl Into<String>,
    ) -> Self {
        Self {
            source_symbol: source_symbol.into(),
            target_symbol: target_symbol.into(),
            relationship_type,
            source_file: source_file.into(),
            target_file: None,
            source_range: None,
            confidence: 1.0,
            weight: 1.0,
            is_direct: true,
            context: None,
            annotations: Map::new(),
        }
    }

    /// `get_key()`: source, target, type and the source position.
    #[must_use]
    pub fn key(&self) -> String {
        let (line, column) = self
            .source_range
            .map_or((0, 0), |r| (r.start.line, r.start.column));
        format!(
            "{}->{}:{}@{line}:{column}",
            self.source_symbol, self.target_symbol, self.relationship_type
        )
    }
}

/// What parsing one file produced (`ParseResult`).
#[derive(Debug, Clone, PartialEq, Default, Serialize, Deserialize)]
pub struct ParseResult {
    pub file_path: String,
    pub language: String,
    pub symbols: Vec<Symbol>,
    pub relationships: Vec<Relationship>,
    pub imports: Vec<String>,
    pub exports: Vec<String>,
    /// A set in Python; kept sorted here so output is deterministic.
    pub dependencies: Vec<String>,
    pub module_docstring: Option<String>,
    pub errors: Vec<String>,
    /// Not in the reference dumps (`python_parse_dump.py` drops it).
    #[serde(default)]
    pub warnings: Vec<String>,
}

impl ParseResult {
    #[must_use]
    pub fn new(file_path: impl Into<String>, language: impl Into<String>) -> Self {
        Self {
            file_path: file_path.into(),
            language: language.into(),
            ..Self::default()
        }
    }

    /// `BaseParser.validate_result`: drop duplicate symbols and
    /// relationships (first wins), then drop relationships whose source AND
    /// target are both unknown to this file (a file-stem source counts as
    /// known), recording each as a warning.
    pub fn validate(&mut self) {
        let mut seen = std::collections::HashSet::new();
        self.symbols
            .retain(|s| seen.insert(format!("{}:{}:{}", s.name, s.symbol_type, s.range.key())));
        let mut seen = std::collections::HashSet::new();
        self.relationships.retain(|r| seen.insert(r.key()));
        let mut known: std::collections::HashSet<&str> =
            self.symbols.iter().map(|s| s.name.as_str()).collect();
        known.extend(self.symbols.iter().filter_map(|s| s.full_name.as_deref()));
        let stem = std::path::Path::new(&self.file_path)
            .file_stem()
            .and_then(|s| s.to_str())
            .map(str::to_owned);
        let mut kept = Vec::with_capacity(self.relationships.len());
        let mut warnings = Vec::new();
        for relationship in self.relationships.drain(..) {
            let source_ok = known.contains(relationship.source_symbol.as_str())
                || stem.as_deref() == Some(relationship.source_symbol.as_str());
            let target_ok = known.contains(relationship.target_symbol.as_str());
            if source_ok || target_ok {
                kept.push(relationship);
            } else {
                warnings.push(format!("Orphaned relationship: {}", relationship.key()));
            }
        }
        self.relationships = kept;
        self.warnings.extend(warnings);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn the_wire_values_round_trip() {
        for value in RelationshipType::ALL {
            assert_eq!(RelationshipType::parse(value.as_str()), Some(*value));
        }
        for value in SymbolType::ALL {
            assert_eq!(SymbolType::parse(value.as_str()), Some(*value));
        }
        assert_eq!(RelationshipType::DefinesBody.as_str(), "defines_body");
        assert_eq!(
            serde_json::to_value(SymbolType::TypeAlias).ok(),
            Some(Value::String("type_alias".to_owned()))
        );
    }

    #[test]
    fn a_python_none_map_reads_as_empty() {
        let symbol: Result<Symbol, _> = serde_json::from_value(serde_json::json!({
            "name": "A", "symbol_type": "class", "scope": "class",
            "range": {"start": {"line": 1, "column": 0}, "end": {"line": 1, "column": 1}},
            "file_path": "a.cs", "parent_symbol": null, "full_name": null,
            "visibility": null, "is_static": false, "is_abstract": false,
            "is_async": false, "docstring": null, "comments": [], "return_type": null,
            "parameter_types": [], "source_text": null, "signature": null,
            "metadata": null
        }));
        assert!(symbol.is_ok_and(|s| s.metadata.is_empty()));
        let relationship: Result<Relationship, _> = serde_json::from_value(serde_json::json!({
            "source_symbol": "a", "target_symbol": "b", "relationship_type": "imports",
            "source_file": "a.cs", "target_file": null, "source_range": null,
            "confidence": 1.0, "weight": 1.0, "is_direct": true, "context": null,
            "annotations": null
        }));
        assert!(relationship.is_ok_and(|r| r.annotations.is_empty()));
    }

    #[test]
    fn validation_keeps_the_first_duplicate_and_drops_orphans() {
        let mut result = ParseResult::new("pkg/mod.py", "python");
        let range = Range::new(1, 0, 2, 0);
        result.symbols.push(Symbol::new(
            "A",
            SymbolType::Class,
            Scope::Global,
            range,
            "pkg/mod.py",
        ));
        result.symbols.push(Symbol::new(
            "A",
            SymbolType::Class,
            Scope::Global,
            range,
            "pkg/mod.py",
        ));
        result.relationships.push(Relationship::new(
            "A",
            "B",
            RelationshipType::Calls,
            "pkg/mod.py",
        ));
        result.relationships.push(Relationship::new(
            "A",
            "B",
            RelationshipType::Calls,
            "pkg/mod.py",
        ));
        result.relationships.push(Relationship::new(
            "mod",
            "X",
            RelationshipType::Imports,
            "pkg/mod.py",
        ));
        result.relationships.push(Relationship::new(
            "Y",
            "Z",
            RelationshipType::Calls,
            "pkg/mod.py",
        ));
        result.validate();
        assert_eq!(result.symbols.len(), 1);
        assert_eq!(result.relationships.len(), 2);
        assert_eq!(result.warnings, ["Orphaned relationship: Y->Z:calls@0:0"]);
    }
}
