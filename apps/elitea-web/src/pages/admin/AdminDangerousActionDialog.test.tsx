/**
 * The one dangerous-action dialog (ADR-0059 decision 6; R3.8b, issue 2614).
 *
 *  1. It blocks until the typed name matches the target, and until a reason
 *     code is chosen (`other` also needs a text).
 *  2. It says how far the action reaches, and "unknown" (never zero) when the
 *     page cannot tell.
 *  3. The reason text is cut to 500 bytes of UTF-8 as it is typed, with a
 *     visible counter.
 *  4. A Cyrillic reason on a DELETE reaches the server percent-encoded in
 *     `Elitea-Reason` and the request succeeds, against the generated mock of
 *     `resetBrandingSettings` (the v2.yaml contract).
 */
import { useState } from 'react';

import { afterEach, beforeEach, describe, expect, it } from 'vitest';
import { screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import {
  getResetBrandingSettingsMockHandler,
  getResetBrandingSettingsResponseMock,
} from '@/shared/api/generated/admin/admin.msw';
import { configureGeneratedClient, resetGeneratedClient } from '@/shared/api/generated/mutator';
import { server } from '@/test/setup';

import { AdminDangerousActionDialog, type AdminDangerReach } from './AdminDangerousActionDialog';
import { MAX_REASON_BYTES, clampReason, reasonBytes, reasonFieldsKeepText, type AdminReason } from './adminDangerousAction';
import { useResetBrandingSettings } from './api/adminBrandingApi';
import { confirmDangerousAction } from './__tests__/dangerousDialog';
import { renderAdminRoute } from './__tests__/testRouter';

const RESET = 'DELETE /api/v2/admin/branding/administration' as const;

function StaticDialog({
  reach = { count: 3, unit: 'projects' },
  onConfirm = () => undefined,
}: {
  reach?: AdminDangerReach;
  onConfirm?: (reason: AdminReason) => void;
}) {
  return (
    <AdminDangerousActionDialog
      open
      action={RESET}
      title="Reset branding"
      change="Every field inherits again."
      reach={reach}
      target="Acme brand"
      confirmLabel="Reset"
      pending={false}
      onClose={() => undefined}
      onConfirm={onConfirm}
    />
  );
}

/** The real API hook behind the dialog: the reason goes out as the DELETE's headers. */
function ResetHarness() {
  const reset = useResetBrandingSettings();
  const [open, setOpen] = useState(true);
  return (
    <>
      {reset.isSuccess ? <span data-testid="admin-dangerous-test-done">done</span> : null}
      <AdminDangerousActionDialog
        open={open}
        action={RESET}
        title="Reset branding"
        change="Every field inherits again."
        reach={{ count: undefined, unit: 'people' }}
        target="branding"
        confirmLabel="Reset"
        pending={reset.isPending}
        onClose={() => setOpen(false)}
        onConfirm={(reason) => reset.mutate(reason, { onSuccess: () => setOpen(false) })}
      />
    </>
  );
}

beforeEach(() => {
  configureGeneratedClient({ baseUrl: '/api/v2' });
});

afterEach(() => {
  resetGeneratedClient();
});

describe('AdminDangerousActionDialog', () => {
  it('blocks until the typed name matches the target and a reason code is chosen', async () => {
    const user = userEvent.setup();
    const confirmed: AdminReason[] = [];
    renderAdminRoute(<StaticDialog onConfirm={(reason) => confirmed.push(reason)} />);

    const confirm = screen.getByTestId('admin-dangerous-confirm');
    expect(confirm).toBeDisabled();
    await user.selectOptions(screen.getByTestId('admin-dangerous-reason-code'), 'incident');
    expect(confirm).toBeDisabled();
    await user.type(screen.getByTestId('admin-dangerous-target'), 'Acme bran');
    expect(confirm).toBeDisabled();
    await user.type(screen.getByTestId('admin-dangerous-target'), 'x');
    expect(confirm).toBeDisabled();
    await user.type(screen.getByTestId('admin-dangerous-target'), '{Backspace}d');
    expect(confirm).toBeEnabled();

    await user.click(confirm);
    expect(confirmed).toEqual([{ code: 'incident', text: '' }]);
  });

  it('asks for a text when the code is other', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<StaticDialog />);
    await user.selectOptions(screen.getByTestId('admin-dangerous-reason-code'), 'other');
    await user.type(screen.getByTestId('admin-dangerous-target'), 'Acme brand');
    expect(screen.getByTestId('admin-dangerous-confirm')).toBeDisabled();
    await user.type(screen.getByTestId('admin-dangerous-reason-text'), 'audit finding');
    expect(screen.getByTestId('admin-dangerous-confirm')).toBeEnabled();
  });

  it('shows the level from the action table and the reach, unknown rather than zero', () => {
    const { unmount } = renderAdminRoute(<StaticDialog />);
    expect(screen.getByTestId('admin-dangerous-level')).toHaveTextContent('Dangerous action');
    expect(screen.getByTestId('admin-dangerous-reach')).toHaveTextContent('Projects reached: 3');
    unmount();
    renderAdminRoute(<StaticDialog reach={{ count: undefined, unit: 'people' }} />);
    expect(screen.getByTestId('admin-dangerous-reach')).toHaveTextContent('People reached: unknown');
    expect(screen.getByTestId('admin-dangerous-reach')).not.toHaveTextContent('0');
  });

  it('cuts the reason to the byte bound on a character boundary, with a counter', () => {
    // Two bytes per Cyrillic letter: 300 letters are 600 bytes.
    const long = 'я'.repeat(300);
    const kept = clampReason(long);
    expect(reasonBytes(kept)).toBe(MAX_REASON_BYTES);
    expect(kept).toBe('я'.repeat(250));
    // A three-byte character never splits.
    expect(reasonBytes(clampReason(`a${'€'.repeat(200)}`))).toBeLessThanOrEqual(MAX_REASON_BYTES);
    expect(decodeURIComponent(encodeURIComponent(clampReason(`a${'€'.repeat(200)}`)))).toBe(
      `a${'€'.repeat(166)}`,
    );
  });

  it('updates the counter as the reason is typed', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<StaticDialog />);
    expect(screen.getByTestId('admin-dangerous-reason-counter')).toHaveTextContent('0 / 500 bytes');
    await user.type(screen.getByTestId('admin-dangerous-reason-text'), 'инцидент');
    expect(screen.getByTestId('admin-dangerous-reason-counter')).toHaveTextContent('16 / 500 bytes');
  });

  it('sends a Cyrillic reason on a DELETE percent-encoded in the headers, and the request succeeds', async () => {
    const seen: Array<{ code: string | null; reason: string | null }> = [];
    server.use(
      getResetBrandingSettingsMockHandler(({ request }) => {
        seen.push({
          code: request.headers.get('Elitea-Reason-Code'),
          reason: request.headers.get('Elitea-Reason'),
        });
        return getResetBrandingSettingsResponseMock();
      }),
    );
    const user = userEvent.setup();
    renderAdminRoute(<ResetHarness />);

    const text = 'Сброс по заявке клиента';
    await confirmDangerousAction(user, 'branding', { code: 'request', text });

    expect(await screen.findByTestId('admin-dangerous-test-done')).toBeInTheDocument();
    await waitFor(() => expect(seen).toHaveLength(1));
    expect(seen[0]?.code).toBe('request');
    expect(seen[0]?.reason).toBe(encodeURIComponent(text));
    expect(decodeURIComponent(seen[0]?.reason ?? '')).toBe(text);
  });
});

