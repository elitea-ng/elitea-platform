use std::sync::Arc;

use reqwest::{Method, StatusCode};
use serde_json::json;

use super::ado_test_support::{
    CapturedRequest, FixtureTransport, client, collection, context, expected_authorization, ok,
    ok_with_etag, policy, settings, status, tool, tools,
};
use super::attachment_test_support::{FakePlatform, PNG};
use super::families::ado::client::{AdoBody, AdoClientError, AdoHttpResponse};
use super::families::ado_wiki::client::{
    AdoWikiClient, GetWikiPage, ModifyWikiPage, PageAddress, RenameWikiPage,
};
use super::families::ado_wiki::config::AdoWikiToolkitConfig;
use super::families::ado_wiki::tools::{build_ado_wiki_toolset, build_with_client};
use super::families::office_host::OfficeFailure;

fn wiki(transport: Arc<FixtureTransport>, default: Option<&str>) -> AdoWikiClient {
    let (ado, _) = client(&json!({}), transport);
    AdoWikiClient::with_client(ado, default.map(Into::into))
}

#[tokio::test]
async fn get_wiki_formats_the_sdk_fields_and_uses_the_default_identifier() {
    let transport = FixtureTransport::new(|request, _| {
        assert_eq!(
            request.path,
            "/contoso/Fabrikam Fiber/_apis/wiki/wikis/Team.wiki"
        );
        assert_eq!(request.query_value("api-version"), Some("7.0"));
        ok(json!({
            "id":"wiki-guid","name":"Team.wiki","type":"projectWiki",
            "url":"https://dev.azure.com/contoso/_apis/wiki/wikis/wiki-guid",
            "projectId":"proj-guid","repositoryId":"repo-guid","mappedPath":"/",
            "remoteUrl":"https://dev.azure.com/contoso/Fabrikam/_wiki/wikis/Team.wiki",
            "versions":[{"version":"wikiMaster"}]
        }))
    });
    assert_eq!(
        wiki(transport, Some("Team.wiki"))
            .get_wiki(None)
            .await
            .expect("wiki"),
        json!({
            "id":"wiki-guid","name":"Team.wiki","type":"projectWiki",
            "url":"https://dev.azure.com/contoso/_apis/wiki/wikis/wiki-guid",
            "project_id":"proj-guid","repository_id":"repo-guid","mapped_path":"/",
            "remote_url":"https://dev.azure.com/contoso/Fabrikam/_wiki/wikis/Team.wiki",
            "versions":[{"version":"wikiMaster","version_type":null,"version_options":null}]
        })
    );
    let none = FixtureTransport::new(|_, _| panic!("no request without a wiki"));
    assert_eq!(
        wiki(none, None).get_wiki(None).await.expect("text"),
        json!(
            "Error during the attempt to extract wiki: Wiki identifier must be provided either as a parameter or configured as default in toolkit settings. Please either pass 'wiki_identified' parameter or configure 'default_wiki_identifier' in the toolkit."
        )
    );
}

