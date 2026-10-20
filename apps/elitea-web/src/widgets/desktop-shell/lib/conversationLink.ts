import { getConfig } from '@/shared/config';

/** The deployment's link to a conversation (Copy Link); `null` without a conversation or a configured deployment. */
export function conversationLink(conversationId: string): string | null {
  const config = getConfig();
  if (config.status !== 'ok' || conversationId === '') return null;
  return `${config.config.vite_server_url.replace(/\/$/, '')}/chat/${conversationId}`;
}

/** Copy text to the system clipboard; a refusal is not an error worth showing. */
export function copyText(text: string): void {
  void navigator.clipboard?.writeText(text).catch(() => undefined);
}
