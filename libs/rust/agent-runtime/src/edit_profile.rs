//! Which edit tools a model family is offered (#1214): str-replace edits
//! suit Claude, Gemini, Qwen and most others; GPT-5, codex-family and o3/o4
//! models are trained on the Codex `*** Begin Patch` envelope.
//!
//! Shared by the desktop's local tools (`elitea-local-tools`
//! `session::tools_for`) and the cloud executor
//! ([`crate::workspace_tools::workspace_tools_for`]), so both offer the
//! same subset for the same model. Tool names never change between
//! profiles; only which edit tools are offered and `apply_patch`'s
//! description do. A profile follows from the model and the agent's
//! `meta.edit_format`, so it is stable for a thread unless the model
//! changes, and the tool list stays cache-friendly.

/// The edit tools a turn offers.
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub enum EditProfile {
    /// `write_file`, `edit_file`, `multi_edit`, `apply_patch` (described
    /// as a unified diff first).
    #[default]
    StrReplace,
    /// `write_file` and `apply_patch` described as the Codex envelope;
    /// `edit_file` and `multi_edit` are not offered.
    Patch,
}

/// The tools [`EditProfile::Patch`] does not offer.
const STR_REPLACE_TOOLS: [&str; 2] = ["edit_file", "multi_edit"];

impl EditProfile {
    /// The profile a model's name suggests: `gpt-5*`, `*codex*`, `o3*` and
    /// `o4*` (any provider prefix before a `/` ignored, any case) take
    /// [`EditProfile::Patch`]; every other model [`EditProfile::StrReplace`].
    #[must_use]
    pub fn for_model(model_name: &str) -> Self {
        let lower = model_name.trim().to_ascii_lowercase();
        let name = lower.rsplit('/').next().unwrap_or_default();
        if name.starts_with("gpt-5")
            || name.contains("codex")
            || name.starts_with("o3")
            || name.starts_with("o4")
        {
            Self::Patch
        } else {
            Self::StrReplace
        }
    }

    /// The profile of a turn: the agent's `meta.edit_format` (`"auto"`,
    /// `"str_replace"` or `"patch"`) when it names one, else
    /// [`EditProfile::for_model`]. An unknown value is `auto`.
    #[must_use]
    pub fn select(model_name: &str, edit_format: Option<&str>) -> Self {
        match edit_format.map(str::trim) {
            Some("str_replace") => Self::StrReplace,
            Some("patch") => Self::Patch,
            _ => Self::for_model(model_name),
        }
    }

    /// Whether a turn with this profile offers `tool`.
    #[must_use]
    pub fn offers(self, tool: &str) -> bool {
        self == Self::StrReplace || !STR_REPLACE_TOOLS.contains(&tool)
    }

    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            Self::StrReplace => "str_replace",
            Self::Patch => "patch",
        }
    }
}

/// `apply_patch`'s description under [`EditProfile::Patch`]: the Codex
/// envelope's grammar (Codex `codex-rs/apply-patch/src/parser.rs`).
pub const APPLY_PATCH_ENVELOPE_DESCRIPTION: &str = "Edit files with a patch in this format; paths are relative to the workspace root, and a file must be read before it is updated or deleted.\n*** Begin Patch\n*** Add File: <path>\n+<each line of the new file>\n*** Update File: <path>\n*** Move to: <new path>   (optional: rename)\n@@ <a line before the change, e.g. a function header>   (optional anchor; a bare @@ starts another chunk)\n <context line, prefixed with a space>\n-<removed line>\n+<added line>\n*** End of File   (optional: the chunk ends the file)\n*** Delete File: <path>\n*** End Patch\nGive about 3 lines of context around each change and add @@ anchors when context alone is not unique. Every file applies or none does; the result says how each chunk matched. A unified diff is accepted too.";

#[cfg(test)]
mod tests {
    use super::EditProfile;

    #[test]
    fn model_families_pick_their_edit_tools() {
        for patch in [
            "gpt-5",
            "gpt-5.1-mini",
            "GPT-5-Codex",
            "openai/gpt-5.2",
            "codex-mini-latest",
            "azure/my-codex-deploy",
            "o3",
            "o3-mini",
            "o4-mini",
        ] {
            assert_eq!(EditProfile::for_model(patch), EditProfile::Patch, "{patch}");
        }
        for str_replace in [
            "claude-sonnet-4-5",
            "anthropic/claude-opus-4",
            "gemini-2.5-pro",
            "qwen3-coder",
            "gpt-4.1",
            "gpt-4o",
            "",
        ] {
            assert_eq!(
                EditProfile::for_model(str_replace),
                EditProfile::StrReplace,
                "{str_replace}"
            );
        }
    }

    #[test]
    fn meta_edit_format_overrides_the_model() {
        assert_eq!(
            EditProfile::select("claude-sonnet-4-5", Some("patch")),
            EditProfile::Patch
        );
        assert_eq!(
            EditProfile::select("gpt-5", Some("str_replace")),
            EditProfile::StrReplace
        );
        assert_eq!(
            EditProfile::select("gpt-5", Some("auto")),
            EditProfile::Patch
        );
        assert_eq!(
            EditProfile::select("gpt-5", Some("bogus")),
            EditProfile::Patch
        );
        assert_eq!(EditProfile::select("gpt-5", None), EditProfile::Patch);
        assert!(!EditProfile::Patch.offers("multi_edit"));
        assert!(!EditProfile::Patch.offers("edit_file"));
        assert!(EditProfile::Patch.offers("apply_patch"));
        assert!(EditProfile::StrReplace.offers("edit_file"));
    }
}
