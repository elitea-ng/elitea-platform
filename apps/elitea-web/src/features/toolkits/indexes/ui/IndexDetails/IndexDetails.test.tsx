import { ThemeProvider } from '@mui/material/styles';
import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { RouterProvider, createMemoryHistory, createRootRoute, createRouter } from '@tanstack/react-router';
import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { beforeEach, describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import { useIndexesStore } from '../../model/indexesStore';
import type { IndexRow } from '../../model/indexesStore';

import { IndexDetails } from './IndexDetails';
import type { IndexDetailsProps, UseToolkitChatParams, UseToolkitChatResult } from './IndexDetails';
import type { ChatMessageListProps, LLMModelSelectorProps } from './IndexChat';
import type { ToolFormFieldProps } from './IndexConfig';
import type { UseToolkitSchemasResult } from './IndexActions';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function FakeToolFormField(props: ToolFormFieldProps) {
  return <div>field:{props.fieldKey}</div>;
}
function FakeModelSelector(_props: LLMModelSelectorProps) {
  return <div>model-selector</div>;
}
function FakeChatMessageList(props: ChatMessageListProps) {
  return <div>messages:{props.chat_history.length}</div>;
}
function FakeClearChatButton(props: { onClear: () => void }) {
  return (
    <button
      type="button"
      onClick={props.onClear}
    >
      Clear
    </button>
  );
}

const noSchemas: UseToolkitSchemasResult = { toolkitSchemas: {}, isFetching: false };

const toolkitChatResult: UseToolkitChatResult = {
  activeConversation: null,
  chatHistory: [],
  isIndexing: false,
  isFullScreenChat: false,
  isRunning: false,
  isStoppingIndexing: false,
  handleClearActiveConversation: vi.fn(),
  handleClearChat: vi.fn(),
  handleIndexData: vi.fn(),
  handleRunTool: vi.fn(),
  llmSettings: {},
  modelList: [],
  onCancelIndexing: vi.fn(),
  onSelectModel: vi.fn(),
  onSetLLMSettings: vi.fn(),
  selectedModel: null,
  stopRunOnIndexChange: vi.fn(),
  toggleFullScreenChat: vi.fn(),
};

const capturedToolkitChatParams: UseToolkitChatParams[] = [];

function fakeUseToolkitChat(params: UseToolkitChatParams): UseToolkitChatResult {
  capturedToolkitChatParams.push(params);
  return toolkitChatResult;
}

const baseIndex: IndexRow = { id: 'idx-1', metadata: { collection: 'my-index', state: 'completed' } };

const baseProps: Omit<IndexDetailsProps, 'useToolkitChat'> = {
  index: baseIndex,
  view: 'edit',
  traceNewIndex: vi.fn(),
  refetchIndexesList: vi.fn(),
  handleDeleteIndex: vi.fn(),
  selectedIndexTools: ['index_data', 'search_index', 'remove_index'],
  toolkitId: 'tk-1',
  values: { type: 'github', id: 'tk-1' },
  useSelectedToolSchema: () => ({ toolSchema: null, isError: false, refetch: () => {} }),
  useToolkitSchemas: () => noSchemas,
  ToolFormField: FakeToolFormField,
  LLMModelSelector: FakeModelSelector,
  ChatMessageList: FakeChatMessageList,
  ClearChatButton: FakeClearChatButton,
};

function renderDetails(overrides: Partial<IndexDetailsProps> = {}) {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  const rootRoute = createRootRoute({
    component: () => (
      <QueryClientProvider client={queryClient}>
        <ThemeProvider
          theme={theme}
          defaultMode={DEFAULT_COLOR_SCHEME}
        >
          <IndexDetails
            {...baseProps}
            useToolkitChat={fakeUseToolkitChat}
            {...overrides}
          />
        </ThemeProvider>
      </QueryClientProvider>
    ),
  });
  const router = createRouter({
    routeTree: rootRoute,
    history: createMemoryHistory({ initialEntries: ['/'] }),
    context: { auth: { getSelectedProjectId: () => 'proj-1' } },
  });
  return render(<RouterProvider router={router} />);
}

beforeEach(() => {
  useIndexesStore.setState({ tempIndexes: [], indexPatches: {}, toolkitScheduler: {}, selectedHistoryItem: null });
  capturedToolkitChatParams.length = 0;
});

describe('IndexDetails', () => {
  it('renders the index name, actions, tab toggler, and chat panel', async () => {
    renderDetails();
    expect(await screen.findByText('my-index')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Reindex' })).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'run' })).toBeInTheDocument();
    expect(screen.getByText('model-selector')).toBeInTheDocument();
  });

  it('create view renders IndexConfig directly, with no tab toggler', async () => {
    renderDetails({ view: 'create', index: { id: 'new_index', metadata: { collection: 'New Index', state: '' } } });
    await screen.findByText('New Index');
    expect(screen.queryByRole('button', { name: 'run' })).not.toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Index' })).toBeInTheDocument();
  });

  it('passes modes:[create_index] to useToolkitChat only in the create view', async () => {
    renderDetails({ view: 'create', index: { id: 'new_index', metadata: { collection: 'New Index', state: '' } } });
    await screen.findByText('New Index');
    expect(capturedToolkitChatParams.at(-1)?.modes).toEqual(['create_index']);
  });

  it('passes modes:[] to useToolkitChat in the edit view', async () => {
    renderDetails({ view: 'edit' });
    await screen.findByText('my-index');
    expect(capturedToolkitChatParams.at(-1)?.modes).toEqual([]);
  });

  it('disables the run tab and defaults to configuration when the index state is not runnable', async () => {
    renderDetails({ index: { id: 'idx-1', metadata: { collection: 'my-index', state: 'failed' } } });
    await screen.findByText('my-index');
    expect(screen.getByRole('button', { name: 'run' })).toBeDisabled();
  });

  it('renders the caller-supplied mcpAuthModal slot', async () => {
    renderDetails({ mcpAuthModal: <div>mcp-auth-modal-slot</div> });
    expect(await screen.findByText('mcp-auth-modal-slot')).toBeInTheDocument();
  });

  it('switching to the History tab renders the history panel (no matching config field shown)', async () => {
    const user = userEvent.setup();
    renderDetails({
      index: {
        id: 'idx-1',
        metadata: { collection: 'my-index', state: 'completed', history: [{ state: 'completed', updated_on: 1 }] },
      },
    });
    await screen.findByText('my-index');
    await user.click(screen.getByRole('button', { name: 'history' }));
    expect(await screen.findByText('Reindexed')).toBeInTheDocument();
  });
});
