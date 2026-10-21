use serde::Deserialize;
use serde_json::{Value, json};

use super::*;

const CASES_JSON: &str = include_str!("../../tests/fixtures/configuration_validation_cases.json");

fn catalog() -> &'static ConfigurationCatalog {
    ConfigurationCatalog::pinned().expect("the embedded rules table loads")
}

fn command_for(configuration_type: &str) -> ConfigurationValidationCommandV1 {
    catalog()
        .command_for(configuration_type)
        .expect("type in catalog")
}

fn issues_of(rules: &TypeRules, settings: &Value) -> Vec<(String, String)> {
    rules
        .evaluate(settings.to_string().as_bytes())
        .expect("settings evaluate")
        .into_iter()
        .map(|issue| (issue.code.code().to_owned(), issue.json_pointer))
        .collect()
}

#[derive(Deserialize)]
struct Cases {
    catalog_revision: String,
    cases: Vec<Case>,
}

#[derive(Deserialize)]
struct Case {
    #[serde(rename = "type")]
    configuration_type: String,
    settings: String,
    valid: bool,
    issues: Vec<(String, String)>,
}

#[test]
fn embedded_table_matches_the_catalog_main_embeds() {
    let catalog = catalog();
    let path = concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/../elitea-main/internal/runtimecomposition/",
        "current_sdk_configuration_catalog_snapshot.json"
    );
    // The container build context omits elitea-main; the repository does not.
    let Ok(raw) = std::fs::read_to_string(path) else {
        return;
    };
    let main: Value = serde_json::from_str(&raw).unwrap();
    assert_eq!(main["catalog_revision"], catalog.revision.as_str());
    assert_eq!(
        main["catalog_digest"],
        format!("sha256:{}", hex(&catalog.digest))
    );
    let entries = main["entries"].as_array().unwrap();
    assert_eq!(entries.len(), catalog.types.len());
    for entry in entries {
        let name = entry["configuration_type"].as_str().unwrap();
        let (rules, digest) = catalog
            .types
            .iter()
            .find(|(rules, _)| rules.configuration_type == name)
            .unwrap_or_else(|| panic!("{name} missing from the Rust table"));
        assert_eq!(entry["schema_id"], rules.schema_id.as_str());
        assert_eq!(entry["section"], rules.section.as_str());
        assert_eq!(entry["schema_revision"], catalog.revision.as_str());
        assert_eq!(entry["schema_digest"], format!("sha256:{}", hex(digest)));
        assert_eq!(entry["validation_supported"], true);
    }
}

fn hex(bytes: &[u8]) -> String {
    use std::fmt::Write as _;
    bytes.iter().fold(String::new(), |mut out, byte| {
        let _ = write!(out, "{byte:02x}");
        out
    })
}

#[test]
fn every_catalog_type_is_covered_and_listed() {
    let names: Vec<&str> = catalog().type_names().collect();
    assert_eq!(names.len(), 32);
    for expected in ["jira", "confluence", "github", "pgvector", "openapi", "sql"] {
        assert!(names.contains(&expected), "{expected}");
    }
    let mut sorted = names.clone();
    sorted.sort_unstable();
    assert_eq!(names, sorted, "the table keeps Main's ordering");
}

#[test]
fn recorded_pydantic_outcomes_match_for_every_type() {
    let corpus: Cases = serde_json::from_str(CASES_JSON).unwrap();
    assert_eq!(corpus.catalog_revision, catalog().revision);
    let mut per_type = std::collections::BTreeMap::<String, usize>::new();
    for case in &corpus.cases {
        let (rules, _) = catalog()
            .types
            .iter()
            .find(|(rules, _)| rules.configuration_type == case.configuration_type)
            .expect("recorded type is in the table");
        let actual = rules
            .evaluate(case.settings.as_bytes())
            .unwrap_or_else(|refusal| {
                panic!(
                    "{} {} refused: {refusal:?}",
                    case.configuration_type, case.settings
                )
            });
        let actual: Vec<(String, String)> = actual
            .into_iter()
            .map(|issue| (issue.code.code().to_owned(), issue.json_pointer))
            .collect();
        assert_eq!(
            (actual.is_empty(), &actual),
            (case.valid, &case.issues),
            "{} {}",
            case.configuration_type,
            case.settings
        );
        *per_type.entry(case.configuration_type.clone()).or_default() += 1;
    }
    // Every type has recorded valid and invalid evidence, not just a table row.
    for name in catalog().type_names() {
        assert!(per_type.get(name).copied().unwrap_or(0) >= 2, "{name}");
        let valid = corpus
            .cases
            .iter()
            .any(|case| case.configuration_type == name && case.valid);
        assert!(valid, "{name} has no recorded valid case");
    }
}

