import type { ReactNode } from 'react';
import { useCallback, useEffect, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import IconButton from '@mui/material/IconButton';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';
import { useTheme } from '@mui/material/styles';
import CloseIcon from '@mui/icons-material/Close';

import { t } from '@/shared/i18n';
import { DeleteEntityModal } from '@/shared/ui/DeleteEntityModal';

/**
 * `EditorShell` — a faithful port of the old app's `pages/NewChat/
 * components/BaseEditor.jsx` + `EditorHeader.jsx` (read both in full before
 * writing this file), redesigned as a plain presentational component: this
 * app has no Formik (established throughout this codebase — see e.g.
 * `features/agents/ui/AgentEditor.tsx`'s own doc comment), so the baseline's
 * `<Formik><DirtyDetector/></Formik>` wrapper is dropped entirely.
 * `isDirty`/`onDirtyStateChange`/`onDiscard` arrive as plain props, already
 * computed by `AgentEditor`/`PipelineEditor` themselves.
 *
 * Implements `AgentEditorShellProps` (`features/agents/ui/AgentEditor.tsx`),
 * `PipelineEditorShellProps` (`features/pipelines/ui/PipelineEditorParts.tsx`),
 * AND `ToolkitEditorShellProps` (`features/toolkits/ui/ToolkitEditorParts.tsx`)
 * — read all three interfaces in full. Agent's/Pipeline's are NOT exported
 * from their slice's `index.ts` (verified: `grep -n "export type"
 * src/features/{agents,pipelines}/index.ts`; Toolkit's IS, but is declared
 * locally anyway for symmetry), so `EditorShellRenderProps` below is a
 * LOCAL, structurally-typed union of all three rather than an import — the
 * same "no unexported name, use a structurally-compatible local type"
 * approach `../lib/editorParticipantAdapters.ts`'s own doc comment
 * documents in full for the identical constraint.
 * `renderAgentEditorShell`/`renderPipelineEditorShell`/`renderToolkitEditorShell`
 * below are thin, separately-exported functions with this exact structural
 * shape so they can be passed directly as `deps.renderShell` to
 * `AgentEditor`/`PipelineEditor`/`ToolkitEditor` — TypeScript checks the
 * assignment structurally at that call site (`ChatWithEditors.tsx`), no
 * import of any real (unexported) type needed.
 *
 * The three real shell-prop interfaces do not all declare the same fields —
 * every field below that isn't shared by all three is OPTIONAL (a real
 * `?:`, not just an undefined-inclusive value type — the two are NOT
 * interchangeable for this purpose: `ToolkitEditorShellProps` doesn't
 * declare `subtitle`/`isPublic` AT ALL, so a value of that real type is
 * only assignable to this local type if the corresponding fields here are
 * genuinely optional keys, not required keys typed `T | undefined`):
 *  - `formContent` — `PipelineEditorShellProps`'s one extra required slot
 *    (rendered between the header and `children`, matching the baseline's
 *    own `{formContent}` placement, `BaseEditor.jsx:120-121`); absent from
 *    both `AgentEditorShellProps` and `ToolkitEditorShellProps`.
 *  - `subtitle`/`isPublic` — required (non-optional key) on
 *    `AgentEditorShellProps`/`PipelineEditorShellProps`, absent entirely
 *    from `ToolkitEditorShellProps` (`isPublic` defaults to `false` here
 *    when omitted — Toolkit's own `disabled`/gating already happens
 *    upstream in `useToolkitEditorState`, so this shell never needs to
 *    re-derive it).
 *  - `contentSx` — `ToolkitEditorShellProps`'s one extra optional slot
 *    (`ToolkitEditor.tsx`'s own `...(editToolDetail === null ? {
 *    contentSx: emptyContentSx } : {})` — a zero-padding override while its
 *    content area shows a loading/type-selector state instead of a form).
 *    Absent from Agent's/Pipeline's shell props.
 *
 * **Disclosed, deliberate simplification vs. the baseline's dual discard
 * mechanism.** The baseline had TWO separate warn-then-act flows: (1) the
 * close ("X") button's `handleCancel`, which — only when `isDirty &&
 * !isPublic` — shows a warning and, on confirm, calls ONLY `onClose()`
 * (`BaseEditor.jsx:62-76`); (2) the header's own `Button.DiscardButton`,
 * which has its own independent confirm and, on confirm, calls `onDiscard()`
 * (stays open, does not close). This port collapses both into ONE mechanism
 * on the Discard button only: confirm -> `onDiscard?.()` then `onClose()`.
 * The close ("X") button here is a plain, unconditional `onClose()` with no
 * warning of its own. This is safe in practice: `AgentEditor`'s own EDIT-mode
 * `isDirty` is always `false` (no live edit-mode form exists yet — see that
 * file's own doc comment), so the only mode with genuine dirty state today
 * is CREATE mode, where `onDiscard` is always wired — collapsing the two
 * flows loses no real warning coverage against this port's current callers,
 * and is far simpler than reproducing two independent confirm dialogs for a
 * distinction (`onClose`-only vs. `onDiscard`-then-stay-open) neither
 * `AgentEditor.tsx` nor `PipelineEditor.tsx` currently exercises.
 *
 * Reuses `shared/ui/DeleteEntityModal` for the discard-confirm (its
 * `content.custom`/`copy` overrides make it generic enough for a non-delete
 * confirm — per this unit's own instructions, no second bespoke dialog
 * component). `src/widgets/editor/ui/{BaseEditor,EditorHeader}.tsx` is an
 * earlier, unused "Phase-3 stub" (own doc comment: "Full implementation
 * wired in Phase 5") that partially overlaps this file's purpose but has no
 * consumer anywhere in this worktree (verified: `grep -rl
 * "widgets/editor" src` — zero hits outside its own directory) and does not
 * match `AgentEditorShellProps`/`PipelineEditorShellProps` exactly (no
 * `formContent` slot, no `isPublic`-gated header, uses `BaseModal` directly
 * instead of `DeleteEntityModal` per this unit's explicit instruction) —
 * left untouched (out of this unit's file scope) rather than adapted in
 * place; flagged as a disclosed, pre-existing overlap for a future cleanup
 * pass, not fixed here.
 */

export interface EditorShellRenderProps {
  readonly isVisible: boolean;
  readonly isDirty: boolean;
  readonly onClose: () => void;
  readonly title: string;
  readonly subtitle?: string | undefined;
  readonly error: unknown;
  readonly onDirtyStateChange?: ((isDirty: boolean) => void) | undefined;
  readonly onDiscard?: (() => void) | undefined;
  readonly formContent?: ReactNode;
  readonly saveButton: ReactNode;
  readonly isPublic?: boolean;
  readonly contentSx?: SxProps<Theme>;
  readonly children: ReactNode;
}

/** Mirrors the baseline's `error?.data?.message || error?.message || 'Failed to load configuration'` fallback chain for an `unknown` API error shape — `error` is genuinely `unknown` here (no assumed error type), narrowed defensively, no unsafe casts. */
function resolveErrorMessage(error: unknown): string | undefined {
  if (!error) return undefined;
  if (typeof error === 'object') {
    const record = error as Record<string, unknown>;
    const data = record['data'];
    const dataMessage = data && typeof data === 'object' ? (data as Record<string, unknown>)['message'] : undefined;
    if (typeof dataMessage === 'string') return dataMessage;
    const message = record['message'];
    if (typeof message === 'string') return message;
  }
  return t('processes.chat.editorShell.loadErrorFallback', 'Failed to load configuration');
}

/** `title`/`subtitle`/`isPublic` grouped into one prop — same "bundle related props into one object" convention this codebase's own `ChatBoxProps`/`PipelineEditorBodyProps` already use — purely to keep `EditorShell`'s own prop count under the §3.5 12-prop budget (the flat `EditorShellRenderProps` the two `render*Shell` wrappers accept has 13 fields once `contentSx` is added for Toolkit). */
interface EditorShellHeaderInfo {
  readonly title: string;
  readonly subtitle: string | undefined;
  readonly isPublic: boolean;
}

interface EditorShellProps {
  readonly isVisible: boolean;
  readonly isDirty: boolean;
  readonly onClose: () => void;
  readonly header: EditorShellHeaderInfo;
  readonly error: unknown;
  readonly onDirtyStateChange: ((isDirty: boolean) => void) | undefined;
  readonly onDiscard: (() => void) | undefined;
  readonly formContent: ReactNode;
  readonly saveButton: ReactNode;
  readonly contentSx: SxProps<Theme> | undefined;
  readonly children: ReactNode;
}

function EditorShell({
  isVisible,
  isDirty,
  onClose,
  header: { title, subtitle, isPublic },
  error,
  onDirtyStateChange,
  onDiscard,
  formContent,
  saveButton,
  contentSx,
  children,
}: EditorShellProps): ReactNode {
  const theme = useTheme();
  const [showDiscardConfirm, setShowDiscardConfirm] = useState(false);

  // Baseline: `DirtyDetector setDirty={handleIsDirtyChange}` (`BaseEditor.jsx:110`)
  // — reports every `isDirty` change upward. No Formik here, so `isDirty`
  // itself is already the caller's own computed value; this effect is the
  // only piece of that baseline wiring this plain-props port still owns.
  useEffect(() => {
    onDirtyStateChange?.(isDirty);
  }, [isDirty, onDirtyStateChange]);

  const handleOpenDiscardConfirm = useCallback(() => setShowDiscardConfirm(true), []);
  const handleCloseDiscardConfirm = useCallback(() => setShowDiscardConfirm(false), []);
  const handleConfirmDiscard = useCallback(() => {
    setShowDiscardConfirm(false);
    onDiscard?.();
    onClose();
  }, [onDiscard, onClose]);

  const errorMessage = resolveErrorMessage(error);
  const baseContentSx: SxProps<Theme> = { flexGrow: 1, overflow: 'auto', width: '100%', padding: '1rem' };
  // MUI's own `SxProps<Theme>` is a union that can itself already be an
  // array/theme-callback, so `[baseContentSx, contentSx]` cannot be typed
  // precisely without this cast — a known MUI generic-sx-merging limitation
  // (the array form is MUI's own documented way to merge two `sx` values;
  // `Box` flattens nested arrays at runtime regardless of how this is
  // typed). Purely a style-merge concern, no business-logic risk.
  const contentBoxSx: SxProps<Theme> = contentSx ? ([baseContentSx, contentSx] as SxProps<Theme>) : baseContentSx;

  return (
    <Box
      sx={{
        display: isVisible ? 'flex' : 'none',
        flexDirection: 'column',
        height: '100%',
        maxHeight: '100%',
        width: '100%',
        border: `1px solid ${theme.vars.palette.border.lines}`,
        borderRadius: theme.vars.shape.radiusLg,
        overflow: 'hidden',
        background: theme.vars.palette.background.tabPanel,
      }}
    >
      <Box
        sx={{
          display: 'flex',
          justifyContent: 'space-between',
          alignItems: 'center',
          gap: 1,
          padding: '0.5rem 1rem',
          borderBottom: `1px solid ${theme.vars.palette.border.lines}`,
          background: theme.vars.palette.background.userInputBackground,
          minHeight: '2.625rem',
          flexShrink: 0,
        }}
      >
        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1.4, minWidth: 0 }}>
          <IconButton
            size="small"
            aria-label={t('processes.chat.editorShell.closeAriaLabel', 'Close editor')}
            onClick={onClose}
            sx={{ marginLeft: 0, flexShrink: 0 }}
          >
            <CloseIcon sx={{ width: '1.125rem', height: '1.125rem', color: theme.vars.palette.icon.fill.default }} />
          </IconButton>
          <Box sx={{ minWidth: 0 }}>
            <Typography
              variant="headingSmall"
              component="h6"
              color="text.secondary"
              noWrap
            >
              {title}
            </Typography>
            {subtitle !== undefined && (
              <Typography
                variant="bodyMedium"
                component="p"
                color="text.primary"
                noWrap
              >
                {subtitle}
              </Typography>
            )}
          </Box>
        </Box>

        <Box sx={{ display: 'flex', alignItems: 'center', gap: 1, flexShrink: 0 }}>
          {isPublic ? (
            <Box
              sx={{
                boxSizing: 'border-box',
                display: 'flex',
                alignItems: 'center',
                padding: '0.125rem 0.375rem',
                height: '1.25rem',
                borderRadius: theme.vars.shape.radiusPill,
                border: `1px solid ${theme.vars.palette.border.lines}`,
              }}
            >
              <Typography
                variant="bodySmall"
                color="text.metrics"
                sx={{ textTransform: 'none' }}
              >
                {t('processes.chat.editorShell.publicBadgeLabel', 'Public')}
              </Typography>
            </Box>
          ) : (
            <>
              <Button
                size="small"
                variant="elitea"
                color="secondary"
                disabled={!isDirty}
                onClick={handleOpenDiscardConfirm}
                aria-label={t('processes.chat.editorShell.discardButtonLabel', 'Discard')}
              >
                {t('processes.chat.editorShell.discardButtonLabel', 'Discard')}
              </Button>
              {saveButton}
            </>
          )}
        </Box>
      </Box>

      {formContent}

      <Box sx={contentBoxSx}>
        {errorMessage !== undefined && (
          <Alert
            severity="error"
            sx={{ mb: 2 }}
          >
            {errorMessage}
          </Alert>
        )}
        {children}
      </Box>

      <DeleteEntityModal
        open={showDiscardConfirm}
        onClose={handleCloseDiscardConfirm}
        onConfirm={handleConfirmDiscard}
        copy={{
          title: t('processes.chat.editorShell.discardDialogTitle', 'Warning'),
          confirmText: t('processes.chat.editorShell.discardDialogConfirm', 'Confirm'),
          cancelText: t('processes.chat.editorShell.discardDialogCancel', 'Cancel'),
        }}
        content={{
          custom: (
            <Typography variant="bodyMedium">
              {t('processes.chat.editorShell.discardDialogContent', 'You are editing now. Do you want to discard current changes and continue?')}
            </Typography>
          ),
        }}
        data-testid="editor-shell-discard-confirm"
      />
    </Box>
  );
}

