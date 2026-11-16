import type { ReactNode } from 'react';

import { ApplicationSaveButton } from '@/entities/application-form';

function noopSave(): void {}

export interface SaveButtonSlotProps {
  readonly isCreateMode: boolean;
  readonly onCreateSave: () => void;
  readonly isCreating: boolean;
  readonly canSaveCreate: boolean;
  readonly onSaveVersion: (() => void) | undefined;
  readonly isSavingVersion: boolean | undefined;
  /** Edit mode: Save is live only while the form holds an edit (#2118, #2120). */
  readonly isDirty: boolean;
}

/** The create-vs-edit `ApplicationSaveButton` branch, extracted purely to keep `AgentEditor` under the complexity budget. */
export function AgentEditorSaveButton({
  isCreateMode,
  onCreateSave,
  isCreating,
  canSaveCreate,
  onSaveVersion,
  isSavingVersion,
  isDirty,
}: SaveButtonSlotProps): ReactNode {
  if (isCreateMode) {
    return (
      <ApplicationSaveButton
        onSave={onCreateSave}
        isSaving={isCreating}
        disabled={!canSaveCreate}
      />
    );
  }
  // Live only with a save to call AND an edit to save.
  const canSave = onSaveVersion !== undefined && isDirty;
  return (
    <ApplicationSaveButton
      onSave={onSaveVersion ?? noopSave}
      isSaving={isSavingVersion ?? false}
      disabled={!canSave}
    />
  );
}
