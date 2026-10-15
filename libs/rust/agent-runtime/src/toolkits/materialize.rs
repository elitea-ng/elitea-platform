//! Claim-materialized configured toolsets for a direct ADK `LlmAgent`.
//!
//! Main freezes toolkit identity and redeems schema-declared secrets before the
//! worker receives the request. This boundary consumes only that immutable
//! request snapshot, applies one deployment-policy generation, and constructs
//! native ADK toolsets. MCP and nested applications have separate authority
//! owners and therefore never fall through this map.
//!
//! The `artifact` family (#906) is the one entry here whose authority is NOT in
//! the snapshot: there is no third-party endpoint and no credential to redeem,
//! only this platform's own storage reached under the live execution CLAIM. So
//! it is materialized only when a caller LENDS that claim — see
//! `materialize_configured_toolsets_with_artifact_authority` — and where none
//! is in scope (pipelines, nested applications) it stays exactly what it was:
//! skipped with `agent_toolkit_skipped`, which is the honest answer for a
//! runtime that cannot serve it there.
//!
//! The index tools (ADR-0030 I1) are the second lent authority: for an
//! indexing family whose frozen toolkit carries Main's `indexes` catalog, the
//! toolset is composed of the family's tools and the five index tools, acting
//! under an `IndexToolAuthority` the host lends
//! (`materialize_configured_toolsets_with_authorities`). No catalog (the
//! Python indexing path) means no index tools; a catalog with the authority
//! withheld ([`IndexToolSupport::Withheld`]) means they are left out, logged
//! with the reason the host states. Only where the index toolset IS composed
//! is an index-only `selected_tools` admissible — it then selects none of the
//! family's own tools and the family is not built at all; elsewhere every
//! family refuses it as the unsupported selection it always was.

use std::fmt;
use std::sync::Arc;

use adk_core::Toolset;

use super::DelegatedAuthorizationCatalog;
use super::families::UnsupportedSetting;
use super::families::artifact::ArtifactToolAuthority;
use super::families::attachment_host::AttachmentHost;
use super::families::bigquery;
use super::families::confluence;
use super::families::index_tools::{
    CompositeToolset, IndexCatalog, IndexToolSupport, IndexToolsWithheld, IndexToolsetErrorCode,
    build_index_toolset, is_index_family, selection_is_index_only,
};
use super::families::jira;
use super::families::memory::{MemoryToolName, MemoryToolsetError, build_memory_toolset};
use super::families::qtest;
#[cfg(feature = "toolkit-sql")]
use super::families::sql;
use super::families::testio;
use super::families::testrail;
use super::families::xray_cloud;
use super::families::{
    aha, artifact, azure, azure_search, bitbucket, carrier, elastic, figma, gcp, github, gitlab,
    gitlab_org, google_places, keycloak, kubernetes, openapi, postman, rally, report_portal,
    salesforce, service_now, sharepoint, slack, sonar, yagmail, zephyr, zephyr_squad,
};
#[cfg(feature = "toolkit-office")]
use super::families::{docx, pptx, xlsx};
use super::family_error::family_error;
use super::policy::ToolAdmissionPolicy;
use super::snapshot::{AdmittedToolSnapshot, FrozenToolKind, FrozenToolReference};

const MAX_AGENT_TOOLSETS: usize = 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ToolsetMaterializationErrorCode {
    InvalidConfiguration,
    UnsupportedToolkit,
    ResourceExhausted,
    DependencyUnavailable,
}

#[derive(Clone, Copy)]
pub struct ToolsetMaterializationError {
    code: ToolsetMaterializationErrorCode,
    /// Set when the toolkit's configuration asks for a setting this runtime
    /// deliberately refuses (#1207 review round 2). Within one
    /// materialization pass such a toolkit is collected as a
    /// [`LeftOutToolkit`]; a caller that cannot report one receives it as an
    /// `UnsupportedToolkit` error carrying the reason.
    refusal: Option<UnsupportedSetting>,
}

impl ToolsetMaterializationError {
    pub(super) const fn new(code: ToolsetMaterializationErrorCode) -> Self {
        Self {
            code,
            refusal: None,
        }
    }

    pub(super) const fn refused(setting: UnsupportedSetting) -> Self {
        Self {
            code: ToolsetMaterializationErrorCode::UnsupportedToolkit,
            refusal: Some(setting),
        }
    }

    #[must_use]
    pub const fn code(self) -> ToolsetMaterializationErrorCode {
        self.code
    }

    /// The refused setting, as a sentence that carries no configuration
    /// value; `None` for every other failure.
    #[must_use]
    pub const fn refusal_reason(self) -> Option<&'static str> {
        match self.refusal {
            Some(setting) => Some(setting.reason()),
            None => None,
        }
    }
}

impl fmt::Debug for ToolsetMaterializationError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("ToolsetMaterializationError")
            .field("code", &self.code)
            .field("refusal", &self.refusal)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for ToolsetMaterializationError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        if let Some(setting) = self.refusal {
            formatter.write_str("the frozen toolkit configuration is refused: ")?;
            return formatter.write_str(setting.reason());
        }
        formatter.write_str(match self.code {
            ToolsetMaterializationErrorCode::DependencyUnavailable => {
                "the toolkit specification could not be retrieved"
            }
            ToolsetMaterializationErrorCode::InvalidConfiguration => {
                "the frozen toolkit configuration is invalid"
            }
            ToolsetMaterializationErrorCode::UnsupportedToolkit => {
                "the frozen toolkit requires a runtime authority that is not available"
            }
            ToolsetMaterializationErrorCode::ResourceExhausted => {
                "the frozen toolkit set exceeds its approved limit"
            }
        })
    }
}

impl std::error::Error for ToolsetMaterializationError {}

/// A configured toolkit left out of the agent, wholly or but for its index
/// tools: its configuration asks for a setting this runtime refuses (#1207
/// review round 2), or its family is not ported.
///
/// Before, such a toolkit was skipped exactly like an unported family — one
/// warning in the worker log — while the capability snapshot listed it as
/// supported, so the user saw an enabled toolkit that did nothing. The
/// ordinary agent path now names every refused toolkit in the run's opening
/// notice ([`left_out_toolkits_notice_text`]), in the same place and shape as
/// the skipped internal tools (#866) and application children (#973).
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct LeftOutToolkit {
    toolkit_name: String,
    toolkit_type: String,
    /// Set when only this one tool of an otherwise built toolkit was left
    /// out (`unsupported_tool_selection`): the toolkit's other selected tools
    /// are served.
    tool_name: Option<String>,
    cause: LeftOutReason,
    reason: &'static str,
}

/// Why a configured toolkit (or part of it) was left out of the run.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum LeftOutReason {
    /// The toolkit's family is not served by this runtime at all.
    UnsupportedFamily,
    /// The family is not served, but the toolkit's index tools are: only they
    /// are available.
    FamilyNotPortedIndexToolsKept,
    /// The configuration asks for a setting this runtime refuses.
    UnsupportedSetting,
    /// The toolkit is built, but one tool of its stored selection is not
    /// served by this runtime (not ported, or not available on this path).
    ToolNotServed,
    /// A tool of the stored selection is blocked by the deployment's policy.
    ToolBlockedByPolicy,
}

impl LeftOutReason {
    /// Stable machine code, carried in the structured run notice.
    #[must_use]
    pub const fn code(self) -> &'static str {
        match self {
            Self::UnsupportedFamily => "unsupported_toolkit_family",
            Self::FamilyNotPortedIndexToolsKept => "index_tools_only",
            Self::UnsupportedSetting => "unsupported_setting",
            Self::ToolNotServed => "unsupported_tool_selection",
            Self::ToolBlockedByPolicy => "tool_blocked_by_policy",
        }
    }
}

/// What the materializer found for a toolkit it did not fully build.
#[derive(Clone, Copy)]
enum ToolkitSkip<'a> {
    Setting(UnsupportedSetting),
    UnsupportedFamily {
        index_tools_kept: bool,
    },
    Tool {
        name: &'a str,
        cause: LeftOutReason,
        reason: &'static str,
    },
}

impl LeftOutToolkit {
    fn new(reference: &FrozenToolReference<'_>, skip: ToolkitSkip<'_>) -> Self {
        let mut tool_name = None;
        let (cause, reason) = match skip {
            ToolkitSkip::Tool {
                name,
                cause,
                reason,
            } => {
                tool_name = Some(bounded_label(name));
                (cause, reason)
            }
            ToolkitSkip::Setting(setting) => (LeftOutReason::UnsupportedSetting, setting.reason()),
            ToolkitSkip::UnsupportedFamily {
                index_tools_kept: false,
            } => (
                LeftOutReason::UnsupportedFamily,
                "this toolkit type is not available on this worker yet, so its tools were left out",
            ),
            ToolkitSkip::UnsupportedFamily {
                index_tools_kept: true,
            } => (
                LeftOutReason::FamilyNotPortedIndexToolsKept,
                "only its index tools are available (its other tools are not ported)",
            ),
        };
        Self {
            toolkit_name: bounded_label(reference.toolkit_name()),
            toolkit_type: bounded_label(reference.tool_type()),
            tool_name,
            cause,
            reason,
        }
    }

