/**
 * Journey 34: Admin › Configuration states what it cannot configure (JRNY-034)
 *
 * ## What this journey is now, and why it changed
 *
 * It used to be "a platform setting saved here is the setting the product
 * reads", over the `resources` section. Unit A14's **Features** page took that
 * section — the reference puts it there (`ConfigurationPage.jsx` subtracts it
 * via `MOVED_TO_FEATURES`; `FeaturesPage.jsx` renders it as "Help Center"), and
 * #217 recorded that it had parked the section here only because the server's
 * schema had it and because leaving it out would have kept #26 open for another
 * unit. The round trip it proved now lives in journey 35 (`admin.features.spec.ts`),
 * unchanged in substance and asserted against the same public route.
 *
 * Most of what is left on this page is every section this platform cannot serve
 * HERE — Pylon plugin configuration, and a maintenance hook that does not
 * exist. That is a true and useful thing for the page to say, and it is most of
 * what this journey asserts: not that a form renders, but that each pane states
 * a server-declared REASON and each endpoint refuses the write rather than
 * accepting and discarding it.
 *
 * **Guardrails is the exception, and is now the page's landing section.** It was
 * on the unavailable list until the toolkit surfaces, the toolkit write paths
 * and the agent tool freeze started reading `toolkit_security.*`. So this
 * journey no longer says "everything here is unavailable"; it says which
 * sections are, which one is not, and — because the difference is the whole
 * point of the page — that the two look different.
 *
 * **LLM Governance is unavailable for a CHANGED REASON, not a changed status.**
 * It used to be withheld because nothing enforced it. #218 made the gateway
 * read and enforce `gateway.governance_config`, so the section now points at
 * `/admin/app/governance` and says the definitions take effect. It stays
 * unavailable HERE because a governance corpus is a list of scoped rows and
 * this page is a flat form over one value document.
 *
 * Before unit A14 every one of those sections answered 200 on both verbs — the
 * GET with schema defaults, the PUT with an empty object and the request body
 * never read. A journey asserting "a configuration form is present and saving
 * shows a toast" would have passed against all of it.
 */
import { test as adminTest, expect, type Page } from '@playwright/test';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

adminTest.describe.configure({ mode: 'serial' });

async function openConfiguration(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/configuration', {
    waitUntil: 'domcontentloaded',
  });
  expect(response?.status(), 'the admin SPA must serve the configuration route, not 404').toBeLessThan(400);
  // The sidebar rendering at all proves the SCHEMA read was authorised; an
  // empty one would mean the read is unwired, which reads identically to a
  // deployment with no sections.
  await expect(page.getByRole('button', { name: /Guardrails/ })).toBeVisible({ timeout: 20_000 });
}

