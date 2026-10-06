//! Phase 1c: API surfaces and contract nodes (`api_surface_extractor.py`).
//!
//! Every node's source text goes through a set of regex matchers — REST
//! routes and clients, gRPC services and stubs, GraphQL roots, FFI exports,
//! data shapes (`obj:`), BDD steps, CLI commands, Pylon class APIs — that
//! yield canonical surface keys (`"POST /users"`, `"grpc:Cart/GetCart"`).
//! [`materialize_contract_nodes`] then turns each distinct `(kind,
//! surface)` into a `contract::<kind>::<surface>` node with a `defines`
//! (server side) or `consumes` (client side) edge from every node that
//! exposes it; the cross-language linker pairs implementors through it.
//!
//! The matchers are transcribed pattern by pattern, quirks included,
//! because the surfaces become node ids: the documentation chunks of
//! `.json`/`.yaml` files trip the FFI and CLI matchers, and those contract
//! nodes exist in the Python index too. Python's `re` syntax goes through
//! [`super::pyre`]; the two patterns `regex` cannot express (a
//! back-reference and a look-ahead) are matched by hand.
//!
//! Source text, as Python reads it: a node whose `source_text` attribute
//! is empty (every rich parser node keeps its text on the parser symbol
//! instead) gets the file slice `[start_line..end_line]` from disk; only
//! when that is empty too does the symbol's own text serve. A Python node
//! sees its file's `APIRouter(prefix=...)` line prepended. Files are read
//! through [`super::repo_files`] (inside the repository, no symbolic
//! links).

use super::pyre::compile;
use super::pystr;
use super::repo_files::TextCache;
use super::{CodeGraph, EdgeData, NodeData};
use indexmap::{IndexMap, IndexSet};
use regex::{Captures, Regex};
use serde_json::{Map, Value, json};
use std::collections::{BTreeSet, HashMap, HashSet};
use std::rc::Rc;
use std::sync::LazyLock;

/// One API surface (`APISurface`).
#[derive(Debug, Clone, PartialEq)]
pub struct ApiSurface {
    pub kind: String,
    pub surface: String,
    pub weight_hint: f64,
    /// Matcher-specific details, in Python's key order (`method`, `path`,
    /// `role`, `service`, `symbol`, ...).
    pub metadata: Map<String, Value>,
}

impl ApiSurface {
    fn new(kind: &str, surface: String, weight_hint: f64, metadata: Value) -> Self {
        let metadata = match metadata {
            Value::Object(map) => map,
            _ => Map::new(),
        };
        Self {
            kind: kind.to_owned(),
            surface,
            weight_hint,
            metadata,
        }
    }

    fn meta_str(&self, key: &str) -> &str {
        self.metadata.get(key).and_then(Value::as_str).unwrap_or("")
    }

    /// The surface as the `api_surface` node attribute holds it.
    #[must_use]
    pub fn to_json(&self) -> Value {
        json!({
            "kind": self.kind,
            "surface": self.surface,
            "weight_hint": self.weight_hint,
            "metadata": Value::Object(self.metadata.clone()),
        })
    }
}

/// Node id → surfaces, in node order (`surfaces_by_node`).
pub type SurfacesByNode = IndexMap<String, Vec<ApiSurface>>;

macro_rules! pattern {
    ($name:ident, $pattern:expr) => {
        pattern!($name, $pattern, "");
    };
    ($name:ident, $pattern:expr, $flags:expr) => {
        static $name: LazyLock<Regex> = LazyLock::new(|| compile($pattern, $flags));
    };
}

fn group<'t>(captures: &Captures<'t>, name: &str) -> &'t str {
    captures.name(name).map_or("", |m| m.as_str())
}

fn group_at<'t>(captures: &Captures<'t>, index: usize) -> &'t str {
    captures.get(index).map_or("", |m| m.as_str())
}

// ── REST ────────────────────────────────────────────────────────────────

const HTTP_METHODS: &[&str] = &["GET", "POST", "PUT", "PATCH", "DELETE", "HEAD", "OPTIONS"];

