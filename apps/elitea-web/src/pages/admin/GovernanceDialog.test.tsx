/**
 * The governance dialog refuses to save a mistyped id list (ADR-0041
 * decision 4). A throttle list that loses its entries saves as a rate limit on
 * the whole project, so the field shows the error and Save stays disabled.
 */
import { describe, expect, it, vi } from 'vitest';
import { render, screen } from '@testing-library/react';

import { ThemeProvider } from '@mui/material/styles';

import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

import { GovernanceDialog } from './GovernanceDialog';
import { EMPTY_DRAFT, type GovernanceDraft } from './useGatewayGovernancePage';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderDialog(patch: Partial<GovernanceDraft>) {
  const onSave = vi.fn();
  render(
    <ThemeProvider theme={theme}>
      <GovernanceDialog
        draft={{ ...EMPTY_DRAFT, type: 'rate_limit', name: 'cap', scopeProjectIds: '7', ...patch }}
        isNew
        isSaving={false}
        saveError={undefined}
        onChange={vi.fn()}
        onCancel={vi.fn()}
        onSave={onSave}
      />
    </ThemeProvider>,
  );
  return onSave;
}

describe('GovernanceDialog id lists', () => {
  it('disables Save and names the bad entry when a throttle list is mistyped', () => {
    renderDialog({ scopeAgentIds: '3, 4.5, abc' });
    expect(screen.getByTestId('governance-save')).toBeDisabled();
    expect(screen.getByText(/Not valid: 4\.5, abc/)).toBeInTheDocument();
  });

  it('enables Save when every entry is a positive integer', () => {
    renderDialog({ scopeAgentIds: '3, 4', scopeUserIds: '11' });
    expect(screen.getByTestId('governance-save')).toBeEnabled();
  });
});
