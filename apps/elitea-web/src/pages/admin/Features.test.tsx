/**
 * Rendering and behaviour tests for `pages/admin/Features.tsx` (unit A14, #200).
 *
 * The bar is the same as the Configuration suite's, and for the same reason:
 * this page's failure mode is a switch that looks live and changes nothing, and
 * a switch that changes nothing renders identically to one that works. So every
 * test asserts one of:
 *
 *  - the REQUEST a control produced — a save that sends the whole section, the
 *    wrong section's keys, or an array element of the wrong type looks the same
 *    on screen as one that does not;
 *  - that a section or a FIELD with nothing behind it states a reason instead of
 *    rendering a control;
 *  - that the reason shown is the SERVER's, not one the page invented;
 *  - that this page and Configuration partition the sections rather than each
 *    keeping a hand-maintained list of the other's.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminConfiguration } from './Configuration';
import { AdminFeatures } from './Features';
import {
  fromConfigListRows,
  listRowError,
  toConfigListRows,
  withoutBlankListEntries,
} from './ConfigurationListEditor';
import { widgetFor } from './ConfigurationSectionForm';
import { renderAdminRoute } from './__tests__/testRouter';

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

const VALIDATION_REASON =
  'publish validation in this service is deterministic (name collisions, sub-agent cycles and depth); ' +
  'there is no AI evaluator for custom criteria to reach.';
const SKILL_VALIDATION_REASON =
  'publish validation for skills in this service is deterministic (version name collisions, empty ' +
  'instructions, publish status); there is no AI evaluator for custom criteria to reach.';

/*
 * `SKILL_REASON` AND `WIDGET_REASON` BOTH USED TO STAND HERE, naming the two
 * Features sections the server withheld. Neither survives: #585 built the skill
 * publishing pipeline and #588 built the support assistant, so both sections are
 * live and the server sends no reason for either.
 *
 * THAT LEFT TWO TESTS WITH NOTHING TO ASSERT — the sidebar's "Not available
 * here" marker and the whole "a section with no consumer" case would have
 * passed by looking at an empty set, which is the failure mode this page exists
 * to prevent in the product. So the fixture keeps ONE withheld section as an
 * explicit stand-in. It is not a real section, and it is not pretending to be:
 * what these tests exercise is the page RENDERING a server-declared refusal, not
 * which product feature happens to lack a consumer this week.
 */
const STAND_IN_REASON = 'this section is withheld by the server for a reason only the server knows';

/** The server's own section list, trimmed to what these tests exercise. */
const SECTIONS = [
  // A Configuration section. Present so the partition can be asserted from both
  // ends rather than assumed.
  {
    id: 'guardrails',
    title: 'Guardrails',
    unavailable_reason: 'these settings configure Pylon plugin runtimes',
    fields: [{ key: 'blocked_toolkits', type: 'array', title: 'Blocked Toolkits' }],
  },
  {
    id: 'mcp_configuration',
    page: 'features',
    title: 'MCP Configuration',
    description: 'Control Model Context Protocol exposure across the platform.',
    fields: [
      { key: 'mcp_enabled', type: 'boolean', title: 'Enable MCP', default: true },
      {
        key: 'mcp_in_menu',
        type: 'boolean',
        title: 'Show MCPs in UI',
        default: true,
        visible_when: { field: 'mcp_enabled', value: true },
      },
    ],
  },
  {
    id: 'agent_publishing',
    page: 'features',
    title: 'Agent Publishing',
    fields: [
      { key: 'is_publish_blocked', type: 'boolean', title: 'Block Agent Publishing', default: false },
      {
        key: 'publish_whitelist_project_ids',
        type: 'array',
        items: { type: 'integer' },
        title: 'Publishing Allowed Projects',
        default: [],
        visible_when: { field: 'is_publish_blocked', value: true },
      },
      {
        key: 'agent_categories',
        type: 'array',
        items: { type: 'string' },
        title: 'Agent Categories',
        default: [],
      },
      {
        key: 'publish_validation_rules',
        type: 'string',
        format: 'textarea',
        title: 'Publish Validation Rules',
        default: '',
        unavailable_reason: VALIDATION_REASON,
      },
    ],
  },
  {
    id: 'skill_publishing',
    page: 'features',
    title: 'Skill Publishing',
    fields: [
      {
        key: 'is_skill_publish_blocked',
        type: 'boolean',
        title: 'Block Skill Publishing',
        default: false,
      },
      {
        key: 'skill_publish_whitelist_project_ids',
        type: 'array',
        items: { type: 'integer' },
        title: 'Skill Publishing Allowed Projects',
        default: [],
        visible_when: { field: 'is_skill_publish_blocked', value: true },
      },
      {
        key: 'skill_categories',
        type: 'array',
        items: { type: 'string' },
        title: 'Skill Categories',
        default: [],
      },
      {
        key: 'skill_publish_validation_rules',
        type: 'string',
        format: 'textarea',
        title: 'Skill Publish Validation Rules',
        default: '',
        unavailable_reason: SKILL_VALIDATION_REASON,
      },
    ],
  },
  {
    id: 'resources',
    page: 'features',
    title: 'Help Center',
    fields: [
      { key: 'resources_documentation_title', type: 'string', title: 'Documentation Card Title', default: 'Documentation' },
    ],
  },
  {
    id: 'support_assistant',
    page: 'features',
    title: 'Support Assistant',
    fields: [
      // A BOOLEAN, matching the server. The reference's `vite_elitea_assistant`
      // was a Vite build-time string of "0"/"1"; nothing here is built at build
      // time, so the switch is a switch.
      { key: 'support_assistant_enabled', type: 'boolean', title: 'Assistant Enabled', default: false },
    ],
  },
  {
    // The stand-in described in the header. Last in order, so it cannot
    // interfere with "opens on the first AVAILABLE section".
    id: 'withheld_stand_in',
    page: 'features',
    title: 'Withheld Example',
    unavailable_reason: STAND_IN_REASON,
    fields: [],
  },
  {
    id: 'voice_features',
    page: 'features',
    title: 'Voice Features',
    fields: [
      {
        key: 'vite_voice_features_enabled',
        type: 'boolean',
        title: 'Voice Features Enabled',
        default: true,
      },
      {
        key: 'vite_voice_features_temporarily_disabled',
        type: 'boolean',
        title: 'Disable Voice Controls but Keep Them Visible',
        default: false,
      },
    ],
  },
];

