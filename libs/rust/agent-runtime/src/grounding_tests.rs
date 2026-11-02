use serde_json::json;

use super::*;

fn slice_utf16(text: &str, span: Span) -> String {
    let units: Vec<u16> = text.encode_utf16().collect();
    String::from_utf16(&units[span.start..span.end]).expect("valid UTF-16 slice")
}

#[test]
fn the_setting_reads_off_required_and_null() {
    assert_eq!(CitationMode::from_value(None), Ok(CitationMode::Off));
    assert_eq!(
        CitationMode::from_value(Some(&Value::Null)),
        Ok(CitationMode::Off)
    );
    assert_eq!(
        CitationMode::from_value(Some(&json!("off"))),
        Ok(CitationMode::Off)
    );
    assert_eq!(
        CitationMode::from_value(Some(&json!("required"))),
        Ok(CitationMode::Required)
    );
    for bad in [json!("Required"), json!(true), json!(1), json!({})] {
        assert_eq!(
            CitationMode::from_value(Some(&bad)),
            Err(InvalidCitationMode)
        );
    }
}

#[test]
fn the_rule_follows_the_instructions_and_names_the_envelope() {
    assert_eq!(with_citation_rule("  "), CITATION_RULE);
    assert_eq!(
        with_citation_rule("Be brief."),
        format!("Be brief.\n\n{CITATION_RULE}")
    );
    assert!(CITATION_RULE.contains(&format!("<{}>", crate::untrusted::ENVELOPE_TAG)));
}

#[test]
fn a_numbered_source_keeps_its_number_in_the_envelope() {
    assert_eq!(numbered_source(3, "search_index"), "3:search_index");
    let long = "é".repeat(200);
    let numbered = numbered_source(12, &long);
    assert!(numbered.starts_with("12:é"));
    assert!(numbered.len() <= 3 + MAX_NUMBERED_NAME_BYTES);
    let wrapped = crate::untrusted::envelope(&numbered_source(7, "jira_get_issue"), "{}");
    assert!(wrapped.starts_with("<untrusted source=\"7:jira_get_issue\">"));
}

#[test]
fn no_source_means_no_indicator() {
    assert_eq!(indicator("The sky is blue.", 0), None);
}

#[test]
fn every_sentence_cited_counts_distinct_sources() {
    let answer =
        "The service runs on port 8080 [1]. It uses Postgres [2, 1]. Logs go to stdout. [3]";
    let result = indicator(answer, 4).expect("indicator");
    assert_eq!(result.sources_available, 4);
    assert_eq!(result.sources_cited, 3);
    assert_eq!(result.claim_spans, 3);
    assert_eq!(result.uncited_spans, 0);
    assert_eq!(result.uncited, Vec::<Span>::new());
    assert_eq!(result.invalid_markers, 0);
}

#[test]
fn uncited_sentences_are_reported_with_offsets() {
    let answer = "Paris is the capital [1]. It has 2 million people. The river is the Seine!";
    let result = indicator(answer, 1).expect("indicator");
    assert_eq!(result.claim_spans, 3);
    assert_eq!(result.uncited_spans, 2);
    assert_eq!(result.sources_cited, 1);
    let texts: Vec<String> = result
        .uncited
        .iter()
        .map(|span| slice_utf16(answer, *span))
        .collect();
    assert_eq!(
        texts,
        ["It has 2 million people.", "The river is the Seine!"]
    );
}

#[test]
fn headings_questions_lead_ins_code_and_tables_are_not_claims() {
    let answer = "# Summary\n\
        Here is what I found:\n\
        - The build is green [1].\n\
        2. The deploy failed.\n\
        Do you want details?\n\
        ```\n\
        let x = 1. Not a claim.\n\
        ```\n\
        | col | col |\n\
        |-----|-----|\n\
        ---\n";
    let result = indicator(answer, 2).expect("indicator");
    assert_eq!(result.claim_spans, 2, "{result:?}");
    assert_eq!(result.uncited_spans, 1);
    assert_eq!(slice_utf16(answer, result.uncited[0]), "The deploy failed.");
}

#[test]
fn invalid_and_link_markers_do_not_cite() {
    let answer =
        "Alpha is true [0]. Beta is true [9]. Gamma is [here](https://x.test) [1](https://y.test).";
    let result = indicator(answer, 2).expect("indicator");
    assert_eq!(result.invalid_markers, 2);
    assert_eq!(result.sources_cited, 0);
    assert_eq!(result.claim_spans, 3);
    assert_eq!(result.uncited_spans, 3);
}

#[test]
fn unicode_offsets_are_utf16_code_units() {
    let answer = "Le café est ouvert 🎉 [1]. 東京は大きい。日本の首都です[1]。Ça coûte 5 €.";
    let result = indicator(answer, 1).expect("indicator");
    assert_eq!(result.claim_spans, 4, "{result:?}");
    assert_eq!(result.uncited_spans, 2);
    let texts: Vec<String> = result
        .uncited
        .iter()
        .map(|span| slice_utf16(answer, *span))
        .collect();
    assert_eq!(texts, ["東京は大きい。", "Ça coûte 5 €."]);
}

#[test]
fn decimals_and_markers_inside_a_sentence_do_not_split_it() {
    let answer = "Version 3.5 adds caching [2] and drops Python 3.9 support.";
    let result = indicator(answer, 2).expect("indicator");
    assert_eq!(result.claim_spans, 1);
    assert_eq!(result.uncited_spans, 0);
    assert_eq!(result.sources_cited, 1);
}

#[test]
fn the_span_list_is_bounded_but_the_count_is_not() {
    let answer = "A claim here.\n".repeat(MAX_UNCITED_SPANS + 10);
    let result = indicator(&answer, 1).expect("indicator");
    assert_eq!(result.uncited_spans, MAX_UNCITED_SPANS + 10);
    assert_eq!(result.uncited.len(), MAX_UNCITED_SPANS);
}

#[test]
fn the_json_form_has_no_score_and_no_text() {
    let value = indicator("Fact one [1]. Fact two.", 3)
        .expect("indicator")
        .to_value();
    assert_eq!(
        value,
        json!({
            "version": 1,
            "sources_available": 3,
            "sources_cited": 1,
            "claim_spans": 2,
            "uncited_spans": 1,
            "uncited": [{"start": 14, "end": 23}],
            "invalid_markers": 0,
        })
    );
}
