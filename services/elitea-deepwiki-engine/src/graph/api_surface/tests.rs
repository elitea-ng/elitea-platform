use super::*;
use crate::graph::EdgeKey;

/// Written by `parity/python_phase1c_cases.py` from the Python engine.
const CASES: &str = include_str!("../../../tests/fixtures/phase1c/api_surface_cases.json");

fn fixture() -> Value {
    serde_json::from_str(CASES).unwrap()
}

fn text<'a>(case: &'a Value, key: &str) -> &'a str {
    case[key].as_str().unwrap_or("")
}

#[test]
fn every_constant_pattern_compiles() {
    for re in [
        &*PY_REST_DECORATOR,
        &*TS_REQUEST_OPTIONS,
        &*TS_REQUEST_OPTIONS_REVERSED,
        &*OBJ_JAVA_FIELD,
        &*OBJ_RUST_FIELD,
        &*OBJ_CSHARP_FIELD,
        &*CLI_COBRA,
        &*JS_GRPC_HANDLER_MAP,
        &*GQL_FIELD,
        &*PYLON_PARAM_NAMED,
        &*API_PREFIX,
    ] {
        assert!(!re.as_str().is_empty());
    }
}

// Every matcher, against the Python engine's answers.
#[test]
fn surfaces_match_the_python_extractor() {
    let fixture = fixture();
    let cases = fixture["cases"].as_array().unwrap();
    assert!(cases.len() > 40);
    for case in cases {
        let bindings: Option<IndexMap<String, String>> = case["bindings"].as_object().map(|map| {
            map.iter()
                .map(|(k, v)| (k.clone(), v.as_str().unwrap().to_owned()))
                .collect()
        });
        let node = NodeView {
            language: text(case, "language"),
            symbol_name: text(case, "symbol_name"),
            symbol_type: text(case, "symbol_type"),
            rel_path: text(case, "rel_path"),
            source_text: text(case, "source_text"),
            symbol_text: "",
        };
        let got = extract_api_surfaces(&node, text(case, "plugin_name"), bindings.as_ref())
            .map(|found| Value::Array(found.iter().map(ApiSurface::to_json).collect()));
        let expected = &case["expected"];
        assert_eq!(
            got.as_ref().unwrap_or(&Value::Null),
            expected,
            "case {}",
            text(case, "rel_path")
        );
    }
}

#[test]
fn helpers_match_python() {
    let fixture = fixture();
    for (name, snake) in fixture["to_snake"].as_object().unwrap() {
        assert_eq!(to_snake(name), snake.as_str().unwrap(), "{name}");
    }
    for (path, stripped) in fixture["strip_prefix"].as_object().unwrap() {
        assert_eq!(
            strip_common_api_prefix(path),
            stripped.as_str().unwrap(),
            "{path}"
        );
    }
    for (text, name) in fixture["plugin_names"].as_object().unwrap() {
        assert_eq!(
            plugin_name_from_metadata_text(text),
            name.as_str().unwrap(),
            "{text}"
        );
    }
    let pylon = fixture["cases"]
        .as_array()
        .unwrap()
        .iter()
        .find(|c| c["rel_path"] == "plugins/configurations/api/v1/configurations.py")
        .unwrap();
    let endpoints = derive_pylon_endpoints(
        "plugins/configurations/api/v1/configurations.py",
        text(pylon, "source_text"),
        "configurations",
    );
    let expected: Vec<String> = serde_json::from_value(fixture["pylon_endpoints"].clone()).unwrap();
    assert_eq!(endpoints, expected);
}

#[test]
fn a_blank_cobra_use_drops_every_surface_of_the_node() {
    let node = NodeView {
        language: "go",
        source_text: "r.GET(\"/x\", h)\nc := &cobra.Command{ Use: \" \" }",
        ..NodeView::default()
    };
    assert_eq!(extract_api_surfaces(&node, "", None), None);
}

