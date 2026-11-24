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
 * Read-only: it changes nothing, so both engines run it in parallel.
 */
import { test, expect, request as playwrightRequest } from '@playwright/test';

import { ADMIN_PERSONAS } from '../../fixtures/adminPersonas';
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
