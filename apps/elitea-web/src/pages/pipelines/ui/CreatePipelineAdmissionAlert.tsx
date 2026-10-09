import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import AlertTitle from '@mui/material/AlertTitle';
import Box from '@mui/material/Box';
import type { SxProps, Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import type { useLivePipelineGraphAdmission } from '@/features/pipelines';
import { t } from '@/shared/i18n';

type PipelineGraphAdmission = ReturnType<typeof useLivePipelineGraphAdmission>;

const alertSx: SxProps<Theme> = { width: '100%', boxSizing: 'border-box', marginBottom: '1rem' };
const listSx: SxProps<Theme> = { margin: '0.25rem 0 0', paddingLeft: '1.25rem', display: 'flex', flexDirection: 'column', gap: '0.25rem' };

export interface CreatePipelineAdmissionAlertProps {
  readonly admission: PipelineGraphAdmission;
}

/**
 * The create page's half of the editor's save gate
 * (`features/pipelines`' `GraphAdmissionGate`): the same verdict, the same
 * inline outlined alert, the same title and body. It lists every reason with
 * the node it belongs to, because this page has no canvas whose node panels
 * could show them.
 */
export function CreatePipelineAdmissionAlert({ admission }: CreatePipelineAdmissionAlertProps): ReactNode {
  if (admission.isAdmissible) return null;
  // A notice (a declared state `reducer`) never blocks a save; only refusals are listed here.
  const refusals = admission.issues.filter((issue) => issue.severity !== 'warning');
  return (
    <Alert
      severity="error"
      variant="outlined"
      sx={alertSx}
      data-testid="create-pipeline-admission"
    >
      <AlertTitle>{t('pages.pipelines.createPipeline.admissionTitle', 'This pipeline cannot be saved')}</AlertTitle>
      <Typography variant="bodySmall">
        {t('pages.pipelines.createPipeline.admissionBody', 'The runtime would refuse this graph, so saving is blocked until it is fixed.')}
      </Typography>
      <Box
        component="ul"
        sx={listSx}
      >
        {admission.parseFailed && (
          <Typography
            component="li"
            variant="bodySmall"
          >
            {t('pages.pipelines.createPipeline.admissionUnparseable', 'The YAML does not parse, so the runtime cannot read a graph out of it.')}
          </Typography>
        )}
        {refusals.map((issue) => (
          <Typography
            key={`${issue.rule}|${issue.nodeId ?? ''}|${issue.field}|${issue.subject}`}
            component="li"
            variant="bodySmall"
            title={issue.citation}
          >
            {issue.nodeId === undefined ? issue.message : `${t('pages.pipelines.createPipeline.admissionNode', 'Node')} ${issue.nodeId}: ${issue.message}`}
          </Typography>
        ))}
      </Box>
    </Alert>
  );
}
