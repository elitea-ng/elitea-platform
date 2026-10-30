use std::cell::Cell;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};

use async_trait::async_trait;

use super::{
    DelegationAssertionError, DelegationAssertionSource, DelegationAssertions, FAILURE_BACKOFF,
    MintedAssertion, REFRESH_MARGIN,
};

const ENDPOINT: &str = "https://Partner.example/app/9/mcp/";
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
    // The endpoint goes to Main as configured: Main normalizes it.
    assert_eq!(
        source.audiences.lock().expect("audiences").as_slice(),
        [ENDPOINT]
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
        assertions.assertion_for("").await,
        Err(DelegationAssertionError::UnusableAudience)
    );
}

// A failed mint is answered from the cache for FAILURE_BACKOFF, per
// endpoint, in both kinds of run; then Main is asked again.
#[tokio::test]
async fn a_failed_mint_backs_off_per_endpoint() {
    for required in [true, false] {
        let failing = Source::new(Err(DelegationAssertionError::Unavailable));
        let assertions = DelegationAssertions::with_clock(failing.clone(), required, now);
        let want = if required {
            Err(DelegationAssertionError::Unavailable)
        } else {
            Ok(None)
        };
        for _ in 0..5 {
            assert_eq!(assertions.assertion_for(ENDPOINT).await, want);
        }
        assert_eq!(failing.calls(), 1, "required={required}");
        // Another endpoint has its own entry.
        assert_eq!(
            assertions.assertion_for("https://other.example/mcp").await,
            want
        );
        assert_eq!(failing.calls(), 2);
        advance(i64::try_from(FAILURE_BACKOFF.as_millis()).expect("backoff"));
        assert_eq!(assertions.assertion_for(ENDPOINT).await, want);
        assert_eq!(failing.calls(), 3);
    }
}

#[tokio::test]
async fn any_other_run_calls_without_an_assertion() {
    let failing = Source::new(Err(DelegationAssertionError::Unavailable));
    let assertions = DelegationAssertions::with_clock(failing.clone(), false, now);
    assert_eq!(assertions.assertion_for(ENDPOINT).await, Ok(None));
    assert_eq!(failing.calls(), 1);

    // "Disabled" is remembered for the rest of the run.
    let disabled = Source::new(Ok(MintedAssertion::Disabled));
    let assertions = DelegationAssertions::with_clock(disabled.clone(), false, now);
    assert_eq!(assertions.assertion_for(ENDPOINT).await, Ok(None));
    assert_eq!(assertions.assertion_for(ENDPOINT).await, Ok(None));
    advance(60 * 60 * 1_000);
    assert_eq!(assertions.assertion_for(ENDPOINT).await, Ok(None));
    assert_eq!(disabled.calls(), 1);
}
