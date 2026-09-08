/**
 * Journey 38: An operator rebrands the platform from the admin console
 * (JRNY-038, ADR-0024 WP4)
 *
 * ## What this journey proves
 *
 * The Branding page is not a form over rows — it is the surface behind
 * `GET /api/v2/branding/bootstrap.js`, which every user loads. So the
 * assertion that matters is not "the page said saved" but "the bootstrap body
 * changed": after a save through the UI, the served pack carries the product
 * name, the hue and the uploaded logo path. Only a request to the real route
 * can show that; a unit test against MSW cannot.
 *
 * The upload is a real multipart POST of bytes GENERATED here (an SVG built
 * from a string), never a committed binary, and the server sniffs and stores
 * them content-addressed; the path it answers is what the page must have
 * written into the draft, which is what the bootstrap body then names.
 *
 * ## Shared state, and how it is kept honest
 *
 * The database layer of the brand pack is one row set for the whole
 * deployment, and both browser projects run this file. Every run therefore
 * takes the platform-flag WRITER lock — the branding rows are platform-wide
 * state in the same sense the feature flags are — writes project-specific
 * values so a stale write from the other project cannot satisfy the
 * assertions by accident, and restores the layer it found in `finally`, so
 * the visual baseline (`e2e/visual/admin.visual.spec.ts`, the all-inherit
 * state) and every other journey see the deployment as they found it.
 */
import { test as adminTest, expect, request as apiRequest, type Page } from '@playwright/test';
import JSZip from 'jszip';

import { checkA11y } from '../../fixtures/axe';
import { withPlatformFlagLock } from '../../fixtures/platformFlags';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

adminTest.use({ storageState: STORAGE_STATE.admin });

const API = `${BASE_URL}/api/v2`;
const BRANDING_URL = `${API}/admin/branding/administration`;
const ASSET_PATH_PREFIX = '/api/v2/branding/assets/logo-full/';

function probeName(projectName: string): string {
  return `E2E ${projectName} brand`;
}

/** Six-digit hex, lower-case: what the server stores and what the JSON body echoes. */
function probeHue(projectName: string): string {
  return projectName === 'chromium' ? '#3366cc' : '#cc6633';
}

/** A small logo generated from text — nothing binary is committed for this. */
function probeLogo(projectName: string): Buffer {
  const svg = [
    '<svg xmlns="http://www.w3.org/2000/svg" width="120" height="40" viewBox="0 0 120 40">',
    `<rect width="120" height="40" rx="6" fill="${probeHue(projectName)}"/>`,
    `<text x="10" y="26" font-family="sans-serif" font-size="14" fill="#ffffff">${projectName}</text>`,
    '</svg>',
  ].join('');
  return Buffer.from(svg, 'utf8');
}

async function readValues(page: Page): Promise<Record<string, unknown>> {
  const response = await page.request.get(BRANDING_URL);
  expect(response.status(), 'the branding read must be authorised for the admin persona').toBe(200);
  const body = (await response.json()) as { values: Record<string, unknown> };
  return body.values;
}

async function writeValues(page: Page, values: Record<string, unknown>): Promise<void> {
  const response = await page.request.put(BRANDING_URL, { data: { values } });
  expect(response.status(), `restoring the branding rows answered ${response.status()}`).toBe(200);
}

async function openBranding(page: Page): Promise<void> {
  const response = await page.goto(BASE_URL + '/admin/app/branding', { waitUntil: 'domcontentloaded' });
  expect(response?.status(), 'the admin SPA must serve the branding route, not 404').toBeLessThan(400);
  // The form mounts only once the settings query resolved.
  await expect(page.getByRole('textbox', { name: 'Product name' })).toBeVisible({ timeout: 20_000 });
}

