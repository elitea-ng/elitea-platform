use super::*;
use std::sync::Mutex;
use std::sync::atomic::{AtomicUsize, Ordering};

/// A model that answers by prompt kind, recording every prompt.
fn scripted(
    entity: impl Fn(&str) -> String + Send + Sync + 'static,
    fact: impl Fn(&str) -> String + Send + Sync + 'static,
    relation: impl Fn(&str) -> String + Send + Sync + 'static,
) -> (Model, Arc<Mutex<Vec<String>>>) {
    let seen = Arc::new(Mutex::new(Vec::new()));
    let log = Arc::clone(&seen);
    let entity = Arc::new(entity);
    let fact = Arc::new(fact);
    let relation = Arc::new(relation);
    let model = Model::new(move |prompt: String| {
        log.lock().map(|mut l| l.push(prompt.clone())).ok();
        let answer = if prompt.starts_with("Extract semantic entities") {
            entity(&prompt)
        } else if prompt.starts_with("Extract SEMANTIC relationships") {
            relation(&prompt)
        } else {
            fact(&prompt)
        };
        Box::pin(async move { Ok(answer) })
    });
    (model, seen)
}

fn quick() -> Tuning {
    Tuning {
        retry_unit: Duration::ZERO,
        ..Tuning::default()
    }
}

