//! Phase 2 chains: a `COMPACT` then `PUSH` segments, written, published,
//! fetched and verified; a missing prerequisite, a tampered `PUSH` and a
//! compare-and-set race refused; compaction supersedes the chain; a
//! `COMPACT`-only thread still reads.
#![allow(clippy::unwrap_used, clippy::expect_used)]

mod common;

use std::collections::BTreeMap;
use std::sync::Arc;

use elitea_workspace_store::bundle::{self, Limits, Written};
use elitea_workspace_store::chain::{self, ChainSegment};
use elitea_workspace_store::compaction::{ChainStats, Policy, SERVER_MAX_PUSH_CHAIN};
use elitea_workspace_store::digest::sha256_hex;
use elitea_workspace_store::fault::FaultTransport;
use elitea_workspace_store::memory::MemoryStore;
use elitea_workspace_store::objects::{self, ObjectDb, ObjectId};
use elitea_workspace_store::refs::Writer;
use elitea_workspace_store::sync::{self, Backoff};
use elitea_workspace_store::transport::{
    FetchPlan, PlanSegment, PublishRequest, RefUpdate, StoreTransport,
};
use elitea_workspace_store::{KIND_COMPACT, KIND_PUSH, StoreError};

use common::{commit_file, repo};

const HANDOFF: &str = "refs/elitea/handoff/1";
const CLOUD: &str = "refs/elitea/cloud";
const FAST: Backoff = Backoff::immediate(3);

fn id(sha: &str) -> ObjectId {
    objects::oid(sha).unwrap()
}

/// A handoff and three cloud turns: `(dir, db, handoff, [turns])`.
fn history() -> (tempfile::TempDir, ObjectDb, String, Vec<String>) {
    let (dir, top, handoff) = repo();
    let turns = vec![
        commit_file(&top, "src/one.rs", "pub fn one() {}\n"),
        commit_file(&top, "src/main.rs", "fn main() { one(); }\n"),
        commit_file(&top, "docs/notes.md", "# notes\n"),
    ];
    let db = ObjectDb::open_git_dir(&top.join(".git")).unwrap();
    (dir, db, handoff, turns)
}

/// The next `PUSH` of a writer whose store holds `published`.
fn push(db: &ObjectDb, published: &BTreeMap<String, String>, cloud: &str) -> Written {
    let prerequisites = chain::push_prerequisites(db, published).unwrap();
    bundle::write_push(
        db,
        &[(CLOUD.to_owned(), id(cloud))],
        &prerequisites,
        Limits::default(),
    )
    .unwrap()
}

fn compact(db: &ObjectDb, refs: &[(&str, &str)]) -> Written {
    let refs: Vec<(String, ObjectId)> = refs
        .iter()
        .map(|(name, sha)| ((*name).to_owned(), id(sha)))
        .collect();
    bundle::write_compact(db, &refs, Limits::default()).unwrap()
}

fn prerequisites_of(written: &Written) -> Vec<String> {
    bundle::parse_header(&written.bytes)
        .unwrap()
        .prerequisites
        .iter()
        .map(ToString::to_string)
        .collect()
}

/// One planned segment as Main lists it: the refs after it.
struct Planned {
    seq: u64,
    kind: &'static str,
    written: Written,
    prerequisites: Vec<String>,
    refs: BTreeMap<String, String>,
}

impl Planned {
    fn segment(&self) -> ChainSegment<'_> {
        ChainSegment {
            seq: self.seq,
            kind: self.kind,
            sha256: &self.written.sha256,
            prerequisites: &self.prerequisites,
            refs: &self.refs,
            bytes: &self.written.bytes,
        }
    }
}

