/**
 * What the ingestion the user just started is doing.
 *
 * IT IS IN THE WIDGET AND NOT THE SOURCES FEATURE because it reports on a run
 * the WIDGET owns: the same run invalidates the graph tab's reads, so the
 * controller that starts it has to sit where both tabs can see it.
 *
 * THE STEPS ARE THE EVIDENCE. An ingestion is minutes long, and a spinner for
 * minutes is indistinguishable from a request that was never sent. The
 * provider streams six lines — "Reading the source files", "Extracting
 * entities", … — and each one is proof the run reached that stage.
 *
 * THE ARTIFACTS ARE SHOWN WHEN IT FINISHES, and the checkpoint among them is
 * the reason: `.ingestion-checkpoint-<source>.json` is the record that the run
 * got far enough to be resumable, and it is named in the terminal body and
 * nowhere else a screen can reach.
 */
import Box from '@mui/material/Box';
import LinearProgress from '@mui/material/LinearProgress';
import Stack from '@mui/material/Stack';
import Typography from '@mui/material/Typography';

import type { IngestionRun } from '@/features/inventory-sources';
import { t } from '@/shared/i18n';
import { BannerMessage } from '@/shared/ui/BannerMessage';

const bannerSx = { display: 'flex', flexDirection: 'column', gap: 1, marginBottom: 2 } as const;

export interface IngestionBannerProps {
  readonly run: IngestionRun;
}

export function IngestionBanner({ run }: IngestionBannerProps): React.JSX.Element | null {
  const running = run.runningSourceId !== null;
  if (!running && run.error === null && run.summary === null) return null;

  return (
    <Box sx={bannerSx} data-testid="inventory-ingestion-banner">
      {running ? (
        <>
          <Typography variant="labelSmall">
            {t('inventory.ingestion.running', 'Ingestion in progress')}
          </Typography>
          <LinearProgress />
        </>
      ) : null}

      {run.steps.length === 0 ? null : (
        <Stack spacing={0.25} data-testid="inventory-ingestion-steps">
          {run.steps.map((step, index) => (
            <Typography key={`${index}:${step}`} variant="bodySmall" color="text.secondary">
              {step}
            </Typography>
          ))}
        </Stack>
      )}

      {run.summary === null ? null : (
        <Typography variant="bodyMedium" data-testid="inventory-ingestion-summary">
          {run.summary}
        </Typography>
      )}

      {run.artifacts.length === 0 ? null : (
        <Stack spacing={0.25} data-testid="inventory-ingestion-artifacts">
          <Typography variant="labelSmall" color="text.secondary">
            {t('inventory.ingestion.wrote', 'Written to the bucket')}
          </Typography>
          {run.artifacts.map((artifact) => (
            <Typography
              key={artifact.name}
              variant="bodySmall"
              color="text.secondary"
              data-testid="inventory-ingestion-artifact"
              data-object-type={artifact.objectType}
            >
              {artifact.name}
            </Typography>
          ))}
        </Stack>
      )}

      {run.error === null ? null : <BannerMessage variant="error" message={run.error} />}
    </Box>
  );
}
