/**
 * What a repository proposes in `.elitea/rules.json`, rule by rule, as an
 * import would copy it (the host has already dropped program pins and
 * narrowed allows: `warnings` says what), then the one button that imports
 * exactly that (named by the proposal's digest).
 */
import { Alert, Box, Button, Stack, Typography } from '@mui/material';

import type { RuleProposal } from '@/shared/desktop/rulesIpc';
import { t } from '@/shared/i18n';

import { formatPattern } from '../model/patternText';
import { RuleSummary } from './ruleParts';

export interface ProposalReviewProps {
  proposal: RuleProposal;
  busy: boolean;
  onImport: (digest: string) => void;
}

export function ProposalReview({ proposal, busy, onImport }: ProposalReviewProps): React.JSX.Element {
  if (proposal.error !== null) {
    return (
      <Alert severity="info" data-testid="rules-proposed">
        {t('workspace.rules.proposedBroken', 'This repository proposes rules, but they are not valid: {{message}}', { message: proposal.error.message })}
      </Alert>
    );
  }
  const rules = proposal.file?.rules ?? [];
  return (
    <Stack spacing={1.5} component="section" data-testid="rules-proposed" aria-label={t('workspace.rules.proposedTitle', 'Proposed by the repository')}>
      <Alert severity="info">
        {t('workspace.rules.proposed', 'This repository proposes {{n}} rules (.elitea/rules.json). They are not used unless you import them; review them first: an import copies exactly these.', { n: proposal.count })}
      </Alert>
      {rules.length > 0 && (
        <Stack component="ol" spacing={1} sx={{ listStyle: 'none', p: 0, m: 0 }}>
          {rules.map((rule, index) => (
            <Box component="li" key={`${index}-${formatPattern(rule.pattern)}`} data-testid={`proposed-rule-${index + 1}`} sx={{ display: 'flex', gap: 1.5, alignItems: 'flex-start' }}>
              <RuleSummary rule={rule} />
            </Box>
          ))}
        </Stack>
      )}
      {proposal.warnings.length > 0 && (
        <Alert severity="warning" data-testid="rules-proposed-warnings">
          <Typography variant="bodySmall" sx={{ fontWeight: 600 }}>
            {t('workspace.rules.proposedWarnings', 'An import grants less than the repository asks:')}
          </Typography>
          <Box component="ul" sx={{ m: 0, pl: 2 }}>
            {proposal.warnings.map((warning) => (
              <Typography component="li" variant="bodySmall" key={warning} sx={{ overflowWrap: 'anywhere' }}>
                {warning}
              </Typography>
            ))}
          </Box>
        </Alert>
      )}
      {proposal.count > 0 && (
        <Box>
          <Button size="small" variant="outlined" onClick={() => onImport(proposal.digest)} disabled={busy}>
            {t('workspace.rules.import', 'Import these rules')}
          </Button>
        </Box>
      )}
    </Stack>
  );
}
