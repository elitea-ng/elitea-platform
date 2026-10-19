/**
 * HTTP actions settings content (T-5D) — the project's HTTP action rules, the
 * only destinations a pipeline `type: http` node may call.
 *
 * Same composition as `Webhooks.tsx`: `DrawerPageHeader` + a plain table +
 * the generated `httpactions` client, with writes wrapped in a local
 * `useMutation` (orval generates query-shaped hooks only).
 *
 * Three views, by permission:
 *  - `models.http_action_rules.manage`: every rule, Add, Edit and the enabled
 *    switch. Every write is a PUT of the full record with the rule's current
 *    `revision`, so a stale edit answers 409 instead of overwriting.
 *  - `models.http_action_rules.list` only: names and enabled flags, read-only,
 *    from the names route; the full-rules route is never asked.
 *  - neither: the no-permission banner, and no rules request at all.
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
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import type {
  HttpActionRule,
  HttpActionRuleWriteRequest,
  ListHttpActionRuleNames200,
  ListHttpActionRules200,
} from '@/shared/api/generated/model';
import {
  createHttpActionRule,
  getListHttpActionRulesQueryKey,
  updateHttpActionRule,
  useListHttpActionRuleNames,
  useListHttpActionRules,
} from '@/shared/api/generated/httpactions/httpactions';

import { httpActionsFeature, webhooksFeature } from '@/features/settings';
import type { HttpActionRuleFormValues } from '@/features/settings';

const { useHttpActionRulePermissions, HttpActionRuleFormDialog, HttpActionRulesTable } = httpActionsFeature;
// The same `{error}`/`{message}` body extraction the Webhooks tab uses.
const { webhookServerErrorMessage: serverErrorMessage } = webhooksFeature;

/** The write body for a rule's current values; `output_bucket` is omitted when blank (the schema refuses ''). */
function writeBody(values: HttpActionRuleFormValues, revision?: number): HttpActionRuleWriteRequest {
  const { output_bucket: outputBucket, ...rest } = values;
  return { ...rest, ...(outputBucket ? { output_bucket: outputBucket } : {}), ...(revision === undefined ? {} : { revision }) };
}

function toFormValues(rule: HttpActionRule): HttpActionRuleFormValues {
  const { name, origin, path_template, method, query_names, configuration_id, toolkit, tool, replay, enabled } = rule;
  return { name, origin, path_template, method, query_names, configuration_id, toolkit, tool, output_bucket: rule.output_bucket ?? '', replay, enabled };
}

interface RulesBodyProps {
  readonly projectId: string;
  readonly permissions: ReturnType<typeof useHttpActionRulePermissions>;
  readonly onEdit: (rule: HttpActionRule) => void;
  readonly onToggleEnabled: (rule: HttpActionRule, enabled: boolean) => void;
}

/** The table (full rules or names only) or the no-permission banner; nothing until permissions resolve. */
function RulesBody({ projectId, permissions, onEdit, onToggleEnabled }: RulesBodyProps) {
  const { resolved, canManage, canList } = permissions;
  // No project means no permission request, so both flags are false without a project id.
  const rulesQuery = useListHttpActionRules(projectId, { query: { enabled: canManage } });
  const namesQuery = useListHttpActionRuleNames(projectId, { query: { enabled: canList && !canManage } });

  if (!resolved) return null;
  if (!canManage && !canList) {
    return <BannerMessage variant="info" message={t('entities.httpActionRule.noAccess', 'You do not have permission to view this setting.')} />;
  }
  if (!canManage) {
    const names = (namesQuery.data?.data as ListHttpActionRuleNames200 | undefined)?.items;
    return <HttpActionRulesTable isLoading={namesQuery.isFetching} names={names} />;
  }
  const rules = (rulesQuery.data?.data as ListHttpActionRules200 | undefined)?.items ?? [];
  return <HttpActionRulesTable isLoading={rulesQuery.isFetching} rules={rules} onEdit={onEdit} onToggleEnabled={onToggleEnabled} />;
}

