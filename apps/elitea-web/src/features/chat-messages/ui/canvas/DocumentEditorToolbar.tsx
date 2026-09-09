/**
 * ui/canvas/DocumentEditorToolbar.tsx — the formatting row for the `document`
 * canvas pane (issue #879). Split out of `DocumentEditor.tsx` for the same
 * §3.5 file-length/prop-budget reasons the table pane's grid config is split
 * from its own header controls.
 *
 * One `editor` prop rather than one callback per button: every button both
 * DISPATCHES a command and READS `editor.isActive(...)` for its pressed
 * state, so a caller would otherwise have to thread both directions through
 * twelve separate props. Tiptap's own `Editor` instance is already the
 * single source of truth for both; re-deriving that here in parallel state
 * would be the second-source-of-truth bug this codebase's docs keep naming.
 */
import type { ReactNode } from 'react';

import type { Editor } from '@tiptap/core';

import FormatBoldIcon from '@mui/icons-material/FormatBold';
import FormatItalicIcon from '@mui/icons-material/FormatItalic';
import CodeIcon from '@mui/icons-material/Code';
import FormatListBulletedIcon from '@mui/icons-material/FormatListBulleted';
import FormatListNumberedIcon from '@mui/icons-material/FormatListNumbered';
import FormatQuoteIcon from '@mui/icons-material/FormatQuote';
import LinkIcon from '@mui/icons-material/Link';
import LinkOffIcon from '@mui/icons-material/LinkOff';
import TableChartIcon from '@mui/icons-material/TableChart';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import ToggleButton from '@mui/material/ToggleButton';
import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';

const IconButtonAny = IconButton as React.ComponentType<any>;
const ToggleButtonAny = ToggleButton as React.ComponentType<any>;

/** One heading level button — H1/H2/H3, matching the "headings" acceptance criterion without a full block-type dropdown. */
const HEADING_LEVELS = [1, 2, 3] as const;

export interface DocumentEditorToolbarProps {
  readonly editor: Editor | null;
  readonly readOnly?: boolean | undefined;
}

/** Prompts for a URL and applies/removes the link mark on the current selection — the "basic" link affordance the issue asks for; a full picker dialog is out of scope for this pass. */
function toggleLink(editor: Editor): void {
  if (editor.isActive('link')) {
    editor.chain().focus().unsetLink().run();
    return;
  }
  // eslint-disable-next-line no-alert -- deliberate minimal UI: no link-picker dialog exists in this feature yet.
  const url = window.prompt(t('features.chatMessages.canvas.document.linkPrompt', 'Link URL'));
  if (url) editor.chain().focus().extendMarkRange('link').setLink({ href: url }).run();
}

