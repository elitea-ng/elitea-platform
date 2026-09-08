/**
 * Per-conversation entity settings: overriding an agent's model inside ONE
 * conversation, switching the agent to another of its versions mid-chat, and
 * the rule that decides which of the two a write is allowed to be.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS PORTS
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy API suite's LLM-settings-override and version-change cases. Two
 * clusters that share one endpoint,
 * `PUT /elitea_core/entity_settings/prompt_lib/{p}/{conversationId}/{participantId}`:
 * an override is accepted for an agent published in the platform's public
 * project and refused for one that is not; the write is a whole-object REPLACE
 * rather than a merge, so omitting a key removes it; and a version switch is
 * the same write with a different intent, which is why a UI that resends the
 * new version's own settings must not be read as attempting an override.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY TWO PROJECTS, AND WHICH IS WHICH
 * ─────────────────────────────────────────────────────────────────────────────
 * The rule discriminates on ONE thing: whether the participant's
 * `entity_meta.project_id` is the platform's public project. That project is
 * resolved from `ELITEA_AI_PROJECT_ID` and defaults to 1
 * (`internal/publicproject`), and 1 is also the project every seeded persona
 * works in — so an agent created the ordinary way is on the ACCEPTING side of
 * the rule and the refusing side is unreachable from there. Both sides are
 * exercised here by putting each fixture in the project the rule needs:
 *
 *   - the accepted cases build their agent in the seeded project, which IS the
 *     public project this deployment resolves;
 *   - the refused cases build theirs in the persona's OWN project, a second
 *     real project the same persona owns, which is what "not published" means
 *     to this endpoint.
 *
 * Nothing is spoofed: each participant names the project its agent genuinely
 * lives in, which is what the product's own attach path sends.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT IS NOT ASSERTED HERE, AND WHY
 * ─────────────────────────────────────────────────────────────────────────────
 * The legacy suite also proves that an applied override CHANGES WHICH MODEL
 * ANSWERS, by running two turns and reading the model name out of the execution
 * trace. That needs a runtime plane, two genuinely distinct configured models,
 * and a trace that reports per-turn model identity — none of which this stack
 * has (it has no worker at all, so no turn ever runs). The durable half of the
 * same case is asserted below instead: an override applied once is still there
 * for every later reader of the conversation, and a write to a SECOND
 * participant does not disturb it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS RUN LEAVES BEHIND
 * ─────────────────────────────────────────────────────────────────────────────
 * Nothing. Every case creates its own `autotest_*` agent and conversation and
 * deletes both in a `finally`, in whichever project it created them in.
 */
import { test, expect } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  agentVersionBody,
  createAgentWithVersion,
  createConversation,
  deleteAgent,
  deleteConversation,
  readCallerPersonalProjectId,
} from '../../fixtures/api';
import { STORAGE_STATE } from '../../../playwright.config';

test.use({ storageState: STORAGE_STATE.admin });

