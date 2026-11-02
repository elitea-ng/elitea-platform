import type { ReactElement } from 'react';

import { fireEvent, screen, within } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';
import { SidePanelHarness } from '@/test/sidePanelHarness';

import { CredentialsTypesPanel } from './CredentialsTypesPanel';
import type { CredentialTypeTag } from './CredentialsTypesPanel';

const TAGS: CredentialTypeTag[] = [
  { id: 'openai1', name: 'Openai', data: { type: 'openai' }, credentialCount: 2 },
  { id: 'azure2', name: 'Azure', data: { type: 'azure' }, credentialCount: 1 },
];

function renderInPanel(ui: ReactElement) {
  renderWithTheme(<SidePanelHarness>{ui}</SidePanelHarness>);
  return screen.getByTestId('side-panel-tab-filters');
}

describe('CredentialsTypesPanel', () => {
  it('fills the side panel Filters tab with one chip per type and its count', async () => {
    const panel = renderInPanel(
      <CredentialsTypesPanel
        tagList={TAGS}
        selectedTypes={[]}
        onToggleType={vi.fn()}
        onClearTypes={vi.fn()}
        isLoading={false}
      />,
    );
    expect(await within(panel).findByRole('button', { name: 'Openai (2)' })).toBeInTheDocument();
    expect(within(panel).getByRole('button', { name: 'Azure (1)' })).toBeInTheDocument();
  });

  it('calls onToggleType with the underlying type, not the display name', async () => {
    const onToggleType = vi.fn();
    const panel = renderInPanel(
      <CredentialsTypesPanel
        tagList={TAGS}
        selectedTypes={[]}
        onToggleType={onToggleType}
        onClearTypes={vi.fn()}
        isLoading={false}
      />,
    );
    fireEvent.click(await within(panel).findByRole('button', { name: 'Openai (2)' }));
    expect(onToggleType).toHaveBeenCalledWith('openai');
  });

  it('marks the chip pressed when its type is selected, and clears every type at once', async () => {
    const onClearTypes = vi.fn();
    const panel = renderInPanel(
      <CredentialsTypesPanel
        tagList={TAGS}
        selectedTypes={['azure']}
        onToggleType={vi.fn()}
        onClearTypes={onClearTypes}
        isLoading={false}
      />,
    );
    expect(await within(panel).findByRole('button', { name: 'Azure (1)' })).toHaveAttribute('aria-pressed', 'true');
    expect(within(panel).getByRole('button', { name: 'Openai (2)' })).toHaveAttribute('aria-pressed', 'false');
    fireEvent.click(within(panel).getByTestId('tags-panel-clear-all'));
    expect(onClearTypes).toHaveBeenCalledTimes(1);
  });

  it('draws nothing where the shell has no side panel', () => {
    renderWithTheme(
      <CredentialsTypesPanel
        tagList={TAGS}
        selectedTypes={[]}
        onToggleType={vi.fn()}
        onClearTypes={vi.fn()}
        isLoading={false}
      />,
    );
    expect(screen.queryByTestId('credentials-filters')).toBeNull();
  });
});
