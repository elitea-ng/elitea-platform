// @ts-nocheck
/**
 * ParticipantsAccordion — accordion-style grouped display of participants.
 *
 * Ported from `[fsd]/features/chat/participants/ui/ExpandedParticipants/ParticipantsAccordion.jsx`.
 */
import { memo } from 'react';

import { Box, Accordion, AccordionDetails, AccordionSummary, Typography } from '@mui/material';

import ExpandMoreIcon from '@mui/icons-material/ExpandMore';

import { t } from '@/shared/i18n';

import type { ExpandedParticipantsListProps } from './ExpandedParticipantsList';

// ---------------------------------------------------------------------------
// Props
// ---------------------------------------------------------------------------

export interface ParticipantsAccordionProps {
  sections: Array<{ title: string; participants: Record<string, unknown>[] }>;
}

export type ParticipantsAccordionWithItemProps = ParticipantsAccordionProps & ExpandedParticipantsListProps;

/**
 * ParticipantsAccordion component — renders participants grouped by type in MUI accordions.
 */
const ParticipantsAccordion = memo((props: ParticipantsAccordionProps): React.ReactElement => {
  const { sections } = props;

  if (!sections?.length) return <Typography variant="bodyMedium" component="p" sx={{ p: 1, color: 'text.disabled' }}>{t('chat-participants.accordion.noParticipants', 'No participants')}</Typography>;

  return (
    <>
      {sections.map((section, index) => (
        <Accordion key={`${section.title}-${index}`} defaultExpanded={index === 0}>
          <AccordionSummary
            expandIcon={<ExpandMoreIcon />}
            aria-label={`${section.title} participants`}
          >
            <Typography variant="headingSmall" component="span">
              {section.title} ({section.participants.length})
            </Typography>
          </AccordionSummary>
          <AccordionDetails>
            <Box sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}>
              {section.participants.map((participant, pIndex) => (
                <Typography key={`${(participant.id as string) || pIndex}`} variant="bodyMedium" component="p" sx={{ px: 1 }}>
                  {participant.entity_meta?.name || t('chat-participants.common.unknown', 'Unknown')}
                </Typography>
              ))}
            </Box>
          </AccordionDetails>
        </Accordion>
      ))}
    </>
  );
});

ParticipantsAccordion.displayName = 'ParticipantsAccordion';

export default ParticipantsAccordion;
