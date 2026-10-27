//! The governed operation client: one redemption, one fresh provider client
//! and one provider call per tool invocation.

use std::collections::BTreeMap;
use std::sync::Arc;
use std::time::{SystemTime, UNIX_EPOCH};

use adk_core::{ReadonlyContext, Result as AdkResult, Tool, ToolContext, Toolset};
use async_trait::async_trait;
use ring::rand::{SecureRandom as _, SystemRandom};
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use super::{
    AuthAdapter, AuthAdapterRegistry, BoundToolInvocation, CredentialEnvelope, CredentialRedeemer,
    EnvelopeKind, GovernedBuild, GovernedError, RedeemOutcome, admit_material,
};
use crate::toolkits::delegated_auth::{
    DelegatedAuthorizationRequirement, delegated_authorization_error,
};
use crate::toolkits::families::attachment_host::AttachmentHost;
use crate::toolkits::families::office_host::SharedOfficeToolHost;
use crate::toolkits::policy::ToolAdmissionPolicy;

/// The value every key of the DECLARATION envelope carries. The declaration
/// client only lists the family's tools; it never executes one.
const DECLARATION_MATERIAL: &str = "elitea-governed-declaration";

/// The clock `not_after` is read against, in Unix milliseconds.
pub type Clock = fn() -> i64;

fn system_clock() -> i64 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .ok()
        .and_then(|elapsed| i64::try_from(elapsed.as_millis()).ok())
        .unwrap_or(i64::MAX)
}

/// The run's governed seam: its adapters and the redeemer of its live claim.
/// One per governed run; never shared between runs, never static.
pub struct GovernedOperationClient {
    registry: Arc<AuthAdapterRegistry>,
    redeemer: Arc<dyn CredentialRedeemer>,
    clock: Clock,
}

impl GovernedOperationClient {
    #[must_use]
    pub fn new(registry: Arc<AuthAdapterRegistry>, redeemer: Arc<dyn CredentialRedeemer>) -> Self {
        Self {
            registry,
            redeemer,
            clock: system_clock,
        }
    }

    /// The same client over another clock (expiry tests).
    #[must_use]
    pub fn with_clock(mut self, clock: Clock) -> Self {
        self.clock = clock;
        self
    }

    #[must_use]
    pub fn registry(&self) -> &AuthAdapterRegistry {
        &self.registry
    }

    /// The governed toolset of one configured toolkit.
    ///
    /// # Errors
    ///
    /// [`GovernedError::ToolkitNotGovernable`] when no adapter governs the
    /// type; [`GovernedError::InvalidConfiguration`] when the configured
    /// target or the declaration client cannot be built.
    pub fn toolset(
        self: &Arc<Self>,
        spec: &GovernedToolkitSpec<'_>,
    ) -> Result<Arc<dyn Toolset>, GovernedError> {
        let adapter = Arc::clone(
            self.registry
                .adapter(spec.toolkit_type)
                .ok_or(GovernedError::ToolkitNotGovernable)?,
        );
        let target = adapter.configured_target(spec.settings)?;
        let settings = spec.settings.clone();
        let declaration = declaration_envelope(adapter.as_ref(), &target)?;
        let declarations = adapter.build_client(
            &GovernedBuild::new(
                spec.toolkit_name,
                &settings,
                spec.policy,
                spec.attachments,
                spec.office,
            ),
            &declaration,
        )?;
        drop(declaration);
        let toolkit = Arc::new(GovernedToolkit {
            adapter,
            name: spec.toolkit_name.to_owned(),
            toolkit_id: spec
                .toolkit_id
                .and_then(|id| i64::try_from(id).ok())
                .unwrap_or(0),
            credential_reference_id: credential_reference_id(spec.settings),
            target,
            settings,
            policy: Arc::clone(spec.policy),
            attachments: spec.attachments.cloned(),
            office: spec.office.cloned(),
        });
        Ok(Arc::new(GovernedToolset {
            client: Arc::clone(self),
            toolkit,
            declarations,
        }))
    }

