//! Governed credential use for configured toolkits (ADR-0039 decisions 6, 8
//! and 10, issue #1120).
//!
//! On a governed run no toolkit holds a credential from claim time. Each tool
//! call is bound to ONE canonical operation by the toolkit's [`AuthAdapter`],
//! redeemed for that one invocation through a [`CredentialRedeemer`] (the
//! worker implements it over `RuntimeContentService.RedeemCredential`, under
//! the live claim), checked against the toolkit's configured target and the
//! envelope's expiry before any provider I/O, and served by a provider client
//! built FRESH from the envelope and dropped with it when the call returns.
//!
//! The seam is toolkit-agnostic: this module and [`client`] name no provider.
//! The reference adapters live in [`adapters`]; another adapter registers
//! through [`AuthAdapterRegistryBuilder::register`] from its own module, with
//! no edit here (the synthetic-adapter suite proves it).
//!
//! Nothing here caches a provider client. Every invocation builds its own from
//! the envelope minted for it, so two runs (or two calls of one run) never
//! share a client instance, and no client outlives the material it was built
//! from. A [`CredentialEnvelope`] is neither `Clone` nor serialisable, and its
//! `Debug` shows no material.
//!
//! Canonical target rule (shared with the Go descriptors): an `https` URL
//! written as `scheme://host[:port][/path]`, host lowercased, no trailing
//! slash, no query, no fragment; a `base_url` target is the toolkit's
//! configured base URL in that form, a `tenant_site` target the configured
//! `SharePoint` site URL (site path applied) in that form.

use std::collections::BTreeMap;
use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Toolset};
use async_trait::async_trait;
use serde_json::{Map, Value};
use zeroize::Zeroizing;

use super::families::attachment_host::AttachmentHost;
use super::families::office_host::SharedOfficeToolHost;
use super::policy::ToolAdmissionPolicy;

pub mod adapters;
pub mod client;

pub use client::GovernedOperationClient;

/// The bound of one canonical resource reference.
const MAX_RESOURCE_REF_BYTES: usize = 512;
/// The bound of one material value.
const MAX_MATERIAL_VALUE_BYTES: usize = 16 * 1_024;
/// The bound of an opaque authorization reference.
const MAX_AUTHORIZATION_REFERENCE_BYTES: usize = 512;

/// A credential class as the operation catalogue names it.
#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum CredentialClass {
    StaticSecret,
    OauthDelegated,
}

impl CredentialClass {
    pub const ALL: [Self; 2] = [Self::StaticSecret, Self::OauthDelegated];

    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::StaticSecret => "static_secret",
            Self::OauthDelegated => "oauth_delegated",
        }
    }
}

/// `CredentialEnvelopeKindV1`.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EnvelopeKind {
    Static,
    OauthAccess,
    DynamicLease,
}

impl EnvelopeKind {
    /// The catalogue class this kind of material belongs to. A dynamic lease
    /// has no reference provider class yet.
    #[must_use]
    pub const fn class(self) -> Option<CredentialClass> {
        match self {
            Self::Static => Some(CredentialClass::StaticSecret),
            Self::OauthAccess => Some(CredentialClass::OauthDelegated),
            Self::DynamicLease => None,
        }
    }
}

/// The catalogue's target kind of a provider.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum TargetKind {
    BaseUrl,
    TenantSite,
    /// A remote MCP server's canonical resource URI (#1123); see
    /// [`canonical_resource_uri`].
    ResourceUri,
}

impl TargetKind {
    #[must_use]
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::BaseUrl => "base_url",
            Self::TenantSite => "tenant_site",
            Self::ResourceUri => "resource_uri",
        }
    }
}

/// The material for ONE invocation (`CredentialEnvelopeV1`). Held in memory
/// for that call only: not `Clone`, not serialisable, zeroized on drop, and
/// its `Debug` names the keys' count, never a key's value.
pub struct CredentialEnvelope {
    redemption_id: String,
    kind: EnvelopeKind,
    material: BTreeMap<String, Zeroizing<String>>,
    not_after_unix_millis: i64,
    target_identity: String,
    obligations: Vec<String>,
    decision_id: String,
}