adminTest('J34: the sidebar marks what this deployment cannot configure', async ({ page }) => {
  await openConfiguration(page);

  await expect(page.getByText('Failed to load the configuration sections.')).toHaveCount(0);

  // Marked in the SIDEBAR, so the shape of what this deployment offers is
  // legible before the operator clicks anything.
  //
  // ENUMERATED, not counted. This was `count() > 5`, and a count cannot tell
  // "a section became available with a consumer behind it" from "a section lost
  // its mark by accident" — both move the number the same way. It is the same
  // correction config_values_postgres_integration_test.go states for the live
  // set on the server side, and the same reason: a section changing status has
  // to be written down next to what justifies it.
  //
  // These two are marked because this platform genuinely cannot serve them
  // HERE: both are authored on their own surfaces (LLM Governance at
  // /admin/app/governance, Service Descriptors on its own page). Observability,
  // Runtime and Admin Panel used to bring the count to five; they were REMOVED
  // from the schema entirely rather than ported — see config_schemas.go's
  // "Observability, Runtime and Admin Panel are GONE too" note — so there is no
  // row left for them to mark.
  // SCOPED to the section list, not the page. The admin NAV RAIL carries its
  // own "Service Descriptors" and "LLM Governance" entries, so an unscoped
  // `getByRole('button', {name: /LLM Governance/})` matches two elements and
  // Playwright's strict mode refuses it. The list declares
  // `aria-label="Configuration sections"` precisely so it can be addressed.
  const sections = page.getByRole('navigation', { name: 'Configuration sections' });
  const marks = sections.getByText('Not available here');
  await expect(marks.first()).toBeVisible();
  await expect(marks).toHaveCount(2);
  for (const section of ['LLM Governance', 'Service Descriptors']) {
    await expect(
      sections.getByRole('button', { name: new RegExp(section) }).getByText('Not available here'),
      `${section} is still not configurable here`,
    ).toBeVisible();
  }

  // And the sections that LEFT that group are not marked. Asserting only the
  // two above would pass on a build where Banner and Maintenance had silently
  // reverted to refusing, since the count would still be right if something
  // else had also changed.
  for (const section of ['Banner', 'Maintenance', 'Guardrails', 'LLM Proxy', 'Authentication']) {
    await expect(
      sections.getByRole('button', { name: new RegExp(section) }).getByText('Not available here'),
      `${section} is available and must not be marked`,
    ).toHaveCount(0);
  }

  // Advanced is not marked either — it is GONE. Its subject is Pylon plugin
  // loading, which the target architecture drops on purpose, so a permanent
  // "not available here" row would be a promise that can never be kept.
  await expect(sections.getByRole('button', { name: /Advanced/ })).toHaveCount(0);

  // Guardrails is NOT among them, and is the section the page lands on. The
  // count above cannot show that on its own: it would still pass if every
  // section were unavailable, which is what it asserted before guardrails had
  // consumers.
  const guardrails = page.getByRole('button', { name: /Guardrails/ });
  await expect(guardrails).toBeVisible();
  await expect(guardrails.getByText('Not available here')).toHaveCount(0);

  // MCP Servers is the OTHER exception, for a different reason, and the
  // difference is worth keeping visible: Guardrails dropped its
  // `unavailable_reason` outright, while MCP Servers still declares one — true
  // of the plugin-config value endpoints, which cannot serve a catalogue that
  // carries a client secret — and is editable anyway because it also declares
  // a `managed_surface`. Marking it would send an operator away from the only
  // page that can edit it.
  const mcpServers = page.getByRole('button', { name: /MCP Servers/ });
  await expect(mcpServers).toBeVisible();
  await expect(mcpServers.getByText('Not available here')).toHaveCount(0);

  // Authentication is the third exception, and the same shape as MCP Servers:
  // it keeps its `unavailable_reason` — true of the plugin-config value
  // endpoints, which cannot hold an OIDC client secret — and declares a
  // `managed_surface` that this build renders.
  const authentication = page.getByRole('button', { name: /Authentication/ });
  await expect(authentication).toBeVisible();
  await expect(authentication.getByText('Not available here')).toHaveCount(0);

  await checkA11y(page);
});

adminTest('J34a: Guardrails offers a real form, including its two tool maps', async ({ page }) => {
  await openConfiguration(page);
  await page.getByRole('button', { name: /Guardrails/ }).click();

  // A save control exists here and nowhere else on this page — the difference
  // between a section with a backend and a section without one.
  await expect(page.getByRole('button', { name: 'Save' })).toBeVisible();
  await expect(page.getByTestId('admin-configuration-unavailable')).toHaveCount(0);

  // The two map fields render as EDITORS, not as the "no editor for this field
  // yet" row. Guardrails is order 1, so this is the first screen of the page,
  // and `blocked_tools`/`sensitive_tools` are the substance of the feature —
  // shipping them inert would have made the landing screen a form whose two
  // most important controls do nothing.
  // Named per field: two identically-labelled buttons on one screen would be
  // ambiguous to a screen reader and to this assertion alike.
  await expect(page.getByRole('button', { name: 'Add toolkit — Blocked Tools' })).toBeVisible();
  await expect(page.getByRole('button', { name: 'Add toolkit — Sensitive Action Tools' })).toBeVisible();
  await expect(
    page.getByText('This platform has no editor for this field yet', { exact: false }),
  ).toHaveCount(0);

  await checkA11y(page);
});