const VALUES: Record<string, Record<string, unknown>> = {
  mcp_configuration: { mcp_enabled: true, mcp_in_menu: true },
  agent_publishing: {
    is_publish_blocked: false,
    publish_whitelist_project_ids: [],
    agent_categories: ['Security Review'],
    publish_validation_rules: 'stored rules nothing runs',
  },
  resources: { resources_documentation_title: 'Handbook' },
  voice_features: {
    vite_voice_features_enabled: true,
    vite_voice_features_temporarily_disabled: false,
  },
};

let saveResponse: () => Response = () => HttpResponse.json({ saved: true, values: {} });

function useFeatureHandlers(): void {
  server.use(
    http.get('*/admin/plugin_config_schemas/administration', () => HttpResponse.json({ sections: SECTIONS })),
    http.get('*/admin/plugin_config_values/administration/:section', ({ request, params }) => {
      recorded.push({ method: 'GET-values', url: request.url, body: null });
      const section = SECTIONS.find((entry) => entry.id === params.section);
      // Mirrors the server: an unavailable section answers 501 with its reason,
      // never 200 with defaults.
      if (section?.unavailable_reason !== undefined) {
        return HttpResponse.json({ error: section.unavailable_reason }, { status: 501 });
      }
      return HttpResponse.json({ values: VALUES[String(params.section)] ?? {} });
    }),
    http.put('*/admin/plugin_config_values/administration/:section', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return saveResponse();
    }),
  );
}

/** A1 (ELITEA-0016): the org-wide listing `ProjectListEditor`'s picker reads — same fixture shape `PlatformModelsPanel.test.tsx`'s own `useProjects` helper uses for the identical endpoint. */
function useAdminProjectsHandler(rows: readonly { id: number; name: string; is_personal?: boolean }[]): void {
  server.use(
    http.get('*/admin/projects/administration', () =>
      HttpResponse.json({
        rows: rows.map((row) => ({
          owner_id: 1,
          owner_name: 'owner',
          admin_names: [],
          status: 'active',
          suspended: false,
          create_success: true,
          is_personal: false,
          ...row,
        })),
        total: rows.length,
        counts: { team: rows.length, personal: 0 },
      }),
    ),
  );
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method === 'PUT');
}

function lastWriteValues(): Record<string, unknown> {
  const body = writes().at(-1)?.body as { values?: Record<string, unknown> } | undefined;
  return body?.values ?? {};
}

async function waitForMcpSection(): Promise<void> {
  await screen.findByRole('switch', { name: 'Enable MCP' });
}

