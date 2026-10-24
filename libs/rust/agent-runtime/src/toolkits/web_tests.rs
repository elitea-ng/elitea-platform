//! The `web` and `host_intel` families: resolution, options, the meter,
//! direct fetch and the egress rules, Shodan's constraints.

use std::sync::Arc;
use std::sync::atomic::Ordering;
use std::time::Duration;

use adk_core::ErrorCategory;
use serde_json::{Value, json};

use super::WebEgressAllowlist;
use super::families::web::meter::Meter;
use super::families::web::meter::{
    ADMISSION_TTL, FailureClass, MeterError, MeterRefusal, Outcome, SETTLE_ECHO_WAIT,
    settle_dropped_total,
};
use super::families::web::registry::REGISTRY;
use super::families::web::{WebToolHost, build_web_toolset};
use super::sdk_conformance;
use super::web_test_support::{
    FakeLookup, FakeMeter, FixtureTransport, all_web_providers, call, context, fixture,
    host_intel_toolset, origin, policy, settings, tool, tools, web_toolset,
};

fn tavily_ok() -> Arc<FixtureTransport> {
    FixtureTransport::answering(&[fixture("tavily", "web_search_ok")])
}

#[tokio::test]
async fn web_tools_are_read_only() {
    let meter = FakeMeter::new();
    let web = web_toolset(&all_web_providers(), Some(&meter), &tavily_ok(), Vec::new());
    let hosts = host_intel_toolset(
        &json!({"providers": {"shodan": {"api_key": "shodan-fixture"}}}),
        Some(&meter),
        &tavily_ok(),
    );
    let mut names = Vec::new();
    for tool in tools(&web).await.into_iter().chain(tools(&hosts).await) {
        assert!(tool.is_read_only(), "{}", tool.name());
        assert!(tool.is_concurrency_safe(), "{}", tool.name());
        names.push(tool.name().to_owned());
    }
    assert_eq!(
        names,
        [
            "web_search",
            "web_fetch",
            "internet_host_search",
            "internet_host_lookup"
        ]
    );
    // No SDK type exists for either family: the day one does, its names and
    // schemas are the contract and these families must be reconciled.
    assert!(!sdk_conformance::has_type("web"));
    assert!(!sdk_conformance::has_type("host_intel"));
}

#[tokio::test]
async fn web_search_is_offered_only_with_a_search_provider() {
    let web = web_toolset(
        &json!({}),
        Some(&FakeMeter::new()),
        &tavily_ok(),
        Vec::new(),
    );
    let names: Vec<String> = tools(&web)
        .await
        .iter()
        .map(|tool| tool.name().to_owned())
        .collect();
    assert_eq!(names, ["web_fetch"], "direct fetch is on by default");
    let none = web_toolset(
        &json!({"direct_fetch_enabled": false}),
        Some(&FakeMeter::new()),
        &tavily_ok(),
        Vec::new(),
    );
    assert!(tools(&none).await.is_empty());
}

#[tokio::test]
async fn registry_capability_slots_match_declared_kinds() {
    use super::families::web::config::ProviderConfig;
    for entry in REGISTRY {
        let handle = (entry.construct)(
            &ProviderConfig::with_key("fixture-key"),
            FixtureTransport::answering(&[fixture("tavily", "error_401")]),
        );
        assert_eq!(handle.kinds(), entry.capabilities, "{}", entry.id);
        assert!(!entry.origins.is_empty(), "{}", entry.id);
        for origin in entry.origins {
            assert!(origin.starts_with("https://"), "{}: {origin}", entry.id);
        }
        assert_ne!(entry.config_type, "", "{}", entry.id);
    }
    let shodan = REGISTRY
        .iter()
        .find(|entry| entry.id == "shodan")
        .expect("shodan");
    assert!(
        !shodan.shared_config_allowed,
        "Shodan's terms bar a shared key"
    );
}

