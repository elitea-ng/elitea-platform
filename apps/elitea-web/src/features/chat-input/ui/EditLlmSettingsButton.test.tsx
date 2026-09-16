import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { EditLlmSettingsButton } from './EditLlmSettingsButton';

describe('EditLlmSettingsButton (A14, ELITEA-0386)', () => {
  it('renders nothing when onEditLlmSettings is omitted', () => {
    renderWithTheme(
      <EditLlmSettingsButton
        onEditLlmSettings={undefined}
        canEdit
        disabled={false}
      />,
    );
    expect(screen.queryByTestId('chat-agent-editor-llm-settings-button')).not.toBeInTheDocument();
  });

  it('renders nothing when the caller cannot edit this participant', () => {
    renderWithTheme(
      <EditLlmSettingsButton
        onEditLlmSettings={vi.fn()}
        canEdit={false}
        disabled={false}
      />,
    );
    expect(screen.queryByTestId('chat-agent-editor-llm-settings-button')).not.toBeInTheDocument();
  });

  it('renders and calls onEditLlmSettings when clicked', () => {
    const onEditLlmSettings = vi.fn();
    renderWithTheme(
      <EditLlmSettingsButton
        onEditLlmSettings={onEditLlmSettings}
        canEdit
        disabled={false}
      />,
    );
    const button = screen.getByTestId('chat-agent-editor-llm-settings-button');
    fireEvent.click(button);
    expect(onEditLlmSettings).toHaveBeenCalledOnce();
  });

  it('is disabled when disabled is true', () => {
    renderWithTheme(
      <EditLlmSettingsButton
        onEditLlmSettings={vi.fn()}
        canEdit
        disabled
      />,
    );
    expect(screen.getByTestId('chat-agent-editor-llm-settings-button')).toBeDisabled();
  });
});