fn rich_node(
    graph: &mut CodeGraph,
    id: &str,
    language: &str,
    rel_path: &str,
    lines: (i64, i64),
    text: &str,
) {
    let mut symbol = crate::parsers::model::Symbol::new(
        "f",
        crate::parsers::model::SymbolType::Function,
        crate::parsers::model::Scope::Global,
        crate::parsers::model::Range::new(1, 0, 1, 0),
        rel_path,
    );
    symbol.source_text = Some(text.to_owned());
    graph.add_node(
        id,
        NodeData {
            rel_path: rel_path.into(),
            file_name: pystr::stem(rel_path).into(),
            language: language.into(),
            symbol_name: "f".to_owned(),
            symbol_type: "function".into(),
            start_line: lines.0,
            end_line: lines.1,
            symbol: Some(Box::new(symbol.into())),
            ..NodeData::default()
        },
    );
}

// A rich node reads its file slice from disk (not its symbol text), a
// Python file's router prefix is prepended, ctypes calls become FFI
// surfaces; contracts take their first owner's attributes.
#[test]
fn the_orchestrator_reads_files_and_materializes_contracts() {
    let dir = std::env::temp_dir().join(format!("dw-api-surface-{}", std::process::id()));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(dir.join("app")).unwrap();
    std::fs::write(
        dir.join("app/users.py"),
        "import ctypes\nlib = ctypes.CDLL('x')\nrouter = APIRouter(prefix=\"/users\")\n\n@router.get(\"/me\")\ndef me():\n    return lib.compute_hash(1)\n",
    )
    .unwrap();
    std::fs::write(
        dir.join("app/client.ts"),
        "export function load() {\n  return axios.get('/api/v1/users/me');\n}\n",
    )
    .unwrap();
    let root = dir.to_str().unwrap();
    let mut graph = CodeGraph::new();
    rich_node(
        &mut graph,
        "python::users::me",
        "python",
        "app/users.py",
        (5, 7),
        "ignored",
    );
    rich_node(
        &mut graph,
        "typescript::client::load",
        "typescript",
        "app/client.ts",
        (1, 3),
        "",
    );
    // No slice (line 0): the symbol's own text serves.
    rich_node(
        &mut graph,
        "go::x::f",
        "go",
        "app/x.go",
        (0, 0),
        "r.GET(\"/users/me\", h)",
    );

    let surfaces = extract_api_surfaces_for_graph(&mut graph, Some(root));
    let keys: Vec<(&str, Vec<&str>)> = surfaces
        .iter()
        .map(|(id, s)| (id.as_str(), s.iter().map(|s| s.surface.as_str()).collect()))
        .collect();
    assert_eq!(
        keys,
        [
            (
                "python::users::me",
                vec!["GET /users/me", "ffi:compute_hash"]
            ),
            (
                "typescript::client::load",
                vec!["GET /api/v1/users/me", "GET /users/me"]
            ),
            ("go::x::f", vec!["GET /users/me"]),
        ]
    );
    assert!(
        graph
            .node("go::x::f")
            .unwrap()
            .extra
            .contains_key("api_surface")
    );

    assert_eq!(materialize_contract_nodes(&mut graph, &surfaces), 3);
    let contract = graph.node("contract::rest::GET /users/me").unwrap();
    assert_eq!(
        (
            contract.rel_path.as_str(),
            contract.language.as_str(),
            contract.signature.as_str()
        ),
        ("app/users.py", "python", "rest")
    );
    let edge = graph
        .edges_between("typescript::client::load", "contract::rest::GET /users/me")
        .next()
        .unwrap();
    assert_eq!(edge.0, &EdgeKey::Auto(0));
    assert_eq!(edge.1.rel_type, "consumes");
    assert_eq!(
        crate::pyjson::dumps(&Value::Object(edge.1.annotations.to_map())),
        r#"{"via": ["dispatch=GET", "route=/users/me"], "obj_kind": "rest", "confidence": "INFERRED"}"#
    );
    let ffi = graph
        .edges_between("python::users::me", "contract::ffi::ffi:compute_hash")
        .next()
        .unwrap()
        .1;
    assert_eq!(ffi.rel_type, "defines");
    assert_eq!(ffi.language, "python");
    std::fs::remove_dir_all(&dir).unwrap();
}