#[tokio::test]
async fn provider_resolution_refuses_disabled_and_unconfigured() {
    let meter = FakeMeter::new();
    let transport = tavily_ok();
    let web = web_toolset(&all_web_providers(), Some(&meter), &transport, Vec::new());
    let overridden = call(
        &web,
        "web_search",
        json!({"query": "tokio", "provider": "brave"}),
    )
    .await;
    assert_eq!(overridden["provider"], "brave");
    let only_tavily = web_toolset(
        &json!({"providers": {"tavily": {"api_key": "k"}}}),
        Some(&meter),
        &transport,
        Vec::new(),
    );
    let disabled = call(
        &only_tavily,
        "web_search",
        json!({"query": "tokio", "provider": "brave"}),
    )
    .await;
    assert_eq!(disabled["error"]["code"], "provider_not_enabled");
    let unknown = call(&only_tavily, "web_search", json!({"query": "tokio"})).await;
    assert_eq!(
        unknown["error"]["code"], "no_provider_configured",
        "no toolkit or platform default resolves"
    );
    let platform = web_toolset(
        &json!({"providers": {"tavily": {"api_key": "k"}}, "platform_default_search_provider": "tavily"}),
        Some(&meter),
        &transport,
        Vec::new(),
    );
    assert_eq!(
        call(&platform, "web_search", json!({"query": "tokio"})).await["provider"],
        "tavily"
    );
}

#[tokio::test]
async fn ddg_instant_never_implicit() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[fixture("ddg_instant", "web_search_ok")]);
    // Even as the platform default, and as the only provider, it is never
    // picked without being named.
    let web = web_toolset(
        &json!({"providers": {"ddg_instant": {}}, "platform_default_search_provider": "ddg_instant"}),
        Some(&meter),
        &transport,
        Vec::new(),
    );
    let implicit = call(&web, "web_search", json!({"query": "tokio rust"})).await;
    assert_eq!(implicit["error"]["code"], "no_provider_configured");
    assert_eq!(transport.count(), 0);
    let named = call(
        &web,
        "web_search",
        json!({"query": "tokio rust", "provider": "ddg_instant"}),
    )
    .await;
    assert_eq!(named["provider"], "ddg_instant");
    let kinds: Vec<&str> = named["results"]
        .as_array()
        .expect("results")
        .iter()
        .map(|result| result["kind"].as_str().expect("kind"))
        .collect();
    assert_eq!(kinds, ["web", "related", "related"]);
    assert_eq!(named["results"][0]["source"], "Wikipedia");
    let (_, url, _) = transport.sent().pop().expect("a request");
    assert!(url.contains("t=elitea"), "the app is named");
    let empty_transport =
        FixtureTransport::answering(&[fixture("ddg_instant", "web_search_empty")]);
    let toolkit_default = web_toolset(
        &json!({"providers": {"ddg_instant": {}}, "default_search_provider": "ddg_instant"}),
        Some(&meter),
        &empty_transport,
        Vec::new(),
    );
    let empty = call(
        &toolkit_default,
        "web_search",
        json!({"query": "what changed in tokio 1.53"}),
    )
    .await;
    assert_eq!(empty["results"], json!([]));
    assert_eq!(empty["notice"], "instant_answers_only");
}

#[tokio::test]
async fn strict_options_refuses_unsupported() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[fixture("ddg_instant", "web_search_ok")]);
    let web = web_toolset(&all_web_providers(), Some(&meter), &transport, Vec::new());
    let refused = call(
        &web,
        "web_search",
        json!({"query": "tokio", "provider": "ddg_instant", "freshness": "week"}),
    )
    .await;
    assert_eq!(refused["error"]["code"], "unsupported_option");
    assert_eq!(refused["error"]["option"], "freshness");
    let both = call(
        &web,
        "web_search",
        json!({"query": "tokio", "provider": "firecrawl", "include_domains": ["docs.rs"], "exclude_domains": ["x.com"]}),
    )
    .await;
    assert_eq!(both["error"]["option"], "exclude_domains");
    assert_eq!(transport.count(), 0, "a refused option reaches no provider");
    assert!(meter.admitted().is_empty(), "and is not metered");
}

#[tokio::test]
async fn lenient_options_lists_ignored() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[fixture("brave", "web_search_ok")]);
    let mut settings = all_web_providers();
    settings["strict_options"] = json!(false);
    let web = web_toolset(&settings, Some(&meter), &transport, Vec::new());
    let result = call(
        &web,
        "web_search",
        json!({"query": "tokio", "provider": "brave", "include_domains": ["docs.rs"], "freshness": "week"}),
    )
    .await;
    assert_eq!(result["ignored_options"], json!(["include_domains"]));
    let (_, url, _) = transport.sent().pop().expect("a request");
    assert!(url.contains("freshness=pw"));
    assert!(!url.contains("docs.rs"), "a dropped option is not sent");
}

