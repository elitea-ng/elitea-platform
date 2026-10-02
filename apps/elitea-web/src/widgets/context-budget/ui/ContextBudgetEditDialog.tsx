import { useEffect, useState, type ReactNode } from 'react';

import { t } from '@/shared/i18n';
import type { ContextBudgetMode } from '@/shared/lib/contextBudget';
import { BaseModal } from '@/shared/ui/BaseModal';
import { ContextBudgetModeControl } from '@/shared/ui/ContextBudgetModeControl';

export interface ContextBudgetEditDialogProps {
  readonly open: boolean;
  readonly currentMode: ContextBudgetMode;
  readonly isSaving: boolean;
  readonly errorMessage?: string | undefined;
  readonly onClose: () => void;
  readonly onSave: (mode: ContextBudgetMode) => void;
}

export function ContextBudgetEditDialog({ open, currentMode, isSaving, errorMessage, onClose, onSave }: ContextBudgetEditDialogProps): ReactNode {
  const [draft, setDraft] = useState<ContextBudgetMode>(currentMode);
  useEffect(() => { if (open) setDraft(currentMode); }, [open, currentMode]);
  return (
    <BaseModal
      open={open}
      onClose={onClose}
      onConfirm={() => onSave(draft)}
      title={t('widgets.contextBudget.edit.defaultTitle', 'Default context settings')}
      data-testid="context-budget-edit-dialog"
      actions={{ confirmText: t('widgets.contextBudget.edit.save', 'Save'), confirming: isSaving }}
      content={<>
        <ContextBudgetModeControl value={draft} onChange={setDraft} disabled={isSaving} />
        <p>{t('widgets.contextBudget.edit.defaultHelp', 'Applies to new runs without a conversation override. Runs already in progress keep their original settings.')}</p>
        {errorMessage !== undefined && <span role="alert" data-testid="context-budget-edit-error">{errorMessage}</span>}
      </>}
    />
  );
}