adminTest('J34b: the sections with no backend say so instead of showing a form', async ({ page }) => {
  await openConfiguration(page);

  // Guardrails left this list when it gained consumers — see J34a;
  // Authentication left it when it gained a typed store and a login path that
  // reads it — see J34f; LiteLLM left it by ceasing to exist, replaced by the
  // managed LLM Proxy section — see J34g; and Banner and Maintenance left it
  // by gaining, respectively, a renderer in the app shell and an enforcing
  // middleware — see J34i and J34j.
  //
  // Advanced did not leave this list, it left the PAGE: its subject is Pylon
  // plugin loading, which the target architecture removes deliberately, so the
  // section is gone rather than permanently withheld. Observability, Runtime
  // and Admin Panel followed it out the door for the same reason — see
  // config_schemas.go's "Observability, Runtime and Admin Panel are GONE too"
  // note — so this loop is repointed at the two names that are still genuinely
  // unavailable: Service Descriptors and LLM Governance, each pointing an
  // operator at a page of its own.
  //
  // Every name here has to be a section that is STILL unavailable, or this
  // journey asserts a refusal that no longer happens and would pass only by
  // never reaching the click.
  //
  // SCOPED to the section list, for the same reason J34's own click is: the
  // admin NAV RAIL carries its own "Service Descriptors" and "LLM Governance"
  // entries, so an unscoped `getByRole('button', {name: /LLM Governance/})`
  // matches two elements and Playwright's strict mode refuses it.
  //
  // Each name carries its OWN expected phrase. One shared regex was wrong here:
  // `/gateway|page of their own/` matched LLM Governance (whose reason names
  // /admin/gateway/governance) and silently could not match Service
  // Descriptors. A single alternation over two genuinely different reasons is
  // an assertion that passes for the wrong section.
  //
  // Service Descriptors' phrase CHANGED with migration 0107. It used to be
  // `/provider hub|ADR-0012|no descriptor store/` — the endpoints' own refusal,
  // which the section borrowed while the page refused too. The page now lists
  // registrations, so that sentence would claim this deployment has no
  // descriptor store while the page it points at shows one. What is asserted
  // instead is the PATH, which is the part an operator has to be able to
  // follow; matching on prose would have to be rewritten again the next time
  // the wording moves.
  const sections = page.getByRole('navigation', { name: 'Configuration sections' });
  const stillUnavailable: ReadonlyArray<readonly [string, RegExp]> = [
    ['Service Descriptors', /\/admin\/app\/service-descriptors/],
    ['LLM Governance', /gateway/],
  ];
  for (const [section, reason] of stillUnavailable) {
    await sections.getByRole('button', { name: new RegExp(section) }).click();
    const notice = page.getByTestId('admin-configuration-unavailable');
    await expect(notice).toBeVisible();
    // The reason names the system, so an operator can tell "this platform
    // cannot configure that" from "that is configured to its defaults". A form
    // over defaults would say the second when the truth is the first.
    //
    // No "Pylon" alternative any more: neither remaining name is a Pylon
    // plugin runtime, so that branch has no section left to match.
    await expect(notice).toContainText(reason);
    await expect(page.getByRole('button', { name: 'Save' })).toHaveCount(0);
  }

  await checkA11y(page);
});