pattern!(
    PY_REST_DECORATOR,
    r"@\s*(?:\w+\.)?(?P<method>get|post|put|patch|delete|head|options|route)\s*\(\s*(?P<args>[^)]+)\)",
    "i"
);
pattern!(PY_ROUTE_METHODS, r"methods\s*=\s*\[([^\]]+)\]", "i");
pattern!(QUOTED_PATH, r#"['"]([^'"]+)['"]"#);
pattern!(
    TS_NEST_DECORATOR,
    r"@\s*(?P<method>Get|Post|Put|Patch|Delete|Head|Options)\s*\(\s*(?P<args>[^)]*)\)"
);
pattern!(
    TS_EXPRESS_CALL,
    r"\b(?:app|router)\s*\.\s*(?P<method>get|post|put|patch|delete|head|options)\s*\(\s*(?P<args>[^,]+),",
    "i"
);
pattern!(
    TS_HTTP_CLIENT_CALL,
    r#"\b(?:axios|client|api|http|request)\s*\.\s*(?P<method>get|post|put|patch|delete|head|options)\s*\(\s*['"`](?P<path>[^'"`]+)['"`]"#,
    "i"
);
pattern!(
    TS_FETCH_CALL,
    r#"\bfetch\s*\(\s*['"`](?P<path>/[^'"`]+)['"`](?P<rest>[^)]*)\)"#
);
pattern!(
    TS_FETCH_METHOD_OPT,
    r#"method\s*:\s*['"`](?P<method>GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)['"`]"#,
    "i"
);
pattern!(
    TS_REQUEST_OPTIONS,
    r#"\{[^{}]{0,400}?method\s*:\s*['"`](?P<method>GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)['"`][^{}]{0,400}?url\s*:\s*['"`](?P<path>/[^'"`]+)['"`]"#,
    "is"
);
pattern!(
    TS_REQUEST_OPTIONS_REVERSED,
    r#"\{[^{}]{0,400}?url\s*:\s*['"`](?P<path>/[^'"`]+)['"`][^{}]{0,400}?method\s*:\s*['"`](?P<method>GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)['"`]"#,
    "is"
);
pattern!(
    PY_ROUTER_PREFIX,
    r#"\b(?:APIRouter|Blueprint|Router)\s*\([^)]*?(?:url_)?prefix\s*=\s*['"]([^'"]+)['"]"#,
    "is"
);
pattern!(
    PY_CTYPES_LIB,
    r"\b(?P<var>\w+)\s*=\s*ctypes\.(?:CDLL|WinDLL|cdll|windll)\b"
);
// `^/(?:api|rest|graphql)(?:/v\d+(?:beta\d*|alpha\d*|rc\d*)?)?(?=/|$)`
// with the look-ahead CONSUMED: the backtracking choices are the same, so
// the match is the prefix plus at most one `/`, which is given back.
pattern!(
    API_PREFIX,
    r"^/(?:api|rest|graphql)(?:/v\d+(?:beta\d*|alpha\d*|rc\d*)?)?(?:/|$)",
    "i"
);
pattern!(JAVA_PATH, r#"@\s*Path\s*\(\s*['"]([^'"]+)['"]"#);
pattern!(
    JAVA_METHOD,
    r"@\s*(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\b"
);
pattern!(
    JAVA_SPRING,
    r#"@\s*(?P<method>Get|Post|Put|Patch|Delete|Head|Options)Mapping\s*\(\s*['"]?(?P<path>[^'")\s,]*)"#
);
pattern!(
    JAVA_REQUEST,
    r#"@\s*RequestMapping\s*\(\s*['"]?(?P<path>[^'")\s,]*)"#
);
pattern!(
    GO_REST,
    r#"\b\w+\s*\.\s*(?P<method>GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS)\s*\(\s*['"](?P<path>[^'"]+)"#
);

/// `_normalize_path`.
#[must_use]
pub fn normalize_path(path: &str) -> String {
    let p = pystr::strip_chars(pystr::strip(path), "\"'");
    if p.is_empty() {
        return "/".to_owned();
    }
    let mut p = if p.starts_with('/') {
        p.to_owned()
    } else {
        format!("/{p}")
    };
    if p.len() > 1 && p.ends_with('/') {
        p.pop();
    }
    p
}

fn rest_surface(method: &str, path: &str, weight_hint: f64, role: &str) -> ApiSurface {
    let method = method.to_uppercase();
    let path = normalize_path(path);
    ApiSurface::new(
        "rest",
        format!("{method} {path}"),
        weight_hint,
        json!({"method": method, "path": path, "role": role}),
    )
}

/// `_strip_common_api_prefix`: the path without a leading `/api[/vN]`
/// (or `/rest`, `/graphql`), or `""` when it has none.
#[must_use]
pub fn strip_common_api_prefix(path: &str) -> String {
    let p = normalize_path(path);
    let Some(found) = API_PREFIX.find(&p) else {
        return String::new();
    };
    let mut end = found.end();
    if p[..end].ends_with('/') {
        end -= 1;
    }
    let new = &p[end..];
    if new.is_empty() || new == p {
        return String::new();
    }
    if new.starts_with('/') {
        normalize_path(new)
    } else {
        normalize_path(&format!("/{new}"))
    }
}

/// `_emit_rest_surfaces`: the canonical surface, plus a prefix-stripped
/// alternate for a `/api[/vN]` path.
fn emit_rest(
    method: &str,
    path: &str,
    weight_hint: f64,
    router_prefix: &str,
    role: &str,
) -> Vec<ApiSurface> {
    let mut full_path = path.to_owned();
    if !router_prefix.is_empty() {
        let prefix = normalize_path(router_prefix);
        let rest = normalize_path(path);
        if !rest.starts_with(&format!("{prefix}/")) && rest != prefix {
            let joined = format!(
                "{}{}",
                prefix.trim_end_matches('/'),
                if rest == "/" { "" } else { rest.as_str() }
            );
            full_path = if joined.is_empty() {
                "/".to_owned()
            } else {
                joined
            };
        }
    }
    let mut surfaces = vec![rest_surface(method, &full_path, weight_hint, role)];
    let stripped = strip_common_api_prefix(&full_path);
    if !stripped.is_empty() && stripped != normalize_path(&full_path) {
        let method = method.to_uppercase();
        surfaces.push(ApiSurface::new(
            "rest",
            format!("{method} {stripped}"),
            f64::max(0.4, weight_hint - 0.1),
            json!({"method": method, "path": stripped, "prefix_stripped": true, "role": role}),
        ));
    }
    surfaces
}

fn emit_server(method: &str, path: &str) -> Vec<ApiSurface> {
    emit_rest(method, path, 0.7, "", "server")
}

fn emit_client(method: &str, path: &str) -> Vec<ApiSurface> {
    emit_rest(method, path, 0.7, "", "client")
}

fn match_rest_python(text: &str) -> Vec<ApiSurface> {
    let mut out = Vec::new();
    let router_prefix = PY_ROUTER_PREFIX
        .captures(text)
        .map_or("", |c| group_at(&c, 1));
    for m in PY_REST_DECORATOR.captures_iter(text) {
        let method = group(&m, "method").to_lowercase();
        let args = group(&m, "args");
        let Some(path) = QUOTED_PATH.captures(args) else {
            continue;
        };
        let path = group_at(&path, 1);
        if method == "route" {
            let mut methods: Vec<String> = PY_ROUTE_METHODS
                .captures(args)
                .map(|c| {
                    group_at(&c, 1)
                        .split(',')
                        .filter(|s| !pystr::strip(s).is_empty())
                        .map(|s| pystr::strip_chars(pystr::strip(s), "\"'").to_uppercase())
                        .collect()
                })
                .unwrap_or_default();
            if methods.is_empty() {
                methods.push("GET".to_owned());
            }
            for method in methods {
                if HTTP_METHODS.contains(&method.as_str()) {
                    out.extend(emit_rest(&method, path, 0.7, router_prefix, "server"));
                }
            }
        } else {
            out.extend(emit_rest(&method, path, 0.7, router_prefix, "server"));
        }
    }
    out
}

fn match_rest_typescript(text: &str) -> Vec<ApiSurface> {
    let mut out = Vec::new();
    for m in TS_NEST_DECORATOR.captures_iter(text) {
        let path = QUOTED_PATH
            .captures(group(&m, "args"))
            .map_or("/", |c| group_at(&c, 1));
        out.extend(emit_server(group(&m, "method"), path));
    }
    for m in TS_EXPRESS_CALL.captures_iter(text) {
        if let Some(path) = QUOTED_PATH.captures(group(&m, "args")) {
            out.extend(emit_server(group(&m, "method"), group_at(&path, 1)));
        }
    }
    for m in TS_HTTP_CLIENT_CALL.captures_iter(text) {
        out.extend(emit_client(group(&m, "method"), group(&m, "path")));
    }
    for m in TS_FETCH_CALL.captures_iter(text) {
        let method = TS_FETCH_METHOD_OPT
            .captures(group(&m, "rest"))
            .map_or("GET", |c| group(&c, "method"));
        out.extend(emit_client(method, group(&m, "path")));
    }
    for m in TS_REQUEST_OPTIONS.captures_iter(text) {
        out.extend(emit_client(group(&m, "method"), group(&m, "path")));
    }
    for m in TS_REQUEST_OPTIONS_REVERSED.captures_iter(text) {
        out.extend(emit_client(group(&m, "method"), group(&m, "path")));
    }
    out
}

fn match_rest_java(text: &str) -> Vec<ApiSurface> {
    let mut out = Vec::new();
    let paths: Vec<&str> = JAVA_PATH
        .captures_iter(text)
        .map(|c| group_at(&c, 1))
        .collect();
    let methods: Vec<&str> = JAVA_METHOD
        .captures_iter(text)
        .map(|c| group_at(&c, 1))
        .collect();
    for path in &paths {
        for method in &methods {
            out.extend(emit_server(method, path));
        }
    }
    for m in JAVA_SPRING.captures_iter(text) {
        out.extend(emit_server(group(&m, "method"), or_root(group(&m, "path"))));
    }
    for m in JAVA_REQUEST.captures_iter(text) {
        out.extend(emit_server("GET", or_root(group(&m, "path"))));
    }
    out
}

fn or_root(path: &str) -> &str {
    if path.is_empty() { "/" } else { path }
}

fn match_rest_go(text: &str) -> Vec<ApiSurface> {
    GO_REST
        .captures_iter(text)
        .flat_map(|m| emit_server(group(&m, "method"), group(&m, "path")))
        .collect()
}

fn match_rest(language: &str, text: &str) -> Vec<ApiSurface> {
    match language {
        "python" => match_rest_python(text),
        "typescript" | "javascript" => match_rest_typescript(text),
        "java" | "kotlin" => match_rest_java(text),
        "go" => match_rest_go(text),
        _ => Vec::new(),
    }
}

// ── gRPC servers ────────────────────────────────────────────────────────

pattern!(PROTO_SERVICE, r"\bservice\s+(\w+)\s*{", "m");
pattern!(PROTO_RPC, r"\brpc\s+(\w+)\s*\(", "m");
pattern!(PY_SERVICER_CLASS, r"\bclass\s+(?P<svc>\w+)Servicer\b");
pattern!(PY_SERVICER_DEF, r"\bdef\s+(?P<rpc>[A-Z]\w*)\s*\(\s*self\b");
pattern!(
    GO_GRPC_SERVER,
    r"\btype\s+(?P<svc>\w+)Server\s+interface\s*{",
    "m"
);
pattern!(GO_GRPC_METHOD, r"^\s+(?P<rpc>[A-Z]\w*)\s*\(", "m");
pattern!(
    GO_GRPC_FULLMETHOD,
    r#"(?P<svc>\w+)_(?P<rpc>\w+)_FullMethodName\s*=\s*"[^"]*""#
);
pattern!(
    JAVA_GRPC_IMPL,
    r"\bextends\s+(?:\w+\.)*(?P<svc>\w+)Grpc\.\w+ImplBase\b"
);
pattern!(
    JAVA_GRPC_METHOD,
    r"\bpublic\s+\w+\s+(?P<rpc>[a-z]\w*)\s*\(\s*\w+(?:Request|Req)\b"
);
pattern!(
    JAVA_GRPC_OBSERVER,
    r"\bpublic\s+void\s+(?P<rpc>[a-z]\w*)\s*\([^)]*StreamObserver\b"
);
// `\bclass\s+\w+\s*:\s*(?:\w+\.)*(?P<svc>\w+)\.(?P=svc)Base\b`: the
// back-reference is checked by hand over the dotted chain.
pattern!(CS_GRPC_IMPL_HEAD, r"\bclass\s+\w+\s*:\s*(\w+(?:\.\w+)*)");
pattern!(
    CS_GRPC_METHOD,
    r"\bpublic\s+override\s+[\w<>,\s]+\s+(?P<rpc>[A-Z]\w*)\s*\("
);
pattern!(
    JS_GRPC_ADDSERVICE,
    r"\.addService\s*\(\s*\w+\.(?P<svc>\w+)\.service\b"
);
pattern!(JS_GRPC_HANDLER_MAP, r"{\s*(?P<methods>[^}]+)}\s*\)");
pattern!(JS_GRPC_METHOD_KEY, r"(?P<rpc>\w+)\s*:");
pattern!(
    CPP_GRPC_SERVICE,
    r"\bclass\s+\w+\s*(?:final\s*)?:\s*public\s+(?P<svc>\w+)::Service\b"
);
pattern!(
    CPP_GRPC_METHOD,
    r"\b(?:grpc::)?Status\s+(?P<rpc>[A-Z]\w*)\s*\(\s*(?:grpc::)?ServerContext\b"
);
pattern!(RUST_GRPC_IMPL, r"\bimpl\s+(?P<svc>\w+)\s+for\s+\w+", "m");
pattern!(RUST_GRPC_METHOD, r"\basync\s+fn\s+(?P<rpc>\w+)\s*\(", "m");

fn grpc(svc: &str, rpc: &str, weight_hint: f64) -> ApiSurface {
    ApiSurface::new(
        "grpc",
        format!("grpc:{svc}/{rpc}"),
        weight_hint,
        json!({"service": svc, "method": rpc}),
    )
}

/// The end of the brace block whose body starts at `start` with one brace
/// already open: the index after the closing brace, or the text end.
fn block_end(text: &str, start: usize) -> usize {
    let bytes = text.as_bytes();
    let mut depth = 1usize;
    let mut i = start;
    while i < bytes.len() && depth > 0 {
        match bytes[i] {
            b'{' => depth += 1,
            b'}' => depth -= 1,
            _ => {}
        }
        i += 1;
    }
    i
}

/// `_CS_GRPC_IMPL.finditer`: the service of each `class X : A.Svc.SvcBase`.
fn csharp_grpc_impls(text: &str) -> Vec<(String, usize)> {
    let mut found = Vec::new();
    for head in CS_GRPC_IMPL_HEAD.captures_iter(text) {
        let Some(chain) = head.get(1) else {
            continue;
        };
        let parts: Vec<&str> = chain.as_str().split('.').collect();
        // `(?:\w+\.)*` is greedy: the LAST segment pair that fits wins.
        let mut end = chain.start();
        let mut svc = None;
        for i in (0..parts.len().saturating_sub(1)).rev() {
            if parts[i + 1].strip_suffix("Base") == Some(parts[i]) {
                svc = Some(parts[i]);
                end += parts[..=i + 1].iter().map(|p| p.len() + 1).sum::<usize>() - 1;
                break;
            }
        }
        if let Some(svc) = svc {
            found.push((svc.to_owned(), end));
        }
    }
    found
}

#[allow(clippy::too_many_lines)] // one block per language, as in Python
fn match_grpc(text: &str, language: &str) -> Vec<ApiSurface> {
    let mut out: Vec<ApiSurface> = Vec::new();

    if matches!(language, "proto" | "schema")
        || (text.contains("service ") && text.contains("rpc "))
    {
        for sm in PROTO_SERVICE.captures_iter(text) {
            let svc = group_at(&sm, 1);
            let start = sm.get(0).map_or(0, |m| m.end());
            let body = &text[start..block_end(text, start)];
            for rpc in PROTO_RPC.captures_iter(body) {
                out.push(grpc(svc, group_at(&rpc, 1), 0.8));
            }
        }
    }

    if language == "python" && text.contains("Servicer") {
        for sm in PY_SERVICER_CLASS.captures_iter(text) {
            let svc = group(&sm, "svc");
            for dm in PY_SERVICER_DEF.captures_iter(text) {
                out.push(grpc(svc, group(&dm, "rpc"), 0.7));
            }
        }
    }

    if language == "go" {
        for sm in GO_GRPC_SERVER.captures_iter(text) {
            let svc = group(&sm, "svc");
            let start = sm.get(0).map_or(0, |m| m.end());
            let body = &text[start..block_end(text, start)];
            for dm in GO_GRPC_METHOD.captures_iter(body) {
                out.push(grpc(svc, group(&dm, "rpc"), 0.7));
            }
        }
        for fm in GO_GRPC_FULLMETHOD.captures_iter(text) {
            out.push(grpc(group(&fm, "svc"), group(&fm, "rpc"), 0.7));
        }
    }

    if language == "java" && (text.contains("ImplBase") || text.contains("Grpc")) {
        for sm in JAVA_GRPC_IMPL.captures_iter(text) {
            let svc = group(&sm, "svc");
            let after = sm.get(0).map_or(0, |m| m.end());
            let Some(open) = text[after..].find('{').map(|i| i + after) else {
                continue;
            };
            let body = &text[open..block_end(text, open + 1)];
            for dm in JAVA_GRPC_METHOD.captures_iter(body) {
                let rpc = pystr::upper_first(group(&dm, "rpc"));
                out.push(grpc(svc, &rpc, 0.7));
            }
            for dm in JAVA_GRPC_OBSERVER.captures_iter(body) {
                let rpc = pystr::upper_first(group(&dm, "rpc"));
                if !out.iter().any(|s| s.meta_str("method") == rpc) {
                    out.push(grpc(svc, &rpc, 0.7));
                }
            }
        }
    }

    if language == "csharp" && text.contains("Base") {
        for (svc, after) in csharp_grpc_impls(text) {
            let Some(open) = text[after..].find('{').map(|i| i + after) else {
                continue;
            };
            let body = &text[open..block_end(text, open + 1)];
            for dm in CS_GRPC_METHOD.captures_iter(body) {
                out.push(grpc(&svc, group(&dm, "rpc"), 0.7));
            }
        }
    }

    if matches!(language, "javascript" | "typescript") && text.contains("addService") {
        for sm in JS_GRPC_ADDSERVICE.captures_iter(text) {
            let svc = group(&sm, "svc");
            let start = sm.get(0).map_or(0, |m| m.end());
            let window = pystr::prefix_chars(&text[start..], 500);
            if let Some(bm) = JS_GRPC_HANDLER_MAP.captures(window) {
                for km in JS_GRPC_METHOD_KEY.captures_iter(group(&bm, "methods")) {
                    let rpc = pystr::upper_first(group(&km, "rpc"));
                    out.push(grpc(svc, &rpc, 0.7));
                }
            }
        }
    }

    if matches!(language, "cpp" | "c++") && text.contains("::Service") {
        // Every service gets every method of the file: found once.
        let methods: Vec<&str> = CPP_GRPC_METHOD
            .captures_iter(text)
            .map(|dm| group(&dm, "rpc"))
            .collect();
        for sm in CPP_GRPC_SERVICE.captures_iter(text) {
            let svc = group(&sm, "svc");
            for rpc in &methods {
                out.push(grpc(svc, rpc, 0.7));
            }
        }
    }

    if language == "rust" && text.contains("impl ") && text.contains("async fn") {
        let impls: Vec<(&str, usize)> = RUST_GRPC_IMPL
            .captures_iter(text)
            .filter_map(|sm| {
                let svc = group(&sm, "svc");
                let lower_start = svc.chars().next().is_some_and(char::is_lowercase);
                if lower_start || matches!(svc, "self" | "Self" | "impl") {
                    return None;
                }
                Some((svc, sm.get(0).map_or(0, |m| m.end())))
            })
            .collect();
        let methods = methods_after(&impls, text);
        for ((svc, _), rpcs) in impls.iter().zip(methods) {
            for rpc in rpcs {
                if rpc.starts_with('_') {
                    continue;
                }
                out.push(grpc(svc, &pascal_from_snake(rpc), 0.6));
            }
        }
    }

    out
}

/// For each `(svc, start)` (in increasing `start` order), the `rpc` of
/// every `RUST_GRPC_METHOD` match in `text[start..]`, as Python's
/// `finditer` over the slice finds them.
///
/// Searching each slice to its end costs impls × file size. Here each
/// search runs once: a leftmost-first search from a position `p` finds the
/// same match as one from any earlier position whose match starts at or
/// after `p`, and the matches after a match depend only on where it ends.
/// Past the slice start, `\b` sees the same text in the slice as in the
/// file, so `captures_at` over the file gives the slice's answer; at the
/// slice start itself a match must begin with `async`, and only then is the
/// slice searched as such.
fn methods_after<'t>(impls: &[(&str, usize)], text: &'t str) -> Vec<Vec<&'t str>> {
    /// The match after a match, once searched for.
    #[derive(Clone, Copy)]
    enum Next {
        NotSearched,
        Searched(Option<usize>),
    }
    /// One match: its end and its `rpc`; the match after it.
    struct Found<'t> {
        end: usize,
        rpc: &'t str,
        next: Next,
    }
    let mut found: Vec<Found<'t>> = Vec::new();
    let mut by_start: HashMap<usize, usize> = HashMap::new();
    // The last search from a slice start: (from, match index or `None`).
    let mut last_search: Option<(usize, Option<usize>, usize)> = None;
    let mut record = |found: &mut Vec<Found<'t>>, start: usize, end: usize, rpc: &'t str| {
        *by_start.entry(start).or_insert_with(|| {
            found.push(Found {
                end,
                rpc,
                next: Next::NotSearched,
            });
            found.len() - 1
        })
    };
    let search = |from: usize| {
        RUST_GRPC_METHOD.captures_at(text, from).and_then(|c| {
            let whole = c.get(0)?;
            Some((whole.start(), whole.end(), group(&c, "rpc")))
        })
    };
    let mut out = Vec::with_capacity(impls.len());
    for &(_, start) in impls {
        // The slice's first match.
        let first = if text[start..].starts_with("async") {
            RUST_GRPC_METHOD.captures(&text[start..]).and_then(|c| {
                let whole = c.get(0)?;
                Some((start + whole.start(), start + whole.end(), group(&c, "rpc")))
            })
        } else {
            match last_search {
                // No match from an earlier position, or the earlier match
                // starts here or later: the same answer.
                Some((from, None, _)) if from <= start => None,
                Some((from, Some(index), at)) if from <= start && at >= start => {
                    Some((at, found[index].end, found[index].rpc))
                }
                _ => search(start),
            }
        };
        let mut current = first.map(|(at, end, rpc)| {
            let index = record(&mut found, at, end, rpc);
            last_search = Some((start, Some(index), at));
            index
        });
        if current.is_none() {
            last_search = Some((start, None, start));
        }
        let mut rpcs = Vec::new();
        while let Some(index) = current {
            rpcs.push(found[index].rpc);
            let next = if let Next::Searched(next) = found[index].next {
                next
            } else {
                let next =
                    search(found[index].end).map(|(at, end, rpc)| record(&mut found, at, end, rpc));
                found[index].next = Next::Searched(next);
                next
            };
            current = next;
        }
        out.push(rpcs);
    }
    out
}