#[tokio::test]
async fn max_results_is_clamped_to_admin_and_provider_caps() {
    let meter = FakeMeter::new();
    let transport = tavily_ok();
    let mut settings = all_web_providers();
    settings["max_results_cap"] = json!(3);
    let web = web_toolset(&settings, Some(&meter), &transport, Vec::new());
    call(
        &web,
        "web_search",
        json!({"query": "tokio", "max_results": 10}),
    )
    .await;
    let (_, _, body) = transport.sent().pop().expect("a request");
    assert_eq!(body.expect("body")["max_results"], 3);
}

#[tokio::test]
async fn meter_refusal_makes_no_provider_call() {
    let refusal = MeterError::Refused(MeterRefusal {
        code: "model_gateway.project_budget_exhausted".to_owned(),
        public_code: Some("PROJECT_BUDGET_EXHAUSTED".to_owned()),
        retry_after_ms: None,
    });
    let meter = FakeMeter::refusing(refusal);
    let transport = tavily_ok();
    let web = web_toolset(&all_web_providers(), Some(&meter), &transport, Vec::new());
    let hosts = host_intel_toolset(
        &json!({"providers": {"shodan": {"api_key": "refusal-key"}}}),
        Some(&meter),
        &transport,
    );
    let results = [
        call(&web, "web_search", json!({"query": "tokio"})).await,
        call(
            &web,
            "web_fetch",
            json!({"url": "https://docs.rs/tokio", "provider": "tavily"}),
        )
        .await,
        call(&web, "web_fetch", json!({"url": "https://docs.rs/tokio"})).await,
        call(
            &hosts,
            "internet_host_search",
            json!({"query": "product:nginx"}),
        )
        .await,
        call(&hosts, "internet_host_lookup", json!({"ip": "203.0.113.7"})).await,
    ];
    for result in &results {
        assert_eq!(
            result["error"]["code"],
            "model_gateway.project_budget_exhausted"
        );
        assert_eq!(result["error"]["public_code"], "PROJECT_BUDGET_EXHAUSTED");
        assert!(
            result["error"]["message"]
                .as_str()
                .is_some_and(|message| message.contains("The provider was not called")),
        );
    }
    assert_eq!(transport.count(), 0, "zero provider requests");
    assert!(
        meter.settled().is_empty(),
        "a refused admission is never settled"
    );
}

#[tokio::test]
async fn toolset_without_meter_refuses() {
    let transport = tavily_ok();
    let web = web_toolset(&all_web_providers(), None, &transport, Vec::new());
    for (name, arguments) in [
        ("web_search", json!({"query": "tokio"})),
        ("web_fetch", json!({"url": "https://docs.rs/tokio"})),
    ] {
        assert_eq!(
            call(&web, name, arguments).await["error"]["code"],
            "metering_unavailable"
        );
    }
    assert_eq!(transport.count(), 0);
}

#[tokio::test]
async fn unpriced_operation_refused_before_provider() {
    let meter = FakeMeter::refusing(MeterError::Unpriced);
    let transport = tavily_ok();
    let web = web_toolset(&all_web_providers(), Some(&meter), &transport, Vec::new());
    let result = call(&web, "web_search", json!({"query": "tokio"})).await;
    assert_eq!(result["error"]["code"], "web_provider_unpriced");
    assert_eq!(transport.count(), 0);
}

#[tokio::test]
async fn search_settles_reported_units_and_echoes_usage() {
    let meter = FakeMeter::new();
    let web = web_toolset(&all_web_providers(), Some(&meter), &tavily_ok(), Vec::new());
    let result = call(&web, "web_search", json!({"query": "tokio 1.53 changelog"})).await;
    assert_eq!(
        result["usage"],
        json!({"units": 1, "unit": "credit", "cost_usd": "0.008"})
    );
    assert_eq!(result["results"][0]["score"], 0.83);
    assert_eq!(result["results"][0]["source"], "github.com");
    assert!(
        result["data_note"]
            .as_str()
            .is_some_and(|note| note.contains("untrusted"))
    );
    let settled = meter.settled();
    assert_eq!(settled.len(), 1);
    assert_eq!(settled[0].op.provider, "tavily");
    assert_eq!(settled[0].op.operation, "web_search");
    assert_eq!(settled[0].units.credits, 1);
    assert_eq!(settled[0].audit.tool, "web_search");
    assert_eq!(
        settled[0].audit.target.len(),
        64,
        "the audit gets a query hash, never the query"
    );
    assert!(!settled[0].audit.target.contains("tokio"));
}

