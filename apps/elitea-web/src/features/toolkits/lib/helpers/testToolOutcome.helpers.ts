/**
 * Maps `../../api/toolkitTestRun.ts`'s `TestToolkitToolOutcome` onto the two
 * surfaces `useToolkitChat.hooks.ts` already owns: the chat transcript
 * (`generateMockMessageTemplate`, same helper `buildRunToolErrorMessage`
 * there already uses for a dispatch failure) and the caller's `onError`
 * Snackbar (`features/toolkits/ui/IndexesTab.tsx`'s local-Snackbar pattern —
 * see `../hooks/useToolkitChatDispatch.hooks.ts`'s `reportMissingTransport`
 * for the existing precedent this one extends).
 *
 * Split out of `useToolkitChat.hooks.ts` purely to stay under the §3.5
 * 400-line/12-complexity budgets (see that file's own module doc comment).
 *
 * A SETTLED run (`'ok'`/`'toolError'` — the tool was reached) becomes a
 * transcript message, because that is what the panel already shows for
 * every other completed run. A REFUSAL (`'unsupportedToolkit'`/
 * `'unknownTool'`/`'timeout'`/`'failure'` — the run never reached the tool)
 * becomes a Snackbar message instead, because it is not something the tool
 * said — it is the platform declining to try, the same class of fact
 * `reportMissingTransport` already reports this way.
 */
import { t } from '@/shared/i18n';

import type { TestToolkitToolOutcome } from '../../api/toolkitTestRun';
import { generateMockMessageTemplate } from '../../indexes/lib/helpers/indexChat.helpers';
import type { ToolkitChatMessage } from '../hooks/useToolkitChat.types';

/** `JSON.stringify` can throw on a circular structure; the worker never sends one (canonical JSON, `response.go`'s own guarantee), but a defensive caller does not have to trust that from the UI layer. */
function formatResult(result: unknown): string {
  try {
    return JSON.stringify(result, null, 2);
  } catch {
    return String(result);
  }
}

/**
 * The transcript message for a SETTLED run (`'ok'`/`'toolError'`), or
 * `undefined` for a refusal — see this module's own doc comment for why the
 * two surfaces are split.
 */
export function buildTestToolChatMessage(toolName: string, outcome: TestToolkitToolOutcome): ToolkitChatMessage | undefined {
  if (outcome.kind === 'ok') {
    const heading = t('features.toolkits.toolkitChat.testTool.resultHeading', 'Tool result:');
    const body = outcome.truncated
      ? t('features.toolkits.toolkitChat.testTool.truncated', 'The result was too large to display.')
      : `\`\`\`json\n${formatResult(outcome.result)}\n\`\`\``;
    return generateMockMessageTemplate(`${heading}\n\n${body}`, 'toolkit');
  }
  if (outcome.kind === 'toolError') {
    const content = t('features.toolkits.toolkitChat.testTool.toolError', 'Tool "{{toolName}}" reported an error:\n\n{{message}}', {
      toolName,
      message: outcome.message,
    });
    return generateMockMessageTemplate(content, 'toolkit');
  }
  return undefined;
}

/**
 * The Snackbar message for a REFUSAL (`'unsupportedToolkit'`/
 * `'unknownTool'`/`'timeout'`/`'failure'`), or `undefined` for a settled run
 * — see this module's own doc comment for why the two surfaces are split.
 */
export function describeTestToolRefusal(toolName: string, outcome: TestToolkitToolOutcome): string | undefined {
  switch (outcome.kind) {
    case 'unsupportedToolkit':
      return t('features.toolkits.toolkitChat.testTool.unsupportedToolkit', 'This toolkit type cannot run tools on this deployment: {{message}}', {
        message: outcome.message,
      });
    case 'unknownTool':
      return t('features.toolkits.toolkitChat.testTool.unknownTool', 'Tool "{{toolName}}" is not available on this toolkit: {{message}}', {
        toolName,
        message: outcome.message,
      });
    case 'timeout':
      return outcome.taskId === undefined
        ? t('features.toolkits.toolkitChat.testTool.timeoutNoTask', 'The tool did not finish in time. It may still be running.')
        : t('features.toolkits.toolkitChat.testTool.timeout', 'The tool is still running (task {{taskId}}). Its result is not ready yet.', {
            taskId: outcome.taskId,
          });
    case 'failure':
      return t('features.toolkits.toolkitChat.testTool.failure', 'The tool could not be run: {{message}}', { message: outcome.message });
    case 'ok':
    case 'toolError':
      return undefined;
    default:
      return outcome satisfies never;
  }
}
