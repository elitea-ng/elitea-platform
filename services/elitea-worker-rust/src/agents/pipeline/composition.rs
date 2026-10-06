//! Admit a bounded frozen pipeline tree before runtime binding.

use std::collections::{BTreeMap, BTreeSet};
use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;

use serde::{Deserialize, Serialize};
use serde_json::{Map, Value};

use super::{
    AdmittedToolSnapshot, ClaimBoundRuntimeContextAuthority, FrozenToolKind, FrozenToolSnapshot,
    NativeAgentAssemblyError, NativeAgentAssemblyErrorCode, OrdinaryNoToolProfile,
    PipelineDefinition, PipelineExecutionProfile, PlatformClient,
    SCOPED_APPLICATION_CONTINUATION_READY, ToolAdmissionPolicy, invalid_pipeline_tool_scope,
    unsupported_pipeline_runtime,
};

// Match the existing nested interrupt parser. Do not admit deeper pauses.
pub(crate) const MAX_PIPELINE_COMPOSITION_DEPTH: usize = 3;
const MAX_PIPELINE_COMPOSITION_HOPS: usize = 25;
pub(crate) const MAX_PIPELINE_CHECKPOINT_PATHS: usize = 128;
const MAX_COMPOSITION_VERSION_BYTES: usize = 4 * 1024 * 1024;

/// One exact saved revision admitted for a descendant graph thread.
#[derive(Clone, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PipelineCheckpointRevision {
    pub(crate) application_id: u64,
    pub(crate) version_id: u64,
    pub(crate) definition_digest: [u8; 32],
}

/// Paths are relative ADK node paths. They authorize no arbitrary prefix.
#[derive(Clone, Default, Deserialize, Eq, PartialEq, Serialize)]
#[serde(deny_unknown_fields)]
pub(crate) struct PipelineCheckpointCatalog {
    pub(crate) root: Option<PipelineCheckpointRevision>,
    pub(crate) descendants: BTreeMap<String, PipelineCheckpointRevision>,
}

pub(super) struct AdmittedSavedPipeline {
    pub(super) version: Map<String, Value>,
    pub(super) profile: PipelineExecutionProfile,
    pub(super) children: BTreeMap<String, Arc<AdmittedSavedPipeline>>,
    pub(super) revision: PipelineCheckpointRevision,
}

pub(super) struct AdmittedPipelineComposition {
    pub(super) children: BTreeMap<String, Arc<AdmittedSavedPipeline>>,
    pub(super) checkpoint_paths: Vec<String>,
}

struct CompositionAdmission<'a> {
    platform: &'a PlatformClient,
    authority: &'a ClaimBoundRuntimeContextAuthority,
    project_id: u64,
    policy: &'a ToolAdmissionPolicy,
    resolving: BTreeSet<(u64, u64)>,
    versions: BTreeMap<(u64, u64), Map<String, Value>>,
    hops: usize,
    version_bytes: usize,
    scoped_ready: bool,
}

type AdmissionFuture<'a> = Pin<
    Box<
        dyn Future<Output = Result<Arc<AdmittedSavedPipeline>, NativeAgentAssemblyError>>
            + Send
            + 'a,
    >,
>;

