/**
 * A REAL `imagegen` TOOLKIT INVOCATION — #864.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * SKIPPED HERE ON PURPOSE — SEE `e2e/live/image.imagegen-toolkit.spec.ts`
 * ─────────────────────────────────────────────────────────────────────────────
 * This journey cannot pass against `deploy/mock-llm/server.py`, for a reason
 * that is architectural rather than a setup mistake this file could fix by
 * trying another credential shape. Two attempts already failed on PR #882:
 *
 *  - `vllm` credential (the original version of this file): bifrost's vLLM
 *    provider hand-refuses `ImageGeneration` outright — `ImageGeneration is
 *    not supported by the vLLM provider` (`core/providers/vllm/vllm.go`,
 *    `NewUnsupportedOperationError`).
 *  - `open_ai` with a custom `api_base` (CI run 34355873222): LOOKS right —
 *    bifrost's OpenAI provider implements `ImageGeneration` — but
 *    `services/elitea-llm-gateway/internal/account/credential_selector.go`'s
 *    `ProviderForCredential` silently REROUTES any `open_ai` credential whose
 *    `api_base` is not `api.openai.com` to the vLLM provider ("only that
 *    provider carries a per-key base URL"), landing on the exact same
 *    hand-refusal as above.
 *  - `azure_open_ai` (this file's own previous revision): Azure's provider
 *    DOES implement `ImageGeneration` and DOES accept a per-credential
 *    `api_base` (`AzureKeyConfig.Endpoint`, no origin check) — but
 *    `account.go`'s `GetConfigForProvider` grants
 *    `NetworkConfig.AllowPrivateNetwork` to `schemas.VLLM` and
 *    `schemas.Ollama` ONLY:
 *
 *        switch provider {
 *        case schemas.VLLM, schemas.Ollama:
 *            cfg.NetworkConfig.AllowPrivateNetwork = a.egress.allowsPrivateNetwork()
 *        }
 *
 *    "Cloud providers keep the guard: an api_base for openai/anthropic/etc.
 *    must never resolve to a private address" (that method's own comment).
 *    `llm-mock` is a Docker-internal, private-network host, so bifrost's
 *    SSRF-safe dialer refuses to connect — measured on CI run 34359971284 as
 *    a `502 Bad Gateway` on every retry (`platform-edge`'s access log: `POST
 *    /llm/v1/images/generations HTTP/1.1" 502 107`, ~183ms — real processing
 *    time, consistent with a blocked dial rather than a routing miss).
 *
 * Of this gateway's seven `supportedProviders`, only OpenAI and Azure
 * implement a real `ImageGeneration`, and only vLLM and Ollama can dial a
 * private host — a set with EMPTY intersection. There is no
 * `(credential type, api_base)` combination that reaches a real
 * `ImageGeneration` dispatch against any private-network stand-in, the mock
 * included. The toolkit itself is real (see below); only a real, PUBLIC
 * provider can prove the dispatch, which is what
 * `e2e/live/image.imagegen-toolkit.spec.ts` does, gated (like every other
 * `e2e/live` journey) on the environment rather than on a permanent skip.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS FILE STILL PROVES
 * ─────────────────────────────────────────────────────────────────────────────
 * The toolkit (`generate_image`/`edit_image`) has been served since the
 * catalogue entry landed, but a real dispatch on the Python worker answered
 * "cannot be built" until the admitted `elitea-sdk` pin carried the toolkit's
 * module (`services/elitea-worker-python/elitea-sdk.lock.json`). Authoring
 * the credential/model row, the toolkit, the agent, and attaching it through
 * the picker (steps 1–4 below) all run and assert for real — a `test.skip`
 * sits between that and step 5, so this file still catches a regression in
 * any of that CRUD/wiring; only the real-provider dispatch and its artifact
 * are out of reach here.
 *
 * `services/elitea-worker-python/tests/unit/test_imagegen_toolkit_dispatch.py`
 * already proves the dispatch at the worker level, against the real admitted
 * SDK, with the outbound HTTP calls stubbed.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THE TOOLKIT AND ITS MODEL ARE AUTHORED THROUGH THE API, NOT A FORM
 * ─────────────────────────────────────────────────────────────────────────────
 * `chat.toolkit.spec.ts` authors its `openapi` toolkit through the create
 * form because the FORM is what turns a pasted specification into
 * `settings.selected_tools` — that parsing step is exactly what used to break.
 * `imagegen` has no such step: its settings are three plain fields
 * (`image_generation_model`, `bucket`, `name_prefix`) with no parsing in
 * between, so an API-authored toolkit here carries the identical stored shape
 * a form submission would. What the form path is NOT a substitute for is the
 * project's `image_generation` MODEL — nothing seeds one by default (the
 * standard `seed-llm` chat model is a DIFFERENT section) — so this journey
 * creates its own credential and model row through the same product API
 * `deploy/scripts/seed-llm-api.py` uses for the chat model it does seed,
 * pointed at the SAME offline mock upstream (`vllm` credential type,
 * `http://llm-mock:8090`, per `deploy/scripts/standalone-stack.sh`'s default
 * `seed-llm` wiring), so the field this journey stores is one the project's
 * OWN model picker would also offer.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS IS PYTHON-WORKER ONLY
 * ─────────────────────────────────────────────────────────────────────────────
 * `imagegen` ships no Rust family (`elitea_sdk/tools/imagegen` has no
 * counterpart under `services/elitea-worker-rust`), the exact posture
 * `TestImageGenIsHandWrittenButStillWorkerGated`
 * (`services/elitea-main/internal/api/v2/toolkits/type_catalogue_test.go`)
 * pins: buildable on Python, hidden with a reason on Rust. A marker naming
 * `generate_image` on the Rust leg would script a call the runtime never lets
 * the model make, so this journey states the skip by name rather than
 * leaving it to fail unexplained on `chat-stream-rust`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THE ASSERTION READS
 * ─────────────────────────────────────────────────────────────────────────────
 * Same discipline as every other file in this directory: the mock quotes the
 * tool's result VERBATIM once the runtime resumes the turn with it
 * (`call_tool_resumed`, deploy/mock-llm/server.py), so the created artifact's
 * `filepath` — `/{bucket}/{name_prefix}generate-0.png` — reaching the STORED
 * reply is `ImageGenAPIWrapper._save_images`'s own return value, round-tripped
 * through a real HTTP call to the mock's images stub and a real artifact
 * write, not anything this file invented.
 */