#[tokio::test]
async fn entities_and_facts_are_converted_as_the_python_pipeline_did() {
    let (model, seen) = scripted(
        |_| {
            r#"```json
[
 {"id": "x1", "type": "Business Rules", "name": "Refund window", "line_start": 4, "line_end": 4,
  "properties": {"description": "30 days", "name": "Refund window"}},
 {"id": "x2", "type": "class", "name": "Users", "line_start": 1, "line_end": 9, "properties": {}},
 {"id": "x3", "type": "business_rule", "name": "refund_window", "line_start": 7, "line_end": 12,
  "properties": {"owner": "billing"}}
]
```"#
                .to_owned()
        },
        |prompt| {
            if prompt.starts_with("Analyze the following code") {
                r#"[{"fact_type": "behavior", "subject": "refund", "predicate": "sends", "object": "email", "line_start": 2, "line_end": 3, "confidence": 0.9}]"#.to_owned()
            } else {
                r#"[{"fact_type": "decision", "title": "Use Postgres", "line_start": 1, "line_end": 2},
                    {"fact_type": "opinion", "title": "dropped"},
                    {"fact_type": "decision", "title": "unknown"}]"#
                    .to_owned()
            }
        },
        |_| "[]".to_owned(),
    );
    let parser_names: HashSet<String> = ["users".to_owned()].into_iter().collect();
    let text = "line\n".repeat(30);
    let stop = StopSignal::default();
    let Ok(extraction) = extract_file(
        &model,
        &FileInput {
            path: "docs/refunds.md",
            text: &text,
            content_hash: "h",
            source_toolkit: "repo",
            parser_names: &parser_names,
        },
        quick(),
        &stop,
    )
    .await
    else {
        panic!("extracts");
    };
    let kinds: Vec<(&str, &str)> = extraction
        .entities
        .iter()
        .map(|e| (e.name.as_str(), e.entity_type.as_str()))
        .collect();
    // As the Python pipeline: the extractor deduplicates by the RAW type
    // and the file by the exact lowercased name, so "Business Rules" /
    // "Refund window" and "business_rule" / "refund_window" stay two (and
    // normalise differently: the map sends `business_rule` to `rule`).
    assert_eq!(
        kinds,
        [
            ("Refund window", "business_rule"),
            ("refund_window", "rule"),
            ("Use Postgres", "fact")
        ],
        "the parser's class is dropped; the text fact keeps its title"
    );
    let rule = &extraction.entities[0];
    assert_eq!(
        rule.id,
        entity_id("business_rule", "Refund window", Some("docs/refunds.md"))
    );
    assert_eq!(
        rule.citation.line_start,
        Some(2),
        "a one-line range widens to five"
    );
    assert_eq!(rule.citation.line_end, Some(6));
    assert_eq!(rule.properties["source_toolkit"], json!("repo"));
    assert_eq!(
        rule.properties["properties"]["description"],
        json!("30 days")
    );
    let fact = &extraction.entities[2];
    assert_eq!(fact.properties["fact_type"], json!("decision"));
    assert_eq!(fact.properties["confidence"], json!(0.7));
    assert_eq!(
        fact.id,
        entity_id("fact", "decision_Use Postgres", Some("docs/refunds.md"))
    );
    let prompts = seen.lock().map(|p| p.clone()).unwrap_or_default();
    assert!(
        prompts
            .iter()
            .any(|p| p.contains("   1 | line") && p.contains("Source file: docs/refunds.md"))
    );
}

#[tokio::test]
async fn citations_past_the_first_chunk_are_absolute() {
    let (model, _) = scripted(
        |prompt| {
            if prompt.contains("SECOND") {
                r#"[{"type": "feature", "name": "Second part", "line_start": 2, "line_end": 6}]"#
                    .to_owned()
            } else {
                "[]".to_owned()
            }
        },
        |_| "[]".to_owned(),
        |_| "[]".to_owned(),
    );
    let first = "first paragraph words\n".repeat(30);
    let text = format!("{first}\n\nSECOND\n{}", "more words here\n".repeat(30));
    let start = chunk::chunks("a.md", &text)
        .into_iter()
        .find(|c| c.text.contains("SECOND"))
        .map(|c| c.start_line)
        .unwrap_or_default();
    assert!(start > 1);
    let Ok(extraction) = extract_file(
        &model,
        &FileInput {
            path: "a.md",
            text: &text,
            content_hash: "h",
            source_toolkit: "repo",
            parser_names: &HashSet::new(),
        },
        quick(),
        &StopSignal::default(),
    )
    .await
    else {
        panic!("extracts");
    };
    let entity = extraction
        .entities
        .first()
        .unwrap_or_else(|| panic!("the entity"));
    assert_eq!(entity.citation.line_start, i64::try_from(start + 1).ok());
}

#[tokio::test]
async fn a_failing_call_is_retried_then_counted() {
    let calls = Arc::new(AtomicUsize::new(0));
    let counter = Arc::clone(&calls);
    let model = Model::new(move |prompt: String| {
        let n = counter.fetch_add(1, Ordering::SeqCst);
        Box::pin(async move {
            if !prompt.starts_with("Extract semantic entities") {
                return Ok("[]".to_owned());
            }
            match n {
                0 => Err(EngineError::new(ErrorType::Runtime, "502")),
                1 => Ok("I cannot answer that".to_owned()),
                _ => Ok(r#"[{"type": "feature", "name": "Late"}]"#.to_owned()),
            }
        })
    });
    let Ok(extraction) = extract_file(
        &model,
        &FileInput {
            path: "a.md",
            text: "text\n",
            content_hash: "h",
            source_toolkit: "repo",
            parser_names: &HashSet::new(),
        },
        quick(),
        &StopSignal::default(),
    )
    .await
    else {
        panic!("extracts");
    };
    assert_eq!(extraction.entities.len(), 1, "the third attempt answered");

    let failing =
        Model::new(|_| Box::pin(async { Err(EngineError::new(ErrorType::Runtime, "down")) }));
    let Ok(extraction) = extract_file(
        &failing,
        &FileInput {
            path: "a.md",
            text: "text\n",
            content_hash: "h",
            source_toolkit: "repo",
            parser_names: &HashSet::new(),
        },
        quick(),
        &StopSignal::default(),
    )
    .await
    else {
        panic!("a failed chunk is not a failed file");
    };
    assert_eq!(extraction.failed_chunks, 1);
    assert!(extraction.entities.is_empty());
}

#[tokio::test]
async fn a_stop_ends_the_extraction() {
    let stop = StopSignal::default();
    stop.request();
    let model = Model::new(|_| Box::pin(async { Ok("[]".to_owned()) }));
    let outcome = extract_file(
        &model,
        &FileInput {
            path: "a.md",
            text: "x\n",
            content_hash: "h",
            source_toolkit: "repo",
            parser_names: &HashSet::new(),
        },
        quick(),
        &stop,
    )
    .await;
    assert!(outcome.is_err_and(|e| e == EngineError::cancelled()));
}

#[test]
fn references_resolve_strictly_then_by_overlap() {
    let resolver = IdResolver::new([
        ("a1", "Payment Service", "service"),
        ("b2", "refund_policy_document", "document"),
        ("c3", "get", "function"),
    ]);
    assert_eq!(resolver.resolve("a1").as_deref(), Some("a1"));
    assert_eq!(resolver.resolve("payment service").as_deref(), Some("a1"));
    assert_eq!(resolver.resolve("Payment-Service").as_deref(), Some("a1"));
    assert_eq!(
        resolver.resolve("service:payment_service").as_deref(),
        Some("a1")
    );
    assert_eq!(
        resolver.resolve("refund_policy").as_deref(),
        Some("b2"),
        "a long substring"
    );
    assert_eq!(
        resolver.resolve("policy document refund").as_deref(),
        Some("b2"),
        "word overlap"
    );
    assert_eq!(resolver.resolve("payments"), None);
    assert_eq!(resolver.resolve("go"), None);
}

#[tokio::test]
async fn relations_resolve_and_low_confidence_ones_are_dropped() {
    let (model, seen) = scripted(
        |_| "[]".to_owned(),
        |_| "[]".to_owned(),
        |_| {
            r#"[{"source_id": "e1", "relation_type": "documents", "target_id": "Refund Window", "confidence": 0.9},
                {"source_id": "e1", "relation_type": "related_to", "target_id": "e2", "confidence": 0.3},
                {"source_id": "e1", "relation_type": "uses", "target_id": "nowhere", "confidence": 0.9},
                {"source_id": "e2", "relation_type": "owned_by", "target_id": "e1"}]"#
                .to_owned()
        },
    );
    let resolver = IdResolver::new([
        ("e1", "Refunds Guide", "document"),
        ("e2", "Refund Window", "rule"),
    ]);
    let entities = vec![
        (
            "e1".to_owned(),
            "Refunds Guide".to_owned(),
            "document".to_owned(),
        ),
        (
            "e2".to_owned(),
            "Refund Window".to_owned(),
            "rule".to_owned(),
        ),
        ("e3".to_owned(), "get".to_owned(), "function".to_owned()),
    ];
    let Ok(relations) = extract_relations(
        &model,
        "docs/r.md",
        "the text",
        &entities,
        &resolver,
        quick(),
        &StopSignal::default(),
    )
    .await
    else {
        panic!("extracts");
    };
    let found: Vec<(&str, &str, &str)> = relations
        .iter()
        .map(|r| {
            (
                r.source_id.as_deref().unwrap_or_default(),
                r.relation_type.as_str(),
                r.target_id.as_deref().unwrap_or_default(),
            )
        })
        .collect();
    assert_eq!(
        found,
        [("e1", "documents", "e2")],
        "no confidence counts as 0"
    );
    assert_eq!(relations[0].discovered_in_file, "docs/r.md");
    let prompts = seen.lock().map(|p| p.clone()).unwrap_or_default();
    let prompt = prompts.last().cloned().unwrap_or_default();
    assert!(prompt.contains("- e1 -> Refunds Guide (document)"));
    assert!(
        !prompt.contains("e3 -> get"),
        "single-word code names are not offered"
    );
}
