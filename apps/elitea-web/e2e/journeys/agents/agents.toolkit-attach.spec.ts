/**
 * A toolkit attached to an agent, and taken off again.
 *
 * Ported from the legacy public suite
 * (`qa/elitea-testing-public/automation/tests/ui/agents/
 * test_agent_with_github_toolkit.py`), by use case:
 *
 *   TA-1 ← `TestAddToolkitToAgent::test_add_toolkit_to_agent`
 *   TA-2 ← `TestRemoveToolkitFromAgent::test_remove_toolkit_from_agent`
 *
 * The file's third case, `TestChatWithAgentToolkit::
 * test_agent_chat_with_github_toolkit`, is NOT ported: it needs a live GitHub
 * credential (the tool call has to reach api.github.com) and the full
 * standalone stack's worker and model. The equivalent shape with a
 * credential-free toolkit is already proven on the streaming project —
 * `e2e/streaming/chat.toolkit.spec.ts` drives an `openapi` toolkit through a
 * real tool call against the mock tool server.
 *
 * ## Why this is not already covered
 *
 * `agents.publishing.spec.ts` proves the identical SHAPE for a SKILL —
 * create over the API, attach, count, reload, read the server back — and
 * `e2e/streaming/chat.toolkit.spec.ts` attaches a toolkit as a step towards a
 * chat turn. Neither states the toolkit ENTITY LINK as its own use case, and
 * neither takes one off again: nothing in this suite exercised
 * `ToolCard`'s remove affordance or the disassociation behind it.
 *
 * ## The attach is asserted on the SERVER, not on the response
 *
 * `readAttachedToolkits` reads the agent version projection the runtime will
 * actually be frozen against. That is the discriminating read here: the
 * agent-as-tool attach once answered 200 and wrote no row at all
 * (`chat.agent-tools.spec.ts`'s defect class 2), which a status-code
 * assertion and a card on screen would both have passed.
 */
import { expect, test, type Page } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  attachToolkitThroughPicker,
  createAgentThroughForm,
  createGithubToolkit,
  deleteAgent,
  deleteGithubToolkit,
  readAttachedToolkits,
  DEFAULT_PROJECT_ID,
  type GithubToolkitFixture,
} from '../../fixtures/api';

/**
 * The type the legacy suite attaches (`github`).
 *
 * Kept, and safe to keep: a toolkit ROW of this type is created and linked
 * without any provider being contacted, and nothing here asks the toolkit to
 * run. Only USING it would need GitHub, which is the case this file does not
 * port.
 *
 * CORRECTED. This comment used to say "the Go API validates no credential at
 * create time", and the create it justified posted
 * `settings: { selected_tools: [] }`. The route answers
 * `400 {"error":"settings.repository is required for github toolkit"}`:
 * `validateToolkitCreate`
 * (`services/elitea-main/internal/api/v2/toolkits/handler.go:987-1004`)
 * requires `repository` AND `github_configuration` for this one type, matching
 * the SDK schema's own `required` list. `createGithubToolkit` in
 * `e2e/fixtures/api.ts` supplies both, with a placeholder credential it also
 * removes.
 */
const TOOLKIT_TYPE = 'github';

const RUN_ID = String(Date.now()).slice(-6);

interface Fixture {
  readonly agentId: string;
  readonly projectId: string;
  readonly toolkit: GithubToolkitFixture;
}

/**
 * A saved agent with one toolkit attached through the product's own picker.
 *
 * The toolkit is created FIRST: the picker lists instances the project
 * already holds, so an agent opened before the row exists would page through
 * a list that cannot contain it.
 */
