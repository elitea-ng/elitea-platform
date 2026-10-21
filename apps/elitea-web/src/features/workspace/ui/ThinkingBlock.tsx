/**
 * A model step's reasoning in the transcript (issue 1228): one folded
 * "Thinking" line with its length, unfolding to the text. The text is the
 * model's, shown as plain text (never Markdown or HTML), and only on demand.
 */
import { useId, useState } from 'react';

import ChevronRightIcon from '@mui/icons-material/ChevronRight';
import ExpandMoreIcon from '@mui/icons-material/ExpandMore';
import Box from '@mui/material/Box';
import ButtonBase from '@mui/material/ButtonBase';
import Collapse from '@mui/material/Collapse';
import type { Theme } from '@mui/material/styles';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { TranscriptItem } from '../model/turnReducer';

type ThinkingItem = Extract<TranscriptItem, { type: 'thinking' }>;

/** "Thinking…" while it streams, then "Thinking · 1,234 characters". */
export function thinkingLabel(item: ThinkingItem): string {
  if (item.chars === undefined) return t('workspace.thinking.streaming', 'Thinking…');
  const chars = new Intl.NumberFormat().format(item.chars);
  return t('workspace.thinking.done', 'Thinking · {{chars}} characters', { chars });
}

export function ThinkingBlock({ item }: { item: ThinkingItem }): React.JSX.Element {
  const [open, setOpen] = useState(false);
  const bodyId = useId();
  return (
    <Box data-testid="thinking-block">
      <ButtonBase
        aria-expanded={open}
        aria-controls={bodyId}
        onClick={() => setOpen((value) => !value)}
        sx={(theme: Theme) => ({
          display: 'flex',
          alignItems: 'center',
          gap: 0.5,
          paddingX: 0.5,
          paddingY: 0.25,
          borderRadius: theme.vars.shape.radiusSm,
          color: theme.vars.palette.text.secondary,
          '& svg': { width: '1rem', height: '1rem' },
          '&:hover': { background: theme.vars.palette.background.button.drawerMenu.hover },
        })}
      >
        {open ? <ExpandMoreIcon aria-hidden /> : <ChevronRightIcon aria-hidden />}
        <Typography variant="labelSmall" component="span">
          {thinkingLabel(item)}
        </Typography>
      </ButtonBase>
      <Collapse in={open} unmountOnExit>
        <Typography
          id={bodyId}
          variant="bodySmall"
          component="pre"
          data-testid="thinking-text"
          sx={(theme: Theme) => ({
            whiteSpace: 'pre-wrap',
            margin: 0,
            marginLeft: 3,
            marginTop: 0.5,
            padding: 1,
            fontFamily: theme.typography.fontFamily,
            borderLeft: `2px solid ${theme.vars.palette.divider}`,
            color: theme.vars.palette.text.secondary,
          })}
        >
          {item.text}
        </Typography>
      </Collapse>
    </Box>
  );
}