adminTest('J34c: an unavailable section refuses its write rather than discarding it', async ({ page }) => {
  await openConfiguration(page);

  // Forged: the page offers no control at all for this section, which is the
  // point — the refusal must be the SERVER's, not the absence of a button.
  const status = await page.evaluate(async () => {
    const response = await fetch('/api/v2/admin/plugin_config_values/administration/auth', {
      method: 'PUT',
      credentials: 'include',
      headers: { 'Content-Type': 'application/json' },
      body: JSON.stringify({ values: { oidc_client_secret: 'hunter2' } }),
    });
    return response.status;
  });
  expect(status, 'an unavailable section must refuse its write, not accept and discard it').toBe(501);
});

adminTest('J34f: the Authentication section renders its editor, not a refusal', async ({ page }) => {
  await openConfiguration(page);

  await page.getByRole('button', { name: /Authentication/ }).click();

  // The editor's own control, so this proves the dedicated surface MOUNTED —
  // not merely that the refusal notice is absent, which a blank pane would also
  // satisfy.
  await expect(page.getByTestId('admin-identity-providers-add')).toBeVisible();
  await expect(page.getByTestId('admin-configuration-unavailable')).toHaveCount(0);

  // The provider read is authorised and answered. This stack federates its
  // logins through the environment rather than an authored row, so the empty
  // state is the truthful branch here; the error alert would mean the route
  // refused, which is what this asserts is NOT happening.
  await expect(page.getByTestId('admin-identity-providers-empty')).toBeVisible();
  await expect(page.getByTestId('admin-identity-providers-error')).toHaveCount(0);

  // SCIM group provisioning renders in the SAME section: a provider federates
  // the login and SCIM pushes the directory, and one of the two is not
  // configuration an operator finds on another screen.
  //
  // Its own read is authorised and answered too. The empty state is the
  // truthful branch on this stack — no group is bound — and the error alert
  // would mean the route refused, which is what this asserts is NOT happening.
  await expect(page.getByTestId('admin-scim-group-bindings-add')).toBeVisible();
  await expect(page.getByTestId('admin-scim-group-bindings-empty')).toBeVisible();
  await expect(page.getByTestId('admin-scim-group-bindings-error')).toHaveCount(0);

  await checkA11y(page);
});

adminTest('J34d: the sections the Features page owns are not offered here', async ({ page }) => {
  await openConfiguration(page);

  // One section editable from two pages would be two drafts of one row, and the
  // two pages would disagree about whether it is dirty. The partition is
  // server-declared (`page: "features"`); this asserts the client honours it.
  for (const moved of ['Help Center', 'MCP Configuration', 'Agent Publishing', 'Voice Features']) {
    await expect(page.getByRole('button', { name: new RegExp(moved) })).toHaveCount(0);
  }
});

adminTest('J34e: the MCP Servers section renders its editor, not a refusal', async ({ page }) => {
  await openConfiguration(page);

  await page.getByRole('button', { name: /MCP Servers/ }).click();

  // The editor's own control, so this proves the dedicated surface MOUNTED —
  // not merely that the refusal notice is absent, which a blank pane would also
  // satisfy.
  await expect(page.getByTestId('admin-mcp-servers-add')).toBeVisible();
  await expect(page.getByTestId('admin-configuration-unavailable')).toHaveCount(0);

  // The catalogue read is authorised and answered. A fresh deployment catalogues
  // nothing, so the empty state is the truthful branch here; the error alert
  // would mean the route refused, which is what this asserts is NOT happening.
  await expect(page.getByTestId('admin-mcp-servers-empty')).toBeVisible();
  await expect(page.getByTestId('admin-mcp-servers-error')).toHaveCount(0);

  await checkA11y(page);
});

