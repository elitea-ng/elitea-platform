import type { KeyboardEvent, ReactNode } from 'react';
import { useEffect, useState } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { BaseModal } from '../BaseModal';
import { t } from '@/shared/i18n';

/** @public Text overrides for {@link DeleteEntityModal}. */
export interface DeleteEntityModalCopyOptions {
  title?: string;
  /** Rendered before the bolded `name`; e.g. `"Are you sure you want to delete "`. */
  textContent?: string;
  confirmText?: string;
  cancelText?: string;
}

/** @public Body-content overrides for {@link DeleteEntityModal}. */
export interface DeleteEntityModalContentOptions {
  /** Rendered after the confirmation sentence (e.g. a warning banner listing what else gets deleted). */
  extra?: ReactNode;
  /** Rendered inline at the end of the confirmation sentence, replacing the trailing `?`. */
  inline?: ReactNode;
  /** Full override of the modal body — bypasses `name`/copy text/`extra` entirely. */
  custom?: ReactNode;
}

/** @public shared/ui component API for the delete-confirmation modal. */
export interface DeleteEntityModalProps {
  open: boolean;
  onClose: () => void;
  onConfirm: () => void;
  /** The entity's display name, bolded inline in the confirmation sentence. */
  name?: string;
  /** Requires the user to type `name` exactly before Confirm is enabled. */
  shouldRequestInputName?: boolean;
  /** Renders Confirm in the destructive colour. Defaults to `true` — this is a delete modal. */
  alarm?: boolean;
  /** Disables Confirm while a confirm action is in flight (forwarded to `BaseModal`'s `actions.confirming`). */
  confirming?: boolean;
  /**
   * The reason the last confirm was REFUSED, shown inside the dialog.
   *
   * #147. A destructive action can be refused by the server after the user
   * confirms it — `DELETE /version/prompt_lib/...` answers 400 "Unpublish
   * first. Cannot delete a published version." Before this slot existed a
   * refused delete left the dialog open with no message, so the only
   * feedback was that nothing happened. The caller keeps the dialog open on
   * a refusal (closing it would read as "deleted") and passes the server's
   * own message here.
   *
   * `role="alert"`, so a screen reader announces it without the user
   * hunting for the change. This app has no toast infrastructure, which is
   * why the message belongs in the dialog rather than beside it.
   */
  errorMessage?: string;
  copy?: DeleteEntityModalCopyOptions;
  content?: DeleteEntityModalContentOptions;
  'data-testid'?: string;
}

interface ResolvedCopy {
  title: string;
  textContent: string;
  confirmText: string;
  cancelText: string;
}

/** Applies the `copy` overrides on top of the `t()` defaults, split out so `DeleteEntityModal` doesn't carry all four `??` checks itself (§3.5 complexity budget). */
function resolveCopy(copy: DeleteEntityModalCopyOptions | undefined): ResolvedCopy {
  return {
    title: copy?.title ?? t('shared.ui.deleteEntityModal.title', 'Delete confirmation'),
    textContent:
      copy?.textContent ?? t('shared.ui.deleteEntityModal.textContent', 'Are you sure you want to delete '),
    confirmText: copy?.confirmText ?? t('shared.ui.deleteEntityModal.confirm', 'Delete'),
    cancelText: copy?.cancelText ?? t('shared.ui.deleteEntityModal.cancel', 'Cancel'),
  };
}

/**
 * Whether Confirm should stay disabled pending a correct type-to-confirm
 * match. A small pure function on purpose — this is the unit's
 * mutation-proof target (see the test file), and a standalone function is
 * far easier to pin down with direct unit tests than the same expression
 * inline in a component body.
 */
export function isConfirmDisabled(
  shouldRequestInputName: boolean,
  name: string | undefined,
  inputName: string,
): boolean {
  return shouldRequestInputName && Boolean(name) && name !== inputName;
}

/** The refusal message's own style — see the `errorMessage` prop doc. */
const errorSx: SxProps<Theme> = (theme: Theme) => ({
  display: 'block',
  marginTop: theme.spacing(1.5),
  color: theme.vars.palette.error.main,
});

interface ConfirmationBodyProps {
  name: string | undefined;
  textContent: string;
  inline: ReactNode | undefined;
  extra: ReactNode | undefined;
  shouldRequestInputName: boolean;
  inputName: string;
  onInputNameChange: (value: string) => void;
}

/** The default confirmation sentence + optional type-to-confirm field, split out to keep `DeleteEntityModal` under the §3.5 cyclomatic-complexity budget. */
function ConfirmationBody({
  name,
  textContent,
  inline,
  extra,
  shouldRequestInputName,
  inputName,
  onInputNameChange,
}: ConfirmationBodyProps): ReactNode {
  return (
    <Box
      sx={{ display: 'flex', flexDirection: 'column', gap: (theme: Theme) => theme.spacing(1.5) }}
      onClick={(event) => event.stopPropagation()}
    >
      <Typography
        color="text.deleteAlertText"
        variant="bodyMedium"
        sx={{ whiteSpaceCollapse: 'preserve' }}
      >
        {textContent}
        <Typography
          component="span"
          variant="headingSmall"
          sx={(theme: Theme) => ({ color: theme.vars.palette.text.deleteAlertEntityName })}
        >
          {name}
        </Typography>
        {inline ?? '?'}
        {shouldRequestInputName &&
          t('shared.ui.deleteEntityModal.typeToConfirmHint', ' Enter the name to complete the action.')}
      </Typography>
      {extra}
      {shouldRequestInputName && (
        <TextField
          fullWidth
          autoComplete="off"
          variant="standard"
          id="delete-entity-modal-input-name"
          name="name"
          label={t('shared.ui.deleteEntityModal.inputNameLabel', 'Name')}
          value={inputName}
          onChange={(event) => onInputNameChange(event.target.value)}
        />
      )}
    </Box>
  );
}

