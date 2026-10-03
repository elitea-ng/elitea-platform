import type { KeyboardEvent, ReactNode } from 'react';
import { useCallback } from 'react';

// [S1] Interim icon: `apps/elitea-ui`'s baseline uses a hand-rolled
// `CloseIcon` (`@/components/Icons/CloseIcon`), which is not one of the 39
// custom SVGs S2 is porting into `shared/ui/icons/` (no `close-icon.tsx`
// exists there yet). Per the S1 brief: reference the icon set where it
// resolves, and fall back rather than block where it does not — `Close` is
// a standard `@mui/icons-material` icon (R-I1-compliant single-icon
// import), used the same way the baseline itself uses MUI icons directly
// for icons outside its custom SVG set (`Visibility`/`VisibilityOff` in
// `SecretField.jsx`, `FullscreenOutlined` in `ResizableCodeMirrorEditor`).
// TODO(S2 follow-up): swap for `@/shared/ui/icons/close-icon`'s `CloseIcon`
// once it lands, for pixel-parity with the baseline's outline glyph.
import CloseIcon from '@mui/icons-material/Close';
import Box from '@mui/material/Box';
import Dialog from '@mui/material/Dialog';
import DialogActions from '@mui/material/DialogActions';
import DialogContent from '@mui/material/DialogContent';
import DialogTitle from '@mui/material/DialogTitle';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { BaseBtn } from '../BaseBtn';
import { t } from '@/shared/i18n';

/** @public */
export type ModalVariant = 'simple' | 'complex';

/** @public */
export interface ModalHeaderOptions {
  /** Rendered to the left of the title, `variant="simple"` only. */
  icon?: ReactNode;
  /** `variant="complex"` only: extra controls rendered in the header, right-aligned. */
  actions?: ReactNode;
  closeButtonDataTestId?: string;
}

/** @public */
export interface ModalActionsOptions {
  /** Overrides the default Cancel/Confirm action pair entirely. */
  node?: ReactNode;
  confirmText?: string;
  cancelText?: string;
  /** Renders the confirm button in the destructive colour. */
  alarm?: boolean;
  /** Disables the confirm button while a confirm action is in flight. */
  confirming?: boolean;
}

/** @public shared/ui component API — consumed once a features/widgets/pages caller exists (none does yet in this pass). */
export interface BaseModalProps {
  open: boolean;
  /** String titles render through `Typography`; pass a node for full control. */
  title?: ReactNode;
  onClose?: () => void;
  onConfirm?: () => void;
  content?: ReactNode;
  onKeyDown?: (event: KeyboardEvent<HTMLDivElement>) => void;
  /** `'simple'`: compact, centred dialog. `'complex'`: full editor-style dialog. */
  variant?: ModalVariant;
  /** `variant="complex"` only: expands to a large, near-fullscreen surface. */
  fullscreen?: boolean;
  'data-testid'?: string;
  footer?: ReactNode;
  /** Header row extras — title icon, heading level, header actions, close-button test id. */
  header?: ModalHeaderOptions;
  /** Action-bar extras — full override node, button copy, destructive/confirming state. */
  actions?: ModalActionsOptions;
}

interface ModalActionsProps {
  actions: ModalActionsOptions | undefined;
  onClose?: (() => void) | undefined;
  onConfirm?: (() => void) | undefined;
}

/**
 * The default Cancel/Confirm pair, split out of `BaseModal` to keep that
 * function under the §3.5 cyclomatic-complexity budget. Only the six
 * `MuiButton` variants `shared/brand/mui-overrides/MuiButton.ts` (T1)
 * actually wires are used here — see `BaseBtn.tsx`'s doc comment for the
 * flagged gap this works around (`elitea`/`alarm` render unstyled).
 */
