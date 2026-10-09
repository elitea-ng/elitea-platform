import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

const alertSx: SxProps<Theme> = { marginBottom: '0.5rem' };

export interface PipelineYamlSerializationAlertProps {
  readonly error: string | undefined;
}

/** Inline, non-blocking: the stored YAML stays in place and the reason is shown above the editor. */
export function PipelineYamlSerializationAlert({ error }: PipelineYamlSerializationAlertProps): ReactNode {
  if (error === undefined) return null;
  return (
    <Alert
      severity="error"
      sx={alertSx}
    >
      {t(
        'features.pipelines.editorPanel.serializationError',
        'This change could not be written as pipeline YAML, so the stored YAML was kept: {{reason}}',
        { reason: error },
      )}
    </Alert>
  );
}
