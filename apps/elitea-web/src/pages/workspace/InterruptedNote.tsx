/**
 * Under a recorded turn that never ended (the app closed while it ran): a note, and Continue when the host kept the
 * turn's resume record (`StoredTurn.resumable`, ADR-0045 decision 7).
 */
import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

/** Continue a recorded turn that ran when the app stopped; `busy`: a turn runs now, so Continue waits. */
export interface ResumeAction {
  onResume: (turnId: string) => void;
  busy: boolean;
}

export function InterruptedNote({ turnId, resumable, resume }: { turnId: string; resumable: boolean; resume: ResumeAction | undefined }): React.JSX.Element {
  return (
    <>
      <Typography variant="bodySmall" sx={(theme: Theme) => ({ color: theme.vars.palette.text.metrics })}>
        {t('workspace.history.interrupted', 'This turn did not finish: the app closed while it ran.')}
      </Typography>
      {resumable && resume !== undefined && (
        <Box>
          <Button size="small" variant="outlined" data-testid="history-turn-continue" disabled={resume.busy} onClick={() => resume.onResume(turnId)}>
            {t('workspace.history.continue', 'Continue')}
          </Button>
        </Box>
      )}
    </>
  );
}
