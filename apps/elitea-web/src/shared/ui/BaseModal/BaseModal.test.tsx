import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { BaseModal } from '.';

// MUI's `Dialog` renders through a portal to `document.body`. RTL's
// `render()` defaults `baseElement` to `document.body`, so the bound
// queries below (`getByRole`, `queryByText`, …) already see portaled
// content — no custom DOM traversal needed.

describe('BaseModal', () => {
  it('renders nothing to the DOM when closed', () => {
    const { queryByText } = renderWithTheme(
      <BaseModal
        open={false}
        title="Hidden"
        content="content"
      />,
    );
    expect(queryByText('Hidden')).not.toBeInTheDocument();
  });

  it('renders the title and content when open', () => {
    const { getByText } = renderWithTheme(
      <BaseModal
        open
        title="Delete agent"
        content="Are you sure?"
      />,
    );
    expect(getByText('Delete agent')).toBeInTheDocument();
    expect(getByText('Are you sure?')).toBeInTheDocument();
  });

  it('names its h2 heading and the dialog by the title alone, not "title Close"', () => {
    const { getByRole } = renderWithTheme(
      <BaseModal
        open
        title="Delete node?"
        content="c"
        onClose={() => {}}
      />,
    );
    expect(getByRole('heading', { level: 2, name: 'Delete node?' })).toBeInTheDocument();
    expect(getByRole('dialog', { name: 'Delete node?' })).toBeInTheDocument();
  });

  it('renders a node title as-is (bypassing the Typography wrapper)', () => {
    const { getByTestId } = renderWithTheme(
      <BaseModal
        open
        title={<span data-testid="custom-title">Custom</span>}
        content="content"
      />,
    );
    expect(getByTestId('custom-title')).toBeInTheDocument();
  });

  it('renders Cancel + Confirm by default when both onClose and onConfirm are given', () => {
    const { getByRole } = renderWithTheme(
      <BaseModal
        open
        title="t"
        content="c"
        onClose={() => {}}
        onConfirm={() => {}}
      />,
    );
    expect(getByRole('button', { name: 'Cancel' })).toBeInTheDocument();
    expect(getByRole('button', { name: 'Confirm' })).toBeInTheDocument();
  });

  it('omits the action bar entirely when there is no onConfirm and no actions override', () => {
    const { queryByRole } = renderWithTheme(
      <BaseModal
        open
        title="t"
        content="c"
        onClose={() => {}}
      />,
    );
    expect(queryByRole('button', { name: 'Cancel' })).not.toBeInTheDocument();
    expect(queryByRole('button', { name: 'Confirm' })).not.toBeInTheDocument();
  });

  it('calls onClose when the close button is clicked', async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    const { getByTestId } = renderWithTheme(
      <BaseModal
        open
        title="t"
        content="c"
        onClose={onClose}
        header={{ closeButtonDataTestId: 'close-btn' }}
      />,
    );
    await user.click(getByTestId('close-btn'));
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('calls onClose on Escape exactly once (not double-fired by Dialog + a manual handler)', async () => {
    const user = userEvent.setup();
    const onClose = vi.fn();
    renderWithTheme(
      <BaseModal
        open
        title="t"
        content="c"
        onClose={onClose}
      />,
    );
    await user.keyboard('{Escape}');
    expect(onClose).toHaveBeenCalledTimes(1);
  });

  it('calls onConfirm when Confirm is clicked, and disables it while confirming', async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    const { getByRole, rerender } = renderWithTheme(
      <BaseModal
        open
        title="t"
        content="c"
        onClose={() => {}}
        onConfirm={onConfirm}
      />,
    );
    await user.click(getByRole('button', { name: 'Confirm' }));
    expect(onConfirm).toHaveBeenCalledTimes(1);

    rerender(
      <BaseModal
        open
        title="t"
        content="c"
        onClose={() => {}}
        onConfirm={onConfirm}
        actions={{ confirming: true }}
      />,
    );
    expect(getByRole('button', { name: 'Confirm' })).toBeDisabled();
  });

  it('activates Confirm on Enter when focused (keyboard path)', async () => {
    const user = userEvent.setup();
    const onConfirm = vi.fn();
    const { getByRole } = renderWithTheme(
      <BaseModal
        open
        title="t"
        content="c"
        onClose={() => {}}
        onConfirm={onConfirm}
      />,
    );
    getByRole('button', { name: 'Confirm' }).focus();
    await user.keyboard('{Enter}');
    expect(onConfirm).toHaveBeenCalledTimes(1);
  });

  it('uses the supplied confirm/cancel button text', () => {
    const { getByRole } = renderWithTheme(
      <BaseModal
        open
        title="t"
        content="c"
        onClose={() => {}}
        onConfirm={() => {}}
        actions={{ confirmText: 'Delete', cancelText: 'Keep' }}
      />,
    );
    expect(getByRole('button', { name: 'Delete' })).toBeInTheDocument();
    expect(getByRole('button', { name: 'Keep' })).toBeInTheDocument();
  });

  it('renders a fully custom actions node instead of Cancel/Confirm', () => {
    const { getByTestId, queryByRole } = renderWithTheme(
      <BaseModal
        open
        title="t"
        content="c"
        actions={{ node: <span data-testid="custom-actions">custom</span> }}
      />,
    );
    expect(getByTestId('custom-actions')).toBeInTheDocument();
    expect(queryByRole('button', { name: 'Confirm' })).not.toBeInTheDocument();
  });

  it('suppresses the action bar in fullscreen complex variant even with onConfirm', () => {
    const { queryByRole } = renderWithTheme(
      <BaseModal
        open
        title="t"
        content="c"
        onClose={() => {}}
        onConfirm={() => {}}
        variant="complex"
        fullscreen
      />,
    );
    expect(queryByRole('button', { name: 'Confirm' })).not.toBeInTheDocument();
  });

  it('renders the footer node outside the content area when supplied', () => {
    const { getByTestId } = renderWithTheme(
      <BaseModal
        open
        title="t"
        content="c"
        footer={<div data-testid="modal-footer">footer</div>}
      />,
    );
    expect(getByTestId('modal-footer')).toBeInTheDocument();
  });

  it('forwards data-testid to the dialog root', () => {
    const { getByTestId } = renderWithTheme(
      <BaseModal
        open
        title="t"
        content="c"
        data-testid="my-modal"
      />,
    );
    expect(getByTestId('my-modal')).toBeInTheDocument();
  });
});
