import { Alert, Box, Button, CircularProgress, Dialog, DialogActions, DialogContent, DialogTitle, Stack, TextField, Typography } from '@mui/material';
import { useEffect, useRef, useState, type ReactNode } from 'react';

import type { AppIpc } from '@/shared/desktop/appEvents';
import { needsAttention, type DoctorIpc } from '@/shared/desktop/doctorIpc';
import type { HostBridge, HostDeployment, HostState } from '@/shared/desktop/hostBridge';
import { t } from '@/shared/i18n';
import { DoctorPanel } from '@/widgets/desktop-shell';

import { launchApp } from './launchApp';

const HEADING = { fontWeight: 600 } as const;

type Phase =
  | { kind: 'loading' }
  | { kind: 'no-host' }
  | { kind: 'connect'; error?: string }
  | { kind: 'confirm'; deployment: HostDeployment; error?: string }
  | { kind: 'signing-in'; deployment: HostDeployment }
  | { kind: 'app'; state: HostState };

function describe(error: unknown): string {
  if (typeof error === 'string') return error;
  return error instanceof Error ? error.message : 'Something went wrong.';
}

/** Mounts the real app into its own container; the shell's screens stay out of its tree. */
function AppHost({ bridge, state }: { bridge: HostBridge; state: HostState }) {
  const ref = useRef<HTMLDivElement>(null);
  const [problem, setProblem] = useState<string | undefined>();
  useEffect(() => {
    const container = ref.current;
    if (container === null) return undefined;
    // Signed out (refresh failed, or the device was revoked): the host has
    // already dropped the session. A reload is the thorough reset — it clears
    // every in-memory store and lands on the connect screen.
    const reboot = (): void => window.location.reload();
    const launched = launchApp({
      bridge,
      state,
      container,
      onSignedOut: reboot,
      onUpgradeRequired: () => setProblem('This version of Elitea is older than your deployment allows. Update the app to continue.'),
    });
    launched.catch((error: unknown) => setProblem(describe(error)));
    return () => {
      void launched.then((root) => root.unmount()).catch(() => undefined);
    };
  }, [bridge, state]);
  return (
    <>
      {problem !== undefined && <Alert severity="warning">{problem}</Alert>}
      <div ref={ref} />
    </>
  );
}

export interface DesktopShellProps {
  bridge: HostBridge | undefined;
  /** The host's Doctor; without it there is no diagnostics notice or dialog. */
  doctor?: DoctorIpc | undefined;
  /** `app://command`, for Help › Run Diagnostics… (listened to live; `ready` is the app's). */
  appIpc?: AppIpc | undefined;
}

/**
 * The launch notice and the Help › Run Diagnostics… dialog, over every
 * screen: signed in or not (a sign-in the stored file blocks is exactly
 * when it is needed).
 */
function useDiagnostics(doctor: DoctorIpc | undefined, appIpc: AppIpc | undefined) {
  const [open, setOpen] = useState(false);
  const [attention, setAttention] = useState(false);
  useEffect(() => {
    if (doctor === undefined) return;
    // This computer's files only: no network before the person chose a deployment.
    doctor
      .run('local')
      .then((checks) => setAttention(needsAttention(checks)))
      .catch(() => undefined);
  }, [doctor]);
  useEffect(() => {
    if (appIpc === undefined || doctor === undefined) return undefined;
    let off: (() => void) | undefined;
    let disposed = false;
    appIpc
      .onCommand((command) => {
        if (command.id === 'run_diagnostics') setOpen(true);
      })
      .then((unsubscribe) => {
        if (disposed) unsubscribe();
        else off = unsubscribe;
      })
      .catch(() => undefined);
    return () => {
      disposed = true;
      off?.();
    };
  }, [appIpc, doctor]);
  const show = (): void => {
    setAttention(false);
    setOpen(true);
  };
  const element =
    doctor === undefined ? null : (
      <>
        {attention && (
          <Alert
            severity="warning"
            sx={{ position: 'fixed', right: 16, bottom: 16, zIndex: (theme) => theme.zIndex.snackbar, maxWidth: 420 }}
            action={<Button onClick={show}>{t('desktop.doctor.run', 'Run Diagnostics')}</Button>}
            onClose={() => setAttention(false)}
          >
            {t('desktop.doctor.attention', 'Something needs attention.')}
          </Alert>
        )}
        <Dialog open={open} onClose={() => setOpen(false)} fullWidth maxWidth="sm" aria-labelledby="doctor-title">
          <DialogTitle id="doctor-title">{t('desktop.doctor.title', 'Diagnostics')}</DialogTitle>
          <DialogContent>{open && <DoctorPanel ipc={doctor} />}</DialogContent>
          <DialogActions>
            <Button onClick={() => setOpen(false)}>{t('desktop.doctor.close', 'Close')}</Button>
          </DialogActions>
        </Dialog>
      </>
    );
  return { element, show: doctor === undefined ? undefined : show };
}

