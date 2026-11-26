/**
 * A Local work turn's side talk (ADR-0061 decisions 3d, 3e and 3f, #3064 #3065), drawn with the chat's side talk
 * (`TurnTranscript`'s `sideTalk`): a hand-off "Mira → CI Doctor: …", a consult "Mira asked CI Doctor: …" and its reply,
 * folded into "Side conversation (n)"; a loop guard's stop as the chat's platform line. The rows are the host's
 * excerpts; the committed side messages carry the full texts.
 */
import type { ReactNode } from 'react';

import { MESSAGE_CLASS } from '@elitea/client-core/conversation';

import { SideTalkRows, type ChatMessage } from '@/features/chat-messages';
import type { WorkspaceTurn } from '@/features/workspace';
import { t } from '@/shared/i18n';

type TurnView = WorkspaceTurn['view'];
type SideItem = NonNullable<TurnView['sideTalk']>[number];

function row(id: string, messageClass: ChatMessage['messageClass'], content: string, author = '', addressees: readonly string[] = []): ChatMessage {
  return { id, role: 'assistant', name: author, authorName: author, addresseeNames: addressees, messageClass, content, createdAt: '' };
}

function anAgent(name: string): string {
  return name === '' ? t('chat.presence.anotherAgent', 'another agent') : name;
}

/** The row's text: a consult's question says who asked whom; a failed reply says so. */
function contentOf(item: SideItem): string {
  const callee = anAgent(item.addressees[0] ?? '');
  if (item.kind === 'consult_question') {
    const where = item.cloud === true ? ` ${t('workspace.consult.cloud', '(in the cloud, without the folder)')}` : '';
    return t('workspace.consult.asked', '{{caller}} asked {{callee}}: {{question}}', { caller: anAgent(item.author), callee, question: item.text }) + where;
  }
  if (item.kind === 'consult_reply' && item.failed === true) return t('workspace.consult.failed', 'No reply: {{reason}}', { reason: item.text });
  return item.text;
}

/** The turn's side rows as chat rows, then the guard's stop line. */
function sideTalkRows(view: TurnView): ChatMessage[] {
  const rows = (view.sideTalk ?? []).map((item) => {
    // A consult's question carries its "asked" sentence, so its label is the caller alone.
    const addressees = item.kind === 'consult_question' ? [] : item.addressees.map(anAgent);
    return row(item.key, MESSAGE_CLASS.AgentToAgent, contentOf(item), anAgent(item.author), addressees);
  });
  const stopped = view.floorStopped;
  if (stopped?.guard === true) rows.push(row('guard-stop', MESSAGE_CLASS.System, stopped.message));
  return rows;
}

export function TurnSideTalk({ view }: { view: TurnView }): ReactNode {
  return <SideTalkRows rows={sideTalkRows(view)} testId="workspace-side-talk" />;
}