    /// One governed invocation: map, redeem, check, build fresh, call, drop.
    async fn invoke(
        &self,
        toolkit: &GovernedToolkit,
        tool_name: &str,
        ctx: Arc<dyn ToolContext>,
        args: Value,
    ) -> AdkResult<Value> {
        let adapter = toolkit.adapter.as_ref();
        // 1. The operation, before anything leaves the process.
        let operation = adapter
            .operation_for(tool_name, &args)
            .map_err(|_| GovernedError::OperationNotMapped.into_adk_error())?;
        let invocation = BoundToolInvocation {
            invocation_id: uuid_v7(self.clock).map_err(GovernedError::into_adk_error)?,
            tool_call_id: ctx.function_call_id().to_owned(),
            toolkit_id: toolkit.toolkit_id,
            operation_id: operation.operation_id().to_owned(),
            resource_ref: operation.resource_ref().to_owned(),
            arguments_digest: *operation.arguments_digest(),
            credential_reference_id: toolkit.credential_reference_id.clone(),
        };
        // 2. One redemption for this invocation.
        let envelope = match self.redeemer.redeem(invocation).await {
            Ok(RedeemOutcome::Envelope(envelope)) => envelope,
            Ok(RedeemOutcome::Denial(denial)) => {
                if denial.code() == "AUTHORIZATION_REQUIRED"
                    && let Some(reference) = denial.authorization_reference()
                    && let Some(requirement) = DelegatedAuthorizationRequirement::governed(
                        toolkit.name.clone(),
                        adapter.provider_type().to_owned(),
                        toolkit.target.clone(),
                        reference.to_owned(),
                    )
                {
                    return Err(delegated_authorization_error(&requirement));
                }
                return Err(
                    GovernedError::CredentialDenied(denial.code().to_owned()).into_adk_error()
                );
            }
            Err(failure) => {
                return Err(GovernedError::RedeemUnavailable {
                    code: failure.code(),
                    retryable: failure.retryable(),
                }
                .into_adk_error());
            }
        };
        // 3. Target, expiry and material, before any provider I/O.
        check_envelope(adapter, &envelope, &toolkit.target, (self.clock)())
            .map_err(GovernedError::into_adk_error)?;
        // 4. A fresh client from this envelope alone; the envelope is dropped
        //    as soon as the client holds its copy.
        let client = adapter
            .build_client(
                &GovernedBuild::new(
                    &toolkit.name,
                    &toolkit.settings,
                    &toolkit.policy,
                    toolkit.attachments.as_ref(),
                    toolkit.office.as_ref(),
                ),
                &envelope,
            )
            .map_err(GovernedError::into_adk_error)?;
        drop(envelope);
        let readonly: Arc<dyn ReadonlyContext> = ctx.clone();
        let tool = client
            .tools(readonly)
            .await?
            .into_iter()
            .find(|tool| tool.name() == tool_name)
            .ok_or_else(|| GovernedError::ToolNotServed.into_adk_error())?;
        // 5. The call; the client and its material drop with this frame.
        let result = tool.execute(ctx, args).await;
        drop(tool);
        drop(client);
        result
    }
}

/// The inputs of one governed toolkit.
pub struct GovernedToolkitSpec<'a> {
    pub toolkit_name: &'a str,
    pub toolkit_type: &'a str,
    pub toolkit_id: Option<u64>,
    pub settings: &'a Map<String, Value>,
    pub policy: &'a Arc<ToolAdmissionPolicy>,
    pub(crate) attachments: Option<&'a AttachmentHost>,
    pub office: Option<&'a SharedOfficeToolHost>,
}

impl<'a> GovernedToolkitSpec<'a> {
    #[must_use]
    pub const fn new(
        toolkit_name: &'a str,
        toolkit_type: &'a str,
        toolkit_id: Option<u64>,
        settings: &'a Map<String, Value>,
        policy: &'a Arc<ToolAdmissionPolicy>,
    ) -> Self {
        Self {
            toolkit_name,
            toolkit_type,
            toolkit_id,
            settings,
            policy,
            attachments: None,
            office: None,
        }
    }

    #[must_use]
    pub(crate) const fn with_hosts(
        mut self,
        attachments: Option<&'a AttachmentHost>,
        office: Option<&'a SharedOfficeToolHost>,
    ) -> Self {
        self.attachments = attachments;
        self.office = office;
        self
    }
}

/// The toolkit's credential reference, when its frozen settings name one
/// (top level, then the configuration object). Empty lets Main resolve the
/// grant by agent and toolkit.
fn credential_reference_id(settings: &Map<String, Value>) -> String {
    const KEY: &str = "credential_reference_id";
    settings
        .get(KEY)
        .and_then(Value::as_str)
        .or_else(|| {
            settings
                .iter()
                .filter(|(name, _)| name.ends_with("_configuration"))
                .find_map(|(_, value)| value.get(KEY).and_then(Value::as_str))
        })
        .filter(|value| {
            value.len() <= 64
                && value
                    .bytes()
                    .all(|byte| byte.is_ascii_alphanumeric() || byte == b'-')
        })
        .unwrap_or_default()
        .to_owned()
}

struct GovernedToolkit {
    adapter: Arc<dyn AuthAdapter>,
    name: String,
    toolkit_id: i64,
    credential_reference_id: String,
    target: String,
    settings: Map<String, Value>,
    policy: Arc<ToolAdmissionPolicy>,
    attachments: Option<AttachmentHost>,
    office: Option<SharedOfficeToolHost>,
}

/// The toolkit's tools as the model sees them; every call goes through
/// [`GovernedOperationClient::invoke`].
struct GovernedToolset {
    client: Arc<GovernedOperationClient>,
    toolkit: Arc<GovernedToolkit>,
    declarations: Arc<dyn Toolset>,
}

