import { useEffect, useState, type ReactNode } from 'react';

import { t } from '@/shared/i18n';
import { BaseModal } from '@/shared/ui/BaseModal';
import { InputBase } from '@/shared/ui/InputBase';

import {
  MAX_MAX_CONTEXT_TOKENS,
  MIN_MAX_CONTEXT_TOKENS,
  validateMaxContextTokens,
} from '../lib/authorContextUpdate';

/**
 * ui/ContextBudgetEditDialog.tsx — the one field the chat panel's pencil
 * opens: the reader's own default context budget.
 *
 * DELIBERATELY ONE FIELD. The reference's `ContextStrategyModal` also carries
 * the conversation's instructions, persona and summary-LLM settings, which is
 * a form over `updateContextStrategy` — a different route, writing the
 * CONVERSATION's own strategy. This dialog writes the reader's DEFAULT, which
 * is the term the panel's own number actually resolves through for a
 * conversation with no strategy of its own, and it writes it through the same
 * `PUT /social/author` Settings › Memory uses. One setting, two places to
 * reach it, not two settings that drift.
 *
 * The field is validated against the same range the server enforces
 * (`internal/domain/contextsettings`), so a refusal is shown before the
 * request rather than as an error afterwards.
 */
export interface ContextBudgetEditDialogProps {
  readonly open: boolean;
  /** The budget currently in force — the reader's own default, or the resolved one when they have never set one. */
  readonly currentMaxTokens: number | undefined;
  readonly isSaving: boolean;
  /** The reason the last save was refused, shown inside the dialog. */
  readonly errorMessage?: string | undefined;
  readonly onClose: () => void;
  readonly onSave: (maxContextTokens: number) => void;
}

export function ContextBudgetEditDialog({
  open,
  currentMaxTokens,
  isSaving,
  errorMessage,
  onClose,
  onSave,
}: ContextBudgetEditDialogProps): ReactNode {
  const [draft, setDraft] = useState('');

  // Re-seed on every open, so a cancelled edit does not come back next time.
  useEffect(() => {
    if (open) setDraft(currentMaxTokens === undefined ? '' : String(currentMaxTokens));
  }, [open, currentMaxTokens]);

  const parsed = validateMaxContextTokens(draft);
  const isInvalid = draft.trim() !== '' && parsed === undefined;

  return (
    <BaseModal
      open={open}
      onClose={onClose}
      onConfirm={() => {
        if (parsed !== undefined) onSave(parsed);
      }}
      title={t('widgets.contextBudget.edit.title', 'Context settings')}
      data-testid="context-budget-edit-dialog"
      actions={{
        confirmText: t('widgets.contextBudget.edit.save', 'Save'),
        confirming: isSaving || parsed === undefined,
      }}
      content={
        <>
          <InputBase
            type="text"
            inputMode="numeric"
            label={t('widgets.contextBudget.edit.maxTokens', 'Max Context Tokens')}
            value={draft}
            onChange={(event) => setDraft(event.target.value)}
            error={isInvalid}
            helperText={
              isInvalid
                ? t('widgets.contextBudget.edit.range', 'Enter a whole number between {{min}} and {{max}}.', {
                    min: MIN_MAX_CONTEXT_TOKENS,
                    max: MAX_MAX_CONTEXT_TOKENS,
                  })
                : t(
                    'widgets.contextBudget.edit.help',
                    'This is your own default for new conversations. Settings › Memory writes the same value.',
                  )
            }
            slotProps={{ htmlInput: { 'data-testid': 'context-budget-max-tokens-input' } }}
          />
          {errorMessage !== undefined && (
            <span
              role="alert"
              data-testid="context-budget-edit-error"
            >
              {errorMessage}
            </span>
          )}
        </>
      }
    />
  );
}
