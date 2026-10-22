/**
 * The socket.io event contract this app speaks — 43 events, their direction
 * and their payload schemas.
 *
 * MAINTAINED BY HAND. It was generated until #126 deleted the Go prototype
 * socket.io server (services/elitea-main/internal/api/socketio/server.go);
 * scripts/gen-socket-contract.mjs read that file as a required input and
 * could not run without it, so the generator, its allow-list and its
 * rendering library are retired. Edit this file directly; the tests in
 * events.test.ts are what hold it to its shape.
 *
 * Evidence for every entry is the CLIENT side — the legacy app's own
 * catalogue and call sites (apps/elitea-ui/src/common/constants.js
 * sioEvents, and the hooks that emit/listen). The Go service registers no
 * socket.io handler and mounts no socket.io path, so nothing here is a
 * statement about what elitea-main serves. A live socket server, when this
 * app has one, is reached through VITE_SOCKET_SERVER (empty in every shipped
 * deployment today — see client.ts's no-op client).
 */
import { z } from 'zod';

/**
 * Free-form metadata blob attached to most streamed agent/tool messages
 * (`response_metadata`). Evidence across dozens of call sites
 * (components/Chat/hooks.js:509-1460) shows wildly different nested keys
 * per discriminant (tool_run_id, metadata{}, tool_name, toolkit_name,
 * thread_id, model_name, node_name, hitl_interrupts[], resource_metadata{},
 * authorization_servers[], server_url, tool_meta{}, ...); every real
 * consumer reads it defensively via optional chaining, so a permissive
 * record — not a fabricated strict shape — is the accurate reproduction of
 * the actual contract.
 */
const responseMetadataSchema = z.record(z.string(), z.unknown());

/**
 * Shared envelope for streamed predict responses (chat_predict /
 * application_predict / chat_predict_attachment, receive direction).
 * Evidence: components/Chat/hooks.js:367-373 (destructures message_id,
 * type, response_metadata, sio_event, question_id from every message).
 * The full envelope is what the old React client actually consumes. The Go
 * prototype server that once emitted a narrower `type`+`content` subset is
 * deleted (#126), so this schema now describes the client's expectation
 * alone, and stays loose because a real server may send less.
 */
const streamEnvelopeSchema = z.looseObject({
  message_id: z.string().optional(),
  sio_event: z.string().optional(),
  question_id: z.string().optional(),
  type: z.string(),
  content: z.unknown().optional(),
  response_metadata: responseMetadataSchema.optional(),
  created_at: z.union([z.string(), z.number()]).optional(),
  done: z.boolean().optional(),
  threadId: z.string().optional(),
});

/**
 * Client -> server "start/continue a run" emit payload. Loose: the old app
 * builds this from a free-form `eventPayload` object
 * ([fsd]/features/chat/ui/chat-box/ChatBox.jsx:929) merged with a
 * `conversation_uuid`. Both `conversation_uuid` and `conversation_id` are
 * accepted: the deleted Go prototype read the second name, and the legacy
 * server the shipped client talks to is not held to either by this file.
 */
const predictEmitSchema = z.looseObject({
  conversation_uuid: z.string().optional(),
  conversation_id: z.union([z.string(), z.number()]).optional(),
  project_id: z.string().optional(),
  version_id: z.union([z.string(), z.number()]).optional(),
  participant_id: z.union([z.string(), z.number()]).optional(),
  content: z.unknown().optional(),
  input: z.unknown().optional(),
});

/**
 * Room enter/leave emit payload. Every observed server has accepted either
 * `room_id` or `conversation_id`, and the leave side has been observed
 * ignoring its payload altogether (rooms tracked by socket id), so this
 * stays permissive rather than pinning one name.
 */
const roomLifecycleEmitSchema = z.looseObject({
  conversation_id: z.union([z.string(), z.number()]).optional(),
  conversation_uuid: z.string().optional(),
  project_id: z.string().optional(),
  room_id: z.string().optional(),
});

