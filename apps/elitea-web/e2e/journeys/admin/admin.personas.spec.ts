/**
 * Journey: every built-in role persona is signed in and reads its console
 * capability document (ADR-0059 R3.13a, R3.1a).
 *
 * The personas are seeded by `scripts/e2e-stack.sh seed` ("built-in role
 * personas") and signed in by `e2e/auth.setup.ts`, one storage state each
 * (`STORAGE_STATE.personas`). This smoke test is what proves the seed and the
 * setup did their part before any persona journey relies on them: each
 * state is a live session, `GET /api/v2/admin/console` answers 200, and the
 * document names exactly the persona's built-in role. The areas each role
 * opens are the server's golden tests (internal/api/v2/adminaccess/testdata);
 * here only the shape a role cannot share with another is asserted: the
 * emergency persona opens no area, every other persona opens one at least.
 *
 * The second half (ADR-0059 decision 6, R3.2, issue 2532) opens the console
 * in a browser as each persona: the navigation shows exactly the persona's
 * labelled areas and items, and a typed URL to an item its role does not open
 * shows the access-denied page for that item. `AdminNav.test.tsx` renders the
 * same table from the server's golden documents; this is the running stack's
 * answer.
 *
 * Read-only: it changes nothing, so both engines run it in parallel.
 */
import { test, expect, request as playwrightRequest } from '@playwright/test';

import { ADMIN_PERSONAS, type BuiltInAdminRole } from '../../fixtures/adminPersonas';
import { API_BASE } from '../../fixtures/api';
import { BASE_URL, STORAGE_STATE } from '../../../playwright.config';

interface ConsoleDocument {
  areas: { id: string; items: { id: string; level: 'read' | 'write' }[] }[];
  roles: { name: string; built_in: boolean }[];
  elevation: unknown;
}

for (const persona of ADMIN_PERSONAS) {
  test(`the ${persona.role} persona reads its console capability document`, async () => {
    const context = await playwrightRequest.newContext({
      baseURL: BASE_URL,
      storageState: STORAGE_STATE.personas[persona.role],
    });
    try {
      const info = await context.get(`${BASE_URL}/auth/info`);
      expect(await info.json(), `${persona.email} must be signed in`).toMatchObject({ authenticated: true });

      const response = await context.get(`${API_BASE}/admin/console`);
      expect(response.status(), await response.text()).toBe(200);
      const document = (await response.json()) as ConsoleDocument;

      expect(document.roles, `${persona.email} holds exactly its built-in role`).toEqual([
        expect.objectContaining({ name: persona.role, built_in: true }),
      ]);
      expect(document.elevation).toBeNull();
      if (persona.role === 'emergency_access') {
        // Its one name, access.breakglass.request, opens the Break-glass
        // item, whose page has not shipped.
        expect(document.areas).toEqual([]);
      } else {
        expect(document.areas.length, `${persona.role} opens at least one area`).toBeGreaterThan(0);
      }
    } finally {
      await context.dispose();
    }
  });
}

/** Every item the web table places, by area: what a global or platform administrator sees. */
const EVERY_AREA: Readonly<Record<string, readonly string[]>> = {
  people_access: ['users', 'access', 'identity-providers', 'scim'],
  projects: ['projects', 'app-requests'],
  models_spend: ['llm-governance', 'budgets-alerts'],
  agents_governance: [
    'agent-registry',
    'connector-requests',
    'delegation-review',
    'toolkits',
    'credential-inventory',
    'permission-trimming',
    'kill-switch',
  ],
  security: ['audit-trail', 'data-retention'],
  platform: [
    'configuration',
    'features',
    'branding',
    'email',
    'schedules-tasks',
    'service-descriptors',
    'secrets',
    'client-applications',
  ],
};

/**
 * Each persona's areas and items in the navigation. The gateway tabs are
 * still tabs of Configuration, which stands for them. SCIM and Access are
 * pages of their own since R3.6 (issue 2537); the security auditor opens
 * Access at read through access.roles.view.
 */
const PERSONA_AREAS: Readonly<Record<BuiltInAdminRole, Readonly<Record<string, readonly string[]>>>> = {
  super_admin: EVERY_AREA,
  admin: EVERY_AREA,
  llm_manager: { projects: ['projects'], models_spend: ['llm-governance', 'budgets-alerts'], platform: ['configuration'] },
  project_administrator: {
    people_access: ['scim'],
    projects: ['projects', 'app-requests'],
    models_spend: ['budgets-alerts'],
  },
  security_auditor: { people_access: ['access'], security: ['audit-trail'] },
  emergency_access: {},
};

for (const persona of ADMIN_PERSONAS) {
  test(`the ${persona.role} persona's navigation shows exactly its areas`, async ({ browser }) => {
    const context = await browser.newContext({ storageState: STORAGE_STATE.personas[persona.role] });
    try {
      const page = await context.newPage();
      await page.goto(`${BASE_URL}/admin/app/`, { waitUntil: 'domcontentloaded' });
      const nav = page.getByTestId('admin-nav');
      await expect(nav).toBeVisible({ timeout: 30_000 });

      const expected = PERSONA_AREAS[persona.role];
      const expectedLinks = Object.values(expected).flat();
      await expect(nav.getByRole('link')).toHaveCount(expectedLinks.length, { timeout: 20_000 });
      const rendered = await nav.locator('section[data-testid^="admin-nav-area-"]').evaluateAll((sections) =>
        Object.fromEntries(
          sections.map((section) => [
            (section.getAttribute('data-testid') ?? '').replace('admin-nav-area-', ''),
            [...section.querySelectorAll('a[data-testid^="admin-nav-item-"]')].map((anchor) =>
              (anchor.getAttribute('data-testid') ?? '').replace('admin-nav-item-', ''),
            ),
          ]),
        ),
      );
      expect(rendered).toEqual(expected);

      // One refusal by typed URL, for every role that Users is not open to.
      if (persona.role === 'super_admin' || persona.role === 'admin') return;
      await page.goto(`${BASE_URL}/admin/app/users`, { waitUntil: 'domcontentloaded' });
      const denied = page.getByTestId('admin-item-denied');
      await expect(denied).toHaveAttribute('data-item-id', 'users', { timeout: 20_000 });
      await expect(page.getByTestId('admin-item-denied-roles')).toContainText('Platform administrator', {
        timeout: 20_000,
      });
    } finally {
      await context.close();
    }
  });
}
