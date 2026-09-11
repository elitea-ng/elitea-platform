/**
 * What one synchronous tool run came back with.
 *
 * The legacy panel this ports (`qa/elitea-testing-public`'s Test-Settings case
 * reads it as `test_tool_result_content`) showed the tool's RAW result, and
 * that is what a person testing a credential needs: the branch names, the
 * project keys, the page titles the provider actually returned. So the ok
 * branch renders the payload verbatim rather than a "Success" chip.
 *
 * ONE BRANCH PER OUTCOME, and no default. `TestToolkitToolOutcome`
 * (`../../api/toolkitTestRun.ts`) is a closed union of the five things
 * `internal/api/v2/toolkitrun/response.go` can write plus the transport
 * catch-all; a `switch` that fell through to a generic message would turn "this
 * toolkit type cannot run tools on this deployment" into "something went
 * wrong", which is the difference between a fixable configuration and a
 * mystery.
 *
 * The timeout branch keeps the JOB ID. A bounded wait that expired is not a
 * failed run — the run is still going, server-side — and the id is the only
 * handle anyone has on it afterwards.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import CircularProgress from '@mui/material/CircularProgress';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { TestToolkitToolOutcome } from '../../api/toolkitTestRun';

export interface TestToolResultPanelProps {
  readonly outcome: TestToolkitToolOutcome | undefined;
  readonly isRunning: boolean;
}

/**
 * The result payload as text. An object is pretty-printed; a string is shown as
 * it came.
 *
 * Not exported: its only caller is `OutcomeBody` below, and an export with no
 * importer fails the `knip --max-issues 0` gate. Its behaviour is measured
 * through the rendered payload instead.
 */
function formatToolResult(result: unknown): string {
  if (typeof result === 'string') return result;
  if (result === undefined) return '';
  try {
    const text = JSON.stringify(result, null, 2);
    // `JSON.stringify` answers `undefined` (not a string) for a function or a
    // bare `undefined` inside a container. Falling through to a cast would put
    // the word "undefined" on screen as if the tool had returned it.
    return text ?? '';
  } catch {
    // A cyclic payload is still a real answer, so it is described rather than
    // swallowed. `JSON.stringify` is what threw, so this is the one place a
    // non-JSON value has to be rendered some other way.
    return Object.prototype.toString.call(result);
  }
}

/** The one `status` string each outcome carries, for the DOM and for a journey to read. */
function statusOf(outcome: TestToolkitToolOutcome): string {
  return outcome.kind;
}

function OutcomeBody({ outcome }: { readonly outcome: TestToolkitToolOutcome }): ReactNode {
  switch (outcome.kind) {
    case 'ok':
      return (
        <>
          <Typography variant="bodyMedium">{t('features.toolkits.testToolPane.resultOk', 'The tool ran and returned:')}</Typography>
          <Box
            component="pre"
            data-testid="test-tool-result-payload"
            sx={payloadSx}
          >
            {formatToolResult(outcome.result)}
          </Box>
          {outcome.truncated && (
            <Typography
              variant="bodySmall"
              data-testid="test-tool-result-truncated"
            >
              {t('features.toolkits.testToolPane.resultTruncated', 'The result was too large to show in full.')}
            </Typography>
          )}
        </>
      );
    case 'authorizationRequired':
      return <Typography variant="bodyMedium">{t('features.toolkits.testToolPane.authorizationRequired', 'Authorize this toolkit to run the selected tool.')}</Typography>;
    case 'skipped':
      return <Typography variant="bodyMedium">{t('features.toolkits.testToolPane.skipped', 'Tool run skipped.')}</Typography>;
    case 'toolError':
      return <Typography variant="bodyMedium">{outcome.message}</Typography>;
    case 'unsupportedToolkit':
    case 'unknownTool':
    case 'failure':
      return <Typography variant="bodyMedium">{outcome.message}</Typography>;
    case 'timeout':
      return (
        <>
          <Typography variant="bodyMedium">{outcome.message}</Typography>
          {outcome.taskId !== undefined && (
            <Typography
              variant="bodySmall"
              data-testid="test-tool-result-task-id"
            >
              {t('features.toolkits.testToolPane.resultJobId', 'Job id:')} {outcome.taskId}
            </Typography>
          )}
        </>
      );
  }
}

export function TestToolResultPanel({ outcome, isRunning }: TestToolResultPanelProps): ReactNode {
  if (isRunning) {
    return (
      <Box
        sx={containerSx}
        data-testid="test-tool-result"
        data-status="running"
      >
        <CircularProgress size={16} />
        <Typography variant="bodyMedium">{t('features.toolkits.testToolPane.running', 'Running the tool…')}</Typography>
      </Box>
    );
  }
  if (outcome === undefined) return null;

  return (
    <Box
      sx={containerSx}
      data-testid="test-tool-result"
      data-status={statusOf(outcome)}
      // `output`, not `role="status"`: the same announcement, from the element
      // the platform already gives that meaning to.
      component="output"
    >
      <OutcomeBody outcome={outcome} />
    </Box>
  );
}

const containerSx: SxProps<Theme> = (theme) => ({
  display: 'flex',
  flexDirection: 'column',
  gap: '0.5rem',
  marginTop: '1rem',
  padding: '0.75rem',
  borderRadius: theme.vars.shape.radiusSm,
  border: `.0625rem solid ${theme.vars.palette.divider}`,
  minWidth: 0,
});

const payloadSx: SxProps<Theme> = (theme) => ({
  margin: 0,
  maxHeight: '18rem',
  overflow: 'auto',
  whiteSpace: 'pre-wrap',
  wordBreak: 'break-word',
  fontFamily: 'monospace',
  ...theme.typography.bodySmall,
  color: theme.vars.palette.text.secondary,
});
