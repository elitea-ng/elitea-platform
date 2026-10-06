import type { ChatMessage } from '@/features/chat/messageModel';

/**
 * What the message actions sheet offers for one message (long-press, or the
 * "More" button under it). Pure: the screen passes what the workspace allows.
 *
 * - Copy, Copy code and Share need `allow_share_out` (client contract 1.3
 *   policy; on by default, an enterprise admin can turn it off). With it off
 *   the sheet says why instead of offering them.
 * - Like / Dislike (with an optional comment) need client contract 1.3 and a
 *   persisted answer (its group uuid).
 * - Read aloud is on-device text to speech: no data leaves the device.
 * - Regenerate is offered on the newest answer only, when the screen can
 *   regenerate it.
 */

export type MessageActionKey = 'copy' | 'copy_code' | 'share' | 'read_aloud' | 'like' | 'dislike' | 'regenerate';

export interface CodeBlock {
  /** The fence's info string (`python`), or null. */
  language: string | null;
  code: string;
}

/** The longest code block offered for copying (a larger one is still copied whole by "Copy"). */
const MAX_CODE_BLOCKS = 20;

/**
 * Fenced code blocks of a Markdown text (``` or ~~~, CommonMark style: the
 * closing fence uses the same character, at least as long, and an unclosed
 * block runs to the end). Indented code blocks are not offered.
 */
export function codeBlocks(markdown: string): CodeBlock[] {
  const lines = markdown.split('\n');
  const blocks: CodeBlock[] = [];
  let open: { char: string; length: number; indent: number; language: string | null; body: string[] } | null = null;
  for (const line of lines) {
    if (open === null) {
      const match = /^( {0,3})(`{3,}|~{3,})\s*([^`\s]*)[^`]*$/.exec(line);
      if (match) {
        open = { char: match[2][0], length: match[2].length, indent: match[1].length, language: match[3] || null, body: [] };
      }
      continue;
    }
    const close = /^ {0,3}(`{3,}|~{3,})\s*$/.exec(line);
    if (close && close[1][0] === open.char && close[1].length >= open.length) {
      blocks.push({ language: open.language, code: open.body.join('\n') });
      open = null;
      if (blocks.length >= MAX_CODE_BLOCKS) break;
      continue;
    }
    // Strip up to the opening fence's indentation, as CommonMark does.
    const strip = Math.min(open.indent, line.length - line.trimStart().length);
    open.body.push(line.slice(strip));
  }
  if (open !== null && blocks.length < MAX_CODE_BLOCKS) blocks.push({ language: open.language, code: open.body.join('\n') });
  return blocks.filter((b) => b.code.trim() !== '');
}

export interface ActionContext {
  /** `allow_share_out` of the workspace policy. */
  shareOut: boolean;
  /** The workspace speaks client contract 1.3 (feedback). */
  contract13: boolean;
  /** Online, and the transcript is not the offline copy. */
  online: boolean;
  /** On-device speech is available. */
  canSpeak: boolean;
  /** The screen can regenerate this message (the newest answer, idle, 1.1+). */
  canRegenerate: boolean;
}

export interface ActionPlan {
  actions: MessageActionKey[];
  codeBlocks: CodeBlock[];
  /** Shown when copy and share are hidden by the workspace policy. */
  shareOutNote: string | null;
}

export const SHARE_OUT_OFF = 'Your workspace does not allow copying or sharing from the app.';

export function planActions(message: Pick<ChatMessage, 'role' | 'text' | 'uuid' | 'isStreaming'>, context: ActionContext): ActionPlan {
  const hasText = message.text.trim() !== '';
  const blocks = message.role === 'assistant' && hasText ? codeBlocks(message.text) : [];
  const actions: MessageActionKey[] = [];
  if (hasText && context.shareOut) {
    actions.push('copy');
    if (blocks.length > 0) actions.push('copy_code');
    actions.push('share');
  }
  if (hasText && context.canSpeak) actions.push('read_aloud');
  const rateable = message.role === 'assistant' && message.uuid !== null && !message.isStreaming && context.contract13 && context.online;
  if (rateable) actions.push('like', 'dislike');
  if (message.role === 'assistant' && context.canRegenerate) actions.push('regenerate');
  return {
    actions,
    codeBlocks: context.shareOut ? blocks : [],
    shareOutNote: hasText && !context.shareOut ? SHARE_OUT_OFF : null,
  };
}

/** A label for a code block in the sheet ("Copy python code", "Copy code 2"). */
export function codeBlockLabel(block: CodeBlock, index: number, count: number): string {
  if (block.language) return count > 1 ? `Copy ${block.language} code (${index + 1})` : `Copy ${block.language} code`;
  return count > 1 ? `Copy code (${index + 1})` : 'Copy code';
}
