/**
 * The `imagegen` TOOLKIT (#864) against a real image-capable provider — the
 * live counterpart of `streaming/chat.imagegen-toolkit.spec.ts`, which that
 * file's own header now explains cannot pass on `chat-stream`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHY THIS CANNOT RUN AGAINST THE MOCK, AND WHY NEITHER `vllm` NOR `open_ai`
 * NOR `azure_open_ai` FIXES THAT
 * ─────────────────────────────────────────────────────────────────────────────
 * `streaming/chat.imagegen-toolkit.spec.ts`'s header carries the full account
 * of the credential types this platform's gateway can serve. The short
 * version: of the seven `account.go` `supportedProviders`, only two
 * providers implement a real `ImageGeneration` in bifrost — OpenAI (hard-gated
 * to `api.openai.com`, `buildKey`'s `isOpenAIOrigin` check, issue #452) and
 * Azure (`AzureKeyConfig.Endpoint` takes any `api_base`, no origin check) —
 * and Azure's own `GetConfigForProvider` in
 * `services/elitea-llm-gateway/internal/account/account.go` grants
 * `NetworkConfig.AllowPrivateNetwork` to `schemas.VLLM` and `schemas.Ollama`
 * ONLY:
 *
 *   switch provider {
 *   case schemas.VLLM, schemas.Ollama:
 *       cfg.NetworkConfig.AllowPrivateNetwork = a.egress.allowsPrivateNetwork()
 *   }
 *
 * — "Cloud providers keep the guard: an api_base for openai/anthropic/etc.
 * must never resolve to a private address" (that method's own comment).
 * `deploy/mock-llm/server.py` runs at a Docker-internal, private-network
 * address (`llm-mock:8090`), so bifrost's SSRF-safe dialer refuses to connect
 * an Azure-typed request to it — measured on PR #882 CI run 34359971284 as a
 * `502 Bad Gateway` on every retry (`platform-edge`'s own access log: `POST
 * /llm/v1/images/generations HTTP/1.1" 502 107`, 183–190ms — real processing
 * time, not an instant refusal, consistent with a blocked dial rather than a
 * routing miss). And `open_ai` with a non-OpenAI `api_base` is silently
 * REROUTED to the vLLM provider by `credential_selector.go`'s
 * `ProviderForCredential` — the exact provider that hand-refuses
 * `ImageGeneration` outright, same as a literal `vllm`-typed credential.
 *
 * The only provider that both implements a real image call AND can reach a
 * PRIVATE host is vLLM — and vLLM refuses `ImageGeneration` unconditionally
 * (`core/providers/vllm/vllm.go`, `NewUnsupportedOperationError`). There is
 * therefore no `(credential type, api_base)` combination that reaches a real
 * `ImageGeneration` dispatch against `deploy/mock-llm/server.py`, or against
 * any other private-network stand-in. A real, PUBLIC provider is the only way
 * out — which is exactly what `image-live` exists for.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * THE PREREQUISITE THIS FILE ADDS TO `image-live`'s SINGLE VARIABLE
 * ─────────────────────────────────────────────────────────────────────────────
 * `e2e/live/README.md` documents `image-live`'s one prerequisite as
 * `E2E_LIVE_IMAGE_MODEL` — "the model name as the picker spells it" — which is
 * what `image.creation.spec.ts` selects through the ordinary CHAT model
 * picker. The `imagegen` toolkit's `image_generation_model` setting is a
 * DIFFERENT lookup: the gateway resolves it against the project's
 * `image_generation` CONFIGURATION SECTION specifically
 * (`internal/application/configurations/models.go`'s
 * `CurrentModelSectionImageGeneration`), not the `llm` section the chat picker
 * reads. This journey therefore assumes the SAME name `E2E_LIVE_IMAGE_MODEL`
 * carries is ALSO registered as an `image_generation`-section row on the
 * deployment `image-live` runs against — the real credential
 * `image.creation.spec.ts` selects, filed a second time under the section this
 * toolkit needs. No credential is created here: there is no
 * `E2E_LIVE_IMAGE_TOOLKIT_*` secret to create one from, on purpose — see
 * `e2e/live/README.md`'s "one prerequisite" table, which a second, private
 * variable would fragment. A deployment that has not done the second filing
 * fails this journey on the gateway's own `mapModel` "model is not configured
 * for this project" — an honest, informative failure that names exactly what
 * is missing, not a false pass.
 *
 * There is no `test.skip` in this file, and there must never be one — see
 * this directory's own README and `scripts/e2e-journey-shape.test.mjs`'s
 * `liveSkips` check.
 */
import { expect, test, type APIRequestContext } from '@playwright/test';

import { BASE_URL } from '../../playwright.config';
import { API_BASE, AUTOTEST_PREFIX, expectStoredAssistantAnswer } from '../fixtures/api';

import { liveImageModel } from './liveEnv';
import { runTag, selectedProjectId, sendLiveTurn } from './liveToolkits';

/** Create the `imagegen` toolkit through the product API — see the header. */
async function createLiveImageGenToolkit(
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
        image_generation_model: liveImageModel(),
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

test('LIVE-IMG-TK-1: a real generate_image call is dispatched by the model and the artifact lands in the bucket', async ({
  page,
}) => {
  test.setTimeout(300_000);

  const tag = runTag();
  const toolkitName = `${AUTOTEST_PREFIX}imagegentk_${tag}`;
  const bucket = `autotest-imagegentk-${tag}`;
  const namePrefix = 'e2e-proof-';
  // The expected filepath is DERIVED from the toolkit's own configured
  // bucket/prefix — `ImageGenAPIWrapper._save_images` names the file
  // `{name_prefix}{generate|edit}-{index}.png` — so this string can only
  // reach the stored reply if the SAME configuration this journey wrote made
  // it all the way through a real toolkit instantiation, a real call to the
  // provider, and a real artifact-bucket write. Nothing in the prompt below
  // names it.
  const expectedFilepath = `/${bucket}/${namePrefix}generate-0.png`;

  await page.goto(`${BASE_URL}/app/chat`);
  await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 60_000 });
  const projectId = await selectedProjectId(page);

  let toolkitId = '';
  try {
    toolkitId = await createLiveImageGenToolkit(page.request, projectId, toolkitName, bucket, namePrefix);

    // A natural-language instruction, not the mock's `[[mock:call_tool …]]`
    // marker syntax: this turn runs against a REAL model, which has to
    // CHOOSE to call `generate_image` out of the schema the runtime offers,
    // exactly the discrimination `liveToolkits.ts`'s `LIVE-TK-*-2` journeys
    // make for the other providers.
    const conversationId = await sendLiveTurn(
      page,
      toolkitId,
      'Use the generate_image tool to create an image described as "a red circle on a white ' +
        `background, tag ${tag}", and report the file path the tool returns.`,
    );

    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 240_000,
      contains: expectedFilepath,
      message:
        `the stored reply never quoted ${expectedFilepath} back — either generate_image was not ` +
        'offered to the model, the model never called it, the gateway/provider images hop failed, ' +
        'or the result never made it into the artifact bucket',
    });

    await page.request.delete(`${API_BASE}/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`);
  } finally {
    if (toolkitId !== '') {
      await page.request.delete(`${API_BASE}/elitea_core/tool/prompt_lib/${projectId}/${toolkitId}`).catch(() => {});
    }
  }
});
