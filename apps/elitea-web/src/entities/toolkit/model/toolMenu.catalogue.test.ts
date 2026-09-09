import { describe, expect, it } from 'vitest';

import type { ToolkitTypeSchemaMap } from './types';
import { toolkitTypeMenuEntries } from './toolMenu';
import servedCatalogue from './__fixtures__/servedToolkitTypeCatalogue.json';

/*
 * The chooser against the REAL served catalogue.
 *
 * The other toolMenu tests drive four hand-written schemas. They prove the
 * filter rules and cannot prove the result: the server used to serve eight
 * types with no `metadata` at all, so a suite of invented schemas passed while
 * the create page offered seven tiles against the reference deployment's sixty.
 *
 * This fixture is the metadata `GET /elitea_core/toolkits/prompt_lib/{id}`
 * actually answers with, copied from the served catalogue. A Go test
 * (services/elitea-main/internal/api/v2/toolkits/type_catalogue_test.go,
 * TestTheWebChooserFixtureMatchesTheServedCatalogue) compares the two, so the
 * fixture cannot quietly drift into a second source of truth. Regenerate it
 * from the server; do not edit it to make a test pass.
 */
const catalogue = servedCatalogue as unknown as ToolkitTypeSchemaMap;

describe('toolkitTypeMenuEntries over the served catalogue', () => {
  it('offers the reference deployment’s toolkit tiles', () => {
    const keys = toolkitTypeMenuEntries(catalogue).map((entry) => entry.key);

    // The types every product journey depends on.
    for (const key of ['github', 'jira', 'confluence', 'artifact', 'custom', 'openapi']) {
      expect(keys).toContain(key);
    }
    // Types this app could not offer at all before the SDK catalogue was served.
    for (const key of ['ado_repos', 'bitbucket', 'gitlab_org', 'sharepoint', 'qtest', 'zephyr_scale', 'pptx', 'figma']) {
      expect(keys).toContain(key);
    }
    // Seven tiles became many. The floor is stated so that a catalogue that
    // silently shrinks back fails here.
    expect(keys.length).toBeGreaterThanOrEqual(30);
  });

  it('drops every type the server marks hidden', () => {
    const keys = toolkitTypeMenuEntries(catalogue).map((entry) => entry.key);

    // Hidden by the SDK's own metadata, exactly as the reference deployment
    // hides them.
    for (const key of ['aws', 'azure', 'elastic', 'keycloak', 'vectorstore', 'zephyr', 'data_analysis']) {
      expect(keys).not.toContain(key);
    }
    // Hidden by the worker capability projection: the admitted Python image
    // cannot import this, so a tile would fail at the first tool call.
    // slack, rally, service_now and google_places used to be on this list too;
    // #869 added their measured SDK dependencies to the worker image.
    for (const key of ['kubernetes']) {
      expect(keys).not.toContain(key);
    }
    // Hidden because it carries no label: mcp_config is the container the
    // pre-built MCP servers are declared in and is never created directly.
    expect(keys).not.toContain('mcp_config');
    // Excluded by the agent/application rule, not by metadata.
    expect(keys).not.toContain('application');
    // Excluded because its categories name it an internal tool.
    expect(keys).not.toContain('sandbox');
  });

  it('gives every offered tile a non-empty accessible name', () => {
    for (const entry of toolkitTypeMenuEntries(catalogue)) {
      expect(entry.label.trim()).not.toBe('');
    }
  });

  it('lets the server name a type this app has never heard of', () => {
    const entries = toolkitTypeMenuEntries(catalogue);
    const byKey = new Map(entries.map((entry) => [entry.key, entry]));

    // No ToolTypes override exists for these, so the label is the server's.
    expect(byKey.get('figma')?.label).toBe('Figma');
    expect(byKey.get('pptx')?.label).toBe('PPTX');
    expect(byKey.get('salesforce')?.label).toBe('Salesforce');
    expect(byKey.get('figma')?.hasKnownLabel).toBe(true);

    // And where an override exists it still wins, so the four working forms
    // keep the names this app has always shown.
    expect(byKey.get('github')?.label).toBe('GitHub');
    expect(byKey.get('sharepoint')?.label).toBe('SharePoint');
  });

  it('offers only application-typed entries in the application chooser', () => {
    // No served type declares metadata.application, so the application chooser
    // is empty rather than showing every toolkit.
    expect(toolkitTypeMenuEntries(catalogue, { isApplication: true })).toEqual([]);
  });
});
