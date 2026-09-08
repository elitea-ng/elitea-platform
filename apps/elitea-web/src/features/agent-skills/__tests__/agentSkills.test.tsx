/**
 * The agent editor's SKILLS section — the requests, not the accordion.
 *
 * The gap this closes is "write-only skills": a skill could be created,
 * versioned, published and exported in this app, and attached to nothing. The
 * failure mode of the fix is a picker that opens, lists skills and writes a row
 * the run-time cannot read, so every test below asserts the ROW the attach
 * produced (the exact body the server keys on) or the list the section shows.
 */
import type { ReactElement } from 'react';

import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AgentSkillsPanel } from '../ui/AgentSkillsPanel';
import { renderWithProviders } from './testUtils';

const BASE = '/api/v2';
const PROJECT = '2';
const VERSION = '11';

interface Recorded {
  readonly url: string;
  readonly method: string;
  readonly body: unknown;
}

let recorded: Recorded[] = [];
let attached: { id: number; name: string; description?: string }[] = [];

const projectSkills = [
  { id: 3, name: 'Reviewer', description: 'reviews code', versions: [{ id: 30, name: 'base' }] },
  { id: 4, name: 'Summariser', versions: [{ id: 40, name: 'base' }] },
  // A skill with no version at all cannot be attached: the server refuses the
  // write, and a row without `skill_version_id` is one the agent run drops.
  { id: 5, name: 'Half-built', versions: [] },
];