/// The handoff's COMPACT, then one PUSH per turn.
fn planned_chain(db: &ObjectDb, handoff: &str, turns: &[String]) -> Vec<Planned> {
    let mut refs = BTreeMap::from([(HANDOFF.to_owned(), handoff.to_owned())]);
    let mut out = vec![Planned {
        seq: 1,
        kind: KIND_COMPACT,
        written: compact(db, &[(HANDOFF, handoff)]),
        prerequisites: Vec::new(),
        refs: refs.clone(),
    }];
    for (n, turn) in turns.iter().enumerate() {
        let written = push(db, &refs, turn);
        refs.insert(CLOUD.to_owned(), turn.clone());
        out.push(Planned {
            seq: n as u64 + 2,
            kind: KIND_PUSH,
            prerequisites: prerequisites_of(&written),
            written,
            refs: refs.clone(),
        });
    }
    out
}

fn quarantine(dir: &tempfile::TempDir, name: &str) -> std::path::PathBuf {
    let path = dir.path().join(name);
    bundle::init_quarantine(&path).unwrap();
    path
}

fn tree_of(db: &ObjectDb, commit: &str) -> BTreeMap<String, objects::FileEntry> {
    db.flatten(&db.commit(&id(commit)).unwrap().tree).unwrap()
}

#[test]
fn a_push_carries_only_what_its_prerequisites_do_not() {
    let (_dir, db, handoff, turns) = history();
    let full = compact(&db, &[(HANDOFF, &handoff), (CLOUD, &turns[0])]);
    let published = BTreeMap::from([(HANDOFF.to_owned(), handoff.clone())]);
    let increment = push(&db, &published, &turns[0]);
    // The turn's commit, its new root and src trees, and the one new file.
    assert_eq!(increment.objects, 4);
    assert_eq!(increment.blobs, 1);
    assert!(increment.bytes.len() < full.bytes.len());
    let header = bundle::parse_header(&increment.bytes).unwrap();
    assert_eq!(header.prerequisites, [id(&handoff)]);
    assert_eq!(header.refs, [(CLOUD.to_owned(), id(&turns[0]))]);
    // Nothing new: a PUSH is refused, the writer publishes a COMPACT.
    let nothing = bundle::write_push(
        &db,
        &[(CLOUD.to_owned(), id(&handoff))],
        &[id(&handoff)],
        Limits::default(),
    )
    .unwrap_err();
    assert!(nothing.to_string().contains("COMPACT"), "{nothing}");
    // A prerequisite the writer does not hold is never named.
    let foreign = BTreeMap::from([(HANDOFF.to_owned(), "1".repeat(40))]);
    assert_eq!(
        chain::push_prerequisites(&db, &foreign).unwrap(),
        Vec::<ObjectId>::new()
    );
}

#[test]
fn a_chain_verifies_and_reads_back_every_turn() {
    let (dir, db, handoff, turns) = history();
    let planned = planned_chain(&db, &handoff, &turns);
    let segments: Vec<ChainSegment<'_>> = planned.iter().map(Planned::segment).collect();
    let verified =
        chain::verify_chain_into(&segments, &quarantine(&dir, "q"), Limits::default()).unwrap();
    assert_eq!(verified.pack_offsets.len(), 4);
    assert_eq!(verified.ref_id(CLOUD), Some(id(&turns[2])));
    assert_eq!(verified.ref_id(HANDOFF), Some(id(&handoff)));
    for turn in &turns {
        assert_eq!(tree_of(&verified.db, turn), tree_of(&db, turn));
    }
    let chain = verified
        .db
        .first_parent_chain(&id(&turns[2]), &id(&handoff), 10)
        .unwrap()
        .unwrap();
    assert_eq!(chain.len(), 3);
}

#[test]
fn a_compact_only_chain_still_reads_as_one_segment() {
    let (dir, db, handoff, turns) = history();
    let written = compact(&db, &[(HANDOFF, &handoff), (CLOUD, &turns[2])]);
    let refs = bundle::parse_header(&written.bytes).unwrap().ref_map();
    let segment = ChainSegment {
        seq: 7,
        kind: KIND_COMPACT,
        sha256: &written.sha256,
        prerequisites: &[],
        refs: &refs,
        bytes: &written.bytes,
    };
    let chained =
        chain::verify_chain_into(&[segment], &quarantine(&dir, "a"), Limits::default()).unwrap();
    let single = bundle::verify_into(
        &written.bytes,
        &quarantine(&dir, "b"),
        &bundle::Expect {
            sha256: Some(&written.sha256),
            bytes: Some(written.bytes.len() as u64),
            refs: Some(&refs),
        },
        Limits::default(),
    )
    .unwrap();
    assert_eq!(chained.header.ref_map(), single.header.ref_map());
    assert_eq!(chained.objects, single.objects);
    assert_eq!(chained.pack_offsets, [single.pack_offset]);
}

