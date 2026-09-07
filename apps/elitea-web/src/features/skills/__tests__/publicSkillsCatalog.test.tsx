/**
 * The consuming half of publishing: the catalog, and the attach that forks out
 * of it.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { PublicSkillsCatalog, publishedVersionIdOf } from '../ui/PublicSkillsCatalog';
import { PERMISSIONS } from '@/shared/lib/permissions';
import {
  alreadyAttachedAgentNames,
  alreadyAttachedVersionIds,
  attachRequestOf,
  reportOutcome,
} from '../ui/AttachPublicSkillDialog';
import { renderWithProviders } from './testUtils';

const BASE = '/api/v2';
const PROJECT = '2';
/** Attaching forks the skill into the project, so the catalog needs this. */
const FORK = new Set([PERMISSIONS.applications.fork]);

let catalogRequests: string[] = [];
let attachBodies: unknown[] = [];

beforeEach(() => {
  catalogRequests = [];
  attachBodies = [];
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/skill_categories/prompt_lib/:projectId`, () =>
      HttpResponse.json({
        categories: [
          { name: 'Development', is_default: true },
          { name: 'Security', is_default: false },
        ],
        total: 2,
      }),
    ),
    http.get(`${BASE}/elitea_core/public_skills/prompt_lib`, ({ request }) => {
      catalogRequests.push(request.url);
      return HttpResponse.json({
        total: 1,
        rows: [
          {
            id: 42,
            name: 'PR Reviewer',
            description: 'Reviews pull requests',
            tags: ['review', 'Development'],
            versions: [
              { id: 5, name: 'base', status: 'draft' },
              { id: 6, name: 'v1.0', status: 'published' },
            ],
            has_published_version: true,
          },
        ],
      });
    }),
    http.get(`${BASE}/elitea_core/applications/prompt_lib/:projectId`, () =>
      HttpResponse.json({ total: 1, rows: [{ id: '3', name: 'Helper' }] }),
    ),
    http.get(`${BASE}/elitea_core/application/prompt_lib/:projectId/:applicationId`, () =>
      HttpResponse.json({ id: '3', name: 'Helper', versions: [{ id: 77, name: 'latest' }] }),
    ),
    // Empty by default — most tests are not about this endpoint. The
    // "already attached" tests below override it per-case.
    http.get(`${BASE}/elitea_core/agents_with_skill/prompt_lib/:projectId/:skillId`, () =>
      HttpResponse.json({ total: 0, rows: [] }),
    ),
    http.post(`${BASE}/elitea_core/attach_public_skill/prompt_lib/:projectId`, async ({ request }) => {
      attachBodies.push(await request.json());
      return HttpResponse.json({ results: [{ agent_version_id: 77, ok: true }] });
    }),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('publishedVersionIdOf', () => {
  it('picks the published version, never merely the first', () => {
    expect(
      publishedVersionIdOf({
        id: 1,
        versions: [
          { id: 5, status: 'draft' },
          { id: 6, status: 'published' },
        ],
      }),
    ).toBe(6);
    // A catalog row with nothing published is not attachable, and says so by
    // resolving to nothing rather than to its draft.
    expect(publishedVersionIdOf({ id: 1, versions: [{ id: 5, status: 'draft' }] })).toBeUndefined();
  });
});

describe('attachRequestOf', () => {
  it('refuses to build a request out of an unselected agent version', () => {
    // `Number('')` is 0, and an attach aimed at agent version 0 is a request
    // the server can only refuse.
    expect(attachRequestOf(42, 6, '')).toBeUndefined();
    expect(attachRequestOf(undefined, 6, '77')).toBeUndefined();
    expect(attachRequestOf(42, 6, '77')).toEqual({
      publicSkillId: 42,
      publicVersionId: 6,
      agentVersionIds: [77],
    });
  });
});

describe('alreadyAttachedVersionIds', () => {
  it('collects the entity_version_id of every row, dropping rows with none', () => {
    const ids = alreadyAttachedVersionIds([
      { application_id: 1, name: 'A', entity_version_id: 77 },
      { application_id: 2, name: 'B', entity_version_id: 90 },
      { application_id: 3, name: 'C' },
    ]);
    expect(ids.has(77)).toBe(true);
    expect(ids.has(90)).toBe(true);
    expect(ids.size).toBe(2);
  });

  it('is an empty set for no rows', () => {
    expect(alreadyAttachedVersionIds([]).size).toBe(0);
  });
});

describe('alreadyAttachedAgentNames', () => {
  it('dedupes names, in first-seen order, dropping blanks', () => {
    expect(
      alreadyAttachedAgentNames([
        { application_id: 1, name: 'Helper', entity_version_id: 77 },
        { application_id: 1, name: 'Helper', entity_version_id: 78 },
        { application_id: 2, name: 'Reviewer', entity_version_id: 90 },
        { application_id: 3, name: '', entity_version_id: 91 },
      ]),
    ).toEqual(['Helper', 'Reviewer']);
  });
});

describe('reportOutcome', () => {
  it('reports a per-agent failure that arrived inside a 200', () => {
    const setMessage = vi.fn();
    const done = vi.fn();
    reportOutcome([{ agent_version_id: 77, ok: false, error: 'agent version not found' }], setMessage, done);
    expect(setMessage).toHaveBeenCalledWith('agent version not found');
    expect(done).not.toHaveBeenCalled();
  });
});

describe('PublicSkillsCatalog', () => {
  it('lists the catalog and narrows it by the selected category', async () => {
    const user = userEvent.setup();
    renderWithProviders(<PublicSkillsCatalog projectId={PROJECT} permissions={FORK} />);

    expect(await screen.findByText('PR Reviewer')).toBeInTheDocument();

    await user.click(screen.getByText('Security'));
    // The filter is applied by the SERVER — the request carries it — rather
    // than by hiding rows the server already returned.
    await waitFor(() =>
      expect(catalogRequests.some((url) => url.includes('category=Security'))).toBe(true),
    );
  });

  it('attaches a published skill to the chosen agent version', async () => {
    const user = userEvent.setup();
    renderWithProviders(<PublicSkillsCatalog projectId={PROJECT} permissions={FORK} />);

    await user.click(await screen.findByText('PR Reviewer'));
    await user.click(await screen.findByLabelText('Agent'));
    await user.click(await screen.findByRole('option', { name: 'Helper' }));
    await user.click(await screen.findByLabelText('Agent version'));
    await user.click(await screen.findByRole('option', { name: 'latest' }));
    await user.click(screen.getByRole('button', { name: 'Attach' }));

    await waitFor(() => expect(attachBodies).toHaveLength(1));
    // The PUBLISHED version id (6), not the draft (5) the row also carries.
    expect(attachBodies[0]).toEqual({
      public_skill_id: 42,
      public_version_id: 6,
      agent_version_ids: [77],
      entity_type: 'agent',
    });
  });

  it('marks the agent version agents_with_skill already named, rather than let a second attach 409', async () => {
    server.use(
      http.get(`${BASE}/elitea_core/agents_with_skill/prompt_lib/:projectId/:skillId`, () =>
        HttpResponse.json({
          total: 1,
          rows: [{ application_id: 3, name: 'Helper', entity_version_id: 77 }],
        }),
      ),
    );
    const user = userEvent.setup();
    renderWithProviders(<PublicSkillsCatalog projectId={PROJECT} permissions={FORK} />);

    await user.click(await screen.findByText('PR Reviewer'));
    expect(await screen.findByTestId('attach-public-skill-already-attached')).toHaveTextContent(
      'Already attached to: Helper',
    );

    await user.click(await screen.findByLabelText('Agent'));
    await user.click(await screen.findByRole('option', { name: 'Helper' }));
    await user.click(await screen.findByLabelText('Agent version'));

    const option = await screen.findByRole('option', { name: 'latest (already attached)' });
    expect(option).toHaveAttribute('aria-disabled', 'true');
  });

  it('shows no banner and no disabled option when agents_with_skill names nothing', async () => {
    const user = userEvent.setup();
    renderWithProviders(<PublicSkillsCatalog projectId={PROJECT} permissions={FORK} />);

    await user.click(await screen.findByText('PR Reviewer'));
    await user.click(await screen.findByLabelText('Agent'));
    await user.click(await screen.findByRole('option', { name: 'Helper' }));
    await user.click(await screen.findByLabelText('Agent version'));

    expect(screen.queryByTestId('attach-public-skill-already-attached')).not.toBeInTheDocument();
    const option = await screen.findByRole('option', { name: 'latest' });
    expect(option).not.toHaveAttribute('aria-disabled', 'true');
  });
});
