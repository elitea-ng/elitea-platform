import type { TextFieldProps } from '@mui/material/TextField';
import type { Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';

/**
 * The hint an EMPTY node field shows (issue 2001). Before this, an empty value was
 * a bare underline under a grey label, so nothing said what the field takes. The
 * label stays above the field (`shrink`) and the hint sits where the text goes.
 */
export function valuePlaceholder(variable: string): string {
  switch (variable.toLowerCase()) {
    case 'system':
      return t('pipelines.valuePlaceholder.system', 'Describe how the assistant should behave');
    case 'task':
      return t('pipelines.valuePlaceholder.task', 'Describe what the assistant should do');
    case 'chat_history':
      return t('pipelines.valuePlaceholder.chatHistory', 'Earlier messages as a JSON list, or leave empty');
    case 'code':
      return t('pipelines.valuePlaceholder.code', 'Write the code to run');
    case 'user_message':
      return t('pipelines.valuePlaceholder.userMessage', 'The message the user will see');
    default:
      return t('pipelines.valuePlaceholder.default', 'Enter a value');
  }
}

/** Props that keep the label above the field and draw the hint in a legible colour on the node's dark surface. */
export function visiblePlaceholderProps(placeholder: string): Pick<TextFieldProps, 'placeholder' | 'slotProps' | 'sx'> {
  return {
    placeholder,
    slotProps: { inputLabel: { shrink: true } },
    sx: (theme: Theme) => ({
      '& input::placeholder, & textarea::placeholder': { color: theme.vars.palette.text.secondary, opacity: 1 },
    }),
  };
}
