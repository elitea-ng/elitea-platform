/**
 * One question row of `AnswerQuestionsControl`: its header, its text, its
 * option buttons and its free-text field.
 *
 * Split out of `AnswerQuestionsControl.tsx` to keep that file under the §3.5
 * 400-line budget — see that file's own header for the feature this supports.
 */
import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import Button from '@mui/material/Button';
import Stack from '@mui/material/Stack';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';

import type { HitlQuestion } from '../../lib/hitlInterrupts';
import { isOptional, optionLabel } from './answerQuestionsHelpers';

export interface QuestionRowProps {
  readonly question: HitlQuestion;
  readonly index: number;
  readonly disabled: boolean;
  readonly picked: readonly string[];
  readonly text: string;
  readonly onPick: (label: string) => void;
  readonly onText: (value: string) => void;
}

/** One question: its header, its text, its option buttons and its free-text field. */
export function QuestionRow({ question, index, disabled, picked, text, onPick, onText }: QuestionRowProps): ReactNode {
  const options = question.options ?? [];
  // A question with no options cannot be answered any other way, so the field
  // is offered whether or not the model set `allow_other`.
  const showText = question.allow_other === true || options.length === 0;

  return (
    <Box
      data-testid={`hitl-answer-question-${index}`}
      sx={{ display: 'flex', flexDirection: 'column', gap: 0.5 }}
    >
      {question.header && (
        <Typography
          variant="caption"
          sx={{ fontWeight: 600, color: 'text.secondary' }}
        >
          {question.header}
        </Typography>
      )}
      {question.question && (
        <Typography
          variant="body2"
          sx={{ color: 'text.primary' }}
        >
          {question.question}
          {/* ELITEA-2792 step 1: an optional question SAYS so. Without it the
              relaxed Next/Submit rule is invisible — the user cannot tell a
              question they may skip from one whose control is enabled because
              they already answered it. */}
          {isOptional(question) && (
            <Typography
              component="span"
              data-testid={`hitl-answer-optional-${index}`}
              variant="caption"
              sx={{ color: 'text.secondary', ml: 0.5 }}
            >
              {t('chatMessages.hitlAnswer.optional', '(optional)')}
            </Typography>
          )}
        </Typography>
      )}
      {options.length > 0 && (
        <Stack
          direction="row"
          spacing={1}
          sx={{ flexWrap: 'wrap', gap: 1 }}
        >
          {options.map((option, optionIndex) => {
            const label = optionLabel(option);
            const isPicked = picked.includes(label);
            return (
              <Button
                key={`${label}-${optionIndex}`}
                data-testid={`hitl-answer-option-${index}-${optionIndex}`}
                size="small"
                variant={isPicked ? 'contained' : 'outlined'}
                color="primary"
                onClick={() => onPick(label)}
                disabled={disabled || !label}
                {...(option.description ? { title: option.description } : {})}
              >
                {label}
              </Button>
            );
          })}
        </Stack>
      )}
      {showText && (
        <TextField
          fullWidth
          size="small"
          value={text}
          onChange={(event) => onText(event.target.value)}
          disabled={disabled}
          placeholder={t('chatMessages.hitlAnswer.otherPlaceholder', 'Type another answer…')}
          slotProps={{ htmlInput: { 'data-testid': `hitl-answer-other-${index}` } }}
        />
      )}
    </Box>
  );
}
