/**
 * DB actions settings content (T-5E 5e-2) — the project's DB action rules,
 * the only databases and modes a pipeline `type: db` node may run against.
 *
 * Same composition as `HttpActions.tsx`: `DrawerPageHeader` + a plain table +
 * the generated `dbactions` client, with writes wrapped in a local
 * `useMutation`.
 *
 * Three views, by permission:
 *  - `models.db_action_rules.manage`: every rule, Add, Edit and the enabled
 *    switch (disable, never hard-delete). Every write is a PUT of the full
 *    record with the rule's current `revision` so stale edits answer 409.
 *  - `models.db_action_rules.list` only: redacted rows (name, dialect, mode,
 *    enabled) read-only from the names route; the full-rules route is never
 *    requested.
 *  - neither: the no-permission banner, and no rules request at all.
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
import { BannerMessage } from '@/shared/ui/BannerMessage';
import { DrawerPageHeader } from '@/shared/ui/settings/DrawerPageHeader';
import type {
  DbActionRule,
  DbActionRuleWriteRequest,
  ListDbActionRuleNames200,
  ListDbActionRules200,
} from '@/shared/api/generated/model';
import {
  createDbActionRule,
  getListDbActionRulesQueryKey,
  updateDbActionRule,
  useListDbActionRuleNames,
  useListDbActionRules,
} from '@/shared/api/generated/dbactions/dbactions';

import { actionRulesFeature } from '@/features/settings';

const { useDbActionRulePermissions, DbActionRuleFormDialog, DbActionRulesTable } = actionRulesFeature.db;
type DbActionRuleFormValues = Parameters<Parameters<typeof DbActionRuleFormDialog>[0]['onSubmit']>[0];

/** The write body for a rule's current values; `marker_table` is omitted for read rules. */
function writeBody(values: DbActionRuleFormValues, revision?: number): DbActionRuleWriteRequest {
  const { marker_table: markerTable, ...rest } = values;
  return {
    ...rest,
    ...(values.mode === 'write' && markerTable ? { marker_table: markerTable } : {}),
    ...(revision === undefined ? {} : { revision }),
  };
}

function toFormValues(rule: DbActionRule): DbActionRuleFormValues {
  const { name, configuration_id, dialect, database_name, mode, enabled } = rule;
  return { name, configuration_id, dialect, database_name, mode, marker_table: rule.marker_table ?? '', enabled };
}

interface RulesBodyProps {
  readonly projectId: string;
  readonly permissions: ReturnType<typeof useDbActionRulePermissions>;
  readonly onEdit: (rule: DbActionRule) => void;
  readonly onToggleEnabled: (rule: DbActionRule, enabled: boolean) => void;
}

/** The table (full rules or redacted names) or the no-permission banner; nothing until permissions resolve. */
function RulesBody({ projectId, permissions, onEdit, onToggleEnabled }: RulesBodyProps) {
  const { resolved, canManage, canList } = permissions;
  const rulesQuery = useListDbActionRules(projectId, { query: { enabled: canManage } });
  const namesQuery = useListDbActionRuleNames(projectId, { query: { enabled: canList && !canManage } });

  if (!resolved) return null;
  if (!canManage && !canList) {
    return <BannerMessage variant="info" message={t('entities.dbActionRule.noAccess', 'You do not have permission to view this setting.')} />;
  }
  if (!canManage) {
    const names = (namesQuery.data?.data as ListDbActionRuleNames200 | undefined)?.items;
    return <DbActionRulesTable isLoading={namesQuery.isFetching} names={names} />;
  }
  const rules = (rulesQuery.data?.data as ListDbActionRules200 | undefined)?.items ?? [];
  return <DbActionRulesTable isLoading={rulesQuery.isFetching} rules={rules} onEdit={onEdit} onToggleEnabled={onToggleEnabled} />;
}

export const DbActionsContent = memo(function DbActionsContent() {
  const projectId = useSelectedProjectStore((s) => s.project?.id ?? '');
  const permissions = useDbActionRulePermissions(projectId);
  const queryClient = useQueryClient();

  const invalidateList = useCallback(
    () => void queryClient.invalidateQueries({ queryKey: getListDbActionRulesQueryKey(projectId) }),
    [queryClient, projectId],
  );
  const createMutation = useMutation({
    mutationFn: (body: DbActionRuleWriteRequest) => createDbActionRule(projectId, body),
    onSuccess: invalidateList,
  });
  const updateMutation = useMutation({
    mutationFn: ({ ruleID, body }: { ruleID: string; body: DbActionRuleWriteRequest }) =>
      updateDbActionRule(projectId, ruleID, body),
    // Also after a failure: a 409 means the list holds a stale revision.
    onSettled: invalidateList,
  });

  const [toast, setToast] = useState<string | null>(null);
  const [formOpen, setFormOpen] = useState(false);
  const [editingRule, setEditingRule] = useState<DbActionRule | null>(null);
  const [formServerError, setFormServerError] = useState<string | undefined>(undefined);
  const initialValues = useMemo(() => (editingRule ? toFormValues(editingRule) : undefined), [editingRule]);

  const openForm = useCallback((rule: DbActionRule | null) => {
    setEditingRule(rule);
    setFormServerError(undefined);
    setFormOpen(true);
  }, []);

  const submitForm = useCallback(
    (values: DbActionRuleFormValues) => {
      setFormServerError(undefined);
      const onError = (error: unknown) => {
        const message = serverErrorMessage(error, t('entities.dbActionRule.error.saveFailed', 'Failed to save the DB action rule'));
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
    (rule: DbActionRule, enabled: boolean) => {
      updateMutation.mutate(
        { ruleID: String(rule.id), body: writeBody({ ...toFormValues(rule), enabled }, rule.revision) },
        { onError: (error) => setToast(serverErrorMessage(error, t('entities.dbActionRule.error.saveFailed', 'Failed to save the DB action rule'))) },
      );
    },
    [updateMutation],
  );

  const closeToast = useCallback(() => setToast(null), []);
  const closeForm = useCallback(() => setFormOpen(false), []);

  return (
    <Paper elevation={0} sx={styles.root}>
      <DrawerPageHeader
        title={t('entities.dbActionRule.pageTitle', 'DB actions')}
        showAddButton={permissions.canManage}
        slotProps={{
          addButton: {
            onAdd: () => openForm(null),
            tooltip: t('entities.dbActionRule.addTooltip', 'Add a DB action rule'),
          },
        }}
      />
      <Box sx={styles.content}>
        <RulesBody projectId={projectId} permissions={permissions} onEdit={openForm} onToggleEnabled={toggleEnabled} />
      </Box>
      <DbActionRuleFormDialog
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
