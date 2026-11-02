import { getConfig } from '@/shared/config';
import { SearchParams } from '@/shared/lib/params';

/** The deployment's link to a conversation (Copy Link); `null` without a conversation or a configured deployment. */
export function conversationLink(conversationId: string): string | null {
  const config = getConfig();
  if (config.status !== 'ok' || conversationId === '') return null;
  return `${config.config.vite_server_url.replace(/\/$/, '')}/chat/${conversationId}`;
}

/** The app's base path: `''` in dev, the deployment's `vite_base_uri` otherwise. */
function appBasename(): string {
  if (import.meta.env.DEV) return '';
  const config = getConfig();
  return config.status === 'ok' ? config.config.vite_base_uri.replace(/\/$/, '') : '';
}

/**
 * The browser app's link to a conversation in its project: the chat route
 * opens it in that project for any member (`shared_chat`). The desktop has no
 * browser origin of its own, so it shares the deployment's link instead.
 */
export function conversationShareLink(conversation: { readonly id: string; readonly name: string }, projectId: string | undefined): string | null {
  if (import.meta.env.MODE === 'desktop' || projectId === undefined) return conversationLink(conversation.id);
  const name = conversation.name.replaceAll(' ', '+');
  return `${window.location.origin}${appBasename()}/${projectId}/chat/${conversation.id}?${SearchParams.Name}=${name}&${SearchParams.SharedChat}=1`;
}

/** Copy text to the system clipboard; a refusal is not an error worth showing. */
export function copyText(text: string): void {
  void navigator.clipboard?.writeText(text).catch(() => undefined);
}
