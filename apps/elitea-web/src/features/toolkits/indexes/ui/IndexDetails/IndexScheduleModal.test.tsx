import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { IndexScheduleModal } from './IndexScheduleModal';

describe('IndexScheduleModal', () => {
  it('is not rendered when closed', () => {
    const { queryByText } = renderWithTheme(
      <IndexScheduleModal
        open={false}
        onClose={vi.fn()}
        onSubmit={vi.fn()}
      />,
    );
    expect(queryByText('Schedule settings')).not.toBeInTheDocument();
  });

  it('seeds the field from the cron prop when opened and previews it', () => {
    const { getByText } = renderWithTheme(
      <IndexScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        cron="0 0 * * 6"
      />,
    );
    expect(getByText('Schedule settings')).toBeInTheDocument();
    expect(getByText('At 00:00, only on Saturday')).toBeInTheDocument();
  });

  it('submits the current cron expression and credentials, then closes, on Apply (no credentials field required)', async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const onClose = vi.fn();

    const { getByRole } = renderWithTheme(
      <IndexScheduleModal
        open
        onClose={onClose}
        onSubmit={onSubmit}
        cron="0 0 * * 6"
      />,
    );

    await user.click(getByRole('button', { name: 'Apply' }));

    expect(onSubmit).toHaveBeenCalledWith('0 0 * * 6', undefined);
    expect(onClose).toHaveBeenCalled();
  });

  it('rejects a sub-daily cron with the index-specific daily-floor message', async () => {
    const user = userEvent.setup();
    const { getByLabelText, getByRole, getByText } = renderWithTheme(
      <IndexScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        cron="0 * * * *"
      />,
    );

    await user.click(getByLabelText('Advanced'));
    expect(getByText('Frequency cannot be more than once per day')).toBeInTheDocument();
    expect(getByRole('button', { name: 'Apply' })).toBeDisabled();
  });

  it('switches to Advanced raw-text mode', async () => {
    const user = userEvent.setup();
    const { getByLabelText, getByRole } = renderWithTheme(
      <IndexScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        cron="0 0 * * 6"
      />,
    );

    await user.click(getByLabelText('Advanced'));
    expect(getByRole('textbox')).toHaveValue('0 0 * * 6');
  });

  it('does not render a credentials slot when credentialsData is absent, even with a renderer supplied', () => {
    const renderCredentialsSelect = vi.fn(() => <div>credentials-slot</div>);
    const { queryByText } = renderWithTheme(
      <IndexScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={vi.fn()}
        cron="0 0 * * 6"
        renderCredentialsSelect={renderCredentialsSelect}
      />,
    );
    expect(queryByText('credentials-slot')).not.toBeInTheDocument();
    expect(renderCredentialsSelect).not.toHaveBeenCalled();
  });

  /* elitea_issues: #3192 — a toolkit type with no credentials schema (e.g. Artifact) must be able to enable scheduled indexing without being forced through a credential picker it does not have. */
  it('submits successfully with no credential selected when the toolkit type has no credentialsData at all', async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const { getByRole } = renderWithTheme(
      <IndexScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={onSubmit}
        cron="0 0 * * 6"
      />,
    );
    await user.click(getByRole('button', { name: 'Apply' }));
    expect(onSubmit).toHaveBeenCalled();
  });

  it('renders the injected credentials slot (with the right props) when credentialsData is present, and blocks Apply until it supplies a value', async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const { getByText, getByRole } = renderWithTheme(
      <IndexScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={onSubmit}
        cron="0 0 * * 6"
        credentialsData={{ description: 'GitHub token', configuration_types: ['github'] }}
        renderCredentialsSelect={(slotProps) => (
          <div>
            <span>credentials-slot:{slotProps.label}</span>
            <span>onlyPublic:{String(slotProps.onlyPublic)}</span>
          </div>
        )}
      />,
    );

    expect(getByText('credentials-slot:GitHub token')).toBeInTheDocument();
    expect(getByText('onlyPublic:true')).toBeInTheDocument();

    await user.click(getByRole('button', { name: 'Apply' }));
    expect(onSubmit).not.toHaveBeenCalled();
  });

  it('submits once the injected slot supplies a credentials value', async () => {
    const user = userEvent.setup();
    const onSubmit = vi.fn();
    const { getByRole } = renderWithTheme(
      <IndexScheduleModal
        open
        onClose={vi.fn()}
        onSubmit={onSubmit}
        cron="0 0 * * 6"
        credentialsData={{ description: 'GitHub token' }}
        renderCredentialsSelect={(slotProps) => (
          <button
            type="button"
            onClick={() => slotProps.onChange('cred-1')}
          >
            pick-credential
          </button>
        )}
      />,
    );

    await user.click(getByRole('button', { name: 'pick-credential' }));
    await user.click(getByRole('button', { name: 'Apply' }));
    expect(onSubmit).toHaveBeenCalledWith('0 0 * * 6', 'cred-1');
  });
});
