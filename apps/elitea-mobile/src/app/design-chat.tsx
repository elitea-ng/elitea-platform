import { Redirect, Stack, useLocalSearchParams } from 'expo-router';
import { useState } from 'react';
import { FlatList } from 'react-native';

import { DesignProvider, useDesign } from '@/design/DesignProvider';
import { AnswerActions } from '@/features/chat/actions/AnswerActions';
import { planActions } from '@/features/chat/actions/messageActions';
import { AssistantAvatar } from '@/features/chat/AssistantAvatar';
import { AttachButton, AttachmentTrayView } from '@/features/chat/attachments/AttachmentTrayView';
import { ChatEmptyState } from '@/features/chat/ChatEmptyState';
import { Composer } from '@/features/chat/Composer';
import type { ChatMessage } from '@/features/chat/messageModel';
import { PauseCard } from '@/features/chat/pauses/PauseCard';
import { TargetChip } from '@/features/chat/TargetChip';
import { ThreadLayout, Toast, useToast } from '@/features/chat/ThreadLayout';
import { LiveReply, PersistedMessage } from '@/features/chat/ThreadRows';
import { GALLERY_BRANDS, parseGalleryParams } from '@/features/designGallery/DesignGallery';
import { SAMPLE_AGENT, SAMPLE_MESSAGES, SAMPLE_PAUSE, SAMPLE_STARTERS, SAMPLE_TOOLS, SAMPLE_TRAY, sampleLive } from '@/features/designGallery/chatFixtures';
import { SAMPLE_HOST } from '@/features/designGallery/fixtures';
import { headerActionOptions } from '@/ui';

type PreviewState = 'conversation' | 'empty' | 'thinking' | 'streaming' | 'approval' | 'dictation' | 'compose';
const STATES: readonly PreviewState[] = ['conversation', 'empty', 'thinking', 'streaming', 'approval', 'dictation', 'compose'];

type Row = { kind: 'message'; message: ChatMessage } | { kind: 'live'; live: ReturnType<typeof sampleLive> };

const SAMPLE_PAUSE_TARGET = { projectId: 42, conversationUuid: '00000000-0000-4000-8000-0000000000aa', messageId: 'm-pause', threadId: null };

const AGENTS = [
  { participantId: 1, name: SAMPLE_AGENT, agentId: 10, agentProjectId: 42, versionId: 1 },
  { participantId: 2, name: 'Approver', agentId: 20, agentProjectId: 42, versionId: 1 },
];

/**
 * Development builds only: the conversation screen with sample data, drawn by the components the
 * real thread (`/c/[id]`) renders (`ThreadLayout`, `PersistedMessage`, `LiveReply`,
 * `AnswerActions`, `PauseCard`, `ChatEmptyState`, `Composer`; D2), so it can be checked and
 * screenshot without a server.
 * `?state=conversation|empty|thinking|streaming|approval|dictation|compose&brand=zefir|magenta|green|lemon`.
 * The scheme follows the device appearance (set the simulator's to preview dark).
 */
export default function DesignChatRoute() {
  const params = useLocalSearchParams<{ brand?: string; state?: string }>();
  if (!__DEV__) return <Redirect href="/" />;
  const state = STATES.includes(params.state as PreviewState) ? (params.state as PreviewState) : 'conversation';
  // Keyed, so a new state from a link starts from its own sample (the composer's text included).
  const key = `${state}:${params.brand ?? ''}`;
  if (params.brand === undefined) return <ChatPreview key={key} state={state} />;
  return (
    <DesignProvider brand={GALLERY_BRANDS[parseGalleryParams(params).brand]}>
      <ChatPreview key={key} state={state} />
    </DesignProvider>
  );
}