/* Review fixes of PR 3083: a locked dialog still closes; an empty target never arms; a kept reason fits its route. */
describe('AdminDangerousActionDialog — locked, empty target, kept text', () => {
  function Closable({ locked, target = 'Acme brand' }: { locked: boolean; target?: string }) {
    const [open, setOpen] = useState(true);
    return open ? (
      <AdminDangerousActionDialog
        open
        action={RESET}
        title="Reset branding"
        change="Every field inherits again."
        reach={{ count: 1, unit: 'projects' }}
        target={target}
        confirmLabel="Reset"
        pending={false}
        locked={locked}
        onClose={() => setOpen(false)}
        onConfirm={() => undefined}
      />
    ) : (
      <span data-testid="admin-dangerous-test-closed">closed</span>
    );
  }

  it('lets a caller who may not confirm close the dialog', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<Closable locked />);
    const dialog = await screen.findByTestId('admin-dangerous-dialog');
    expect(screen.getByTestId('admin-dangerous-confirm')).toBeDisabled();
    expect(screen.getByTestId('admin-dangerous-target')).toBeDisabled();
    expect(dialog).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Cancel' }));
    expect(await screen.findByTestId('admin-dangerous-test-closed')).toBeInTheDocument();
  });

  it('never arms on an empty target', async () => {
    const user = userEvent.setup();
    renderAdminRoute(<Closable locked={false} target="  " />);
    await screen.findByTestId('admin-dangerous-dialog');
    await user.selectOptions(screen.getByTestId('admin-dangerous-reason-code'), 'incident');
    expect(screen.getByTestId('admin-dangerous-confirm')).toBeDisabled();
  });

  it("keeps the request's own reason whole and cuts the dialog's text to the route's bound", () => {
    const own = 'x'.repeat(700);
    const fields = reasonFieldsKeepText(
      'POST /api/v2/admin/execution_suspensions platform',
      { code: 'incident', text: 'y'.repeat(400) },
      own,
      1024,
    );
    expect(fields.reason_code).toBe('incident');
    expect(fields.reason.startsWith(`${own}\n`)).toBe(true);
    expect(reasonBytes(fields.reason)).toBe(1024);
    const full = reasonFieldsKeepText(
      'POST /api/v2/admin/execution_suspensions platform',
      { code: 'incident', text: 'short' },
      'x'.repeat(1024),
      1024,
    );
    expect(full.reason).toBe('x'.repeat(1024));
  });
});
