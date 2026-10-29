use serde_json::{Map, Value, json};

use super::*;

fn draft(kind: RunStepKindV1, name: &str) -> StepDraft {
    StepDraft::new(kind, name, 10)
}

fn identity(execution_id: &str, generation: u64) -> ExecutionIdentityV1 {
    command_identity(
        execution_id,
        generation,
        &format!("{execution_id}-{generation}"),
    )
}

#[test]
fn content_is_cut_at_a_utf8_boundary_and_says_so() {
    // 'é' is two bytes: a cut in its middle steps back to its start.
    let text = format!("{}é tail", "a".repeat(MAX_STEP_CONTENT_BYTES - 1));
    let content = step_content(&StepText::plain(text.clone()));
    assert!(content.truncated);
    assert_eq!(content.content.len(), MAX_STEP_CONTENT_BYTES - 1);
    assert!(std::str::from_utf8(&content.content).is_ok());
    assert_eq!(
        content.original_size_bytes,
        i64::try_from(text.len()).unwrap()
    );
    assert_eq!(content.media_type, "text/plain");

    let (kept, cut) = cut_utf8("日本語", 4);
    assert_eq!((kept, cut), ("日", true));
    assert_eq!(cut_utf8("short", 5), ("short", false));

    let whole = step_content(&StepText::json(&json!({"a": 1})));
    assert!(!whole.truncated);
    assert_eq!(whole.media_type, "application/json");
    assert_eq!(whole.content, br#"{"a":1}"#);
    assert_eq!(whole.original_size_bytes, 7);

    // A cut JSON document is no longer JSON: it is stored as text.
    let big = json!({"text": "x".repeat(MAX_STEP_CONTENT_BYTES)});
    let cut = step_content(&StepText::json(&big));
    assert!(cut.truncated);
    assert_eq!(cut.media_type, "text/plain");
}

#[test]
fn the_summary_is_a_json_object_of_at_most_8_kib() {
    let mut small = Map::new();
    small.insert("model".to_owned(), json!("gpt"));
    small.insert("input_tokens".to_owned(), json!(12));
    assert_eq!(
        bounded_summary(&small),
        r#"{"input_tokens":12,"model":"gpt"}"#
    );

    let mut large = Map::new();
    for index in 0..64 {
        large.insert(format!("key-{index:02}"), json!("v".repeat(400)));
    }
    large.insert("huge".to_owned(), json!("h".repeat(20_000)));
    let bounded = bounded_summary(&large);
    assert!(bounded.len() <= MAX_STEP_SUMMARY_BYTES, "{}", bounded.len());
    let parsed: Value = serde_json::from_str(&bounded).expect("still JSON");
    assert_eq!(parsed["summary_truncated"], json!(true));
    assert!(
        parsed.get("key-00").is_some(),
        "keys kept in order while they fit"
    );
}

#[test]
fn a_recorded_step_is_bounded_and_identified() {
    let recorder = RunStepRecorder::new("execution-1");
    let mut step = draft(RunStepKindV1::ToolCall, &"n".repeat(1_000));
    step.toolkit_id = "42".to_owned();
    step.ended_at_unix_ms = 5; // before its start: clamped
    step.outputs = Some(StepText::plain("x".repeat(MAX_STEP_CONTENT_BYTES + 1)));
    let id = recorder.record(step).expect("recorded");
    assert!(uuid::Uuid::parse_str(&id).is_ok());
    let batch = recorder.next_batch();
    assert_eq!(batch.len(), 1);
    let step = &batch[0];
    assert_eq!(step.step_id, id);
    assert_eq!(step.kind, RunStepKindV1::ToolCall as i32);
    assert_eq!(step.name.len(), MAX_NAME_BYTES);
    assert_eq!(step.toolkit_id, "42");
    assert_eq!(step.ended_at_unix_ms, step.started_at_unix_ms);
    assert!(step.outputs.as_ref().expect("outputs").truncated);
    assert_eq!(step.summary_json, "{}");
}

#[test]
fn sixty_five_steps_are_two_batches_in_order() {
    let recorder = RunStepRecorder::new("execution-1");
    let ids: Vec<String> = (0..65)
        .map(|index| {
            recorder
                .record(draft(RunStepKindV1::ModelCall, &format!("m{index}")))
                .expect("recorded")
        })
        .collect();
    assert!(recorder.should_flush_early());
    let first = recorder.next_batch();
    assert_eq!(first.len(), MAX_STEPS_PER_REPORT);
    // Nothing leaves the buffer until Main accepted it.
    assert_eq!(recorder.pending_len(), 65);
    recorder.acknowledge(first.len());
    assert!(!recorder.should_flush_early());
    let second = recorder.next_batch();
    assert_eq!(second.len(), 1);
    recorder.acknowledge(1);
    assert_eq!(recorder.next_batch(), Vec::<RunStepV1>::new());
    let sent: Vec<String> = first
        .iter()
        .chain(&second)
        .map(|step| step.step_id.clone())
        .collect();
    assert_eq!(sent, ids);
}

#[test]
fn a_batch_is_also_cut_by_its_encoded_size() {
    let recorder = RunStepRecorder::new("execution-1");
    for _ in 0..5 {
        let mut step = draft(RunStepKindV1::ToolCall, "read_file");
        step.inputs = Some(StepText::plain("i".repeat(MAX_STEP_CONTENT_BYTES)));
        step.outputs = Some(StepText::plain("o".repeat(MAX_STEP_CONTENT_BYTES)));
        recorder.record(step);
    }
    let batch = recorder.next_batch();
    // Two 512 KiB steps exceed 1 MiB with the request envelope: one each.
    assert_eq!(batch.len(), 1);
    let encoded: usize = batch.iter().map(prost::Message::encoded_len).sum();
    assert!(encoded < MAX_REPORT_BYTES);
}

#[test]
fn an_output_cites_the_retrievals_whose_documents_it_names() {
    let recorder = RunStepRecorder::new("execution-1");
    let mut first = draft(RunStepKindV1::Retrieval, "search_index");
    first.retrieval = Some(RetrievalDraft {
        documents: vec![
            RetrievedDocument {
                id: "doc-alpha".to_owned(),
                source: "wiki/alpha.md".to_owned(),
                score: 0.9,
            },
            RetrievedDocument {
                id: "1".to_owned(), // too short to count as a citation
                source: String::new(),
                score: 0.5,
            },
        ],
        ..RetrievalDraft::default()
    });
    let first = recorder.record(first).expect("recorded");
    let mut second = draft(RunStepKindV1::Retrieval, "search_attachment");
    second.retrieval = Some(RetrievalDraft {
        documents: vec![RetrievedDocument {
            id: "att-7".to_owned(),
            source: "report.pdf".to_owned(),
            score: 2.0,
        }],
        ..RetrievalDraft::default()
    });
    let _second = recorder.record(second).expect("recorded");
    let mut third = draft(RunStepKindV1::Retrieval, "search_index");
    third.retrieval = Some(RetrievalDraft {
        documents: vec![RetrievedDocument {
            id: "doc-gamma".to_owned(),
            source: "wiki/gamma.md".to_owned(),
            score: 0.7,
        }],
        ..RetrievalDraft::default()
    });
    let third = recorder.record(third).expect("recorded");
    let mut output = draft(RunStepKindV1::Output, "output");
    output.outputs = Some(StepText::plain(
        "See wiki/gamma.md and doc-alpha (section 1).",
    ));
    recorder.record(output);
    let batch = recorder.next_batch();
    let output = batch.last().expect("output");
    assert_eq!(output.cited_step_ids, [first, third]);
    assert_eq!(output.cited_document_ids, ["doc-alpha", "doc-gamma"]);
    // Only output steps cite.
    assert_eq!(batch[0].cited_step_ids, Vec::<String>::new());
}

#[test]
fn retrievals_are_read_from_the_index_and_attachment_tools() {
    let search = retrieval_from_tool(
        "jira___search_index",
        &json!({"query": "login bug", "index_name": "tickets", "filter": "{\"status\": \"open\"}", "search_top": 3}),
        &json!([
            {"page_content": "a", "metadata": {"id": "T-1", "source": "jira/T-1"}, "score": 0.8},
            {"page_content": "b", "metadata": {"chunk_id": 4, "filename": "notes.txt"}, "score": 0.4},
            {"page_content": "c", "metadata": {}, "score": null},
        ]),
    )
    .expect("a retrieval");
    assert_eq!(search.index_name, "tickets");
    assert_eq!(search.query, "login bug");
    assert_eq!(search.top_k, 3);
    assert_eq!(search.filters, Some(json!({"status": "open"})));
    assert_eq!(
        search.documents,
        [
            RetrievedDocument {
                id: "T-1".to_owned(),
                source: "jira/T-1".to_owned(),
                score: 0.8
            },
            RetrievedDocument {
                id: "4".to_owned(),
                source: "notes.txt".to_owned(),
                score: 0.4
            },
            RetrievedDocument {
                id: "hit-3".to_owned(),
                source: String::new(),
                score: 0.0
            },
        ]
    );
    let summary = retrieval_summary(&search);
    assert_eq!(summary.filters_json, r#"{"status":"open"}"#);
    assert_eq!(
        summary.documents.iter().map(|d| d.rank).collect::<Vec<_>>(),
        [1, 2, 3]
    );

    let stepback = retrieval_from_tool(
        "stepback_search_index",
        &json!({"query": "q"}),
        &json!("Found 1 documents matching the query\n[{\"metadata\": {\"id\": \"x-1\"}, \"score\": 1.0}]"),
    )
    .expect("a retrieval");
    assert_eq!(stepback.documents[0].id, "x-1");
    assert_eq!(stepback.top_k, 10);

    let none = retrieval_from_tool(
        "search_index",
        &json!({"query": "q"}),
        &json!("No documents found by query 'q'"),
    )
    .expect("a retrieval with no documents");
    assert_eq!(none.documents, Vec::<RetrievedDocument>::new());

    let attachment = retrieval_from_tool(
        "search_attachment",
        &json!({"query": "revenue", "top_k": 2}),
        &json!({"query": "revenue", "results": [{"attachment_id": "a-1", "name": "q3.pdf", "score": 3.2}]}),
    )
    .expect("a retrieval");
    assert_eq!(attachment.index_name, "attachments");
    assert_eq!(attachment.documents[0].id, "a-1");
    assert_eq!(attachment.documents[0].source, "q3.pdf");

    assert!(retrieval_from_tool("read_file", &json!({}), &json!("text")).is_none());
}

#[test]
fn at_most_100_documents_are_reported() {
    let draft = RetrievalDraft {
        documents: (0..150)
            .map(|index| RetrievedDocument {
                id: format!("d{index}"),
                source: String::new(),
                score: f64::NAN,
            })
            .collect(),
        ..RetrievalDraft::default()
    };
    let summary = retrieval_summary(&draft);
    assert_eq!(summary.documents.len(), MAX_RETRIEVED_DOCUMENTS);
    assert_eq!(summary.documents[99].rank, 100);
    assert!(summary.documents.iter().all(|d| d.score == 0.0));
}

#[test]
fn nothing_is_recorded_while_suspended() {
    let recorder = RunStepRecorder::new("execution-1");
    {
        let _replay = recorder.suspend();
        assert!(
            recorder
                .record(draft(RunStepKindV1::ToolCall, "t"))
                .is_none()
        );
    }
    assert!(
        recorder
            .record(draft(RunStepKindV1::ToolCall, "t"))
            .is_some()
    );
    assert_eq!(recorder.pending_len(), 1);
}

#[test]
fn dropped_steps_are_counted() {
    let recorder = RunStepRecorder::new("execution-1");
    recorder.record(draft(RunStepKindV1::ToolCall, "t"));
    recorder.record(draft(RunStepKindV1::ToolCall, "t"));
    let before = dropped_run_steps_total();
    assert_eq!(recorder.drop_pending("test"), 2);
    assert!(dropped_run_steps_total() >= before + 2);
    assert_eq!(recorder.pending_len(), 0);
}

#[test]
fn one_recorder_per_command_and_a_newer_generation_replaces_older_ones() {
    let registry = RunStepRegistry::default();
    let first = registry.recorder(&identity("e", 1));
    assert!(Arc::ptr_eq(&first, &registry.recorder(&identity("e", 1))));
    assert!(registry.find(&identity("e", 2)).is_none());
    registry.bind_claim(
        &identity("e", 2),
        &ExecutionFenceV1 {
            lease_epoch: 3,
            ..ExecutionFenceV1::default()
        },
    );
    assert!(
        registry.find(&identity("e", 1)).is_none(),
        "the older generation's claim is gone"
    );
    let second = registry.find(&identity("e", 2)).expect("tracked");
    let (bound, fence) = second.claim().expect("bound");
    assert_eq!(bound, identity("e", 2));
    assert_eq!(fence.lease_epoch, 3);
    registry.forget(&identity("e", 2));
    assert!(registry.find(&identity("e", 2)).is_none());
}

#[test]
fn timestamps_parse_as_unix_milliseconds() {
    assert_eq!(rfc3339_unix_ms("1970-01-01T00:00:01.5Z"), 1_500);
    assert_eq!(rfc3339_unix_ms("not a time"), 0);
}

#[test]
fn an_aged_step_flushes_early_unless_a_failed_report_is_backing_off() {
    let recorder = RunStepRecorder::new("execution-1");
    assert!(!recorder.should_flush_early(), "nothing buffered");
    recorder.record(draft(RunStepKindV1::ToolCall, "tool"));
    assert!(!recorder.should_flush_early(), "one young step waits");
    recorder.age_pending(MAX_UNSENT_STEP_AGE);
    assert!(recorder.should_flush_early(), "an aged step goes out");

    recorder.note_early_failure();
    assert!(
        !recorder.should_flush_early(),
        "the backoff window holds it"
    );
    assert_eq!(recorder.pending_len(), 1, "and keeps it buffered");
    recorder.expire_early_backoff();
    assert!(recorder.should_flush_early());

    recorder.note_early_failure();
    recorder.acknowledge(1);
    recorder.record(draft(RunStepKindV1::ToolCall, "next"));
    recorder.age_pending(MAX_UNSENT_STEP_AGE);
    assert!(
        recorder.should_flush_early(),
        "a delivered report clears the backoff"
    );
    recorder.drop_pending("test");
    assert!(!recorder.should_flush_early(), "dropped steps leave no age");
}

/// Main's bounds, written out independently of the worker's constants:
/// services/elitea-main/internal/application/runevidence/service.go
/// (`normalize`, `retrievalJSON`, `normalizeContent`). Main refuses the
/// WHOLE report when one step exceeds any of them.
mod main_bounds {
    pub(super) const NODE_ID: usize = 256;
    pub(super) const NAME: usize = 512;
    pub(super) const TOOLKIT_ID: usize = 128;
    pub(super) const SUMMARY: usize = 8 * 1024;
    pub(super) const CITED_IDS: usize = 100;
    pub(super) const ID: usize = 256;
    pub(super) const QUERY: usize = 8 * 1024;
    pub(super) const FILTERS: usize = 8 * 1024;
    pub(super) const DOCUMENTS: usize = 100;
    pub(super) const SOURCE: usize = 2048;
    pub(super) const CONTENT: usize = 256 * 1024;
}

fn assert_within_main_bounds(step: &RunStepV1) {
    use main_bounds as max;
    assert!(uuid::Uuid::parse_str(&step.step_id).is_ok());
    assert!(step.started_at_unix_ms > 0 && step.ended_at_unix_ms >= step.started_at_unix_ms);
    assert!(step.node_id.len() <= max::NODE_ID, "node_id");
    assert!(step.name.len() <= max::NAME, "name");
    assert!(step.toolkit_id.len() <= max::TOOLKIT_ID, "toolkit_id");
    assert!(step.summary_json.len() <= max::SUMMARY, "summary");
    assert!(
        serde_json::from_str::<Value>(&step.summary_json).is_ok_and(|value| value.is_object()),
        "summary is an object"
    );
    assert!(step.cited_step_ids.len() <= max::CITED_IDS, "cited steps");
    assert!(
        step.cited_document_ids.len() <= max::CITED_IDS,
        "cited documents"
    );
    for id in step.cited_step_ids.iter().chain(&step.cited_document_ids) {
        assert!(id.len() <= max::ID, "cited id");
    }
    for side in [&step.inputs, &step.outputs].into_iter().flatten() {
        assert!(side.content.len() <= max::CONTENT, "content");
        assert!(["application/json", "text/plain"].contains(&side.media_type.as_str()));
    }
    if let Some(retrieval) = &step.retrieval {
        assert!(retrieval.index_name.len() <= max::NAME, "index name");
        assert!(retrieval.query.len() <= max::QUERY, "query");
        assert!(retrieval.filters_json.len() <= max::FILTERS, "filters");
        assert!(
            retrieval.filters_json.is_empty()
                || serde_json::from_str::<Value>(&retrieval.filters_json)
                    .is_ok_and(|value| value.is_object()),
            "filters are an object"
        );
        assert!(retrieval.documents.len() <= max::DOCUMENTS, "documents");
        for document in &retrieval.documents {
            assert!(document.document_id.len() <= max::ID, "document id");
            assert!(document.source.len() <= max::SOURCE, "source");
        }
    }
    assert!(
        step.encoded_len() <= MAX_REPORT_BYTES,
        "one step fits a report"
    );
}

/// Every field at an over-limit value (multi-byte text, so cuts must land on
/// UTF-8 boundaries) still yields steps Main accepts.
#[test]
fn a_step_with_every_field_over_the_limit_fits_mains_bounds() {
    let recorder = RunStepRecorder::new("execution-1");
    let long = |bytes: usize| "é".repeat(bytes / 2 + 7);
    let documents: Vec<RetrievedDocument> = (0..150)
        .map(|index| RetrievedDocument {
            id: format!("{index:04}-{}", long(400)),
            source: long(3_000),
            score: f64::NAN,
        })
        .collect();
    let mut retrieval = draft(RunStepKindV1::Retrieval, &long(1_000));
    retrieval.node_id = long(1_000);
    retrieval.toolkit_id = long(1_000);
    retrieval.started_at_unix_ms = 0;
    retrieval.ended_at_unix_ms = -1;
    retrieval.inputs = Some(StepText::json(&json!({"q": long(400_000)})));
    retrieval.outputs = Some(StepText::plain(long(400_000)));
    for index in 0..40 {
        retrieval = retrieval.summary(&format!("key-{index}-{}", long(300)), json!(long(1_000)));
    }
    retrieval.retrieval = Some(RetrievalDraft {
        index_name: long(1_000),
        query: long(20_000),
        filters: Some(json!({"f": long(20_000)})),
        top_k: u32::MAX,
        documents: documents.clone(),
    });
    recorder.record(retrieval);
    // 120 more retrievals: the output below cites more than 100 of each.
    for index in 0..120 {
        let mut extra = draft(RunStepKindV1::Retrieval, "search_index");
        extra.retrieval = Some(RetrievalDraft {
            index_name: "index".to_owned(),
            query: String::new(),
            filters: None,
            top_k: 1,
            documents: vec![RetrievedDocument {
                id: format!("extra-{index:04}-{}", long(400)),
                source: String::new(),
                score: 1.0,
            }],
        });
        recorder.record(extra);
    }
    let everything: Vec<String> = documents
        .iter()
        .map(|document| document.id.clone())
        .chain((0..120).map(|index| format!("extra-{index:04}-{}", long(400))))
        .collect();
    let mut output = draft(RunStepKindV1::Output, "output");
    output.outputs = Some(StepText::plain(everything.join(" ")));
    recorder.record(output);
    let mut steps = Vec::new();
    loop {
        let batch = recorder.next_batch();
        if batch.is_empty() {
            break;
        }
        recorder.acknowledge(batch.len());
        steps.extend(batch);
    }
    assert_eq!(steps.len(), 122);
    let output = steps.last().expect("output");
    assert_eq!(output.cited_step_ids.len(), 100, "cited steps are capped");
    assert_eq!(
        output.cited_document_ids.len(),
        100,
        "cited documents are capped"
    );
    for step in &steps {
        assert_within_main_bounds(step);
    }
}

/// Main refuses a step whose start or end is unset (<= 0): a missing or
/// unparseable timestamp becomes the wall clock, and the end never precedes
/// the start.
#[test]
fn a_missing_timestamp_becomes_the_wall_clock() {
    let before = now_unix_ms();
    let recorder = RunStepRecorder::new("execution-1");
    for (started, ended) in [
        (0, 0),
        (-5, 0),
        (rfc3339_unix_ms("not a time"), 0),
        (0, 77),
        (88, 0),
    ] {
        let mut step = draft(RunStepKindV1::ModelCall, "model");
        step.started_at_unix_ms = started;
        step.ended_at_unix_ms = ended;
        recorder.record(step);
    }
    let batch = recorder.next_batch();
    for step in &batch {
        assert!(step.started_at_unix_ms > 0);
        assert!(step.ended_at_unix_ms >= step.started_at_unix_ms);
    }
    assert!(batch[0].started_at_unix_ms >= before);
    assert_eq!(batch[3].started_at_unix_ms, 77, "a known end stands in");
    assert_eq!(batch[3].ended_at_unix_ms, 77);
    assert_eq!(batch[4].started_at_unix_ms, 88);
    assert!(batch[4].ended_at_unix_ms >= before);
}

/// Binding a claim creates a recorder but records nothing: only a recorded
/// step says an earlier attempt ran in this process.
#[test]
fn binding_a_claim_is_not_having_recorded() {
    let registry = RunStepRegistry::default();
    let identity = identity("execution-bound", 1);
    registry.bind_claim(&identity, &ExecutionFenceV1::default());
    let recorder = registry.find(&identity).expect("bound");
    assert!(!recorder.has_recorded());
    {
        let _replay = recorder.suspend();
        recorder.record(draft(RunStepKindV1::ToolCall, "replayed"));
    }
    assert!(
        !recorder.has_recorded(),
        "a suppressed replay is not a step"
    );
    recorder.record(draft(RunStepKindV1::ToolCall, "tool"));
    assert!(recorder.has_recorded());
}