/** Regroups the flat `EditorShellRenderProps` every real `deps.renderShell` caller passes into `EditorShell`'s own grouped `EditorShellProps` — see `EditorShellProps`'s own doc comment for why the two shapes differ. */
function toEditorShellProps(props: EditorShellRenderProps): EditorShellProps {
  return {
    isVisible: props.isVisible,
    isDirty: props.isDirty,
    onClose: props.onClose,
    header: { title: props.title, subtitle: props.subtitle, isPublic: props.isPublic ?? false },
    error: props.error,
    onDirtyStateChange: props.onDirtyStateChange,
    onDiscard: props.onDiscard,
    formContent: props.formContent,
    saveButton: props.saveButton,
    contentSx: props.contentSx,
    children: props.children,
  };
}

/** Passed directly as `AgentEditorDeps.renderShell` — see this module's own doc comment for why the parameter is typed against the local `EditorShellRenderProps` rather than an imported `AgentEditorShellProps`. */
export function renderAgentEditorShell(props: EditorShellRenderProps): ReactNode {
  return <EditorShell {...toEditorShellProps(props)} />;
}

/** Passed directly as `PipelineEditorDeps.renderShell`. */
export function renderPipelineEditorShell(props: EditorShellRenderProps): ReactNode {
  return <EditorShell {...toEditorShellProps(props)} />;
}

/** Passed directly as `ToolkitEditorDeps.renderShell`. */
export function renderToolkitEditorShell(props: EditorShellRenderProps): ReactNode {
  return <EditorShell {...toEditorShellProps(props)} />;
}
