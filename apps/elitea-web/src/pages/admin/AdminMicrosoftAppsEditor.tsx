/**
 * Admin › Configuration › Microsoft apps — the Default app registry (ADR-0057
 * decisions 5 and 11).
 *
 * An administrator registers a Microsoft Entra app once (client ID, secret,
 * authority, the credential types it serves, the permissions it may ask for, the
 * projects that may use it); people then pick "Default app" in an Outlook, Teams,
 * Microsoft Graph, Dataverse or SharePoint credential and never see a client ID or a
 * secret. The page reaches this component through the server-declared
 * `managed_surface: microsoft_apps`, never a section id.
 *
 * Everything an Entra administrator needs is printed above the list, with this
 * deployment's values: the one redirect URI, the permissions, the consent URL. With
 * the registry empty a credential form shows only Custom app; nothing here is a
 * prerequisite for it.
 *
 * The client secret is write-only: no read returns it, the field is empty on an edit,
 * and the table says only that one is stored.
 */
import { useMemo, useState } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import FormControlLabel from '@mui/material/FormControlLabel';
import LinearProgress from '@mui/material/LinearProgress';
import Switch from '@mui/material/Switch';
import Typography from '@mui/material/Typography';

import type { MicrosoftApp, MicrosoftAppRequest, MicrosoftAppTestResult } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

import { AdminWritableSection } from './AdminWritable';
import { ADMIN_ACTION_PERMISSION, ADMIN_ITEM } from './adminItemIds';
import { useAdminActionControl, useAdminWritable } from './adminWriteAccess';
import { DeploymentChecklist } from './AdminMicrosoftAppChecklist';
import { AdminMicrosoftAppDialog } from './AdminMicrosoftAppDialog';
import { AdminMicrosoftAppsTable } from './AdminMicrosoftAppsTable';
import { MicrosoftAppTestPanel } from './AdminMicrosoftAppTestPanel';
import {
  microsoftAppFailure,
  useAdminMicrosoftApps,
  useSaveMicrosoftApp,
  useSaveMicrosoftAppSettings,
  useSetMicrosoftAppEnabled,
  useTestMicrosoftApp,
} from './api/adminMicrosoftAppsApi';

interface DialogState {
  readonly open: boolean;
  readonly editing: MicrosoftApp | undefined;
  readonly serverError: string | undefined;
  readonly fieldError: { readonly field: string; readonly message: string } | undefined;
}

const CLOSED_DIALOG: DialogState = { open: false, editing: undefined, serverError: undefined, fieldError: undefined };

function refusalSentence(error: unknown, fallback: string): string {
  const failure = microsoftAppFailure(error);
  if (failure.status === 403) {
    return t('pages.admin.microsoftApps.error.forbidden', 'You do not have permission to manage Microsoft apps.');
  }
  if (failure.status === 503) {
    return t(
      'pages.admin.microsoftApps.error.unavailable',
      'The Microsoft app registry is not available on this deployment. It needs the platform vault master key.',
    );
  }
  return failure.message ?? fallback;
}

/** Microsoft apps are the Client applications item (R3.4b, issue 2613). */
export function AdminMicrosoftAppsEditor() {
  return (
    <AdminWritableSection itemId={ADMIN_ITEM.clientApplications}>
      <MicrosoftAppsEditor />
    </AdminWritableSection>
  );
}