import { expect, test } from '@playwright/test';
import type { APIRequestContext } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import {
  API_BASE,
  AUTOTEST_PREFIX,
  MOCK_CALL_TOOL_SENTINEL,
  attachToolkitThroughPicker,
  callToolWithArgumentsPrompt,
  createAgentThroughForm,
  expectStoredAssistantAnswer,
  fillComposer,
  readCallerPersonalProjectId,
} from '../fixtures/api';

/** `chat-stream-e2e.sh` sets this; the local default matches its own `STANDALONE_WORKER` default. */
const WORKER = process.env['E2E_WORKER'] ?? 'python';

/**
 * The offline-mock upstream `standalone-stack.sh`'s default `seed-llm` wiring
 * points the CHAT model at (`API_BASE="http://llm-mock:8090"`) — reused here
 * for the image-generation credential's `api_base`, for the CRUD/wiring
 * coverage this file still runs (steps 1–4; see the file header for why the
 * DISPATCH half is skipped regardless of which of the three credential
 * shapes below authors this row). `azure_open_ai` is kept as the type,
 * matching the header's account: it is the only one of the three where the
 * refusal is a network-level SSRF guard rather than an immediate "bad
 * credential" 4xx, so the row this step creates is the same shape a real
 * deployment's operator would author against a real Azure endpoint.
 */
const MOCK_UPSTREAM_BASE = 'http://llm-mock:8090';
const MOCK_CREDENTIAL_TYPE = 'azure_open_ai';

/** What `deploy/mock-llm/server.py`'s images stub advertises as a model id. */
const IMAGE_MODEL_NAME = process.env['MOCK_LLM_IMAGE_MODEL'] ?? 'E2E-MOCK-IMAGE-MODEL';

/** The chat model — same default `chat.toolkit.spec.ts` pins the turn to. */
const CHAT_MODEL_NAME = process.env['E2E_MOCK_MODEL'] ?? 'vllm/E2E-MOCK-MODEL';

interface ImageGenFixture {
  readonly credentialId: string;
  readonly modelConfigId: string;
  readonly toolkitId: string;
}

/**
 * Create the image-generation credential and model row this project needs,
 * through the product API — the same write route
 * `deploy/scripts/seed-llm-api.py` uses for the chat model, adapted to the
 * `image_generation` section (`internal/application/configurations/models.go`
 * `CurrentModelSectionImageGeneration`).
 */
