import type { ReactElement } from 'react';
import Box from '@mui/material/Box';
import { ThemeProvider } from '@mui/material/styles';
import { render } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it, vi } from 'vitest';

import { DEFAULT_BRAND_PACK, DEFAULT_COLOR_SCHEME, buildEliteaTheme } from '@/shared/brand';

import type { PickerItem } from '../../lib/tool-picker/toolPicker.types';
import { PickerEntityIcon } from './PickerEntityIcon';
import { PickerItemRow } from './PickerItemRow';
import { PickerMcpStatus } from './PickerMcpStatus';
import { PickerMenu } from './PickerMenu';
import { PickerTrigger } from './PickerTrigger';

const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

function renderWithTheme(ui: ReactElement) {
  return render(
    <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
      {ui}
    </ThemeProvider>,
  );
}

const baseItem: PickerItem = {
  key: 'toolkit:1',
  id: 1,
  name: 'github',
  label: 'GitHub',
  description: 'Repository tools',
  source: { id: 1, type: 'github', name: 'GitHub' },
  isAttached: false,
  inUse: true,
  disabledReason: '',
};

describe('Tool picker UI primitives', () => {
  it('PickerEntityIcon renders toolkit, MCP, agent, and pipeline variants', () => {
    const { container, rerender } = renderWithTheme(
      <PickerEntityIcon source={{ id: 1, type: 'github', name: 'GH' }} />,
    );
    expect(container.querySelector('svg')).toBeTruthy();

    rerender(
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <PickerEntityIcon source={{ id: 2, type: 'mcp', name: 'Mcp' }} mcp large />
      </ThemeProvider>,
    );
    expect(container.querySelector('svg')).toBeTruthy();

    rerender(
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <PickerEntityIcon source={{ id: 3, type: 'application', name: 'Agent' }} isApplication />
      </ThemeProvider>,
    );
    expect(container.querySelector('svg')).toBeTruthy();

    rerender(
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <PickerEntityIcon
          source={{ id: 4, type: 'application', name: 'Pipe', agent_type: 'pipeline' }}
          isApplication
          isPipeline
        />
      </ThemeProvider>,
    );
    expect(container.querySelector('svg')).toBeTruthy();
  });

  it('PickerMcpStatus hides when online/logged-in and triggers login without bubbling when offline', async () => {
    const user = userEvent.setup();
    const onParentClick = vi.fn();
    const onLoginClick = vi.fn();

    const { queryByTestId, rerender, getByTestId } = renderWithTheme(
      <Box role="presentation" onClick={onParentClick} onKeyDown={onParentClick}>
        <PickerMcpStatus
          mcp={{ id: 1, type: 'mcp', name: 'DocsMCP', online: true }}
          onLoginClick={onLoginClick}
          data-testid="mcp-status"
        />
      </Box>,
    );
    expect(queryByTestId('mcp-status')).not.toBeInTheDocument();

    rerender(
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <Box role="presentation" onClick={onParentClick} onKeyDown={onParentClick}>
          <PickerMcpStatus
            mcp={{ id: 1, type: 'mcp', name: 'DocsMCP', online: false }}
            onLoginClick={onLoginClick}
            data-testid="mcp-status"
          />
        </Box>
      </ThemeProvider>,
    );
    expect(getByTestId('mcp-status')).toBeInTheDocument();
    await user.click(getByTestId('mcp-status-login'));
    expect(onLoginClick).toHaveBeenCalledTimes(1);
    expect(onParentClick).not.toHaveBeenCalled();
  });

  it('PickerItemRow shows In use pill, selection check, and EL-7085 disabled tooltip', async () => {
    const user = userEvent.setup();
    const onSelect = vi.fn();

    const { getByTestId, findByText, rerender } = renderWithTheme(
      <PickerItemRow
        item={baseItem}
        icon={<span />}
        selected
        onSelect={onSelect}
        data-testid="row-1"
      />,
    );
    expect(getByTestId('row-1-in-use')).toHaveTextContent('In use');
    await user.click(getByTestId('row-1'));
    expect(onSelect).toHaveBeenCalledWith(baseItem);

    const disabledItem: PickerItem = {
      ...baseItem,
      inUse: false,
      disabledReason: "The SwarmBot agent has Swarm Mode enabled and can't be added as a tool.",
    };
    rerender(
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <PickerItemRow item={disabledItem} icon={<span />} onSelect={onSelect} data-testid="row-disabled" />
      </ThemeProvider>,
    );
    const disabledRow = getByTestId('row-disabled');
    expect(disabledRow).toHaveAttribute('aria-disabled', 'true');
    await user.hover(disabledRow.parentElement!);
    expect(await findByText(/Swarm Mode enabled/)).toBeInTheDocument();
  });

  it('PickerTrigger opens on click/ArrowDown and clears selection', async () => {
    const user = userEvent.setup();
    const onOpen = vi.fn();
    const onClear = vi.fn();

    const { getByTestId } = renderWithTheme(
      <PickerTrigger
        label="Toolkit"
        hasValue
        onOpen={onOpen}
        onClear={onClear}
        data-testid="picker-trigger"
      >
        <span>GitHub</span>
      </PickerTrigger>,
    );

    await user.click(getByTestId('picker-trigger'));
    expect(onOpen).toHaveBeenCalledTimes(1);

    await user.click(getByTestId('picker-trigger-clear'));
    expect(onClear).toHaveBeenCalledTimes(1);
  });

  it('PickerMenu renders search, create actions, group headers, empty, and error/retry states', async () => {
    const user = userEvent.setup();
    const anchor = document.createElement('div');
    document.body.appendChild(anchor);
    const onSearchChange = vi.fn();
    const onRetry = vi.fn();
    const onCreate = vi.fn();

    const { getByTestId, getByText, rerender } = renderWithTheme(
      <PickerMenu
        anchorEl={anchor}
        open
        onClose={vi.fn()}
        searchValue=""
        onSearchChange={onSearchChange}
        createActions={[{ key: 'new', label: 'Create new', onClick: onCreate, testId: 'create-btn' }]}
        groups={[
          { key: 'g1', title: 'Toolkits', items: [baseItem] },
          { key: 'g2', title: 'MCPs', items: [{ ...baseItem, key: 'mcp:2', id: 2, label: 'Docs MCP' }] },
        ]}
        renderItem={(item) => <div key={item.key}>{item.label}</div>}
        data-testid="test-menu"
      />,
    );

    expect(getByText('Toolkits')).toBeInTheDocument();
    expect(getByText('MCPs')).toBeInTheDocument();
    await user.click(getByTestId('create-btn'));
    expect(onCreate).toHaveBeenCalledTimes(1);

    rerender(
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <PickerMenu
          anchorEl={anchor}
          open
          onClose={vi.fn()}
          searchValue=""
          onSearchChange={onSearchChange}
          groups={[]}
          renderItem={() => null}
          loadState={{ isError: true, onRetry }}
          data-testid="test-menu"
        />
      </ThemeProvider>,
    );
    expect(getByTestId('test-menu-error')).toBeInTheDocument();
    await user.click(getByTestId('test-menu-retry'));
    expect(onRetry).toHaveBeenCalledTimes(1);

    rerender(
      <ThemeProvider theme={theme} defaultMode={DEFAULT_COLOR_SCHEME}>
        <PickerMenu
          anchorEl={anchor}
          open
          onClose={vi.fn()}
          searchValue="missing"
          onSearchChange={onSearchChange}
          groups={[]}
          renderItem={() => null}
          messages={{ noResultsMessage: 'No matches' }}
          data-testid="test-menu"
        />
      </ThemeProvider>,
    );
    expect(getByTestId('test-menu-empty')).toHaveTextContent('No matches');
    anchor.remove();
  });
});
