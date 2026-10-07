import { expect, type Page } from '@playwright/test';

import { BASE_URL } from '../../../playwright.config';

/**
 * What scripts/e2e-stack.sh seeds for these journeys (project 90200).
 *
 * Toolkit 9001 is READ-ONLY across the suite: DWIKI-001–003 list and read
 * it, the quick-fix and edit journeys change one page and put it back.
 * Toolkit 9002 is the one that gets generated, re-configured and deleted;
 * its repository differs so the wiki it produces never overlaps the seeded
 * one. Spec files run in any order and in parallel.
 */
export const SEEDED = {
  projectId: '90200',
  projectName: 'e2e-deepwiki',
  bucket: 'wiki-artifacts',
  readOnly: {
    toolkitId: '9001',
    wikiId: 'acme--e2e-service--main',
    wikiTitle: 'E2E Service Wiki',
    repository: 'acme/e2e-service',
    page: 'wiki_pages/architecture/router.md',
    pageHeading: 'Router',
    brokenPage: 'wiki_pages/architecture/request-flow.md',
  },
  mutable: {
    toolkitId: '9002',
    wikiId: 'acme--e2e-generated--main',
    repository: 'acme/e2e-generated',
  },
} as const;

export const ERROR_BOUNDARY_TEXT = /something went wrong|unexpected error/i;
export const NOT_FOUND_TEXT = /not found|404/i;

/** Opens a DeepWiki route with project 90200 selected, and asserts the shell did not fall over. */
export async function openDeepWiki(page: Page, path: string): Promise<void> {
  await page.addInitScript(
    ([id, name]) => {
      localStorage.setItem('el.project.id', id);
      localStorage.setItem('el.project.name', name);
      sessionStorage.setItem('el.project.id', id);
      sessionStorage.setItem('el.project.name', name);
    },
    [SEEDED.projectId, SEEDED.projectName] as const,
  );
  await page.goto(`${BASE_URL}${path}`, { waitUntil: 'domcontentloaded' });
  await page.waitForURL((url) => url.origin === new URL(BASE_URL).origin, { timeout: 30_000 });
  await page.waitForLoadState('networkidle');
  await expect(page.getByText(NOT_FOUND_TEXT)).toHaveCount(0);
  await expect(page.getByText(ERROR_BOUNDARY_TEXT)).toHaveCount(0);
}

/** The artifact object route the wiki browser reads pages from — used to verify what LANDED, not what the screen says. */
export function objectUrl(key: string): string {
  return `${BASE_URL}/api/v2/artifacts/objects/${SEEDED.projectId}/${SEEDED.bucket}/${key}`;
}

/** Reads one wiki object through the API with the page's own session. */
export async function readObject(page: Page, key: string): Promise<{ status: number; text: string }> {
  const response = await page.request.get(objectUrl(key));
  return { status: response.status(), text: await response.text() };
}

/**
 * Lists the keys under a wiki id, the way DeleteWikiButton does. `limit`
 * is the listing's page size: a wiki generated from a real repository can
 * hold more than the default 200 objects.
 */
export async function listKeys(page: Page, wikiId: string, limit = 200): Promise<string[]> {
  const response = await page.request.get(
    `${BASE_URL}/api/v2/artifacts/objects/${SEEDED.projectId}/${SEEDED.bucket}?prefix=${encodeURIComponent(wikiId + '/')}&limit=${limit}`,
  );
  if (response.status() === 404) return [];
  expect(response.ok(), `listing ${wikiId}: ${response.status()}`).toBe(true);
  const body = (await response.json()) as { items?: { key: string }[]; objects?: { key: string }[] };
  return (body.items ?? body.objects ?? []).map((o) => o.key);
}

/**
 * Types into a CodeMirror editor. `insertText` is one transaction carrying
 * the exact text, so bracket auto-closing never interleaves with it.
 */
export async function replaceEditorText(page: Page, text: string): Promise<void> {
  const content = page.locator('.cm-content').last();
  await content.click();
  await page.keyboard.press(process.platform === 'darwin' ? 'Meta+A' : 'Control+A');
  await page.keyboard.press('Backspace');
  await page.keyboard.insertText(text);
}
