/**
 * Journey 12: Attach a file <5 MiB (JRNY-012)
 * Journey 13: Attach a file >5 MiB chunked with progress (JRNY-013)
 * Journey 26: Live-push stream drop → automatic reconnect (JRNY-026, rescoped
 *             from socket.io to SSE by #152 — see the block above the test)
 *
 * Spec §8.5 acceptance (from parity/manifest/chat.json JRNY-012/013/026).
 *
 * Every assertion below is anchored to something a stubbed route cannot
 * produce: the attach control's real accessible name, the capacity counter
 * the component computes from its own attachment state, the multipart FIELD
 * LAYOUT of the upload request, the bytes read back out of object storage
 * afterwards, and the status codes + JSON bodies elitea-main really returns
 * (202 chunk ack / 201 final array).
 *
 * CLOSED, and now asserted (this header used to record both as product gaps):
 *  - The PRE-SEND PREVIEW exists. `ChatBox` passes the staged list to
 *    `NewChatInput` and `buildChatBoxInputSlots` fills `slots.attachmentList`
 *    with `FileList`, so a picked file shows as a named, removable chip
 *    (`chat-attachment-chip-<i>`) before the message is sent — J12c below.
 *    Until that wiring landed the only pre-send signal was the attach row's
 *    remaining-capacity counter, and a user who picked a file saw nothing.
 *  - `isUploading`/`uploadProgress` now reach `UserInputFooter`, so the
 *    determinate `UploadProgressIndicator` can render. The percent itself is
 *    still asserted through the WIRE (two chunk requests, 202 then 201) and
 *    not the UI: on a loopback stack a 6 MiB upload finishes faster than a
 *    poll can catch an intermediate frame, so a UI assertion on it would be a
 *    flake, not a check.
 *
 * PRODUCT GAPS deliberately NOT asserted around (verified 2026-08-08):
 *  - Uploaded attachments never appear in the transcript: the optimistic
 *    user message carries no attachments and elitea-main does not persist a
 *    `chat_messages_attachment` row (see conversations/attachments.go), so
 *    `chat-artifact-file-card` is unassertable end-to-end.
 *  - Attachment rejection produces no UI at all (AttachmentButton's `onError`
 *    is never supplied by ChatBoxInputSlots), so there is no negative path.
 *
 * APP DEFECT found while writing this file — deep-linking a conversation
 * crashes the chat route, so these journeys drive `/app/chat` and let the
 * send create the conversation (which is the real JRNY-012 flow anyway):
 * GET /elitea_core/messages/... returns `{items,total,page,...}`, but
 * src/pages/chat/useChatPageData.ts:75 handles only `rows`/bare-array and
 * passes the paging OBJECT as `message_groups`; convertMessagesToChatHistory
 * then does `[...(messageGroups ?? [])]` and throws
 * "(e ?? []) is not iterable" into the error boundary.
 */
import { test, expect } from '@playwright/test';
import type { Request } from '@playwright/test';
import path from 'path';
import * as os from 'os';
import * as fs from 'fs';

import { checkA11y } from '../../fixtures/axe';
import { BASE_URL } from '../../../playwright.config';
import { API_BASE, DEFAULT_PROJECT_ID, deleteConversation } from '../../fixtures/api';

/** Exact accessible name of the attach control (`AttachmentButton.tsx`'s `ariaLabel` + en.json). */
const ATTACH_NAME = 'attach files';
/** MAX_ATTACHMENTS (src/shared/lib/attachments.ts). */
const MAX_ATTACHMENTS = 10;
/** CHUNK_SIZE (src/shared/api/upload.ts:22). */
const CHUNK_SIZE = 5 * 1024 * 1024;

/**
 * The upload target is the conversation's UUID, not its numeric id.
 *
 * That is not cosmetic and not a client preference: the endpoint keys the
 * stored object `{this segment}/{filename}`, and admission refuses any
 * attachment reference whose name is not prefixed by the conversation's UUID
 * (services/elitea-main/internal/application/agentexecution/attachments.go,
 * `currentTurnAttachments` — an authorisation check). The composer used to send
 * the numeric id here, which stored bytes no turn could ever be admitted with;
 * elitea-main now refuses a numeric id at this route outright, so a regression
 * shows up as a 400 rather than as an unmatched pattern.
 */
const UUID_SEGMENT = '[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}';
const uploadUrlRe = new RegExp(`/elitea_core/attachments/prompt_lib/${DEFAULT_PROJECT_ID}/(${UUID_SEGMENT})$`);

