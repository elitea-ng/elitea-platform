import type { ReactNode } from 'react';
import { memo, forwardRef, useImperativeHandle, useRef, useCallback, useMemo, useState } from 'react';

import AttachFileIcon from '@mui/icons-material/AttachFile';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import type { Theme } from '@mui/material/styles';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { ATTACHMENT_LIMITS, getRemainingAttachmentCapacity, validateAttachmentFiles } from '@/shared/lib/attachments';

/**
 * Chat button primitive: AttachmentButton
 *
 * Renders an icon button that opens a file-picker dialog. Supports drag-and-
 * drop via the `onDrop` imperative handle (wired by the composition root that
 * also injects `attachmentButtonRef` into `NewChatInput`).
 *
 * Every newly-picked/dropped file is validated via `validateAttachmentFiles`
 * (`shared/lib/attachments.ts` — count/size/per-image-size limits from
 * `ATTACHMENT_LIMITS`); rejected files are dropped and reported through
 * `onError`, not silently attached. `limits` stays a loose
 * `Record<string, number>` (unchanged shape, still accepted from
 * `PlusChatButton`'s own pass-through prop of the same name/type) — real
 * `ATTACHMENT_LIMITS` values are the base, and any matching keys the caller
 * supplies override them (`{ ...ATTACHMENT_LIMITS, ...limits }`); unrelated
 * keys are ignored rather than rejected, since nothing in this codebase
 * currently supplies validated `limits` data.
 *
 * `onError` (not a toast) — no toast/snackbar primitive exists yet in
 * `shared/ui` (established gap, see `features/mcps/model/useMcpAuthModal.ts`'s
 * identical disclosure); the caller decides how to surface the message
 * until one lands.
 *
 * `disabled` factors in every signal this component can actually observe
 * today (`disableAttachments`/in-flight processing/at-capacity/at-max-size).
 * Baseline additionally factors in `isLoading`/`noFileTypesAvailable` from a
 * dynamic backend file-types query (`useFileTypes`/`useAllowedExtensions`) —
 * no such hook exists anywhere in this codebase yet (disclosed gap, not
 * invented here).
 *
 * Prop contract (injected by the composition root through `slots.attachmentButton`):
 *   - `onAttachFiles`  — called with the validated, accepted File(s)
 *   - `disableAttachments` — disables the button / hides file picker
 *   - `attachments` — current list of attached files (used for count/size limit checks)
 *   - `limits` — optional overrides merged onto `ATTACHMENT_LIMITS`
 *   - `onError` — called with a human-readable message when files are rejected
 *   - `showLabel` — render as a full-width MENU ROW (paperclip + "Attach files"
 *     + a right-aligned "<n> left" counter) instead of the bare icon button.
 *     Baseline prop of the same name (`AttachmentButton.jsx:34`); it is how the
 *     "+" menu shows attachments as a one-click action with its remaining
 *     capacity visible, rather than hiding the count in a hover tooltip.
 *
 * Imperative handle (`AttachmentButtonHandle`):
 *   - `onDrop(event)` — validates & dispatches dropped files to `onAttachFiles`
 */
export interface AttachmentButtonHandle {
  onDrop(event: { dataTransfer: { files: readonly File[] }; preventDefault(): void }): void;
}

export interface AttachmentButtonProps {
  onAttachFiles?: (files: readonly File[]) => void;
  disableAttachments?: boolean;
  attachments?: readonly File[];
  limits?: Record<string, number>;
  onError?: (message: string) => void;
  showLabel?: boolean;
  /**
   * Render NOTHING — no button, no row, no file input — and expose only the
   * imperative drop/paste handle.
   *
   * `PlusChatButton` keeps one always-mounted instance for that handle, because
   * the visible rows live in a Popper that unmounts with the menu. That
   * instance used to render the normal icon button inside a 0x0,
   * `pointer-events: none` Box, which put a SECOND, permanently dead
   * "attach files" button in the accessibility tree — and, with the menu
   * closed, it was the FIRST match for that name on the page. Every automated
   * reader (a screen reader, a test, an agent driving the browser) found the
   * dead one, clicked it, got nothing, and concluded attachments were broken.
   * Hiding it with `aria-hidden` instead traded that for an
   * `aria-hidden-focus` violation: the button was still focusable. Rendering
   * no control is the only form that is both invisible and honest.
   */
  dropTargetOnly?: boolean;
}

