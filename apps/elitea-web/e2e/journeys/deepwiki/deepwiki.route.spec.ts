/**
 * Journey DWIKI-001/002/003: the wiki browser lists a project's wikis and
 * renders a page.
 *
 * ## What this stack can and cannot prove
 *
 * It runs NO DeepWiki provider service (deploy/docker-compose.e2e-standalone.yml
 * carries elitea-main, elitea-web, oidc-mock, postgres, nats, rustfs and
 * traefik). So generating a wiki and asking questions about one cannot be
 * exercised here at all: every invocation would fail at the facade's mTLS hop
 * to a service that is not there.
 *
 * What it CAN prove is the half that reads: a wiki that already exists in the
 * artifact store is listed, and its page is fetched and rendered. The seed
 * writes that wiki the way the provider would — objects under
 * `p/{project}/b/wiki-artifacts/o/{wiki_id}/…`, which is elitea-main's own
 * physical layout (internal/infra/storage/ref.go) — so what is asserted here is
 * the real read path and not a mock of it.
 *
 * ## Why the assertions are shaped this way
 *
 * "No wiki has been generated for this project yet" is what this screen shows
 * for an empty bucket, and it is ALSO what it showed for every stack before
 * issue #665 was fixed, when the provider wrote to routes elitea-main does not
 * serve. A journey that only asserted "the route renders" would have passed
 * throughout that. So the assertions name the SEEDED wiki by its title and its
 * page by its content: both come from the object store, and neither can be
 * produced by a screen that is merely mounted.
 */
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';

import { expect, test } from '@playwright/test';

import { STORAGE_STATE } from '../../../playwright.config';
import { openDeepWiki, SEEDED as FIXTURE } from './helpers';

/**
 * The catalogue card's name, read from the bundle the app ships (ADR-0024
 * WP8 moved it there from a literal). Read at test time so a renamed key or
 * a changed value fails HERE, not in a stale copy of the string.
 */
const EN_BUNDLE = JSON.parse(
  readFileSync(join(dirname(fileURLToPath(import.meta.url)), '../../../src/shared/i18n/en.json'), 'utf8'),
) as Record<string, string>;
const WIKIS_CATALOG_NAME = EN_BUNDLE['apps.catalog.wikis.name'];

/** The read-only toolkit and its wiki, as scripts/e2e-stack.sh seeds them. */
const SEEDED = { projectName: FIXTURE.projectName, ...FIXTURE.readOnly } as const;

/** What scripts/e2e-stack.sh seeds. Changing either without the other is a red journey. */

/**
 * Navigate and wait until the browser is BACK ON THE APP ORIGIN.
 *
 * This is what an earlier version of this file got wrong, and it failed on
 * chromium and passed on webkit — the same bug, with one engine happening to
 * finish the hop before the assertion ran. `networkidle` is not enough: an auth
 * redirect is a navigation, and a URL read mid-hop reports the identity
 * provider rather than a routing decision.
 */

