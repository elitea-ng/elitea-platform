/**
 * The answer's stored `message_items`, in the order they are stored — and the
 * canvas opener that issue 853 is about.
 *
 * ── WHAT WAS MISSING ──────────────────────────────────────────────────────
 * `ApplicationAnswer` filtered `message_items` down to `text_message` and
 * rendered those alone. A canvas is carved OUT of a text item — the create
 * route deletes the item it splits and writes text / `canvas_message` / text
 * in its place — so from the moment a user made a canvas, the transcript
 * showed the two halves of their answer and silently dropped the middle. No
 * component anywhere in this app mounted a control that could open it again,
 * which is why the canvas journey next door drives the canvas through
 * `page.request` and says so in its header.
 *
 * This module is the branch that was missing. It walks the items ONCE, in
 * their stored order, and hands back a discriminated list; the component
 * below renders a `Markdown` for a text item and the already-ported `Canvas`
 * block for a canvas item. Interleaving matters: text / canvas / text is
 * exactly what the split writes, and rendering the canvas after all the text
 * would put the user's own paragraph in the wrong place.
 *
 * ── READING THE WIRE ──────────────────────────────────────────────────────
 * `MessageItemWire` deliberately models only `id` and `item_details.content`
 * (see `entities/message/lib/wire.ts`), so every other field is read
 * defensively here, the same way `ApplicationAnswer` and `UserMessage`
 * already read `item_type`. The canvas shape is not invented: the
 * conversation read serves `item_details` as `{id, item_type, uuid, name,
 * canvas_type, canvas_content, code_language, editors, latest_version:{id,
 * canvas_content, code_language, created_at}}`.
 *
 * The canvas is addressed by `item_details.uuid` — the message ITEM's uuid,
 * which is the id `PUT /elitea_core/canvas/prompt_lib/{p}/{canvasID}` takes
 * and the id the editor's own socket room is named after. An item whose
 * details carry no uuid renders read-only rather than opening an editor that
 * could not save: a PUT to an undefined id is a 404 that would surface to
 * the user as a lost edit, which is the failure this whole area exists to
 * stop.
 */
import type { ReactNode } from 'react';

import { Canvas } from '../canvas/Canvas';
import type { CanvasEditPayload, CodeBlockInfo } from '../canvas/Canvas';

import { Markdown } from '@/shared/ui/Markdown';

import type { ChatMessage } from '../../lib/convertMessagesToChatHistory';

const TEXT_MESSAGE_ITEM_TYPE = 'text_message';
const CANVAS_MESSAGE_ITEM_TYPE = 'canvas_message';

/** The three canvas kinds `Canvas` renders; anything else the server invents falls back to `code`. */
const CANVAS_TYPES = ['code', 'diagram', 'table'] as const;
type CanvasKind = (typeof CANVAS_TYPES)[number];

/** One rendered entry of an answer: a paragraph of text, or a canvas document. */
export type AnswerItem =
  | { readonly kind: 'text'; readonly key: string; readonly content: string }
  | {
      readonly kind: 'canvas';
      readonly key: string;
      readonly content: string;
      readonly name: string | undefined;
      readonly language: string;
      readonly canvasType: CanvasKind;
      readonly canvasId: string | undefined;
      readonly messageItemId: number | undefined;
    };

function asRecord(value: unknown): Record<string, unknown> | undefined {
  return typeof value === 'object' && value !== null ? (value as Record<string, unknown>) : undefined;
}

function asString(value: unknown): string | undefined {
  return typeof value === 'string' && value !== '' ? value : undefined;
}

function canvasKind(value: unknown): CanvasKind {
  const named = CANVAS_TYPES.find((kind) => kind === value);
  return named ?? 'code';
}

/**
 * One canvas item, read out of its `item_details`.
 *
 * `latest_version` first and the flat keys second: the version IS the
 * document, and the flat `canvas_content` beside it is the same string kept
 * for the REST normaliser. Reading only the flat one would show a stale
 * document the moment the two ever disagree.
 */
function readCanvasItem(raw: Record<string, unknown>, index: number): AnswerItem {
  const details = asRecord(raw['item_details']) ?? {};
  const version = asRecord(details['latest_version']);
  const content = asString(version?.['canvas_content']) ?? asString(details['canvas_content']) ?? '';
  const language = asString(version?.['code_language']) ?? asString(details['code_language']) ?? 'markdown';
  const canvasId = asString(details['uuid']) ?? asString(raw['uuid']);
  const itemId = typeof raw['id'] === 'number' ? raw['id'] : undefined;
  return {
    kind: 'canvas',
    key: canvasId ?? `canvas-item-${String(index)}`,
    content,
    name: asString(details['name']),
    language,
    canvasType: canvasKind(details['canvas_type']),
    canvasId,
    messageItemId: itemId,
  };
}

/** The answer's items, in stored order, as the two kinds this surface renders. */
export function readAnswerItems(messageItems: ChatMessage['messageItems']): readonly AnswerItem[] {
  const raw = (messageItems ?? []) as unknown as readonly Record<string, unknown>[];
  const items: AnswerItem[] = [];
  raw.forEach((item, index) => {
    if (item['item_type'] === TEXT_MESSAGE_ITEM_TYPE) {
      const details = asRecord(item['item_details']);
      items.push({
        kind: 'text',
        key: asString(item['uuid']) ?? `text-item-${String(index)}`,
        content: asString(details?.['content']) ?? '',
      });
      return;
    }
    if (item['item_type'] === CANVAS_MESSAGE_ITEM_TYPE) items.push(readCanvasItem(item, index));
  });
  return items;
}

/** @public Props for `AnswerMessageItems`. */
export interface AnswerMessageItemsProps {
  readonly items: readonly AnswerItem[];
  /** Opens the canvas editor for one block. Omitted, the block renders with no open control — which is what a surface with no editor mounted must do. */
  readonly onEditCanvas?: ((payload: CanvasEditPayload) => void) | undefined;
  /** The block currently open in the editor, so the one being edited shows its placeholder instead of a second copy. */
  readonly selectedCodeBlockInfo?: CodeBlockInfo | undefined;
  /** While the answer streams, the open control is disabled — the document is still being written. */
  readonly isStreaming?: boolean;
  /** The word TTS is reading, for the text items only. */
  readonly spokenRange?: { readonly start: number; readonly end: number } | undefined;
}

/** Renders an answer's stored items in order: markdown for text, a canvas block for a canvas. */
export function AnswerMessageItems({
  items,
  onEditCanvas,
  selectedCodeBlockInfo,
  isStreaming = false,
  spokenRange,
}: AnswerMessageItemsProps): ReactNode {
  return (
    <>
      {items.map((item) =>
        item.kind === 'text' ? (
          <Markdown key={item.key} spokenRange={spokenRange}>
            {item.content}
          </Markdown>
        ) : (
          <Canvas
            key={item.key}
            content={item.content}
            isStreaming={isStreaming}
            language={item.language}
            type={item.canvasType}
            {...(item.name !== undefined ? { name: item.name } : {})}
            {...(onEditCanvas !== undefined && item.canvasId !== undefined ? { onEdit: onEditCanvas } : {})}
            {...(selectedCodeBlockInfo !== undefined ? { selectedCodeBlockInfo } : {})}
            canvasRef={{
              ...(item.canvasId !== undefined ? { canvasId: item.canvasId } : {}),
              ...(item.messageItemId !== undefined ? { messageItemId: item.messageItemId } : {}),
            }}
          />
        ),
      )}
    </>
  );
}