function MicrosoftAppsEditor() {
  const listQuery = useAdminMicrosoftApps();
  const access = useAdminWritable(ADMIN_ITEM.clientApplications);
  const locked = useAdminActionControl(ADMIN_ACTION_PERMISSION.microsoftApps).disabled;
  const saveMutation = access.guard(useSaveMicrosoftApp());
  const enabledMutation = access.guard(useSetMicrosoftAppEnabled());
  const settingsMutation = access.guard(useSaveMicrosoftAppSettings());
  // The test signs in to Entra with the stored secret: a write-level action.
  const testMutation = access.guard(useTestMicrosoftApp());

  const [dialog, setDialog] = useState<DialogState>(CLOSED_DIALOG);
  const [notice, setNotice] = useState<{ severity: 'success' | 'error'; text: string } | undefined>(undefined);
  const [testResult, setTestResult] = useState<{ app: MicrosoftApp; result: MicrosoftAppTestResult } | undefined>(undefined);

  const apps = useMemo(() => listQuery.data?.apps ?? [], [listQuery.data]);
  const typeInfo = useMemo(() => listQuery.data?.checklist.types ?? [], [listQuery.data]);
  const busyIds = useMemo(() => {
    const ids = new Set<string>();
    if (enabledMutation.isPending && enabledMutation.variables) ids.add(enabledMutation.variables.id);
    if (testMutation.isPending && testMutation.variables) ids.add(testMutation.variables);
    return ids;
  }, [enabledMutation.isPending, enabledMutation.variables, testMutation.isPending, testMutation.variables]);

  const handleSubmit = (body: MicrosoftAppRequest): void => {
    setDialog((previous) => ({ ...previous, serverError: undefined, fieldError: undefined }));
    saveMutation.mutate(
      { id: dialog.editing?.id, body },
      {
        onSuccess: () => {
          setDialog(CLOSED_DIALOG);
          setNotice({ severity: 'success', text: t('pages.admin.microsoftApps.result.saved', 'Saved.') });
        },
        // The dialog STAYS OPEN holding what was typed, with the refusal beside its field.
        onError: (error: unknown) => {
          const failure = microsoftAppFailure(error);
          const message = refusalSentence(error, t('pages.admin.microsoftApps.error.save', 'Failed to save that Microsoft app.'));
          setDialog((previous) =>
            failure.field === undefined
              ? { ...previous, serverError: message }
              : { ...previous, fieldError: { field: failure.field, message } },
          );
        },
      },
    );
  };

  const handleToggle = (app: MicrosoftApp): void => {
    setNotice(undefined);
    enabledMutation.mutate(
      { id: app.id, enabled: app.state === 'disabled' },
      {
        onError: (error: unknown) => {
          setNotice({
            severity: 'error',
            text: refusalSentence(error, t('pages.admin.microsoftApps.error.save', 'Failed to save that Microsoft app.')),
          });
        },
      },
    );
  };

  const handleTest = (app: MicrosoftApp): void => {
    setNotice(undefined);
    setTestResult(undefined);
    testMutation.mutate(app.id, {
      onSuccess: (result) => {
        setTestResult({ app, result });
      },
      onError: (error: unknown) => {
        setNotice({
          severity: 'error',
          text: refusalSentence(error, t('pages.admin.microsoftApps.error.test', 'The test could not run.')),
        });
      },
    });
  };

  const handleCustomApps = (allowed: boolean): void => {
    setNotice(undefined);
    settingsMutation.mutate(allowed, {
      onError: (error: unknown) => {
        setNotice({
          severity: 'error',
          text: refusalSentence(error, t('pages.admin.microsoftApps.error.settings', 'Failed to save the setting.')),
        });
      },
    });
  };

  const showEmpty = !listQuery.isLoading && listQuery.error == null && apps.length === 0;

  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '1rem' }} data-testid="admin-microsoft-apps-editor">
      <Typography variant="bodySmall" color="text.secondary">
        {t(
          'pages.admin.microsoftApps.description',
          'Register the Microsoft Entra apps people sign in with for Outlook, Teams, Microsoft Graph, Dataverse and SharePoint credentials. People pick one as Default app and never see its client ID or secret; the secret stays in this platform.',
        )}
      </Typography>

      {listQuery.isLoading ? <LinearProgress /> : null}
      {listQuery.error == null ? null : (
        <Alert severity="warning" data-testid="admin-microsoft-apps-error">
          {refusalSentence(listQuery.error, t('pages.admin.microsoftApps.error.load', 'Failed to load the Microsoft apps.'))}
        </Alert>
      )}
      {notice === undefined ? null : (
        <Alert severity={notice.severity} onClose={() => setNotice(undefined)} data-testid="admin-microsoft-apps-notice">
          {notice.text}
        </Alert>
      )}

      {listQuery.data === undefined ? null : <DeploymentChecklist checklist={listQuery.data.checklist} />}

      {listQuery.data === undefined ? null : (
        <FormControlLabel
          label={t('pages.admin.microsoftApps.customApps', 'Allow Custom app: people may also enter their own client ID and secret')}
          control={
            <Switch
              checked={listQuery.data.settings.custom_apps}
              disabled={locked || settingsMutation.isPending}
              onChange={(event) => {
                handleCustomApps(event.target.checked);
              }}
              data-testid="admin-microsoft-apps-custom"
            />
          }
        />
      )}

      <Box>
        <Button
          size="small"
          variant="elitea"
          color="primary"
          disabled={locked}
          onClick={() => setDialog({ ...CLOSED_DIALOG, open: true })}
          sx={{ textTransform: 'none' }}
          data-testid="admin-microsoft-apps-add"
        >
          {t('pages.admin.microsoftApps.add', 'Register Microsoft app')}
        </Button>
      </Box>

      {showEmpty ? (
        <Typography variant="bodyMedium" color="text.secondary" data-testid="admin-microsoft-apps-empty">
          {t(
            'pages.admin.microsoftApps.empty',
            'No Microsoft app is registered, so credentials offer only Custom app. Register one after setting it up in Entra with the steps above.',
          )}
        </Typography>
      ) : null}

      {apps.length > 0 ? (
        <AdminMicrosoftAppsTable
          apps={apps}
          busyIds={busyIds}
          onEdit={(app) => setDialog({ ...CLOSED_DIALOG, open: true, editing: app })}
          onTest={handleTest}
          onToggle={handleToggle}
        />
      ) : null}

      {testResult === undefined ? null : <MicrosoftAppTestPanel appName={testResult.app.name} result={testResult.result} />}

      <AdminMicrosoftAppDialog
        open={dialog.open}
        editing={dialog.editing}
        typeInfo={typeInfo}
        isSaving={saveMutation.isPending}
        serverError={dialog.serverError}
        fieldError={dialog.fieldError}
        onClose={() => setDialog(CLOSED_DIALOG)}
        onSubmit={handleSubmit}
      />
    </Box>
  );
}
