/**
 * ui/canvas/Canvas.tsx — in-chat canvas for code / diagram / table editing,
 * ported from `apps/elitea-ui/src/components/Canvas.jsx` (C4 batch).
 *
 * Renders a syntax-highlighted code block (or unformatted content for
 * diagrams/tables) with an edit button that opens the canvas editor and a
 * copy button that copies the content to the clipboard.
 *
 * ── THE OPENER (issue 853) ────────────────────────────────────────────────
 * This component was ported complete and mounted by NOTHING, which is the
 * whole of that issue: a canvas carved out of an answer could be created,
 * edited and saved through the API, and no pixel on the chat surface could
 * reach it. `ApplicationAnswer` now renders one of these for every
 * `canvas_message` item in a stored answer, so the block IS the opener.
 *
 * Shape follows the legacy transcript rather than inventing one: a right-
 * aligned toolbar row (open + copy) above the canvas content, no wrapping
 * card. The one addition is `name` — the legacy row shows the canvas title
 * only while somebody is editing it, which leaves a stored canvas with no
 * label at all; the stored `item_details.name` ("Edit code", "Edit table")
 * is rendered instead, so the affordance says what it opens.
 */
import { useCallback, useMemo, useState } from 'react';

import { Box, IconButton, Typography } from '@mui/material';

const IconButtonAny = IconButton as React.ComponentType<any>;

import Tooltip from '@mui/material/Tooltip';

import type { CanvasEditorPresence } from '@/entities/canvas/model/types';
import { realCanvasEditors } from '@/entities/canvas/model/selectors';
import { t } from '@/shared/i18n';

import { EditingPlaceholder } from '../EditingPlaceholder';

export interface CanvasProps {
  /** The content to display — code for `type=code`, mermaid for diagrams, markdown for tables. */
  readonly content?: string;
  /** The canvas's stored name (`item_details.name`), captioned on the toolbar row. */
  readonly name?: string;
  /** Called when the user clicks the edit button — receives the canvas edit payload. */
  readonly onEdit?: (payload: CanvasEditPayload) => void;
  /** Canvas identity and positioning — bundled to stay within §3.5 prop/deps budget. */
  readonly canvasRef?: {
    readonly canvasId?: string;
    readonly messageItemId?: string | number;
    readonly startPos?: number;
    readonly endPos?: number;
  };
  /** The selected code block info (for block-level editing). */
  readonly selectedCodeBlockInfo?: CodeBlockInfo;
  /** Whether the message is currently streaming. */
  readonly isStreaming?: boolean;
  /** The programming language — `'markdown'`, `'javascript'`, `'python'`, etc. */
  readonly language?: string;
  /** Canvas type: `'code'`, `'diagram'`, `'table'`, or `'document'` (issue #879: rich-text prose, round-tripped as Markdown). */
  readonly type?: 'code' | 'diagram' | 'table' | 'document';
  /** List of editors currently working on this canvas. */
  readonly editors?: readonly CanvasEditorPresence[];
  /** Interaction UUID for canvas tracking. */
  readonly interaction_uuid?: string;
  /** Conversation UUID for canvas tracking. */
  readonly conversation_uuid?: string;
}

export interface CodeBlockInfo {
  readonly codeBlock: string;
  readonly language: string;
  readonly isBlock: boolean;
  readonly canvasId?: string;
  readonly messageItemId?: string | number;
  readonly blockId?: number;
  readonly viewOnly?: boolean;
  /** True while a brand-new canvas is being created for this block (canvasId not yet assigned). */
  readonly isCreatingCanvas?: boolean;
}

export interface CanvasEditPayload {
  /** Raw content from the code block. */
  readonly rawData: string;
  /** Parsed code from inside the triple-backtick fence. */
  readonly codeBlock: string;
  /** Language identifier for the editor. */
  readonly language: string;
  /** Whether this is a block-level edit. */
  readonly isBlock: boolean;
  readonly startPos?: number;
  readonly endPos?: number;
  readonly canvasId?: string;
  readonly messageItemId?: string | number;
  readonly blockId?: number;
  /** When true, only the current user can edit. */
  readonly viewOnly?: boolean;
}

/**
 * Renders a canvas — a syntax-highlighted code block or diagram/table with
 * inline edit and copy actions.
 *
 * Matches the baseline `Canvas.jsx` behaviour:
 * - Code blocks are wrapped in a fenced code fence and rendered with syntax highlighting.
 * - Diagrams (mermaid) and tables are rendered as plain markdown content.
 * - An edit button opens the canvas editor with the right language and payload.
 * - A copy button copies the content to the clipboard.
 * - Real-time editor presence is shown (filtered admin/system users).
 */
