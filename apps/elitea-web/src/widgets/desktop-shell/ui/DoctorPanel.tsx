/**
 * The Doctor's checks (README, "Diagnostics"), each with its status, what
 * the host found and, when the host can repair it, a Fix button. Used by the
 * Help › Run Diagnostics… dialog (any screen, signed in or not) and by
 * Settings › Troubleshoot. A repair runs only on the click; the checks are
 * run again after it. A repair that deletes what the app keeps (the host
 * says what in `fix_confirm`) asks first, and runs only once confirmed.
 */
import CheckCircleOutlined from '@mui/icons-material/CheckCircleOutlined';
import ErrorOutlined from '@mui/icons-material/ErrorOutlined';
import WarningAmberOutlined from '@mui/icons-material/WarningAmberOutlined';
import { Alert, Box, Button, CircularProgress, Dialog, DialogActions, DialogContent, DialogContentText, DialogTitle, Stack, Typography } from '@mui/material';
import { useCallback, useEffect, useState } from 'react';

import type { DoctorCheck, DoctorIpc, DoctorStatus } from '@/shared/desktop/doctorIpc';
import { toWorkspaceIpcError } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

function StatusIcon({ status }: { status: DoctorStatus }): React.JSX.Element {
  if (status === 'ok') return <CheckCircleOutlined color="success" fontSize="small" titleAccess={t('desktop.doctor.status.ok', 'OK')} />;
  if (status === 'warn') return <WarningAmberOutlined color="warning" fontSize="small" titleAccess={t('desktop.doctor.status.warn', 'Warning')} />;
  return <ErrorOutlined color="error" fontSize="small" titleAccess={t('desktop.doctor.status.fail', 'Problem')} />;
}

export interface DoctorPanelProps {
  ipc: DoctorIpc;
}

export function DoctorPanel({ ipc }: DoctorPanelProps): React.JSX.Element {
  const [checks, setChecks] = useState<DoctorCheck[] | null>(null);
  const [busy, setBusy] = useState<string | null>(null);
  const [outcome, setOutcome] = useState<{ severity: 'success' | 'error'; text: string } | null>(null);
  /** The repair waiting for the person's confirmation. */
  const [confirming, setConfirming] = useState<DoctorCheck | null>(null);

  const runChecks = useCallback(async () => {
    setChecks(null);
    setChecks(await ipc.run().catch(() => []));
  }, [ipc]);

  useEffect(() => {
    void runChecks();
  }, [runChecks]);

  const repair = async (fixId: string, confirmed = false): Promise<void> => {
    setBusy(fixId);
    setOutcome(null);
    try {
      setOutcome({ severity: 'success', text: await ipc.fix(fixId, confirmed) });
    } catch (error) {
      setOutcome({ severity: 'error', text: toWorkspaceIpcError(error).message });
    } finally {
      setBusy(null);
    }
    await runChecks();
  };

  return (
    <Stack spacing={2}>
      <Stack direction="row" spacing={2} sx={{ alignItems: 'center', justifyContent: 'space-between' }}>
        <Typography color="text.secondary">
          {t('desktop.doctor.intro', 'What Elitea keeps on this computer and needs from your deployment. A repair runs only when you click Fix.')}
        </Typography>
        <Button onClick={() => void runChecks()} disabled={checks === null || busy !== null}>
          {t('desktop.doctor.runAgain', 'Run again')}
        </Button>
      </Stack>
      {outcome !== null && <Alert severity={outcome.severity}>{outcome.text}</Alert>}
      {checks === null ? (
        <CircularProgress size={24} aria-label={t('desktop.doctor.running', 'Running diagnostics')} />
      ) : (
        <Stack component="ul" spacing={1.5} sx={{ listStyle: 'none', p: 0, m: 0 }} aria-label={t('desktop.doctor.checks', 'Diagnostics')}>
          {checks.map((check) => (
            <Box component="li" key={check.id} data-testid={`doctor-check-${check.id}`} data-status={check.status} sx={{ display: 'flex', gap: 1.5, alignItems: 'flex-start' }}>
              <Box sx={{ pt: 0.25 }}>
                <StatusIcon status={check.status} />
              </Box>
              <Box sx={{ flex: 1, minWidth: 0 }}>
                <Typography sx={{ fontWeight: 600 }}>{check.title}</Typography>
                <Typography variant="bodySmall" color="text.secondary" sx={{ overflowWrap: 'anywhere' }}>
                  {check.message}
                </Typography>
              </Box>
              {check.fix_id !== undefined && (
                <Button
                  size="small"
                  variant="outlined"
                  disabled={busy !== null}
                  onClick={() => (check.fix_confirm === undefined ? void repair(check.fix_id ?? '') : setConfirming(check))}
                  aria-label={`${t('desktop.doctor.fix', 'Fix')}: ${check.title}`}
                >
                  {busy === check.fix_id ? <CircularProgress size={16} /> : (check.fix_label ?? t('desktop.doctor.fix', 'Fix'))}
                </Button>
              )}
            </Box>
          ))}
        </Stack>
      )}
      <Dialog open={confirming !== null} onClose={() => setConfirming(null)} aria-labelledby="doctor-confirm-title">
        <DialogTitle id="doctor-confirm-title">{confirming?.fix_label ?? t('desktop.doctor.fix', 'Fix')}</DialogTitle>
        <DialogContent>
          <DialogContentText>{confirming?.fix_confirm}</DialogContentText>
        </DialogContent>
        <DialogActions>
          <Button onClick={() => setConfirming(null)}>{t('desktop.doctor.confirmCancel', 'Cancel')}</Button>
          <Button
            color="warning"
            variant="contained"
            onClick={() => {
              const fixId = confirming?.fix_id;
              setConfirming(null);
              if (fixId !== undefined) void repair(fixId, true);
            }}
          >
            {confirming?.fix_label ?? t('desktop.doctor.fix', 'Fix')}
          </Button>
        </DialogActions>
      </Dialog>
    </Stack>
  );
}
