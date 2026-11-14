/**
 * The last step of the Enhance with AI wizard: what will be saved, and, once
 * saved, what happened to each fix.
 *
 * A DRAFT VERSION IS NOT A RELEASE. Accepting the instruction patch creates a
 * new draft version through the agent's normal version route. Publishing it is
 * a separate act on the agent's release path: this step offers the change
 * request dialog (ADR-0040) for it and never publishes.
 */
import type { ReactNode } from 'react';

import Alert from '@mui/material/Alert';
import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { BaseBtn } from '@/shared/ui/BaseBtn';

import type { AppliedFix } from '../api/evaluationInsightsApi';
import { evaluationErrorSentence } from '../lib/evaluationError';

export interface EnhanceReviewProps {
  readonly counts: { readonly instructions: number; readonly dimensions: number; readonly cases: number };
  readonly applied: readonly AppliedFix[] | undefined;
  readonly applying: boolean;
  /** A failure of the apply call itself, not of one fix. */
  readonly error: unknown;
  /** Absent when the page supplied no change-request dialog: no publish step is offered. */
  readonly onRequestPublish: ((versionId: number) => void) | undefined;
}

function kindLabel(fix: AppliedFix): string {
  switch (fix.kind) {
    case 'instructions':
      return t('features.agentEvaluation.enhance.kindInstructions', 'Agent instructions');
    case 'dimension':
      return t('features.agentEvaluation.enhance.kindDimension', 'Dimension {{name}}', { name: fix.label });
    case 'case':
      return t('features.agentEvaluation.enhance.kindCase', 'Case {{id}}', { id: fix.label });
  }
}

function AppliedRow({
  fix,
  onRequestPublish,
}: {
  readonly fix: AppliedFix;
  readonly onRequestPublish: ((versionId: number) => void) | undefined;
}): ReactNode {
  const failed = fix.error !== undefined;
  return (
    <Box data-testid={`enhance-applied-${fix.kind}-${fix.id}`}>
      <Typography variant="bodyMedium" color={failed ? 'error' : 'text.primary'} role={failed ? 'alert' : undefined}>
        {failed
          ? t('features.agentEvaluation.enhance.fixFailed', '{{what}} was not saved: {{reason}}', {
              what: kindLabel(fix),
              reason: evaluationErrorSentence(fix.error),
            })
          : t('features.agentEvaluation.enhance.fixSaved', '{{what}} saved.', { what: kindLabel(fix) })}
      </Typography>
      {fix.version !== undefined && (
        <>
          <Typography variant="bodySmall" color="text.secondary" data-testid="enhance-draft-created">
            {t(
              'features.agentEvaluation.enhance.draftCreated',
              'Draft version {{name}} created. It is not published.',
              {
                name: fix.version.name,
              },
            )}
          </Typography>
          {onRequestPublish !== undefined && (
            <BaseBtn
              variant="text"
              data-testid="enhance-request-publish"
              onClick={() => onRequestPublish(Number(fix.version?.id))}
            >
              {t('features.agentEvaluation.enhance.requestPublish', 'Request publish')}
            </BaseBtn>
          )}
        </>
      )}
    </Box>
  );
}

export function EnhanceReview({ counts, applied, applying, error, onRequestPublish }: EnhanceReviewProps): ReactNode {
  const total = counts.instructions + counts.dimensions + counts.cases;
  return (
    <Box sx={{ display: 'flex', flexDirection: 'column', gap: '0.5rem' }} data-testid="enhance-review">
      {applied !== undefined &&
        applied.map((fix) => (
          <AppliedRow key={`${fix.kind}-${fix.id}`} fix={fix} onRequestPublish={onRequestPublish} />
        ))}
      {(applied === undefined || total > 0) && (
        <Typography variant="bodyMedium" data-testid="enhance-review-summary">
          {total === 0
            ? t(
                'features.agentEvaluation.enhance.reviewNone',
                'Nothing is accepted. Go back and accept at least one change.',
              )
            : t(
                'features.agentEvaluation.enhance.reviewSummary',
                'Will save: {{instructions}} instruction draft, {{dimensions}} dimension change(s), {{cases}} expected-output change(s).',
                counts,
              )}
        </Typography>
      )}
      {applying && (
        <Typography component="output" variant="bodySmall" color="text.secondary">
          {t('features.agentEvaluation.enhance.applying', 'Saving…')}
        </Typography>
      )}
      {error !== undefined && error !== null && <Alert severity="error">{evaluationErrorSentence(error)}</Alert>}
    </Box>
  );
}