#[async_trait]
impl Toolset for GovernedToolset {
    fn name(&self) -> &str {
        self.declarations.name()
    }

    async fn tools(&self, ctx: Arc<dyn ReadonlyContext>) -> AdkResult<Vec<Arc<dyn Tool>>> {
        Ok(self
            .declarations
            .tools(ctx)
            .await?
            .into_iter()
            .map(|declaration| {
                Arc::new(GovernedTool {
                    client: Arc::clone(&self.client),
                    toolkit: Arc::clone(&self.toolkit),
                    declaration,
                }) as Arc<dyn Tool>
            })
            .collect())
    }
}

struct GovernedTool {
    client: Arc<GovernedOperationClient>,
    toolkit: Arc<GovernedToolkit>,
    declaration: Arc<dyn Tool>,
}

#[async_trait]
impl Tool for GovernedTool {
    fn name(&self) -> &str {
        self.declaration.name()
    }

    fn description(&self) -> &str {
        self.declaration.description()
    }

    fn declaration(&self) -> Value {
        self.declaration.declaration()
    }

    fn enhanced_description(&self) -> String {
        self.declaration.enhanced_description()
    }

    fn is_long_running(&self) -> bool {
        self.declaration.is_long_running()
    }

    fn parameters_schema(&self) -> Option<Value> {
        self.declaration.parameters_schema()
    }

    fn response_schema(&self) -> Option<Value> {
        self.declaration.response_schema()
    }

    fn required_scopes(&self) -> &[&str] {
        self.declaration.required_scopes()
    }

    fn is_read_only(&self) -> bool {
        self.declaration.is_read_only()
    }

    fn is_concurrency_safe(&self) -> bool {
        self.declaration.is_concurrency_safe()
    }

    async fn execute(&self, ctx: Arc<dyn ToolContext>, args: Value) -> AdkResult<Value> {
        self.client
            .invoke(&self.toolkit, self.declaration.name(), ctx, args)
            .await
    }
}

/// Refuse an envelope for another target, an expired one, or one whose
/// material the adapter's class does not allow. Pure: no I/O.
pub(crate) fn check_envelope(
    adapter: &dyn AuthAdapter,
    envelope: &CredentialEnvelope,
    target: &str,
    now_unix_millis: i64,
) -> Result<(), GovernedError> {
    if envelope.target_identity() != target {
        return Err(GovernedError::TargetMismatch);
    }
    if envelope.not_after_unix_millis() <= now_unix_millis {
        return Err(GovernedError::EnvelopeExpired);
    }
    admit_material(adapter, envelope).map(|_| ())
}

/// The declaration envelope: the adapter's first accepted class with a
/// fixed, non-secret value under each of its keys.
fn declaration_envelope(
    adapter: &dyn AuthAdapter,
    target: &str,
) -> Result<CredentialEnvelope, GovernedError> {
    let (kind, keys) = [
        (EnvelopeKind::Static, super::CredentialClass::StaticSecret),
        (
            EnvelopeKind::OauthAccess,
            super::CredentialClass::OauthDelegated,
        ),
    ]
    .into_iter()
    .map(|(kind, class)| (kind, adapter.material_keys(class)))
    .find(|(_, keys)| !keys.is_empty())
    .ok_or(GovernedError::ClassNotSupported)?;
    let material: BTreeMap<String, Zeroizing<String>> = keys
        .iter()
        .map(|key| {
            (
                (*key).to_owned(),
                Zeroizing::new(DECLARATION_MATERIAL.to_owned()),
            )
        })
        .collect();
    Ok(CredentialEnvelope::new(
        String::new(),
        kind,
        material,
        i64::MAX,
        target.to_owned(),
    ))
}

/// A UUID v7 (RFC 9562): 48 bits of Unix milliseconds, version 7, 74 random
/// bits, variant `10`.
pub(crate) fn uuid_v7(clock: Clock) -> Result<String, GovernedError> {
    let mut bytes = [0u8; 16];
    SystemRandom::new()
        .fill(&mut bytes[6..])
        .map_err(|_| GovernedError::RedeemUnavailable {
            code: "governed.random_unavailable",
            retryable: true,
        })?;
    let millis = u64::try_from(clock()).unwrap_or(0) & 0xFFFF_FFFF_FFFF;
    bytes[..6].copy_from_slice(&millis.to_be_bytes()[2..]);
    bytes[6] = (bytes[6] & 0x0F) | 0x70;
    bytes[8] = (bytes[8] & 0x3F) | 0x80;
    let hex = bytes
        .iter()
        .fold(String::with_capacity(32), |mut hex, byte| {
            use std::fmt::Write as _;
            let _ = write!(hex, "{byte:02x}");
            hex
        });
    Ok(format!(
        "{}-{}-{}-{}-{}",
        &hex[..8],
        &hex[8..12],
        &hex[12..16],
        &hex[16..20],
        &hex[20..]
    ))
}