async function seedImageGenerationModel(
  request: APIRequestContext,
  projectId: string,
  suffix: string,
): Promise<{ readonly credentialId: string; readonly modelConfigId: string }> {
  const credentialTitle = `${AUTOTEST_PREFIX}imagegen-cred-${suffix}`;
  const credential = await request.post(`${BASE_URL}/api/v2/configurations/configurations/${projectId}`, {
    data: {
      elitea_title: credentialTitle,
      label: credentialTitle,
      type: MOCK_CREDENTIAL_TYPE,
      section: 'ai_credentials',
      data: { api_key: 'not-needed-against-the-mock', api_base: MOCK_UPSTREAM_BASE },
      shared: false,
    },
  });
  expect(
    credential.status(),
    `the image-generation credential must be created: ${(await credential.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
  const credentialBody = (await credential.json()) as { id?: string | number; status_ok?: boolean };
  const credentialId = String(credentialBody.id ?? '');
  expect(credentialId, 'the credential must carry an id').not.toBe('');

  const modelRow = await request.post(`${BASE_URL}/api/v2/configurations/configurations/${projectId}`, {
    data: {
      elitea_title: IMAGE_MODEL_NAME,
      label: IMAGE_MODEL_NAME,
      type: 'image_generation_model',
      section: 'image_generation',
      data: { name: IMAGE_MODEL_NAME, ai_credentials: { elitea_title: credentialTitle, private: false } },
      shared: false,
    },
  });
  expect(
    modelRow.status(),
    `the image-generation model row must be created: ${(await modelRow.text()).slice(0, 300)}`,
  ).toBeLessThan(300);
  const modelBody = (await modelRow.json()) as { id?: string | number; status_ok?: boolean };
  expect(
    modelBody.status_ok,
    'the model row must be admitted (status_ok) — every reader of an image_generation row, ' +
      'gateway included, selects on that column',
  ).toBe(true);
  const modelConfigId = String(modelBody.id ?? '');
  expect(modelConfigId, 'the model row must carry an id').not.toBe('');

  return { credentialId, modelConfigId };
}

/** Create the `imagegen` toolkit through the API — see the header for why the form is not required here. */
async function createImageGenToolkit(
  request: APIRequestContext,
  projectId: string,
  toolkitName: string,
  bucket: string,
  namePrefix: string,
): Promise<string> {
  const created = await request.post(`${BASE_URL}/api/v2/elitea_core/tools/prompt_lib/${projectId}`, {
    data: {
      name: toolkitName,
      type: 'imagegen',
      settings: {
        image_generation_model: IMAGE_MODEL_NAME,
        bucket,
        name_prefix: namePrefix,
        selected_tools: ['generate_image', 'edit_image'],
      },
    },
  });
  expect(
    created.status(),
    `the imagegen toolkit must be created: ${(await created.text()).slice(0, 300)}`,
  ).toBe(201);
  const toolkitId = String(((await created.json()) as { id?: string | number }).id ?? '');
  expect(toolkitId, 'the created toolkit must carry an id').not.toBe('');
  return toolkitId;
}

/** Best-effort cleanup, in dependency order. Never throws — this is a teardown, not an assertion. */
async function cleanUp(
  request: APIRequestContext,
  projectId: string,
  fixture: (ImageGenFixture & { readonly agentId?: string }) | undefined,
): Promise<void> {
  if (fixture === undefined) return;
  if (fixture.agentId !== undefined) {
    await request.delete(`${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${fixture.agentId}`).catch(() => {});
  }
  await request.delete(`${BASE_URL}/api/v2/elitea_core/tool/prompt_lib/${projectId}/${fixture.toolkitId}`).catch(() => {});
  await request
    .delete(`${BASE_URL}/api/v2/configurations/configuration/${projectId}/${fixture.modelConfigId}`)
    .catch(() => {});
  await request
    .delete(`${BASE_URL}/api/v2/configurations/configuration/${projectId}/${fixture.credentialId}`)
    .catch(() => {});
}

test('a generate_image call is dispatched by the model and the artifact lands in the bucket', async ({
  page,
}) => {
  test.skip(
    WORKER !== 'python',
    'imagegen ships no Rust family (#864); the Rust worker hides the tile, so a marker naming ' +
      'generate_image would script a call the runtime never offers the model',
  );
  test.setTimeout(300_000);

  const suffix = String(Date.now() % 1_000_000);
  const toolkitName = `${AUTOTEST_PREFIX}imagegen-${suffix}`;
  const agentName = `${AUTOTEST_PREFIX}imagegenagent-${suffix}`;
  const bucket = `autotest-imagegen-${suffix}`;
  const namePrefix = 'e2e-proof-';

  const projectId = await readCallerPersonalProjectId(page.request);
  expect(projectId, 'this persona works in its own project').not.toBe('');

  let fixture: (ImageGenFixture & { agentId?: string }) | undefined;
  try {
    // ── 1. The image-generation model this project's toolkit will name ─────
    const { credentialId, modelConfigId } = await seedImageGenerationModel(page.request, projectId, suffix);

    // ── 2. The toolkit itself ───────────────────────────────────────────────
    const toolkitId = await createImageGenToolkit(page.request, projectId, toolkitName, bucket, namePrefix);
    fixture = { credentialId, modelConfigId, toolkitId };

    // ── 3. Author the agent, pin the deterministic chat model ──────────────
    const agent = await createAgentThroughForm(page, agentName);
    expect(
      agent.projectId,
      'the agent and the toolkit must land in the same project, or the attach addresses nothing',
    ).toBe(projectId);
    fixture = { ...fixture, agentId: agent.agentId };

    const storedAgent = await page.request.get(
      `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agent.agentId}`,
    );
    expect(storedAgent.ok(), 'the agent the form created must be readable').toBe(true);
    const storedMeta =
      ((await storedAgent.json()) as { version_details?: { meta?: Record<string, unknown> } }).version_details
        ?.meta ?? {};
    const pinned = await page.request.put(
      `${BASE_URL}/api/v2/elitea_core/version/prompt_lib/${projectId}/${agent.agentId}/${agent.versionId}`,
      { data: { meta: storedMeta, llm_settings: { model_name: CHAT_MODEL_NAME } } },
    );
    expect(
      pinned.status(),
      `the mock chat model must be pinnable: ${(await pinned.text()).slice(0, 300)}`,
    ).toBeLessThan(300);

    // ── 4. Attach the toolkit through the agent page's picker ──────────────
    await expect(
      page.getByTestId('agent-toolkits-section'),
      'the save must land on the agent edit page, where the Tools panel lives',
    ).toBeVisible({ timeout: 30_000 });
    await attachToolkitThroughPicker(page, toolkitName);

    const withTools = await page.request.get(
      `${BASE_URL}/api/v2/elitea_core/application/prompt_lib/${projectId}/${agent.agentId}`,
    );
    const frozenTools =
      ((await withTools.json()) as {
        version_details?: { tools?: readonly { tool_id?: number; type?: string }[] };
      }).version_details?.tools ?? [];
    const attached = frozenTools.find((tool) => String(tool.tool_id ?? '') === toolkitId);
    expect(
      attached,
      'the picker reported success but the version carries no mapping — the attach was a no-op',
    ).toBeDefined();
    expect(attached?.type, 'the mapped toolkit must be the imagegen one').toBe('imagegen');

    // Everything ABOVE this line is real product coverage: the credential
    // and image_generation_model row, the imagegen toolkit itself, the agent,
    // and attaching the toolkit through the picker all go through the same
    // product routes and UI a user would, and would fail here if any of them
    // regressed. Everything BELOW it is what this file's header explains
    // cannot pass against `deploy/mock-llm/server.py` — no credential/api_base
    // combination this gateway serves reaches a real `ImageGeneration` call
    // against a private host. See `e2e/live/image.imagegen-toolkit.spec.ts`
    // for the same dispatch against a real, public provider.
    test.skip(
      true,
      'no (credential type, api_base) this gateway serves reaches a real ImageGeneration call ' +
        'against a private host (see this file header) — dispatch is proven on ' +
        'e2e/live/image.imagegen-toolkit.spec.ts instead',
    );

    // ── 5. Chat, and script a real generate_image call ─────────────────────
    const conversationCreated = page.waitForResponse(
      (r) =>
        /\/elitea_core\/conversations\/prompt_lib\/\d+$/.test(new URL(r.url()).pathname) &&
        r.request().method() === 'POST',
      { timeout: 45_000 },
    );
    await page.getByTestId('chat-with-agent-button').click();
    const conversation = (await (await conversationCreated).json()) as { id?: string | number };
    const conversationId = String(conversation.id ?? '');
    expect(conversationId, 'the Chat button must create a conversation').not.toBe('');
    await page.waitForURL(new RegExp(`/app/chat/${conversationId}(?:[/?#]|$)`), { timeout: 45_000 });

    // The expected filepath is DERIVED from the toolkit's own configured
    // bucket/prefix — `ImageGenAPIWrapper._save_images` names the file
    // `{name_prefix}{generate|edit}-{index}.png`, so this string can only
    // reach the stored reply if the SAME configuration this journey wrote
    // made it all the way through toolkit instantiation and the real save.
    const expectedFilepath = `/${bucket}/${namePrefix}generate-0.png`;

    const prompt = callToolWithArgumentsPrompt(
      'generate_image',
      { prompt: `a red circle on a white background ${suffix}` },
      `create this image ${suffix}`,
    );
    const sendButton = await fillComposer(page, prompt);
    await sendButton.click();

    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 120_000,
      contains: expectedFilepath,
      message:
        `the stored reply never quoted ${expectedFilepath} back — either generate_image was not ` +
        'offered to the model, the call was never dispatched, the gateway/mock images hop failed, ' +
        'or the result never made it into the artifact bucket',
    });
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 5_000,
      contains: MOCK_CALL_TOOL_SENTINEL,
      message: 'the reply quoted the artifact path but never reached the resumed-call sentinel',
    });

    await page.request.delete(`${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`);
  } finally {
    await cleanUp(page.request, projectId, fixture);
  }
});