/// The packs indexed into a quarantine.
fn packs(quarantine: &std::path::Path) -> usize {
    std::fs::read_dir(quarantine.join("objects/pack"))
        .unwrap()
        .filter(|entry| {
            entry
                .as_ref()
                .unwrap()
                .path()
                .extension()
                .is_some_and(|e| e == "pack")
        })
        .count()
}

#[test]
fn a_missing_prerequisite_is_refused_before_its_pack_is_indexed() {
    let (dir, db, handoff, turns) = history();
    let planned = planned_chain(&db, &handoff, &turns);
    // Turn 2's PUSH needs turn 1's commit; the chain skips turn 1.
    let segments = [planned[0].segment(), planned[2].segment()];
    let q = quarantine(&dir, "q");
    let error = chain::verify_chain_into(&segments, &q, Limits::default()).unwrap_err();
    assert!(matches!(error, StoreError::Integrity(_)), "{error}");
    assert!(error.to_string().contains("does not hold"), "{error}");
    assert_eq!(packs(&q), 1, "only the COMPACT was indexed");

    // A chain must start with a COMPACT.
    let error = chain::verify_chain_into(
        &[planned[1].segment()],
        &quarantine(&dir, "r"),
        Limits::default(),
    )
    .unwrap_err();
    assert!(error.to_string().contains("COMPACT"), "{error}");
}

#[test]
fn a_tampered_push_is_refused_with_nothing_usable() {
    let (dir, db, handoff, turns) = history();
    let planned = planned_chain(&db, &handoff, &turns);
    // A flipped byte: the content address no longer holds.
    let mut flipped = planned[1].written.bytes.clone();
    let last = flipped.len() - 30;
    flipped[last] ^= 0xff;
    let mut bad = planned[1].segment();
    bad.bytes = &flipped;
    let error = chain::verify_chain_into(
        &[planned[0].segment(), bad],
        &quarantine(&dir, "a"),
        Limits::default(),
    )
    .unwrap_err();
    assert!(error.to_string().contains("content address"), "{error}");

    // Re-addressed (a writer that lies about the bytes): the pack is broken.
    let readdressed = sha256_hex(&flipped);
    bad.sha256 = &readdressed;
    let q = quarantine(&dir, "b");
    let error =
        chain::verify_chain_into(&[planned[0].segment(), bad], &q, Limits::default()).unwrap_err();
    assert!(matches!(error, StoreError::Integrity(_)), "{error}");
    assert_eq!(packs(&q), 1);

    // Refs other than the plan's.
    let mut lying = planned[1].segment();
    let other = BTreeMap::from([
        (HANDOFF.to_owned(), handoff.clone()),
        (CLOUD.to_owned(), turns[1].clone()),
    ]);
    lying.refs = &other;
    let error = chain::verify_chain_into(
        &[planned[0].segment(), lying],
        &quarantine(&dir, "c"),
        Limits::default(),
    )
    .unwrap_err();
    assert!(error.to_string().contains("plan"), "{error}");

    // Prerequisites other than the plan's.
    let mut renamed = planned[2].segment();
    let named = vec![handoff.clone()];
    renamed.prerequisites = &named;
    let error = chain::verify_chain_into(
        &[planned[0].segment(), planned[1].segment(), renamed],
        &quarantine(&dir, "d"),
        Limits::default(),
    )
    .unwrap_err();
    assert!(error.to_string().contains("prerequisites"), "{error}");
}

