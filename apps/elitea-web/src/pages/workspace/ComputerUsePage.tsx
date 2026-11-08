/**
 * Settings › Computer use (desktop build only; ADR-0047 decision 7, #1584):
 * the device switch (off unless turned on), the macOS grants read live
 * (Accessibility is required; Screen Recording only for screenshots, and
 * without it the screenshot tools are not offered), links to the System
 * Settings panes, the stop shortcut, and the remembered "always for this
 * app" answers with revoke.
 */
import { Alert, Box, Button, FormControlLabel, Stack, Switch, Typography } from '@mui/material';
import { useCallback, useEffect, useMemo, useState } from 'react';

import { shortcutLabel, tauriComputerIpc, type ComputerIpc, type ComputerSettings, type GrantState, type PrivacyGrant } from '@/shared/desktop/computerIpc';
import { t } from '@/shared/i18n';

function grantText(grant: GrantState): string {
  switch (grant) {
    case 'granted':
      return t('desktop.computerUse.granted', 'Granted');
    case 'denied':
      return t('desktop.computerUse.denied', 'Not granted');
    case 'not_required':
      return t('desktop.computerUse.notRequired', 'Not needed on this system');
    case 'unknown':
      return t('desktop.computerUse.unknown', 'Not known yet');
  }
}

const needsGrant = (grant: GrantState): boolean => grant !== 'granted' && grant !== 'not_required';

interface GrantRowProps {
  readonly id: PrivacyGrant;
  readonly title: string;
  readonly help: string;
  readonly grant: GrantState;
  readonly onOpen: (grant: PrivacyGrant) => void;
}

function GrantRow({ id, title, help, grant, onOpen }: GrantRowProps): React.JSX.Element {
  return (
    <Box data-testid={`computer-grant-${id.replaceAll('_', '-')}`} data-grant={grant} sx={{ display: 'flex', alignItems: 'center', gap: 2 }}>
      <Box sx={{ flexGrow: 1, minWidth: 0 }}>
        <Typography variant="labelMedium" component="p">
          {title}: {grantText(grant)}
        </Typography>
        <Typography variant="bodySmall" color="text.secondary" component="p">
          {help}
        </Typography>
      </Box>
      {needsGrant(grant) ? (
        <Button size="small" onClick={() => onOpen(id)} data-testid={`computer-open-${id.replaceAll('_', '-')}`}>
          {t('desktop.computerUse.openSettings', 'Open System Settings')}
        </Button>
      ) : null}
    </Box>
  );
}

function Permissions({ settings, onOpen }: { readonly settings: ComputerSettings; readonly onOpen: (grant: PrivacyGrant) => void }): React.JSX.Element {
  return (
    <Stack spacing={2}>
      <Typography component="h2" variant="headingSmall">
        {t('desktop.computerUse.permissions', 'Permissions')}
      </Typography>
      <GrantRow
        id="accessibility"
        title={t('desktop.computerUse.accessibility', 'Accessibility')}
        help={t('desktop.computerUse.accessibilityHelp', 'Required: reading apps’ controls and pressing, typing and scrolling in them.')}
        grant={settings.accessibility}
        onOpen={onOpen}
      />
      <GrantRow
        id="screen_recording"
        title={t('desktop.computerUse.screenRecording', 'Screen Recording')}
        help={t('desktop.computerUse.screenRecordingHelp', 'Only for screenshots. Without it the agent is not offered {{tools}}.', { tools: settings.screenshot_tools.join(', ') })}
        grant={settings.screen_recording}
        onOpen={onOpen}
      />
      <Typography variant="bodySmall" color="text.secondary" component="p">
        {t('desktop.computerUse.inputMonitoringHelp', 'If the pause on your own input or the stop shortcut does not work, macOS may also ask for Input Monitoring.')}{' '}
        <Button size="small" onClick={() => onOpen('input_monitoring')} data-testid="computer-open-input-monitoring">
          {t('desktop.computerUse.openInputMonitoring', 'Open Input Monitoring')}
        </Button>
      </Typography>
    </Stack>
  );
}

function rememberedText(sees: boolean, acts: boolean): string {
  if (sees && acts) return t('desktop.computerUse.seesAndActs', 'Always see and use');
  return acts ? t('desktop.computerUse.acts', 'Always use') : t('desktop.computerUse.sees', 'Always see');
}

interface RememberedProps {
  readonly settings: ComputerSettings;
  readonly busy: boolean;
  readonly onForget: (bundleId: string) => void;
}

function Remembered({ settings, busy, onForget }: RememberedProps): React.JSX.Element {
  return (
    <Stack spacing={1}>
      <Typography component="h2" variant="headingSmall">
        {t('desktop.computerUse.rememberedTitle', 'Always allowed apps')}
      </Typography>
      {settings.remembered.length === 0 ? (
        <Typography color="text.secondary" data-testid="computer-remembered-empty">
          {t('desktop.computerUse.rememberedEmpty', 'No app is always allowed. The agent asks each time.')}
        </Typography>
      ) : (
        <Stack component="ul" spacing={1} sx={{ listStyle: 'none', p: 0, m: 0 }}>
          {settings.remembered.map((app) => (
            <Box component="li" key={app.bundle_id} data-testid="computer-remembered-app" data-bundle-id={app.bundle_id} sx={{ display: 'flex', alignItems: 'center', gap: 2 }}>
              <Box sx={{ flexGrow: 1, minWidth: 0 }}>
                <Typography variant="labelMedium" component="p" noWrap>
                  {app.bundle_id}
                </Typography>
                <Typography variant="bodySmall" color="text.secondary" component="p">
                  {rememberedText(app.sees, app.acts)}
                </Typography>
              </Box>
              <Button size="small" color="error" disabled={busy} onClick={() => onForget(app.bundle_id)} aria-label={t('desktop.computerUse.revokeApp', 'Revoke {{app}}', { app: app.bundle_id })}>
                {t('desktop.computerUse.revoke', 'Revoke')}
              </Button>
            </Box>
          ))}
        </Stack>
      )}
    </Stack>
  );
}