impl CredentialEnvelope {
    #[must_use]
    pub fn new(
        redemption_id: String,
        kind: EnvelopeKind,
        material: BTreeMap<String, Zeroizing<String>>,
        not_after_unix_millis: i64,
        target_identity: String,
    ) -> Self {
        Self {
            redemption_id,
            kind,
            material,
            not_after_unix_millis,
            target_identity,
            obligations: Vec::new(),
            decision_id: String::new(),
        }
    }

    #[must_use]
    pub fn with_obligations(mut self, obligations: Vec<String>) -> Self {
        self.obligations = obligations;
        self
    }

    #[must_use]
    pub fn with_decision_id(mut self, decision_id: String) -> Self {
        self.decision_id = decision_id;
        self
    }

    #[must_use]
    pub fn redemption_id(&self) -> &str {
        &self.redemption_id
    }

    #[must_use]
    pub const fn kind(&self) -> EnvelopeKind {
        self.kind
    }

    /// One material value; `None` when the envelope does not carry the key.
    #[must_use]
    pub fn material(&self, key: &str) -> Option<&str> {
        self.material.get(key).map(|value| value.as_str())
    }

    pub fn material_keys(&self) -> impl Iterator<Item = &str> {
        self.material.keys().map(String::as_str)
    }

    #[must_use]
    pub const fn not_after_unix_millis(&self) -> i64 {
        self.not_after_unix_millis
    }

    #[must_use]
    pub fn target_identity(&self) -> &str {
        &self.target_identity
    }

    #[must_use]
    pub fn obligations(&self) -> &[String] {
        &self.obligations
    }

    #[must_use]
    pub fn decision_id(&self) -> &str {
        &self.decision_id
    }
}

impl fmt::Debug for CredentialEnvelope {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CredentialEnvelope")
            .field("kind", &self.kind)
            .field(
                "material",
                &format_args!("<{} redacted>", self.material.len()),
            )
            .field("not_after_unix_millis", &self.not_after_unix_millis)
            .finish_non_exhaustive()
    }
}

/// `CredentialRedeemDenialV1`: a refusal with a safe, stable code.
pub struct CredentialRedeemDenial {
    code: String,
    decision_id: String,
    authorization_reference: String,
}

/// The denial codes the contract names. Anything else reads as `DENIED`.
pub const DENIAL_CODES: [&str; 7] = [
    "DENIED",
    "INVOCATION_MISMATCH",
    "CLASS_NOT_SUPPORTED",
    "BACKEND_UNAVAILABLE",
    "AUTHORIZATION_REQUIRED",
    "CONTEXT_MISMATCH",
    "NOT_GOVERNED",
];

impl CredentialRedeemDenial {
    #[must_use]
    pub fn new(code: &str, decision_id: String, authorization_reference: String) -> Self {
        let code = DENIAL_CODES
            .iter()
            .find(|known| **known == code)
            .copied()
            .unwrap_or("DENIED");
        Self {
            code: code.to_owned(),
            decision_id,
            authorization_reference,
        }
    }

    /// One of [`DENIAL_CODES`].
    #[must_use]
    pub fn code(&self) -> &str {
        &self.code
    }

    #[must_use]
    pub fn decision_id(&self) -> &str {
        &self.decision_id
    }

    /// The opaque reference of an `AUTHORIZATION_REQUIRED` denial, when it is
    /// a bounded printable token; `None` otherwise.
    #[must_use]
    pub fn authorization_reference(&self) -> Option<&str> {
        let reference = self.authorization_reference.as_str();
        (!reference.is_empty()
            && reference.len() <= MAX_AUTHORIZATION_REFERENCE_BYTES
            && reference.bytes().all(|byte| (0x21..=0x7e).contains(&byte)))
        .then_some(reference)
    }
}