async function openSection(title: string): Promise<void> {
  await userEvent.click(screen.getByRole('button', { name: new RegExp(title) }));
}

beforeEach(() => {
  recorded = [];
  saveResponse = () => HttpResponse.json({ saved: true, values: {} });
  configureGeneratedClient({ baseUrl: '/api/v2' });
  useFeatureHandlers();
});

afterEach(() => {
  resetGeneratedClient();
});

/* ── the partition ─────────────────────────────────────────────────────── */

describe('the two admin schema pages partition the sections', () => {
  it('Features shows only the sections the SERVER placed there', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();

    const nav = screen.getByRole('navigation', { name: 'Feature sections' });
    for (const title of [
      'MCP Configuration',
      'Agent Publishing',
      'Skill Publishing',
      'Help Center',
      'Support Assistant',
      'Voice Features',
    ]) {
      expect(within(nav).getByText(title)).toBeInTheDocument();
    }
    // `guardrails` declares no page and belongs to Configuration.
    expect(within(nav).queryByText('Guardrails')).not.toBeInTheDocument();
  });

  it('Configuration DROPS the sections placed on Features — including resources', async () => {
    renderAdminRoute(<AdminConfiguration />);
    // The nav element renders before the query resolves, so waiting for the
    // nav alone would assert against an empty list and pass for the wrong
    // reason.
    await screen.findByText('Guardrails');
    const nav = screen.getByRole('navigation', { name: 'Configuration sections' });

    expect(within(nav).getByText('Guardrails')).toBeInTheDocument();
    // The entanglement #217 recorded: it rendered `resources` on Configuration
    // and said it should move when Features landed. If this regresses, one
    // section is editable from two pages and the two would disagree about
    // whether it is dirty.
    expect(within(nav).queryByText('Help Center')).not.toBeInTheDocument();
    expect(within(nav).queryByText('MCP Configuration')).not.toBeInTheDocument();
    expect(within(nav).queryByText('Support Assistant')).not.toBeInTheDocument();
  });
});

/* ── which section opens ───────────────────────────────────────────────── */

describe('AdminFeatures — which section opens', () => {
  it('opens on the first AVAILABLE section rather than the first in order', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    expect(screen.queryByTestId('admin-features-unavailable')).not.toBeInTheDocument();
  });

  it('never fetches values for a section the server declared unavailable', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    await openSection('Withheld Example');

    await screen.findByTestId('admin-features-unavailable');
    const reads = recorded.filter(
      (entry) => entry.method === 'GET-values' && entry.url.includes('/withheld_stand_in'),
    );
    expect(reads).toHaveLength(0);
  });

  it('marks the unavailable sections in the sidebar before they are opened', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    // The stand-in, alone. Skill Publishing and Support Assistant were the two
    // real ones and both became live; see the header for why the fixture still
    // carries a withheld section at all.
    expect(screen.getAllByText('Not available here')).toHaveLength(1);
  });

  it('renders the live Skill Publishing form, with its one unavailable field disclosed', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    await openSection('Skill Publishing');

    expect(screen.queryByTestId('admin-features-unavailable')).toBeNull();
    expect(await screen.findByLabelText('Block Skill Publishing')).toBeInTheDocument();
    expect(screen.getByText('Skill Categories')).toBeInTheDocument();
    // The whitelist is hidden until the block switch is on — the same
    // `visible_when` rule the agent section uses.
    expect(screen.queryByText('Skill Publishing Allowed Projects')).toBeNull();
    // The AI-rules field states the server's reason instead of taking input.
    expect(screen.getByText(SKILL_VALIDATION_REASON)).toBeInTheDocument();
  });

  it('renders the live Support Assistant form', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    await openSection('Support Assistant');

    expect(screen.queryByTestId('admin-features-unavailable')).toBeNull();
    // A SWITCH, not a text box: the reference's `vite_elitea_assistant` was a
    // build-time string of "0"/"1".
    expect(await screen.findByLabelText('Assistant Enabled')).toBeInTheDocument();
  });
});

/* ── the sections with nothing behind them ─────────────────────────────── */

describe('AdminFeatures — a section with no consumer', () => {
  it("shows the SERVER's reason and no control at all", async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    await openSection('Withheld Example');

    const notice = await screen.findByTestId('admin-features-unavailable');
    expect(notice).toHaveTextContent(STAND_IN_REASON);
    // A DISABLED control would still read as "configurable, just not now".
    expect(screen.queryByRole('switch')).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Discard' })).not.toBeInTheDocument();
  });
});

