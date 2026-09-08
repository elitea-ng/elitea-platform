import { beforeAll, describe, expect, it, vi } from 'vitest';
import { waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';

import { configureGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { renderWithRouterSocketAndProject } from '../../../__tests__/testUtils';

import { ToolBase } from './ToolBase';
import type { ToolBaseProps } from './ToolBase';
import type { EditToolDetail, ToolSchema } from './types';

beforeAll(() => {
  class StubResizeObserver {
    observe(): void {}
    unobserve(): void {}
    disconnect(): void {}
  }
  window.ResizeObserver = StubResizeObserver;
});

// `ToolBase` now renders the real `NameDescriptionInput` by default (R2
// fix) — its hook chain (`useGetCurrentToolkitSchemas`) bottoms out at
// `useSelectedProjectId`/`useSocketClient`, both of which need a real
// `<RouterProvider>`/`SocketClientContext.Provider` ancestor (throw
// otherwise). `renderWithRouterSocketAndProject` (this slice's own shared
// test harness, `NameDescriptionInput.test.tsx`/`ToolCustom.test.tsx`'s own
// harness too) supplies that stack; the plain `QueryClientProvider` +
// `ThemeProvider` harness this file used before R1/R2 is no longer enough.
function renderToolBase(props: ToolBaseProps) {
  return renderWithRouterSocketAndProject(<ToolBase {...props} />, 'proj-1');
}

const SCHEMA: ToolSchema = {
  title: 'jira',
  properties: {
    url: { title: 'URL', type: 'string' },
    label: { title: 'Label', type: 'string' },
  },
  required: ['url'],
};

function detail(overrides: Partial<EditToolDetail> = {}): EditToolDetail {
  return { name: '', description: '', settings: {}, ...overrides };
}

beforeAll(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

function mockPlatformSettings(mcpEnabled: boolean): void {
  server.use(
    http.get('/api/v2/elitea_core/platform_settings/prompt_lib', () =>
      HttpResponse.json({
        chat_enabled: true,
        applications_enabled: true,
        skills_enabled: true,
        toolkits_enabled: true,
        datasources_enabled: true,
        pipelines_enabled: true,
        publishing_enabled: true,
        moderation_enabled: true,
        support_chat_enabled: true,
        mcp_enabled: mcpEnabled,
      }),
    ),
  );
  server.use(http.get('/api/v2/elitea_core/toolkits/prompt_lib/:projectId', () => HttpResponse.json({})));
}

describe('ToolBase', () => {
  it('renders the schema\'s fields inside a Configuration accordion by default', async () => {
    mockPlatformSettings(true);
    const { getByText } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema: SCHEMA,
    });
    await waitFor(() => expect(getByText('Configuration')).toBeInTheDocument());
    expect(getByText('URL')).toBeInTheDocument();
    expect(getByText('Label')).toBeInTheDocument();
  });

  it('renders flat (no accordion) when shouldUseAccordionView is false', async () => {
    mockPlatformSettings(true);
    const { queryByText, getByText } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema: SCHEMA,
      shouldUseAccordionView: false,
    });
    await waitFor(() => expect(getByText('URL')).toBeInTheDocument());
    expect(queryByText('Configuration')).not.toBeInTheDocument();
  });

  it('renders the tools chip picker for the selected_tools schema property', async () => {
    mockPlatformSettings(true);
    const schema: ToolSchema = {
      ...SCHEMA,
      properties: { ...SCHEMA.properties, selected_tools: { items: { enum: ['read_issue', 'create_issue'] } } },
    };
    const { getByText } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema,
    });
    await waitFor(() => expect(getByText('Read issue')).toBeInTheDocument());
    expect(getByText('Create issue')).toBeInTheDocument();
  });

  it.each([['echo_marker'], []])('prefers explicit MCP discovery over stale catalogue names: %j', async (...names) => {
    mockPlatformSettings(true);
    const schema: ToolSchema = {
      title: 'mcp',
      properties: { selected_tools: { items: { enum: ['stale_tool'] } } },
    };
    const { queryByText, getByText } = renderToolBase({
      editToolDetail: detail({ type: 'mcp', settings: { available_mcp_tools: names, selected_tools: [] } }),
      setEditToolDetail: vi.fn(), editField: vi.fn(), toolErrors: {},
      showValidation: false, setToolErrors: vi.fn(), schema,
    });
    await waitFor(() => expect(getByText('Tools')).toBeInTheDocument());
    expect(queryByText('Stale tool')).not.toBeInTheDocument();
    expect(queryByText('Echo marker') !== null).toBe(names.length > 0);
  });

  it('does not render the tools picker when showTools is false', async () => {
    mockPlatformSettings(true);
    const schema: ToolSchema = {
      ...SCHEMA,
      properties: { ...SCHEMA.properties, selected_tools: { items: { enum: ['read_issue'] } } },
    };
    const { getByText, queryByText } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema,
      showTools: false,
    });
    await waitFor(() => expect(getByText('URL')).toBeInTheDocument());
    expect(queryByText('Read issue')).not.toBeInTheDocument();
  });

  it('renders the caller-supplied mcpAuthStatus slot for an mcp-titled schema', async () => {
    mockPlatformSettings(true);
    const { getByText } = renderToolBase({
      editToolDetail: detail({ type: 'mcp' }),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema: { title: 'mcp', properties: {} },
      slots: { mcpAuthStatus: <div>mcp auth status</div> },
    });
    await waitFor(() => expect(getByText('mcp auth status')).toBeInTheDocument());
  });

  it('does not render the mcpAuthStatus slot for a non-mcp schema', async () => {
    mockPlatformSettings(true);
    const { getByText, queryByText } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema: SCHEMA,
      slots: { mcpAuthStatus: <div>mcp auth status</div> },
    });
    await waitFor(() => expect(getByText('URL')).toBeInTheDocument());
    expect(queryByText('mcp auth status')).not.toBeInTheDocument();
  });

  it('renders the sharepointOAuthStatus slot for a sharepoint-titled schema', async () => {
    mockPlatformSettings(true);
    const { getByText } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema: { title: 'sharepoint', properties: {} },
      slots: { sharepointOAuthStatus: <div>sharepoint auth status</div> },
    });
    await waitFor(() => expect(getByText('sharepoint auth status')).toBeInTheDocument());
  });

  it('invokes the renderNameDescriptionInput slot with the resolved name/description context', async () => {
    mockPlatformSettings(true);
    const renderNameDescriptionInput = vi.fn(() => <div>name/description widget</div>);
    const { getByText } = renderToolBase({
      editToolDetail: detail({ name: 'My Tool', description: 'A tool' }),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema: SCHEMA,
      slots: { renderNameDescriptionInput },
    });
    await waitFor(() => expect(getByText('name/description widget')).toBeInTheDocument());
    expect(renderNameDescriptionInput).toHaveBeenCalledWith(expect.objectContaining({ name: 'My Tool', description: 'A tool' }));
  });

  it('does not render the name/description slot when hideNameDescriptionInput is set', async () => {
    mockPlatformSettings(true);
    const renderNameDescriptionInput = vi.fn(() => <div>name/description widget</div>);
    const { getByText, queryByText } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema: SCHEMA,
      hideNameDescriptionInput: true,
      slots: { renderNameDescriptionInput },
    });
    await waitFor(() => expect(getByText('URL')).toBeInTheDocument());
    expect(queryByText('name/description widget')).not.toBeInTheDocument();
    expect(renderNameDescriptionInput).not.toHaveBeenCalled();
  });

  // R2 regression guard: `resolveNameDescriptionSlot` (`ToolBase.render.tsx`)
  // used to unconditionally return `null` unless a `slots.
  // renderNameDescriptionInput` render-prop was supplied — the live
  // composition root (`ToolkitForm.hooks.ts`) never supplies one, so this
  // was silently blank in production. On the pre-fix code this test's
  // `getByLabelText` calls below throw (no such element); after the fix,
  // the real `../NameDescriptionInput.tsx` renders by default.
  it('renders the real NameDescriptionInput fields by default when no slot is supplied', async () => {
    mockPlatformSettings(true);
    const { getByLabelText } = renderToolBase({
      editToolDetail: detail({ name: 'My Jira Tool', description: 'A jira tool' }),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema: SCHEMA,
    });
    await waitFor(() => expect(getByLabelText('Toolkit Name', { exact: false })).toHaveValue('My Jira Tool'));
    expect(getByLabelText('Description')).toHaveValue('A jira tool');
  });

  it('renders priority fields before the rest of the schema properties', async () => {
    mockPlatformSettings(true);
    const { container, getByText } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema: SCHEMA,
      priorityFieldsOrder: ['label'],
    });
    await waitFor(() => expect(getByText('URL')).toBeInTheDocument());
    const labels = Array.from(container.querySelectorAll('label')).map((el) => el.textContent ?? '');
    const labelIndex = labels.findIndex((text) => text.includes('Label'));
    const urlIndex = labels.findIndex((text) => text.includes('URL'));
    expect(labelIndex).toBeGreaterThanOrEqual(0);
    expect(labelIndex).toBeLessThan(urlIndex);
  });

  it('renders advanced fields inside an "Advanced Settings" accordion, and excludes them from the main pass', async () => {
    mockPlatformSettings(true);
    const { getByText, getAllByText } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema: SCHEMA,
      advancedFields: ['label'],
    });
    await waitFor(() => expect(getByText('Advanced Settings')).toBeInTheDocument());
    // Rendered exactly once — inside the advanced-fields accordion, not
    // duplicated into the main field pass too (`isMainPassField` excludes it).
    expect(getAllByText('Label')).toHaveLength(1);
  });

  it('renders fieldNeedToRenderAtBottom fields after the rest of the schema properties', async () => {
    mockPlatformSettings(true);
    const { container, getByText } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema: SCHEMA,
      fieldNeedToRenderAtBottom: ['url'],
    });
    await waitFor(() => expect(getByText('URL')).toBeInTheDocument());
    const labels = Array.from(container.querySelectorAll('label')).map((el) => el.textContent ?? '');
    const labelIndex = labels.findIndex((text) => text.includes('Label'));
    const urlIndex = labels.findIndex((text) => text.includes('URL'));
    expect(urlIndex).toBeGreaterThanOrEqual(0);
    expect(labelIndex).toBeLessThan(urlIndex);
  });

  it('renders a metadata section (ToolSection) when showSections is set with a matching sectionProps entry', async () => {
    mockPlatformSettings(true);
    const schema: ToolSchema = {
      ...SCHEMA,
      properties: { ...SCHEMA.properties, client_id: { title: 'Client ID', type: 'string' } },
    };
    const { getByText, getByRole } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema,
      showSections: true,
      sections: {
        sections: { auth: { required: true, subsections: [{ name: 'OAuth', fields: ['client_id'] }] } },
        sectionProps: ['client_id'],
      },
    });
    await waitFor(() => expect(getByText('Auth')).toBeInTheDocument());
    expect(getByRole('radio', { name: 'OAuth' })).toBeInTheDocument();
  });

  it('commits a selected-tool toggle via editField (the tools chip picker)', async () => {
    mockPlatformSettings(true);
    const editField = vi.fn();
    const schema: ToolSchema = {
      ...SCHEMA,
      properties: { ...SCHEMA.properties, selected_tools: { items: { enum: ['read_issue'] } } },
    };
    const user = userEvent.setup();
    const { getByText } = renderToolBase({
      editToolDetail: detail(),
      setEditToolDetail: vi.fn(),
      editField,
      toolErrors: {},
      showValidation: false,
      setToolErrors: vi.fn(),
      schema,
    });
    await waitFor(() => expect(getByText('Read issue')).toBeInTheDocument());
    await user.click(getByText('Read issue'));
    expect(editField).toHaveBeenCalledWith('settings.selected_tools', ['read_issue']);
  });

  // R1 regression guard. The live composition root
  // (`ToolkitForm/ToolkitForm.hooks.ts`'s `toolComponentProps`, not owned by
  // this cluster) spreads a FLAT prop bag — no `toolDetail`/`formState`
  // grouping — onto `ToolBase` for any typed (non-custom) toolkit schema.
  // On the pre-fix code (`ToolBaseProps` required a grouped `{toolDetail:
  // {value, onChange}, formState, ...}` shape) this object literal wouldn't
  // even have type-checked, and forcing it through at runtime (mirroring
  // the real untyped `Record<string, unknown>` spread the composition root
  // actually performs) threw a `TypeError` on `ToolBase`'s very first line
  // (`toolDetail.value`) — the whole page crashed instead of rendering.
  it('renders without crashing when given the exact flat prop shape the live ToolkitForm composition root produces', async () => {
    mockPlatformSettings(true);
    // Deliberately built as an untyped bag (not object-literal-checked
    // against `ToolBaseProps`) — `ToolkitForm.hooks.ts`'s real
    // `toolComponentProps` is `Record<string, unknown>` too, and carries
    // several extra keys (`configurationErrors`, `configuration`,
    // `onCreateConfiguration`, ...) that `ToolBase` never reads.
    const toolComponentProps: Record<string, unknown> = {
      editToolDetail: detail({ name: 'My Jira Tool', description: '' }),
      setEditToolDetail: vi.fn(),
      editField: vi.fn(),
      toolErrors: {},
      setToolErrors: vi.fn(),
      showValidation: false,
      configurationErrors: {},
      setConfigurationErrors: vi.fn(),
      configurationName: '',
      setConfigurationName: vi.fn(),
      configuration: undefined,
      setConfiguration: vi.fn(),
      schema: SCHEMA,
      configurationSchema: undefined,
      hideConfigurationNameInput: false,
      showOnlyRequiredFields: false,
      showOnlyConfigurationFields: false,
      showNameFieldForcedly: false,
      showToolkitIcon: false,
      hideNameDescriptionInput: false,
      hideNameInput: false,
      disabledConfigFieldsForOldToolkits: false,
      shouldInitRequiredFields: false,
      isMCP: false,
      needToCheckSection: false,
      disabled: false,
      onSyntaxError: vi.fn(),
      excludedFields: [],
      onCredentialReload: vi.fn(),
      onCreateConfiguration: vi.fn(),
      onTestConnection: vi.fn(),
    };
    const { getByText, getByLabelText } = renderToolBase(toolComponentProps);
    await waitFor(() => expect(getByText('URL')).toBeInTheDocument());
    expect(getByText('Label')).toBeInTheDocument();
    expect(getByLabelText('Toolkit Name', { exact: false })).toHaveValue('My Jira Tool');
  });
});
