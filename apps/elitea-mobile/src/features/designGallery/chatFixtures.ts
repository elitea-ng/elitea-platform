import type { ChatMessage } from '@/features/chat/messageModel';
import type { ToolStep, TurnPause } from '@/features/chat/toolActivity';
import { INITIAL_TURN } from '@/features/chat/turnReducer';
import type { LiveTurn } from '@/features/chat/turnController';
import type { TrayItem } from '@/features/chat/attachments/tray';

/**
 * Sample conversation for the chat preview (`/design-chat`, development builds only). Never sent
 * anywhere; only the fields the real rows show.
 */
export const SAMPLE_AGENT = 'Research';

const minutesAgo = (m: number) => Date.now() - m * 60_000;

const message = (over: Partial<ChatMessage> & Pick<ChatMessage, 'key' | 'role' | 'text'>): ChatMessage => ({
  groupId: null,
  uuid: null,
  authorParticipantId: over.role === 'assistant' ? 1 : null,
  mine: over.role === 'user',
  author: over.role === 'assistant' ? SAMPLE_AGENT : null,
  createdAt: minutesAgo(5),
  isStreaming: false,
  hasUnsupportedItems: false,
  attachments: [],
  error: null,
  replyToGroupId: null,
  generation: null,
  ...over,
});

export const SAMPLE_TOOLS: ToolStep[] = [
  tool('t1', 'search_issues', 'GitHub', 'done', 0, 900),
  tool('t2', 'read_file', 'GitHub', 'done', 1000, 2400),
];

function tool(id: string, name: string, toolkit: string, status: ToolStep['status'], start: number, end: number | null): ToolStep {
  const base = minutesAgo(4);
  return {
    id,
    name,
    displayName: toolkit,
    toolkit,
    parentAgent: null,
    status,
    startedAt: base + start,
    endedAt: end === null ? null : base + end,
    inputs: '{\n  "query": "launch"\n}',
    inputsTruncated: false,
    output: '{\n  "items": 2\n}',
    outputTruncated: false,
    partial: false,
    error: null,
    chunks: null,
  } as ToolStep;
}

/** Newest last (the screen reverses for its inverted list). */
export const SAMPLE_MESSAGES: ChatMessage[] = [
  message({ key: 'm1', role: 'user', text: 'Can you summarise the launch plan and flag anything risky?', createdAt: minutesAgo(6) }),
  message({
    key: 'm2',
    role: 'assistant',
    uuid: '00000000-0000-4000-8000-000000000002',
    groupId: 2,
    text:
      'Here is the **launch plan** in short:\n\n1. Beta in week 1\n2. Store review in week 2\n3. Public release in week 3\n\n' +
      'The risky part is the review buffer. A quick check of the dates:\n\n```python\nweeks = ["beta", "review", "release"]\nprint(len(weeks))\n```',
  }),
  message({
    key: 'm3',
    role: 'user',
    text: 'Great. Attach it to the release notes.',
    createdAt: minutesAgo(3),
    attachments: [
      {
        key: 'a1',
        name: 'release-notes.pdf',
        filepath: '/files/c/release-notes.pdf',
        storedName: 'release-notes.pdf',
        conversationUuid: null,
        kind: 'document',
        inlineDataUrl: null,
      } as ChatMessage['attachments'][number],
    ],
  }),
  message({
    key: 'm4',
    role: 'assistant',
    uuid: '00000000-0000-4000-8000-000000000004',
    groupId: 4,
    createdAt: minutesAgo(2),
    text: 'Done. I added the plan to **release-notes.pdf** and linked the two GitHub issues I found.',
  }),
];

export function sampleLive(kind: 'thinking' | 'streaming'): LiveTurn {
  return {
    questionId: 'q-live',
    userText: 'And what about the Android build?',
    turn:
      kind === 'thinking'
        ? { ...INITIAL_TURN, phase: 'streaming' }
        : {
            ...INITIAL_TURN,
            phase: 'streaming',
            content: 'The Android build runs on the same pipeline. Edge-to-edge and predictive back are',
            tools: [tool('t3', 'list_workflows', 'GitHub', 'running', 0, null)],
          },
    execution: null,
    streamStatus: null,
    canReconnect: false,
    failure: null,
    settled: false,
    queued: false,
    continuation: false,
    attachmentNames: [],
    regeneration: null,
  };
}

export const SAMPLE_PAUSE: TurnPause = {
  kind: 'hitl',
  shape: 'single',
  threadId: null,
  messageId: null,
  interrupts: [
    {
      interruptId: 'i1',
      toolCallId: null,
      message: 'Research wants to send the release notes to the team.',
      availableActions: ['approve', 'reject', 'edit'],
      guardrailType: null,
      actionLabel: 'Approval needed',
      policyMessage: null,
      toolName: 'send_email',
      toolkitName: 'Mail',
      toolArgs: { to: 'team@acme.example', subject: 'Release notes' },
      questions: [],
      parentAgentName: null,
      childThreadId: null,
    },
  ],
};

export const SAMPLE_TRAY: TrayItem[] = [
  {
    id: 'f1',
    name: 'launch-plan.pdf',
    size: 182_000,
    kind: 'document',
    previewUri: null,
    state: 'ready',
    progress: null,
    error: null,
    ready: null,
    filepath: null,
    pickedUri: 'file:///launch-plan.pdf',
  } as TrayItem,
  {
    id: 'f2',
    name: 'budget.xlsx',
    size: 48_000,
    kind: 'document',
    previewUri: null,
    state: 'uploading',
    progress: 0.4,
    error: null,
    ready: null,
    filepath: null,
    pickedUri: 'file:///budget.xlsx',
  } as TrayItem,
];

export const SAMPLE_STARTERS = ['Summarise the latest release', 'Find open bugs for this sprint', 'Draft a status update', 'What changed since Monday?'];