#[tokio::test(start_paused = true)]
async fn settle_reuses_event_id_on_retry() {
    let meter = FakeMeter::new();
    meter.settle_failures.store(3, Ordering::SeqCst);
    let web = web_toolset(&all_web_providers(), Some(&meter), &tavily_ok(), Vec::new());
    let result = call(&web, "web_search", json!({"query": "tokio"})).await;
    assert!(result["results"].is_array());
    tokio::time::sleep(Duration::from_mins(1)).await;
    let settled = meter.settled();
    assert_eq!(
        settled.len(),
        4,
        "three failures, then the settle that landed"
    );
    assert!(
        settled
            .iter()
            .all(|settle| settle.event_id == settled[0].event_id)
    );
}

/// Each execution of a tool call is its own billed event: a re-execution
/// under the same call id (an ADK retry, a backend that reuses `call_0`)
/// calls the provider again and must not collapse into the first bill.
#[tokio::test]
async fn re_executed_tool_call_settles_a_new_event() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[
        fixture("tavily", "web_search_ok"),
        fixture("tavily", "web_search_ok"),
    ]);
    let web = web_toolset(&all_web_providers(), Some(&meter), &transport, Vec::new());
    for _ in 0..2 {
        let result = call(&web, "web_search", json!({"query": "tokio"})).await;
        assert!(result["results"].is_array(), "{result}");
    }
    let settled = meter.settled();
    assert_eq!(settled.len(), 2);
    assert_ne!(settled[0].event_id, settled[1].event_id);
}

/// The settlement outlives the tool call: the result returns after the
/// echo wait while the settle keeps retrying, and the retries stop before
/// the admission expires, counted as dropped.
#[tokio::test(start_paused = true)]
async fn settle_retry_outlives_the_tool_call_and_stops_before_expiry() {
    let meter = FakeMeter::new();
    meter.settle_failures.store(u32::MAX, Ordering::SeqCst);
    let web = web_toolset(&all_web_providers(), Some(&meter), &tavily_ok(), Vec::new());
    let dropped = settle_dropped_total();
    let started = tokio::time::Instant::now();
    let result = call(&web, "web_search", json!({"query": "tokio"})).await;
    assert!(
        result["results"].is_array(),
        "the result still reaches the model"
    );
    assert!(
        result["usage"].get("cost_usd").is_none(),
        "no settlement, no cost echo"
    );
    assert!(started.elapsed() <= SETTLE_ECHO_WAIT + Duration::from_secs(1));
    let attempts_at_return = meter.settled().len();
    tokio::time::sleep(ADMISSION_TTL + Duration::from_mins(1)).await;
    assert!(
        meter.settled().len() > attempts_at_return,
        "the settle kept retrying"
    );
    assert!(settle_dropped_total() > dropped);
}

#[tokio::test]
async fn provider_failure_reports_its_failure_class() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[fixture("tavily", "error_401")]);
    let web = web_toolset(&all_web_providers(), Some(&meter), &transport, Vec::new());
    let result = call(&web, "web_search", json!({"query": "tokio"})).await;
    assert_eq!(result["error"]["code"], "auth_failed", "{result}");
    let settled = meter.settled();
    assert_eq!(settled[0].outcome, Outcome::ProviderError);
    assert_eq!(settled[0].failure, Some(FailureClass::Refused));
}

#[tokio::test]
async fn provider_fetch_checks_the_url_before_the_vendor_sees_it() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[fixture("tavily", "web_fetch_ok")]);
    let web = web_toolset(&all_web_providers(), Some(&meter), &transport, Vec::new());
    for url in [
        "https://10.0.0.1/admin",
        "https://localhost/",
        "https://[::1]/",
        "https://user:pass@docs.rs/",
        "file:///etc/passwd",
        "https://docs.rs:8443/",
    ] {
        let result = call(&web, "web_fetch", json!({"url": url, "provider": "tavily"})).await;
        assert_eq!(result["error"]["code"], "url_not_allowed", "{url}");
    }
    assert_eq!(transport.count(), 0);
    let ok = call(
        &web,
        "web_fetch",
        json!({"url": "https://docs.rs/tokio/latest/tokio/", "provider": "tavily"}),
    )
    .await;
    assert_eq!(ok["backend"], "tavily");
    assert_eq!(ok["status"], 200);
}