/// `"".join(w.capitalize() for w in rpc.split("_"))`.
fn pascal_from_snake(name: &str) -> String {
    name.split('_').map(pystr::capitalize).collect()
}

// ── gRPC clients ────────────────────────────────────────────────────────

pattern!(
    GO_GRPC_CLIENT_CHAIN,
    r"New(?P<svc>[A-Z]\w*?)Client\s*\([^()]*\)\s*\.\s*(?P<rpc>[A-Z]\w*)\s*\("
);
pattern!(
    GO_GRPC_CLIENT_BIND,
    r"(?P<var>\w+)\s*:?=\s*[\w.]*New(?P<svc>[A-Z]\w*?)Client\s*\("
);
pattern!(
    PY_GRPC_CLIENT_CHAIN,
    r"(?P<svc>[A-Z]\w*?)Stub\s*\([^()]*\)\s*\.\s*(?P<rpc>[A-Z]\w*)\s*\("
);
pattern!(
    PY_GRPC_CLIENT_BIND,
    r"(?P<var>\w+)\s*=\s*[\w.]*?(?P<svc>[A-Z]\w*?)Stub\s*\("
);
pattern!(
    JAVA_GRPC_CLIENT_BIND,
    r"(?P<var>\w+)\s*=\s*(?:\w+\.)*(?P<svc>[A-Z]\w*?)Grpc\.new\w*Stub\s*\("
);
pattern!(
    JAVA_GRPC_CLIENT_TYPE,
    r"(?:\w+\.)*(?P<svc>[A-Z]\w*?)Grpc\.\w*Stub\s+(?P<var>\w+)\b"
);
pattern!(
    NEW_CLIENT_BIND,
    r"(?P<var>\w+)\s*=\s*new\s+(?:\w+\.)*(?P<svc>[A-Z]\w*?)Client\s*\("
);
pattern!(
    CPP_GRPC_CLIENT_BIND,
    r"(?P<var>\w+)\s*=\s*(?:[\w:]+::)?(?P<svc>[A-Z]\w*?)::NewStub\s*\("
);
pattern!(
    CPP_GRPC_CLIENT_TYPE,
    r"(?:\w+::)*(?P<svc>[A-Z]\w*?)::Stub\s*>\s*(?P<var>\w+)\b"
);
pattern!(
    RUST_GRPC_CLIENT_BIND,
    r"(?P<var>\w+)\s*=\s*(?:\w+::)*(?P<svc>[A-Z]\w*?)Client::(?:connect|new|with_\w+)\s*\("
);
pattern!(
    GRPC_CLIENT_CALL,
    r"\b(?P<var>\w+)\s*\.\s*(?P<rpc>[A-Z]\w*)\s*\("
);
pattern!(
    JAVA_GRPC_CLIENT_CALL,
    r"\b(?P<var>\w+)\s*\.\s*(?P<rpc>[a-z]\w*)\s*\("
);
pattern!(
    CPP_GRPC_CLIENT_CALL,
    r"\b(?P<var>\w+)\s*->\s*(?P<rpc>[A-Z]\w*)\s*\("
);
pattern!(
    RUST_GRPC_CLIENT_CALL,
    r"\b(?P<var>\w+)\s*\.\s*(?P<rpc>[a-z][a-z0-9_]*)\s*\("
);