export function DesktopShell({ bridge, doctor, appIpc }: DesktopShellProps) {
  const diagnostics = useDiagnostics(doctor, appIpc);
  const shell = <ShellScreens bridge={bridge} onDiagnose={diagnostics.show} />;
  return (
    <>
      {shell}
      {diagnostics.element}
    </>
  );
}

/** An error, with the way to the Doctor (a stored file can be what blocks sign-in). */
function ErrorAlert({ text, onDiagnose }: { text: string; onDiagnose: (() => void) | undefined }) {
  return (
    <Alert
      severity="error"
      action={onDiagnose === undefined ? undefined : <Button color="inherit" size="small" onClick={onDiagnose}>{t('desktop.doctor.run', 'Run Diagnostics')}</Button>}
    >
      {text}
    </Alert>
  );
}

function ShellScreens({ bridge, onDiagnose }: { bridge: HostBridge | undefined; onDiagnose: (() => void) | undefined }) {
  const [phase, setPhase] = useState<Phase>(bridge === undefined ? { kind: 'no-host' } : { kind: 'loading' });
  const [url, setUrl] = useState('');
  /** The attempt the person cancelled: its rejection is not an error to show. */
  const cancelled = useRef(false);

  useEffect(() => {
    if (bridge === undefined) return;
    bridge
      .state()
      .then((state) => {
        if (state.signedIn && state.origin !== null) setPhase({ kind: 'app', state });
        else {
          if (state.origin !== null) setUrl(state.origin);
          setPhase({ kind: 'connect' });
        }
      })
      .catch((error: unknown) => setPhase({ kind: 'connect', error: describe(error) }));
  }, [bridge]);

  if (bridge === undefined || phase.kind === 'no-host') {
    return <Centered><Alert severity="info">Open Elitea from the desktop app. This page needs the app's host.</Alert></Centered>;
  }
  if (phase.kind === 'loading') {
    return <Centered><CircularProgress aria-label="Starting" /></Centered>;
  }
  if (phase.kind === 'app') return <AppHost bridge={bridge} state={phase.state} />;

  const connect = (): void => {
    bridge.connect(url.trim()).then(
      (deployment) => setPhase({ kind: 'confirm', deployment }),
      (error: unknown) => setPhase({ kind: 'connect', error: describe(error) }),
    );
  };

  const signIn = (deployment: HostDeployment): void => {
    cancelled.current = false;
    setPhase({ kind: 'signing-in', deployment });
    bridge.signIn().then(
      (state) => setPhase({ kind: 'app', state }),
      (error: unknown) =>
        setPhase(cancelled.current ? { kind: 'confirm', deployment } : { kind: 'confirm', deployment, error: describe(error) }),
    );
  };

  const cancelSignIn = (): void => {
    cancelled.current = true;
    void bridge.cancelSignIn().catch(() => undefined);
  };

  if (phase.kind === 'connect') {
    return (
      <Centered>
        <Box
          component="form"
          onSubmit={(event) => {
            event.preventDefault();
            connect();
          }}
          sx={{ width: '100%' }}
        >
          <Stack spacing={2}>
            <Typography component="h1" sx={HEADING}>Connect to your Elitea</Typography>
            <TextField
              label="Deployment address"
              placeholder="https://elitea.example.com"
              value={url}
              onChange={(event) => setUrl(event.target.value)}
              slotProps={{ htmlInput: { inputMode: 'url', autoCapitalize: 'none', spellCheck: false } }}
              required
            />
            {phase.error !== undefined && <ErrorAlert text={phase.error} onDiagnose={onDiagnose} />}
            <Button type="submit" variant="contained">Continue</Button>
          </Stack>
        </Box>
      </Centered>
    );
  }

  const { deployment } = phase;
  return (
    <Centered>
      <Stack spacing={2} sx={{ width: '100%' }}>
        <Typography component="h1" sx={HEADING}>{deployment.displayName}</Typography>
        <Typography color="text.secondary">{deployment.origin}</Typography>
        {phase.kind === 'confirm' && phase.error !== undefined && <ErrorAlert text={phase.error} onDiagnose={onDiagnose} />}
        {phase.kind === 'signing-in' ? (
          <Stack direction="row" spacing={2} sx={{ alignItems: 'center' }}>
            <CircularProgress size={20} />
            <Typography>Finish signing in in your browser.</Typography>
            <Button onClick={cancelSignIn}>{t('desktop.signIn.cancel', 'Cancel')}</Button>
          </Stack>
        ) : (
          <>
            <Button variant="contained" onClick={() => signIn(deployment)}>Sign in with your browser</Button>
            <Button onClick={() => setPhase({ kind: 'connect' })}>Use a different address</Button>
          </>
        )}
      </Stack>
    </Centered>
  );
}

function Centered({ children }: { children: ReactNode }) {
  return (
    <Box sx={{ minHeight: '100vh', display: 'flex', alignItems: 'center', justifyContent: 'center', p: 3 }}>
      <Box sx={{ width: '100%', maxWidth: 420 }}>{children}</Box>
    </Box>
  );
}
