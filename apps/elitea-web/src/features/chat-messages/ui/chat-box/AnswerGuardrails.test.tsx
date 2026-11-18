/**
 * Issue 2012: a message the gateway blocked, redacted or warned about says so
 * in the chat, with the guardrail's name and what it found, never the value.
 * A block replaces the generic runtime error.
 */
import { screen, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { resetGuardrailNoticeCache, mayHaveGuardrailNotice } from '../../model/useGuardrailNotices';
import { AnswerGuardrails } from './AnswerGuardrails';

const URL = '/api/v2/elitea_core/guardrail_notices/prompt_lib/3/run-1';
/** The turn's verdicts are read this long after it settles, so a test waits past it. */
const SETTLE = { timeout: 5000 };

const notice = (over: Record<string, unknown>) => ({
  kind: 'redacted', stage: 'input', action: 'mask', guardrail: 'pii', detector: 'personal_data',
  labels: ['email'], requests: 1, first_seen_at: '2026-11-13T10:00:00Z', ...over,
});

let asked = 0;
function serve(notices: unknown[]): void {
  server.use(http.get(URL, () => {
    asked += 1;
    return HttpResponse.json({ run_id: 'run-1', notices });
  }));
}

const live = { executionId: 'run-1' } as const;
const failedBlock = { taskId: 'run-1', exception: 'The model service rejected this request.', failureCode: 'MODEL_REQUEST_REJECTED' } as const;

describe('AnswerGuardrails', () => {
  beforeEach(() => {
    asked = 0;
    configureGeneratedClient({ baseUrl: '/api/v2' });
    resetGuardrailNoticeCache();
  });
  afterEach(() => { resetGeneratedClient(); });

  it('says a message was redacted, naming the guardrail and never a value', async () => {
    serve([notice({})]);
    renderWithTheme(<AnswerGuardrails answer={live} projectId="3" settled />);
    const el = await screen.findByTestId('guardrail-notice-redacted', {}, SETTLE);
    expect(el).toHaveTextContent('Parts of your message were redacted by the guardrail "pii" before they reached the model.');
    expect(asked).toBe(1);
  });

  it('warns without hiding the answer error trace', async () => {
    serve([notice({ kind: 'warned', action: 'warn', guardrail: 'heads-up', labels: ['email', 'iban'] })]);
    renderWithTheme(<AnswerGuardrails answer={live} projectId="3" settled><p>kept</p></AnswerGuardrails>);
    expect(await screen.findByTestId('guardrail-notice-warned', {}, SETTLE)).toHaveTextContent(
      'The guardrail "heads-up" warns that your message holds sensitive content (email, iban). It was sent unchanged.');
    expect(screen.getByText('kept')).toBeInTheDocument();
  });

  it('says nothing of what was found when it has no labels, and prints no empty parentheses', async () => {
    serve([notice({ kind: 'warned', action: 'warn', guardrail: 'heads-up', labels: [] })]);
    renderWithTheme(<AnswerGuardrails answer={live} projectId="3" settled />);
    const el = await screen.findByTestId('guardrail-notice-warned', {}, SETTLE);
    expect(el).toHaveTextContent('The guardrail "heads-up" warns that your message holds sensitive content. It was sent unchanged.');
    expect(el.textContent).not.toContain('()');
  });

  it('asks once more when the first read is empty (the verdict was not written yet)', async () => {
    server.use(http.get(URL, () => {
      asked += 1;
      return HttpResponse.json({ run_id: 'run-1', notices: asked === 1 ? [] : [notice({ kind: 'warned', action: 'warn' })] });
    }));
    renderWithTheme(<AnswerGuardrails answer={live} projectId="3" settled />);
    expect(await screen.findByTestId('guardrail-notice-warned', {}, { timeout: 8000 })).toBeInTheDocument();
    expect(asked).toBe(2);
  }, 12000);

  it('asks again after a failed read, and does not keep the failure for good', async () => {
    server.use(http.get(URL, () => {
      asked += 1;
      return asked === 1
        ? HttpResponse.json({ error: 'boom' }, { status: 500 })
        : HttpResponse.json({ run_id: 'run-1', notices: [notice({})] });
    }));
    const first = renderWithTheme(<AnswerGuardrails answer={live} projectId="3" settled />);
    expect(await screen.findByTestId('guardrail-notice-redacted', {}, { timeout: 8000 })).toBeInTheDocument();
    first.unmount();
    // Found notices are remembered: a remount does not ask a third time.
    renderWithTheme(<AnswerGuardrails answer={live} projectId="3" settled />);
    expect(screen.getByTestId('guardrail-notice-redacted')).toBeInTheDocument();
    expect(asked).toBe(2);
  }, 12000);

  it('replaces the generic runtime error of a blocked message', async () => {
    serve([notice({ kind: 'blocked', action: 'block', guardrail: 'keys', labels: ['github-token'] })]);
    renderWithTheme(<AnswerGuardrails answer={failedBlock} projectId="3" settled><p>generic runtime error</p></AnswerGuardrails>);
    expect(screen.getByText('generic runtime error')).toBeInTheDocument();
    expect(await screen.findByTestId('guardrail-notice-blocked', {}, SETTLE)).toHaveTextContent(
      'Your message was blocked by the guardrail "keys". Remove the flagged content and send it again.');
    await waitFor(() => { expect(screen.queryByText('generic runtime error')).not.toBeInTheDocument(); });
  });

  it('names a blocked answer', async () => {
    serve([notice({ kind: 'blocked', stage: 'output', action: 'block', guardrail: 'no-secrets' })]);
    renderWithTheme(<AnswerGuardrails answer={failedBlock} projectId="3" settled />);
    expect(await screen.findByTestId('guardrail-notice-blocked', {}, SETTLE)).toHaveTextContent('The answer was blocked by the guardrail "no-secrets".');
  });

  it('says a closed guardrail that could not run stopped the request', async () => {
    serve([notice({ kind: 'unavailable', action: 'block', guardrail: 'cloud', labels: [] })]);
    renderWithTheme(<AnswerGuardrails answer={{ ...failedBlock, failureCode: 'MODEL_UNAVAILABLE' }} projectId="3" settled><p>generic runtime error</p></AnswerGuardrails>);
    expect(await screen.findByTestId('guardrail-notice-unavailable', {}, SETTLE)).toHaveTextContent(
      'The guardrail "cloud" could not run, so the request was stopped. Try again, or ask an administrator.');
    expect(screen.queryByText('generic runtime error')).not.toBeInTheDocument();
  });

  it('keeps the generic error when the failure was not a guardrail', async () => {
    serve([]);
    renderWithTheme(<AnswerGuardrails answer={failedBlock} projectId="3" settled><p>generic runtime error</p></AnswerGuardrails>);
    await waitFor(() => { expect(asked).toBe(1); }, SETTLE);
    expect(screen.getByText('generic runtime error')).toBeInTheDocument();
    expect(screen.queryByTestId('guardrail-notices')).not.toBeInTheDocument();
  });

  it('does not ask while streaming, for a stored answer, or for another failure', () => {
    const base = { projectId: '3', runId: 'run-1', settled: true, failed: false, failureCode: undefined, live: true };
    expect(mayHaveGuardrailNotice(base)).toBe(true);
    expect(mayHaveGuardrailNotice({ ...base, settled: false })).toBe(false);
    expect(mayHaveGuardrailNotice({ ...base, live: false })).toBe(false);
    expect(mayHaveGuardrailNotice({ ...base, failed: true, failureCode: 'MODEL_TIMEOUT' })).toBe(false);
    expect(mayHaveGuardrailNotice({ ...base, failed: true, live: false, failureCode: 'MODEL_REQUEST_REJECTED' })).toBe(true);
    expect(mayHaveGuardrailNotice({ ...base, runId: undefined })).toBe(false);
  });

  it('never breaks the answer when the read fails', async () => {
    server.use(http.get(URL, () => { asked += 1; return HttpResponse.json({ error: 'boom' }, { status: 500 }); }));
    renderWithTheme(<AnswerGuardrails answer={failedBlock} projectId="3" settled><p>generic runtime error</p></AnswerGuardrails>);
    await waitFor(() => { expect(asked).toBe(1); }, SETTLE);
    expect(screen.getByText('generic runtime error')).toBeInTheDocument();
  });
});
