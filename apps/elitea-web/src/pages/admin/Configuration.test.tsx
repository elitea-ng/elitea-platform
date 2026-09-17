/**
 * Rendering and behaviour tests for `pages/admin/Configuration.tsx` (unit A14).
 *
 * The bar is not "the page renders". Almost every section of this page is one
 * this deployment cannot serve, and the failure this unit exists to remove is a
 * form that looks live and saves into a void — so each test asserts one of:
 *
 *  - the REQUEST a control produced (a save that sends the whole section, or the
 *    wrong section's keys, looks identical on screen to one that does not);
 *  - that a section with no backend states a REASON instead of rendering a form;
 *  - that the reason shown is the SERVER's, not one the page invented.
 */
import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminConfiguration } from './Configuration';
import { isFieldVisible, widgetFor } from './ConfigurationSectionForm';
import { linkUrlError, toConfigLinks, withoutBlankLinks } from './ConfigurationLinksEditor';
import { renderAdminRoute } from './__tests__/testRouter';

interface RecordedRequest {
  readonly method: string;
  readonly url: string;
  readonly body: unknown;
}

let recorded: RecordedRequest[] = [];

const PYLON_REASON =
  'these settings configure Pylon plugin runtimes: the reference page collects them from plugin heartbeats';

const SECTIONS = [
  {
    id: 'guardrails',
    title: 'Guardrails',
    unavailable_reason: PYLON_REASON,
    fields: [{ key: 'blocked_toolkits', type: 'array', title: 'Blocked Toolkits' }],
  },
  {
    id: 'resources',
    title: 'Resources',
    description: 'Configure resource cards displayed on the Help Center.',
    fields: [
      {
        key: 'resources_documentation_enabled',
        type: 'boolean',
        title: 'Documentation Card Enabled',
        default: true,
      },
      {
        key: 'resources_documentation_title',
        type: 'string',
        title: 'Documentation Card Title',
        default: 'Documentation',
      },
      {
        key: 'resources_documentation_links',
        type: 'array',
        title: 'Documentation Links',
        default: [],
      },
    ],
  },
  {
    // A section that is STILL unavailable. It used to be `advanced`, which is
    // no longer declared at all: its subject is Pylon plugin loading, which the
    // target architecture removes on purpose, so the row was removed rather
    // than left saying "not available here" forever. `runtime` followed
    // `advanced` out the door for the same reason — see config_schemas.go's
    // "Observability, Runtime and Admin Panel are GONE too" note — so this
    // fixture now stands in with `llm_proxy`, a section that genuinely stays
    // withheld (its data is three unrelated shapes a flat form cannot express).
    id: 'llm_proxy',
    title: 'LLM Proxy',
    unavailable_reason: 'these settings configure Pylon plugin runtimes',
    fields: [],
  },
];

const STORED_VALUES: Record<string, unknown> = {
  resources_documentation_enabled: true,
  resources_documentation_title: 'Handbook',
  resources_documentation_links: [{ title: 'Start here', url: 'https://docs.example.com/start' }],
};

let saveResponse: () => Response = () => HttpResponse.json({ saved: true, values: STORED_VALUES });

function useConfigHandlers(): void {
  server.use(
    http.get('*/admin/plugin_config_schemas/administration', ({ request }) => {
      recorded.push({ method: 'GET-schemas', url: request.url, body: null });
      return HttpResponse.json({ sections: SECTIONS });
    }),
    http.get('*/admin/plugin_config_values/administration/:section', ({ request, params }) => {
      recorded.push({ method: 'GET-values', url: request.url, body: null });
      // Mirrors the server: an unavailable section answers 501 with its reason,
      // never 200 with defaults.
      const section = SECTIONS.find((entry) => entry.id === params.section);
      if (section?.unavailable_reason !== undefined) {
        return HttpResponse.json({ error: section.unavailable_reason }, { status: 501 });
      }
      return HttpResponse.json({ values: STORED_VALUES });
    }),
    http.put('*/admin/plugin_config_values/administration/:section', async ({ request }) => {
      recorded.push({ method: 'PUT', url: request.url, body: await request.json() });
      return saveResponse();
    }),
  );
}

