import type { ChangeEvent, ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useRef, useState } from 'react';

import DeleteOutlinedIcon from '@mui/icons-material/DeleteOutlined';
import Box from '@mui/material/Box';
import IconButton from '@mui/material/IconButton';
import Tooltip from '@mui/material/Tooltip';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { t } from '@/shared/i18n';
import { MAX_CONVERSATION_STARTERS, MAX_CONVERSATION_STARTER_LENGTH } from '@/shared/lib/limits';
import { BaseBtn } from '@/shared/ui/BaseBtn';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { combineSx } from '@/shared/ui/lib/combineSx';
import { StyledInputEnhancer } from '@/shared/ui/StyledInputEnhancer';

import { toString } from '../lib/helpers/conversationStarters.helpers';
import { useFieldFocus } from '../lib/useFieldFocus';

const STARTER_FOCUS_PREFIX = 'conversation_starters_';

/**
 * Ported from `apps/elitea-ui/src/components/ConversationStarters.jsx` (its
 * EDITOR half — the default export; the file's other two exports,
 * `EllipsisTextWithTooltip`/`ConversationStartersView`, are the chat-side
 * display list and are already covered in this app by
 * `features/chat-messages`' own `*ConversationStarter*` components).
 *
 * #307 — this component had no port at all. `CreateAgentForm` carried a
 * `conversationStartersSlot` for it and every caller left the slot empty, so
 * the ONE field the agent edit page did send on save was the one field the
 * page had no input for: the value round-tripped from the server and could
 * not be changed by anyone.
 *
 * DISCLOSED REDESIGN — no ambient form context (see `../model/types.ts`'s
 * module doc comment): the baseline reads `values.version_details
 * .conversation_starters` off `useFormikContext()` and writes through
 * `setFieldValue`/`handleChange`. Here the list is a prop and every mutation
 * goes out through `onStartersChange` — the caller owns the state, exactly
 * as `WelcomeMessageInput`/`ApplicationVariables` already do in this slice.
 *
 * Deliberately NOT locally mirrored (unlike `WelcomeMessageInput`'s
 * `inputValue`): the list has structural edits (add/delete) as well as text
 * edits, and a local mirror of a list is precisely where an add-then-type
 * race loses a keystroke. The caller's write is synchronous in both real
 * call sites (RHF `setValue` / `useState`).
 *
 * Baseline behaviours kept, each of which is a real gate rather than
 * decoration:
 *  - a hard cap of `MAX_CONVERSATION_STARTERS` (4) rows, with the add
 *    control disabled and tooltipped at the cap;
 *  - `MAX_CONVERSATION_STARTER_LENGTH` (768) per row, enforced by the
 *    input's own `maxLength`;
 *  - the newly-added row takes focus, so "add" is followed by typing rather
 *    than by hunting for the new field;
 *  - `disabled` hides the delete and add controls entirely rather than
 *    merely disabling them (`ConversationStarters.jsx:132,152` — `{!disabled
 *    && ...}`), which is what makes the read-only public-agent view of this
 *    panel actually read-only.
 *
 * Dropped: `data-tour` (the product-tour domain has no port — same
 * disclosure `WelcomeMessageInput` already carries) and the baseline's
 * per-row full-screen dialog test ids (`StyledInputEnhancer` owns its own
 * full-screen modal here and names it itself).
 */
export interface ConversationStartersEditorProps {
  readonly starters: readonly string[] | undefined;
  readonly onStartersChange: (next: readonly string[]) => void;
  readonly disabled?: boolean | undefined;
  readonly sx?: SxProps<Theme> | undefined;
}

interface ConversationStarterRowProps {
  readonly value: string;
  readonly index: number;
  readonly disabled: boolean;
  readonly onChange: (index: number, value: string) => void;
  readonly onDelete: (index: number) => void;
  readonly registerInput: (index: number, element: HTMLInputElement | HTMLTextAreaElement | null) => void;
}