    #[cfg(test)]
    pub(crate) fn fixture(
        name: &str,
        kind: &str,
        cause: LeftOutReason,
        reason: &'static str,
    ) -> Self {
        Self {
            toolkit_name: bounded_label(name),
            toolkit_type: bounded_label(kind),
            tool_name: None,
            cause,
            reason,
        }
    }

    /// One tool of a built toolkit that the runtime does not serve.
    #[must_use]
    pub fn unserved_tool(toolkit: &str, kind: &str, tool: &str, reason: &'static str) -> Self {
        Self {
            toolkit_name: bounded_label(toolkit),
            toolkit_type: bounded_label(kind),
            tool_name: Some(bounded_label(tool)),
            cause: LeftOutReason::ToolNotServed,
            reason,
        }
    }

    /// The one tool left out, when only a tool (not the toolkit) was.
    #[must_use]
    pub fn tool_name(&self) -> Option<&str> {
        self.tool_name.as_deref()
    }

    /// Machine-readable cause, for the structured run notice.
    #[must_use]
    pub const fn reason_code(&self) -> &'static str {
        self.cause.code()
    }

    #[must_use]
    pub const fn cause(&self) -> LeftOutReason {
        self.cause
    }

    #[must_use]
    pub fn toolkit_name(&self) -> &str {
        &self.toolkit_name
    }

    #[must_use]
    pub fn toolkit_type(&self) -> &str {
        &self.toolkit_type
    }

    #[must_use]
    pub const fn reason(&self) -> &'static str {
        self.reason
    }
}

/// The longest label (toolkit name or type, attached agent name) a notice
/// repeats, in BYTES. They come from stored rows the runtime does not
/// otherwise bound, and a notice rides a size-capped node event.
pub const MAX_NOTICE_LABEL_BYTES: usize = 128;

/// A stored label made safe to repeat in a notice: control characters
/// removed, cut at [`MAX_NOTICE_LABEL_BYTES`] on a character boundary. The one
/// bound every notice in the worker shares.
#[must_use]
pub fn bounded_label(value: &str) -> String {
    let mut out = String::new();
    for character in value.chars().filter(|character| !character.is_control()) {
        if out.len() + character.len_utf8() > MAX_NOTICE_LABEL_BYTES {
            break;
        }
        out.push(character);
    }
    out
}

/// Join already-sorted, deduplicated notice lines, keeping at most `max` and
/// ALWAYS saying how many were not shown, so a long list never reads as the
/// whole list.
#[must_use]
pub fn capped_notice_lines(mut lines: Vec<String>, max: usize) -> String {
    let hidden = lines.len().saturating_sub(max);
    lines.truncate(max);
    if hidden > 0 {
        lines.push(format!("({hidden} more not shown)"));
    }
    lines.join("\n")
}

/// How many leading items of each list a capped notice keeps: the lists are
/// taken round-robin (rank 0 of every list, then rank 1, ...) while fewer
/// than `max_items` are kept and the next item's `cost` still fits in
/// `max_cost`, so one long list cannot push the others out. It stops at the
/// first item that does not fit. Shared by the model notice
/// (`left_out_toolkits_notice_text`: a line cap) and the worker's structured
/// chat notice (`skipped_capabilities`: an entry cap and a byte budget).
#[must_use]
pub fn round_robin_take<T>(
    lists: &[Vec<T>],
    max_items: usize,
    max_cost: usize,
    mut cost: impl FnMut(&T) -> usize,
) -> Vec<usize> {
    let mut kept = vec![0; lists.len()];
    let (mut items, mut spent) = (0usize, 0usize);
    let mut rank = 0;
    loop {
        let mut took = false;
        for (index, list) in lists.iter().enumerate() {
            let Some(item) = list.get(rank) else {
                continue;
            };
            let size = cost(item);
            if items == max_items || spent.saturating_add(size) > max_cost {
                return kept;
            }
            items += 1;
            spent += size;
            kept[index] += 1;
            took = true;
        }
        if !took {
            return kept;
        }
        rank += 1;
    }
}

/// Most lines a model-visible toolkit notice lists.
const MAX_NOTICE_LINES: usize = 32;

/// One deterministic line per toolkit left out, naming the toolkit and the
/// reason; `None` when nothing was left out. The set decides the text, never
/// the order the version listed the toolkits in.
#[must_use]
pub fn left_out_toolkits_notice_text(left_out: &[LeftOutToolkit]) -> Option<String> {
    // Whole-toolkit lines come first, then per-tool lines; when the cap cuts
    // the list, the two kinds are taken round-robin (as the chat notice
    // does), so many omitted tools cannot push a whole toolkit out. A tool
    // blocked by the deployment's policy is NOT named to the model: the
    // notice never listed blocked names, and still does not.
    let mut toolkits = Vec::new();
    let mut tools = Vec::new();
    for toolkit in left_out {
        if let Some(tool) = &toolkit.tool_name {
            if toolkit.cause == LeftOutReason::ToolBlockedByPolicy {
                continue;
            }
            tools.push(format!(
                "tool '{}.{}' ({}) is not enabled on this worker: {}",
                toolkit.toolkit_name, tool, toolkit.toolkit_type, toolkit.reason
            ));
        } else {
            let state = if toolkit.cause == LeftOutReason::FamilyNotPortedIndexToolsKept {
                "only partly enabled"
            } else {
                "not enabled"
            };
            toolkits.push(format!(
                "toolkit '{}' ({}) is {state} on this worker: {}",
                toolkit.toolkit_name, toolkit.toolkit_type, toolkit.reason
            ));
        }
    }
    for lines in [&mut toolkits, &mut tools] {
        lines.sort_unstable();
        lines.dedup();
    }
    let total = toolkits.len() + tools.len();
    if total == 0 {
        return None;
    }
    let lists = [toolkits, tools];
    let kept = round_robin_take(&lists, MAX_NOTICE_LINES, usize::MAX, |_| 0);
    let [toolkits, tools] = lists;
    let mut lines: Vec<String> = toolkits.into_iter().take(kept[0]).collect();
    lines.extend(tools.into_iter().take(kept[1]));
    let hidden = total - lines.len();
    if hidden > 0 {
        lines.push(format!("({hidden} more not shown)"));
    }
    Some(lines.join("\n"))
}

/// The configured toolsets one pass built, and the toolkits it refused.
pub struct ConfiguredToolsets {
    pub toolsets: Vec<Arc<dyn Toolset>>,
    pub delegated_authorization: DelegatedAuthorizationCatalog,
    /// Toolkits left out for a refused setting, for the caller to report.
    pub left_out: Vec<LeftOutToolkit>,
}

pub async fn materialize_configured_toolsets_with_tokens_and_authorization(
    snapshot: &AdmittedToolSnapshot<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
) -> Result<(Vec<Arc<dyn Toolset>>, DelegatedAuthorizationCatalog), ToolsetMaterializationError> {
    materialize_configured_toolsets_with_artifact_authority(
        snapshot,
        policy,
        delegated_tokens,
        None,
    )
    .await
}

