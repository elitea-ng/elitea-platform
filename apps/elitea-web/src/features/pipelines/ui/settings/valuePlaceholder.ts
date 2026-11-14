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

/**
 * Props that keep the label above the field and draw the hint in a legible colour on the node's dark
 * surface, and that line the Value field up with the Type select beside it (#2001).
 *
 * The standard `TextField` brand style pads its root (8px on top), pads its input row 12px on the
 * left and gives the input an 8px bottom margin; the `SingleSelect` next to it has none of those, and
 * is `size="small"`. Left alone, the two labels, the two underlines and the two text starts sat at
 * different heights and the Value text began under the caret. This zeroes those extras and uses the
 * same `small` size, so both fields share one label line, one text line and one underline.
 * (`&&&` beats the brand style's descendant selector without naming a `.Mui*` class.)
 */
export function visiblePlaceholderProps(placeholder: string): Pick<TextFieldProps, 'placeholder' | 'size' | 'slotProps' | 'sx'> {
  return {
    placeholder,
    size: 'small',
    slotProps: { inputLabel: { shrink: true }, input: { sx: { '&&&': { padding: 0 } } } },
    sx: (theme: Theme) => ({
      padding: 0,
      '& input, & textarea': { marginBottom: 0 },
      '& input::placeholder, & textarea::placeholder': { color: theme.vars.palette.text.secondary, opacity: 1 },
    }),
  };
}