#[tokio::test]
async fn get_wiki_page_returns_expanded_metadata_and_explains_a_missing_page() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => {
            assert_eq!(
                request.path,
                "/contoso/Fabrikam Fiber/_apis/wiki/wikis/W/pages/12"
            );
            assert_eq!(request.query_value("recursionLevel"), Some("full"));
            assert_eq!(request.query_value("includeContent"), Some("true"));
            ok_with_etag(
                &json!({
                    "id":12,"path":"/Home","gitItemPath":"/Home.md","order":0,
                    "isParentPage":true,"url":"https://dev.azure.com/contoso/_apis/wiki/wikis/W/pages/%2FHome",
                    "remoteUrl":"https://dev.azure.com/contoso/Fabrikam/_wiki/wikis/W?pagePath=%2FHome",
                    "content":"# Home ![logo](/.attachments/logo.png)",
                    "subPages":[{"id":13,"path":"/Home/Child","order":0,"subPages":[{"id":14,"path":"/Home/Child/Leaf","order":1}]}]
                }),
                "\"etag-12\"",
            )
        }
        1 => {
            assert_eq!(request.query_value("path"), Some("/Missing"));
            status(StatusCode::NOT_FOUND, Some("WikiPageNotFoundException"))
        }
        _ => panic!("unexpected request"),
    });
    let wiki = wiki(transport, None);
    let page = wiki
        .get_wiki_page(GetWikiPage {
            wiki_identified: Some("W"),
            address: Some(PageAddress::Id(12)),
            include_content: true,
            recursion_level: "full",
        })
        .await
        .expect("page");
    assert_eq!(
        page,
        json!({
            "eTag":"\"etag-12\"",
            "page":{
                "id":12,"path":"/Home","git_item_path":"/Home.md",
                "remote_url":"https://dev.azure.com/contoso/Fabrikam/_wiki/wikis/W?pagePath=%2FHome",
                "url":"https://dev.azure.com/contoso/_apis/wiki/wikis/W/pages/%2FHome",
                "order":0,"is_parent_page":true,"is_non_conformant":null,
                "sub_pages":[{
                    "id":13,"path":"/Home/Child","order":0,"git_item_path":null,"url":null,"remote_url":null,
                    "sub_pages":[{"id":14,"path":"/Home/Child/Leaf","order":1}]
                }],
                "content":"# Home ![logo](/.attachments/logo.png)"
            }
        })
    );
    assert_eq!(
        wiki.get_wiki_page(GetWikiPage {
            wiki_identified: Some("W"),
            address: Some(PageAddress::Path("/Missing")),
            include_content: false,
            recursion_level: "oneLevel",
        })
        .await
        .expect("missing page text"),
        json!(
            "Page path '/Missing' not found in wiki 'W'. Please verify the page exists and the identifier is correct."
        )
    );
}

#[tokio::test]
async fn page_content_tools_return_the_raw_markdown() {
    let transport = FixtureTransport::new(|request, _| {
        assert_eq!(
            request.path,
            "/contoso/Fabrikam Fiber/_apis/wiki/wikis/W/pages"
        );
        assert_eq!(request.query_value("path"), Some("/Home"));
        assert_eq!(request.query_value("includeContent"), Some("true"));
        ok(json!({"id":12,"path":"/Home","content":"Hello ![x](/.attachments/x.png)"}))
    });
    let toolset = build_with_client(
        "wiki",
        &[],
        &policy(&[]),
        Arc::new(wiki(transport, Some("W"))),
    )
    .expect("toolset");
    let served = tools(&toolset).await;
    assert_eq!(served.len(), 8);
    assert!(
        tool(&served, "get_wiki")
            .description()
            .ends_with("\nDefault wiki: W")
    );
    assert_eq!(
        tool(&served, "get_wiki_page_by_path")
            .execute(
                context(),
                json!({"page_name":"/Home","process_images":true})
            )
            .await
            .expect("content"),
        json!("Hello ![x](/.attachments/x.png)")
    );
}

