/**
 * The Import dialog (issues 3142 and 3143): a team zip's preview draws each
 * member with the avatar media the zip carries, the primary action says why
 * it cannot run, and after the write the dialog says which model each agent
 * got, warning when it is the project's default instead of the file's.
 */
import { screen, waitFor, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import type { ReadImportResult } from '../lib/importTypes';
import { ImportDialog } from '../ui/ImportDialog';
import { renderWithProviders } from './testUtils';

const STILL = new Uint8Array([0x52, 0x49, 0x46, 0x46]);

function teamPending(): ReadImportResult {
  const agent = (uuid: string, name: string, llm: Record<string, unknown>) => ({
    entity: 'agents',
    import_uuid: uuid,
    name,
    description: `${name} agent`,
    versions: [{ name: 'base', import_version_uuid: `${uuid}-base`, llm_settings: llm }],
  });
  return {
    document: {
      applications: [agent('lead', 'Coordinator', { reasoning_effort: 'high' }), agent('impl', 'Implementer', { model_name: 'frontier-1m' })],
      toolkits: [{ name: 'GitHub', type: 'github' }],
      skills: [],
    },
    skipped: [],
    team: { name: 'Dev team', description: 'Builds the platform', members: ['lead', 'impl'], lead: 'lead', leadName: 'Coordinator' },
    avatars: [
      { versionKey: 'lead-base', slot: 'idle', path: 'avatars/coordinator/idle.mp4', bytes: STILL },
      { versionKey: 'lead-base', slot: 'still', path: 'avatars/coordinator/still.webp', bytes: STILL },
    ],
  };
}

const renderAvatar = (avatar: { readonly iconUrl?: string | undefined }, size: string) => (
  <img
    alt=""
    data-testid="live-avatar"
    data-size={size}
    src={avatar.iconUrl}
  />
);

const created: string[] = [];
const revoked: string[] = [];

const original = { create: Object.getOwnPropertyDescriptor(URL, 'createObjectURL'), revoke: Object.getOwnPropertyDescriptor(URL, 'revokeObjectURL') };

beforeEach(() => {
  created.length = 0;
  revoked.length = 0;
  let next = 0;
  URL.createObjectURL = (blob: Blob) => {
    next += 1;
    const url = `blob:test/${String(next)}-${blob.type}`;
    created.push(url);
    return url;
  };
  URL.revokeObjectURL = (url: string) => {
    revoked.push(url);
  };
});
afterEach(() => {
  for (const [key, descriptor] of [['createObjectURL', original.create], ['revokeObjectURL', original.revoke]] as const) {
    if (descriptor === undefined) Reflect.deleteProperty(URL, key);
    else Object.defineProperty(URL, key, descriptor);
  }
});

describe('ImportDialog (issues 3142 and 3143)', () => {
  it("draws the zip's own still for a member, the monogram for one without, and frees the URL on close", async () => {
    const onClose = vi.fn();
    const { unmount } = renderWithProviders(
      <ImportDialog
        pending={teamPending()}
        isImporting={false}
        onImport={vi.fn()}
        onClose={onClose}
        testIdPrefix="teams"
        renderAvatar={renderAvatar}
      />,
    );
    const rows = within(await screen.findByRole('list', { name: 'Members of Dev team' })).getAllByRole('listitem');
    // The still slot, not the first (a video), is the face.
    await waitFor(() => expect(within(rows[0] as HTMLElement).getByTestId('live-avatar')).toHaveAttribute('src', 'blob:test/1-image/webp'));
    expect(within(rows[1] as HTMLElement).queryByTestId('live-avatar')).toBeNull();
    expect(rows[1]).toHaveTextContent('Model: frontier-1m');
    expect(screen.getByRole('heading', { name: 'Dev team' })).toBeInTheDocument();
    expect(screen.getByText('Builds the platform')).toBeInTheDocument();
    expect(screen.getByTestId('teams-import-notices')).toHaveTextContent('1 toolkits import without credentials');
    expect(screen.getByTestId('teams-import-notices')).toHaveTextContent('Coordinator: the file names no model');
    unmount();
    expect(revoked).toEqual(created);
  });

  it('says why Import cannot run while it uploads the avatar media, and when it is blocked', () => {
    const { rerender } = renderWithProviders(
      <ImportDialog
        pending={teamPending()}
        isImporting
        onImport={vi.fn()}
        onClose={vi.fn()}
        testIdPrefix="teams"
      />,
    );
    const confirm = screen.getByTestId('teams-import-confirm');
    expect(confirm).toBeDisabled();
    expect(screen.getByTestId('teams-import-reason')).toHaveTextContent('Uploading 2 avatar files, then importing');
    expect(confirm).toHaveAttribute('aria-describedby', screen.getByTestId('teams-import-reason').id);
    rerender(
      <ImportDialog
        pending={teamPending()}
        isImporting={false}
        onImport={vi.fn()}
        onClose={vi.fn()}
        testIdPrefix="teams"
        blockedReason="Select a project first."
      />,
    );
    expect(screen.getByRole('button', { name: 'Import team' })).toBeDisabled();
    expect(screen.getByTestId('teams-import-reason')).toHaveTextContent('Select a project first.');
  });

  it('names the model each agent got, and warns for the one that got the project default', async () => {
    const user = userEvent.setup();
    const onImport = vi.fn().mockResolvedValue({
      status: 201,
      result: {
        agents: [
          { name: 'Coordinator', model: { name: 'local-qwen', project_id: 7, context_window: 262_144, defaulted: true } },
          { name: 'Implementer', model: { name: 'frontier-1m', project_id: 1, context_window: 1_000_000, defaulted: false, requested: 'frontier-1m' } },
        ],
        toolkits: [],
        skills: [],
      },
      errors: {},
    });
    renderWithProviders(
      <ImportDialog
        pending={teamPending()}
        isImporting={false}
        onImport={onImport}
        onClose={vi.fn()}
        testIdPrefix="teams"
      />,
    );
    await user.click(screen.getByRole('button', { name: 'Import team' }));
    const warning = await screen.findByTestId('teams-import-model-defaulted');
    expect(warning).toHaveTextContent(/Coordinator: the file names no model, so it runs on this project's default, local-qwen \(262.144-token context\)/);
    expect(screen.getByTestId('teams-import-models')).toHaveTextContent(/Implementer runs on frontier-1m \(1.000.000-token context\)/);
  });
});
