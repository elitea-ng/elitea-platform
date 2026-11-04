import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { ThreadGrantFieldContext } from '@/entities/thread-target';
import { ThreadGrantField } from '@/features/unattended';
import { getGetPipelineScheduleResponseMock } from '@/shared/api/generated/applications/applications.msw';
import { getListConversationsMockHandler } from '@/shared/api/generated/chat/chat.msw';
import { configureGeneratedClient, EliteaApiError, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { renderWithRouterAndProject } from '../../__tests__/testUtils';
import { PipelineScheduleModal, type PipelineScheduleModalProps } from './PipelineScheduleModal';

describe('PipelineScheduleModal', () => {
  it('is not rendered when closed', () => {
    const { queryByText } = renderWithTheme(
      <PipelineScheduleModal
        open={false}
        onClose={vi.fn()}
        onSubmit={vi.fn()}
      />,
    );
    expect(queryByText('Schedule settings')).not.toBeInTheDocument();
  });

  it('seeds the field from the cron prop when opened', () => {
    const { getByText } = renderWithTheme(
      <PipelineScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        cron="0 0 * * 6"
      />,
    );
    expect(getByText('Schedule settings')).toBeInTheDocument();
    expect(getByText('At 00:00, only on Saturday')).toBeInTheDocument();
  });

  it('submits the current cron expression and closes on Apply', async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const onClose = vi.fn();

    const { getByRole } = renderWithTheme(
      <PipelineScheduleModal
        open
        onClose={onClose}
        onSubmit={onSubmit}
        cron="0 0 * * 6"
      />,
    );

    await user.click(getByRole('button', { name: 'Apply' }));

    expect(onSubmit).toHaveBeenCalledWith('0 0 * * 6');
    expect(onClose).toHaveBeenCalled();
  });

  it('switches to the Advanced raw-text mode', async () => {
    const user = userEvent.setup();
    const { getByLabelText, getByRole } = renderWithTheme(
      <PipelineScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        cron="0 0 * * 6"
      />,
    );

    await user.click(getByLabelText('Advanced'));
    expect(getByRole('textbox')).toHaveValue('0 0 * * 6');
  });

  it('shows the human-readable cron description in Advanced mode', async () => {
    const user = userEvent.setup();
    const { getByLabelText, getByText } = renderWithTheme(
      <PipelineScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        cron="0 0 * * 6"
      />,
    );

    await user.click(getByLabelText('Advanced'));

    expect(getByText('At 00:00, only on Saturday')).toBeInTheDocument();
  });

  it('shows the parse error and disables Apply for an invalid Advanced-mode expression', async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const { getByLabelText, getByRole } = renderWithTheme(
      <PipelineScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={onSubmit}
        cron="0 0 * * 6"
      />,
    );

    await user.click(getByLabelText('Advanced'));
    const textbox = getByRole('textbox');
    await user.clear(textbox);
    await user.type(textbox, 'not a cron');

    expect(getByRole('button', { name: 'Apply' })).toBeDisabled();
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('resets to Default mode when reopened', async () => {
    const user = userEvent.setup();
    const { getByLabelText, rerender, queryByRole } = renderWithTheme(
      <PipelineScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        cron="0 0 * * 6"
      />,
    );

    await user.click(getByLabelText('Advanced'));
    rerender(
      <PipelineScheduleModal
        open={false}
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        cron="0 0 * * 6"
      />,
    );
    rerender(
      <PipelineScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        cron="0 0 * * 6"
      />,
    );

    expect(queryByRole('textbox')).not.toBeInTheDocument();
  });
});

/** The thread of the v2.yaml examples, in the cloud; and a Local work thread that is not. */
const THREAD = '6f1c2d3e-4a5b-4c6d-8e7f-901234567890';
const LISTING = {
  total: 2,
  rows: [
    { id: 11, uuid: THREAD, name: 'Nightly checks', source: 'local_work', workspace_executor: 'cloud', created_at: '2026-11-03T18:00:00Z', updated_at: '2026-11-03T18:00:00Z', duration: -1, message_groups_count: 4, is_pinned: false, is_archived: false },
    { id: 12, uuid: '0a0b0c0d-0000-4000-8000-000000000001', name: 'On my laptop', source: 'local_work', workspace_executor: 'local', created_at: '2026-11-03T18:00:00Z', updated_at: '2026-11-03T18:00:00Z', duration: -1, message_groups_count: 1, is_pinned: false, is_archived: false },
  ],
};

function refused(status: number, body: unknown): EliteaApiError {
  return new EliteaApiError({ kind: 'http', status, url: '/api/v2/pipeline_schedules/prompt_lib/1/7', body });
}

function renderTargeted(props: Partial<PipelineScheduleModalProps> = {}, withGrant = false) {
  const onSubmit = vi.fn<PipelineScheduleModalProps['onSubmit']>(() => Promise.resolve(undefined));
  const onClose = vi.fn();
  const modal = (
    <PipelineScheduleModal open onClose={onClose} onSubmit={onSubmit} cron="0 0 * * 6" projectId="1" {...props} />
  );
  renderWithRouterAndProject(withGrant ? <ThreadGrantFieldContext.Provider value={ThreadGrantField}>{modal}</ThreadGrantFieldContext.Provider> : modal, '1');
  return { onSubmit, onClose, user: userEvent.setup() };
}

async function chooseThread(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.click(await screen.findByLabelText('A handed-off workspace thread'));
  await user.click(await screen.findByRole('combobox', { name: 'Thread' }));
  await user.click(await screen.findByRole('option', { name: 'Nightly checks' }));
  await user.type(screen.getByTestId('thread-target-prompt'), 'Run the nightly checks.');
}

describe('PipelineScheduleModal — Runs on a handed-off workspace thread (ADR-0045 decision 9)', () => {
  beforeEach(() => {
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(getListConversationsMockHandler(LISTING));
  });
  afterEach(() => resetGeneratedClient());

  it('shows no Runs choice without a project', () => {
    renderWithTheme(<PipelineScheduleModal open onClose={vi.fn()} onSubmit={vi.fn()} cron="0 0 * * 6" />);
    expect(screen.queryByTestId('thread-target-section')).toBeNull();
  });

  it('keeps the pipeline save as it was when the pipeline runs', async () => {
    const { onSubmit, onClose, user } = renderTargeted();
    expect(await screen.findByLabelText('This pipeline')).toBeChecked();
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    expect(onSubmit).toHaveBeenCalledWith('0 0 * * 6');
    expect(onSubmit.mock.calls[0]).toHaveLength(1);
    expect(onClose).toHaveBeenCalled();
  });

  it('lists only the threads in the cloud, and sends the thread, the message and no grant', async () => {
    const { onSubmit, onClose, user } = renderTargeted();
    await user.click(await screen.findByLabelText('A handed-off workspace thread'));
    await user.click(await screen.findByRole('combobox', { name: 'Thread' }));
    const listbox = await screen.findByRole('listbox');
    expect(within(listbox).getAllByRole('option').map(option => option.textContent)).toEqual(['Nightly checks']);
    await user.click(within(listbox).getByRole('option', { name: 'Nightly checks' }));
    await user.type(screen.getByTestId('thread-target-prompt'), 'Run the nightly checks.');
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    await waitFor(() => expect(onClose).toHaveBeenCalled());
    expect(onSubmit).toHaveBeenCalledWith('0 0 * * 6', { target_kind: 'workspace_thread', conversation_uuid: THREAD, prompt: 'Run the nightly checks.' });
  });

  it('marks a missing thread and message, and does not save', async () => {
    const { onSubmit, onClose, user } = renderTargeted();
    await user.click(await screen.findByLabelText('A handed-off workspace thread'));
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    expect(await screen.findByText('Select a thread.')).toBeInTheDocument();
    expect(screen.getByText('Type the message that each run starts with.')).toBeInTheDocument();
    expect(onSubmit).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('seeds a stored thread schedule, keeps its grant, and clears the thread with target_kind pipeline', async () => {
    const stored = getGetPipelineScheduleResponseMock();
    const { onSubmit, user } = renderTargeted({ target: stored });
    expect(await screen.findByLabelText('A handed-off workspace thread')).toBeChecked();
    expect(screen.getByTestId('thread-target-prompt')).toHaveValue('Run the nightly checks and fix what fails.');
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledWith('0 0 * * 6', {
      target_kind: 'workspace_thread',
      conversation_uuid: THREAD,
      prompt: 'Run the nightly checks and fix what fails.',
      grant: { continue_step_limit: true, continue_output_limit: true, goal_check: true, chain: { max_continuations: 5 } },
    }));
    await user.click(screen.getByLabelText('This pipeline'));
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    await waitFor(() => expect(onSubmit).toHaveBeenLastCalledWith('0 0 * * 6', { target_kind: 'pipeline' }));
  });

  it('shows not_handed_off on the thread field and stays open', async () => {
    const { onSubmit, onClose, user } = renderTargeted();
    onSubmit.mockResolvedValue(refused(422, { error: 'this conversation was never handed off to the cloud; hand it off from the desktop first', code: 'not_handed_off' }));
    await chooseThread(user);
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    expect(await screen.findByText('This thread was never sent to the cloud. Send it to the cloud from the desktop app first.')).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('explains thread_not_governable on the thread field', async () => {
    const { onSubmit, onClose, user } = renderTargeted();
    onSubmit.mockResolvedValue(refused(422, { error: 'the thread does not address an agent at this version', code: 'thread_not_governable' }));
    await chooseThread(user);
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    expect(await screen.findByText(/a governed schedule or webhook needs a thread whose agent is this version/u)).toBeInTheDocument();
    expect(onClose).not.toHaveBeenCalled();
  });

  it('sends an edited grant without the opt-ins, and shows its refusal under the grant', async () => {
    const { onSubmit, onClose, user } = renderTargeted({}, true);
    onSubmit.mockResolvedValueOnce(refused(422, { error: 'unattended_grant_refused', reason: 'over_maximum', field: 'chain.max_continuations', message: 'The unattended grant was refused: over_maximum.' }));
    await chooseThread(user);
    await user.click(screen.getByRole('button', { name: 'Run to completion settings' }));
    const dialog = screen.getByRole('dialog', { name: 'Run to completion' });
    expect(within(dialog).queryByRole('checkbox', { name: 'Hook asks' })).toBeNull();
    await user.click(within(dialog).getByRole('button', { name: 'Change limits' }));
    await user.type(within(dialog).getByTestId('unattended-chain-max-continuations'), '500');
    await user.keyboard('{Escape}');
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    expect(onSubmit).toHaveBeenCalledWith('0 0 * * 6', {
      target_kind: 'workspace_thread',
      conversation_uuid: THREAD,
      prompt: 'Run the nightly checks.',
      grant: { continue_step_limit: true, continue_output_limit: true, goal_check: true, chain: { max_continuations: 500 } },
    });
    expect(await screen.findByTestId('thread-target-grant-error')).toHaveTextContent('Most automatic continuations: the value is more than the platform maximum.');
    expect(onClose).not.toHaveBeenCalled();
  });
  it('keeps an edited grant when Runs switches away and back', async () => {
    const { onSubmit, user } = renderTargeted({}, true);
    await chooseThread(user);
    await user.click(screen.getByRole('button', { name: 'Run to completion settings' }));
    const dialog = screen.getByRole('dialog', { name: 'Run to completion' });
    await user.click(within(dialog).getByRole('button', { name: 'Change limits' }));
    await user.type(within(dialog).getByTestId('unattended-chain-max-continuations'), '7');
    await user.keyboard('{Escape}');
    await user.click(screen.getByLabelText('This pipeline'));
    await user.click(screen.getByLabelText('A handed-off workspace thread'));
    await user.click(screen.getByRole('button', { name: 'Run to completion settings' }));
    expect(within(screen.getByRole('dialog', { name: 'Run to completion' })).getByTestId('unattended-chain-line')).toHaveTextContent('up to 7 continuations');
    await user.keyboard('{Escape}');
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    await waitFor(() => expect(onSubmit).toHaveBeenCalledWith('0 0 * * 6', {
      target_kind: 'workspace_thread',
      conversation_uuid: THREAD,
      prompt: 'Run the nightly checks.',
      grant: { continue_step_limit: true, continue_output_limit: true, goal_check: true, chain: { max_continuations: 7 } },
    }));
  });

  it('shows a chain limit that is not valid, and does not save', async () => {
    const { onSubmit, onClose, user } = renderTargeted({}, true);
    await chooseThread(user);
    await user.click(screen.getByRole('button', { name: 'Run to completion settings' }));
    const dialog = screen.getByRole('dialog', { name: 'Run to completion' });
    await user.click(within(dialog).getByRole('button', { name: 'Change limits' }));
    await user.type(within(dialog).getByTestId('unattended-chain-max-continuations'), '5.5');
    await user.keyboard('{Escape}');
    expect(screen.getByTestId('thread-target-grant-invalid')).toHaveTextContent('A chain limit is not valid. Open the settings and correct it.');
    expect(screen.getByTestId('thread-target-grant')).toHaveAttribute('data-tone', 'invalid');
    await user.click(screen.getByRole('button', { name: 'Apply' }));
    expect(onSubmit).not.toHaveBeenCalled();
    expect(onClose).not.toHaveBeenCalled();
    expect(screen.getByTestId('thread-target-grant-invalid')).toBeInTheDocument();
  });
});