/**
 * chat_leave_rooms has been observed emitted with an ARRAY of stream ids
 * (components/Chat/hooks.js:73, `emitLeaveRoom([streamId])`) as well as
 * the object shape above and no payload at all — the server ignores the
 * payload regardless, so all three are accepted.
 */
const leaveRoomsEmitSchema = z.union([
  roomLifecycleEmitSchema,
  z.array(z.union([z.string(), z.number()])),
  z.undefined(),
]);

// -- Canvas (hooks/chat/useCanvasSocket.js) --------------------------------
const canvasJoinEmitSchema = z.looseObject({
  project_id: z.string().optional(),
  canvas_uuid: z.string().optional(),
});
const canvasLeaveEmitSchema = z.looseObject({
  canvas_uuid: z.string().optional(),
  project_id: z.string().optional(),
  canvas_content: z.unknown().optional(),
  code_language: z.string().optional(),
});
const canvasEditEmitSchema = z.looseObject({
  project_id: z.string().optional(),
  canvas_uuid: z.string().optional(),
  content: z.unknown().optional(),
});
const canvasSyncReceiveSchema = z.looseObject({ content: z.unknown().optional() });
const canvasErrorReceiveSchema = z.unknown();
const canvasDetailReceiveSchema = z.looseObject({ content: z.unknown().optional() });
/** The one canvas event whose received shape the client fully specifies (hooks/chat/useCanvasSocket.js:130-154). */
const canvasEditorJoinedReceiveSchema = z.looseObject({ user_id: z.unknown().optional() });
const canvasEditorsChangeReceiveSchema = z.unknown();
/** Broadcast of the raw edit payload verbatim (`data`) — arbitrary shape. */
const canvasContentChangeReceiveSchema = z.record(z.string(), z.unknown());

// -- Notifications ----------------------------------------------------------
/** widgets/sidebar-root/ui/button/NotificationButton.jsx:39-43 never reads the payload — it only flips a boolean on receipt. */
const notificationsNotifyReceiveSchema = z.unknown();

// -- MCP ----------------------------------------------------------------------
const mcpStatusReceiveSchema = z.looseObject({
  project_id: z.union([z.string(), z.number()]).optional(),
  connected: z.boolean().optional(),
  type: z.string().optional(),
});
const testMcpConnectionEmitSchema = z.looseObject({
  stream_id: z.string().optional(),
  message_id: z.string().optional(),
  project_id: z.union([z.string(), z.number()]).optional(),
  toolkit_config: z
    .looseObject({
      toolkit_id: z.union([z.string(), z.number()]).optional(),
      toolkit_name: z.string().optional(),
      type: z.string().optional(),
      settings: z.unknown().optional(),
    })
    .optional(),
  mcp_tokens: z.record(z.string(), z.unknown()).optional(),
});
const testMcpConnectionReceiveSchema = streamEnvelopeSchema;

// -- Chat message/participant lifecycle (receive-only) -----------------------
const chatMessageSyncReceiveSchema = z.record(z.string(), z.unknown());
const chatMessageDeleteReceiveSchema = z.looseObject({ message_group_uid: z.string().optional() });
const chatMessageDeleteAllReceiveSchema = z.looseObject({
  conversation_id: z.union([z.string(), z.number()]).optional(),
});
const chatParticipantDeleteReceiveSchema = z.looseObject({
  conversation_id: z.union([z.string(), z.number()]).optional(),
  participant_id: z.union([z.string(), z.number()]).optional(),
});
const chatParticipantUpdateReceiveSchema = z.record(z.string(), z.unknown());
const chatConversationNameUpdatedReceiveSchema = z.looseObject({
  conversation_id: z.union([z.string(), z.number()]).optional(),
  name: z.string().optional(),
});
const socketValidationErrorReceiveSchema = z.looseObject({
  message_id: z.string().optional(),
  type: z.string().optional(),
  content: z.unknown().optional(),
});

