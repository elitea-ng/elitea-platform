//! Embeddings for the local index (ADR-0029 decision 7): OFF unless the
//! person turns them on for a workspace and chooses how. The policy says
//! which ways they may choose (`local_work.index_local_embeddings`, default
//! on; `local_work.index_remote_embeddings`, default off; both need the
//! index itself allowed).
//!
//! * **On this computer** (`local:<model id>`): a model of the pinned
//!   catalogue (`elitea_local_embed::catalog`), downloaded only once the
//!   person chooses it, into `<app data>/models/<weights sha256>/` (owner-only,
//!   shared by every workspace), verified before every load, run by candle
//!   (Metal on Apple silicon) on a capped thread pool. Nothing leaves the
//!   device.
//! * **Through the deployment** (`remote:<dim>:<model>`): the platform's
//!   `/llm/v1/embeddings` route over the app's one HTTP client, with the
//!   native token and `X-Project-Id` = the workspace's bound project. The
//!   background embedder sends no execution id; a search inside a turn
//!   sends the turn's. The width is probed when the person picks the model.
//!
//! The policy is read on every pass: a choice it no longer allows makes no
//! vectors and its vectors are dropped (as a `path_deny` change drops what
//! it covers); a search offers no dense side for it.
//!
//! The background embedder (`elitea_local_index::embedding`) runs on mains
//! power only and not in Low Power Mode ([`Power`]), unless the person asks
//! for a rebuild; it waits while a turn runs, and drops the model after two
//! idle minutes.

use std::sync::{Arc, Mutex, PoisonError};
use std::time::{Duration, Instant};

use elitea_llm_wire::headers::{EXECUTION_HEADER, PROJECT_HEADER};
use elitea_llm_wire::refusal::{Refusal, refusal_code};
use elitea_llm_wire::route::EMBEDDINGS_ROUTE;
use elitea_local_embed::candle_backend::{CandleEmbedder, DevicePreference};
use elitea_local_embed::catalog::{self, ModelSpec};
use elitea_local_embed::manager::ModelStore;
use elitea_local_embed::{EmbedError, Embedder, vector};
use elitea_local_index::embedding::{Conditions, EmbedderSource, OFF};
use elitea_local_index::sqlite_store::VectorSet;
use elitea_local_tools::policy::LocalWorkPolicy;

use crate::d0::api::Credentials;
use crate::d0::turn::PolicySource;
use crate::net::SharedHttp;
use crate::workspaces::WorkspaceStore;

/// The prefix of an on-device choice.
pub const LOCAL: &str = "local:";
/// The prefix of a choice through the deployment.
pub const REMOTE: &str = "remote:";

/// How long one remote embeddings call may take.
const REMOTE_TIMEOUT: Duration = Duration::from_secs(60);

/// What a workspace's `embeddings` setting says.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum Choice {
    Off,
    Local(&'static ModelSpec),
    Remote {
        model: String,
        dim: usize,
    },
    /// A value this build does not know (a newer app wrote it): treated as
    /// off, its vectors dropped.
    Unknown,
}

impl Choice {
    #[must_use]
    pub fn parse(value: &str) -> Self {
        if value.is_empty() || value == OFF {
            return Self::Off;
        }
        if let Some(rest) = value.strip_prefix(REMOTE) {
            return rest
                .split_once(':')
                .and_then(|(dim, model)| Some((dim.parse::<usize>().ok()?, model)))
                .filter(|(dim, model)| (1..=8192).contains(dim) && valid_model(model))
                .map_or(Self::Unknown, |(dim, model)| Self::Remote {
                    model: model.to_owned(),
                    dim,
                });
        }
        value
            .strip_prefix(LOCAL)
            .and_then(catalog::model)
            .map_or(Self::Unknown, Self::Local)
    }

    /// The setting's value.
    #[must_use]
    pub fn value(&self) -> String {
        match self {
            Self::Off | Self::Unknown => OFF.to_owned(),
            Self::Local(spec) => spec.stamp(),
            Self::Remote { model, dim } => format!("{REMOTE}{dim}:{model}"),
        }
    }

    /// The vectors it makes.
    #[must_use]
    pub fn vector_set(&self) -> Option<VectorSet> {
        match self {
            Self::Local(spec) => Some(VectorSet {
                origin: "local".into(),
                model: spec.id.into(),
                dim: spec.dimension,
            }),
            Self::Remote { model, dim } => Some(VectorSet {
                origin: "remote".into(),
                model: model.clone(),
                dim: *dim,
            }),
            Self::Off | Self::Unknown => None,
        }
    }
}

/// A model name as the gateway takes it: printable, no spaces, bounded.
#[must_use]
pub fn valid_model(model: &str) -> bool {
    !model.is_empty()
        && model.len() <= 200
        && model
            .chars()
            .all(|c| c.is_ascii_graphic() && c != '"' && c != '\\')
}

/// Where the on-device models download from: the policy's mirror when it
/// is an `https` URL (the server accepts nothing else), else Hugging Face.
/// Whatever serves them, every file is checked against its pinned SHA-256.
#[must_use]
pub fn model_base(policy: &dyn PolicySource) -> Option<String> {
    let policy: LocalWorkPolicy = policy
        .local_work()
        .and_then(|section| serde_json::from_value(section).ok())
        .unwrap_or_default();
    let mirror = policy.index_model_mirror_url.trim_end_matches('/');
    mirror
        .strip_prefix("https://")
        .is_some_and(|rest| !rest.is_empty() && !rest.contains(['?', '#', '@']))
        .then(|| mirror.to_owned())
}

/// Which embedders the policy allows now.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct Allowed {
    pub local: bool,
    pub remote: bool,
}

