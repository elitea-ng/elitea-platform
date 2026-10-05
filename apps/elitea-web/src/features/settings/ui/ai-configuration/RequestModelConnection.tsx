/**
 * RequestModelConnection — Settings › AI Configuration's "Request a model
 * connection" affordance: one button, one small dialog, one POST.
 *
 * ## The wire
 *
 * There is NO new route and no new column. This files a row into the same
 * `centry.moderation_state` table, over the same create call, that the App
 * Catalogue's "Request Access" card already uses
 * (`POST /admin/moderation_status/default/{projectId}/{entityId}` —
 * `services/elitea-main/internal/api/v2/moderation/requests.go`). Only two
 * values differ:
 *
 *   - `issue_type` is `Model Connection Request`, which is what separates this
 *     from the app-access queue in the operator's admin listing (the queue's
 *     `issue_type` filter is the only discriminator between them);
 *   - `entity_id` is `provider:<type>` or `model:<name>`, so one request names
 *     exactly one thing and two people asking for the same provider land on
 *     the same address.
 *
 * The server takes the author from the session and always stores `pending`,
 * and it refuses a non-empty `meta`, so neither is sent — the requester owns
 * `issue_type` and `description`, nothing else.
 *
 * ## Approval is CLERICAL
 *
 * Nothing is provisioned when an operator approves. The row's `status` moves
 * and the requester gets a notification; no configuration is created and no
 * connection is verified. That is pinned server-side by
 * `TestApprovingAModelConnectionProvisionsNothing`. Do not add a
 * provisioning hook here or on the decision path without also changing what
 * this dialog tells the user it is doing.
 *
 * ## Why this does not reuse `features/apps`' `useModerationRequests`
 *
 * That hook holds the same POST machinery, but it lives in another feature
 * slice and `.dependency-cruiser.cjs`'s `no-sideways-features` forbids
 * `features/settings` importing it — the same constraint, and the same
 * resolution, as `ConfigurationSection.tsx`'s local `useCanEditConfiguration`.
 * It is also catalogue-shaped: it drives a `useQueries` over
 * `APPLICATION_CATALOG`, which would cost this page two status requests for
 * entries it never renders. The generated client in `shared/` is the shared
 * part, and it is what both call.
 *
 * The caller-side request LIST is deliberately not this component's job: it
 * files a request and says so. Reading back "you already asked for this" is a
 * separate surface.
 */
import { useCallback, useState } from 'react';
import type { ChangeEvent } from 'react';

import AddLinkIcon from '@mui/icons-material/AddLink';
import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Snackbar from '@mui/material/Snackbar';
import Typography from '@mui/material/Typography';
import type { Theme } from '@mui/material/styles';

import { useMutation } from '@tanstack/react-query';

import { createModerationRequest } from '@/shared/api/generated/admin/admin';
import type { ModerationRequestCreate } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';
import { RadioButtonGroup } from '@/shared/ui/RadioButtonGroup';

/** The `issue_type` an operator's model-connection queue filters on. */
export const MODEL_CONNECTION_ISSUE_TYPE = 'Model Connection Request';

const TOAST_AUTO_HIDE_MS = 4000;

/** Which of the two things a request can name. */
export type ModelConnectionKind = 'provider' | 'model';

/**
 * `provider:<type>` / `model:<name>`, the address the request is filed
 * against.
 *
 * The name is percent-encoded, the `kind:` prefix is not. `entity_id` travels
 * as a PATH SEGMENT (`getCreateModerationRequestUrl` interpolates it raw), so
 * a model name containing a slash — `meta-llama/Llama-3.1-70B` and every other
 * vendor-prefixed id — would otherwise split the segment and address a route
 * that does not exist. A colon in a path segment is legal and needs no
 * encoding, and leaving it literal keeps the stored value readable in the
 * admin queue, which is the only place a human ever reads it.
 */
export function buildModelConnectionEntityId(kind: ModelConnectionKind, name: string): string {
  return `${kind}:${encodeURIComponent(name.trim())}`;
}

interface RequestModelConnectionDialogProps {
  open: boolean;
  isSubmitting: boolean;
  onClose: () => void;
  onSubmit: (kind: ModelConnectionKind, name: string, description: string) => void;
}

const contentSx = {
  display: 'flex',
  flexDirection: 'column' as const,
  gap: '1.25rem',
};

