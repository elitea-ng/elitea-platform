/*
 * ONE CASE PER SERVED TOOLKIT TYPE.
 *
 * The form renderer is generic, so the 126 test files under features/toolkits
 * drive it with two or three hand-written schemas and reach a high percentage
 * without saying anything about whether any given TYPE renders. That is the
 * [[unit-suite-blind-to-composition-root]] gap in its usual place: both halves
 * are correct in isolation and nothing joins them.
 *
 * The table here is the SERVED CATALOGUE, imported from a fixture a Go test
 * writes out of the real handler
 * (services/elitea-main/internal/api/v2/toolkits/web_fixture_test.go). A type
 * added to the pinned SDK snapshot therefore joins this suite with no edit to
 * this file — and a type the fixture does not carry fails the Go test rather
 * than quietly not being covered here.
 *
 * The first test in this file is the gate that makes that true: the settings
 * fixture and the chooser fixture must name the same types. Everything after it
 * is per-type behaviour.
 *
 * Do NOT edit servedToolkitTypeSettings.json by hand. Regenerate it:
 *   ELITEA_WRITE_TOOLKIT_WEB_FIXTURES=1 go test ./internal/api/v2/toolkits/ \
 *     -run TestTheWebPerTypeSettingsFixture
 */
import { waitFor } from '@testing-library/react';
import { HttpResponse, http } from 'msw';
import { beforeAll, beforeEach, describe, expect, it, vi } from 'vitest';