async function agentWithAttachedToolkit(page: Page, suffix: string): Promise<Fixture> {
  const toolkit = await createGithubToolkit(
    page.request,
    DEFAULT_PROJECT_ID,
    `${AUTOTEST_PREFIX}tk_${suffix}_${RUN_ID}`,
  );

  const agent = await createAgentThroughForm(page, `${AUTOTEST_PREFIX}ag_${suffix}_${RUN_ID}`);
  await expect(
    page.getByTestId('agent-toolkits-section'),
    'the save must land on the agent edit page, where the Tools panel lives',
  ).toBeVisible({ timeout: 30_000 });

  await attachToolkitThroughPicker(page, toolkit.toolkitName);

  return { agentId: agent.agentId, projectId: agent.projectId, toolkit };
}

async function cleanUp(page: Page, fixture: Fixture | undefined): Promise<void> {
  if (fixture === undefined) return;
  await deleteGithubToolkit(page.request, fixture.projectId, fixture.toolkit).catch(() => {});
  await deleteAgent(page.request, fixture.agentId).catch(() => {});
}

test('TA-1: a toolkit attached from the agent editor is on the card and in the stored version (legacy test_add_toolkit_to_agent)', async ({
  page,
}) => {
  // An agent through the form, a toolkit over the API, and a picker that
  // pages twenty instances at a time.
  test.setTimeout(240_000);

  let fixture: Fixture | undefined;
  try {
    fixture = await agentWithAttachedToolkit(page, 'add');

    // On screen…
    await expect(
      page.getByTestId('agent-toolkit-card').filter({ hasText: fixture.toolkit.toolkitName }),
      'the attached toolkit must appear in the Tools panel',
    ).toBeVisible({ timeout: 30_000 });

    // …and in the projection the runtime reads. Polled, because the attach
    // and this read are two requests: asserting once would be asserting the
    // order they happened to arrive in.
    await expect
      .poll(
        async () => {
          const attached = await readAttachedToolkits(page, fixture!.projectId, fixture!.agentId);
          return attached.map((row) => row.name);
        },
        {
          timeout: 30_000,
          message: 'the attach answered, but no toolkit row reached the stored agent version',
        },
      )
      .toContain(fixture.toolkit.toolkitName);

    const attached = await readAttachedToolkits(page, fixture.projectId, fixture.agentId);
    const row = attached.find((entry) => entry.name === fixture?.toolkit.toolkitName);
    expect(row?.type, 'the stored row must carry the toolkit type, not an empty string').toBe(
      TOOLKIT_TYPE,
    );
    expect(row?.toolId, 'the stored row must address the toolkit instance').toBe(fixture.toolkit.toolkitId);

    // It SURVIVES a reload — the card is drawn from the server, not from the
    // editor state the attach left behind.
    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('agent-toolkits-section')).toBeVisible({ timeout: 30_000 });
    await expect(
      page.getByTestId('agent-toolkit-card').filter({ hasText: fixture.toolkit.toolkitName }),
    ).toBeVisible({ timeout: 30_000 });
  } finally {
    await cleanUp(page, fixture);
  }
});