function ConversationStarterRow({
  value,
  index,
  disabled,
  onChange,
  onDelete,
  registerInput,
}: ConversationStarterRowProps): ReactNode {
  const { toggleFieldFocus, isFocused } = useFieldFocus();
  const focusId = `${STARTER_FOCUS_PREFIX}${index}`;

  const handleChange = useCallback(
    (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => onChange(index, event.target.value),
    [index, onChange],
  );
  const handleFocus = useCallback(() => toggleFieldFocus(focusId), [toggleFieldFocus, focusId]);
  const handleBlur = useCallback(() => toggleFieldFocus(null), [toggleFieldFocus]);
  const handleDelete = useCallback(() => onDelete(index), [index, onDelete]);
  const handleInputRef = useCallback(
    (element: HTMLInputElement | HTMLTextAreaElement | null) => registerInput(index, element),
    [index, registerInput],
  );

  return (
    <Box sx={rowSx}>
      <Box sx={inputWrapperSx}>
        <StyledInputEnhancer
          autoComplete="off"
          label={t('features.agents.conversationStarters.starterLabel', 'Starter')}
          placeholder={t('features.agents.conversationStarters.placeholder', 'Chat message')}
          expand={{ minRows: 1, maxRows: 15 }}
          value={value}
          onChange={handleChange}
          onFocus={handleFocus}
          onBlur={handleBlur}
          disabled={disabled}
          inputRef={handleInputRef}
          slotProps={{
            htmlInput: {
              maxLength: MAX_CONVERSATION_STARTER_LENGTH,
              'data-testid': 'agent-conversation-starter-input',
            },
          }}
        />
        {/* #848 — never unmount: an unmounted counter shrinks this row and
          * shifts every row after it (and the `+ Starter` button once this
          * is the last row) up on blur, so a real click already in flight
          * lands on nothing. `visibility: hidden` keeps the line's height
          * reserved instead. */}
        <Typography
          variant="bodySmall"
          sx={combineSx(counterSx, { visibility: isFocused(focusId) && value.length > 0 ? 'visible' : 'hidden' })}
          data-testid="agent-conversation-starter-counter"
        >
          {`${MAX_CONVERSATION_STARTER_LENGTH - value.length} characters left`}
        </Typography>
      </Box>
      {!disabled && (
        <Box sx={deleteWrapperSx}>
          <Tooltip
            placement="top"
            title={t('features.agents.conversationStarters.delete', 'Delete')}
          >
            <IconButton
              aria-label={t('features.agents.conversationStarters.deleteStarter', 'delete starter')}
              onClick={handleDelete}
              disableRipple
              data-testid="agent-conversation-starter-delete"
            >
              <DeleteOutlinedIcon fontSize="small" />
            </IconButton>
          </Tooltip>
        </Box>
      )}
    </Box>
  );
}

export function ConversationStartersEditor({
  starters,
  onStartersChange,
  disabled = false,
  sx,
}: ConversationStartersEditorProps): ReactNode {
  // `toString` is the baseline's own coercion (`conversationStartersHelpers
  // .toString`, mapped over the raw field): a cleared row arrives as `null`
  // from the API and would otherwise flip the input from controlled to
  // uncontrolled mid-edit.
  const values = useMemo(() => (starters ?? []).map((entry) => toString(entry)), [starters]);

  const inputRefs = useRef(new Map<number, HTMLInputElement | HTMLTextAreaElement>());
  const [pendingFocusIndex, setPendingFocusIndex] = useState<number | undefined>(undefined);

  const registerInput = useCallback((index: number, element: HTMLInputElement | HTMLTextAreaElement | null) => {
    if (element === null) inputRefs.current.delete(index);
    else inputRefs.current.set(index, element);
  }, []);

  useEffect(() => {
    if (pendingFocusIndex === undefined) return;
    inputRefs.current.get(pendingFocusIndex)?.focus();
    setPendingFocusIndex(undefined);
  }, [pendingFocusIndex, values.length]);

  const handleChange = useCallback(
    (index: number, value: string) => {
      onStartersChange(values.map((entry, position) => (position === index ? value : entry)));
    },
    [onStartersChange, values],
  );

  const handleDelete = useCallback(
    (index: number) => {
      // The refs are keyed by index, so every row after the deleted one
      // shifts onto a different element; dropping the map lets each
      // surviving row re-register on its next render instead of holding a
      // node that now belongs to a different starter.
      inputRefs.current.clear();
      onStartersChange(values.filter((_, position) => position !== index));
    },
    [onStartersChange, values],
  );

  const handleAdd = useCallback(() => {
    setPendingFocusIndex(values.length);
    onStartersChange([...values, '']);
  }, [onStartersChange, values]);

  const isAtLimit = values.length >= MAX_CONVERSATION_STARTERS;

  const accordionItems = useMemo(
    () => [
      {
        title: t('features.agents.conversationStarters.title', 'Chat starters'),
        content: (
          <Box>
            {values.map((value, index) => (
              <ConversationStarterRow
                // Index is the identity here, exactly as in the baseline:
                // starters are an ordered list of plain strings with no ids,
                // and two rows may legitimately hold the same text.
                // eslint-disable-next-line react-x/no-array-index-key -- see above
                key={index}
                value={value}
                index={index}
                disabled={disabled}
                onChange={handleChange}
                onDelete={handleDelete}
                registerInput={registerInput}
              />
            ))}
            {!disabled && (
              <Tooltip
                placement="top-start"
                title={
                  isAtLimit
                    ? t('features.agents.conversationStarters.limitReached', 'You have reached the limit of chat starters')
                    : ''
                }
              >
                <Box sx={values.length === 0 ? addWrapperEmptySx : addWrapperSx}>
                  <BaseBtn
                    data-testid="agent-conversation-starter-add"
                    variant="text"
                    disabled={isAtLimit}
                    onClick={handleAdd}
                  >
                    {t('features.agents.conversationStarters.add', '+ Starter')}
                  </BaseBtn>
                </Box>
              </Tooltip>
            )}
          </Box>
        ),
      },
    ],
    [values, disabled, handleChange, handleDelete, handleAdd, registerInput, isAtLimit],
  );

  return (
    <BasicAccordion
      data-testid="agent-conversation-starters-section"
      showMode="left"
      slotSx={{ accordion: accordionSx, ...(sx !== undefined ? { root: sx } : {}) }}
      items={accordionItems}
    />
  );
}

const accordionSx: SxProps<Theme> = (theme: Theme) => ({
  background: theme.vars.palette.background.tabPanel,
});

const rowSx: SxProps<Theme> = {
  display: 'flex',
  gap: '1rem',
  marginTop: '1rem',
  '&:first-of-type': { marginTop: '0.5rem' },
};

const inputWrapperSx: SxProps<Theme> = { width: '100%' };

const deleteWrapperSx: SxProps<Theme> = { paddingBottom: '0.5rem' };

const addWrapperSx: SxProps<Theme> = { marginTop: '1.5rem' };

const addWrapperEmptySx: SxProps<Theme> = { marginTop: '0.75rem' };

const counterSx: SxProps<Theme> = {
  textAlign: 'right',
  width: '100%',
};
