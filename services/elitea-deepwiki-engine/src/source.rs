//! What a wiki is named after: the repository string, an artifact-folder
//! source, and the canonical wiki id derived from them.
//!
//! Ports of `elitea_deepwiki.wiki_context.display_repository_for` /
//! `wiki_id_for` and `elitea_deepwiki.artifact_source.parse_artifact_source`.
//! The Go host (`run.WikiIDFor`) and the browser (`toolkitSettings`) carry
//! their own copies; all of them must agree, because a wiki id is an
//! object-key prefix and the key the browser matches a manifest on.
//!
//! ONE RULE FOR AN ARTIFACT FOLDER: its wiki id is the one its generation
//! files it under, [`artifact_wiki_id`] — `artifact--{bucket}--{prefix
//! segments}--{branch}`, `normalize_wiki_id` of the repo identifier. Every
//! reader (the fixture runner, the context paths, a direct `ask`) derives
//! that id, never one of its own. A git repository is unchanged.

use crate::errors::EngineError;
pub use elitea_engine_core::pyvalue::{py_repr, py_str, py_truthy};
pub use elitea_repo_ingest::source::{
    ARTIFACT_SCHEME, ArtifactSource, MAX_KEY_BYTES, fold_unsafe_runs, is_artifact_source,
    parse_artifact_source, refuse_unsafe_key,
};
use serde_json::Value;

/// `str(mapping.get(key) or "")`.
fn str_or_empty(mapping: &serde_json::Map<String, Value>, key: &str) -> String {
    match mapping.get(key) {
        Some(value) if py_truthy(value) => py_str(value),
        _ => String::new(),
    }
}

/// The repository a wiki is NAMED after.
///
/// For a git source it is the repository itself. For an artifact folder it
/// is `{bucket}/{prefix}`: the scheme is dropped because this string becomes
/// the wiki id.
///
/// # Errors
///
/// A `ValueError` when the repository is an unusable artifact source.
pub fn display_repository_for(repo_config: Option<&Value>) -> Result<String, EngineError> {
    let mut repository = String::new();
    if let Some(Value::Object(config)) = repo_config {
        repository = str_or_empty(config, "repository");
        if repository.is_empty()
            && let Some(Value::Object(provider)) = config.get("provider_config")
        {
            repository = str_or_empty(provider, "repository");
        }
    }
    if is_artifact_source(&repository) {
        let source = parse_artifact_source(&repository)?;
        return Ok(if source.prefix.is_empty() {
            source.bucket
        } else {
            format!("{}/{}", source.bucket, source.prefix)
        });
    }
    Ok(repository.trim().trim_matches('/').to_owned())
}

/// The wiki id a generation of `source` on `branch` is filed under:
/// `normalize_wiki_id("artifact://bucket/prefix:{branch}:{sha8}")`, the
/// listing digest left out because it never reaches the id. So
/// `artifact://docs/handbook` on `main` is `artifact--docs--handbook--main`.
#[must_use]
pub fn artifact_wiki_id(source: &ArtifactSource, branch: Option<&str>) -> String {
    use crate::wiki::compose::{build_repo_identifier, normalize_wiki_id};
    let branch = branch.unwrap_or_default();
    normalize_wiki_id(&build_repo_identifier(
        &source.url(),
        branch,
        Some("00000000"),
    ))
}