#[tokio::test]
async fn modify_writes_with_the_page_etag_and_retries_an_unresolved_version() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => {
            assert_eq!(request.path, "/contoso/Fabrikam Fiber/_apis/wiki/wikis");
            collection(&json!([{"id":"wiki-guid","name":"W"}]))
        }
        1 => {
            assert_eq!(request.method, Method::GET);
            assert_eq!(request.query_value("includeContent"), None);
            ok_with_etag(&json!({"id":12,"path":"/Home"}), "\"v1\"")
        }
        2 => {
            assert_eq!(request.method, Method::PUT);
            assert_eq!(request.if_match.as_deref(), Some("\"v1\""));
            assert_eq!(
                request.query_value("versionDescriptor.version"),
                Some("main")
            );
            status(
                StatusCode::NOT_FOUND,
                Some("The version '{0}' either is invalid or does not exist."),
            )
        }
        3 => {
            assert_eq!(request.method, Method::PUT);
            assert_eq!(request.if_match.as_deref(), Some("\"v1\""));
            assert_eq!(request.query_value("versionDescriptor.version"), None);
            assert_eq!(request.body, Some(json!({"content":"# New"})));
            ok_with_etag(
                &json!({"id":12,"path":"/Home","url":"https://dev.azure.com/contoso/_apis/wiki/wikis/W/pages/12"}),
                "\"v2\"",
            )
        }
        _ => panic!("unexpected request"),
    });
    assert_eq!(
        wiki(transport, None)
            .modify_wiki_page(ModifyWikiPage {
                wiki_identified: Some("W"),
                page_name: "/Home",
                page_content: "# New",
                version_identifier: "main",
                version_type: "branch",
                expanded: false,
            })
            .await
            .expect("modified"),
        json!({
            "eTag":"\"v2\"","id":12,
            "page":"https://dev.azure.com/contoso/_apis/wiki/wikis/W/pages/12"
        })
    );
}

#[tokio::test]
async fn modify_creates_a_missing_wiki_and_a_new_page() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => collection(&json!([{"id":"other","name":"Other"}])),
        1 => {
            assert_eq!(request.path, "/contoso/_apis/projects");
            collection(&json!([{"id":"proj-guid","name":"Fabrikam Fiber"}]))
        }
        2 => {
            assert_eq!(request.method, Method::POST);
            assert_eq!(
                request.body,
                Some(json!({"name":"New.wiki","projectId":"proj-guid"}))
            );
            ok(json!({"id":"new-guid","name":"New.wiki"}))
        }
        3 => status(StatusCode::NOT_FOUND, Some("not found")),
        4 => {
            assert_eq!(request.if_match, None);
            ok_with_etag(&json!({"id":1,"path":"/Start"}), "\"v1\"")
        }
        _ => panic!("unexpected request"),
    });
    let created = wiki(transport, Some("New.wiki"))
        .modify_wiki_page(ModifyWikiPage {
            wiki_identified: None,
            page_name: "/Start",
            page_content: "hi",
            version_identifier: "wikiMaster",
            version_type: "branch",
            expanded: true,
        })
        .await
        .expect("created");
    assert_eq!(created["page"]["path"], json!("/Start"));
    assert_eq!(created["eTag"], json!("\"v1\""));
}

#[tokio::test]
async fn rename_moves_the_page_and_delete_reports_the_sdk_sentences() {
    let transport = FixtureTransport::new(|request, index| match index {
        0 => {
            assert_eq!(
                request.path,
                "/contoso/Fabrikam Fiber/_apis/wiki/wikis/W/pagemoves"
            );
            assert_eq!(
                request.query_value("comment"),
                Some("Page rename from '/Old' to '/New'")
            );
            assert_eq!(request.body, Some(json!({"newPath":"/New","path":"/Old"})));
            ok_with_etag(
                &json!({"path":"/Old","newPath":"/New","newOrder":0,"page":{"id":3,"path":"/New","gitItemPath":"/New.md"}}),
                "\"v9\"",
            )
        }
        1 => {
            assert_eq!(request.method, Method::DELETE);
            assert_eq!(request.query_value("path"), Some("/New"));
            Ok(AdoHttpResponse::fixture(StatusCode::OK, AdoBody::Empty))
        }
        2 => {
            assert_eq!(
                request.path,
                "/contoso/Fabrikam Fiber/_apis/wiki/wikis/W/pages/3"
            );
            Ok(AdoHttpResponse::fixture(StatusCode::OK, AdoBody::Empty))
        }
        _ => panic!("unexpected request"),
    });
    let wiki = wiki(transport, None);
    assert_eq!(
        wiki.rename_wiki_page(RenameWikiPage {
            wiki_identified: Some("W"),
            old_page_name: "/Old",
            new_page_name: "/New",
            version_identifier: "wikiMaster",
            version_type: "branch",
        })
        .await
        .expect("renamed"),
        json!({
            "eTag":"\"v9\"",
            "page_move":{"path":"/Old","new_path":"/New","new_order":0,"page":{"id":3,"path":"/New","git_item_path":"/New.md"}}
        })
    );
    assert_eq!(
        wiki.delete_page(Some("W"), PageAddress::Path("/New"))
            .await
            .expect("deleted"),
        json!("Page '/New' in wiki 'W' has been deleted")
    );
    assert_eq!(
        wiki.delete_page(Some("W"), PageAddress::Id(3))
            .await
            .expect("deleted"),
        json!("Page with id '3' in wiki 'W' has been deleted")
    );
}