#[test]
fn a_type_outside_the_table_is_refused_by_name_never_validated() {
    let mut command = command_for("jira");
    command.configuration_type = "not_a_real_type".into();
    command.schema_id = "elitea.configuration.not_a_real_type".into();
    let refusal = catalog().bind(&command).unwrap_err();
    assert_eq!(
        refusal,
        BindingRefusal::UnsupportedConfigurationType {
            configuration_type: "not_a_real_type".into()
        }
    );
    assert_eq!(refusal.failure(), RuntimeFailureKind::UnsupportedCapability);
    // Types the Python worker never validated either (LLM and embedding model
    // configurations are Main-side normalizers, not SDK registry entries).
    for outside in ["embedding", "ai_credentials", "openai", "mcp"] {
        let mut command = command_for("jira");
        command.configuration_type = outside.into();
        assert!(matches!(
            catalog().bind(&command),
            Err(BindingRefusal::UnsupportedConfigurationType { configuration_type })
                if configuration_type == outside
        ));
    }
}

#[test]
fn binding_checks_identity_catalog_and_schema_in_python_order() {
    let catalog = catalog();
    assert!(catalog.bind(&command_for("jira")).is_ok());

    // Input shape (empty identities, short or missing digests) belongs to
    // command verification; here a missing digest simply cannot match.
    let mut missing = command_for("jira");
    missing.catalog_digest = None;
    assert_eq!(
        catalog.bind(&missing).unwrap_err(),
        BindingRefusal::IncompatibleVersion
    );
    let mut short = command_for("jira");
    short.schema_digest.as_mut().unwrap().value.pop();
    assert_eq!(
        catalog.bind(&short).unwrap_err(),
        BindingRefusal::IncompatibleVersion
    );

    let mut revision = command_for("jira");
    revision.catalog_revision = "other".into();
    assert_eq!(
        catalog.bind(&revision).unwrap_err(),
        BindingRefusal::IncompatibleVersion
    );

    let mut catalog_digest = command_for("jira");
    catalog_digest.catalog_digest.as_mut().unwrap().value[0] ^= 1;
    assert_eq!(
        catalog.bind(&catalog_digest).unwrap_err(),
        BindingRefusal::IncompatibleVersion
    );

    // A schema digest of another type must not validate this type.
    let mut crossed = command_for("jira");
    crossed.schema_digest = command_for("confluence").schema_digest;
    assert_eq!(
        catalog.bind(&crossed).unwrap_err(),
        BindingRefusal::IncompatibleVersion
    );
    let mut schema_id = command_for("jira");
    schema_id.schema_id = "elitea.configuration.confluence".into();
    assert_eq!(
        catalog.bind(&schema_id).unwrap_err(),
        BindingRefusal::IncompatibleVersion
    );
    let mut schema_revision = command_for("jira");
    schema_revision.schema_revision = "other".into();
    assert_eq!(
        catalog.bind(&schema_revision).unwrap_err(),
        BindingRefusal::IncompatibleVersion
    );

    // Unsupported is reported only after the catalog itself matched.
    let mut unknown_under_old_catalog = command_for("jira");
    unknown_under_old_catalog.configuration_type = "nope".into();
    unknown_under_old_catalog.catalog_revision = "other".into();
    assert_eq!(
        catalog.bind(&unknown_under_old_catalog).unwrap_err(),
        BindingRefusal::IncompatibleVersion
    );
}

