import type { ReactNode } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { act, renderHook, waitFor } from '@testing-library/react';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { useToolkitFormConfiguration } from './ToolkitForm.configuration.hooks';
import type { CoreState } from './ToolkitForm.core.hooks';
import type { ResolvedToolkitFormProps } from './ToolkitForm.types';

const BASE = '/api/v2';

function createWrapper(): { wrapper: ({ children }: { children: ReactNode }) => ReactNode } {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  function Wrapper({ children }: { children: ReactNode }): ReactNode {
    return <QueryClientProvider client={client}>{children}</QueryClientProvider>;
  }
  return { wrapper: Wrapper };
}

function baseProps(overrides: Partial<ResolvedToolkitFormProps> = {}): ResolvedToolkitFormProps {
  return {
    editToolDetail: { type: 'github', settings: {} },
    onChangeToolDetail: vi.fn(),
    isEditing: false,
    hasNotSavedCredentials: false,
    isViewToggleVisible: true,
    hideConfigurationNameInput: false,
    showOnlyRequiredFields: false,
    showOnlyConfigurationFields: false,
    showNameFieldForcedly: false,
    showToolkitIcon: false,
    hideNameDescriptionInput: false,
    hideNameInput: false,
    hideOperationButtons: false,
    forceCustomView: false,
    isTeamProject: false,
    projectId: 'proj-1',
    formValues: { type: 'github', settings: {} },
    formInitialValues: { type: 'github', settings: {} },
    onSave: vi.fn(),
    ...overrides,
  };
}

function baseCore(overrides: Partial<CoreState> = {}): CoreState {
  return {
    view: 'form',
    setView: vi.fn(),
    onManualViewChange: vi.fn(),
    showValidation: false,
    setShowValidation: vi.fn(),
    toolErrors: {},
    setToolErrors: vi.fn(),
    serverToolErrors: {},
    setServerToolErrors: vi.fn(),
    configuration: {},
    setConfiguration: vi.fn(),
    configurationErrors: {},
    setConfigurationErrors: vi.fn(),
    configurationName: '',
    setConfigurationName: vi.fn(),
    toolkitSchemas: undefined,
    isFetching: false,
    toolType: 'github',
    effectiveToolSchema: undefined,
    toolListReadFailed: false,
    retryToolListRead: vi.fn(),
    ToolComponent: undefined,
    isValidSchema: true,
    nameIsRequired: false,
    hasErrors: false,
    mergedToolErrors: {},
    editField: vi.fn().mockResolvedValue(undefined),
    ...overrides,
  };
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: BASE });
  server.use(
    http.get(`${BASE}/configurations/configurations/proj-1`, () => HttpResponse.json({ items: [], total: 0, limit: 20, offset: 0 })),
    http.get(`${BASE}/configurations/available/`, () => HttpResponse.json([])),
  );
});

afterEach(() => {
  resetGeneratedClient();
});

