/**
 * Visual parity with the baseline's `ChatHitlActions.jsx`: test ids, labels,
 * button order, the default message, and the decided (disabled) state.
 */
import { ThemeProvider } from '@mui/material/styles';
import { fireEvent, render, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { ChatHitlActions } from './ChatHitlActions';
import type { ChatHitlActionsProps, HitlInterrupt, HitlResumePayload } from './ChatHitlActions';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

const GENERIC: HitlInterrupt = {
  message: '',
  available_actions: ['approve', 'edit', 'reject', 'block_with_comment'],
  tool_call_id: 'call_1',
  interrupt_id: 'int_1',
};

const SENSITIVE: HitlInterrupt = {
  guardrail_type: 'sensitive_tool',
  available_actions: ['approve', 'reject', 'block_with_comment'],
  tool_name: 'delete_repo',
  action_label: 'Delete repository',
  tool_args: { repo: 'acme/app', token: '***' },
  policy_message: 'Deleting a repository is irreversible.',
};

function renderCard(props: Partial<ChatHitlActionsProps> & { hitlInterrupt: HitlInterrupt }) {
  return render(
    <ThemeProvider theme={theme}>
      <ChatHitlActions {...props} />
    </ThemeProvider>,
  );
}

describe('ChatHitlActions — baseline parity', () => {
  it('renders the generic panel with the default message and Approve, Edit, Reject, Block with Comment in order', () => {
    renderCard({ hitlInterrupt: GENERIC });

    const panel = screen.getByTestId('chat-hitl-actions-panel');
    expect(panel.textContent).toContain('Here are some results. Choose the action to proceed.');
    const labels = within(panel)
      .getAllByRole('button')
      .map((button) => button.textContent);
    expect(labels).toEqual(['Approve', 'Edit', 'Reject', 'Block with Comment']);
    expect(screen.getByTestId('chat-hitl-approve-button')).toBeTruthy();
    expect(screen.getByTestId('chat-hitl-reject-button')).toBeTruthy();
    expect(screen.getByTestId('chat-hitl-edit-button')).toBeTruthy();
  });

  it('renders the interrupt message as Markdown', () => {
    renderCard({ hitlInterrupt: { ...GENERIC, message: 'Run **this** step?' } });
    expect(screen.getByText('this').tagName).toBe('STRONG');
  });

  it('carries toolCallId and interruptId on every resume', () => {
    const onHitlResume = vi.fn();
    renderCard({ hitlInterrupt: GENERIC, toolCallId: 'call_1', interruptId: 'int_1', onHitlResume });

    fireEvent.click(screen.getByTestId('chat-hitl-approve-button'));
    fireEvent.click(screen.getByTestId('chat-hitl-edit-button'));
    fireEvent.change(screen.getByPlaceholderText(/Enter your edited message/), { target: { value: ' new ' } });
    fireEvent.click(screen.getByRole('button', { name: 'Submit Edit' }));

    const payloads = onHitlResume.mock.calls.map((call) => call[0] as HitlResumePayload);
    expect(payloads).toEqual([
      { action: 'approve', toolCallId: 'call_1', interruptId: 'int_1' },
      { action: 'edit', value: 'new', toolCallId: 'call_1', interruptId: 'int_1' },
    ]);
  });

  it('renders a decided pause with every control disabled instead of hiding it', () => {
    renderCard({ hitlInterrupt: { ...GENERIC, decided: true }, disabled: true });

    const buttons = within(screen.getByTestId('chat-hitl-actions-panel')).getAllByRole('button');
    expect(buttons).toHaveLength(4);
    buttons.forEach((button) => expect(button.hasAttribute('disabled')).toBe(true));
  });

  it('renders the sensitive card with Authorize, Block AND the comment control, and masked params', () => {
    const onHitlResume = vi.fn();
    renderCard({ hitlInterrupt: SENSITIVE, toolCallId: 'call_s', interruptId: 'int_s', onHitlResume });

    const panel = screen.getByTestId('sensitive-action-panel');
    expect(panel.textContent).toContain('⚠️ Sensitive Action Authorization Required');
    expect(panel.textContent).toContain('Agent is about to perform:');
    expect(panel.textContent).toContain('Delete repository');
    expect(panel.textContent).toContain('Deleting a repository is irreversible.');
    expect(screen.getByTestId('sensitive-action-block-with-comment-button')).toBeTruthy();

    fireEvent.click(screen.getByRole('button', { name: /Parameters/ }));
    expect(panel.textContent).toContain('token:***');

    fireEvent.click(screen.getByTestId('sensitive-action-authorize-button'));
    expect(onHitlResume).toHaveBeenCalledWith({ action: 'approve', toolCallId: 'call_s', interruptId: 'int_s' });
  });

  it('omits the comment control on a sensitive pause that does not offer it', () => {
    renderCard({ hitlInterrupt: { ...SENSITIVE, available_actions: ['approve', 'reject'] } });
    expect(screen.getByTestId('sensitive-action-block-button')).toBeTruthy();
    expect(screen.queryByTestId('sensitive-action-block-with-comment-button')).toBeNull();
  });

  it('renders nothing without an interrupt', () => {
    const { container } = renderCard({ hitlInterrupt: undefined as unknown as HitlInterrupt });
    expect(container.textContent).toBe('');
  });
});

describe('ChatHitlActions message hardening', () => {
  it('renders an interrupt message as Markdown without raw HTML (no beacon images from model text)', () => {
    const { container } = renderWithTheme(
      <ChatHitlActions hitlInterrupt={{ message: 'Check <img src="https://example.invalid/x.png"> **now**', available_actions: ['approve'] }} />,
    );
    expect(container.querySelector('img')).toBeNull();
    expect(container.querySelector('strong')?.textContent).toBe('now');
  });
});