/// The languages with a gRPC client matcher.
const GRPC_CLIENT_LANGUAGES: &[&str] = &[
    "go",
    "python",
    "java",
    "csharp",
    "javascript",
    "typescript",
    "cpp",
    "c++",
    "rust",
];

/// `extract_grpc_stub_bindings`: `{var: service}` for the client stubs
/// bound in `text`.
#[must_use]
pub fn extract_grpc_stub_bindings(text: &str, language: &str) -> IndexMap<String, String> {
    let mut bindings = IndexMap::new();
    let mut assign = |re: &Regex, overwrite: bool| {
        for m in re.captures_iter(text) {
            let var = group(&m, "var").to_owned();
            let svc = group(&m, "svc").to_owned();
            if overwrite {
                bindings.insert(var, svc);
            } else {
                bindings.entry(var).or_insert(svc);
            }
        }
    };
    match language {
        "go" => assign(&GO_GRPC_CLIENT_BIND, true),
        "python" => assign(&PY_GRPC_CLIENT_BIND, true),
        "java" => {
            assign(&JAVA_GRPC_CLIENT_BIND, true);
            assign(&JAVA_GRPC_CLIENT_TYPE, false);
        }
        "csharp" | "javascript" | "typescript" => assign(&NEW_CLIENT_BIND, true),
        "cpp" | "c++" => {
            assign(&CPP_GRPC_CLIENT_BIND, true);
            assign(&CPP_GRPC_CLIENT_TYPE, false);
        }
        "rust" => assign(&RUST_GRPC_CLIENT_BIND, true),
        _ => {}
    }
    bindings
}

fn match_grpc_client(
    text: &str,
    language: &str,
    file_bindings: Option<&IndexMap<String, String>>,
) -> Vec<ApiSurface> {
    let mut out = Vec::new();
    let mut seen: HashSet<(String, String)> = HashSet::new();
    let mut emit = |svc: &str, rpc: &str| {
        if svc.is_empty() || rpc.is_empty() || !seen.insert((svc.to_owned(), rpc.to_owned())) {
            return;
        }
        out.push(ApiSurface::new(
            "grpc",
            format!("grpc:{svc}/{rpc}"),
            0.7,
            json!({"service": svc, "method": rpc, "role": "client"}),
        ));
    };

    let chain = match language {
        "go" => Some(&*GO_GRPC_CLIENT_CHAIN),
        "python" => Some(&*PY_GRPC_CLIENT_CHAIN),
        _ => None,
    };
    if let Some(chain) = chain {
        for m in chain.captures_iter(text) {
            emit(group(&m, "svc"), group(&m, "rpc"));
        }
    }

    let mut bindings: IndexMap<String, String> = file_bindings.cloned().unwrap_or_default();
    for (var, svc) in extract_grpc_stub_bindings(text, language) {
        bindings.entry(var).or_insert(svc);
    }
    if bindings.is_empty() {
        return out;
    }
    let call: &Regex = match language {
        "java" | "javascript" | "typescript" => &JAVA_GRPC_CLIENT_CALL,
        "cpp" | "c++" => &CPP_GRPC_CLIENT_CALL,
        "rust" => &RUST_GRPC_CLIENT_CALL,
        _ => &GRPC_CLIENT_CALL,
    };
    for m in call.captures_iter(text) {
        let Some(svc) = bindings.get(group(&m, "var")).filter(|s| !s.is_empty()) else {
            continue;
        };
        let rpc = group(&m, "rpc");
        let rpc = match language {
            "java" | "javascript" | "typescript" => pystr::upper_first(rpc),
            "rust" => pascal_from_snake(rpc),
            "csharp" if rpc.ends_with("Async") && rpc.chars().count() > 5 => {
                rpc[..rpc.len() - 5].to_owned()
            }
            _ => rpc.to_owned(),
        };
        emit(svc, &rpc);
    }
    out
}

