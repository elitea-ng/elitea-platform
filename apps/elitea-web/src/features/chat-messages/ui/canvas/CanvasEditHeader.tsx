/**
 * ui/canvas/CanvasEditHeader.tsx — header bar for the canvas editor panel,
 * ported from `apps/elitea-ui/src/pages/NewChat/CanvasEditHeader.jsx` (C4 batch).
 *
 * Shows the title, undo/redo, copy, regenerate, delete, language select,
 * and table-editing buttons (add column, add row, delete rows/cols, import).
 */
import { useMemo } from 'react';

import { Box, IconButton, Typography } from '@mui/material';

const IconButtonAny = IconButton as React.ComponentType<
  React.ComponentProps<typeof IconButton> & { variant?: string }
>;

import Tooltip from '@mui/material/Tooltip';

import { t } from '@/shared/i18n';


import { CANVAS_LANGUAGE_OPTIONS } from './canvasLanguageOptions';
import { CanvasTableControls } from './table/CanvasTableControls';
import type { CanvasEditHeaderTable } from './table/canvasTableTypes';

/** Action callbacks grouped to stay within §3.5 prop budget. */
export interface CanvasEditHeaderActions {
  readonly onClose?: (() => void) | undefined;
  readonly onUndo?: (() => void) | undefined;
  readonly disableUndo?: boolean | undefined;
  readonly onRedo?: (() => void) | undefined;
  readonly disableRedo?: boolean | undefined;
  readonly onCopy?: (() => void) | undefined;
  readonly onRegenerate?: (() => void) | undefined;
  readonly onDelete?: (() => void) | undefined;
  /** Expands the editor to the whole viewport, or brings it back. Omitted, no control is rendered. */
  readonly onToggleFullScreen?: (() => void) | undefined;
  /** Which way the toggle above currently points — it is a two-state control, not a one-way expand. */
  readonly isFullScreen?: boolean | undefined;
}

/** Table-editing actions grouped to stay within §3.5 prop budget. */

/** Language select config grouped to stay within §3.5 prop budget. */
export interface CanvasEditHeaderLangSelect {
  readonly showLangSelect?: boolean | undefined;
  readonly onChangeLanguage?: ((language: string) => void) | undefined;
  readonly language?: string | undefined;
  readonly disableLanguageSelect?: boolean | undefined;
}

/** @public Props for `CanvasEditHeader`. */
export interface CanvasEditHeaderProps {
  /** Display title for the editor. */
  readonly title?: string;
  /** Action callbacks (undo, redo, copy, delete, etc.). */
  readonly actions?: CanvasEditHeaderActions;
  /** Table-editing actions. */
  readonly table?: CanvasEditHeaderTable;
  /** Whether this is a whole-message canvas. */
  readonly isThisWholeMessage?: boolean | undefined;
  /** Language selector configuration. */
  readonly langSelect?: CanvasEditHeaderLangSelect;
  /** When true, all action buttons are disabled. */
  readonly disabledAll?: boolean | undefined;
  /** Called when the user closes the editor (shorthand for actions?.onClose). */
  readonly onClose?: (() => void) | undefined;
}

/**
 * Renders the header bar for the canvas editor.
 *
 * Matches the baseline `CanvasEditHeader.jsx` layout:
 * - Left: close button + title (truncated with ellipsis)
 * - Right: undo, redo, copy, regenerate (whole-message), delete (whole-message),
 *   language select, and the table-editing cluster (`./table/CanvasTableControls`,
 *   which renders nothing unless `table.isTableEditing`)
 * - Action buttons are disabled when `disabledAll` is true
 */