/** The formatting row: bold/italic/code, H1-H3, bullet/ordered list, blockquote, link, and a 3x3 table insert. Renders nothing while `editor` has not mounted yet (the tiptap chunk loads async — see `./DocumentEditor.tsx`). */
export function DocumentEditorToolbar({ editor, readOnly }: DocumentEditorToolbarProps): ReactNode {
  if (!editor) return null;
  const disabled = readOnly === true;

  return (
    <Box
      data-testid="canvas-document-toolbar"
      sx={{ display: 'flex', flexWrap: 'wrap', alignItems: 'center', gap: '4px', padding: '4px 0' }}
    >
      <Tooltip title={t('features.chatMessages.canvas.document.bold', 'Bold')} placement="top">
        <span>
          <ToggleButtonAny
            value="bold"
            size="small"
            selected={editor.isActive('bold')}
            disabled={disabled}
            onChange={() => editor.chain().focus().toggleBold().run()}
            aria-label={t('features.chatMessages.canvas.document.bold', 'Bold')}
            data-testid="canvas-document-bold"
          >
            <FormatBoldIcon fontSize="small" />
          </ToggleButtonAny>
        </span>
      </Tooltip>
      <Tooltip title={t('features.chatMessages.canvas.document.italic', 'Italic')} placement="top">
        <span>
          <ToggleButtonAny
            value="italic"
            size="small"
            selected={editor.isActive('italic')}
            disabled={disabled}
            onChange={() => editor.chain().focus().toggleItalic().run()}
            aria-label={t('features.chatMessages.canvas.document.italic', 'Italic')}
            data-testid="canvas-document-italic"
          >
            <FormatItalicIcon fontSize="small" />
          </ToggleButtonAny>
        </span>
      </Tooltip>
      <Tooltip title={t('features.chatMessages.canvas.document.code', 'Inline code')} placement="top">
        <span>
          <ToggleButtonAny
            value="code"
            size="small"
            selected={editor.isActive('code')}
            disabled={disabled}
            onChange={() => editor.chain().focus().toggleCode().run()}
            aria-label={t('features.chatMessages.canvas.document.code', 'Inline code')}
            data-testid="canvas-document-code"
          >
            <CodeIcon fontSize="small" />
          </ToggleButtonAny>
        </span>
      </Tooltip>

      {HEADING_LEVELS.map((level) => (
        <Tooltip key={level} title={t('features.chatMessages.canvas.document.heading', 'Heading {{level}}', { level })} placement="top">
          <span>
            <ToggleButtonAny
              value={`h${String(level)}`}
              size="small"
              selected={editor.isActive('heading', { level })}
              disabled={disabled}
              onChange={() => editor.chain().focus().toggleHeading({ level }).run()}
              aria-label={t('features.chatMessages.canvas.document.heading', 'Heading {{level}}', { level })}
              data-testid={`canvas-document-heading-${String(level)}`}
            >
              {`H${String(level)}`}
            </ToggleButtonAny>
          </span>
        </Tooltip>
      ))}

      <Tooltip title={t('features.chatMessages.canvas.document.bulletList', 'Bullet list')} placement="top">
        <span>
          <ToggleButtonAny
            value="bulletList"
            size="small"
            selected={editor.isActive('bulletList')}
            disabled={disabled}
            onChange={() => editor.chain().focus().toggleBulletList().run()}
            aria-label={t('features.chatMessages.canvas.document.bulletList', 'Bullet list')}
            data-testid="canvas-document-bullet-list"
          >
            <FormatListBulletedIcon fontSize="small" />
          </ToggleButtonAny>
        </span>
      </Tooltip>
      <Tooltip title={t('features.chatMessages.canvas.document.orderedList', 'Numbered list')} placement="top">
        <span>
          <ToggleButtonAny
            value="orderedList"
            size="small"
            selected={editor.isActive('orderedList')}
            disabled={disabled}
            onChange={() => editor.chain().focus().toggleOrderedList().run()}
            aria-label={t('features.chatMessages.canvas.document.orderedList', 'Numbered list')}
            data-testid="canvas-document-ordered-list"
          >
            <FormatListNumberedIcon fontSize="small" />
          </ToggleButtonAny>
        </span>
      </Tooltip>
      <Tooltip title={t('features.chatMessages.canvas.document.blockquote', 'Quote')} placement="top">
        <span>
          <ToggleButtonAny
            value="blockquote"
            size="small"
            selected={editor.isActive('blockquote')}
            disabled={disabled}
            onChange={() => editor.chain().focus().toggleBlockquote().run()}
            aria-label={t('features.chatMessages.canvas.document.blockquote', 'Quote')}
            data-testid="canvas-document-blockquote"
          >
            <FormatQuoteIcon fontSize="small" />
          </ToggleButtonAny>
        </span>
      </Tooltip>

      <Tooltip
        title={
          editor.isActive('link')
            ? t('features.chatMessages.canvas.document.unlink', 'Remove link')
            : t('features.chatMessages.canvas.document.link', 'Link')
        }
        placement="top"
      >
        <span>
          <IconButtonAny
            variant="elitea"
            color="tertiary"
            size="small"
            disabled={disabled}
            onClick={() => toggleLink(editor)}
            aria-label={t('features.chatMessages.canvas.document.link', 'Link')}
            data-testid="canvas-document-link"
          >
            {editor.isActive('link') ? <LinkOffIcon fontSize="small" /> : <LinkIcon fontSize="small" />}
          </IconButtonAny>
        </span>
      </Tooltip>

      <Tooltip title={t('features.chatMessages.canvas.document.insertTable', 'Insert table')} placement="top">
        <span>
          <IconButtonAny
            variant="elitea"
            color="tertiary"
            size="small"
            disabled={disabled}
            onClick={() => editor.chain().focus().insertTable({ rows: 3, cols: 3, withHeaderRow: true }).run()}
            aria-label={t('features.chatMessages.canvas.document.insertTable', 'Insert table')}
            data-testid="canvas-document-insert-table"
          >
            <TableChartIcon fontSize="small" />
          </IconButtonAny>
        </span>
      </Tooltip>
    </Box>
  );
}
