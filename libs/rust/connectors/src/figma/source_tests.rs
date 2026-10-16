//! The Figma connector against recorded REST answers (no network), in the
//! shapes Figma documents: `GET /v1/files/:key` (`name`, `lastModified`,
//! `version`, `document.children`), `GET /v1/files/:key/nodes?ids=` (`nodes`
//! keyed by id, each `{document}` or `null`), `GET /v1/projects/:id/files`,
//! `GET /v1/teams/:id/projects` and `GET /v1/files/:key/comments`.

use std::collections::BTreeSet;
use std::sync::Arc;
use std::time::Duration;

use elitea_content_source::{ContentSource, SourceError};
use serde_json::{Value, json};

use super::config::{FigmaConnectorConfig, FigmaIndexArguments};
use super::source::FigmaSource;
use crate::egress::HostAllowlist;
use crate::source::SourceLimits;
use crate::source::fixture::{Reply, Scripted, query};
use crate::transport::{Request, StatusCode};

const TOKEN: &str = "figd_fixture_token";

fn config() -> FigmaConnectorConfig {
    FigmaConnectorConfig::parse(
        json!({"figma_configuration": {"token": TOKEN}})
            .as_object()
            .unwrap(),
    )
    .expect("config")
}

fn arguments(value: &Value) -> FigmaIndexArguments {
    FigmaIndexArguments::parse(value.as_object().unwrap()).expect("arguments")
}

fn allow() -> HostAllowlist {
    HostAllowlist::parse(Some("api.figma.com"))
}

fn figma(transport: &Arc<Scripted>, args: &Value) -> FigmaSource {
    FigmaSource::new(config(), arguments(args), allow(), transport.clone()).expect("source")
}

/// A text node as Figma returns it.
fn text(characters: &str, size: f64, weight: f64) -> Value {
    json!({
        "id": "t:0", "name": "Text", "type": "TEXT", "characters": characters,
        "style": {"fontSize": size, "fontWeight": weight, "fontFamily": "Inter"},
        "fills": [{"type": "SOLID", "color": {"r": 0.1, "g": 0.1, "b": 0.1, "a": 1.0}}],
        "absoluteBoundingBox": {"x": 0.0, "y": 0.0, "width": 80.0, "height": 20.0},
    })
}

#[allow(clippy::needless_pass_by_value, reason = "fixtures pass vec! literals")]
fn frame(id: &str, name: &str, width: f64, height: f64, children: Vec<Value>) -> Value {
    json!({
        "id": id, "name": name, "type": "FRAME",
        "absoluteBoundingBox": {"x": 10.0, "y": 20.0, "width": width, "height": height},
        "children": children,
    })
}

fn login() -> Value {
    frame(
        "1:2",
        "Login",
        375.0,
        812.0,
        vec![
            text("Welcome back", 24.0, 700.0),
            text("Sign in to continue", 14.0, 400.0),
            json!({"id": "9:9", "name": "Primary Button", "type": "INSTANCE", "children": [text("Sign In", 16.0, 600.0)]}),
        ],
    )
}

fn dashboard() -> Value {
    frame(
        "1:3",
        "Dashboard",
        1440.0,
        900.0,
        vec![text("Usage this month", 20.0, 700.0)],
    )
}

/// One recorded file: pages by id, each a `CANVAS` node with its frames.
#[derive(Clone)]
struct File {
    name: &'static str,
    pages: Vec<(&'static str, &'static str, Vec<Value>)>,
}

impl File {
    fn shallow(&self) -> Value {
        json!({
            "name": self.name, "role": "owner", "lastModified": "2026-10-01T09:00:00Z",
            "editorType": "figma", "version": "4242", "thumbnailUrl": "https://s3.example/t.png",
            "document": {"id": "0:0", "name": "Document", "type": "DOCUMENT", "children":
                self.pages.iter().map(|(id, name, _)| json!({"id": id, "name": name, "type": "CANVAS", "children": []})).collect::<Vec<_>>()},
        })
    }

    fn page(&self, id: &str) -> Option<Value> {
        self.pages.iter().find(|page| page.0 == id).map(|(id, name, frames)| {
            json!({"id": id, "name": name, "type": "CANVAS", "children": frames})
        })
    }

    /// `GET /v1/files/K/nodes?ids=…` over pages and, for a frame id, the frame.
    fn nodes(&self, ids: &str) -> Value {
        let mut nodes = serde_json::Map::new();
        for id in ids.split(',') {
            let document = self.page(id).or_else(|| {
                self.pages
                    .iter()
                    .flat_map(|page| page.2.iter())
                    .find(|frame| frame["id"] == id)
                    .cloned()
            });
            nodes.insert(
                id.to_owned(),
                document.map_or(
                    Value::Null,
                    |document| json!({"document": document, "components": {}}),
                ),
            );
        }
        json!({"name": self.name, "lastModified": "2026-10-01T09:00:00Z", "version": "4242", "nodes": nodes})
    }
}

fn one_file() -> File {
    File {
        name: "Mobile App",
        pages: vec![
            ("0:1", "Auth", vec![login()]),
            ("0:2", "Home", vec![dashboard()]),
        ],
    }
}

fn serve(file: File) -> Arc<Scripted> {
    Scripted::new(move |request, _| match request.url().path() {
        "/v1/files/K1" => Reply::json(&file.shallow()),
        "/v1/files/K1/nodes" => Reply::json(&file.nodes(&query(request, "ids").expect("ids"))),
        other => panic!("unexpected request {other}"),
    })
}

fn keys(references: &[elitea_content_source::DocumentRef]) -> Vec<&str> {
    references.iter().map(|r| r.key.as_str()).collect()
}