// ── GraphQL, FFI ────────────────────────────────────────────────────────

pattern!(
    GQL_FIELD,
    r"\b(?P<op>type|extend\s+type)\s+(?P<root>Query|Mutation|Subscription)\s*{",
    "i"
);
pattern!(
    GQL_RESOLVER_DEC,
    r"@\s*(?P<op>Query|Mutation|Subscription|Resolver|FieldResolver)\b"
);

fn match_graphql(text: &str) -> Vec<ApiSurface> {
    let mut out = Vec::new();
    if text.contains("type Query")
        || text.contains("type Mutation")
        || text.contains("type Subscription")
    {
        for m in GQL_FIELD.captures_iter(text) {
            let root = group(&m, "root").to_lowercase();
            out.push(ApiSurface::new(
                "graphql",
                format!("gql:{root}"),
                0.6,
                json!({"root": root}),
            ));
        }
    }
    for m in GQL_RESOLVER_DEC.captures_iter(text) {
        let op = group(&m, "op").to_lowercase();
        out.push(ApiSurface::new(
            "graphql",
            format!("gql:{op}"),
            0.5,
            json!({"resolver": op}),
        ));
    }
    out
}

pattern!(FFI_EXTERN_C, r#"extern\s+["']C["']"#);
pattern!(FFI_CTYPES, r"\bctypes\.(?:CDLL|WinDLL|cdll|windll)\b");
pattern!(FFI_JNI, r"\bnative\s+\w+\s+\w+\s*\(");
pattern!(
    FFI_PINVOKE,
    r#"\[\s*DllImport\s*\(\s*['"]([^'"]+)['"]\s*\)"#
);
pattern!(FFI_WASM, r"#\s*\[\s*wasm_bindgen\s*\]");

fn match_ffi(text: &str, symbol_name: &str) -> Vec<ApiSurface> {
    let mut out = Vec::new();
    let triggered = FFI_EXTERN_C.is_match(text)
        || FFI_CTYPES.is_match(text)
        || FFI_JNI.is_match(text)
        || FFI_WASM.is_match(text);
    if triggered && !symbol_name.is_empty() {
        out.push(ApiSurface::new(
            "ffi",
            format!("ffi:{symbol_name}"),
            0.6,
            json!({"symbol": symbol_name}),
        ));
    }
    for m in FFI_PINVOKE.captures_iter(text) {
        let library = group_at(&m, 1);
        out.push(ApiSurface::new(
            "ffi",
            format!("ffi:{library}"),
            0.7,
            json!({"library": library}),
        ));
        if !symbol_name.is_empty() && symbol_name != library {
            out.push(ApiSurface::new(
                "ffi",
                format!("ffi:{symbol_name}"),
                0.6,
                json!({"symbol": symbol_name, "library": library}),
            ));
        }
    }
    out
}

// ── Data shapes (`obj:`) ────────────────────────────────────────────────

pattern!(OBJ_PY_CLASS, r"^\s*class\s+(?P<name>\w+)\s*[:\(]", "m");
pattern!(
    OBJ_PY_FIELD,
    r"^[ \t]+(?P<name>[A-Za-z_]\w*)\s*:\s*[^=#\n]+(?:=\s*[^#\n]+)?\s*(?:#.*)?$",
    "m"
);
pattern!(OBJ_PY_DUNDER, r"^__\w+__$");
pattern!(
    OBJ_TS_INTERFACE,
    r"\b(?:export\s+)?interface\s+(?P<name>\w+)(?:\s+extends\s+[^\{]+)?\s*\{(?P<body>[^{}]*)\}",
    "s"
);
pattern!(
    OBJ_TS_TYPE,
    r"\b(?:export\s+)?type\s+(?P<name>\w+)\s*=\s*\{(?P<body>[^{}]*)\}",
    "s"
);
pattern!(
    OBJ_TS_FIELD,
    r"(?:^|[;,\n])\s*(?:readonly\s+)?(?P<name>[A-Za-z_]\w*)\s*\??\s*:"
);
pattern!(
    OBJ_GO_STRUCT,
    r"\btype\s+(?P<name>\w+)\s+struct\s*\{(?P<body>[^{}]*)\}",
    "s"
);
pattern!(
    OBJ_GO_FIELD,
    r"^\s*(?P<name>[A-Z]\w*)\s+[^`\n]+(?:`(?P<tag>[^`]+)`)?",
    "m"
);
pattern!(OBJ_GO_JSON_TAG, r#"json:"([^,"]+)"#);
pattern!(
    OBJ_JAVA_CLASS_HEADER,
    r"\b(?:public\s+|private\s+|protected\s+|static\s+|final\s+|abstract\s+)*class\s+(?P<name>\w+)(?:\s+extends\s+\w+)?(?:\s+implements\s+[^{]+)?\s*\{"
);
pattern!(
    OBJ_RECORD,
    r"\b(?:public\s+)?record\s+(?P<name>\w+)\s*\((?P<params>[^)]*)\)"
);
pattern!(
    OBJ_JAVA_FIELD,
    r"\b(?:public|private|protected)\s+(?:static\s+|final\s+)*[\w<>\[\],\s\.]+?\s+(?P<name>[a-zA-Z_]\w*)\s*[=;]",
    "m"
);
pattern!(
    OBJ_RUST_STRUCT,
    r"\b(?:pub\s+)?struct\s+(?P<name>\w+)\s*\{(?P<body>[^{}]*)\}",
    "s"
);
pattern!(
    OBJ_RUST_FIELD,
    r#"(?:#\[serde\([^)]*?rename\s*=\s*\"(?P<rename>[^\"]+)\"[^)]*\)\]\s*)?(?:pub\s+)?(?P<name>[a-zA-Z_]\w*)\s*:\s*[^,\n]+,?"#
);
pattern!(
    OBJ_CSHARP_CLASS_HEADER,
    r"\b(?:public\s+|internal\s+|private\s+|sealed\s+|abstract\s+)*class\s+(?P<name>\w+)(?:\s*:\s*[^{]+)?\s*\{"
);
pattern!(
    OBJ_CSHARP_FIELD,
    r"\b(?:public|private|protected|internal)\s+(?:static\s+|readonly\s+|virtual\s+)*[\w<>\[\],\.\?]+\s+(?P<name>[A-Za-z_]\w*)\s*(?:\{|=|;)",
    "m"
);
pattern!(SNAKE_1, r"(.)([A-Z][a-z]+)");
pattern!(SNAKE_2, r"([a-z0-9])([A-Z])");

/// `_to_snake`: `OrderId` → `order_id`, `HTTPRequest` → `http_request`.
#[must_use]
pub fn to_snake(name: &str) -> String {
    let once = SNAKE_1.replace_all(name, "${1}_${2}");
    SNAKE_2.replace_all(&once, "${1}_${2}").to_lowercase()
}

/// `_obj_surface`.
fn obj_surface(name: &str, fields: &[String]) -> Option<ApiSurface> {
    let name = pystr::strip(name);
    if name.is_empty() {
        return None;
    }
    let mut norm: Vec<String> = Vec::new();
    let mut seen: HashSet<String> = HashSet::new();
    for field in fields {
        if field.is_empty() {
            continue;
        }
        let snake = to_snake(pystr::strip(field));
        if snake.is_empty() || seen.contains(&snake) || OBJ_PY_DUNDER.is_match(&snake) {
            continue;
        }
        seen.insert(snake.clone());
        norm.push(snake);
    }
    if norm.is_empty() {
        return None;
    }
    norm.sort();
    Some(ApiSurface::new(
        "obj",
        format!("obj:{}#{}", to_snake(name), norm.join(",")),
        0.65,
        json!({"type": name, "fields": norm}),
    ))
}

/// `_split_param_list`: the last word of each top-level `,` part.
fn split_param_list(params: &str) -> Vec<String> {
    let mut parts: Vec<String> = Vec::new();
    let mut depth = 0i64;
    let mut current = String::new();
    for ch in params.chars() {
        if "<([{".contains(ch) {
            depth += 1;
        } else if ">)]}".contains(ch) {
            depth -= 1;
        }
        if ch == ',' && depth == 0 {
            parts.push(std::mem::take(&mut current));
        } else {
            current.push(ch);
        }
    }
    if !pystr::strip(&current).is_empty() {
        parts.push(current);
    }
    parts
        .iter()
        .filter_map(|part| pystr::split_whitespace(part).last())
        .map(|last| last.trim_end_matches([',', ';']).to_owned())
        .collect()
}

fn names(re: &Regex, body: &str) -> Vec<String> {
    re.captures_iter(body)
        .map(|c| group(&c, "name").to_owned())
        .collect()
}

fn iter_obj_typescript(text: &str) -> Vec<(String, Vec<String>)> {
    let mut out = Vec::new();
    for re in [&*OBJ_TS_INTERFACE, &*OBJ_TS_TYPE] {
        for m in re.captures_iter(text) {
            out.push((
                group(&m, "name").to_owned(),
                names(&OBJ_TS_FIELD, group(&m, "body")),
            ));
        }
    }
    out
}

fn iter_obj_go(text: &str) -> Vec<(String, Vec<String>)> {
    OBJ_GO_STRUCT
        .captures_iter(text)
        .map(|m| {
            let fields = OBJ_GO_FIELD
                .captures_iter(group(&m, "body"))
                .map(|f| {
                    OBJ_GO_JSON_TAG
                        .captures(group(&f, "tag"))
                        .map_or_else(|| group(&f, "name"), |t| group_at(&t, 1))
                        .to_owned()
                })
                .collect();
            (group(&m, "name").to_owned(), fields)
        })
        .collect()
}

/// Java and C#: each class header scans the WHOLE rest of the text for
/// fields (Python: `text[m.end():]`), then records list their parameters.
fn iter_obj_classes(text: &str, header: &Regex, field: &Regex) -> Vec<(String, Vec<String>)> {
    let mut out = Vec::new();
    for m in header.captures_iter(text) {
        let end = m.get(0).map_or(0, |all| all.end());
        out.push((group(&m, "name").to_owned(), names(field, &text[end..])));
    }
    for m in OBJ_RECORD.captures_iter(text) {
        out.push((
            group(&m, "name").to_owned(),
            split_param_list(group(&m, "params")),
        ));
    }
    out
}

fn iter_obj_rust(text: &str) -> Vec<(String, Vec<String>)> {
    OBJ_RUST_STRUCT
        .captures_iter(text)
        .map(|m| {
            let fields = OBJ_RUST_FIELD
                .captures_iter(group(&m, "body"))
                .map(|f| {
                    let rename = group(&f, "rename");
                    if rename.is_empty() {
                        group(&f, "name").to_owned()
                    } else {
                        rename.to_owned()
                    }
                })
                .collect();
            (group(&m, "name").to_owned(), fields)
        })
        .collect()
}

fn iter_obj_python(text: &str) -> Vec<(String, Vec<String>)> {
    let classes: Vec<Captures<'_>> = OBJ_PY_CLASS.captures_iter(text).collect();
    let mut out = Vec::new();
    for (i, m) in classes.iter().enumerate() {
        let start = m.get(0).map_or(0, |all| all.end());
        let end = classes
            .get(i + 1)
            .and_then(|next| next.get(0))
            .map_or(text.len(), |next| next.start());
        // A class header overlapping the previous one cannot occur (the
        // matches do not overlap), but keep the slice well-formed.
        let body = text.get(start..end.max(start)).unwrap_or("");
        out.push((group(m, "name").to_owned(), names(&OBJ_PY_FIELD, body)));
    }
    out
}

fn match_objects(text: &str, language: &str) -> Vec<ApiSurface> {
    let shapes = match language {
        "python" => iter_obj_python(text),
        "typescript" | "javascript" => iter_obj_typescript(text),
        "go" => iter_obj_go(text),
        "java" | "kotlin" => iter_obj_classes(text, &OBJ_JAVA_CLASS_HEADER, &OBJ_JAVA_FIELD),
        "rust" => iter_obj_rust(text),
        "csharp" | "c#" | "cs" => {
            iter_obj_classes(text, &OBJ_CSHARP_CLASS_HEADER, &OBJ_CSHARP_FIELD)
        }
        _ => return Vec::new(),
    };
    shapes
        .iter()
        .filter_map(|(name, fields)| obj_surface(name, fields))
        .collect()
}

// ── BDD, CLI ────────────────────────────────────────────────────────────

pattern!(
    BDD_DECORATOR,
    r#"@\s*(?P<kind>given|when|then|step)\s*\(\s*['"](?P<text>[^'"]+)['"]\s*\)"#,
    "i"
);
pattern!(
    BDD_GHERKIN,
    r"^\s*(?P<kind>Given|When|Then|And|But)\s+(?P<text>.+)$",
    "m"
);

fn bdd(m: &Captures<'_>, weight_hint: f64) -> ApiSurface {
    let text = pystr::strip(group(m, "text")).to_lowercase();
    ApiSurface::new(
        "bdd",
        format!("bdd:{text}"),
        weight_hint,
        json!({"kind": group(m, "kind").to_lowercase()}),
    )
}

fn match_bdd(text: &str, rel_path: &str) -> Vec<ApiSurface> {
    let mut out: Vec<ApiSurface> = BDD_DECORATOR
        .captures_iter(text)
        .map(|m| bdd(&m, 0.7))
        .collect();
    let lower = rel_path.to_lowercase();
    if [".feature", ".story"]
        .iter()
        .any(|suffix| lower.ends_with(suffix))
    {
        out.extend(BDD_GHERKIN.captures_iter(text).map(|m| bdd(&m, 0.6)));
    }
    out
}

pattern!(
    CLI_CLICK,
    r#"@\s*(?:\w+\.)?(?:command|group)\s*\(\s*(?:name\s*=\s*)?['"]([^'"]+)['"]"#
);
pattern!(
    CLI_ARGPARSE,
    r#"add_subparsers\s*\(.*?\)\.add_parser\s*\(\s*['"]([^'"]+)['"]"#,
    "s"
);
pattern!(
    CLI_COBRA,
    r#"&cobra\.Command\s*{[^}]*?Use:\s*['"]([^'"]+)['"]"#,
    "s"
);

fn cli(command: &str, framework: &str) -> ApiSurface {
    ApiSurface::new(
        "cli",
        format!("cli:{command}"),
        0.6,
        json!({"framework": framework}),
    )
}

/// `_match_cli`. `None` where Python raises: a cobra `Use:` of blanks only
/// makes `.split()[0]` an `IndexError`, and the caller then drops every
/// surface of the node.
fn match_cli(text: &str) -> Option<Vec<ApiSurface>> {
    let mut out: Vec<ApiSurface> = CLI_CLICK
        .captures_iter(text)
        .map(|m| cli(group_at(&m, 1), "click"))
        .collect();
    out.extend(
        CLI_ARGPARSE
            .captures_iter(text)
            .map(|m| cli(group_at(&m, 1), "argparse")),
    );
    for m in CLI_COBRA.captures_iter(text) {
        let first = pystr::split_whitespace(group_at(&m, 1)).next()?;
        out.push(cli(first, "cobra"));
    }
    Some(out)
}

// ── Pylon class APIs ────────────────────────────────────────────────────

pattern!(
    PYLON_API_BASE,
    r"\bclass\s+\w+\s*\(\s*[^)]*?(?:APIBase|MethodView|Resource)\b"
);
pattern!(
    PYLON_URL_PARAMS,
    r"\burl_params\s*=\s*\[(?P<body>.*?)\]",
    "s"
);
pattern!(
    PYLON_METHOD_DEF,
    r"^\s*def\s+(?P<m>get|post|put|patch|delete|head|options)\s*\(",
    "mi"
);
pattern!(
    PYLON_ROUTE_FROM_PATH,
    r"(/api(?:/v\d+\w*)?/[A-Za-z0-9_/\-]+?)\.py$"
);
pattern!(PYLON_PARAM, r"<[^>]+>");
pattern!(PYLON_PARAM_NAMED, r"<(?:[^:>]+:)?(?P<name>[^>]+)>");
pattern!(PYLON_MOUNT, r"^(/api(?:/v\d+\w*)?)/(.+)$");
pattern!(PLUGIN_NAME, r"^[A-Za-z][A-Za-z0-9_\-]*$");

fn pylon_route_from_rel_path(rel_path: &str) -> String {
    if rel_path.is_empty() {
        return String::new();
    }
    let norm = format!("/{}", rel_path.trim_start_matches('/'));
    PYLON_ROUTE_FROM_PATH
        .captures(&norm)
        .map(|c| group_at(&c, 1).to_owned())
        .unwrap_or_default()
}

/// The `url_params = [...]` suffixes, each part rewritten by `param`.
fn pylon_suffixes(text: &str, param: impl Fn(&str) -> String) -> Vec<String> {
    let Some(pm) = PYLON_URL_PARAMS.captures(text) else {
        return vec![String::new()];
    };
    group(&pm, "body")
        .split(',')
        .map(|raw| {
            let tok = pystr::strip_chars(pystr::strip(raw), "'\"");
            if tok.is_empty() {
                String::new()
            } else {
                format!("/{}", param(tok).trim_start_matches('/'))
            }
        })
        .collect()
}

/// `_match_pylon_api`: a Pylon / Flask class API (`APIBase`, `MethodView`,
/// `Resource`), routed by its file path, one surface per verb method.
fn match_pylon_api(
    symbol_type: &str,
    text: &str,
    rel_path: &str,
    plugin_name: &str,
) -> Vec<ApiSurface> {
    if symbol_type.to_lowercase() != "class" || text.is_empty() || !PYLON_API_BASE.is_match(text) {
        return Vec::new();
    }
    let base = pylon_route_from_rel_path(rel_path);
    if base.is_empty() {
        return Vec::new();
    }
    let suffixes = pylon_suffixes(text, |tok| {
        PYLON_PARAM.replace_all(tok, "{var}").into_owned()
    });
    // A Python set: its order is the hash seed's. Sorted here; the order
    // reaches only the edge insertion order, not the rows.
    let methods: BTreeSet<String> = PYLON_METHOD_DEF
        .captures_iter(text)
        .map(|m| group(&m, "m").to_uppercase())
        .collect();
    if methods.is_empty() {
        return Vec::new();
    }
    let mut bases = vec![base.clone()];
    if !plugin_name.is_empty()
        && let Some(mm) = PYLON_MOUNT.captures(&base)
    {
        let rest = group_at(&mm, 2);
        if !rest.starts_with(&format!("{plugin_name}/")) && rest != plugin_name {
            bases.push(format!("{}/{plugin_name}/{rest}", group_at(&mm, 1)));
        }
    }
    let mut out = Vec::new();
    let mut seen: HashSet<(String, String)> = HashSet::new();
    for method in &methods {
        for base in &bases {
            for suffix in &suffixes {
                let full = format!("{base}{suffix}");
                for surface in emit_rest(method, &full, 0.65, "", "server") {
                    if seen.insert((surface.kind.clone(), surface.surface.clone())) {
                        out.push(surface);
                    }
                }
            }
        }
    }
    out
}

/// `plugin_name_from_metadata_text`: the `name` of a Pylon plugin's
/// `metadata.json` (its URL mount segment), or `""`.
#[must_use]
pub fn plugin_name_from_metadata_text(text: &str) -> String {
    if text.is_empty() {
        return String::new();
    }
    let mut payload = text;
    if payload.starts_with("[File:")
        && let Some(newline) = payload.find('\n')
    {
        payload = &payload[newline + 1..];
    }
    let Ok(Value::Object(meta)) = serde_json::from_str::<Value>(payload) else {
        return String::new();
    };
    // `(meta.get("name") or "").strip()`: a falsy name is "", a non-string
    // truthy one raises (caught: "").
    let name = match meta.get("name") {
        Some(Value::String(name)) => pystr::strip(name),
        _ => "",
    };
    if PLUGIN_NAME.is_match(name) {
        name.to_owned()
    } else {
        String::new()
    }
}

/// `derive_pylon_endpoints`: the deployed REST endpoints of a Pylon class
/// API (`/api/v{N}/{plugin}/{file}{url_params}`), for the page writer.
#[must_use]
pub fn derive_pylon_endpoints(rel_path: &str, source_text: &str, plugin_name: &str) -> Vec<String> {
    if source_text.is_empty() || !PYLON_API_BASE.is_match(source_text) {
        return Vec::new();
    }
    let base = pylon_route_from_rel_path(rel_path);
    if base.is_empty() {
        return Vec::new();
    }
    let mut deployed = base.clone();
    if !plugin_name.is_empty()
        && let Some(mm) = PYLON_MOUNT.captures(&base)
        && !group_at(&mm, 2).starts_with(&format!("{plugin_name}/"))
    {
        deployed = format!("{}/{plugin_name}/{}", group_at(&mm, 1), group_at(&mm, 2));
    }
    let suffixes = pylon_suffixes(source_text, |tok| {
        PYLON_PARAM_NAMED
            .replace_all(tok, |c: &Captures<'_>| format!("{{{}}}", group(c, "name")))
            .into_owned()
    });
    let methods: BTreeSet<String> = PYLON_METHOD_DEF
        .captures_iter(source_text)
        .map(|m| group(&m, "m").to_uppercase())
        .collect();
    let mut out: IndexSet<String> = IndexSet::new();
    for method in &methods {
        for suffix in &suffixes {
            out.insert(format!("{method} {deployed}{suffix}"));
        }
    }
    out.into_iter().collect()
}

// ── Dispatcher ──────────────────────────────────────────────────────────

/// What `extract_api_surfaces` reads of one node.
#[derive(Debug, Clone, Copy, Default)]
pub struct NodeView<'a> {
    pub language: &'a str,
    pub symbol_name: &'a str,
    pub symbol_type: &'a str,
    pub rel_path: &'a str,
    /// The node's `source_text` attribute as the orchestrator left it
    /// (file slice and router-prefix line applied).
    pub source_text: &'a str,
    /// The parser symbol's own text, the fallback for an empty attribute.
    pub symbol_text: &'a str,
}

/// `extract_api_surfaces`: every surface of one node, de-duplicated by
/// `(kind, surface)`, first wins. `None` where Python raises (the caller
/// then keeps no surface for the node).
#[must_use]
pub fn extract_api_surfaces(
    node: &NodeView<'_>,
    plugin_name: &str,
    grpc_bindings: Option<&IndexMap<String, String>>,
) -> Option<Vec<ApiSurface>> {
    let text = if node.source_text.is_empty() {
        node.symbol_text
    } else {
        node.source_text
    };
    if text.is_empty() {
        return Some(Vec::new());
    }
    let language = node.language.to_lowercase();
    let language = language.as_str();
    let mut surfaces = match_rest(language, text);
    surfaces.extend(match_grpc(text, language));
    surfaces.extend(match_grpc_client(text, language, grpc_bindings));
    surfaces.extend(match_graphql(text));
    surfaces.extend(match_ffi(text, node.symbol_name));
    surfaces.extend(match_objects(text, language));
    surfaces.extend(match_bdd(text, node.rel_path));
    surfaces.extend(match_cli(text)?);
    surfaces.extend(match_pylon_api(
        node.symbol_type,
        node.source_text,
        node.rel_path,
        plugin_name,
    ));
    let mut seen: HashSet<(String, String)> = HashSet::new();
    surfaces.retain(|s| seen.insert((s.kind.clone(), s.surface.clone())));
    Some(surfaces)
}

// ── Phase 1c orchestrator ───────────────────────────────────────────────

/// The per-file reads of one pass, cached as Python caches them.
struct FileScans {
    texts: TextCache,
    /// Line spans (`splitlines()`) of the file last sliced, for the slice
    /// fallback. One file only: nodes come file by file.
    lines: Option<(String, Vec<(usize, usize)>)>,
    router_prefix: HashMap<String, String>,
    /// Per file: each `ctypes` library variable (sorted) with its compiled
    /// call pattern, built once per file rather than once per node.
    ctypes_libs: HashMap<String, Rc<[(String, Regex)]>>,
    grpc_bindings: HashMap<String, IndexMap<String, String>>,
}

impl FileScans {
    fn new(repo_root: Option<&str>) -> Self {
        Self {
            texts: TextCache::new(repo_root),
            lines: None,
            router_prefix: HashMap::new(),
            ctypes_libs: HashMap::new(),
            grpc_bindings: HashMap::new(),
        }
    }

    /// `_file_slice`: lines `[start..=end]` (1-based) joined by `\n`.
    fn slice(&mut self, rel_path: &str, start: i64, end: i64) -> String {
        if rel_path.is_empty() || !self.texts.has_root() || start <= 0 || end < start {
            return String::new();
        }
        if self.lines.as_ref().is_none_or(|(path, _)| path != rel_path) {
            let spans = self
                .texts
                .text(rel_path)
                .map_or_else(Vec::new, pystr::splitline_spans);
            self.lines = Some((rel_path.to_owned(), spans));
        }
        let (Some((_, spans)), Some(text)) = (self.lines.as_ref(), self.texts.text(rel_path))
        else {
            return String::new();
        };
        let s = usize::try_from(start - 1).unwrap_or(0);
        let e = usize::try_from(end).unwrap_or(usize::MAX).min(spans.len());
        if e <= s {
            return String::new();
        }
        spans[s..e]
            .iter()
            .map(|&(a, b)| &text[a..b])
            .collect::<Vec<_>>()
            .join("\n")
    }

    fn python_router_prefix(&mut self, rel_path: &str) -> String {
        if rel_path.is_empty() || !self.texts.has_root() {
            return String::new();
        }
        if let Some(prefix) = self.router_prefix.get(rel_path) {
            return prefix.clone();
        }
        let prefix = self
            .texts
            .text(rel_path)
            .and_then(|text| PY_ROUTER_PREFIX.captures(text))
            .map(|c| group_at(&c, 1).to_owned())
            .unwrap_or_default();
        self.router_prefix
            .insert(rel_path.to_owned(), prefix.clone());
        prefix
    }

    fn ctypes_lib_vars(&mut self, rel_path: &str) -> Rc<[(String, Regex)]> {
        if rel_path.is_empty() || !self.texts.has_root() {
            return Rc::from(Vec::new());
        }
        if let Some(vars) = self.ctypes_libs.get(rel_path) {
            return Rc::clone(vars);
        }
        let vars: BTreeSet<String> = self
            .texts
            .text(rel_path)
            .map(|text| {
                PY_CTYPES_LIB
                    .captures_iter(text)
                    .map(|c| group(&c, "var").to_owned())
                    .collect()
            })
            .unwrap_or_default();
        let compiled: Rc<[(String, Regex)]> = vars
            .into_iter()
            .map(|lib| {
                let call = compile(
                    &format!(r"\b{}\.(?P<fn>[A-Za-z_]\w*)\s*\(", regex::escape(&lib)),
                    "",
                );
                (lib, call)
            })
            .collect();
        self.ctypes_libs
            .insert(rel_path.to_owned(), Rc::clone(&compiled));
        compiled
    }

    /// `_grpc_stub_bindings`: cached by path, whatever language asks first.
    fn grpc_bindings(
        &mut self,
        rel_path: &str,
        language: &str,
    ) -> Option<&IndexMap<String, String>> {
        if rel_path.is_empty()
            || !self.texts.has_root()
            || !GRPC_CLIENT_LANGUAGES.contains(&language)
        {
            return None;
        }
        if !self.grpc_bindings.contains_key(rel_path) {
            let bindings = self
                .texts
                .text(rel_path)
                .map(|text| extract_grpc_stub_bindings(text, language))
                .unwrap_or_default();
            self.grpc_bindings.insert(rel_path.to_owned(), bindings);
        }
        self.grpc_bindings.get(rel_path)
    }
}

/// The Pylon plugin name from the first `metadata.json` node that has one.
fn plugin_name_of(graph: &CodeGraph) -> String {
    graph
        .nodes()
        .filter(|(_, data)| data.rel_path == "metadata.json")
        .map(|(_, data)| plugin_name_from_metadata_text(&data.source_text))
        .find(|name| !name.is_empty())
        .unwrap_or_default()
}

/// The surfaces of one node, as `extract_api_surfaces_for_graph`'s loop
/// body computes them.
fn node_surfaces(data: &NodeData, scans: &mut FileScans, plugin_name: &str) -> Vec<ApiSurface> {
    let language = data.language.to_lowercase();
    // `data["source_text"]` through the loop: the slice fallback, then the
    // router-prefix line.
    let mut source_text = data.source_text.clone();
    if source_text.is_empty() && !data.rel_path.is_empty() {
        source_text = scans.slice(&data.rel_path, data.start_line, data.end_line);
    }
    if language == "python" {
        let prefix = scans.python_router_prefix(&data.rel_path);
        if !prefix.is_empty() {
            source_text = format!("router = APIRouter(prefix=\"{prefix}\")\n{source_text}");
        }
    }
    let symbol_text = data
        .symbol
        .as_deref()
        .and_then(|s| s.source_text.as_deref())
        .unwrap_or("");
    let view = NodeView {
        language: &data.language,
        symbol_name: &data.symbol_name,
        symbol_type: &data.symbol_type,
        rel_path: &data.rel_path,
        source_text: &source_text,
        symbol_text,
    };
    let bindings = scans.grpc_bindings(&data.rel_path, &language);
    let Some(mut surfaces) = extract_api_surfaces(&view, plugin_name, bindings) else {
        return Vec::new();
    };
    if language == "python" && !source_text.is_empty() {
        let lib_vars = scans.ctypes_lib_vars(&data.rel_path);
        let mut seen: HashSet<String> = surfaces
            .iter()
            .filter(|s| s.kind == "ffi")
            .map(|s| s.surface.clone())
            .collect();
        for (lib, call) in lib_vars.iter() {
            for m in call.captures_iter(&source_text) {
                let function = group(&m, "fn");
                let key = format!("ffi:{function}");
                if !seen.insert(key.clone()) {
                    continue;
                }
                surfaces.push(ApiSurface::new(
                    "ffi",
                    key,
                    0.6,
                    json!({"symbol": function, "via": format!("{lib} = ctypes.CDLL(...)")}),
                ));
            }
        }
    }
    surfaces
}

/// `extract_api_surfaces_for_graph`: the surfaces of every node, also
/// stored on the node as its `api_surface` attribute.
///
/// `repo_root` enables the file reads (slice fallback, router prefixes,
/// `ctypes` libraries, file-wide gRPC stub bindings).
pub fn extract_api_surfaces_for_graph(
    graph: &mut CodeGraph,
    repo_root: Option<&str>,
) -> SurfacesByNode {
    let mut scans = FileScans::new(repo_root);
    let plugin_name = plugin_name_of(graph);
    let mut out = SurfacesByNode::new();
    for (id, data) in graph.nodes() {
        let surfaces = node_surfaces(data, &mut scans, &plugin_name);
        if !surfaces.is_empty() {
            out.insert(id.to_owned(), surfaces);
        }
    }
    for (id, surfaces) in &out {
        if let Some(data) = graph.node_mut(id) {
            data.extra.insert(
                "api_surface".to_owned(),
                Value::Array(surfaces.iter().map(ApiSurface::to_json).collect()),
            );
        }
    }
    out
}

/// `_contract_node_id`.
#[must_use]
pub fn contract_node_id(kind: &str, surface: &str) -> String {
    format!("contract::{kind}::{surface}")
}

/// `materialize_contract_nodes`: one `contract` node per distinct `(kind,
/// surface)` — its attributes from the FIRST owner in node order — and a
/// `defines` (server) or `consumes` (client) edge from every owner.
/// Returns the number of contract nodes added.
pub fn materialize_contract_nodes(
    graph: &mut CodeGraph,
    surfaces_by_node: &SurfacesByNode,
) -> usize {
    let mut added = 0;
    for (owner, surfaces) in surfaces_by_node {
        let Some(owner_data) = graph.node(owner) else {
            continue;
        };
        let rel_path = owner_data.rel_path.clone();
        let file_name = owner_data.file_name.clone();
        let language = owner_data.language.clone();
        for surface in surfaces {
            let id = contract_node_id(&surface.kind, &surface.surface);
            if !graph.has_node(&id) {
                let mut extra = Map::new();
                extra.insert("is_architectural".to_owned(), Value::Bool(true));
                extra.insert("is_doc".to_owned(), Value::Bool(false));
                graph.add_node(
                    &id,
                    NodeData {
                        symbol_name: surface.surface.clone(),
                        symbol_type: "contract".into(),
                        signature: surface.kind.clone(),
                        rel_path: rel_path.clone(),
                        file_name: file_name.clone(),
                        language: language.clone(),
                        extra: extra.into(),
                        ..NodeData::default()
                    },
                );
                added += 1;
            }
            let mut via: Vec<Value> = Vec::new();
            for (key, label) in [
                ("method", "dispatch"),
                ("path", "route"),
                ("url_params", "url_params"),
                ("symbol", "symbol"),
            ] {
                let value = surface.meta_str(key);
                if !value.is_empty() {
                    via.push(Value::String(format!("{label}={value}")));
                }
            }
            let mut annotations = Map::new();
            if !via.is_empty() {
                annotations.insert("via".to_owned(), Value::Array(via));
            }
            let role = surface.meta_str("role");
            let client = role.to_lowercase() == "client";
            annotations.insert("obj_kind".to_owned(), Value::String(surface.kind.clone()));
            annotations.insert(
                "confidence".to_owned(),
                Value::String(if client { "INFERRED" } else { "EXTRACTED" }.to_owned()),
            );
            graph.add_edge(
                owner,
                &id,
                EdgeData {
                    rel_type: if client { "consumes" } else { "defines" }.into(),
                    language: language.to_string(),
                    annotations: annotations.into(),
                    ..EdgeData::default()
                },
            );
        }
    }
    added
}

#[cfg(test)]
mod tests;
