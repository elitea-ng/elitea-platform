/**
 * Webhooks settings content (#876) — the management UI for the outbound
 * project webhook registry that, until this issue, was API-only. See
 * `apps/elitea-web/src/entries/docs/content/how-tos/credentials-toolkits/
 * webhooks-and-triggers.mdx`'s "Webhook API" section, which this replaces
 * the "Not available yet" note under.
 *
 * Composes `DrawerPageHeader` + `WebhooksTable` + the generated `webhooks`
 * client (`shared/api/generated/webhooks`) — no hand-written client, per the
 * issue's own instruction to reuse the generated one once the spec describes
 * these routes.
 *
 * `useListWebhooks` (a real GET) is the one generated hook this page uses
 * as-is. The three writes go through the generated RAW functions
 * (`createWebhook`/`updateWebhook`/`deleteWebhook`) wrapped in a local
 * `useMutation`, the same pattern `RequestModelConnection.tsx` uses for
 * `createModerationRequest`: `orval.config.ts`'s `query.useQuery: true`
 * makes every generated `useXxx` a query-shaped hook regardless of HTTP verb
 * (there is no generated `useMutation` variant), so a write needs its own
 * `useMutation` wrapper rather than the generated `useCreateWebhook` etc.
 */
import { memo, useCallback, useMemo, useState } from 'react';

import { useMutation, useQueryClient } from '@tanstack/react-query';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Paper from '@mui/material/Paper';
import Snackbar from '@mui/material/Snackbar';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { useSelectedProjectStore } from '@/widgets/app-shell';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import type { ListWebhooks200, Webhook, WebhookWriteRequest } from '@/shared/api/generated/model';
import {
  createWebhook,
  deleteWebhook,
  getListWebhooksQueryKey,
  updateWebhook,
  useListWebhooks,
} from '@/shared/api/generated/webhooks/webhooks';

import { webhooksFeature } from '@/features/settings';
import type { WebhookFormValues, WebhookViewRow } from '@/features/settings';

const { useWebhookPermissions, generateWebhookSecret, WebhookFormDialog, WebhookSecretDialog, WebhooksTable } = webhooksFeature;

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
    secret: webhook.secret ?? '',
    active: webhook.active,
  };
}

/** The three writes' shared shape — every one replaces the full record (handler.go's Update has no partial-patch mode). */
function writeBody(url: string, events: readonly string[], secret: string, active: boolean): WebhookWriteRequest {
  return { url, events: [...events], secret, active };
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

  const openCreate = useCallback(() => {
    setEditingRow(null);
    setFormOpen(true);
  }, []);
  const openEdit = useCallback((row: WebhookViewRow) => {
    setEditingRow(row);
    setFormOpen(true);
  }, []);
  const closeForm = useCallback(() => setFormOpen(false), []);

  const submitForm = useCallback(
    (values: WebhookFormValues) => {
      if (editingRow) {
        // An edit keeps the row's existing secret — rotating is a separate
        // action (see WebhookFormDialog's own header comment on why).
        updateMutation.mutate(
          { webhookID: editingRow.id, body: writeBody(values.url, values.events, editingRow.secret, values.active) },
          {
            onSuccess: () => setFormOpen(false),
            onError: onError(t('entities.webhook.error.updateFailed', 'Failed to update the webhook')),
          },
        );
        return;
      }
      const secret = generateWebhookSecret();
      createMutation.mutate(writeBody(values.url, values.events, secret, values.active), {
        onSuccess: () => {
          setFormOpen(false);
          setRevealSecret(secret);
        },
        onError: onError(t('entities.webhook.error.createFailed', 'Failed to create the webhook')),
      });
    },
    [editingRow, createMutation, updateMutation, onError],
  );

  /* ── rotate ────────────────────────────────────────────────────────── */
  const rotateSecret = useCallback(
    (row: WebhookViewRow) => {
      const secret = generateWebhookSecret();
      updateMutation.mutate(
        { webhookID: row.id, body: writeBody(row.url, row.events, secret, row.active) },
        {
          onSuccess: () => setRevealSecret(secret),
          onError: onError(t('entities.webhook.error.rotateFailed', 'Failed to rotate the secret')),
        },
      );
    },
    [updateMutation, onError],
  );

  /* ── enable / disable ─────────────────────────────────────────────── */
  const toggleActive = useCallback(
    (row: WebhookViewRow, active: boolean) => {
      updateMutation.mutate(
        { webhookID: row.id, body: writeBody(row.url, row.events, row.secret, active) },
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