adminTest('J38: a rebrand saved through the page reaches the bootstrap route', async ({ page }, testInfo) => {
  const project = testInfo.project.name;
  await withPlatformFlagLock(async () => {
    const original = await readValues(page);
    try {
      await openBranding(page);
      await expect(page.getByRole('heading', { name: 'Branding' })).toBeVisible();

      // The preview and the derived swatches are on screen before anything is edited.
      await expect(page.getByTestId('branding-preview-light')).toBeVisible();
      await expect(page.getByTestId('branding-preview-dark')).toBeVisible();
      const primaryBefore = await page
        .getByTestId('branding-swatch-light-primary')
        .getAttribute('data-value');

      await page.getByTestId('branding-field-product_name').fill(probeName(project));
      await page.getByTestId('branding-field-brand_hue').fill(probeHue(project));

      // The swatch strip is DERIVED from the hue through the real builder, so
      // the primary swatch must move with it.
      await expect
        .poll(async () => page.getByTestId('branding-swatch-light-primary').getAttribute('data-value'))
        .not.toBe(primaryBefore);

      // A real upload: the server answers a content-addressed path, and the
      // page shows that path (still draft state) under the control.
      await page.getByTestId('branding-upload-input-logo-full').setInputFiles({
        name: `${project}-logo.svg`,
        mimeType: 'image/svg+xml',
        buffer: probeLogo(project),
      });
      const shownPath = page.getByTestId('branding-asset-path-logo-full');
      await expect(shownPath).toContainText(ASSET_PATH_PREFIX, { timeout: 20_000 });
      const logoPath = (await shownPath.textContent())?.trim() ?? '';

      await page.getByTestId('branding-save').click();
      await expect(page.getByTestId('branding-toast-success')).toContainText('Branding saved', {
        timeout: 20_000,
      });

      // What every user will load. The body is `window.elitea_brand = {…};`,
      // a JSON pack, so the three values appear in JSON's own spelling.
      const bootstrap = await page.request.get(`${API}/branding/bootstrap.js`);
      expect(bootstrap.status()).toBe(200);
      const body = await bootstrap.text();
      expect(body).toContain(`"name":${JSON.stringify(probeName(project))}`);
      expect(body).toContain(`"hue":${JSON.stringify(probeHue(project))}`);
      expect(body).toContain(`"logoFull":${JSON.stringify(logoPath)}`);

      // The stored rows agree with the page: the read answers the same three.
      const stored = await readValues(page);
      expect(stored['product_name']).toBe(probeName(project));
      expect(stored['brand_hue']).toBe(probeHue(project));
      expect(stored['logo_full']).toBe(logoPath);

      // The layers panel names the database as the deciding layer for the
      // three fields, and the product default for one nobody set.
      const layers = page.getByTestId('branding-layers');
      await expect(layers.getByTestId('branding-layer-row-product_name')).toContainText('Set here');
      await expect(layers.getByTestId('branding-layer-row-docs_url')).toContainText('Product default');

      await checkA11y(page);
    } finally {
      await writeValues(page, original);
    }
  });
});

adminTest('J38b: the server refusal lands beside the field it names', async ({ page }, testInfo) => {
  const project = testInfo.project.name;
  await withPlatformFlagLock(async () => {
    const original = await readValues(page);
    try {
      await openBranding(page);
      await page.getByTestId('branding-field-product_name').fill(probeName(project));
      await page.getByTestId('branding-field-brand_hue').fill('not a colour');
      await page.getByTestId('branding-save').click();

      // The toast carries the server's sentence, and so does the field.
      const toast = page.getByTestId('branding-toast-error');
      await expect(toast).toContainText('"brand_hue"', { timeout: 20_000 });
      await expect(page.locator('#branding-brand_hue-helper-text')).toContainText('six-digit hex');

      // Nothing was written: the refusal is a refusal, not a partial save.
      const stored = await readValues(page);
      expect(stored['product_name']).toBe(original['product_name']);
      expect(stored['brand_hue']).toBe(original['brand_hue']);

      await checkA11y(page);
    } finally {
      await writeValues(page, original);
    }
  });
});

/**
 * J38d — the branding package round trip (ADR-0024 WP9).
 *
 * The page's two package buttons are on screen and pass axe; the proof of the
 * feature is over `page.request`, because a download and a multipart import
 * are what the routes ARE: the export answers a zip with a filename, and the
 * same bytes posted back with `dry_run=true` are accepted with `ok: true` and
 * change nothing. No platform lock: the dry run writes nothing, and the export
 * of whatever brand is current is a valid package whichever journey is
 * mid-flight.
 */