#[tokio::test]
async fn domain_lists_hold() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[fixture("tavily", "web_fetch_ok")]);
    let mut settings = all_web_providers();
    settings["fetch_allow_domains"] = json!(["docs.rs"]);
    settings["fetch_deny_domains"] = json!(["private.docs.rs"]);
    let web = web_toolset(&settings, Some(&meter), &transport, Vec::new());
    for (url, code) in [
        ("https://example.com/", "domain_not_allowed"),
        ("https://private.docs.rs/", "domain_not_allowed"),
    ] {
        let result = call(&web, "web_fetch", json!({"url": url, "provider": "tavily"})).await;
        assert_eq!(result["error"]["code"], code, "{url}");
    }
    let allowed = call(
        &web,
        "web_fetch",
        json!({"url": "https://api.docs.rs/x", "provider": "tavily"}),
    )
    .await;
    assert_eq!(allowed["backend"], "tavily");
}

fn direct_settings() -> Value {
    json!({"allow_http": true})
}

#[tokio::test]
async fn web_fetch_result_is_framed_untrusted() {
    let (port, _) = origin().await;
    let meter = FakeMeter::new();
    let web = web_toolset(&direct_settings(), Some(&meter), &tavily_ok(), vec![port]);
    let result = call(
        &web,
        "web_fetch",
        json!({"url": format!("http://docs.example.com:{port}/page")}),
    )
    .await;
    assert_eq!(result["backend"], "direct", "{result}");
    assert_eq!(result["title"], "Tokio & friends");
    let content = result["content"].as_str().expect("content");
    assert!(content.starts_with("<web_content nonce=\""), "{content}");
    assert!(content.contains("Changelog") && content.contains("Fixed a race."));
    for stripped in ["steal()", "p{}", "Menu Home", "input"] {
        assert!(!content.contains(stripped), "{stripped} leaked: {content}");
    }
    let nonce = content
        .strip_prefix("<web_content nonce=\"")
        .and_then(|rest| rest.split('"').next())
        .expect("nonce");
    assert_eq!(nonce.len(), 32);
    assert!(content.ends_with(&format!("</web_content nonce=\"{nonce}\">")));
    assert!(result["data_note"].is_string());
    assert_eq!(meter.settled()[0].op.provider, "direct");
    assert_eq!(meter.settled()[0].units.pages, 1);
    assert_eq!(
        meter.settled()[0].audit.target,
        "docs.example.com",
        "host only"
    );
}

#[tokio::test]
async fn direct_fetch_revalidates_every_redirect() {
    let (port, served) = origin().await;
    let meter = FakeMeter::new();
    let web = web_toolset(&direct_settings(), Some(&meter), &tavily_ok(), vec![port]);
    let fetch = |path: &str| json!({"url": format!("http://docs.example.com:{port}{path}")});
    let followed = call(&web, "web_fetch", fetch("/redirect-ok")).await;
    assert_eq!(
        followed["final_url"],
        format!("http://docs.example.com:{port}/page")
    );
    let literal = call(&web, "web_fetch", fetch("/redirect-metadata")).await;
    assert_eq!(literal["error"]["code"], "url_not_allowed", "{literal}");
    let named = call(&web, "web_fetch", fetch("/redirect-metadata-name")).await;
    assert_eq!(named["error"]["code"], "address_not_allowed", "{named}");
    let looped = call(&web, "web_fetch", fetch("/loop")).await;
    assert_eq!(looped["error"]["code"], "too_many_redirects");
    let before = served.load(Ordering::SeqCst);
    let private = call(
        &web,
        "web_fetch",
        json!({"url": format!("http://private.example.com:{port}/page")}),
    )
    .await;
    assert_eq!(private["error"]["code"], "address_not_allowed");
    assert_eq!(served.load(Ordering::SeqCst), before, "nothing was dialled");
}

/// The provider allowlist's private permits never reach the direct fetch:
/// a CIDR an operator added for a provider call does not open a private or
/// loopback address to a model-chosen URL.
#[tokio::test]
async fn direct_fetch_ignores_the_provider_allowlists_private_permits() {
    let meter = FakeMeter::new();
    let host = WebToolHost::new(
        Arc::clone(&meter) as Arc<dyn Meter>,
        WebEgressAllowlist::parse_csv("127.0.0.1,10.0.0.0/8,localhost").expect("allowlist"),
        "https://elitea.example",
    )
    .with_lookup(FakeLookup::new());
    let web = build_web_toolset("web", &settings(&json!({})), &policy(), Some(&host))
        .expect("web toolset");
    for url in [
        "https://docs.example.com/page",
        "https://private.example.com/page",
    ] {
        let result = call(&web, "web_fetch", json!({"url": url})).await;
        assert_eq!(
            result["error"]["code"], "address_not_allowed",
            "{url}: {result}"
        );
    }
}