/// The same materialization, with the live execution claim LENT to the one
/// family that needs it.
///
/// `artifacts` is an `Option` rather than a required argument because only the
/// ordinary agent path holds a claim that outlives assembly. A caller with
/// none passes `None` and gets exactly what it got before #906: an artifact
/// toolkit skipped with a warning naming it. That is deliberately not an
/// error — the toolkit is a real capability of the product that this runtime
/// cannot serve in that position, and refusing the whole profile would turn
/// one unavailable tool into an agent that stops answering.
///
/// These callers (pipelines, nested applications, direct tool calls) have no
/// opening notice to name a refused toolkit in, so a refused setting is an
/// `UnsupportedToolkit` ERROR here, carrying the reason — visible as
/// "configuration type is not supported" rather than a toolkit that silently
/// does nothing. The ordinary agent path uses
/// [`materialize_configured_toolsets_reporting_refusals`] instead.
pub async fn materialize_configured_toolsets_with_artifact_authority(
    snapshot: &AdmittedToolSnapshot<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<(Vec<Arc<dyn Toolset>>, DelegatedAuthorizationCatalog), ToolsetMaterializationError> {
    materialize_configured_toolsets_with_authorities(
        snapshot,
        policy,
        delegated_tokens,
        artifacts,
        IndexToolSupport::Withheld(IndexToolsWithheld::NotServedOnThisPath),
    )
    .await
}

/// The same materialization, with both lent authorities: the claim for the
/// `artifact` family and the index tools' host for the indexing families
/// (ADR-0030 I1). Either may be withheld, with the consequences each family
/// documents: no artifact family (unless its index tools are composed, which
/// then stand alone), or no index tools.
pub async fn materialize_configured_toolsets_with_authorities(
    snapshot: &AdmittedToolSnapshot<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: Option<&ArtifactToolAuthority>,
    index_tools: IndexToolSupport<'_>,
) -> Result<(Vec<Arc<dyn Toolset>>, DelegatedAuthorizationCatalog), ToolsetMaterializationError> {
    let mut refusal = None;
    let materialized = materialize_all(
        snapshot,
        policy,
        delegated_tokens,
        artifacts,
        index_tools,
        &mut |_, skip| {
            // An unported family stays a quiet skip here: these callers have
            // no run notice, and refusing the profile would stop the agent.
            if let ToolkitSkip::Setting(setting) = skip {
                refusal.get_or_insert(ToolsetMaterializationError::refused(setting));
            }
        },
    )
    .await?;
    if let Some(error) = refusal {
        return Err(error);
    }
    Ok((materialized.toolsets, materialized.delegated_authorization))
}

/// The ordinary agent path: build every configured toolkit, and REPORT the
/// ones refused for a setting this runtime does not honour instead of
/// failing the agent, so the run can name them
/// ([`left_out_toolkits_notice_text`]).
pub async fn materialize_configured_toolsets_reporting_refusals(
    snapshot: &AdmittedToolSnapshot<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: Option<&ArtifactToolAuthority>,
    index_tools: IndexToolSupport<'_>,
) -> Result<ConfiguredToolsets, ToolsetMaterializationError> {
    let mut refused = Vec::new();
    let mut materialized = materialize_all(
        snapshot,
        policy,
        delegated_tokens,
        artifacts,
        index_tools,
        &mut |reference, skip| refused.push(LeftOutToolkit::new(reference, skip)),
    )
    .await?;
    materialized.left_out = refused;
    Ok(materialized)
}

async fn materialize_all(
    snapshot: &AdmittedToolSnapshot<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: Option<&ArtifactToolAuthority>,
    index_tools: IndexToolSupport<'_>,
    on_refused: &mut (dyn FnMut(&FrozenToolReference<'_>, ToolkitSkip<'_>) + Send),
) -> Result<ConfiguredToolsets, ToolsetMaterializationError> {
    if snapshot.len() > MAX_AGENT_TOOLSETS {
        return Err(resource_exhausted());
    }
    let mut toolsets = Vec::new();
    let mut delegated_authorization = DelegatedAuthorizationCatalog::default();
    for reference in snapshot
        .iter()
        .filter(|reference| reference.kind() == FrozenToolKind::Configured)
    {
        // The `memory` toolkit (unit M9) has no family half: its authority is
        // the lent vector token, like the index tools.
        // (Spelled as a literal: the Go drift check reads this dispatch.)
        if reference.tool_type() == "memory" {
            if let Some(toolset) = memory_toolset_for(reference, policy, index_tools, on_refused)? {
                toolsets.push(Arc::new(toolset) as Arc<dyn Toolset>);
                report_unserved_selected_tools(reference, policy, artifacts, false, on_refused);
            }
            continue;
        }
        let index_toolset = index_toolset_for(reference, policy, index_tools)?;
        // With the index toolset composed, two toolkits have no family half
        // to build: one whose stored selection names index tools only (none
        // of the family's own tools are selected, so the family would refuse
        // the selection), and an `artifact` toolkit on a path that lends no
        // artifact authority (the family is skipped there; its index tools
        // are not). Without the index toolset both keep their old answer.
        //
        // A family half that is REFUSED (an unsupported setting, reported to
        // the run) or SKIPPED (a selection or family this runtime does not
        // serve) is left out alone: the toolkit's index tools, which depend
        // on none of the family's settings, are still served (review round
        // 2). Without an index toolset such a toolkit is omitted as before.
        // Whether the family half was built or refused for a setting: either
        // way the stored selection's unserved tools are named in the notice.
        let mut report_selection = false;
        let family = if index_toolset.is_some() && index_tools_stand_alone(reference, artifacts)? {
            None
        } else {
            match materialize(reference, policy, delegated_tokens, artifacts).await {
                Ok((toolset, authorization)) => {
                    delegated_authorization
                        .merge(authorization)
                        .map_err(|()| invalid_configuration())?;
                    report_selection = true;
                    Some(toolset)
                }
                Err(ToolsetMaterializationError {
                    refusal: Some(setting),
                    ..
                }) => {
                    report_selection = true;
                    tracing::warn!(
                        event = "agent_toolkit_refused",
                        reason_code = "unsupported_setting",
                        toolkit_type = reference.tool_type(),
                        toolkit_id = reference.tool_id(),
                        reason = setting.reason(),
                        index_tools_kept = index_toolset.is_some(),
                        "agent toolkit configuration asks for a setting this runtime refuses; the toolkit's own tools were left out"
                    );
                    on_refused(reference, ToolkitSkip::Setting(setting));
                    None
                }
                Err(error)
                    if error.code() == ToolsetMaterializationErrorCode::UnsupportedToolkit =>
                {
                    tracing::warn!(
                        event = "agent_toolkit_skipped",
                        reason_code = "unsupported_toolkit_family",
                        toolkit_type = reference.tool_type(),
                        toolkit_id = reference.tool_id(),
                        index_tools_kept = index_toolset.is_some(),
                        "agent toolkit is unavailable in this runtime and its own tools were omitted from the native toolsets"
                    );
                    on_refused(
                        reference,
                        ToolkitSkip::UnsupportedFamily {
                            index_tools_kept: index_toolset.is_some(),
                        },
                    );
                    None
                }
                Err(error) => return Err(error),
            }
        };
        if report_selection {
            report_unserved_selected_tools(
                reference,
                policy,
                artifacts,
                index_toolset.is_some(),
                on_refused,
            );
        }
        let toolset: Arc<dyn Toolset> = match (family, index_toolset) {
            (Some(family), Some(index)) => Arc::new(CompositeToolset::new(
                reference.toolkit_name(),
                vec![family, Arc::new(index)],
            )),
            (Some(family), None) => family,
            (None, Some(index)) => Arc::new(CompositeToolset::new(
                reference.toolkit_name(),
                vec![Arc::new(index)],
            )),
            (None, None) => continue,
        };
        toolsets.push(toolset);
    }
    Ok(ConfiguredToolsets {
        toolsets,
        delegated_authorization,
        left_out: Vec::new(),
    })
}

/// The `memory` toolkit's toolset when the host lends an authority that
/// serves memory; `None` otherwise, logged with the reason (the toolkit is
/// then skipped, as it was before the family existed).
fn memory_toolset_for(
    reference: &FrozenToolReference<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    index_tools: IndexToolSupport<'_>,
    on_refused: &mut (dyn FnMut(&FrozenToolReference<'_>, ToolkitSkip<'_>) + Send),
) -> Result<Option<adk_tool::BasicToolset>, ToolsetMaterializationError> {
    let authority = match index_tools {
        IndexToolSupport::Lent(authority) if authority.host().serves_memory() => authority,
        IndexToolSupport::Lent(_) => {
            tracing::warn!(
                event = "agent_toolkit_skipped",
                reason_code = "no_memory_embedding_model",
                toolkit_type = reference.tool_type(),
                toolkit_id = reference.tool_id(),
                "the memory toolkit needs the deployment's memory embedding model (runtime.json memory_embedding_model); it was skipped"
            );
            return Ok(None);
        }
        IndexToolSupport::Withheld(reason) => {
            if reason.is_warning() {
                tracing::warn!(
                    event = "agent_toolkit_skipped",
                    reason_code = reason.reason_code(),
                    toolkit_type = reference.tool_type(),
                    toolkit_id = reference.tool_id(),
                    "the memory toolkit needs the vector store and the claim's vector token; it was skipped"
                );
            } else {
                tracing::info!(
                    event = "agent_toolkit_skipped",
                    reason_code = reason.reason_code(),
                    toolkit_type = reference.tool_type(),
                    toolkit_id = reference.tool_id(),
                    "the memory toolkit is not served on this path; it was skipped"
                );
            }
            return Ok(None);
        }
    };
    let settings = reference.settings().ok_or_else(invalid_configuration)?;
    build_memory_toolset(
        reference.toolkit_name(),
        reference.tool_id(),
        settings,
        policy,
        authority,
    )
    .map(Some)
    .or_else(|error| {
        // A setting the user can fix skips THIS toolkit with a visible
        // reason (the run's notice names it); it never fails the run.
        let setting = match error {
            MemoryToolsetError::InvalidNamespace => UnsupportedSetting::MemoryNamespace,
            MemoryToolsetError::InvalidScope => UnsupportedSetting::MemoryScope,
            MemoryToolsetError::InvalidSelection => UnsupportedSetting::MemorySelection,
            MemoryToolsetError::Invalid => return Err(invalid_configuration()),
            MemoryToolsetError::ResourceExhausted => return Err(resource_exhausted()),
        };
        tracing::warn!(
            event = "agent_toolkit_refused",
            reason_code = "invalid_memory_setting",
            toolkit_type = reference.tool_type(),
            toolkit_id = reference.tool_id(),
            reason = setting.reason(),
            "the memory toolkit's settings are malformed; the toolkit was left out"
        );
        on_refused(reference, ToolkitSkip::Setting(setting));
        Ok(None)
    })
}

const REASON_NOT_PORTED: &str = "this tool is not available on this worker yet, so it was left out";
const REASON_NEEDS_FILE_STORE: &str =
    "this tool needs the platform file store, which this run does not lend, so it was left out";
const REASON_INDEX_RUN: &str =
    "starting an index run is not a chat tool on this worker; use the toolkit's index action";
const REASON_INDEX_TOOLS: &str = "the index tools are not available for this toolkit on this path";
const REASON_POLICY: &str = "this tool is blocked by this deployment's policy";

/// The stored selection's tools a family does not serve, for the run's
/// notice: `(name, needs_file_store)`. `catalog` is the family's whole served
/// list with, per tool, whether it needs the run's file store; a selected name
/// outside it is not ported, and one that needs the file store is omitted when
/// the run lends none. ONE rule for every family (review round 2: it was eight
/// per-family copies).
fn omitted_selection(
    catalog: &[(&'static str, bool)],
    selected: &[Box<str>],
    file_store: bool,
) -> Vec<(String, bool)> {
    selected
        .iter()
        .filter_map(
            |name| match catalog.iter().find(|(served, _)| *served == name.as_ref()) {
                None => Some((name.to_string(), false)),
                Some((_, true)) if !file_store => Some((name.to_string(), true)),
                Some(_) => None,
            },
        )
        .collect()
}

/// The served tool list of a configured family, from the family's own tool
/// list (so it cannot drift from what the family builds); `None` for a type
/// this runtime does not build here (`openapi`, whose tools are the stored
/// spec's operations, and any unported family). Live 4.45.0: the Office
/// families had none, so a selected tool they did not serve vanished with no
/// notice. `materialize_tests::every_served_family_reports_an_unknown_selected_tool`
/// fails for the next family added without one.
#[allow(
    clippy::too_many_lines,
    reason = "one dispatch table over the toolkit families; each arm is a call"
)]
fn family_tool_catalog(tool_type: &str) -> Option<Vec<(&'static str, bool)>> {
    use super::families::{
        ado_boards, ado_plans, ado_repos, ado_wiki, imagegen, zephyr_enterprise, zephyr_essential,
        zephyr_scale,
    };
    Some(match tool_type {
        "ado_boards" => ado_boards::tools::tool_catalog(),
        "ado_plans" => ado_plans::tools::tool_catalog(),
        "ado_repos" => ado_repos::tools::tool_catalog(),
        "ado_wiki" => ado_wiki::tools::tool_catalog(),
        "aha" => aha::tools::tool_catalog(),
        "artifact" => artifact::tools::tool_catalog(),
        "azure" => azure::tools::tool_catalog(),
        "azure_search" => azure_search::tools::tool_catalog(),
        "bigquery" => bigquery::tools::tool_catalog(),
        "bitbucket" => bitbucket::tools::tool_catalog(),
        "carrier" => carrier::tools::tool_catalog(),
        "confluence" => confluence::tools::tool_catalog(),
        #[cfg(feature = "toolkit-office")]
        "docx" => docx::schema::tool_catalog(),
        "elastic" => elastic::tools::tool_catalog(),
        "figma" => figma::tools::tool_catalog(),
        "gcp" => gcp::tools::tool_catalog(),
        "github" => github::tools::tool_catalog(),
        "gitlab" => gitlab::tools::tool_catalog(),
        "gitlab_org" => gitlab_org::tools::tool_catalog(),
        "google_places" => google_places::tools::tool_catalog(),
        "imagegen" => imagegen::tool_catalog(),
        "jira" => jira::tools::tool_catalog(),
        "k8s" => kubernetes::tools::tool_catalog(),
        "keycloak" => keycloak::tools::tool_catalog(),
        "memory" => MemoryToolName::ALL
            .iter()
            .map(|tool| (tool.as_str(), false))
            .collect(),
        "postman" => postman::tools::tool_catalog(),
        #[cfg(feature = "toolkit-office")]
        "pptx" => pptx::schema::tool_catalog(),
        "qtest" => qtest::tools::tool_catalog(),
        "rally" => rally::tools::tool_catalog(),
        "report_portal" => report_portal::tools::tool_catalog(),
        "salesforce" => salesforce::tools::tool_catalog(),
        "service_now" => service_now::tools::tool_catalog(),
        "sharepoint" => sharepoint::tools::tool_catalog(),
        "slack" => slack::tools::tool_catalog(),
        "sonar" => sonar::tools::tool_catalog(),
        #[cfg(feature = "toolkit-sql")]
        "sql" => sql::tools::tool_catalog(),
        "testio" => testio::tools::tool_catalog(),
        "testrail" => testrail::tools::tool_catalog(),
        #[cfg(feature = "toolkit-office")]
        "xlsx" => xlsx::schema::tool_catalog(),
        "xray_cloud" => xray_cloud::tools::tool_catalog(),
        "yagmail" => yagmail::tools::tool_catalog(),
        "zephyr" => zephyr::tools::tool_catalog(),
        "zephyr_enterprise" => zephyr_enterprise::tools::tool_catalog(),
        "zephyr_essential" => zephyr_essential::tools::tool_catalog(),
        "zephyr_scale" => zephyr_scale::tools::tool_catalog(),
        "zephyr_squad" => zephyr_squad::tools::tool_catalog(),
        _ => return None,
    })
}

/// The stored settings with the selected names the family does not serve
/// removed, when at least one served name remains; `None` when nothing
/// needs removing (or nothing would remain, which the family itself answers
/// as before: an empty selection must never widen to "every tool").
///
/// Several families refuse a selection naming any tool they do not serve
/// (`slack`, `postman`, ...): one stale or SDK-only name then left the whole
/// toolkit out with no notice. Pruned here, the toolkit is built with the
/// tools it does serve and each removed name is reported by
/// [`report_unserved_selected_tools`]. Index tools are kept: they belong to
/// the composed index toolset, and every family already knows them.
fn prune_unserved_selection(
    reference: &FrozenToolReference<'_>,
    settings: &serde_json::Map<String, serde_json::Value>,
) -> Option<serde_json::Map<String, serde_json::Value>> {
    let catalog = family_tool_catalog(reference.tool_type())?;
    let names = settings.get("selected_tools")?.as_array()?;
    let served = |name: &str| {
        catalog.iter().any(|(tool, _)| *tool == name)
            || super::families::index_tools::is_sdk_index_tool_name(name)
    };
    let mut kept = Vec::with_capacity(names.len());
    for value in names {
        // A malformed entry is the family's to refuse, as before.
        let name = value.as_str()?;
        if served(name.trim()) {
            kept.push(value.clone());
        }
    }
    if kept.len() == names.len()
        || !kept
            .iter()
            .filter_map(serde_json::Value::as_str)
            .any(|name| !super::families::index_tools::is_sdk_index_tool_name(name.trim()))
    {
        return None;
    }
    let mut pruned = settings.clone();
    pruned.insert("selected_tools".to_owned(), serde_json::Value::Array(kept));
    Some(pruned)
}

/// Report each tool of the stored selection that the built toolkit does not
/// serve, and each one the deployment's policy blocks. Before this the only
/// trace was one worker log line with COUNTS (`selected_count=5
/// materialized_count=2`): the person who enabled five tools saw an agent
/// with two, and nobody could say which three.
///
/// Run by the reporting materializer ONLY; other callers (direct, pipeline)
/// neither collect nor compute it. Index tools the composed index toolset
/// serves are not omissions.
fn report_unserved_selected_tools(
    reference: &FrozenToolReference<'_>,
    policy: &ToolAdmissionPolicy,
    artifacts: Option<&ArtifactToolAuthority>,
    index_tools_composed: bool,
    on_refused: &mut (dyn FnMut(&FrozenToolReference<'_>, ToolkitSkip<'_>) + Send),
) {
    let selected: Vec<Box<str>> = reference
        .settings()
        .and_then(|settings| settings.get("selected_tools"))
        .and_then(serde_json::Value::as_array)
        .map(|names| {
            names
                .iter()
                .filter_map(serde_json::Value::as_str)
                .map(Box::from)
                .collect()
        })
        .unwrap_or_default();
    let file_store = artifacts.and_then(ArtifactToolAuthority::office).is_some();
    let Some(catalog) = family_tool_catalog(reference.tool_type()) else {
        return;
    };
    let unserved = omitted_selection(&catalog, &selected, file_store);
    let mut omitted: Vec<(String, LeftOutReason, &'static str)> = Vec::new();
    for (name, needs_file_store) in unserved {
        if super::families::index_tools::is_sdk_index_tool_name(&name) {
            if name == "index_data" {
                omitted.push((name, LeftOutReason::ToolNotServed, REASON_INDEX_RUN));
            } else if !index_tools_composed {
                omitted.push((name, LeftOutReason::ToolNotServed, REASON_INDEX_TOOLS));
            }
        } else if needs_file_store {
            omitted.push((name, LeftOutReason::ToolNotServed, REASON_NEEDS_FILE_STORE));
        } else {
            omitted.push((name, LeftOutReason::ToolNotServed, REASON_NOT_PORTED));
        }
    }
    // Served but blocked by policy: its own cause, not "not ported".
    for name in &selected {
        if !omitted.iter().any(|(omitted, ..)| omitted == name.as_ref())
            && policy.tool_decision(reference.tool_type(), name)
                != crate::toolkits::policy::ToolAdmissionDecision::Allowed
        {
            omitted.push((
                name.to_string(),
                LeftOutReason::ToolBlockedByPolicy,
                REASON_POLICY,
            ));
        }
    }
    omitted.sort();
    omitted.dedup();
    for (name, cause, reason) in &omitted {
        on_refused(
            reference,
            ToolkitSkip::Tool {
                name,
                cause: *cause,
                reason,
            },
        );
    }
}

/// Whether, with the index toolset composed, this toolkit has no family
/// half: its stored selection is index-only, or it is an `artifact` toolkit
/// on a path lending no artifact authority (logged, since the family's own
/// tools are then left out while its index tools are served).
fn index_tools_stand_alone(
    reference: &FrozenToolReference<'_>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<bool, ToolsetMaterializationError> {
    let settings = reference.settings().ok_or_else(invalid_configuration)?;
    if selection_is_index_only(settings).map_err(|_| invalid_configuration())? {
        return Ok(true);
    }
    if reference.tool_type() == "artifact" && artifacts.is_none() {
        tracing::warn!(
            event = "agent_toolkit_tools_skipped",
            reason_code = "no_artifact_authority",
            toolkit_type = reference.tool_type(),
            toolkit_id = reference.tool_id(),
            "this path lends no artifact authority, so the artifact toolkit serves only its index tools"
        );
        return Ok(true);
    }
    Ok(false)
}

/// The index toolset of an indexing family whose frozen toolkit carries
/// Main's catalog, when the host lends an authority (ADR-0030 I1); `None`
/// otherwise — logged with the host's reason when a catalog is present and
/// the authority is withheld.
fn index_toolset_for(
    reference: &FrozenToolReference<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    index_tools: IndexToolSupport<'_>,
) -> Result<Option<adk_tool::BasicToolset>, ToolsetMaterializationError> {
    let Some(catalog) = reference
        .indexes()
        .filter(|_| is_index_family(reference.tool_type()))
    else {
        return Ok(None);
    };
    let authority = match index_tools {
        IndexToolSupport::Lent(authority) => authority,
        IndexToolSupport::Withheld(reason) => {
            if reason.is_warning() {
                tracing::warn!(
                    event = "agent_index_tools_unavailable",
                    reason_code = reason.reason_code(),
                    toolkit_type = reference.tool_type(),
                    toolkit_id = reference.tool_id(),
                    "{}",
                    reason.message()
                );
            } else {
                tracing::info!(
                    event = "agent_index_tools_unavailable",
                    reason_code = reason.reason_code(),
                    toolkit_type = reference.tool_type(),
                    toolkit_id = reference.tool_id(),
                    "{}",
                    reason.message()
                );
            }
            return Ok(None);
        }
    };
    // A catalog Main froze that this runtime cannot read is a disagreement
    // between the two freeze rules (both sides apply `indexcatalog`'s entry
    // rule and its 1024-entry cap), not a configuration the user can fix:
    // the toolkit's index tools are left out with a warning and the family's
    // own tools are served (review round 2).
    let catalog = match IndexCatalog::parse(catalog) {
        Ok(catalog) => catalog,
        Err(error) => {
            tracing::warn!(
                event = "agent_index_tools_unavailable",
                reason_code = "invalid_catalog",
                toolkit_type = reference.tool_type(),
                toolkit_id = reference.tool_id(),
                error = %error,
                "the toolkit's frozen index catalog is not readable; its index tools were left out"
            );
            return Ok(None);
        }
    };
    let settings = reference.settings().ok_or_else(invalid_configuration)?;
    // A toolkit without an id cannot be addressed by Main's index delete
    // flow, which `remove_index` runs through; Main freezes every configured
    // toolkit with one.
    let toolkit_id = reference.tool_id().ok_or_else(invalid_configuration)?;
    build_index_toolset(
        reference.toolkit_name(),
        toolkit_id,
        reference.tool_type(),
        settings,
        catalog,
        policy,
        authority,
    )
    .map(Some)
    .map_err(|error| match error.code() {
        IndexToolsetErrorCode::InvalidConfiguration => invalid_configuration(),
        IndexToolsetErrorCode::ResourceExhausted => resource_exhausted(),
    })
}

#[allow(
    clippy::too_many_lines,
    reason = "one dispatch table over the toolkit families; each arm is a call"
)]
async fn materialize(
    reference: &FrozenToolReference<'_>,
    policy: &Arc<ToolAdmissionPolicy>,
    delegated_tokens: &serde_json::Map<String, serde_json::Value>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<(Arc<dyn Toolset>, DelegatedAuthorizationCatalog), ToolsetMaterializationError> {
    if reference.kind() != FrozenToolKind::Configured {
        return Err(unsupported_toolkit());
    }
    let stored = reference.settings().ok_or_else(invalid_configuration)?;
    let pruned = prune_unserved_selection(reference, stored);
    let settings = pruned.as_ref().unwrap_or(stored);
    let name = reference.toolkit_name();
    // The platform side of the attachment tools (files and pictures), when the
    // path lends the artifact authority with its Office capability.
    let attachments = artifacts
        .and_then(ArtifactToolAuthority::office)
        .map(|host| AttachmentHost::new(Arc::clone(host)));
    if reference.tool_type() == "artifact" {
        let toolset = materialize_artifact(name, settings, policy, artifacts)?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    #[cfg(feature = "toolkit-office")]
    if matches!(reference.tool_type(), "pptx" | "docx" | "xlsx") {
        let toolset = materialize_office(reference.tool_type(), name, settings, policy, artifacts)?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if reference.tool_type() == "github" {
        let toolset = materialize_github(name, settings, policy, artifacts)?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if reference.tool_type() == "imagegen" {
        return materialize_imagegen(name, settings, policy, artifacts)
            .map(|toolset| (toolset, DelegatedAuthorizationCatalog::default()));
    }
    if matches!(
        reference.tool_type(),
        "aha" | "bigquery" | "bitbucket" | "carrier" | "confluence" | "figma" | "gitlab" | "jira"
    ) {
        let toolset = materialize_ported_saas(
            reference.tool_type(),
            name,
            settings,
            policy,
            attachments.as_ref(),
        )?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if matches!(
        reference.tool_type(),
        "azure"
            | "azure_search"
            | "elastic"
            | "gcp"
            | "gitlab_org"
            | "google_places"
            | "k8s"
            | "keycloak"
    ) {
        let toolset = materialize_a_to_k(reference.tool_type(), name, settings, policy)?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if reference.tool_type() == "openapi" {
        let remote = openapi::source::load(settings)
            .await
            .map_err(|error| match error {
                openapi::source::SourceError::Invalid => invalid_configuration(),
                openapi::source::SourceError::TooLarge => resource_exhausted(),
                openapi::source::SourceError::Unavailable => ToolsetMaterializationError::new(
                    ToolsetMaterializationErrorCode::DependencyUnavailable,
                ),
            })?;
        let config = match remote.as_ref() {
            Some(spec) => openapi::config::OpenApiToolkitConfig::parse_with_spec(
                name,
                settings,
                delegated_tokens,
                spec,
            ),
            None => openapi::config::OpenApiToolkitConfig::parse(name, settings, delegated_tokens),
        }
        .map_err(|error| openapi_materialization_error(error.code()))?
        .with_toolkit_id(reference.tool_id());
        let materialized = openapi::tools::build_openapi_toolset(name, config, policy)
            .map_err(|error| openapi_toolset_materialization_error(error.code()))?;
        return Ok((
            Arc::new(materialized.toolset),
            materialized.delegated_authorization,
        ));
    }
    if reference.tool_type() == "sharepoint" {
        let config =
            sharepoint::config::SharePointToolkitConfig::parse(name, settings, delegated_tokens)
                .map_err(|error| sharepoint_materialization_error(error.code()))?
                .with_toolkit_id(reference.tool_id());
        // The Office host of the live claim, when this path lends one: it
        // reads artifacts for `upload_file` and turns documents into text.
        let office = artifacts.and_then(ArtifactToolAuthority::office);
        let materialized =
            sharepoint::tools::build_sharepoint_toolset(name, config, policy, office)
                .map_err(|error| sharepoint_toolset_materialization_error(error.code()))?;
        return Ok((
            Arc::new(materialized.toolset),
            materialized.delegated_authorization,
        ));
    }
    if let Some(toolset) = materialize_zephyr_rest(
        reference.tool_type(),
        name,
        settings,
        policy,
        attachments.as_ref(),
    )? {
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if reference.tool_type().starts_with("ado_") {
        let toolset = materialize_ado(
            reference.tool_type(),
            name,
            settings,
            policy,
            attachments.as_ref(),
        )?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    if matches!(
        reference.tool_type(),
        "qtest" | "testio" | "testrail" | "xray_cloud"
    ) {
        let toolset = materialize_test_management(
            reference.tool_type(),
            name,
            settings,
            policy,
            attachments.as_ref(),
        )?;
        return Ok((toolset, DelegatedAuthorizationCatalog::default()));
    }
    let toolset = materialize_p_to_z(reference.tool_type(), name, settings, policy)?;
    Ok((toolset, DelegatedAuthorizationCatalog::default()))
}

/// The bearer-token Zephyr REST families (shared `zephyr_rest` transport);
/// `None` for any other type.
fn materialize_zephyr_rest(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    attachments: Option<&AttachmentHost>,
) -> Result<Option<Arc<dyn Toolset>>, ToolsetMaterializationError> {
    use super::families::{zephyr_enterprise, zephyr_essential, zephyr_scale};
    let toolset = match tool_type {
        "zephyr_enterprise" => zephyr_enterprise::tools::build_zephyr_enterprise_toolset(
            name,
            zephyr_enterprise::config::ZephyrEnterpriseToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        ),
        "zephyr_essential" => zephyr_essential::tools::build_zephyr_essential_toolset(
            name,
            zephyr_essential::config::ZephyrEssentialToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
            attachments.cloned(),
        ),
        "zephyr_scale" => zephyr_scale::tools::build_zephyr_scale_toolset(
            name,
            zephyr_scale::config::ZephyrScaleToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        ),
        _ => return Ok(None),
    }
    .map_err(|error| family_error(error.code()))?;
    Ok(Some(Arc::new(toolset)))
}

/// The Azure DevOps families, which share one client (`families::ado`).
fn materialize_ado(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    attachments: Option<&AttachmentHost>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    use super::families::ado_boards;
    use super::families::ado_plans;
    use super::families::ado_repos;
    use super::families::ado_wiki;
    let toolset = match tool_type {
        "ado_boards" => ado_boards::tools::build_ado_boards_toolset(
            name,
            ado_boards::config::AdoBoardsToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
            attachments.cloned(),
        )
        .map_err(|error| family_error(error.code()))?,
        "ado_plans" => ado_plans::tools::build_ado_plans_toolset(
            name,
            ado_plans::config::AdoPlansToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "ado_repos" => ado_repos::tools::build_ado_repos_toolset(
            name,
            ado_repos::config::AdoReposToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "ado_wiki" => ado_wiki::tools::build_ado_wiki_toolset(
            name,
            ado_wiki::config::AdoWikiToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        _ => return Err(unsupported_toolkit()),
    };
    Ok(Arc::new(toolset))
}

fn materialize_a_to_k(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    let toolset = match tool_type {
        "azure" => azure::tools::build_azure_toolset(
            name,
            azure::config::AzureToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "azure_search" => azure_search::tools::build_azure_search_read_only_toolset(
            name,
            azure_search::config::AzureSearchToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "elastic" => elastic::tools::build_elastic_toolset(
            name,
            elastic::config::ElasticToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "gcp" => gcp::tools::build_gcp_toolset(
            name,
            gcp::config::GcpToolkitConfig::parse(settings).map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "gitlab_org" => gitlab_org::tools::build_gitlab_org_toolset(
            name,
            gitlab_org::config::GitLabOrgToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "google_places" => google_places::tools::build_google_places_read_only_toolset(
            name,
            google_places::config::GooglePlacesToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "keycloak" => keycloak::tools::build_keycloak_toolset(
            name,
            keycloak::config::KeycloakToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "k8s" => kubernetes::tools::build_kubernetes_toolset(
            name,
            kubernetes::config::KubernetesToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        _ => return Err(unsupported_toolkit()),
    };
    Ok(Arc::new(toolset))
}

/// The hosted-service families ported from the Python-only set in one batch
/// (`aha`, `bigquery`, `bitbucket`, `carrier`, `confluence`, `figma`, `gitlab`,
/// `jira`). They live outside
/// `materialize_a_to_k` only to keep each dispatch function under clippy's
/// line limit; the arms stay at the indentation the elitea-main capability
/// gates read.
fn materialize_ported_saas(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    attachments: Option<&AttachmentHost>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    let toolset = match tool_type {
        "aha" => aha::tools::build_aha_toolset(
            name,
            aha::config::AhaToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
            attachments.cloned(),
        )
        .map_err(|error| family_error(error.code()))?,
        "bigquery" => bigquery::tools::build_bigquery_toolset(
            name,
            bigquery::config::BigQueryToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "bitbucket" => bitbucket::tools::build_bitbucket_toolset(
            name,
            bitbucket::config::BitbucketToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "carrier" => carrier::tools::build_carrier_toolset(
            name,
            carrier::config::CarrierToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
            attachments.cloned(),
        )
        .map_err(|error| family_error(error.code()))?,
        "confluence" => confluence::tools::build_confluence_toolset(
            name,
            confluence::config::ConfluenceToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
            attachments.cloned(),
        )
        .map_err(|error| family_error(error.code()))?,
        "figma" => figma::tools::build_figma_toolset(
            name,
            figma::config::FigmaToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "gitlab" => gitlab::tools::build_gitlab_toolset(
            name,
            gitlab::config::GitLabToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "jira" => jira::tools::build_jira_toolset(
            name,
            jira::config::JiraToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
            attachments.cloned(),
        )
        .map_err(|error| family_error(error.code()))?,
        _ => return Err(unsupported_toolkit()),
    };
    Ok(Arc::new(toolset))
}

fn materialize_p_to_z(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    let toolset = match tool_type {
        "postman" => postman::tools::build_postman_toolset(
            name,
            postman::config::PostmanToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "rally" => rally::tools::build_rally_toolset(
            name,
            rally::config::RallyToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "report_portal" => report_portal::tools::build_report_portal_toolset(
            name,
            report_portal::config::ReportPortalToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "salesforce" => salesforce::tools::build_salesforce_toolset(
            name,
            salesforce::config::SalesforceToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "service_now" => service_now::tools::build_service_now_toolset(
            name,
            service_now::config::ServiceNowToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "slack" => slack::tools::build_slack_toolset(
            name,
            slack::config::SlackToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "sonar" => sonar::tools::build_sonar_read_only_toolset(
            name,
            sonar::config::SonarToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        // Without `toolkit-sql` a `sql` toolkit falls through to unsupported
        // and is skipped like any family this runtime cannot serve.
        #[cfg(feature = "toolkit-sql")]
        "sql" => sql::tools::build_sql_toolset(
            name,
            sql::config::SqlToolkitConfig::parse(settings).map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "yagmail" => yagmail::tools::build_yagmail_toolset(
            name,
            yagmail::config::YagmailToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "zephyr" => zephyr::tools::build_zephyr_toolset(
            name,
            zephyr::config::ZephyrToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        "zephyr_squad" => zephyr_squad::tools::build_zephyr_squad_toolset(
            name,
            zephyr_squad::config::ZephyrSquadToolkitConfig::parse(settings)
                .map_err(|_| invalid_configuration())?,
            policy,
        )
        .map_err(|_| invalid_configuration())?,
        // MCP and nested applications are rejected above by kind.
        _ => return Err(unsupported_toolkit()),
    };
    Ok(Arc::new(toolset))
}

/// The test-management families. A selection that names only tools a
/// partial family does not serve skips the toolkit, as an unsupported family
/// is skipped.
fn materialize_test_management(
    tool_type: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    attachments: Option<&AttachmentHost>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    let toolset = match tool_type {
        "qtest" => qtest::tools::build_qtest_toolset(
            name,
            qtest::config::QtestToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
            attachments.cloned(),
        )
        .map_err(|error| family_error(error.code()))?,
        "testio" => testio::tools::build_testio_toolset(
            name,
            testio::config::TestIoToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        "testrail" => testrail::tools::build_testrail_toolset(
            name,
            testrail::config::TestRailToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
            attachments.cloned(),
        )
        .map_err(|error| family_error(error.code()))?,
        "xray_cloud" => xray_cloud::tools::build_xray_cloud_toolset(
            name,
            xray_cloud::config::XrayToolkitConfig::parse(settings)
                .map_err(|error| family_error(error.code()))?,
            policy,
        )
        .map_err(|error| family_error(error.code()))?,
        _ => return Err(unsupported_toolkit()),
    };
    Ok(Arc::new(toolset))
}

const fn invalid_configuration() -> ToolsetMaterializationError {
    ToolsetMaterializationError::new(ToolsetMaterializationErrorCode::InvalidConfiguration)
}

const fn unsupported_toolkit() -> ToolsetMaterializationError {
    ToolsetMaterializationError::new(ToolsetMaterializationErrorCode::UnsupportedToolkit)
}

const fn resource_exhausted() -> ToolsetMaterializationError {
    ToolsetMaterializationError::new(ToolsetMaterializationErrorCode::ResourceExhausted)
}

/// The `artifact` family. No authority lent means no family: the toolkit is
/// skipped (logged by the caller), not bound to tools that would fail on
/// every call.
fn materialize_artifact(
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    let authority = artifacts.ok_or_else(unsupported_toolkit)?;
    let config = artifact::config::ArtifactToolkitConfig::parse(settings)
        .map_err(|error| artifact_config_materialization_error(error.code()))?;
    let toolset = artifact::tools::build_artifact_toolset(name, &config, policy, authority)
        .map_err(|error| artifact_toolset_materialization_error(error.code()))?;
    Ok(Arc::new(toolset))
}

/// The `github` family. Like `pptx`, it may lend the platform file store it
/// copies from (`create_file`'s filepath, `apply_git_patch_from_file`); a path
/// that lends none still serves every other tool.
fn materialize_github(
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    let office = artifacts.and_then(|authority| authority.office().cloned());
    let toolset = github::tools::build_github_toolset(
        name,
        github::config::GitHubToolkitConfig::parse(settings)
            .map_err(|_| invalid_configuration())?,
        policy,
        office,
    )
    .map_err(|error| github_toolset_materialization_error(error.code()))?;
    Ok(Arc::new(toolset))
}

/// The Office families (ADR-0032 P3, P4): `pptx`, `docx` and `xlsx`. Like
/// `artifact`, their authority is the live claim: the Office host rides on the
/// artifact authority. A path that lends none (the desktop until P6) skips the
/// toolkit as unsupported.
#[cfg(feature = "toolkit-office")]
fn materialize_office(
    family: &str,
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    use super::families::office_common::{OfficeConfigError, OfficeToolsetError};
    let authority = artifacts.ok_or_else(unsupported_toolkit)?;
    let config_error = |error: OfficeConfigError| office_config_materialization_error(&error);
    let toolset_error = |error: OfficeToolsetError| office_toolset_materialization_error(&error);
    let built = match family {
        "docx" => {
            let config = docx::config::DocxToolkitConfig::parse(settings).map_err(config_error)?;
            let toolset = docx::tools::build_docx_toolset(name, &config, policy, authority)
                .map_err(toolset_error)?;
            toolset.map(|t| Arc::new(t) as Arc<dyn Toolset>)
        }
        "xlsx" => {
            let config = xlsx::config::XlsxToolkitConfig::parse(settings).map_err(config_error)?;
            let toolset = xlsx::tools::build_xlsx_toolset(name, &config, policy, authority)
                .map_err(toolset_error)?;
            toolset.map(|t| Arc::new(t) as Arc<dyn Toolset>)
        }
        "pptx" => {
            let config = pptx::config::PptxToolkitConfig::parse(settings).map_err(config_error)?;
            let toolset = pptx::tools::build_pptx_toolset(name, &config, policy, authority)
                .map_err(toolset_error)?;
            toolset.map(|t| Arc::new(t) as Arc<dyn Toolset>)
        }
        _ => return Err(unsupported_toolkit()),
    };
    built.ok_or_else(unsupported_toolkit)
}

#[cfg(feature = "toolkit-office")]
fn office_config_materialization_error(
    error: &super::families::office_common::OfficeConfigError,
) -> ToolsetMaterializationError {
    use super::families::office_common::OfficeConfigErrorCode;
    match error.code() {
        OfficeConfigErrorCode::InvalidConfiguration => invalid_configuration(),
        OfficeConfigErrorCode::ResourceExhausted => resource_exhausted(),
        // A toolkit saved without a usable bucket is skipped and named in the
        // run's notice (live 4.45.0: it failed every turn of the agent).
        OfficeConfigErrorCode::BucketName => {
            ToolsetMaterializationError::refused(UnsupportedSetting::BucketName)
        }
    }
}

#[cfg(feature = "toolkit-office")]
fn office_toolset_materialization_error(
    error: &super::families::office_common::OfficeToolsetError,
) -> ToolsetMaterializationError {
    use super::families::office_common::OfficeToolsetErrorCode;
    match error.code() {
        OfficeToolsetErrorCode::ResourceExhausted => resource_exhausted(),
        OfficeToolsetErrorCode::InvalidConfiguration
        | OfficeToolsetErrorCode::InvalidDefinition => invalid_configuration(),
    }
}

/// The `imagegen` family (#864). Like `pptx`, its authority is the live claim:
/// the images API and the object API ride on the Office host of the artifact
/// authority. A path that lends no host that reaches the images API (the
/// desktop until P6) skips the toolkit as unsupported.
fn materialize_imagegen(
    name: &str,
    settings: &serde_json::Map<String, serde_json::Value>,
    policy: &Arc<ToolAdmissionPolicy>,
    artifacts: Option<&ArtifactToolAuthority>,
) -> Result<Arc<dyn Toolset>, ToolsetMaterializationError> {
    use super::families::imagegen::{self, ImageGenConfigErrorCode};
    let authority = artifacts.ok_or_else(unsupported_toolkit)?;
    let config =
        imagegen::ImageGenToolkitConfig::parse(settings).map_err(|error| match error.code() {
            ImageGenConfigErrorCode::InvalidConfiguration => invalid_configuration(),
            ImageGenConfigErrorCode::ResourceExhausted => resource_exhausted(),
        })?;
    let toolset = imagegen::build_imagegen_toolset(name, &config, policy, authority)
        .map_err(|_| invalid_configuration())?
        .ok_or_else(unsupported_toolkit)?;
    Ok(Arc::new(toolset))
}

const fn artifact_config_materialization_error(
    code: artifact::config::ArtifactConfigErrorCode,
) -> ToolsetMaterializationError {
    match code {
        artifact::config::ArtifactConfigErrorCode::InvalidConfiguration => invalid_configuration(),
        artifact::config::ArtifactConfigErrorCode::ResourceExhausted => resource_exhausted(),
        artifact::config::ArtifactConfigErrorCode::BucketName => {
            ToolsetMaterializationError::refused(UnsupportedSetting::BucketName)
        }
    }
}

const fn artifact_toolset_materialization_error(
    code: artifact::tools::ArtifactToolsetErrorCode,
) -> ToolsetMaterializationError {
    match code {
        artifact::tools::ArtifactToolsetErrorCode::InvalidConfiguration
        | artifact::tools::ArtifactToolsetErrorCode::InvalidDefinition => invalid_configuration(),
        artifact::tools::ArtifactToolsetErrorCode::ResourceExhausted => resource_exhausted(),
    }
}

const fn openapi_materialization_error(
    code: openapi::config::OpenApiConfigErrorCode,
) -> ToolsetMaterializationError {
    match code {
        openapi::config::OpenApiConfigErrorCode::InvalidConfiguration => invalid_configuration(),
        openapi::config::OpenApiConfigErrorCode::ResourceExhausted => resource_exhausted(),
        openapi::config::OpenApiConfigErrorCode::UnsupportedCapability => unsupported_toolkit(),
    }
}

const fn github_toolset_materialization_error(
    code: github::tools::GitHubToolsetErrorCode,
) -> ToolsetMaterializationError {
    match code {
        github::tools::GitHubToolsetErrorCode::InvalidConfiguration
        | github::tools::GitHubToolsetErrorCode::Client
        | github::tools::GitHubToolsetErrorCode::InvalidDefinition => invalid_configuration(),
        github::tools::GitHubToolsetErrorCode::UnsupportedSelection => unsupported_toolkit(),
    }
}

const fn openapi_toolset_materialization_error(
    code: openapi::tools::OpenApiToolsetErrorCode,
) -> ToolsetMaterializationError {
    match code {
        openapi::tools::OpenApiToolsetErrorCode::InvalidConfiguration
        | openapi::tools::OpenApiToolsetErrorCode::Client
        | openapi::tools::OpenApiToolsetErrorCode::InvalidDefinition => invalid_configuration(),
        openapi::tools::OpenApiToolsetErrorCode::ResourceExhausted => resource_exhausted(),
        openapi::tools::OpenApiToolsetErrorCode::UnsupportedCapability => unsupported_toolkit(),
    }
}

const fn sharepoint_materialization_error(
    code: sharepoint::config::SharePointConfigErrorCode,
) -> ToolsetMaterializationError {
    match code {
        sharepoint::config::SharePointConfigErrorCode::InvalidConfiguration => {
            invalid_configuration()
        }
        sharepoint::config::SharePointConfigErrorCode::ResourceExhausted => resource_exhausted(),
        // The only unsupported capability of this family is an app-only
        // credential: named, so the user is told why the toolkit is off.
        sharepoint::config::SharePointConfigErrorCode::UnsupportedCapability => {
            ToolsetMaterializationError::refused(UnsupportedSetting::SharePointAppOnly)
        }
    }
}

const fn sharepoint_toolset_materialization_error(
    code: sharepoint::tools::SharePointToolsetErrorCode,
) -> ToolsetMaterializationError {
    match code {
        sharepoint::tools::SharePointToolsetErrorCode::InvalidConfiguration
        | sharepoint::tools::SharePointToolsetErrorCode::Client
        | sharepoint::tools::SharePointToolsetErrorCode::InvalidDefinition => {
            invalid_configuration()
        }
        sharepoint::tools::SharePointToolsetErrorCode::ResourceExhausted => resource_exhausted(),
        sharepoint::tools::SharePointToolsetErrorCode::UnsupportedCapability
        | sharepoint::tools::SharePointToolsetErrorCode::UnsupportedSelection => {
            unsupported_toolkit()
        }
    }
}

#[cfg(test)]
mod notice_helper_tests {
    use super::{family_tool_catalog, omitted_selection, round_robin_take};

    fn names(selected: &[&str]) -> Vec<Box<str>> {
        selected.iter().map(|name| Box::from(*name)).collect()
    }

    /// Review round 2: ONE rule replaces the eight per-family copies.
    #[test]
    fn omitted_selection_is_one_rule_over_a_family_catalog() {
        let catalog = [("read", false), ("attach", true)];
        let selected = names(&["read", "attach", "nope"]);
        assert_eq!(
            omitted_selection(&catalog, &selected, false),
            [("attach".to_owned(), true), ("nope".to_owned(), false)]
        );
        assert_eq!(
            omitted_selection(&catalog, &selected, true),
            [("nope".to_owned(), false)]
        );
        assert!(omitted_selection(&catalog, &[], false).is_empty());
    }

    #[test]
    fn each_reporting_family_states_its_file_store_tools() {
        let needs = |family: &str| -> Vec<&'static str> {
            family_tool_catalog(family)
                .expect(family)
                .into_iter()
                .filter_map(|(name, needs)| needs.then_some(name))
                .collect()
        };
        assert_eq!(needs("aha"), ["attach_file"]);
        assert!(!needs("jira").is_empty());
        assert!(needs("confluence").contains(&"add_file_to_page"));
        for family in ["bigquery", "bitbucket", "figma", "github", "gitlab"] {
            assert!(needs(family).is_empty(), "{family}");
            assert!(!family_tool_catalog(family).expect(family).is_empty());
        }
        assert!(needs("slack").is_empty());
        // A type with no fixed tool list reports none.
        assert!(family_tool_catalog("openapi").is_none());
    }

    #[test]
    fn round_robin_take_is_fair_and_honours_both_caps() {
        let long: Vec<u32> = (0..10).collect();
        let short = vec![100, 101];
        let lists = [long, short];
        // Item cap: 2 + 2 + then the long list's tail.
        assert_eq!(round_robin_take(&lists, 6, usize::MAX, |_| 0), [4, 2]);
        // Cost cap: three items of cost 2 fit in 7, the fourth does not.
        assert_eq!(round_robin_take(&lists, 100, 7, |_| 2), [2, 1]);
        assert_eq!(round_robin_take(&lists, 100, usize::MAX, |_| 1), [10, 2]);
        let empty: [Vec<u32>; 2] = [Vec::new(), Vec::new()];
        assert_eq!(round_robin_take(&empty, 5, 5, |_| 1), [0, 0]);
    }
}

#[cfg(test)]
mod materialize_tests {
    use std::collections::BTreeMap;

    use serde_json::{Map, Value, json};

    use super::{
        LeftOutReason, LeftOutToolkit, family_tool_catalog, omitted_selection,
        prune_unserved_selection, report_unserved_selected_tools,
    };
    use crate::toolkits::policy::ToolAdmissionPolicy;
    use crate::toolkits::snapshot::FrozenToolSnapshot;

    const CAPABILITY: &str = include_str!(
        "../../../../../services/elitea-main/internal/runtimecomposition/current_rust_worker_toolkit_capability_snapshot.json"
    );
    /// Served types whose tools are not a fixed list: MCP is materialized on
    /// its own path, and an `openapi` toolkit's tools are its stored spec's
    /// operations.
    const NO_FIXED_CATALOG: [&str; 3] = ["mcp", "mcp_config", "openapi"];
    const MADE_UP: &str = "made_up_tool_nobody_serves";

    fn served_types() -> Vec<String> {
        let document: Value = serde_json::from_str(CAPABILITY).expect("capability JSON");
        document["supported_tool_types"]
            .as_array()
            .expect("supported types")
            .iter()
            .map(|name| name.as_str().expect("a type").to_owned())
            .filter(|name| {
                // A family behind a feature this build does not carry.
                (cfg!(feature = "toolkit-office")
                    || !matches!(name.as_str(), "pptx" | "docx" | "xlsx"))
                    && (cfg!(feature = "toolkit-sql") || name != "sql")
            })
            .collect()
    }

    /// Live 4.45.0 (D3): the Office families had no catalogue, so a selected
    /// tool they did not serve was dropped with no notice. This walks EVERY
    /// type the worker declares served, so the next family added without a
    /// catalogue fails here, and checks the catalogue is the family's own
    /// served list where the capability file declares one.
    #[test]
    fn every_served_family_reports_an_unknown_selected_tool() {
        let document: Value = serde_json::from_str(CAPABILITY).expect("capability JSON");
        let declared = document["supported_tools"].as_object().expect("tools");
        let policy = ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy");
        let types = served_types();
        assert!(
            types.len() >= 40,
            "the capability file stopped listing types"
        );
        for tool_type in &types {
            if NO_FIXED_CATALOG.contains(&tool_type.as_str()) {
                continue;
            }
            let catalog = family_tool_catalog(tool_type)
                .unwrap_or_else(|| panic!("{tool_type} has no tool catalogue"));
            assert!(!catalog.is_empty(), "{tool_type}");
            if let Some(names) = declared.get(tool_type) {
                for name in names.as_array().expect("names") {
                    let name = name.as_str().expect("a name");
                    assert!(
                        catalog.iter().any(|(tool, _)| *tool == name),
                        "{tool_type}.{name} is declared served but missing from its catalogue"
                    );
                }
            }
            let served = catalog[0].0;
            let selected: Vec<Box<str>> = vec![served.into(), MADE_UP.into()];
            assert_eq!(
                omitted_selection(&catalog, &selected, true),
                [(MADE_UP.to_owned(), false)],
                "{tool_type}"
            );

            // Through the reporting path itself, as the run sees it.
            let version = json!({"tools": [{
                "id": 7, "type": tool_type, "toolkit_name": "kit",
                "settings": {"selected_tools": [served, MADE_UP]}
            }]});
            let snapshot =
                FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
                    .expect("snapshot")
                    .apply_policy(&policy);
            let reference = snapshot.iter().next().expect("reference");
            let mut left_out: Vec<LeftOutToolkit> = Vec::new();
            report_unserved_selected_tools(reference, &policy, None, false, &mut |r, skip| {
                left_out.push(LeftOutToolkit::new(r, skip));
            });
            assert!(
                left_out
                    .iter()
                    .any(|toolkit| toolkit.tool_name() == Some(MADE_UP)
                        && toolkit.cause() == LeftOutReason::ToolNotServed),
                "{tool_type}: {left_out:?}"
            );
        }
    }

    /// The Office families' tools all act on a document in the file store,
    /// so a run that lends none names each selected tool as needing it.
    #[cfg(feature = "toolkit-office")]
    #[test]
    fn office_tools_need_the_file_store() {
        for family in ["pptx", "docx", "xlsx", "imagegen"] {
            let catalog = family_tool_catalog(family).expect(family);
            assert!(catalog.iter().all(|(_, needs)| *needs), "{family}");
        }
    }

    /// A strict family (one that refuses a selection naming any tool it does
    /// not serve) is built with its served tools; the unknown names are
    /// pruned and reported, never widened to "every tool".
    #[test]
    fn unserved_names_are_pruned_but_never_widen_the_selection() {
        let policy = ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy");
        let version = json!({"tools": [
            {"id": 1, "type": "slack", "toolkit_name": "a",
             "settings": {"selected_tools": ["send_message", MADE_UP]}},
            {"id": 2, "type": "slack", "toolkit_name": "b",
             "settings": {"selected_tools": [MADE_UP]}},
            {"id": 3, "type": "slack", "toolkit_name": "c",
             "settings": {"selected_tools": ["send_message"]}},
            {"id": 4, "type": "slack", "toolkit_name": "d",
             "settings": {"selected_tools": ["search_index", MADE_UP]}}
        ]});
        let snapshot =
            FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
                .expect("snapshot")
                .apply_policy(&policy);
        let pruned: Vec<Option<Map<String, Value>>> = snapshot
            .iter()
            .map(|reference| {
                prune_unserved_selection(reference, reference.settings().expect("settings"))
            })
            .collect();
        assert_eq!(
            pruned[0].as_ref().expect("pruned")["selected_tools"],
            json!(["send_message"])
        );
        assert!(
            pruned[1].is_none(),
            "nothing served remains: the family decides"
        );
        assert!(pruned[2].is_none(), "nothing to prune");
        assert!(
            pruned[3].is_none(),
            "an index-only remainder is not widened"
        );
    }
}