impl Allowed {
    /// From the `local_work` group: both need local work and the index.
    #[must_use]
    pub fn of(policy: &dyn PolicySource) -> Self {
        let policy: LocalWorkPolicy = policy
            .local_work()
            .and_then(|section| serde_json::from_value(section).ok())
            .unwrap_or_default();
        let index = policy.allowed && policy.local_index;
        Self {
            local: index && policy.index_local_embeddings,
            remote: index && policy.index_remote_embeddings,
        }
    }

    #[must_use]
    pub fn allows(self, choice: &Choice) -> bool {
        match choice {
            Choice::Local(_) => self.local,
            Choice::Remote { .. } => self.remote,
            Choice::Off | Choice::Unknown => false,
        }
    }
}

/// The models of this computer.
pub struct LocalModels {
    store: ModelStore,
    threads: usize,
}

impl LocalModels {
    /// The cache under `<app data>/models`; CPU work on half the cores.
    #[must_use]
    pub fn new(data_dir: &std::path::Path) -> Self {
        let cores = std::thread::available_parallelism().map_or(2, std::num::NonZero::get);
        Self {
            store: ModelStore::new(data_dir.join("models")),
            threads: (cores / 2).max(1),
        }
    }

    fn load(&self, spec: &'static ModelSpec) -> Result<Arc<dyn Embedder>, EmbedError> {
        let dir = self.store.verified_dir(spec)?;
        let started = Instant::now();
        let embedder =
            CandleEmbedder::load(spec, &dir, DevicePreference::Auto)?.with_threads(self.threads)?;
        log::info!(
            "embedding model {} loaded on {} in {:?}",
            spec.id,
            embedder.device_name(),
            started.elapsed()
        );
        Ok(Arc::new(embedder))
    }
}

/// What a remote embedder needs of the app.
#[derive(Clone)]
pub struct RemoteDeps {
    pub http: SharedHttp,
    pub credentials: Arc<dyn Credentials>,
    pub workspaces: Arc<WorkspaceStore>,
    /// The app's runtime: the embedder's calls are blocking (the embedder
    /// thread, a search's blocking task) and run their request on it.
    pub runtime: tokio::runtime::Handle,
}

/// Every embedder the app can make, as the policy allows them now.
pub struct Embedders {
    pub local: LocalModels,
    pub remote: Option<RemoteDeps>,
    pub policy: Arc<dyn PolicySource>,
}

impl EmbedderSource for Embedders {
    fn vector_set(&self, choice: &str) -> Option<VectorSet> {
        let choice = Choice::parse(choice);
        if !Allowed::of(self.policy.as_ref()).allows(&choice) {
            return None;
        }
        if matches!(choice, Choice::Remote { .. }) && self.remote.is_none() {
            return None;
        }
        choice.vector_set()
    }

    fn load(&self, value: &str, workspace_id: &str) -> Result<Arc<dyn Embedder>, EmbedError> {
        let choice = Choice::parse(value);
        if !Allowed::of(self.policy.as_ref()).allows(&choice) {
            return Err(EmbedError::Load(format!(
                "the policy does not allow the embedder {value}"
            )));
        }
        match choice {
            Choice::Local(spec) => self.local.load(spec),
            Choice::Remote { model, dim } => {
                let deps = self
                    .remote
                    .clone()
                    .ok_or_else(|| EmbedError::Remote("not available in this app".into()))?;
                let project_id = deps
                    .workspaces
                    .get(workspace_id)
                    .ok()
                    .flatten()
                    .and_then(|workspace| workspace.project_id)
                    .ok_or_else(|| {
                        EmbedError::Remote("the folder is not bound to a project".into())
                    })?;
                Ok(Arc::new(RemoteEmbedder {
                    deps,
                    project_id,
                    model,
                    dim,
                }))
            }
            Choice::Off | Choice::Unknown => {
                Err(EmbedError::Load(format!("no embedder is called {value}")))
            }
        }
    }

    fn per_workspace(&self, choice: &str) -> bool {
        choice.starts_with(REMOTE)
    }
}

/// The deployment's embeddings route, for one project.
pub struct RemoteEmbedder {
    deps: RemoteDeps,
    project_id: i64,
    model: String,
    dim: usize,
}

#[derive(serde::Deserialize)]
struct EmbeddingsAnswer {
    data: Vec<EmbeddingRow>,
}

#[derive(serde::Deserialize)]
struct EmbeddingRow {
    #[serde(default)]
    index: usize,
    embedding: Vec<f32>,
}

/// How a call backs off a `429`.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct Backoff {
    /// Retries after the first try.
    pub retries: u32,
    /// The wait before the first retry when the service names none; it
    /// doubles with every retry.
    pub base: Duration,
    /// The longest single wait. A `Retry-After` above it is not waited for
    /// inside the call: the call gives up with [`EmbedError::RateLimited`]
    /// and the caller decides when to come back.
    pub max_wait: Duration,
}

impl Backoff {
    /// Background embedding: patient (1 s, 2 s, 4 s … up to a minute a
    /// wait, six retries).
    pub const DOCUMENTS: Self = Self {
        retries: 6,
        base: Duration::from_secs(1),
        max_wait: Duration::from_secs(60),
    };
    /// A search's query embedding: a person waits for it.
    pub const QUERY: Self = Self {
        retries: 1,
        base: Duration::from_millis(500),
        max_wait: Duration::from_secs(2),
    };
    /// A single probe or one-off call: no retry.
    pub const NONE: Self = Self {
        retries: 0,
        base: Duration::ZERO,
        max_wait: Duration::ZERO,
    };

    /// How long to wait before retry number `retry` (from 0): what the
    /// service asked, else the doubling backoff.
    ///
    /// # Errors
    ///
    /// The retries are spent, or the wait is longer than [`Self::max_wait`]:
    /// the wait that was asked (or would have been).
    pub fn wait(&self, retry: u32, retry_after: Option<Duration>) -> Result<Duration, Duration> {
        let asked = retry_after.unwrap_or_else(|| self.base.saturating_mul(1_u32 << retry.min(16)));
        if retry >= self.retries || asked > self.max_wait {
            Err(asked)
        } else {
            Ok(asked)
        }
    }
}