adminTest('J34g: the LLM Proxy section renders its editor, not a refusal', async ({ page }) => {
  await openConfiguration(page);

  // The section that replaced LiteLLM. LiteLLM is gone — ADR-0015 replaced it
  // with the Bifrost-based elitea-llm-gateway — so the old name must not be on
  // this page at all: an operator who finds it would look for controls over a
  // subsystem that no longer exists.
  await expect(page.getByRole('button', { name: /LiteLLM/ })).toHaveCount(0);

  const llmProxy = page.getByRole('button', { name: /LLM Proxy/ });
  await expect(llmProxy).toBeVisible();
  // Editable despite still declaring an `unavailable_reason` — true of the
  // plugin-config value endpoints, which cannot serve a live status report or a
  // price catalogue — because it also declares a `managed_surface`. Marking it
  // would send an operator away from the only page that can edit it.
  await expect(llmProxy.getByText('Not available here')).toHaveCount(0);

  await llmProxy.click();

  // The editor's own control, so this proves the dedicated surface MOUNTED —
  // not merely that the refusal notice is absent, which a blank pane would also
  // satisfy.
  await expect(page.getByTestId('llm-proxy-tabs')).toBeVisible();
  await expect(page.getByTestId('admin-configuration-unavailable')).toHaveCount(0);

  // The status panel reaches the gateway through elitea-main. On a stack with no
  // gateway wired it reports that as a state rather than as a load failure, so
  // either the loaded snapshot or the explained unreachable notice is correct
  // here — what must NOT happen is the panel failing to read its own route.
  await expect(page.getByTestId('llm-proxy-status-error')).toHaveCount(0);

  await checkA11y(page);
});

adminTest('J34h: the LLM Proxy model catalogue is authorised and answers', async ({ page }) => {
  await openConfiguration(page);
  await page.getByRole('button', { name: /LLM Proxy/ }).click();
  await page.getByRole('tab', { name: 'Models & pricing' }).click();

  // Wait for a POSITIVE terminal state before judging. `llm-proxy-add-price`
  // renders unconditionally and the error test-ids are absent while the request
  // is still in flight, so asserting only those would pass against a route that
  // 403s or 500s a moment later — absence read as success.
  //
  // A fresh deployment has not run a price sync, so the empty state is the
  // truthful branch; a stack with a catalogue shows the table. Either proves the
  // read completed.
  await expect(
    page.getByTestId('llm-proxy-models-empty').or(page.getByTestId('llm-proxy-models-table')),
  ).toBeVisible();
  await expect(page.getByTestId('llm-proxy-add-price')).toBeVisible();
  await expect(page.getByTestId('llm-proxy-models-load-error')).toHaveCount(0);
  await expect(page.getByTestId('llm-proxy-models-error')).toHaveCount(0);

  await checkA11y(page);
});

adminTest('J34i: the Banner section is an editable form, and the save round-trips', async ({ page }) => {
  await openConfiguration(page);

  const banner = page.getByRole('button', { name: /^Banner/ });
  await expect(banner).toBeVisible();
  // It left the unavailable list by acquiring a consumer: `platform_settings`
  // publishes the resolved banner and `widgets/app-shell` renders it. Before
  // that the legacy SPA took the banner from a BUILD-TIME environment variable,
  // so this form wrote rows nothing anywhere read.
  await expect(banner.getByText('Not available here')).toHaveCount(0);

  await banner.click();
  await expect(page.getByTestId('admin-configuration-unavailable')).toHaveCount(0);

  const message = page.getByRole('textbox', { name: 'Banner Message' });
  await expect(message).toBeVisible();

  // The round trip, and then back. Leaving the banner up would put a bar across
  // the top of every screen for every journey that runs after this one —
  // `describe.configure({mode: 'serial'})` means they share this deployment.
  const raised = 'Journey 34 banner check.';
  await message.fill(raised);
  await page.getByRole('button', { name: 'Save' }).click();
  await expect(page.getByTestId('admin-configuration-saved')).toBeVisible();

  await page.reload({ waitUntil: 'domcontentloaded' });
  await page.getByRole('button', { name: /^Banner/ }).click();
  // The value came back from the STORE, not from the form state that wrote it —
  // a save that reported success and stored nothing looks identical until the
  // reload.
  await expect(page.getByRole('textbox', { name: 'Banner Message' })).toHaveValue(raised);

  await page.getByRole('textbox', { name: 'Banner Message' }).fill('');
  await page.getByRole('button', { name: 'Save' }).click();
  await expect(page.getByTestId('admin-configuration-saved')).toBeVisible();

  await checkA11y(page);
});

