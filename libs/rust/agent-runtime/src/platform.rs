//! The platform-write vocabulary: what the runtime asks the platform to
//! store on the turn's behalf (ADR-0029 decision 2).
//!
//! These are the request bodies and outcomes of the four `artifact` toolkit
//! operations and the two builder writes (`skills_builder`,
//! `project_context_builder`), moved from the worker's
//! `transport::runtime_context`. The host interface's
//! [`crate::host::PlatformWriter`] carries them: the cloud worker over the
//! claim-bound runtime-context routes, the desktop host over the public API.
//! Field names are main's and main decodes with `DisallowUnknownFields`, so
//! the serialised shape here is the contract.

/// The `skills_builder` request body. Field names are main's
/// (`RuntimeSkillWriteRequest`), and main decodes with
/// `DisallowUnknownFields`, so a key added on one side and not the other fails
/// the write loudly instead of half-applying it.
#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize)]
pub struct SkillWriteRequest {
    pub name: String,
    pub description: String,
    pub instructions: String,
}

/// The `project_context_builder` request body.
///
/// `enabled` is an `Option` and is SKIPPED when absent, not sent as null: main
/// reads absence as "keep whatever the project already had", and a null would
/// decode to the same `nil` only by accident of Go's zero values — sending
/// nothing is the contract both sides can state.
#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize)]
pub struct ProjectContextWriteRequest {
    pub content: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub enabled: Option<bool>,
}

/// The four `artifact` request bodies (#906). Field names are main's
/// (`RuntimeArtifact*Request`), and main decodes with
/// `DisallowUnknownFields`, so a key added on one side and not the other fails
/// the call loudly instead of being silently ignored.
///
/// Every one of them carries a `bucket`, and none of them carries a project:
/// the bucket is resolved INSIDE the claim's project, so naming it widens
/// nothing.
#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize)]
pub struct ArtifactListRequest {
    pub bucket: String,
    pub prefix: String,
    pub recursive: bool,
    pub limit: i32,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize)]
pub struct ArtifactReadRequest {
    pub bucket: String,
    pub name: String,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize)]
pub struct ArtifactWriteRequest {
    pub bucket: String,
    pub name: String,
    pub content: String,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize)]
pub struct ArtifactDeleteRequest {
    pub bucket: String,
    pub name: String,
}

/// One listed artifact.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactFile {
    pub name: String,
    pub byte_length: u64,
    pub media_type: String,
    pub modified_at: String,
}

/// One bucket listing, already proven to belong to the claimed execution's own
/// project.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactListOutcome {
    pub bucket: String,
    pub files: Vec<ArtifactFile>,
    pub truncated: bool,
}

/// One artifact read.
///
/// `over_limit` is the agent-path cap's refusal and it is NOT an error: the
/// content is empty, `char_length` and `byte_length` say how big the file
/// actually is, and `max_chars` says what was allowed. The tool turns the
/// three into the structured `content_too_large` result the SDK worker
/// produces for the same file, so a caller can choose a slice that fits.
///
/// It is deliberately not `Clone`: the bytes are tenant document content and
/// belong in exactly one place, the prompt this turn is building.
pub struct ArtifactReadOutcome {
    pub name: String,
    pub media_type: String,
    pub byte_length: u64,
    pub char_length: u64,
    pub total_lines: u64,
    pub max_chars: u64,
    pub over_limit: bool,
    pub content: String,
}

/// One written artifact.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactWriteOutcome {
    pub bucket: String,
    pub name: String,
    pub media_type: String,
    pub byte_length: u64,
}

/// One deleted artifact.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactDeleteOutcome {
    pub name: String,
    pub deleted: bool,
}

/// One written skill, already proven by main to belong to the claimed
/// execution's own project.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SkillWriteOutcome {
    pub skill_id: String,
    pub name: String,
    pub created: bool,
}

/// One written project context.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ProjectContextWriteOutcome {
    pub content_bytes: u64,
    pub enabled: bool,
    pub created: bool,
}