export function Canvas({
  content = '',
  name,
  onEdit,
  canvasRef,
  selectedCodeBlockInfo,
  isStreaming = false,
  language = 'markdown',
  type = 'code',
  editors = [],
  interaction_uuid,
  conversation_uuid,
}: CanvasProps): React.ReactElement {
  const { canvasId, messageItemId, startPos, endPos } = canvasRef ?? {};
  // Stable per-instance id (baseline: `useCheckIsBlockEditing`'s `useState(new Date().getTime())`)
  // — matched back against `selectedCodeBlockInfo.blockId` while a brand-new canvas this block
  // requested is still being created (canvasId not assigned yet).
  const [blockId] = useState(() => Date.now());
  // True when this block's canvas is the one currently open in the editor — either an existing
  // canvas (canvasId match) or a new one this block just requested (isCreatingCanvas + blockId match).
  const isBlockEditing = useMemo(
    () =>
      Boolean(
        (canvasId && canvasId === selectedCodeBlockInfo?.canvasId) ||
          (selectedCodeBlockInfo?.isCreatingCanvas && selectedCodeBlockInfo?.blockId === blockId),
      ),
    [canvasId, selectedCodeBlockInfo?.canvasId, selectedCodeBlockInfo?.isCreatingCanvas, selectedCodeBlockInfo?.blockId, blockId],
  );
  // Filter out admin/system editors (baseline: CANVAS_ADMIN_USER / CANVAS_SYSTEM_USER)
  const realEditors = useMemo(
    () =>
      realCanvasEditors(editors).filter(
        (editor) =>
          editor.userName !== '__admin__' && editor.userName !== '__system__',
      ),
    [editors],
  );

  const editingTitle = useMemo(
    () => {
      if (type === 'document') return t('features.chatMessages.canvas.block.editingDocument', 'Document editing...');
      if (type === 'code' && language !== 'mermaid') return t('features.chatMessages.canvas.block.editingCode', 'Code editing...');
      if (type === 'diagram' || language === 'mermaid') return t('features.chatMessages.canvas.block.editingDiagram', 'Diagram editing...');
      return t('features.chatMessages.canvas.block.editingTable', 'Table editing...');
    },
    [language, type],
  );

  const editButtonTitle = useMemo(
    () => {
      if (type === 'document') return t('features.chatMessages.canvas.block.openDocument', 'Edit document');
      if (type === 'code' && language !== 'mermaid') return t('features.chatMessages.canvas.block.openCode', 'Edit code');
      if (type === 'diagram' || language === 'mermaid') return t('features.chatMessages.canvas.block.openDiagram', 'Edit diagram');
      return t('features.chatMessages.canvas.block.openTable', 'Edit table');
    },
    [language, type],
  );

  // Wrap raw content in a fenced code block if it doesn't already start with one
  const realContent = useMemo(() => {
    if (!content.startsWith('```')) {
      switch (type) {
        case 'code':
          return `\`\`\`${language}\n${content}\n\`\`\`\n`;
        case 'diagram':
          return `\`\`\`mermaid\n${content}\n\`\`\`\n`;
        case 'table':
          return content;
        // A document's stored content IS the Markdown source — rendered as
        // prose here (the same `CanvasContent` markdown surface a table
        // uses), never fenced: fencing it would show the reader raw
        // Markdown syntax instead of the document they wrote.
        case 'document':
          return content;
        default:
          return content;
      }
    }
    return content;
  }, [content, language, type]);

  const onCopy = useCallback(async () => {
    try {
      await navigator.clipboard.writeText(content);
      // TODO: toast info — "The code has been copied into clipboard"
    } catch {
      // Clipboard write failed — non-fatal
    }
  }, [content]);

  const onClickEdit = useCallback(() => {
    onEdit?.({
      rawData: content,
      codeBlock: extraCodeFromBlock(content),
      language: type === 'table' ? 'markdownTable' : type === 'diagram' ? 'mermaid' : type === 'document' ? 'document' : language,
      isBlock: true,
      ...((canvasRef?.startPos ?? startPos) != null ? { startPos: canvasRef?.startPos ?? startPos } : {}),
      ...((canvasRef?.endPos ?? endPos) != null ? { endPos: canvasRef?.endPos ?? endPos } : {}),
      ...((canvasRef?.canvasId ?? canvasId) != null ? { canvasId: canvasRef?.canvasId ?? canvasId } : {}),
      ...((canvasRef?.messageItemId ?? messageItemId) != null ? { messageItemId: canvasRef?.messageItemId ?? messageItemId } : {}),
      // Baseline always sends the block's own stable id (`useCheckIsBlockEditing`'s `blockId`), not
      // `selectedCodeBlockInfo.blockId` (that field belongs to whichever block previously opened — a
      // new click here is a new edit request, matched back to THIS block instance via its own id).
      blockId,
      viewOnly: !!realEditors.length,
    });
  }, [onEdit, content, type, language, canvasRef, realEditors, messageItemId, canvasId, startPos, endPos, blockId]);

  // This block's canvas is already open for editing (by this user or elsewhere) — swap the
  // toolbar+content for a placeholder instead (baseline: Canvas.jsx's `!isBlockEditing` ternary).
  if (isBlockEditing) {
    return <EditingPlaceholder title={editingTitle} />;
  }

  // The label the toolbar carries: whoever else is editing wins, because that
  // is the state the reader must not act on blindly; otherwise the canvas's
  // own stored name, and only then the generic verb.
  const blockTitle = realEditors.length > 0 ? editingTitle : (name ?? editButtonTitle);

  return (
    <Box sx={{ width: '100%' }} data-testid="canvas-block">
      {/* Toolbar row */}
      <Box
        sx={{
          width: '100%',
          display: 'flex',
          flexDirection: 'row',
          justifyContent: 'space-between',
          alignItems: 'center',
          padding: '8px 0px 8px 8px',
          gap: '8px',
        }}
      >
        <Box sx={{ display: 'flex', alignItems: 'center', justifyContent: 'flex-start', gap: '8px', minWidth: 0 }}>
          {/* TODO: AuthorContainer — avatar row showing active editors */}
          <Typography
            variant="bodySmall"
            color="text.primary"
            data-testid="canvas-block-title"
            sx={{ overflow: 'hidden', whiteSpace: 'nowrap', textOverflow: 'ellipsis' }}
          >
            {blockTitle}
          </Typography>
        </Box>
        <Box sx={{ display: 'flex', alignItems: 'center', gap: '8px' }}>
        {onEdit && (
          <Tooltip title={realEditors.length > 0 ? t('features.chatMessages.canvas.block.watch', 'Watch editing') : editButtonTitle} placement="top">
            <span>
              <IconButtonAny
                variant="elitea"
                color="tertiary"
                size="small"
                onClick={onClickEdit}
                disabled={isStreaming}
                data-testid="canvas-block-open"
                aria-label={editButtonTitle}
              >
                ✏️
              </IconButtonAny>
            </span>
          </Tooltip>
        )}
        <Tooltip title={t('features.chatMessages.canvas.block.copy', 'Copy code')} placement="top">
          <IconButtonAny
            variant="elitea"
            color="tertiary"
            size="small"
            onClick={onCopy}
            data-testid="canvas-block-copy"
            aria-label={t('features.chatMessages.canvas.block.copy', 'Copy code')}
          >
            📋
          </IconButtonAny>
        </Tooltip>
        </Box>
      </Box>

      {/* Content area */}
      <CanvasContent
        content={realContent}
        {...(interaction_uuid != null && { interaction_uuid })}
        {...(conversation_uuid != null && { conversation_uuid })}
        {...(canvasId != null && { canvasId })}
        isStreaming={isStreaming}
      />
    </Box>
  );
}