/** A name no other worker, engine or run can collide with. */
function autotestName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}_${Math.random().toString(36).slice(2, 8)}`;
}

const conversationURL = (projectId: string, conversationId: string): string =>
  `${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`;

const participantsURL = (projectId: string, conversationId: string): string =>
  `${API_BASE}/elitea_core/participants/prompt_lib/${projectId}/${conversationId}`;

const entitySettingsURL = (
  projectId: string,
  conversationId: string,
  participantId: string,
): string =>
  `${API_BASE}/elitea_core/entity_settings/prompt_lib/${projectId}/${conversationId}/${participantId}`;

const versionsURL = (projectId: string, applicationId: string): string =>
  `${API_BASE}/elitea_core/versions/prompt_lib/${projectId}/${applicationId}`;

/**
 * The model reference every fixture version below carries.
 *
 * A settings write is compared FIELD BY FIELD against the version's own
 * `llm_settings` (`temperature`, `max_tokens`, `top_p`, `model_name`), so a
 * version with no model at all would make "the same settings" and "no settings"
 * the same request and half of these cases would prove nothing. The name is not
 * required to resolve to a served model: no turn runs here.
 */
const BASELINE_MODEL = { modelName: 'autotest-model-a', temperature: 0.3, maxTokens: 512 } as const;

/** One conversation with one agent participant, in the project the case needs. */
interface AgentParticipantFixture {
  readonly projectId: string;
  readonly conversationId: string;
  readonly agentId: string;
  readonly versionId: string;
  readonly participantId: string;
}

/**
 * Build the fixture every case starts from: an agent, a conversation, and the
 * agent attached to it as the participant whose settings are then written.
 *
 * The attach names the agent's own project in `entity_meta.project_id`, which
 * is what the product sends and what the accept/refuse rule reads.
 */
async function seedAgentParticipant(
  request: APIRequestContext,
  tag: string,
  projectId: string,
): Promise<AgentParticipantFixture> {
  const agentName = autotestName(tag);
  const agent = await createAgentWithVersion(
    request,
    agentName,
    { model: BASELINE_MODEL },
    projectId,
  );
  const conversationId = await createConversation(request, autotestName(`${tag}conv`), projectId);

  const attached = await request.post(participantsURL(projectId, conversationId), {
    data: [
      {
        entity_name: 'application',
        entity_meta: { id: agent.id, project_id: projectId, name: agentName },
        entity_settings: { version_id: agent.versionId },
      },
    ],
  });
  expect(
    attached.status(),
    `the agent could not be attached in project ${projectId}: ${(await attached.text()).slice(0, 200)}`,
  ).toBe(200);
  const participants = (await attached.json()) as readonly { id?: unknown }[];
  const participantId = String(participants[0]?.id ?? '');
  expect(participantId, 'the attach answered no participant id').not.toBe('');

  return {
    projectId,
    conversationId,
    agentId: agent.id,
    versionId: agent.versionId,
    participantId,
  };
}

async function dropFixture(
  request: APIRequestContext,
  fixture: AgentParticipantFixture,
): Promise<void> {
  await deleteConversation(request, fixture.conversationId, fixture.projectId);
  await request.delete(
    `${API_BASE}/elitea_core/application/prompt_lib/${fixture.projectId}/${fixture.agentId}`,
  );
}

/** The participant's settings AS STORED, read back through the conversation. */
async function storedEntitySettings(
  request: APIRequestContext,
  fixture: AgentParticipantFixture,
): Promise<Record<string, unknown>> {
  const response = await request.get(conversationURL(fixture.projectId, fixture.conversationId));
  expect(
    response.status(),
    `the conversation read failed: ${(await response.text()).slice(0, 200)}`,
  ).toBe(200);
  const body = (await response.json()) as {
    participants?: readonly { id?: unknown; entity_settings?: unknown }[];
  };
  const row = (body.participants ?? []).find(
    (participant) => String(participant.id ?? '') === fixture.participantId,
  );
  expect(row, 'the participant this case wrote settings for is not in the conversation').toBeDefined();
  const settings = row?.entity_settings;
  return typeof settings === 'object' && settings !== null
    ? (settings as Record<string, unknown>)
    : {};
}

/**
 * The persona's own project, or a skip-free failure naming why there is none.
 *
 * `auth.setup.ts` waits for it before any journey runs, so an absence here is
 * a broken setup rather than an unconfigured environment — and stating that as
 * a failure is deliberate: a case that quietly stopped when the second project
 * was missing would report the refusing half of this rule as covered while
 * never having reached it.
 */
async function ownProjectId(request: APIRequestContext): Promise<string> {
  const id = await readCallerPersonalProjectId(request);
  expect(
    id,
    'the persona owns no project of its own, so nothing here can sit OUTSIDE the public project ' +
      '— see e2e/auth.setup.ts, which waits for one before the suite starts',
  ).not.toBe('');
  return id;
}

/* ── the accepting side: an agent in the public project ───────────────────── */

test('a model override is accepted for an agent in the public project and echoed back', async ({
  request,
}) => {
  const fixture = await seedAgentParticipant(request, 'ovaccept', DEFAULT_PROJECT_ID);
  try {
    const override = {
      version_id: fixture.versionId,
      llm_settings: {
        model_name: 'autotest-model-b',
        model_project_id: DEFAULT_PROJECT_ID,
        reasoning_effort: 'high',
        max_tokens: 1024,
      },
    };
    const response = await request.put(
      entitySettingsURL(fixture.projectId, fixture.conversationId, fixture.participantId),
      { data: override },
    );
    expect(
      response.status(),
      `the override was refused for an agent that IS in the public project: ${(await response.text()).slice(0, 300)}`,
    ).toBe(200);

    const echoed = ((await response.json()) as { entity_settings?: Record<string, unknown> })
      .entity_settings;
    const echoedLLM = (echoed?.['llm_settings'] ?? {}) as Record<string, unknown>;
    expect(echoedLLM['model_name']).toBe('autotest-model-b');
    expect(echoedLLM['reasoning_effort']).toBe('high');
    expect(echoedLLM['max_tokens']).toBe(1024);

    // The STORE, not the echo: the echo is the request body the handler decided
    // to keep, so a write that never reached the column answers correctly.
    const stored = await storedEntitySettings(request, fixture);
    const storedLLM = (stored['llm_settings'] ?? {}) as Record<string, unknown>;
    expect(
      storedLLM['model_name'],
      'the override was echoed and then not stored — the conversation would answer with the version default',
    ).toBe('autotest-model-b');
    expect(String(stored['version_id'] ?? '')).toBe(fixture.versionId);
  } finally {
    await dropFixture(request, fixture);
  }
});

/**
 * The write REPLACES the stored document; it does not merge into it.
 *
 * This is the property the whole cluster turns on. A merge would make the
 * "clear the override" case impossible to express — there is no way to send
 * "remove this key" — and it would leave a field from a previous override
 * steering every later turn after the user had chosen different settings.
 */
test('a second override fully replaces the first, and omitting the key clears it', async ({
  request,
}) => {
  const fixture = await seedAgentParticipant(request, 'ovreplace', DEFAULT_PROJECT_ID);
  const url = entitySettingsURL(fixture.projectId, fixture.conversationId, fixture.participantId);
  try {
    const first = await request.put(url, {
      data: {
        version_id: fixture.versionId,
        llm_settings: {
          model_name: 'autotest-model-b',
          model_project_id: DEFAULT_PROJECT_ID,
          reasoning_effort: 'high',
          max_tokens: 2048,
        },
      },
    });
    expect(first.status()).toBe(200);
    expect(
      ((await storedEntitySettings(request, fixture))['llm_settings'] as Record<string, unknown>)?.[
        'max_tokens'
      ],
    ).toBe(2048);

    // #2 names no max_tokens. A merge would keep 2048.
    const second = await request.put(url, {
      data: {
        version_id: fixture.versionId,
        llm_settings: {
          model_name: 'autotest-model-b',
          model_project_id: DEFAULT_PROJECT_ID,
          reasoning_effort: 'low',
        },
      },
    });
    expect(second.status()).toBe(200);
    const replaced = (await storedEntitySettings(request, fixture))['llm_settings'] as Record<
      string,
      unknown
    >;
    expect(replaced['reasoning_effort']).toBe('low');
    expect(
      replaced['max_tokens'],
      'a field from the PREVIOUS override survived the one that replaced it',
    ).toBeUndefined();

    // #3 names no llm_settings at all: the agent goes back to its version's
    // own settings, which is what the "reset" control does.
    const cleared = await request.put(url, { data: { version_id: fixture.versionId } });
    expect(cleared.status()).toBe(200);
    const afterClear = await storedEntitySettings(request, fixture);
    const clearedLLM = afterClear['llm_settings'];
    expect(
      clearedLLM === undefined || clearedLLM === null || Object.keys(clearedLLM as object).length === 0,
      `clearing left an override behind: ${JSON.stringify(clearedLLM)}`,
    ).toBe(true);
    // …and the version pin is NOT collateral damage: the participant still
    // names the version it is chatting with.
    expect(String(afterClear['version_id'] ?? '')).toBe(fixture.versionId);
  } finally {
    await dropFixture(request, fixture);
  }
});

/**
 * An override outlives the write that made it, and is not disturbed by a write
 * to another participant.
 *
 * The legacy case proves this by running two turns and reading the model name
 * out of the trace of each. This stack runs no turns (module header), so what
 * is asserted is the state every later turn WOULD read: the stored document,
 * after an unrelated settings write in the same conversation.
 */
test('an override stays applied through later writes to another participant', async ({
  request,
}) => {
  const fixture = await seedAgentParticipant(request, 'ovpersist', DEFAULT_PROJECT_ID);
  const secondName = autotestName('ovpersist2');
  const second = await createAgentWithVersion(
    request,
    secondName,
    { model: BASELINE_MODEL },
    DEFAULT_PROJECT_ID,
  );
  try {
    const applied = await request.put(
      entitySettingsURL(fixture.projectId, fixture.conversationId, fixture.participantId),
      {
        data: {
          version_id: fixture.versionId,
          llm_settings: {
            model_name: 'autotest-model-b',
            model_project_id: DEFAULT_PROJECT_ID,
            reasoning_effort: 'medium',
          },
        },
      },
    );
    expect(applied.status()).toBe(200);

    // A SECOND agent joins the conversation and gets settings of its own.
    const attached = await request.post(
      participantsURL(fixture.projectId, fixture.conversationId),
      {
        data: [
          {
            entity_name: 'application',
            entity_meta: { id: second.id, project_id: DEFAULT_PROJECT_ID, name: secondName },
            entity_settings: { version_id: second.versionId },
          },
        ],
      },
    );
    expect(attached.status()).toBe(200);
    const rows = (await attached.json()) as readonly { id?: unknown }[];
    const otherParticipantId = rows
      .map((row) => String(row.id ?? ''))
      .find((id) => id !== fixture.participantId);
    expect(otherParticipantId, 'the second agent did not become its own participant').toBeDefined();

    const otherWrite = await request.put(
      entitySettingsURL(
        fixture.projectId,
        fixture.conversationId,
        String(otherParticipantId),
      ),
      { data: { version_id: second.versionId } },
    );
    expect(otherWrite.status()).toBe(200);

    const stillThere = (await storedEntitySettings(request, fixture))['llm_settings'] as Record<
      string,
      unknown
    >;
    expect(
      stillThere?.['model_name'],
      'the first participant lost its override when the second one was written',
    ).toBe('autotest-model-b');
    expect(stillThere?.['reasoning_effort']).toBe('medium');
  } finally {
    await dropFixture(request, fixture);
    await deleteAgent(request, second.id);
  }
});

/* ── the refusing side: an agent outside the public project ───────────────── */

/**
 * A genuine override on an agent that is not published is refused, and the
 * refusal says why in words a user can act on.
 *
 * "Genuine" is the whole distinction, and the next case is its other half: the
 * server compares the settings it was sent against the version's own, so only a
 * request that would really CHANGE the model is refused. The message is matched
 * on its two acting words rather than on its exact sentence — it is copy, and
 * copy is allowed to be rewritten — but a refusal that named neither the
 * publish state nor where to publish would leave the user with nothing to do.
 */
test('a real override is refused for an agent outside the public project', async ({ request }) => {
  const projectId = await ownProjectId(request);
  const fixture = await seedAgentParticipant(request, 'ovrefuse', projectId);
  try {
    const response = await request.put(
      entitySettingsURL(fixture.projectId, fixture.conversationId, fixture.participantId),
      {
        data: {
          version_id: fixture.versionId,
          llm_settings: {
            model_name: 'autotest-model-b',
            model_project_id: projectId,
            temperature: 0.9,
            max_tokens: 4096,
          },
        },
      },
    );
    expect(
      response.status(),
      `an override on a non-published agent was accepted: ${(await response.text()).slice(0, 300)}`,
    ).toBe(400);
    const message = String(((await response.json()) as { error?: unknown }).error ?? '').toLowerCase();
    expect(
      message.includes('published') || message.includes('agent studio'),
      `the refusal explains nothing a user can act on: "${message}"`,
    ).toBe(true);

    // Refused means UNWRITTEN. A guard that answered 400 after the update
    // would leave the override steering the conversation anyway.
    const stored = await storedEntitySettings(request, fixture);
    expect(stored['llm_settings'], 'a refused override reached the column').toBeUndefined();
  } finally {
    await dropFixture(request, fixture);
  }
});

/**
 * Switching version resends that version's OWN settings, and must not be read
 * as an override attempt.
 *
 * This is what the editor does when a user picks another version from the
 * conversation's version menu: it sends the new `version_id` together with the
 * settings that version already carries. Those settings match the stored ones,
 * so the write is a switch and not an override — the server keeps the switch
 * and drops the settings rather than refusing the request, which is why the
 * previous case has to send settings that genuinely differ to reach the 400.
 */
test('switching version resends the version’s own settings and is not refused', async ({
  request,
}) => {
  const projectId = await ownProjectId(request);
  const fixture = await seedAgentParticipant(request, 'vswitch', projectId);
  try {
    // A second version, with a model reference of its own.
    const created = await request.post(versionsURL(projectId, fixture.agentId), {
      data: agentVersionBody({
        name: autotestName('v2'),
        agentType: 'openai',
        instructions: 'The second version follows a different brief.',
        model: { modelName: 'autotest-model-c', temperature: 0.7, maxTokens: 256 },
      }),
    });
    expect(created.status(), `the second version was refused: ${(await created.text()).slice(0, 300)}`).toBe(201);
    const secondVersionId = String(((await created.json()) as { id?: unknown }).id ?? '');
    expect(secondVersionId, 'the version create answered no id').not.toBe('');

    const url = entitySettingsURL(fixture.projectId, fixture.conversationId, fixture.participantId);

    // 1. The switch, carrying the new version's own settings verbatim.
    const switched = await request.put(url, {
      data: {
        version_id: secondVersionId,
        llm_settings: {
          model_name: 'autotest-model-c',
          model_project_id: projectId,
          temperature: 0.7,
          max_tokens: 256,
        },
      },
    });
    expect(
      switched.status(),
      `a version switch that resent the version's own settings was refused: ${(await switched.text()).slice(0, 300)}`,
    ).toBe(200);
    let stored = await storedEntitySettings(request, fixture);
    expect(String(stored['version_id'] ?? ''), 'the switch did not reach the store').toBe(secondVersionId);
    expect(
      stored['llm_settings'],
      'a settings document was stored for an agent that may not carry one; a later turn would read an override nobody chose',
    ).toBeUndefined();

    // 2. The minimal payload — no settings key at all — must always work.
    const minimal = await request.put(url, { data: { version_id: fixture.versionId } });
    expect(minimal.status(), `a bare version switch was refused: ${(await minimal.text()).slice(0, 300)}`).toBe(200);
    stored = await storedEntitySettings(request, fixture);
    expect(String(stored['version_id'] ?? '')).toBe(fixture.versionId);

    // 3. …and switching BACK, resending the base version's own settings, is
    //    accepted too. The direction is not what the rule looks at.
    const back = await request.put(url, {
      data: {
        version_id: fixture.versionId,
        llm_settings: {
          model_name: BASELINE_MODEL.modelName,
          model_project_id: projectId,
          temperature: BASELINE_MODEL.temperature,
          max_tokens: BASELINE_MODEL.maxTokens,
        },
      },
    });
    expect(back.status(), `switching back was refused: ${(await back.text()).slice(0, 300)}`).toBe(200);
    stored = await storedEntitySettings(request, fixture);
    expect(String(stored['version_id'] ?? '')).toBe(fixture.versionId);
  } finally {
    await dropFixture(request, fixture);
  }
});

