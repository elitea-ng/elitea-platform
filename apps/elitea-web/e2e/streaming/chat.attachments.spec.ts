/**
 * An attachment rides a REAL agent turn (#606), sent from the composer the way
 * a person sends it.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT THIS COVERS THAT `e2e/journeys/chat/chat.attachments.spec.ts` DOES NOT
 * ─────────────────────────────────────────────────────────────────────────────
 * That file runs in the `chromium`/`webkit` projects against
 * `docker-compose.e2e-standalone.yml` — a stack with NO runtime plane, NO agent
 * worker and NO model backend. It can therefore assert the UPLOAD PROTOCOL and
 * nothing past it: the multipart field layout, the 202/201 chunk acks, the
 * capacity counter, and the bytes read back out of object storage. An agent
 * turn cannot happen there at all, so what the platform DOES with an attached
 * file is unassertable in it.
 *
 * This file asserts the rest, on the full standalone stack, in three phases:
 * the composer's own send (phase 1), the platform contract underneath it
 * (phase 2), and what the transcript renders after a reload (phase 3). It
 * deliberately does NOT re-assert the upload protocol; the two files share a
 * stack-shaped boundary, not assertions.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * PHASE 1 — THE IDENTIFIER THE WHOLE FEATURE TURNS ON
 * ─────────────────────────────────────────────────────────────────────────────
 * A chat attachment is stored under `{conversationUUID}/{filename}` and it must
 * be, because the admission gate treats that prefix as an AUTHORISATION claim:
 *
 *     internal/application/agentexecution/attachments.go (currentTurnAttachments)
 *         if !validUUID(conversationUUID) ||
 *             !strings.HasPrefix(ref.Name, conversationUUID+"/") {
 *             return nil, ErrInvalidCurrentAgentStart
 *         }
 *
 * — "this file was uploaded to this conversation", the one thing standing
 * between a caller and reading any object in the project's attachment bucket
 * into a model's context. See that function's own doc comment.
 *
 * THIS PHASE PINNED A DEFECT UNTIL 2026-08-29, and the shape of the fix is why
 * the assertions below are written as equalities against BOTH identifiers. The
 * composer uploaded to the conversation's NUMERIC id
 * (`resolveUploadConversationId` returned `createdConversation?.id`), while
 * `finalizeAttachment` keyed the object by that URL parameter verbatim under a
 * comment asserting it was already the UUID. So the upload answered 201, the
 * start that followed was refused 400 ("Invalid agent execution request",
 * `api/v2/agentexecution/route.go`) BEFORE `admissions.Submit` ran — no message
 * group was ever written — and the user watched an upload succeed and lost the
 * question entirely, while the bytes sat in the bucket until retention expired
 * them. Measured against the live standalone stack on 2026-08-29: the same
 * start body was admitted 200 with a UUID-keyed filepath and refused 400 with
 * an id-keyed one, everything else equal.
 *
 * THE CLIENT WAS THE HALF THAT MOVED. `resolveUploadConversationId` now
 * resolves the UUID on both paths — `createdConversation.uuid` for a
 * conversation this send just created, `deps.conversationUuid` for one already
 * open — and elitea-main refuses a NUMERIC conversation id at the upload route
 * outright rather than storing an object no turn can use. A regression in
 * either half fails below on its own assertion and says which one moved.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * PHASE 2 — THE PER-LEG CONTRACT, READ OUT OF THE CODE AND THEN MEASURED
 * ─────────────────────────────────────────────────────────────────────────────
 * Phase 2 adds no traffic of its own: it reads what phase 1's composer send
 * produced. Everything under it is real — admission, dispatch, the worker, the
 * model call — and the only thing this file supplies is the file and the
 * question.
 *
 * ELITEA-MAIN (both legs). The admission transaction — not the worker — writes
 * the attachment items, so the question group and its files exist together or
 * not at all. Each item gets a `content` SCAFFOLD: one `{"type":"text"}` chunk
 * naming the bucket, the filename and the filepath, carrying an
 * `elitea_attachment` marker saying the document's text is not extracted yet
 * (`attachments.go:242-275`, `attachmentContentScaffold`). Those chunks are
 * CONCATENATED, flat, into the runtime input's `input_attachments`
 * (`currentTurnInputAttachments`, reached from `start.go:325` and its adhoc
 * twin `adhoc.go:329`). All of that happens before either worker sees the
 * command, which is why the item assertions are shared and only the terminal
 * state forks.
 *
 * ONE CONTRACT, BOTH LEGS — and it was two until 2026-08-29. Each worker
 * splices the attachment chunks into the human message after the user's own
 * text, and each READS a chunk still flagged `needs_content_extraction` and
 * appends its text as a second chunk. So on both legs the file's CONTENT
 * reaches the model, and the mock — which echoes every `type: "text"` part of
 * the last user message (`deploy/mock-llm/server.py:187-215`, `_message_text`
 * joins them) — hands it straight back. That is what makes the unique token
 * INSIDE the uploaded file a proof of arrival rather than of intent, and it is
 * why there is now a single assertion pair below instead of a fork.
 *
 * PYTHON (`E2E_WORKER=python`) reads through the SDK artifact toolkit's
 * `read_multiple_files` (`elitea_worker/agents/sdk_adapter.py:529-624`,
 * `_read_attachment_documents`), spliced by `sdk_adapter.py:926-937`.
 *
 * RUST (`E2E_WORKER=rust`) has no vault and no `artifact` toolkit family
 * (`toolkits/materialize.rs` rejects it by kind), so it reads over the ONE
 * channel it holds — elitea-main's private mTLS content listener, on the same
 * live claim and fence that authorized the turn:
 *
 *     services/elitea-main/internal/infra/storage/runtime_attachment_object.go
 *         POST …/runtime-context/attachments/{bucket}/{name}
 *     services/elitea-worker-rust/src/agents/attachments.rs
 *         pending_attachment_reads / read_attachment_documents
 *         resolved_attachment_chunks / append_attachment_parts
 *
 * That route authorizes on the CLAIM, never on the request: the project comes
 * from the claimed execution row and the object key must be prefixed by the
 * claim's own conversation — the same sentence admission enforces on the way in
 * (`currentTurnAttachments`). This spec exercises the permitted case; the
 * refusals are pinned by that route's own unit tests, which is where a 403 can
 * be asserted without a second conversation's worth of fixture.
 *
 * A FORMAT THAT IS NOT ALREADY TEXT is extracted by elitea-main now
 * (internal/infra/extract: PDF, DOCX, PPTX, XLSX), and `chat.attachment-pdf.spec.ts`
 * holds the PDF journey. A file with no text the platform can read is answered
 * 422 with a reason, and the worker adds a note that says the file could not
 * be read and why, rather than failing the turn — pylon's own rule for a file
 * the platform cannot read (`rpc/chat_all.py:384-386`). That path is
 * unit-tested rather than staged here; this spec keeps the .txt shape.
 *
 * That read is NOT the `attachments` INTERNAL TOOL — that toggle is on the
 * runtime's platform list and is merely SKIPPED with an
 * `agent_internal_tool_skipped` warning (`internal_tools.rs:41` and the
 * `PLATFORM_INTERNAL_TOOLS` comment above it). The two are independent: content
 * extraction is decided by the input chunk's marker, with the toggle off.
 *
 * `E2E_WORKER` comes from `scripts/chat-stream-e2e.sh`, the one place that
 * knows which runtime the stack runs; the local default is the native runtime,
 * matching the long-lived dev stack. It no longer selects an ASSERTION here —
 * both legs are held to the same contract — and is kept only because a failure
 * message that names the runtime under test is worth more than one that does
 * not.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * PHASE 3 — THE RELOAD, AND THE SECOND HALF THAT USED TO BE MISSING
 * ─────────────────────────────────────────────────────────────────────────────
 * The chat page reads the FLAT transcript route: `useChatPageData.ts` runs
 * `conversationApi.useMessageList`, i.e.
 * `GET /elitea_core/messages/prompt_lib/{p}/{c}`, and hands its rows to
 * `ChatBox` AS `message_groups`. Those rows used to carry
 * `{id, uid, role, content, metadata}` and nothing else — `ListMessages` had no
 * item projection at all, and the items lived ONLY on the details route, and
 * only when `messages_limit` was supplied. So `ChatMessage.messageItems` was
 * always empty here, `UserMessage`'s `findAttachmentItems` always returned `[]`,
 * and `MessageAttachmentList` rendered nothing: everything #606 built on the
 * read side had no producer on the page that needed it, while its unit tests
 * passed because they hand `UserMessage` the `messageItems` the real page never
 * got.
 *
 * `ListMessages` now projects each group's `attachment_message` items in the
 * SAME shape the details route embeds them in, so this phase asserts what the
 * reader SEES: the card, the file's name on it, and the download control that
 * makes the stored bytes reachable.
 *
 * The card's label carries the conversation-uuid PREFIX (`{uuid}/{filename}`),
 * because the item's `name` is the object KEY — that is what addresses the
 * stored object and what `DeleteMessage` hands back to the object store. Hence
 * `toContainText(fileName)` rather than an equality: this asserts the file the
 * user attached is named on the card, without pinning a display decision that
 * belongs to `getAttachmentName`.
 *
 * ─────────────────────────────────────────────────────────────────────────────
 * WHAT IS DELIBERATELY NOT ASSERTED
 * ─────────────────────────────────────────────────────────────────────────────
 *  - The chunked (>5 MiB) upload protocol, the multipart field layout and the
 *    attach row's capacity ceiling. All three are JRNY-012/013's, they are
 *    stack-independent, and a second copy here would drift out of one of them.
 *  - #607's content WRITEBACK — the worker reporting extracted text back onto
 *    the stored item. The native runtime explicitly returns none
 *    (`agents/result.rs:103-107`, `attachment_contents: Vec::new()`), so the
 *    stored `content` array is asserted at its ADMISSION shape, the one both
 *    legs agree on.
 */