/**
 * Opens the composer's "+" menu and returns the attach row inside it.
 *
 * On the chat surface the attach control is NOT a bare button in the footer
 * any more — it is the first row of the "+" menu, which is what the product
 * shows (`PlusChatButton`; baseline `NewChatInput.jsx`'s `fromTheChat`
 * branch). It is a `menuitem`, not a `button`, because it sits inside a
 * `role="menu"`, and it carries its remaining capacity as VISIBLE text
 * rather than in a hover tooltip — so the capacity assertions below read the
 * row instead of `getByRole('tooltip')`.
 *
 * The menu is left open: the hidden file input only exists while it is
 * rendered, and nothing here closes it.
 */
async function openAttachRow(page: import('@playwright/test').Page) {
  const plus = page.getByTestId('plus-menu-button');
  await expect(plus).toBeEnabled({ timeout: 20_000 });
  await plus.click();
  const row = page.getByTestId('plus-menu-attachments');
  await expect(row).toBeVisible();
  return row;
}

/**
 * The AttachmentButton's OWN hidden input — scoped to the attach row's own
 * container (the row and the input are siblings in the component's fragment),
 * not to any file input that happens to exist on the shell.
 */
function attachInput(page: import('@playwright/test').Page) {
  return page
    .getByTestId('plus-menu-attachments')
    .locator('xpath=ancestor::div[1]')
    .locator('input[type="file"]');
}

/**
 * Closes the "+" menu if it is open.
 *
 * `openAttachRow` leaves it open because the hidden file input only exists
 * while it is rendered. Once a file is staged the composer grows by a chip
 * row, and the menu's Popper — anchored to the "+" button — then sits OVER the
 * text area and swallows every pointer event aimed at it. Closing it is what a
 * user does before typing anyway.
 */
async function closeAttachMenu(page: import('@playwright/test').Page): Promise<void> {
  const row = page.getByTestId('plus-menu-attachments');
  if ((await row.count()) === 0) return;
  await page.getByTestId('plus-menu-button').click();
  await expect(row).toHaveCount(0);
}

/** Types a question and sends it. Send stays disabled while the question is empty. */
async function send(page: import('@playwright/test').Page, question: string): Promise<void> {
  await closeAttachMenu(page);
  await page.getByTestId('chat-message-input').click();
  await page.keyboard.type(question);
  const sendButton = page.getByTestId('chat-send-button');
  await expect(sendButton).toBeVisible();
  await sendButton.click();
}