function ModalActions({ actions, onClose, onConfirm }: ModalActionsProps): ReactNode {
  if (actions?.node) return actions.node;
  return (
    <>
      {onClose && (
        <BaseBtn
          variant="secondary"
          onClick={onClose}
        >
          {actions?.cancelText ?? t('shared.ui.baseModal.cancel', 'Cancel')}
        </BaseBtn>
      )}
      {onConfirm && (
        <BaseBtn
          variant="contained"
          color={actions?.alarm ? 'error' : 'primary'}
          onClick={onConfirm}
          disabled={actions?.confirming}
        >
          {actions?.confirmText ?? t('shared.ui.baseModal.confirm', 'Confirm')}
        </BaseBtn>
      )}
    </>
  );
}

interface ModalHeaderProps {
  title?: ReactNode | undefined;
  header: ModalHeaderOptions | undefined;
  isSimple: boolean;
  onClose?: (() => void) | undefined;
}

/** The title row + close button, split out for the same reason as `ModalActions`. */
function ModalHeader({ title, header, isSimple, onClose }: ModalHeaderProps): ReactNode {
  // The heading is the title ALONE: `DialogTitle` renders a plain `div` row,
  // and the `<h2>` inside it carries `base-modal-title` (the dialog's
  // `aria-labelledby`). With `DialogTitle` itself as the `<h2>`, the close
  // button's "Close" joined the heading's accessible name ("Delete node?
  // Close"), so a dialog was not findable by its own title. Every dialog
  // title is `headingMedium` (typography spec §2).
  const titleNode =
    typeof title === 'string' ? (
      <Typography
        id="base-modal-title"
        variant="headingMedium"
        component="h2"
        sx={{ color: 'text.secondary' }}
      >
        {title}
      </Typography>
    ) : (
      <Box
        id="base-modal-title"
        component="h2"
        sx={{ m: 0, minWidth: 0, fontSize: 'inherit', fontWeight: 'inherit', lineHeight: 'inherit' }}
      >
        {title}
      </Box>
    );

  return (
    <DialogTitle
      component="div"
      // Its own id: without one, MUI gives the row the Dialog's
      // `aria-labelledby` id too, duplicating `base-modal-title`.
      id="base-modal-header"
      sx={{
        width: '100%',
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'space-between',
        height: '3.75rem',
        gap: (theme: Theme) => theme.spacing(1),
      }}
    >
      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          gap: (theme: Theme) => theme.spacing(1),
          minWidth: 0,
        }}
      >
        {isSimple && header?.icon}
        {titleNode}
      </Box>
      <Box
        sx={{
          display: 'flex',
          alignItems: 'center',
          gap: (theme: Theme) => theme.spacing(1),
        }}
      >
        {!isSimple && header?.actions}
        <BaseBtn
          variant="text"
          aria-label={t('shared.ui.baseModal.close', 'Close')}
          data-testid={header?.closeButtonDataTestId}
          startIcon={<CloseIcon />}
          onClick={onClose}
        />
      </Box>
    </DialogTitle>
  );
}

function paperSx(isSimple: boolean, isFullscreen: boolean) {
  return (theme: Theme) => ({
    display: 'flex',
    flexDirection: 'column' as const,
    alignItems: 'flex-start' as const,
    width: isFullscreen ? '80vw' : isSimple ? '31.25rem' : '37.5rem',
    maxWidth: isFullscreen ? '80vw' : '60%',
    ...(isFullscreen ? { height: 'calc(100vh - 10rem)' } : {}),
    background: isSimple
      ? theme.vars.palette.background.modal.simple
      : theme.vars.palette.background.tabPanel,
  });
}

function contentSx(isSimple: boolean, isFullscreen: boolean, hasActions: boolean) {
  return (theme: Theme) => ({
    width: '100%',
    overflow: 'auto' as const,
    maxHeight: isFullscreen ? 'none' : 'calc(100vh - 23.75rem)',
    boxSizing: 'border-box' as const,
    color: theme.vars.palette.text.secondary,
    ...(isFullscreen ? { flex: 1, minHeight: 0 } : {}),
    ...(isSimple
      ? { background: 'transparent' }
      : {
          background: theme.vars.palette.background.secondary,
          borderTop: `0.0625rem solid ${theme.vars.palette.border.lines}`,
          ...(hasActions ? { borderBottom: `0.0625rem solid ${theme.vars.palette.border.lines}` } : {}),
        }),
  });
}

