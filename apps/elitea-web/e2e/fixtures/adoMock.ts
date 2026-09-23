/**
 * The fake Azure DevOps (`deploy/ado-mock/server.py`) and the agents that talk
 * to it — shared by the `chat.ado-*.spec.ts` journeys.
 *
 * Why a fixture rather than a helper inside one spec: the ADO image cases split
 * along the toolkit that owns them (`ado_boards` reads comments and work items,
 * `ado_wiki` reads pages through the wiki's git repository) and the two halves
 * need the same credential, the same agent scaffolding and the same journal
 * readers. One copy keeps the two files describing the SAME backend.
 */
import type { APIRequestContext, Page } from '@playwright/test';
import { expect } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  createAgentWithVersion,
  readCallerIdentity,
  readCallerPersonalProjectId,
  readStoredTranscript,
} from './api';

/** Where the fake ADO is published on the host — the journal/state reads below. */
export const ADO_HOST = `http://localhost:${process.env['STANDALONE_ADO_PORT'] ?? '8097'}`;
/** Where the TOOLKIT reaches it: a compose-network address, never the published one. */
export const ADO_ORGANIZATION_URL = process.env['STANDALONE_ADO_URL'] ?? 'http://ado-mock:8097';
export const ADO_PAT = process.env['STANDALONE_ADO_PAT'] ?? 'e2e-ado-pat';
/** The seeded project and wiki — `deploy/ado-mock/server.py`'s `_seed`. */
export const ADO_PROJECT = 'E2E';
export const ADO_WIKI = 'e2ewiki';

export const MOCK_MODEL = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';
export const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/**
 * The python leg, for the reason `chat.qtest-toolkit.spec.ts` gives about its
 * own family: `ado_boards`/`ado_wiki` are SDK toolkits with no counterpart in
 * `services/elitea-worker-rust/src/toolkits/families/`, so a rust-leg run would
 * fail at toolset materialization with `unsupported_toolkit_family` — a fact
 * about the native worker's roadmap, not about these cases. And the image
 * pipeline under test IS the SDK's: `parse_file_content` picks
 * `EliteAImageLoader`, which is what calls the model with the picture.
 */
export const ADO_LEG_REASON =
  'the ado_boards/ado_wiki toolkit families are SDK-only — the native worker refuses them at materialization';

export function onPythonLeg(): boolean {
  return (process.env['E2E_WORKER'] ?? 'rust') === 'python';
}

export interface AdoAttachment {
  readonly id: string;
  readonly name: string;
  readonly md5: string;
  readonly size: number;
  readonly status: number;
  readonly url: string;
}

export interface AdoState {
  readonly attachments: Record<string, AdoAttachment>;
  readonly work_items: Record<string, string>;
  readonly comments: Record<string, readonly number[]>;
  readonly wiki_pages: readonly string[];
  readonly git_items: Record<string, readonly string[]>;
  /** attachment tag (or `git:<repo>:<path>`) -> how many times its BYTES were served. */
  readonly downloads: Record<string, number>;
}

export interface AdoJournalEntry {
  readonly method: string;
  readonly path: string;
  readonly query: string;
  readonly status: number;
  readonly auth_ok: boolean;
  readonly attachment?: string;
  readonly file_name?: string;
  readonly git_path?: string;
  readonly download_count?: number;
  readonly expand?: string;
}

export async function readAdoState(page: Page): Promise<AdoState> {
  const response = await page.request.get(`${ADO_HOST}/__state`, { timeout: 15_000 });
  expect(response.ok(), `the fake ADO must serve its state at ${ADO_HOST}/__state`).toBe(true);
  return (await response.json()) as AdoState;
}

export async function readAdoJournal(page: Page): Promise<readonly AdoJournalEntry[]> {
  const response = await page.request.get(`${ADO_HOST}/__journal`, { timeout: 15_000 });
  expect(response.ok(), `the fake ADO must serve its journal at ${ADO_HOST}/__journal`).toBe(true);
  return (await response.json()) as readonly AdoJournalEntry[];
}

/** Back to the seeded state, journal and download counters included. */
export async function resetAdo(page: Page): Promise<void> {
  const response = await page.request.post(`${ADO_HOST}/__reset`, { timeout: 15_000 });
  expect(response.ok(), 'the fake ADO must be resettable between tests').toBe(true);
}

/**
 * Every image the MODEL was shown, in request order.
 *
 * The mock LLM records `{md5, format, bytes}` per image part
 * (`deploy/mock-llm/server.py`, `_request_images`). It is the only place the
 * bytes are visible AFTER they left the platform, so every "described once",
 * "described again", "never described" claim below is counted here and nowhere
 * else — a description embedded in a comment could have come from a cache, and
 * a model's prose is written by the model that was told what to say.
 */
export interface ShownImage {
  readonly md5: string;
  readonly format: string;
  readonly bytes: number;
}

export function shownImages(entries: readonly unknown[]): readonly ShownImage[] {
  return entries.flatMap(
    (entry) => (entry as { images?: readonly ShownImage[] }).images ?? [],
  );
}