/**
 * Renders the markdown/code content area.
 *
 * For code-type canvases the content is rendered inside a `<pre>` with a
 * syntax-highlighted code fence; for diagrams/tables it renders as plain
 * markdown (baseline delegates to `Markdown` component — a TODO for C4).
 */
function CanvasContent({
  content,
  interaction_uuid: _interaction_uuid,
  conversation_uuid: _conversation_uuid,
  canvasId: _canvasId,
  isStreaming: _isStreaming,
}: {
  readonly content: string;
  readonly interaction_uuid?: string | undefined;
  readonly conversation_uuid?: string | undefined;
  readonly canvasId?: string | undefined;
  readonly isStreaming?: boolean | undefined;
}): React.ReactElement {
  // TODO: replace with shared Markdown component once C3/C5 ships it
  // Baseline: <Markdown> component with interaction_uuid, conversation_uuid, canvasId props
  return (
    <Box
      component="pre"
      data-testid="canvas-block-content"
      // Theme tokens, not the port's literal `#f5f5f5`: this component was
      // mounted by nothing until issue 853, so a ground that only works in the
      // light theme had never been rendered against the dark one.
      sx={(theme) => ({
        background: theme.vars.palette.background.aiAnswerBkg,
        color: theme.vars.palette.text.secondary,
        borderRadius: theme.vars.shape.radiusMd,
        margin: 0,
        padding: '1rem',
        overflow: 'auto',
        whiteSpace: 'pre-wrap',
        fontSize: '0.875rem',
        fontFamily: 'monospace',
      })}
    >
      {content || ' '}
    </Box>
  );
}

/**
 * Extracts the code from inside a fenced code block.
 *
 * Ported from `extraCodeFromBlock` in `apps/elitea-ui/src/components/Canvas.jsx` —
 * strips the opening/closing triple-backtick fence and trims trailing empty lines.
 *
 * Exported for reuse by `CanvasEditor.tsx` (sibling file, same `ui/canvas/` slice),
 * which strips the same fence off incoming real-time sync payloads.
 */
export function extraCodeFromBlock(code: string): string {
  const trimmed = trimEmptyStringsAtEnd((code || '').split('\n'));
  // If the code starts with ``` and ends with ```, strip them
  if (trimmed.length > 2 && trimmed[0]?.startsWith('```') && trimmed[trimmed.length - 1]?.startsWith('```')) {
    return trimmed.slice(1, trimmed.length - 1).join('\n');
  }
  return code;
}

/**
 * Trims empty strings from the end of an array.
 *
 * Ported from the baseline helper at `apps/elitea-ui/src/components/Canvas.jsx` (C4).
 */
function trimEmptyStringsAtEnd(array: string[]): string[] {
  let endIndex = array.length - 1;
  while (endIndex >= 0 && array[endIndex] === '') endIndex--;
  return array.slice(0, endIndex + 1);
}