const fieldWrapperSx = {
  display: 'flex',
  flexDirection: 'column' as const,
  gap: '0.5rem',
};

const actionsSx = {
  display: 'flex',
  alignItems: 'center',
  justifyContent: 'flex-end',
  gap: '0.5rem',
};

function secondaryTextSx(theme: Theme) {
  return { color: theme.vars.palette.text.secondary };
}

/** The dialog's own copy for the free-text field, which names a provider type or a model. */
function nameFieldCopy(kind: ModelConnectionKind): { label: string; placeholder: string } {
  if (kind === 'provider') {
    return {
      label: t('ai-configuration.requestModelConnection.providerLabel', 'Provider type *'),
      placeholder: t('ai-configuration.requestModelConnection.providerPlaceholder', 'For example: anthropic'),
    };
  }
  return {
    label: t('ai-configuration.requestModelConnection.modelLabel', 'Model name *'),
    placeholder: t('ai-configuration.requestModelConnection.modelPlaceholder', 'For example: claude-opus-4'),
  };
}

/**
 * The form. Split from the button/toast host below so each stays inside the
 * §3.5 complexity budget, and so the host owns no field state — every field
 * resets by remount when the dialog is reopened.
 */
function RequestModelConnectionDialog({ open, isSubmitting, onClose, onSubmit }: RequestModelConnectionDialogProps) {
  const [kind, setKind] = useState<ModelConnectionKind>('provider');
  const [name, setName] = useState('');
  const [description, setDescription] = useState('');
  const [nameError, setNameError] = useState('');
  const [descriptionError, setDescriptionError] = useState('');

  const handleKindChange = useCallback((value: string) => {
    setKind(value === 'model' ? 'model' : 'provider');
  }, []);

  const handleNameChange = useCallback((event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    setName(event.target.value);
    setNameError('');
  }, []);

  const handleDescriptionChange = useCallback((event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
    setDescription(event.target.value);
    setDescriptionError('');
  }, []);

  const handleSubmit = useCallback(() => {
    const trimmedName = name.trim();
    const trimmedDescription = description.trim();
    /* Both messages are set in one pass rather than short-circuiting on the
       first: a form that reveals its second problem only after the first is
       fixed costs the user a round trip per field. */
    setNameError(
      trimmedName ? '' : t('ai-configuration.requestModelConnection.nameRequired', 'Name what should be connected'),
    );
    setDescriptionError(
      trimmedDescription
        ? ''
        : t('ai-configuration.requestModelConnection.descriptionRequired', 'Describe why this project needs it'),
    );
    if (!trimmedName || !trimmedDescription) return;
    onSubmit(kind, trimmedName, trimmedDescription);
  }, [description, kind, name, onSubmit]);

  const nameCopy = nameFieldCopy(kind);

  const content = (
    <Box sx={contentSx}>
      <Typography
        variant="bodyMedium"
        sx={secondaryTextSx}
      >
        {t(
          'ai-configuration.requestModelConnection.description',
          'Ask an operator to connect a provider or a model to this project. Approval is recorded and you are notified — it does not create the configuration for you.',
        )}
      </Typography>

      <Box sx={fieldWrapperSx}>
        <Typography variant="bodyMedium">
          {t('ai-configuration.requestModelConnection.kindLabel', 'What should be connected?')}
        </Typography>
        <RadioButtonGroup
          aria-label={t('ai-configuration.requestModelConnection.kindLabel', 'What should be connected?')}
          value={kind}
          onChange={handleKindChange}
          wrapRow
          items={[
            { value: 'provider', label: t('ai-configuration.requestModelConnection.kindProvider', 'A provider') },
            { value: 'model', label: t('ai-configuration.requestModelConnection.kindModel', 'A model') },
          ]}
        />
      </Box>

      <Box sx={fieldWrapperSx}>
        <InputBase
          label={nameCopy.label}
          value={name}
          error={Boolean(nameError)}
          helperText={nameError}
          placeholder={nameCopy.placeholder}
          onChange={handleNameChange}
        />
      </Box>

      <Box sx={fieldWrapperSx}>
        <InputBase
          label={t('ai-configuration.requestModelConnection.reasonLabel', 'Description *')}
          value={description}
          error={Boolean(descriptionError)}
          helperText={descriptionError}
          placeholder={t(
            'ai-configuration.requestModelConnection.reasonPlaceholder',
            'Describe what this project needs the connection for...',
          )}
          onChange={handleDescriptionChange}
          expand={{ minRows: 5, maxRows: 5 }}
        />
      </Box>
    </Box>
  );

  const actions = (
    <Box sx={actionsSx}>
      <BaseBtn
        variant="secondary"
        disabled={isSubmitting}
        onClick={onClose}
      >
        {t('ai-configuration.requestModelConnection.cancel', 'Cancel')}
      </BaseBtn>
      {/*
        Enabled on an empty form, unlike `features/apps`' RequestAccessModal,
        which disables its send button on `!reason.trim()`. That modal ALSO
        carries a "please provide a reason" message, and disabling the button
        makes it unreachable: the message can only be shown by a submit the
        button prevents. A disabled control explains nothing, so the validation
        below is what runs and what the user reads.
      */}
      <BaseBtn
        variant="contained"
        disabled={isSubmitting}
        onClick={handleSubmit}
      >
        {t('ai-configuration.requestModelConnection.send', 'Send request')}
      </BaseBtn>
    </Box>
  );

  return (
    <BaseModal
      open={open}
      variant="simple"
      title={t('ai-configuration.requestModelConnection.title', 'Request a model connection')}
      content={content}
      onClose={onClose}
      actions={{ node: actions }}
    />
  );
}

