import { act, screen, waitFor } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { ToolEvents } from '@/entities/toolkit';

import { renderWithProviders } from '../../../__tests__/testUtils';
import { eventEmitter } from '@/shared/lib/eventEmitter';

import type { SaveToolkitPayload, ToolkitsOperationButtonsProps } from './ToolkitsOperationButtons';
import { ToolkitsOperationButtons } from './ToolkitsOperationButtons';

/**
 * `ToolkitsOperationButtons` coordinates entirely through `eventEmitter`
 * (`../../../lib/eventEmitter.ts`, a plain module-scope pub/sub bus — see
 * that file's own doc comment) rather than props/DOM for its Save/Update
 * triggers, mirroring the baseline's own event-driven design
 * (`ToolkitsOperationButtons.jsx:222-244`). Every test below drives the
 * component the same way its real caller (`ToolkitForm.tsx`) and that
 * caller's own caller (a page-level Save button, out of this sub-unit's
 * fence) do: `eventEmitter.emit(ToolEvents.X, reason)`.
 *
 * Listeners attached directly in a test (to observe an emitted event) are
 * always removed in a `finally` block — `eventEmitter` is a module
 * singleton, not reset between tests by `src/test/setup.ts`, so a stray
 * listener would leak into a later test's assertions.
 */
interface BasePropsOverrides {
  readonly isAdding?: boolean;
  readonly hasErrors?: boolean;
  readonly hasNotSavedToolConfiguration?: boolean;
  readonly setShowValidation?: (show: boolean) => void;
  readonly onCreateConfiguration?: () => Promise<unknown>;
  readonly onConfigurationCreated?: () => void;
  readonly toolSchema?: ToolkitsOperationButtonsProps['toolSchema'];
  readonly formValues?: Record<string, unknown>;
  readonly formInitialValues?: Record<string, unknown>;
  readonly isTeamProject?: boolean;
  readonly onSave?: (payload: SaveToolkitPayload) => Promise<Record<string, unknown>>;
  readonly onSaveSuccess?: (savedValues: Record<string, unknown>) => void;
  readonly onSaveError?: (message: string) => void;
}

/** Builds `ToolkitsOperationButtonsProps` from flat, per-test overrides — §3.5's `component-props` budget grouped the component's own props (`status`/`form`/`save`), but tests read more naturally against the pre-grouping flat field names, so this is the one place that reconciles the two. */
function baseProps(overrides: BasePropsOverrides = {}): ToolkitsOperationButtonsProps {
  return {
    isAdding: overrides.isAdding ?? false,
    status: { hasErrors: overrides.hasErrors ?? false, hasNotSavedToolConfiguration: overrides.hasNotSavedToolConfiguration ?? false },
    setShowValidation: overrides.setShowValidation ?? vi.fn(),
    onCreateConfiguration: overrides.onCreateConfiguration ?? vi.fn(),
    onTestConnection: vi.fn(),
    onRevertCredentials: vi.fn(),
    toolSchema: overrides.toolSchema,
    form: {
      values: overrides.formValues ?? { id: 'tk-1', name: 'My Toolkit', settings: {} },
      initialValues: overrides.formInitialValues ?? { id: 'tk-1', name: 'My Toolkit', settings: {} },
    },
    isTeamProject: overrides.isTeamProject ?? false,
    save: {
      onSave: overrides.onSave ?? vi.fn().mockResolvedValue({ id: 'tk-1', name: 'My Toolkit' }),
      onSuccess: overrides.onSaveSuccess,
      onError: overrides.onSaveError,
      onConfigurationCreated: overrides.onConfigurationCreated,
    },
    projectId: 'proj-1',
  };
}

