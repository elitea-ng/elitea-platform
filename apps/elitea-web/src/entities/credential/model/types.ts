/**
 * Credential domain type — an AI-provider credential. Called "Configuration"
 * in the old app's code (`api/configurations.js`) but "Credential"
 * everywhere user-facing. No OpenAPI schema exists for this resource
 * (chat/agent-authoring domain, not in the W2 manifest).
 *
 * Evidence:
 * - apps/elitea-ui/src/api/configurations.js:67-184 — `getConfigurationsList`
 *   response `{ items, total, limit, offset, shared: {...} }`.
 * - apps/elitea-ui/src/pages/Credentials/EditCredential.jsx:60-73 — field
 *   set: `type`, `section`, `data` (settings blob), `elitea_title`,
 *   `data.title` (fallback), `label`, `shared`.
 * - apps/elitea-ui/src/[fsd]/features/credentials/lib/helpers/
 *   credential.helpers.js:32-73 — `id = credential.uid || credential.id`,
 *   `credential_url = data?.base_url || data?.url`.
 * - apps/elitea-ui/src/api/configurations.js:434-438 — list-model rows
 *   synthesize a composite id `${project_id}_${name}` (no stable id from the
 *   API for that endpoint).
 */
export interface Credential {
  readonly id: string;
  /** Present on most endpoints; `id` is the fallback when absent. */
  readonly uid?: string;
  /** Stable authorization identity; distinct from the row's numeric id. */
  readonly uuid?: string;
  readonly type: string;
  /** Opaque provider-settings blob (model tiers, base_url/url, oauth_discovery_endpoint, ...). */
  readonly data?: Readonly<Record<string, unknown>>;
  readonly eliteaTitle?: string;
  readonly label?: string;
  readonly shared?: boolean;
  readonly section?: string;
  /** Absent for platform-shared credentials; present for project-local ones. */
  readonly projectId?: string;
  readonly isPinned?: boolean;
}

export interface CredentialPage {
  readonly items: readonly Credential[];
  readonly total: number;
  readonly limit: number;
  readonly offset: number;
  readonly shared?: { readonly items: readonly Credential[]; readonly total: number };
}

/**
 * Model info returned by GET /configurations/models/{projectId}.
 * Ported from `apps/elitea-ui/src/api/configurations.js`'s `listModels`
 * response shape (the endpoint has no OpenAPI entry — the shape is observed
 * from the RTK Query request/response cycle).
 */
export interface ModelInfo {
  /** Unique model identifier — `project_id` + `_` + `name`. */
  readonly id: string;
  /** Canonical model name (e.g. `gpt-4o`). */
  readonly name: string;
  /** Display name shown to users. */
  readonly display_name: string;
  /** Provider type slug (e.g. `open_ai`, `anthropic`, `ollama`). */
  readonly type: string;
  /** Display label (alias for `display_name` in some responses). */
  readonly label: string;
  /** Owning project ID. */
  readonly project_id: string;
  /** Whether this model is the default for its section. */
  readonly default: boolean;
  /** Whether this is a low-tier (cheaper/faster) model. */
  readonly low_tier?: boolean;
  /** Whether this is a high-tier (more capable) model. */
  readonly high_tier?: boolean;
  /** Provider integration name (e.g. `OpenAI`, `Anthropic`). */
  readonly integration_name?: string;
  /** Model capabilities (chat_completion, embedding, etc.). */
  readonly capabilities?: Record<string, boolean>;
}
