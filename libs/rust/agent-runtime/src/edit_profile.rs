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
//!
//! The edit tools' shared pieces live here too (the `multi_edit` and
//! `apply_patch` schemas, the envelope description, and
//! [`EditProfile::select_specs`]), so the two tool tables differ only in
//! their spec types.

use serde_json::{Value, json};

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

    /// The tools of `table` a turn with this profile is offered, in table
    /// order: those it [`offers`](Self::offers), with `apply_patch`
    /// replaced by `envelope` (the Codex-envelope description) under
    /// [`EditProfile::Patch`]. `name` reads a spec's tool name.
    #[must_use]
    pub fn select_specs<'a, T>(
        self,
        table: &'a [T],
        name: impl Fn(&T) -> &str,
        envelope: &'a T,
    ) -> Vec<&'a T> {
        table
            .iter()
            .filter(|spec| self.offers(name(spec)))
            .map(|spec| {
                if self == Self::Patch && name(spec) == APPLY_PATCH {
                    envelope
                } else {
                    spec
                }
            })
            .collect()
    }

    #[must_use]
    pub fn as_str(self) -> &'static str {
        match self {
            Self::StrReplace => "str_replace",
            Self::Patch => "patch",
        }
    }
}

/// The patch tool's name, under every profile.
pub const APPLY_PATCH: &str = "apply_patch";

/// The most edits one `multi_edit` call takes.
pub const MULTI_EDIT_MAX_EDITS: usize = 50;

/// `multi_edit`'s description.
pub const MULTI_EDIT_DESCRIPTION: &str = "Apply several replacements to one file you have read, in one atomic write. Every old_string is matched against the original file (not after earlier edits), must be unique unless replace_all, and must not overlap another; if one fails, nothing is changed. Matching and the result are as for edit_file.";

/// `multi_edit`'s parameters.
#[must_use]
pub fn multi_edit_params() -> Value {
    json!({
        "type": "object",
        "properties": {
            "path": { "type": "string", "description": "Path relative to the workspace root." },
            "edits": {
                "type": "array",
                "minItems": 1,
                "maxItems": MULTI_EDIT_MAX_EDITS,
                "items": {
                    "type": "object",
                    "properties": {
                        "old_string": { "type": "string" },
                        "new_string": { "type": "string" },
                        "replace_all": { "type": "boolean" }
                    },
                    "required": ["old_string", "new_string"]
                }
            }
        },
        "required": ["path", "edits"]
    })
}

/// `apply_patch`'s description under [`EditProfile::StrReplace`]: a unified
/// diff first, the envelope accepted too.
pub const APPLY_PATCH_DESCRIPTION: &str = "Apply a patch to files in the workspace: a unified diff (git diff format), or a `*** Begin Patch` envelope (*** Add File / *** Update File with @@ chunks / *** Delete File / *** Move to). Read each file first. Every file applies or none does; the result says how each hunk matched.";

/// `apply_patch`'s parameters, under every profile.
#[must_use]
pub fn patch_param() -> Value {
    json!({
        "type": "object",
        "properties": { "patch": { "type": "string" } },
        "required": ["patch"]
    })
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

    #[test]
    fn select_specs_drops_str_replace_tools_and_swaps_the_envelope() {
        let table = [
            ("read_file", "read"),
            ("edit_file", "edit"),
            ("multi_edit", "multi"),
            ("apply_patch", "diff"),
        ];
        let envelope = ("apply_patch", "envelope");
        let all = EditProfile::StrReplace.select_specs(&table, |spec| spec.0, &envelope);
        assert_eq!(all.into_iter().copied().collect::<Vec<_>>(), table);
        let patch = EditProfile::Patch.select_specs(&table, |spec| spec.0, &envelope);
        assert_eq!(
            patch.into_iter().copied().collect::<Vec<_>>(),
            [("read_file", "read"), ("apply_patch", "envelope")]
        );
    }
}
