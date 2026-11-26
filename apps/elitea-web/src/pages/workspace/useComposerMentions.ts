/**
 * The workspace composer's "@" menu (ADR-0061 decision 2b, #3056): the thread's agents first (persona name, Local or
 * Cloud), then the folder's files and folders. Picking an agent inserts `@Name ` — the text Main routes on
 * (`addressedTo`), exactly what typing it does; picking a file is unchanged.
 *
 * The agents come from the kept thread read the send uses (`threadFloorQuery`), so the menu, the To chip and the send
 * agree on who is in the thread. `requestAddress` lets the Participants tab's "Address" put `@Name ` into the
 * composer of the same thread.
 */
import { createElement, useEffect, useMemo, useRef, type ReactNode, type RefObject } from 'react';

import FolderOutlinedIcon from '@mui/icons-material/FolderOutlined';
import InsertDriveFileOutlinedIcon from '@mui/icons-material/InsertDriveFileOutlined';
import SmartToyOutlinedIcon from '@mui/icons-material/SmartToyOutlined';
import { useQuery } from '@tanstack/react-query';
import { create, type StoreApi, type UseBoundStore } from 'zustand';

import type { SuggestionItem } from '@/features/workspace';
import type { WorkspaceIpc } from '@/shared/desktop/workspaceIpc';
import { t } from '@/shared/i18n';

import type { ThreadAgent } from './useEnsureConversation';
import { threadFloorQuery, type ThreadFloor } from './useSendPrompt';

const FILE_LIMIT = 50;
/** At most this many agents lead the menu. */
const AGENT_LIMIT = 8;

type FileEntry = Awaited<ReturnType<WorkspaceIpc['files']>>[number];

/** The thread's agents and meta (the kept read); empty before its first turn. */
export function useThreadFloor(projectId: number, conversationId: string): ThreadFloor | undefined {
  const floor = useQuery({ ...threadFloorQuery(projectId, conversationId), enabled: conversationId !== '' });
  return conversationId === '' ? undefined : floor.data;
}

/** The thread's agents an "@" query matches: a name that starts with it, then one that contains it (case-insensitive). */
export function matchingAgents(query: string, agents: readonly ThreadAgent[]): ThreadAgent[] {
  const named = agents.filter((agent) => agent.name !== '');
  const needle = query.toLowerCase();
  if (needle === '') return named.slice(0, AGENT_LIMIT);
  const starts = named.filter((agent) => agent.name.toLowerCase().startsWith(needle));
  const contains = named.filter((agent) => !starts.includes(agent) && agent.name.toLowerCase().includes(needle));
  return [...starts, ...contains].slice(0, AGENT_LIMIT);
}

/** The text an agent pick inserts. */
export function agentToken(agent: Pick<ThreadAgent, 'name'>): string {
  return `@${agent.name} `;
}

export interface MentionMenu {
  readonly agents: readonly ThreadAgent[];
  readonly files: readonly FileEntry[];
}

/** The menu rows of `menu`: its agents (with where each runs), then its files and folders. */
export function mentionItems(menu: MentionMenu): SuggestionItem[] {
  const agents: SuggestionItem[] = menu.agents.map((agent) => ({
    key: `agent:${String(agent.participantId)}`,
    label: `@${agent.name}`,
    icon: createElement(SmartToyOutlinedIcon, { fontSize: 'small' }),
    section: t('workspace.composer.agents', 'Agents in this thread'),
    badges: [{ label: agent.cloud ? t('workspace.participants.cloud', 'Cloud') : t('workspace.participants.local', 'Local') }],
  }));
  const files: SuggestionItem[] = menu.files.map((f) => ({
    key: `${f.kind}:${f.path}`,
    label: f.kind === 'dir' ? `${f.path}/` : f.path,
    icon: createElement(f.kind === 'dir' ? FolderOutlinedIcon : InsertDriveFileOutlinedIcon, { fontSize: 'small' }),
    ...(agents.length > 0 ? { section: t('workspace.composer.files', 'Files in this folder') } : {}),
  }));
  return [...agents, ...files];
}