import { toolkitTypeMenuEntries } from '@/entities/toolkit';
import servedCatalogue from '@/entities/toolkit/model/__fixtures__/servedToolkitTypeCatalogue.json';
import servedSettings from '@/entities/toolkit/model/__fixtures__/servedToolkitTypeSettings.json';
import { configureGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { resolveIndexesTabVisibility } from '../lib/helpers/indexesTabVisibility';
import { adjustLabel, isSecretField } from '../lib/helpers/toolBase.helpers';
import { resolveFieldKind } from '../ui/form/ToolBase/ToolBaseProperty.kinds';
import { getToolComponent } from '../lib/helpers/toolComponent.helpers';
import { ToolBase } from '../ui/form/ToolBase/ToolBase';
import type { EditToolDetail, ToolPropertySchema, ToolSchema } from '../ui/form/ToolBase/types';

import { renderWithRouterSocketAndProject } from './testUtils';

interface ServedTypeMetadata {
  readonly label?: string;
  readonly hidden?: boolean;
  readonly categories?: readonly string[];
  readonly unavailable_reason?: string;
}

interface ServedTypeSettings extends ToolSchema {
  readonly metadata?: ServedTypeMetadata;
  readonly name_required?: boolean;
}

const settingsByType = servedSettings as unknown as Readonly<Record<string, ServedTypeSettings>>;
const catalogueByType = servedCatalogue as unknown as Readonly<
  Record<string, { readonly metadata?: ServedTypeMetadata }>
>;

const servedTypes = Object.keys(settingsByType).sort();

/**
 * The types the create page actually offers a tile for.
 *
 * Derived by the APP'S OWN rule — `toolkitTypeMenuEntries`, the function the
 * chooser calls — rather than by a copy of it here. A copy is how a test comes
 * to agree with itself instead of with the product: an earlier draft of this
 * file used "labelled and not hidden", which offered `sandbox`, and the E2E run
 * proved there is no such tile (its `categories` carry `internal_tool`).
 *
 * The `mcp` key is dropped for the same reason the hook drops it: the non-MCP
 * chooser filters MCP types out before it ever calls the entry builder.
 */
const offeredTypes = toolkitTypeMenuEntries(settingsByType as never)
  .map((entry) => entry.key)
  .filter((type) => type.toLowerCase() !== 'mcp' && !type.toLowerCase().endsWith('mcp'))
  .sort();

/** The types whose schema offers `index_data` — the Indexes tab's discriminator. */
const indexingTypes = servedTypes.filter((type) => {
  const selectedTools = settingsByType[type]?.properties?.selected_tools;
  const args = selectedTools?.args_schemas;
  return args !== undefined && Object.hasOwn(args, 'index_data');
});

beforeAll(() => {
  class StubResizeObserver {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  }
  window.ResizeObserver = StubResizeObserver;
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

beforeEach(() => {
  server.use(
    http.get('/api/v2/elitea_core/platform_settings/prompt_lib', () =>
      HttpResponse.json({
        chat_enabled: true,
        applications_enabled: true,
        skills_enabled: true,
        toolkits_enabled: true,
        datasources_enabled: true,
        pipelines_enabled: true,
        publishing_enabled: true,
        moderation_enabled: true,
        support_chat_enabled: true,
        mcp_enabled: true,
      }),
    ),
    http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json(settingsByType)),
  );
});

/** Everything ToolBase needs beyond the schema, with no per-type variation. */
function toolBaseProps(type: string): {
  readonly editToolDetail: EditToolDetail;
  readonly setEditToolDetail: () => void;
  readonly editField: () => void;
  readonly toolErrors: Record<string, never>;
  readonly setToolErrors: () => void;
  readonly showValidation: boolean;
} {
  return {
    editToolDetail: { name: `autotest ${type}`, description: '', settings: {}, type },
    setEditToolDetail: vi.fn(),
    editField: vi.fn(),
    toolErrors: {},
    setToolErrors: vi.fn(),
    showValidation: false,
  };
}

/**
 * The three field kinds ToolBase does NOT draw itself.
 *
 * Each is delegated to a caller-supplied slot — the credential picker, the
 * model select and the OpenAPI spec editor all live outside this slice — and
 * with no slot supplied the renderer draws nothing at all. They are excluded
 * from the label assertion for that reason, and the exclusion is keyed on the
 * form's OWN `resolveFieldKind` rather than on a list of property names, so a
 * property that stops being slot-delegated is asserted from then on.
 */
const SLOT_DELEGATED_KINDS = new Set(['openapiSpec', 'credentialLike', 'selectedTools']);

/** The property names ToolBase renders a labelled control for. */
function labelledProperties(schema: ServedTypeSettings): readonly string[] {
  const properties = schema.properties ?? {};
  return Object.keys(properties).filter((key) => {
    const property = properties[key];
    if (property === undefined) return false;
    // A `$ref` property is a saved-credential reference: the served schema
    // points it at the type's `$defs` block and the form resolves it to the
    // `configuration` kind, which is slot-delegated too.
    if (Object.hasOwn(property, '$ref')) return false;
    const isSecret = isSecretField(key, property.format, property.secret, property);
    return !SLOT_DELEGATED_KINDS.has(resolveFieldKind({ key, schema: property, isSecret }));
  });
}

/**
 * The label the form draws for one property.
 *
 * `ToolBaseProperty` uses `adjustLabel(schema.title || key)`, so a property
 * with no `title` — every property of the four hand-written elitea_core types —
 * renders a humanised key, not the raw key. Reimplementing that rule here would
 * make this test agree with itself rather than with the form, so the form's own
 * helper is called.
 */
function labelOf(key: string, property: ToolPropertySchema | undefined): string {
  return adjustLabel(property?.title !== undefined && property.title !== '' ? property.title : key);
}

describe('the served toolkit catalogue', () => {
  // ── the gate ─────────────────────────────────────────────────────────────
  it('carries a settings fixture for every type the chooser fixture names', () => {
    const chooserTypes = Object.keys(catalogueByType).sort();
    const missing = chooserTypes.filter((type) => settingsByType[type] === undefined);
    const extra = servedTypes.filter((type) => catalogueByType[type] === undefined);

    expect(
      missing,
      'these served toolkit types have no settings fixture, so no per-type test below covers them.'
        + ' Regenerate with ELITEA_WRITE_TOOLKIT_WEB_FIXTURES=1.',
    ).toEqual([]);
    expect(extra, 'these types are in the settings fixture but not in the chooser fixture').toEqual([]);
    // A catalogue that collapsed back to the eight hand-written types would
    // otherwise satisfy every per-type test below by having eight types.
    expect(servedTypes.length).toBeGreaterThanOrEqual(52);
  });

  it('offers a tile for every type the chooser rule admits, and withholds the rest', () => {
    // Enough tiles that the chooser is a catalogue rather than the eight
    // hand-written types.
    expect(offeredTypes.length).toBeGreaterThanOrEqual(30);

    // Every withheld type says WHY. A type hidden with no reason is
    // indistinguishable from one the platform never had, which is the whole
    // point of serving it hidden rather than dropping it.
    const withheld = servedTypes.filter((type) => settingsByType[type]?.metadata?.hidden === true);
    expect(withheld.length).toBeGreaterThan(0);

    // The three exclusions the chooser rule makes beyond `hidden`, each pinned
    // by the type that exercises it: an internal tool, the agent type, and the
    // MCP container the non-MCP chooser filters out.
    expect(offeredTypes, 'sandbox is an internal_tool').not.toContain('sandbox');
    expect(offeredTypes, 'application is the agent type').not.toContain('application');
    expect(offeredTypes, 'mcp belongs to the MCP tab').not.toContain('mcp');
  });

  // ── per-type: the form ───────────────────────────────────────────────────
  describe.each(offeredTypes.map((type) => [type] as const))('%s', (type) => {
    const schema = settingsByType[type] as ServedTypeSettings;

    it('resolves to a form component and renders every labelled settings field', async () => {
      // getToolComponent picks ToolBase only for a schema with a truthy
      // `.type`; a schema that lost it falls back to ToolCustom's JSON editor,
      // which is what the create page showed while the endpoint was broken.
      expect(getToolComponent(type, schema as never)).toBeDefined();

      const { getAllByText, container } = renderWithRouterSocketAndProject(
        <ToolBase {...toolBaseProps(type)} schema={schema} />,
        'proj-1',
      );
      // The form MOUNTED — an input of its own, not a heading. Queried by
      // element rather than by a MUI class: R-T6 forbids reaching for an
      // internal MUI selector, and a plain `input, textarea` is what every
      // field this test then looks for is made of.
      await waitFor(() => expect(container.querySelector('input, textarea')).not.toBeNull());

      for (const key of labelledProperties(schema)) {
        const label = labelOf(key, schema.properties?.[key]);
        // getAllByText, not getByText: a section-grouped schema renders the
        // same label in the section heading and in the field.
        expect(
          getAllByText(label, { exact: false }).length,
          `${type}: the form does not render a control for ${key} (${label})`,
        ).toBeGreaterThan(0);
      }
    });

  });

  /*
   * The two rules that need no render, over EVERY served type — the thirty-seven
   * with a tile and the nineteen served hidden. A withheld type has no tile to
   * render, but its schema still decides whether the Indexes tab appears and
   * still declares which of its fields are secret, and both are read the moment
   * a deployment un-hides it.
   */
  describe.each(servedTypes.map((type) => [type] as const))('%s (schema rules)', (type) => {
    const schema = settingsByType[type] as ServedTypeSettings;

    it('offers the Indexes tab exactly when its schema offers index_data', () => {
      const visibility = resolveIndexesTabVisibility({
        isMCP: false,
        toolkitTypeSchema: schema,
        selectedTools: ['index_data'],
      });
      expect(
        visibility.hidden,
        `${type}: the Indexes tab visibility disagrees with the served args_schemas`,
      ).toBe(!indexingTypes.includes(type));
    });

    it('never offers the Indexes tab on the MCP route', () => {
      // The negative half of the pair — an MCP screen must not show the tab
      // whatever the type's schema says.
      expect(
        resolveIndexesTabVisibility({ isMCP: true, toolkitTypeSchema: schema, selectedTools: ['index_data'] })
          .hidden,
      ).toBe(true);
    });

    it('agrees with the form about which of its fields are secret', () => {
      const properties = schema.properties ?? {};
      for (const key of Object.keys(properties)) {
        const property = properties[key];
        if (property === undefined) continue;
        const declaredSecret =
          property.secret === true
          || property.format === 'password'
          || (property.anyOf ?? []).some((branch) => branch.secret === true || branch.format === 'password');
        if (!declaredSecret) continue;
        expect(
          isSecretField(key, property.format, property.secret, property),
          `${type}.${key} is declared secret in the served schema but isSecretField does not agree,`
            + ' so the form would render it as a plain text input',
        ).toBe(true);
      }
    });
  });

  // ── the index family as a whole ──────────────────────────────────────────
  it('recognises the whole indexing family, not one representative', () => {
    // Sixteen SDK types plus the two elitea_core-native ones. The count is
    // asserted so a fixture regenerated from a catalogue that lost its
    // args_schemas cannot pass the per-type tests above by making every type
    // agree that the tab is hidden.
    expect(indexingTypes.length).toBeGreaterThanOrEqual(16);
    for (const type of ['github', 'confluence', 'jira', 'sharepoint', 'artifact']) {
      expect(indexingTypes, `${type} is an indexing family member`).toContain(type);
    }
    for (const type of ['pptx', 'slack', 'sonar']) {
      expect(indexingTypes, `${type} offers no index_data`).not.toContain(type);
    }
  });
});