impl CompositionAdmission<'_> {
    fn saved<'a>(
        &'a mut self,
        identity: (u64, u64),
        project_id: Option<u64>,
        fallback: &'a OrdinaryNoToolProfile,
        depth: usize,
    ) -> AdmissionFuture<'a> {
        Box::pin(async move {
            if identity.0 == 0
                || identity.1 == 0
                || project_id.is_some_and(|project| project != self.project_id)
            {
                return Err(invalid_pipeline_tool_scope());
            }
            if depth > MAX_PIPELINE_COMPOSITION_DEPTH || self.hops == MAX_PIPELINE_COMPOSITION_HOPS
            {
                return Err(composition_exhausted());
            }
            self.hops += 1;
            if !self.resolving.insert(identity) {
                return Err(NativeAgentAssemblyError::new(
                    NativeAgentAssemblyErrorCode::InvalidConfiguration,
                    "the saved pipeline composition contains a cycle",
                ));
            }
            let result = async {
                let version = if let Some(version) = self.versions.get(&identity) {
                    version.clone()
                } else {
                    let version = self
                        .platform
                        .resolve_application_version(self.authority, identity.0, identity.1)
                        .await
                        .map_err(NativeAgentAssemblyError::from)?
                        .into_version_details();
                    self.version_bytes = self
                        .version_bytes
                        .checked_add(
                            serde_json::to_vec(&version)
                                .map_err(|_| invalid_pipeline_tool_scope())?
                                .len(),
                        )
                        .ok_or_else(composition_exhausted)?;
                    if self.version_bytes > MAX_COMPOSITION_VERSION_BYTES {
                        return Err(composition_exhausted());
                    }
                    self.versions.insert(identity, version.clone());
                    version
                };
                let mut profile =
                    PipelineExecutionProfile::from_nested_version(&version, fallback)?;
                let frozen = FrozenToolSnapshot::from_version_details(&version)
                    .map_err(|_| invalid_pipeline_tool_scope())?;
                profile.validate_tool_snapshot(&frozen, self.policy)?;
                let admitted = frozen.apply_policy(self.policy);
                let children = self
                    .children(&profile, &admitted, depth, self.scoped_ready)
                    .await?;
                let revision = PipelineCheckpointRevision {
                    application_id: identity.0,
                    version_id: identity.1,
                    definition_digest: profile.definition().definition_digest(),
                };
                Ok(Arc::new(AdmittedSavedPipeline {
                    version,
                    profile,
                    children,
                    revision,
                }))
            }
            .await;
            self.resolving.remove(&identity);
            result
        })
    }

    async fn children(
        &mut self,
        profile: &PipelineExecutionProfile,
        snapshot: &AdmittedToolSnapshot<'_>,
        depth: usize,
        ordinary_agents_supported: bool,
    ) -> Result<BTreeMap<String, Arc<AdmittedSavedPipeline>>, NativeAgentAssemblyError> {
        let aliases = profile
            .definition()
            .application_selections()
            .map(|selection| selection.alias().to_owned())
            .collect::<BTreeSet<_>>();
        let mut references = Vec::new();
        for alias in aliases {
            let mut matching = snapshot
                .iter()
                .filter(|reference| reference.toolkit_name() == alias);
            let reference = matching.next().ok_or_else(invalid_pipeline_tool_scope)?;
            if matching.next().is_some() || reference.kind() != FrozenToolKind::Application {
                return Err(invalid_pipeline_tool_scope());
            }
            match reference.application_agent_type() {
                Some("pipeline") => references.push((
                    alias,
                    reference
                        .application_identity()
                        .ok_or_else(invalid_pipeline_tool_scope)?,
                    reference.application_project_id(),
                )),
                Some("agent") if ordinary_agents_supported => {}
                // A nested ordinary Agent needs a scoped event/catalog owner.
                // Do not bind a tool whose pause could be lost or misattributed.
                Some("agent") => return Err(unsupported_pipeline_runtime()),
                _ => return Err(invalid_pipeline_tool_scope()),
            }
        }
        let mut children = BTreeMap::new();
        for (alias, identity, project_id) in references {
            let child = self
                .saved(identity, project_id, profile.shell(), depth + 1)
                .await?;
            children.insert(alias, child);
        }
        Ok(children)
    }
}

pub(super) async fn admit_root_composition(
    platform: &PlatformClient,
    authority: &ClaimBoundRuntimeContextAuthority,
    project_id: u64,
    policy: &ToolAdmissionPolicy,
    profile: &PipelineExecutionProfile,
    snapshot: &AdmittedToolSnapshot<'_>,
) -> Result<AdmittedPipelineComposition, NativeAgentAssemblyError> {
    let mut admission = CompositionAdmission {
        platform,
        authority,
        project_id,
        policy,
        resolving: BTreeSet::new(),
        versions: BTreeMap::new(),
        hops: 0,
        version_bytes: 0,
        scoped_ready: SCOPED_APPLICATION_CONTINUATION_READY,
    };
    let children = admission.children(profile, snapshot, 0, true).await?;
    let checkpoint_paths = composition_paths(profile.definition(), &children)?;
    Ok(AdmittedPipelineComposition {
        children,
        checkpoint_paths,
    })
}