/// A `Retry-After` header: delta-seconds (fractions allowed). The HTTP-date
/// form is not read (it needs a date parser the app does not otherwise
/// carry); the backoff covers it.
#[must_use]
pub fn parse_retry_after(value: &str) -> Option<Duration> {
    value
        .trim()
        .parse::<f64>()
        .ok()
        .filter(|seconds| seconds.is_finite() && *seconds >= 0.0)
        .and_then(|seconds| Duration::try_from_secs_f64(seconds).ok())
}

/// Wait `delay` unless `stop` answers `true` first (looked at every
/// 100 ms); whether the wait ran its course.
async fn sleep_unless(delay: Duration, stop: &(dyn Fn() -> bool + Sync)) -> bool {
    const SLICE: Duration = Duration::from_millis(100);
    let until = Instant::now() + delay;
    loop {
        if stop() {
            return false;
        }
        let left = until.saturating_duration_since(Instant::now());
        if left.is_zero() {
            return true;
        }
        tokio::time::sleep(left.min(SLICE)).await;
    }
}

/// POST `inputs` to `/llm/v1/embeddings`; the vectors in input order. One
/// try, no backoff (a probe).
///
/// # Errors
///
/// Signed out, unreachable, refused (the gateway's code), or an answer that
/// does not fit.
pub async fn embed_remote(
    deps: &RemoteDeps,
    project_id: i64,
    model: &str,
    inputs: &[&str],
    execution_id: Option<&str>,
) -> Result<Vec<Vec<f32>>, EmbedError> {
    embed_remote_with(
        deps,
        (project_id, model),
        inputs,
        execution_id,
        Backoff::NONE,
        &|| false,
    )
    .await
}

/// [`embed_remote`] that backs off a `429` as `backoff` says, honouring
/// `Retry-After`, and gives up with [`EmbedError::Paused`] once `stop`
/// answers `true` while it waits. A `402` is [`EmbedError::BudgetExhausted`]
/// at once: waiting does not refill a budget.
///
/// # Errors
///
/// As [`embed_remote`], plus the two above and [`EmbedError::RateLimited`]
/// once the retries or the longest wait are spent.
pub async fn embed_remote_with(
    deps: &RemoteDeps,
    (project_id, model): (i64, &str),
    inputs: &[&str],
    execution_id: Option<&str>,
    backoff: Backoff,
    stop: &(dyn Fn() -> bool + Sync),
) -> Result<Vec<Vec<f32>>, EmbedError> {
    let remote = |message: String| EmbedError::Remote(message);
    let client = deps.http.client().await;
    let body = serde_json::json!({"model": model, "input": inputs, "encoding_format": "float"});
    let mut bearer = deps
        .credentials
        .bearer()
        .await
        .map_err(|error| remote(error.message))?;
    let (mut refreshed, mut retries) = (false, 0_u32);
    loop {
        let mut request = client
            .post(format!("{}{EMBEDDINGS_ROUTE}", bearer.origin))
            .timeout(REMOTE_TIMEOUT)
            .bearer_auth(&bearer.token)
            .header(PROJECT_HEADER, project_id.to_string());
        if let Some(id) = execution_id {
            request = request.header(EXECUTION_HEADER, id);
        }
        let response = request
            .json(&body)
            .send()
            .await
            .map_err(|_| remote("could not reach the deployment".into()))?;
        let status = response.status().as_u16();
        if status == 401 && !refreshed {
            refreshed = true;
            bearer = deps
                .credentials
                .refreshed()
                .await
                .map_err(|error| remote(error.message))?;
            continue;
        }
        let retry_after = response
            .headers()
            .get(reqwest::header::RETRY_AFTER)
            .and_then(|value| value.to_str().ok())
            .and_then(parse_retry_after);
        let bytes = response
            .bytes()
            .await
            .map_err(|_| remote("the answer was cut off".into()))?;
        if !(200..300).contains(&status) {
            let code = refusal_code(status, &bytes);
            return Err(match Refusal::from_status(status) {
                Some(Refusal::BudgetExhausted(_)) => {
                    EmbedError::BudgetExhausted(format!("{code} (HTTP {status})"))
                }
                Some(Refusal::RateLimited) => match backoff.wait(retries, retry_after) {
                    Ok(delay) => {
                        retries += 1;
                        if sleep_unless(delay, stop).await {
                            continue;
                        }
                        EmbedError::Paused
                    }
                    Err(asked) => EmbedError::RateLimited { retry_after: asked },
                },
                _ => remote(format!("{code} (HTTP {status})")),
            });
        }
        let mut answer: EmbeddingsAnswer = serde_json::from_slice(&bytes)
            .map_err(|_| EmbedError::Shape("not an embeddings answer".into()))?;
        if answer.data.len() != inputs.len() {
            return Err(EmbedError::Shape(format!(
                "{} vectors for {} inputs",
                answer.data.len(),
                inputs.len()
            )));
        }
        answer.data.sort_by_key(|row| row.index);
        return Ok(answer.data.into_iter().map(|row| row.embedding).collect());
    }
}

/// The deployment's catalogue route; `?section=embedding` keeps its
/// embedding models (client contract 1.9).
pub const MODELS_ROUTE: &str = "/api/v2/configurations/models/";

/// One embedding model the workspace's project can use through the
/// deployment.
#[derive(Debug, Clone, PartialEq, Eq, serde::Serialize)]
pub struct RemoteModel {
    /// What a `remote:<model>` choice names.
    pub name: String,
    pub display_name: Option<String>,
}

#[derive(serde::Deserialize)]
struct ModelList {
    items: Vec<ModelListItem>,
}

#[derive(serde::Deserialize)]
struct ModelListItem {
    name: String,
    #[serde(default)]
    display_name: Option<String>,
    /// USD per 1M input tokens, when the catalogue prices the model and the
    /// caller may see amounts (the server serves it for chat models today).
    #[serde(default)]
    input_cost_per_1m_tokens: Option<f64>,
}

