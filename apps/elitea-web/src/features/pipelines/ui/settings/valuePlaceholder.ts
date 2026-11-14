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

/** Props that keep the label above the field and draw the hint in a legible colour on the node's dark surface. Placeholder only: no layout. */
export function visiblePlaceholderProps(placeholder: string): Pick<TextFieldProps, 'placeholder' | 'slotProps' | 'sx'> {
  return {
    placeholder,
    slotProps: { inputLabel: { shrink: true } },
    sx: (theme: Theme) => ({
      '& input::placeholder, & textarea::placeholder': { color: theme.vars.palette.text.secondary, opacity: 1 },
    }),
  };
}

/** How far (in spacing units) the aligned field's label row sits above a stock standard field's: the 8px root padding it drops. */
export const ALIGNED_LABEL_ROW_SHIFT = -1;

/**
 * The Type/Value row only (#2001): lines a Value field up with the `SingleSelect` beside it.
 *
 * The standard `TextField` brand style pads its root 8px on top, pads the input row 4px on top and
 * 12px on the left, and gives a single-line input an 8px bottom margin. The select has none of
 * those and is `size="small"`, so the labels, text and underlines sat at different heights and the
 * text started under the caret. This drops the root padding, the top padding and the left indent,
 * and (single-line only) the bottom margin. A multiline field keeps its textarea margin, so its
 * last line stays clear of the underline, and its right padding, so text stays clear of the hover
 * toolbar. `&&&` beats the brand style's descendant selector without naming a `.Mui*` class.
 */
export function alignedFieldProps(placeholder: string, multiline: boolean): Pick<TextFieldProps, 'placeholder' | 'size' | 'slotProps' | 'sx'> {
  const base = visiblePlaceholderProps(placeholder);
  const baseSx = base.sx as (theme: Theme) => Record<string, unknown>;
  return {
    placeholder,
    size: 'small',
    slotProps: { inputLabel: { shrink: true }, input: { sx: { '&&&': { paddingTop: 0, paddingLeft: 0 } } } },
    sx: (theme: Theme) => ({
      ...baseSx(theme),
      padding: 0,
      ...(multiline ? {} : { '& input': { marginBottom: 0 } }),
    }),
  };
}
