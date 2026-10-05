//! The Phase 1c feature flags (`feature_flags.py`).
//!
//! Python hard-codes three of the four Phase 1c switches ON
//! (`api_surface_extraction`, `cross_language_linking`,
//! `markdown_structure` take their dataclass defaults; no environment
//! variable reaches them) and reads one from the environment:
//! `DEEPWIKI_TEST_LINKER`, default OFF. The same names and defaults here,
//! so a deployment keeps its environment.
//!
//! Python's `_env_bool` reads `1`/`true`/`yes` as on and ANY other
//! non-empty value as off, so a typo silently disables the pass. This
//! engine strict-parses instead, as `config.rs` does: `1`/`true`/`yes` and
//! `0`/`false`/`no` (any case, surrounding blanks ignored), empty or unset
//! for the default, anything else an error.

/// The variable that switches the test linker on.
pub const TEST_LINKER_ENV: &str = "DEEPWIKI_TEST_LINKER";

/// A flag value that cannot be used.
#[derive(Debug, Clone, thiserror::Error, PartialEq, Eq)]
#[error("{name} must be one of 1/true/yes/0/false/no, got '{value}'")]
pub struct FlagError {
    pub name: String,
    pub value: String,
}

/// Which Phase 1c passes run.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
#[allow(clippy::struct_excessive_bools)] // one switch per pass, as in Python
pub struct Phase1cFlags {
    /// API surfaces per node, then contract nodes (always on in Python).
    pub api_surface_extraction: bool,
    /// The L0..L3 cross-language linker (always on in Python).
    pub cross_language_linking: bool,
    /// Test ↔ production links (`DEEPWIKI_TEST_LINKER`, default off).
    pub test_linker: bool,
    /// Markdown `contains` / `references` edges (always on in Python).
    pub markdown_structure: bool,
}

impl Default for Phase1cFlags {
    fn default() -> Self {
        Self {
            api_surface_extraction: true,
            cross_language_linking: true,
            test_linker: false,
            markdown_structure: true,
        }
    }
}

impl Phase1cFlags {
    /// Every pass off: the Phase 1 graph only.
    #[must_use]
    pub fn none() -> Self {
        Self {
            api_surface_extraction: false,
            cross_language_linking: false,
            test_linker: false,
            markdown_structure: false,
        }
    }

    /// Read the flags through `lookup` (the environment in production).
    ///
    /// # Errors
    ///
    /// A [`FlagError`] for a value that is not a boolean.
    pub fn from_lookup(lookup: impl Fn(&str) -> Option<String>) -> Result<Self, FlagError> {
        Ok(Self {
            test_linker: env_bool(&lookup, TEST_LINKER_ENV, false)?,
            ..Self::default()
        })
    }

    /// Read the flags from the process environment.
    ///
    /// # Errors
    ///
    /// A [`FlagError`] for a value that is not a boolean.
    pub fn from_env() -> Result<Self, FlagError> {
        Self::from_lookup(|name| std::env::var(name).ok())
    }
}

fn env_bool(
    lookup: &impl Fn(&str) -> Option<String>,
    name: &str,
    default: bool,
) -> Result<bool, FlagError> {
    let Some(raw) = lookup(name) else {
        return Ok(default);
    };
    match raw.trim().to_lowercase().as_str() {
        "" => Ok(default),
        "1" | "true" | "yes" => Ok(true),
        "0" | "false" | "no" => Ok(false),
        _ => Err(FlagError {
            name: name.to_owned(),
            value: raw,
        }),
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn flags(value: Option<&str>) -> Result<Phase1cFlags, FlagError> {
        Phase1cFlags::from_lookup(|name| {
            assert_eq!(name, TEST_LINKER_ENV);
            value.map(str::to_owned)
        })
    }

    #[test]
    fn defaults_match_feature_flags_py() {
        let defaults = flags(None).unwrap();
        assert_eq!(defaults, Phase1cFlags::default());
        assert!(defaults.api_surface_extraction);
        assert!(defaults.cross_language_linking);
        assert!(defaults.markdown_structure);
        assert!(!defaults.test_linker);
        assert_eq!(flags(Some("  ")).unwrap(), defaults);
    }

    #[test]
    fn the_test_linker_is_strict_parsed() {
        assert!(flags(Some("1")).unwrap().test_linker);
        assert!(flags(Some(" YES ")).unwrap().test_linker);
        assert!(!flags(Some("false")).unwrap().test_linker);
        let error = flags(Some("on")).unwrap_err();
        assert_eq!(error.name, TEST_LINKER_ENV);
        assert!(error.to_string().contains("'on'"));
    }
}