export function CanvasEditHeader({
  title = 'Edit response',
  actions,
  table,
  isThisWholeMessage,
  langSelect,
  disabledAll,
  onClose: topLevelOnClose,
}: CanvasEditHeaderProps): React.ReactElement {
  const {
    onClose: actionsOnClose,
    onUndo,
    disableUndo = false,
    onRedo,
    disableRedo = false,
    onCopy,
    onRegenerate,
    onDelete,
    onToggleFullScreen,
    isFullScreen = false,
  } = actions ?? {};

  const onClose = topLevelOnClose ?? actionsOnClose;


  const {
    showLangSelect,
    onChangeLanguage,
    language = 'text',
    disableLanguageSelect,
  } = langSelect ?? {};

  // Normalize language name for display (baseline: cpp → c++, js → javascript, ts → typescript)
  const finalLanguage = useMemo(
    () => {
      if (language === 'cpp') return 'c++';
      if (language === 'js') return 'javascript';
      if (language === 'ts') return 'typescript';
      return language ?? 'text';
    },
    [language],
  );

  // Language options — matches CodeMirrorEditorHelpers.languageOptions from the baseline
  // (apps/elitea-ui/src/[fsd]/shared/lib/helpers/codeMirrorEditor.helpers.js:63-236).
  const languageOptions = CANVAS_LANGUAGE_OPTIONS;

  return (
    <Box
      sx={{
        display: 'flex',
        flexDirection: 'row',
        justifyContent: 'space-between',
        alignItems: 'center',
        paddingTop: '4px',
        paddingBottom: '4px',
        gap: '8px',
        paddingRight: '8px',
        boxSizing: 'border-box',
        height: '36px',
      }}
    >
      {/* Left side: close button + title */}
      <Box
        sx={{
          display: 'flex',
          flexDirection: 'row',
          justifyContent: 'flex-start',
          gap: '8px',
          alignItems: 'center',
        }}
      >
        {onClose && (
          <IconButtonAny
            variant="elitea"
            color="tertiary"
            size="small"
            onClick={onClose}
            data-testid="canvas-edit-close"
            aria-label="Close editor"
          >
            ✕
          </IconButtonAny>
        )}
        <Typography
          variant="bodyMedium"
          color="text.secondary"
          sx={{
            flex: 1,
            overflow: 'hidden',
            whiteSpace: 'nowrap',
            textOverflow: 'ellipsis',
          }}
        >
          {title}
        </Typography>
      </Box>

      {/* Right side: action buttons */}
      <Box
        sx={{
          display: 'flex',
          flexDirection: 'row',
          justifyContent: 'flex-end',
          gap: '8px',
          alignItems: 'center',
        }}
      >
        {/* Undo */}
        {onUndo && (
          <Tooltip title="Undo" placement="top">
            <span>
              <IconButtonAny
                variant="elitea"
                color="tertiary"
                size="small"
                onClick={onUndo}
                data-testid="canvas-edit-undo"
                disabled={disableUndo || disabledAll}
                aria-label="Undo"
              >
                ↩
              </IconButtonAny>
            </span>
          </Tooltip>
        )}

        {/* Redo */}
        {onRedo && (
          <Tooltip title="Redo" placement="top">
            <span>
              <IconButtonAny
                variant="elitea"
                color="tertiary"
                size="small"
                onClick={onRedo}
                data-testid="canvas-edit-redo"
                disabled={disableRedo || disabledAll}
                aria-label="Redo"
              >
                ↪
              </IconButtonAny>
            </span>
          </Tooltip>
        )}

        {/*
          Full screen. It sits with the other always-available controls rather
          than with the table cluster: a diagram and a long code document need
          the room as much as a wide table does.

          NOT disabled by `disabledAll`. That flag means "this document cannot
          be EDITED" — a read-only canvas, or one still being created — and a
          reader who cannot type still has to be able to see the whole of what
          somebody else is writing.
        */}
        {onToggleFullScreen && (
          <Tooltip
            title={isFullScreen
              ? t('features.chatMessages.canvas.editor.exitFullScreen', 'Exit full screen')
              : t('features.chatMessages.canvas.editor.fullScreen', 'Full screen')}
            placement="top"
          >
            <span>
              <IconButtonAny
                variant="elitea"
                color="tertiary"
                size="small"
                onClick={onToggleFullScreen}
                data-testid="canvas-edit-fullscreen"
                aria-pressed={isFullScreen}
                aria-label={isFullScreen
                  ? t('features.chatMessages.canvas.editor.exitFullScreen', 'Exit full screen')
                  : t('features.chatMessages.canvas.editor.fullScreen', 'Full screen')}
              >
                {isFullScreen ? '⤡' : '⤢'}
              </IconButtonAny>
            </span>
          </Tooltip>
        )}

        {/* Copy */}
        {onCopy && (
          <Tooltip title="Copy" placement="top">
            <span>
              <IconButtonAny
                variant="elitea"
                color="tertiary"
                size="small"
                onClick={onCopy}
                data-testid="canvas-edit-copy"
                aria-label="Copy"
              >
                📋
              </IconButtonAny>
            </span>
          </Tooltip>
        )}

        {/* Regenerate (whole-message only) */}
        {isThisWholeMessage && onRegenerate && (
          <Tooltip title="Regenerate" placement="top">
            <span>
              <IconButtonAny
                variant="elitea"
                color="tertiary"
                size="small"
                onClick={onRegenerate}
                disabled={disabledAll}
                aria-label="Regenerate"
              >
                🔄
              </IconButtonAny>
            </span>
          </Tooltip>
        )}

        {/* Delete (whole-message only) */}
        {isThisWholeMessage && onDelete && (
          <Tooltip title="Delete the message" placement="top">
            <span>
              <IconButtonAny
                variant="elitea"
                color="tertiary"
                size="small"
                onClick={onDelete}
                disabled={disabledAll}
                aria-label="Delete the message"
              >
                🗑
              </IconButtonAny>
            </span>
          </Tooltip>
        )}

        {/* Language selector */}
        {showLangSelect && onChangeLanguage && (
          <Box>
            <select
              value={finalLanguage}
              onChange={(e) => onChangeLanguage(e.target.value)}
              disabled={disabledAll || disableLanguageSelect}
              style={{
                padding: '0.25rem 0.5rem',
                borderRadius: '4px',
                border: '1px solid',
                borderColor: 'divider',
                fontSize: '0.875rem',
                background: disabledAll ? '#f0f0f0' : undefined,
                cursor: disabledAll || disableLanguageSelect ? 'not-allowed' : 'pointer',
                margin: '5px 0 0 0',
              }}
              aria-label="Select language"
            >
              {languageOptions.map((option) => (
                <option key={option.value} value={option.value}>
                  {option.label}
                </option>
              ))}
            </select>
          </Box>
        )}

        <CanvasTableControls
          table={table}
          disabledAll={disabledAll === true}
        />
      </Box>
    </Box>
  );
}

export type { CanvasEditHeaderTable } from './table/canvasTableTypes';
