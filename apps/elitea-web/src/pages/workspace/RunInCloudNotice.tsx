/**
 * What the composer says about a pipeline (#1239 A3): pipelines run in the
 * cloud, never on this computer. Either the pick is a pipeline (`cloudOnly`:
 * the send itself runs it in the cloud), or the host refused a start with
 * `pipeline_unsupported` (`refusedPrompt`): the notice offers "Run in cloud"
 * for that prompt instead of the bare refusal.
 */
import Alert from '@mui/material/Alert';
import Button from '@mui/material/Button';

import { t } from '@/shared/i18n';

export interface RunInCloudNoticeProps {
  cloudOnly: boolean;
  /** The prompt the host refused as a pipeline; `null` when nothing was refused so. */
  refusedPrompt: string | null;
  running: boolean;
  /** Why the last "Run in cloud" did not start. */
  error: string | null;
  onRun: (prompt: string) => void;
}

export function RunInCloudNotice({ cloudOnly, refusedPrompt, running, error, onRun }: RunInCloudNoticeProps): React.JSX.Element | null {
  return (
    <>
      {error !== null && <Alert severity="error">{error}</Alert>}
      {refusedPrompt !== null ? (
        <Alert
          severity="info"
          data-testid="workspace-run-in-cloud-offer"
          action={
            <Button color="inherit" size="small" disabled={running} onClick={() => onRun(refusedPrompt)}>
              {t('workspace.runInCloud.action', 'Run in cloud')}
            </Button>
          }
        >
          {t('workspace.error.pipelineUnsupported', 'Pipelines do not run on this computer yet. Run this one in the cloud: it opens as a chat.')}
        </Alert>
      ) : (
        cloudOnly && (
          <Alert severity="info" data-testid="workspace-run-in-cloud-hint">
            {t('workspace.runInCloud.hint', 'This is a pipeline: it runs in the cloud, not on this computer. Sending starts it in a new chat and opens it.')}
          </Alert>
        )
      )}
    </>
  );
}
