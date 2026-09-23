import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { resetBackendCapabilitiesForTests, setBackendCapabilityForTests } from '@/shared/config/backendCapabilities';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { CreateSkill } from './CreateSkill';
import { renderSkillsRoute } from './__tests__/testRouter';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  // The Generate button is hidden while the draft route is unmounted — see
  // `shared/config/backendCapabilities`.
  setBackendCapabilityForTests('aiGeneration', true);
});
afterEach(() => {
  resetGeneratedClient();
  resetBackendCapabilitiesForTests();
});

describe('CreateSkill', () => {
  /* elitea_issues: #5399 — saving with Instructions empty shows inline
     "Instructions are required." validation, never a raw backend error;
     the mutation is never even sent (no request mock is registered here). */
  it('validates incomplete drafts before sending', async () => {
    const user = userEvent.setup();
    renderSkillsRoute(<CreateSkill />, '/skills/create');
    await user.type(await screen.findByTestId('skill-name-input'), 'Reviewer');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(screen.getByText('Description is required.')).toBeInTheDocument();
    expect(screen.getByText('Instructions are required.')).toBeInTheDocument();
  });

  it('creates a base skill and navigates to its editor', async () => {
    const user = userEvent.setup();
    server.use(
      http.post(`${BASE}/elitea_core/skills/prompt_lib/:projectId`, () =>
        HttpResponse.json({
          id: 'skill-1',
          project_id: 'project-1',
          name: 'Reviewer',
          type: 'skill',
          is_default: false,
          created_at: '2026-01-01T00:00:00Z',
          updated_at: '2026-01-01T00:00:00Z',
        }),
      ),
    );
    const { router } = renderSkillsRoute(<CreateSkill />, '/skills/create');
    await user.type(await screen.findByTestId('skill-name-input'), 'Reviewer');
    await user.type(screen.getByTestId('skill-description-input'), 'Review code');
    await user.type(screen.getByTestId('skill-instructions-input'), 'Be careful');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/skills/all/skill-1'));
  });

  it('discards back to the list', async () => {
    const user = userEvent.setup();
    const { router } = renderSkillsRoute(<CreateSkill />, '/skills/create');
    await user.type(await screen.findByTestId('skill-name-input'), 'x');
    await user.click(screen.getByRole('button', { name: 'Discard' }));
    await waitFor(() => expect(router.state.location.pathname).toBe('/skills/all'));
  });

  it('uses an AI-generated draft in the create form', async () => {
    const user = userEvent.setup();
    server.use(
      http.post(`${BASE}/elitea_core/generate_skill_draft/prompt_lib/:projectId`, () =>
        HttpResponse.json({
          name: 'Generated',
          description: 'Generated description',
          instructions: 'Generated instructions',
          tags: ['ai'],
        }),
      ),
    );
    renderSkillsRoute(<CreateSkill />, '/skills/create');
    await user.click(await screen.findByRole('button', { name: 'Generate with AI' }));
    await user.type(screen.getByLabelText('What should this skill do?'), 'Review code');
    await user.click(screen.getByRole('button', { name: 'Generate' }));
    await user.click(await screen.findByRole('button', { name: 'Use draft' }));
    expect(screen.getByTestId('skill-name-input')).toHaveValue('Generated');
  });

  it('shows a safe error when creation fails', async () => {
    const user = userEvent.setup();
    server.use(
      http.post(`${BASE}/elitea_core/skills/prompt_lib/:projectId`, () =>
        HttpResponse.json({ error: 'internal details' }, { status: 500 }),
      ),
    );
    renderSkillsRoute(<CreateSkill />, '/skills/create');
    await user.type(await screen.findByTestId('skill-name-input'), 'Reviewer');
    await user.type(screen.getByTestId('skill-description-input'), 'Review code');
    await user.type(screen.getByTestId('skill-instructions-input'), 'Be careful');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(await screen.findByRole('alert')).toHaveTextContent('Failed to create');
  });
});
