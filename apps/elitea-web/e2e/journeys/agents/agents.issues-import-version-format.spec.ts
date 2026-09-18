/**
 * elitea_issues package I1-agents — #5192 "Handle Flexible Version Format
 * When Importing Agents". The filed report's literal example
 * (`{"name": "My Agent", "version": 1.0}` / `null` rejected as "not a
 * string") targets a field this app's import wizard does not have: the
 * per-version identifier here is `versions[].name`, decoded in
 * `ExportImportPost` (`internal/api/v2/eliteacore/handler.go`) as a loose
 * `map[string]any` with a comma-ok `.(string)` assertion on every field,
 * INCLUDING that one — a JSON number or `null` fails the assertion silently
 * and falls back to a default (`"latest"`) rather than erroring the whole
 * import. Re-judged: DEFECT-CHECK, NOT reproduced — there is no rigid
 * string-typed field for a non-string JSON value to fail against. This
 * journey pins that: importing an agent whose version carries a NUMERIC
 * `name` (mirroring the issue's own `1.0` example) and a stray top-level
 * `version: null` key succeeds rather than failing the import.
 */
import { test, expect } from '@playwright/test';

import {
  API_BASE,
  AUTOTEST_PREFIX,
  DEFAULT_PROJECT_ID,
  deleteAgent,
  readAgentList,
} from '../../fixtures/api';

const SUFFIX = '-importverfmt';

function uniqueName(tag: string): string {
  return `${AUTOTEST_PREFIX}${tag}-${Date.now()}${SUFFIX}`;
}

/* onetest: elitea_issues #5192 — an import whose version carries a non-string (numeric) name, and a stray top-level `version: null`, still succeeds */
test('J-import-version-format: importing a version with a numeric name and a null `version` key succeeds', async ({
  request,
}) => {
  const name = uniqueName('agent');

  const importResponse = await request.post(`${API_BASE}/elitea_core/import_wizard/prompt_lib/${DEFAULT_PROJECT_ID}`, {
    data: [
      {
        entity: 'agents',
        name,
        description: `${AUTOTEST_PREFIX}flexible version format fixture`,
        // The issue's own literal example — a non-string, and a null — sent
        // as an extra top-level key this schema never reads. It must be
        // ignored, not crash the decode.
        version: null,
        import_uuid: `${name}-uuid`,
        versions: [
          {
            // The issue's own example value (`1.0`) as a genuine JSON
            // number, not a string — this app's per-version identifier.
            name: 1.0,
            import_version_uuid: `${name}-version-uuid`,
            instructions: 'You are a helpful assistant.',
            agent_type: 'openai',
            llm_settings: { model_name: 'E2E-MOCK-MODEL' },
            meta: {},
            tools: [],
            variables: [],
            conversation_starters: [],
            welcome_message: 'Hello!',
          },
        ],
      },
    ],
  });

  expect(
    [200, 201, 207],
    `the import must not fail the whole batch over a non-string version field: ${importResponse.status()} ${(await importResponse.text()).slice(0, 300)}`,
  ).toContain(importResponse.status());

  let createdId: string | undefined;
  try {
    await expect
      .poll(
        async () => {
          const { rows } = await readAgentList(request, { query: name });
          const match = rows.find((row) => row.name === name);
          createdId = match?.id !== undefined ? String(match.id) : undefined;
          return createdId !== undefined;
        },
        { timeout: 20_000 },
      )
      .toBe(true);
  } finally {
    if (createdId !== undefined) await deleteAgent(request, createdId);
  }
});
