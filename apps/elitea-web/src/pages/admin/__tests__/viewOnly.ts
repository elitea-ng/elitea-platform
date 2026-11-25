/**
 * Test helpers for the read-only mode of an admin page (ADR-0059 decision 6;
 * R3.4a, issue 2534; R3.4b, issue 2613): serve the capability document with
 * one item at a level, and list the write controls still enabled.
 */
import { screen, within } from '@testing-library/react';
import { HttpResponse, http } from 'msw';

import { server } from '@/test/setup';

/**
 * The labels a write control carries: what a reader must never find enabled.
 * A filter's "Apply" reads, so only "Apply to …" is a write.
 */
const WRITE_CONTROL =
  /\b(save|create|new|add|edit|delete|remove|clear|revoke|approve|reject|deny|invite|import|upload|reset|enable|disable|suspend|resume|rotate|grant|sync|adopt|publish|rename|bind|apply to)\b/i;

/**
 * Serves `GET /admin/console` with `itemIds` at `level`, and the item read
 * that names the writing roles. The caller holds the built-in role
 * `security_auditor` at `read` and `admin` at `write`.
 */
export function serveConsoleLevel(level: 'read' | 'write', ...itemIds: readonly string[]): void {
  const role = level === 'read' ? 'security_auditor' : 'admin';
  server.use(
    http.get('*/admin/console', () =>
      HttpResponse.json({
        areas: [
          {
            id: 'platform',
            label_key: 'pages.admin.console.area.platform',
            items: itemIds.map((id) => ({ id, label_key: `pages.admin.console.item.${id}`, level })),
          },
        ],
        roles: [{ name: role, built_in: true }],
        elevation: null,
        admin_console_areas: true,
      }),
    ),
    http.get('*/admin/console/items/:itemId', ({ params }) =>
      HttpResponse.json({
        id: String(params.itemId),
        area: 'platform',
        label_key: `pages.admin.console.item.${String(params.itemId)}`,
        area_label_key: 'pages.admin.console.area.platform',
        available: true,
        granting_roles: [
          { name: 'admin', display_name: 'Platform administrator', built_in: true, level: 'write' },
          { name: 'security_auditor', display_name: 'Security auditor', built_in: true, level: 'read' },
        ],
      }),
    ),
  );
}

/** The buttons, switches and checkboxes in `container` that write and are still enabled. */
export function enabledWriteControls(container: HTMLElement = document.body): HTMLElement[] {
  const scope = within(container);
  const buttons = scope
    .queryAllByRole('button')
    .filter((button) => WRITE_CONTROL.test(button.getAttribute('aria-label') ?? button.textContent));
  // A grid's "Select all rows" selects nothing when no row is selectable: a read.
  const toggles = [...scope.queryAllByRole('checkbox'), ...scope.queryAllByRole('switch')].filter(
    (toggle) => toggle.getAttribute('aria-label') !== 'Select all rows',
  );
  return [...buttons, ...toggles].filter((control) => !(control as HTMLButtonElement | HTMLInputElement).disabled);
}

/** Waits for the "View only" notice of the page. */
export async function findViewOnlyNotice(): Promise<HTMLElement> {
  return screen.findByTestId('admin-view-only');
}