impl fmt::Debug for CredentialRedeemDenial {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("CredentialRedeemDenial")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

/// `RedeemCredentialResponseV1.result`.
#[derive(Debug)]
pub enum RedeemOutcome {
    Envelope(CredentialEnvelope),
    Denial(CredentialRedeemDenial),
}

/// `BoundToolInvocationV1`, as the worker sends it.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BoundToolInvocation {
    pub invocation_id: String,
    pub tool_call_id: String,
    pub toolkit_id: i64,
    pub operation_id: String,
    pub resource_ref: String,
    /// SHA-256 over the call's canonical JSON arguments.
    pub arguments_digest: [u8; 32],
    pub credential_reference_id: String,
}

/// A transport or binding failure of the redeem RPC (a gRPC status, not a
/// policy refusal). Carries a stable code only.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct RedeemFailure {
    code: &'static str,
    retryable: bool,
}

impl RedeemFailure {
    #[must_use]
    pub const fn new(code: &'static str, retryable: bool) -> Self {
        Self { code, retryable }
    }

    #[must_use]
    pub const fn code(self) -> &'static str {
        self.code
    }

    #[must_use]
    pub const fn retryable(self) -> bool {
        self.retryable
    }
}

/// The host side of redemption: one `RedeemCredential` call under the live
/// claim and the run's security context digest. The worker implements it;
/// a test implements it with a recording fake.
#[async_trait]
pub trait CredentialRedeemer: Send + Sync {
    async fn redeem(&self, invocation: BoundToolInvocation)
    -> Result<RedeemOutcome, RedeemFailure>;
}

/// The one operation a tool call performs.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BoundOperation {
    operation_id: &'static str,
    resource_ref: String,
    arguments_digest: [u8; 32],
}

impl BoundOperation {
    /// Bind `operation_id` on `resource_ref` to the digest of `arguments`.
    ///
    /// # Errors
    ///
    /// [`OperationNotMapped`] when the arguments cannot be encoded.
    pub fn new(
        operation_id: &'static str,
        resource_ref: String,
        arguments: &Value,
    ) -> Result<Self, OperationNotMapped> {
        let encoded = crate::canonical::to_vec(arguments).map_err(|_| OperationNotMapped)?;
        let digest = ring::digest::digest(&ring::digest::SHA256, &encoded);
        let arguments_digest = digest.as_ref().try_into().map_err(|_| OperationNotMapped)?;
        Ok(Self {
            operation_id,
            resource_ref,
            arguments_digest,
        })
    }

    #[must_use]
    pub const fn operation_id(&self) -> &'static str {
        self.operation_id
    }

    #[must_use]
    pub fn resource_ref(&self) -> &str {
        &self.resource_ref
    }

    #[must_use]
    pub const fn arguments_digest(&self) -> &[u8; 32] {
        &self.arguments_digest
    }
}

/// A tool the adapter has no canonical operation for: denied, never
/// redeemed, never widened to a broader call.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct OperationNotMapped;

impl fmt::Display for OperationNotMapped {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("the tool is not mapped to a governed operation")
    }
}

impl std::error::Error for OperationNotMapped {}

/// One row of an adapter's tool table: the tool, its canonical operation,
/// the catalogue resource kind, and the ONE argument the tool acts on.
///
/// The resource is bound only from `id_argument`, the argument the family
/// itself reads for the resource it touches. A tool whose touched resource
/// cannot be told from its arguments (a query, a list of ids, a create) has
/// no `id_argument` and binds an empty resource, which Main's scope gate
/// refuses for a scoped grant. `unbound_with` names arguments whose presence
/// makes the touched resource undeterminable too (the family then acts on
/// something other than, or more than, `id_argument`).
#[derive(Clone, Copy, Debug)]
pub struct ToolOperation {
    pub tool: &'static str,
    pub operation_id: &'static str,
    pub resource_kind: &'static str,
    pub id_argument: Option<&'static str>,
    pub unbound_with: &'static [&'static str],
}

impl ToolOperation {
    /// A row whose resource is `id_argument` (`None`: undeterminable).
    #[must_use]
    pub const fn new(
        tool: &'static str,
        operation_id: &'static str,
        resource_kind: &'static str,
        id_argument: Option<&'static str>,
    ) -> Self {
        Self {
            tool,
            operation_id,
            resource_kind,
            id_argument,
            unbound_with: &[],
        }
    }