#[tokio::test]
async fn frames_are_listed_and_rendered_like_the_sdk_header_and_text_by_role() {
    let transport = serve(one_file());
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    let listed = source.list().await.expect("listing");
    assert_eq!(
        keys(&listed),
        vec!["file/K1/frame/1-2.txt", "file/K1/frame/1-3.txt"]
    );
    assert!(
        listed
            .iter()
            .all(|r| r.mime == "text/plain" && r.version.len() == 64)
    );
    let login = source.fetch("file/K1/frame/1-2.txt").await.expect("login");
    assert_eq!(
        String::from_utf8(login.bytes.clone()).unwrap(),
        "FRAME: Login [10,20 375x812] #1:2\n  Headings: Welcome back\n  Buttons: Sign In\n  Body: Sign in to continue"
    );
    assert_eq!(login.title, "Login");
    assert_eq!(
        login.uri.as_deref(),
        Some("https://www.figma.com/design/K1?node-id=1-2")
    );
    for (name, value) in [
        ("file_key", json!("K1")),
        ("file_name", json!("Mobile App")),
        ("page_id", json!("0:1")),
        ("page_name", json!("Auth")),
        ("node_id", json!("1:2")),
        ("node_name", json!("Login")),
        ("image_width", json!(375)),
        ("image_height", json!(812)),
        ("type", json!("image")),
    ] {
        assert_eq!(login.metadata[name], value, "{name}");
    }
    // Figma's file `version` and `lastModified` move on every save and are
    // not part of any frame's version, so they are not stored.
    for volatile in ["updated_on", "file_version", "last_modified", "version"] {
        assert!(!login.metadata.contains_key(volatile), "{volatile}");
    }
    assert!(
        !login.metadata.contains_key("text"),
        "the body is the bytes"
    );
    assert_eq!(login.reference.size, login.bytes.len() as u64);
    // Pages come from one shallow read, then their content in one batch; the
    // fetches read nothing.
    assert_eq!(
        transport.seen(),
        vec![
            "GET /v1/files/K1?depth=1",
            "GET /v1/files/K1/nodes?ids=0%3A1%2C0%3A2"
        ]
    );
    assert!(matches!(
        source.fetch("file/K1/frame/9-9.txt").await,
        Err(SourceError::NotFound(_))
    ));
}

#[tokio::test]
async fn the_token_goes_to_api_figma_com_only() {
    let transport = serve(one_file());
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    source.list().await.expect("listing");
    let requests = transport.requests();
    assert!(!requests.is_empty());
    for request in &requests {
        assert_eq!(request.url().scheme(), "https");
        assert_eq!(request.url().host_str(), Some("api.figma.com"));
        let token = request.headers().get("x-figma-token").expect("token");
        assert_eq!(token.to_str().unwrap(), TOKEN);
        assert!(token.is_sensitive());
        assert!(request.headers().get("authorization").is_none());
        assert!(!request.url().as_str().contains(TOKEN));
    }
    // Without api.figma.com on the allowlist nothing is built, let alone sent.
    let refused = FigmaSource::new(
        config(),
        arguments(&json!({"urls_or_file_keys": "K1"})),
        HostAllowlist::parse(Some("example.com")),
        transport.clone(),
    )
    .err()
    .expect("refused");
    assert!(
        refused.to_string().contains("egress allowlist"),
        "{refused}"
    );
    assert_eq!(transport.requests().len(), requests.len());
}

#[tokio::test]
async fn pages_are_selected_by_url_include_and_exclude_and_frames_by_type() {
    let file = File {
        name: "Mobile App",
        pages: vec![
            ("0:1", "Auth", vec![login()]),
            ("0:2", "Home", vec![dashboard()]),
            (
                "0:3",
                "Misc",
                vec![json!({
                    "id": "1:9", "name": "Card", "type": "COMPONENT",
                    "absoluteBoundingBox": {"x": 0.0, "y": 0.0, "width": 200.0, "height": 100.0},
                    "children": [text("Card title", 18.0, 700.0)],
                })],
            ),
        ],
    };
    let list = |args: Value| {
        let transport = serve(file.clone());
        let source = figma(&transport, &args);
        async move {
            let listed = source.list().await.expect("listing");
            (
                keys(&listed)
                    .into_iter()
                    .map(str::to_owned)
                    .collect::<Vec<_>>(),
                transport.seen(),
            )
        }
    };
    // A URL's node-id selects that page (and the include list is ignored).
    let (found, seen) = list(json!({
        "urls_or_file_keys": "https://www.figma.com/design/K1/Mobile?node-id=0-2",
        "node_ids_include": ["0:1"],
    }))
    .await;
    assert_eq!(found, vec!["file/K1/frame/1-3.txt"]);
    assert_eq!(seen[1], "GET /v1/files/K1/nodes?ids=0%3A2");
    // The include list, in either spelling.
    let (found, _) = list(json!({"urls_or_file_keys": "K1", "node_ids_include": ["0-1"]})).await;
    assert_eq!(found, vec!["file/K1/frame/1-2.txt"]);
    // The exclude list applies when nothing is included.
    let (found, _) =
        list(json!({"urls_or_file_keys": "K1", "node_ids_exclude": ["0-1", "0:2"]})).await;
    assert_eq!(found, vec!["file/K1/frame/1-9.txt"]);
    // Node types filter a page's top-level nodes.
    let (found, _) =
        list(json!({"urls_or_file_keys": "K1", "node_types_include": ["component"]})).await;
    assert_eq!(found, vec!["file/K1/frame/1-9.txt"]);
    let (found, _) =
        list(json!({"urls_or_file_keys": "K1", "node_types_exclude": ["FRAME"]})).await;
    assert_eq!(found, vec!["file/K1/frame/1-9.txt"]);
}

