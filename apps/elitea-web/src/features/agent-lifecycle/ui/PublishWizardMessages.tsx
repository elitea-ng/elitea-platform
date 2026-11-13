import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { NormalizedPublishValidation } from '../lib/publishValidation';

/**
 * The inline notices under the publish wizard's step body. Split out of
 * `PublishVersionDialog` for the §3.5 cyclomatic-complexity budget (12); the
 * conditions are exactly the ones the dialog used to carry inline.
 */
export interface PublishWizardMessagesProps {
  readonly isPreparation: boolean;
  /** #2025: Continue was pressed with a field missing. */
  readonly attempted: boolean;
  readonly categoryChosen: boolean;
  /** Validation FAILED, so the wizard cannot advance to Publishing. */
  readonly blocked: boolean;
  /** Publish was pressed while blocked: the reason is spelled out. */
  readonly publishRefused: boolean;
  readonly validation: NormalizedPublishValidation | undefined;
  readonly error: string | undefined;
}

export function PublishWizardMessages({
  isPreparation,
  attempted,
  categoryChosen,
  blocked,
  publishRefused,
  validation,
  error,
}: PublishWizardMessagesProps): ReactNode {
  return (
    <>
      {isPreparation && attempted && (
        <Typography
          role="alert"
          variant="bodySmall"
          data-testid="publish-missing-summary"
        >
          {t(
            'features.agentLifecycle.publish.missingSummary',
            'Some required fields are missing. Fill in the marked fields to continue.',
          )}
        </Typography>
      )}
      {isPreparation && !categoryChosen && (
        <Typography variant="bodySmall">
          {t(
            'features.agentLifecycle.publish.categoryRequired',
            'Choose a category. The Catalog groups published agents by it, and an agent with no category does not appear there.',
          )}
        </Typography>
      )}
      {blocked && (
        <Typography
          role="alert"
          variant="bodySmall"
        >
          {t('features.agentLifecycle.publish.blocked', 'This version cannot be published until validation passes.')}
        </Typography>
      )}
      {blocked && publishRefused && <PublishBlockedReason validation={validation} />}
      {error !== undefined && (
        <Typography
          role="alert"
          variant="bodySmall"
          data-testid="publish-error"
        >
          {error}
        </Typography>
      )}
    </>
  );
}

function PublishBlockedReason({ validation }: { readonly validation: NormalizedPublishValidation | undefined }): ReactNode {
  return (
    <Box
      role="alert"
      data-testid="publish-blocked-reason"
    >
      {(validation?.criticalIssues ?? []).map((finding) => (
        <Typography
          key={finding.id}
          variant="bodySmall"
        >
          {finding.text}
        </Typography>
      ))}
      {(validation?.criticalIssues.length ?? 0) === 0 && validation?.summary !== undefined && (
        <Typography variant="bodySmall">{validation.summary}</Typography>
      )}
    </Box>
  );
}
