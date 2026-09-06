/**
 * The agent/pipeline lifecycle plane — the REQUESTS, not the buttons.
 *
 * Every route exercised here was generated months ago and had zero callers
 * (`endpoints.manifest.json` recorded `"usedBy": []` for publish, unpublish,
 * publish_validate, fork and import_wizard alike). A control that renders and
 * calls nothing looks exactly like one that works, so every test below asserts
 * the request that reached the server, or the answer the server's own reply
 * produced — never that a button exists.
 */
import type { ReactElement } from 'react';

import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { EntityLifecycleControls } from '../ui/EntityLifecycleControls';
import { EntityImportButton } from '../ui/EntityImportButton';
import { EntityLifecycleMenu } from '../ui/EntityLifecycleMenu';
import { useEntityImport } from '../model/useEntityLifecycle';
import { renderHookWithProviders, renderWithProviders } from './testUtils';

const BASE = '/api/v2';
const PROJECT = '2';

interface Recorded {
  readonly url: string;
  readonly method: string;
  readonly body: unknown;
}

let recorded: Recorded[] = [];

function record(request: Request, body: unknown): void {
  recorded.push({ url: request.url, method: request.method, body });
}

const passValidation = {
  status: 'PASS',
  critical_issues: [],
  warnings: [],
  recommendations: [],
  validation_token: '0123456789abcdef',
};

function controls(overrides: Partial<Parameters<typeof EntityLifecycleControls>[0]> = {}): ReactElement {
  return (
    <EntityLifecycleControls
      entity="agents"
      projectId={PROJECT}
      projects={[
        { id: '2', name: 'Private' },
        { id: '5', name: 'Team' },
      ]}
      entityId="7"
      entityName="Audit Agent"
      tab="all"
      activeVersionId="11"
      activeVersionStatus="draft"
      {...overrides}
    />
  );
}

