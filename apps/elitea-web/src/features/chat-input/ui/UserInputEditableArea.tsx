import type { ChangeEvent, ClipboardEvent, CompositionEvent, KeyboardEvent, ReactNode, RefObject } from 'react';

import UnfoldLessIcon from '@mui/icons-material/UnfoldLess';
import UnfoldMoreIcon from '@mui/icons-material/UnfoldMore';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import TextField from '@mui/material/TextField';
import Typography from '@mui/material/Typography';

import { t } from '@/shared/i18n';
import { combineSx } from '@/shared/ui/lib/combineSx';

import type { HighlightRange, UserInputHighlightOverlaySlotProps, UserInputSlots } from './UserInput.types';
import { MAX_ROWS } from './UserInput.types';
import type { UserInputStyles } from './UserInput.styles';

/**
 * The highlight-overlay + `TextField` + expand/collapse affordance,
 * factored out of `UserInput.tsx` purely to keep that component's own
 * cyclomatic complexity under the §3.5 budget (≤12) — this piece alone
 * accounted for roughly half of `UserInput`'s branches (the highlight
 * overlay's conditional render, the expand-icon ternary, the caret-colour
 * override). §3.5 props budget: 8 (grouped).
 */
export interface UserInputEditableAreaProps {
  readonly refs: { readonly inputRef: RefObject<HTMLTextAreaElement | null>; readonly mirrorRef: RefObject<HTMLDivElement | null> };
  readonly content: { readonly value: string; readonly hasHighlights: boolean; readonly ranges: readonly HighlightRange[] };
  readonly highlightOverlay: UserInputSlots['highlightOverlay'];
  readonly focusState: { readonly isFocused: boolean; readonly onFocus: () => void; readonly onBlur: () => void };
  /** `minRows`: the desktop's resize handle (`ComposerResizeHandle`); it replaces the expand button there. */
  readonly rowState: { readonly rows: number; readonly showExpandIcon: boolean; readonly onClickExpander: () => void; readonly minRows?: number | undefined };
  readonly handlers: {
    readonly onChange: (event: ChangeEvent<HTMLTextAreaElement>) => void;
    readonly onKeyDown: (event: KeyboardEvent<HTMLDivElement>) => void;
    readonly onKeyUp: (event: KeyboardEvent<HTMLDivElement>) => void;
    readonly onCompositionStart: (event: CompositionEvent<HTMLDivElement>) => void;
    readonly onCompositionEnd: (event: CompositionEvent<HTMLDivElement>) => void;
    readonly onPaste: (event: ClipboardEvent<HTMLDivElement>) => void;
  };
  readonly field: { readonly placeholder: string | undefined; readonly color: string | undefined; readonly iconColor: string | undefined };
  readonly disabled: boolean | undefined;
  readonly styles: UserInputStyles;
}

function renderHighlightOverlay(props: UserInputEditableAreaProps): ReactNode {
  const { refs, content, highlightOverlay, styles } = props;
  if (!content.hasHighlights) return null;
  const overlayProps: UserInputHighlightOverlaySlotProps = { text: content.value, ranges: content.ranges };
  return (
    <Typography
      ref={refs.mirrorRef}
      aria-hidden="true"
      component="div"
      color="text.secondary"
      sx={styles.mirrorDiv}
    >
      {highlightOverlay?.(overlayProps)}
    </Typography>
  );
}

function ExpandAdornment({
  rows,
  onClick,
  iconColor,
  styles,
}: {
  readonly rows: number;
  readonly onClick: () => void;
  readonly iconColor: string | undefined;
  readonly styles: UserInputStyles;
}): ReactNode {
  return (
    <IconButton
      size="small"
      color="tertiary"
      sx={styles.expandButton}
      onClick={onClick}
      aria-label={t('chatInput.userInput.expand', 'Expand input')}
    >
      {rows === MAX_ROWS ? (
        <UnfoldLessIcon sx={styles.expandIcon(iconColor)} />
      ) : (
        <UnfoldMoreIcon sx={styles.expandIcon(iconColor)} />
      )}
    </IconButton>
  );
}

export function UserInputEditableArea(props: UserInputEditableAreaProps): ReactNode {
  const { refs, content, focusState, rowState, handlers, field, disabled, styles } = props;

  return (
    <Box sx={styles.textFieldWrapper}>
      {renderHighlightOverlay(props)}
      <TextField
        data-testid="chat-input"
        value={content.value}
        fullWidth
        id="standard-multiline-static"
        label=""
        multiline
        {...(rowState.minRows === undefined ? { maxRows: rowState.rows } : { minRows: rowState.minRows, maxRows: Math.max(rowState.rows, rowState.minRows) })}
        variant="standard"
        autoComplete="off"
        onChange={handlers.onChange}
        onKeyDown={handlers.onKeyDown}
        onKeyUp={handlers.onKeyUp}
        onCompositionStart={handlers.onCompositionStart}
        onCompositionEnd={handlers.onCompositionEnd}
        onPaste={handlers.onPaste}
        disabled={disabled}
        placeholder={focusState.isFocused ? '' : (field.placeholder ?? '')}
        onFocus={focusState.onFocus}
        onBlur={focusState.onBlur}
        sx={styles.textField}
        slotProps={{
          htmlInput: { 'data-testid': 'chat-message-input' },
          input: {
            inputRef: refs.inputRef,
            sx: combineSx(
              styles.textFieldInput(field.color),
              content.hasHighlights ? styles.transparentCaretText(field.color) : undefined,
            ),
            disableUnderline: true,
            endAdornment: rowState.showExpandIcon && rowState.minRows === undefined ? (
              <ExpandAdornment
                rows={rowState.rows}
                onClick={rowState.onClickExpander}
                iconColor={field.iconColor}
                styles={styles}
              />
            ) : null,
          },
        }}
      />
    </Box>
  );
}