#[tokio::test]
async fn arguments_and_selection_follow_the_sdk_contract() {
    let transport = FixtureTransport::new(|_, _| panic!("no request for invalid arguments"));
    let toolset = build_with_client("wiki", &[], &policy(&[]), Arc::new(wiki(transport, None)))
        .expect("toolset");
    let served = tools(&toolset).await;
    assert_eq!(
        tool(&served, "get_wiki_page")
            .execute(context(), json!({"wiki_identified":"W"}))
            .await
            .expect("model-visible"),
        json!("At least one of 'page_path' or 'page_id' must be provided")
    );
    for (name, arguments) in [
        (
            "get_wiki_page",
            json!({"page_id":1,"recursion_level":"deep"}),
        ),
        ("delete_page_by_id", json!({"page_id":"x"})),
        (
            "modify_wiki_page",
            json!({"page_name":"/a","page_content":"x"}),
        ),
        (
            "rename_wiki_page",
            json!({"old_page_name":"","new_page_name":"/b","version_identifier":"m"}),
        ),
    ] {
        assert!(
            tool(&served, name)
                .execute(context(), arguments.clone())
                .await
                .is_err(),
            "{name} {arguments}"
        );
    }
    build_ado_wiki_toolset(
        "wiki",
        AdoWikiToolkitConfig::parse(&settings(&json!({"default_wiki_identifier":"Team.wiki"})))
            .expect("config"),
        &policy(&[]),
        None,
    )
    .expect("production constructor");
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let transport = FixtureTransport::new(|_, _| ok(json!({})));
    let toolset = build_with_client("gate", &[], &policy(&[]), Arc::new(wiki(transport, None)))
        .expect("complete ado_wiki toolset");
    super::sdk_conformance::assert_sdk_conformance("ado_wiki", &tools(&toolset).await);
}

#[tokio::test]
async fn the_instance_image_settings_parse_and_a_wrong_argument_is_refused() {
    // #985: `process_images` and `image_description_prompt` may be set on the
    // instance. A wrongly typed setting or argument is refused before any call.
    let (_, images) = AdoWikiToolkitConfig::parse(&settings(
        &json!({"process_images":false,"image_description_prompt":"short caption"}),
    ))
    .expect("valid image settings")
    .into_parts();
    for invalid in [
        json!({"process_images":"no"}),
        json!({"image_description_prompt":3}),
    ] {
        assert!(
            AdoWikiToolkitConfig::parse(&settings(&invalid)).is_err(),
            "{invalid} must be refused"
        );
    }
    let transport = FixtureTransport::new(|_, _| panic!("no request for invalid arguments"));
    let client = Arc::new(wiki(transport, Some("Team.wiki")).with_image_defaults(images));
    let toolset = build_with_client("wiki", &[], &policy(&[]), client).expect("toolset");
    let served = tools(&toolset).await;
    for (name, arguments) in [
        (
            "get_wiki_page_by_path",
            json!({"page_name":"/p","process_images":"yes"}),
        ),
        (
            "get_wiki_page_by_id",
            json!({"page_id":1,"image_description_prompt":1}),
        ),
        (
            "get_wiki_page",
            json!({"page_path":"/p","process_images":0}),
        ),
    ] {
        assert!(
            tool(&served, name)
                .execute(context(), arguments.clone())
                .await
                .is_err(),
            "{name} {arguments}"
        );
    }
}