beforeEach(() => {
  recorded = [];
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.post(`${BASE}/elitea_core/publish_validate/prompt_lib/:projectId/:versionId`, async ({ request }) => {
      record(request, await request.json());
      return HttpResponse.json(passValidation);
    }),
    http.post(`${BASE}/elitea_core/publish/prompt_lib/:projectId/:versionId`, async ({ request }) => {
      record(request, await request.json());
      return HttpResponse.json({
        public_agent_id: '7',
        public_version_id: '12',
        version_name: 'v1',
        catalog_agent_id: '31',
        catalog_version_id: '44',
      });
    }),
    http.post(`${BASE}/elitea_core/unpublish/prompt_lib/:projectId/:versionId`, async ({ request }) => {
      record(request, await request.json().catch(() => null));
      return HttpResponse.json({ status: 'deleted' });
    }),
    http.get(`${BASE}/elitea_core/export_import/prompt_lib/:projectId/:entityId`, ({ request }) => {
      record(request, null);
      return HttpResponse.json({
        ok: true,
        applications: [{ id: '7', name: 'Audit Agent', owner_id: '2', original_exported: true, versions: [] }],
        toolkits: [],
      });
    }),
    http.post(`${BASE}/elitea_core/fork/prompt_lib/:projectId`, async ({ request }) => {
      record(request, await request.json());
      return HttpResponse.json({ result: { agents: [] }, errors: {} }, { status: 201 });
    }),
    http.post(`${BASE}/elitea_core/import_wizard/prompt_lib/:projectId`, async ({ request }) => {
      record(request, await request.json());
      return HttpResponse.json({ result: { agents: [{ id: '9' }] }, errors: {} }, { status: 201 });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

async function openMenu(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(screen.getByTestId('agent-lifecycle-menu-button'));
}

/**
 * Picks a category. It is REQUIRED by the wizard, because the Catalog buckets
 * published agents by `meta.category` and renders no bucket for an agent that
 * has none — a publish without one is published and invisible.
 */
async function chooseCategory(user: ReturnType<typeof userEvent.setup>, name = 'Development'): Promise<void> {
  await user.click(screen.getByRole('combobox'));
  await user.click(await screen.findByRole('option', { name }));
}

describe('publish', () => {
  it('validates first, then publishes with the token the validation returned', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-publish-menuitem'));

    await user.type(screen.getByTestId('publish-version-name'), 'v1');
    await chooseCategory(user);
    await user.click(screen.getByTestId('publish-terms-agree'));
    await user.click(screen.getByRole('button', { name: 'Continue' }));

    // The wizard advances only when the validation ANSWER arrives.
    expect(await screen.findByTestId('publish-validation-status')).toHaveTextContent('PASS');
    await user.click(screen.getByRole('button', { name: 'Publish' }));

    await waitFor(() => expect(recorded.filter((call) => call.url.includes('/publish/'))).toHaveLength(1));
    const validate = recorded.find((call) => call.url.includes('/publish_validate/'));
    const publish = recorded.find((call) => call.url.includes('/publish/'));
    expect(validate?.url).toContain(`/prompt_lib/${PROJECT}/11`);
    expect(validate?.body).toMatchObject({ version_name: 'v1' });
    expect(publish?.body).toMatchObject({ version_name: 'v1', validation_token: '0123456789abcdef' });
  });

  it('sends the chosen category, because the catalogue groups by it', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-publish-menuitem'));
    await user.type(screen.getByTestId('publish-version-name'), 'v1');
    await user.click(screen.getByRole('combobox'));
    await user.click(await screen.findByRole('option', { name: 'Development' }));
    await user.click(screen.getByTestId('publish-terms-agree'));
    await user.click(screen.getByRole('button', { name: 'Continue' }));

    await waitFor(() => expect(recorded.some((call) => call.url.includes('/publish_validate/'))).toBe(true));
    expect(recorded.find((call) => call.url.includes('/publish_validate/'))?.body).toMatchObject({
      category: 'Development',
    });
  });

  // The terms checkbox is the gate production puts on Continue. Without the
  // guard the wizard would validate on the first click regardless.
  it('sends nothing until the terms are accepted', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-publish-menuitem'));
    await user.type(screen.getByTestId('publish-version-name'), 'v1');
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    expect(recorded).toHaveLength(0);
  });

  // The server refuses `v 1` with a 400 and a regex message. Sending it and
  // rendering the refusal is worse than not sending it.
  it('sends nothing when the version name breaks the server rule', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-publish-menuitem'));
    await user.type(screen.getByTestId('publish-version-name'), 'v 1');
    await chooseCategory(user);
    await user.click(screen.getByTestId('publish-terms-agree'));
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    expect(recorded).toHaveLength(0);
  });

  // `publish_validate` answers 422 on FAIL with the SAME body it uses for a
  // pass. Reported as a transport error, the author sees "the request failed"
  // for the one case where it worked and said no.
  it('renders a 422 FAIL as a result and refuses to publish', async () => {
    server.use(
      http.post(`${BASE}/elitea_core/publish_validate/prompt_lib/:projectId/:versionId`, () =>
        HttpResponse.json(
          {
            status: 'FAIL',
            validation_token: null,
            critical_issues: [{ rule: 'generic_name', issue: 'The name is too generic' }],
          },
          { status: 422 },
        ),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-publish-menuitem'));
    await user.type(screen.getByTestId('publish-version-name'), 'v1');
    await chooseCategory(user);
    await user.click(screen.getByTestId('publish-terms-agree'));
    await user.click(screen.getByRole('button', { name: 'Continue' }));

    expect(await screen.findByTestId('publish-validation-status')).toHaveTextContent('FAIL');
    expect(screen.getByText('The name is too generic')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Publish' }));
    expect(recorded.some((call) => call.url.includes('/publish/'))).toBe(false);
  });

  // The deployment-wide guardrail answers 403. Told "the version did not pass
  // validation", the author would edit an agent that is not the problem.
  it('names the guardrail when the server answers 403', async () => {
    server.use(
      http.post(`${BASE}/elitea_core/publish/prompt_lib/:projectId/:versionId`, () =>
        HttpResponse.json({ error: 'publishing is blocked on this deployment' }, { status: 403 }),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-publish-menuitem'));
    await user.type(screen.getByTestId('publish-version-name'), 'v1');
    await chooseCategory(user);
    await user.click(screen.getByTestId('publish-terms-agree'));
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByTestId('publish-validation-status');
    await user.click(screen.getByRole('button', { name: 'Publish' }));

    expect(await screen.findByTestId('lifecycle-error')).toHaveTextContent('publishing is blocked on this deployment');
  });

  // The catalogue ids are the difference between "the Published tab lists it"
  // and "the Catalog serves it" — the split the server change closed.
  it('reports the catalogue when the publish returned catalogue ids', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-publish-menuitem'));
    await user.type(screen.getByTestId('publish-version-name'), 'v1');
    await chooseCategory(user);
    await user.click(screen.getByTestId('publish-terms-agree'));
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByTestId('publish-validation-status');
    await user.click(screen.getByRole('button', { name: 'Publish' }));

    expect(await screen.findByTestId('lifecycle-notice')).toHaveTextContent('published to the Catalog');
  });

  it('does not claim the Catalog when the publish named no catalogue rows', async () => {
    server.use(
      http.post(`${BASE}/elitea_core/publish/prompt_lib/:projectId/:versionId`, () =>
        HttpResponse.json({ public_agent_id: '7', public_version_id: '12', version_name: 'v1' }),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-publish-menuitem'));
    await user.type(screen.getByTestId('publish-version-name'), 'v1');
    await chooseCategory(user);
    await user.click(screen.getByTestId('publish-terms-agree'));
    await user.click(screen.getByRole('button', { name: 'Continue' }));
    await screen.findByTestId('publish-validation-status');
    await user.click(screen.getByRole('button', { name: 'Publish' }));

    const notice = await screen.findByTestId('lifecycle-notice');
    expect(notice).toHaveTextContent('This version is published.');
    expect(notice).not.toHaveTextContent('Catalog');
  });
});

describe('unpublish', () => {
  it('is offered only for a published version, and posts to the version', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls({ activeVersionStatus: 'published' }));
    await openMenu(user);
    expect(screen.queryByTestId('agent-publish-menuitem')).not.toBeInTheDocument();
    await user.click(screen.getByTestId('agent-unpublish-menuitem'));

    await waitFor(() => expect(recorded.some((call) => call.url.includes('/unpublish/'))).toBe(true));
    expect(recorded.find((call) => call.url.includes('/unpublish/'))?.url).toContain(`/prompt_lib/${PROJECT}/11`);
    expect(await screen.findByTestId('lifecycle-notice')).toHaveTextContent('no longer published');
  });

  it('is not offered for a draft version', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    expect(screen.queryByTestId('agent-unpublish-menuitem')).not.toBeInTheDocument();
    expect(screen.getByTestId('agent-publish-menuitem')).toBeInTheDocument();
  });
});

describe('share', () => {
  it('copies a VERSION link and says so', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-share-version-menuitem'));

    expect(await screen.findByTestId('lifecycle-notice')).toHaveTextContent('copied to the clipboard');
    expect(await navigator.clipboard.readText()).toContain('/agents/all/7/11');
  });

  it('copies an ENTITY link with no version segment', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-share-entity-menuitem'));

    await screen.findByTestId('lifecycle-notice');
    const link = await navigator.clipboard.readText();
    expect(link).toContain('/agents/all/7?');
    expect(link).not.toContain('/agents/all/7/11');
  });

  // Share is a link, not a server call. A share that quietly posted somewhere
  // would be inventing a contract the server does not have.
  it('reaches no endpoint at all', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-share-entity-menuitem'));
    await screen.findByTestId('lifecycle-notice');
    expect(recorded).toHaveLength(0);
  });
});