#[tokio::test]
async fn empty_sizeless_and_textless_nodes_are_not_documents() {
    let sizeless = json!({"id": "2:1", "name": "Sizeless", "type": "FRAME", "children": [text("Hidden", 14.0, 400.0)]});
    let arrow = json!({"id": "2:2", "name": "Arrow", "type": "VECTOR",
        "absoluteBoundingBox": {"x": 0.0, "y": 0.0, "width": 50.0, "height": 5.0}});
    let empty = frame("2:3", "Empty", 100.0, 100.0, Vec::new());
    let label = json!({"id": "2:4", "name": "Page title", "type": "TEXT", "characters": "Overview",
        "style": {"fontSize": 32.0}, "absoluteBoundingBox": {"x": 0.0, "y": 0.0, "width": 90.0, "height": 40.0}});
    let file = File {
        name: "Doc",
        pages: vec![("0:1", "P", vec![sizeless, arrow, empty, label])],
    };
    let transport = serve(file);
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    let listed = source.list().await.expect("listing");
    assert_eq!(keys(&listed), vec!["file/K1/frame/2-4.txt"]);
    let doc = source
        .fetch("file/K1/frame/2-4.txt")
        .await
        .expect("text node");
    assert_eq!(doc.metadata["type"], "text");
    assert!(
        String::from_utf8(doc.bytes)
            .unwrap()
            .contains("Headings: Overview")
    );
}

#[tokio::test]
async fn a_frame_over_the_threshold_is_indexed_through_its_children() {
    let big = frame(
        "3:1",
        "Spec sheet",
        20_000.0,
        900.0,
        vec![
            json!({"id": "3:2", "name": "Caption", "type": "TEXT", "characters": "Screens overview",
                "style": {"fontSize": 20.0}, "absoluteBoundingBox": {"x": 0.0, "y": 0.0, "width": 300.0, "height": 30.0}}),
            frame(
                "3:3",
                "Screen A",
                375.0,
                812.0,
                vec![text("Screen A body", 14.0, 400.0)],
            ),
        ],
    );
    // Its only text has no size, so extraction finds no children to stand
    // for it and the frame itself is the (text) document.
    let bare = frame(
        "3:4",
        "Huge and bare",
        30_000.0,
        900.0,
        vec![
            json!({"id": "3:7", "name": "x", "type": "TEXT", "characters": "Only text", "style": {"fontSize": 14}}),
        ],
    );
    let childless = json!({"id": "3:5", "name": "Huge empty", "type": "FRAME",
        "absoluteBoundingBox": {"x": 0.0, "y": 0.0, "width": 30_000.0, "height": 10.0}, "children": []});
    let file = File {
        name: "Doc",
        pages: vec![("0:1", "P", vec![big, bare, childless])],
    };
    let transport = serve(file);
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    let listed = source.list().await.expect("listing");
    assert_eq!(
        keys(&listed),
        vec![
            "file/K1/frame/3-2.txt",
            "file/K1/frame/3-3.txt",
            "file/K1/frame/3-4.txt",
        ],
        "the parent of extracted children is not itself a document"
    );
    let kinds: Vec<Value> = {
        let mut kinds = Vec::new();
        for reference in &listed {
            let doc = source.fetch(&reference.key).await.expect("doc");
            kinds.push(doc.metadata["type"].clone());
        }
        kinds
    };
    assert_eq!(kinds, vec![json!("text"), json!("image"), json!("text")]);
}

#[tokio::test]
async fn versions_follow_the_indexed_text_so_a_save_rewrites_only_what_changed() {
    let versions = |file: File| async move {
        let transport = serve(file);
        let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
        source
            .list()
            .await
            .expect("listing")
            .into_iter()
            .map(|r| (r.key, r.version))
            .collect::<std::collections::BTreeMap<_, _>>()
    };
    let before = versions(one_file()).await;
    assert_eq!(versions(one_file()).await, before, "unchanged is unchanged");
    // One frame's text changes.
    let mut edited = one_file();
    edited.pages[1].2 = vec![frame(
        "1:3",
        "Dashboard",
        1440.0,
        900.0,
        vec![text("Usage this quarter", 20.0, 700.0)],
    )];
    let after = versions(edited).await;
    assert_eq!(
        before["file/K1/frame/1-2.txt"],
        after["file/K1/frame/1-2.txt"]
    );
    assert_ne!(
        before["file/K1/frame/1-3.txt"],
        after["file/K1/frame/1-3.txt"]
    );
    // A page rename (the frame's metadata) changes that page's frames only.
    let mut renamed = one_file();
    renamed.pages[0].1 = "Authentication";
    let after = versions(renamed).await;
    assert_ne!(
        before["file/K1/frame/1-2.txt"],
        after["file/K1/frame/1-2.txt"]
    );
    assert_eq!(
        before["file/K1/frame/1-3.txt"],
        after["file/K1/frame/1-3.txt"]
    );
    // A frame that disappears is simply not listed.
    let mut removed = one_file();
    removed.pages[1].2.clear();
    let after = versions(removed).await;
    assert_eq!(
        after.keys().map(String::as_str).collect::<Vec<_>>(),
        vec!["file/K1/frame/1-2.txt"]
    );
}

fn ids_of(request: &Request) -> Vec<String> {
    query(request, "ids")
        .map(|ids| ids.split(',').map(str::to_owned).collect())
        .unwrap_or_default()
}

#[tokio::test]
async fn pages_are_read_ten_to_a_request_and_a_failing_batch_is_split() {
    // Twelve pages of one frame each; Figma times out on any request for more
    // than two pages at once.
    let pages: Vec<(&'static str, &'static str, Vec<Value>)> = (1..=12)
        .map(|n| {
            let id: &'static str = Box::leak(format!("0:{n}").into_boxed_str());
            let frame_id = format!("{n}:1");
            (
                id,
                "P",
                vec![frame(
                    &frame_id,
                    &format!("F{n}"),
                    100.0,
                    100.0,
                    vec![text(&format!("Text {n}"), 14.0, 400.0)],
                )],
            )
        })
        .collect();
    let file = File { name: "Big", pages };
    let transport = Scripted::new({
        let file = file.clone();
        move |request, _| match request.url().path() {
            "/v1/files/K1" => Reply::json(&file.shallow()),
            "/v1/files/K1/nodes" if ids_of(request).len() > 2 => {
                Reply::status(StatusCode::GATEWAY_TIMEOUT)
            }
            "/v1/files/K1/nodes" => Reply::json(&file.nodes(&query(request, "ids").unwrap())),
            other => panic!("unexpected request {other}"),
        }
    });
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    let listed = source.list().await.expect("every page is read in the end");
    assert_eq!(listed.len(), 12);
    let sizes: Vec<usize> = transport
        .requests()
        .iter()
        .filter(|request| request.url().path().ends_with("/nodes"))
        .map(|request| ids_of(request).len())
        .collect();
    assert_eq!(sizes[0], 10, "the first request carries ten pages");
    assert_eq!(sizes[1], 5, "a failed batch splits in half");
    assert!(sizes.iter().filter(|size| **size > 2).count() >= 3);
}