adminTest('J38d: the exported branding package imports back clean as a dry run', async ({ page }) => {
  await openBranding(page);
  await expect(page.getByTestId('branding-package-download')).toBeVisible();
  await expect(page.getByTestId('branding-package-import')).toBeVisible();
  await expect(page.getByTestId('branding-package-versions')).toBeVisible();
  await checkA11y(page);

  const exported = await page.request.get(`${API}/admin/branding/package/administration`);
  expect(exported.status(), 'the export must be authorised for the admin persona').toBe(200);
  expect(exported.headers()['content-type']).toContain('application/zip');
  expect(exported.headers()['content-disposition']).toMatch(/attachment; filename=".+-branding\.zip"/);
  const zip = await exported.body();
  expect(zip.length).toBeGreaterThan(0);
  // A zip starts with the local-file-header signature.
  expect(zip.subarray(0, 2).toString('latin1')).toBe('PK');

  const checked = await page.request.post(`${API}/admin/branding/package/administration?dry_run=true`, {
    multipart: { file: { name: 'roundtrip-branding.zip', mimeType: 'application/zip', buffer: zip } },
  });
  expect(checked.status(), `the dry run answered ${checked.status()}`).toBe(200);
  const report = (await checked.json()) as {
    ok: boolean;
    dry_run: boolean;
    applied: boolean;
    problems: unknown[];
    manifest?: { product: string };
  };
  expect(report.ok).toBe(true);
  expect(report.dry_run).toBe(true);
  expect(report.applied).toBe(false);
  expect(report.problems).toEqual([]);
  expect(report.manifest?.product).toBeTruthy();

  // Nothing changed: the dry run kept no version, so the list is unchanged in kind.
  const versions = await page.request.get(`${API}/admin/branding/package/administration/versions`);
  expect(versions.status()).toBe(200);
  expect(Array.isArray(((await versions.json()) as { versions: unknown[] }).versions)).toBe(true);
});

/* ── J38e: the second pack, on every surface ───────────────────────────── */

/** A 1×1 transparent PNG — the smallest raster the e-mail logo slot accepts (it refuses SVG). */
const PNG_1X1 = Buffer.from(
  'iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAYAAAAfFcSJAAAADUlEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg==',
  'base64',
);

/**
 * Bytes that carry the WOFF2 signature and nothing a font engine can use. The
 * upload path sniffs the signature, and the journey asserts the `@font-face`
 * rule the shell writes for it — not that a glyph renders. A real font is
 * 20–300 KiB of binary this repository does not commit.
 */
const FAKE_WOFF2 = Buffer.concat([Buffer.from('wOF2', 'latin1'), Buffer.alloc(64)]);

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/** Uploads one asset through the API and returns the content-addressed path the server answers. */
async function uploadAsset(
  page: Page,
  kind: string,
  name: string,
  mimeType: string,
  buffer: Buffer,
): Promise<string> {
  const response = await page.request.post(`${API}/admin/branding/assets/${kind}`, {
    multipart: { file: { name, mimeType, buffer } },
  });
  const text = await response.text();
  expect(response.status(), `uploading ${kind} answered ${response.status()}: ${text}`).toBe(200);
  const asset = JSON.parse(text) as { path: string };
  expect(asset.path).toContain(`/api/v2/branding/assets/${kind}/`);
  return asset.path;
}

/** The primary colour the theme resolved: one of the `--el-*` variables MUI declares on the root. */
/**
 * Every branding key set to "inherit" — the row set the E2E stack seeds and the
 * visual baselines photograph. Derived from a read so it follows the schema
 * (a new key is inherited too) rather than a hand-kept list.
 */
function inheritValues(values: Record<string, unknown>): Record<string, unknown> {
  const out: Record<string, unknown> = {};
  for (const [key, value] of Object.entries(values)) {
    if (Array.isArray(value)) out[key] = [];
    else if (typeof value === 'number') out[key] = 0;
    else if (value !== null && typeof value === 'object') out[key] = {};
    else out[key] = '';
  }
  return out;
}

/**
 * Restores the rows through a request context of its own, not the page's. A
 * journey that hits its timeout loses its page mid-`finally`
 * (`page.request` then fails with "Target page, context or browser has been
 * closed"), the restore never lands, and every retry starts from the branded
 * state the failure left — so the retry's "before" equals its "after" and it
 * fails for a reason unrelated to the first failure. Measured on #797's
 * webkit shard: three attempts, all "primary did not move".
 */
async function restoreValues(values: Record<string, unknown>): Promise<void> {
  const api = await apiRequest.newContext({ baseURL: BASE_URL, storageState: STORAGE_STATE.admin });
  try {
    const response = await api.put(BRANDING_URL, { data: { values } });
    expect(response.status(), `restoring the branding rows answered ${response.status()}`).toBe(200);
  } finally {
    await api.dispose();
  }
}

async function readPrimary(page: Page): Promise<string> {
  return page.evaluate(() =>
    getComputedStyle(document.documentElement).getPropertyValue('--el-palette-primary-main').trim(),
  );
}