/* ── a conversation that holds an agent from ANOTHER project ──────────────── */

/**
 * A conversation outside the public project may hold a participant that names
 * the public (catalogue) project, and the server tells the two apart.
 *
 * ── The use case ───────────────────────────────────────────────────────────
 *
 * The legacy product let a conversation in project A chat with an agent
 * PUBLISHED into the catalogue: the published version answers, while the turn
 * is billed and authorised under the caller's own project. An agent that is
 * merely private to some other project was never reachable that way — that is
 * the tenancy boundary, not an omission.
 *
 * ── What makes this a server rule and not a client one ─────────────────────
 *
 * The attach accepts BOTH participants. Nothing on
 * `POST /elitea_core/participants/...` checks the project an `entity_meta`
 * names, so a conversation can end up holding an agent it must never be
 * allowed to use, and every guard therefore has to live on the surfaces that
 * act on a participant. This case attaches both and then exercises one of
 * those surfaces.
 *
 * ── Which surface, and why this one ────────────────────────────────────────
 *
 * The turn itself is the other surface, and the one the rule was written for:
 * `ResolveCurrentApplicationTurn` admits a participant whose project is the
 * catalogue AND whose version is `published`, reading that version out of the
 * catalogue project's own schema, and refuses every other foreign project.
 * This stack composes no runtime plane, so it mounts no message POST at all
 * (see api.chat-contract.spec.ts, which says so at length) and the same 405
 * would come back for an admitted turn and a refused one — three inputs that
 * cannot produce three answers cannot be told apart. The turn half is asserted
 * where it can be: `e2e/streaming/chat.delegation.spec.ts` runs it against the
 * full stack, and the Postgres integration cases in
 * `services/elitea-main/internal/infra/db/repos` pin the admission rule
 * directly, published against draft against stranger.
 *
 * What IS assertable here is the same discriminator on the settings write —
 * the participant's `entity_meta.project_id` against the platform's public
 * project — with the conversation standing somewhere else entirely. Every
 * other case in this file has the conversation and the agent in ONE project,
 * so none of them can say what happens when they differ, which is the whole
 * shape of chatting with a catalogue agent.
 */