adminTest('J34j: the Maintenance section is an editable form', async ({ page }) => {
  await openConfiguration(page);

  const maintenance = page.getByRole('button', { name: /Maintenance/ });
  await expect(maintenance).toBeVisible();
  await expect(maintenance.getByText('Not available here')).toHaveCount(0);

  await maintenance.click();
  await expect(page.getByTestId('admin-configuration-unavailable')).toHaveCount(0);
  // `switch`, not `checkbox`. The form renders a boolean as MUI's Switch, whose
  // input carries role="switch" — the idiom admin.features.spec.ts already uses
  // for `Enable MCP`. A `checkbox` locator finds nothing and reads as "the form
  // did not render", which is a different and much more alarming failure.
  await expect(page.getByRole('switch', { name: 'Maintenance Mode' })).toBeVisible();
  await expect(page.getByRole('textbox', { name: 'Splash Message' })).toBeVisible();

  // THE SWITCH IS NOT FLIPPED HERE, deliberately. Enabling it would make
  // elitea-main answer 503 to every non-admin request on this deployment, and
  // these journeys share one stack in serial — every later journey that signs in
  // as anyone else would fail against a platform this test closed. The
  // enforcement itself is covered where it can be covered in isolation:
  // `internal/api/middleware/maintenance_internal_test.go` for who is admitted
  // and what escapes the window, and `widgets/app-shell`'s tests for the splash
  // replacing the shell and an exempt caller keeping the product.
  await expect(page.getByRole('switch', { name: 'Maintenance Mode' })).not.toBeChecked();

  await checkA11y(page);
});

adminTest('J34k: the LLM Proxy usage report is authorised and answers', async ({ page }) => {
  await openConfiguration(page);
  await page.getByRole('button', { name: /LLM Proxy/ }).click();
  await page.getByRole('tab', { name: 'Usage' }).click();

  // The screen LiteLLM's admin UI had and the Bifrost port lost — migration
  // 0084 recorded the loss in its own header ("a meter and nothing else").
  //
  // Wait on a POSITIVE terminal state. The totals tiles render for a zero
  // report as well as a busy one, so their presence proves the read COMPLETED,
  // where asserting only the absence of the error test-ids would pass while the
  // request was still in flight — absence read as success.
  await expect(page.getByTestId('llm-proxy-usage-totals')).toBeVisible();
  await expect(page.getByTestId('llm-proxy-usage-load-error')).toHaveCount(0);
  await expect(page.getByTestId('llm-proxy-usage-error')).toHaveCount(0);

  // Each breakdown is its own statement on the server: a section that failed
  // must not be indistinguishable from one with no spend in it. On a fresh
  // deployment the ledger is empty, so the explained empty state is the
  // truthful branch and a table is the other; a per-section ERROR is neither.
  for (const section of ['models', 'projects', 'members']) {
    await expect(
      page
        .getByTestId(`llm-proxy-usage-${section}`)
        .or(page.getByTestId(`llm-proxy-usage-${section}-empty`)),
    ).toBeVisible();
    await expect(page.getByTestId(`llm-proxy-usage-${section}-error`)).toHaveCount(0);
  }

  await checkA11y(page);
});