import { expect, test } from '@playwright/test';
import * as fs from 'fs';
import * as os from 'os';
import * as path from 'path';

import { BASE_URL } from '../../playwright.config';
import {
  attachmentItemsOf,
  expectStoredAssistantAnswer,
  readStoredAssistantAnswer,
  readStoredMessageGroups,
} from '../fixtures/api';

/**
 * Route shapes, matched WITHOUT pinning a project id — the chat driver acts
 * inside its OWN personal project (#290), so a hardcoded `1` would assert
 * against a project this persona never opens. Every id below is read back off
 * the app's own requests.
 */
const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const UPLOAD_RE = /\/elitea_core\/attachments\/prompt_lib\/(\d+)\/([^/?]+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;

/** The model `seed-llm` seeds into every personal project (see `chat.streaming.spec.ts`). */
const MODEL_NAME = process.env['E2E_CHAT_MODEL'] || 'E2E-MOCK-MODEL';

/** Which runtime answers the turn. `chat-stream-e2e.sh` sets it; the local default is the dev stack's native runtime. */
const WORKER = process.env['E2E_WORKER'] ?? 'rust';

/** The bucket `finalizeAttachment` keys chat uploads into when no project policy overrides it. */
const ATTACHMENT_BUCKET = 'chat-attachments';

/** `chat_conversations.uuid` as it appears in a URL — the identifier an attachment is keyed by. */
const UUID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

test('an attachment rides a real turn: the composer sends it, the platform stores it, the reload shows it', async ({
  page,
}) => {
  // Three phases, one of them a whole agent turn. Every wait below is bounded
  // well under this, so a real hang fails on its own step, not on the clock.
  test.setTimeout(300_000);

  const stamp = `${String(Date.now())}${String(Math.floor(Math.random() * 1e6))}`;
  // The token lives INSIDE the file and nowhere else — not in the prompt, not
  // in the filename. That is what makes "the answer contains it" a statement
  // about the file's CONTENT having been read, rather than about the request
  // echoing something this test typed. ASCII only: the object is read back as
  // raw bytes and compared as latin1.
  const token = `ATTACHTOKEN${stamp}`;
  const fileName = `e2e-turn-attachment-${stamp}.txt`;
  const contents = [
    'E2E attachment content probe.',
    `The secret word is ${token}.`,
    'End of file.',
  ].join('\n');
  const tmpFile = path.join(os.tmpdir(), fileName);
  fs.writeFileSync(tmpFile, contents);

  // The prompts carry NO token: only the attachment does. Short enough to
  // survive `ChatBox`'s 50-char conversation-name truncation, which keeps a
  // failed run's leftovers identifiable in the store.
  const prompt = `autotest attach ${stamp}`;
  expect(prompt.length, 'the prompt must fit the 50-char conversation-name truncation').toBeLessThanOrEqual(50);

  let projectId = '';
  let conversationId = '';

  try {
    // ═══ PHASE 1 — the composer's own send, end to end ═══════════════════
    await page.goto(BASE_URL + '/app/chat');
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });

    // The seeded model is not decoration: an adhoc turn resolves against a
    // `dummy` participant carrying it, and the start route reads
    // `llm_settings.model_name`. With nothing selected the send is refused
    // before the attachment reaches admission at all.
    await page.getByTestId('model-selector-button').click();
    const modelOption = page.getByRole('option').filter({ hasText: MODEL_NAME }).first();
    await expect(modelOption, `the seeded model ${MODEL_NAME} must be offered`).toBeVisible({ timeout: 20_000 });
    await modelOption.click();
    await expect(page.getByTestId('model-selector-name')).toContainText(MODEL_NAME, { timeout: 10_000 });

    // ── Attach through the REAL control ────────────────────────────────────
    // On the chat surface the attach control is the first row of the composer's
    // "+" menu, not a bare footer button, and its hidden file input only exists
    // while that menu is rendered — so the menu is opened and left open. Scoped
    // to the row's own container (the row and the input are siblings in
    // `AttachmentButton`'s fragment) rather than to any file input on the shell.
    const plus = page.getByTestId('plus-menu-button');
    await expect(plus).toBeEnabled({ timeout: 20_000 });
    await plus.click();
    const attachRow = page.getByTestId('plus-menu-attachments');
    await expect(attachRow).toBeVisible();
    const attachInput = attachRow.locator('xpath=ancestor::div[1]').locator('input[type="file"]');
    await expect(attachInput).toHaveCount(1);
    await attachInput.setInputFiles(tmpFile);

    // The file really entered attachment state. The counter is computed by
    // `getRemainingAttachmentCapacity` from the component's OWN state, so a
    // build that swallowed the picked file keeps reporting the full capacity
    // and this fails here — before the send, where the diagnosis is cheap.
    await expect(attachRow, 'the picked file must enter the composer’s attachment state').toContainText('9 left');

    // All three responses are armed BEFORE the click, so a create that 4xx's,
    // an upload that never happens or a start that never fires each fail on
    // their own assertion rather than timing out later against a blank list.
    const created = page.waitForResponse(
      (r) => CONVERSATIONS_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
      { timeout: 45_000 },
    );
    const uploaded = page.waitForResponse(
      (r) => UPLOAD_RE.test(new URL(r.url()).pathname) && r.request().method() === 'POST',
      { timeout: 60_000 },
    );
    const started = page.waitForResponse(
      (r) => START_RE.test(r.url()) && r.request().method() === 'POST',
      { timeout: 60_000 },
    );

    const input = page.getByTestId('chat-message-input');
    await expect(input).toBeEditable({ timeout: 20_000 });
    await input.fill(prompt);
    const sendButton = page.getByTestId('chat-send-button');
    await expect(sendButton).toBeEnabled({ timeout: 10_000 });
    await sendButton.click();

    const createdResponse = await created;
    expect(createdResponse.status(), 'the send must create a real conversation').toBe(201);
    projectId = CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? '';
    expect(projectId, 'the conversation must belong to a project').not.toBe('');
    const conversation = (await createdResponse.json()) as { id?: string; uuid?: string };
    conversationId = String(conversation.id ?? '');
    const conversationUuid = String(conversation.uuid ?? '');
    expect(conversationId, 'the conversation must carry a server-assigned id').toMatch(/^\d+$/);
    expect(
      conversationUuid,
      'the conversation must carry a uuid — everything below is keyed by it',
    ).toMatch(UUID_RE);

    // ── The upload, keyed by the identifier admission authorises against ────
    const uploadResponse = await uploaded;
    expect(
      uploadResponse.status(),
      `the composer’s attachment must upload: ${(await uploadResponse.text()).slice(0, 300)}`,
    ).toBe(201);
    const uploadTarget = UPLOAD_RE.exec(new URL(uploadResponse.url()).pathname)?.[2] ?? '';
    const uploadBody = (await uploadResponse.json()) as Array<{ filepath?: unknown; file_size?: unknown }>;
    expect(uploadBody, 'a single-shot upload answers with one entry').toHaveLength(1);
    const composerFilepath = String(uploadBody[0]?.filepath ?? '');
    expect(composerFilepath, 'the stored filepath is /{bucket}/{conversation uuid}/{file}').toBe(
      `/${ATTACHMENT_BUCKET}/${conversationUuid}/${fileName}`,
    );
    expect(uploadBody[0]?.file_size, 'the stored length must be the file’s own').toBe(
      Buffer.byteLength(contents),
    );

    // THE FIX, named against both identifiers rather than against a shape. The
    // composer must key the object by the conversation's UUID; keying it by the
    // numeric id stores bytes admission will refuse, and used to lose the whole
    // turn with them.
    expect(
      uploadTarget,
      'the composer must upload keyed by the conversation uuid (resolveUploadConversationId)',
    ).toBe(conversationUuid);
    expect(
      uploadTarget,
      'the composer has regressed to uploading by the conversation’s NUMERIC id: admission will refuse ' +
        'every attachment keyed this way, and the question is lost with it',
    ).not.toBe(conversationId);

    // BYTE IDENTITY, one hop further out than the request body: read the object
    // back through the artifacts route and compare. `file_size` pins the
    // LENGTH, this pins the CONTENT — and it is what makes phase 2's token
    // assertion meaningful, because the token is provably in storage before
    // anything claims a model saw it.
    const storedComposerObject = await page.request.get(
      `${BASE_URL}/api/v2/artifacts/objects/${projectId}${composerFilepath}`,
    );
    expect(storedComposerObject.status(), 'the uploaded object must be readable back out of storage').toBe(200);
    expect((await storedComposerObject.body()).toString('latin1')).toBe(contents);

    // ── …and the turn is ADMITTED ──────────────────────────────────────────
    const startResponse = await started;
    expect(
      startResponse.status(),
      'the composer’s attachment send must be admitted. A 400 here is the identifier defect returning: ' +
        'the admission gate refuses any attachment whose object key is not prefixed by the conversation ' +
        `uuid, and the refusal happens BEFORE admissions.Submit, so the question is lost too. Body: ${(await startResponse.text()).slice(0, 300)}`,
    ).toBe(200);
    const startBody = (await startResponse.json()) as { events_url?: string };
    expect(startBody.events_url, 'the start response must carry the stream to read').toMatch(
      /^\/api\/v2\/executions\/\d+\/[0-9a-f]+\/events$/,
    );

    // ═══ PHASE 2 — what the platform stored for that turn ═════════════════
    //
    // No traffic of its own: the turn phase 1 sent is the turn read here.

    // ── The question group carries the attachment item ─────────────────────
    const groups = await readStoredMessageGroups(page, projectId, conversationId);
    const attachments = attachmentItemsOf(groups);
    expect(
      attachments,
      'the admission must write exactly one attachment_message item onto the question group',
    ).toHaveLength(1);
    const details = attachments[0]?.details ?? {};
    // The item's name is the OBJECT KEY, conversation prefix included — that is
    // what makes it address the stored object, and what `DeleteMessage` later
    // hands back to the object store.
    expect(details['name']).toBe(`${conversationUuid}/${fileName}`);
    expect(details['bucket']).toBe(ATTACHMENT_BUCKET);
    expect(details['filepath'], 'the item must address the same object the upload stored').toBe(
      composerFilepath,
    );
    // `attachment_type` is pylon's three-value column, not a MIME type. A .txt
    // is `document` — not `text` — because this path extracts nothing at
    // admission, which is the value pylon gives it pre-extraction too
    // (`AttachmentKind`'s own note).
    expect(details['attachment_type'], 'a .txt is classified document, not image and not text').toBe('document');

    // The content SCAFFOLD, decoded — the very chunks that become
    // `input_attachments`. Asserted as an ARRAY of chunks, not a string: the
    // client walks it looking for an `image_url` entry, and a scalar here would
    // break every inline image render.
    const content = details['content'];
    expect(Array.isArray(content), 'item_details.content must be a decoded chunk ARRAY').toBe(true);
    const chunks = content as ReadonlyArray<Record<string, unknown>>;
    expect(chunks.length, 'the scaffold is one chunk, not an empty array').toBeGreaterThanOrEqual(1);
    expect(chunks[0]?.['type']).toBe('text');
    const header = String(chunks[0]?.['text'] ?? '');
    expect(header, 'the scaffold names the bucket the file lives in').toContain(`Bucket: ${ATTACHMENT_BUCKET}`);
    expect(header, 'the scaffold names the file, which is how the model learns it exists').toContain(fileName);
    // The extraction marker is what tells a worker this document's text has NOT
    // been read yet. Its absence would make the python leg's read a silent
    // no-op that still looked like success.
    const marker = chunks[0]?.['elitea_attachment'] as Record<string, unknown> | undefined;
    expect(marker, 'a document chunk must carry the extraction marker').toBeDefined();
    expect(marker?.['needs_content_extraction']).toBe(true);
    expect(marker?.['bucket']).toBe(ATTACHMENT_BUCKET);
    expect(marker?.['name']).toBe(`${conversationUuid}/${fileName}`);

    // ── The terminal state, ONE contract on both legs ─────────────────────
    //
    // Two assertions, weakest first, so a partial failure names its own half.
    // The split IS the boundary between rendering the file and reading it.
    //
    // 1. The FILE was ANNOUNCED to the model. The document's header chunk names
    //    the file and reaches the prompt whether or not the read succeeded, and
    //    the mock echoes every text part of the last user message — so the
    //    filename in the stored answer proves the chunk was spliced in at all.
    //    This is a real answer, not a refusal: `expectStoredAssistantAnswer`
    //    requires a non-`is_error` row, so a turn that died fails here.
    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 120_000,
      contains: fileName,
      message:
        `the ${WORKER} worker neither announced nor refused the attached file. If it REFUSED, ` +
        'the runtime rejected a non-empty input_attachments; if it ANSWERED without the ' +
        'filename, the header chunk never reached the human message ' +
        '(rust: agents/attachments.rs append_attachment_parts; python: sdk_adapter.py:926-937)',
    });
    // 2. The file's CONTENT reached the model. The token exists only inside the
    //    uploaded bytes — not in the prompt, not in the filename — so it can
    //    only be here if the worker really read the object and appended its
    //    text as a second chunk. Read once rather than polled: the assertion
    //    above already waited for the row to be finalised.
    //
    //    ON THE RUST LEG this is the claim-authorized object route end to end:
    //    the marker's (bucket, name) reached
    //    `POST …/runtime-context/attachments/{bucket}/{name}` over the live
    //    claim, elitea-main checked the claim's project AND conversation
    //    against the key, served the bytes as text, and
    //    `resolved_attachment_chunks` spliced them in beside the header. A
    //    failure here with assertion 1 passing means the announce works and the
    //    READ does not — check the worker's
    //    `agent_input_attachment_read_failed` event for the refusal code.
    const answer = await readStoredAssistantAnswer(page, projectId, conversationId);
    expect(
      answer.content,
      'the attached file was announced to the model but its CONTENT never arrived: the worker ' +
        'produced no text for a chunk still flagged needs_content_extraction, so the model was ' +
        'told a file exists and never shown it',
    ).toContain(token);

    // ═══ PHASE 3 — the reload, where the reader finally sees the file ═════
    //
    // The question survives, which is the persistence claim, AND so does the
    // attachment: the card is rendered from `message_items` on the flat
    // transcript route, which is the projection `ListMessages` gained for
    // exactly this page.
    await page.goto(`${BASE_URL}/app/chat/${conversationId}`, { waitUntil: 'domcontentloaded' });
    await expect(page.getByText('Something went wrong.')).toHaveCount(0);
    await expect(page.getByTestId('chat-input')).toBeVisible({ timeout: 30_000 });
    const list = page.getByTestId('chat-message-list');
    await expect(list.getByTestId('user-message').filter({ hasText: prompt }).first()).toBeVisible({
      timeout: 30_000,
    });

    const cards = list.getByTestId('chat-artifact-file-card');
    await expect(
      cards,
      'the stored attachment did not reach the chat transcript. The page reads ListMessages: if its ' +
        'rows carry no message_items, `UserMessage`’s findAttachmentItems has nothing to filter and ' +
        'MessageAttachmentList renders null — the read-path half of #606',
    ).toHaveCount(1);
    const card = cards.first();
    await expect(
      card,
      'the card must name the file the user attached (its label is the item’s object key, prefix included)',
    ).toContainText(fileName);

    // The DOWNLOAD control, which is what makes the stored bytes reachable from
    // the transcript at all — a card nobody can open is not a rendered
    // attachment, it is a label.
    //
    // ASSERTED ONLY AFTER THE HOVER, because before it the control does not
    // exist as far as any user or any assistive technology is concerned:
    // `NormalAttachment` renders the whole action row `display: none` until
    // `isHovering`, and a `display: none` subtree is excluded from the
    // accessibility tree, so `getByRole` matches ZERO elements rather than one
    // hidden one (measured — this assertion first ran before the hover and
    // failed with "Received: 0" on a card that was rendering perfectly).
    await card.hover();
    const download = card.getByRole('button', { name: 'Download attachment' });
    await expect(
      download,
      'the rendered card exposes no download control, so the stored bytes are unreachable from the transcript',
    ).toBeVisible({ timeout: 10_000 });
  } finally {
    fs.unlinkSync(tmpFile);
    if (projectId !== '' && conversationId !== '') {
      await page.request
        .delete(`${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`)
        .catch(() => undefined);
    }
  }
});