#[test]
fn settings_documents_are_refused_like_the_python_loader_refuses_them() {
    let rules = catalog().bind(&command_for("jira")).unwrap();
    let invalid = |raw: &[u8]| rules.evaluate(raw).unwrap_err();
    assert_eq!(invalid(b""), SettingsRefusal::InvalidInput);
    assert_eq!(invalid(b"[]"), SettingsRefusal::InvalidInput);
    assert_eq!(invalid(b"\"jira\""), SettingsRefusal::InvalidInput);
    assert_eq!(invalid(b"{"), SettingsRefusal::InvalidInput);
    assert_eq!(invalid(b"{\"a\":NaN}"), SettingsRefusal::InvalidInput);
    assert_eq!(invalid(b"{\"a\":1e999}"), SettingsRefusal::InvalidInput);
    assert_eq!(invalid(&[b'{', 0xff, b'}']), SettingsRefusal::InvalidInput);
    // Duplicate members, at any depth.
    assert_eq!(
        invalid(br#"{"base_url":"a","base_url":"b"}"#),
        SettingsRefusal::InvalidInput
    );
    assert_eq!(
        invalid(br#"{"base_url":"a","x":{"y":1,"y":2}}"#),
        SettingsRefusal::InvalidInput
    );
    assert_eq!(
        invalid(br#"{"base_url":"a","x":[{"y":1,"y":2}]}"#),
        SettingsRefusal::InvalidInput
    );
    // Size, string and nesting bounds.
    let oversize = format!("{{\"base_url\":\"{}\"}}", "a".repeat(MAX_SETTINGS_BYTES));
    assert_eq!(
        invalid(oversize.as_bytes()),
        SettingsRefusal::ResourceExhausted
    );
    let long = format!("{{\"base_url\":\"{}\"}}", "a".repeat(MAX_STRING_BYTES + 1));
    assert_eq!(invalid(long.as_bytes()), SettingsRefusal::ResourceExhausted);
    let ok_long = format!("{{\"base_url\":\"{}\"}}", "a".repeat(MAX_STRING_BYTES));
    assert_eq!(rules.evaluate(ok_long.as_bytes()).unwrap(), []);
    assert!(rules.evaluate(nested(62).as_bytes()).is_ok());
    assert_eq!(
        invalid(nested(64).as_bytes()),
        SettingsRefusal::ResourceExhausted
    );
    // Distinct failures keep distinct registered kinds.
    assert_eq!(
        SettingsRefusal::InvalidInput.failure(),
        RuntimeFailureKind::InvalidInput
    );
    assert_eq!(
        SettingsRefusal::ResourceExhausted.failure(),
        RuntimeFailureKind::ResourceExhausted
    );
}

#[test]
fn results_never_echo_values_and_stay_in_the_closed_vocabulary() {
    let rules = catalog().bind(&command_for("jira")).unwrap();
    let secret = "super-secret-token-value";
    let issues = rules
        .evaluate(
            json!({"base_url": 5, "hosting": secret, "api_key": [secret]})
                .to_string()
                .as_bytes(),
        )
        .unwrap();
    assert_eq!(issues.len(), 3);
    for issue in &issues {
        assert!(!issue.json_pointer.contains(secret));
        assert!(!issue.code.safe_message().contains(secret));
        assert_eq!(IssueCode::from_wire(issue.code.code()), Some(issue.code));
    }
    assert_eq!(
        issues
            .iter()
            .map(|issue| (issue.code.code(), issue.json_pointer.as_str()))
            .collect::<Vec<_>>(),
        [
            ("INVALID_VALUE", "/api_key"),
            ("INVALID_VALUE", "/base_url"),
            ("VALUE_NOT_ALLOWED", "/hosting"),
        ]
    );
}

#[test]
fn issue_list_is_capped_deduplicated_and_ordered() {
    let rules = catalog().bind(&command_for("sharepoint")).unwrap();
    let scopes: Vec<Value> = (0..100).map(|_| json!(1)).collect();
    let issues = issues_of(
        rules,
        &json!({"client_id": "c", "client_secret": "s", "site_url": "u", "scopes": scopes}),
    );
    assert_eq!(issues.len(), MAX_ISSUES);
    let mut sorted = issues.clone();
    sorted.sort_by(|a, b| (&a.1, &a.0).cmp(&(&b.1, &b.0)));
    assert_eq!(issues, sorted);
    assert!(issues.iter().all(|issue| issue.0 == "INVALID_VALUE"));
}

#[test]
fn model_validator_hooks_replace_field_errors_with_one_root_issue() {
    let github = catalog().bind(&command_for("github")).unwrap();
    assert_eq!(
        issues_of(github, &json!({"base_url": 5, "username": "u"})),
        [("INVALID_CONFIGURATION".to_owned(), String::new())]
    );
    assert_eq!(
        issues_of(
            github,
            &json!({"base_url": "b", "username": "u", "password": "p"})
        ),
        [] as [(String, String); 0]
    );
    let openapi = catalog().bind(&command_for("openapi")).unwrap();
    assert_eq!(
        issues_of(openapi, &json!({"client_id": "c"})),
        [("INVALID_CONFIGURATION".to_owned(), String::new())]
    );
    assert_eq!(issues_of(openapi, &json!({})), [] as [(String, String); 0]);
}

#[test]
fn canonical_messages_are_main_s_exact_text() {
    for (code, message) in [
        (
            "INVALID_CONFIGURATION",
            "Configuration fields are inconsistent.",
        ),
        (
            "INVALID_VALUE",
            "Value does not satisfy the configuration schema.",
        ),
        ("REQUIRED_FIELD", "A required value is missing."),
        (
            "VALUE_NOT_ALLOWED",
            "Value is not one of the allowed choices.",
        ),
    ] {
        let parsed = IssueCode::from_wire(code).unwrap();
        assert_eq!(parsed.safe_message(), message);
    }
    assert!(IssueCode::from_wire("SOMETHING_ELSE").is_none());
}

fn jira_issues(settings: &str) -> Result<Vec<(String, String)>, SettingsRefusal> {
    let rules = catalog().bind(&command_for("jira")).unwrap();
    rules.evaluate(settings.as_bytes()).map(|issues| {
        issues
            .into_iter()
            .map(|issue| (issue.code.code().to_owned(), issue.json_pointer))
            .collect()
    })
}

fn sql_issues(settings: &str) -> Result<Vec<(String, String)>, SettingsRefusal> {
    let rules = catalog().bind(&command_for("sql")).unwrap();
    rules.evaluate(settings.as_bytes()).map(|issues| {
        issues
            .into_iter()
            .map(|issue| (issue.code.code().to_owned(), issue.json_pointer))
            .collect()
    })
}

#[test]
fn integer_strings_obey_pydantic_s_digit_limit() {
    // Recorded with pydantic 2.12.5 on Python 3.13 (`int_parsing_size` or
    // `int_parsing` for the refused spellings, both INVALID_VALUE here).
    let port = |text: String| {
        sql_issues(&format!(
            r#"{{"host":"h","username":"u","password":"p","port":"{text}"}}"#
        ))
        .unwrap()
    };
    let invalid = vec![("INVALID_VALUE".to_owned(), "/port".to_owned())];
    let nines = |count: usize| "9".repeat(count);
    assert_eq!(port(nines(4300)), [] as [(String, String); 0]);
    assert_eq!(port(nines(4301)), invalid);
    assert_eq!(
        port(format!("+{}", nines(4300))),
        [] as [(String, String); 0]
    );
    assert_eq!(port(format!("+{}", nines(4301))), invalid);
    // The sign counts toward the limit only when it is `-`.
    assert_eq!(
        port(format!("-{}", nines(4299))),
        [] as [(String, String); 0]
    );
    assert_eq!(port(format!("-{}", nines(4300))), invalid);
    // Whitespace, a zero fraction, leading zeros and underscores do not count.
    assert_eq!(
        port(format!(" {} ", nines(4300))),
        [] as [(String, String); 0]
    );
    assert_eq!(
        port(format!("{}.000", nines(4300))),
        [] as [(String, String); 0]
    );
    assert_eq!(port(format!("{}.0", nines(4301))), invalid);
    assert_eq!(
        port(format!("{}{}", "0".repeat(5000), nines(4300))),
        [] as [(String, String); 0]
    );
    assert_eq!(
        port(format!("{}_{}", nines(2000), nines(2300))),
        [] as [(String, String); 0]
    );
    assert_eq!(port(format!("{}_{}", nines(2000), nines(2301))), invalid);
}

#[test]
fn integer_literals_beyond_f64_are_valid_ints_up_to_python_s_limit() {
    let big = "7".repeat(400);
    // In an int field, and in an extra key the models do not declare.
    assert_eq!(
        sql_issues(&format!(
            r#"{{"host":"h","username":"u","password":"p","port":{big}}}"#
        )),
        Ok(vec![])
    );
    assert_eq!(
        sql_issues(&format!(
            r#"{{"host":"h","username":"u","password":"p","port":-{big},"extra":{big}}}"#
        )),
        Ok(vec![])
    );
    // A string field still refuses the number, with a verdict not a failure.
    assert_eq!(
        jira_issues(&format!(r#"{{"base_url":{big}}}"#)),
        Ok(vec![("INVALID_VALUE".to_owned(), "/base_url".to_owned())])
    );
    // Python's json refuses a literal over 4300 digits as malformed.
    let limit = "7".repeat(4300);
    assert_eq!(
        jira_issues(&format!(r#"{{"base_url":"u","extra":{limit}}}"#)),
        Ok(vec![])
    );
    let over = "7".repeat(4301);
    assert_eq!(
        jira_issues(&format!(r#"{{"base_url":"u","extra":{over}}}"#)),
        Err(SettingsRefusal::InvalidInput)
    );
    assert_eq!(
        jira_issues(&format!(r#"{{"base_url":"u","extra":-{over}}}"#)),
        Err(SettingsRefusal::InvalidInput)
    );
    // Non-finite floats stay refused.
    assert_eq!(
        jira_issues(&format!(r#"{{"base_url":"u","extra":{big}.5e999}}"#)),
        Err(SettingsRefusal::InvalidInput)
    );
}

/// `{"base_url":"a","x":[[...0...]]}` with `depth` brackets: the root is depth
/// 0, the innermost array `depth`, the scalar `depth + 1` (as
/// `_check_json_limits` counts).
fn nested(depth: usize) -> String {
    format!(
        "{{\"base_url\":\"a\",\"x\":{}0{}}}",
        "[".repeat(depth),
        "]".repeat(depth)
    )
}

#[test]
fn nesting_is_bounded_like_python_s_limit_walk_not_serde_json_s_parser() {
    // Python: a value at depth > 64 is RESOURCE_EXHAUSTED. serde_json's own
    // parser would have stopped at 128 levels with a syntax error.
    assert_eq!(jira_issues(&nested(63)), Ok(vec![]));
    for depth in [
        64,
        65,
        100,
        127,
        128,
        129,
        200,
        1000,
        strict_json::HARD_MAX_DEPTH,
    ] {
        assert_eq!(
            jira_issues(&nested(depth)),
            Err(SettingsRefusal::ResourceExhausted),
            "depth {depth}"
        );
    }
    // Past the hard ceiling parsing stops; the verdict is the same.
    for depth in [
        strict_json::HARD_MAX_DEPTH + 1,
        strict_json::HARD_MAX_DEPTH + 5000,
    ] {
        assert_eq!(
            jira_issues(&nested(depth)),
            Err(SettingsRefusal::ResourceExhausted),
            "depth {depth}"
        );
    }
    // Objects count the same as arrays.
    let objects = format!(
        "{{\"base_url\":\"a\",\"x\":{}0{}}}",
        "{\"k\":".repeat(200),
        "}".repeat(200)
    );
    assert_eq!(
        jira_issues(&objects),
        Err(SettingsRefusal::ResourceExhausted)
    );
    // `json.loads` runs before the limit walk: a syntax error or a duplicate
    // member later in a too-deep document is still malformed input, and a
    // too-deep non-object root is refused as a non-object.
    let deep = format!("{}0{}", "[".repeat(200), "]".repeat(200));
    assert_eq!(
        jira_issues(&format!("{{\"x\":{deep},\"y\":}}")),
        Err(SettingsRefusal::InvalidInput)
    );
    assert_eq!(
        jira_issues(&format!("{{\"x\":{deep},\"y\":1,\"y\":2}}")),
        Err(SettingsRefusal::InvalidInput)
    );
    assert_eq!(jira_issues(&deep), Err(SettingsRefusal::InvalidInput));
    // So is an integer literal past the digit bound, which `int()` refuses
    // inside `json.loads`; an overflowing float is only caught by the walk,
    // after the nesting verdict.
    let over = "7".repeat(MAX_INTEGER_LITERAL_DIGITS + 1);
    assert_eq!(
        jira_issues(&format!("{{\"x\":{deep},\"y\":{over}}}")),
        Err(SettingsRefusal::InvalidInput)
    );
    assert_eq!(
        jira_issues(&format!("{{\"x\":{deep},\"y\":1e999}}")),
        Err(SettingsRefusal::ResourceExhausted)
    );
}

#[test]
fn serde_json_s_private_number_marker_is_an_ordinary_object_key() {
    // Under `arbitrary_precision` serde_json's own `Value` turns this object
    // into a number; Python's `json.loads` sees an object with one key.
    const MARKER: &str = "$serde_json::private::Number";
    // In a field: an object is not a string, an int or a bool.
    assert_eq!(
        jira_issues(&format!(r#"{{"base_url":{{"{MARKER}":"123"}}}}"#)),
        Ok(vec![("INVALID_VALUE".to_owned(), "/base_url".to_owned())])
    );
    assert_eq!(
        sql_issues(&format!(
            r#"{{"host":"h","username":"u","password":"p","port":{{"{MARKER}":"5"}}}}"#
        )),
        Ok(vec![("INVALID_VALUE".to_owned(), "/port".to_owned())])
    );
    // Nested, in an extra key the model ignores: valid, whatever the text.
    for text in ["123", "abc", "1e999", ""] {
        assert_eq!(
            jira_issues(&format!(
                r#"{{"base_url":"u","extra":[{{"{MARKER}":"{text}"}}]}}"#
            )),
            Ok(vec![]),
            "{text}"
        );
    }
    // As a top-level key: an extra member, not a number.
    assert_eq!(
        jira_issues(&format!(r#"{{"base_url":"u","{MARKER}":"123"}}"#)),
        Ok(vec![])
    );
    // As the only top-level key: an object missing its required field, not
    // a non-object document.
    assert_eq!(
        jira_issues(&format!(r#"{{"{MARKER}":"123"}}"#)),
        Ok(vec![("REQUIRED_FIELD".to_owned(), "/base_url".to_owned())])
    );
    // Duplicated, it is a duplicate member like any other.
    assert_eq!(
        jira_issues(&format!(
            r#"{{"base_url":"u","{MARKER}":"1","{MARKER}":"2"}}"#
        )),
        Err(SettingsRefusal::InvalidInput)
    );
    // A hook sees the object's Python truthiness (non-empty dict), not a 0.
    let github = catalog().bind(&command_for("github")).unwrap();
    assert_eq!(
        issues_of(github, &json!({"base_url": "b", "username": {MARKER: "0"}})),
        [("INVALID_CONFIGURATION".to_owned(), String::new())]
    );
}

#[test]
fn lexical_rules_follow_python_s_strict_decoder() {
    let ok = |body: &str| jira_issues(&format!(r#"{{"base_url":"u","x":{body}}}"#));
    // Numbers: the JSON grammar, any size, kept as text.
    for literal in [
        "0",
        "-0",
        "1E5",
        "1e+5",
        "1.5e-3",
        "-12.000",
        "123456789012345678901234567890",
    ] {
        assert_eq!(ok(literal), Ok(vec![]), "{literal}");
    }
    for literal in [
        "01",
        "1.",
        ".5",
        "-",
        "+1",
        "1e",
        "1e+",
        "0x10",
        "1_000",
        "NaN",
        "Infinity",
        "-Infinity",
        "True",
        "None",
    ] {
        assert_eq!(ok(literal), Err(SettingsRefusal::InvalidInput), "{literal}");
    }
    // Strings: every escape, surrogate pairs, raw non-ASCII; no raw control
    // characters, unknown escapes or lone surrogates.
    assert_eq!(ok(r#""\" \\ \/ \b \f \n \r \t é 😀 é 😀""#), Ok(vec![]));
    for literal in [
        r#""\x""#,
        r#""\u12""#,
        r#""\ud83d""#,
        r#""\ud83dA""#,
        r#""\ude00""#,
        "\"a\tb\"",
        "\"a\u{1}b\"",
        "\"unterminated",
    ] {
        assert_eq!(ok(literal), Err(SettingsRefusal::InvalidInput), "{literal}");
    }
    let rules = catalog().bind(&command_for("jira")).unwrap();
    let decoded = |raw: &str| {
        let hosting = format!(r#"{{"base_url":"u","hosting":{raw}}}"#);
        rules.evaluate(hosting.as_bytes()).unwrap()
    };
    // `Cloud` decodes to the enum member `Cloud`.
    assert_eq!(decoded(r#""Cloud""#), []);
    assert_eq!(decoded(r#""Cloud\u0000""#).len(), 1);
    // Structure: JSON whitespace only, no trailing commas or data, no BOM.
    assert_eq!(
        jira_issues(" \t\r\n{ \"base_url\" : \"u\" , \"x\" : [ 1 , 2 ] , \"y\" : { } } \n"),
        Ok(vec![])
    );
    for document in [
        "{\"base_url\":\"u\",}",
        "{\"base_url\":\"u\" \"x\":1}",
        "{\"base_url\" \"u\"}",
        "{\"base_url\":\"u\",\"x\":[1,]}",
        "{\"base_url\":\"u\",\"x\":[,1]}",
        "{\"base_url\":\"u\"} x",
        "{\"base_url\":\"u\"}{}",
        "\u{feff}{\"base_url\":\"u\"}",
        "{\"base_url\":\"u\"\u{b}}",
        "{\"base_url\":\"u\",x:1}",
        "{'base_url':'u'}",
        "{\"base_url\":\"u\",\"x\":}",
        "{\"base_url\":\"u\",\"x\"}",
        "null",
        "{\"base_url\":\"u\"",
        "{\"base_url\":\"u\"]",
    ] {
        assert_eq!(
            jira_issues(document),
            Err(SettingsRefusal::InvalidInput),
            "{document:?}"
        );
    }
}

#[test]
fn hook_ports_are_pinned_to_the_sdk_source_they_were_read_from() {
    // The generator records sha256(inspect.getsource(validator)) per type;
    // the Rust hook embeds the digest it was ported from. The table refuses
    // to load when they differ, so this test also guards `pinned()`.
    let catalog = catalog();
    let mut hooked = Vec::new();
    for (rules, _) in &catalog.types {
        assert_eq!(
            rules.hook.map(Hook::ported_source_sha256),
            rules.hook_source_sha256.as_deref(),
            "{}",
            rules.configuration_type
        );
        if let Some(hook) = rules.hook {
            hooked.push((rules.configuration_type.as_str(), hook));
        }
    }
    assert_eq!(
        hooked,
        [("github", Hook::GithubAuth), ("openapi", Hook::OpenapiAuth)]
    );
    assert_eq!(
        Hook::GithubAuth.ported_source_sha256(),
        "c36d06e44c8c4a15a0299f25c3930407fc1a4f25a213be6fd52b92223ceef6ce"
    );
    assert_eq!(
        Hook::OpenapiAuth.ported_source_sha256(),
        "a59a4aaf6b3ed3d10acd6556a064416c1750f59bda993f458e4dd2f917b20b62"
    );
    // A regenerated table whose hook source moved on is refused outright.
    let drifted = RULES_JSON.replacen(Hook::GithubAuth.ported_source_sha256(), &"0".repeat(64), 1);
    assert_ne!(drifted, RULES_JSON);
    assert!(ConfigurationCatalog::from_json(&drifted).is_none());
    // So is a table that drops the digest while keeping the hook.
    let dropped = RULES_JSON.replacen(
        &format!("\"{}\"", Hook::OpenapiAuth.ported_source_sha256()),
        "null",
        1,
    );
    assert_ne!(dropped, RULES_JSON);
    assert!(ConfigurationCatalog::from_json(&dropped).is_none());
}

#[test]
fn every_code_main_accepts_round_trips_with_its_canonical_message() {
    let expected = [
        (
            "INVALID_CONFIGURATION",
            "Configuration fields are inconsistent.",
        ),
        (
            "INVALID_VALUE",
            "Value does not satisfy the configuration schema.",
        ),
        ("REQUIRED_FIELD", "A required value is missing."),
        (
            "VALUE_NOT_ALLOWED",
            "Value is not one of the allowed choices.",
        ),
        ("UNKNOWN_FIELD", "This field is not allowed."),
        ("VALUE_OUT_OF_RANGE", "Value is outside the allowed range."),
    ];
    assert_eq!(IssueCode::ALL.len(), expected.len());
    for (code, message) in expected {
        let parsed = IssueCode::from_wire(code).unwrap();
        assert_eq!(parsed.code(), code);
        assert_eq!(parsed.safe_message(), message);
        assert!(IssueCode::ALL.contains(&parsed));
    }
    assert!(IssueCode::from_wire("invalid_value").is_none());
}

#[test]
fn digests_in_the_table_are_lowercase_hex_only() {
    let digest = "0f".repeat(32);
    assert_eq!(decode_digest(&digest), Some([0x0f; 32]));
    assert_eq!(decode_digest(&format!("sha256:{digest}")), Some([0x0f; 32]));
    assert_eq!(decode_digest(&"0F".repeat(32)), None);
    assert_eq!(decode_digest(&"0f".repeat(31)), None);
    assert_eq!(decode_digest(&format!("{digest}0f")), None);
    assert_eq!(decode_digest(&format!("0g{}", &digest[2..])), None);
}