adminTest('J34l: platform providers are authored from the admin panel', async ({ page }) => {
  await openConfiguration(page);
  await page.getByRole('button', { name: /LLM Proxy/ }).click();
  await page.getByRole('tab', { name: 'Providers & models' }).click();

  // A PLATFORM provider is the public project's `shared = true` credential — the
  // scope the gateway has resolved since issue #316 and that nothing could
  // author from here. Wait on a POSITIVE terminal state: either the table or the
  // explained empty state proves the read completed, where asserting only the
  // absence of the error test-id would pass while the request was in flight.
  await expect(
    page.getByTestId('llm-providers-table').or(page.getByTestId('llm-providers-empty')),
  ).toBeVisible();
  await expect(page.getByTestId('llm-providers-load-error')).toHaveCount(0);
  await expect(page.getByTestId('llm-providers-add')).toBeVisible();

  // NO CREDENTIAL IS PUBLISHED HERE. These journeys share one deployment in
  // serial, and a platform provider resolves for every project on it — so a
  // credential this test created would join the model resolution of every later
  // journey, and a bogus one would make them fail in ways that point nowhere
  // near this file. The write path's guards are covered where they can be
  // covered in isolation: internal/api/v2/configurations/global_providers_test.go
  // for the type allowlist, the forced `shared`, the body bound and the
  // redaction, and LlmProxyProvidersPanel.test.tsx for the untouched-secret
  // contract.
  await page.getByTestId('llm-providers-add').click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible();
  // The dialog offers a provider SELECT rather than a free-text type: the
  // server refuses anything outside the gateway's set, and a text field would
  // make that refusal the operator's first feedback.
  await expect(page.getByTestId('llm-provider-type')).toBeVisible();
  await expect(page.getByTestId('llm-provider-api_key')).toHaveValue('');
  await page.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toHaveCount(0);

  // The MODELS half of the same tab. A model names a credential, so the two are
  // one screen: the commonest mistake is a model naming a provider that is not
  // there, and two screens would put the cause and the effect a click apart.
  await expect(
    page.getByTestId('platform-models-table').or(page.getByTestId('platform-models-empty')),
  ).toBeVisible();
  await expect(page.getByTestId('platform-models-load-error')).toHaveCount(0);
  await expect(page.getByTestId('platform-models-add')).toBeVisible();

  await checkA11y(page);
});

adminTest('J34n: a platform model cannot be authored without a platform provider', async ({
  page,
}) => {
  await openConfiguration(page);
  await page.getByRole('button', { name: /LLM Proxy/ }).click();
  await page.getByRole('tab', { name: 'Providers & models' }).click();

  // Wait on a POSITIVE terminal state before opening the dialog: the dialog's
  // provider select is filled from this same listing, so an assertion made
  // while it was in flight would be about an empty form rather than an empty
  // platform.
  await expect(
    page.getByTestId('platform-models-table').or(page.getByTestId('platform-models-empty')),
  ).toBeVisible();

  await page.getByTestId('platform-models-add').click();
  const dialog = page.getByRole('dialog');
  await expect(dialog).toBeVisible();

  // THE DEFECT THIS PINS. The provider select used to offer "None — infer from
  // the model name" beside the published providers, and choosing it omitted the
  // `data.ai_credentials` link. That link is required on all five model types,
  // so the row failed provider admission and was stored with `status_ok =
  // false` — listed on this screen and served by no reader, the gateway
  // included. Asserted as an ABSENCE, which is what the removal is.
  //
  // The OPEN LISTBOX is the positive terminal state, not the first option: this
  // deployment publishes no platform credential (see J34l for why none is
  // created here), so the correct list is EMPTY and waiting for an option would
  // wait for ever. What must not be in it is the one entry that never depended
  // on the platform having a provider.
  await dialog.getByRole('combobox', { name: /Platform provider/ }).click();
  await expect(page.getByRole('listbox')).toBeVisible();
  await expect(page.getByRole('option', { name: /None/ })).toHaveCount(0);
  await expect(page.getByRole('option', { name: /infer/i })).toHaveCount(0);
  await page.keyboard.press('Escape');

  // Save waits for one. Everything else the form needs is filled in, so the
  // disabled button is about the provider and nothing else — and the helper
  // text says what an unlinked model would do, because "Save is greyed out"
  // with no reason is the state an operator files a bug about.
  await dialog.getByTestId('platform-model-name').fill('autotest_unlinked_model');
  await dialog.getByTestId('platform-model-wire-name').fill('autotest-unlinked');
  await expect(dialog.getByTestId('platform-model-save')).toBeDisabled();
  await expect(dialog.getByText(/served to nobody|publishes no providers yet/)).toBeVisible();

  // The tier flags are offered for the chat kind, which is the kind a new
  // model opens on. They decide which of a project's default model slots may
  // select the model, and a platform model that carried neither could be
  // addressed by name and chosen as nobody's default.
  await expect(dialog.getByTestId('platform-model-low-tier')).toBeVisible();
  await expect(dialog.getByTestId('platform-model-high-tier')).toBeVisible();

  // "Available to" — WHO gets the model. A platform model used to be offered
  // to every project the moment it existed; it can now be granted to every
  // project, to none, or to a chosen set. The control opens on "All projects",
  // which is what publishing one used to mean, so an operator who ignores it
  // gets the behaviour they had. The project picker is NOT here yet, and its
  // absence is the assertion: it is mounted for the one scope that reads it,
  // so a dialog opened to rename a model does not fetch a page of projects.
  //
  // The three choices, the wire they produce and the grant a stored model
  // opens on are pinned where they can be pinned without publishing a platform
  // credential on this shared deployment: PlatformModelsPanel.test.tsx for the
  // form, e2e/journeys/api/api.model-grants.spec.ts for the enforcement.
  const scope = dialog.getByRole('combobox', { name: /Available to/ });
  await expect(scope).toHaveText('All projects');
  await expect(dialog.getByTestId('platform-model-shared-with')).toHaveCount(0);
  await scope.click();
  await expect(page.getByRole('option', { name: 'Selected projects' })).toBeVisible();
  await expect(page.getByRole('option', { name: 'No project' })).toBeVisible();
  await page.keyboard.press('Escape');

  await dialog.getByRole('button', { name: 'Cancel' }).click();
  await expect(dialog).toHaveCount(0);

  await checkA11y(page);
});