// -- ASR (features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js) ---
const asrStartEmitSchema = z.looseObject({
  project_id: z.string().optional(),
  model_name: z.string().optional(),
  model_project_id: z.union([z.string(), z.number()]).optional(),
  language: z.string().optional(),
});
/** `audio` is a PCM16 ArrayBuffer (useStreamingSpeechRecognition.hooks.js:201) — not zod-structurally representable, validated as present. */
const asrAudioChunkEmitSchema = z.looseObject({ audio: z.unknown() });
const asrStopEmitSchema = z.looseObject({});
const asrTranscriptDeltaReceiveSchema = z.looseObject({ delta: z.string().optional() });
const asrTranscriptDoneReceiveSchema = z.looseObject({ transcript: z.string().optional() });
const asrErrorReceiveSchema = z.looseObject({ error: z.string().optional() });
const asrSpeechStartedReceiveSchema = z.looseObject({});
const asrVadFlushReceiveSchema = z.looseObject({});

// -- TTS (features/chat/lib/hooks/useTextToSpeech.hooks.js) ------------------
const ttsStartEmitSchema = z.looseObject({
  project_id: z.union([z.string(), z.number()]).optional(),
  model_name: z.string().optional(),
  model_project_id: z.union([z.string(), z.number()]).optional(),
  text: z.string().optional(),
  voice: z.string().optional(),
  speed: z.number().optional(),
});
const ttsStopEmitSchema = z.looseObject({});
/** Declared in constants.js:932 but grep-verified UNUSED anywhere else in the old app — dead constant, not merely an unimplemented backend feature. */
const ttsNextUnusedSchema = z.unknown();
const ttsAudioChunkReceiveSchema = z.looseObject({ audio: z.unknown(), sample_rate: z.number().optional() });
const ttsDoneReceiveSchema = z.looseObject({ char_end: z.number().optional() });
const ttsErrorReceiveSchema = z.looseObject({ error: z.string().optional() });

/** The 43 catalogued socket.io channel event names, in constants.js declaration order (R-A3: only this module and client.ts import socket.io-client-adjacent types). */
export const SOCKET_EVENT_NAMES = ["socket_validation_error", "chat_predict", "chat_continue_predict", "application_continue_message", "chat_enter_room", "chat_leave_rooms", "chat_participant_delete", "chat_message_delete", "chat_message_delete_all", "chat_message_sync", "chat_participant_update", "chat_conversation_name_updated", "application_predict", "application_leave_rooms", "promptlib_predict", "promptlib_leave_rooms", "notifications_notify", "chat_canvas_join", "chat_canvas_leave_rooms", "chat_canvas_edit", "chat_canvas_sync", "chat_canvas_error", "chat_canvas_detail", "chat_canvas_editor_joined", "chat_canvas_editors_change", "chat_canvas_content_change", "chat_predict_attachment", "mcp_status", "test_mcp_connection", "asr_start", "asr_audio_chunk", "asr_stop", "asr_transcript_delta", "asr_transcript_done", "asr_error", "asr_speech_started", "asr_vad_flush", "tts_start", "tts_stop", "tts_next", "tts_audio_chunk", "tts_done", "tts_error"] as const;

export type SocketEventName = (typeof SOCKET_EVENT_NAMES)[number];

/** Not exported: only consumed inline via SocketEventContract.direction below — export when a Wave-2 consumer needs to name it directly. */
type SocketEventDirection = 'emit' | 'receive' | 'bidirectional';

export interface SocketEventContract<TEmit = unknown, TReceive = unknown> {
  readonly name: SocketEventName;
  readonly direction: SocketEventDirection;
  readonly emitSchema: z.ZodType<TEmit> | null;
  readonly receiveSchema: z.ZodType<TReceive> | null;
  readonly evidence: readonly string[];
}