/// A redirect's body is dropped, not read: the origin's 302 announces a
/// 1 MiB body and stalls after 16 bytes, and the fetch still follows the
/// Location at once instead of waiting out its 20 s timeout.
#[tokio::test]
async fn direct_fetch_does_not_read_redirect_bodies() {
    let (port, _) = origin().await;
    let meter = FakeMeter::new();
    let web = web_toolset(&direct_settings(), Some(&meter), &tavily_ok(), vec![port]);
    let result = tokio::time::timeout(
        Duration::from_secs(5),
        call(
            &web,
            "web_fetch",
            json!({"url": format!("http://docs.example.com:{port}/redirect-stall")}),
        ),
    )
    .await
    .expect("the redirect body was not awaited");
    assert_eq!(
        result["final_url"],
        format!("http://docs.example.com:{port}/page"),
        "{result}"
    );
}

#[tokio::test]
async fn direct_fetch_refuses_dns_rebinding() {
    let (port, served) = origin().await;
    let meter = FakeMeter::new();
    let web = web_toolset(&direct_settings(), Some(&meter), &tavily_ok(), vec![port]);
    let before = served.load(Ordering::SeqCst);
    // The name checks public, then re-resolves to a private address for the
    // connection: the connector only ever gets checked addresses.
    let result = call(
        &web,
        "web_fetch",
        json!({"url": format!("http://rebind.example.com:{port}/page")}),
    )
    .await;
    assert!(result["error"]["code"].is_string(), "{result}");
    assert_eq!(served.load(Ordering::SeqCst), before);
}

#[tokio::test]
async fn direct_fetch_caps_download_and_output() {
    let (port, _) = origin().await;
    let meter = FakeMeter::new();
    let web = web_toolset(&direct_settings(), Some(&meter), &tavily_ok(), vec![port]);
    let big = call(
        &web,
        "web_fetch",
        json!({"url": format!("http://docs.example.com:{port}/big"), "max_bytes": 2000}),
    )
    .await;
    assert_eq!(big["truncated"], true);
    assert_eq!(
        big["bytes"],
        5 * 1_024 * 1_024,
        "the download stops at 5 MiB"
    );
    let framed = big["content"].as_str().expect("content");
    let body = framed
        .split_once("\">")
        .and_then(|(_, rest)| rest.rsplit_once("</web_content"))
        .map(|(body, _)| body)
        .expect("a framed body");
    assert!(body.trim().len() <= 2_000, "{}", body.len());
    let pdf = call(
        &web,
        "web_fetch",
        json!({"url": format!("http://docs.example.com:{port}/pdf")}),
    )
    .await;
    assert_eq!(pdf["error"]["code"], "unsupported_content_type");
    let https_only = web_toolset(&json!({}), Some(&meter), &tavily_ok(), vec![port]);
    let plain = call(
        &https_only,
        "web_fetch",
        json!({"url": format!("http://docs.example.com:{port}/page")}),
    )
    .await;
    assert_eq!(
        plain["error"]["code"], "url_not_allowed",
        "http needs allow_http"
    );
}

#[tokio::test]
async fn rate_limit_is_a_retryable_tool_error() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[fixture("brave", "error_429")]);
    let web = web_toolset(&all_web_providers(), Some(&meter), &transport, Vec::new());
    let error = tool(&web, "web_search")
        .await
        .execute(
            context("call-429"),
            json!({"query": "tokio", "provider": "brave"}),
        )
        .await
        .expect_err("a rate limit");
    assert_eq!(error.category, ErrorCategory::RateLimited);
    assert_eq!(error.retry.retry_after_ms, Some(7_000));
    assert_eq!(
        meter.settled().len(),
        1,
        "an admitted call is settled even when it fails"
    );
}

fn shodan_settings(key: &str) -> Value {
    json!({"providers": {"shodan": {"api_key": key}}})
}

