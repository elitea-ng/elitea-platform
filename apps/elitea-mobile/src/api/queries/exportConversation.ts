import { sessionApiClient, type ApiClient } from '@/api/client';
import { ApiError } from '@/api/errors';

/**
 * The whole transcript as a Markdown document (client contract 1.3,
 * exportConversation `format=md`). The server refuses (400) a conversation
 * too long for one document rather than send a truncated one. Online only;
 * the text is handed to the share sheet through a temporary file and never
 * kept (src/features/chat/export/shareTranscript.ts).
 */

/** A bound on what the app holds in memory for one export. */
export const MAX_EXPORT_CHARS = 16 * 1024 * 1024;

/** `cancelled`: the workspace changed (switch, sign-out) while the transcript downloaded. */
export type ExportFailure = 'too_long' | 'not_allowed' | 'not_available' | 'offline' | 'cancelled' | 'failed';

export class ExportError extends Error {
  constructor(readonly reason: ExportFailure, readonly status?: number) {
    super(`export: ${reason}`);
    this.name = 'ExportError';
  }
}

export function exportErrorText(error: unknown): string {
  if (!(error instanceof ExportError)) return "Couldn't export this conversation. Try again.";
  switch (error.reason) {
    case 'too_long':
      return 'This conversation is too long to export from the app. Export it from the web app.';
    case 'not_allowed':
      return "You can't export this conversation.";
    case 'not_available':
      return 'This workspace does not export conversations to mobile yet.';
    case 'offline':
      return "Couldn't reach the workspace. Check your connection and try again.";
    case 'cancelled':
      return 'The workspace changed, so nothing was exported.';
    default:
      return "Couldn't export this conversation. Try again.";
  }
}

export interface ExportedTranscript {
  markdown: string;
  /** A safe file name (one path segment, `.md`). */
  fileName: string;
}

/** `Content-Disposition` → a safe `.md` file name, else `conversation-<id>.md`. */
export function exportFileName(disposition: string | null, conversationId: string): string {
  let name: string | null = null;
  const star = /filename\*\s*=\s*(?:UTF-8|utf-8)''([^;]+)/.exec(disposition ?? '');
  if (star) {
    try {
      name = decodeURIComponent(star[1].trim());
    } catch {
      name = null;
    }
  }
  if (name === null) {
    const plain = /filename\s*=\s*("([^"]*)"|[^;]+)/.exec(disposition ?? '');
    name = plain ? (plain[2] ?? plain[1]).trim() : null;
  }
  const base = (name ?? '')
    .replace(/\.(md|markdown)$/i, '')
    .replace(/[^\p{L}\p{N}._ -]+/gu, '_')
    .replace(/^[.\s_]+|[.\s_]+$/g, '')
    .slice(0, 80);
  return `${base !== '' ? base : `conversation-${conversationId.replace(/[^0-9A-Za-z-]/g, '').slice(0, 40) || 'export'}`}.md`;
}

export async function fetchTranscriptMarkdown(
  projectId: number,
  conversationId: string,
  client: ApiClient = sessionApiClient(),
): Promise<ExportedTranscript> {
  if (!Number.isSafeInteger(projectId) || projectId <= 0 || !/^[0-9A-Za-z-]{1,64}$/.test(conversationId)) {
    throw new ExportError('failed');
  }
  let result;
  try {
    result = await client.GET('/elitea_core/conversation_export/prompt_lib/{project_id}/{conversation_id}', {
      params: { path: { project_id: String(projectId), conversation_id: conversationId }, query: { format: 'md' } },
      parseAs: 'text',
    });
  } catch (error) {
    if (error instanceof ApiError && (error.kind === 'network' || error.kind === 'timeout')) throw new ExportError('offline');
    throw error;
  }
  const { data, response } = result;
  const status = response.status;
  if (!response.ok) {
    if (status === 400) throw new ExportError('too_long', status);
    if (status === 403) throw new ExportError('not_allowed', status);
    if (status === 404 || status === 501) throw new ExportError('not_available', status);
    throw new ExportError('failed', status);
  }
  const markdown = typeof data === 'string' ? data : '';
  if (markdown.trim() === '' || markdown.length > MAX_EXPORT_CHARS) throw new ExportError('failed');
  return { markdown, fileName: exportFileName(response.headers.get('Content-Disposition'), conversationId) };
}
