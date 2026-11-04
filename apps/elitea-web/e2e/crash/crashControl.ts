/**
 * Helpers for the crash-recovery browser check (`playwright.crash.config.ts`).
 *
 * The harness (scripts/crash-recovery/crashctl.py) and the spec talk through
 * JSON files in CRASH_CONTROL_DIR, always written tmp-then-rename so a reader
 * never sees half a file. Nothing here records raw message text, tokens or cookies.
 */
import { createHash } from 'crypto';
import { existsSync, mkdirSync, readFileSync, renameSync, writeFileSync } from 'fs';
import path from 'path';

import { expect } from '@playwright/test';
import type { Page } from '@playwright/test';

export function requireEnv(name: string): string {
  const value = process.env[name];
  if (value === undefined || value === '') throw new Error(`${name} must be set by the crash harness`);
  return value;
}

export const controlDir = (): string => requireEnv('CRASH_CONTROL_DIR');

export function writeControl(name: string, body: unknown): void {
  const target = path.join(controlDir(), name);
  mkdirSync(path.dirname(target), { recursive: true });
  const tmp = `${target}.${process.pid}.tmp`;
  writeFileSync(tmp, JSON.stringify(body), 'utf8');
  renameSync(tmp, target);
}

export function readControl<T>(name: string): T | undefined {
  const target = path.join(controlDir(), name);
  if (!existsSync(target)) return undefined;
  try {
    return JSON.parse(readFileSync(target, 'utf8')) as T;
  } catch {
    return undefined; // mid-write on a non-atomic writer; the next poll reads it
  }
}

export const sha256 = (text: string): string => createHash('sha256').update(text).digest('hex');

/** Poll a control file (500 ms, bounded); `abort.json` fails the wait. */
export async function waitForControl<T>(
  name: string,
  timeoutMs: number,
  page: Page,
): Promise<T> {
  const deadline = Date.now() + timeoutMs;
  for (;;) {
    const abort = readControl<{ reason?: string }>('abort.json');
    if (abort !== undefined) throw new Error(`harness aborted the run: ${abort.reason ?? 'no reason given'}`);
    const found = readControl<T>(name);
    if (found !== undefined) return found;
    if (Date.now() > deadline) throw new Error(`${name} did not appear within ${String(timeoutMs)} ms`);
    await page.waitForTimeout(500);
  }
}

/**
 * The OIDC mock sign-in of `e2e/auth.setup.ts`'s performOidcLogin, minus its
 * project-provisioning waits (the harness owns the project and conversation).
 */
export async function performOidcLogin(page: Page, baseUrl: string, subject: string, statePath: string): Promise<void> {
  await page.goto(`${baseUrl}/auth/oidc/login`, { waitUntil: 'domcontentloaded' });
  await page.waitForURL(/oidc\.localhost(:\d+)?/, { timeout: 30_000 });
  await page.getByLabel('Subject').fill(subject);
  await page.getByRole('button', { name: 'Authorize', exact: true }).click();
  await page.waitForURL(`${baseUrl}/**`, { timeout: 30_000 });
  const info = await page.request.get(`${baseUrl}/auth/info`);
  const body = (await info.json()) as { authenticated?: boolean; user_id?: string };
  expect(body, `OIDC session not authenticated for the crash subject`).toMatchObject({ authenticated: true });
  mkdirSync(path.dirname(statePath), { recursive: true });
  await page.context().storageState({ path: statePath });
}
