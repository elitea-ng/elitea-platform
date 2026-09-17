/**
 * Regression coverage for #137 defect 2: an EMPTY secrets list used to render
 * eight skeletons forever.
 *
 *     if (isFetching || rows.length === 0)   // → 8 <Skeleton>
 *
 * A project with no secrets is the normal first-run state and is exactly what
 * a correctly-routed backend returns, so the product's default secrets screen
 * was an indefinite loading spinner with no grid, no column headers and no
 * pagination footer.
 *
 * These tests hold the three states apart: loading (skeletons), settled-empty
 * (grid + "No secrets" + footer, zero skeletons) and settled-with-rows.
 */
import { describe, expect, it, vi } from 'vitest';
import { screen, within } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import type { SecretRow } from '@/entities/secret';

import { SECRETS_SKELETON_TESTID, SecretsTable, type SecretsTableProps } from './SecretsTable';
import type { SecretPermissions } from '../../lib/secrets/secretPermissions';

/** An admin or an editor: every secrets string granted. */
const FULL_SECRET_PERMISSIONS: SecretPermissions = {
  canUnsecret: true,
  canCreate: true,
  canEdit: true,
  canDelete: true,
  canHide: true,
};

/** A viewer after #402: the NAME listing, and nothing else. */
const LIST_ONLY_SECRET_PERMISSIONS: SecretPermissions = {
  canUnsecret: false,
  canCreate: false,
  canEdit: false,
  canDelete: false,
  canHide: false,
};

function makeProps(overrides: Partial<SecretsTableProps> = {}): SecretsTableProps {
  return {
    rows: [],
    setRows: vi.fn(),
    rowModesModel: {},
    setRowModesModel: vi.fn(),
    isFetching: false,
    isShowSecretMap: {},
    permissions: FULL_SECRET_PERMISSIONS,
    validationErrors: {},
    onValidationChange: vi.fn(),
    actions: {
      onSave: () => async () => {},
      onCancel: () => () => {},
      onShowSecret: () => async () => {},
      onHideSecret: () => {},
      onCopySecretValue: () => async () => {},
      onActionsMenuClick: () => () => {},
      onEdit: () => async () => {},
      onHide: () => () => {},
      onDelete: () => () => {},
      onCloseAlert: () => () => {},
      onConfirmAlert: () => () => {},
    },
    menu: { anchorEl: null, anchorRowId: null, onCloseMenu: vi.fn() },
    dialog: { openAlert: null, openAlertType: '' },
    ...overrides,
  };
}

const existingRow: SecretRow = {
  id: 'existing-API_KEY',
  name: 'API_KEY',
  secretName: '{{secret.API_KEY}}',
  isDefault: false,
  secretValue: '{{secret.API_KEY}}',
  isNew: false,
};

describe('SecretsTable', () => {
  it('renders the grid, an empty state and the footer when the list settled with no secrets', () => {
    renderWithTheme(<SecretsTable {...makeProps({ rows: [], isFetching: false })} />);

    // The defect: eight skeletons instead of a table.
    expect(screen.queryAllByTestId(SECRETS_SKELETON_TESTID)).toHaveLength(0);
    expect(screen.getByRole('grid')).toBeInTheDocument();
    expect(screen.getByText('No secrets')).toBeInTheDocument();
    // The footer only renders past the skeleton guard.
    expect(screen.getByText('Rows per page')).toBeInTheDocument();
  });

  it('renders skeletons only while the list is genuinely fetching', () => {
    renderWithTheme(<SecretsTable {...makeProps({ rows: [], isFetching: true })} />);

    expect(screen.getAllByTestId(SECRETS_SKELETON_TESTID).length).toBeGreaterThan(0);
    expect(screen.queryByRole('grid')).not.toBeInTheDocument();
    expect(screen.queryByText('No secrets')).not.toBeInTheDocument();
  });

  it('renders the rows, and no empty state, once there are secrets', () => {
    renderWithTheme(<SecretsTable {...makeProps({ rows: [existingRow] })} />);

    const grid = screen.getByRole('grid');
    expect(within(grid).getAllByText('API_KEY').length).toBeGreaterThan(0);
    expect(screen.queryByText('No secrets')).not.toBeInTheDocument();
    expect(screen.queryAllByTestId(SECRETS_SKELETON_TESTID)).toHaveLength(0);
  });
});