/** Keyed registry — the single source of truth every socket/ module reads from. */
export const SOCKET_EVENTS = {
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:882
   *   - apps/elitea-ui/src/components/Chat/hooks.js:1527
   */
  socket_validation_error: {
    name: "socket_validation_error",
    direction: "receive",
    emitSchema: null,
    receiveSchema: socketValidationErrorReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:882", "apps/elitea-ui/src/components/Chat/hooks.js:1527"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:883
   *   - apps/elitea-ui/src/components/Chat/hooks.js:239,496
   *   - apps/elitea-ui/src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:929
   */
  chat_predict: {
    name: "chat_predict",
    direction: "bidirectional",
    emitSchema: predictEmitSchema,
    receiveSchema: streamEnvelopeSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:883", "apps/elitea-ui/src/components/Chat/hooks.js:239,496", "apps/elitea-ui/src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:929"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:884
   *   - apps/elitea-ui/src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:174
   */
  chat_continue_predict: {
    name: "chat_continue_predict",
    direction: "emit",
    emitSchema: predictEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:884", "apps/elitea-ui/src/[fsd]/features/chat/ui/chat-box/ChatBox.jsx:174"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:885
   *   NOTE: Declared, grep-verified UNUSED anywhere else in the old app (no emit/listener call site). Typed by analogy to chat_continue_predict.
   */
  application_continue_message: {
    name: "application_continue_message",
    direction: "emit",
    emitSchema: predictEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:885"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:886
   *   - apps/elitea-ui/src/[fsd]/features/agent/lib/hooks/useApplicationChat.hooks.js:114
   *   - apps/elitea-ui/src/[fsd]/features/pipelines/lib/hooks/usePipelineChat.hooks.js:95,156
   *   - apps/elitea-ui/src/[fsd]/features/toolkits/lib/hooks/useToolkitChat.hooks.js:208,218-224
   */
  chat_enter_room: {
    name: "chat_enter_room",
    direction: "emit",
    emitSchema: roomLifecycleEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:886", "apps/elitea-ui/src/[fsd]/features/agent/lib/hooks/useApplicationChat.hooks.js:114", "apps/elitea-ui/src/[fsd]/features/pipelines/lib/hooks/usePipelineChat.hooks.js:95,156", "apps/elitea-ui/src/[fsd]/features/toolkits/lib/hooks/useToolkitChat.hooks.js:208,218-224"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:887
   *   - apps/elitea-ui/src/components/Chat/hooks.js:73,240,1529
   *   - apps/elitea-ui/src/[fsd]/features/pipelines/lib/hooks/usePipelineChat.hooks.js:96
   *   - apps/elitea-ui/src/[fsd]/features/toolkits/lib/hooks/useToolkitChat.hooks.js:209
   */
  chat_leave_rooms: {
    name: "chat_leave_rooms",
    direction: "emit",
    emitSchema: leaveRoomsEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:887", "apps/elitea-ui/src/components/Chat/hooks.js:73,240,1529", "apps/elitea-ui/src/[fsd]/features/pipelines/lib/hooks/usePipelineChat.hooks.js:96", "apps/elitea-ui/src/[fsd]/features/toolkits/lib/hooks/useToolkitChat.hooks.js:209"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:888
   *   - apps/elitea-ui/src/components/Chat/hooks.js:1584-1596
   */
  chat_participant_delete: {
    name: "chat_participant_delete",
    direction: "receive",
    emitSchema: null,
    receiveSchema: chatParticipantDeleteReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:888", "apps/elitea-ui/src/components/Chat/hooks.js:1584-1596"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:889
   *   - apps/elitea-ui/src/components/Chat/hooks.js:1554-1566
   */
  chat_message_delete: {
    name: "chat_message_delete",
    direction: "receive",
    emitSchema: null,
    receiveSchema: chatMessageDeleteReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:889", "apps/elitea-ui/src/components/Chat/hooks.js:1554-1566"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:890
   *   - apps/elitea-ui/src/components/Chat/hooks.js:1569-1581
   */
  chat_message_delete_all: {
    name: "chat_message_delete_all",
    direction: "receive",
    emitSchema: null,
    receiveSchema: chatMessageDeleteAllReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:890", "apps/elitea-ui/src/components/Chat/hooks.js:1569-1581"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:891
   *   - apps/elitea-ui/src/components/Chat/hooks.js:1540-1551
   */
  chat_message_sync: {
    name: "chat_message_sync",
    direction: "receive",
    emitSchema: null,
    receiveSchema: chatMessageSyncReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:891", "apps/elitea-ui/src/components/Chat/hooks.js:1540-1551"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:892
   *   - apps/elitea-ui/src/components/Chat/hooks.js:1599-1610
   */
  chat_participant_update: {
    name: "chat_participant_update",
    direction: "receive",
    emitSchema: null,
    receiveSchema: chatParticipantUpdateReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:892", "apps/elitea-ui/src/components/Chat/hooks.js:1599-1610"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:893
   *   - apps/elitea-ui/src/components/Chat/hooks.js:1613-1624
   */
  chat_conversation_name_updated: {
    name: "chat_conversation_name_updated",
    direction: "receive",
    emitSchema: null,
    receiveSchema: chatConversationNameUpdatedReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:893", "apps/elitea-ui/src/components/Chat/hooks.js:1613-1624"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:894
   *   - apps/elitea-ui/src/[fsd]/features/agent/lib/hooks/useApplicationChat.hooks.js:114-115
   *   - apps/elitea-ui/src/[fsd]/features/pipelines/lib/hooks/usePipelineChat.hooks.js:95-96
   *   - apps/elitea-ui/src/[fsd]/features/toolkits/lib/hooks/useToolkitChat.hooks.js:207-209
   */
  application_predict: {
    name: "application_predict",
    direction: "bidirectional",
    emitSchema: predictEmitSchema,
    receiveSchema: streamEnvelopeSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:894", "apps/elitea-ui/src/[fsd]/features/agent/lib/hooks/useApplicationChat.hooks.js:114-115", "apps/elitea-ui/src/[fsd]/features/pipelines/lib/hooks/usePipelineChat.hooks.js:95-96", "apps/elitea-ui/src/[fsd]/features/toolkits/lib/hooks/useToolkitChat.hooks.js:207-209"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:895
   */
  application_leave_rooms: {
    name: "application_leave_rooms",
    direction: "emit",
    emitSchema: leaveRoomsEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:895"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:896
   *   NOTE: Declared, grep-verified UNUSED anywhere else in the old app. Every server implementation seen so far answers it under the LITERAL "application_predict" name, so it is effectively emit-only. Typed by analogy to application_predict/chat_predict.
   */
  promptlib_predict: {
    name: "promptlib_predict",
    direction: "emit",
    emitSchema: predictEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:896"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:897
   *   NOTE: Declared, grep-verified UNUSED anywhere else in the old app.
   */
  promptlib_leave_rooms: {
    name: "promptlib_leave_rooms",
    direction: "emit",
    emitSchema: leaveRoomsEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:897"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:898
   *   - apps/elitea-ui/src/[fsd]/widgets/sidebar-root/ui/button/NotificationButton.jsx:43
   */
  notifications_notify: {
    name: "notifications_notify",
    direction: "receive",
    emitSchema: null,
    receiveSchema: notificationsNotifyReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:898", "apps/elitea-ui/src/[fsd]/widgets/sidebar-root/ui/button/NotificationButton.jsx:43"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:901
   *   - apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:7-23
   */
  chat_canvas_join: {
    name: "chat_canvas_join",
    direction: "emit",
    emitSchema: canvasJoinEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:901", "apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:7-23"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:902
   *   - apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:25-35
   */
  chat_canvas_leave_rooms: {
    name: "chat_canvas_leave_rooms",
    direction: "emit",
    emitSchema: canvasLeaveEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:902", "apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:25-35"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:903
   *   - apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:37-49
   */
  chat_canvas_edit: {
    name: "chat_canvas_edit",
    direction: "emit",
    emitSchema: canvasEditEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:903", "apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:37-49"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:905
   *   - apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:51-76
   */
  chat_canvas_sync: {
    name: "chat_canvas_sync",
    direction: "receive",
    emitSchema: null,
    receiveSchema: canvasSyncReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:905", "apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:51-76"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:906
   *   - apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:78-101
   */
  chat_canvas_error: {
    name: "chat_canvas_error",
    direction: "receive",
    emitSchema: null,
    receiveSchema: canvasErrorReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:906", "apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:78-101"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:907
   *   - apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:103-128
   */
  chat_canvas_detail: {
    name: "chat_canvas_detail",
    direction: "receive",
    emitSchema: null,
    receiveSchema: canvasDetailReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:907", "apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:103-128"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:908
   *   - apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:130-154
   *   NOTE: The Go server both triggers chat_canvas_join AND emits this back — see the S5 report for the emitter/listener asymmetry this documents.
   */
  chat_canvas_editor_joined: {
    name: "chat_canvas_editor_joined",
    direction: "receive",
    emitSchema: null,
    receiveSchema: canvasEditorJoinedReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:908", "apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:130-154"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:909
   *   - apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:156-180
   */
  chat_canvas_editors_change: {
    name: "chat_canvas_editors_change",
    direction: "receive",
    emitSchema: null,
    receiveSchema: canvasEditorsChangeReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:909", "apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:156-180"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:910
   *   - apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:182-206
   *   NOTE: The Go server emits this back to the room when chat_canvas_edit is received — same asymmetry class as chat_canvas_editor_joined.
   */
  chat_canvas_content_change: {
    name: "chat_canvas_content_change",
    direction: "receive",
    emitSchema: null,
    receiveSchema: canvasContentChangeReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:910", "apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:182-206"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:911
   *   - apps/elitea-ui/src/components/Chat/hooks.js:1478
   */
  chat_predict_attachment: {
    name: "chat_predict_attachment",
    direction: "receive",
    emitSchema: null,
    receiveSchema: streamEnvelopeSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:911", "apps/elitea-ui/src/components/Chat/hooks.js:1478"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:914
   *   - apps/elitea-ui/src/hooks/chat/useMCPParticipantStatusMonitor.js:24
   *   - apps/elitea-ui/src/hooks/application/useAgentMCPToolsStatusMonitor.js:73
   *   - apps/elitea-ui/src/[fsd]/features/toolkits/lib/hooks/useGetCurrentToolkitSchemas.hooks.js:40
   */
  mcp_status: {
    name: "mcp_status",
    direction: "receive",
    emitSchema: null,
    receiveSchema: mcpStatusReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:914", "apps/elitea-ui/src/hooks/chat/useMCPParticipantStatusMonitor.js:24", "apps/elitea-ui/src/hooks/application/useAgentMCPToolsStatusMonitor.js:73", "apps/elitea-ui/src/[fsd]/features/toolkits/lib/hooks/useGetCurrentToolkitSchemas.hooks.js:40"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:917
   *   - apps/elitea-ui/src/[fsd]/features/mcp/lib/hooks/useMcpAuthCheck.hooks.js:84-133
   */
  test_mcp_connection: {
    name: "test_mcp_connection",
    direction: "bidirectional",
    emitSchema: testMcpConnectionEmitSchema,
    receiveSchema: testMcpConnectionReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:917", "apps/elitea-ui/src/[fsd]/features/mcp/lib/hooks/useMcpAuthCheck.hooks.js:84-133"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:920
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:215-220
   */
  asr_start: {
    name: "asr_start",
    direction: "emit",
    emitSchema: asrStartEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:920", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:215-220"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:921
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:200-203
   */
  asr_audio_chunk: {
    name: "asr_audio_chunk",
    direction: "emit",
    emitSchema: asrAudioChunkEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:921", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:200-203"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:922
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:251,261
   */
  asr_stop: {
    name: "asr_stop",
    direction: "emit",
    emitSchema: asrStopEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:922", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:251,261"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:923
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:113-116,140
   */
  asr_transcript_delta: {
    name: "asr_transcript_delta",
    direction: "receive",
    emitSchema: null,
    receiveSchema: asrTranscriptDeltaReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:923", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:113-116,140"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:924
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:118-124,141
   */
  asr_transcript_done: {
    name: "asr_transcript_done",
    direction: "receive",
    emitSchema: null,
    receiveSchema: asrTranscriptDoneReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:924", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:118-124,141"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:925
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:136-138,144
   */
  asr_error: {
    name: "asr_error",
    direction: "receive",
    emitSchema: null,
    receiveSchema: asrErrorReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:925", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:136-138,144"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:926
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:126-129,142
   */
  asr_speech_started: {
    name: "asr_speech_started",
    direction: "receive",
    emitSchema: null,
    receiveSchema: asrSpeechStartedReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:926", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:126-129,142"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:927
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:131-134,143
   */
  asr_vad_flush: {
    name: "asr_vad_flush",
    direction: "receive",
    emitSchema: null,
    receiveSchema: asrVadFlushReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:927", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useStreamingSpeechRecognition.hooks.js:131-134,143"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:930
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js:465-472
   */
  tts_start: {
    name: "tts_start",
    direction: "emit",
    emitSchema: ttsStartEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:930", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js:465-472"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:931
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js:446,525
   */
  tts_stop: {
    name: "tts_stop",
    direction: "emit",
    emitSchema: ttsStopEmitSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:931", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js:446,525"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:932
   *   NOTE: Declared, grep-verified UNUSED anywhere else in the old app — only mentioned in a code COMMENT (useTextToSpeech.hooks.js:608). Dead constant.
   */
  tts_next: {
    name: "tts_next",
    direction: "emit",
    emitSchema: ttsNextUnusedSchema,
    receiveSchema: null,
    evidence: ["apps/elitea-ui/src/common/constants.js:932"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:933
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js:562-586,635
   */
  tts_audio_chunk: {
    name: "tts_audio_chunk",
    direction: "receive",
    emitSchema: null,
    receiveSchema: ttsAudioChunkReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:933", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js:562-586,635"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:934
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js:588-626,636
   */
  tts_done: {
    name: "tts_done",
    direction: "receive",
    emitSchema: null,
    receiveSchema: ttsDoneReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:934", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js:588-626,636"],
  },
  /**
   * evidence:
   *   - apps/elitea-ui/src/common/constants.js:935
   *   - apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js:628-633,637
   */
  tts_error: {
    name: "tts_error",
    direction: "receive",
    emitSchema: null,
    receiveSchema: ttsErrorReceiveSchema,
    evidence: ["apps/elitea-ui/src/common/constants.js:935", "apps/elitea-ui/src/[fsd]/features/chat/lib/hooks/useTextToSpeech.hooks.js:628-633,637"],
  },
} as const satisfies Record<SocketEventName, SocketEventContract>;

type EmittableEventName = {
  [K in SocketEventName]: (typeof SOCKET_EVENTS)[K]['direction'] extends 'emit' | 'bidirectional' ? K : never;
}[SocketEventName];

type ReceivableEventName = {
  [K in SocketEventName]: (typeof SOCKET_EVENTS)[K]['direction'] extends 'receive' | 'bidirectional' ? K : never;
}[SocketEventName];

export type { EmittableEventName, ReceivableEventName };

export type EmitPayloadOf<E extends EmittableEventName> = (typeof SOCKET_EVENTS)[E]['emitSchema'] extends z.ZodType<infer T>
  ? T
  : never;

export type ReceivePayloadOf<E extends ReceivableEventName> = (typeof SOCKET_EVENTS)[E]['receiveSchema'] extends z.ZodType<infer T>
  ? T
  : never;