#[tokio::test]
async fn a_page_figma_cannot_return_whole_is_read_as_its_frames() {
    let file = File {
        name: "Heavy",
        pages: vec![("0:1", "Huge page", vec![login(), dashboard()])],
    };
    let transport = Scripted::new({
        let file = file.clone();
        move |request, _| match request.url().path() {
            "/v1/files/K1" => Reply::json(&file.shallow()),
            "/v1/files/K1/nodes" => {
                let ids = query(request, "ids").unwrap();
                match (ids.as_str(), query(request, "depth").as_deref()) {
                    // The whole page times out; its shallow form answers.
                    ("0:1", None) => Reply::status(StatusCode::INTERNAL_SERVER_ERROR),
                    ("0:1", Some("1")) => Reply::json(&json!({"nodes": {"0:1": {"document": {
                        "id": "0:1", "name": "Huge page", "type": "CANVAS",
                        "children": [{"id": "1:2", "name": "Login", "type": "FRAME"},
                                     {"id": "1:3", "name": "Dashboard", "type": "FRAME"}]}}}})),
                    _ => Reply::json(&file.nodes(&ids)),
                }
            }
            other => panic!("unexpected request {other}"),
        }
    });
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    let listed = source.list().await.expect("assembled from frames");
    assert_eq!(
        keys(&listed),
        vec!["file/K1/frame/1-2.txt", "file/K1/frame/1-3.txt"]
    );
    let doc = source.fetch("file/K1/frame/1-3.txt").await.expect("doc");
    assert_eq!(doc.metadata["page_name"], "Huge page");

    // A frame that fails alone fails the listing: it must not look deleted.
    let transport = Scripted::new({
        let file = file.clone();
        move |request, _| match request.url().path() {
            "/v1/files/K1" => Reply::json(&file.shallow()),
            "/v1/files/K1/nodes" => {
                let ids = query(request, "ids").unwrap();
                let depth = query(request, "depth");
                if depth.is_none() && (ids == "0:1" || ids.split(',').any(|id| id == "1:3")) {
                    return Reply::status(StatusCode::INTERNAL_SERVER_ERROR);
                }
                match (ids.as_str(), depth.as_deref()) {
                    ("0:1", Some("1")) => Reply::json(&json!({"nodes": {"0:1": {"document": {
                        "id": "0:1", "name": "Huge page", "type": "CANVAS",
                        "children": [{"id": "1:2", "type": "FRAME"}, {"id": "1:3", "type": "FRAME"}]}}}})),
                    _ => Reply::json(&file.nodes(&ids)),
                }
            }
            other => panic!("unexpected request {other}"),
        }
    });
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    let error = source.list().await.expect_err("fails closed");
    assert!(
        error.to_string().contains("could not return a frame"),
        "{error}"
    );
}

#[tokio::test]
async fn a_refused_credential_or_missing_file_is_an_error_never_an_empty_listing() {
    for (status, expected) in [
        (
            StatusCode::FORBIDDEN,
            "refused the credential (HTTP 403) for file 'K1'",
        ),
        (
            StatusCode::UNAUTHORIZED,
            "refused the credential (HTTP 401)",
        ),
        (StatusCode::NOT_FOUND, "did not find file 'K1'"),
    ] {
        let transport = Scripted::new(move |_, _| Reply::status(status));
        let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
        let error = source.list().await.expect_err("an error");
        assert!(error.to_string().contains(expected), "{error}");
        assert!(!error.to_string().contains(TOKEN));
    }
    // A bad token on the nodes read, after the file read worked.
    let file = one_file();
    let transport = Scripted::new(move |request, _| match request.url().path() {
        "/v1/files/K1" => Reply::json(&file.shallow()),
        _ => Reply::status(StatusCode::FORBIDDEN),
    });
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    assert!(
        source
            .list()
            .await
            .expect_err("refused")
            .to_string()
            .contains("refused the credential")
    );
}

#[tokio::test(start_paused = true)]
async fn a_rate_limit_waits_for_retry_after_and_then_lists() {
    let file = one_file();
    let transport = Scripted::new(move |request, index| {
        if index == 0 {
            return Reply::rate_limited(Some("2"));
        }
        match request.url().path() {
            "/v1/files/K1" => Reply::json(&file.shallow()),
            _ => Reply::json(&file.nodes(&query(request, "ids").unwrap())),
        }
    });
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    let started = tokio::time::Instant::now();
    assert_eq!(source.list().await.expect("after the wait").len(), 2);
    assert_eq!(started.elapsed(), Duration::from_secs(2));
    assert_eq!(
        transport.seen().len(),
        3,
        "the limited request was repeated"
    );

    // Figma's Retry-After can be hours: that is not slept through.
    let transport = Scripted::new(|_, _| Reply::rate_limited(Some("3600")));
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    let error = source.list().await.expect_err("rate limited");
    assert!(error.to_string().contains("rate limited"), "{error}");
    assert_eq!(transport.seen().len(), 1, "no wait of an hour, no retry");
}