// ── inline image description (#985) ──────────────────────────────────────

const PAGE: &str = "# Page\n\n![screenshot.png](/.attachments/screenshot.png)\n\n![again.png](/.attachments/screenshot.png)\n\n![spec.pdf](/.attachments/spec.pdf)\n\n![broken.png](/.attachments/broken.png)\n\n![missing.png](/.attachments/missing.png)\n\n![web.png](https://example.com/web.png)\n";

/// A project wiki whose repository holds a picture, a broken picture and a
/// PDF; one page links all of them and one external picture.
fn wiki_provider(request: &CapturedRequest, _: usize) -> Result<AdoHttpResponse, AdoClientError> {
    match request.path.as_str() {
        "/contoso/Fabrikam Fiber/_apis/wiki/wikis/W/pages"
        | "/contoso/Fabrikam Fiber/_apis/wiki/wikis/W/pages/12" => {
            ok(json!({"id":12,"path":"/Home","content":PAGE}))
        }
        "/contoso/Fabrikam Fiber/_apis/wiki/wikis/W" => ok(json!({
            "id":"wiki-guid","name":"W","repositoryId":"repo-1","mappedPath":"/",
            "versions":[{"version":"wikiMaster"}]
        })),
        "/contoso/Fabrikam Fiber/_apis/git/repositories/repo-1/items" => {
            match request.query_value("path") {
                Some("/.attachments/screenshot.png") => Ok(AdoHttpResponse::fixture(
                    StatusCode::OK,
                    AdoBody::Bytes(PNG.to_vec()),
                )),
                Some("/.attachments/broken.png") => Ok(AdoHttpResponse::fixture(
                    StatusCode::OK,
                    AdoBody::Bytes(b"not a picture".to_vec()),
                )),
                _ => status(StatusCode::NOT_FOUND, None),
            }
        }
        other => panic!("unexpected request {other}"),
    }
}

async fn wiki_picture_tools(
    transport: Arc<FixtureTransport>,
    platform: Option<&Arc<FakePlatform>>,
    extra: &serde_json::Value,
) -> Vec<Arc<dyn adk_core::Tool>> {
    let (_, images) = AdoWikiToolkitConfig::parse(&settings(extra))
        .expect("valid settings")
        .into_parts();
    let client = wiki(transport, Some("W"))
        .with_image_defaults(images)
        .with_attachments(platform.map(FakePlatform::host));
    let toolset = build_with_client("wiki", &[], &policy(&[]), Arc::new(client)).expect("toolset");
    tools(&toolset).await
}

fn repository_reads(transport: &FixtureTransport) -> Vec<String> {
    transport
        .requests()
        .iter()
        .filter(|request| request.path.ends_with("/items"))
        .filter_map(|request| request.query_value("path").map(ToOwned::to_owned))
        .collect()
}