/**
 * #402: the viewer can now LIST, and can do nothing else here.
 *
 * Both directions are measured. A caller with every string keeps every control,
 * so a test that simply deleted the controls would fail. A caller with the list
 * alone sees the secret NAMES and no control that could only answer 403.
 */
describe('SecretsTable — the controls a list-only caller may use (issue 402)', () => {
  it('renders every row control for a caller that holds every secrets string', () => {
    renderWithTheme(
      <SecretsTable {...makeProps({ rows: [existingRow], permissions: FULL_SECRET_PERMISSIONS })} />,
    );

    expect(screen.getByRole('button', { name: 'More actions' })).toBeInTheDocument();
    expect(screen.getAllByRole('button', { name: 'Show' }).length).toBeGreaterThan(0);
    expect(screen.getByRole('button', { name: 'Copy' })).toBeInTheDocument();
  });

  it('shows the secret name, and no write or reveal control, for a list-only caller', () => {
    renderWithTheme(
      <SecretsTable
        {...makeProps({ rows: [existingRow], permissions: LIST_ONLY_SECRET_PERMISSIONS })}
      />,
    );

    // The point of the grant: the name is on screen.
    const grid = screen.getByRole('grid');
    expect(within(grid).getAllByText('API_KEY').length).toBeGreaterThan(0);
    expect(within(grid).getAllByText('{{secret.API_KEY}}').length).toBeGreaterThan(0);

    // Every control that could only answer 403 is absent.
    expect(screen.queryByRole('button', { name: 'More actions' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Show' })).not.toBeInTheDocument();
    expect(screen.queryByRole('button', { name: 'Copy' })).not.toBeInTheDocument();
  });
});

/*
 * elitea_issues: 4860 — a new row always sorts to the front of the list
 * (`sortedRows` above), so it always falls on page 1. Creating one while
 * viewing any later page used to leave the viewer on that later page with
 * nothing visibly different, while the new draft row sat unseen on page 1.
 */
describe('SecretsTable — creating a row while on a later page (elitea_issues 4860)', () => {
  function manyExistingRows(count: number): SecretRow[] {
    return Array.from({ length: count }, (_, i) => ({
      id: `existing-KEY_${String(i).padStart(2, '0')}`,
      name: `KEY_${String(i).padStart(2, '0')}`,
      secretName: `{{secret.KEY_${String(i).padStart(2, '0')}}}`,
      isDefault: false,
      secretValue: `{{secret.KEY_${String(i).padStart(2, '0')}}}`,
      isNew: false,
    }));
  }

  it('jumps back to page 1 once a new (unsaved) row appears', async () => {
    const user = userEvent.setup();
    const rows = manyExistingRows(11); // DEFAULT_PAGE_SIZE is 10 — two pages.

    const { rerender } = renderWithTheme(<SecretsTable {...makeProps({ rows })} />);

    expect(screen.getByText('Page 1 of 2')).toBeInTheDocument();
    await user.click(screen.getByRole('button', { name: 'Go to page 2' }));
    expect(screen.getByText('Page 2 of 2')).toBeInTheDocument();

    // The "+" action (owned by the caller, above this component) prepends a
    // new draft row — simulated here by re-rendering with one added.
    const rowsWithDraft: SecretRow[] = [
      { id: 'draft-1', name: '', secretName: '', isDefault: false, secretValue: '', isNew: true },
      ...rows,
    ];
    rerender(<SecretsTable {...makeProps({ rows: rowsWithDraft })} />);

    expect(await screen.findByText('Page 1 of 2')).toBeInTheDocument();
  });
});
