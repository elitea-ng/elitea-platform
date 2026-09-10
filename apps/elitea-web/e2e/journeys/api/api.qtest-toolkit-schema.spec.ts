/**
 * The QTest toolkit's SERVED schema — the contract every schema-driven form
 * in this app renders from (`ToolBase`/`IndexConfig`-style forms all read
 * `args_schemas` off `GET /elitea_core/toolkits/prompt_lib/{project}`, the
 * same catalogue `admin.guardrails.spec.ts`'s `readCatalogue`/`toolsOf`
 * already read for a different toolkit type).
 *
 * Ported by use case from the w1-test-case-version-id package
 * (ELITEA-2861, ELITEA-2862). The other five cases in that package
 * (ELITEA-2856/2857/2858/2859/2860) need a REAL QTest connection — a real
 * Test Run id, a real Test Case id, and a server that actually calls
 * qTest's API to verify the write landed — and are recorded LIVE-ONLY.
 * ELITEA-2859 specifically ("the Status tooltip lists only Passed / Failed /
 * Blocked") is additionally recorded NA: the served schema's `status` field
 * is a free-text string with NO enum at all, and its own description names
 * NINE standard values ('Passed', 'Failed', 'Skipped', 'Blocked', 'Broken',
 * 'No Result', 'Pending', 'Unknown', 'Incomplete') — 'Unexecuted' is not
 * among them, so the five-value dropdown the manual case describes does not
 * exist to test against.
 *
 * What IS answerable without a live QTest connection is the CONTRACT this
 * platform actually serves for the two read-only tools ELITEA-2861 and
 * ELITEA-2862 are about — `testcase_version_id`'s numeric-id guidance and
 * its pointer to `get_test_case_versions`, and that lookup tool's own
 * required parameter — which is exactly what a schema-driven form in this
 * app renders its field description/tooltip and required-marker FROM.
 */
import { expect, test } from '@playwright/test';

import { API_BASE, DEFAULT_PROJECT_ID } from '../../fixtures/api';

interface JsonSchemaProperty {
  readonly type?: string;
  readonly title?: string;
  readonly description?: string;
  readonly anyOf?: readonly { readonly type?: string }[];
}

interface ToolArgsSchema {
  readonly properties?: Record<string, JsonSchemaProperty>;
  readonly required?: readonly string[];
}

interface ServedToolkitType {
  readonly properties?: {
    readonly selected_tools?: { readonly args_schemas?: Record<string, ToolArgsSchema> };
  };
}

async function readCatalogue(page: import('@playwright/test').Page): Promise<Record<string, ServedToolkitType>> {
  const response = await page.request.get(`${API_BASE}/elitea_core/toolkits/prompt_lib/${DEFAULT_PROJECT_ID}`);
  expect(response.status(), await response.text()).toBe(200);
  return (await response.json()) as Record<string, ServedToolkitType>;
}

function argsSchemas(catalogue: Record<string, ServedToolkitType>, type: string): Record<string, ToolArgsSchema> {
  return catalogue[type]?.properties?.selected_tools?.args_schemas ?? {};
}

/** `anyOf: [{type}, {type: "null"}]` -> the real (non-null) declared type. */
function nonNullType(property: JsonSchemaProperty | undefined): string | undefined {
  if (property === undefined) return undefined;
  if (property.type !== undefined) return property.type;
  return property.anyOf?.find((entry) => entry.type !== 'null')?.type;
}

test('this deployment serves the qtest toolkit type', async ({ page }) => {
  const catalogue = await readCatalogue(page);
  expect(
    Object.keys(catalogue),
    'these cases are about the qtest toolkit; nothing below means anything if this deployment does not serve it',
  ).toContain('qtest');
});

test('ELITEA-2861: testcase_version_id is optional, numeric, and points to the lookup tool', async ({ page }) => {
  const catalogue = await readCatalogue(page);
  const schemas = argsSchemas(catalogue, 'qtest');
  const updateStatus = schemas['update_test_run_status'];
  expect(updateStatus, 'the qtest schema must offer update_test_run_status').toBeTruthy();

  const versionId = updateStatus?.properties?.['testcase_version_id'];
  expect(versionId, 'update_test_run_status must declare testcase_version_id').toBeTruthy();

  // Optional: not in `required`, and nullable in its type.
  expect(updateStatus?.required ?? []).not.toContain('testcase_version_id');
  expect(nonNullType(versionId)).toBe('integer');

  // The description is what a schema-driven form in this app renders as the
  // field's tooltip — it must say the value is NUMERIC and point at the
  // lookup tool, the two claims ELITEA-2861 makes about the tooltip.
  expect(versionId?.description ?? '').toMatch(/numeric/i);
  expect(versionId?.description ?? '').toMatch(/get_test_case_versions/i);

  // And the required fields the same tool declares — a caller with only a
  // test_run_id and a status must be able to omit it (backward compatibility,
  // ELITEA-2856's own claim, provable here without a live call: the schema
  // does not demand it).
  expect(updateStatus?.required ?? []).toEqual(expect.arrayContaining(['test_run_id', 'status']));
});

test('ELITEA-2862: get_test_case_versions is offered, with test_case_id required', async ({ page }) => {
  const catalogue = await readCatalogue(page);
  const schemas = argsSchemas(catalogue, 'qtest');
  const lookup = schemas['get_test_case_versions'];
  expect(lookup, 'the qtest schema must offer get_test_case_versions').toBeTruthy();

  expect(lookup?.required ?? []).toContain('test_case_id');
  expect(nonNullType(lookup?.properties?.['test_case_id'])).toBe('string');

  // version_name is the optional filter — present, but not required.
  expect(lookup?.properties?.['version_name']).toBeTruthy();
  expect(lookup?.required ?? []).not.toContain('version_name');
});
