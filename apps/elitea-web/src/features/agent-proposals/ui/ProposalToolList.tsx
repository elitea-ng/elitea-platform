/**
 * The proposed tools with their ADR-0041 category. An undeclared tool or a
 * wildcard is `write`, the server's own conservative reading; the list says
 * so rather than showing a tool as safer than the platform treats it.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Chip from '@mui/material/Chip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import type { AgentProposalToolCategory } from '@/shared/api/generated/model';
import { t } from '@/shared/i18n';

export interface ProposalToolListProps {
  readonly tools: readonly AgentProposalToolCategory[];
}

function categoryLabel(category: string): string {
  switch (category) {
    case 'read':
      return t('features.agentProposals.category.read', 'Read');
    case 'write':
      return t('features.agentProposals.category.write', 'Write');
    case 'external_send':
      return t('features.agentProposals.category.externalSend', 'Sends externally');
    case 'money':
      return t('features.agentProposals.category.money', 'Spends money');
    case 'destructive':
      return t('features.agentProposals.category.destructive', 'Destructive');
    default:
      return category;
  }
}

export function ProposalToolList({ tools }: ProposalToolListProps): ReactNode {
  return (
    <Box sx={listSx} data-testid="agent-proposal-tools">
      <Typography variant="labelMedium">{t('features.agentProposals.review.tools', 'Proposed tools')}</Typography>
      {tools.length === 0 ? (
        <Typography variant="bodySmall" color="text.secondary">
          {t('features.agentProposals.review.noTools', 'No tools were proposed.')}
        </Typography>
      ) : (
        <>
          {tools.map((tool) => (
            <Box key={tool.key} sx={rowSx} data-testid={`agent-proposal-tool-${tool.key}`}>
              <Typography variant="bodySmall" sx={{ overflowWrap: 'anywhere' }}>
                {tool.key}
              </Typography>
              <Chip size="small" label={categoryLabel(tool.category)} data-category={tool.category} />
            </Box>
          ))}
          <Typography variant="bodySmall" color="text.secondary">
            {t('features.agentProposals.review.toolsNote', 'Tools are not added by Create. Add them in the agent editor, where their gates apply.')}
          </Typography>
        </>
      )}
    </Box>
  );
}

const listSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '0.25rem' };
const rowSx: SxProps<Theme> = { display: 'flex', alignItems: 'center', gap: '0.5rem' };