export const HttpActionsContent = memo(function HttpActionsContent() {
  const projectId = useSelectedProjectStore((s) => s.project?.id ?? '');
  const permissions = useHttpActionRulePermissions(projectId);
  const queryClient = useQueryClient();

  const invalidateList = useCallback(
    () => void queryClient.invalidateQueries({ queryKey: getListHttpActionRulesQueryKey(projectId) }),
    [queryClient, projectId],
  );
  const createMutation = useMutation({
    mutationFn: (body: HttpActionRuleWriteRequest) => createHttpActionRule(projectId, body),
    onSuccess: invalidateList,
  });
  const updateMutation = useMutation({
    mutationFn: ({ ruleID, body }: { ruleID: string; body: HttpActionRuleWriteRequest }) =>
      updateHttpActionRule(projectId, ruleID, body),
    // Also after a failure: a 409 means the list holds a stale revision.
    onSettled: invalidateList,
  });

  const [toast, setToast] = useState<string | null>(null);
  const [formOpen, setFormOpen] = useState(false);
  const [editingRule, setEditingRule] = useState<HttpActionRule | null>(null);
  const [formServerError, setFormServerError] = useState<string | undefined>(undefined);
  // Stable per opened rule: the dialog resets its fields whenever this identity changes.
  const initialValues = useMemo(() => (editingRule ? toFormValues(editingRule) : undefined), [editingRule]);

  const openForm = useCallback((rule: HttpActionRule | null) => {
    setEditingRule(rule);
    setFormServerError(undefined);
    setFormOpen(true);
  }, []);

  const submitForm = useCallback(
    (values: HttpActionRuleFormValues) => {
      setFormServerError(undefined);
      const onError = (error: unknown) => {
        const message = serverErrorMessage(error, t('entities.httpActionRule.error.saveFailed', 'Failed to save the HTTP action rule'));
        setFormServerError(message);
        setToast(message);
      };
      const onSuccess = () => setFormOpen(false);
      if (editingRule) {
        updateMutation.mutate({ ruleID: String(editingRule.id), body: writeBody(values, editingRule.revision) }, { onSuccess, onError });
      } else {
        createMutation.mutate(writeBody(values), { onSuccess, onError });
      }
    },
    [editingRule, createMutation, updateMutation],
  );

  const toggleEnabled = useCallback(
    (rule: HttpActionRule, enabled: boolean) => {
      updateMutation.mutate(
        { ruleID: String(rule.id), body: writeBody({ ...toFormValues(rule), enabled }, rule.revision) },
        { onError: (error) => setToast(serverErrorMessage(error, t('entities.httpActionRule.error.saveFailed', 'Failed to save the HTTP action rule'))) },
      );
    },
    [updateMutation],
  );

  const closeToast = useCallback(() => setToast(null), []);
  const closeForm = useCallback(() => setFormOpen(false), []);

  return (
    <Paper elevation={0} sx={styles.root}>
      <DrawerPageHeader
        title={t('entities.httpActionRule.pageTitle', 'HTTP actions')}
        showAddButton={permissions.canManage}
        slotProps={{
          addButton: {
            onAdd: () => openForm(null),
            tooltip: t('entities.httpActionRule.addTooltip', 'Add an HTTP action rule'),
          },
        }}
      />
      <Box sx={styles.content}>
        <RulesBody projectId={projectId} permissions={permissions} onEdit={openForm} onToggleEnabled={toggleEnabled} />
      </Box>
      <HttpActionRuleFormDialog
        open={formOpen}
        projectId={projectId}
        isSaving={createMutation.isPending || updateMutation.isPending}
        initialValues={initialValues}
        serverError={formServerError}
        onClose={closeForm}
        onSubmit={submitForm}
      />
      <Snackbar open={toast !== null} autoHideDuration={4000} onClose={closeToast} anchorOrigin={{ vertical: 'bottom', horizontal: 'center' }}>
        {toast ? (
          <Alert onClose={closeToast} severity="error" variant="filled">
            {toast}
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