/**
 * A `BaseModal`-composed confirmation dialog for destructive actions, with
 * an optional "type the name to confirm" safeguard. Ported from
 * `apps/elitea-ui/src/[fsd]/shared/ui/modal/DeleteEntityModal.jsx`.
 *
 * Deviations from the baseline:
 *  - The baseline's `titleIcon`/`title`/`textContent`/`confirmButtonText`/
 *    `cancelButtonText` were 5 separate flat text props; grouped into a
 *    `copy` option object (same 12-prop-budget reasoning as `BaseModal`'s
 *    own `header`/`actions` grouping — see that file's doc comment).
 *    `extraContent`/`inlineExtraContent`/`customContent` are grouped the
 *    same way into `content`. `titleIcon` itself is dropped: it took a
 *    `ModalConstants.MODAL_ICON_TYPE` string baseline `shared/ui` cannot
 *    import (layer rule R-L1), same gap `BaseModal.header.icon` already
 *    documents — this component does not surface a title icon at all
 *    rather than half-plumb one through.
 *  - The baseline's confirm button was `Button.OneClickButton`, a
 *    double-submit guard that does not exist in this unit's `shared/ui`
 *    yet. Approximated with `BaseModal`'s own `actions.confirming` (which
 *    already disables the button) — see `confirming` below.
 *  - `BaseModal` (S1, not touched by this unit) exposes exactly one
 *    confirm-button disable mechanism, `actions.confirming`. The
 *    type-to-confirm safeguard (`shouldRequestInputName`) has no separate
 *    "invalid" state to hook into, so `isConfirmDisabled` — name required
 *    but not yet typed correctly — is OR'd into `confirming` rather than
 *    gating whether `onConfirm` is passed at all (which would hide the
 *    button instead of graying it out).
 *  - The baseline reset `inputName` twice: an effect on `open` AND a
 *    manual clear wrapped around every `onClose` call. Since a controlled
 *    modal's `open` prop flips to `false` as an effect of that same
 *    `onClose` call in every real caller, the wrapping was redundant; only
 *    the `open`-keyed effect is kept here.
 */
export function DeleteEntityModal({
  open,
  onClose,
  onConfirm,
  name,
  shouldRequestInputName = false,
  alarm = true,
  confirming = false,
  errorMessage,
  copy,
  content,
  'data-testid': dataTestId,
}: DeleteEntityModalProps): ReactNode {
  const [inputName, setInputName] = useState('');

  useEffect(() => {
    if (!open) setInputName('');
  }, [open]);

  const resolvedCopy = resolveCopy(copy);
  const confirmDisabled = isConfirmDisabled(shouldRequestInputName, name, inputName);

  // `BaseModal` attaches this handler to the MUI `Dialog` root. Enter therefore
  // bubbles here from every child control.
  const handleKeyDown = (event: KeyboardEvent<HTMLDivElement>): void => {
    if (event.key !== 'Enter' || confirmDisabled) return;
    // Enter goes to a focused button first. Let the browser activate that button.
    // Enter on Cancel or Close must not confirm the destructive action.
    if (event.target instanceof HTMLElement && event.target.closest('button') !== null) return;
    event.preventDefault();
    onConfirm();
  };

  const body = (
    <>
      {content?.custom ?? (
        <ConfirmationBody
          name={name}
          textContent={resolvedCopy.textContent}
          inline={content?.inline}
          extra={content?.extra}
          shouldRequestInputName={shouldRequestInputName}
          inputName={inputName}
          onInputNameChange={setInputName}
        />
      )}
      {/* Outside the body override on purpose: the refusal is about the
          ACTION, so a caller that replaces the whole body with `content.custom`
          must still be able to report one. */}
      {errorMessage !== undefined && errorMessage !== '' && (
        <Typography
          role="alert"
          variant="bodySmall"
          sx={errorSx}
        >
          {errorMessage}
        </Typography>
      )}
    </>
  );

  return (
    <BaseModal
      open={open}
      variant="simple"
      title={resolvedCopy.title}
      content={body}
      onClose={onClose}
      onConfirm={onConfirm}
      onKeyDown={handleKeyDown}
      actions={{
        alarm,
        confirmText: resolvedCopy.confirmText,
        cancelText: resolvedCopy.cancelText,
        confirming: confirmDisabled || confirming,
      }}
      {...(dataTestId !== undefined ? { 'data-testid': dataTestId } : {})}
    />
  );
}
