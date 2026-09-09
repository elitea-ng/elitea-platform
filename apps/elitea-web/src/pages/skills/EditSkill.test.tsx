import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { EditSkill, skillIconVersionId, skillVersionKey, toSkillForm, toSkillIconMeta } from './EditSkill';
import { renderSkillsRoute } from './__tests__/testRouter';

const BASE = '/api/v2';
const detail = {
  id: 'skill-1',
  project_id: 'project-1',
  name: 'Reviewer',
  description: 'Reviews code',
  type: 'skill',
  is_default: false,
  created_at: '2026-01-01T00:00:00Z',
  updated_at: '2026-01-01T00:00:00Z',
  version_details: { id: 'v1', name: 'base', instructions: 'Be careful', tags: ['quality'] },
  versions: [
    { id: 'v1', name: 'base', instructions: 'Be careful', tags: ['quality'] },
    { id: 'v2', name: 'second', instructions: 'Be very careful', tags: [] },
  ],
};

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  // The test pane POSTs `predict_llm`, which no router mounts, so it is
  // hidden by default — see `shared/config/backendCapabilities`.
  setBackendCapabilityForTests('llmPredictStreaming', true);
  server.use(
    http.get(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId`, () =>
      HttpResponse.json(detail),
    ),
    http.get(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId/:versionId`, () =>
      HttpResponse.json(detail),
    ),
  );
});
afterEach(() => {
  resetGeneratedClient();
  resetBackendCapabilitiesForTests();
  vi.restoreAllMocks();
});

