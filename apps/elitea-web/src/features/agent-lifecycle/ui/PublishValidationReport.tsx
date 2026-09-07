import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { NormalizedPublishValidation, PublishValidationFinding } from '../lib/publishValidation';

/**
 * The wizard's Validation step.
 *
 * Every list is rendered, including the empty ones' absence — a PASS with no
 * findings says so in words. A validation screen that shows nothing at all is
 * indistinguishable from one whose request never ran, and the author is about
 * to make a decision on it.
 */
export interface PublishValidationReportProps {
  readonly validation: NormalizedPublishValidation;
}

function FindingList({ title, findings }: { readonly title: string; readonly findings: readonly PublishValidationFinding[] }): ReactNode {
  if (findings.length === 0) return null;
  return (
    <Box sx={sectionSx}>
      <Typography variant="labelMedium">{title}</Typography>
      <Box component="ul" sx={listSx}>
        {findings.map((finding) => (
          <Box component="li" key={finding.id}>
            <Typography variant="bodySmall">{finding.text}</Typography>
            {finding.fix !== undefined && (
              <Typography variant="bodySmall" color="text.secondary">
                {finding.fix}
              </Typography>
            )}
          </Box>
        ))}
      </Box>
    </Box>
  );
}

export function PublishValidationReport({ validation }: PublishValidationReportProps): ReactNode {
  const clean =
    validation.criticalIssues.length === 0 &&
    validation.warnings.length === 0 &&
    validation.recommendations.length === 0;
  return (
    <Box data-testid="publish-validation-report">
      <Typography
        variant="labelMedium"
        data-testid="publish-validation-status"
      >
        {validation.status}
      </Typography>
      {validation.summary !== undefined && <Typography variant="bodySmall">{validation.summary}</Typography>}
      <FindingList
        title={t('features.agentLifecycle.publish.criticalIssues', 'Critical issues')}
        findings={validation.criticalIssues}
      />
      <FindingList
        title={t('features.agentLifecycle.publish.warnings', 'Warnings')}
        findings={validation.warnings}
      />
      <FindingList
        title={t('features.agentLifecycle.publish.recommendations', 'Recommendations')}
        findings={validation.recommendations}
      />
      {clean && (
        <Typography variant="bodySmall">
          {t('features.agentLifecycle.publish.noFindings', 'Validation found nothing to report.')}
        </Typography>
      )}
      {validation.status === 'FAIL' && (
        <Typography
          role="alert"
          variant="bodySmall"
        >
          {t(
            'features.agentLifecycle.publish.failed',
            'Validation failed. Fix the critical issues above, then run validation again.',
          )}
        </Typography>
      )}
    </Box>
  );
}

const sectionSx: SxProps<Theme> = { marginTop: '0.75rem' };
const listSx: SxProps<Theme> = { margin: 0, paddingLeft: '1.25rem' };