interface DeviceSwitchProps {
  readonly settings: ComputerSettings;
  readonly busy: boolean;
  readonly onChange: (enabled: boolean) => void;
}

function DeviceSwitch({ settings, busy, onChange }: DeviceSwitchProps): React.JSX.Element {
  const allowed = settings.supported && settings.policy_allows;
  const missing = needsGrant(settings.accessibility);
  const label = t('desktop.computerUse.switch', 'Let the agent use this computer');
  return (
    <Box>
      {allowed || !settings.supported ? null : (
        <Alert severity="info" data-testid="computer-policy-off" sx={{ mb: 2 }}>
          {t('desktop.computerUse.policyOff', 'Your organisation does not allow computer use. An administrator turns it on in Admin › Configuration › Native client policy.')}
        </Alert>
      )}
      <FormControlLabel
        control={
          <Switch
            checked={settings.enabled}
            disabled={busy || (!settings.enabled && (!allowed || missing))}
            onChange={(event) => onChange(event.target.checked)}
            slotProps={{ input: { 'aria-label': label } }}
            data-testid="computer-enabled"
          />
        }
        label={label}
      />
      {allowed && missing && !settings.enabled ? (
        <Typography variant="bodySmall" color="text.secondary" component="p" data-testid="computer-needs-accessibility">
          {t('desktop.computerUse.needsAccessibility', 'Grant Accessibility first.')}
        </Typography>
      ) : null}
    </Box>
  );
}

const messageOf = (caught: unknown): string => (caught instanceof Error ? caught.message : String(caught));

function ComputerUseSettings({ ipc }: { readonly ipc: ComputerIpc }): React.JSX.Element {
  const [settings, setSettings] = useState<ComputerSettings | null>(null);
  const [error, setError] = useState<string | null>(null);
  const [busy, setBusy] = useState(false);

  const run = useCallback((work: () => Promise<ComputerSettings>) => {
    setBusy(true);
    setError(null);
    work()
      .then(setSettings)
      .catch((caught: unknown) => setError(messageOf(caught)))
      .finally(() => setBusy(false));
  }, []);

  useEffect(() => {
    run(() => ipc.settings());
    // The grants change in System Settings: read them again when the person comes back.
    const refresh = (): void => {
      if (document.visibilityState === 'visible') void ipc.settings().then(setSettings, () => undefined);
    };
    window.addEventListener('focus', refresh);
    document.addEventListener('visibilitychange', refresh);
    return () => {
      window.removeEventListener('focus', refresh);
      document.removeEventListener('visibilitychange', refresh);
    };
  }, [ipc, run]);

  const openPrivacy = (grant: PrivacyGrant): void => {
    ipc.openPrivacy(grant).catch((caught: unknown) => setError(messageOf(caught)));
  };

  if (settings === null) {
    return error === null ? <Typography>{t('desktop.computerUse.loading', 'Loading…')}</Typography> : <Alert severity="error">{error}</Alert>;
  }
  return (
    <Stack spacing={3}>
      <Typography color="text.secondary">
        {t(
          'desktop.computerUse.help',
          'The agent can see and use the apps on this computer, asking you first for each app. While it works a panel stays on top with Pause and Stop; using the keyboard or the pointer pauses it until you press Resume.',
        )}
      </Typography>
      {settings.supported ? null : <Alert severity="info">{t('desktop.computerUse.unsupported', 'Computer use runs on macOS, Windows and Linux only.')}</Alert>}
      {settings.supported && settings.read_only_reason !== null ? (
        <Alert severity="warning" data-testid="computer-read-only" data-reason={settings.read_only_reason}>
          {t(
            'desktop.computerUse.readOnly',
            'On this session the agent can see your apps but not use them: Elitea cannot watch your keyboard and pointer here, so it could not pause when you use the computer. Sign in to an X11 session to let it act.',
          )}
        </Alert>
      ) : null}
      <DeviceSwitch settings={settings} busy={busy} onChange={(enabled) => run(() => ipc.setEnabled(enabled))} />
      {error === null ? null : (
        <Alert severity="error" data-testid="computer-error">
          {error}
        </Alert>
      )}
      {settings.supported ? <Permissions settings={settings} onOpen={openPrivacy} /> : null}
      <Stack spacing={1}>
        <Typography component="h2" variant="headingSmall">
          {t('desktop.computerUse.stopTitle', 'Stop shortcut')}
        </Typography>
        <Typography data-testid="computer-stop-shortcut">
          {t('desktop.computerUse.stopHelp', 'Press {{shortcut}} anywhere to stop the agent using the computer.', { shortcut: shortcutLabel(settings.stop_shortcut) })}
        </Typography>
      </Stack>
      <Remembered settings={settings} busy={busy} onForget={(bundleId) => run(() => ipc.forgetApp(bundleId))} />
    </Stack>
  );
}

export default function ComputerUsePage({ ipc }: { readonly ipc?: ComputerIpc }): React.JSX.Element {
  const computer = useMemo(() => ipc ?? tauriComputerIpc(), [ipc]);
  return (
    <Stack spacing={2} sx={{ p: 3, maxWidth: 760 }}>
      <Typography component="h1" variant="headingMedium">
        {t('desktop.computerUse.pageTitle', 'Computer use')}
      </Typography>
      {computer === undefined ? <Typography>{t('desktop.computerUse.noHost', 'Computer use runs in the desktop app.')}</Typography> : <ComputerUseSettings ipc={computer} />}
    </Stack>
  );
}