/// GET `path` (with `query`) as the signed-in person and read the JSON
/// answer as `T`; a `401` is retried once with a refreshed token.
async fn get_json<T: serde::de::DeserializeOwned>(
    deps: &RemoteDeps,
    path: &str,
    query: &[(&str, &str)],
    what: &str,
) -> Result<T, EmbedError> {
    let remote = |message: String| EmbedError::Remote(message);
    let client = deps.http.client().await;
    let mut bearer = deps
        .credentials
        .bearer()
        .await
        .map_err(|error| remote(error.message))?;
    for attempt in 0..2 {
        let response = client
            .get(format!("{}{path}", bearer.origin))
            .query(query)
            .timeout(REMOTE_TIMEOUT)
            .bearer_auth(&bearer.token)
            .header(
                crate::tokens::CLIENT_VERSION_HEADER,
                env!("CARGO_PKG_VERSION"),
            )
            .send()
            .await
            .map_err(|_| remote("could not reach the deployment".into()))?;
        let status = response.status().as_u16();
        if status == 401 && attempt == 0 {
            bearer = deps
                .credentials
                .refreshed()
                .await
                .map_err(|error| remote(error.message))?;
            continue;
        }
        let bytes = response
            .bytes()
            .await
            .map_err(|_| remote("the answer was cut off".into()))?;
        if !(200..300).contains(&status) {
            return Err(remote(format!(
                "{} (HTTP {status})",
                refusal_code(status, &bytes)
            )));
        }
        return serde_json::from_slice(&bytes)
            .map_err(|_| EmbedError::Shape(format!("not {what}")));
    }
    Err(remote("the deployment refused the sign-in".into()))
}

async fn model_list(deps: &RemoteDeps, project_id: i64) -> Result<ModelList, EmbedError> {
    get_json(
        deps,
        &format!("{MODELS_ROUTE}{project_id}"),
        &[("section", "embedding"), ("include_shared", "true")],
        "a model list",
    )
    .await
}

/// The embedding models `project_id` can use (its own and the shared
/// ones), by name, deduplicated, valid names only.
///
/// # Errors
///
/// Signed out, unreachable, refused, or an answer that does not fit (the
/// caller falls back to a typed name).
pub async fn remote_models(
    deps: &RemoteDeps,
    project_id: i64,
) -> Result<Vec<RemoteModel>, EmbedError> {
    let list = model_list(deps, project_id).await?;
    let mut seen = std::collections::HashSet::new();
    Ok(list
        .items
        .into_iter()
        .filter(|item| valid_model(&item.name) && seen.insert(item.name.clone()))
        .map(|item| RemoteModel {
            name: item.name,
            display_name: item.display_name.filter(|name| !name.trim().is_empty()),
        })
        .collect())
}

/// USD per 1M input tokens of `model`, when the deployment's catalogue
/// says (it prices chat models today: an embedding model usually has no
/// price here, and the estimate then shows tokens and the budget only).
pub async fn remote_price(deps: &RemoteDeps, project_id: i64, model: &str) -> Option<f64> {
    model_list(deps, project_id)
        .await
        .ok()?
        .items
        .into_iter()
        .find(|item| item.name == model)
        .and_then(|item| item.input_cost_per_1m_tokens)
        .filter(|price| price.is_finite() && *price >= 0.0)
}

/// What is left of a project's monthly budget, as the platform tells the
/// caller: amounts only for a project admin or the owner of a personal
/// project (`can_see_amounts`); everyone else gets the percentage.
#[derive(Debug, Clone, Copy, PartialEq, serde::Serialize)]
pub struct ProjectBudgetLeft {
    /// No enforced ceiling.
    pub unlimited: bool,
    /// USD left this month (`None`: unlimited, or amounts not shown).
    pub remaining_usd: Option<f64>,
    /// Percent of the ceiling spent (`None`: unlimited).
    pub percent_used: Option<f64>,
    /// The platform's own "close to the limit" flag.
    pub warning_active: bool,
}

#[derive(serde::Deserialize)]
struct BudgetAnswer {
    limit_source: String,
    #[serde(default)]
    remaining: Option<f64>,
    #[serde(default)]
    percent_used: Option<f64>,
    #[serde(default)]
    warning_active: bool,
}

/// `getProjectBudget`: `GET /api/v2/elitea_core/project_budget/prompt_lib/{project}/budget`.
///
/// # Errors
///
/// Signed out, unreachable, refused, or an answer that does not fit.
pub async fn project_budget(
    deps: &RemoteDeps,
    project_id: i64,
) -> Result<ProjectBudgetLeft, EmbedError> {
    let answer: BudgetAnswer = get_json(
        deps,
        &format!("/api/v2/elitea_core/project_budget/prompt_lib/{project_id}/budget"),
        &[],
        "a budget",
    )
    .await?;
    Ok(ProjectBudgetLeft {
        unlimited: answer.limit_source == "unlimited",
        remaining_usd: answer.remaining,
        percent_used: answer.percent_used,
        warning_active: answer.warning_active,
    })
}

impl RemoteEmbedder {
    fn call(
        &self,
        inputs: &[&str],
        execution_id: Option<&str>,
        backoff: Backoff,
        stop: &(dyn Fn() -> bool + Sync),
    ) -> Result<Vec<Vec<f32>>, EmbedError> {
        let vectors = self.deps.runtime.block_on(embed_remote_with(
            &self.deps,
            (self.project_id, &self.model),
            inputs,
            execution_id,
            backoff,
            stop,
        ))?;
        vectors
            .into_iter()
            .map(|mut values| {
                if values.len() != self.dim {
                    return Err(EmbedError::Shape(format!(
                        "{} dimensions, expected {}",
                        values.len(),
                        self.dim
                    )));
                }
                vector::normalize(&mut values);
                Ok(values)
            })
            .collect()
    }
}