#[allow(clippy::too_many_arguments)] // Existing explicit frozen admission owners.
pub(super) async fn admit_tool_composition(
    platform: &PlatformClient,
    authority: &ClaimBoundRuntimeContextAuthority,
    project_id: u64,
    policy: &ToolAdmissionPolicy,
    identity: (u64, u64),
    selected_project_id: Option<u64>,
    fallback: &OrdinaryNoToolProfile,
) -> Result<Arc<AdmittedSavedPipeline>, NativeAgentAssemblyError> {
    admit_tool_composition_with_scoped(
        platform,
        authority,
        project_id,
        policy,
        identity,
        selected_project_id,
        fallback,
        SCOPED_APPLICATION_CONTINUATION_READY,
    )
    .await
}

#[allow(clippy::too_many_arguments)] // Keep the private scoped policy beside frozen authority.
pub(super) async fn admit_tool_composition_with_scoped(
    platform: &PlatformClient,
    authority: &ClaimBoundRuntimeContextAuthority,
    project_id: u64,
    policy: &ToolAdmissionPolicy,
    identity: (u64, u64),
    selected_project_id: Option<u64>,
    fallback: &OrdinaryNoToolProfile,
    scoped_ready: bool,
) -> Result<Arc<AdmittedSavedPipeline>, NativeAgentAssemblyError> {
    let mut admission = CompositionAdmission {
        platform,
        authority,
        project_id,
        policy,
        resolving: BTreeSet::new(),
        versions: BTreeMap::new(),
        hops: 0,
        version_bytes: 0,
        scoped_ready,
    };
    // The tool root is not an ADK subgraph wrapper.
    let admitted = admission
        .saved(identity, selected_project_id, fallback, 0)
        .await?;
    checkpoint_catalog(&admitted)?;
    Ok(admitted)
}

pub(super) fn composition_paths(
    definition: &PipelineDefinition,
    children: &BTreeMap<String, Arc<AdmittedSavedPipeline>>,
) -> Result<Vec<String>, NativeAgentAssemblyError> {
    let mut paths = BTreeMap::new();
    collect_paths(definition, children, "", 0, &mut paths)?;
    Ok(paths.into_keys().collect())
}

pub(super) fn checkpoint_catalog(
    admitted: &AdmittedSavedPipeline,
) -> Result<PipelineCheckpointCatalog, NativeAgentAssemblyError> {
    let mut paths = BTreeMap::new();
    collect_paths(
        admitted.profile.definition(),
        &admitted.children,
        "",
        0,
        &mut paths,
    )?;
    Ok(PipelineCheckpointCatalog {
        root: Some(admitted.revision.clone()),
        descendants: paths
            .into_iter()
            .filter_map(|(path, revision)| revision.map(|value| (path, value)))
            .collect(),
    })
}

fn collect_paths(
    definition: &PipelineDefinition,
    children: &BTreeMap<String, Arc<AdmittedSavedPipeline>>,
    prefix: &str,
    depth: usize,
    paths: &mut BTreeMap<String, Option<PipelineCheckpointRevision>>,
) -> Result<(), NativeAgentAssemblyError> {
    for (node_id, selection) in definition.application_nodes() {
        let path = if prefix.is_empty() {
            node_id.to_owned()
        } else {
            format!("{prefix}/{node_id}")
        };
        if depth == MAX_PIPELINE_COMPOSITION_DEPTH || paths.len() == MAX_PIPELINE_CHECKPOINT_PATHS {
            return Err(composition_exhausted());
        }
        let child = children.get(selection.alias());
        if paths
            .insert(path.clone(), child.map(|child| child.revision.clone()))
            .is_some()
        {
            return Err(invalid_pipeline_tool_scope());
        }
        if let Some(child) = child {
            collect_paths(
                child.profile.definition(),
                &child.children,
                &path,
                depth + 1,
                paths,
            )?;
        }
    }
    Ok(())
}

