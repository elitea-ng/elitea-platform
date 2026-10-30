use std::cell::Cell;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use async_trait::async_trait;

use super::{
    DelegationAssertionError, DelegationAssertionSource, DelegationAssertions, MintedAssertion,
    REFRESH_MARGIN, normalize_audience,
};

const ENDPOINT: &str = "https://Partner.example/app/9/mcp/";
const AUDIENCE: &str = "https://partner.example/app/9/mcp";
const TOKEN: &str = "eyJhbGciOiJFUzI1NiJ9.eyJpc3MiOiJ4In0.c2ln";

thread_local! {
    // The current-thread test runtime runs every test on its own thread.
    static NOW: Cell<i64> = const { Cell::new(1_900_000_000_000) };
}

fn now() -> i64 {
    NOW.with(Cell::get)
}

fn advance(millis: i64) {
    NOW.with(|now| now.set(now.get() + millis));
}

/// Answers each mint with `answer`, counting the calls and the audiences.
struct Source {
    answer: Result<MintedAssertion, DelegationAssertionError>,
    calls: AtomicUsize,
    audiences: std::sync::Mutex<Vec<String>>,
}

impl Source {
    fn new(answer: Result<MintedAssertion, DelegationAssertionError>) -> Arc<Self> {
        Arc::new(Self {
            answer,
            calls: AtomicUsize::new(0),
            audiences: std::sync::Mutex::new(Vec::new()),
        })
    }

    fn issued(lifetime_millis: i64) -> Arc<Self> {
        Self::new(Ok(MintedAssertion::Issued {
            token: TOKEN.to_owned(),
            expires_at_unix_millis: now() + lifetime_millis,
        }))
    }

    fn calls(&self) -> usize {
        self.calls.load(Ordering::SeqCst)
    }
}

#[async_trait]
impl DelegationAssertionSource for Source {
    async fn mint(&self, audience: &str) -> Result<MintedAssertion, DelegationAssertionError> {
        self.calls.fetch_add(1, Ordering::SeqCst);
        self.audiences
            .lock()
            .expect("audiences")
            .push(audience.to_owned());
        self.answer.clone()
    }
}

#[test]
fn the_audience_is_normalized_as_main_does() {
    for (endpoint, want) in [
        (
            "https://MCP.Example.com/app/1/mcp/",
            Some("https://mcp.example.com/app/1/mcp"),
        ),
        (
            "https://mcp.example.com:443/mcp",
            Some("https://mcp.example.com/mcp"),
        ),
        (
            "https://mcp.example.com:8443/mcp",
            Some("https://mcp.example.com:8443/mcp"),
        ),
        ("https://mcp.example.com", Some("https://mcp.example.com")),
        ("https://[::1]:9443/mcp", Some("https://[::1]:9443/mcp")),
        ("http://mcp.example.com/mcp", None),
        ("https://user:pw@mcp.example.com/mcp", None),
        ("https://mcp.example.com/mcp?x=1", None),
        ("https://mcp.example.com/mcp#fragment", None),
        ("not a url", None),
    ] {
        assert_eq!(normalize_audience(endpoint).as_deref(), want, "{endpoint}");
    }
}

#[tokio::test]
async fn an_assertion_is_cached_per_audience_until_shortly_before_it_expires() {
    let source = Source::issued(5 * 60 * 1_000);
    let assertions = DelegationAssertions::with_clock(source.clone(), true, now);
    for _ in 0..3 {
        assert_eq!(
            assertions.assertion_for(ENDPOINT).await,
            Ok(Some(TOKEN.to_owned()))
        );
    }
    assert_eq!(source.calls(), 1);
    assert_eq!(
        source.audiences.lock().expect("audiences").as_slice(),
        [AUDIENCE]
    );
    // Within the refresh margin of its expiry, a new one is minted.
    advance(5 * 60 * 1_000 - i64::try_from(REFRESH_MARGIN.as_millis()).expect("margin"));
    assertions.assertion_for(ENDPOINT).await.expect("refresh");
    assert_eq!(source.calls(), 2);
}

#[tokio::test]
async fn a_governed_run_refuses_a_call_it_cannot_sign() {
    for (answer, want) in [
        (
            Err(DelegationAssertionError::Unavailable),
            DelegationAssertionError::Unavailable,
        ),
        (
            Ok(MintedAssertion::Disabled),
            DelegationAssertionError::Disabled,
        ),
        (
            Ok(MintedAssertion::Issued {
                token: "not a jws".to_owned(),
                expires_at_unix_millis: now() + 60_000,
            }),
            DelegationAssertionError::Unavailable,
        ),
    ] {
        let assertions = DelegationAssertions::with_clock(Source::new(answer), true, now);
        assert_eq!(assertions.assertion_for(ENDPOINT).await, Err(want));
    }
    let assertions = DelegationAssertions::with_clock(Source::issued(60_000), true, now);
    assert_eq!(
        assertions.assertion_for("http://partner.example/mcp").await,
        Err(DelegationAssertionError::UnusableAudience)
    );
}

#[tokio::test]
async fn any_other_run_calls_without_an_assertion() {
    let failing = Source::new(Err(DelegationAssertionError::Unavailable));
    let assertions = DelegationAssertions::with_clock(failing.clone(), false, now);
    assert_eq!(assertions.assertion_for(ENDPOINT).await, Ok(None));
    // A failure is not cached: the next call asks again.
    assert_eq!(assertions.assertion_for(ENDPOINT).await, Ok(None));
    assert_eq!(failing.calls(), 2);

    // "Disabled" is remembered for a while.
    let disabled = Source::new(Ok(MintedAssertion::Disabled));
    let assertions = DelegationAssertions::with_clock(disabled.clone(), false, now);
    assert_eq!(assertions.assertion_for(ENDPOINT).await, Ok(None));
    assert_eq!(assertions.assertion_for(ENDPOINT).await, Ok(None));
    assert_eq!(disabled.calls(), 1);
    advance(2 * 60 * 1_000);
    assert_eq!(assertions.assertion_for(ENDPOINT).await, Ok(None));
    assert_eq!(disabled.calls(), 2);
}
