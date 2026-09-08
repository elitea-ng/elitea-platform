/**
 * `describeRunError`/`buildRunToolErrorMessage` — split out of
 * `../hooks/useToolkitChat.hooks.ts`'s `executeRunTool` catch block into
 * their own module so that file has room for the REST test-tool outcome
 * wiring (`onTestToolOutcome`, `../hooks/useToolkitChatDispatch.hooks.ts`'s
 * "fifth case") without breaching the §3.5 400-line file budget. No
 * behaviour changed by the move.
 */
import { generateMockMessageTemplate } from '../../indexes/lib/helpers/indexChat.helpers';
import type { ToolkitChatMessage } from '../hooks/useToolkitChat.types';

function describeRunError(error: unknown): string {
  if (error instanceof Error) return error.message;
  if (typeof error === 'string') return error;
  try {
    return JSON.stringify(error);
  } catch {
    return 'Unknown error';
  }
}

/** The catch-block half of `executeRunTool`, split out to stay under the §3.5 complexity budget. */
export function buildRunToolErrorMessage(tool: string, error: unknown): ToolkitChatMessage {
  const errorMessage = describeRunError(error);
  return generateMockMessageTemplate(
    `❌ Failed to execute tool "${tool}"\n\n**Error:** ${errorMessage}\n\nPlease check your toolkit configuration and try again.`,
    'toolkit',
  );
}
