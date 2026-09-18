/**
 * `elitea_issues: #6627` — "[BUG] Agent header icon does not update after
 * selecting a new icon — only appears after a page reload". Re-judged as
 * FEATURE-ABSENT against this app (no editable icon existed anywhere — see
 * this component's own doc comment); this pins the behaviour the issue asked
 * for, at the unit level: picking an icon repaints the SAME render, with no
 * refetch/reload in between.
 */
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithProviders } from '../__tests__/testUtils';

import { AgentIconEditor } from './AgentIconEditor';

const BASE = '/api/v2';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/elitea_core/default_icons/prompt_lib/7`, () =>
      HttpResponse.json([{ name: 'image_0.png', url: '/app/default_entity_icons/image_0.png' }]),
    ),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AgentIconEditor', () => {
  it('renders the fallback glyph with no icon, and opens the picker on click', async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <AgentIconEditor
        projectId="7"
        applicationId={9}
        versionId="42"
        agentName="My Agent"
        iconMeta={null}
      />,
    );

    expect(screen.queryByRole('dialog')).not.toBeInTheDocument();
    await user.click(screen.getByTestId('agent-icon-edit-button'));
    await waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
  });

  it('selecting a default icon repaints the button on the SAME render — no reload, no stale src', async () => {
    let putBody: unknown;
    server.use(
      http.put(`${BASE}/elitea_core/upload_icon/prompt_lib/7/42`, async ({ request }) => {
        putBody = await request.json();
        return HttpResponse.json({ ok: true });
      }),
    );
    const user = userEvent.setup();
    renderWithProviders(
      <AgentIconEditor
        projectId="7"
        applicationId={9}
        versionId="42"
        agentName="My Agent"
        iconMeta={null}
      />,
    );

    // Before: the fallback glyph, no <img>.
    expect(screen.queryByRole('img')).not.toBeInTheDocument();

    const button = screen.getByTestId('agent-icon-edit-button');
    await user.click(button);
    await waitFor(() => expect(screen.getByRole('dialog')).toBeInTheDocument());
    await user.click(await screen.findByAltText('image_0.png'));

    // After: bound server-side, AND the BUTTON's own icon already shows the
    // new <img> — the exact "immediately, no reload" assertion #6627 was
    // filed over. The dialog itself does not auto-close on select (same
    // convention `SkillIconDialog`'s `handleSelect` already uses), so the
    // query is scoped to the trigger button, not the whole document — the
    // picker's OWN tile also renders an `image_0.png` <img> while open.
    await waitFor(() => expect(putBody).toEqual({ name: 'image_0.png', url: '/app/default_entity_icons/image_0.png' }));
    await waitFor(() => {
      const img = button.querySelector('img');
      expect(img).toHaveAttribute('src', '/app/default_entity_icons/image_0.png');
    });
  });

  it('disables the button while there is no saved version to bind to', () => {
    renderWithProviders(
      <AgentIconEditor
        projectId="7"
        applicationId={9}
        versionId={undefined}
        agentName="Draft agent"
        iconMeta={null}
      />,
    );
    expect(screen.getByTestId('agent-icon-edit-button')).toBeDisabled();
  });
});
