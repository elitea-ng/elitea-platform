//! The Go/Rust contract and the resolver's rule.

use std::collections::HashMap;
use std::io;
use std::net::IpAddr;
use std::sync::Arc;

use elitea_egress_guard::GuardedResolver;
use elitea_egress_guard::{
    Allowlist, Class, Lookup, MAX_RESOLVED_ADDRESSES, PublicPolicy, ResolveError, classify,
    resolve_checked, resolve_checked_with,
};
use serde::Deserialize;

#[derive(Deserialize)]
struct VectorFile {
    vectors: Vec<Vector>,
}

#[derive(Deserialize)]
struct Vector {
    ip: String,
    verdict: String,
    liftable: bool,
}

/// The vector file `addrcheck.TestClassifyMatchesSharedVectors` reads too.
const VECTORS: &str = include_str!("../../../go/egresslib/testdata/classify_vectors.json");

#[test]
fn classify_matches_shared_vectors() {
    let file: VectorFile = serde_json::from_str(VECTORS).expect("vector file");
    assert!(file.vectors.len() >= 40, "the vector file lost its cases");
    for vector in file.vectors {
        let address: IpAddr = vector.ip.parse().expect("vector address");
        let got = classify(address);
        assert_eq!(got.as_str(), vector.verdict, "classify({})", vector.ip);
        assert_eq!(
            got == Class::Private,
            vector.liftable,
            "{}: liftable disagrees with the verdict",
            vector.ip
        );
    }
}

