use std::sync::Arc;

use reqwest::{Method, StatusCode};
use serde_json::json;

use super::ado_test_support::{
    FixtureTransport, client, collection, context, ok, ok_with_etag, policy, settings, status,
    tool, tools,
};
use super::families::ado::client::{AdoBody, AdoHttpResponse};
use super::families::ado_wiki::client::{
    AdoWikiClient, GetWikiPage, ModifyWikiPage, PageAddress, RenameWikiPage,
};
use super::families::ado_wiki::config::AdoWikiToolkitConfig;
use super::families::ado_wiki::tools::{build_ado_wiki_toolset, build_with_client};

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