test('TA-2: removing a toolkit from an agent takes it off the card and out of the stored version (legacy test_remove_toolkit_from_agent)', async ({
  page,
}) => {
  test.setTimeout(240_000);

  let fixture: Fixture | undefined;
  try {
    fixture = await agentWithAttachedToolkit(page, 'rm');

    const card = page.getByTestId('agent-toolkit-card').filter({ hasText: fixture.toolkit.toolkitName });
    await expect(card).toBeVisible({ timeout: 30_000 });

    // ── the product's own remove affordance ────────────────────────────────
    // Scoped to THIS card: an agent may hold several toolkits and every card
    // carries the same testid, so an unscoped locator is ambiguous the moment
    // a second one exists.
    //
    // HOVERED FIRST, which is what a mouse user does. `ToolCard`'s action
    // cluster is reveal-on-row-hover: every button carries
    // `agents-tool-card-action` and `actionButtonSx`'s `display: 'none'`, and
    // the card header's `&:hover` rule (`ToolCard.styles.ts:68`) is the only
    // thing that puts them back in flow — the same behaviour the baseline gave
    // its `#DeleteButton`. Without the hover the button resolves in the DOM and
    // is never visible, so the click waits out the whole test timeout.
    //
    // NOT `force: true`: forcing would skip the visibility wait and click a
    // control a real user cannot see, which would pass whether or not the
    // reveal still works. `hover()` is the user's own action, and the
    // visibility assertion below states the reveal as part of the contract.
    //
    // The card is the header alone here (`showActions` is false until "Show
    // tools" is clicked), so hovering its centre lands on the header that
    // carries the rule.
    //
    // What this journey does NOT cover: `display: 'none'` also takes the
    // button out of the tab order and the accessibility tree, so a keyboard or
    // touch user has no way to reach it — a `:focus-within` rule cannot fire
    // on a control that can never hold focus. That is the same defect
    // `OpenAPISchemaInput` (`editorWrapperSx`), `BucketList` and
    // `ImageAttachment` were each already fixed for, by revealing through
    // opacity/visibility plus `:focus-within` instead of `display`. It is left
    // open here because reserving the cluster's width changes the card's
    // resting layout, which is a product decision rather than this journey's.
    await card.hover();
    const remove = card.getByTestId('agent-toolkit-delete-button');
    await expect(
      remove,
      'hovering the toolkit card must reveal its remove button',
    ).toBeVisible({ timeout: 15_000 });
    await remove.click();

    // `ToolCard` opens `DeleteEntityModal` with `confirmText: 'Remove'` and
    // WITHOUT `shouldRequestInputName`, so there is no name to type — unlike
    // the credential and agent deletes, which do ask for one.
    const confirm = page.getByRole('button', { name: 'Remove', exact: true });
    await expect(confirm).toBeEnabled({ timeout: 15_000 });
    await confirm.click();

    // Gone from the panel. `.count()` for the absence: it reads the DOM as it
    // stands rather than waiting for an attach that must not happen.
    await expect
      .poll(
        async () =>
          page.getByTestId('agent-toolkit-card').filter({ hasText: fixture!.toolkit.toolkitName }).count(),
        { timeout: 30_000, message: 'the removed toolkit is still on the agent editor' },
      )
      .toBe(0);

    // …and gone from the STORED version, which is the half a client-side
    // splice would also satisfy on screen.
    await expect
      .poll(
        async () => {
          const attached = await readAttachedToolkits(page, fixture!.projectId, fixture!.agentId);
          return attached.map((entry) => entry.name);
        },
        {
          timeout: 30_000,
          message: 'the toolkit was removed on screen but is still attached to the stored version',
        },
      )
      .not.toContain(fixture.toolkit.toolkitName);

    // The TOOLKIT ITSELF survives. Detaching is not deleting: the row holds
    // settings and a credential reference, and taking it off one agent must
    // not destroy it for every other.
    // Read BY ID, never a page of the list: the instance list sorts by name
    // and pages twenty at a time, so "it is on page one" is a statement about
    // how many toolkits the stack happens to hold.
    const stillThere = await page.request.get(
      `${API_BASE}/elitea_core/tool/prompt_lib/${fixture.projectId}/${fixture.toolkit.toolkitId}`,
    );
    expect(
      stillThere.status(),
      `removing a toolkit from an agent must not delete the toolkit: ${(await stillThere.text()).slice(0, 300)}`,
    ).toBe(200);
    expect(((await stillThere.json()) as { name?: string }).name).toBe(fixture.toolkit.toolkitName);

    // It survives a reload too — the removal reached the server, it did not
    // merely repaint.
    await page.reload({ waitUntil: 'domcontentloaded' });
    await expect(page.getByTestId('agent-toolkits-section')).toBeVisible({ timeout: 30_000 });
    await expect(
      page.getByTestId('agent-toolkit-card').filter({ hasText: fixture.toolkit.toolkitName }),
    ).toHaveCount(0);
  } finally {
    await cleanUp(page, fixture);
  }
});