export interface RequestModelConnectionProps {
  /** The project the request is filed against — the same id the page is rendering. */
  projectId: string;
}

type ToastKind = 'success' | 'error' | null;

export function RequestModelConnection({ projectId }: RequestModelConnectionProps) {
  const [open, setOpen] = useState(false);
  const [toast, setToast] = useState<ToastKind>(null);

  /*
   * A locally-owned `useMutation`, NOT `useCreateModerationRequest` /
   * `getCreateModerationRequestQueryOptions`: orval generates this POST
   * query-shaped, and running a non-idempotent create through the query
   * machinery inherits `retry` and input-keyed dedup — a resubmitted request
   * could return a cached prior response with no network call, and a
   * transient failure could silently replay the POST. `useMutation` has
   * neither by default. Same reasoning, same conclusion as
   * `features/apps/api/useModerationRequests.ts`.
   */
  const { mutate, isPending } = useMutation({
    mutationFn: ({ entityId, description }: { entityId: string; description: string }) => {
      const body: ModerationRequestCreate = {
        issue_type: MODEL_CONNECTION_ISSUE_TYPE,
        description,
      };
      return createModerationRequest(projectId, entityId, body);
    },
    onSuccess: () => {
      setOpen(false);
      setToast('success');
    },
    /* The dialog stays OPEN on failure. Closing it would discard what the
       user typed, and the most likely failures here (403 without
       `admin.moderation.create`, a transport error) are ones a retry or an
       edit can answer. */
    onError: () => setToast('error'),
  });

  const handleOpen = useCallback(() => setOpen(true), []);
  const handleClose = useCallback(() => setOpen(false), []);
  const handleCloseToast = useCallback(() => setToast(null), []);

  const handleSubmit = useCallback(
    (kind: ModelConnectionKind, name: string, description: string) => {
      mutate({ entityId: buildModelConnectionEntityId(kind, name), description });
    },
    [mutate],
  );

  return (
    <>
      <BaseBtn
        variant="secondary"
        size="small"
        startIcon={<AddLinkIcon fontSize="small" />}
        onClick={handleOpen}
      >
        {t('ai-configuration.requestModelConnection.button', 'Request a model connection')}
      </BaseBtn>

      {/* Mounted only while open, so every field resets between requests
          without this component holding (or having to clear) form state. */}
      {open && (
        <RequestModelConnectionDialog
          open={open}
          isSubmitting={isPending}
          onClose={handleClose}
          onSubmit={handleSubmit}
        />
      )}

      <Snackbar
        open={toast !== null}
        autoHideDuration={TOAST_AUTO_HIDE_MS}
        onClose={handleCloseToast}
        anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}
      >
        <Alert
          onClose={handleCloseToast}
          severity={toast === 'error' ? 'error' : 'success'}
          variant="filled"
        >
          {toast === 'error'
            ? t('ai-configuration.requestModelConnection.sendFailed', 'The request could not be sent')
            : t('ai-configuration.requestModelConnection.sent', 'Your model connection request has been sent')}
        </Alert>
      </Snackbar>
    </>
  );
}
