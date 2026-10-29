import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor } from '@testing-library/react';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { useEditorStateStore } from '@/shared/lib/editorState';

import { VersionSelector } from './VersionSelector';

const versions = [
  { id: '1', name: 'base', status: 'draft', agentType: 'chat', createdAt: '2024-01-01' },
  { id: '2', name: 'v2', status: 'published', agentType: 'chat', createdAt: '2024-02-01' },
];

describe('VersionSelector', () => {
  it('shows the selected version name and opens the menu with every version', () => {
    renderWithTheme(
      <VersionSelector
        versions={versions}
        selectedVersion={versions[0]}
        onSelect={vi.fn()}
      />,
    );
    expect(screen.getByRole('button', { name: /^version selector menu/ })).toBeInTheDocument();
    fireEvent.click(screen.getByRole('button', { name: /^version selector menu/ }));
    expect(screen.getByRole('menuitem', { name: 'base' })).toBeInTheDocument();
    expect(screen.getByRole('menuitem', { name: 'v2' })).toBeInTheDocument();
  });

  it('shows the version icon instead of the name in small view', () => {
    renderWithTheme(
      <VersionSelector
        versions={versions}
        selectedVersion={versions[0]}
        onSelect={vi.fn()}
        isSmallView
      />,
    );
    expect(screen.queryByText('base')).not.toBeInTheDocument();
  });

  it('selects a version and closes the editor when there is no dirty-editor guard', () => {
    const onSelect = vi.fn();
    const onCloseEditor = vi.fn();
    renderWithTheme(
      <VersionSelector
        versions={versions}
        selectedVersion={versions[0]}
        onSelect={onSelect}
        onCloseEditor={onCloseEditor}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: /^version selector menu/ }));
    fireEvent.click(screen.getByRole('menuitem', { name: 'v2' }));
    expect(onCloseEditor).toHaveBeenCalledTimes(1);
    expect(onSelect).toHaveBeenCalledWith(versions[1]);
  });

  it('routes through onShowVersionChangeAlert when an editor is open and dirty', () => {
    useEditorStateStore.getState().setEditingAgent(true);
    try {
      const onSelect = vi.fn();
      const onShowVersionChangeAlert = vi.fn((proceed: () => void) => proceed());
      renderWithTheme(
        <VersionSelector
          versions={versions}
          selectedVersion={versions[0]}
          onSelect={onSelect}
          isEditorDirty
          onShowVersionChangeAlert={onShowVersionChangeAlert}
        />,
      );
      fireEvent.click(screen.getByRole('button', { name: /^version selector menu/ }));
      fireEvent.click(screen.getByRole('menuitem', { name: 'v2' }));
      expect(onShowVersionChangeAlert).toHaveBeenCalledTimes(1);
      expect(onSelect).toHaveBeenCalledWith(versions[1]);
    } finally {
      useEditorStateStore.getState().setEditingAgent(false);
    }
  });

  it('does not render a refresh header when onRefresh is not provided', () => {
    renderWithTheme(
      <VersionSelector
        versions={versions}
        selectedVersion={versions[0]}
        onSelect={vi.fn()}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: /^version selector menu/ }));
    expect(screen.queryByText('Versions')).not.toBeInTheDocument();
  });

  it('renders a refresh header that calls onRefresh and shows a spinner while pending', async () => {
    let resolveRefresh: (() => void) | undefined;
    const onRefresh = vi.fn(
      () =>
        new Promise<void>((resolve) => {
          resolveRefresh = resolve;
        }),
    );
    renderWithTheme(
      <VersionSelector
        versions={versions}
        selectedVersion={versions[0]}
        onSelect={vi.fn()}
        onRefresh={onRefresh}
      />,
    );
    fireEvent.click(screen.getByRole('button', { name: /^version selector menu/ }));
    fireEvent.click(screen.getByLabelText('Refresh versions'));
    expect(onRefresh).toHaveBeenCalledTimes(1);
    expect(screen.getByLabelText('Refresh versions')).toBeDisabled();

    resolveRefresh?.();
    await waitFor(() => expect(screen.getByLabelText('Refresh versions')).not.toBeDisabled());
  });
});