describe('fork', () => {
  // Two calls in order: read the source with `?fork=true`, then write to the
  // TARGET project. Reading without `fork=true` drops the parent pointers;
  // writing to the source project makes a copy where the original already is.
  it('reads the fork document from the source and writes it to the chosen project', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-fork-menuitem'));

    await user.click(screen.getByRole('combobox'));
    await user.click(await screen.findByRole('option', { name: 'Team' }));
    await user.click(screen.getByRole('button', { name: 'Fork' }));

    await waitFor(() => expect(recorded.some((call) => call.url.includes('/fork/'))).toBe(true));
    const read = recorded.find((call) => call.url.includes('/export_import/'));
    const write = recorded.find((call) => call.url.includes('/fork/'));
    expect(read?.url).toContain(`/prompt_lib/${PROJECT}/7`);
    expect(read?.url).toContain('fork=true');
    expect(write?.url).toContain('/fork/prompt_lib/5');
    expect(write?.body).toMatchObject({ applications: [{ id: '7', name: 'Audit Agent' }] });
  });

  it('writes nothing until a target project is chosen', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls({ projects: [{ id: '5', name: 'Team' }], projectId: undefined }));
    await openMenu(user);
    await user.click(screen.getByTestId('agent-fork-menuitem'));
    await user.click(screen.getByRole('button', { name: 'Fork' }));
    expect(recorded.some((call) => call.url.includes('/fork/'))).toBe(false);
  });

  // 207 means part of it forked. Reported as a plain success, the errors
  // channel the server just sent is invisible.
  it('reports a 207 as a partial copy, not a success', async () => {
    server.use(
      http.post(`${BASE}/elitea_core/fork/prompt_lib/:projectId`, () =>
        HttpResponse.json({ result: {}, errors: { agents: [{ msg: 'nope' }] } }, { status: 207 }),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(controls());
    await openMenu(user);
    await user.click(screen.getByTestId('agent-fork-menuitem'));
    await user.click(screen.getByRole('button', { name: 'Fork' }));

    expect(await screen.findByTestId('lifecycle-notice')).toHaveTextContent('part of it was not copied');
  });
});

