/**
 * Canvas domain type — a collaboratively-edited code/content block attached
 * to a chat message. No OpenAPI schema exists for this resource (P6:
 * socket.io is the second API and is not spec-described).
 *
 * **S5 cross-check performed** (`shared/api/socket/events.ts`, now landed):
 * S5 could NOT independently confirm a payload shape for any of the canvas
 * receive events — `canvasSyncReceiveSchema`/`canvasDetailReceiveSchema` are
 * `{content: z.unknown().optional()}` (confirms the top-level `content`
 * unwrap this slice's `lib/normalise.ts` performs, but leaves `content`'s
 * inner shape opaque), `canvasErrorReceiveSchema` is a bare `z.unknown()`,
 * and — the field this module doc specifically flags —
 * **`canvasEditorsChangeReceiveSchema` is a bare `z.unknown()`**: no source
 * pinned `chat_canvas_editors_change`'s payload shape. `CanvasEditorPresence.
 * userName` below is therefore OLD-APP-CLIENT-EVIDENCED (what
 * `CanvasEditor.jsx` reads off an incoming payload) and NOT confirmed against
 * any server — whoever wires the real canvas feature onto socket data should
 * double-check this shape against an actual `chat_canvas_editors_change`
 * payload before trusting it, rather than assuming it is verified the way the
 * schema-backed entities are.
 *
 * Evidence:
 * - apps/elitea-ui/src/[fsd]/features/chat/api/chat.api.js:279-316,433-456 —
 *   `createCanvas`/`editCanvas`/`canvasDetails` REST endpoints.
 * - apps/elitea-ui/src/common/constants.js:900-910 — the 9 canvas socket
 *   event names.
 * - apps/elitea-ui/src/hooks/chat/useCanvasSocket.js:16-191 — payload shapes
 *   (`{project_id, canvas_uuid}` join; `{project_id, canvas_uuid, content}`
 *   edit; unwrapped `message.content` sync/detail).
 * - apps/elitea-ui/src/pages/NewChat/CanvasEditor.jsx:290-313,428-453 —
 *   editors-change payload `{editors, canvas_uuid, message_group_uuid}`,
 *   language-change request, leave-room payload.
 *
 * Canvas content is triple-denormalized in the old app (nested inside a
 * message's `message_items[].item_details.latest_version`, and mirrored
 * into three parallel conversation containers on every change) — see
 * `entities/conversation` and `entities/message` module docs, and
 * `lib/normalise.ts` in this slice for the flattening this type is the
 * OUTPUT of.
 */

/**
 * UNCONFIRMED against a live payload — see the module doc's S5 cross-check
 * note. Evidenced only from apps/elitea-ui/src/pages/NewChat/CanvasEditor.jsx
 * :298-301 (`editor.user_name`), which S5 could not independently verify
 * (`canvasEditorsChangeReceiveSchema` is a bare `z.unknown()`).
 */
export interface CanvasEditorPresence {
  readonly userName: string;
  /**
   * The server-derived principal id of this entry, when the roster carries
   * one. ADDED to the old client's shape, and the reason is a defect the name
   * alone cannot avoid: a display name is not an identity. A viewer whose name
   * the client does not hold — or holds in a different spelling from the one
   * the server puts on the roster — cannot recognise its OWN entry, and the
   * "somebody else holds this canvas" rule then fires against the only person
   * editing it. `undefined` means the roster did not name an id.
   */
  readonly userId?: string;
}

export interface Canvas {
  readonly uuid: string;
  readonly name?: string;
  /** e.g. `"code"`. */
  readonly canvasType?: string;
  readonly codeLanguage?: string;
  readonly content?: string;
  readonly editors?: readonly CanvasEditorPresence[];
  /** The chat message this canvas block lives inside. */
  readonly messageGroupUuid?: string;
}