export interface AdoAgent {
  readonly projectId: string;
  readonly conversationId: string;
  readonly toolkitId: string;
  readonly dispose: () => Promise<void>;
}

export interface AdoToolkitSpec {
  /** `ado_boards` or `ado_wiki`. */
  readonly type: 'ado_boards' | 'ado_wiki';
  readonly selectedTools: readonly string[];
  /** Extra settings beyond the credential + project, e.g. `default_wiki_identifier`. */
  readonly settings?: Readonly<Record<string, unknown>>;
  /** A second toolkit instance in the same agent — ELITEA-2610's per-instance claim. */
  readonly second?: {
    readonly selectedTools: readonly string[];
    readonly settings?: Readonly<Record<string, unknown>>;
  };
}

async function createCredential(
  request: APIRequestContext,
  projectId: string,
  title: string,
): Promise<void> {
  const credential = await request.post(`${API_BASE}/configurations/configurations/${projectId}`, {
    data: {
      type: 'ado',
      elitea_title: title,
      label: title,
      shared: false,
      // `AdoConfiguration`'s own two fields (`sdk_config_schemas.json`). The
      // organization_url is a compose-network URL on purpose: the azure-devops
      // client resolves EVERY service off it, so one host serves boards, wiki,
      // git, core and test plans at once.
      data: { organization_url: ADO_ORGANIZATION_URL, token: ADO_PAT },
    },
  });
  expect(
    credential.status(),
    `the ado credential must be creatable: ${(await credential.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
}

async function createToolkit(
  request: APIRequestContext,
  projectId: string,
  name: string,
  spec: { type: string; selectedTools: readonly string[]; settings?: Readonly<Record<string, unknown>> },
  credentialTitle: string,
): Promise<string> {
  const toolkit = await request.post(`${API_BASE}/elitea_core/tools/prompt_lib/${projectId}`, {
    data: {
      name,
      type: spec.type,
      settings: {
        // An OBJECT reference with `private: false`, for the reason
        // chat.qtest-toolkit.spec.ts records: `true` resolves the title in the
        // CALLER's personal project and answers `configuration_not_found` for
        // a row that plainly exists.
        ado_configuration: { elitea_title: credentialTitle, private: false },
        project: ADO_PROJECT,
        selected_tools: [...spec.selectedTools],
        ...spec.settings,
      },
    },
  });
  expect(
    toolkit.status(),
    `the ${spec.type} toolkit must be creatable: ${(await toolkit.text()).slice(0, 300)}`,
  ).toBe(201);
  const toolkitId = String(((await toolkit.json()) as { id?: unknown }).id ?? '');
  expect(toolkitId, 'the created toolkit must carry an id').not.toBe('');
  return toolkitId;
}

/**
 * An ADO toolkit pointed at the fake backend, attached to an agent pinned to
 * the deterministic model, with a conversation open in the browser.
 *
 * Built over the API rather than through the forms for the reason
 * `createMockToolAgent` gives: the subject here is what happens DURING a turn.
 * The attach is READ BACK, because the relation route has answered 200 while
 * writing nothing before (`chat.agent-tools.spec.ts`'s defect class 2).
 */
export async function createAdoAgent(page: Page, label: string, spec: AdoToolkitSpec): Promise<AdoAgent> {
  const suffix = `${String(Date.now() % 1_000_000)}${label}`;
  const credentialTitle = `${AUTOTEST_PREFIX}adocred-${suffix}`;
  const toolkitName = `${AUTOTEST_PREFIX}adotk-${suffix}`;
  const secondName = `${AUTOTEST_PREFIX}adotk2-${suffix}`;
  const agentName = `${AUTOTEST_PREFIX}adoagent-${suffix}`;

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona must work inside its own project').not.toBe('');
  const caller = await readCallerIdentity(page.request);

  await createCredential(page.request, projectId, credentialTitle);
  const toolkitId = await createToolkit(page.request, projectId, toolkitName, spec, credentialTitle);
  const secondId = spec.second
    ? await createToolkit(
        page.request,
        projectId,
        secondName,
        { type: spec.type, selectedTools: spec.second.selectedTools, settings: spec.second.settings },
        credentialTitle,
      )
    : '';

  const agent = await createAgentWithVersion(
    page.request,
    agentName,
    {
      instructions: 'You are an autotest agent. Call the tools you are asked to call.',
      model: { modelName: MOCK_MODEL },
    },
    projectId,
  );

  for (const id of [toolkitId, secondId].filter(Boolean)) {
    const attached = await page.request.patch(
      `${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${id}`,
      {
        data: {
          entity_version_id: Number(agent.versionId),
          entity_id: Number(agent.id),
          entity_type: 'agent',
          has_relation: true,
        },
      },
    );
    expect(
      attached.status(),
      `the toolkit must attach to the agent version: ${(await attached.text()).slice(0, 300)}`,
    ).toBeLessThan(300);
  }

  const storedAgent = await page.request.get(
    `${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${agent.id}`,
  );
  const tools =
    ((await storedAgent.json()) as { version_details?: { tools?: readonly { name?: string }[] } })
      .version_details?.tools ?? [];
  expect(
    tools.map((tool) => tool.name),
    'the agent version carries no reference to the toolkit — the attach was a no-op',
  ).toContain(toolkitName);

  const conversation = await page.request.post(
    `${API_BASE}/elitea_core/conversations/prompt_lib/${projectId}`,
    { data: { name: `${AUTOTEST_PREFIX}adoconv-${suffix}`, is_private: true } },
  );
  expect(
    conversation.status(),
    `the conversation must be created: ${(await conversation.text()).slice(0, 300)}`,
  ).toBe(201);
  const conversationId = String(((await conversation.json()) as { id?: unknown }).id ?? '');

  // BOTH participants: a conversation POSTed to the API carries neither, and
  // its first send 422s at admission.
  const participants = await page.request.post(
    `${API_BASE}/elitea_core/participants/prompt_lib/${projectId}/${conversationId}`,
    {
      data: [
        {
          entity_name: 'application',
          entity_meta: { id: agent.id, project_id: Number(projectId), name: agentName },
          entity_settings: { version_id: agent.versionId },
        },
        { entity_name: 'user', entity_meta: { id: Number(caller.id) } },
      ],
    },
  );
  expect(
    participants.status(),
    `the participants must be added: ${(await participants.text()).slice(0, 300)}`,
  ).toBe(200);

  await page.goto(`${BASE_URL}/app/chat/${conversationId}`);
  await expect(page.getByTestId('chat-message-input')).toBeEditable({ timeout: 45_000 });

  return {
    projectId,
    conversationId,
    toolkitId,
    dispose: async (): Promise<void> => {
      await page.request.delete(`${API_BASE}/elitea_core/application/prompt_lib/${projectId}/${agent.id}`);
      for (const id of [toolkitId, secondId].filter(Boolean)) {
        await page.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${id}`);
      }
    },
  };
}