/* ── the field with nothing behind it, inside a section that works ─────── */

describe('AdminFeatures — a field the server says cannot be set', () => {
  it('renders it read-only with the server’s reason, alongside working controls', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    await openSection('Agent Publishing');

    const field = await screen.findByTestId('admin-config-field-unavailable-publish_validation_rules');
    const input = within(field).getByRole('textbox');
    expect(input).toBeDisabled();
    // The stored value is shown: the operator can see what the platform holds.
    expect(input).toHaveValue('stored rules nothing runs');
    expect(field).toHaveTextContent('no AI evaluator');

    // And the section is otherwise live — withholding the whole section to
    // disclose one field would have taken three working controls away.
    expect(screen.getByRole('switch', { name: 'Block Agent Publishing' })).toBeEnabled();
    expect(screen.getByRole('button', { name: 'Save' })).toBeInTheDocument();
  });

  it('an unavailable spec never resolves to an editable widget, whatever its type', () => {
    // The branch order is the assertion: `unavailable_reason` is checked before
    // the type, so a later branch cannot hand a refused field a live control.
    for (const type of ['string', 'boolean', 'array', 'integer']) {
      expect(
        widgetFor({ key: 'k_links', type, title: 'T', unavailable_reason: 'because' }),
      ).toBe('unavailable');
    }
    // …and without the reason, the same spec resolves normally.
    expect(widgetFor({ key: 'k_links', type: 'array', title: 'T' })).toBe('links');
  });
});

/* ── the save ──────────────────────────────────────────────────────────── */