function writes(): RecordedRequest[] {
  return recorded.filter((entry) => entry.method === 'PUT');
}

async function waitForResources(): Promise<void> {
  await screen.findByRole('textbox', { name: 'Documentation Card Title' });
}

beforeEach(() => {
  recorded = [];
  saveResponse = () => HttpResponse.json({ saved: true, values: STORED_VALUES });
  configureGeneratedClient({ baseUrl: '/api/v2' });
  useConfigHandlers();
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminConfiguration — which section opens', () => {
  it('opens on the first AVAILABLE section, not the first one in schema order', async () => {
    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();

    // `guardrails` is first in the list and unavailable. Opening on it would
    // show a refusal notice as the page's front door.
    expect(screen.queryByTestId('admin-configuration-unavailable')).not.toBeInTheDocument();
    expect(screen.getByText('Configure resource cards displayed on the Help Center.')).toBeInTheDocument();
  });

  it('never fetches values for a section the server declared unavailable', async () => {
    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();
    await userEvent.click(screen.getByRole('button', { name: /Guardrails/ }));

    await screen.findByTestId('admin-configuration-unavailable');
    // Asking for values it has already been told it cannot have would put a
    // load error on top of an explanation.
    const guardrailReads = recorded.filter(
      (entry) => entry.method === 'GET-values' && entry.url.includes('/guardrails'),
    );
    expect(guardrailReads).toHaveLength(0);
  });

  it('marks unavailable sections in the sidebar before they are opened', async () => {
    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();
    expect(screen.getAllByText('Not available here')).toHaveLength(2);
  });
});