fn plan_segment(seq: u64, kind: &str, prerequisites: usize) -> PlanSegment {
    PlanSegment {
        seq,
        kind: kind.into(),
        sha256: "a".repeat(64),
        bytes: 1,
        prerequisites: vec!["b".repeat(40); prerequisites],
        refs: BTreeMap::new(),
        url: String::new(),
        expires_at: None,
    }
}

#[test]
fn a_fetch_plan_out_of_shape_is_refused_before_a_byte_is_fetched() {
    let plan = |segments: Vec<PlanSegment>| FetchPlan {
        head_seq: segments.last().map_or(0, |s| s.seq),
        refs: BTreeMap::new(),
        segments,
    };
    assert!(
        chain::check_plan(&plan(vec![
            plan_segment(1, KIND_COMPACT, 0),
            plan_segment(2, KIND_PUSH, 1)
        ]))
        .is_ok()
    );
    for bad in [
        vec![],
        vec![plan_segment(2, KIND_PUSH, 1)],
        vec![plan_segment(1, KIND_COMPACT, 1)],
        vec![
            plan_segment(2, KIND_COMPACT, 0),
            plan_segment(1, KIND_PUSH, 1),
        ],
        vec![
            plan_segment(1, KIND_COMPACT, 0),
            plan_segment(2, KIND_COMPACT, 0),
        ],
        vec![
            plan_segment(1, KIND_COMPACT, 0),
            plan_segment(2, KIND_PUSH, 0),
        ],
        vec![
            plan_segment(1, KIND_COMPACT, 0),
            plan_segment(2, KIND_PUSH, 9),
        ],
    ] {
        assert!(chain::check_plan(&plan(bad.clone())).is_err(), "{bad:?}");
    }
    let mut not_head = plan(vec![plan_segment(1, KIND_COMPACT, 0)]);
    not_head.head_seq = 2;
    assert!(chain::check_plan(&not_head).is_err());
    let unknown = plan(vec![
        plan_segment(1, KIND_COMPACT, 0),
        plan_segment(2, "DELTA", 1),
    ]);
    assert!(matches!(
        chain::check_plan(&unknown),
        Err(StoreError::Unsupported(_))
    ));
}

fn request(written: &Written, kind: &str, refs: Vec<RefUpdate>) -> PublishRequest {
    PublishRequest {
        epoch: 1,
        kind: kind.into(),
        segment_sha256: written.sha256.clone(),
        bytes: written.bytes.len() as u64,
        prerequisites: if kind == KIND_PUSH {
            prerequisites_of(written)
        } else {
            Vec::new()
        },
        refs,
        object_format: "sha1".into(),
    }
}

fn update(name: &str, old: Option<&str>, new: &str) -> RefUpdate {
    RefUpdate {
        name: name.into(),
        old: old.map(str::to_owned),
        new: new.into(),
    }
}

/// The desktop's COMPACT, then the executor's turns as PUSH segments.
async fn published_chain(store: &Arc<MemoryStore>, db: &ObjectDb, handoff: &str, turns: &[String]) {
    let first = compact(db, &[(HANDOFF, handoff)]);
    sync::upload_and_publish(
        store.as_ref(),
        &first.bytes,
        &request(&first, KIND_COMPACT, vec![update(HANDOFF, None, handoff)]),
        FAST,
    )
    .await
    .unwrap();
    store.transition(1, Writer::Executor);
    let mut old: Option<String> = None;
    for turn in turns {
        let written = push(db, &store.refs(), turn);
        let published = sync::upload_and_publish(
            store.as_ref(),
            &written.bytes,
            &request(
                &written,
                KIND_PUSH,
                vec![update(CLOUD, old.as_deref(), turn)],
            ),
            FAST,
        )
        .await
        .unwrap();
        assert!(published.superseded.is_empty(), "a PUSH supersedes nothing");
        old = Some(turn.clone());
    }
}

