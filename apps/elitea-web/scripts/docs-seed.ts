#!/usr/bin/env npx tsx
/**
 * docs-seed.ts — idempotent fixture seeding for the embedded docs screenshot
 * pass (embedded-docs programme, units W4a and W4b).
 *
 * Creates one `autotest_docs_*`-named instance of every entity kind the
 * manifest's shots need (agent, pipeline, skill, credentials, secret,
 * artifact bucket + file, toolkits, an MCP connection, a conversation with a
 * participant, tags, a personal token) against a RUNNING e2e stack
 * (`apps/elitea-web/scripts/e2e-stack.sh up && … seed`), through the same
 * `/api/v2` surface the Playwright suite's own fixtures use — but this file
 * does NOT import `e2e/fixtures/*`: `docs-shots.ts`'s header already
 * established that `e2e/` is spec-adjacent, not a library `scripts/` reaches
 * into, and this script follows that precedent rather than re-litigating it.
 * Every request shape below was verified live against a running stack before
 * being written here (see the unit's report for the probe transcript); where
 * a shape disagreed with a guess drawn from the client source, the LIVE
 * answer is what is encoded.
 *
 * ## Idempotency
 *
 * Every entity is looked up BY NAME first (the list route each screen itself
 * calls) and reused if found, created otherwise. Re-running this script
 * against a stack it already seeded changes nothing and is safe.
 *
 * ## What this script cannot do on the e2e-standalone stack (W4a)
 *
 * `deploy/docker-compose.e2e-standalone.yml` — the compose file
 * `scripts/e2e-stack.sh` brings up — carries NO worker and NO llm-mock
 * service (unlike `docker-compose.standalone-full.yml`, which does). This
 * was confirmed live: `runtimecomposition`'s `AgentStart` use case is
 * therefore never composed, and every route gated on it —
 * `POST /elitea_core/messages/prompt_lib/{project}/{conversationId}` (a
 * chat turn), `PUT /pipeline_schedules/prompt_lib/{project}/{versionId}` (a
 * pipeline schedule) and pipeline triggers generally — answers
 * `405 method not allowed`, not a slow timeout. Without `--vllm-api-base`/
 * `--vllm-model` this script still creates the conversation and its
 * participant (the composer/empty-state shots need that much), attempts the
 * schedule write once and records whether it landed, and does NOT pretend
 * to produce a real assistant turn.
 *
 * ## What W4b adds: a REAL model, on a stack that has a worker
 *
 * `--vllm-api-base`/`--vllm-model` (`docker-compose.standalone-full.yml`,
 * which composes a worker) wire a real OpenAI-compatible endpoint through
 * `deploy/scripts/seed-llm-api.py` (`seedRealModel`), then this script sends
 * 3 real turns through the actual chat composer (`sendRealChatTurns`) and,
 * with `--wiki-repository`, creates a `wikis`-type DeepWiki toolkit
 * (`createWikiToolkit` — SQL only; the product has no route that creates
 * one) pointed at that repository, with a MOCK embedding model
 * (`seedMockEmbedding`) since a chat/reasoning-only vLLM endpoint typically
 * serves no `/v1/embeddings` at all. `--project-name` targets a project
 * other than the fixed `"1"` W4a always used — see `getOrCreateProject`'s
 * own doc for why that matters on a shared standalone stack.
 *
 * Two admin-console platform_config flags this seed also cannot touch by any
 * route (`moderation_enabled`, `support_chat_enabled`) are hardcoded `false`
 * literals in `PlatformSettings` (`internal/api/v2/eliteacore/handler.go`),
 * not database rows — there is no write path for them at all on this build.
 *
 * ## Output
 *
 * Prints a JSON map of every id/name this run produced or reused, and writes
 * it to `--seed-map` (default `playwright-results/docs-seed.json`, gitignored).
 * `docs-shots.ts` reads that file to resolve manifest route placeholders
 * (`:agentId`, `:pipelineId`, `:conversationId`, `:toolkitId`, `:skillId`).
 *
 * ## CLI
 *
 *   npx tsx scripts/docs-seed.ts [--base-url URL] [--seed-map PATH]
 *                                [--skip-platform-flags] [--skip-publish]
 *                                [--project-name NAME] [--admin-email EMAIL]
 *                                [--vllm-api-base URL] [--vllm-model NAME]
 *                                [--vllm-api-key KEY]
 *                                [--wiki-repository owner/repo]
 *                                [--postgres-container NAME]
 */
import { execFileSync } from "node:child_process";
import { existsSync, mkdirSync, writeFileSync } from "node:fs";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

import {
  chromium,
  type APIRequestContext,
  type Browser,
  type BrowserContext,
  type Page,
} from "playwright";

import { BASE_URL, STORAGE_STATE } from "../playwright.config";

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const WEB_ROOT = resolve(SCRIPT_DIR, "..");
/** Two levels up from `apps/elitea-web` — where `deploy/scripts/*.py` live. */
const REPO_ROOT = resolve(WEB_ROOT, "..", "..");
const DEFAULT_SEED_MAP_PATH = join(
  WEB_ROOT,
  "playwright-results/docs-seed.json",
);

const PREFIX = "autotest_docs_";
const DEFAULT_PROJECT_ID = "1";
/**
 * The project every helper below writes into. A `let`, not the original
 * `const "1"`: `resolveProjectId` (unit W4b) reassigns it once, at the very
 * start of `main`, before any helper runs — every helper closes over this
 * binding by name rather than taking a `projectId` parameter, so the
 * reassignment is enough and no call site below needed to change.
 */
let PROJECT_ID: string = DEFAULT_PROJECT_ID;

interface Cli {
  readonly baseUrl: string;
  readonly seedMapPath: string;
  readonly skipPlatformFlags: boolean;
  readonly skipPublish: boolean;
  /**
   * When set, the seed targets THIS project by name instead of the fixed
   * `"1"` ("Default Project") every prior invocation used — unit W4b, for a
   * stack that is not `docker-compose.e2e-standalone.yml` and where seeding
   * into project 1 is not what the caller wants (a shared standalone stack
   * this script does not own). The project is looked up by name first
   * (idempotent, like every other entity below) and created via
   * `POST /projects/project/administration` if missing, with `--admin-email`
   * as the project's own admin so that persona's ordinary "seeded" storage
   * state (auth.setup.ts pins member/admin to "Default Project", NOT this
   * one) still works for every route this script's entities need — the
   * caller picks the project explicitly through the switcher on every OTHER
   * route, the same way a real user would.
   */
  readonly projectName: string | undefined;
  /** The project's own admin, granted at creation (see projectName above). */
  readonly adminEmail: string;
  /**
   * A REAL, reachable OpenAI-compatible endpoint (unit W4b) — vLLM, Ollama,
   * LM Studio, TGI. Seeded through `deploy/scripts/seed-llm-api.py`, the same
   * product API `standalone-stack.sh seed-llm` uses, so a model row seeded
   * here is indistinguishable from one an operator saves in the UI. NO `/v1`
   * suffix (bifrost appends it). Omitted ⇒ no model is wired and every
   * model-dependent step below (real chat turns, a real pipeline run,
   * DeepWiki generation) is skipped with a note, exactly as before.
   */
  readonly vllmApiBase: string | undefined;
  readonly vllmModel: string | undefined;
  readonly vllmApiKey: string;
  /**
   * A repository DeepWiki can actually clone (unit W4b) — public, over
   * HTTPS, no credential required. Omitted ⇒ no wiki toolkit is created (the
   * product has no route that creates one at all; see createWikiToolkit's
   * own doc for why this is SQL and not an API call).
   */
  readonly wikiRepository: string | undefined;
  /**
   * The running postgres container `createWikiToolkit` and
   * `writePlatformConfigFlags` exec into. Overridable because more than one
   * compose project's postgres can be running on the same host at once (an
   * `elitea-e2e` stack alongside `elitea-standalone`, say), and the
   * name-pattern heuristic those two functions otherwise fall back to
   * (`/e2e/i`) would pick the wrong one — or none — for a project named
   * anything else.
   */
  readonly postgresContainer: string | undefined;
}

