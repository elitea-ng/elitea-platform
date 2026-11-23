/**
 * Webhooks settings content (#876) — the management UI for the outbound
 * project webhook registry that, until this issue, was API-only. See
 * `apps/elitea-docs/src/content/docs/how-tos/credentials-toolkits/
 * webhooks-and-triggers.mdx`'s "Webhook API" section, which this replaces
 * the "Not available yet" note under.
 *
 * Composes `DrawerPageHeader` + `WebhooksTable` + the generated `webhooks`
 * client (`shared/api/generated/webhooks`) — no hand-written client, per the
 * issue's own instruction to reuse the generated one once the spec describes
 * these routes.
 *
 * `useListWebhooks` (a real GET) is the one generated hook this page uses
 * as-is. The four writes go through the generated RAW functions
 * (`createWebhook`/`updateWebhook`/`rotateWebhookSecret`/`deleteWebhook`)
 * wrapped in a local `useMutation`, the same pattern `RequestModelConnection.tsx` uses for
 * `createModerationRequest`: `orval.config.ts`'s `query.useQuery: true`
 * makes every generated `useXxx` a query-shaped hook regardless of HTTP verb
 * (there is no generated `useMutation` variant), so a write needs its own
 * `useMutation` wrapper rather than the generated `useCreateWebhook` etc.
 *
 * The signing secret is write-only (issue 2963). No read returns it, so the
 * page never holds a stored secret: an edit or a toggle omits `secret` and
 * the server keeps it, and the value is shown exactly once, from the create
 * or rotate response, in `WebhookSecretDialog`.
 */
import { memo, useCallback, useMemo, useState } from 'react';

