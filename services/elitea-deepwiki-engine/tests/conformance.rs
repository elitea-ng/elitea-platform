//! The engine against the committed provider fixtures it shares with the
//! Python engine and the Go host (`conformance/provider/fixtures/deepwiki`).
//! Reading them — not copies of them — is what keeps three implementations
//! of one contract from drifting apart.

use elitea_deepwiki_engine::errors::{ErrorType, classify};
use elitea_deepwiki_engine::runner::fixture::answer_fragments;
use serde_json::Value;
use std::path::PathBuf;

fn fixture(relative: &str) -> Value {
    let path = PathBuf::from(env!("CARGO_MANIFEST_DIR"))
        .join("../../conformance/provider/fixtures/deepwiki")
        .join(relative);
    let text = std::fs::read_to_string(&path).unwrap_or_else(|e| panic!("{}: {e}", path.display()));
    serde_json::from_str(&text).unwrap_or_else(|e| panic!("{}: {e}", path.display()))
}

fn error_type(name: &str) -> ErrorType {
    match name {
        "FileNotFoundError" => ErrorType::FileNotFound,
        "ValueError" => ErrorType::Value,
        "MemoryError" => ErrorType::Memory,
        "KeyError" => ErrorType::Key,
        "RuntimeError" => ErrorType::Runtime,
        "TypeError" => ErrorType::Type,
        _ => ErrorType::Generic,
    }
}

#[test]
fn every_recorded_error_classifies_as_recorded() {
    let errors = fixture("spi/errors.json");
    let recorded = errors["recorded"].as_object().expect("recorded");
    assert!(recorded.len() >= 8, "the fixture lost cases");
    for (case, body) in recorded {
        let objects: Value =
            serde_json::from_str(body["result"].as_str().expect("result")).expect("objects");
        let data = objects[0]["data"].as_str().expect("data");
        // "<Operation> failed for model '<m>': <exception text>"
        let message = data.split_once("': ").map_or(data, |(_, rest)| rest);
        let kind = error_type(body["error_type"].as_str().expect("error_type"));
        assert_eq!(
            classify(kind, message),
            body["error_category"].as_str().expect("category"),
            "{case}: {message}"
        );
    }
}

#[test]
fn the_engine_hop_of_the_golden_token_fixture_is_the_shape_this_engine_writes() {
    let golden = fixture("stream/token_events.json");
    let hop = &golden["hops"]["engine_socket"];
    assert_eq!(
        hop["progress_line"]
            .as_object()
            .map(|o| o.keys().collect::<Vec<_>>()),
        Some(vec![&"thinking".to_owned()])
    );
    assert_eq!(
        hop["token_line"]
            .as_object()
            .map(|o| o.keys().collect::<Vec<_>>()),
        Some(vec![&"token".to_owned()])
    );
    let lines = golden["golden_sequence"]["engine_lines"]
        .as_array()
        .expect("lines");
    let streamed: String = lines.iter().filter_map(|l| l["token"].as_str()).collect();
    assert_eq!(
        streamed,
        golden["golden_sequence"]["streaming_text"]
            .as_str()
            .expect("text")
    );
    // The fixture's cutter keeps the rule the golden sequence states:
    // fragments join back in order with no separator.
    assert_eq!(answer_fragments(&streamed, 3).concat(), streamed);
}