    /// The same row, binding an empty resource when any of `arguments` is
    /// present.
    #[must_use]
    pub const fn unbound_with(mut self, arguments: &'static [&'static str]) -> Self {
        self.unbound_with = arguments;
        self
    }

    /// The resource this row binds for `arguments`: `<provider>:<kind>/<id>`
    /// from `id_argument`, or empty when the touched resource cannot be
    /// determined.
    #[must_use]
    pub fn resource_ref(&self, provider_type: &str, arguments: &Value) -> String {
        if self
            .unbound_with
            .iter()
            .any(|name| arguments.get(*name).is_some_and(|value| !value.is_null()))
        {
            return String::new();
        }
        self.id_argument
            .and_then(|name| resource_identifier(arguments.get(name)?))
            .map(|id| self.resource(provider_type, &id))
            .unwrap_or_default()
    }

    /// `<provider>:<kind>/<id>`, or empty when it would be over the bound.
    #[must_use]
    pub fn resource(&self, provider_type: &str, id: &str) -> String {
        let resource = format!("{provider_type}:{}/{id}", self.resource_kind);
        if resource.len() > MAX_RESOURCE_REF_BYTES {
            String::new()
        } else {
            resource
        }
    }
}

/// What an adapter needs to build a provider client for one toolkit: the
/// frozen settings (never trusted for a credential on a governed run), the
/// admission policy and the platform hosts this path lends.
pub struct GovernedBuild<'a> {
    toolkit_name: &'a str,
    settings: &'a Map<String, Value>,
    policy: &'a Arc<ToolAdmissionPolicy>,
    attachments: Option<&'a AttachmentHost>,
    office: Option<&'a SharedOfficeToolHost>,
}

impl<'a> GovernedBuild<'a> {
    pub(crate) const fn new(
        toolkit_name: &'a str,
        settings: &'a Map<String, Value>,
        policy: &'a Arc<ToolAdmissionPolicy>,
        attachments: Option<&'a AttachmentHost>,
        office: Option<&'a SharedOfficeToolHost>,
    ) -> Self {
        Self {
            toolkit_name,
            settings,
            policy,
            attachments,
            office,
        }
    }

    #[must_use]
    pub const fn toolkit_name(&self) -> &'a str {
        self.toolkit_name
    }

    #[must_use]
    pub const fn settings(&self) -> &'a Map<String, Value> {
        self.settings
    }

    #[must_use]
    pub const fn policy(&self) -> &'a Arc<ToolAdmissionPolicy> {
        self.policy
    }

    #[must_use]
    pub const fn office(&self) -> Option<&'a SharedOfficeToolHost> {
        self.office
    }

    pub(crate) const fn attachments(&self) -> Option<&'a AttachmentHost> {
        self.attachments
    }
}

/// One provider family's governed seam.
pub trait AuthAdapter: Send + Sync {
    /// The toolkit type this adapter governs (`jira`, `sharepoint`, ...).
    fn provider_type(&self) -> &'static str;

    fn target_kind(&self) -> TargetKind;

    /// The envelope keys this adapter reads for `class`; empty when the
    /// adapter does not accept the class.
    fn material_keys(&self, class: CredentialClass) -> &'static [&'static str];

    /// The catalogue's probe operation.
    fn probe_operation(&self) -> &'static str;

    /// The tools this adapter maps. A tool not listed is not mapped.
    fn tool_operations(&self) -> &'static [ToolOperation];

    /// The canonical configured target of a toolkit (see the module's
    /// target rule).
    ///
    /// # Errors
    ///
    /// [`GovernedError::InvalidConfiguration`] for a missing or malformed
    /// target.
    fn configured_target(&self, settings: &Map<String, Value>) -> Result<String, GovernedError>;

    /// Map one call to its operation. Unknown tool: [`OperationNotMapped`].
    ///
    /// # Errors
    ///
    /// [`OperationNotMapped`] for a tool the table does not list.
    fn operation_for(
        &self,
        tool_name: &str,
        arguments: &Value,
    ) -> Result<BoundOperation, OperationNotMapped> {
        table_operation(
            self.provider_type(),
            self.tool_operations(),
            tool_name,
            arguments,
        )
    }

    /// A FRESH provider toolset whose only credential is `envelope`. Called
    /// once per invocation; the result is dropped when the call returns.
    ///
    /// # Errors
    ///
    /// A [`GovernedError`] when the material or the configuration cannot
    /// build a client. The error never carries material.
    fn build_client(
        &self,
        build: &GovernedBuild<'_>,
        envelope: &CredentialEnvelope,
    ) -> Result<Arc<dyn Toolset>, GovernedError>;
}