#[tokio::test]
async fn wiki_pictures_are_read_from_the_repository_and_described_in_place() {
    let platform = FakePlatform::new();
    platform.reply("A dashboard.");
    let transport = FixtureTransport::new(wiki_provider);
    let served = wiki_picture_tools(Arc::clone(&transport), Some(&platform), &json!({})).await;
    // process_images defaults on for the page reads.
    let text = tool(&served, "get_wiki_page_by_path")
        .execute(
            context(),
            json!({"page_name":"/Home","image_description_prompt":"caption"}),
        )
        .await
        .expect("content");
    let text = text.as_str().expect("text");
    // The SDK's shape: the description replaces the URL of every link to the
    // picture, which is read and described once.
    assert!(text.contains("![screenshot.png](A dashboard.)"), "{text}");
    assert!(
        text.contains("![again.png](/.attachments/screenshot.png)")
            || text.contains("![again.png](A dashboard.)"),
        "{text}"
    );
    // A PDF linked with image syntax is left exactly as it is, and not read.
    assert!(
        text.contains("![spec.pdf](/.attachments/spec.pdf)\n\n"),
        "{text}"
    );
    // A broken or missing picture keeps its link and gets a note; an external
    // picture is not fetched.
    assert!(
        text.contains("![broken.png](/.attachments/broken.png)\n[image unavailable: could not read image dimensions]"),
        "{text}"
    );
    assert!(
        text.contains("![missing.png](/.attachments/missing.png)\n[image unavailable: the attachment was not found in the wiki]"),
        "{text}"
    );
    assert!(
        text.contains("![web.png](https://example.com/web.png)\n[image unavailable: only wiki attachments are read"),
        "{text}"
    );
    let calls = platform.model_calls();
    assert_eq!(calls.len(), 1);
    assert_eq!(calls[0].prompt, "caption");
    assert_eq!(calls[0].images_png[0], PNG);
    let reads = repository_reads(&transport);
    assert!(
        reads.iter().all(|path| path != "/.attachments/spec.pdf"),
        "{reads:?}"
    );
    let read = transport
        .requests()
        .into_iter()
        .find(|request| request.query_value("path") == Some("/.attachments/screenshot.png"))
        .expect("the picture was read");
    assert_eq!(
        read.query_value("versionDescriptor.version"),
        Some("wikiMaster")
    );
    assert_eq!(
        read.query_value("versionDescriptor.versionType"),
        Some("branch")
    );
    assert_eq!(read.accept.as_deref(), Some("application/octet-stream"));
    assert_eq!(
        read.authorization.as_deref(),
        Some(expected_authorization().as_str())
    );
}

#[tokio::test]
async fn wiki_pictures_follow_the_call_then_the_toolkit_then_the_tool_default() {
    // Off by the toolkit: the page is returned as stored and nothing is read.
    let platform = FakePlatform::new();
    let transport = FixtureTransport::new(wiki_provider);
    let served = wiki_picture_tools(
        Arc::clone(&transport),
        Some(&platform),
        &json!({"process_images":false}),
    )
    .await;
    let text = tool(&served, "get_wiki_page_by_id")
        .execute(context(), json!({"page_id":12}))
        .await
        .expect("content");
    assert_eq!(text, json!(PAGE));
    assert_eq!(repository_reads(&transport), Vec::<String>::new());
    assert!(platform.model_calls().is_empty());

    // The call turns it back on, against the toolkit.
    platform.reply("From the call.");
    let text = tool(&served, "get_wiki_page_by_id")
        .execute(context(), json!({"page_id":12,"process_images":true}))
        .await
        .expect("content");
    assert!(
        text.as_str()
            .expect("text")
            .contains("![screenshot.png](From the call.)")
    );

    // get_wiki_page describes the content it includes, with the toolkit's
    // prompt when the call names none.
    let platform = FakePlatform::new();
    platform.reply("In the page.");
    let transport = FixtureTransport::new(wiki_provider);
    let served = wiki_picture_tools(
        Arc::clone(&transport),
        Some(&platform),
        &json!({"image_description_prompt":"toolkit prompt"}),
    )
    .await;
    let page = tool(&served, "get_wiki_page")
        .execute(
            context(),
            json!({"page_path":"/Home","include_content":true}),
        )
        .await
        .expect("page");
    assert!(
        page["page"]["content"]
            .as_str()
            .expect("content")
            .contains("![screenshot.png](In the page.)"),
        "{page}"
    );
    assert_eq!(platform.model_calls()[0].prompt, "toolkit prompt");
    // Without include_content there is nothing to describe.
    let before = platform.model_calls().len();
    tool(&served, "get_wiki_page")
        .execute(context(), json!({"page_path":"/Home"}))
        .await
        .expect("page");
    assert_eq!(platform.model_calls().len(), before);
}