function parseArgs(argv: readonly string[]): Cli {
  let baseUrl = BASE_URL;
  let seedMapPath = DEFAULT_SEED_MAP_PATH;
  let skipPlatformFlags = false;
  let skipPublish = false;
  let projectName: string | undefined;
  let adminEmail = "e2e-admin@autotest.local";
  let vllmApiBase: string | undefined;
  let vllmModel: string | undefined;
  let vllmApiKey = "not-used-by-self-hosted";
  let wikiRepository: string | undefined;
  let postgresContainer: string | undefined;
  for (let i = 0; i < argv.length; i += 1) {
    const arg = argv[i];
    switch (arg) {
      case "--base-url": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--base-url requires a value");
        baseUrl = value.replace(/\/+$/, "");
        break;
      }
      case "--seed-map": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--seed-map requires a value");
        seedMapPath = resolve(value);
        break;
      }
      case "--skip-platform-flags":
        skipPlatformFlags = true;
        break;
      case "--skip-publish":
        skipPublish = true;
        break;
      case "--project-name": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--project-name requires a value");
        projectName = value;
        break;
      }
      case "--admin-email": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--admin-email requires a value");
        adminEmail = value;
        break;
      }
      case "--vllm-api-base": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--vllm-api-base requires a value");
        vllmApiBase = value.replace(/\/v1\/?$/, "");
        break;
      }
      case "--vllm-model": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--vllm-model requires a value");
        vllmModel = value;
        break;
      }
      case "--vllm-api-key": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--vllm-api-key requires a value");
        vllmApiKey = value;
        break;
      }
      case "--wiki-repository": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--wiki-repository requires a value");
        wikiRepository = value;
        break;
      }
      case "--postgres-container": {
        const value = argv[++i];
        if (value === undefined) throw new Error("--postgres-container requires a value");
        postgresContainer = value;
        break;
      }
      default:
        throw new Error(`unknown argument: ${arg}`);
    }
  }
  return {
    baseUrl,
    seedMapPath,
    skipPlatformFlags,
    skipPublish,
    projectName,
    adminEmail,
    vllmApiBase,
    vllmModel,
    vllmApiKey,
    wikiRepository,
    postgresContainer,
  };
}

/** Same one-shot "run setup once" pattern docs-shots.ts uses. */
function ensureStorageState(persona: "member" | "admin", baseUrl: string): void {
  if (existsSync(STORAGE_STATE[persona])) return;
  console.log(
    `docs-seed: missing storageState for ${persona} — running ` +
      `"npx playwright test --project=setup" against ${baseUrl} first.`,
  );
  execFileSync("npx", ["playwright", "test", "--project=setup"], {
    cwd: WEB_ROOT,
    stdio: "inherit",
    env: { ...process.env, PLAYWRIGHT_BASE_URL: baseUrl, E2E_REUSE_STACK: "1" },
  });
  if (!existsSync(STORAGE_STATE[persona])) {
    throw new Error(
      `docs-seed: setup ran but ${STORAGE_STATE[persona]} still does not exist (persona: ${persona}).`,
    );
  }
}

/** Thin JSON helper over an APIRequestContext. Throws with the body on a bad status. */
async function api(
  request: APIRequestContext,
  baseUrl: string,
  method: "GET" | "POST" | "PUT" | "PATCH" | "DELETE",
  path: string,
  data?: unknown,
): Promise<{ status: number; body: any }> {
  const url = `${baseUrl}/api/v2${path}`;
  const response = await request.fetch(url, {
    method,
    data,
    headers: data !== undefined ? { "Content-Type": "application/json" } : undefined,
  });
  const status = response.status();
  const text = await response.text();
  let body: unknown;
  try {
    body = text === "" ? {} : JSON.parse(text);
  } catch {
    body = text;
  }
  return { status, body };
}

function requireOk(
  label: string,
  result: { status: number; body: any },
  okStatuses: readonly number[] = [200, 201],
): any {
  if (!okStatuses.includes(result.status)) {
    throw new Error(
      `${label}: ${result.status} ${JSON.stringify(result.body).slice(0, 300)}`,
    );
  }
  return result.body;
}

interface SeedResult {
  readonly projectId: string;
  readonly agentId: string;
  readonly agentVersionId: string;
  readonly agentPublished: boolean;
  readonly pipelineId: string;
  readonly pipelineVersionId: string;
  readonly pipelineScheduleOk: boolean;
  readonly skillId: string;
  readonly conversationId: string;
  /** How many of the 3 attempted turns actually landed a non-error assistant row (0 when no model is wired). */
  readonly chatTurnsLanded: number;
  /** The `elitea_title` of the real model wired in, or absent when none was. */
  readonly realModelName: string | undefined;
  /** The `wikis`-type toolkit's id (unit W4b), or absent when none was created. */
  readonly wikiToolkitId: string | undefined;
  readonly toolkitId: string; // github — the default `:toolkitId` resolution
  readonly openapiToolkitId: string;
  readonly jiraToolkitId: string;
  readonly artifactToolkitId: string;
  readonly mcpToolkitId: string;
  readonly githubCredentialId: string;
  readonly jiraCredentialId: string;
  readonly openapiCredentialId: string;
  readonly aiCredentialId: string;
  readonly secretName: string;
  readonly bucketName: string;
  readonly tagIds: readonly string[];
  readonly tokenUuid: string;
  readonly mcpPrebuiltKey: string;
  readonly platformFlagsWritten: boolean;
  readonly notes: readonly string[];
}

/** Finds a row by `name` in a `{rows:[...]}`-or-array-shaped list, or undefined. */
function findByName<T extends { name?: unknown }>(
  rows: readonly T[],
  name: string,
): T | undefined {
  return rows.find((row) => row.name === name);
}

async function listRows(
  request: APIRequestContext,
  baseUrl: string,
  path: string,
): Promise<readonly any[]> {
  const { body } = await api(request, baseUrl, "GET", path);
  if (Array.isArray(body)) return body;
  if (Array.isArray(body?.rows)) return body.rows;
  if (Array.isArray(body?.items)) return body.items;
  if (Array.isArray(body?.buckets)) return body.buckets;
  return [];
}

/** The caller's own numeric user id — `GET /social/author`'s `id`, a string
 * the route never zero-pads or prefixes (unit W4b, `createWikiToolkit`'s
 * `owner_id`/`author_id`). */
async function resolveOwnUserId(
  request: APIRequestContext,
  baseUrl: string,
): Promise<string> {
  const { status, body } = await api(request, baseUrl, "GET", "/social/author");
  const id = body?.id;
  if (status !== 200 || typeof id !== "string" || id === "") {
    throw new Error(`resolveOwnUserId: GET /social/author answered ${status}: ${JSON.stringify(body).slice(0, 200)}`);
  }
  return id;
}

/**
 * GET-or-create the project this run seeds into (unit W4b).
 *
 * `--project-name` is how a caller points this script at a project other
 * than the fixed `"1"` every prior invocation used — a shared standalone
 * stack this script does not own, where writing into "Default Project"
 * would mix docs fixtures into whatever else lives there. Looked up by name
 * first (idempotent, like every other entity in this file) through the SAME
 * route the app's own project switcher reads
 * (`GET /projects/project/default/1` — the literal "1" is a query subject,
 * not a scope, see that route's own doc), and created via
 * `POST /projects/project/administration` otherwise — central-permission
 * gated, so `adminRequest` (not `request`) is required.
 *
 * `adminEmail` becomes the new project's own project-admin
 * (`project_admin_email`), which matters more than it looks: `e2e/
 * auth.setup.ts`'s "seeded" persona pin only ever selects "Default
 * Project" — it has no notion of this script's project at all — so a
 * caller whose ordinary sign-in leaves them on a DIFFERENT project has to
 * switch to this one through the app's own switcher on every route that
 * needs it (the same thing a real user would do), which only works if they
 * hold a role here in the first place.
 */