/// The table mapping [`AuthAdapter::operation_for`] performs by default:
/// the tool's row, its resource ([`ToolOperation::resource_ref`]), and the
/// arguments' digest.
///
/// # Errors
///
/// [`OperationNotMapped`] for a tool `rows` does not list.
pub fn table_operation(
    provider_type: &str,
    rows: &[ToolOperation],
    tool_name: &str,
    arguments: &Value,
) -> Result<BoundOperation, OperationNotMapped> {
    let row = table_row(rows, tool_name)?;
    BoundOperation::new(
        row.operation_id,
        row.resource_ref(provider_type, arguments),
        arguments,
    )
}

/// The row `rows` lists for `tool_name`.
///
/// # Errors
///
/// [`OperationNotMapped`] for a tool `rows` does not list.
pub fn table_row<'a>(
    rows: &'a [ToolOperation],
    tool_name: &str,
) -> Result<&'a ToolOperation, OperationNotMapped> {
    rows.iter()
        .find(|row| row.tool == tool_name)
        .ok_or(OperationNotMapped)
}

/// A resource identifier argument as text: a non-empty string or a number,
/// bounded and free of control characters.
#[must_use]
pub fn resource_identifier(value: &Value) -> Option<String> {
    let text = match value {
        Value::String(text) => text.trim().to_owned(),
        Value::Number(number) => number.to_string(),
        _ => return None,
    };
    (!text.is_empty()
        && text.len() <= MAX_RESOURCE_REF_BYTES / 2
        && !text.chars().any(char::is_control))
    .then_some(text)
}

/// The adapters a composition root admitted. Immutable once built.
#[derive(Clone, Default)]
pub struct AuthAdapterRegistry {
    adapters: BTreeMap<&'static str, Arc<dyn AuthAdapter>>,
}

impl AuthAdapterRegistry {
    #[must_use]
    pub fn builder() -> AuthAdapterRegistryBuilder {
        AuthAdapterRegistryBuilder::default()
    }

    #[must_use]
    pub fn adapter(&self, provider_type: &str) -> Option<&Arc<dyn AuthAdapter>> {
        self.adapters.get(provider_type)
    }

    pub fn provider_types(&self) -> impl Iterator<Item = &'static str> + '_ {
        self.adapters.keys().copied()
    }
}

/// Builds an [`AuthAdapterRegistry`]; refuses a duplicate provider type.
#[derive(Default)]
pub struct AuthAdapterRegistryBuilder {
    adapters: BTreeMap<&'static str, Arc<dyn AuthAdapter>>,
}

/// A second adapter for a provider type already registered.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DuplicateAdapter(pub &'static str);

impl AuthAdapterRegistryBuilder {
    /// Add one adapter.
    ///
    /// # Errors
    ///
    /// [`DuplicateAdapter`] when its provider type is taken.
    pub fn register(mut self, adapter: Arc<dyn AuthAdapter>) -> Result<Self, DuplicateAdapter> {
        let provider = adapter.provider_type();
        if self.adapters.contains_key(provider) {
            return Err(DuplicateAdapter(provider));
        }
        self.adapters.insert(provider, adapter);
        Ok(self)
    }

    #[must_use]
    pub fn build(self) -> AuthAdapterRegistry {
        AuthAdapterRegistry {
            adapters: self.adapters,
        }
    }
}