beforeEach(() => {
  recorded = [];
  attached = [];
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/application_skills/prompt_lib/:projectId/:appVersionId`, ({ request }) => {
      recorded.push({ url: request.url, method: 'GET', body: null });
      return HttpResponse.json({ items: attached, total: attached.length });
    }),
    http.get(`${BASE}/elitea_core/skills/prompt_lib/:projectId`, ({ request }) => {
      recorded.push({ url: request.url, method: 'GET', body: null });
      const query = new URL(request.url).searchParams.get('query') ?? '';
      const items = projectSkills.filter((skill) => skill.name.toLowerCase().includes(query.toLowerCase()));
      return HttpResponse.json({ items, total: items.length });
    }),
    http.patch(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId`, async ({ request, params }) => {
      const body = (await request.json()) as Record<string, unknown>;
      recorded.push({ url: request.url, method: 'PATCH', body });
      const skillId = Number(params['skillId']);
      if (body['has_relation'] === true) {
        const skill = projectSkills.find((candidate) => candidate.id === skillId);
        attached = [...attached, { id: skillId, name: skill?.name ?? '' }];
        return HttpResponse.json(
          { skill_id: skillId, skill_version_id: body['skill_version_id'], skill_name: skill?.name, version_name: 'base' },
          { status: 201 },
        );
      }
      attached = attached.filter((candidate) => candidate.id !== skillId);
      return HttpResponse.json({ ok: true });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

function panel(props: Partial<Parameters<typeof AgentSkillsPanel>[0]> = {}): ReactElement {
  return (
    <AgentSkillsPanel
      projectId={PROJECT}
      appVersionId={VERSION}
      {...props}
    />
  );
}

describe('AgentSkillsPanel', () => {
  it('reads the skills of THIS agent version, not the project list', async () => {
    attached = [{ id: 3, name: 'Reviewer', description: 'reviews code' }];
    renderWithProviders(panel());

    expect(await screen.findByText('Reviewer')).toBeInTheDocument();
    const read = recorded.find((call) => call.url.includes('/application_skills/'));
    expect(read?.url).toContain(`/application_skills/prompt_lib/${PROJECT}/${VERSION}`);
    // The project-wide list must NOT be fetched to render the section — that
    // is the request #367 fixed on the server side, and asking for it here
    // would put the same wrong list back on the screen.
    expect(recorded.some((call) => call.url.includes('/skills/prompt_lib/'))).toBe(false);
  });

  it('shows the counter production shows', async () => {
    attached = [{ id: 3, name: 'Reviewer' }];
    renderWithProviders(panel());
    await waitFor(() => expect(screen.getByTestId('agent-skills-counter')).toHaveTextContent('1/5 skills added.'));
  });

  // The picker's list is fetched when the MENU opens, which is what production
  // does — the request appears in its network log at that moment and not before.
  it('fetches the picker list only when the menu opens', async () => {
    const user = userEvent.setup();
    renderWithProviders(panel());
    await screen.findByTestId('agent-skills-counter');
    expect(recorded.some((call) => call.url.includes('/skills/prompt_lib/'))).toBe(false);

    await user.click(screen.getByTestId('agent-add-skill-button'));
    await waitFor(() => expect(recorded.some((call) => call.url.includes('/skills/prompt_lib/'))).toBe(true));
    const list = recorded.find((call) => call.url.includes('/skills/prompt_lib/'));
    expect(list?.url).toContain('sort_by=created_at');
    expect(list?.url).toContain('sort_order=desc');
  });

  // The attach body is the whole contract: the server keys the row on
  // (entity_version_id, skill_id, entity_type) and refuses a missing
  // skill_version_id, which is also the column both readers join through.
  it('attaches with the version id, the agent entity type and the agent VERSION id', async () => {
    const user = userEvent.setup();
    renderWithProviders(panel());
    await screen.findByTestId('agent-skills-counter');
    await user.click(screen.getByTestId('agent-add-skill-button'));
    await user.click(await screen.findByTestId('agent-skill-option-3'));

    await waitFor(() => expect(recorded.some((call) => call.method === 'PATCH')).toBe(true));
    const attach = recorded.find((call) => call.method === 'PATCH');
    expect(attach?.url).toContain(`/skill/prompt_lib/${PROJECT}/3`);
    expect(attach?.body).toEqual({
      has_relation: true,
      entity_version_id: 11,
      entity_type: 'agent',
      skill_version_id: 30,
    });
  });

  it('shows the attached skill after the attach, without a reload', async () => {
    const user = userEvent.setup();
    renderWithProviders(panel());
    await screen.findByTestId('agent-skills-counter');
    await user.click(screen.getByTestId('agent-add-skill-button'));
    await user.click(await screen.findByTestId('agent-skill-option-3'));

    await waitFor(() => expect(screen.getByTestId('agent-skills-counter')).toHaveTextContent('1/5'));
    expect(within(screen.getByTestId('agent-attached-skill')).getByText('Reviewer')).toBeInTheDocument();
  });

  it('detaches with the same key and no skill version', async () => {
    attached = [{ id: 3, name: 'Reviewer' }];
    const user = userEvent.setup();
    renderWithProviders(panel());
    await user.click(await screen.findByTestId('agent-detach-skill-3'));

    await waitFor(() => expect(recorded.some((call) => call.method === 'PATCH')).toBe(true));
    expect(recorded.find((call) => call.method === 'PATCH')?.body).toEqual({
      has_relation: false,
      entity_version_id: 11,
      entity_type: 'agent',
    });
  });

  it('offers no attach for a skill that has no version to attach', async () => {
    const user = userEvent.setup();
    renderWithProviders(panel());
    await screen.findByTestId('agent-skills-counter');
    await user.click(screen.getByTestId('agent-add-skill-button'));
    expect(await screen.findByTestId('agent-skill-option-5')).toHaveAttribute('aria-disabled', 'true');
  });

  it('offers no second attach for a skill that is already on this version', async () => {
    attached = [{ id: 3, name: 'Reviewer' }];
    const user = userEvent.setup();
    renderWithProviders(panel());
    await screen.findByTestId('agent-skills-counter');
    await user.click(screen.getByTestId('agent-add-skill-button'));
    expect(await screen.findByTestId('agent-skill-option-3')).toHaveAttribute('aria-disabled', 'true');
    expect(screen.getByTestId('agent-skill-option-4')).not.toHaveAttribute('aria-disabled', 'true');
  });

  // The server caps a version at five skills; a sixth is refused. The counter
  // production renders reads "n/5" for the same reason.
  it('stops offering the add control at the server cap', async () => {
    attached = [1, 2, 3, 4, 5].map((id) => ({ id, name: `Skill ${String(id)}` }));
    renderWithProviders(panel());
    await waitFor(() => expect(screen.getByTestId('agent-skills-counter')).toHaveTextContent('5/5'));
    expect(screen.getByTestId('agent-add-skill-button')).toBeDisabled();
  });

  // `entity_skill_mapping` is keyed by the VERSION id. Before the first save
  // there is no version, so there is no row to write — the section says so
  // rather than collecting a choice it cannot honour.
  it('asks for a save before it offers a picker, and reads nothing', () => {
    renderWithProviders(panel({ appVersionId: undefined }));
    expect(screen.getByText(/Save this agent once/)).toBeInTheDocument();
    expect(screen.queryByTestId('agent-add-skill-button')).not.toBeInTheDocument();
    expect(recorded).toHaveLength(0);
  });

  it('treats an unparseable version id the same way', () => {
    renderWithProviders(panel({ appVersionId: 'not-a-number' }));
    expect(screen.getByText(/Save this agent once/)).toBeInTheDocument();
    expect(recorded).toHaveLength(0);
  });

  it('hides the write controls for a read-only viewer but still lists the skills', async () => {
    attached = [{ id: 3, name: 'Reviewer' }];
    renderWithProviders(panel({ disabled: true }));
    expect(await screen.findByText('Reviewer')).toBeInTheDocument();
    expect(screen.queryByTestId('agent-add-skill-button')).not.toBeInTheDocument();
    expect(screen.queryByTestId('agent-detach-skill-3')).not.toBeInTheDocument();
  });

  it('reports a failed read instead of rendering an empty section', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/application_skills/prompt_lib/:projectId/:appVersionId`, () =>
        HttpResponse.json({ error: 'nope' }, { status: 500 }),
      ),
    );
    renderWithProviders(panel());
    expect(await screen.findByRole('alert')).toHaveTextContent('could not be read');
  });

  it('narrows the picker with the search box', async () => {
    const user = userEvent.setup();
    renderWithProviders(panel());
    await screen.findByTestId('agent-skills-counter');
    await user.click(screen.getByTestId('agent-add-skill-button'));
    await screen.findByTestId('agent-skill-option-3');
    const search = screen.getByTestId('agent-skill-search');
    await user.click(search);
    await user.type(search, 'Summ');
    expect(search).toHaveValue('Summ');

    await waitFor(() => expect(recorded.some((call) => call.url.includes('query=Summ'))).toBe(true));
    await waitFor(() => expect(screen.queryByTestId('agent-skill-option-3')).not.toBeInTheDocument());
    expect(screen.getByTestId('agent-skill-option-4')).toBeInTheDocument();
  });
});