// ─────────────────────────────────────────────────────────────────────────────
// Journey 12: Attach a small file (<5 MiB)
// ─────────────────────────────────────────────────────────────────────────────
test('J12: attach a small file to a chat message', async ({ page }) => {
  const tmpFile = path.join(os.tmpdir(), 'e2e-small-file-attach.txt');
  // ASCII only: the stored object is read back as raw bytes and decoded as
  // latin1, so a non-ASCII byte would not round-trip through the comparison.
  const contents = 'E2E test attachment content - small file.';
  fs.writeFileSync(tmpFile, contents);
  const byteLength = Buffer.byteLength(contents);
  let createdConversationId: string | undefined;

  try {
    await page.goto(BASE_URL + '/app/chat');

    await checkA11y(page);

    const attach = await openAttachRow(page);
    await expect(attach).toHaveAttribute('aria-label', ATTACH_NAME);

    // Capacity counter BEFORE attaching — computed by
    // getRemainingAttachmentCapacity from the component's own state.
    await expect(attach).toContainText(`${MAX_ATTACHMENTS} left`);

    // The hidden input is the attach row's own sibling inside the menu.
    const input = attachInput(page);
    await expect(input).toHaveCount(1);
    await input.setInputFiles(tmpFile);

    // The file really entered attachment state: remaining capacity drops by one.
    // A build that swallowed the picked file keeps reporting 10. No hover
    // choreography any more — the counter is visible text on the row, so it
    // is readable without provoking a tooltip.
    await expect(attach).toContainText(`${MAX_ATTACHMENTS - 1} left`);

    // ...and the user can SEE it. The counter alone proves the state changed;
    // the chip proves the composer told the user which file is riding this
    // message. See J12c for the removal half.
    await expect(page.getByTestId('chat-attachment-chip-0')).toContainText('e2e-small-file-attach.txt');

    // Arm the upload watcher BEFORE sending: the send creates the conversation
    // over REST first, then uploads into it (resolveConversationForSend →
    // uploadPendingAttachments). The multipart body is only retained for
    // INTERCEPTED requests, so capture it in a pass-through route handler.
    const uploads: Array<{ readonly request: Request; readonly body: string }> = [];
    await page.route(/\/elitea_core\/attachments\/prompt_lib\//, async (route) => {
      const r = route.request();
      uploads.push({ request: r, body: r.postDataBuffer()?.toString('latin1') ?? '' });
      await route.continue();
    });

    await send(page, 'autotest_attach small file journey');

    // Exactly ONE upload request: a <5 MiB file takes the single-shot path.
    await expect.poll(() => uploads.length, { timeout: 30_000 }).toBe(1);
    const { request: req, body } = uploads[0]!;
    expect(req.url()).toMatch(uploadUrlRe);
    const convId = uploadUrlRe.exec(req.url())?.[1];
    expect(convId, 'the upload must be keyed by the conversation uuid').toMatch(new RegExp(`^${UUID_SEGMENT}$`));
    createdConversationId = convId;

    // Single-shot multipart body (upload.ts uploadSmallFile): `file` +
    // `overwrite_attachments=1`, and NO chunk fields — this file is under
    // CHUNK_SIZE, so a regressed threshold would show up as chunk_index here.
    expect(body).toContain('name="overwrite_attachments"');
    expect(body).toContain('filename="e2e-small-file-attach.txt"');
    expect(body).not.toContain('name="chunk_index"');

    // The server round trip: 201 + [{filepath, file_size}] (attachments.go).
    const resp = await req.response();
    expect(resp).not.toBeNull();
    expect(resp!.status()).toBe(201);
    const uploaded = (await resp!.json()) as Array<{ filepath?: unknown; file_size?: unknown }>;
    expect(Array.isArray(uploaded)).toBe(true);
    expect(uploaded).toHaveLength(1);
    expect(uploaded[0]!.filepath).toBe(`/chat-attachments/${convId}/e2e-small-file-attach.txt`);
    expect(uploaded[0]!.file_size).toBe(byteLength);

    // BYTE IDENTITY — the bytes STORED are the bytes CHOSEN.
    //
    // This used to be `expect(body).toContain(contents)` on the captured
    // multipart body. That is not portable: chromium exposes the whole encoded
    // body through CDP, but WebKit surfaces only the part HEADERS — the
    // received body there is literally
    //   `…name="file"; filename="e2e-small-file-attach.txt"\r\n
    //    Content-Type: text/plain\r\n\r\n\r\n--…`
    // with an EMPTY payload between the blank line and the next boundary,
    // because the File/Blob part is never read back into the protocol message.
    // The app sends the bytes on both engines; only Playwright's view differs.
    //
    // So the guarantee moves one hop further out, where it is stronger and
    // engine-independent: read the object back out of storage and compare
    // bytes. `file_size` above pins the LENGTH, this pins the CONTENT — a
    // build that uploaded a same-length wrong buffer passes the former and
    // fails this. Key layout from finalizeAttachment (conversations/
    // attachments.go:433) — bucket `chat-attachments`, key `<convId>/<name>`.
    const stored = await page.request.get(
      `${API_BASE}/artifacts/objects/${DEFAULT_PROJECT_ID}/chat-attachments/${convId}/e2e-small-file-attach.txt`,
    );
    expect(stored.status()).toBe(200);
    expect((await stored.body()).toString('latin1')).toBe(contents);

    // The send went through: the question is in the transcript.
    await expect(page.getByTestId('chat-message-list')).toContainText('autotest_attach small file journey');
  } finally {
    fs.unlinkSync(tmpFile);
    if (createdConversationId !== undefined) {
      await deleteConversation(page.request, createdConversationId).catch(() => undefined);
    }
  }
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 12b: the client-side attachment limit is really enforced
// ─────────────────────────────────────────────────────────────────────────────
test('J12b: attaching MAX_ATTACHMENTS files disables further attachment', async ({ page }) => {
  const files: string[] = [];
  for (let i = 0; i < MAX_ATTACHMENTS; i++) {
    const p = path.join(os.tmpdir(), `e2e-cap-attach-${i}.txt`);
    fs.writeFileSync(p, `capacity probe ${i}`);
    files.push(p);
  }

  try {
    await page.goto(BASE_URL + '/app/chat');
    const attach = await openAttachRow(page);

    await attachInput(page).setInputFiles(files);

    // isAtMaxCapacity → the row goes disabled, its input with it, and the
    // counter reads zero. `aria-disabled` rather than `toBeDisabled()`: a
    // `menuitem` is a div, so there is no `disabled` DOM property to read.
    await expect(attach).toHaveAttribute('aria-disabled', 'true');
    await expect(attachInput(page)).toBeDisabled();
    await expect(attach).toContainText('0 left');

    await checkA11y(page);
  } finally {
    for (const f of files) fs.unlinkSync(f);
  }
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 12c: the staged file is visible, named and removable BEFORE sending
// ─────────────────────────────────────────────────────────────────────────────
/**
 * The defect this guards is a wiring one, and it survived a full unit suite.
 * `useAttachmentState` held the file, `FileList` knew how to draw it, and the
 * send path uploaded it — but nothing joined them: `ChatBox` passed no
 * `attachments` prop to `NewChatInput`, and `NewChatInput` passed no
 * `attachmentList` slot to `UserInput`. Every component was correct on its own
 * and the composer still showed the user nothing.
 *
 * Two files, not one, because the removal assertion has to distinguish "the
 * right chip went" from "the list went".
 */
test('J12c: staged files show as removable chips before the message is sent', async ({ page }) => {
  const keep = path.join(os.tmpdir(), 'e2e-chip-keep.txt');
  const drop = path.join(os.tmpdir(), 'e2e-chip-drop.txt');
  fs.writeFileSync(keep, 'keep me');
  fs.writeFileSync(drop, 'drop me');

  try {
    await page.goto(BASE_URL + '/app/chat');
    const attach = await openAttachRow(page);
    await attachInput(page).setInputFiles([keep, drop]);
    await expect(attach).toContainText(`${MAX_ATTACHMENTS - 2} left`);

    // Close the "+" menu before touching a chip: the menu is a Popper behind a
    // ClickAwayListener, so the first mousedown anywhere outside it closes the
    // menu, and an assertion on the row afterwards would be asserting on an
    // unmounted element rather than on the product.
    await closeAttachMenu(page);

    const first = page.getByTestId('chat-attachment-chip-0');
    const second = page.getByTestId('chat-attachment-chip-1');
    await expect(first).toContainText('e2e-chip-keep.txt');
    await expect(second).toContainText('e2e-chip-drop.txt');

    // Remove the SECOND one. `onDeleteAttachment(index)` splices by index, so
    // a fix that removed the wrong entry (or cleared the list) fails here.
    await second.getByTestId('chat-attachment-remove-1').click();
    await expect(page.getByTestId('chat-attachment-chip-1')).toHaveCount(0);
    await expect(page.getByTestId('chat-attachment-chip-0')).toContainText('e2e-chip-keep.txt');

    // The capacity counter and the chip list agree — they read the same state,
    // and disagreement is how the original defect hid.
    await expect(await openAttachRow(page)).toContainText(`${MAX_ATTACHMENTS - 1} left`);

    await checkA11y(page);
  } finally {
    fs.unlinkSync(keep);
    fs.unlinkSync(drop);
  }
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 13: Attach a large file (>5 MiB, chunked with progress)
// ─────────────────────────────────────────────────────────────────────────────
test('J13: attach a large file chunked upload with progress', async ({ page }) => {
  const tmpFile = path.join(os.tmpdir(), 'e2e-large-file-attach.bin');
  const SIX_MIB = 6 * 1024 * 1024;
  fs.writeFileSync(tmpFile, Buffer.alloc(SIX_MIB, 0xab));
  let createdConversationId: string | undefined;

  try {
    const chunkRequests: Request[] = [];
    page.on('request', (r) => {
      if (r.method() === 'POST' && uploadUrlRe.test(r.url())) chunkRequests.push(r);
    });

    await page.goto(BASE_URL + '/app/chat');
    await checkA11y(page);

    const attach = await openAttachRow(page);

    await attachInput(page).setInputFiles(tmpFile);
    await expect(attach).toContainText(`${MAX_ATTACHMENTS - 1} left`);

    await send(page, 'autotest_attach large file journey');

    // 6 MiB / CHUNK_SIZE(5 MiB) = exactly 2 chunk POSTs (createFileChunks).
    // A regressed CHUNK_SIZE, or a build that stopped chunking, yields 1.
    // Settles at exactly 2 and STAYS there — a third request (e.g. a retry
    // loop, or a chunk splitter that over-slices) would break the protocol
    // just as badly as one request, and `poll` alone would not notice.
    await expect.poll(() => chunkRequests.length, { timeout: 60_000 }).toBe(Math.ceil(SIX_MIB / CHUNK_SIZE));
    await page.waitForTimeout(1_000);
    expect(chunkRequests).toHaveLength(2);

    const [first, second] = chunkRequests as [Request, Request];
    expect(first.url()).toBe(second.url());
    expect(first.url()).toMatch(uploadUrlRe);
    const convId = uploadUrlRe.exec(first.url())?.[1];
    expect(convId, 'the upload must be keyed by the conversation uuid').toMatch(new RegExp(`^${UUID_SEGMENT}$`));
    createdConversationId = convId;

    // Intermediate chunk → 202 in-progress ack. Its body echoes the very form
    // fields the client sent (chunk_index / total_chunks / file_id), which is
    // how this asserts the chunk protocol: Playwright does not retain the
    // multipart body of a 5 MiB request, so the echo is the evidence.
    const firstResp = await first.response();
    expect(firstResp).not.toBeNull();
    expect(firstResp!.status()).toBe(202);
    const ack = (await firstResp!.json()) as Record<string, unknown>;
    expect(ack['status']).toBe('chunk_received');
    expect(ack['chunk_index']).toBe(0);
    expect(ack['total_chunks']).toBe(2);
    expect(ack['file_id']).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/);

    // Final chunk → 201 with the assembled file: full 6 MiB, not one chunk.
    const secondResp = await second.response();
    expect(secondResp).not.toBeNull();
    expect(secondResp!.status()).toBe(201);
    const uploaded = (await secondResp!.json()) as Array<{ filepath?: unknown; file_size?: unknown }>;
    expect(uploaded).toHaveLength(1);
    expect(uploaded[0]!.filepath).toBe(`/chat-attachments/${convId}/e2e-large-file-attach.bin`);
    expect(uploaded[0]!.file_size).toBe(SIX_MIB);
  } finally {
    fs.unlinkSync(tmpFile);
    if (createdConversationId !== undefined) {
      await deleteConversation(page.request, createdConversationId).catch(() => undefined);
    }
  }
});

// ─────────────────────────────────────────────────────────────────────────────
// Journey 26: Live-push stream drops → client re-opens it automatically (SSE)
// ─────────────────────────────────────────────────────────────────────────────
//
// RESCOPED (#152). The original acceptance — "socket disconnect → sidebar
// indicator → reconnect → rooms rejoined" — describes an architecture the
// product no longer uses for this surface. Chat live-push moved to SSE (#92 /
// #93); socket.io is retained only for voice and video, neither of which the
// Go stack implements — the prototype server was deleted with #126 and
// elitea-main mounts no socket.io path at all. So there are no rooms to rejoin, and asserting the old wording
// against the new mechanism would be a test that reads as coverage and proves
// nothing. Rewritten here against SSE reconnect semantics rather than retired,
// because the underlying user-visible guarantee — "the connection drops, live
// updates come back on their own" — is unchanged; only the mechanism moved.
// parity/manifest/chat.json records the decision the way its schema provides
// for: JRNY-026 is `waived` with a `replacesBehaviour` naming this journey. The
// acceptance text is left byte-for-byte alone because uictl holds it immutable
// against the git baseline — editing it in place to describe SSE is exactly the
// "old wording pointing at the new mechanism" this rescope exists to avoid.
//
// The observable is genuinely different, and that difference IS the journey:
//
//   * socket.io reconnected AND re-emitted a join for every room it had been
//     in — hence "rooms rejoined". Nothing equivalent exists here.
//   * `EventSource` reconnects on a transport-level drop entirely inside the
//     browser: no application code runs, and `useEventSource` observes nothing
//     (see its own doc comment — an HTTP error STATUS is terminal, a dropped
//     connection is not). The only thing an end-to-end test can see is a new
//     request to the same stream URL.
//
// The precondition is inverted rather than dropped. It used to pin "this stack
// has no socket server", so a socket-shaped journey could not silently pass;
// it now pins "the notification SSE stream really is mounted and streaming", so
// this journey cannot pass against a stack where live push is dead. That was
// not a hypothetical: before #152 this endpoint answered 404 in every
// deployment — RouterConfig.CurrentNotificationEvents was mounted only by
// production_router.go, which NewRouter never reaches — and the client's only
// signal was a console warning.
//
// NOT COVERED HERE, deliberately:
//  - "a notification created mid-stream arrives live". No API creates a
//    notification: the /api/v2/notifications routes are list/update/delete only
//    (router.go:598-603) and every emitter is legacy Python, so driving one
//    would need a DB write no fixture here has.
//  - The socket.io half of JRNY-026 (voice/video). There is no server to
//    connect to, so it stays uncovered rather than faked; the connection dot's
//    real state is asserted instead, and it is CORRECT state on a chat-only
//    stack rather than a defect.

/** The stream `useNotificationsSSE` opens, minus its project id. */
const SSE_PATH_PREFIX = '/notifications/events/prompt_lib/';
const SSE_URL = `${API_BASE}${SSE_PATH_PREFIX}${DEFAULT_PROJECT_ID}`;

/**
 * Tells J26.1's OWN stream apart from the sidebar's. Both open the same url.
 *
 * `events.go` reads only `cursor` and `Last-Event-ID`, so an unknown query
 * parameter changes nothing the server does. The opening `notifications_ready`
 * frame still arrives, because the route sends it whenever no cursor is
 * supplied.
 */
const PROBE_QUERY = 'j26_probe=1';
const PROBE_URL = `${SSE_URL}?${PROBE_QUERY}`;

/** Every request to that url, with the probe marker or without it. */
const SSE_ROUTE = new RegExp(`${SSE_PATH_PREFIX}${DEFAULT_PROJECT_ID}(\\?|$)`);

/**
 * J26.1 — the platform contract: the stream is mounted, and a transport-level
 * drop is recovered by the browser with no application code involved.
 *
 * Driven through a real `EventSource` in the page rather than through the
 * sidebar's own subscription. The sidebar's half is J26.2 below, which asserts
 * the same stream from the app's side. The server, the auth cookie, the RBAC
 * check and the reconnect are all real here; only the subscriber is the
 * test's own.
 *
 * THE SIDEBAR'S STREAM IS REFUSED FOR THE LIFE OF THIS TEST, and that is what
 * keeps this journey deterministic on webkit.
 *
 * `events.go` admits at most FOUR concurrent streams per principal
 * (`newCurrentNotificationAdmission(64, 4)`), and answers the fifth with 429
 * plus `Retry-After: 2`. An HTTP status fails an `EventSource` for good, so
 * the probe reported `ready: false` with no reconnect left to make.
 *
 * The suite runs four parallel workers as ONE persona, and every worker's page
 * mounts `NotificationButton`. The member principal therefore already sits at
 * the cap. The probe was the fifth stream, and the page's own sidebar stream
 * was the fourth.
 *
 * Measured on 2026-08-21 against the real stack: hold three member streams
 * open, run this test alone, and it fails at `outcome.ready` every time. It
 * flaked the same way on `main` before this change set (run 32283253507).
 *
 * Aborting the sidebar's stream costs the server nothing, because the request
 * never leaves the browser. It leaves the probe as this page's ONLY stream,
 * which is the same budget every other journey uses.
 *
 * The body then re-attempts the whole scenario until a deadline, for the
 * refusal it cannot prevent — another worker can still hold the last slot.
 */
test('J26.1: the notification SSE stream is mounted, and a dropped connection is re-opened automatically', async ({ page }) => {
  test.setTimeout(90_000);

  // Only the PROBE's own requests are counted. Counting the sidebar's too
  // would let its retries satisfy the reconnect assertion below on their own.
  const probeRequests: string[] = [];
  const probeStatuses: number[] = [];
  page.on('request', (request: Request) => {
    if (request.url().includes(PROBE_QUERY)) probeRequests.push(request.url());
  });
  // An `EventSource` shows its consumer no status. Record the wire status so a
  // refusal names itself instead of arriving as a bare `false`.
  page.on('response', (response) => {
    if (response.url().includes(PROBE_QUERY)) probeStatuses.push(response.status());
  });

  // Drop the FIRST probe attempt at the transport level. `route.abort()` is a
  // connection failure, not an HTTP status — precisely the case WHATWG says
  // EventSource must retry. A status would be terminal, and a test built on one
  // would assert the opposite of this journey while looking identical.
  let dropped = 0;
  await page.route(SSE_ROUTE, async (route) => {
    if (!route.request().url().includes(PROBE_QUERY)) {
      await route.abort();
      return;
    }
    if (dropped === 0) {
      dropped += 1;
      await route.abort();
      return;
    }
    await route.continue();
  });

  await page.goto(BASE_URL + '/app/chat');

  /**
   * The scenario repeats until a deadline, and the re-attempt covers ONE
   * thing: the admission gate.
   *
   * The gate answers a refused stream with 429 and `Retry-After: 2`. That
   * refusal is not the condition under test, and the browser cannot recover
   * from it — an HTTP status fails an `EventSource` for good. Each attempt
   * proves the reconnect on its own, because the assertions below read the
   * LAST attempt's counters only.
   *
   * A refusal resolves in under a second, so the deadline buys many attempts
   * while another worker holds the last slot. A broken reconnect instead
   * spends the whole per-attempt budget, so the deadline stops after two. The
   * test fails in both of those cases, and it fails inside its own timeout.
   */
  const deadline = Date.now() + 40_000;
  const budgetMs = 20_000;
  let outcome = { ready: false, errors: 0, readyState: 0 };
  for (;;) {
    dropped = 0;
    probeRequests.length = 0;
    probeStatuses.length = 0;
    outcome = await page.evaluate(
      async ({ url, budget }) =>
        new Promise<{ ready: boolean; errors: number; readyState: number }>((resolve) => {
          const source = new EventSource(url, { withCredentials: true });
          let errors = 0;
          const finish = (ready: boolean): void => {
            const readyState = source.readyState;
            source.close();
            resolve({ ready, errors, readyState });
          };
          source.addEventListener('error', () => {
            errors += 1;
            // CLOSED means the browser gave up (an HTTP status) — report it
            // rather than sitting out the budget, so the failure names itself.
            if (source.readyState === EventSource.CLOSED) finish(false);
          });
          // The opening handshake of services/elitea-main/internal/api/v2/
          // notifications/events.go. Receiving it proves a real stream
          // from elitea-main, not a 404 the client tolerates in silence.
          source.addEventListener('notifications_ready', () => finish(true));
          setTimeout(() => finish(false), budget);
        }),
      { url: PROBE_URL, budget: budgetMs },
    );
    if (outcome.ready || Date.now() >= deadline) break;
    // The gate's own `Retry-After`, plus a margin for the slot to be released.
    await page.waitForTimeout(3_000);
  }

  expect(dropped).toBe(1);
  // The abort produced an error event, and the browser retried anyway: more
  // than one PROBE request is the only end-to-end evidence of the reconnect,
  // since no application code runs for it.
  expect(outcome.errors).toBeGreaterThan(0);
  expect(probeRequests.length).toBeGreaterThan(1);
  expect(
    outcome.ready,
    `probe stream statuses: ${probeStatuses.join(', ') || 'none'} (429 means the per-principal stream cap is full)`,
  ).toBe(true);
});

/**
 * J26.2 — the app's OWN subscription. Was red, for a reason that had nothing
 * to do with SSE wiring; fixed in #166/#167.
 *
 * `NotificationButton` passes `personal_project_id` (→ useNotificationsSSE),
 * and `app/session-store.ts` used to fill that field with the USER ID, with a
 * comment saying so: `/forward-auth/info` (internal/api/v2/auth/session.go)
 * returns only `authenticated`/`user_id`/`email`, so there was no project id
 * to send. The stream's authorize() resolves
 * `models.notifications.notifications.list` against whatever project id is in
 * the URL, so the sidebar asked about a project the user is not a member of
 * and got 403 — terminal for EventSource, no retry, one console warning.
 *
 * The field now comes from `GET /social/author` — the same endpoint the old
 * SPA reads it from (`slices/settings.js`'s `authorDetails` matcher) — and
 * that handler resolves it from the database instead of returning a hardcoded
 * "1" in every fallback branch (#167). Every branch of that resolver is
 * membership-checked, which is what makes this assertion hold rather than
 * happening to hold on a single-project deployment.
 *
 * THE ADMISSION GATE APPLIES HERE TOO, and unlike J26.1 this journey cannot
 * dodge it — the stream under test IS the sidebar's own.
 *
 * `events.go` admits four concurrent streams per principal and answers the
 * fifth with 429 (see J26.1's header). The suite runs parallel workers as ONE
 * persona and every page that reaches `/app/chat` mounts `NotificationButton`,
 * so the member principal sits at the cap and a page that arrives mid-overlap
 * is refused. Measured 2026-08-26 against the real stack: five pages on one
 * context yield statuses `[200], [200], [200], [200], [429, 429]`. Run alone
 * this test passed; run beside its neighbours it failed on `responses[0]`
 * being 429, which reads exactly like a broken subscription and is not one.
 *
 * So the scenario re-attempts until a slot frees, exactly as J26.1 does, and
 * each attempt starts from `about:blank` so the previous attempt's stream is
 * released rather than competing with the next one. What it does NOT do is
 * accept "429 or 200" — that would gut the assertion. A refusal only buys
 * another attempt; the last attempt still has to produce a real 200, and a
 * 403 (the #166/#167 regression this journey exists for) still fails, with
 * its status named.
 */
test('J26.2: the sidebar live-push subscription connects', async ({ page }) => {
  test.setTimeout(90_000);

  const requests: string[] = [];
  const failures: string[] = [];
  const responses: Array<{ url: string; status: number }> = [];
  page.on('request', (request: Request) => {
    if (request.url().includes(SSE_PATH_PREFIX)) requests.push(request.url());
  });
  // Chromium surfaces the rejected stream as a 403 RESPONSE; WebKit surfaces it
  // as a failed request ('cancelled') with no response at all. Both are watched
  // so this fails fast and by name on either engine — measured, not assumed: a
  // stream that really establishes yields a 200 response on both.
  page.on('requestfailed', (request: Request) => {
    if (request.url().includes(SSE_PATH_PREFIX)) {
      failures.push(`${request.url()} (${request.failure()?.errorText ?? 'unknown'})`);
    }
  });
  page.on('response', (response) => {
    if (response.url().includes(SSE_PATH_PREFIX)) {
      responses.push({ url: response.url(), status: response.status() });
    }
  });

  const deadline = Date.now() + 45_000;
  for (;;) {
    // The assertions below read the LAST attempt only, so each attempt starts
    // from an empty record. Same arrays throughout: the listeners above are
    // bound once, to these.
    requests.length = 0;
    failures.length = 0;
    responses.length = 0;

    await page.goto(BASE_URL + '/app/chat');

    // The client half is live regardless: the sidebar really does open a
    // stream. This part passes even when the stream is refused, and is what
    // makes the assertions below specific — the subscription exists, the
    // question is only whether it is authorized.
    await expect.poll(() => requests.length, { timeout: 20_000 }).toBeGreaterThan(0);
    // Wait for the attempt to SETTLE (admitted, refused, or dropped) rather
    // than reading a stream that is still opening.
    await expect
      .poll(() => responses.length + failures.length, { timeout: 15_000 })
      .toBeGreaterThan(0);

    // 403 is NOT retried: it is the regression this journey guards, it will
    // not clear on a later attempt, and it should fail fast rather than burn
    // the whole deadline first.
    const status = responses[0]?.status;
    if (status === 200 || status === 403 || Date.now() >= deadline) break;

    // Drop this page's own stream before re-attempting, so the retry competes
    // with the other workers only and not with itself.
    await page.goto('about:blank');
    await page.waitForTimeout(3_000);
  }

  expect(failures, `the sidebar's notification stream was dropped: ${failures.join(', ')}`).toEqual([]);
  expect(
    responses[0]?.status,
    `sidebar stream statuses: ${responses.map((r) => r.status).join(', ') || 'none'} (429 means the per-principal stream cap stayed full for the whole deadline; 403 is the authorize() regression this journey guards)`,
  ).toBe(200);
});

/**
 * The socket.io indicator, kept from the original journey and re-read rather
 * than deleted. On a chat-only stack `vite_socket_server` is empty, the noop
 * client reports `disconnected`, and the dot is telling the truth — socket.io
 * is retained for voice/video only. This is the tripwire that fires the day a
 * socket server is added and JRNY-026's voice/video half becomes writable.
 */
test('J26.3: the sidebar connection indicator reports the real socket state', async ({ page }) => {
  const cfg = await page.request.get(`${BASE_URL}/app/config.js`);
  expect(cfg.status()).toBe(200);
  expect(await cfg.text()).toContain('vite_socket_server: ""');

  await page.goto(BASE_URL + '/app/chat');

  // The dot's state is carried by its accessible name (MUI Tooltip title →
  // aria-label on the child), not by its visibility.
  const connectionDot = page.getByTestId('sidebar-connection-dot');
  await expect(connectionDot).toHaveAttribute('aria-label', 'Disconnected', { timeout: 20_000 });

  await checkA11y(page);
});
