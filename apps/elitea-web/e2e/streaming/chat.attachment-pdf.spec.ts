/**
 * A REAL PDF attached in chat reaches the model as its text.
 *
 * Before the attachment-extraction change, elitea-main served an attachment to
 * the native runtime only when its raw bytes were valid UTF-8 and at most
 * 128 KiB. A PDF was refused with 422, the worker logged the refusal as
 * `dependency_unavailable`, and the model saw the file name and a header that
 * falsely said the content was embedded. The model then answered a "summarise
 * this" request about a document it had never read.
 *
 * Now elitea-main extracts the PDF (internal/infra/extract, PDFium in
 * WebAssembly) and serves its text with a page map; the native worker inlines
 * it within the turn's budget and marks it COMPLETE or PARTIAL
 * (services/elitea-worker-rust/src/agents/attachment_context.rs). The Python
 * worker reads the same file through the SDK artifact toolkit.
 *
 * THE PROOF. The PDF below is generated here and carries a token that exists
 * only inside its text layer — not in the prompt and not in the file name. The
 * mock model echoes every text part of the last user message
 * (deploy/mock-llm/server.py), so the token in the stored answer proves that
 * the PDF's TEXT reached the model. The answer must also not carry the
 * "could not be read" note, which is what an unreadable file produces.
 *
 * `chat.attachments.spec.ts` covers the upload protocol, the stored item and
 * the reload card for a .txt; this spec adds only what a PDF changes.
 */
import { expect, test } from "@playwright/test";
import * as fs from "fs";
import * as os from "os";
import * as path from "path";

import { BASE_URL } from "../../playwright.config";
import {
  expectStoredAssistantAnswer,
  readStoredAssistantAnswer,
} from "../fixtures/api";

const CONVERSATIONS_RE = /\/elitea_core\/conversations\/prompt_lib\/(\d+)$/;
const UPLOAD_RE = /\/elitea_core\/attachments\/prompt_lib\/(\d+)\/([^/?]+)$/;
const START_RE = /\/elitea_core\/messages\/prompt_lib\/(\d+)\/[0-9a-f-]+/;
const MODEL_NAME = process.env["E2E_CHAT_MODEL"] || "E2E-MOCK-MODEL";
const WORKER = process.env["E2E_WORKER"] ?? "rust";

/**
 * A one-page PDF 1.4 with a Helvetica text layer and a correct cross-reference
 * table — a real PDF any reader opens, not text with a .pdf name. The same
 * shape is extracted in services/elitea-main/internal/infra/extract's tests.
 */
function onePagePdf(lines: readonly string[]): Buffer {
  const escape = (value: string) =>
    value.replace(/[\\()]/g, (character) => `\\${character}`);
  const stream = [
    "BT",
    "/F1 12 Tf",
    "72 720 Td",
    "16 TL",
    ...lines.map((line) => `(${escape(line)}) Tj T*`),
    "ET",
  ].join("\n");
  const objects = [
    "<< /Type /Catalog /Pages 2 0 R >>",
    "<< /Type /Pages /Kids [3 0 R] /Count 1 >>",
    "<< /Type /Page /Parent 2 0 R /MediaBox [0 0 612 792] /Contents 4 0 R /Resources << /Font << /F1 5 0 R >> >> >>",
    `<< /Length ${String(Buffer.byteLength(stream, "latin1"))} >>\nstream\n${stream}\nendstream`,
    "<< /Type /Font /Subtype /Type1 /BaseFont /Helvetica >>",
  ];
  let body = "%PDF-1.4\n";
  const offsets: number[] = [];
  objects.forEach((object, index) => {
    offsets.push(Buffer.byteLength(body, "latin1"));
    body += `${String(index + 1)} 0 obj\n${object}\nendobj\n`;
  });
  const xref = Buffer.byteLength(body, "latin1");
  body += `xref\n0 ${String(objects.length + 1)}\n0000000000 65535 f \n`;
  body += offsets
    .map((offset) => `${String(offset).padStart(10, "0")} 00000 n \n`)
    .join("");
  body += `trailer\n<< /Size ${String(objects.length + 1)} /Root 1 0 R >>\nstartxref\n${String(xref)}\n%%EOF\n`;
  return Buffer.from(body, "latin1");
}