test.describe('DeepWiki', () => {
  test.use({ storageState: STORAGE_STATE.member });

  test('DWIKI-001: /deepwiki resolves the project’s wiki toolkit', async ({ page }) => {
    // The project has exactly one wiki toolkit, so this route renders it rather
    // than offering a choice. Deliberately NOT a redirect — see the route's own
    // header for why the URL must not depend on how many toolkits exist.
    await openDeepWiki(page, '/app/deepwiki');
    expect(page.url()).toContain('/deepwiki');

    // The project really switched. Without this the next assertions could pass
    // against project 1 for the wrong reason — or fail for one.
    await expect(page.getByText(SEEDED.projectName)).toBeVisible({ timeout: 20_000 });

    // Neither of the two "nothing to show" states. Both are real screens and
    // both would mean the toolkit was not resolved.
    await expect(page.getByTestId('deepwiki-no-toolkits')).toHaveCount(0);
    await expect(page.getByTestId('deepwiki-toolkit-error')).toHaveCount(0);
  });

  test('DWIKI-002: the seeded wiki is listed, read from the artifact bucket', async ({ page }) => {
    await openDeepWiki(page, `/app/deepwiki/${SEEDED.toolkitId}`);

    // BY TITLE, which lives only in the manifest object. A mounted-but-empty
    // screen cannot produce it, and neither can one whose repository filter
    // does not match the seeded wiki.
    await expect(page.getByText(SEEDED.wikiTitle)).toBeVisible({ timeout: 20_000 });

    // The repository the toolkit is configured for is shown beside it, which is
    // what says the manifest was matched against the toolkit's identity rather
    // than listed unfiltered.
    await expect(page.getByText(SEEDED.repository, { exact: false }).first()).toBeVisible();

    // And NOT the empty state. This is the assertion that fails on a stack
    // where the provider writes to routes elitea-main does not serve (#665) —
    // the defect that kept this feature's flag off.
    await expect(page.getByText(/No wiki has been generated/i)).toHaveCount(0);
  });

  test('DWIKI-003: the wiki page is fetched and rendered as markdown', async ({ page }) => {
    await openDeepWiki(page, `/app/deepwiki/${SEEDED.toolkitId}`);

    // The first page opens without a click.
    const content = page.getByTestId('wiki-page-content');
    await expect(content).toBeVisible({ timeout: 20_000 });

    // The page is listed by the key the MANIFEST claims.
    await expect(page.getByText(SEEDED.page)).toBeVisible();

    // Rendered as MARKDOWN, not as source: the heading is an <h1> and the
    // literal `#` is gone. A viewer that dumped the text would pass a
    // contains-check and fail this one.
    await expect(content.locator('h1')).toHaveText(SEEDED.pageHeading);
    await expect(content).not.toContainText(`# ${SEEDED.pageHeading}`);

    // Neither failure state. `wiki-page-unreadable` in particular is what a
    // reader that mis-decodes a text object shows, and it renders content-free.
    await expect(page.getByTestId('wiki-page-error')).toHaveCount(0);
    await expect(page.getByTestId('wiki-page-unreadable')).toHaveCount(0);
  });

  test('DWIKI-001b: a toolkit that does not exist is reported, not rendered empty', async ({
    page,
  }) => {
    // "This toolkit could not be read" and "this wiki has no pages" are
    // different facts. Rendering the browser for an unreadable toolkit would
    // say the second about a repository the screen never learned the name of.
    //
    // THIS TEST FOUND A ROUTING BUG, which is worth recording because the bug
    // was invisible: as `deepwiki.tsx` the index route became the PARENT of
    // `deepwiki.$toolkitId.tsx`, and a parent that renders its own UI without
    // an `<Outlet/>` swallows its child. `/deepwiki/999999` matched, rendered
    // the index, and showed the project's own wiki as though the id had been
    // honoured. Every other assertion in this file passed throughout.
    await openDeepWiki(page, '/app/deepwiki/999999');
    await expect(page.getByTestId('deepwiki-toolkit-error')).toBeVisible({ timeout: 20_000 });
    await expect(page.getByTestId('wiki-page-content')).toHaveCount(0);
  });

  test('DWIKI-015: the App Catalog names the feature from the i18n bundle, never the engine behind it', async ({
    page,
  }) => {
    // ADR-0024 decision 8: a sub-application is a native screen, and the
    // only vendor-shaped things left were its TEXT. The catalogue card is the
    // one place a user reads the feature's name and description together.
    expect(WIKIS_CATALOG_NAME, 'apps.catalog.wikis.name is in en.json').toBeTruthy();
    await openDeepWiki(page, '/app/apps/catalog');

    // The card, by the bundle's name. `.first()` because the card renders
    // the name once as its title and once as the type label.
    await expect(page.getByText(WIKIS_CATALOG_NAME, { exact: true }).first()).toBeVisible({ timeout: 20_000 });
    await expect(page.getByRole('link', { name: /Documentation/ }).first()).toBeVisible();

    // And the engine's product name nowhere on the page — not in a card, not
    // in a tooltip source, not in the navigation. This is the assertion the
    // pre-WP8 description ("DeepWiki turns a code repository…") fails.
    await expect(page.getByText(/DeepWiki/)).toHaveCount(0);
  });
});