impl Embedder for RemoteEmbedder {
    fn stamp(&self) -> String {
        format!("{REMOTE}{}", self.model)
    }

    fn dimension(&self) -> usize {
        self.dim
    }

    fn embed_documents(&self, texts: &[&str]) -> Result<Vec<Vec<f32>>, EmbedError> {
        self.embed_documents_until(texts, &|| false)
    }

    fn embed_documents_until(
        &self,
        texts: &[&str],
        stop: &(dyn Fn() -> bool + Sync),
    ) -> Result<Vec<Vec<f32>>, EmbedError> {
        if texts.is_empty() {
            return Ok(Vec::new());
        }
        // Background work: no turn to attribute it to.
        self.call(texts, None, Backoff::DOCUMENTS, stop)
    }

    fn embed_query(&self, text: &str) -> Result<Vec<f32>, EmbedError> {
        self.embed_query_for(text, None)
    }

    fn embed_query_for(
        &self,
        text: &str,
        execution_id: Option<&str>,
    ) -> Result<Vec<f32>, EmbedError> {
        self.call(&[text], execution_id, Backoff::QUERY, &|| false)?
            .pop()
            .ok_or_else(|| EmbedError::Shape("no vector".into()))
    }
}

/// How long a power reading is trusted.
const POWER_TTL: Duration = Duration::from_secs(60);

/// Mains power and not Low Power Mode, read from the system at most once a
/// minute. A computer that cannot tell (no battery, no `pmset`) runs.
#[derive(Default)]
pub struct Power {
    cached: Mutex<Option<(Instant, Option<&'static str>)>>,
}

impl Conditions for Power {
    fn hold(&self) -> Option<&'static str> {
        let mut cached = self.cached.lock().unwrap_or_else(PoisonError::into_inner);
        if let Some((at, hold)) = *cached
            && at.elapsed() < POWER_TTL
        {
            return hold;
        }
        let hold = read_power();
        *cached = Some((Instant::now(), hold));
        hold
    }
}

#[cfg(target_os = "macos")]
fn read_power() -> Option<&'static str> {
    let run = |args: &[&str]| {
        std::process::Command::new("/usr/bin/pmset")
            .args(args)
            .output()
            .ok()
            .filter(|out| out.status.success())
            .map(|out| String::from_utf8_lossy(&out.stdout).into_owned())
    };
    if run(&["-g", "batt"]).is_some_and(|text| on_battery(&text)) {
        return Some("on_battery");
    }
    if run(&["-g"]).is_some_and(|text| low_power(&text)) {
        return Some("low_power");
    }
    None
}

#[cfg(not(target_os = "macos"))]
fn read_power() -> Option<&'static str> {
    // Linux: a mains supply reporting offline while a battery exists.
    let Ok(entries) = std::fs::read_dir("/sys/class/power_supply") else {
        return None;
    };
    let mut battery = false;
    let mut mains_online = false;
    for entry in entries.flatten() {
        let read =
            |name: &str| std::fs::read_to_string(entry.path().join(name)).unwrap_or_default();
        match read("type").trim() {
            "Battery" => battery = true,
            "Mains" => mains_online |= read("online").trim() == "1",
            _ => {}
        }
    }
    (battery && !mains_online).then_some("on_battery")
}

/// `pmset -g batt`: "Now drawing from 'Battery Power'".
#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
fn on_battery(text: &str) -> bool {
    text.lines()
        .next()
        .is_some_and(|line| line.contains("'Battery Power'"))
}