/// The canonical `{owner}--{repo}--{branch}`; for an artifact folder,
/// [`artifact_wiki_id`].
///
/// # Errors
///
/// A `ValueError` when the repository is an unusable artifact source.
pub fn wiki_id_for(
    repo_config: Option<&Value>,
    branch: Option<&str>,
) -> Result<String, EngineError> {
    if let Some(Value::Object(config)) = repo_config {
        let mut repository = str_or_empty(config, "repository");
        if repository.is_empty()
            && let Some(Value::Object(provider)) = config.get("provider_config")
        {
            repository = str_or_empty(provider, "repository");
        }
        if is_artifact_source(&repository) {
            return Ok(artifact_wiki_id(
                &parse_artifact_source(&repository)?,
                branch,
            ));
        }
    }
    let mut repository = display_repository_for(repo_config)?;
    if repository.is_empty() {
        "fixture/repository".clone_into(&mut repository);
    }
    let branch = branch
        .map(str::trim)
        .filter(|b| !b.is_empty())
        .unwrap_or("main");
    Ok(format!("{}--{branch}", repository.replace('/', "--")))
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::errors::ErrorType;
    use serde_json::json;

    #[test]
    fn a_git_repository_names_itself() {
        let config = json!({"repository": " acme/notes/ "});
        assert_eq!(
            display_repository_for(Some(&config)).ok().as_deref(),
            Some("acme/notes")
        );
        assert_eq!(
            wiki_id_for(Some(&config), Some("dev")).ok().as_deref(),
            Some("acme--notes--dev")
        );
        assert_eq!(
            wiki_id_for(Some(&config), Some("  ")).ok().as_deref(),
            Some("acme--notes--main")
        );
    }

    #[test]
    fn the_provider_block_is_the_fallback() {
        let config = json!({"repository": "", "provider_config": {"repository": "o/r"}});
        assert_eq!(
            display_repository_for(Some(&config)).ok().as_deref(),
            Some("o/r")
        );
    }

    #[test]
    fn no_repository_is_the_fixture_name() {
        assert_eq!(
            wiki_id_for(None, None).ok().as_deref(),
            Some("fixture--repository--main")
        );
    }

    #[test]
    fn an_artifact_folder_drops_its_scheme() {
        let config = json!({"repository": "artifact://Docs/handbook/"});
        assert_eq!(
            display_repository_for(Some(&config)).ok().as_deref(),
            Some("docs/handbook")
        );
        let whole = json!({"repository": "ARTIFACT://docs"});
        assert_eq!(
            display_repository_for(Some(&whole)).ok().as_deref(),
            Some("docs")
        );
    }

    #[test]
    fn an_artifact_folder_has_its_generation_id() {
        for (repository, branch, expected) in [
            (
                "artifact://docs/handbook",
                Some("main"),
                "artifact--docs--handbook--main",
            ),
            (
                "ARTIFACT://Docs/handbook/",
                None,
                "artifact--docs--handbook--main",
            ),
            (
                "artifact://docs",
                Some(" Release/V1 "),
                "artifact--docs--release-v1",
            ),
            (
                "artifact://my-b/a b/c.d_e",
                Some("dev"),
                "artifact--my-b--a-b--c-d-e--dev",
            ),
            // Split on `:` from the right: the head of the branch joins
            // the repository (the browser and the Go host make this split).
            (
                "artifact://docs/handbook",
                Some("v1:rc"),
                "artifact--docs--handbook-v1--rc",
            ),
        ] {
            let config = json!({"repository": repository});
            assert_eq!(
                wiki_id_for(Some(&config), branch).ok().as_deref(),
                Some(expected),
                "{repository}"
            );
        }
        // The same id the generation derives from its repo identifier.
        let source = parse_artifact_source("artifact://docs/handbook").ok();
        let generated =
            crate::wiki::compose::normalize_wiki_id("artifact://docs/handbook:main:1cea09ba");
        assert_eq!(
            source.map(|s| artifact_wiki_id(&s, Some("main"))),
            Some(generated)
        );
        // A git repository is unchanged (no lower-casing, no folding).
        let git = json!({"repository": "Acme/My_Repo"});
        assert_eq!(
            wiki_id_for(Some(&git), Some("Feature/X")).ok().as_deref(),
            Some("Acme--My_Repo--Feature/X")
        );
    }

    #[test]
    fn unsafe_artifact_sources_are_value_errors() {
        for bad in [
            "artifact://x",
            "artifact://docs/a/../b",
            "artifact://docs/a\\b",
            "artifact://1docs",
        ] {
            let config = json!({"repository": bad});
            let error = display_repository_for(Some(&config)).err();
            assert_eq!(error.map(|e| e.error_type), Some(ErrorType::Value), "{bad}");
        }
    }

    /// The Python engine's own wiki ids for every artifact listing in the
    /// ingest fixture (`libs/rust/repo-ingest/tests/fixtures`, written by
    /// `parity/python_artifact_source.py`). The ingest crate checks the
    /// listing and its identity; the NAME it is filed under is this engine's,
    /// and every reader — the generation, `ask`, the context paths — must
    /// derive the same one.
    #[test]
    fn every_reader_derives_the_python_wiki_id_of_a_folder() {
        use crate::wiki::compose::{build_repo_identifier, normalize_wiki_id};
        let fixture: Value = serde_json::from_str(include_str!(
            "../../../libs/rust/repo-ingest/tests/fixtures/artifact_source.json"
        ))
        .unwrap_or_else(|e| panic!("{e}"));
        let text = |value: &Value| value.as_str().unwrap_or_default().to_owned();
        let mut checked = 0;
        for case in fixture["listings"].as_array().into_iter().flatten() {
            if case.get("error").is_some() {
                continue;
            }
            let name = text(&case["name"]);
            let branch = text(&case["branch"]);
            let folder = parse_artifact_source(&text(&case["repository"]))
                .unwrap_or_else(|e| panic!("{name}: {e}"));
            let identifier = build_repo_identifier(
                &text(&case["canonical_repository"]),
                &text(&case["marker"]["branch"]),
                Some(&text(&case["digest"])),
            );
            assert_eq!(identifier, text(&case["repo_identifier"]), "{name}");
            let wiki_id = text(&case["wiki_id"]);
            assert_eq!(normalize_wiki_id(&identifier), wiki_id, "{name}");
            let config = json!({
                "provider_type": "artifact",
                "provider_config": {"bucket": folder.bucket, "prefix": folder.prefix},
                "repository": case["repository"],
                "branch": branch,
                "project": null,
                "is_cloud": null,
            });
            let ask = crate::ask::parse_request(
                json!({"question": "q", "repo_config": config})
                    .as_object()
                    .unwrap_or_else(|| unreachable!()),
            )
            .map(|request| request.wiki_id);
            assert_eq!(ask.ok().as_deref(), Some(wiki_id.as_str()), "{name}");
            assert_eq!(
                wiki_id_for(Some(&config), Some(&branch)).ok().as_deref(),
                Some(wiki_id.as_str()),
                "{name}"
            );
            checked += 1;
        }
        assert!(checked > 0, "the fixture holds no accepted listing");
    }
}