import { useMutation, useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Paper from '@mui/material/Paper';
import Snackbar from '@mui/material/Snackbar';
import type { SxProps, Theme } from '@mui/material/styles';

import { serverErrorMessage } from '@/shared/api/serverErrorMessage';
import { t } from '@/shared/i18n';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import type { ListWebhooks200, Webhook, WebhookWithSecret, WebhookWriteRequest } from '@/shared/api/generated/model';
import {
  createWebhook,
  deleteWebhook,
  getListWebhooksQueryKey,
  rotateWebhookSecret,
  updateWebhook,
  useListWebhooks,
} from '@/shared/api/generated/webhooks/webhooks';

import { webhooksFeature } from '@/features/settings';
import type { WebhookFormValues, WebhookViewRow } from '@/features/settings';

const { useWebhookPermissions, generateWebhookSecret, WebhookFormDialog, WebhookSecretDialog, WebhooksTable } =
  webhooksFeature;

const EMPTY_WEBHOOKS: Webhook[] = [];

interface ToastState {
  readonly severity: 'success' | 'error';
  readonly message: string;
}

function toViewRow(webhook: Webhook): WebhookViewRow {
  return {
    id: webhook.id,
    url: webhook.url,
    events: webhook.events,
    hasSecret: webhook.has_secret,
    active: webhook.active,
  };
}

/**
 * The create and update body. An update omits `secret`, which keeps the
 * stored one: the page cannot send it back, because no read returns it.
 */
function writeBody(url: string, events: readonly string[], active: boolean, secret?: string): WebhookWriteRequest {
  return secret === undefined ? { url, events: [...events], active } : { url, events: [...events], secret, active };
}

export const WebhooksContent = memo(function WebhooksContent() {
  const projectId = useSelectedProjectStore((s) => s.project?.id ?? '');
  const { canList, controls: permissions } = useWebhookPermissions(projectId);
  const queryClient = useQueryClient();

  const listQuery = useListWebhooks(projectId, { query: { enabled: !!projectId && canList } });
  const listBody = listQuery.data?.data as ListWebhooks200 | undefined;
  const webhooks = listBody?.items ?? EMPTY_WEBHOOKS;
  const rows = useMemo(() => webhooks.map(toViewRow), [webhooks]);

  const invalidateList = useCallback(
    () => void queryClient.invalidateQueries({ queryKey: getListWebhooksQueryKey(projectId) }),
    [queryClient, projectId],
  );

  const createMutation = useMutation({
    mutationFn: (body: WebhookWriteRequest) => createWebhook(projectId, body),
    onSuccess: invalidateList,
  });
  const updateMutation = useMutation({
    mutationFn: ({ webhookID, body }: { webhookID: string; body: WebhookWriteRequest }) =>
      updateWebhook(projectId, webhookID, body),
    onSuccess: invalidateList,
  });
  const rotateMutation = useMutation({
    mutationFn: (webhookID: string) => rotateWebhookSecret(projectId, webhookID),
    onSuccess: invalidateList,
  });
  const deleteMutation = useMutation({
    mutationFn: (webhookID: string) => deleteWebhook(projectId, webhookID),
    onSuccess: invalidateList,
  });

  const [toast, setToast] = useState<ToastState | null>(null);
  const closeToast = useCallback(() => setToast(null), []);
  const onError = useCallback((message: string) => () => setToast({ severity: 'error', message }), []);

  /* ── create / edit form ───────────────────────────────────────────── */
  const [formOpen, setFormOpen] = useState(false);
  const [editingRow, setEditingRow] = useState<WebhookViewRow | null>(null);
  const [revealSecret, setRevealSecret] = useState<string | null>(null);
  // The server's own reason the LAST submit was refused (typically the SSRF
  // guard's 400 — internal/api/webhook/ssrf.go), surfaced inline on the URL
  // field rather than only as the generic toast below. `undefined` when the
  // form has not failed since it was last opened.
  const [formServerError, setFormServerError] = useState<string | undefined>(undefined);

  const openCreate = useCallback(() => {
    setEditingRow(null);
    setFormServerError(undefined);
    setFormOpen(true);
  }, []);
  const openEdit = useCallback((row: WebhookViewRow) => {
    setEditingRow(row);
    setFormServerError(undefined);
    setFormOpen(true);
  }, []);
  const closeForm = useCallback(() => setFormOpen(false), []);

  const submitForm = useCallback(
    (values: WebhookFormValues) => {
      setFormServerError(undefined);
      if (editingRow) {
        // An edit keeps the row's existing secret — rotating is a separate
        // action (see WebhookFormDialog's own header comment on why).
        updateMutation.mutate(
          { webhookID: editingRow.id, body: writeBody(values.url, values.events, values.active) },
          {
            onSuccess: () => setFormOpen(false),
            onError: (error) => {
              const message = serverErrorMessage(
                error,
                t('entities.webhook.error.updateFailed', 'Failed to update the webhook'),
              );
              setFormServerError(message);
              onError(message)();
            },
          },
        );
        return;
      }
      const secret = generateWebhookSecret();
      createMutation.mutate(writeBody(values.url, values.events, values.active, secret), {
        onSuccess: (response) => {
          setFormOpen(false);
          // The create response is the one place the server returns the
          // secret; the dialog shows that value, once.
          setRevealSecret((response.data as WebhookWithSecret).secret ?? secret);
        },
        onError: (error) => {
          const message = serverErrorMessage(
            error,
            t('entities.webhook.error.createFailed', 'Failed to create the webhook'),
          );
          setFormServerError(message);
          onError(message)();
        },
      });
    },
    [editingRow, createMutation, updateMutation, onError],
  );

  /* ── rotate ────────────────────────────────────────────────────────── */
  // The server generates the new value and returns it in this response only.
  const rotateSecret = useCallback(
    (row: WebhookViewRow) => {
      rotateMutation.mutate(row.id, {
        onSuccess: (response) => setRevealSecret((response.data as WebhookWithSecret).secret ?? ''),
        onError: onError(t('entities.webhook.error.rotateFailed', 'Failed to rotate the secret')),
      });
    },
    [rotateMutation, onError],
  );

  /* ── enable / disable ─────────────────────────────────────────────── */
  const toggleActive = useCallback(
    (row: WebhookViewRow, active: boolean) => {
      updateMutation.mutate(
        { webhookID: row.id, body: writeBody(row.url, row.events, active) },
        { onError: onError(t('entities.webhook.error.updateFailed', 'Failed to update the webhook')) },
      );
    },
    [updateMutation, onError],
  );

  /* ── delete ────────────────────────────────────────────────────────── */
  const deleteWebhookRow = useCallback(
    (row: WebhookViewRow) => {
      deleteMutation.mutate(row.id, {
        onError: onError(t('entities.webhook.error.deleteFailed', 'Failed to delete the webhook')),
      });
    },
    [deleteMutation, onError],
  );

  return (
    <Paper elevation={0} sx={styles.root}>
      <DrawerPageHeader
        title={t('entities.webhook.pageTitle', 'Webhooks')}
        showAddButton={permissions.canCreate}
        slotProps={{
          addButton: {
            onAdd: openCreate,
            disabled: listQuery.isFetching,
            tooltip: t('entities.webhook.addTooltip', 'Register a new webhook'),
          },
        }}
      />
      <Box sx={styles.content}>
        <WebhooksTable
          projectId={projectId}
          rows={rows}
          isLoading={listQuery.isFetching}
          permissions={permissions}
          onEdit={openEdit}
          onRotate={rotateSecret}
          onToggleActive={toggleActive}
          onDelete={deleteWebhookRow}
        />
      </Box>
      <WebhookFormDialog
        open={formOpen}
        isSaving={createMutation.isPending || updateMutation.isPending}
        initialValues={editingRow ? { url: editingRow.url, events: editingRow.events, active: editingRow.active } : undefined}
        serverError={formServerError}
        onClose={closeForm}
        onSubmit={submitForm}
      />
      <WebhookSecretDialog
        open={revealSecret !== null}
        secret={revealSecret ?? ''}
        onClose={() => setRevealSecret(null)}
      />
      <Snackbar open={toast !== null} autoHideDuration={4000} onClose={closeToast} anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}>
        {toast ? (
          <Alert onClose={closeToast} severity={toast.severity} variant="filled">
            {toast.message}
          </Alert>
        ) : undefined}
      </Snackbar>
    </Paper>
  );
});

const styles: Record<string, SxProps<Theme>> = {
  root: {
    display: 'flex',
    flexDirection: 'column',
    height: '100%',
    overflow: 'hidden',
    borderRadius: 'var(--el-shape-radiusSm, 0px)',
  },
  content: {
    flex: 1,
    minHeight: 0,
    padding: '0 1.5rem 1.5rem',
    overflow: 'auto',
  },
};