/**
 * The app's base modal shell: a header (title + close), a scrollable
 * content area, and an optional action bar. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/modal/BaseModal.jsx`. Colours/geometry
 * live in `shared/brand/mui-overrides/MuiDialog.ts` (R-T12) for the paper
 * surface; the rest is local layout `sx` (spacing/radii/typography tokens
 * only, no raw values — R-T9/T10/T11).
 *
 * Deviations from the baseline:
 *  - The baseline's `title`/`titleIcon`/`titleVariant`/`headerActions`/
 *    `closeButtonDataTestId`/`confirmButtonText`/`cancelButtonText`/`alarm`/
 *    `confirming`/`actions` were 10 separate flat props; combined with the
 *    rest that put the component at 19 props, breaching the §3.5 12-prop
 *    budget (measured: `ChatBox.jsx` at 58 props is the defect that budget
 *    exists to prevent). Grouped into `header`/`actions` option objects —
 *    same information, 12 props.
 *  - `header.icon` takes a `ReactNode` directly instead of a
 *    `ModalConstants.MODAL_ICON_TYPE` string key — the baseline's
 *    icon-type-to-component map lived in app-level constants
 *    (`@/[fsd]/shared/lib/constants`) that `shared/ui` cannot import (layer
 *    rule R-L1: `shared/` is beneath every other layer). Callers pass the
 *    icon element they want.
 *  - Neither action button carries `autoFocus` (the baseline auto-focused
 *    Cancel, or Confirm when there was no Cancel) — `jsx-a11y/no-autofocus`
 *    (R-C1's linter) flags forced focus moves as a usability hazard for
 *    screen-reader and low-vision users. `Dialog`'s own default focus
 *    management (focuses the dialog surface, or its first tabbable
 *    descendant) already gets a keyboard user into the dialog.
 */
export function BaseModal({
  open,
  title,
  onClose,
  onConfirm,
  content,
  onKeyDown,
  variant = 'complex',
  fullscreen = false,
  'data-testid': dataTestId,
  footer,
  header,
  actions,
}: BaseModalProps): ReactNode {
  const isSimple = variant === 'simple';
  const isFullscreen = variant === 'complex' && fullscreen;
  const hasActions = Boolean(actions?.node || onConfirm);

  // `Dialog`'s own `onClose` prop already fires on Escape (MUI's Modal
  // listens natively and calls `onClose(event, 'escapeKeyDown')`) — an
  // additional manual `if (event.key === 'Escape') onClose?.()` here would
  // double-invoke `onClose` for every Escape press. `onKeyDown` is passed
  // straight through for any OTHER key handling a caller needs.
  const handleKeyDown = useCallback(
    (event: KeyboardEvent<HTMLDivElement>) => {
      onKeyDown?.(event);
    },
    [onKeyDown],
  );

  return (
    <Dialog
      open={open}
      aria-labelledby="base-modal-title"
      maxWidth={false}
      data-testid={dataTestId}
      onClose={onClose}
      onKeyDown={handleKeyDown}
      slotProps={{ paper: { sx: paperSx(isSimple, isFullscreen) } }}
    >
      <ModalHeader
        title={title}
        header={header}
        isSimple={isSimple}
        onClose={onClose}
      />

      <DialogContent sx={contentSx(isSimple, isFullscreen, hasActions)}>{content}</DialogContent>

      {hasActions && !isFullscreen && (
        <DialogActions
          sx={(theme: Theme) => ({
            justifyContent: 'flex-end',
            alignSelf: 'flex-end',
            alignItems: 'center',
            padding: `${theme.spacing(1.5)} ${theme.spacing(3)}`,
            gap: theme.spacing(1.5),
            height: '3.75rem',
            ...(isSimple ? { background: theme.vars.palette.background.modal.simple } : {}),
          })}
        >
          <ModalActions
            actions={actions}
            onClose={onClose}
            onConfirm={onConfirm}
          />
        </DialogActions>
      )}
      {footer}
    </Dialog>
  );
}
