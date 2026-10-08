use adk_rust::graph::GraphError;
use ring::digest;
use serde_json::{Map, Number, Value, json};

use super::data_shaping::{
    Budget, FieldSelection, MissingPolicy, NullPolicy, Pointer, RawLimits, RawRetain, RetainMode,
    RetainSpec, Selected, ShapingCode, ShapingConfigurationError, ShapingError, ShapingLimitKind,
    ShapingLimits, canonical_key, copy_digest, decimal, envelope_row, exact_i64, parse_envelope,
    parse_node_yaml, valid_name,
};

fn number(text: &str) -> Number {
    match serde_json::from_str::<Value>(text).expect("number literal") {
        Value::Number(number) => number,
        other => panic!("not a number: {other}"),
    }
}

fn value(text: &str) -> Value {
    serde_json::from_str(text).expect("json literal")
}

fn pointer(text: &str) -> Pointer {
    Pointer::parse(text).expect("pointer")
}

fn retain(yaml: &str) -> Result<RetainSpec, ShapingConfigurationError> {
    let raw = serde_yaml_ng::from_str::<RawRetain>(yaml).expect("raw retain");
    RetainSpec::from_raw(raw)
}

fn limits(yaml: &str, allow_groups: bool) -> Result<ShapingLimits, ShapingConfigurationError> {
    let raw = serde_yaml_ng::from_str::<RawLimits>(yaml).expect("raw limits");
    ShapingLimits::from_raw(Some(&raw), allow_groups)
}

fn object(value: Value) -> Map<String, Value> {
    match value {
        Value::Object(map) => map,
        other => panic!("not an object: {other}"),
    }
}

// ---------------------------------------------------------------- pointers

#[test]
fn pointer_parsing_follows_rfc_6901_and_the_bounds() {
    let parsed = pointer("/a~01b/c~1d//");
    assert_eq!(parsed.as_str(), "/a~01b/c~1d//");
    assert_eq!(parsed.first_token(), "a~1b");
    assert_eq!(parsed.token_count(), 4);
    assert_eq!(pointer("/").token_count(), 1);
    assert_eq!(pointer("/").first_token(), "");

    let longest = format!("/{}", "a".repeat(511));
    assert!(Pointer::parse(&longest).is_ok());
    let too_long = format!("/{}", "a".repeat(512));
    let most_tokens = "/a".repeat(32);
    let too_many_tokens = "/a".repeat(33);
    for refused in [
        "",
        "a",
        "a/b",
        "/~2",
        "/~",
        "/a~",
        "/~a",
        too_long.as_str(),
        too_many_tokens.as_str(),
    ] {
        assert!(
            matches!(
                Pointer::parse(refused),
                Err(ShapingConfigurationError::Invalid(_))
            ),
            "{refused:?} must be refused"
        );
    }
    assert!(Pointer::parse(&most_tokens).is_ok());
}