describe('the pipeline copy of the menu', () => {
  // `Publish` answers 400 `pipeline_not_publishable` for a pipeline. A control
  // whose only possible outcome is a refusal must not be offered.
  it('offers share and fork but never publish', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls({ entity: 'pipelines' }));
    await user.click(screen.getByTestId('pipeline-lifecycle-menu-button'));
    expect(screen.getByTestId('pipeline-fork-menuitem')).toBeInTheDocument();
    expect(screen.getByTestId('pipeline-share-entity-menuitem')).toBeInTheDocument();
    expect(screen.queryByTestId('pipeline-publish-menuitem')).not.toBeInTheDocument();
    expect(screen.queryByTestId('pipeline-unpublish-menuitem')).not.toBeInTheDocument();
  });

  it('builds its share link on the pipelines route', async () => {
    const user = userEvent.setup();
    renderWithProviders(controls({ entity: 'pipelines' }));
    await user.click(screen.getByTestId('pipeline-lifecycle-menu-button'));
    await user.click(screen.getByTestId('pipeline-share-entity-menuitem'));
    await screen.findByTestId('lifecycle-notice');
    expect(await navigator.clipboard.readText()).toContain('/pipelines/all/7');
  });
});

describe('EntityLifecycleMenu on its own', () => {
  it('offers no version share when there is no open version', async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <EntityLifecycleMenu
        testIdPrefix="agent"
        isPublished={false}
        onShareEntity={vi.fn()}
        onFork={vi.fn()}
      />,
    );
    await user.click(screen.getByTestId('agent-lifecycle-menu-button'));
    expect(screen.queryByTestId('agent-share-version-menuitem')).not.toBeInTheDocument();
    expect(screen.getByTestId('agent-share-entity-menuitem')).toBeInTheDocument();
  });

  it('disables the trigger while the entity id is unknown', () => {
    renderWithProviders(
      <EntityLifecycleMenu
        testIdPrefix="agent"
        disabled
        isPublished={false}
        onShareEntity={vi.fn()}
        onFork={vi.fn()}
      />,
    );
    expect(screen.getByTestId('agent-lifecycle-menu-button')).toBeDisabled();
  });
});

describe('import', () => {
  function exportedFile(content: string, name = 'agent.json'): File {
    const file = new File([content], name, { type: 'application/json' });
    Object.defineProperty(file, 'text', { value: () => Promise.resolve(content) });
    return file;
  }

  const document = JSON.stringify({
    ok: true,
    applications: [{ name: 'Audit Agent', versions: [] }],
    toolkits: [{ name: 'GitHub' }],
    skills: [{ name: 'Reviewer' }],
  });

  it('previews the file, then posts the WHOLE document to the import wizard', async () => {
    const user = userEvent.setup();
    const { result } = renderHookWithProviders(() => useEntityImport(PROJECT));
    renderWithProviders(
      <EntityImportButton
        testIdPrefix="agents"
        isImporting={false}
        onImport={async (doc) => {
          await result.current.run.mutateAsync(doc);
        }}
      />,
    );
    await user.upload(screen.getByTestId('agents-import-input'), exportedFile(document));

    // The preview names what will be created before anything is written.
    expect(await screen.findByText('Audit Agent')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Import' }));

    await waitFor(() => expect(recorded.some((call) => call.url.includes('/import_wizard/'))).toBe(true));
    const sent = recorded.find((call) => call.url.includes('/import_wizard/'));
    expect(sent?.url).toContain(`/import_wizard/prompt_lib/${PROJECT}`);
    // The toolkits and skills halves must ride along; a body narrowed to
    // `{applications}` imports an agent with none of its attachments.
    expect(sent?.body).toMatchObject({
      applications: [{ name: 'Audit Agent' }],
      toolkits: [{ name: 'GitHub' }],
      skills: [{ name: 'Reviewer' }],
    });
  });

  it('refuses a file that is not an export and posts nothing', async () => {
    const user = userEvent.setup({ applyAccept: false });
    renderWithProviders(
      <EntityImportButton
        testIdPrefix="agents"
        isImporting={false}
        onImport={vi.fn()}
      />,
    );
    await user.upload(screen.getByTestId('agents-import-input'), exportedFile('{"ok":true}', 'other.json'));
    expect(await screen.findByRole('alert')).toHaveTextContent('holds no agents');
    expect(recorded).toHaveLength(0);
  });

  it('refuses a non-JSON extension before it reads the file', async () => {
    const user = userEvent.setup({ applyAccept: false });
    renderWithProviders(
      <EntityImportButton
        testIdPrefix="agents"
        isImporting={false}
        onImport={vi.fn()}
      />,
    );
    await user.upload(screen.getByTestId('agents-import-input'), exportedFile(document, 'agent.md'));
    expect(await screen.findByRole('alert')).toHaveTextContent('Only .json');
  });
});