test('a conversation elsewhere may write settings for a catalogue participant, not a stranger’s', async ({
  request,
}) => {
  const ownProject = await ownProjectId(request);
  expect(
    ownProject,
    'the persona’s own project must not BE the public project, or the two halves of this rule are the same project',
  ).not.toBe(DEFAULT_PROJECT_ID);

  const catalogueName = autotestName('xpubagent');
  const catalogueAgent = await createAgentWithVersion(
    request,
    catalogueName,
    { model: BASELINE_MODEL },
    DEFAULT_PROJECT_ID,
  );
  const strangerName = autotestName('xprivagent');
  const strangerAgent = await createAgentWithVersion(
    request,
    strangerName,
    { model: BASELINE_MODEL },
    ownProject,
  );
  // The conversation stands in the persona's OWN project — not the catalogue,
  // and not a third place. That is the caller's project in the use case.
  const conversationId = await createConversation(request, autotestName('xconv'), ownProject);

  try {
    const attached = await request.post(participantsURL(ownProject, conversationId), {
      data: [
        {
          entity_name: 'application',
          entity_meta: {
            id: catalogueAgent.id,
            project_id: DEFAULT_PROJECT_ID,
            name: catalogueName,
          },
          entity_settings: { version_id: catalogueAgent.versionId },
        },
        {
          entity_name: 'application',
          entity_meta: { id: strangerAgent.id, project_id: ownProject, name: strangerName },
          entity_settings: { version_id: strangerAgent.versionId },
        },
      ],
    });
    expect(
      attached.status(),
      `a cross-project participant could not be attached, which is where this rule starts: ${(
        await attached.text()
      ).slice(0, 300)}`,
    ).toBe(200);

    const rows = (await attached.json()) as readonly {
      id?: unknown;
      entity_meta?: Record<string, unknown>;
    }[];
    const idFor = (agentId: string): string =>
      String(
        rows.find((row) => String(row.entity_meta?.['id'] ?? '') === agentId)?.id ?? '',
      );
    const catalogueParticipantId = idFor(catalogueAgent.id);
    const strangerParticipantId = idFor(strangerAgent.id);
    expect(
      catalogueParticipantId,
      'the catalogue agent did not become a participant of a conversation in another project',
    ).not.toBe('');
    expect(strangerParticipantId, 'the second agent did not become a participant').not.toBe('');

    // The CATALOGUE participant: accepted, although its project is not the
    // conversation's. A rule that read the conversation's project instead of
    // the participant's would refuse this.
    const catalogueWrite = await request.put(
      entitySettingsURL(ownProject, conversationId, catalogueParticipantId),
      {
        data: {
          version_id: catalogueAgent.versionId,
          llm_settings: {
            model_name: 'autotest-model-b',
            model_project_id: DEFAULT_PROJECT_ID,
            reasoning_effort: 'high',
          },
        },
      },
    );
    expect(
      catalogueWrite.status(),
      `a participant from the public project was refused inside a conversation elsewhere: ${(
        await catalogueWrite.text()
      ).slice(0, 300)}`,
    ).toBe(200);

    // The STRANGER: refused, in the very same conversation, through the very
    // same endpoint, with the only difference being the project its
    // `entity_meta` names.
    const strangerWrite = await request.put(
      entitySettingsURL(ownProject, conversationId, strangerParticipantId),
      {
        data: {
          version_id: strangerAgent.versionId,
          llm_settings: {
            model_name: 'autotest-model-b',
            model_project_id: ownProject,
            temperature: 0.9,
            max_tokens: 4096,
          },
        },
      },
    );
    expect(
      strangerWrite.status(),
      `an agent that is not in the public project was writable from another project’s conversation: ${(
        await strangerWrite.text()
      ).slice(0, 300)}`,
    ).toBe(400);

    // …and the accepted one really is stored, so the pair above is a rule and
    // not two different failures.
    const conversation = await request.get(conversationURL(ownProject, conversationId));
    expect(conversation.status()).toBe(200);
    const participants =
      ((await conversation.json()) as {
        participants?: readonly { id?: unknown; entity_settings?: Record<string, unknown> }[];
      }).participants ?? [];
    const stored = participants.find(
      (row) => String(row.id ?? '') === catalogueParticipantId,
    )?.entity_settings;
    expect(
      ((stored?.['llm_settings'] ?? {}) as Record<string, unknown>)['model_name'],
      'the catalogue participant’s override was echoed and not stored',
    ).toBe('autotest-model-b');
  } finally {
    await deleteConversation(request, conversationId, ownProject);
    await request.delete(
      `${API_BASE}/elitea_core/application/prompt_lib/${ownProject}/${strangerAgent.id}`,
    );
    await deleteAgent(request, catalogueAgent.id);
  }
});