#[tokio::test]
async fn projects_and_teams_expand_to_their_files_once_each() {
    let file = one_file();
    let transport = Scripted::new(move |request, _| match request.url().path() {
        "/v1/teams/55/projects" => Reply::json(
            &json!({"name": "Team", "projects": [{"id": "7", "name": "Apps"}, {"id": 8, "name": "Web"}]}),
        ),
        "/v1/projects/7/files" => Reply::json(&json!({"name": "Apps", "files": [
            {"key": "K1", "name": "Mobile App", "thumbnail_url": "https://s3.example/t.png", "last_modified": "2026-10-01T09:00:00Z"}]})),
        "/v1/projects/8/files" => Reply::json(
            &json!({"name": "Web", "files": [{"key": "K1", "name": "Mobile App"}, {"key": "K2", "name": "Site"}]}),
        ),
        "/v1/files/K1" | "/v1/files/K2" => Reply::json(&file.shallow()),
        path if path.ends_with("/nodes") => {
            Reply::json(&file.nodes(&query(request, "ids").unwrap()))
        }
        other => panic!("unexpected request {other}"),
    });
    let source = figma(
        &transport,
        &json!({"urls_or_file_keys": "K1", "team_ids": ["55"]}),
    );
    let listed = source.list().await.expect("listing");
    let files: BTreeSet<&str> = listed
        .iter()
        .map(|r| r.key.split('/').nth(1).unwrap())
        .collect();
    assert_eq!(files, BTreeSet::from(["K1", "K2"]));
    assert_eq!(
        transport
            .seen()
            .iter()
            .filter(|r| r.starts_with("GET /v1/files/K1?"))
            .count(),
        1,
        "K1 is read once however many lists name it"
    );
}

#[tokio::test]
async fn comments_are_documents_of_their_own_pinned_to_their_frame() {
    let file = one_file();
    let resolved = Arc::new(std::sync::atomic::AtomicBool::new(false));
    let flag = Arc::clone(&resolved);
    let transport = Scripted::new(move |request, _| match request.url().path() {
        "/v1/files/K1" => Reply::json(&file.shallow()),
        "/v1/files/K1/nodes" => Reply::json(&file.nodes(&query(request, "ids").unwrap())),
        "/v1/files/K1/comments" => Reply::json(&json!({"comments": [
            {"id": "100", "file_key": "K1", "parent_id": "", "user": {"id": "u1", "handle": "Ada", "img_url": "https://s3.example/a.png"},
             "created_at": "2026-10-02T10:00:00Z",
             "resolved_at": if flag.load(std::sync::atomic::Ordering::SeqCst) { json!("2026-10-03T10:00:00Z") } else { Value::Null },
             "message": "Make the button larger", "client_meta": {"node_id": "1:2", "node_offset": {"x": 1, "y": 2}}, "order_id": "1"},
            {"id": "101", "user": {"handle": "Bo"}, "created_at": "2026-10-02T11:00:00Z", "message": "Overall looks good", "client_meta": {"x": 5, "y": 6}},
            {"id": "102", "user": {"handle": "Cy"}, "created_at": "2026-10-02T12:00:00Z", "message": ""},
        ]})),
        other => panic!("unexpected request {other}"),
    });
    // Comments are off unless asked for.
    let off = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    assert!(
        !keys(&off.list().await.unwrap())
            .iter()
            .any(|k| k.contains("comment"))
    );
    assert!(!transport.seen().iter().any(|r| r.contains("comments")));

    let source = figma(
        &transport,
        &json!({"urls_or_file_keys": "K1", "include_comments": true}),
    );
    let first = source.list().await.expect("listing");
    assert_eq!(
        keys(&first),
        vec![
            "file/K1/comment/100.txt",
            "file/K1/comment/101.txt",
            "file/K1/frame/1-2.txt",
            "file/K1/frame/1-3.txt",
        ]
    );
    let pinned = source
        .fetch("file/K1/comment/100.txt")
        .await
        .expect("comment");
    assert_eq!(
        String::from_utf8(pinned.bytes).unwrap(),
        "Comment by Ada on 2026-10-02T10:00:00Z\nMake the button larger"
    );
    assert_eq!(pinned.metadata["parent_key"], "file/K1/frame/1-2.txt");
    assert_eq!(pinned.metadata["node_id"], "1:2");
    assert_eq!(pinned.metadata["type"], "comment");
    assert_eq!(pinned.metadata["author"], "Ada");
    let loose = source
        .fetch("file/K1/comment/101.txt")
        .await
        .expect("comment");
    assert!(!loose.metadata.contains_key("parent_key"));

    // Resolving a comment rewrites that comment only.
    resolved.store(true, std::sync::atomic::Ordering::SeqCst);
    let second = source.list().await.expect("relisting");
    let version = |list: &[elitea_content_source::DocumentRef], key: &str| {
        list.iter().find(|r| r.key == key).unwrap().version.clone()
    };
    assert_ne!(
        version(&first, "file/K1/comment/100.txt"),
        version(&second, "file/K1/comment/100.txt")
    );
    assert_eq!(
        version(&first, "file/K1/comment/101.txt"),
        version(&second, "file/K1/comment/101.txt")
    );
    assert_eq!(
        version(&first, "file/K1/frame/1-2.txt"),
        version(&second, "file/K1/frame/1-2.txt")
    );
}

#[tokio::test]
async fn the_document_cap_and_the_size_cap_hold() {
    let transport = serve(one_file());
    let capped = figma(&transport, &json!({"urls_or_file_keys": "K1"})).with_limits(SourceLimits {
        max_documents: 1,
        ..SourceLimits::default()
    });
    let error = capped.list().await.expect_err("over the cap");
    assert!(
        error
            .to_string()
            .contains("more documents than the listing cap"),
        "{error}"
    );

    let long = "word ".repeat(400);
    let file = File {
        name: "Long",
        pages: vec![(
            "0:1",
            "P",
            vec![frame(
                "4:1",
                "Essay",
                400.0,
                400.0,
                vec![text(&long, 14.0, 400.0)],
            )],
        )],
    };
    let transport = serve(file);
    let small = figma(&transport, &json!({"urls_or_file_keys": "K1"})).with_limits(SourceLimits {
        max_document_bytes: 300,
        ..SourceLimits::default()
    });
    let listed = small.list().await.expect("listing");
    assert_eq!(
        listed[0].size, 300,
        "text over the document cap is cut, not refused"
    );
    assert_eq!(small.fetch(&listed[0].key).await.unwrap().bytes.len(), 300);
}