describe('useToolkitFormConfiguration', () => {
  it('is loading while core.isFetching is true and no toolkit schemas have loaded yet', () => {
    const { wrapper } = createWrapper();
    const core = baseCore({ isFetching: true, toolkitSchemas: undefined });
    const { result } = renderHook(() => useToolkitFormConfiguration(baseProps(), core), { wrapper });
    expect(result.current.isLoading).toBe(true);
  });

  it('is not loading once toolkit schemas have loaded, even while core.isFetching stays true', () => {
    const { wrapper } = createWrapper();
    const core = baseCore({ isFetching: true, toolkitSchemas: {} });
    const { result } = renderHook(() => useToolkitFormConfiguration(baseProps(), core), { wrapper });
    expect(result.current.isLoading).toBe(false);
  });

  it('is loading when editToolDetail.isLoadingConfigurations is true, overriding an otherwise-not-loading state', () => {
    const { wrapper } = createWrapper();
    const core = baseCore({ isFetching: false, toolkitSchemas: {} });
    const props = baseProps({ editToolDetail: { type: 'github', settings: {}, isLoadingConfigurations: true } });
    const { result } = renderHook(() => useToolkitFormConfiguration(props, core), { wrapper });
    expect(result.current.isLoading).toBe(true);
  });

  it('resolves shouldShowDisabledConfigFields=true when editing a saved toolkit whose type supports configuration and has no configuration title yet', async () => {
    server.use(
      http.get(`${BASE}/configurations/configurations/proj-1`, () => HttpResponse.json({ items: [{ type: 'integration_github' }], total: 1, limit: 20, offset: 0 })),
    );
    const { wrapper } = createWrapper();
    const core = baseCore({ configuration: {} });
    const { result } = renderHook(() => useToolkitFormConfiguration(baseProps({ isEditing: true }), core), { wrapper });
    await waitFor(() => expect(result.current.shouldShowDisabledConfigFields).toBe(true));
  });

  it('resolves shouldShowDisabledConfigFields=false when the integrations list has no matching type', async () => {
    server.use(
      http.get(`${BASE}/configurations/configurations/proj-1`, () => HttpResponse.json({ items: [{ type: 'integration_jira' }], total: 1, limit: 20, offset: 0 })),
    );
    const { wrapper } = createWrapper();
    const core = baseCore({ configuration: {} });
    const { result } = renderHook(() => useToolkitFormConfiguration(baseProps({ isEditing: true }), core), { wrapper });
    await waitFor(() => expect(result.current.shouldShowDisabledConfigFields).toBe(false));
  });

  it('creates a configuration, then mirrors its title onto editToolDetail.settings via editField', async () => {
    server.use(
      http.post(`${BASE}/configurations/configurations/proj-1`, () =>
        HttpResponse.json({ id: '1', type: 'github', title: 'My GitHub Config', project_id: 'proj-1' }),
      ),
    );
    const { wrapper } = createWrapper();
    const editField = vi.fn().mockResolvedValue(undefined);
    const setConfiguration = vi.fn();
    const core = baseCore({ editField, setConfiguration });
    const props = baseProps({ personalProjectId: 'proj-1' });
    const { result } = renderHook(() => useToolkitFormConfiguration(props, core), { wrapper });

    let created: boolean | undefined;
    await act(async () => {
      created = await result.current.onCreateConfiguration();
    });

    expect(created).toBe(true);
    expect(setConfiguration).toHaveBeenCalledWith({ elitea_title: 'My GitHub Config', private: true });
    expect(editField).toHaveBeenCalledWith('settings', expect.objectContaining({ elitea_title: 'My GitHub Config', private: true }));
  });

  it('does not call editField when the created configuration carries no title (settings.title absent)', async () => {
    server.use(
      http.post(`${BASE}/configurations/configurations/proj-1`, () => HttpResponse.json({ id: '2', type: 'github', project_id: 'proj-1' })),
    );
    const { wrapper } = createWrapper();
    const editField = vi.fn().mockResolvedValue(undefined);
    const core = baseCore({ editField });
    const { result } = renderHook(() => useToolkitFormConfiguration(baseProps(), core), { wrapper });

    let created: boolean | undefined;
    await act(async () => {
      created = await result.current.onCreateConfiguration();
    });

    expect(created).toBe(true);
    expect(editField).not.toHaveBeenCalled();
  });

  it('onCreateConfiguration returns false and skips the save step when creation is blocked (existing field error)', async () => {
    const { wrapper } = createWrapper();
    const editField = vi.fn().mockResolvedValue(undefined);
    const core = baseCore({ editField, configurationErrors: { some_field: true } });
    const { result } = renderHook(() => useToolkitFormConfiguration(baseProps(), core), { wrapper });

    let created: boolean | undefined;
    await act(async () => {
      created = await result.current.onCreateConfiguration();
    });

    expect(created).toBe(false);
    expect(editField).not.toHaveBeenCalled();
  });

  it('exposes onTestConnection, passed straight through from useCreateConfiguration', async () => {
    server.use(http.post(`${BASE}/configurations/check_connection/proj-1/github`, () => HttpResponse.json({ tools: ['x'] })));
    const { wrapper } = createWrapper();
    const { result } = renderHook(() => useToolkitFormConfiguration(baseProps(), baseCore()), { wrapper });

    let ok: boolean | undefined;
    await act(async () => {
      ok = await result.current.onTestConnection();
    });
    expect(ok).toBe(true);
  });

  it('onRevertCredentials reverts every changed credential-like settings field and resets the local configuration state', () => {
    const { wrapper } = createWrapper();
    const editField = vi.fn().mockResolvedValue(undefined);
    const setConfiguration = vi.fn();
    const core = baseCore({ editField, setConfiguration });
    const props = baseProps({
      editToolDetail: { type: 'github', settings: { api_key: { elitea_title: 'Changed', private: false } } },
      formInitialValues: { settings: { api_key: { elitea_title: 'Original', private: false }, elitea_title: 'Original Top', private: true } },
    });
    const { result } = renderHook(() => useToolkitFormConfiguration(props, core), { wrapper });

    act(() => {
      result.current.onRevertCredentials();
    });

    expect(editField).toHaveBeenCalledWith('settings.api_key', { elitea_title: 'Original', private: false });
    expect(setConfiguration).toHaveBeenCalledWith({ elitea_title: 'Original Top', private: true });
  });

  it('onRevertCredentials resets to an empty title/undefined private when there are no initial values', () => {
    const { wrapper } = createWrapper();
    const setConfiguration = vi.fn();
    const core = baseCore({ setConfiguration });
    const props = baseProps({ formInitialValues: undefined });
    const { result } = renderHook(() => useToolkitFormConfiguration(props, core), { wrapper });

    act(() => {
      result.current.onRevertCredentials();
    });

    expect(setConfiguration).toHaveBeenCalledWith({ elitea_title: '', private: undefined });
  });

  it('wires an externally-supplied revertCredentialsRef to the current onRevertCredentials callback', () => {
    const { wrapper } = createWrapper();
    const revertCredentialsRef: { current: (() => void) | undefined } = { current: undefined };
    const props = baseProps({ revertCredentialsRef });
    const { result } = renderHook(() => useToolkitFormConfiguration(props, baseCore()), { wrapper });

    expect(revertCredentialsRef.current).toBe(result.current.onRevertCredentials);
  });

  it('resets validation/tool-error/configuration state whenever updateKey changes', () => {
    const { wrapper } = createWrapper();
    const setShowValidation = vi.fn();
    const setToolErrors = vi.fn();
    const setConfigurationErrors = vi.fn();
    const setConfigurationName = vi.fn();
    const setConfiguration = vi.fn();
    const core = baseCore({ setShowValidation, setToolErrors, setConfigurationErrors, setConfigurationName, setConfiguration });
    const { rerender } = renderHook(({ props }: { props: ResolvedToolkitFormProps }) => useToolkitFormConfiguration(props, core), {
      wrapper,
      initialProps: { props: baseProps({ updateKey: 'a' }) },
    });

    expect(setShowValidation).toHaveBeenCalledWith(false);
    expect(setToolErrors).toHaveBeenCalledWith({});
    expect(setConfigurationErrors).toHaveBeenCalledWith({});
    expect(setConfigurationName).toHaveBeenCalledWith('');
    const callsBefore = setShowValidation.mock.calls.length;

    rerender({ props: baseProps({ updateKey: 'a' }) });
    expect(setShowValidation.mock.calls.length).toBe(callsBefore);

    rerender({ props: baseProps({ updateKey: 'b' }) });
    expect(setShowValidation.mock.calls.length).toBe(callsBefore + 1);
  });

  it('seeds the local configuration state from editToolDetail.settings on the resetting effect', () => {
    const { wrapper } = createWrapper();
    const setConfiguration = vi.fn();
    const core = baseCore({ setConfiguration });
    const props = baseProps({
      updateKey: 'seed',
      editToolDetail: { type: 'github', settings: { elitea_title: 'Seeded', private: true } },
    });
    renderHook(() => useToolkitFormConfiguration(props, core), { wrapper });
    expect(setConfiguration).toHaveBeenCalledWith({ elitea_title: 'Seeded', private: true });
  });

  it('onCredentialReload calls toolkitValidation.refetch when notReload is not set', () => {
    const { wrapper } = createWrapper();
    const refetch = vi.fn();
    const props = baseProps({ toolkitValidation: { isError: false, error: undefined, refetch } });
    const { result } = renderHook(() => useToolkitFormConfiguration(props, baseCore()), { wrapper });

    act(() => {
      result.current.onCredentialReload();
    });
    expect(refetch).toHaveBeenCalledTimes(1);
  });

  it('onCredentialReload does nothing when notReload is true and no key is given', () => {
    const { wrapper } = createWrapper();
    const refetch = vi.fn();
    const setServerToolErrors = vi.fn();
    const core = baseCore({ setServerToolErrors });
    const props = baseProps({ toolkitValidation: { isError: false, error: undefined, refetch } });
    const { result } = renderHook(() => useToolkitFormConfiguration(props, core), { wrapper });
    // `setServerToolErrors({})` fires once already, from the mount-time
    // toolkitValidation-not-erroring effect (unrelated to this call) —
    // asserting on the call count delta isolates just this interaction.
    const callsBeforeReload = setServerToolErrors.mock.calls.length;

    act(() => {
      result.current.onCredentialReload({ notReload: true });
    });
    expect(refetch).not.toHaveBeenCalled();
    expect(setServerToolErrors.mock.calls.length).toBe(callsBeforeReload);
  });

  it('onCredentialReload sets the server error for the given key when notReload+key are supplied', () => {
    const { wrapper } = createWrapper();
    const setServerToolErrors = vi.fn();
    const core = baseCore({ setServerToolErrors });
    const { result } = renderHook(() => useToolkitFormConfiguration(baseProps(), core), { wrapper });

    act(() => {
      result.current.onCredentialReload({ notReload: true, key: 'api_key', credentialMessage: 'Bad credential' });
    });
    const updater = setServerToolErrors.mock.calls.at(-1)?.[0] as (prev: Record<string, string | undefined>) => Record<string, string | undefined>;
    expect(updater({ existing: 'kept' })).toEqual({ existing: 'kept', api_key: 'Bad credential' });
  });

  it('onCredentialReload clears the error for the given key when clearValidationError is set', () => {
    const { wrapper } = createWrapper();
    const setServerToolErrors = vi.fn();
    const core = baseCore({ setServerToolErrors });
    const { result } = renderHook(() => useToolkitFormConfiguration(baseProps(), core), { wrapper });

    act(() => {
      result.current.onCredentialReload({ notReload: true, key: 'api_key', clearValidationError: true, credentialMessage: 'ignored' });
    });
    const updater = setServerToolErrors.mock.calls.at(-1)?.[0] as (prev: Record<string, string | undefined>) => Record<string, string | undefined>;
    expect(updater({})).toEqual({ api_key: undefined });
  });

  it('clears serverToolErrors when toolkitValidation is not in an error state', () => {
    const { wrapper } = createWrapper();
    const setServerToolErrors = vi.fn();
    const setShowValidation = vi.fn();
    const core = baseCore({ setServerToolErrors, setShowValidation });
    const props = baseProps({ toolkitValidation: { isError: false, error: undefined, refetch: vi.fn() } });
    renderHook(() => useToolkitFormConfiguration(props, core), { wrapper });
    expect(setServerToolErrors).toHaveBeenCalledWith({});
    expect(setShowValidation).not.toHaveBeenCalledWith(true);
  });

  it('parses settings_errors into serverToolErrors and triggers validation display when toolkitValidation is in an error state', () => {
    const { wrapper } = createWrapper();
    const setServerToolErrors = vi.fn();
    const setShowValidation = vi.fn();
    const core = baseCore({ setServerToolErrors, setShowValidation });
    const props = baseProps({
      toolkitValidation: {
        isError: true,
        error: { data: { settings_errors: [{ msg: 'field is required', loc: ['settings', 'url'] }] } },
        refetch: vi.fn(),
      },
    });
    renderHook(() => useToolkitFormConfiguration(props, core), { wrapper });
    expect(setServerToolErrors).toHaveBeenCalledWith({ url: 'field is required' });
    expect(setShowValidation).toHaveBeenCalledWith(true);
  });

  it('does not set serverToolErrors/showValidation when the error state carries no parseable field errors', () => {
    const { wrapper } = createWrapper();
    const setServerToolErrors = vi.fn();
    const setShowValidation = vi.fn();
    const core = baseCore({ setServerToolErrors, setShowValidation });
    const props = baseProps({
      toolkitValidation: { isError: true, error: { data: { settings_errors: [] } }, refetch: vi.fn() },
    });
    renderHook(() => useToolkitFormConfiguration(props, core), { wrapper });
    expect(setServerToolErrors).not.toHaveBeenCalledWith({});
    expect(setShowValidation).not.toHaveBeenCalledWith(true);
  });

  it('sets the form type field from routeToolkitType when the form has no type set yet', () => {
    const { wrapper } = createWrapper();
    const onSetFormField = vi.fn();
    const props = baseProps({ routeToolkitType: 'jira', formValues: { type: '' }, onSetFormField });
    renderHook(() => useToolkitFormConfiguration(props, baseCore()), { wrapper });
    expect(onSetFormField).toHaveBeenCalledWith('type', 'jira');
  });

  it('does not overwrite an already-set form type field, even when routeToolkitType differs', () => {
    const { wrapper } = createWrapper();
    const onSetFormField = vi.fn();
    const props = baseProps({ routeToolkitType: 'jira', formValues: { type: 'github' }, onSetFormField });
    renderHook(() => useToolkitFormConfiguration(props, baseCore()), { wrapper });
    expect(onSetFormField).not.toHaveBeenCalledWith('type', expect.anything());
  });

  it('does not set the form type field when there is no routeToolkitType', () => {
    const { wrapper } = createWrapper();
    const onSetFormField = vi.fn();
    const props = baseProps({ routeToolkitType: undefined, formValues: { type: '' }, onSetFormField });
    renderHook(() => useToolkitFormConfiguration(props, baseCore()), { wrapper });
    expect(onSetFormField).not.toHaveBeenCalled();
  });

  it('falls back editToolDetailSettings to {} when editToolDetail.settings is entirely absent, on revert', () => {
    const { wrapper } = createWrapper();
    const setConfiguration = vi.fn();
    const editField = vi.fn().mockResolvedValue(undefined);
    const core = baseCore({ setConfiguration, editField });
    const props = baseProps({ editToolDetail: { type: 'github' }, formInitialValues: undefined });
    const { result } = renderHook(() => useToolkitFormConfiguration(props, core), { wrapper });

    expect(() => {
      act(() => {
        result.current.onRevertCredentials();
      });
    }).not.toThrow();
    expect(editField).not.toHaveBeenCalled();
    expect(setConfiguration).toHaveBeenCalledWith({ elitea_title: '', private: undefined });
  });

  it('does not fetch the configurations list, and falls back to an empty projectId, when projectId is undefined', () => {
    let hitConfigurationsList = false;
    server.use(
      http.get(`${BASE}/configurations/configurations/`, () => {
        hitConfigurationsList = true;
        return HttpResponse.json({ items: [], total: 0, limit: 20, offset: 0 });
      }),
    );
    const { wrapper } = createWrapper();
    const props = baseProps({ projectId: undefined });
    const { result } = renderHook(() => useToolkitFormConfiguration(props, baseCore()), { wrapper });
    expect(hitConfigurationsList).toBe(false);
    expect(result.current.shouldShowDisabledConfigFields).toBe(false);
  });

  it('falls back editToolDetail.type to "" for useCreateConfiguration when the toolkit has no type yet', async () => {
    server.use(http.post(`${BASE}/configurations/configurations/proj-1`, () => HttpResponse.json({ id: '3', type: '' })));
    const { wrapper } = createWrapper();
    const props = baseProps({ editToolDetail: {} });
    const { result } = renderHook(() => useToolkitFormConfiguration(props, baseCore()), { wrapper });

    let created: boolean | undefined;
    await act(async () => {
      created = await result.current.onCreateConfiguration();
    });
    expect(created).toBe(true);
  });

  it('forwards getAccessToken/onConfigAuthRequired through to useCreateConfiguration without throwing', () => {
    const { wrapper } = createWrapper();
    const getAccessToken = vi.fn().mockReturnValue('tok');
    const onConfigAuthRequired = vi.fn();
    const props = baseProps({ getAccessToken, onConfigAuthRequired });
    expect(() => renderHook(() => useToolkitFormConfiguration(props, baseCore()), { wrapper })).not.toThrow();
  });

  it('defaults settings_errors to [] when toolkitValidation.error carries no settings_errors key, and does not flip showValidation', () => {
    const { wrapper } = createWrapper();
    const setServerToolErrors = vi.fn();
    const setShowValidation = vi.fn();
    const core = baseCore({ setServerToolErrors, setShowValidation });
    const props = baseProps({
      toolkitValidation: { isError: true, error: { data: {} }, refetch: vi.fn() },
    });
    expect(() => renderHook(() => useToolkitFormConfiguration(props, core), { wrapper })).not.toThrow();
    expect(setShowValidation).not.toHaveBeenCalledWith(true);
  });
});