adminTest('J34m: the LLM Proxy request log is authorised and answers', async ({ page }) => {
  await openConfiguration(page);
  await page.getByRole('button', { name: /LLM Proxy/ }).click();
  await page.getByRole('tab', { name: 'Logs' }).click();

  // The tab the Usage report cannot be: a billing delta rides only a BILLED
  // request, so a refusal never reaches the ledger Usage reads. This reads
  // gateway.llm_request_logs, written by the gateway off the request path.
  //
  // Wait on a POSITIVE terminal state. The summary tiles render for an empty
  // window as well as a busy one, so their presence proves the read COMPLETED —
  // where asserting only the absence of the error test-ids would pass while the
  // request was still in flight.
  await expect(page.getByTestId('llm-logs-summary')).toBeVisible();
  await expect(page.getByTestId('llm-logs-load-error')).toHaveCount(0);
  await expect(page.getByTestId('llm-logs-error')).toHaveCount(0);
  await expect(page.getByTestId('llm-logs-summary-error')).toHaveCount(0);

  // Either rows or the explained empty state — both prove the page resolved.
  await expect(
    page.getByTestId('llm-logs-table').or(page.getByTestId('llm-logs-empty')),
  ).toBeVisible();

  // The one thing this screen must always say, whatever the window holds: it
  // records no prompts and no responses, and has no column that could.
  await expect(page.getByTestId('llm-logs-no-payload')).toBeVisible();

  // The failures filter narrows SERVER-side; the page is capped there, so a
  // client-side filter would silently exclude everything past the cap.
  await page.getByRole('switch', { name: 'Failures only' }).click();
  await expect(
    page.getByTestId('llm-logs-table').or(page.getByTestId('llm-logs-empty')),
  ).toBeVisible();
  await expect(page.getByTestId('llm-logs-load-error')).toHaveCount(0);

  await checkA11y(page);
});