/// `pmset -g`: a ` lowpowermode 1` (or `powermode 1` on newer systems)
/// line.
#[cfg_attr(not(target_os = "macos"), allow(dead_code))]
fn low_power(text: &str) -> bool {
    text.lines().any(|line| {
        let mut words = line.split_whitespace();
        matches!(
            (words.next(), words.next()),
            (Some("lowpowermode" | "powermode"), Some("1"))
        )
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::d0::api::{ApiError, Bearer};
    use crate::testutil::{Res, serve};
    use serde_json::{Value, json};

    struct Policy(Mutex<Option<Value>>);

    impl PolicySource for Policy {
        fn local_work(&self) -> Option<Value> {
            self.0.lock().unwrap().clone()
        }
    }

    struct Signed(String);

    #[test]
    fn a_mirror_is_used_only_when_it_is_a_plain_https_url() {
        struct Mirror(&'static str);
        impl PolicySource for Mirror {
            fn local_work(&self) -> Option<serde_json::Value> {
                Some(serde_json::json!({"allowed": true, "local_index": true,
                    "index_model_mirror_url": self.0}))
            }
        }
        assert_eq!(
            model_base(&Mirror("https://models.example.com/hf/")).as_deref(),
            Some("https://models.example.com/hf")
        );
        for refused in [
            "",
            "http://models.example.com",
            "https://",
            "https://user:secret@models.example.com",
            "https://models.example.com/?token=1",
            "https://models.example.com/#x",
        ] {
            assert_eq!(model_base(&Mirror(refused)), None, "{refused}");
        }
    }

    #[async_trait::async_trait]
    impl Credentials for Signed {
        async fn bearer(&self) -> Result<Bearer, ApiError> {
            Ok(Bearer {
                origin: self.0.clone(),
                token: "elnat_test".into(),
            })
        }
        async fn refreshed(&self) -> Result<Bearer, ApiError> {
            self.bearer().await
        }
        fn identity(&self) -> Option<String> {
            Some("me".into())
        }
    }

    #[test]
    fn choices_parse_and_unknown_ones_are_off() {
        assert_eq!(Choice::parse("off"), Choice::Off);
        assert_eq!(Choice::parse(""), Choice::Off);
        assert!(matches!(
            Choice::parse("local:multilingual-e5-small"),
            Choice::Local(spec) if spec.dimension == 384
        ));
        assert_eq!(Choice::parse("local:nope"), Choice::Unknown);
        assert_eq!(Choice::parse("remote"), Choice::Unknown);
        assert_eq!(Choice::parse("remote:0:m"), Choice::Unknown);
        assert_eq!(Choice::parse("remote:12:bad model"), Choice::Unknown);
        let remote = Choice::parse("remote:1536:text-embedding-3-small");
        assert_eq!(
            remote,
            Choice::Remote {
                model: "text-embedding-3-small".into(),
                dim: 1536
            }
        );
        assert_eq!(remote.value(), "remote:1536:text-embedding-3-small");
        assert_eq!(
            remote.vector_set().unwrap().stamp(),
            "remote:text-embedding-3-small:1536"
        );
        assert_eq!(
            Choice::parse("local:qwen3-embedding-0.6b")
                .vector_set()
                .unwrap()
                .stamp(),
            "local:qwen3-embedding-0.6b:512"
        );
    }

    #[test]
    fn the_policy_decides_which_embedders_exist() {
        let dir = tempfile::tempdir().unwrap();
        let policy = Arc::new(Policy(Mutex::new(Some(json!({
            "allowed": true, "local_index": true, "index_local_embeddings": true
        })))));
        let embedders = Embedders {
            local: LocalModels::new(dir.path()),
            remote: None,
            policy: policy.clone(),
        };
        assert!(
            embedders
                .vector_set("local:multilingual-e5-small")
                .is_some()
        );
        assert!(
            embedders.vector_set("remote:4:m").is_none(),
            "remote is off"
        );
        // Allowed but not downloaded: refused before anything loads.
        let refused = embedders
            .load("local:multilingual-e5-small", "ws")
            .err()
            .unwrap();
        assert!(refused.to_string().contains("not downloaded"), "{refused}");
        // The policy turns the index (or local embeddings) off: no set.
        *policy.0.lock().unwrap() = Some(json!({"allowed": true, "local_index": false,
            "index_local_embeddings": true}));
        assert!(
            embedders
                .vector_set("local:multilingual-e5-small")
                .is_none()
        );
        *policy.0.lock().unwrap() = Some(json!({"allowed": true, "local_index": true}));
        assert!(
            embedders
                .vector_set("local:multilingual-e5-small")
                .is_none()
        );
        assert!(embedders.load("local:multilingual-e5-small", "ws").is_err());
        assert!(embedders.per_workspace("remote:4:m"));
        assert!(!embedders.per_workspace("local:multilingual-e5-small"));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_remote_embedder_sends_the_project_and_only_a_turns_execution_id() {
        let server = serve(|req| {
            let body: Value = serde_json::from_str(&req.body).unwrap();
            let n = body["input"].as_array().unwrap().len();
            // Answered out of order: the embedder puts them back.
            let data: Vec<Value> = (0..n)
                .rev()
                .map(|i| json!({"index": i, "embedding": [i as f64 + 1.0, 0.0, 0.0, 1.0]}))
                .collect();
            Res::json(200, &json!({"data": data, "model": body["model"]}))
        })
        .await;
        let app = tempfile::tempdir().unwrap();
        let folder = tempfile::tempdir().unwrap();
        let workspaces = Arc::new(WorkspaceStore::new(app.path().to_owned()));
        let id = workspaces.add(folder.path()).unwrap().id;
        let policy = Arc::new(Policy(Mutex::new(Some(json!({
            "allowed": true, "local_index": true, "index_remote_embeddings": true
        })))));
        let embedders = Arc::new(Embedders {
            local: LocalModels::new(app.path()),
            remote: Some(RemoteDeps {
                http: SharedHttp::new("test"),
                credentials: Arc::new(Signed(server.origin.clone())),
                workspaces: workspaces.clone(),
                runtime: tokio::runtime::Handle::current(),
            }),
            policy: policy.clone(),
        });
        let choice = "remote:4:text-embedding-3-small";
        assert_eq!(
            embedders.vector_set(choice).unwrap().stamp(),
            "remote:text-embedding-3-small:4"
        );
        let unbound = embedders.load(choice, &id).err().unwrap();
        assert!(unbound.to_string().contains("not bound"), "{unbound}");
        workspaces.bind_project(&id, 42).unwrap();
        let (sources, ws) = (embedders.clone(), id.clone());
        let (documents, query) = tokio::task::spawn_blocking(move || {
            let embedder = sources.load(choice, &ws).unwrap();
            let documents = embedder.embed_documents(&["a", "b"]).unwrap();
            let query = embedder.embed_query_for("q", Some("exec-7")).unwrap();
            (documents, query)
        })
        .await
        .unwrap();
        assert_eq!(documents.len(), 2);
        assert!(documents[0][0] < documents[1][0], "input order kept");
        assert!((query.iter().map(|v| v * v).sum::<f32>() - 1.0).abs() < 1e-5);
        let seen = server.seen();
        assert_eq!(seen.len(), 2);
        assert!(seen.iter().all(|req| req.path == EMBEDDINGS_ROUTE
            && req.headers.get("x-project-id").map(String::as_str) == Some("42")
            && req.headers.get("authorization").map(String::as_str) == Some("Bearer elnat_test")));
        assert!(
            !seen[0].headers.contains_key("x-elitea-execution-id"),
            "background: none"
        );
        assert_eq!(
            seen[1]
                .headers
                .get("x-elitea-execution-id")
                .map(String::as_str),
            Some("exec-7")
        );
        // The policy turns remote embeddings off: gone at once.
        *policy.0.lock().unwrap() = Some(json!({"allowed": true, "local_index": true}));
        assert!(embedders.vector_set(choice).is_none());
        assert!(embedders.load(choice, &id).is_err());
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_project_embedding_models_are_listed_for_the_picker() {
        let server = serve(|req| {
            if req.path.ends_with("/7?section=embedding&include_shared=true") {
                Res::json(
                    200,
                    &json!({"total": 4, "items": [
                        {"name": "text-embedding-3-small", "project_id": 7, "display_name": "Small"},
                        {"name": "text-embedding-3-small", "project_id": 1},
                        {"name": "bad model", "project_id": 7},
                        {"name": "nomic-embed", "project_id": 1, "display_name": " "}
                    ]}),
                )
            } else {
                Res::json(403, &json!({"error": "forbidden"}))
            }
        })
        .await;
        let app = tempfile::tempdir().unwrap();
        let deps = RemoteDeps {
            http: SharedHttp::new("test"),
            credentials: Arc::new(Signed(server.origin.clone())),
            workspaces: Arc::new(WorkspaceStore::new(app.path().to_owned())),
            runtime: tokio::runtime::Handle::current(),
        };
        let models = remote_models(&deps, 7).await.unwrap();
        assert_eq!(
            models,
            [
                RemoteModel {
                    name: "text-embedding-3-small".into(),
                    display_name: Some("Small".into())
                },
                RemoteModel {
                    name: "nomic-embed".into(),
                    display_name: None
                },
            ]
        );
        let seen = server.seen();
        assert!(seen[0].path.starts_with(MODELS_ROUTE));
        assert_eq!(
            seen[0].headers.get("authorization").map(String::as_str),
            Some("Bearer elnat_test")
        );
        assert!(seen[0].headers.contains_key("x-client-version"));
        let refused = remote_models(&deps, 8).await.unwrap_err();
        assert!(refused.to_string().contains("403"), "{refused}");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_refusal_and_a_wrong_width_are_errors() {
        let server = serve(|req| {
            if req.body.contains("refuse") {
                Res::json(402, &json!({"error": {"code": "member_budget_exhausted"}}))
            } else {
                Res::json(
                    200,
                    &json!({"data": [{"index": 0, "embedding": [1.0, 0.0]}]}),
                )
            }
        })
        .await;
        let app = tempfile::tempdir().unwrap();
        let deps = RemoteDeps {
            http: SharedHttp::new("test"),
            credentials: Arc::new(Signed(server.origin.clone())),
            workspaces: Arc::new(WorkspaceStore::new(app.path().to_owned())),
            runtime: tokio::runtime::Handle::current(),
        };
        let embedder = RemoteEmbedder {
            deps,
            project_id: 1,
            model: "m".into(),
            dim: 4,
        };
        let (refused, narrow) = tokio::task::spawn_blocking(move || {
            (
                embedder.embed_documents(&["refuse"]).err().unwrap(),
                embedder.embed_documents(&["ok"]).err().unwrap(),
            )
        })
        .await
        .unwrap();
        assert!(
            matches!(refused, EmbedError::BudgetExhausted(ref m)
                if m.contains("budget_exhausted") && m.contains("402")),
            "a 402 is its own state, not a generic failure: {refused}"
        );
        assert!(matches!(narrow, EmbedError::Shape(_)), "{narrow}");
    }

    #[test]
    fn retry_after_is_read_as_seconds_and_a_date_is_left_to_the_backoff() {
        assert_eq!(parse_retry_after("7"), Some(Duration::from_secs(7)));
        assert_eq!(parse_retry_after(" 0.5 "), Some(Duration::from_millis(500)));
        assert_eq!(parse_retry_after("0"), Some(Duration::ZERO));
        for unusable in [
            "",
            "soon",
            "-1",
            "NaN",
            "inf",
            "Wed, 21 Oct 2026 07:28:00 GMT",
        ] {
            assert_eq!(parse_retry_after(unusable), None, "{unusable}");
        }
    }

    #[test]
    fn the_backoff_doubles_honours_the_service_and_is_bounded() {
        let backoff = Backoff {
            retries: 3,
            base: Duration::from_secs(1),
            max_wait: Duration::from_secs(5),
        };
        assert_eq!(backoff.wait(0, None), Ok(Duration::from_secs(1)));
        assert_eq!(backoff.wait(1, None), Ok(Duration::from_secs(2)));
        assert_eq!(backoff.wait(2, None), Ok(Duration::from_secs(4)));
        // The retries are spent.
        assert_eq!(backoff.wait(3, None), Err(Duration::from_secs(8)));
        // What the service asks replaces the doubling ...
        assert_eq!(
            backoff.wait(2, Some(Duration::from_millis(300))),
            Ok(Duration::from_millis(300))
        );
        // ... unless it is longer than the bound: the caller is told.
        assert_eq!(
            backoff.wait(0, Some(Duration::from_secs(600))),
            Err(Duration::from_secs(600))
        );
        // No overflow however many retries.
        assert!(Backoff::DOCUMENTS.wait(60, None).is_err());
        assert_eq!(Backoff::NONE.wait(0, None), Err(Duration::ZERO));
    }

    fn deps_for(origin: String, app: &std::path::Path) -> RemoteDeps {
        RemoteDeps {
            http: SharedHttp::new("test"),
            credentials: Arc::new(Signed(origin)),
            workspaces: Arc::new(WorkspaceStore::new(app.to_owned())),
            runtime: tokio::runtime::Handle::current(),
        }
    }

    fn limited(retry_after: Option<&str>) -> Res {
        let mut res = Res::json(429, &json!({"error": {"code": "rate_limited"}}));
        if let Some(value) = retry_after {
            res.headers.push(("Retry-After", value.to_owned()));
        }
        res
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_429_waits_the_retry_after_then_succeeds() {
        let calls = Arc::new(std::sync::atomic::AtomicUsize::new(0));
        let seen = calls.clone();
        let server = serve(move |_| {
            if seen.fetch_add(1, std::sync::atomic::Ordering::SeqCst) < 2 {
                limited(Some("0.15"))
            } else {
                Res::json(
                    200,
                    &json!({"data": [{"index": 0, "embedding": [1.0, 0.0]}]}),
                )
            }
        })
        .await;
        let app = tempfile::tempdir().unwrap();
        let deps = deps_for(server.origin.clone(), app.path());
        let backoff = Backoff {
            retries: 3,
            base: Duration::from_millis(1),
            max_wait: Duration::from_secs(1),
        };
        let started = Instant::now();
        let vectors = embed_remote_with(&deps, (1, "m"), &["a"], None, backoff, &|| false)
            .await
            .unwrap();
        assert_eq!(vectors.len(), 1);
        assert_eq!(server.seen().len(), 3);
        assert!(
            started.elapsed() >= Duration::from_millis(300),
            "two Retry-After waits of 150 ms, not the 1 ms backoff: {:?}",
            started.elapsed()
        );
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_429_is_retried_a_bounded_number_of_times() {
        let server = serve(|_| limited(None)).await;
        let app = tempfile::tempdir().unwrap();
        let deps = deps_for(server.origin.clone(), app.path());
        let backoff = Backoff {
            retries: 2,
            base: Duration::from_millis(5),
            max_wait: Duration::from_secs(1),
        };
        let error = embed_remote_with(&deps, (1, "m"), &["a"], None, backoff, &|| false)
            .await
            .unwrap_err();
        assert_eq!(server.seen().len(), 3, "the try and two retries");
        assert_eq!(
            error,
            EmbedError::RateLimited {
                retry_after: Duration::from_millis(20)
            }
        );
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn a_retry_after_beyond_the_bound_is_not_waited_for() {
        let server = serve(|_| limited(Some("3600"))).await;
        let app = tempfile::tempdir().unwrap();
        let deps = deps_for(server.origin.clone(), app.path());
        let started = Instant::now();
        let error = embed_remote_with(&deps, (1, "m"), &["a"], None, Backoff::DOCUMENTS, &|| false)
            .await
            .unwrap_err();
        assert_eq!(
            error,
            EmbedError::RateLimited {
                retry_after: Duration::from_secs(3600)
            }
        );
        assert_eq!(server.seen().len(), 1);
        assert!(started.elapsed() < Duration::from_secs(5));
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_backoff_can_be_cancelled_while_it_waits() {
        let server = serve(|_| limited(Some("30"))).await;
        let app = tempfile::tempdir().unwrap();
        let deps = deps_for(server.origin.clone(), app.path());
        let stop = Arc::new(std::sync::atomic::AtomicBool::new(false));
        let flag = stop.clone();
        tokio::spawn(async move {
            tokio::time::sleep(Duration::from_millis(250)).await;
            flag.store(true, std::sync::atomic::Ordering::SeqCst);
        });
        let started = Instant::now();
        let stopper = stop.clone();
        let error = embed_remote_with(
            &deps,
            (1, "m"),
            &["a"],
            None,
            Backoff::DOCUMENTS,
            &move || stopper.load(std::sync::atomic::Ordering::SeqCst),
        )
        .await
        .unwrap_err();
        assert_eq!(error, EmbedError::Paused);
        assert!(
            started.elapsed() < Duration::from_secs(5),
            "{:?}",
            started.elapsed()
        );
        assert_eq!(server.seen().len(), 1, "nothing sent after the stop");
    }

    #[tokio::test(flavor = "multi_thread", worker_threads = 2)]
    async fn the_project_budget_and_the_model_price_are_read_when_the_platform_gives_them() {
        let server = serve(|req| {
            if req.path.contains("/project_budget/prompt_lib/7/budget") {
                Res::json(
                    200,
                    &json!({"limit_source": "explicit", "enabled": true, "warning_pct": 80,
                        "percent_used": 91.5, "warning_active": true, "spend_available": true,
                        "remaining": 4.25, "can_see_amounts": true}),
                )
            } else if req.path.contains("/project_budget/prompt_lib/8/budget") {
                // A member who may not see amounts: the cost fields are gone.
                Res::json(
                    200,
                    &json!({"limit_source": "unlimited", "enabled": false, "warning_pct": 80,
                        "percent_used": null, "warning_active": false, "spend_available": false,
                        "can_see_amounts": false}),
                )
            } else if req.path.starts_with(MODELS_ROUTE) {
                Res::json(
                    200,
                    &json!({"items": [
                        {"name": "priced", "input_cost_per_1m_tokens": 0.02},
                        {"name": "unpriced"}]}),
                )
            } else {
                Res::json(404, &json!({}))
            }
        })
        .await;
        let app = tempfile::tempdir().unwrap();
        let deps = deps_for(server.origin.clone(), app.path());
        let budget = project_budget(&deps, 7).await.unwrap();
        assert_eq!(
            budget,
            ProjectBudgetLeft {
                unlimited: false,
                remaining_usd: Some(4.25),
                percent_used: Some(91.5),
                warning_active: true
            }
        );
        let hidden = project_budget(&deps, 8).await.unwrap();
        assert!(hidden.unlimited && hidden.remaining_usd.is_none());
        assert!(project_budget(&deps, 9).await.is_err());
        assert_eq!(remote_price(&deps, 7, "priced").await, Some(0.02));
        assert_eq!(remote_price(&deps, 7, "unpriced").await, None);
        assert_eq!(remote_price(&deps, 7, "absent").await, None);
        assert!(
            server
                .seen()
                .iter()
                .all(|req| req.headers.contains_key("x-client-version")),
            "the budget route wants the client version"
        );
    }

    #[test]
    fn power_readings_parse() {
        assert!(on_battery(
            "Now drawing from 'Battery Power'\n -InternalBattery-0 (id=1)\t80%; discharging;"
        ));
        assert!(!on_battery(
            "Now drawing from 'AC Power'\n -InternalBattery-0 (id=1)\t100%; charged;"
        ));
        assert!(low_power(
            "System-wide power settings:\nCurrently in use:\n lowpowermode         1\n sleep 1\n"
        ));
        assert!(!low_power(
            "Currently in use:\n lowpowermode         0\n powermode 0\n"
        ));
    }
}