/// Why a governed tool call or toolkit was refused. Every variant carries a
/// stable code; none carries material, a provider body or an argument.
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum GovernedError {
    /// The toolkit's type has no adapter: refused for governed runs.
    ToolkitNotGovernable,
    /// The tool has no canonical operation.
    OperationNotMapped,
    /// The envelope names another target than the toolkit's.
    TargetMismatch,
    /// The envelope's `not_after` has passed.
    EnvelopeExpired,
    /// The envelope's class is not one this adapter accepts.
    ClassNotSupported,
    /// The envelope carries a key the adapter's class does not allow, or
    /// lacks the one it needs.
    MaterialNotAllowed,
    /// Main refused the redemption with this stable code.
    CredentialDenied(String),
    /// The redeem RPC failed (transport, claim or fence).
    RedeemUnavailable { code: &'static str, retryable: bool },
    /// The toolkit's configuration cannot build a governed client.
    InvalidConfiguration,
    /// The fresh client did not serve the tool that was called.
    ToolNotServed,
}

impl GovernedError {
    /// The stable code, as the tool error's code.
    #[must_use]
    pub const fn code(&self) -> &'static str {
        match self {
            Self::ToolkitNotGovernable => "governed.toolkit_not_governable",
            Self::OperationNotMapped => "governed.operation_not_mapped",
            Self::TargetMismatch => "governed.target_mismatch",
            Self::EnvelopeExpired => "governed.envelope_expired",
            Self::ClassNotSupported => "governed.class_not_supported",
            Self::MaterialNotAllowed => "governed.material_not_allowed",
            Self::CredentialDenied(_) => "governed.credential_denied",
            Self::RedeemUnavailable { .. } => "governed.redeem_unavailable",
            Self::InvalidConfiguration => "governed.invalid_configuration",
            Self::ToolNotServed => "governed.tool_not_served",
        }
    }

    /// The error as the model and the run read it.
    #[must_use]
    pub fn into_adk_error(self) -> AdkError {
        let category = match &self {
            Self::ToolkitNotGovernable | Self::ClassNotSupported => ErrorCategory::Unsupported,
            Self::OperationNotMapped
            | Self::TargetMismatch
            | Self::MaterialNotAllowed
            | Self::CredentialDenied(_) => ErrorCategory::Forbidden,
            Self::EnvelopeExpired => ErrorCategory::Unauthorized,
            Self::RedeemUnavailable { .. } => ErrorCategory::Unavailable,
            Self::InvalidConfiguration => ErrorCategory::InvalidInput,
            Self::ToolNotServed => ErrorCategory::NotFound,
        };
        let message = self.to_string();
        AdkError::new(ErrorComponent::Tool, category, self.code(), message)
    }
}

impl fmt::Display for GovernedError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::ToolkitNotGovernable => formatter.write_str(
                "this toolkit has no governed credential adapter, so a governed run cannot use it",
            ),
            Self::OperationNotMapped => formatter.write_str(
                "this tool is not mapped to a governed operation, so a governed run cannot call it",
            ),
            Self::TargetMismatch => formatter
                .write_str("the redeemed credential is for another target than this toolkit's"),
            Self::EnvelopeExpired => formatter.write_str("the redeemed credential has expired"),
            Self::ClassNotSupported => formatter
                .write_str("the redeemed credential's class is not supported by this toolkit"),
            Self::MaterialNotAllowed => formatter
                .write_str("the redeemed credential does not match this toolkit's material keys"),
            Self::CredentialDenied(code) => {
                write!(formatter, "the credential use was refused: {code}")
            }
            Self::RedeemUnavailable { code, .. } => {
                write!(formatter, "the credential could not be redeemed: {code}")
            }
            Self::InvalidConfiguration => formatter
                .write_str("the toolkit configuration cannot build a governed provider client"),
            Self::ToolNotServed => {
                formatter.write_str("the governed provider client does not serve this tool")
            }
        }
    }
}

impl std::error::Error for GovernedError {}

