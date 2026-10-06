/** Creates immutable hidden Test contexts. Restore only reads existing durable records. */
import { useCallback, useEffect, useRef, useState } from "react";
import { conversationApi } from "@/entities/conversation";
import {
  EditorTestContext,
  EditorTestRunsPage,
  type EditorTestRun,
} from "@/shared/api/generated/model";
import type { ChatBoxProps } from "@/widgets/chat-box";

type ConversationWire = Awaited<ReturnType<typeof conversationApi.details>>;
type ActiveConversation = NonNullable<ChatBoxProps["conversation"]>["active"];
type MessageGroups = NonNullable<
  NonNullable<ActiveConversation>["message_groups"]
>;
export interface PipelineTestChatIdentity {
  readonly projectId: string | undefined;
  readonly applicationId: string | undefined;
  readonly pipelineName: string | undefined;
  readonly versionId: string | undefined;
  readonly agentType: string | undefined;
  readonly userId: string | undefined;
}
export interface PipelineTestRestore {
  readonly conversationId: string | undefined;
  readonly onComplete: () => void;
}
interface State {
  readonly key: string;
  readonly conversation: ActiveConversation;
  readonly activeParticipant: unknown;
  readonly isCreating: boolean;
  readonly hasFailed: boolean;
  readonly groups: MessageGroups;
  readonly runs: readonly EditorTestRun[];
  readonly selectedRun: EditorTestRun | undefined;
}
export interface UsePipelineTestConversationResult extends State {
  readonly displayKey: string;
  readonly staleVersionId: string | undefined;
  readonly release: () => void;
  readonly ensure: () => void;
  readonly selectRun: (responseId: string) => void;
}
const EMPTY: State = {
  key: "",
  conversation: undefined,
  activeParticipant: undefined,
  isCreating: false,
  hasFailed: false,
  groups: [],
  runs: [],
  selectedRun: undefined,
};
const LIVE_TEST_CONVERSATIONS = new Map<string, State>();
export function resetPipelineTestConversationsForTests(): void {
  LIVE_TEST_CONVERSATIONS.clear();
}
function editorTestIdentityKey(
  identity: PipelineTestChatIdentity,
): string | undefined {
  const values = [
    identity.userId,
    identity.projectId,
    identity.applicationId,
    identity.versionId,
  ];
  return values.every(
    (value) => value !== undefined && /^[1-9][0-9]*$/.test(value),
  )
    ? values.join(":")
    : undefined;
}
function selectedGroups(
  groups: MessageGroups,
  run: EditorTestRun | undefined,
): MessageGroups {
  if (!run) return [];
  return groups.filter(
    (group) =>
      group.uuid === run.response_message_id || group.uuid === run.question_id,
  );
}
function assertTestOwnership(detail: ConversationWire, identity: PipelineTestChatIdentity, restoring: boolean) {
 const context=EditorTestContext.parse(detail.meta?.['editor_test']);
 if (detail.source!=='editor_test' || detail.meta?.['is_hidden']!==true || detail.is_private!==true || !detail.uuid) throw new Error('Unsupported editor Test context');
 if (context.actor_id!==identity.userId || context.project_id!==identity.projectId || context.application_id!==identity.applicationId) throw new Error('Foreign Test context');
 if (!restoring && context.application_version_id!==identity.versionId) throw new Error('Unexpected Test version');
 return context;
}
function assertTestParticipant(detail: ConversationWire, context: EditorTestContext) {
 const application=detail.participants?.find((row)=>row.entity_name==='application');
 if (!application?.id || String(application.entity_meta?.['id'])!==context.application_id
   || String(application.entity_meta?.['project_id'])!==context.project_id
   || String(application.entity_settings?.['version_id'])!==context.application_version_id) throw new Error('Invalid Test participant');
 return application;
}
function stateFromDetail(detail:ConversationWire,identity:PipelineTestChatIdentity,restoring:boolean,key:string):State {
 const context=assertTestOwnership(detail,identity,restoring);
 const application=assertTestParticipant(detail,context);
 const participants=[...(detail.participants ?? [])];
  const runs = restoring
    ? EditorTestRunsPage.parse(detail.editor_test_runs).rows
    : [];
  const selectedRun = runs[0];
  const groups =
    restoring && Array.isArray(detail["message_groups"])
      ? (detail["message_groups"] as MessageGroups)
      : [];
  return {
    ...EMPTY,
    key,
    activeParticipant: application,
    groups,
    runs,
    selectedRun,
    conversation: {
      id: detail.id,
      uuid: detail.uuid!,
      name: detail.name,
      participants,
      message_groups: selectedGroups(groups, selectedRun),
      meta: detail.meta ?? {},
      ...(restoring && selectedRun?.phase === "TERMINAL"
        ? { isPlayback: true }
        : {}),
    },
  };
}
function ownsVisibleConversation(state:State,key:string|undefined):boolean {return Boolean(state.conversation && state.key===key);}
export function usePipelineTestConversation(
  identity: PipelineTestChatIdentity,
  restore?: PipelineTestRestore,
): UsePipelineTestConversationResult {
  const { mutateAsync: createConversation } = conversationApi.useCreate();
  const [state, setState] = useState<State>(EMPTY);
  const scope = useRef({active:true});
  const operation = useRef(0);
  const displayGeneration = useRef(0);
  const pending = useRef(false);
  const current = useRef({
    identity,
    restore,
    key: editorTestIdentityKey(identity),
  });
  current.current = { identity, restore, key: editorTestIdentityKey(identity) };
  const key = current.current.key;
  const restoreId = restore?.conversationId;
  const retainedRestore = useRef<
    { key: string; identityKey: string | undefined } | undefined
  >(undefined);
  const restoreKey = restoreId
    ? `${key}:restore:${restoreId}`
    : retainedRestore.current?.identityKey === key
      ? retainedRestore.current?.key
      : key;
  const currentKey = useRef(restoreKey);
  currentKey.current = restoreKey;
  useEffect(() => {
    const window={active:true};scope.current=window;
    if (!restoreId && retainedRestore.current?.key===restoreKey) return ()=>{window.active=false;};
    const ticket = ++operation.current;
    pending.current = false;
    setState(EMPTY);
    if (
      !restoreId ||
      !identity.projectId ||
      !identity.userId ||
      !identity.applicationId
    )
      return ()=>{window.active=false;};
    pending.current = true;
    setState({ ...EMPTY, key: restoreKey ?? "", isCreating: true });
    const abort = new AbortController();
    void conversationApi
      .details(
        {
          projectId: identity.projectId,
          id: restoreId,
          editor_test_runs: true,
          runs_limit: 50,
          messages_limit: 50,
          sort_order: "desc",
        },
        abort.signal,
      )
      .then((detail) => {
        if (!window.active || ticket !== operation.current || currentKey.current !== restoreKey)
          return;
        const restored = stateFromDetail(
          detail,
          current.current.identity,
          true,
          restoreKey ?? "",
        );
        retainedRestore.current = { key: restoreKey ?? "", identityKey: key };
        setState(restored);
        current.current.restore?.onComplete();
      })
      .catch(() => {
        if (window.active && ticket === operation.current)
          setState({ ...EMPTY, key: restoreKey ?? "", hasFailed: true });
      })
      .finally(() => {
        if (window.active && ticket === operation.current) pending.current = false;
      });
    return () => {
      abort.abort();
      window.active=false;
    };
  }, [restoreKey,restoreId,identity.projectId,identity.userId,identity.applicationId,key]);
  const ensure = useCallback(() => {
    const input = current.current;
    const window=scope.current;
    if (
      input.restore?.conversationId ||
      !input.key ||
      pending.current ||
      ownsVisibleConversation(state,currentKey.current)
    )
      return;
    const live = LIVE_TEST_CONVERSATIONS.get(input.key);
    if (live?.conversation?.id) {
      pending.current = true;
      const ticket = ++operation.current;
      const expected = input.key;
      setState({ ...live, isCreating: true });
      void conversationApi
        .details({
          projectId: input.identity.projectId!,
          id: live.conversation.id,
          editor_test_runs: true,
          runs_limit: 50,
          messages_limit: 50,
          sort_order: "desc",
        })
        .then((detail) => {
          if (!window.active || ticket !== operation.current || currentKey.current !== expected)
            return;
          const recovered = stateFromDetail(
            detail,
            input.identity,
            true,
            expected,
          );
          const next = {
            ...recovered,
            conversation: { ...recovered.conversation, isPlayback: false },
          };
          LIVE_TEST_CONVERSATIONS.set(expected, next);
          setState(next);
        })
        .catch(() => {
          if (window.active && ticket === operation.current)
            setState({ ...EMPTY, key: expected, hasFailed: true });
        })
        .finally(() => {
          if (window.active && ticket === operation.current) pending.current = false;
        });
      return;
    }
    pending.current = true;
    const ticket = ++operation.current;
    const expected = input.key;
    setState({ ...EMPTY, key: expected, isCreating: true });
    void createConversation({
      projectId: input.identity.projectId!,
      name: input.identity.pipelineName?.slice(0, 50) || "Pipeline Test",
      is_private: true,
      source: "editor_test",
      meta: { is_hidden: true },
      participants: [
        {
          entity_name: "application",
          entity_meta: {
            id: input.identity.applicationId,
            project_id: input.identity.projectId,
            name: input.identity.pipelineName,
          },
          entity_settings: {
            version_id: input.identity.versionId,
            agent_type: input.identity.agentType ?? "pipeline",
            variables: [],
            icon_meta: {},
          },
        },
      ],
    })
      .then((detail) => {
        if (!window.active || ticket !== operation.current || currentKey.current !== expected)
          return;
        const next = stateFromDetail(detail, input.identity, false, expected);
        LIVE_TEST_CONVERSATIONS.set(expected, next);
        setState(next);
      })
      .catch(() => {
        if (window.active && ticket === operation.current)
          setState({ ...EMPTY, key: expected, hasFailed: true });
      })
      .finally(() => {
        if (window.active && ticket === operation.current) pending.current = false;
      });
  }, [createConversation, state]);
  const selectRun = useCallback(
    (responseId: string) =>
      setState((previous) => {
        const run = previous.runs.find(
          (entry) => entry.response_message_id === responseId,
        );
        if (!run || !previous.conversation) return previous;
        return {
          ...previous,
          selectedRun: run,
          conversation: {
            ...previous.conversation,
            message_groups: selectedGroups(previous.groups, run),
            isPlayback: run.phase === "TERMINAL",
          },
        };
      }),
    [],
  );
  const release = useCallback(() => {
    ++operation.current;pending.current=false;retainedRestore.current=undefined;
    ++displayGeneration.current;
    const identityKey=current.current.key;
    if(identityKey)LIVE_TEST_CONVERSATIONS.delete(identityKey);
    setState(EMPTY);
  },[]);
  const visible = state.key === restoreKey ? state : EMPTY;
  return { ...visible, displayKey: `${restoreKey ?? 'pending-test'}:${displayGeneration.current}`, release, ensure, selectRun, staleVersionId: undefined };
}
