import { render } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import type { PipelineEditorDeps } from './PipelineEditor';
import { PipelineEditorSaveButton } from './PipelineEditorParts';

function buildDeps(overrides: Partial<PipelineEditorDeps> = {}): PipelineEditorDeps {
  return {
    renderShell: () => null,
    ...overrides,
  };
}

/*
 * elitea_issues: #2223, #2664 — the pipeline Canvas Save button used to
 * enable itself the instant an edit-mode `onSaveVersion` handler existed
 * (every open of the Canvas editor), regardless of whether the user had
 * changed anything, and stayed enabled across a bare Configuration<->Flow
 * tab switch (no edit made). It must instead track `isDirty`.
 */
describe('PipelineEditorSaveButton (edit mode)', () => {
  it('elitea_issues 2223 - is disabled by default when no edits have been made, even though a save handler is wired', () => {
    const deps = buildDeps({ onSaveVersion: vi.fn() });

    const { getByTestId } = render(
      <PipelineEditorSaveButton
        isCreateMode={false}
        onCreateSave={() => {}}
        isCreating={false}
        canSaveCreate={false}
        isSavingVersion={false}
        deps={deps}
        onSaveSuccess={() => {}}
        isDirty={false}
      />,
    );

    expect(getByTestId('pipeline-save-button')).toBeDisabled();
  });

  it('elitea_issues 2664 - stays disabled across a tab switch that made no real change (isDirty still false)', () => {
    const deps = buildDeps({ onSaveVersion: vi.fn() });

    const { getByTestId, rerender } = render(
      <PipelineEditorSaveButton
        isCreateMode={false}
        onCreateSave={() => {}}
        isCreating={false}
        canSaveCreate={false}
        isSavingVersion={false}
        deps={deps}
        onSaveSuccess={() => {}}
        isDirty={false}
      />,
    );

    // Simulate switching from Configuration to Flow editor and back: no
    // edit was made, so the caller's computed `isDirty` never flips.
    rerender(
      <PipelineEditorSaveButton
        isCreateMode={false}
        onCreateSave={() => {}}
        isCreating={false}
        canSaveCreate={false}
        isSavingVersion={false}
        deps={deps}
        onSaveSuccess={() => {}}
        isDirty={false}
      />,
    );

    expect(getByTestId('pipeline-save-button')).toBeDisabled();
  });

  it('enables once the caller reports a real change (isDirty true)', () => {
    const deps = buildDeps({ onSaveVersion: vi.fn() });

    const { getByTestId } = render(
      <PipelineEditorSaveButton
        isCreateMode={false}
        onCreateSave={() => {}}
        isCreating={false}
        canSaveCreate={false}
        isSavingVersion={false}
        deps={deps}
        onSaveSuccess={() => {}}
        isDirty
      />,
    );

    expect(getByTestId('pipeline-save-button')).not.toBeDisabled();
  });

  it('stays disabled when isDirty is true but no save handler is wired', () => {
    const deps = buildDeps();

    const { getByTestId } = render(
      <PipelineEditorSaveButton
        isCreateMode={false}
        onCreateSave={() => {}}
        isCreating={false}
        canSaveCreate={false}
        isSavingVersion={false}
        deps={deps}
        onSaveSuccess={() => {}}
        isDirty
      />,
    );

    expect(getByTestId('pipeline-save-button')).toBeDisabled();
  });
});
