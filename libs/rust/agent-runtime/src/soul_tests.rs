use serde_json::json;

use super::{MAX_TEXT_BYTES, SCHEMA_ID, Soul, SoulError, with_soul};

fn mira() -> Soul {
    Soul::admit(Some(&json!({
        "schema": "elitea.soul/v1",
        "display_name": "Mira",
        "pronouns": "they/them",
        "role": "Release coordinator",
        "voice": ["concise", "warm"],
        "values": ["cite sources"],
        "boundaries": ["never merges without a green CI"],
        "relationships": [{"agent": "ci-doctor", "stance": "delegate failing builds"}],
        "proactive_goals": ["watch CI"],
        "body": "Short sentences.\n\nNo exclamation marks."
    })))
    .unwrap()
    .unwrap()
}

#[test]
fn no_soul_is_not_an_error() {
    assert_eq!(Soul::admit(None), Ok(None));
    assert_eq!(Soul::admit(Some(&serde_json::Value::Null)), Ok(None));
}

#[test]
fn a_capability_or_unknown_key_is_refused() {
    for value in [
        json!({"schema": SCHEMA_ID, "tools": ["github"]}),
        json!({"schema": SCHEMA_ID, "credentials": {}}),
        json!({"schema": SCHEMA_ID, "relationships": [{"agent": "a", "stance": "b", "permissions": []}]}),
        json!({"schema": SCHEMA_ID, "mood": "sunny"}),
        json!("a string"),
    ] {
        assert_eq!(Soul::admit(Some(&value)), Err(SoulError::Shape), "{value}");
    }
    assert_eq!(
        Soul::admit(Some(&json!({"schema": "elitea.soul/v2"}))),
        Err(SoulError::Schema)
    );
}

#[test]
fn the_text_limit_matches_mains_measure() {
    let room = MAX_TEXT_BYTES - SCHEMA_ID.len();
    let at_limit =
        json!({"schema": SCHEMA_ID, "body": "é".repeat(room / 2) + &"a".repeat(room % 2)});
    assert!(Soul::admit(Some(&at_limit)).is_ok());
    let over = json!({"schema": SCHEMA_ID, "display_name": "M", "body": "é".repeat(room / 2) + &"a".repeat(room % 2)});
    assert_eq!(Soul::admit(Some(&over)), Err(SoulError::TooLarge));
}

#[test]
fn the_persona_renders_before_the_instructions_and_goals_stay_out() {
    let text = with_soul(Some(&mira()), "Do the release.");
    let persona = text.find("## Persona").unwrap();
    let instructions = text.find("Do the release.").unwrap();
    assert!(persona < instructions, "{text}");
    assert!(text.starts_with("## Persona\n\nYou are Mira (they/them).\nRole: Release coordinator"));
    assert!(text.contains("- ci-doctor: delegate failing builds"));
    assert!(
        !text.contains("watch CI"),
        "proactive goals are for the heartbeat"
    );
    assert_eq!(with_soul(None, "Do the release."), "Do the release.");
    let goals_only = Soul::admit(Some(
        &json!({"schema": SCHEMA_ID, "proactive_goals": ["x"]}),
    ))
    .unwrap()
    .unwrap();
    assert_eq!(with_soul(Some(&goals_only), "Do it."), "Do it.");
}

#[test]
fn soul_md_round_trips_losslessly() {
    let soul = mira();
    let text = soul.to_markdown();
    assert!(text.starts_with("---\nschema: elitea.soul/v1\n"), "{text}");
    assert!(!text.contains("body:"), "{text}");
    assert_eq!(Soul::from_markdown(&text), Ok(soul));
    let bare = Soul {
        schema: SCHEMA_ID.to_owned(),
        ..Soul::default()
    };
    assert_eq!(Soul::from_markdown(&bare.to_markdown()), Ok(bare));
}

#[test]
fn a_malformed_soul_md_is_refused() {
    assert_eq!(
        Soul::from_markdown("no front matter"),
        Err(SoulError::FrontMatter)
    );
    assert_eq!(
        Soul::from_markdown("---\nschema: elitea.soul/v1\n"),
        Err(SoulError::FrontMatter)
    );
    assert_eq!(
        Soul::from_markdown("---\nschema: elitea.soul/v1\ntools: [x]\n---\n"),
        Err(SoulError::FrontMatter)
    );
    assert_eq!(
        Soul::from_markdown("---\nschema: elitea.soul/v1\nbody: hidden\n---\n"),
        Err(SoulError::Shape)
    );
}