#[tokio::test]
async fn a_refused_model_budget_stops_the_wiki_descriptions() {
    let platform = FakePlatform::new();
    platform.fail_model(OfficeFailure::BudgetRefused);
    let page = "![a.png](/.attachments/screenshot.png)\n![b.png](/.attachments/screenshot.png?v=2)";
    let transport = FixtureTransport::new(move |request, index| {
        if request.path.ends_with("/pages") {
            return ok(json!({"id":12,"path":"/Home","content":page}));
        }
        wiki_provider(request, index)
    });
    let served = wiki_picture_tools(Arc::clone(&transport), Some(&platform), &json!({})).await;
    let text = tool(&served, "get_wiki_page_by_path")
        .execute(context(), json!({"page_name":"/Home"}))
        .await
        .expect("the call still answers");
    let text = text.as_str().expect("text");
    assert_eq!(platform.model_calls().len(), 1, "{text}");
    assert_eq!(
        text.matches("[image unavailable: the model budget is exhausted")
            .count(),
        2,
        "{text}"
    );
}

#[tokio::test]
async fn failed_wiki_picture_reads_count_against_the_per_call_limit() {
    let platform = FakePlatform::new();
    let mut page = String::new();
    for n in 0..200 {
        let n = n.to_string();
        page.push_str("![m.png](/.attachments/missing-");
        page.push_str(&n);
        page.push_str(".png)\n");
    }
    let transport = FixtureTransport::new(move |request, index| {
        if request.path.ends_with("/pages") {
            return ok(json!({"id":12,"path":"/Home","content":page.clone()}));
        }
        wiki_provider(request, index)
    });
    let served = wiki_picture_tools(Arc::clone(&transport), Some(&platform), &json!({})).await;
    let text = tool(&served, "get_wiki_page_by_path")
        .execute(context(), json!({"page_name":"/Home"}))
        .await
        .expect("the call still answers");
    let text = text.as_str().expect("text");
    let limit = super::families::attachment_host::MAX_PICTURES;
    assert_eq!(repository_reads(&transport).len(), limit);
    assert!(platform.model_calls().is_empty());
    assert_eq!(
        text.matches("[image skipped: per-call image limit of 20 reached]")
            .count(),
        200 - limit,
        "{text}"
    );
}

#[tokio::test]
async fn a_wiki_page_too_large_with_descriptions_comes_back_without_them() {
    let filler = "x".repeat(super::families::ado::client::MAX_OUTPUT_BYTES - 2_000);
    let content = format!("{filler}\n![s.png](/.attachments/screenshot.png)\n");
    let dropped = super::families::ado::inline_images::DESCRIPTIONS_DROPPED;
    let page = content.clone();
    let transport = FixtureTransport::new(move |request, index| {
        if request.path.ends_with("/pages") {
            return ok(json!({"id":12,"path":"/Home","content":page.clone()}));
        }
        wiki_provider(request, index)
    });
    let platform = FakePlatform::new();
    platform.reply(&"d".repeat(4_000));
    platform.reply(&"d".repeat(4_000));
    let served = wiki_picture_tools(Arc::clone(&transport), Some(&platform), &json!({})).await;
    // A page read by path: the text without descriptions, and a note.
    let text = tool(&served, "get_wiki_page_by_path")
        .execute(context(), json!({"page_name":"/Home"}))
        .await
        .expect("the page fits without its descriptions");
    assert_eq!(text, json!(format!("{content}\n\n{dropped}")));
    // get_wiki_page: the page without descriptions, and the note beside it.
    let page = tool(&served, "get_wiki_page")
        .execute(
            context(),
            json!({"page_path":"/Home","include_content":true}),
        )
        .await
        .expect("the page fits without its descriptions");
    assert_eq!(page["page"]["content"], content.as_str());
    assert_eq!(page["image_descriptions"], dropped);
    assert_eq!(platform.model_calls().len(), 2);
}