async function getOrCreateProject(
  adminRequest: APIRequestContext,
  baseUrl: string,
  name: string,
  adminEmail: string,
): Promise<string> {
  // Unit W4c: this USED to list `/projects/project/default/1` — the same
  // route the app's own project switcher reads — which is scoped to
  // projects the calling admin is a MEMBER of (`ListCurrentUserProjects`).
  // `POST /projects/project/administration` makes `adminEmail` the new
  // project's admin, NOT the identity behind `adminRequest`, so that admin
  // is never a member of a project this function just created — every
  // rerun against a long-lived stack therefore found nothing and created
  // ANOTHER project of the same name. Measured live: three "docs-shots"
  // projects (ids 8, 9, 10) exist on the shared `elitea-standalone` stack
  // from three separate runs, none of them reused.
  // `GET /admin/projects/{mode}?search=` (`internal/api/v2/admin/
  // projects.go`) is unfiltered by membership — it is the admin Projects
  // table's own backing route — so it sees a project this identity was
  // never added to. `search` is a substring match server-side, so the
  // filter below still requires an EXACT `name` match against the returned
  // rows (not `findByName`, which only returns the first match — this needs
  // every match, to pick deterministically among duplicates).
  // Picking the LOWEST id keeps this deterministic if a stack already has
  // duplicates (as this one now does) — it does not retroactively pick out
  // any ONE of them as "the" project a caller cares about, so a unit that
  // needs a SPECIFIC pre-existing project (not just "a project with this
  // name") should still target it explicitly, the way this unit's own
  // capture run does via `--project-id`.
  const rows = await listRows(
    adminRequest,
    baseUrl,
    `/admin/projects/administration?search=${encodeURIComponent(name)}`,
  );
  const matches = rows.filter((row) => (row as { name?: unknown }).name === name);
  if (matches.length > 0) {
    const lowestId = matches.reduce((min, row) => {
      const id = Number((row as { id: unknown }).id);
      return id < min ? id : min;
    }, Number((matches[0] as { id: unknown }).id));
    return String(lowestId);
  }
  const created = requireOk(
    `create project ${name}`,
    await api(adminRequest, baseUrl, "POST", "/projects/project/administration", {
      name,
      project_admin_email: adminEmail,
    }),
    [200, 201],
  );
  const id = created?.id;
  if (id === undefined || id === null) {
    throw new Error(`getOrCreateProject(${name}): response carried no id: ${JSON.stringify(created).slice(0, 300)}`);
  }
  return String(id);
}

/** GET-or-create an application (agent or pipeline share this table/route).
 *
 * `agentsType`: the list route defaults to AGENTS only — a pipeline never
 * appears in an unfiltered read (confirmed live: `?agents_type=pipeline` is
 * required, and its absence duplicated a fresh pipeline every rerun before
 * this was found). Agents pass `undefined` (the default filter already
 * suits them); pipelines pass `'pipeline'`. */
async function getOrCreateApplication(
  request: APIRequestContext,
  baseUrl: string,
  name: string,
  body: Record<string, unknown>,
  agentsType?: "pipeline",
): Promise<{ id: string; versionId: string; reused: boolean }> {
  const query = agentsType === undefined ? "" : `&agents_type=${agentsType}`;
  const rows = await listRows(
    request,
    baseUrl,
    `/elitea_core/applications/prompt_lib/${PROJECT_ID}?page_size=100${query}`,
  );
  const existing = findByName(rows, name);
  if (existing) {
    const detail = requireOk(
      `read existing application ${name}`,
      await api(
        request,
        baseUrl,
        "GET",
        `/elitea_core/application/prompt_lib/${PROJECT_ID}/${String(existing.id)}`,
      ),
    );
    const versionId = String(detail.version_details?.id ?? "");
    if (versionId !== "") {
      return { id: String(existing.id), versionId, reused: true };
    }
  }
  const created = requireOk(
    `create application ${name}`,
    await api(request, baseUrl, "POST", `/elitea_core/applications/prompt_lib/${PROJECT_ID}`, {
      name,
      ...body,
    }),
  );
  return {
    id: String(created.id),
    versionId: String(created.version_details?.id ?? ""),
    reused: false,
  };
}

async function getOrCreateSkill(
  request: APIRequestContext,
  baseUrl: string,
  name: string,
): Promise<string> {
  const rows = await listRows(
    request,
    baseUrl,
    `/elitea_core/skills/prompt_lib/${PROJECT_ID}?page_size=100`,
  );
  const existing = findByName(rows, name);
  if (existing) return String(existing.id);
  const created = requireOk(
    `create skill ${name}`,
    await api(request, baseUrl, "POST", `/elitea_core/skills/prompt_lib/${PROJECT_ID}`, {
      name,
      description: `${PREFIX}docs seed skill`,
      versions: [
        {
          name: "base",
          instructions:
            "You are a documentation-writing helper. Answer in one short, plain sentence.",
          tags: [],
        },
      ],
    }),
  );
  return String(created.id);
}

async function getOrCreateConfiguration(
  request: APIRequestContext,
  baseUrl: string,
  title: string,
  type: string,
  data: Record<string, unknown>,
): Promise<string> {
  const rows = await listRows(
    request,
    baseUrl,
    `/configurations/configurations/${PROJECT_ID}?page_size=200`,
  );
  const existing = rows.find((row) => row.elitea_title === title);
  if (existing) return String(existing.id);
  const created = requireOk(
    `create configuration ${title}`,
    await api(request, baseUrl, "POST", `/configurations/configurations/${PROJECT_ID}`, {
      type,
      elitea_title: title,
      label: title,
      shared: false,
      data,
    }),
  );
  return String(created.id);
}

async function getOrCreateToolkit(
  request: APIRequestContext,
  baseUrl: string,
  name: string,
  type: string,
  settings: Record<string, unknown>,
): Promise<string> {
  const rows = await listRows(
    request,
    baseUrl,
    `/elitea_core/tools/prompt_lib/${PROJECT_ID}?limit=200`,
  );
  const existing = findByName(rows, name);
  if (existing) return String(existing.id);
  const created = requireOk(
    `create toolkit ${name}`,
    await api(request, baseUrl, "POST", `/elitea_core/tools/prompt_lib/${PROJECT_ID}`, {
      name,
      type,
      settings,
    }),
  );
  return String(created.id);
}

/** Attaches a toolkit to an agent version — the ToolMenu picker's own PATCH,
 * body shape confirmed live (NOT `application_id`/`version_id`, which 500s). */
async function attachToolkitToAgent(
  request: APIRequestContext,
  baseUrl: string,
  toolkitId: string,
  agentId: string,
  agentVersionId: string,
): Promise<void> {
  const result = await api(
    request,
    baseUrl,
    "PATCH",
    `/elitea_core/tool/prompt_lib/${PROJECT_ID}/${toolkitId}`,
    {
      entity_id: Number(agentId),
      entity_version_id: Number(agentVersionId),
      entity_type: "agent",
      has_relation: true,
    },
  );
  // 201 on a fresh attach; a re-run's relation already exists and the same
  // route answers 201 again (INSERT ... ON CONFLICT-shaped) or occasionally
  // 500 "failed to add the tool relation" for an already-present row on some
  // builds — either way harmless to this seed, so only a hard 4xx surfaces.
  if (result.status >= 400 && result.status !== 500) {
    throw new Error(
      `attachToolkitToAgent(${toolkitId} -> ${agentId}): ${result.status} ${JSON.stringify(result.body).slice(0, 200)}`,
    );
  }
}

async function getOrCreateConversation(
  request: APIRequestContext,
  baseUrl: string,
  name: string,
): Promise<string> {
  const rows = await listRows(
    request,
    baseUrl,
    `/elitea_core/conversations/prompt_lib/${PROJECT_ID}?page_size=100`,
  );
  const existing = findByName(rows, name);
  if (existing) return String(existing.id);
  const created = requireOk(
    `create conversation ${name}`,
    await api(request, baseUrl, "POST", `/elitea_core/conversations/prompt_lib/${PROJECT_ID}`, {
      name,
    }),
  );
  return String(created.id);
}

/**
 * Attaches the AGENT as a participant, AND the human as one too (unit W4b).
 *
 * The second one is not optional: `src/pages/agents/ui/ChatWithAgentButton.tsx`'s
 * own doc comment says why — "nothing server-side creates one on the REST
 * path… the agent resolver joins the author through it — a conversation
 * without the mapping refuses every send as 422." `ResolveCurrentApplicationTurn`
 * (`services/elitea-main/internal/db/queries/agent_chat.sql`) INNER JOINs a
 * `chat_participants` row with `entity_name = 'user'` and
 * `entity_meta->>'id' = actor_user_id` before it ever looks at the
 * application side; W4a's version of this function never added one, which
 * is why `sendRealChatTurns` answered `422 unsupported_agent_execution` on
 * every attempt (measured live) even with a real, reachable model wired in
 * — the conversation had no author participant to resolve the sender
 * against, not a model problem at all.
 */
