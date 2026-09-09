#!/usr/bin/env npx tsx
/**
 * docs-seed.ts — idempotent fixture seeding for the embedded docs screenshot
 * pass (embedded-docs programme, unit W4a).
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
 * ## What this script cannot do on the e2e-standalone stack
 *
 * `deploy/docker-compose.e2e-standalone.yml` — the compose file
 * `scripts/e2e-stack.sh` brings up — carries NO worker and NO llm-mock
 * service (unlike `docker-compose.standalone-full.yml`, which does). This
 * was confirmed live: `runtimecomposition`'s `AgentStart` use case is
 * therefore never composed, and every route gated on it —
 * `POST /elitea_core/messages/prompt_lib/{project}/{conversationId}` (a
 * chat turn), `PUT /pipeline_schedules/prompt_lib/{project}/{versionId}` (a
 * pipeline schedule) and pipeline triggers generally — answers
 * `405 method not allowed`, not a slow timeout. This script still creates
 * the conversation and its participant (the composer/empty-state shots need
 * that much), attempts the schedule write once and records whether it
 * landed, and does NOT pretend to produce a real assistant turn: there is no
 * marker text to send, because nothing downstream would answer it.
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
} from "playwright";

import { BASE_URL, STORAGE_STATE } from "../playwright.config";

const SCRIPT_DIR = dirname(fileURLToPath(import.meta.url));
const WEB_ROOT = resolve(SCRIPT_DIR, "..");
const DEFAULT_SEED_MAP_PATH = join(
  WEB_ROOT,
  "playwright-results/docs-seed.json",
);

const PREFIX = "autotest_docs_";
const PROJECT_ID = "1";

interface Cli {
  readonly baseUrl: string;
  readonly seedMapPath: string;
  readonly skipPlatformFlags: boolean;
  readonly skipPublish: boolean;
}

function parseArgs(argv: readonly string[]): Cli {
  let baseUrl = BASE_URL;
  let seedMapPath = DEFAULT_SEED_MAP_PATH;
  let skipPlatformFlags = false;
  let skipPublish = false;
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
      default:
        throw new Error(`unknown argument: ${arg}`);
    }
  }
  return { baseUrl, seedMapPath, skipPlatformFlags, skipPublish };
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
  readonly agentId: string;
  readonly agentVersionId: string;
  readonly agentPublished: boolean;
  readonly pipelineId: string;
  readonly pipelineVersionId: string;
  readonly pipelineScheduleOk: boolean;
  readonly skillId: string;
  readonly conversationId: string;
  readonly conversationTurnsOk: boolean;
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

async function ensureParticipant(
  request: APIRequestContext,
  baseUrl: string,
  conversationId: string,
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
      {
        entity_name: "application",
        entity_meta: { id: agentId, name: agentName, project_id: PROJECT_ID },
        entity_settings: { version_id: agentVersionId, agent_type: "openai" },
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

/** The `sqlBin exec … psql` wiring `scripts/e2e-stack.sh` itself uses, minus
 * the container-name-resolution helper library (bash-only) — reimplemented
 * minimally in Node so this script stays a single `tsx` invocation. Best
 * effort: a stack whose compose project name differs, or that has no
 * postgres container reachable by name pattern, gets a clear console
 * warning and every other entity this script seeds is unaffected. */
function writePlatformConfigFlags(): boolean {
  const composeCandidates: readonly (readonly [string, readonly string[]])[] = [
    ["podman", ["ps", "--format", "{{.Names}}"]],
    ["docker", ["ps", "--format", "{{.Names}}"]],
  ];
  let containerBin = "";
  let names: readonly string[] = [];
  for (const [bin, args] of composeCandidates) {
    try {
      const out = execFileSync(bin, args, { encoding: "utf8" });
      containerBin = bin;
      names = out.split("\n").filter((n) => n.trim() !== "");
      break;
    } catch {
      // try the next candidate
    }
  }
  const postgresName = names.find((n) => /postgres/i.test(n) && /e2e/i.test(n));
  if (containerBin === "" || postgresName === undefined) {
    console.warn(
      "docs-seed: could not find a running e2e postgres container (tried podman/docker ps) " +
        "— skipping platform_config flag writes (mcp_in_menu, voice_features, analytics stay at their defaults).",
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

    console.log("docs-seed: credentials + toolkits …");
    const githubCredentialId = await getOrCreateConfiguration(
      req,
      baseUrl,
      `${PREFIX}github_cred`,
      "github",
      { base_url: "https://autotest.invalid/api" },
    );
    const toolkitId = await getOrCreateToolkit(req, baseUrl, `${PREFIX}github`, "github", {
      repository: `${PREFIX}org/${PREFIX}repo`,
      github_configuration: { elitea_title: `${PREFIX}github_cred`, private: false },
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
    const conversationId = await getOrCreateConversation(req, baseUrl, `${PREFIX}conversation`);
    await ensureParticipant(
      req,
      baseUrl,
      conversationId,
      agent.id,
      agent.versionId,
      `${PREFIX}agent`,
    );
    // A real turn needs `POST /elitea_core/messages/prompt_lib/{project}/
    // {conversationId}?execution_contract=...`, which answers 405 on this
    // stack (see the module doc) — not attempted here, recorded instead.
    const conversationTurnsOk = false;
    notes.push(
      "conversation has NO turns: this compose stack has no worker/llm-mock, so " +
        "the chat-send route (gated on runtimecomposition.AgentStart) answers " +
        "405 method not allowed. chat-conversation-view and siblings will show " +
        "an empty conversation with the seeded agent as the only participant, " +
        "not an actual exchange — mask/caption accordingly, or capture on the " +
        "real-model lane stack instead (docker-compose.standalone-full.yml).",
    );

    console.log("docs-seed: admin-only fixtures …");
    const mcpPrebuiltKey = "github";
    const mcpPrebuiltOk = await ensureMcpPrebuilt(adminReq, baseUrl, mcpPrebuiltKey);
    if (!mcpPrebuiltOk) {
      notes.push("admin mcp_prebuilt_servers write did not return 200 — mcp-prebuilt-admin may be unseeded.");
    }

    let platformFlagsWritten = false;
    if (!cli.skipPlatformFlags) {
      platformFlagsWritten = writePlatformConfigFlags();
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
      agentId: agent.id,
      agentVersionId: agent.versionId,
      agentPublished,
      pipelineId: pipeline.id,
      pipelineVersionId: pipeline.versionId,
      pipelineScheduleOk,
      skillId,
      conversationId,
      conversationTurnsOk,
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