describe('ToolkitsOperationButtons', () => {
  it('emits SaveEvent for ToolkitsCreateToolkit when there are no errors and no unsaved configuration', () => {
    renderWithProviders(<ToolkitsOperationButtons {...baseProps({ isAdding: true })} />);
    const saveSpy = vi.fn();
    eventEmitter.on(ToolEvents.SaveEvent, saveSpy);
    try {
      act(() => {
        eventEmitter.emit(ToolEvents.ToolkitsCreateToolkit, 'saveNewVersion');
      });
      expect(saveSpy).toHaveBeenCalledWith('saveNewVersion');
    } finally {
      eventEmitter.off(ToolEvents.SaveEvent, saveSpy);
    }
  });

  it('blocks ToolkitsCreateToolkit when hasErrors is set: triggers validation display, never emits SaveEvent', () => {
    const setShowValidation = vi.fn();
    renderWithProviders(
      <ToolkitsOperationButtons {...baseProps({ isAdding: true, hasErrors: true, setShowValidation })} />,
    );
    const saveSpy = vi.fn();
    eventEmitter.on(ToolEvents.SaveEvent, saveSpy);
    try {
      act(() => {
        eventEmitter.emit(ToolEvents.ToolkitsCreateToolkit, 'saveNewVersion');
      });
      expect(setShowValidation).toHaveBeenCalledWith(true);
      expect(saveSpy).not.toHaveBeenCalled();
    } finally {
      eventEmitter.off(ToolEvents.SaveEvent, saveSpy);
    }
  });

  it('blocks ToolkitsCreateToolkit when there is an unsaved configuration too (both gates share the same guard)', () => {
    const setShowValidation = vi.fn();
    renderWithProviders(
      <ToolkitsOperationButtons
        {...baseProps({ isAdding: true, hasNotSavedToolConfiguration: true, setShowValidation })}
      />,
    );
    const saveSpy = vi.fn();
    eventEmitter.on(ToolEvents.SaveEvent, saveSpy);
    try {
      act(() => {
        eventEmitter.emit(ToolEvents.ToolkitsCreateToolkit, 'saveNewVersion');
      });
      expect(setShowValidation).toHaveBeenCalledWith(true);
      expect(saveSpy).not.toHaveBeenCalled();
    } finally {
      eventEmitter.off(ToolEvents.SaveEvent, saveSpy);
    }
  });

  it('ToolkitsUpdateToolkit calls onSave with the resolved payload and then onSaveSuccess', async () => {
    const onSave = vi.fn<(payload: SaveToolkitPayload) => Promise<Record<string, unknown>>>().mockResolvedValue({
      id: 'tk-1',
      name: 'My Toolkit',
    });
    const onSaveSuccess = vi.fn();
    renderWithProviders(
      <ToolkitsOperationButtons
        {...baseProps({
          formValues: { id: 'tk-1', name: 'My Toolkit', settings: {} },
          onSave,
          onSaveSuccess,
        })}
      />,
    );

    act(() => {
      eventEmitter.emit(ToolEvents.ToolkitsUpdateToolkit);
    });

    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1));
    expect(onSave).toHaveBeenCalledWith({ projectId: 'proj-1', toolId: 'tk-1', values: { id: 'tk-1', name: 'My Toolkit', settings: {} }, name: 'My Toolkit' });
    await waitFor(() => expect(onSaveSuccess).toHaveBeenCalledWith({ id: 'tk-1', name: 'My Toolkit' }));
  });

  it('resolves the save name from a schema-flagged toolkit_name settings property, not formValues.name', async () => {
    const onSave = vi.fn().mockResolvedValue({});
    renderWithProviders(
      <ToolkitsOperationButtons
        {...baseProps({
          formValues: { id: 'tk-2', name: 'ignored', settings: { api_key: 'server-name-value' } },
          toolSchema: { properties: { api_key: { toolkit_name: true } } },
          onSave,
        })}
      />,
    );

    act(() => {
      eventEmitter.emit(ToolEvents.ToolkitsUpdateToolkit);
    });

    await waitFor(() => expect(onSave).toHaveBeenCalledWith(expect.objectContaining({ name: 'server-name-value' })));
  });

  it('ToolkitsUpdateToolkit reports onSaveError when onSave rejects, without throwing out of the handler', async () => {
    const onSave = vi.fn().mockRejectedValue(new Error('save failed'));
    const onSaveError = vi.fn();
    renderWithProviders(<ToolkitsOperationButtons {...baseProps({ onSave, onSaveError })} />);

    act(() => {
      eventEmitter.emit(ToolEvents.ToolkitsUpdateToolkit);
    });

    await waitFor(() => expect(onSaveError).toHaveBeenCalledWith('save failed'));
  });

  it('ToolkitsCreateToolkitWithConfiguration creates the configuration and calls onConfigurationCreated on success', async () => {
    const onCreateConfiguration = vi.fn().mockResolvedValue(true);
    const onConfigurationCreated = vi.fn();
    renderWithProviders(
      <ToolkitsOperationButtons
        {...baseProps({ isAdding: true, hasNotSavedToolConfiguration: true, onCreateConfiguration, onConfigurationCreated })}
      />,
    );

    act(() => {
      eventEmitter.emit(ToolEvents.ToolkitsCreateToolkitWithConfiguration, 'saveNewVersion');
    });

    await waitFor(() => expect(onCreateConfiguration).toHaveBeenCalledTimes(1));
    await waitFor(() => expect(onConfigurationCreated).toHaveBeenCalledTimes(1));
  });

  it('ToolkitsCreateToolkitWithConfiguration does NOT call onConfigurationCreated when configuration creation fails', async () => {
    const onCreateConfiguration = vi.fn().mockResolvedValue(false);
    const onConfigurationCreated = vi.fn();
    renderWithProviders(
      <ToolkitsOperationButtons
        {...baseProps({ isAdding: true, hasNotSavedToolConfiguration: true, onCreateConfiguration, onConfigurationCreated })}
      />,
    );

    act(() => {
      eventEmitter.emit(ToolEvents.ToolkitsCreateToolkitWithConfiguration, 'saveNewVersion');
    });

    await waitFor(() => expect(onCreateConfiguration).toHaveBeenCalledTimes(1));
    expect(onConfigurationCreated).not.toHaveBeenCalled();
  });

  it('shows the credential-warning modal (not a direct save) when a team credential changed on an existing toolkit', async () => {
    const onSave = vi.fn().mockResolvedValue({});
    renderWithProviders(
      <ToolkitsOperationButtons
        {...baseProps({
          isAdding: false,
          isTeamProject: true,
          formValues: { id: 'tk-1', settings: { cred: { elitea_title: 'private-x', private: true } } },
          formInitialValues: { id: 'tk-1', settings: { cred: { elitea_title: 'team-x', private: false } } },
          onSave,
        })}
      />,
    );

    expect(screen.queryByText(/Confirm changes/i)).not.toBeInTheDocument();

    act(() => {
      eventEmitter.emit(ToolEvents.ToolkitsUpdateToolkit);
    });

    await waitFor(() => expect(screen.getByText(/Confirm changes/i)).toBeInTheDocument());
    expect(onSave).not.toHaveBeenCalled();
  });

  it('confirming the credential warning proceeds with the deferred save', async () => {
    const onSave = vi.fn().mockResolvedValue({});
    const { getByText } = renderWithProviders(
      <ToolkitsOperationButtons
        {...baseProps({
          isAdding: false,
          isTeamProject: true,
          formValues: { id: 'tk-1', settings: { cred: { elitea_title: 'private-x', private: true } } },
          formInitialValues: { id: 'tk-1', settings: { cred: { elitea_title: 'team-x', private: false } } },
          onSave,
        })}
      />,
    );

    act(() => {
      eventEmitter.emit(ToolEvents.ToolkitsUpdateToolkit);
    });
    await waitFor(() => expect(getByText(/Confirm changes/i)).toBeInTheDocument());

    act(() => {
      getByText(/Confirm changes/i).click();
    });

    await waitFor(() => expect(onSave).toHaveBeenCalledTimes(1));
  });

  it('stops responding to emitted events once unmounted (listener cleanup)', () => {
    const onSave = vi.fn();
    const { unmount } = renderWithProviders(<ToolkitsOperationButtons {...baseProps({ onSave })} />);
    unmount();

    act(() => {
      eventEmitter.emit(ToolEvents.ToolkitsUpdateToolkit);
    });

    expect(onSave).not.toHaveBeenCalled();
  });
});