async function ensureParticipant(
  request: APIRequestContext,
  baseUrl: string,
  conversationId: string,
  userId: string,
  agentId: string,
  agentVersionId: string,
  agentName: string,
): Promise<void> {
  const result = await api(
    request,
    baseUrl,
    "POST",
    `/elitea_core/participants/prompt_lib/${PROJECT_ID}/${conversationId}`,
    [
      { entity_name: "user", entity_meta: { id: Number(userId) } },
      {
        entity_name: "application",
        entity_meta: { id: agentId, name: agentName, project_id: PROJECT_ID },
        entity_settings: { version_id: agentVersionId, agent_type: "openai", variables: [], icon_meta: {} },
      },
    ],
  );
  if (result.status >= 400) {
    throw new Error(
      `ensureParticipant: ${result.status} ${JSON.stringify(result.body).slice(0, 200)}`,
    );
  }
}

async function ensureTag(
  request: APIRequestContext,
  baseUrl: string,
  name: string,
): Promise<string> {
  const rows = await listRows(
    request,
    baseUrl,
    `/elitea_core/tags/prompt_lib/${PROJECT_ID}?page_size=200`,
  );
  const existing = findByName(rows, name);
  if (existing) return String(existing.id);
  const created = requireOk(
    `create tag ${name}`,
    await api(request, baseUrl, "POST", `/elitea_core/tags/prompt_lib/${PROJECT_ID}`, { name }),
  );
  return String(created.id);
}

async function ensureSecret(
  request: APIRequestContext,
  baseUrl: string,
  name: string,
): Promise<string> {
  const rows = await listRows(
    request,
    baseUrl,
    `/secrets/secrets/default/${PROJECT_ID}`,
  );
  if (rows.some((row) => row.name === name)) return name;
  requireOk(
    `create secret ${name}`,
    await api(request, baseUrl, "POST", `/secrets/secrets/default/${PROJECT_ID}`, {
      name,
      value: `${PREFIX}not_a_real_value`,
    }),
  );
  return name;
}

async function ensureBucketWithFile(
  request: APIRequestContext,
  baseUrl: string,
  bucketName: string,
): Promise<string> {
  const rows = await listRows(request, baseUrl, `/artifacts/buckets/${PROJECT_ID}`);
  if (!rows.some((row) => row.name === bucketName)) {
    const created = await api(
      request,
      baseUrl,
      "POST",
      `/artifacts/buckets/${PROJECT_ID}`,
      { name: bucketName },
    );
    if (![200, 201, 409].includes(created.status)) {
      throw new Error(
        `ensureBucketWithFile: create bucket ${created.status} ${JSON.stringify(created.body).slice(0, 200)}`,
      );
    }
  }
  // Upload is idempotent (`?overwrite=true`), so this is safe on every rerun
  // and also gives the bucket its SECOND file (the manifest wants two).
  for (const [fileName, content] of [
    ["readme.txt", "Elitea docs screenshot fixture file."],
    ["notes.md", "# Docs seed\n\nA second fixture file for the bucket file list.\n"],
  ] as const) {
    const uploadUrl = `${baseUrl}/api/v2/artifacts/objects/${PROJECT_ID}/${bucketName}?overwrite=true`;
    const response = await request.post(uploadUrl, {
      multipart: {
        file: { name: fileName, mimeType: "text/plain", buffer: Buffer.from(content) },
      },
    });
    if (response.status() !== 201) {
      throw new Error(
        `ensureBucketWithFile: upload ${fileName} -> ${response.status()}: ${(await response.text()).slice(0, 200)}`,
      );
    }
  }
  return bucketName;
}

async function ensureToken(
  request: APIRequestContext,
  baseUrl: string,
  name: string,
): Promise<string> {
  const { body } = await api(request, baseUrl, "GET", "/auth/token/");
  const rows: readonly any[] = Array.isArray(body) ? body : [];
  const existing = rows.find((row) => row.name === name);
  if (existing) return String(existing.uuid);
  const created = requireOk(
    `create token ${name}`,
    await api(request, baseUrl, "POST", "/auth/token/", { name, expires: null }),
    [200, 201],
  );
  return String(created.uuid);
}

/**
 * Mints a FRESH, project-scoped PAT and returns its raw bearer (unit W4b,
 * `seedRealModel`/`seedMockEmbedding`'s `--token`).
 *
 * Not reused like `ensureToken` above: the list route (`GET /auth/token/`)
 * never re-reveals a token's raw value (`presentToken(record, false)` —
 * `services/elitea-main/internal/api/v2/auth/tokens.go`), so a rerun has
 * nothing to read back and minting again is the only option. One extra
 * token row per run is the same cost this file already accepts for the
 * platform_config write below; nothing downstream of this script reads
 * `auth_core__token` by name.
 */
async function mintProjectScopedToken(
  request: APIRequestContext,
  baseUrl: string,
  name: string,
  projectId: string,
): Promise<string> {
  const created = requireOk(
    `mint token ${name}`,
    await api(request, baseUrl, "POST", "/auth/token/", {
      name,
      expires: null,
      project_id: Number(projectId),
    }),
    [200, 201],
  );
  const token = created?.token;
  if (typeof token !== "string" || token === "") {
    throw new Error(
      `mintProjectScopedToken(${name}): response carried no token: ${JSON.stringify(created).slice(0, 200)}`,
    );
  }
  return token;
}

/**
 * Wires a REAL model into `projectId` through the product's own API (unit
 * W4b) — `deploy/scripts/seed-llm-api.py`, the exact script
 * `standalone-stack.sh seed-llm` shells out to, so a model row seeded here
 * is written the same way an operator's own credential would be (see that
 * script's own doc on why `status_ok` matters). Returns the model's own
 * `elitea_title` (the name a chat caller sends) on success, or `undefined`
 * with a note appended on failure — this never throws, because a model that
 * fails to wire must not abort every OTHER entity this script seeds; it
 * only means the model-dependent steps downstream (real chat turns, a real
 * pipeline run, DeepWiki generation) stay in their pre-W4b "cannot do this
 * here" shape.
 *
 * `vllmApiBase` must carry NO trailing `/v1` (bifrost appends it itself;
 * `parseArgs` already strips one if the caller passed it anyway).
 */
async function seedRealModel(
  request: APIRequestContext,
  baseUrl: string,
  projectId: string,
  vllmApiBase: string,
  vllmModel: string,
  vllmApiKey: string,
  notes: string[],
): Promise<string | undefined> {
  const modelName = `vllm/${vllmModel.replace(/^vllm\//, "")}`;
  try {
    const token = await mintProjectScopedToken(request, baseUrl, `${PREFIX}llm_seed_token`, projectId);
    execFileSync(
      "python3",
      [
        join(REPO_ROOT, "deploy/scripts/seed-llm-api.py"),
        "--base-url", baseUrl,
        "--project", projectId,
        "--token", token,
        "--credential-title", `${PREFIX}vllm_credential`,
        "--credential-type", "vllm",
        "--api-key", vllmApiKey,
        "--api-base", vllmApiBase,
        "--model", modelName,
      ],
      { stdio: "inherit" },
    );
    return modelName;
  } catch (error) {
    notes.push(
      `seedRealModel: seed-llm-api.py failed (${error instanceof Error ? error.message : String(error)}) — ` +
        "real chat turns, the pipeline run and DeepWiki generation are skipped; every other entity this script seeds is unaffected.",
    );
    return undefined;
  }
}