#[tokio::test]
async fn a_failed_listing_is_not_remembered() {
    let file = one_file();
    let first = Arc::new(std::sync::atomic::AtomicBool::new(true));
    let flag = Arc::clone(&first);
    let transport = Scripted::new(move |request, _| {
        if flag.swap(false, std::sync::atomic::Ordering::SeqCst) {
            return Reply::status(StatusCode::FORBIDDEN);
        }
        match request.url().path() {
            "/v1/files/K1" => Reply::json(&file.shallow()),
            _ => Reply::json(&file.nodes(&query(request, "ids").unwrap())),
        }
    });
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    assert!(source.fetch("file/K1/frame/1-2.txt").await.is_err());
    assert!(
        source.fetch("file/K1/frame/1-2.txt").await.is_ok(),
        "lists again"
    );
}

/// Figma answers `"nodes": {"id": null}` for a node it cannot return.
#[tokio::test]
async fn a_null_node_fails_the_listing_instead_of_reading_as_deleted() {
    // A page the user named that Figma has no node for is skipped (see
    // `named_nodes_missing_from_a_file_are_skipped_never_fatal`), but a
    // page the listing discovered and then cannot read fails: the shallow
    // answer below is null for it.
    // A page that times out, whose shallow form is null too.
    let file = one_file();
    let transport = Scripted::new({
        let file = file.clone();
        move |request, _| match request.url().path() {
            "/v1/files/K1" => Reply::json(&file.shallow()),
            "/v1/files/K1/nodes" => {
                let ids = query(request, "ids").unwrap();
                let depth = query(request, "depth");
                if depth.is_none() && ids.split(',').any(|id| id == "0:1") {
                    return Reply::status(StatusCode::INTERNAL_SERVER_ERROR);
                }
                match (ids.as_str(), depth.as_deref()) {
                    ("0:1", Some("1")) => Reply::json(&json!({"nodes": {"0:1": null}})),
                    _ => Reply::json(&file.nodes(&ids)),
                }
            }
            other => panic!("unexpected request {other}"),
        }
    });
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    let error = source.list().await.expect_err("a null shallow page fails");
    assert!(error.to_string().contains("could not return"), "{error}");

    // A frame of the fallback that Figma answers null for.
    let transport = Scripted::new({
        let file = file.clone();
        move |request, _| match request.url().path() {
            "/v1/files/K1" => Reply::json(&file.shallow()),
            "/v1/files/K1/nodes" => {
                let ids = query(request, "ids").unwrap();
                let depth = query(request, "depth");
                if depth.is_none() && ids.split(',').any(|id| id == "0:1") {
                    return Reply::status(StatusCode::INTERNAL_SERVER_ERROR);
                }
                match (ids.as_str(), depth.as_deref()) {
                    ("0:1", Some("1")) => Reply::json(&json!({"nodes": {"0:1": {"document": {
                        "id": "0:1", "name": "Auth", "type": "CANVAS",
                        "children": [{"id": "1:2", "type": "FRAME"}, {"id": "7:7", "type": "FRAME"}]}}}})),
                    _ => Reply::json(&file.nodes(&ids)),
                }
            }
            other => panic!("unexpected request {other}"),
        }
    });
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    let error = source.list().await.expect_err("a null frame fails");
    assert!(
        error.to_string().contains("could not return a frame"),
        "{error}"
    );
}

#[tokio::test]
async fn a_project_file_widens_a_url_restricted_selection_to_the_whole_file() {
    let file = one_file();
    let transport = Scripted::new(move |request, _| match request.url().path() {
        "/v1/projects/7/files" => {
            Reply::json(&json!({"name": "Apps", "files": [{"key": "K1", "name": "Mobile App"}]}))
        }
        "/v1/files/K1" => Reply::json(&file.shallow()),
        "/v1/files/K1/nodes" => Reply::json(&file.nodes(&query(request, "ids").unwrap())),
        other => panic!("unexpected request {other}"),
    });
    let url = "https://www.figma.com/design/K1/x?node-id=0-1";
    let restricted = figma(&transport, &json!({"urls_or_file_keys": url}));
    assert_eq!(
        keys(&restricted.list().await.unwrap()),
        vec!["file/K1/frame/1-2.txt"]
    );
    let widened = figma(
        &transport,
        &json!({"urls_or_file_keys": url, "project_ids": ["7"]}),
    );
    assert_eq!(
        keys(&widened.list().await.unwrap()),
        vec!["file/K1/frame/1-2.txt", "file/K1/frame/1-3.txt"]
    );
}

