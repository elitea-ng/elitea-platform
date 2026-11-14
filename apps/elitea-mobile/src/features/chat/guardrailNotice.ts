import type { GuardrailNotice } from '@/api/queries/guardrailNotices';

/**
 * The sentence for one guardrail notice (issue 2012): a pure function of the
 * typed notice. It names the guardrail and the kind of data found, never the
 * data. The wording matches the web chat.
 */

/** A notice that explains why the turn failed, so the generic failure line is redundant. */
export function explainsFailure(notice: GuardrailNotice): boolean {
  return notice.kind === 'blocked' || notice.kind === 'unavailable';
}

export function noticeText(notice: GuardrailNotice): string {
  const name = notice.guardrail;
  const found = notice.labels.join(', ');
  const output = notice.stage === 'output';
  switch (notice.kind) {
    case 'blocked':
      return output
        ? `The answer was blocked by the guardrail "${name}".`
        : `Your message was blocked by the guardrail "${name}". Remove the flagged content and send it again.`;
    case 'redacted':
      return output
        ? `Parts of the answer were redacted by the guardrail "${name}".`
        : `Parts of your message were redacted by the guardrail "${name}" before they reached the model.`;
    case 'warned':
      return output
        ? `The guardrail "${name}" warns that the answer holds sensitive content (${found}). It is shown unchanged.`
        : `The guardrail "${name}" warns that your message holds sensitive content (${found}). It was sent unchanged.`;
    default:
      return `The guardrail "${name}" could not run, so the request was stopped. Try again, or ask an administrator.`;
  }
}

/** The line a failed live turn shows when a guardrail stopped it: the notice itself carries the detail. */
export const GUARDRAIL_STOPPED = 'Stopped by a content guardrail.';