#[tokio::test]
async fn a_published_chain_is_fetched_whole_and_verifies() {
    let (dir, db, handoff, turns) = history();
    let store = Arc::new(MemoryStore::new(1, Writer::Desktop));
    published_chain(&store, &db, &handoff, &turns).await;
    assert_eq!(
        store.live_kinds(),
        [KIND_COMPACT, KIND_PUSH, KIND_PUSH, KIND_PUSH]
    );
    let (plan, bytes) = sync::fetch_chain(store.as_ref(), FAST).await.unwrap();
    assert_eq!(plan.segments.len(), 4);
    let segments: Vec<ChainSegment<'_>> = plan
        .segments
        .iter()
        .zip(&bytes)
        .map(|(segment, bytes)| ChainSegment {
            seq: segment.seq,
            kind: &segment.kind,
            sha256: &segment.sha256,
            prerequisites: &segment.prerequisites,
            refs: &segment.refs,
            bytes,
        })
        .collect();
    let verified =
        chain::verify_chain_into(&segments, &quarantine(&dir, "q"), Limits::default()).unwrap();
    assert_eq!(verified.header.ref_map(), plan.refs);
    // A reader that holds seq 3 gets only the PUSH after it.
    let since = store.fetch_plan(3).await.unwrap();
    assert_eq!(
        since.segments.iter().map(|s| s.seq).collect::<Vec<_>>(),
        [4]
    );
}

#[tokio::test]
async fn a_tampered_stored_push_never_passes_the_fetch() {
    let (_dir, db, handoff, turns) = history();
    let store = Arc::new(MemoryStore::new(1, Writer::Desktop));
    published_chain(&store, &db, &handoff, &turns).await;
    let plan = store.fetch_plan(0).await.unwrap();
    store.tamper(&plan.segments[2].sha256, 40);
    let error = sync::fetch_chain(store.as_ref(), FAST).await.unwrap_err();
    assert!(matches!(error, StoreError::Integrity(_)), "{error}");
}

#[tokio::test]
async fn two_pushes_from_one_head_race_and_one_meets_a_ref_conflict() {
    let (_dir, db, handoff, turns) = history();
    let store = Arc::new(MemoryStore::new(1, Writer::Desktop));
    published_chain(&store, &db, &handoff, &turns[..1]).await;
    let published = store.refs();
    // Two writers believe the cloud ref is turn 1.
    let a = push(&db, &published, &turns[1]);
    let b = push(&db, &published, &turns[2]);
    let faulty = FaultTransport::new(store.clone());
    for written in [&a, &b] {
        sync::upload(&faulty, 1, &written.bytes, FAST)
            .await
            .unwrap();
    }
    let won = sync::publish(
        &faulty,
        &request(
            &a,
            KIND_PUSH,
            vec![update(CLOUD, Some(&turns[0]), &turns[1])],
        ),
        FAST,
    )
    .await
    .unwrap();
    let lost = sync::publish(
        &faulty,
        &request(
            &b,
            KIND_PUSH,
            vec![update(CLOUD, Some(&turns[0]), &turns[2])],
        ),
        FAST,
    )
    .await
    .unwrap_err();
    assert_eq!(lost.code(), "ref_conflict");
    assert_eq!(store.refs()[CLOUD], turns[1]);
    // The winner's replay is answered with its original result.
    let replay = sync::publish(
        &faulty,
        &request(
            &a,
            KIND_PUSH,
            vec![update(CLOUD, Some(&turns[0]), &turns[1])],
        ),
        FAST,
    )
    .await
    .unwrap();
    assert_eq!(replay.seq, won.seq);
}