#[tokio::test(start_paused = true)]
async fn shodan_lookup_refuses_private_ip() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[fixture("shodan", "host_lookup_ok")]);
    let hosts = host_intel_toolset(&shodan_settings("private-ip-key"), Some(&meter), &transport);
    for ip in [
        "10.0.0.1",
        "127.0.0.1",
        "169.254.169.254",
        "::1",
        "fd00:ec2::254",
        "192.0.0.1",
    ] {
        let result = call(&hosts, "internet_host_lookup", json!({"ip": ip})).await;
        assert_eq!(result["error"]["code"], "not_public_ip", "{ip}");
    }
    assert_eq!(transport.count(), 0);
    assert!(meter.admitted().is_empty(), "not billed");
}

#[tokio::test(start_paused = true)]
async fn shodan_strips_raw_banners() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[fixture("shodan", "host_lookup_ok")]);
    let hosts = host_intel_toolset(&shodan_settings("banner-key"), Some(&meter), &transport);
    let record = call(&hosts, "internet_host_lookup", json!({"ip": "203.0.113.7"})).await;
    let text = record.to_string();
    assert!(!text.contains("ignore previous instructions"), "{text}");
    assert!(!text.contains("SSH-2.0"));
    assert_eq!(record["attribution"], "Data from Shodan (shodan.io)");
    assert_eq!(record["ports"], json!([22, 443]));
    assert_eq!(record["services"][0]["tls_subject_cn"], "example.org");
    assert_eq!(record["vulns"], json!(["CVE-2024-0001"]));
    assert_eq!(record["location"]["country"], "DE");
}

#[tokio::test(start_paused = true)]
async fn shodan_key_never_in_logged_url() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[fixture("shodan", "error_401")]);
    let hosts = host_intel_toolset(
        &shodan_settings("SECRET-SHODAN-KEY"),
        Some(&meter),
        &transport,
    );
    let result = call(&hosts, "internet_host_lookup", json!({"ip": "203.0.113.7"})).await;
    assert_eq!(result["error"]["code"], "auth_failed");
    assert!(!result.to_string().contains("SECRET-SHODAN-KEY"));
    for settle in meter.settled() {
        assert!(!format!("{settle:?}").contains("SECRET-SHODAN-KEY"));
    }
    // The key does ride in the query string, so nothing may log a URL.
    let (_, url, _) = transport.sent().pop().expect("a request");
    assert!(url.contains("key=SECRET-SHODAN-KEY"));
}

#[tokio::test(start_paused = true)]
async fn shodan_never_calls_scan() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[
        fixture("shodan", "host_search_ok"),
        fixture("shodan", "host_count_ok"),
        fixture("shodan", "host_lookup_ok"),
    ]);
    let hosts = host_intel_toolset(&shodan_settings("scan-key"), Some(&meter), &transport);
    call(
        &hosts,
        "internet_host_search",
        json!({"query": "product:nginx"}),
    )
    .await;
    let counted = call(
        &hosts,
        "internet_host_search",
        json!({"query": "nginx", "count_only": true}),
    )
    .await;
    assert_eq!(counted["total"], 1234);
    assert_eq!(counted["usage"]["units"], 0, "count_only bills 0 credits");
    call(&hosts, "internet_host_lookup", json!({"ip": "203.0.113.7"})).await;
    for (method, url, _) in transport.sent() {
        assert_eq!(method, "GET");
        let path = reqwest::Url::parse(&url).expect("url").path().to_owned();
        assert!(
            [
                "/shodan/host/search",
                "/shodan/host/count",
                "/shodan/host/203.0.113.7"
            ]
            .contains(&path.as_str()),
            "{path}"
        );
    }
    let source = include_str!("families/web/adapters/shodan.rs");
    let code: String = source
        .lines()
        .filter(|line| !line.trim_start().starts_with("//"))
        .collect();
    assert!(!code.contains("/shodan/scan") && !code.contains("/dns/domain"));
}

#[tokio::test(start_paused = true)]
async fn shodan_limits_one_request_per_second_per_key() {
    let meter = FakeMeter::new();
    let transport = FixtureTransport::answering(&[fixture("shodan", "host_lookup_ok")]);
    let hosts = host_intel_toolset(&shodan_settings("spaced-key"), Some(&meter), &transport);
    let started = tokio::time::Instant::now();
    for _ in 0..3 {
        call(&hosts, "internet_host_lookup", json!({"ip": "203.0.113.7"})).await;
    }
    assert!(started.elapsed() >= Duration::from_secs(2));
}