struct FakeLookup(HashMap<&'static str, Vec<IpAddr>>);

impl Lookup for FakeLookup {
    fn lookup(&self, host: String) -> elitea_egress_guard::LookupFuture {
        let answer = self.0.get(host.as_str()).cloned();
        Box::pin(async move { answer.ok_or_else(|| io::Error::other("no such host")) })
    }
}

fn ip(text: &str) -> IpAddr {
    text.parse().expect("address")
}

fn fake() -> FakeLookup {
    FakeLookup(HashMap::from([
        (
            "mixed.example",
            vec![ip("93.184.216.34"), ip("10.0.0.1"), ip("169.254.169.254")],
        ),
        ("private.example", vec![ip("192.168.1.10")]),
        (
            "metadata.example",
            vec![ip("169.254.169.254"), ip("fd00:ec2::254")],
        ),
        ("rebind.example", vec![ip("127.0.0.1")]),
        ("public.example", vec![ip("93.184.216.34")]),
        ("api.public.example", vec![ip("93.184.216.35")]),
        (
            "many.example",
            (0..=MAX_RESOLVED_ADDRESSES)
                .map(|index| IpAddr::from([93, 184, 216, u8::try_from(index).expect("index")]))
                .collect(),
        ),
    ]))
}

#[tokio::test]
async fn resolver_drops_private_addresses() {
    let lookup = fake();
    let none = Allowlist::default().private_permits();
    let admitted = resolve_checked(&lookup, "mixed.example", 443, &none)
        .await
        .expect("the public address remains");
    assert_eq!(admitted, vec![ip("93.184.216.34")]);
    assert_eq!(
        resolve_checked(&lookup, "private.example", 443, &none).await,
        Err(ResolveError::NotPublic)
    );
    assert_eq!(
        resolve_checked(&lookup, "rebind.example", 443, &none).await,
        Err(ResolveError::NotPublic),
        "a public-looking name that resolves to loopback is refused"
    );
    assert_eq!(
        resolve_checked(&lookup, "localhost", 443, &none).await,
        Err(ResolveError::NotPublic)
    );
    assert_eq!(
        resolve_checked(&lookup, "many.example", 443, &none).await,
        Err(ResolveError::TooManyAddresses)
    );
    assert_eq!(
        resolve_checked(&lookup, "missing.example", 443, &none).await,
        Err(ResolveError::Lookup)
    );
    let permitted = Allowlist::parse_csv("192.168.1.0/24")
        .expect("list")
        .private_permits();
    assert_eq!(
        resolve_checked(&lookup, "private.example", 443, &permitted).await,
        Ok(vec![ip("192.168.1.10")]),
        "a private range the allowlist names is lifted"
    );
}

#[tokio::test]
async fn refuses_metadata_even_when_allowlisted() {
    let lookup = fake();
    let wide = Allowlist::parse_csv("0.0.0.0/0,::/0,169.254.0.0/16,fd00::/8")
        .expect("list")
        .private_permits();
    assert_eq!(
        resolve_checked(&lookup, "metadata.example", 443, &wide).await,
        Err(ResolveError::NotPublic)
    );
    assert_eq!(
        resolve_checked(&lookup, "169.254.169.254", 80, &wide).await,
        Err(ResolveError::NotPublic)
    );
}

#[tokio::test]
async fn guarded_resolver_hands_reqwest_checked_addresses_only() {
    use reqwest::dns::Resolve as _;
    let resolver = GuardedResolver::new(
        Arc::new(fake()),
        443,
        Allowlist::default().private_permits(),
    );
    let addresses: Vec<_> = resolver
        .resolve("mixed.example".parse().expect("name"))
        .await
        .expect("resolved")
        .collect();
    assert_eq!(
        addresses,
        vec!["93.184.216.34:443".parse().expect("socket")]
    );
    assert!(
        resolver
            .resolve("metadata.example".parse().expect("name"))
            .await
            .is_err()
    );
}

#[tokio::test]
async fn open_policy_changes_nothing() {
    let lookup = fake();
    let none = Allowlist::default().private_permits();
    let open = PublicPolicy::Open;
    assert_eq!(
        resolve_checked_with(&lookup, "public.example", 443, &none, &open).await,
        Ok(vec![ip("93.184.216.34")])
    );
    assert_eq!(
        resolve_checked_with(&lookup, "private.example", 443, &none, &open).await,
        Err(ResolveError::NotPublic)
    );
}

#[tokio::test]
async fn allowlist_policy_refuses_unnamed_hosts_before_lookup() {
    let lookup = fake();
    let none = Allowlist::default().private_permits();
    let policy = PublicPolicy::allowlist([
        "*.public.example:443",
        "public.example:8443",
        "93.184.216.0/24",
    ])
    .expect("policy");
    // Wildcard on its pinned port.
    assert_eq!(
        resolve_checked_with(&lookup, "api.public.example", 443, &none, &policy).await,
        Ok(vec![ip("93.184.216.35")])
    );
    assert_eq!(
        resolve_checked_with(&lookup, "api.public.example", 80, &none, &policy).await,
        Err(ResolveError::NotAllowed)
    );
    // The exact host on its own port only.
    assert_eq!(
        resolve_checked_with(&lookup, "public.example", 8443, &none, &policy).await,
        Ok(vec![ip("93.184.216.34")])
    );
    assert_eq!(
        resolve_checked_with(&lookup, "public.example", 443, &none, &policy).await,
        Err(ResolveError::NotAllowed)
    );
    // A name the lookup does not even know is refused by the policy first.
    assert_eq!(
        resolve_checked_with(&lookup, "unknown.example", 443, &none, &policy).await,
        Err(ResolveError::NotAllowed)
    );
    // IP literals: the CIDR names them; another block does not.
    assert_eq!(
        resolve_checked_with(&lookup, "93.184.216.99", 443, &none, &policy).await,
        Ok(vec![ip("93.184.216.99")])
    );
    assert_eq!(
        resolve_checked_with(&lookup, "198.51.100.7", 443, &none, &policy).await,
        Err(ResolveError::NotAllowed)
    );
}

#[tokio::test]
async fn allowlist_never_lifts_the_ssrf_rule() {
    let lookup = fake();
    let none = Allowlist::default().private_permits();
    let policy =
        PublicPolicy::allowlist(["metadata.example", "169.254.169.254", "private.example"])
            .expect("policy");
    for host in ["metadata.example", "169.254.169.254", "private.example"] {
        assert_eq!(
            resolve_checked_with(&lookup, host, 443, &none, &policy).await,
            Err(ResolveError::NotPublic),
            "{host}"
        );
    }
}

#[tokio::test]
async fn guarded_resolver_applies_the_public_policy() {
    use reqwest::dns::Resolve as _;
    let resolver = GuardedResolver::new(
        Arc::new(fake()),
        443,
        Allowlist::default().private_permits(),
    )
    .with_public_policy(PublicPolicy::allowlist(["public.example"]).expect("policy"));
    let addresses: Vec<_> = resolver
        .resolve("public.example".parse().expect("name"))
        .await
        .expect("resolved")
        .collect();
    assert_eq!(
        addresses,
        vec!["93.184.216.34:443".parse().expect("socket")]
    );
    let refused = resolver
        .resolve("mixed.example".parse().expect("name"))
        .await
        .err()
        .expect("refused");
    assert_eq!(
        refused.downcast_ref::<ResolveError>(),
        Some(&ResolveError::NotAllowed)
    );
}