describe('EditSkill', () => {
  it('maps basic and versioned skill responses into editable form values', () => {
    const {
      description: _description,
      version_details: _versionDetails,
      ...skillWithVersions
    } = detail;
    const {
      version_details: _withoutVersionDetails,
      versions: _versions,
      ...skillWithoutVersions
    } = detail;
    expect(toSkillForm(undefined)).toEqual({ name: '', description: '', instructions: '', tags: [] });
    expect(toSkillForm(skillWithVersions)).toEqual({
      name: 'Reviewer',
      description: '',
      instructions: 'Be careful',
      tags: ['quality'],
    });
    expect(toSkillForm(skillWithoutVersions)).toMatchObject({ instructions: '', tags: [] });
    expect(skillVersionKey({ name: 'named', instructions: '', tags: [] })).toBe('named');
    expect(skillVersionKey({ id: 2, name: 'named', instructions: '', tags: [] })).toBe('2');
  });

  it('reads the icon out of version_details.meta.icon_meta, and refuses a half-filled one', () => {
    const withIcon = {
      ...detail,
      version_details: {
        ...detail.version_details,
        meta: { icon_meta: { name: 'skill_a.png', url: '/icons/7/skill_a.png' } },
      },
    };
    expect(toSkillIconMeta(withIcon)).toEqual({ name: 'skill_a.png', url: '/icons/7/skill_a.png' });

    // Everything the reset and the absent cases produce must read as "no icon":
    // returning a half-filled meta would draw a broken image.
    for (const iconMeta of [{}, { name: '', url: '' }, { name: 'a' }, { url: '/icons/7/a' }, null]) {
      expect(
        toSkillIconMeta({
          ...detail,
          version_details: { ...detail.version_details, meta: { icon_meta: iconMeta } },
        }),
      ).toBeNull();
    }
    expect(toSkillIconMeta(undefined)).toBeNull();
    expect(toSkillIconMeta(detail)).toBeNull();
  });

  it('binds an icon to the route version when it names one, else to the skill default', () => {
    expect(skillIconVersionId(detail, '9')).toBe('9');
    // A named (non-numeric) version cannot address a skill_versions row, so the
    // default version id is used instead of sending the name to the server.
    expect(skillIconVersionId(detail, 'base')).toBe('v1');
    expect(skillIconVersionId(detail, undefined)).toBe('v1');
    expect(skillIconVersionId(undefined, undefined)).toBeUndefined();
  });

  it('loads the form and ephemeral test panel', async () => {
    renderSkillsRoute(<EditSkill />, '/skills/all/skill-1');
    expect(await screen.findByTestId('skill-name-input')).toHaveValue('Reviewer');
    expect(screen.getByTestId('skill-test-panel')).toBeInTheDocument();
    // The icon control is live now that the route family exists: the fixture
    // skill has a version id, so there is something for the bind to address.
    expect(screen.getByTestId('skill-icon-button')).toBeEnabled();
  });

  it('saves edits through the version-aware endpoint', async () => {
    const user = userEvent.setup();
    let updated = false;
    server.use(
      http.put(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId`, () => {
        updated = true;
        return HttpResponse.json(detail);
      }),
    );
    renderSkillsRoute(<EditSkill />, '/skills/all/skill-1');
    const name = await screen.findByTestId('skill-name-input');
    await user.type(name, ' updated');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(updated).toBe(true));
  });

  it('creates a named version from the current instructions', async () => {
    const user = userEvent.setup();
    let created = false;
    server.use(
      http.post(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId`, () => {
        created = true;
        return HttpResponse.json(detail);
      }),
    );
    renderSkillsRoute(<EditSkill />, '/skills/all/skill-1');
    await user.click(await screen.findByRole('button', { name: 'New version' }));
    await user.type(screen.getByLabelText('Version name'), 'v3');
    await user.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() => expect(created).toBe(true));
  });

  it('sets the route-selected version as default', async () => {
    const user = userEvent.setup();
    let defaulted = false;
    server.use(
      http.patch(`${BASE}/elitea_core/skill_default_version/prompt_lib/:projectId/:skillId`, () => {
        defaulted = true;
        return HttpResponse.json(detail);
      }),
    );
    renderSkillsRoute(<EditSkill />, '/skills/all/skill-1/v2');
    await user.click(await screen.findByRole('button', { name: 'Set default' }));
    await waitFor(() => expect(defaulted).toBe(true));
  });

  it('navigates when another version is selected', async () => {
    const user = userEvent.setup();
    const { router } = renderSkillsRoute(<EditSkill />, '/skills/all/skill-1');
    await user.click(await screen.findByRole('combobox'));
    await user.click(screen.getByRole('option', { name: 'second' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/skills/all/skill-1/v2'));
  });

  it('discards edits and deletes the skill', async () => {
    const user = userEvent.setup();
    let deleted = false;
    server.use(
      http.delete(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId`, () => {
        deleted = true;
        return new HttpResponse(null, { status: 204 });
      }),
    );
    const { router } = renderSkillsRoute(<EditSkill />, '/skills/all/skill-1');
    const name = await screen.findByTestId('skill-name-input');
    await user.type(name, ' changed');
    await user.click(screen.getByRole('button', { name: 'Discard' }));
    expect(name).toHaveValue('Reviewer');
    await user.click(screen.getByRole('button', { name: 'Delete' }));
    const deleteButtons = screen.getAllByRole('button', { name: 'Delete' });
    await user.click(deleteButtons[deleteButtons.length - 1]!);
    await waitFor(() => expect(deleted).toBe(true));
    await waitFor(() => expect(router.state.location.pathname).toBe('/skills/all'));
  });

  it('exports the selected skill as Markdown', async () => {
    const user = userEvent.setup();
    server.use(
      http.get(`${BASE}/elitea_core/skill_export/prompt_lib/:projectId/:skillId`, () =>
        HttpResponse.text('# Reviewer'),
      ),
    );
    vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:skill');
    vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
    renderSkillsRoute(<EditSkill />, '/skills/all/skill-1');
    await user.click(await screen.findByRole('button', { name: 'Export' }));
    await waitFor(() => expect(click).toHaveBeenCalled());
  });

  it('shows load and save failures without exposing raw server errors', async () => {
    const user = userEvent.setup();
    server.use(
      http.put(`${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId`, () =>
        HttpResponse.json({ error: 'secret' }, { status: 500 }),
      ),
    );
    renderSkillsRoute(<EditSkill />, '/skills/all/skill-1');
    await user.type(await screen.findByTestId('skill-name-input'), ' changed');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to save');
  });

  // ---- #874: compare, restore, delete-version -----------------------------

  it('hides restore/delete-version on base, but offers compare once a second version exists', async () => {
    renderSkillsRoute(<EditSkill />, '/skills/all/skill-1');
    await screen.findByTestId('skill-name-input');
    expect(screen.getByRole('button', { name: 'Compare versions' })).toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Restore' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Delete version' })).not.toBeInTheDocument();
  });

  it('offers restore and delete-version on a NAMED (non-base) version', async () => {
    renderSkillsRoute(<EditSkill />, '/skills/all/skill-1/v2');
    await screen.findByTestId('skill-name-input');
    expect(screen.getByRole('button', { name: 'Restore' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Delete version' })).toBeInTheDocument();
  });

  it('compares the open version against another and shows the instructions diff', async () => {
    const user = userEvent.setup();
    renderSkillsRoute(<EditSkill />, '/skills/all/skill-1/v2');
    await user.click(await screen.findByRole('button', { name: 'Compare versions' }));
    const modal = await screen.findByTestId('skill-compare-versions-modal');
    expect(modal).toHaveTextContent('second');
    // The picker defaults to the only other version, "base".
    expect(screen.getByTestId('skill-compare-versions-select')).toHaveValue('v1');
    // The two versions' instructions differ ("Be careful" vs "Be very careful"),
    // so the instructions field renders an actual diff, not "No differences.".
    expect(screen.getByTestId('skill-compare-diff-Instructions')).toBeInTheDocument();
  });

  it('restores a named version onto base and navigates there', async () => {
    const user = userEvent.setup();
    let restoredVersionId: string | undefined;
    server.use(
      http.post(
        `${BASE}/elitea_core/skill_version_restore/prompt_lib/:projectId/:skillId/:versionId`,
        ({ params }) => {
          restoredVersionId = params['versionId'] as string;
          return HttpResponse.json(detail);
        },
      ),
    );
    const { router } = renderSkillsRoute(<EditSkill />, '/skills/all/skill-1/v2');
    await user.click(await screen.findByRole('button', { name: 'Restore' }));
    await user.click(screen.getByRole('button', { name: 'Confirm' }));
    await waitFor(() => expect(restoredVersionId).toBe('v2'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/skills/all/skill-1/v1'));
  });

  it('deletes a named version and navigates back to base', async () => {
    const user = userEvent.setup();
    let deletedVersionId: string | undefined;
    server.use(
      http.delete(
        `${BASE}/elitea_core/skill/prompt_lib/:projectId/:skillId/:versionId`,
        ({ params }) => {
          deletedVersionId = params['versionId'] as string;
          return new HttpResponse(null, { status: 204 });
        },
      ),
    );
    const { router } = renderSkillsRoute(<EditSkill />, '/skills/all/skill-1/v2');
    await user.click(await screen.findByRole('button', { name: 'Delete version' }));
    await user.click(await screen.findByRole('button', { name: 'Delete' }));
    await waitFor(() => expect(deletedVersionId).toBe('v2'));
    await waitFor(() => expect(router.state.location.pathname).toBe('/skills/all/skill-1/v1'));
  });
});
