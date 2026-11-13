import type { ReactNode } from 'react';

import Box from '@mui/material/Box';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

import type { AiEditFields } from '../../lib/aiEditDraft';
import { TextDiffHighlight } from './TextDiffHighlight';

/**
 * The review step of "Edit with AI": the proposed value of every field the
 * edit can touch, pre-filled and editable, so Apply writes each one to its own
 * field. The instructions also get the word-level diff against the current text.
 */
export interface AiEditReviewFormProps {
  readonly current: AiEditFields;
  readonly draft: AiEditFields;
  readonly onChange: (next: AiEditFields) => void;
}

type TextKey = 'name' | 'description' | 'welcome_message';

interface FieldProps {
  readonly draft: AiEditFields;
  readonly field: TextKey;
  readonly label: string;
  readonly multiline: boolean;
  readonly onChange: (next: AiEditFields) => void;
}

function Field({ draft, field, label, multiline, onChange }: FieldProps): ReactNode {
  return (
    <TextField
      fullWidth
      multiline={multiline}
      minRows={multiline ? 2 : 1}
      maxRows={8}
      label={label}
      value={draft[field]}
      onChange={(event) => onChange({ ...draft, [field]: event.target.value })}
    />
  );
}

export function AiEditReviewForm({ current, draft, onChange }: AiEditReviewFormProps): ReactNode {
  return (
    <Box sx={reviewSx}>
      <Typography variant="labelMedium">{t('features.agents.aiEdit.proposedChanges', 'Proposed changes')}</Typography>
      <Field draft={draft} field="name" label={t('features.agents.aiEdit.fieldName', 'Name')} multiline={false} onChange={onChange} />
      <Field draft={draft} field="description" label={t('features.agents.aiEdit.fieldDescription', 'Description')} multiline onChange={onChange} />
      <TextDiffHighlight
        original={current.instructions}
        modified={draft.instructions}
        mode="modified"
      />
      <TextField
        fullWidth
        multiline
        minRows={6}
        maxRows={16}
        label={t('features.agents.aiEdit.fieldInstructions', 'Instructions')}
        value={draft.instructions}
        onChange={(event) => onChange({ ...draft, instructions: event.target.value })}
      />
      <Field draft={draft} field="welcome_message" label={t('features.agents.aiEdit.fieldWelcome', 'Welcome message')} multiline onChange={onChange} />
      <TextField
        fullWidth
        multiline
        minRows={2}
        maxRows={8}
        label={t('features.agents.aiEdit.fieldStarters', 'Conversation starters (one per line)')}
        value={draft.conversation_starters.join('\n')}
        onChange={(event) => onChange({ ...draft, conversation_starters: event.target.value.split('\n') })}
      />
    </Box>
  );
}

const reviewSx: SxProps<Theme> = { display: 'flex', flexDirection: 'column', gap: '1rem', minWidth: '30rem' };