adminTest(
  'J38e: a second pack reaches every surface at once — shell, admin console, sub-application, login, e-mail',
  async ({ page }, testInfo) => {
    // Seven page loads, four uploads and a zip on webkit do not fit the 30 s
    // default; a timeout here is what poisoned the retries (see restoreValues).
    adminTest.setTimeout(150_000);
    const project = testInfo.project.name;
    // Distinct from J38's values, so a row either journey left behind cannot
    // satisfy the other.
    const name = `${probeName(project)} pack`;
    const hue = probeHue(project);
    const docsUrl = `https://docs.example.com/${project}`;
    const supportEmail = `support-${project}@example.com`;
    const fontFamily = `E2E ${project} Sans`;

    await withPlatformFlagLock(async () => {
      // RETRY SAFETY. The baseline this journey measures against is the
      // all-inherit row set (bootstrap.js serves its inert body). A retry
      // after an attempt whose restore did not land would read the branded
      // rows as "original", so the baseline is taken from the served pack,
      // not from whatever the rows hold: if a pack is being served, the rows
      // are put back to inherit first. That is the stack's seeded state, so
      // nothing is lost on it; a deployment with a pack of its own is not
      // where this suite runs.
      let original = await readValues(page);
      const served = await page.request.get(`${API}/branding/bootstrap.js`);
      if (!(await served.text()).includes('no deployment brand pack configured')) {
        original = inheritValues(original);
        await restoreValues(original);
      }
      try {
        // The un-branded shell first: what the pack must move away from.
        await page.goto(BASE_URL + '/app/', { waitUntil: 'domcontentloaded' });
        await expect(page.getByTestId('brand-logo-mark').first()).toBeVisible({ timeout: 20_000 });
        const primaryBefore = await readPrimary(page);
        expect(primaryBefore, 'the theme declares --el-palette-primary-main on the root').not.toBe('');

        // Four assets, four kinds, through the API (J38 covers the page's
        // file input). Each answers the same-origin path the rows then carry.
        const logoMark = await uploadAsset(page, 'logo-mark', `${project}-mark.svg`, 'image/svg+xml', probeLogo(project));
        const favicon = await uploadAsset(page, 'favicon', `${project}-favicon.svg`, 'image/svg+xml', probeLogo(project));
        const logoEmail = await uploadAsset(page, 'logo-email', `${project}-email.png`, 'image/png', PNG_1X1);
        const font = await uploadAsset(page, 'font', `${project}.woff2`, 'font/woff2', FAKE_WOFF2);

        // One save carries the whole pack: identity, hue, links, assets, font.
        await writeValues(page, {
          ...original,
          product_name: name,
          brand_hue: hue,
          docs_url: docsUrl,
          support_email: supportEmail,
          logo_mark: logoMark,
          favicon,
          logo_email: logoEmail,
          font_faces: [{ family: fontFamily, url: font }],
          font_family: `"${fontFamily}", sans-serif`,
        });

        // ── Surface 1: the application shell ──────────────────────────────
        // Channel C end to end: rows → resolver → bootstrap.js → the global →
        // the theme and the document head. Each assertion is a different
        // consumer of the pack.
        await page.goto(BASE_URL + '/app/', { waitUntil: 'domcontentloaded' });
        // `PageTitleSetter` suffixes every title with the product name.
        await expect(page).toHaveTitle(new RegExp(`${escapeRegExp(name)}$`), { timeout: 20_000 });
        // A new hue drops the stated scheme tokens server-side, so the client
        // derives every palette variable from it: primary must move.
        await expect.poll(() => readPrimary(page), { timeout: 10_000 }).not.toBe(primaryBefore);
        // `BrandLogo` renders an <img> for a custom slot (the compiled SVG otherwise).
        const mark = page.getByTestId('brand-logo-mark').first();
        await expect(mark).toHaveAttribute('src', logoMark);
        await expect(mark).toHaveAttribute('alt', name);
        // `BrandDocumentHead`: the favicon link repointed, the @font-face written.
        await expect(page.locator('link[rel="icon"]')).toHaveAttribute('href', favicon);
        await expect
          .poll(() => page.locator('head style[data-el-fonts]').evaluate((element) => element.textContent ?? ''), {
            timeout: 10_000,
          })
          .toContain(font);

        // ── Surface 1b: the admin console ─────────────────────────────────
        // A second SPA with its own `index.html` and bootstrap tag (WP4); its
        // nav header renders the same logo slot, so the pack must reach it
        // through its own document, not the app's.
        await page.goto(BASE_URL + '/admin/app/users', { waitUntil: 'domcontentloaded' });
        const adminMark = page.getByTestId('admin-nav').getByTestId('brand-logo-mark').first();
        await expect(adminMark).toBeVisible({ timeout: 20_000 });
        await expect(adminMark).toHaveAttribute('src', logoMark);
        await expect(adminMark).toHaveAttribute('alt', name);
        await expect.poll(() => readPrimary(page), { timeout: 10_000 }).not.toBe(primaryBefore);

        // ── Surface 2: a sub-application screen ───────────────────────────
        // ADR-0024 decision 8: the App Catalog's documentation link comes from
        // the pack's docs origin through `docsLink()`, never from a literal.
        await page.goto(BASE_URL + '/app/apps/catalog', { waitUntil: 'domcontentloaded' });
        const docs = page.getByRole('link', { name: /Documentation/ }).first();
        await expect(docs).toBeVisible({ timeout: 20_000 });
        await expect(docs).toHaveAttribute('href', new RegExp(`^${escapeRegExp(docsUrl)}/`));
        await expect(page).toHaveTitle(new RegExp(`${escapeRegExp(name)}$`));
        await checkA11y(page);

        // ── Surfaces 3 and 4: the login page and the e-mails ──────────────
        // Neither is reachable in a browser on this stack (OIDC owns
        // /forward-auth, and there is no relay), so they are read the way an
        // operator reviews them: the branding package renders both through
        // the production renderers (`browserauth.RenderLoginPreview`, the
        // mail composer) under the pack that is live right now.
        const exported = await page.request.get(`${API}/admin/branding/package/administration`);
        expect(exported.status(), 'the export must be authorised for the admin persona').toBe(200);
        const zip = await JSZip.loadAsync(await exported.body());

        const login = await zip.file('preview/login.html')?.async('string');
        expect(login, 'preview/login.html is in the package').toBeTruthy();
        expect(login).toContain(`<title>${name} login</title>`);
        // The hue reaches the sign-in button as a hash-pinned inline style.
        expect(login).toContain(`.sign-in-button{background:${hue};`);
        // No full logo was uploaded, so the page falls back to the name.
        expect(login).toContain(`<p class="brand-name">${name}</p>`);
        expect(login).toContain(`href="${favicon}"`);

        const invitation = await zip.file('preview/email-invitation.html')?.async('string');
        expect(invitation, 'preview/email-invitation.html is in the package').toBeTruthy();
        expect(invitation).toContain(name);
        // The hue colours the header name (no e-mail logo without a public
        // origin — see below) and the support link; the action button carries
        // it as a background only when the invitation has a link to offer.
        expect(invitation).toMatch(new RegExp(`(?:background|color):${escapeRegExp(hue)};`));
        expect(invitation).toContain(`mailto:${supportEmail}`);
        // The uploaded raster is the e-mail logo whenever the deployment states
        // its public origin (the composer needs an absolute URL); without one
        // the header names the product instead. Both are the composer's
        // documented behaviour, so the assertion follows the deployment.
        if (invitation?.includes('<img')) {
          expect(invitation).toContain(logoEmail);
          expect(invitation).toContain(`alt="${name}"`);
        }

        // The send path answers truthfully for what it can do. A stack
        // without a relay refuses with 503 and says so; one with a relay sends
        // and names the recipient. Either way nothing pretends.
        const sent = await page.request.post(`${API}/admin/branding/test_email/administration`, {
          data: { to: supportEmail },
        });
        const sentBody = (await sent.json()) as { sent?: boolean; to?: string; error?: string };
        if (sent.status() === 503) {
          expect(sentBody.error).toContain('not configured');
        } else {
          expect(sent.status(), `the test e-mail answered ${sent.status()}: ${JSON.stringify(sentBody)}`).toBe(200);
          expect(sentBody).toMatchObject({ sent: true, to: supportEmail });
        }
      } finally {
        await restoreValues(original);
      }
    });

    // The restore reached the served pack too, not only the rows.
    const after = await page.request.get(`${API}/branding/bootstrap.js`);
    expect(await after.text()).not.toContain(name);
  },
);

adminTest('J38c: the Configuration page points at the Branding page instead of a plain form', async ({ page }) => {
  const response = await page.goto(BASE_URL + '/admin/app/configuration', { waitUntil: 'domcontentloaded' });
  expect(response?.status()).toBeLessThan(400);
  const sections = page.getByRole('navigation', { name: 'Configuration sections' });
  await sections.getByRole('button', { name: /Branding/ }).click();
  await expect(page.getByTestId('admin-configuration-branding-card')).toBeVisible({ timeout: 20_000 });
  // No generic Save over the same rows.
  await expect(page.getByRole('button', { name: 'Save' })).toHaveCount(0);
  await page.getByTestId('admin-configuration-branding-link').click();
  await page.waitForURL((url) => url.pathname === '/admin/app/branding', { timeout: 20_000 });
  await expect(page.getByRole('heading', { name: 'Branding' })).toBeVisible({ timeout: 20_000 });
  await checkA11y(page);
});
