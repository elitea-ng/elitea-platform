/**
 * The sentence for one guardrail notice (issue 2012). A pure function of the
 * typed notice, so web, the desktop shell and the tests read one wording. It
 * names the guardrail and the kind of data found, never the data.
 */
import { t } from '@/shared/i18n';
import type { RunGuardrailNotice } from '@/shared/api/generated/model';

export type GuardrailNoticeSeverity = 'error' | 'warning' | 'info';

export function guardrailNoticeSeverity(kind: string): GuardrailNoticeSeverity {
  if (kind === 'blocked' || kind === 'unavailable') return 'error';
  return kind === 'warned' ? 'warning' : 'info';
}

/** Whether a notice explains why the turn failed, so the generic error is redundant. */
export function guardrailNoticeExplainsFailure(notices: readonly RunGuardrailNotice[]): boolean {
  return notices.some((notice) => notice.kind === 'blocked' || notice.kind === 'unavailable');
}

function guardrailLabelList(notice: RunGuardrailNotice): string {
  return notice.labels.join(', ');
}

export function guardrailNoticeText(notice: RunGuardrailNotice): string {
  const name = notice.guardrail;
  const found = guardrailLabelList(notice);
  const output = notice.stage === 'output';
  switch (notice.kind) {
    case 'blocked':
      return output
        ? t('chatMessages.guardrail.blockedOutput', 'The answer was blocked by the guardrail "{{name}}".', { name })
        : t('chatMessages.guardrail.blockedInput', 'Your message was blocked by the guardrail "{{name}}". Remove the flagged content and send it again.', { name });
    case 'redacted':
      return output
        ? t('chatMessages.guardrail.redactedOutput', 'Parts of the answer were redacted by the guardrail "{{name}}".', { name })
        : t('chatMessages.guardrail.redactedInput', 'Parts of your message were redacted by the guardrail "{{name}}" before they reached the model.', { name });
    case 'warned':
      return output
        ? t('chatMessages.guardrail.warnedOutput', 'The guardrail "{{name}}" warns that the answer holds sensitive content ({{found}}). It is shown unchanged.', { name, found })
        : t('chatMessages.guardrail.warnedInput', 'The guardrail "{{name}}" warns that your message holds sensitive content ({{found}}). It was sent unchanged.', { name, found });
    default:
      return t('chatMessages.guardrail.unavailable', 'The guardrail "{{name}}" could not run, so the request was stopped. Try again, or ask an administrator.', { name });
  }
}