describe('AdminConfiguration — the unavailable sections', () => {
  it("shows the SERVER's reason rather than a form or an empty pane", async () => {
    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();
    await userEvent.click(screen.getByRole('button', { name: /Guardrails/ }));

    const notice = await screen.findByTestId('admin-configuration-unavailable');
    expect(notice).toHaveTextContent('Pylon plugin runtimes');
    // No control from that section is drawn — a disabled field would still read
    // as "this is configurable, just not right now".
    expect(screen.queryByRole('textbox', { name: 'Blocked Toolkits' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
  });

  it('offers no save control at all on an unavailable section', async () => {
    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();
    await userEvent.click(screen.getByRole('button', { name: /LLM Proxy/ }));

    await screen.findByTestId('admin-configuration-unavailable');
    expect(screen.queryByRole('button', { name: 'Save' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Discard' })).not.toBeInTheDocument();
  });
});

describe('AdminConfiguration — the save', () => {
  it('sends ONLY the fields the operator changed', async () => {
    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();

    const title = screen.getByRole('textbox', { name: 'Documentation Card Title' });
    await userEvent.clear(title);
    await userEvent.type(title, 'Platform Handbook');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    const body = writes()[0]?.body as { values: Record<string, unknown> };
    expect(body.values).toEqual({ resources_documentation_title: 'Platform Handbook' });
    // Re-asserting untouched fields is how a save of one card makes an unrelated
    // stored value the server would now refuse fail the whole request.
    expect(Object.keys(body.values)).toHaveLength(1);
    expect(writes()[0]?.url).toContain('/administration/resources');
  });

  it('is disabled until something changes, and again after a successful save', async () => {
    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();

    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();

    await userEvent.type(
      screen.getByRole('textbox', { name: 'Documentation Card Title' }),
      '!',
    );
    expect(screen.getByRole('button', { name: 'Save' })).toBeEnabled();

    await userEvent.click(screen.getByRole('button', { name: 'Save' }));
    await screen.findByTestId('admin-configuration-saved');
    await waitFor(() => {
      expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
    });
  });

  it('drops blank link rows rather than sending them', async () => {
    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();

    await userEvent.click(screen.getByRole('button', { name: /Add link/ }));
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => {
      expect(writes()).toHaveLength(1);
    });
    const body = writes()[0]?.body as { values: Record<string, unknown> };
    // The row the operator added and left empty is not a link, and the server
    // would have to decide what an untitled, URL-less entry means.
    expect(body.values.resources_documentation_links).toEqual([
      { title: 'Start here', url: 'https://docs.example.com/start' },
    ]);
  });

  it("renders the server's refusal sentence, not a generic failure", async () => {
    saveResponse = () =>
      HttpResponse.json(
        {
          error:
            '"resources_documentation_links"[0] url must use http or https; "javascript" links are refused',
        },
        { status: 400 },
      );

    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();
    await userEvent.type(screen.getByRole('textbox', { name: 'Documentation Card Title' }), '!');
    await userEvent.click(screen.getByRole('button', { name: 'Save' }));

    const alert = await screen.findByTestId('admin-configuration-error');
    // Every refusal from this endpoint names an input the operator can act on.
    // Collapsing them into "Failed to save" throws that away.
    expect(alert).toHaveTextContent('must use http or https');
    expect(alert).not.toHaveTextContent('Failed to save this section.');
  });

  it('discards edits when the operator switches section, rather than carrying them across', async () => {
    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();

    await userEvent.type(screen.getByRole('textbox', { name: 'Documentation Card Title' }), 'XYZ');
    await userEvent.click(screen.getByRole('button', { name: /Guardrails/ }));
    await screen.findByTestId('admin-configuration-unavailable');
    await userEvent.click(screen.getByRole('button', { name: /Resources/ }));
    await waitForResources();

    // Carrying them would post another section's keys, which the server refuses
    // as unknown — an accurate refusal for a request nobody meant to make.
    expect(screen.getByRole('textbox', { name: 'Documentation Card Title' })).toHaveValue('Handbook');
    expect(screen.getByRole('button', { name: 'Save' })).toBeDisabled();
  });

  it('restores the server value on Discard', async () => {
    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();

    await userEvent.clear(screen.getByRole('textbox', { name: 'Documentation Card Title' }));
    await userEvent.type(screen.getByRole('textbox', { name: 'Documentation Card Title' }), 'Draft');
    await userEvent.click(screen.getByRole('button', { name: 'Discard' }));

    expect(screen.getByRole('textbox', { name: 'Documentation Card Title' })).toHaveValue('Handbook');
    expect(writes()).toHaveLength(0);
  });
});

describe('AdminConfiguration — the links editor', () => {
  it('shows the stored links and warns on a scheme the server will refuse', async () => {
    renderAdminRoute(<AdminConfiguration />);
    await waitForResources();

    const url = screen.getByRole('textbox', { name: 'URL' });
    expect(url).toHaveValue('https://docs.example.com/start');

    await userEvent.clear(url);
    await userEvent.type(url, 'javascript:alert(1)');
    // The client warning is a courtesy; the server refuses it regardless (see
    // J34c). It is asserted because a page that accepted it silently would send
    // the operator round a save round-trip to find out.
    expect(await screen.findByText(/Only http:\/\/ and https:\/\/ links/)).toBeInTheDocument();
  });
});

describe('link helpers', () => {
  it('accepts http and https and nothing else', () => {
    expect(linkUrlError('https://example.com')).toBeUndefined();
    expect(linkUrlError('http://wiki.internal.corp')).toBeUndefined();
    // The scheme is compared case-INSENSITIVELY, and that cuts both ways: a
    // pasted `HTTPS://` must not be flagged, and `JAVASCRIPT:` must still be.
    expect(linkUrlError('HTTPS://example.com')).toBeUndefined();
    expect(linkUrlError('  https://example.com  ')).toBeUndefined();
    expect(linkUrlError('')).toBeUndefined();
    expect(linkUrlError('javascript:alert(1)')).toBeDefined();
    expect(linkUrlError('JAVASCRIPT:alert(1)')).toBeDefined();
    expect(linkUrlError('data:text/html,<script>')).toBeDefined();
    expect(linkUrlError('file:///etc/passwd')).toBeDefined();
  });

  it('treats a schemeless value as incomplete rather than valid', () => {
    expect(linkUrlError('docs.example.com')).toBeDefined();
  });

  it('drops only entirely blank rows', () => {
    expect(
      withoutBlankLinks([
        { title: '', url: '' },
        { title: 'Title only', url: '' },
        { title: '', url: 'https://example.com' },
      ]),
    ).toEqual([
      { title: 'Title only', url: '' },
      { title: '', url: 'https://example.com' },
    ]);
  });

  it('reads anything the store might hold without throwing', () => {
    expect(toConfigLinks(undefined)).toEqual([]);
    expect(toConfigLinks('nope')).toEqual([]);
    expect(toConfigLinks([null, { title: 1, url: 2 }, { title: 'a', url: 'b' }])).toEqual([
      { title: '', url: '' },
      { title: '', url: '' },
      { title: 'a', url: 'b' },
    ]);
  });
});

describe('field widgets', () => {
  it('routes a *_links field to the links editor whatever its declared type', () => {
    expect(widgetFor({ key: 'resources_x_links', type: 'array', title: 'L' })).toBe('links');
  });

  it('routes an enum string to a select and a textarea format to multiline', () => {
    expect(widgetFor({ key: 'k', type: 'string', title: 'T', enum: ['a', 'b'] })).toBe('select');
    expect(widgetFor({ key: 'k', type: 'string', title: 'T', format: 'textarea' })).toBe('multiline');
    expect(widgetFor({ key: 'k', type: 'string', title: 'T' })).toBe('text');
  });

  it('has NO widget for a shape it cannot edit honestly', () => {
    // The reference falls back to a raw JSON editor here, so a field whose
    // editor was never written still looks editable.
    expect(widgetFor({ key: 'blocked_tools', type: 'object', title: 'T' })).toBe('none');
    expect(widgetFor({ key: 'form_users', type: 'array', title: 'T' })).toBe('none');
  });

  it('A1 (ELITEA-0016): routes BOTH publish-guardrail whitelist fields to the project picker, matched on suffix like `_links` is', () => {
    expect(
      widgetFor({ key: 'publish_whitelist_project_ids', type: 'array', items: { type: 'integer' }, title: 'T' }),
    ).toBe('projectList');
    expect(
      widgetFor({ key: 'skill_publish_whitelist_project_ids', type: 'array', items: { type: 'integer' }, title: 'T' }),
    ).toBe('projectList');
    // A field that merely CONTAINS the phrase, or an unrelated integer array, is unaffected.
    expect(widgetFor({ key: 'agent_categories', type: 'array', items: { type: 'string' }, title: 'T' })).toBe('list');
  });

  it('honours visible_when, including the all-of array form', () => {
    const field = { key: 'k', type: 'string', title: 'T' };
    expect(isFieldVisible(field, {})).toBe(true);
    expect(isFieldVisible({ ...field, visible_when: { field: 'a', value: true } }, { a: true })).toBe(true);
    expect(isFieldVisible({ ...field, visible_when: { field: 'a', value: true } }, { a: false })).toBe(false);
    expect(
      isFieldVisible(
        { ...field, visible_when: [{ field: 'a', value: true }, { field: 'b', value: 'x' }] },
        { a: true, b: 'x' },
      ),
    ).toBe(true);
    expect(
      isFieldVisible(
        { ...field, visible_when: [{ field: 'a', value: true }, { field: 'b', value: 'x' }] },
        { a: true, b: 'y' },
      ),
    ).toBe(false);
  });
});