#[tokio::test]
async fn a_comment_is_rewritten_when_the_frame_it_is_pinned_to_becomes_listed() {
    let state = Arc::new(std::sync::Mutex::new(File {
        name: "Mobile App",
        pages: vec![(
            "0:1",
            "Auth",
            vec![frame("1:2", "Login", 375.0, 812.0, vec![])],
        )],
    }));
    let transport = Scripted::new({
        let state = Arc::clone(&state);
        move |request, _| {
            let file = state.lock().unwrap().clone();
            match request.url().path() {
                "/v1/files/K1" => Reply::json(&file.shallow()),
                "/v1/files/K1/nodes" => Reply::json(&file.nodes(&query(request, "ids").unwrap())),
                "/v1/files/K1/comments" => Reply::json(&json!({"comments": [
                    {"id": "100", "user": {"handle": "Ada"}, "created_at": "2026-10-02T10:00:00Z",
                     "message": "Make it larger", "client_meta": {"node_id": "1:2"}}]})),
                other => panic!("unexpected request {other}"),
            }
        }
    });
    let source = |transport: &Arc<Scripted>| {
        figma(
            transport,
            &json!({"urls_or_file_keys": "K1", "include_comments": true}),
        )
    };
    let first = source(&transport).list().await.expect("listing");
    assert_eq!(
        keys(&first),
        vec!["file/K1/comment/100.txt"],
        "no text, no frame"
    );
    let before = source(&transport).fetch("file/K1/comment/100.txt").await;
    // (A fresh source has not listed: its fetch lists first, then answers.)
    assert!(!before.expect("comment").metadata.contains_key("parent_key"));
    // The frame gains text: it is listed, and the comment gains its parent.
    state.lock().unwrap().pages[0].2 = vec![frame(
        "1:2",
        "Login",
        375.0,
        812.0,
        vec![text("Welcome", 24.0, 700.0)],
    )];
    let fresh = source(&transport);
    let second = fresh.list().await.expect("relisting");
    let version = |list: &[elitea_content_source::DocumentRef]| {
        list.iter()
            .find(|r| r.key == "file/K1/comment/100.txt")
            .unwrap()
            .version
            .clone()
    };
    assert_ne!(version(&first), version(&second));
    let comment = fresh.fetch("file/K1/comment/100.txt").await.unwrap();
    assert_eq!(comment.metadata["parent_key"], "file/K1/frame/1-2.txt");
}

#[tokio::test]
async fn node_ids_that_differ_never_share_a_key() {
    let file = File {
        name: "Instances",
        pages: vec![(
            "0:1",
            "P",
            vec![
                frame(
                    "I1:2;3:4",
                    "Semicolon",
                    100.0,
                    100.0,
                    vec![text("One", 14.0, 400.0)],
                ),
                frame(
                    "I1:2_3:4",
                    "Underscore",
                    100.0,
                    100.0,
                    vec![text("Two", 14.0, 400.0)],
                ),
                frame(
                    "I1-2~3",
                    "Dash",
                    100.0,
                    100.0,
                    vec![text("Three", 14.0, 400.0)],
                ),
            ],
        )],
    };
    let transport = serve(file);
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    let listed = source.list().await.expect("listing");
    assert_eq!(
        keys(&listed),
        vec![
            "file/K1/frame/I1-2_3-4.txt",
            "file/K1/frame/I1-2~5F3-4.txt",
            "file/K1/frame/I1~2D2~7E3.txt",
        ]
    );
    for (key, node_id) in [
        ("file/K1/frame/I1-2_3-4.txt", "I1:2;3:4"),
        ("file/K1/frame/I1-2~5F3-4.txt", "I1:2_3:4"),
    ] {
        let doc = source.fetch(key).await.expect("doc");
        assert_eq!(doc.metadata["node_id"], node_id);
    }
}

#[tokio::test]
async fn deeply_nested_text_is_indexed() {
    let mut node = text("Buried treasure", 14.0, 400.0);
    for _ in 0..16 {
        node = json!({"id": "n:0", "name": "Group", "type": "GROUP", "children": [node]});
    }
    let file = File {
        name: "Deep",
        pages: vec![(
            "0:1",
            "P",
            vec![frame("1:2", "Deep", 100.0, 100.0, vec![node])],
        )],
    };
    let transport = serve(file);
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"}));
    source.list().await.expect("listing");
    let doc = source.fetch("file/K1/frame/1-2.txt").await.expect("doc");
    assert!(
        String::from_utf8(doc.bytes)
            .unwrap()
            .contains("Buried treasure")
    );
}

#[tokio::test]
async fn a_nodes_answer_over_the_response_cap_is_split_like_a_timeout() {
    // Four pages of 600 bytes of text each; the cap admits one page per
    // answer but not two.
    let pages: Vec<(&'static str, &'static str, Vec<Value>)> = (1..=4)
        .map(|n| {
            let id: &'static str = Box::leak(format!("0:{n}").into_boxed_str());
            (
                id,
                "P",
                vec![frame(
                    &format!("{n}:1"),
                    &format!("F{n}"),
                    100.0,
                    100.0,
                    vec![text(&"word ".repeat(120), 14.0, 400.0)],
                )],
            )
        })
        .collect();
    let file = File {
        name: "Wide",
        pages,
    };
    let transport = serve(file.clone());
    let one_page = file.nodes("0:1").to_string().len();
    let source = figma(&transport, &json!({"urls_or_file_keys": "K1"})).with_limits(SourceLimits {
        max_listing_page_bytes: one_page + one_page / 2,
        ..SourceLimits::default()
    });
    let listed = source.list().await.expect("split until each page fits");
    assert_eq!(listed.len(), 4);
    let sizes: Vec<usize> = transport
        .requests()
        .iter()
        .filter(|request| request.url().path().ends_with("/nodes"))
        .map(|request| ids_of(request).len())
        .collect();
    assert_eq!(sizes[0], 4, "the first request carries every page");
    assert!(sizes.contains(&1), "{sizes:?}");
}

fn two_files() -> Arc<Scripted> {
    let a = one_file();
    let b = File {
        name: "Other",
        pages: vec![(
            "0:7",
            "Misc",
            vec![frame(
                "7:1",
                "Misc frame",
                100.0,
                100.0,
                vec![text("Only in B", 14.0, 400.0)],
            )],
        )],
    };
    Scripted::new(move |request, _| match request.url().path() {
        "/v1/files/K1" => Reply::json(&a.shallow()),
        "/v1/files/K1/nodes" => Reply::json(&a.nodes(&query(request, "ids").unwrap())),
        "/v1/files/K2" => Reply::json(&b.shallow()),
        "/v1/files/K2/nodes" => Reply::json(&b.nodes(&query(request, "ids").unwrap())),
        other => panic!("unexpected request {other}"),
    })
}

