// "Start from a blueprint" (issue 3137): the gallery renders from the build's
// index, Import goes through the real reader into the shared import dialog,
// and Craft opens Build with AI with the blueprint's brief.
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';
import { renderWithQueryAndTheme } from '@/test/renderWithQueryAndTheme';
import { server } from '@/test/setup';

import { readBlueprintIndex } from '../model/blueprintIndex';
import { BlueprintGalleryButton } from './BlueprintGalleryButton';

const BASE = '/api/v2';
let imported: unknown[] = [];

const INDEX = {
  schema: 'elitea.blueprints/v1',
  agents: [
    {
      slug: 'research-scout',
      name: 'Research Scout',
      description: 'Finds sources',
      file: 'agents/research-scout.agent.md',
      avatar: { default: 'owl' },
      brief: 'A research agent that cites every claim.',
      showcases: ['Citations'],
      requirements: [{ type: 'search', label: 'A web search toolkit' }],
      surface: 'cloud',
    },
  ],
  teams: [
    {
      slug: 'incident-desk',
      name: 'Incident Desk',
      description: 'First response',
      file: 'teams/incident-desk.team.zip',
      lead: 'incident-triage',
      members: [
        { slug: 'comms-drafter', name: 'Comms Drafter', avatar: { default: 'otter' } },
        { slug: 'incident-triage', name: 'Incident Triage', avatar: { default: 'bear' } },
      ],
      showcases: ['Team with a lead'],
      requirements: [],
      brief: 'An incident team.',
      surface: 'cloud',
    },
  ],
};

const AGENT_MD = '---\nschema: elitea.agent/v1\nname: Research Scout\navatar: default:owl\nmemory_seeds: [{ text: Cite sources }]\n---\nYou research.\n';

beforeEach(() => {
  imported = [];
  configureGeneratedClient({ baseUrl: BASE });
  setBackendCapabilityForTests('aiGeneration', true);
  server.use(
    http.get('*/blueprints/index.json', () => HttpResponse.json(INDEX)),
    http.get('*/blueprints/agents/research-scout.agent.md', () => new HttpResponse(AGENT_MD, { headers: { 'Content-Type': 'text/markdown' } })),
    http.post(`${BASE}/elitea_core/import_wizard/prompt_lib/:projectId`, async ({ request }) => {
      imported.push(await request.json());
      return HttpResponse.json({ result: { agents: [{ id: '9' }] }, errors: {} }, { status: 201 });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
  resetBackendCapabilitiesForTests();
});

async function openGallery(initialTab: 'agents' | 'teams' = 'agents') {
  const user = userEvent.setup();
  renderWithQueryAndTheme(<BlueprintGalleryButton projectId="7" initialTab={initialTab} testIdPrefix="agents" onAgentCreated={vi.fn()} />);
  await user.click(screen.getByTestId('agents-blueprints-open'));
  return user;
}

describe('BlueprintGalleryButton', () => {
  it('renders the agent cards of the index: name, description, showcases, needs', async () => {
    await openGallery();
    const card = await screen.findByTestId('blueprint-card-research-scout');
    expect(within(card).getByText('Research Scout')).toBeInTheDocument();
    expect(within(card).getByText('Finds sources')).toBeInTheDocument();
    expect(within(card).getByText('Citations')).toBeInTheDocument();
    expect(within(card).getByText(/A web search toolkit/)).toBeInTheDocument();
  });

  // The photo avatars of a team (the Elitea dev team's WebP stills) render through the shared AgentAvatar from the
  // app's own built files: an app path, so the desktop serves it from its bundle and the web from its origin.
  it('shows the photo avatar of a member from the built blueprint files', async () => {
    const team = INDEX.teams[0];
    server.use(
      http.get('*/blueprints/index.json', () =>
        HttpResponse.json({
          ...INDEX,
          teams: [{ ...team, members: [{ slug: 'coordinator', name: 'Coordinator', avatar: { still: 'avatars/elitea-dev-team/coordinator/still.webp' } }] }],
        }),
      ),
    );
    await openGallery('teams');
    const card = await screen.findByTestId('blueprint-card-incident-desk');
    const image = card.querySelector('img');
    expect(image?.getAttribute('src')).toMatch(/\/blueprints\/avatars\/elitea-dev-team\/coordinator\/still\.webp$/);
  });

  it('opens on the Teams tab from the Teams page', async () => {
    await openGallery('teams');
    expect(await screen.findByTestId('blueprint-card-incident-desk')).toBeInTheDocument();
    expect(screen.queryByTestId('blueprint-card-research-scout')).not.toBeInTheDocument();
  });

  it('imports a blueprint through the reader and the shared dialog, then shows the follow-ups', async () => {
    const user = await openGallery();
    await user.click(await screen.findByTestId('blueprint-import-research-scout'));
    const dialog = await screen.findByTestId('blueprints-import-dialog');
    expect(within(dialog).getByRole('heading', { name: 'Research Scout' })).toBeInTheDocument();
    // Issue 3142: a blueprint names no model; the preview says so before the write.
    expect(within(dialog).getByTestId('blueprints-import-model')).toHaveTextContent("Model: this project's default");
    expect(within(dialog).getByTestId('blueprints-import-stat-seeds')).toHaveTextContent('Memory seeds1');
    await user.click(within(dialog).getByRole('button', { name: 'Import' }));
    await waitFor(() => expect(imported).toHaveLength(1));
    const request = imported[0] as { applications: { name: string }[] };
    expect(request.applications[0]?.name).toBe('Research Scout');
    expect(await screen.findByTestId('blueprints-import-seeds')).toHaveTextContent('1');
  });

  it('names a blueprint whose file cannot be loaded', async () => {
    server.use(http.get('*/blueprints/agents/research-scout.agent.md', () => new HttpResponse(null, { status: 404 })));
    const user = await openGallery();
    await user.click(await screen.findByTestId('blueprint-import-research-scout'));
    expect(await screen.findByTestId('blueprint-import-error')).toHaveTextContent('Research Scout');
  });

  it('crafts from a blueprint: Build with AI opens with its brief', async () => {
    const user = await openGallery();
    await user.click(await screen.findByTestId('blueprint-craft-research-scout'));
    expect(await screen.findByPlaceholderText(/Describe your agent/)).toHaveValue('A research agent that cites every claim.');
  });
});

describe('readBlueprintIndex', () => {
  it('puts a team lead first, drops entries without a file and reads another schema as empty', () => {
    const index = readBlueprintIndex({ ...INDEX, agents: [...INDEX.agents, { name: 'No file' }] });
    expect(index.agents.map((entry) => entry.slug)).toEqual(['research-scout']);
    expect(index.teams[0]?.members.map((member) => member.slug)).toEqual(['incident-triage', 'comms-drafter']);
    expect(readBlueprintIndex({ ...INDEX, schema: 'elitea.blueprints/v2' })).toEqual({ agents: [], teams: [] });
  });
});