/// Toolkit types that hold no provider credential (their authority is the
/// platform's own, lent by the host), served unchanged on a governed run.
#[must_use]
pub fn is_credential_free(tool_type: &str) -> bool {
    matches!(tool_type, "artifact" | "memory" | "pptx" | "docx" | "xlsx")
}

/// A URL in the canonical target form (see the module documentation).
#[must_use]
pub fn canonical_target(url: &reqwest::Url) -> String {
    let mut canonical = url.clone();
    canonical.set_query(None);
    canonical.set_fragment(None);
    if canonical.path() == "/" {
        canonical.set_path("");
    }
    canonical.to_string().trim_end_matches('/').to_owned()
}

/// The bound of a `resource_uri` target (the target binding column's).
const MAX_RESOURCE_URI_BYTES: usize = 512;

/// The canonical `resource_uri` target of a remote MCP server endpoint
/// (#1123): Main's `credentialadapters.CanonicalResourceURI`, the string an
/// MCP access token is bound to (RFC 8707). It is NOT [`canonical_target`]:
/// `https` only, host lowercased, port 443 dropped, an empty path is `/`,
/// trailing slashes are KEPT, and any `%`, query, fragment, user info or dot
/// segment is refused (`None`). The shared vectors in `targets.json`
/// (`resource_uri_vectors`) pin both sides.
///
/// This side refuses more than Main where the two serializers could
/// disagree (any non-ASCII byte, a path character outside RFC 3986's
/// unreserved and sub-delimiter set, port 0): such a target can never match
/// an envelope, so refusing it here is the same answer, earlier.
#[must_use]
pub fn canonical_resource_uri(raw: &str) -> Option<String> {
    let raw = raw.trim();
    if raw.is_empty()
        || raw.len() > MAX_RESOURCE_URI_BYTES
        || !raw.is_ascii()
        || raw
            .bytes()
            .any(|byte| matches!(byte, b'\\' | b'%' | b'?' | b'#') || byte <= b' ' || byte == 0x7f)
    {
        return None;
    }
    let rest = raw
        .get(..8)?
        .eq_ignore_ascii_case("https://")
        .then(|| &raw[8..])?;
    let (authority, path) = rest.find('/').map_or((rest, ""), |at| rest.split_at(at));
    if authority.is_empty() || authority.contains('@') {
        return None;
    }
    if path
        .split('/')
        .any(|segment| segment == "." || segment == "..")
        || !path
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || b"/-._~!$&'()*+,;=:@".contains(&byte))
    {
        return None;
    }
    let url = reqwest::Url::parse(raw).ok()?;
    if url.scheme() != "https" || url.port() == Some(0) {
        return None;
    }
    let host = url.host_str()?;
    let mut canonical = String::with_capacity(raw.len());
    canonical.push_str("https://");
    canonical.push_str(host);
    if let Some(port) = url.port() {
        canonical.push(':');
        canonical.push_str(&port.to_string());
    }
    canonical.push_str(if path.is_empty() { "/" } else { path });
    (canonical.len() <= MAX_RESOURCE_URI_BYTES).then_some(canonical)
}

/// Validate an envelope against an adapter's class and keys: the class must
/// be accepted, every key must be allowed for it, and at least one must be
/// present. Each value is bounded and printable.
pub(crate) fn admit_material(
    adapter: &dyn AuthAdapter,
    envelope: &CredentialEnvelope,
) -> Result<CredentialClass, GovernedError> {
    let class = envelope
        .kind()
        .class()
        .ok_or(GovernedError::ClassNotSupported)?;
    let allowed = adapter.material_keys(class);
    if allowed.is_empty() {
        return Err(GovernedError::ClassNotSupported);
    }
    let mut present = 0usize;
    for (key, value) in &envelope.material {
        if !allowed.contains(&key.as_str())
            || value.is_empty()
            || value.len() > MAX_MATERIAL_VALUE_BYTES
            || !value.bytes().all(|byte| (0x20..=0x7e).contains(&byte))
        {
            return Err(GovernedError::MaterialNotAllowed);
        }
        present += 1;
    }
    if present == 0 {
        return Err(GovernedError::MaterialNotAllowed);
    }
    Ok(class)
}