test("a real PDF attached in chat reaches the model as its text", async ({
  page,
}) => {
  test.setTimeout(300_000);

  const stamp = `${String(Date.now())}${String(Math.floor(Math.random() * 1e6))}`;
  const token = `PDFTOKEN${stamp}`;
  const fileName = `e2e-report-${stamp}.pdf`;
  const pdf = onePagePdf([
    "Quarterly report for the e2e attachment journey.",
    `The secret word is ${token}.`,
    "Revenue grew in every region.",
  ]);
  const tmpFile = path.join(os.tmpdir(), fileName);
  fs.writeFileSync(tmpFile, pdf);

  const prompt = `summarise attached pdf ${stamp}`;
  expect(prompt.length).toBeLessThanOrEqual(50);

  let projectId = "";
  let conversationId = "";
  try {
    await page.goto(BASE_URL + "/app/chat");
    await expect(page.getByTestId("chat-input")).toBeVisible({
      timeout: 30_000,
    });

    await page.getByTestId("model-selector-button").click();
    const modelOption = page
      .getByRole("menuitem")
      .filter({ hasText: MODEL_NAME })
      .first();
    await expect(
      modelOption,
      `the seeded model ${MODEL_NAME} must be offered`,
    ).toBeVisible({ timeout: 20_000 });
    await modelOption.click();
    await expect(page.getByTestId("model-selector-name")).toContainText(
      MODEL_NAME,
      { timeout: 10_000 },
    );

    const plus = page.getByTestId("plus-menu-button");
    await expect(plus).toBeEnabled({ timeout: 20_000 });
    await plus.click();
    const attachRow = page.getByTestId("plus-menu-attachments");
    await expect(attachRow).toBeVisible();
    const attachInput = attachRow
      .locator("xpath=ancestor::div[1]")
      .locator('input[type="file"]');
    await expect(attachInput).toHaveCount(1);
    await attachInput.setInputFiles(tmpFile);
    await expect(
      attachRow,
      "the picked PDF must enter the composer’s attachment state",
    ).toContainText("9 left");

    const created = page.waitForResponse(
      (r) =>
        CONVERSATIONS_RE.test(new URL(r.url()).pathname) &&
        r.request().method() === "POST",
      { timeout: 45_000 },
    );
    const uploaded = page.waitForResponse(
      (r) =>
        UPLOAD_RE.test(new URL(r.url()).pathname) &&
        r.request().method() === "POST",
      { timeout: 60_000 },
    );
    const started = page.waitForResponse(
      (r) => START_RE.test(r.url()) && r.request().method() === "POST",
      {
        timeout: 60_000,
      },
    );

    const input = page.getByTestId("chat-message-input");
    await expect(input).toBeEditable({ timeout: 20_000 });
    await input.fill(prompt);
    const sendButton = page.getByTestId("chat-send-button");
    await expect(sendButton).toBeEnabled({ timeout: 10_000 });
    await sendButton.click();

    const createdResponse = await created;
    expect(createdResponse.status()).toBe(201);
    projectId =
      CONVERSATIONS_RE.exec(new URL(createdResponse.url()).pathname)?.[1] ?? "";
    conversationId = String(
      ((await createdResponse.json()) as { id?: string }).id ?? "",
    );
    expect(projectId).not.toBe("");
    expect(conversationId).toMatch(/^\d+$/);

    const uploadResponse = await uploaded;
    expect(
      uploadResponse.status(),
      `the PDF must upload: ${(await uploadResponse.text()).slice(0, 300)}`,
    ).toBe(201);
    const startResponse = await started;
    expect(
      startResponse.status(),
      `the turn must be admitted: ${(await startResponse.text()).slice(0, 300)}`,
    ).toBe(200);

    await expectStoredAssistantAnswer(page, projectId, conversationId, {
      timeout: 120_000,
      contains: fileName,
      message: `the ${WORKER} worker did not answer the turn that carries the PDF`,
    });
    const answer = await readStoredAssistantAnswer(
      page,
      projectId,
      conversationId,
    );
    expect(
      answer.content,
      `the PDF's TEXT never reached the model on the ${WORKER} leg. On the native leg check ` +
        "`agent_input_attachment_unreadable` (elitea-main refused it, with a reason) and " +
        "`agent_input_attachment_read_failed` (the read itself failed) in the worker log.",
    ).toContain(token);
    // The unreadable note both workers write names the file:
    // `"<name>" could not be read`. The bare phrase also appears in the
    // attachment instructions the mock model echoes back ("a note that says
    // which part could not be read and why"), so it proves nothing.
    expect(
      answer.content,
      "a readable PDF must not be reported as unreadable",
    ).not.toContain(`"${fileName}" could not be read`);
  } finally {
    fs.unlinkSync(tmpFile);
    if (projectId !== "" && conversationId !== "") {
      await page.request
        .delete(
          `${BASE_URL}/api/v2/elitea_core/conversation/prompt_lib/${projectId}/${conversationId}`,
        )
        .catch(() => undefined);
    }
  }
});