#[tokio::test]
async fn a_push_needs_known_prerequisites_and_room_in_the_chain() {
    let (_dir, db, handoff, turns) = history();
    let store = Arc::new(MemoryStore::new(1, Writer::Desktop));
    published_chain(&store, &db, &handoff, &turns[..1]).await;
    // A prerequisite no ref holds.
    let written = bundle::write_push(
        &db,
        &[(CLOUD.to_owned(), id(&turns[2]))],
        &[id(&turns[1])],
        Limits::default(),
    )
    .unwrap();
    sync::upload(store.as_ref(), 1, &written.bytes, FAST)
        .await
        .unwrap();
    let error = sync::publish(
        store.as_ref(),
        &request(
            &written,
            KIND_PUSH,
            vec![update(CLOUD, Some(&turns[0]), &turns[2])],
        ),
        FAST,
    )
    .await
    .unwrap_err();
    assert_eq!(error.code(), "prerequisite_unknown");

    // Main's bound on the chain: fake segments, the store checks no bytes
    // beyond their address.
    let mut old = turns[0].clone();
    for n in 1..SERVER_MAX_PUSH_CHAIN {
        let bytes = format!("segment {n}").into_bytes();
        let new = format!("{n:040x}");
        let published = sync::upload_and_publish(
            store.as_ref(),
            &bytes,
            &PublishRequest {
                epoch: 1,
                kind: KIND_PUSH.into(),
                segment_sha256: sha256_hex(&bytes),
                bytes: bytes.len() as u64,
                prerequisites: vec![old.clone()],
                refs: vec![update(CLOUD, Some(&old), &new)],
                object_format: "sha1".into(),
            },
            FAST,
        )
        .await;
        assert!(published.is_ok(), "push {n}: {published:?}");
        old = new;
    }
    let bytes = b"one too many".to_vec();
    let error = sync::upload_and_publish(
        store.as_ref(),
        &bytes,
        &PublishRequest {
            epoch: 1,
            kind: KIND_PUSH.into(),
            segment_sha256: sha256_hex(&bytes),
            bytes: bytes.len() as u64,
            prerequisites: vec![old.clone()],
            refs: vec![update(CLOUD, Some(&old), &"f".repeat(40))],
            object_format: "sha1".into(),
        },
        FAST,
    )
    .await
    .unwrap_err();
    assert_eq!(error.code(), "segment_chain_too_long");
}

#[tokio::test]
async fn the_policy_compacts_and_the_compact_supersedes_the_chain() {
    let (dir, db, handoff, turns) = history();
    let store = Arc::new(MemoryStore::new(1, Writer::Desktop));
    published_chain(&store, &db, &handoff, &turns[..2]).await;
    let plan = store.fetch_plan(0).await.unwrap();
    let stats = ChainStats::of(plan.segments.iter().map(|s| (s.kind.as_str(), s.bytes)));
    assert_eq!(stats.push_segments, 2);
    let policy = Policy {
        max_push_segments: 2,
        ..Policy::default()
    };
    assert_eq!(policy.next_kind(stats, 1), KIND_COMPACT);
    let full = compact(&db, &[(HANDOFF, &handoff), (CLOUD, &turns[2])]);
    let published = sync::upload_and_publish(
        store.as_ref(),
        &full.bytes,
        &request(
            &full,
            KIND_COMPACT,
            vec![update(CLOUD, Some(&turns[1]), &turns[2])],
        ),
        FAST,
    )
    .await
    .unwrap();
    assert_eq!(published.superseded.len(), 3, "the COMPACT and both PUSH");
    assert_eq!(store.live_kinds(), [KIND_COMPACT]);
    let (plan, bytes) = sync::fetch_chain(store.as_ref(), FAST).await.unwrap();
    assert_eq!(plan.segments.len(), 1);
    let segment = &plan.segments[0];
    let verified = chain::verify_chain_into(
        &[ChainSegment {
            seq: segment.seq,
            kind: &segment.kind,
            sha256: &segment.sha256,
            prerequisites: &segment.prerequisites,
            refs: &segment.refs,
            bytes: &bytes[0],
        }],
        &quarantine(&dir, "q"),
        Limits::default(),
    )
    .unwrap();
    assert_eq!(verified.ref_id(CLOUD), Some(id(&turns[2])));
    // The next turn starts a new chain on the new COMPACT.
    let next = ChainStats::of(plan.segments.iter().map(|s| (s.kind.as_str(), s.bytes)));
    assert_eq!(policy.next_kind(next, 1), KIND_PUSH);
}