/**
 * Wires a MOCK embedding model into `projectId` (unit W4b), for DeepWiki's
 * `embedding_model` setting — the real vLLM endpoint this stack's chat model
 * uses answers `404 Not Found` on `/v1/embeddings` (measured live: it is a
 * chat/reasoning-only server), so a real embedding model is simply not
 * available. `llm-mock`'s `/v1/embeddings` (`deploy/mock-llm/server.py`) is
 * already reachable from the gateway (`llm-mock:8090` is in the default
 * `GATEWAY_EGRESS_ALLOWLIST`) and answers deterministic vectors — real
 * plumbing, a fake vector, which is enough for the engine's indexing step to
 * proceed rather than refuse for lack of any embedding model at all. This
 * also seeds an unused `vllm/E2E-MOCK-MODEL` chat-model row alongside the
 * embedding one (the two share one credential in this script and
 * `seed-llm-api.py` always seeds a chat row): harmless clutter, and it never
 * wins the "first model seeded" resolution `seedRealModel` above already
 * holds (see that script's own id-ordering note).
 */
async function seedMockEmbedding(
  request: APIRequestContext,
  baseUrl: string,
  projectId: string,
  notes: string[],
): Promise<string | undefined> {
  try {
    const token = await mintProjectScopedToken(request, baseUrl, `${PREFIX}embedding_seed_token`, projectId);
    execFileSync(
      "python3",
      [
        join(REPO_ROOT, "deploy/scripts/seed-llm-api.py"),
        "--base-url", baseUrl,
        "--project", projectId,
        "--token", token,
        "--credential-title", `${PREFIX}mock_embedding_credential`,
        "--credential-type", "vllm",
        "--api-key", `mock-key-project-${projectId}`,
        "--api-base", "http://llm-mock:8090",
        "--model", "vllm/E2E-MOCK-MODEL",
        "--embedding-model", "vllm/E2E-MOCK-EMBEDDING",
      ],
      { stdio: "inherit" },
    );
    return "vllm/E2E-MOCK-EMBEDDING";
  } catch (error) {
    notes.push(
      `seedMockEmbedding: seed-llm-api.py failed (${error instanceof Error ? error.message : String(error)}) — ` +
        "the wiki toolkit's embedding_model stays unset.",
    );
    return undefined;
  }
}

async function ensureMcpPrebuilt(
  adminRequest: APIRequestContext,
  baseUrl: string,
  key: string,
): Promise<boolean> {
  const result = await api(
    adminRequest,
    baseUrl,
    "PUT",
    `/admin/mcp_prebuilt_servers/administration/${key}`,
    {
      key,
      display_name: "GitHub (docs seed)",
      url: "https://autotest.invalid/mcp",
      enabled: true,
    },
  );
  return result.status === 200;
}

/** Finds the container engine (podman preferred, docker fallback) and every
 * RUNNING container name — shared by every raw-SQL exception in this file. */
function listRunningContainers(): { bin: string; names: readonly string[] } {
  const composeCandidates: readonly (readonly [string, readonly string[]])[] = [
    ["podman", ["ps", "--format", "{{.Names}}"]],
    ["docker", ["ps", "--format", "{{.Names}}"]],
  ];
  for (const [bin, args] of composeCandidates) {
    try {
      const out = execFileSync(bin, args, { encoding: "utf8" });
      return { bin, names: out.split("\n").filter((n) => n.trim() !== "") };
    } catch {
      // try the next candidate
    }
  }
  return { bin: "", names: [] };
}

/**
 * Resolves the postgres container the raw-SQL exceptions below exec into.
 *
 * `explicit` (unit W4b, `--postgres-container`) wins verbatim when given —
 * needed the moment more than one compose project's postgres runs on the
 * same host at once (an `elitea-e2e` stack alongside `elitea-standalone`,
 * say): the original `/e2e/i` heuristic below would pick the WRONG one
 * silently (a name match, just not this run's stack) rather than none at
 * all. Without it, the original heuristic — the first running container
 * matching both `/postgres/i` and `/e2e/i` — is kept as the default so
 * every pre-W4b invocation (against `docker-compose.e2e-standalone.yml`,
 * whose postgres container name contains "e2e") is unaffected.
 */
function resolvePostgresContainer(
  names: readonly string[],
  explicit: string | undefined,
): string | undefined {
  if (explicit !== undefined) {
    return names.includes(explicit) ? explicit : undefined;
  }
  return names.find((n) => /postgres/i.test(n) && /e2e/i.test(n));
}

/** The `sqlBin exec … psql` wiring `scripts/e2e-stack.sh` itself uses, minus
 * the container-name-resolution helper library (bash-only) — reimplemented
 * minimally in Node so this script stays a single `tsx` invocation. Best
 * effort: a stack whose compose project name differs, or that has no
 * postgres container reachable by name pattern, gets a clear console
 * warning and every other entity this script seeds is unaffected. */
function writePlatformConfigFlags(postgresContainer: string | undefined): boolean {
  const { bin: containerBin, names } = listRunningContainers();
  const postgresName = resolvePostgresContainer(names, postgresContainer);
  if (containerBin === "" || postgresName === undefined) {
    console.warn(
      "docs-seed: could not find a running postgres container (tried podman/docker ps; " +
        "pass --postgres-container to name it explicitly) — skipping platform_config " +
        "flag writes (mcp_in_menu, voice_features, analytics stay at their defaults).",
    );
    return false;
  }
  const sql = `
    INSERT INTO centry.platform_config (section, key, value, updated_by) VALUES
      ('mcp_configuration', 'mcp_enabled', 'true', 'docs-seed'),
      ('mcp_configuration', 'mcp_in_menu', 'true', 'docs-seed'),
      ('voice_features', 'vite_voice_features_enabled', 'true', 'docs-seed'),
      ('analytics', 'analytics_enabled', 'true', 'docs-seed')
    ON CONFLICT (section, key) DO UPDATE SET value = EXCLUDED.value, updated_by = EXCLUDED.updated_by;
  `;
  try {
    execFileSync(
      containerBin,
      ["exec", "-i", postgresName, "psql", "-U", "elitea", "-d", "elitea", "-c", sql],
      { stdio: "inherit" },
    );
    return true;
  } catch (error) {
    console.warn(
      `docs-seed: platform_config write failed (${error instanceof Error ? error.message : String(error)}) — continuing without it.`,
    );
    return false;
  }
}

/**
 * Creates a `wikis`-type toolkit directly in SQL (unit W4b) — the ONLY way
 * one can be made to exist at all today.
 *
 * `apps/elitea-web/src/entities/wiki/api/wikiToolkitApi.ts`'s own doc
 * comment says so, next to its `WIKI_TOOLKIT_TYPE = 'wikis'` constant:
 * "NOTHING IN THIS SERVICE CREATES SUCH A ROW YET" —
 * `toolkitTypeSchemas` in `internal/api/v2/toolkits/handler.go` carries no
 * entry for it, so `POST /elitea_core/tools/prompt_lib/{project}`
 * (`getOrCreateToolkit` above) would refuse `type: "wikis"` outright. The one
 * existing precedent for making one exist is `scripts/e2e-stack.sh`'s own
 * fixture seed, which runs exactly this INSERT for ITS projects; this
 * function is the same INSERT, parameterised for an arbitrary project
 * instead of the fixed ids 90200/90300 that script owns.
 *
 * `repository` is set DIRECTLY in the toolkit's own settings rather than
 * through a separate `code_toolkit` reference — the other branch
 * `getConfiguredRepoIdentity` (that same API module) already reads, and the
 * one a project with no dedicated code toolkit uses; there is no reason to
 * create a second toolkit here only to point at it.
 *
 * Idempotent by `name` within the project, like every other entity this
 * script seeds — a rerun updates `settings` in place rather than creating a
 * second wiki toolkit.
 */