#[tokio::test]
async fn named_nodes_missing_from_a_file_are_skipped_never_fatal() {
    // `node_ids_include` applies to every file; an id absent from one file
    // just does not match there.
    let transport = two_files();
    let source = figma(
        &transport,
        &json!({"urls_or_file_keys": "K1,K2", "node_ids_include": ["0:1"]}),
    );
    let listed = source
        .list()
        .await
        .expect("an absent include id is skipped");
    assert_eq!(keys(&listed), vec!["file/K1/frame/1-2.txt"]);

    // A URL node that was deleted from the file: the run succeeds with
    // nothing from it (its chunks go away because it is no longer listed).
    let transport = serve(one_file());
    let source = figma(
        &transport,
        &json!({"urls_or_file_keys": "https://www.figma.com/design/K1/x?node-id=0-9"}),
    );
    assert!(source.list().await.expect("deleted URL node").is_empty());

    // The surviving one of two URL nodes is still listed.
    let source = figma(
        &transport,
        &json!({"urls_or_file_keys":
            "https://www.figma.com/design/K1/x?node-id=0-9&node-id=0-2"}),
    );
    assert_eq!(
        keys(&source.list().await.expect("partial")),
        vec!["file/K1/frame/1-3.txt"]
    );

    // A named page that times out and whose shallow form is null is skipped
    // too; a discovered one fails (`a_null_node_fails_the_listing...`).
    let file = one_file();
    let transport = Scripted::new(move |request, _| match request.url().path() {
        "/v1/files/K1" => Reply::json(&file.shallow()),
        "/v1/files/K1/nodes" => {
            let ids = query(request, "ids").unwrap();
            if ids.split(',').any(|id| id == "0:1") {
                return if query(request, "depth").is_some() {
                    Reply::json(&json!({"nodes": {"0:1": null}}))
                } else {
                    Reply::status(StatusCode::INTERNAL_SERVER_ERROR)
                };
            }
            Reply::json(&file.nodes(&ids))
        }
        other => panic!("unexpected request {other}"),
    });
    let source = figma(
        &transport,
        &json!({"urls_or_file_keys": "K1", "node_ids_include": ["0:1", "0:2"]}),
    );
    assert_eq!(
        keys(&source.list().await.expect("named null page skipped")),
        vec!["file/K1/frame/1-3.txt"]
    );
}

#[tokio::test]
async fn comments_follow_a_restricted_selection() {
    let file = one_file();
    let transport = Scripted::new(move |request, _| match request.url().path() {
        "/v1/files/K1" => Reply::json(&file.shallow()),
        "/v1/files/K1/nodes" => Reply::json(&file.nodes(&query(request, "ids").unwrap())),
        "/v1/files/K1/comments" => Reply::json(&json!({"comments": [
            {"id": "100", "user": {"handle": "Ada"}, "created_at": "t", "message": "On the login frame", "client_meta": {"node_id": "1:2"}},
            {"id": "101", "user": {"handle": "Bo"}, "created_at": "t", "message": "File-level", "client_meta": {"x": 5, "y": 6}},
            {"id": "102", "user": {"handle": "Cy"}, "created_at": "t", "message": "On the dashboard", "client_meta": {"node_id": "1:3"}},
            {"id": "103", "parent_id": "100", "user": {"handle": "Di"}, "created_at": "t", "message": "Reply on login"},
            {"id": "104", "user": {"handle": "Ed"}, "created_at": "t", "message": "Deep pin", "client_meta": {"node_id": "9:9"}},
            {"id": "105", "parent_id": "102", "user": {"handle": "Fy"}, "created_at": "t", "message": "Reply on dashboard"},
        ]})),
        other => panic!("unexpected request {other}"),
    });
    let comment_keys = |listed: &[elitea_content_source::DocumentRef]| -> Vec<String> {
        listed
            .iter()
            .filter(|r| r.key.contains("/comment/"))
            .map(|r| r.key.clone())
            .collect()
    };
    let page = figma(
        &transport,
        &json!({"urls_or_file_keys": "https://www.figma.com/design/K1/x?node-id=0-1",
                "include_comments": true}),
    );
    assert_eq!(
        comment_keys(&page.list().await.expect("page selection")),
        vec![
            "file/K1/comment/100.txt",
            "file/K1/comment/103.txt",
            "file/K1/comment/104.txt"
        ],
        "pinned inside the selected page, replies included; no file-level or other-page comment"
    );
    let excluded = figma(
        &transport,
        &json!({"urls_or_file_keys": "K1", "node_ids_exclude": ["0:2"], "include_comments": true}),
    );
    assert_eq!(
        comment_keys(&excluded.list().await.expect("exclude selection")).len(),
        3,
        "leaving a page out restricts the comments too"
    );
    let whole = figma(
        &transport,
        &json!({"urls_or_file_keys": "K1", "include_comments": true}),
    );
    assert_eq!(
        comment_keys(&whole.list().await.expect("whole file")).len(),
        6,
        "the whole file includes the file-level comments"
    );
}

#[tokio::test]
async fn hidden_frames_and_layers_are_not_indexed() {
    let mut hidden_text = text("Old copy", 14.0, 400.0);
    hidden_text["visible"] = json!(false);
    let mut hidden_frame = frame(
        "5:1",
        "Archived",
        100.0,
        100.0,
        vec![text("Archived text", 14.0, 400.0)],
    );
    hidden_frame["visible"] = json!(false);
    let file = File {
        name: "Hidden",
        pages: vec![(
            "0:1",
            "P",
            vec![
                frame(
                    "4:1",
                    "Live",
                    100.0,
                    100.0,
                    vec![text("Shown copy", 14.0, 400.0), hidden_text],
                ),
                hidden_frame,
            ],
        )],
    };
    let source = figma(&serve(file), &json!({"urls_or_file_keys": "K1"}));
    let listed = source.list().await.expect("listing");
    assert_eq!(keys(&listed), vec!["file/K1/frame/4-1.txt"]);
    let body =
        String::from_utf8(source.fetch("file/K1/frame/4-1.txt").await.unwrap().bytes).unwrap();
    assert!(
        body.contains("Shown copy") && !body.contains("Old copy"),
        "{body}"
    );
}