function ChatPreview({ state }: { state: PreviewState }) {
  const design = useDesign();
  const { toast, show: showToast, hide: hideToast } = useToast();
  const [draft, setDraft] = useState<{ text: string; key: number } | null>(null);
  const [reading, setReading] = useState<string | null>(null);
  const newest = SAMPLE_MESSAGES[SAMPLE_MESSAGES.length - 1];
  const rows: Row[] = [];
  if (state === 'thinking' || state === 'streaming') rows.push({ kind: 'live', live: sampleLive(state) });
  if (state !== 'empty') for (const message of [...SAMPLE_MESSAGES].reverse()) rows.push({ kind: 'message', message });
  const avatar = <AssistantAvatar name={SAMPLE_AGENT} imageUri={null} mascot />;
  return (
    <>
      <Stack.Screen
        options={{
          title: 'Zefir launch planning',
          headerShown: true,
          ...headerActionOptions(
            [
              { id: 'export', icon: 'share', label: 'Export conversation', onPress: () => {} },
              { id: 'people', icon: 'people', label: 'Participants', onPress: () => {} },
            ],
            design,
          ),
        }}
      />
      <ThreadLayout
        renderTranscript={(composerHeight) =>
          state === 'empty' ? (
            <ChatEmptyState
              agentName={SAMPLE_AGENT}
              iconUri={null}
              welcome="I find answers in your team's GitHub, docs and tickets."
              suggestions={SAMPLE_STARTERS}
              onSuggestion={(text) => setDraft((d) => ({ text, key: (d?.key ?? 0) + 1 }))}
              bottomInset={composerHeight}
            />
          ) : (
            <FlatList
              inverted
              data={rows}
              keyExtractor={(row) => (row.kind === 'live' ? 'live' : row.message.key)}
              contentContainerStyle={{ paddingTop: composerHeight + design.space[2], paddingBottom: 8 }}
              ListHeaderComponent={
                state === 'approval' ? (
                  <PauseCard pause={SAMPLE_PAUSE} target={SAMPLE_PAUSE_TARGET} offline={false} host={SAMPLE_HOST} onContinue={async () => null} onOpenWeb={() => {}} />
                ) : undefined
              }
              renderItem={({ item }) =>
                item.kind === 'live' ? (
                  <LiveReply
                    live={item.live}
                    author={SAMPLE_AGENT}
                    avatar={avatar}
                    onRetry={() => {}}
                    onReconnect={() => {}}
                    onDismiss={() => {}}
                    onOpenTool={() => {}}
                    usageAvailable
                  />
                ) : (
                  <PersistedMessage
                    message={item.message}
                    highlight={{ people: [], agents: [] }}
                    opening={null}
                    onOpenFile={() => {}}
                    tools={item.message.key === 'm2' ? SAMPLE_TOOLS : []}
                    onOpenTool={() => {}}
                    onActions={() => {}}
                    avatar={avatar}
                    actions={
                      <AnswerActions
                        message={item.message}
                        plan={planActions(item.message, {
                          shareOut: true,
                          contract13: true,
                          online: true,
                          canSpeak: true,
                          canRegenerate: item.message.key === newest.key && state === 'conversation',
                        })}
                        projectId={null}
                        reading={reading === item.message.key}
                        onReadAloud={() => setReading(item.message.key)}
                        onStopReading={() => setReading(null)}
                        onRegenerate={() => {}}
                        onMore={() => {}}
                        onDislike={() => {}}
                        onNote={showToast}
                      />
                    }
                  />
                )
              }
            />
          )
        }
        floating={
          <>
            <Toast toast={toast} onHide={hideToast} />
          </>
        }
        composer={
          <Composer
            busy={state === 'thinking' || state === 'streaming'}
            onStop={state === 'thinking' || state === 'streaming' ? async () => ({ problem: null, restoreText: null }) : null}
            placeholder={`Message ${SAMPLE_AGENT}…`}
            draft={draft}
            initialText={state === 'compose' ? 'Can you also check the Android build?' : null}
            disabledReason={state === 'approval' ? 'Waiting for your approval above.' : null}
            dictation
            // The composer's own listening state (D2: the real component, as users see it).
            dictationPreview={state === 'dictation' ? { heard: "Summarise yesterday's meeting and draft a follow-up", level: 0.4 } : null}
            accessory={
              state === 'compose' ? (
                <>
                  <TargetChip current={AGENTS[0]} choices={AGENTS} onChoose={() => {}} />
                  <AttachmentTrayView items={SAMPLE_TRAY} onRemove={() => {}} onRetry={() => {}} />
                </>
              ) : null
            }
            leading={<AttachButton disabledReason={null} onPick={() => {}} />}
            onSend={() => null}
          />
        }
      />
    </>
  );
}