describe('AdminFeatures — the save', () => {
  it('sends ONLY the field the operator changed, to the section they are on', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();

    await userEvent.click(screen.getByRole('switch', { name: 'Enable MCP' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(writes()).toHaveLength(1);
    expect(writes()[0]?.url).toContain('/administration/mcp_configuration');
    // The whole section would be `{mcp_enabled, mcp_in_menu}`; a save that
    // re-asserts an untouched field is a save that can fail over one.
    expect(lastWriteValues()).toEqual({ mcp_enabled: false });
  });

  it('does not carry a draft across a section change', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();

    await userEvent.click(screen.getByRole('switch', { name: 'Enable MCP' }));
    await openSection('Help Center');
    await screen.findByRole('textbox', { name: 'Documentation Card Title' });

    // Nothing to save: the other section's edit was dropped, not carried into a
    // body the server would refuse as an unknown key.
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
  });

  it('shows the SERVER’s refusal sentence rather than a generic failure', async () => {
    saveResponse = () =>
      HttpResponse.json(
        { error: '"agent_categories"[0] must be a string' },
        { status: 400 },
      );
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();

    await userEvent.click(screen.getByRole('switch', { name: 'Enable MCP' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    const error = await screen.findByTestId('admin-features-error');
    expect(error).toHaveTextContent('must be a string');
  });
});

/* ── the list editor ───────────────────────────────────────────────────── */

describe('AdminFeatures — the array fields', () => {
  it('adds, edits and saves a string list without the blank row reaching the wire', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    await openSection('Agent Publishing');

    const editor = await screen.findByTestId('admin-config-list-agent_categories');
    await userEvent.click(within(editor).getByRole('button', { name: /Add entry/ }));

    // The blank row SURVIVES on screen — filtering it here would delete the row
    // on the render that created it.
    const rows = within(editor).getAllByRole('textbox');
    expect(rows).toHaveLength(2);

    await userEvent.type(rows[1] as HTMLElement, 'Compliance');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(lastWriteValues()).toEqual({ agent_categories: ['Security Review', 'Compliance'] });
  });

  it('drops a row the operator left blank, rather than sending an empty category', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    await openSection('Agent Publishing');

    const editor = await screen.findByTestId('admin-config-list-agent_categories');
    await userEvent.click(within(editor).getByRole('button', { name: /Add entry/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(lastWriteValues()).toEqual({ agent_categories: ['Security Review'] });
  });

  it('A1 (ELITEA-0016): the whitelist is a project-NAME picker — picking one by name sends its id as a NUMBER on the wire', async () => {
    useAdminProjectsHandler([
      { id: 4, name: 'Marketing Team' },
      { id: 9, name: 'Finance Team' },
    ]);
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    await openSection('Agent Publishing');

    // The whitelist is behind `visible_when`.
    await userEvent.click(screen.getByRole('switch', { name: 'Block Agent Publishing' }));
    const editor = await screen.findByTestId('admin-config-list-publish_whitelist_project_ids');

    // No raw-integer text row anywhere — the reference's own defect this
    // control replaces (a bare id list an operator has to look up elsewhere).
    expect(within(editor).queryByRole('textbox', { name: /Enter a whole number/ })).not.toBeInTheDocument();

    await userEvent.click(within(editor).getByRole('combobox', { name: /Publishing Allowed Projects/ }));
    await userEvent.click(await screen.findByRole('option', { name: 'Marketing Team' }));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    // The NAME was picked; the wire still carries the plain project id, a
    // NUMBER — not the string a hand-typed row could have sent.
    expect(lastWriteValues().publish_whitelist_project_ids).toEqual([4]);
  });

  it('A1 (ELITEA-0016): the Team/Private/Public filter narrows which projects the picker offers', async () => {
    useAdminProjectsHandler([
      { id: 4, name: 'Marketing Team', is_personal: false },
      { id: 7, name: 'project_user_7', is_personal: true },
    ]);
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    await openSection('Agent Publishing');
    await userEvent.click(screen.getByRole('switch', { name: 'Block Agent Publishing' }));
    const editor = await screen.findByTestId('admin-config-list-publish_whitelist_project_ids');

    const combobox = within(editor).getByRole('combobox', { name: /Publishing Allowed Projects/ });
    await userEvent.click(combobox);
    expect(await screen.findByRole('option', { name: 'Marketing Team' })).toBeInTheDocument();
    expect(screen.getByRole('option', { name: 'project_user_7' })).toBeInTheDocument();
    await userEvent.keyboard('{Escape}');

    await userEvent.click(within(editor).getByRole('combobox', { name: 'Show' }));
    await userEvent.click(await screen.findByRole('option', { name: 'Private' }));
    await userEvent.click(combobox);

    expect(await screen.findByRole('option', { name: 'project_user_7' })).toBeInTheDocument();
    expect(screen.queryByRole('option', { name: 'Marketing Team' })).not.toBeInTheDocument();
  });

  it('removes a row', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    await openSection('Agent Publishing');

    const editor = await screen.findByTestId('admin-config-list-agent_categories');
    await userEvent.click(within(editor).getByRole('button', { name: /Remove entry 1/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    expect(lastWriteValues()).toEqual({ agent_categories: [] });
  });

  it('hides a field whose visible_when is unmet', async () => {
    renderAdminRoute(<AdminFeatures />);
    await waitForMcpSection();
    await openSection('Agent Publishing');
    await screen.findByTestId('admin-config-list-agent_categories');

    expect(
      screen.queryByTestId('admin-config-list-publish_whitelist_project_ids'),
    ).not.toBeInTheDocument();
  });
});

/* ── the list helpers, directly ────────────────────────────────────────── */

describe('the list editor helpers', () => {
  it('reads mixed stored shapes into text rows', () => {
    expect(toConfigListRows(['a', 4, { bad: true }, null])).toEqual(['a', '4', '', '']);
    // Anything that is not an array reads as no rows rather than throwing.
    expect(toConfigListRows(undefined)).toEqual([]);
    expect(toConfigListRows('nope')).toEqual([]);
  });

  it('flags only a non-integral integer row, and never a blank one', () => {
    expect(listRowError('', 'integer')).toBeUndefined();
    expect(listRowError('   ', 'integer')).toBeUndefined();
    expect(listRowError('4', 'integer')).toBeUndefined();
    expect(listRowError('-4', 'integer')).toBeUndefined();
    expect(listRowError('4.5', 'integer')).toBeDefined();
    expect(listRowError('12a', 'integer')).toBeDefined();
    // A string list accepts anything, including something that looks numeric.
    expect(listRowError('4.5', 'string')).toBeUndefined();
  });

  it('converts valid integer rows and keeps invalid ones as typed', () => {
    expect(fromConfigListRows(['4', '12a', ''], 'integer')).toEqual([4, '12a', '']);
    expect(fromConfigListRows(['4', 'x'], 'string')).toEqual(['4', 'x']);
  });

  it('drops only blanks on the way out', () => {
    expect(withoutBlankListEntries(['a', '', '  ', 4, 0])).toEqual(['a', 4, 0]);
    // Zero is a legitimate project id and must survive a truthiness filter.
    expect(withoutBlankListEntries([0])).toEqual([0]);
    expect(withoutBlankListEntries('nope')).toEqual([]);
  });
});
