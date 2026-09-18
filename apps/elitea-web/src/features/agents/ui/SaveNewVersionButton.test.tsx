import { createRef } from 'react';
import type { ComponentProps } from 'react';

import { QueryClientProvider } from '@tanstack/react-query';
import { render, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { getSaveApplicationNewVersionMockHandler } from '@/shared/api/generated/applications/applications.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { MAX_VERSION_LENGTH } from '@/shared/lib/limits';
import { server } from '@/test/setup';

import { createTestQueryClient } from '../__tests__/testUtils';
import { SaveNewVersionButton, type SaveNewVersionButtonHandle } from './SaveNewVersionButton';

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

function renderButton(props: Partial<ComponentProps<typeof SaveNewVersionButton>> = {}) {
  const queryClient = createTestQueryClient();
  return render(
    <QueryClientProvider client={queryClient}>
      <SaveNewVersionButton
        applicationId="3"
        projectId="p1"
        existingVersionNames={['base', 'v1']}
        version={{ instructions: 'Be helpful' }}
        {...props}
      />
    </QueryClientProvider>,
  );
}

describe('SaveNewVersionButton', () => {
  it('opens the version-name dialog on click', async () => {
    const user = userEvent.setup();
    renderButton();
    expect(screen.queryByText('Create version')).not.toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Save As Version' }));
    expect(screen.getByText('Create version')).toBeInTheDocument();
  });

  /* elitea_issues: #5755 — the Create-version dialog title carries a green checkmark */
  it('shows a checkmark icon next to the Create-version dialog title', async () => {
    const user = userEvent.setup();
    renderButton();
    await user.click(screen.getByRole('button', { name: 'Save As Version' }));
    expect(screen.getByTestId('save-new-version-dialog-check-icon')).toBeInTheDocument();
  });

  it('rejects a duplicate version name without calling the API', async () => {
    let requestCount = 0;
    server.use(
      getSaveApplicationNewVersionMockHandler(() => {
        requestCount += 1;
        return { id: '9', application_id: '3', name: 'base', status: 'draft' };
      }),
    );
    const user = userEvent.setup();
    renderButton();
    await user.click(screen.getByRole('button', { name: 'Save As Version' }));
    await user.type(screen.getByLabelText('Version name'), 'base');
    await user.click(screen.getByRole('button', { name: 'Save' }));
    expect(screen.getByText(/already exists/)).toBeInTheDocument();
    expect(requestCount).toBe(0);
  });

  it('creates a new version and calls onSuccess, closing the dialog', async () => {
    server.use(
      getSaveApplicationNewVersionMockHandler({
        id: '9',
        application_id: '3',
        name: 'v2',
        status: 'draft',
        instructions: 'Be helpful',
      }),
    );
    const onSuccess = vi.fn();
    const user = userEvent.setup();
    renderButton({ onSuccess });
    await user.click(screen.getByRole('button', { name: 'Save As Version' }));
    await user.type(screen.getByLabelText('Version name'), 'v2');
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(onSuccess).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(screen.queryByText('Create version')).not.toBeInTheDocument());
  });

  it('exposes an imperative onSaveVersion handle that opens the dialog', async () => {
    const ref = createRef<SaveNewVersionButtonHandle>();
    renderButton({ ref });
    expect(screen.queryByText('Create version')).not.toBeInTheDocument();
    ref.current?.onSaveVersion();
    expect(await screen.findByText('Create version')).toBeInTheDocument();
  });

  it('calls onClickHandler instead of opening the dialog when provided', async () => {
    const onClickHandler = vi.fn();
    const user = userEvent.setup();
    renderButton({ onClickHandler });
    await user.click(screen.getByRole('button', { name: 'Save As Version' }));
    expect(onClickHandler).toHaveBeenCalledTimes(1);
    expect(screen.queryByText('Create version')).not.toBeInTheDocument();
  });

  it(`caps the version name input at ${MAX_VERSION_LENGTH} characters`, async () => {
    const user = userEvent.setup();
    renderButton();
    await user.click(screen.getByRole('button', { name: 'Save As Version' }));
    expect(screen.getByLabelText('Version name')).toHaveAttribute('maxlength', String(MAX_VERSION_LENGTH));
  });

  it('reports the API error via onError on the very first failed save attempt (no stale-closure silent swallow)', async () => {
    server.use(
      http.post('*/elitea_core/versions/prompt_lib/:projectId/:applicationId', () =>
        HttpResponse.json({ error: 'boom' }, { status: 500 }),
      ),
    );
    const onError = vi.fn();
    const user = userEvent.setup();
    renderButton({ onError });
    await user.click(screen.getByRole('button', { name: 'Save As Version' }));
    await user.type(screen.getByLabelText('Version name'), 'v2');
    await user.click(screen.getByRole('button', { name: 'Save' }));

    await waitFor(() => expect(onError).toHaveBeenCalledTimes(1));
    expect(onError).toHaveBeenCalledWith(expect.stringContaining('500'));
  });

  it("reports the CURRENT attempt's error, not a stale message from a previous failed attempt", async () => {
    let requestCount = 0;
    server.use(
      http.post('*/elitea_core/versions/prompt_lib/:projectId/:applicationId', () => {
        requestCount += 1;
        return HttpResponse.json({ error: 'boom' }, { status: requestCount === 1 ? 500 : 400 });
      }),
    );
    const onError = vi.fn();
    const user = userEvent.setup();
    renderButton({ onError });
    await user.click(screen.getByRole('button', { name: 'Save As Version' }));
    await user.type(screen.getByLabelText('Version name'), 'v2');

    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onError).toHaveBeenCalledTimes(1));
    expect(onError).toHaveBeenLastCalledWith(expect.stringContaining('500'));

    await user.click(screen.getByRole('button', { name: 'Save' }));
    await waitFor(() => expect(onError).toHaveBeenCalledTimes(2));
    expect(onError).toHaveBeenLastCalledWith(expect.stringContaining('400'));
  });
});
