/**
 * Conversation-scoped model and execution-loop settings for the regular chat
 * surface. The provider settings live on the signed-in user's participant;
 * `steps_limit` lives in conversation metadata because it bounds the agent
 * loop rather than one model request.
 */
import { useCallback, useEffect, useMemo, useState } from 'react';

import { conversationApi } from '@/entities/conversation';
import { useUpdateParticipantSettingsMutation } from '@/entities/participant';
import { DEFAULT_MAX_TOKENS, DEFAULT_STEPS_LIMIT, DEFAULT_TEMPERATURE } from '@/shared/lib/constants';
import type { ChatBoxProps } from '@/widgets/chat-box';

type ActiveConversation = NonNullable<ChatBoxProps['conversation']>['active'];

interface ParticipantWire {
  readonly id?: string | number;
  readonly entity_name?: unknown;
  readonly entity_meta?: Readonly<Record<string, unknown>>;
  readonly entity_settings?: Readonly<Record<string, unknown>>;
}

const EMPTY_MODEL_SETTINGS: Readonly<Record<string, unknown>> = Object.freeze({});

export interface UseChatModelSettingsParams {
  readonly activeConversation: ActiveConversation;
  readonly projectId: string | number | undefined;
  readonly userId: string | undefined;
}

export interface UseChatModelSettingsResult {
  readonly settings: Readonly<Record<string, unknown>>;
  readonly onSetSettings: (settings: Readonly<Record<string, unknown>>) => void;
}

function participantsOf(conversation: ActiveConversation): readonly ParticipantWire[] {
  return (conversation?.participants ?? []) as readonly ParticipantWire[];
}

function isUserParticipant(participant: ParticipantWire, userId: string | undefined): boolean {
  const entityUserId = participant.entity_meta?.['id'];
  const comparableId = typeof entityUserId === 'string' || typeof entityUserId === 'number'
    ? String(entityUserId)
    : undefined;
  return participant.entity_name === 'user' && comparableId !== undefined && userId !== undefined && comparableId === userId;
}

function userParticipant(conversation: ActiveConversation, userId: string | undefined): ParticipantWire | undefined {
  return participantsOf(conversation).find((participant) => isUserParticipant(participant, userId));
}

function savedModelSettings(conversation: ActiveConversation, userId: string | undefined): Readonly<Record<string, unknown>> {
  const author = userParticipant(conversation, userId);
  const dummy = participantsOf(conversation).find((participant) => participant.entity_name === 'dummy');
  const settings = author?.entity_settings?.['llm_settings'] ?? dummy?.entity_settings?.['llm_settings'];
  return settings && typeof settings === 'object' && !Array.isArray(settings)
    ? settings as Readonly<Record<string, unknown>>
    : EMPTY_MODEL_SETTINGS;
}

function positiveProjectId(value: unknown): number | undefined {
  const parsed = Number(value);
  return Number.isInteger(parsed) && parsed > 0 ? parsed : undefined;
}

/**
 * The modal's starting values.
 *
 * `model_project_id` comes from the saved settings ONLY. A chat with no saved
 * model runs on the implied default, and that default is often a model SHARED
 * from another project (project 1). Defaulting the id to the CURRENT project
 * made the picker look for the shared default in the wrong project: it showed
 * "None", and the turn asked the current project for a model it does not have
 * (UI-PD-2). The send path now takes the project from the resolved default
 * model instead (`resolveSendLlmSettings`).
 */
function initialSettings(
  persisted: Readonly<Record<string, unknown>>,
  conversationMeta: Readonly<Record<string, unknown>> | undefined,
): Readonly<Record<string, unknown>> {
  const modelProjectId = positiveProjectId(persisted['model_project_id']);
  const stepsLimit = conversationMeta?.['steps_limit'];
  return {
    ...persisted,
    temperature: persisted['temperature'] ?? DEFAULT_TEMPERATURE,
    max_tokens: persisted['max_tokens'] ?? DEFAULT_MAX_TOKENS,
    steps_limit: typeof stepsLimit === 'number' ? stepsLimit : DEFAULT_STEPS_LIMIT,
    ...(modelProjectId !== undefined ? { model_project_id: modelProjectId } : {}),
  };
}

function modelSettingsOnly(settings: Readonly<Record<string, unknown>>): Readonly<Record<string, unknown>> {
  const { steps_limit: _stepsLimit, ...modelSettings } = settings;
  return modelSettings;
}

/** Owns the gear modal's live values and persists Apply to the real backend records. */
export function useChatModelSettings({
  activeConversation,
  projectId,
  userId,
}: UseChatModelSettingsParams): UseChatModelSettingsResult {
  const persisted = savedModelSettings(activeConversation, userId);
  const conversationMeta = activeConversation?.meta;
  const seeded = useMemo(
    () => initialSettings(persisted, conversationMeta),
    [persisted, conversationMeta],
  );
  const [settings, setSettings] = useState<Readonly<Record<string, unknown>>>(seeded);
  const { mutateAsync: updateParticipantSettings } = useUpdateParticipantSettingsMutation();
  const { mutateAsync: editConversation } = conversationApi.useEdit();

  useEffect(() => {
    setSettings(seeded);
  }, [seeded]);

  const persist = useCallback(
    async (next: Readonly<Record<string, unknown>>): Promise<void> => {
      const conversationId = activeConversation?.id;
      if (conversationId === undefined || projectId === undefined) return;

      const author = userParticipant(activeConversation, userId);
      const participantId = author?.id;
      const writes: Promise<unknown>[] = [];
      if (participantId !== undefined) {
        writes.push(updateParticipantSettings({
          projectId,
          conversationId: String(conversationId),
          participantId: String(participantId),
          settings: {
            ...author?.entity_settings,
            llm_settings: modelSettingsOnly(next),
          },
        }));
      }

      const stepsLimit = next['steps_limit'];
      if (typeof stepsLimit === 'number' && Number.isInteger(stepsLimit) && stepsLimit > 0) {
        writes.push(editConversation({
          projectId,
          id: conversationId,
          meta: { ...conversationMeta, steps_limit: stepsLimit },
        }));
      }
      await Promise.all(writes);
    },
    [activeConversation, projectId, userId, updateParticipantSettings, editConversation, conversationMeta],
  );

  const onSetSettings = useCallback(
    (updated: Readonly<Record<string, unknown>>) => {
      const next = { ...settings, ...updated };
      setSettings(next);
      void persist(next).catch((error: unknown) => {
        console.error('[chat] failed to save model settings', error);
      });
    },
    [settings, persist],
  );

  return { settings, onSetSettings };
}