/**
 * Send one scripted prompt and wait for THIS turn's answer to be stored.
 *
 * Not `expectStoredAssistantAnswer` on its own: that helper reads the NEWEST
 * assistant row, and several of these journeys take two or three turns in one
 * conversation — a poll issued moments after the click finds the PREVIOUS
 * turn's finished row and returns while the new turn is still running. So the
 * rows that exist beforehand are recorded and the poll demands a row that is
 * not one of them AND carries `is_error === false`, the presence test that
 * means the projection is terminal.
 *
 * The mock quotes the tool result VERBATIM on the resume
 * (`_tool_results_this_turn`, `deploy/mock-llm/server.py`), so the returned
 * text carries the comment bodies the toolkit produced — descriptions,
 * `[image unavailable: …]` notes and all. That is what lets these journeys
 * assert on the RESPONSE STRUCTURE without a trace-panel read.
 */
export async function runPrompt(page: Page, agent: AdoAgent, prompt: string): Promise<string> {
  const before = new Set(
    (await readStoredTranscript(page, agent.projectId, agent.conversationId))
      .filter((row) => row.role !== 'user')
      .map((row) => row.id),
  );
  const started = page.waitForResponse(
    (response) => START_RE.test(response.url()) && response.request().method() === 'POST',
    { timeout: 60_000 },
  );
  const input = page.getByTestId('chat-message-input');
  await expect(input).toBeEditable({ timeout: 30_000 });
  await input.fill(prompt);
  await expect(page.getByTestId('chat-send-button')).toBeEnabled({ timeout: 10_000 });
  await page.getByTestId('chat-send-button').click();
  const startResponse = await started;
  expect(
    startResponse.status(),
    `the ado turn was refused: ${(await startResponse.text()).slice(0, 300)}`,
  ).toBe(200);

  // Hand-rolled rather than `expect.poll`, for one reason: a REFUSED turn is
  // stored as an assistant row too, and a poll can only keep asking until its
  // timeout — so a turn that failed in the first second still cost four
  // minutes of wall time and reported "no answer" instead of the refusal. This
  // loop stops the moment the run is terminal, whichever way it went.
  const deadline = Date.now() + 240_000;
  for (;;) {
    const rows = await readStoredTranscript(page, agent.projectId, agent.conversationId);
    const fresh = rows.filter((row) => row.role !== 'user' && !before.has(row.id));
    const finished = fresh.filter((row) => row.metadata['is_error'] === false);
    if (finished.length > 0) return finished.map((row) => row.content).join('\n');
    const refused = fresh.find((row) => row.isError);
    if (refused) {
      throw new Error(
        `the ado turn was stored as an ERROR, not an answer: ${refused.content.slice(0, 500) || '<empty>'}`,
      );
    }
    if (Date.now() > deadline) {
      throw new Error('the ado turn produced no stored answer — the toolkit may not have materialized');
    }
    await page.waitForTimeout(1_000);
  }
}
