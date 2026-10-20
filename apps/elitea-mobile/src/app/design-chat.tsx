import { Redirect, Stack, useLocalSearchParams } from 'expo-router';
import { File } from 'expo-file-system';
import { useEffect, useRef, useState } from 'react';
import { FlatList } from 'react-native';

import { DesignProvider, useDesign } from '@/design/DesignProvider';
import { AnswerActions } from '@/features/chat/actions/AnswerActions';
import { planActions } from '@/features/chat/actions/messageActions';
import { AssistantAvatar } from '@/features/chat/AssistantAvatar';
import { AttachButton, AttachmentTrayView } from '@/features/chat/attachments/AttachmentTrayView';
import { attachmentLimits } from '@/features/chat/attachments/prepare';
import { useDocumentScan } from '@/features/chat/attachments/scan/useDocumentScan';
import type { ScanFormat } from '@/features/chat/attachments/scan/scanPipeline';
import { useAttachmentTray } from '@/features/chat/attachments/useAttachmentTray';
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
import { log } from '@/lib/log';
import { headerActionOptions } from '@/ui';

type PreviewState = 'conversation' | 'empty' | 'thinking' | 'streaming' | 'approval' | 'dictation' | 'compose' | 'scan';
const STATES: readonly PreviewState[] = ['conversation', 'empty', 'thinking', 'streaming', 'approval', 'dictation', 'compose', 'scan'];

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
 * `?state=conversation|empty|thinking|streaming|approval|dictation|compose|scan&brand=zefir|magenta|green|lemon`.
 * `state=scan`: the attach menu's "Scan document" runs the real scan pipeline (VisionKit on a
 * device, the sample pages on the simulator; PDF and on-device text recognition) into a real
 * tray (nothing is uploaded); `&scan=pdf|text` starts it at once with that format.
 * The scheme follows the device appearance (set the simulator's to preview dark).
 */
export default function DesignChatRoute() {
  const params = useLocalSearchParams<{ brand?: string; state?: string; scan?: string }>();
  if (!__DEV__) return <Redirect href="/" />;
  const state = STATES.includes(params.state as PreviewState) ? (params.state as PreviewState) : 'conversation';
  // Keyed, so a new state from a link starts from its own sample (the composer's text included).
  const autoScan = params.scan === 'pdf' || params.scan === 'text' ? params.scan : null;
  const key = `${state}:${params.brand ?? ''}:${autoScan ?? ''}`;
  if (params.brand === undefined) return <ChatPreview key={key} state={state} autoScan={autoScan} />;
  return (
    <DesignProvider brand={GALLERY_BRANDS[parseGalleryParams(params).brand]}>
      <ChatPreview key={key} state={state} autoScan={autoScan} />
    </DesignProvider>
  );
}

function ChatPreview({ state, autoScan }: { state: PreviewState; autoScan: ScanFormat | null }) {
  const design = useDesign();
  const { toast, show: showToast, hide: hideToast } = useToast();
  const showScanNote = (text: string) => showToast(text);
  const scanPreview = useScanPreview(state === 'scan', autoScan, showScanNote);
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
              state === 'scan' ? (
                <AttachmentTrayView items={scanPreview.items} onRemove={(id) => scanPreview.tray.remove(id)} onRetry={() => {}} />
              ) : state === 'compose' ? (
                <>
                  <TargetChip current={AGENTS[0]} choices={AGENTS} onChoose={() => {}} />
                  <AttachmentTrayView items={SAMPLE_TRAY} onRemove={() => {}} onRetry={() => {}} />
                </>
              ) : null
            }
            leading={
              <AttachButton
                disabledReason={null}
                scan={state === 'scan' ? scanPreview.source : null}
                onPick={(source) => (source === 'scan' && state === 'scan' ? void scanPreview.run() : undefined)}
              />
            }
            onSend={() => null}
          />
        }
      />
    </>
  );
}

/**
 * `state=scan`: the real document scan into a real tray (prepared, never uploaded: the preview
 * has no conversation). What was made is logged to Metro (`[design-scan]`), the recognised text
 * of a `.txt` included, so the simulator run can be checked without a server.
 */
function useScanPreview(enabled: boolean, autoScan: ScanFormat | null, note: (text: string) => void) {
  const limits = attachmentLimits(null);
  const { tray, items } = useAttachmentTray(null, null, limits);
  const documentScan = useDocumentScan(limits, autoScan ? { chooseFormat: async (formats) => (formats.includes(autoScan) ? autoScan : null) } : {});
  const run = async () => {
    try {
      const outcome = await documentScan.scan(() => tray.usage());
      if (outcome.kind === 'refused') return note(outcome.message);
      if (outcome.kind !== 'files') return;
      for (const file of outcome.files) {
        const text = file.mimeType === 'text/plain' ? new File(file.uri).textSync() : null;
        log.info('[design-scan] made', { name: file.name, size: file.size, mimeType: file.mimeType, uri: file.uri, recognised: text });
      }
      const { refused, warnings } = tray.add(outcome.files);
      const said = [...refused.map((r) => r.message), ...outcome.warnings, ...warnings];
      if (said.length > 0) note(said.join('\n'));
    } catch (error) {
      log.warn('[design-scan] failed', { error });
      note("The scan couldn't be saved. Try again.");
    }
  };
  const started = useRef(false);
  useEffect(() => {
    if (!enabled || autoScan === null || started.current) return;
    started.current = true;
    void run();
  });
  return { source: documentScan.source, tray, items, run };
}
