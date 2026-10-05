//! Helpers both C++ visitors share: tree navigation, the Python parser's
//! name tables and its type-string decomposition.

use super::source::Source;
use crate::graph::pystr::strip;
use tree_sitter::Node;

/// `node.children`: every child, named or not, in order.
pub(super) fn children(node: Node<'_>) -> Vec<Node<'_>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).collect()
}

/// `_find_child_by_type(node, kind)`: the first child of that kind.
pub(super) fn find<'t>(node: Node<'t>, kind: &str) -> Option<Node<'t>> {
    let mut cursor = node.walk();
    node.children(&mut cursor).find(|c| c.kind() == kind)
}

/// The first child whose kind is one of `kinds`.
pub(super) fn find_any<'t>(node: Node<'t>, kinds: &[&str]) -> Option<Node<'t>> {
    let mut cursor = node.walk();
    node.children(&mut cursor)
        .find(|c| kinds.contains(&c.kind()))
}

/// The type kinds a return, constant or parameter type is read from.
pub(super) const TYPE_KINDS: &[&str] = &[
    "primitive_type",
    "type_identifier",
    "qualified_identifier",
    "template_type",
    "sized_type_specifier",
    "placeholder_type_specifier",
];

/// `PRIMITIVE_TYPES` of `_extract_user_defined_types_from_field`.
const PRIMITIVE_TYPES: &[&str] = &[
    "int",
    "float",
    "double",
    "char",
    "bool",
    "void",
    "short",
    "long",
    "unsigned",
    "signed",
    "unsigned char",
    "unsigned int",
    "unsigned short",
    "unsigned long",
    "unsigned long long",
    "signed char",
    "signed int",
    "long long",
    "long int",
    "long double",
    "short int",
    "int8_t",
    "int16_t",
    "int32_t",
    "int64_t",
    "uint8_t",
    "uint16_t",
    "uint32_t",
    "uint64_t",
    "size_t",
    "ptrdiff_t",
    "ssize_t",
    "string",
    "wstring",
    "auto",
    "decltype",
    "char16_t",
    "char32_t",
    "wchar_t",
];

/// `RelationshipExtractor.cpp_keywords`: never a `references` target.
const CPP_KEYWORDS: &[&str] = &[
    "auto",
    "break",
    "case",
    "char",
    "const",
    "continue",
    "default",
    "do",
    "double",
    "else",
    "enum",
    "extern",
    "float",
    "for",
    "goto",
    "if",
    "inline",
    "int",
    "long",
    "register",
    "return",
    "short",
    "signed",
    "sizeof",
    "static",
    "struct",
    "switch",
    "typedef",
    "union",
    "unsigned",
    "void",
    "volatile",
    "while",
    "class",
    "namespace",
    "template",
    "typename",
    "this",
    "public",
    "private",
    "protected",
    "virtual",
    "constexpr",
    "nullptr",
    "true",
    "false",
    "operator",
    "new",
    "delete",
    "throw",
    "try",
    "catch",
    "bool",
    "explicit",
    "friend",
    "mutable",
    "using",
    "static_cast",
    "dynamic_cast",
    "reinterpret_cast",
    "const_cast",
];

pub(super) fn is_keyword(name: &str) -> bool {
    CPP_KEYWORDS.contains(&name)
}

/// The 17 qualified standard containers a qualified TEMPLATE field or
/// return type is filtered against.
const STD_QUALIFIED_TEMPLATES: &[&str] = &[
    "std::string",
    "std::wstring",
    "std::vector",
    "std::list",
    "std::map",
    "std::set",
    "std::unordered_map",
    "std::unordered_set",
    "std::shared_ptr",
    "std::unique_ptr",
    "std::weak_ptr",
    "std::optional",
    "std::variant",
    "std::tuple",
    "std::pair",
    "std::function",
    "std::array",
];

/// The four the longer qualified list adds.
const STD_QUALIFIED_EXTRA: &[&str] = &[
    "std::deque",
    "std::queue",
    "std::stack",
    "std::priority_queue",
];

/// The unqualified standard containers.
const STD_UNQUALIFIED: &[&str] = &[
    "string",
    "wstring",
    "vector",
    "list",
    "map",
    "set",
    "unordered_map",
    "unordered_set",
    "shared_ptr",
    "unique_ptr",
    "weak_ptr",
    "optional",
    "variant",
    "tuple",
    "pair",
    "function",
    "array",
    "deque",
    "queue",
    "stack",
    "priority_queue",
];

/// The short std set (`visit_field_declaration`, a qualified template
/// return type).
pub(super) fn is_std_short(name: &str) -> bool {
    STD_QUALIFIED_TEMPLATES.contains(&name)
}

/// The 21 qualified names (a plain qualified return type).
pub(super) fn is_std_qualified(name: &str) -> bool {
    STD_QUALIFIED_TEMPLATES.contains(&name) || STD_QUALIFIED_EXTRA.contains(&name)
}

/// The 21 unqualified names (an unqualified template return type).
pub(super) fn is_std_unqualified(name: &str) -> bool {
    STD_UNQUALIFIED.contains(&name)
}

/// Both lists (a template argument).
pub(super) fn is_std_any(name: &str) -> bool {
    is_std_unqualified(name) || is_std_qualified(name)
}

