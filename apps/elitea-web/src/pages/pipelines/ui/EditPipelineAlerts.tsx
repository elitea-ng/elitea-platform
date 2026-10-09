import type { ReactNode } from 'react';

import Typography from '@mui/material/Typography';

import { applicationServerErrorMessage } from '@/features/agents';
import { t } from '@/shared/i18n';

export interface EditPipelineAlertsProps {
  /** The pipeline-detail fetch failed (`useEditPipelineData.isError`). */
  readonly isError: boolean;
  /** The last Save was refused before it was issued, because the live graph is inadmissible. */
  readonly admissionRefused: boolean;
  /** The last Save reached the server and failed (`useEditPipelineForm.saveError`). */
  readonly saveError: unknown;
}

/**
 * The editor's three inline `role="alert"` banners, in one place.
 *
 * This app has no toast infrastructure (see `useEditPipelineForm`'s
 * `saveError` doc comment), so every failure on this page is an inline
 * banner. They live here rather than in `EditPipeline.tsx` for the same §3.5
 * file-length reason every other `./ui/` and `./lib/` split on this page
 * exists: the page sits at the 400-line ceiling, and the admission banner is
 * what pushed it over.
 *
 * The ADMISSION banner is the one that is not merely a failure report. The
 * graph veto is published by `features/pipelines`' `GraphAdmissionGate` as an
 * RHF `root.*` error, which disables the Save button — and `handleSubmit`
 * deletes every `root.*` error before deciding whether to submit
 * (react-hook-form 7.83, `dist/index.esm.mjs:3002`). `handleSave` therefore
 * re-asks the question itself, and when it refuses, nothing else on screen
 * would say so: the gate that would have rendered the reasons is unmounted in
 * precisely the case that makes the refusal reachable (`ConfigurationTab`'s
 * error branch, which a post-save detail refetch failure now triggers).
 */
export function EditPipelineAlerts({ isError, admissionRefused, saveError }: EditPipelineAlertsProps): ReactNode {
  return (
    <>
      {isError && (
        <Typography
          role="alert"
          variant="bodyMedium"
        >
          {t('pages.pipelines.editPipeline.error', 'Failed to load this pipeline.')}
        </Typography>
      )}
      {admissionRefused && (
        <Typography
          role="alert"
          variant="bodyMedium"
        >
          {t(
            'pages.pipelines.editPipeline.admissionRefused',
            'This pipeline was not saved: the runtime would refuse this flow graph.',
          )}
        </Typography>
      )}
      {saveError !== undefined && (
        <Typography
          role="alert"
          variant="bodyMedium"
        >
          {/* The server's own readable refusal (e.g. the pipeline size or node limit), else the generic text. */}
          {applicationServerErrorMessage(saveError, t('pages.pipelines.editPipeline.saveError', 'Failed to save your changes.'))}
        </Typography>
      )}
    </>
  );
}
