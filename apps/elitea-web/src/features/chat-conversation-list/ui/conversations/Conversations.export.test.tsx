/**
 * ISSUE 851, measured at the COMPOSITION ROOT.
 *
 * Both halves of this feature were separately correct and joined to nothing:
 * `buildMoveAndExportItems` built an Export entry, `ConversationItem` accepted
 * an `onExport`, and no caller anywhere in the app ever passed one — so the
 * entry was rendered `disabled: true` with two children labelled `Option1`/
 * `Option2`, in every build there has ever been. A unit test of either half
 * would have passed the whole time.
 *
 * So this file mounts the real `Conversations`, opens the real row menu, and
 * clicks the real entry. What it asserts is the REQUEST on the wire: the
 * export route, with the format the clicked entry named. Nothing short of the
 * whole chain — composition root -> render-prop factory -> row -> menu
 * builder -> entity fetcher -> download helper — can satisfy that.
 */
import type { ReactElement } from 'react';

import { QueryClient, QueryClientProvider } from '@tanstack/react-query';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { http, HttpResponse } from 'msw';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';
import type { MockInstance } from 'vitest';

import type { Conversation } from '@/entities/conversation';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { server } from '@/test/setup';

import { Conversations, type ConversationsProps } from './Conversations';

/** `FolderItem`/`TypographyWithConditionalTooltip` need a real one; jsdom has none. */
class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

const CONVERSATION: Conversation = { id: 'conv-7', name: 'autotest_export_row', isPrivate: true, authorId: 'user-1' };

function renderConversations(overrides: Partial<ConversationsProps> = {}): ReturnType<typeof renderWithTheme> {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false, gcTime: 0 } } });
  const props: ConversationsProps = {
    conversations: [CONVERSATION],
    pinnedConversations: [CONVERSATION],
    dateGroups: [],
    setDateGroups: vi.fn(),
    ungroupedConversationsCount: 0,
    totalConversationsAmount: 1,
    onSelectConversation: vi.fn(),
    onCollapsed: vi.fn(),
    onEditConversation: vi.fn(),
    onPlaybackConversation: vi.fn(),
    onDeleteConversation: vi.fn(),
    onPinConversation: vi.fn(),
    onCreateConversation: vi.fn(),
    onCancelCreateConversation: vi.fn(),
    onChangeActiveConversationName: vi.fn(),
    onCreateFolder: vi.fn(),
    onCancelCreateFolder: vi.fn(),
    folders: [],
    setFolders: vi.fn(),
    onDeleteFolder: vi.fn(),
    onEditFolder: vi.fn(),
    onPinFolder: vi.fn(),
    onMoveToFolderConversation: vi.fn(),
    onMoveToNewFolderConversation: vi.fn(),
    moveTargetConversationToNewFolder: vi.fn(),
    cancelMovingTargetConversationToNewFolder: vi.fn(),
    onClickCreateNewFolder: vi.fn(),
    toastError: vi.fn(),
    projectId: 'p1',
    currentUserId: 'user-1',
    ...overrides,
  };
  return renderWithTheme(
    (
      <QueryClientProvider client={queryClient}>
        <Conversations {...props} />
      </QueryClientProvider>
    ) as ReactElement,
  );
}

/**
 * Opens the row's ⋮ menu, then its Export submenu.
 *
 * The hover is part of the gesture, not decoration: the menu trigger is
 * `display: none` until the row is hovered (`ConversationItem.styles.ts`'s
 * `menuWrapper`), so without it the click fails on a missing element and reads
 * like a missing control.
 */
async function openExportSubmenu(user: ReturnType<typeof userEvent.setup>): Promise<void> {
  await user.hover(await screen.findByText(CONVERSATION.name));
  await user.click(await screen.findByRole('button', { name: /more actions/i }));
  await user.click(await screen.findByRole('menuitem', { name: 'Export' }));
}

/** The save-as gesture, spied so a test can say whether a file really reached the browser. Held in a variable rather than read back off the prototype, which oxlint's `unbound-method` rightly refuses. */
let anchorClick: MockInstance<() => void>;

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
  vi.stubGlobal('ResizeObserver', ResizeObserverStub);
  Object.defineProperty(window, 'innerWidth', { writable: true, configurable: true, value: 1440 });
  vi.spyOn(URL, 'createObjectURL').mockReturnValue('blob:mock-url');
  vi.spyOn(URL, 'revokeObjectURL').mockImplementation(() => undefined);
  anchorClick = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => undefined);
  server.use(
    http.get('*/auth/permissions/prompt_lib/:projectId', () => HttpResponse.json([], { status: 200 })),
    http.get('*/social/author', () => HttpResponse.json({ data: { id: 'user-1', name: 'User One' } }, { status: 200 })),
  );
});

afterEach(() => {
  resetGeneratedClient();
  vi.unstubAllGlobals();
  vi.restoreAllMocks();
});

describe('Conversations — the row menu really exports (issue 851)', () => {
  it('asks the export route for Markdown and saves it under the server’s filename', async () => {
    const user = userEvent.setup();
    const requested: string[] = [];
    server.use(
      http.get('*/elitea_core/conversation_export/prompt_lib/:projectId/:conversationId', ({ request }) => {
        requested.push(request.url);
        return new HttpResponse('# autotest_export_row\n', {
          status: 200,
          headers: { 'Content-Type': 'text/markdown', 'Content-Disposition': 'attachment; filename="autotest_export_row.md"' },
        });
      }),
    );

    renderConversations();
    await openExportSubmenu(user);
    await user.click(await screen.findByRole('menuitem', { name: 'Markdown (.md)' }));

    await waitFor(() => expect(requested).toHaveLength(1));
    const url = new URL(requested[0] ?? '', window.location.origin);
    expect(url.pathname).toBe('/api/v2/elitea_core/conversation_export/prompt_lib/p1/conv-7');
    expect(url.searchParams.get('format')).toBe('md');
    // The browser was really handed a file, not just a 200.
    expect(anchorClick).toHaveBeenCalledTimes(1);
  });

  it('asks for JSON when the JSON entry is the one clicked', async () => {
    const user = userEvent.setup();
    const formats: (string | null)[] = [];
    server.use(
      http.get('*/elitea_core/conversation_export/prompt_lib/:projectId/:conversationId', ({ request }) => {
        formats.push(new URL(request.url).searchParams.get('format'));
        return new HttpResponse('{}', { status: 200, headers: { 'Content-Type': 'application/json' } });
      }),
    );

    renderConversations();
    await openExportSubmenu(user);
    await user.click(await screen.findByRole('menuitem', { name: 'JSON (.json)' }));

    await waitFor(() => expect(formats).toEqual(['json']));
  });

  // The refusal must reach the person who clicked. `downloadFromApi` resolves
  // its failure rather than throwing, so a caller that ignored the result
  // would show nothing at all and leave the user waiting for a file.
  it('reports the server’s own reason when the export is refused', async () => {
    const user = userEvent.setup();
    const toastError = vi.fn();
    server.use(
      http.get('*/elitea_core/conversation_export/prompt_lib/:projectId/:conversationId', () =>
        HttpResponse.json({ error: 'this conversation has more than 10000 messages and cannot be exported in one document' }, { status: 400 }),
      ),
    );

    renderConversations({ toastError });
    await openExportSubmenu(user);
    await user.click(await screen.findByRole('menuitem', { name: 'Markdown (.md)' }));

    await waitFor(() =>
      expect(toastError).toHaveBeenCalledWith('this conversation has more than 10000 messages and cannot be exported in one document'),
    );
    expect(anchorClick).not.toHaveBeenCalled();
  });
});