#[test]
fn pointer_resolution_is_exact_and_missing_is_distinct_from_null() {
    let document =
        value(r#"{"a":{"b":[10,20,{"c":null}]},"":{"":1},"n":null,"s":"x","~/":2,"00":3}"#);
    let cases: &[(&str, Option<Value>)] = &[
        ("/a/b/0", Some(json!(10))),
        ("/a/b/1", Some(json!(20))),
        ("/a/b/2/c", Some(Value::Null)),
        ("/n", Some(Value::Null)),
        ("/~0~1", Some(json!(2))),
        ("//", Some(json!(1))),
        ("/00", Some(json!(3))),
        ("/a/b/00", None),
        ("/a/b/01", None),
        ("/a/b/-", None),
        ("/a/b/3", None),
        ("/a/b/+1", None),
        ("/a/b/18446744073709551616", None),
        ("/n/x", None),
        ("/s/0", None),
        ("/a/b/0/x", None),
        ("/missing", None),
    ];
    for (text, expected) in cases {
        assert_eq!(
            pointer(text).resolve(&document),
            expected.as_ref(),
            "{text}"
        );
    }
}

#[test]
fn pointer_removal_targets_object_keys_and_array_slots() {
    let mut document = value(r#"{"a":{"b":[1,2,3],"c":4}}"#);
    assert!(pointer("/a/c").remove_from(&mut document));
    assert_eq!(document, value(r#"{"a":{"b":[1,2,3]}}"#));
    assert!(pointer("/a/b/1").remove_from(&mut document));
    assert_eq!(document, value(r#"{"a":{"b":[1,3]}}"#));
    assert!(!pointer("/a/b/5").remove_from(&mut document));
    assert!(!pointer("/a/b/-").remove_from(&mut document));
    assert!(!pointer("/x/y").remove_from(&mut document));
    assert!(pointer("/a/b").remove_from(&mut document));
    assert_eq!(document, value(r#"{"a":{}}"#));
    let mut scalar = json!(1);
    assert!(!pointer("/a").remove_from(&mut scalar));
}

#[test]
fn names_are_bounded_literal_keys_without_control_characters() {
    assert!(valid_name("line"));
    assert!(valid_name("a/b ~ c"));
    assert!(valid_name(&"n".repeat(256)));
    for refused in ["", "a\nb", "a\u{0}", "tab\t", "del\u{7f}", "c1\u{85}"] {
        assert!(!valid_name(refused), "{refused:?}");
    }
    assert!(!valid_name(&"n".repeat(257)));
}

// ---------------------------------------------------------------- selection

#[test]
fn field_selection_applies_missing_then_null_policy() {
    let row = value(r#"{"present":1,"null":null}"#);
    let present = json!(1);
    let missing_policies = [
        MissingPolicy::Error,
        MissingPolicy::Null,
        MissingPolicy::Skip,
    ];
    let null_policies = [NullPolicy::Keep, NullPolicy::Error, NullPolicy::Skip];
    for missing in missing_policies {
        for null in null_policies {
            let select = |path: &str| {
                FieldSelection {
                    pointer: pointer(path),
                    missing,
                    null,
                }
                .select(&row)
            };
            assert_eq!(select("/present"), Ok(Selected::Value(&present)));
            let on_null = match null {
                NullPolicy::Keep => Ok(Selected::Null),
                NullPolicy::Error => Err(ShapingCode::NullValue),
                NullPolicy::Skip => Ok(Selected::Skip),
            };
            assert_eq!(select("/null"), on_null, "{missing:?} {null:?}");
            let on_missing = match missing {
                MissingPolicy::Error => Err(ShapingCode::MissingField),
                MissingPolicy::Skip => Ok(Selected::Skip),
                MissingPolicy::Null => on_null,
            };
            assert_eq!(select("/absent"), on_missing, "{missing:?} {null:?}");
        }
    }
}

#[test]
fn policies_deserialize_in_snake_case_and_have_distinct_tags() {
    let missing: Vec<MissingPolicy> =
        serde_yaml_ng::from_str("[error, null, skip]").expect("missing policies");
    assert_eq!(
        missing,
        [
            MissingPolicy::Error,
            MissingPolicy::Null,
            MissingPolicy::Skip
        ]
    );
    let null: Vec<NullPolicy> = serde_yaml_ng::from_str("[keep, error, skip]").expect("null");
    assert_eq!(
        null,
        [NullPolicy::Keep, NullPolicy::Error, NullPolicy::Skip]
    );
    assert!(serde_yaml_ng::from_str::<MissingPolicy>("keep").is_err());
    assert_eq!(MissingPolicy::default(), MissingPolicy::Error);
    assert_eq!(NullPolicy::default(), NullPolicy::Keep);
    let tags = missing
        .iter()
        .map(|policy| policy.tag())
        .collect::<Vec<_>>();
    assert_eq!(tags, [0, 1, 2]);
    let tags = null.iter().map(|policy| policy.tag()).collect::<Vec<_>>();
    assert_eq!(tags, [0, 1, 2]);
}

// ---------------------------------------------------------------- retain

#[test]
fn retain_projects_all_except_and_only() {
    let parent = value(r#"{"id":"A","items":[1],"meta":{"k":null}}"#);
    let none = retain("mode: none").expect("none");
    assert_eq!(none.mode(), RetainMode::None);
    assert_eq!(none.project(&parent), Ok(Map::new()));

    let all = retain("mode: all").expect("all");
    assert_eq!(all.mode(), RetainMode::All);
    assert_eq!(all.project(&parent), Ok(object(parent.clone())));

    let except = retain("{mode: except, fields: [items, absent]}").expect("except");
    assert_eq!(except.mode(), RetainMode::Except);
    assert_eq!(
        except.project(&parent),
        Ok(object(value(r#"{"id":"A","meta":{"k":null}}"#)))
    );

    let only = retain(
        "mode: only
fields:
  - {path: /id, output: order}
  - {path: /meta/k, output: k, missing: skip}
  - {path: /gone, output: gone, missing: null}
  - {path: /quoted, output: quoted, missing: 'null'}
  - {path: /skip, output: skipped, missing: skip}",
    )
    .expect("only");
    assert_eq!(only.mode(), RetainMode::Only);
    assert_eq!(
        only.outputs().collect::<Vec<_>>(),
        ["order", "k", "gone", "quoted", "skipped"]
    );
    assert_eq!(
        only.project(&parent),
        Ok(object(value(
            r#"{"order":"A","k":null,"gone":null,"quoted":null}"#
        )))
    );

    let strict = retain("{mode: only, fields: [{path: /id, output: a}, {path: /x, output: b}]}")
        .expect("strict");
    let failure = strict.project(&parent).expect_err("missing");
    assert_eq!(failure.code, ShapingCode::MissingField);
    assert_eq!(failure.field, Some(1));
    // `only` accepts any parent shape because selections are pointers.
    assert_eq!(
        retain("{mode: only, fields: [{path: /0, output: a}]}")
            .expect("array parent")
            .project(&json!([7])),
        Ok(object(json!({"a": 7})))
    );
}

#[test]
fn retain_all_and_except_require_an_object_parent() {
    for spec in ["mode: all", "{mode: except, fields: [a]}"] {
        let spec = retain(spec).expect("spec");
        for parent in [json!([1]), json!("x"), Value::Null] {
            let failure = spec.project(&parent).expect_err("invalid row");
            assert_eq!(failure.code, ShapingCode::InvalidRow);
            assert_eq!(failure.field, None);
        }
    }
}

#[test]
fn retain_configuration_is_refused_when_malformed() {
    let sixty_four = (0..64)
        .map(|index| format!("{{path: /f{index}, output: o{index}}}"))
        .collect::<Vec<_>>();
    let sixty_five = (0..65)
        .map(|index| format!("{{path: /f{index}, output: o{index}}}"))
        .collect::<Vec<_>>();
    assert!(
        retain(&format!(
            "{{mode: only, fields: [{}]}}",
            sixty_four.join(",")
        ))
        .is_ok()
    );
    assert!(retain("{mode: only, fields: []}").is_ok());
    assert!(retain("{mode: except, fields: []}").is_ok());
    for refused in [
        "{mode: all, fields: [a]}".to_owned(),
        "{mode: none, fields: []}".to_owned(),
        "{mode: only, fields: [{path: /a, output: x}, {path: /b, output: x}]}".to_owned(),
        "{mode: except, fields: [a, a]}".to_owned(),
        "{mode: except, fields: ['']}".to_owned(),
        "{mode: only, fields: [{path: a, output: x}]}".to_owned(),
        "{mode: only, fields: [{path: /~2, output: x}]}".to_owned(),
        "{mode: only, fields: [{path: /a, output: ''}]}".to_owned(),
        "{mode: only, fields: [a]}".to_owned(),
        "{mode: except, fields: [{path: /a, output: x}]}".to_owned(),
        format!("{{mode: only, fields: [{}]}}", sixty_five.join(",")),
    ] {
        assert!(
            matches!(
                retain(&refused),
                Err(ShapingConfigurationError::Invalid(_)
                    | ShapingConfigurationError::ResourceExhausted)
            ),
            "{refused}"
        );
    }
    assert!(serde_yaml_ng::from_str::<RawRetain>("mode: some").is_err());
    assert!(serde_yaml_ng::from_str::<RawRetain>("{mode: all, extra: 1}").is_err());
    assert!(
        serde_yaml_ng::from_str::<RawRetain>(
            "{mode: only, fields: [{path: /a, output: x, other: 1}]}"
        )
        .is_err()
    );
}

#[test]
fn retain_digest_distinguishes_configurations() {
    let digest_of = |yaml: &str| {
        let mut context = digest::Context::new(&digest::SHA256);
        retain(yaml).expect("spec").digest_into(&mut context);
        copy_digest(context.finish().as_ref())
    };
    let only = digest_of("{mode: only, fields: [{path: /a, output: x}]}");
    assert_eq!(
        only,
        digest_of("{mode: only, fields: [{path: /a, output: x, missing: error}]}")
    );
    assert_ne!(
        only,
        digest_of("{mode: only, fields: [{path: /a, output: x, missing: skip}]}")
    );
    assert_ne!(
        digest_of("{mode: except, fields: [ab, c]}"),
        digest_of("{mode: except, fields: [a, bc]}")
    );
    assert_ne!(digest_of("mode: all"), digest_of("mode: none"));
    assert_ne!(
        digest_of("mode: all"),
        digest_of("{mode: except, fields: []}")
    );
}

// ---------------------------------------------------------------- envelope

#[test]
fn envelopes_are_strict() {
    let envelope = value(r#"{"parent_index":2.0,"position":1e1,"data":{"x":1}}"#);
    let parsed = parse_envelope(&envelope).expect("envelope");
    assert_eq!(parsed.parent_index, 2);
    assert_eq!(parsed.position, 10);
    assert_eq!(parsed.data, &object(json!({"x": 1})));
    assert!(parse_envelope(&value(r#"{"parent_index":-0,"position":0,"data":{}}"#)).is_some());
    for refused in [
        r#"{"parent_index":0,"position":0,"data":{},"extra":1}"#,
        r#"{"parent_index":0,"position":0}"#,
        r#"{"parent_index":0,"position":0,"datum":{}}"#,
        r#"{"parent_index":-1,"position":0,"data":{}}"#,
        r#"{"parent_index":0,"position":1.5,"data":{}}"#,
        r#"{"parent_index":"0","position":0,"data":{}}"#,
        r#"{"parent_index":18446744073709551616,"position":0,"data":{}}"#,
        r#"{"parent_index":0,"position":0,"data":[1]}"#,
        r#"{"parent_index":0,"position":0,"data":null}"#,
        "[0,0,{}]",
    ] {
        assert!(parse_envelope(&value(refused)).is_none(), "{refused}");
    }
    assert!(
        parse_envelope(&value(
            r#"{"parent_index":18446744073709551615,"position":0,"data":{}}"#
        ))
        .is_some()
    );
}

#[test]
fn envelope_rows_have_exactly_three_sorted_keys() {
    let row = envelope_row(3, 4, object(json!({"b": 1, "a": 2})));
    assert_eq!(
        serde_json::to_string(&row).expect("row"),
        r#"{"data":{"a":2,"b":1},"parent_index":3,"position":4}"#
    );
    let parsed = parse_envelope(&row).expect("round trip");
    assert_eq!((parsed.parent_index, parsed.position), (3, 4));
}

// ---------------------------------------------------------------- numbers

#[test]
fn decimal_normalization_is_canonical() {
    let cases: &[(&str, bool, &str, i64)] = &[
        ("0", false, "", 0),
        ("-0", false, "", 0),
        ("0.0", false, "", 0),
        ("-0.000e-5", false, "", 0),
        ("1", false, "1", 0),
        ("1.0", false, "1", 0),
        ("1e0", false, "1", 0),
        ("10e-1", false, "1", 0),
        ("100", false, "1", 2),
        ("1e2", false, "1", 2),
        ("1.50", false, "15", -1),
        ("-1E+2", true, "1", 2),
        ("0.001", false, "1", -3),
        ("120.0340", false, "120034", -3),
    ];
    for (text, negative, digits, exponent) in cases {
        let normalized = decimal(&number(text)).expect(text);
        assert_eq!(
            (
                normalized.negative,
                normalized.digits.as_str(),
                normalized.exponent
            ),
            (*negative, *digits, *exponent),
            "{text}"
        );
    }
    for unsupported in ["1e99999999999999999999", "1e-99999999999999999999"] {
        assert_eq!(
            decimal(&number(unsupported)).map(|_| ()),
            Err(ShapingCode::UnsupportedNumber),
            "{unsupported}"
        );
    }
    // The fraction length adjustment itself overflows i64.
    assert_eq!(
        decimal(&number("0.1e-9223372036854775808")).map(|_| ()),
        Err(ShapingCode::UnsupportedNumber)
    );
}

#[test]
fn exact_integers_are_checked_without_floating_point() {
    let cases: &[(&str, Result<i64, ShapingCode>)] = &[
        ("2", Ok(2)),
        ("2.0", Ok(2)),
        ("1e2", Ok(100)),
        ("10e-1", Ok(1)),
        ("-0", Ok(0)),
        ("-12.30e1", Ok(-123)),
        ("2.5", Err(ShapingCode::TypeMismatch)),
        ("1e-400", Err(ShapingCode::TypeMismatch)),
        ("9223372036854775807", Ok(i64::MAX)),
        ("9223372036854775808", Err(ShapingCode::IntegerOverflow)),
        ("-9223372036854775808", Ok(i64::MIN)),
        ("-9223372036854775809", Err(ShapingCode::IntegerOverflow)),
        ("18446744073709551615", Err(ShapingCode::IntegerOverflow)),
        ("1e400", Err(ShapingCode::IntegerOverflow)),
        ("1e9223372036854775807", Err(ShapingCode::IntegerOverflow)),
        (
            "1e99999999999999999999",
            Err(ShapingCode::UnsupportedNumber),
        ),
        (
            "1e-99999999999999999999",
            Err(ShapingCode::UnsupportedNumber),
        ),
    ];
    for (text, expected) in cases {
        assert_eq!(exact_i64(&number(text)), *expected, "{text}");
    }
}

// ---------------------------------------------------------------- canonical keys

#[test]
fn canonical_keys_use_exact_value_equality() {
    let key = |text: &str| canonical_key(&value(text), 32).expect(text);
    for equal in [
        ["1", "1.0"],
        ["1e0", "10e-1"],
        ["1", "10e-1"],
        ["-0", "0"],
        ["0.0", "0"],
        ["100", "1e2"],
        [r#"{"a":1,"b":2}"#, r#"{"b":2.0,"a":1}"#],
        ["[1,[2]]", "[1e0,[20e-1]]"],
    ] {
        assert_eq!(key(equal[0]), key(equal[1]), "{equal:?}");
    }
    for different in [
        [r#""1""#, "1"],
        ["[1,2]", "[2,1]"],
        ["-1", "1"],
        ["null", "false"],
        ["false", "true"],
        ["[]", "{}"],
        [r#""""#, "null"],
        ["[[]]", "[[],[]]"],
        [r#"{"a":"b"}"#, r#"{"ab":""}"#],
        [r#""\u00e9""#, r#""e\u0301""#],
        [r#""A""#, r#""a""#],
        ["0.1", "1"],
    ] {
        assert_ne!(key(different[0]), key(different[1]), "{different:?}");
    }
}

#[test]
fn canonical_key_tuples_are_injective() {
    let tuple = |parts: &[Value]| {
        let mut key = super::data_shaping::CanonicalKey::default();
        for part in parts {
            key.push(part, 32).expect("component");
        }
        key
    };
    assert_ne!(
        tuple(&[json!("ab"), json!("c")]),
        tuple(&[json!("a"), json!("bc")])
    );
    assert_ne!(tuple(&[json!([1]), json!(2)]), tuple(&[json!([1, 2])]));
    assert_eq!(
        tuple(&[json!(1), json!("x")]),
        tuple(&[value("1.0"), json!("x")])
    );
    assert_eq!(
        tuple(&[json!({"k": [1]})]),
        canonical_key(&json!({"k": [1]}), 32).expect("key")
    );
}

#[test]
fn canonical_keys_refuse_excess_depth_iteratively() {
    let mut deep = Value::Null;
    for _ in 0..1_000 {
        deep = Value::Array(vec![deep]);
    }
    assert_eq!(
        canonical_key(&deep, 32).map(|_| ()),
        Err(ShapingCode::LimitExceeded)
    );
    let mut shallow = json!(1);
    for _ in 0..31 {
        shallow = Value::Array(vec![shallow]);
    }
    assert!(canonical_key(&shallow, 32).is_ok());
    assert_eq!(
        canonical_key(&Value::Array(vec![shallow]), 32).map(|_| ()),
        Err(ShapingCode::LimitExceeded)
    );
    assert_eq!(
        canonical_key(&number_value("1e99999999999999999999"), 32).map(|_| ()),
        Err(ShapingCode::UnsupportedNumber)
    );
    // Release the nested value without deep recursion.
    while let Value::Array(mut items) = deep {
        deep = items.pop().unwrap_or(Value::Null);
    }
}

fn number_value(text: &str) -> Value {
    Value::Number(number(text))
}

// ---------------------------------------------------------------- limits

#[test]
fn limits_default_to_their_ceilings_and_only_lower_them() {
    let defaults = ShapingLimits::from_raw(None, true).expect("defaults");
    assert_eq!(
        (
            defaults.input_items,
            defaults.output_items,
            defaults.groups,
            defaults.bytes,
            defaults.depth,
            defaults.values
        ),
        (10_000, 10_000, Some(1_000), 524_288, 32, 32_768)
    );
    assert_eq!(
        ShapingLimits::from_raw(None, false).expect("split").groups,
        None
    );
    let maximal = limits(
        "{input_items: 10000, output_items: 10000, groups: 1000, bytes: 524288, depth: 32, values: 32768}",
        true,
    )
    .expect("ceilings");
    assert_eq!(maximal.bytes, 524_288);
    let lowered = limits("{bytes: 1, depth: 2}", false).expect("lowered");
    assert_eq!(
        (lowered.bytes, lowered.depth, lowered.values),
        (1, 2, 32_768)
    );
    for refused in [
        "{input_items: 0}",
        "{input_items: 10001}",
        "{output_items: 10001}",
        "{groups: 1001}",
        "{groups: 0}",
        "{bytes: 524289}",
        "{depth: 33}",
        "{depth: 0}",
        "{values: 32769}",
    ] {
        assert!(
            matches!(
                limits(refused, true),
                Err(ShapingConfigurationError::Invalid(_))
            ),
            "{refused}"
        );
    }
    assert!(matches!(
        limits("{groups: 5}", false),
        Err(ShapingConfigurationError::Invalid(_))
    ));
    assert!(serde_yaml_ng::from_str::<RawLimits>("{rows: 5}").is_err());
    assert!(serde_yaml_ng::from_str::<RawLimits>("{bytes: -1}").is_err());
}

#[test]
fn limits_are_digested_as_authored() {
    let digest_of = |limits: &ShapingLimits| {
        let mut context = digest::Context::new(&digest::SHA256);
        limits.digest_into(&mut context);
        copy_digest(context.finish().as_ref())
    };
    let absent = ShapingLimits::from_raw(None, true).expect("absent");
    let empty = limits("{}", true).expect("empty");
    let authored = limits("{bytes: 524288}", true).expect("authored");
    assert_eq!(digest_of(&absent), digest_of(&empty));
    assert_ne!(digest_of(&absent), digest_of(&authored));
    assert_eq!(
        digest_of(&authored),
        digest_of(&limits("{bytes: 524288}", true).expect("again"))
    );
    assert_ne!(
        digest_of(&limits("{input_items: 7}", true).expect("input")),
        digest_of(&limits("{output_items: 7}", true).expect("output"))
    );
}

#[test]
fn input_and_group_checks_use_the_limits() {
    let limits = limits("{input_items: 2, groups: 3}", true).expect("limits");
    assert_eq!(limits.check_input(2), Ok(()));
    assert_eq!(limits.check_input(3), Err(ShapingLimitKind::InputItems));
    assert_eq!(limits.check_groups(3), Ok(()));
    assert_eq!(limits.check_groups(4), Err(ShapingLimitKind::Groups));
    let split = ShapingLimits::from_raw(None, false).expect("split");
    assert_eq!(split.check_groups(usize::MAX), Ok(()));
    assert_eq!(ShapingLimitKind::Bytes.field(), "limits.bytes");
    assert_eq!(ShapingLimitKind::InputItems.field(), "limits.input_items");
}

// ---------------------------------------------------------------- budget

#[test]
fn budget_charges_the_exact_compact_bytes_of_the_list() {
    let rows = vec![
        json!({"a": "x\"y", "b": [1, 2.50, null]}),
        json!({}),
        json!({"nested": {"deep": [true, false]}, "unicode": "é"}),
    ];
    let limits = ShapingLimits::from_raw(None, false).expect("limits");
    let mut budget = Budget::new(&limits).expect("budget");
    assert_eq!(budget.bytes_used(), 2);
    for row in &rows {
        budget.charge_row(row).expect("row");
    }
    let expected = serde_json::to_vec(&Value::Array(rows.clone())).expect("bytes");
    assert_eq!(budget.bytes_used(), expected.len());

    let exact = limits_with(&format!("{{bytes: {}}}", expected.len()));
    let mut budget = Budget::new(&exact).expect("budget");
    for row in &rows {
        budget.charge_row(row).expect("fits exactly");
    }
    let short = limits_with(&format!("{{bytes: {}}}", expected.len() - 1));
    let mut budget = Budget::new(&short).expect("budget");
    budget.charge_row(&rows[0]).expect("first");
    budget.charge_row(&rows[1]).expect("second");
    assert_eq!(budget.charge_row(&rows[2]), Err(ShapingLimitKind::Bytes));
    assert!(Budget::new(&limits_with("{bytes: 1}")).is_err());
}

fn limits_with(yaml: &str) -> ShapingLimits {
    limits(yaml, false).expect("limits")
}

#[test]
fn budget_fails_at_the_first_violating_row() {
    let row = json!({"a": 1});
    // The list counts 1 value, each row here counts 2.
    let mut budget = Budget::new(&limits_with("{values: 5}")).expect("budget");
    budget.charge_row(&row).expect("first");
    budget.charge_row(&row).expect("second");
    assert_eq!(budget.charge_row(&row), Err(ShapingLimitKind::Values));

    let mut budget = Budget::new(&limits_with("{output_items: 2}")).expect("budget");
    budget.charge_row(&row).expect("first");
    budget.charge_row(&row).expect("second");
    assert_eq!(budget.charge_row(&row), Err(ShapingLimitKind::OutputItems));

    // The list is depth 1 and each row depth 2, so `{"a":[1]}` reaches depth 4.
    let mut budget = Budget::new(&limits_with("{depth: 3}")).expect("budget");
    budget.charge_row(&row).expect("depth 3");
    assert_eq!(
        budget.charge_row(&json!({"a": [1]})),
        Err(ShapingLimitKind::Depth)
    );
    let mut budget = Budget::new(&limits_with("{depth: 1}")).expect("list only");
    assert_eq!(budget.charge_row(&json!(1)), Err(ShapingLimitKind::Depth));

    let mut deep = Value::Null;
    for _ in 0..10_000 {
        deep = Value::Array(vec![deep]);
    }
    let mut budget =
        Budget::new(&ShapingLimits::from_raw(None, false).expect("limits")).expect("budget");
    assert_eq!(budget.charge_row(&deep), Err(ShapingLimitKind::Depth));
    while let Value::Array(mut items) = deep {
        deep = items.pop().unwrap_or(Value::Null);
    }
}

#[test]
fn budget_value_charges_are_a_lower_bound_of_the_rows() {
    let collected = [json!("abc"), json!({"k": [1, 2]}), json!(null)];
    let row = json!({"all": collected.clone()});
    let limits = ShapingLimits::from_raw(None, true).expect("limits");
    let mut lower = Budget::new(&limits).expect("lower");
    for value in &collected {
        lower.charge_value(value).expect("value");
    }
    let mut exact = Budget::new(&limits).expect("exact");
    exact.charge_row(&row).expect("row");
    assert!(lower.bytes_used() <= exact.bytes_used());
    assert!(lower.values_used() <= exact.values_used());

    let mut tight = Budget::new(&limits_with("{values: 3}")).expect("budget");
    tight.charge_value(&json!(1)).expect("one");
    tight.charge_value(&json!(1)).expect("two");
    assert_eq!(tight.charge_value(&json!(1)), Err(ShapingLimitKind::Values));
    let mut tight = Budget::new(&limits_with("{bytes: 6}")).expect("budget");
    tight.charge_value(&json!("ab")).expect("four bytes");
    assert_eq!(
        tight.charge_value(&json!("a")),
        Err(ShapingLimitKind::Bytes)
    );
    let mut shallow = Budget::new(&limits_with("{depth: 2}")).expect("budget");
    shallow.charge_value(&json!(1)).expect("scalar");
    assert_eq!(
        shallow.charge_value(&json!([1])),
        Err(ShapingLimitKind::Depth)
    );
}

// ---------------------------------------------------------------- errors

#[test]
fn shaping_codes_have_stable_snake_case_names() {
    let codes = [
        (ShapingCode::InvalidSource, "invalid_source"),
        (ShapingCode::InvalidRow, "invalid_row"),
        (ShapingCode::InvalidEnvelope, "invalid_envelope"),
        (ShapingCode::MissingField, "missing_field"),
        (ShapingCode::NullValue, "null_value"),
        (ShapingCode::TypeMismatch, "type_mismatch"),
        (ShapingCode::IntegerOverflow, "integer_overflow"),
        (ShapingCode::UnsupportedNumber, "unsupported_number"),
        (ShapingCode::FieldCollision, "field_collision"),
        (ShapingCode::RegroupConflict, "regroup_conflict"),
        (ShapingCode::RegroupInconsistent, "regroup_inconsistent"),
        (ShapingCode::LimitExceeded, "limit_exceeded"),
    ];
    for (code, name) in codes {
        assert_eq!(code.as_str(), name);
    }
}

#[test]
fn shaping_errors_name_only_fields_and_indices() {
    let row = json!({"secret": "SECRET-SENTINEL-42"});
    let selection = FieldSelection {
        pointer: pointer("/SECRET-SENTINEL-42"),
        missing: MissingPolicy::Error,
        null: NullPolicy::Keep,
    };
    let code = selection.select(&row).expect_err("missing");
    let error = ShapingError::new(code, "operations[1].field")
        .at_item(7)
        .at_position(3);
    assert_eq!(error.code(), ShapingCode::MissingField);
    let message = error.message();
    assert_eq!(
        message,
        "graph.shaping.missing_field: operations[1].field at item 7 position 3"
    );
    assert!(!message.contains("SECRET-SENTINEL-42"));
    match error.into_graph_error("summarize") {
        GraphError::NodeExecutionFailed { node, message } => {
            assert_eq!(node, "summarize");
            assert!(message.starts_with("graph.shaping.missing_field: "));
            assert!(!message.contains("SECRET"));
        }
        other => panic!("unexpected error: {other}"),
    }
    assert_eq!(
        ShapingError::limit(ShapingLimitKind::Bytes).message(),
        "graph.shaping.limit_exceeded: limits.bytes"
    );
    assert_eq!(
        ShapingError::new(ShapingCode::InvalidRow, "source")
            .at_item(0)
            .message(),
        "graph.shaping.invalid_row: source at item 0"
    );
}

// ---------------------------------------------------------------- yaml

#[test]
fn node_yaml_is_bounded() {
    let parsed: RawLimits = parse_node_yaml("bytes: 5").expect("yaml");
    assert_eq!(parsed.bytes, Some(5));
    assert!(matches!(
        parse_node_yaml::<RawLimits>(""),
        Err(ShapingConfigurationError::ResourceExhausted)
    ));
    let oversized = format!("bytes: 5\n#{}", "x".repeat(64 * 1024));
    assert!(matches!(
        parse_node_yaml::<RawLimits>(&oversized),
        Err(ShapingConfigurationError::ResourceExhausted)
    ));
    assert!(matches!(
        parse_node_yaml::<RawLimits>("unknown: 1"),
        Err(ShapingConfigurationError::MalformedYaml { .. })
    ));
}
