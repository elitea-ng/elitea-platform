import type { ChangeEvent, ReactNode } from 'react';
import { useCallback, useEffect, useMemo, useState } from 'react';

import Box from '@mui/material/Box';
import Typography from '@mui/material/Typography';
import type { SxProps, Theme } from '@mui/material/styles';

import { MAX_WELCOME_MESSAGE_LENGTH } from '@/shared/lib/limits';
import { t } from '@/shared/i18n';
import { BasicAccordion } from '@/shared/ui/BasicAccordion';
import { combineSx } from '@/shared/ui/lib/combineSx';
import { StyledInputEnhancer } from '@/shared/ui/StyledInputEnhancer';

import { useFieldFocus } from '../lib/useFieldFocus';

const WELCOME_MESSAGE_FOCUS_KEY = 'welcome_message';

/**
 * Ported from `apps/elitea-ui/src/[fsd]/features/agent/ui/agent-details/
 * configurations/input/WelcomeMessageInput.jsx`, which wrapped the baseline's
 * `components/WelcomeMessage.jsx`. Inlined here (private `WelcomeMessage`
 * function below) rather than kept as a separate file: `WelcomeMessage.jsx`
 * has exactly one consumer in the whole baseline tree
 * (`grep -rl "components/WelcomeMessage'"` — only this file), so it is a
 * genuine implementation detail of this component, not a shared one (unlike
 * `VariableList.jsx`, which `ApplicationVariables.tsx`'s own doc comment
 * explains is NOT inlined for exactly the opposite reason).
 *
 * DISCLOSED REDESIGN — no ambient form context (see `../model/types.ts`'s
 * module doc comment): `welcomeMessage` is a prop, `onWelcomeMessageChange`
 * replaces `formik.setFieldValue('version_details.welcome_message', ...)`.
 * The baseline's `debounce(..., 100)` (MUI's `debounce` util, imported
 * alongside `Box` from `@mui/material`) around the Formik commit is dropped:
 * the caller now owns commit timing (a `useState`/react-hook-form `setValue`
 * caller can debounce on its own side if it wants to), matching this
 * codebase's "the caller supplies the state-write" convention
 * (`ApplicationValidator.tsx`'s injected `useValidate`). Local state still
 * mirrors the prop for the same reason the baseline kept it: typing stays
 * visually instant even if a debounced/async caller commit lags behind.
 *
 * `data-tour={AGENT_TOUR_TARGET_IDS.welcomeMessage}` is dropped: the
 * baseline's product-tour overlay (`features/interactive-tours`) is a
 * separate top-level domain, out of this Wave-2 batch's scope entirely, and
 * `no-sideways-features` would forbid importing it even once built.
 *
 * `Text.CharacterCounter` has no confirmed `shared/ui` port in this worktree
 * (`find shared/ui -iname '*CharacterCounter*'` — no match); the counter
 * hint is reproduced as a plain `Typography`, matching every other counter
 * hint in this sub-unit's owned files (`CreateAgentForm.tsx`'s name/
 * description counters use the same plain-`Typography` shape).
 */
export interface WelcomeMessageInputProps {
  readonly welcomeMessage: string | undefined;
  readonly onWelcomeMessageChange: (value: string) => void;
  readonly versionId: number | undefined;
  readonly disabled?: boolean | undefined;
  readonly sx?: SxProps<Theme> | undefined;
}

export function WelcomeMessageInput({
  welcomeMessage,
  onWelcomeMessageChange,
  versionId,
  disabled,
  sx,
}: WelcomeMessageInputProps): ReactNode {
  const [inputValue, setInputValue] = useState(welcomeMessage ?? '');
  const { toggleFieldFocus, isFocused } = useFieldFocus();

  // Resync from the caller's value whenever IT changes externally (a version
  // switch, or a future discard/reset action) — the baseline's own
  // `components/WelcomeMessage.jsx` effect is `useEffect(() => { if
  // (welcome_message !== inputValue) setInputValue(welcome_message); },
  // [welcome_message])`: its dependency is the VALUE itself, not a version
  // id. `versionId` stays a second dependency (not the baseline's own
  // signal, but a real one this port already has and a version switch is
  // exactly the case this guard needs to survive) — the `welcomeMessage !==
  // inputValue` guard still prevents this from clobbering in-progress typing
  // on every render triggered by the caller's own state round-trip.
  useEffect(() => {
    if ((welcomeMessage ?? '') !== inputValue) {
      setInputValue(welcomeMessage ?? '');
    }
    // eslint-disable-next-line react-hooks/exhaustive-deps -- `inputValue` deliberately excluded: it's the guard, not a resync trigger (matching the baseline's identical omission).
  }, [welcomeMessage, versionId]);

  const handleInput = useCallback(
    (event: ChangeEvent<HTMLInputElement | HTMLTextAreaElement>) => {
      setInputValue(event.target.value);
      onWelcomeMessageChange(event.target.value);
    },
    [onWelcomeMessageChange],
  );

  const handleFocus = useCallback(() => toggleFieldFocus(WELCOME_MESSAGE_FOCUS_KEY), [toggleFieldFocus]);
  const handleBlur = useCallback(() => toggleFieldFocus(null), [toggleFieldFocus]);

  const showCounter = isFocused(WELCOME_MESSAGE_FOCUS_KEY) && inputValue.length > 0;

  const accordionItems = useMemo(
    () => [
      {
        title: 'Welcome message',
        content: (
          <Box>
            <StyledInputEnhancer
              placeholder={t('features.agents.welcomeMessageInput.placeholder', 'Input your welcome message')}
              expand={{ minRows: 1, maxRows: 15 }}
              value={inputValue}
              onChange={handleInput}
              onFocus={handleFocus}
              onBlur={handleBlur}
              disabled={disabled}
              slotProps={{
                htmlInput: { maxLength: MAX_WELCOME_MESSAGE_LENGTH, 'data-testid': 'agent-welcome-message-input' },
              }}
            />
            {/* #848 — never unmount: this line's height must stay reserved
              * even while hidden, or a control below it (`+ Starter` in
              * `ConversationStartersEditor`, mounted right after this
              * accordion) loses its first click on blur. `visibility:
              * hidden` keeps the box in flow without showing stale text. */}
            <Typography
              variant="bodySmall"
              sx={combineSx(counterSx, { visibility: showCounter ? 'visible' : 'hidden' })}
              data-testid="agent-welcome-message-counter"
            >
              {`${MAX_WELCOME_MESSAGE_LENGTH - inputValue.length} characters left`}
            </Typography>
          </Box>
        ),
      },
    ],
    [inputValue, handleInput, handleFocus, handleBlur, disabled, showCounter],
  );

  return (
    <BasicAccordion
      showMode="left"
      slotSx={{ accordion: accordionSx, ...(sx !== undefined ? { root: sx } : {}) }}
      items={accordionItems}
    />
  );
}

const accordionSx: SxProps<Theme> = (theme: Theme) => ({
  background: theme.vars.palette.background.tabPanel,
});

const counterSx: SxProps<Theme> = {
  textAlign: 'right',
  width: '100%',
};