export const AttachmentButton = memo(
  forwardRef<AttachmentButtonHandle, AttachmentButtonProps>(
    ({ disableAttachments = false, attachments = [], onAttachFiles, limits, onError, showLabel = false, dropTargetOnly = false }, ref) => {
      const fileInputRef = useRef<HTMLInputElement>(null);
      const [isProcessing, setIsProcessing] = useState(false);

      const effectiveLimits = useMemo<typeof ATTACHMENT_LIMITS>(() => ({ ...ATTACHMENT_LIMITS, ...limits }), [limits]);

      const processFiles = useCallback(
        (files: readonly File[]) => {
          if (files.length === 0) return;
          setIsProcessing(true);
          try {
            const { validFiles, errors } = validateAttachmentFiles(files, attachments, effectiveLimits);
            if (errors.length > 0) onError?.(errors.join('\n'));
            if (validFiles.length > 0) onAttachFiles?.(validFiles);
          } finally {
            setIsProcessing(false);
          }
        },
        [attachments, effectiveLimits, onAttachFiles, onError],
      );

      useImperativeHandle(
        ref,
        () => ({
          onDrop: (event: { dataTransfer: { files: readonly File[] }; preventDefault(): void }) => {
            event.preventDefault();
            if (disableAttachments) return;
            processFiles(Array.from(event.dataTransfer.files));
          },
        }),
        [processFiles, disableAttachments],
      );

      const capacity = getRemainingAttachmentCapacity(attachments, effectiveLimits);
      const isDisabled = disableAttachments || isProcessing || capacity.isAtMaxCapacity || capacity.isAtMaxSize;

      const handleButtonClick = useCallback(() => {
        if (capacity.isAtMaxCapacity) {
          onError?.(
            t('widgets.chat.attachmentButton.fileLimitReachedError', "You've reached the {{max}}-file limit.", {
              max: effectiveLimits.MAX_ATTACHMENTS,
            }),
          );
          return;
        }
        fileInputRef.current?.click();
      }, [capacity.isAtMaxCapacity, effectiveLimits.MAX_ATTACHMENTS, onError]);

      const handleFileChange = useCallback(
        (e: React.ChangeEvent<HTMLInputElement>) => {
          const files = e.target.files;
          if (files && files.length > 0) {
            processFiles(Array.from(files));
          }
          // Reset so the same file can be re-selected if needed
          e.target.value = '';
        },
        [processFiles],
      );

      // AFTER every hook, so the handle above is live on this instance too.
      if (dropTargetOnly) return null;

      const tooltipText = attachmentTooltip(isProcessing, capacity, effectiveLimits.MAX_ATTACHMENTS);

      // The row form carries the remaining count as visible text, so the
      // tooltip that exists to surface it on the icon form is redundant there.
      const control = showLabel ? (
        <AttachmentMenuRow
          isDisabled={isDisabled}
          remaining={capacity.remainingAttachments}
          onActivate={handleButtonClick}
        />
      ) : (
        <Tooltip title={tooltipText} placement="top">
          <Box component="span">
            <IconButton
              color="secondary"
              aria-label={t('widgets.chat.attachmentButton.ariaLabel', 'attach files')}
              disabled={isDisabled}
              onClick={handleButtonClick}
              sx={{ marginLeft: 0 }}
            >
              <AttachFileIcon fontSize="small" />
            </IconButton>
          </Box>
        </Tooltip>
      );

      return (
        <>
          {control}

          {/* Hidden file input — only accept when attachments aren't disabled */}
          <input
            ref={fileInputRef}
            type="file"
            multiple
            hidden
            disabled={isDisabled}
            onChange={handleFileChange}
            aria-hidden="true"
          />
        </>
      );
    },
  ),
);

/**
 * The icon form's hover text. Module-level rather than inline, because the
 * component sits at the §3.5 cyclomatic-complexity-12 ceiling and this chain
 * alone is four of its branches — the same reason `AttachmentMenuRow` below is
 * its own component.
 */
function attachmentTooltip(
  isProcessing: boolean,
  capacity: ReturnType<typeof getRemainingAttachmentCapacity>,
  maxAttachments: number,
): string {
  if (isProcessing) return t('widgets.chat.attachmentButton.processingTooltip', 'Processing...');
  if (capacity.isAtMaxCapacity) {
    return t('widgets.chat.attachmentButton.maxAttachmentsTooltip', 'Max {{max}} attachments', { max: maxAttachments });
  }
  if (capacity.isAtMaxSize) return t('widgets.chat.attachmentButton.sizeLimitTooltip', 'Size limit reached');
  if (capacity.remainingAttachments === 1) {
    return t('widgets.chat.attachmentButton.oneFileLeftTooltip', '{{count}} file left', { count: capacity.remainingAttachments });
  }
  return t('widgets.chat.attachmentButton.filesLeftTooltip', '{{count}} files left', { count: capacity.remainingAttachments });
}

AttachmentButton.displayName = 'AttachmentButton';

/**
 * The `showLabel` form: a full-width menu row instead of a bare icon button.
 *
 * Its own component rather than a branch inside `AttachmentButton`, because
 * that component is already at the §3.5 cyclomatic-complexity-12 ceiling —
 * the keyboard handler and the disabled-state branches tipped it to 14.
 */
function AttachmentMenuRow({ isDisabled, remaining, onActivate }: {
  readonly isDisabled: boolean;
  readonly remaining: number;
  readonly onActivate: () => void;
}): ReactNode {
  return (
    <Box
      role="menuitem"
      tabIndex={isDisabled ? -1 : 0}
      aria-label={t('widgets.chat.attachmentButton.ariaLabel', 'attach files')}
      aria-disabled={isDisabled}
      data-testid="plus-menu-attachments"
      onClick={isDisabled ? undefined : onActivate}
      onKeyDown={(e) => {
        if (isDisabled || (e.key !== 'Enter' && e.key !== ' ')) return;
        e.preventDefault();
        onActivate();
      }}
      sx={attachRowSx(isDisabled)}
    >
      <AttachFileIcon fontSize="small" />
      <Typography variant="labelMedium" sx={{ flex: 1 }}>
        {t('widgets.chat.attachmentButton.attachFilesLabel', 'Attach Files')}
      </Typography>
      <Typography variant="labelSmall" sx={{ color: 'text.disabled' }}>
        {t('widgets.chat.attachmentButton.filesLeftCounter', '{{count}} left', { count: remaining })}
      </Typography>
    </Box>
  );
}

/** The `showLabel` row's styling — matches the "+" menu's other rows (`PlusChatButton.parts.tsx`'s `rowSx`). */
function attachRowSx(isDisabled: boolean) {
  return (theme: Theme) => ({
    display: 'flex',
    alignItems: 'center',
    gap: 1,
    padding: '0.375rem 1rem',
    height: '2.75rem',
    cursor: isDisabled ? 'default' : 'pointer',
    color: isDisabled ? theme.vars.palette.text.disabled : theme.vars.palette.text.secondary,
    ...(isDisabled ? {} : { '&:hover': { backgroundColor: theme.vars.palette.action.hover } }),
  });
}