function createWikiToolkit(
  postgresContainer: string | undefined,
  projectId: string,
  ownerUserId: string,
  name: string,
  repository: string,
  codeToolkitConfigId: string,
  llmModel: string,
  embeddingModel: string | undefined,
  notes: string[],
): string | undefined {
  const { bin: containerBin, names } = listRunningContainers();
  const postgresName = resolvePostgresContainer(names, postgresContainer);
  if (containerBin === "" || postgresName === undefined) {
    notes.push(
      "createWikiToolkit: could not find a running postgres container (tried podman/docker ps; " +
        "pass --postgres-container to name it explicitly) — no wiki toolkit was created; " +
        "deepwiki-workspace/deepwiki-browser stay on the empty/no-wiki-yet state.",
    );
    return undefined;
  }
  const settings = JSON.stringify({
    repository,
    branch: "main",
    llm_model: llmModel,
    // `code_toolkit`: the github CREDENTIAL's `configurations` row id — NOT
    // the `elitea_tools` toolkit id despite the field's name. Measured live
    // (unit W4b): `CredentialResolver.Resolve` (services/elitea-main/
    // internal/api/v2/deepwiki/credentials.go) reads it through
    // `r.configurations.Get(ctx, projectID, toolkitID)`, which is the
    // `configurations` table, and answers `ErrToolkitNotResolvable` ("The
    // requested code toolkit is not a repository configuration in this
    // project") for anything else, an `elitea_tools` id included — the
    // error name is the field name, not the table it actually reads.
    code_toolkit: Number(codeToolkitConfigId),
    ...(embeddingModel !== undefined ? { embedding_model: embeddingModel } : {}),
  });
  // `owner_id`/`author_id` are NOT NULL with no default (measured against a
  // running stack: omitting either fails a null-constraint violation naming
  // a column the INSERT never mentioned). `scripts/e2e-stack.sh`'s own
  // fixture sets `owner_id` to the PROJECT id and `author_id` to a real
  // user id; this follows the same convention rather than inventing a
  // second one.
  const sql = `
    INSERT INTO p_${projectId}.elitea_tools (name, type, description, owner_id, author_id, settings)
    SELECT '${name.replace(/'/g, "''")}', 'wikis',
           'Seeded by scripts/docs-seed.ts (embedded-docs unit W4b)',
           ${Number(projectId)}, ${Number(ownerUserId)},
           '${settings.replace(/'/g, "''")}'::jsonb
    WHERE NOT EXISTS (
      SELECT 1 FROM p_${projectId}.elitea_tools WHERE name = '${name.replace(/'/g, "''")}'
    );
    UPDATE p_${projectId}.elitea_tools
       SET settings = '${settings.replace(/'/g, "''")}'::jsonb
     WHERE name = '${name.replace(/'/g, "''")}' AND type = 'wikis';
    SELECT id FROM p_${projectId}.elitea_tools WHERE name = '${name.replace(/'/g, "''")}' AND type = 'wikis';
  `;
  try {
    const out = execFileSync(
      containerBin,
      ["exec", "-i", postgresName, "psql", "-U", "elitea", "-d", "elitea", "-v", "ON_ERROR_STOP=1", "-tAc", sql],
      { encoding: "utf8" },
    );
    const id = out.trim().split("\n").pop()?.trim();
    if (id === undefined || id === "" || Number.isNaN(Number(id))) {
      throw new Error(`unexpected psql output: ${out}`);
    }
    return id;
  } catch (error) {
    notes.push(
      `createWikiToolkit: SQL failed (${error instanceof Error ? error.message : String(error)}) — ` +
        "no wiki toolkit was created; deepwiki-workspace/deepwiki-browser stay on the empty/no-wiki-yet state.",
    );
    return undefined;
  }
}

/**
 * Sends `count` real turns into an existing conversation through the actual
 * chat composer (unit W4b) — not a raw API call: `startAgentExecution`'s
 * body shape has no fixture precedent this file's "e2e/ is spec-adjacent,
 * not a library" rule lets it read (`e2e/fixtures/api.ts` never calls it
 * either — `fillComposer` there drives the same two testids this function
 * does), and the UI is the one thing every real caller actually uses to send
 * a message.
 *
 * Each turn polls the STORED transcript (`GET /elitea_core/messages/
 * prompt_lib/{project}/{conversationId}`) for a newest assistant row that
 * carries `metadata.is_error` (present, not merely truthy) before moving to
 * the next prompt — the same "presence, not truthiness" rule
 * `e2e/fixtures/api.ts`'s `expectStoredAssistantAnswer` documents, and for
 * the same reason: an absent key means the run has not landed yet, not that
 * it succeeded with nothing to say.
 *
 * Returns the number of turns that actually landed a non-error assistant
 * row (0..count) — never throws, so a model that answers the first prompt
 * and then fails does not erase the ones that worked.
 */
async function sendRealChatTurns(
  memberContext: BrowserContext,
  baseUrl: string,
  projectId: string,
  conversationId: string,
  prompts: readonly string[],
  notes: string[],
): Promise<number> {
  let page: Page | undefined;
  let landed = 0;
  try {
    page = await memberContext.newPage();
    await page.goto(`${baseUrl}/app/chat/${conversationId}`, { waitUntil: "domcontentloaded" });
    for (let turnIndex = 0; turnIndex < prompts.length; turnIndex += 1) {
      const prompt = prompts[turnIndex] as string;
      const input = page.getByTestId("chat-message-input");
      await input.waitFor({ state: "visible", timeout: 30_000 });
      const sendButton = page.getByTestId("chat-send-button");
      // Retried, not filled once: `e2e/fixtures/api.ts`'s `fillComposer` docs
      // a re-render that discards a fill landing before the composer settles.
      let sent = false;
      const fillDeadline = Date.now() + 30_000;
      while (!sent && Date.now() < fillDeadline) {
        await input.fill(prompt);
        if (await sendButton.isEnabled({ timeout: 2_000 }).catch(() => false)) {
          await sendButton.click();
          sent = true;
        }
      }
      if (!sent) {
        notes.push(`sendRealChatTurns: composer never accepted "${prompt.slice(0, 40)}…" — stopping after ${landed} turn(s).`);
        break;
      }
      const turnDeadline = Date.now() + 120_000;
      let outcome: "landed" | "errored" | "timed-out" = "timed-out";
      while (Date.now() < turnDeadline) {
        const { status, body } = await api(memberContext.request, baseUrl, "GET",
          `/elitea_core/messages/prompt_lib/${projectId}/${conversationId}?sort_order=desc&limit=5`);
        const items: readonly any[] = Array.isArray(body?.items) ? body.items : [];
        const assistant = items.find((item) => item.role === "assistant" && "is_error" in (item.metadata ?? {}));
        if (status === 200 && assistant !== undefined) {
          outcome = assistant.metadata?.is_error === true ? "errored" : "landed";
          break;
        }
        await new Promise((r) => setTimeout(r, 2_000));
      }
      if (outcome === "landed") {
        landed += 1;
        continue;
      }
      if (outcome === "errored") {
        notes.push(`sendRealChatTurns: turn ${turnIndex + 1} answered with is_error — stopping after ${landed} turn(s).`);
      } else {
        notes.push(`sendRealChatTurns: turn ${turnIndex + 1} never finished within 120s — stopping after ${landed} turn(s).`);
      }
      break;
    }
  } catch (error) {
    notes.push(`sendRealChatTurns: ${error instanceof Error ? error.message : String(error)} — stopping after ${landed} turn(s).`);
  } finally {
    await page?.close();
  }
  return landed;
}

const PIPELINE_TEMPLATE = `state:
  input:
    type: str
  messages:
    type: list
entry_point: LLM_1
nodes:
  - id: LLM_1
    type: llm
    input:
      - input
    input_mapping:
      system:
        type: fstring
        value: You are a helpful documentation assistant.
      task:
        type: variable
        value: input
    output:
      - messages
    transition: Decision_1
  - id: Decision_1
    type: decision
    nodes:
      - Printer_1
    description: Decide whether to show the printer output.
    default_output: Printer_1
  - id: Printer_1
    type: printer
    input_mapping:
      printer:
        type: variable
        value: messages
    transition: END
`;

/** Long enough (and tagged enough) to clear the publish quality gate's
 * "instructions are too short" critical issue, verified live. */
const AGENT_INSTRUCTIONS =
  "You are the Elitea documentation assistant. You help readers understand " +
  "how to use the Elitea platform: agents, pipelines, toolkits, credentials, " +
  "skills and the chat experience. Answer clearly, in short paragraphs, and " +
  "point to the relevant part of the product when it helps.";