const fn composition_exhausted() -> NativeAgentAssemblyError {
    NativeAgentAssemblyError::new(
        NativeAgentAssemblyErrorCode::ResourceExhausted,
        "the saved pipeline composition exceeds its resource bound",
    )
}

pub(super) fn composition_definitions(
    admitted: &AdmittedSavedPipeline,
) -> BTreeMap<String, PipelineDefinition> {
    composition_definitions_for(admitted.profile.definition(), &admitted.children)
}

pub(super) fn composition_definitions_for(
    definition: &PipelineDefinition,
    children: &BTreeMap<String, Arc<AdmittedSavedPipeline>>,
) -> BTreeMap<String, PipelineDefinition> {
    fn append(
        definition: &PipelineDefinition,
        children: &BTreeMap<String, Arc<AdmittedSavedPipeline>>,
        prefix: &str,
        output: &mut BTreeMap<String, PipelineDefinition>,
    ) {
        for (node, selection) in definition.application_nodes() {
            if let Some(child) = children.get(selection.alias()) {
                let path = if prefix.is_empty() {
                    node.to_owned()
                } else {
                    format!("{prefix}/{node}")
                };
                output.insert(path.clone(), child.profile.definition().clone());
                append(child.profile.definition(), &child.children, &path, output);
            }
        }
    }
    let mut result = BTreeMap::new();
    append(definition, children, "", &mut result);
    result
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::protocol::control::test_runtime_context_authority;
    use crate::transport::runtime_context::{
        RuntimeContextClient, RuntimeContextConfig, RuntimeContextRpc, RuntimeContextTransportError,
    };
    use async_trait::async_trait;
    use bytes::Bytes;
    use http::{Request, Response, StatusCode, Version};
    use http_body_util::Full;
    use serde_json::json;
    use std::fmt::Write as _;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::time::Duration;
    use tonic::body::Body;

    struct FrozenVersions {
        versions: BTreeMap<(u64, u64), Map<String, Value>>,
        reads: Arc<AtomicUsize>,
    }

    #[async_trait]
    impl RuntimeContextRpc for FrozenVersions {
        async fn post(
            &self,
            request: Request<Body>,
        ) -> Result<Response<Body>, RuntimeContextTransportError> {
            let parts = request.uri().path().split('/').collect::<Vec<_>>();
            let version = parts.last().and_then(|value| value.parse::<u64>().ok());
            let application = parts
                .get(parts.len() - 3)
                .and_then(|value| value.parse::<u64>().ok());
            let identity = application
                .zip(version)
                .ok_or(RuntimeContextTransportError::Unavailable)?;
            self.reads.fetch_add(1, Ordering::SeqCst);
            let details = self
                .versions
                .get(&identity)
                .ok_or(RuntimeContextTransportError::Unavailable)?;
            let raw =
                json!({"schema_version":"elitea.runtime.application-version.v1", "project_id":17,
                "application_id":identity.0, "version_id":identity.1, "version_details":details})
                .to_string();
            Response::builder()
                .status(StatusCode::OK)
                .version(Version::HTTP_2)
                .header("content-type", "application/json")
                .header("cache-control", "private, no-cache, no-store")
                .header("pragma", "no-cache")
                .header("content-length", raw.len())
                .body(Body::new(Full::new(Bytes::from(raw))))
                .map_err(|_| RuntimeContextTransportError::Unavailable)
        }
    }

    fn client(
        versions: BTreeMap<(u64, u64), Map<String, Value>>,
    ) -> (PlatformClient, Arc<AtomicUsize>) {
        let reads = Arc::new(AtomicUsize::new(0));
        let runtime = RuntimeContextClient::with_rpc(
            FrozenVersions {
                versions,
                reads: reads.clone(),
            },
            RuntimeContextConfig {
                origin: "https://content.internal".to_owned(),
                deadline: Duration::from_secs(1),
                max_response_bytes: 32 * 1024,
                max_application_response_bytes: 1024 * 1024,
                max_attachment_response_bytes: 1024 * 1024,
                max_artifact_response_bytes: 2 * 1024 * 1024,
            },
        )
        .unwrap();
        (PlatformClient::new(Arc::new(runtime)), reads)
    }

    fn version(child: Option<(u64, u64)>) -> Map<String, Value> {
        let instructions = if child.is_some() {
            "state: {answer: str}\nentry_point: delegate\nnodes:\n  - id: delegate\n    type: agent\n    tool: child\n    input_mapping: {task: {type: fixed, value: work}}\n    output: [answer]\n    transition: END\n"
        } else {
            "state: {answer: str}\nentry_point: answer\nnodes:\n  - id: answer\n    type: state_modifier\n    template: done\n    output: [answer]\n    transition: END\n"
        };
        let tools = child.map_or_else(
            || json!([]),
            |identity| {
                json!([{"id":44,"type":"application",
            "name":"child", "toolkit_name":"child", "agent_type":"pipeline", "author_id":11,
            "settings":{"application_id":identity.0,"application_version_id":identity.1},
            "meta":{},"variables":[],"is_pinned":false,"created_at":"2026-08-21T10:00:00Z"}])
            },
        );
        json!({"agent_type":"pipeline","instructions":instructions,"meta":{},"variables":[],"tools":tools,"llm_settings":null})
            .as_object().unwrap().clone()
    }

    fn branches(children: &[(u64, u64)], node_count: usize) -> Map<String, Value> {
        let mut tools = Vec::new();
        for (index, identity) in children.iter().enumerate() {
            tools.push(
                json!({"id":44+index,"type":"application", "name":format!("child_{index}"),
                "toolkit_name":format!("child_{index}"),"agent_type":"pipeline","author_id":11,
                "settings":{"application_id":identity.0,"application_version_id":identity.1},
                "meta":{},"variables":[],"is_pinned":false,"created_at":"2026-08-21T10:00:00Z"}),
            );
        }
        let mut instructions = "state: {answer: str}\nentry_point: node_0\nnodes:\n".to_owned();
        for index in 0..node_count {
            let alias = index % children.len();
            let next = if index + 1 == node_count {
                "END".to_owned()
            } else {
                format!("node_{}", index + 1)
            };
            writeln!(instructions, "  - id: node_{index}\n    type: agent\n    tool: child_{alias}\n    input_mapping: {{task: {{type: fixed, value: work}}}}\n    output: [answer]\n    transition: {next}").unwrap();
        }
        json!({"agent_type":"pipeline","instructions":instructions,"meta":{},"variables":[],"tools":tools,"llm_settings":null})
            .as_object().unwrap().clone()
    }

    #[tokio::test]
    async fn diamond_reuses_exact_version_fetch_and_bounds_hops_and_paths_before_binding() {
        let policy = ToolAdmissionPolicy::new(&[], &BTreeMap::new()).unwrap();
        let fallback = fallback();
        let authority = test_runtime_context_authority();
        let versions = BTreeMap::from([
            ((3, 4), branches(&[(5, 6), (7, 8)], 2)),
            ((5, 6), version(Some((9, 10)))),
            ((7, 8), version(Some((9, 10)))),
            ((9, 10), version(None)),
        ]);
        let (platform, reads) = client(versions);
        let admitted = admit_tool_composition(
            &platform,
            &authority,
            17,
            &policy,
            (3, 4),
            Some(17),
            &fallback,
        )
        .await
        .unwrap();
        assert_eq!(checkpoint_catalog(&admitted).unwrap().descendants.len(), 4);
        assert_eq!(reads.load(Ordering::SeqCst), 4);

        let identities = (0..26)
            .map(|index| (100 + index * 2, 101 + index * 2))
            .collect::<Vec<_>>();
        let mut versions = identities
            .iter()
            .map(|identity| (*identity, version(None)))
            .collect::<BTreeMap<_, _>>();
        versions.insert((3, 4), branches(&identities, 26));
        let (platform, reads) = client(versions);
        let error = admit_tool_composition(
            &platform,
            &authority,
            17,
            &policy,
            (3, 4),
            Some(17),
            &fallback,
        )
        .await
        .err()
        .unwrap();
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::ResourceExhausted
        );
        assert_eq!(reads.load(Ordering::SeqCst), 25);

        let (platform, reads) = client(BTreeMap::from([
            ((3, 4), branches(&[(5, 6)], 16)),
            ((5, 6), branches(&[(7, 8)], 16)),
            ((7, 8), version(None)),
        ]));
        let error = admit_tool_composition(
            &platform,
            &authority,
            17,
            &policy,
            (3, 4),
            Some(17),
            &fallback,
        )
        .await
        .err()
        .unwrap();
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::ResourceExhausted
        );
        assert_eq!(reads.load(Ordering::SeqCst), 3);
    }

    fn fallback() -> OrdinaryNoToolProfile {
        OrdinaryNoToolProfile::validate(&crate::agents::assembly_tests::ordinary_request(
            crate::agents::request::AgentExecutionKind::Application,
        ))
        .unwrap()
    }

    #[tokio::test]
    async fn admits_exact_three_wrapper_lineage_without_runtime_binding() {
        let versions = BTreeMap::from([
            ((3, 4), version(Some((5, 6)))),
            ((5, 6), version(Some((7, 8)))),
            ((7, 8), version(Some((9, 10)))),
            ((9, 10), version(None)),
        ]);
        let (platform, reads) = client(versions);
        let fallback = fallback();
        let policy = ToolAdmissionPolicy::new(&[], &BTreeMap::new()).unwrap();
        let admitted = admit_tool_composition(
            &platform,
            &test_runtime_context_authority(),
            17,
            &policy,
            (3, 4),
            Some(17),
            &fallback,
        )
        .await
        .unwrap();
        let catalog = checkpoint_catalog(&admitted).unwrap();
        assert_eq!(
            catalog.descendants.keys().cloned().collect::<Vec<_>>(),
            [
                "delegate",
                "delegate/delegate",
                "delegate/delegate/delegate"
            ]
        );
        assert_eq!(catalog.descendants["delegate/delegate"].application_id, 7);
        assert_eq!(reads.load(Ordering::SeqCst), 4);
    }

    #[tokio::test]
    async fn rejects_cycles_depth_unavailable_and_foreign_scope_before_binding() {
        let policy = ToolAdmissionPolicy::new(&[], &BTreeMap::new()).unwrap();
        let fallback = fallback();
        let (platform, reads) = client(BTreeMap::from([((3, 4), version(Some((3, 4))))]));
        let error = admit_tool_composition(
            &platform,
            &test_runtime_context_authority(),
            17,
            &policy,
            (3, 4),
            Some(17),
            &fallback,
        )
        .await
        .err()
        .unwrap();
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::InvalidConfiguration
        );
        assert_eq!(reads.load(Ordering::SeqCst), 1);
        let (platform, reads) = client(BTreeMap::new());
        assert!(
            admit_tool_composition(
                &platform,
                &test_runtime_context_authority(),
                17,
                &policy,
                (3, 4),
                Some(18),
                &fallback
            )
            .await
            .is_err()
        );
        assert_eq!(reads.load(Ordering::SeqCst), 0);
        assert!(
            admit_tool_composition(
                &platform,
                &test_runtime_context_authority(),
                17,
                &policy,
                (3, 4),
                Some(17),
                &fallback
            )
            .await
            .is_err()
        );
        let versions = (0..5)
            .map(|index| {
                (
                    (3 + index * 2, 4 + index * 2),
                    version(Some((5 + index * 2, 6 + index * 2))),
                )
            })
            .collect();
        let (platform, reads) = client(versions);
        let error = admit_tool_composition(
            &platform,
            &test_runtime_context_authority(),
            17,
            &policy,
            (3, 4),
            Some(17),
            &fallback,
        )
        .await
        .err()
        .unwrap();
        assert_eq!(
            error.code(),
            NativeAgentAssemblyErrorCode::ResourceExhausted
        );
        assert_eq!(reads.load(Ordering::SeqCst), 4);
    }
}
