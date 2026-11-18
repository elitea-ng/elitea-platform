import { render, screen } from '@testing-library/react-native';

import type { GuardrailNotice } from '@/api/queries/guardrailNotices';
import { explainsFailure, GUARDRAIL_STOPPED, noticeText } from '@/features/chat/guardrailNotice';
import { GuardrailNoticeRows, LiveReply } from '@/features/chat/ThreadRows';
import { sampleLive } from '@/features/designGallery/chatFixtures';

const notice = (over: Partial<GuardrailNotice> = {}): GuardrailNotice => ({
  kind: 'redacted', stage: 'input', action: 'mask', guardrail: 'pii', detector: 'personal_data',
  labels: ['email'], requests: 1, first_seen_at: '2026-11-13T10:00:00Z', ...over,
});

describe('noticeText (issue 2012)', () => {
  it('names the guardrail and what it found, never a value', () => {
    expect(noticeText(notice())).toBe('Parts of your message were redacted by the guardrail "pii" before they reached the model.');
    expect(noticeText(notice({ stage: 'output' }))).toBe('Parts of the answer were redacted by the guardrail "pii".');
    expect(noticeText(notice({ kind: 'warned', action: 'warn', labels: ['email', 'iban'] }))).toBe(
      'The guardrail "pii" warns that your message holds sensitive content (email, iban). It was sent unchanged.',
    );
    expect(noticeText(notice({ kind: 'blocked', guardrail: 'keys' }))).toBe(
      'Your message was blocked by the guardrail "keys". Remove the flagged content and send it again.',
    );
    expect(noticeText(notice({ kind: 'blocked', stage: 'output', guardrail: 'keys' }))).toBe('The answer was blocked by the guardrail "keys".');
    expect(noticeText(notice({ kind: 'unavailable', guardrail: 'cloud', labels: [] }))).toBe(
      'The guardrail "cloud" could not run, so the request was stopped. Try again, or ask an administrator.',
    );
  });

  it('names no data when there are no labels, with no empty parentheses', () => {
    const text = noticeText(notice({ kind: 'warned', action: 'warn', labels: [] }));
    expect(text).toBe('The guardrail "pii" warns that your message holds sensitive content. It was sent unchanged.');
    expect(text).not.toContain('()');
  });

  it('only a block or an unavailable guardrail explains a failed turn', () => {
    expect(['blocked', 'unavailable', 'redacted', 'warned'].map((kind) => explainsFailure(notice({ kind })))).toEqual([true, true, false, false]);
  });
});

describe('the thread rows', () => {
  it('draws one line per notice under the answer', async () => {
    await render(<GuardrailNoticeRows notices={[notice(), notice({ kind: 'warned', guardrail: 'heads-up', action: 'warn' })]} />);
    expect(screen.getByTestId('guardrail-notice-redacted')).toBeTruthy();
    expect(screen.getByTestId('guardrail-notice-warned')).toBeTruthy();
    expect(screen.getByText(/redacted by the guardrail "pii"/)).toBeTruthy();
  });

  it('a failed live turn that a guardrail stopped says so instead of the generic failure', async () => {
    const base = sampleLive('thinking');
    const failed = { ...base, failure: 'turn' as const, turn: { ...base.turn, phase: 'failed' as const, error: { message: 'The agent could not finish this reply.', retryable: false } } };
    const props = { live: failed, author: 'Research', onRetry: jest.fn(), onReconnect: jest.fn(), onDismiss: jest.fn(), onOpenTool: jest.fn(), usageAvailable: true } as const;
    const view = await render(<LiveReply {...props} />);
    expect(screen.queryByText(GUARDRAIL_STOPPED)).toBeNull();
    await view.rerender(<LiveReply {...props} guardrailStopped />);
    expect(screen.getByText(GUARDRAIL_STOPPED)).toBeTruthy();
  });
});