async function main(): Promise<void> {
  const cli = parseArgs(process.argv.slice(2));
  ensureStorageState("member", cli.baseUrl);
  ensureStorageState("admin", cli.baseUrl);

  const browser: Browser = await chromium.launch({
    headless: true,
    args: ["--no-sandbox"],
  });
  let memberContext: BrowserContext | undefined;
  let adminContext: BrowserContext | undefined;
  const notes: string[] = [];

  try {
    memberContext = await browser.newContext({ storageState: STORAGE_STATE.member });
    adminContext = await browser.newContext({ storageState: STORAGE_STATE.admin });
    const req = memberContext.request;
    const adminReq = adminContext.request;
    const baseUrl = cli.baseUrl;

    if (cli.projectName !== undefined) {
      console.log(`docs-seed: resolving project "${cli.projectName}" …`);
      PROJECT_ID = await getOrCreateProject(adminReq, baseUrl, cli.projectName, cli.adminEmail);
      console.log(`docs-seed: project "${cli.projectName}" is id ${PROJECT_ID}.`);
    }

    // A REAL model, wired through the product's own API (unit W4b) — before
    // any entity below, since the agent/pipeline/conversation created next
    // do not name a model of their own and rely on the project's resolved
    // default (the first `llm`-section row, lowest id — see
    // `seedRealModel`'s own doc).
    let realModelName: string | undefined;
    if (cli.vllmApiBase !== undefined && cli.vllmModel !== undefined) {
      console.log("docs-seed: wiring a real model …");
      realModelName = await seedRealModel(
        req,
        baseUrl,
        PROJECT_ID,
        cli.vllmApiBase,
        cli.vllmModel,
        cli.vllmApiKey,
        notes,
      );
    }
    let mockEmbeddingName: string | undefined;
    if (cli.wikiRepository !== undefined) {
      console.log("docs-seed: wiring a mock embedding model (DeepWiki) …");
      mockEmbeddingName = await seedMockEmbedding(req, baseUrl, PROJECT_ID, notes);
    }

    console.log("docs-seed: credentials + toolkits …");
    // `https://autotest.invalid/api` used to sit here. It looked inert (every
    // W4a shot this toolkit feeds is a create-form or a list row, never an
    // actual invocation), but a REAL agent run loads every attached
    // toolkit's tools unconditionally before it ever reaches the model —
    // unit W4b measured this live: `EliteAGitHubAPIWrapper` cannot resolve
    // `base_url` against a host that refuses every connection, and the
    // failure surfaces as an opaque `ToolException` that kills the whole
    // turn, not a scoped "this one tool is unavailable". `https://api.github.com`
    // is real and answers unauthenticated (rate-limited) reads, which is all
    // a public repository needs — harmless for W4a's UI-only shots and
    // required for W4b's real turns.
    const githubCredentialId = await getOrCreateConfiguration(
      req,
      baseUrl,
      `${PREFIX}github_cred`,
      "github",
      { base_url: "https://api.github.com" },
    );
    // `cli.wikiRepository` doubles as "the real repo to use everywhere a
    // repository is needed" (unit W4b) — a project set up for a real-model
    // run and DeepWiki generation has one real target, not two.
    const githubRepository = cli.wikiRepository ?? `${PREFIX}org/${PREFIX}repo`;
    const toolkitId = await getOrCreateToolkit(req, baseUrl, `${PREFIX}github`, "github", {
      repository: githubRepository,
      github_configuration: { elitea_title: `${PREFIX}github_cred`, private: false },
      // `active_branch`/`base_branch`: NOT optional despite the catalogue
      // schema defaulting both to `"main"` — `elitea_sdk/tools/github/
      // __init__.py`'s `get_toolkit` reads `tool['settings']['active_branch']`
      // and `tool['settings']['base_branch']` by PLAIN dict indexing, not
      // `.get()`, so an agent whose github toolkit settings omit either key
      // fails EVERY turn with `ToolException: 'active_branch'` (or
      // `'base_branch'`) the instant tool-loading runs — measured live,
      // unit W4b, before this fix existed. The catalogue's own declared
      // default is never applied by that code path; it must be written
      // here.
      active_branch: "main",
      base_branch: "main",
      // index_data/search_data: added only if this deployment's catalogue
      // offers them for github, so the Indexes tab shots have a real toggle
      // to show without inventing a second, index-only toolkit.
      selected_tools: await (async () => {
        const catalogue = requireOk(
          "read toolkit catalogue",
          await api(req, baseUrl, "GET", `/elitea_core/toolkits/prompt_lib/${PROJECT_ID}`),
        );
        const schemas = catalogue?.github?.properties?.selected_tools?.args_schemas ?? {};
        return ["index_data", "search_data"].filter((tool) => tool in schemas);
      })(),
    });

    const jiraCredentialId = await getOrCreateConfiguration(
      req,
      baseUrl,
      `${PREFIX}jira_cred`,
      "jira",
      {
        base_url: "https://autotest.invalid",
        username: `${PREFIX}not_a_real_user`,
        password: `${PREFIX}not_a_real_pw`,
      },
    );
    const jiraToolkitId = await getOrCreateToolkit(req, baseUrl, `${PREFIX}jira`, "jira", {
      jira_configuration: { elitea_title: `${PREFIX}jira_cred`, private: false },
      url: "https://autotest.invalid",
      selected_tools: [],
    });

    const openapiCredentialId = await getOrCreateConfiguration(
      req,
      baseUrl,
      `${PREFIX}openapi_cred`,
      "openapi",
      {},
    );
    const openapiSpec = JSON.stringify({
      openapi: "3.0.0",
      info: { title: "Docs seed API", version: "1.0.0" },
      servers: [{ url: "https://autotest.invalid/api" }],
      paths: {
        "/status": {
          get: {
            operationId: "get_status",
            summary: "Get status",
            responses: { "200": { description: "ok" } },
          },
        },
      },
    });
    const openapiToolkitId = await getOrCreateToolkit(
      req,
      baseUrl,
      `${PREFIX}openapi`,
      "openapi",
      {
        openapi_configuration: { elitea_title: `${PREFIX}openapi_cred`, private: false },
        spec: openapiSpec,
        selected_tools: ["get_status"],
      },
    );

    const bucketName = "autotest-docs-bucket";
    await ensureBucketWithFile(req, baseUrl, bucketName);
    const artifactToolkitId = await getOrCreateToolkit(
      req,
      baseUrl,
      `${PREFIX}artifact`,
      "artifact",
      { bucket: bucketName, selected_tools: [] },
    );

    const mcpToolkitId = await getOrCreateToolkit(req, baseUrl, `${PREFIX}mcp`, "mcp", {
      url: "https://autotest.invalid/mcp",
      selected_tools: [],
    });

    const aiCredentialId = await getOrCreateConfiguration(
      req,
      baseUrl,
      `${PREFIX}ai_credentials`,
      "open_ai",
      { api_base: "https://autotest.invalid/v1", api_key: `${PREFIX}not_a_real_key` },
    );

    console.log("docs-seed: secret + skill + tags + token …");
    const secretName = await ensureSecret(req, baseUrl, `${PREFIX}secret`);
    const skillId = await getOrCreateSkill(req, baseUrl, `${PREFIX}skill`);
    const tagIds = [
      await ensureTag(req, baseUrl, `${PREFIX}tag_1`),
      await ensureTag(req, baseUrl, `${PREFIX}tag_2`),
    ];
    const tokenUuid = await ensureToken(req, baseUrl, `${PREFIX}token`);

    console.log("docs-seed: agent …");
    const agent = await getOrCreateApplication(req, baseUrl, `${PREFIX}agent`, {
      description: `${PREFIX}documentation assistant agent`,
      type: "agent",
      versions: [
        {
          name: "base",
          agent_type: "openai",
          instructions: AGENT_INSTRUCTIONS,
          conversation_starters: [],
          tags: tagIds,
        },
      ],
    });
    await attachToolkitToAgent(req, baseUrl, toolkitId, agent.id, agent.versionId);

    let agentPublished = false;
    if (!cli.skipPublish) {
      const versions = requireOk(
        `read versions of ${agent.id}`,
        await api(req, baseUrl, "GET", `/elitea_core/versions/prompt_lib/${PROJECT_ID}/${agent.id}`),
      );
      const alreadyPublished = (versions.items ?? []).some(
        (v: { status?: string }) => v.status === "published",
      );
      const publishResult = alreadyPublished
        ? { status: 200, body: { reused: true } }
        : await api(
            req,
            baseUrl,
            "POST",
            `/elitea_core/publish/prompt_lib/${PROJECT_ID}/${agent.id}`,
            { version_name: "v1" },
          );
      agentPublished = publishResult.status === 200 || publishResult.status === 201;
      if (!agentPublished) {
        notes.push(
          `agent publish did not succeed (status ${publishResult.status}: ` +
            `${JSON.stringify(publishResult.body).slice(0, 200)}) — agent-publish-dialog ` +
            "and catalog-agents shots can still show the DIALOG, not a published row.",
        );
      }
    }

    console.log("docs-seed: pipeline …");
    const pipeline = await getOrCreateApplication(req, baseUrl, `${PREFIX}pipeline`, {
      description: `${PREFIX}documentation pipeline (llm -> decision -> printer)`,
      type: "interface",
      versions: [
        {
          name: "base",
          agent_type: "pipeline",
          instructions: PIPELINE_TEMPLATE,
          conversation_starters: [],
          variables: [],
          meta: { step_limit: 25, internal_tools: [] },
        },
      ],
    }, "pipeline");

    let pipelineScheduleOk = false;
    if (pipeline.versionId !== "") {
      const scheduleResult = await api(
        req,
        baseUrl,
        "PUT",
        `/pipeline_schedules/prompt_lib/${PROJECT_ID}/${pipeline.versionId}`,
        { cron: "0 */6 * * *", active: true },
      );
      pipelineScheduleOk = scheduleResult.status === 200 || scheduleResult.status === 201;
      if (!pipelineScheduleOk) {
        notes.push(
          `pipeline schedule write answered ${scheduleResult.status} — this route is gated on ` +
            "the same execution plane as chat sending (runtimecomposition.AgentStart), which " +
            "this compose file does not compose. pipeline-trigger-schedule-panel can only show " +
            "the panel's empty/unset state on this stack.",
        );
      }
    }

    console.log("docs-seed: conversation + participant …");
    const memberUserId = await resolveOwnUserId(req, baseUrl);
    const conversationId = await getOrCreateConversation(req, baseUrl, `${PREFIX}conversation`);
    await ensureParticipant(
      req,
      baseUrl,
      conversationId,
      memberUserId,
      agent.id,
      agent.versionId,
      `${PREFIX}agent`,
    );
    // A real turn needs `POST /elitea_core/messages/prompt_lib/{project}/
    // {conversationId}?execution_contract=...`, which answers 405 on a stack
    // with no worker/llm-mock (see the module doc). This stack has one when
    // `realModelName` is set (unit W4b) — attempt the real send through the
    // actual composer (`sendRealChatTurns`), not a raw API call.
    let chatTurnsLanded = 0;
    if (realModelName !== undefined && memberContext !== undefined) {
      console.log("docs-seed: sending real chat turns …");
      chatTurnsLanded = await sendRealChatTurns(
        memberContext,
        baseUrl,
        PROJECT_ID,
        conversationId,
        [
          "Summarise what a toolkit is in two sentences.",
          `Use the ${PREFIX}github toolkit to look at the repository it is configured for, then tell me its name.`,
          "Write a tiny Python function that adds two numbers, in a fenced code block.",
        ],
        notes,
      );
    } else {
      notes.push(
        "conversation has NO turns: no --vllm-api-base/--vllm-model was given (or seedRealModel " +
          "failed — see its own note above), so the chat-send route has no model to answer with. " +
          "chat-conversation-view and siblings will show an empty conversation with the seeded " +
          "agent as the only participant, not an actual exchange.",
      );
    }
    if (realModelName !== undefined && chatTurnsLanded < 3) {
      notes.push(
        `sendRealChatTurns landed ${chatTurnsLanded}/3 turns — see the notes above for why the ` +
          "rest did not land. Screens depending on the missing turns (a code block for the canvas " +
          "shot, a tool call, etc.) may still show an emptier state than intended.",
      );
    }

    // A DeepWiki `wikis`-type toolkit (unit W4b) — SQL only; see
    // `createWikiToolkit`'s own doc for why no API route can do this.
    let wikiToolkitId: string | undefined;
    if (cli.wikiRepository !== undefined && realModelName !== undefined) {
      console.log("docs-seed: creating the DeepWiki toolkit …");
      // The wiki PAGE BROWSER reads this exact bucket
      // (`GET /artifacts/objects/{project}/wiki-artifacts`, 404 with no
      // bucket at all — measured live) and the host uploads generated pages
      // into it; `ensureBucketWithFile` also seeds two throwaway files,
      // which is harmless here (this bucket is not the artifact-toolkit
      // fixture bucket) but wasteful, so this is a plain bucket-only create.
      const wikiBucketResult = await api(req, baseUrl, "POST", "/artifacts/buckets/" + PROJECT_ID, {
        name: "wiki-artifacts",
      });
      if (![200, 201, 409].includes(wikiBucketResult.status)) {
        notes.push(
          `wiki-artifacts bucket create answered ${wikiBucketResult.status} — DeepWiki's page browser ` +
            "will 404 reading it until this bucket exists.",
        );
      }
      wikiToolkitId = createWikiToolkit(
        cli.postgresContainer,
        PROJECT_ID,
        memberUserId,
        `${PREFIX}wiki`,
        cli.wikiRepository,
        githubCredentialId,
        realModelName,
        mockEmbeddingName,
        notes,
      );
    } else if (cli.wikiRepository !== undefined) {
      notes.push("no wiki toolkit was created: --wiki-repository was given but no model is wired (see the note above).");
    }

    console.log("docs-seed: admin-only fixtures …");
    const mcpPrebuiltKey = "github";
    const mcpPrebuiltOk = await ensureMcpPrebuilt(adminReq, baseUrl, mcpPrebuiltKey);
    if (!mcpPrebuiltOk) {
      notes.push("admin mcp_prebuilt_servers write did not return 200 — mcp-prebuilt-admin may be unseeded.");
    }

    let platformFlagsWritten = false;
    if (!cli.skipPlatformFlags) {
      platformFlagsWritten = writePlatformConfigFlags(cli.postgresContainer);
    }
    if (!platformFlagsWritten) {
      notes.push(
        "platform_config flags (mcp_in_menu, voice_features, analytics) were not written " +
          "(podman/docker exec unavailable, or --skip-platform-flags) — verify " +
          "PlatformSettings' defaults on this build before relying on those shots.",
      );
    }
    notes.push(
      "moderation_enabled and support_chat_enabled cannot be turned on by any route: both are " +
        "hardcoded `false` literals in PlatformSettings (internal/api/v2/eliteacore/handler.go), " +
        "not platform_config rows — apps-catalog and support-assistant are unseedable here.",
    );
    notes.push(
      "cost_budgets_enabled is a composition-time option (WithCostBudgets), not a database row " +
        "— settings-usage cannot be turned on by this script.",
    );

    const result: SeedResult = {
      projectId: PROJECT_ID,
      agentId: agent.id,
      agentVersionId: agent.versionId,
      agentPublished,
      pipelineId: pipeline.id,
      pipelineVersionId: pipeline.versionId,
      pipelineScheduleOk,
      skillId,
      conversationId,
      chatTurnsLanded,
      realModelName,
      wikiToolkitId,
      toolkitId,
      openapiToolkitId,
      jiraToolkitId,
      artifactToolkitId,
      mcpToolkitId,
      githubCredentialId,
      jiraCredentialId,
      openapiCredentialId,
      aiCredentialId,
      secretName,
      bucketName,
      tagIds,
      tokenUuid,
      mcpPrebuiltKey,
      platformFlagsWritten,
      notes,
    };

    mkdirSync(dirname(cli.seedMapPath), { recursive: true });
    writeFileSync(cli.seedMapPath, JSON.stringify(result, null, 2));
    console.log("");
    console.log(JSON.stringify(result, null, 2));
    console.log("");
    console.log(`docs-seed: wrote ${cli.seedMapPath}`);
  } finally {
    await memberContext?.close();
    await adminContext?.close();
    await browser.close();
  }
}

main().catch((error: unknown) => {
  console.error(
    `docs-seed: ${error instanceof Error ? (error.stack ?? error.message) : String(error)}`,
  );
  process.exitCode = 1;
});
