use serde_yaml_ng::Value;

use super::{BoundedYamlError, YamlBudget, YamlBudgetLimit, from_str, from_str_as_yaml_error};

const BUDGET: YamlBudget = YamlBudget {
    nodes: 100,
    scalar_bytes: 1_000,
    depth: 8,
};

fn limit(yaml: &str, budget: YamlBudget) -> Option<YamlBudgetLimit> {
    match from_str::<Value>(yaml, budget) {
        Ok(_) => None,
        Err(BoundedYamlError::BudgetExceeded(limit)) => Some(limit),
        Err(BoundedYamlError::Malformed(error)) => panic!("unexpected malformed YAML: {error}"),
    }
}

/// A flow sequence of `items` one-byte scalars: `items + 1` nodes and `items` scalar bytes.
fn sequence(items: usize) -> String {
    format!("[{}]", "v,".repeat(items))
}

#[test]
fn node_budget_holds_at_limit_and_refuses_limit_plus_one() {
    assert_eq!(limit(&sequence(BUDGET.nodes - 1), BUDGET), None);
    assert_eq!(
        limit(&sequence(BUDGET.nodes), BUDGET),
        Some(YamlBudgetLimit::Nodes)
    );
}

#[test]
fn scalar_byte_budget_holds_at_limit_and_refuses_limit_plus_one() {
    let at = "a".repeat(BUDGET.scalar_bytes);
    assert_eq!(limit(&at, BUDGET), None);
    let over = "a".repeat(BUDGET.scalar_bytes + 1);
    assert_eq!(limit(&over, BUDGET), Some(YamlBudgetLimit::ScalarBytes));
}

#[test]
fn depth_budget_holds_at_limit_and_refuses_limit_plus_one() {
    let nested = |depth: usize| format!("{}{}", "[".repeat(depth), "]".repeat(depth));
    assert_eq!(limit(&nested(BUDGET.depth), BUDGET), None);
    assert_eq!(
        limit(&nested(BUDGET.depth + 1), BUDGET),
        Some(YamlBudgetLimit::Depth)
    );
}

#[test]
fn every_reference_counts_its_expanded_nodes_and_bytes() {
    // Anchored sequence: 1 + 9 nodes. Each reference replays all 10, plus the outer sequence.
    let yaml = |references: usize| {
        format!(
            "[&s {}, {}]",
            sequence(9),
            vec!["*s"; references].join(", ")
        )
    };
    assert_eq!(limit(&yaml(8), BUDGET), None);
    assert_eq!(limit(&yaml(9), BUDGET), Some(YamlBudgetLimit::Nodes));

    let scalar = "b".repeat(300);
    assert_eq!(limit(&format!("[&s {scalar}, *s, *s]"), BUDGET), None);
    assert_eq!(
        limit(&format!("[&s {scalar}, *s, *s, *s]"), BUDGET),
        Some(YamlBudgetLimit::ScalarBytes)
    );
}

#[test]
fn mapping_keys_and_tags_are_counted() {
    // Mapping (1) + two keys and two values (4).
    let budget = YamlBudget { nodes: 5, ..BUDGET };
    assert_eq!(limit("{a: 1, b: 2}", budget), None);
    assert_eq!(
        limit("{a: 1, b: 2, c: 3}", budget),
        Some(YamlBudgetLimit::Nodes)
    );
    // Tag (1) + tag text (1) + content (1).
    let budget = YamlBudget { nodes: 3, ..BUDGET };
    assert_eq!(limit("!custom value", budget), None);
    assert_eq!(
        limit("[!custom value]", budget),
        Some(YamlBudgetLimit::Nodes)
    );
}

#[test]
fn documents_within_budget_match_the_unbounded_parser() {
    for yaml in [
        "",
        "plain",
        "{a: [1, 2.5, true, null, ~], b: {c: 'quoted'}}",
        "base: &base {x: 1}\nderived: *base\n",
        "!custom {a: 1}",
        "200: ok\n'201': created\n",
    ] {
        let bounded = from_str::<Value>(yaml, BUDGET).expect("bounded parse");
        #[expect(
            clippy::disallowed_methods,
            reason = "reference parser for the comparison"
        )]
        let unbounded = serde_yaml_ng::from_str::<Value>(yaml).expect("unbounded parse");
        assert_eq!(bounded, unbounded, "{yaml:?}");
        let json = from_str::<serde_json::Value>(yaml, BUDGET);
        #[expect(
            clippy::disallowed_methods,
            reason = "reference parser for the comparison"
        )]
        let unbounded_json = serde_yaml_ng::from_str::<serde_json::Value>(yaml);
        assert_eq!(json.ok(), unbounded_json.ok(), "{yaml:?}");
    }
}

#[test]
fn malformed_documents_are_not_reported_as_budget_failures() {
    for yaml in [
        "[unclosed",
        "a: 1\n---\nb: 2\n",
        "*undefined",
        "{a: 1, a: 2}",
    ] {
        assert!(
            matches!(
                from_str::<Value>(yaml, BUDGET),
                Err(BoundedYamlError::Malformed(_))
            ),
            "{yaml:?}"
        );
    }
}

#[test]
fn yaml_error_variant_reports_budget_failures_as_malformed_documents() {
    let error =
        from_str_as_yaml_error::<Value>(&sequence(BUDGET.nodes), BUDGET).expect_err("over budget");
    assert!(error.to_string().contains("expansion budget"));
}