/** The "@" menu's rows for an open "@" token (`query`; `null`: no "@" token is open). */
export function useMentionMenu(ipc: WorkspaceIpc, workspaceId: string, token: { readonly kind: string; readonly query: string } | null, agents: readonly ThreadAgent[] | undefined): MentionMenu {
  const query = token?.kind === 'file' ? token.query : null;
  const files = useQuery({
    queryKey: ['workspace', 'files', workspaceId, query ?? ''],
    queryFn: () => ipc.files(workspaceId, query ?? '', FILE_LIMIT),
    enabled: query !== null,
    staleTime: 5_000,
    placeholderData: (previous) => previous,
  });
  // Only a thread of two or more agents addresses one: with one, it answers anyway.
  const shown = useMemo(() => (query === null || (agents?.length ?? 0) < 2 ? [] : matchingAgents(query, agents ?? [])), [query, agents]);
  return { agents: shown, files: query === null ? [] : (files.data ?? []) };
}

interface AddressRequests {
  /** The latest "Address" per thread: the name and a counter, so the same name twice inserts twice. */
  readonly byThread: Readonly<Record<string, { readonly name: string; readonly seq: number }>>;
  readonly request: (threadKey: string, name: string) => void;
}

/** @public A fresh store; the composer reads the lazily built one below (built on first use, not at import). */
export function createAddressStore(): UseBoundStore<StoreApi<AddressRequests>> {
  return create<AddressRequests>()((set) => ({
    byThread: {},
    request: (threadKey, name) => set((state) => ({ byThread: { ...state.byThread, [threadKey]: { name, seq: (state.byThread[threadKey]?.seq ?? 0) + 1 } } })),
  }));
}

let instance: UseBoundStore<StoreApi<AddressRequests>> | undefined;

function addressStore(): UseBoundStore<StoreApi<AddressRequests>> {
  instance ??= createAddressStore();
  return instance;
}

function useAddressStore<T>(selector: (state: AddressRequests) => T): T {
  return addressStore()(selector);
}

/** The To chip's place above the input (`render`, with the text being typed); nothing without one. */
export function ToSlot({ render, draft }: { readonly render: ((draft: string) => ReactNode) | undefined; readonly draft: string }): ReactNode {
  return render === undefined ? null : render(draft);
}

/** Puts `@Name ` into the composer of the thread `threadKey` (the Participants tab's "Address"). */
export function requestAddress(threadKey: string, name: string): void {
  if (threadKey !== '' && name !== '') addressStore().getState().request(threadKey, name);
}

interface InsertHandle {
  readonly getInputContent: () => string;
  readonly replaceRange: (start: number, end: number, text: string) => void;
}

/** Appends each "Address" request of `threadKey` to the composer (`input`), after a space when the text has none. */
export function useAddressRequests(threadKey: string | undefined, input: RefObject<InsertHandle | null>): void {
  const request = useAddressStore((state) => (threadKey === undefined || threadKey === '' ? undefined : state.byThread[threadKey]));
  // A request made before this composer showed the thread (another thread view, a remount) is not replayed: the count
  // of requests seen is kept per thread, and starts again from the thread's latest when the composer shows another.
  const done = useRef({ threadKey, seq: request?.seq ?? 0 });
  useEffect(() => {
    if (done.current.threadKey !== threadKey) {
      done.current = { threadKey, seq: request?.seq ?? 0 };
      return;
    }
    const handle = input.current;
    if (request === undefined || handle === null || request.seq <= done.current.seq) return;
    done.current = { threadKey, seq: request.seq };
    const content = handle.getInputContent();
    const gap = content === '' || /\s$/.test(content) ? '' : ' ';
    handle.replaceRange(content.length, content.length, `${gap}${agentToken(request)}`);
  }, [request, input, threadKey]);
}