/// `_collect_qualified_parts_from_node`: the parts of a (nested)
/// `qualified_identifier`, from the EXACT node text.
pub(super) fn qualified_parts_from_node(node: Node<'_>, source: &Source) -> Vec<String> {
    let mut parts = Vec::new();
    collect_parts(node, source, &mut parts);
    parts
}

fn collect_parts(node: Node<'_>, source: &Source, parts: &mut Vec<String>) {
    for child in children(node) {
        match child.kind() {
            "namespace_identifier"
            | "identifier"
            | "type_identifier"
            | "operator_name"
            | "template_type" => parts.push(source.raw(child).to_owned()),
            "template_function" => {
                if let Some(name) = find(child, "identifier") {
                    parts.push(source.raw(name).to_owned());
                }
            }
            "qualified_identifier" => collect_parts(child, source, parts),
            _ => {}
        }
    }
}

/// `_split_template_args`: split on top-level commas, `<>`-aware.
pub(super) fn split_template_args(args: &str) -> Vec<String> {
    let mut result = Vec::new();
    let mut depth = 0_i64;
    let mut current = String::new();
    for c in args.chars() {
        match c {
            '<' => {
                depth += 1;
                current.push(c);
            }
            '>' => {
                depth -= 1;
                current.push(c);
            }
            ',' if depth == 0 => {
                result.push(strip(&current).to_owned());
                current.clear();
            }
            _ => current.push(c),
        }
    }
    let tail = strip(&current);
    if !tail.is_empty() {
        result.push(tail.to_owned());
    }
    result
}

/// `_extract_user_defined_types_from_field`: the user-defined types a type
/// string names, each with whether it is held by pointer or reference.
///
/// Python quirks kept: `const` / `volatile` are removed as SUBSTRINGS
/// (`constexpr_t` loses its `const`), a template argument is always
/// "pointer or reference", and the template name is not namespace-stripped.
pub(super) fn user_defined_types(
    field_type: &str,
    template_params: &[String],
    depth: usize,
) -> Vec<(String, bool)> {
    if depth > 10 {
        return Vec::new();
    }
    let excluded =
        |name: &str| PRIMITIVE_TYPES.contains(&name) || template_params.iter().any(|p| p == name);
    let mut results = Vec::new();
    let is_pointer_or_ref = field_type.contains('*') || field_type.contains('&');
    let stripped = field_type
        .replace("const", "")
        .replace("volatile", "")
        .replace(['*', '&'], "");
    let mut base = strip(&stripped).to_owned();
    for prefix in ["enum ", "struct ", "class ", "union "] {
        if let Some(rest) = base.strip_prefix(prefix) {
            let rest = strip(rest).to_owned();
            base = rest;
            break;
        }
    }
    if let (Some(open), Some(close)) = (base.find('<'), base.rfind('>')) {
        let template_base = strip(&base[..open]);
        if !template_base.is_empty() && !excluded(template_base) {
            results.push((template_base.to_owned(), false));
        }
        // Python's `base[start+1:end]` is empty when `>` precedes `<`.
        let args = base.get(open + 1..close).unwrap_or("");
        for arg in split_template_args(args) {
            let cleaned = arg.replace("const", "").replace(['*', '&'], "");
            let clean = strip(&cleaned);
            if clean.is_empty() || excluded(clean) {
                continue;
            }
            if clean.contains('<') && clean.contains('>') {
                results.extend(user_defined_types(clean, template_params, depth + 1));
            } else {
                results.push((clean.to_owned(), true));
            }
        }
    } else if !base.is_empty() && !excluded(&base) {
        results.push((base, is_pointer_or_ref));
    }
    results
}

/// `name[0].isupper()` for a non-empty name.
pub(super) fn starts_upper(name: &str) -> bool {
    name.chars().next().is_some_and(char::is_uppercase)
}

#[cfg(test)]
mod tests {
    use super::*;

    fn udt(t: &str) -> Vec<(String, bool)> {
        user_defined_types(t, &["T".to_owned()], 0)
    }

    fn pair(n: &str, p: bool) -> (String, bool) {
        (n.to_owned(), p)
    }

    #[test]
    fn type_strings_decompose_as_python_does() {
        assert_eq!(udt("Point"), [pair("Point", false)]);
        assert_eq!(udt("const Point&"), [pair("Point", true)]);
        assert_eq!(udt("int"), []);
        assert_eq!(udt("T*"), []);
        assert_eq!(
            udt("std::map<string, std::vector<Foo*>>"),
            [
                pair("std::map", false),
                pair("std::vector", false),
                pair("Foo", true)
            ]
        );
        assert_eq!(udt("enum Color"), [pair("Color", false)]);
        assert_eq!(udt("constexpr_t"), [pair("expr_t", false)]);
        assert_eq!(udt("a>b<c"), [pair("a>b", false)]);
    }

    #[test]
    fn template_arguments_split_at_top_level_commas() {
        assert_eq!(
            split_template_args(" a, b<c, d> ,e "),
            ["a", "b<c, d>", "e"]
        );
        assert_eq!(split_template_args("a,"), ["a"]);
        assert_eq!(split_template_args(",a"), ["", "a"]);
    }
}
