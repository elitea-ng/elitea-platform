import userEvent from '@testing-library/user-event';
import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import type { AgentToolAssociation } from '../lib/types';

import { ToolCard } from './ToolCard';
import type { ToolCardProps } from './ToolCard.types';

class ResizeObserverStub {
  observe(): void {}
  unobserve(): void {}
  disconnect(): void {}
}

function baseProps(overrides: Partial<ToolCardProps> = {}): ToolCardProps {
  const tool: AgentToolAssociation = {
    id: 'tool-1',
    type: 'github',
    name: 'GitHub',
    settings: {},
    ...overrides.tool,
  };
  return {
    tool,
    context: { viewMode: 'Owner' },
    disassociate: { onDisassociateTool: vi.fn() },
    variables: { onChangeVariable: vi.fn() },
    toolSelection: { onSelectedToolsChange: vi.fn() },
    ...overrides,
  };
}

describe('ToolCard', () => {
  beforeEach(() => {
    vi.stubGlobal('ResizeObserver', ResizeObserverStub);
    vi.stubGlobal('open', vi.fn());
  });

  afterEach(() => {
    vi.unstubAllGlobals();
  });

  it('renders the resolved toolkit name and card container', () => {
    const { getByTestId, getByText } = renderWithTheme(<ToolCard {...baseProps()} />);
    expect(getByTestId('agent-toolkit-card')).toBeInTheDocument();
    expect(getByText('GitHub')).toBeInTheDocument();
  });

  it('falls back through the name chain (elitea_title > name > toolkit_name > configuration_title > capitalized type)', () => {
    const { getByText } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 't', type: 'jira', settings: { configuration_title: 'My Jira Config' } },
        })}
      />,
    );
    expect(getByText('My Jira Config')).toBeInTheDocument();
  });

  it('keeps the detach control focusable and reveals it on focus, not only on hover (#849)', () => {
    const { getByTestId, getByRole, container } = renderWithTheme(<ToolCard {...baseProps()} />);

    // A plain `getByRole` — no `{ hidden: true }` escape hatch. The control
    // used to be `display: 'none'` until a mouse hovered the card header,
    // which removed it from the accessibility tree AND from the tab order, so
    // a keyboard or touch user had no way to detach a toolkit at all.
    const detach = getByRole('button', { name: 'delete tool' });
    expect(detach).toBe(getByTestId('agent-toolkit-delete-button'));

    // Hidden by opacity, so the control keeps its box and stays reachable.
    // Pinned so a later change cannot go back to `display: none`.
    const resting = getComputedStyle(detach);
    expect(resting.display).not.toBe('none');
    expect(resting.opacity).toBe('0');
    expect(resting.visibility).not.toBe('hidden');

    // …and it really can hold focus, which is what `:focus-within` needs.
    detach.focus();
    expect(document.activeElement).toBe(detach);

    // jsdom does not evaluate `:hover`/`:focus-within` in `getComputedStyle`,
    // so the reveal itself is asserted against the rule the card header
    // injects. Without the focus-within half, focusing the button above would
    // leave it invisible — reachable but unusable.
    const rules = Array.from(container.ownerDocument.querySelectorAll('style'))
      .map((style) => style.textContent ?? '')
      .join('\n');
    expect(rules).toMatch(/:hover \.agents-tool-card-action\s*,[^{]*:focus-within \.agents-tool-card-action\s*\{\s*opacity:\s*1/);
    // A device with no pointer that can hover shows the actions outright.
    expect(rules).toMatch(/@media \(hover:\s*none\)/);
  });

  it('opens and confirms the delete modal, calling onDisassociateTool with isAttachmentToolkit', async () => {
    const user = userEvent.setup();
    const onDisassociateTool = vi.fn();
    const { getByTestId, getByRole } = renderWithTheme(
      <ToolCard
        {...baseProps({
          disassociate: { onDisassociateTool },
        })}
      />,
    );
    await user.click(getByTestId('agent-toolkit-delete-button'));
    await user.click(getByRole('button', { name: 'Remove' }));
    expect(onDisassociateTool).toHaveBeenCalledWith({ isAttachmentToolkit: false });
  });

  it('computes isAttachmentToolkit from tool.id === context.attachmentToolkitId', async () => {
    const user = userEvent.setup();
    const onDisassociateTool = vi.fn();
    const { getByTestId, getByRole } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 'tool-1', type: 'artifact', settings: {} },
          context: { viewMode: 'Owner', attachmentToolkitId: 'tool-1' },
          disassociate: { onDisassociateTool },
        })}
      />,
    );
    await user.click(getByTestId('agent-toolkit-delete-button'));
    await user.click(getByRole('button', { name: 'Remove' }));
    expect(onDisassociateTool).toHaveBeenCalledWith({ isAttachmentToolkit: true });
  });

  it('shows the version selector and resolves the agent/pipeline entity type + label for a pipeline tool', () => {
    const { getByText, queryByTestId } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 5, type: 'application', name: 'My Pipeline', agent_type: 'pipeline', settings: { application_version_id: 1 } },
          versionSelector: { versions: [{ id: 1, name: 'base' }], onSelectVersion: vi.fn() },
        })}
      />,
    );
    expect(getByText('My Pipeline')).toBeInTheDocument();
    expect(getByText('base')).toBeInTheDocument();
    // Non-agent/pipeline-only BaseCardBody toggle must not render for an agent/pipeline tool.
    expect(queryByTestId('base-card-body-toggle')).not.toBeInTheDocument();
  });

  it('renders BaseCardBody (not the version selector) for a plain toolkit', () => {
    const { queryByText } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 't', type: 'github', name: 'GitHub', description: 'Reads repos', settings: {} },
        })}
      />,
    );
    expect(queryByText('Reads repos')).toBeInTheDocument();
  });

  it('shows the attachment-toolkit indicator icon and the corresponding remove-confirmation suffix', async () => {
    const user = userEvent.setup();
    const { container, getByTestId, getByText } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 'att', type: 'artifact', settings: {} },
          context: { viewMode: 'Owner', attachmentToolkitId: 'att' },
        })}
      />,
    );
    expect(container.querySelector('svg')).toBeInTheDocument();
    await user.click(getByTestId('agent-toolkit-delete-button'));
    expect(getByText(/used to keep attached files/)).toBeInTheDocument();
  });

  it('shows a blocked-by-organization banner for a blocked toolkit type, hidden while actions are expanded', async () => {
    const user = userEvent.setup();
    const { getByText, queryByText, getByTestId } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 't', type: 'github', settings: { selected_tools: ['create_issue'] } },
          context: { viewMode: 'Owner', blockedToolkitTypes: ['github'] },
        })}
      />,
    );
    expect(getByText('Github toolkit is blocked by your organization.')).toBeInTheDocument();

    await user.click(getByTestId('base-card-body-toggle'));
    expect(queryByText('Github toolkit is blocked by your organization.')).not.toBeInTheDocument();
  });

  it('shows a "some tools are not available" banner when a selected tool is missing from availableTools', () => {
    const { getByText } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 't', type: 'github', settings: { selected_tools: ['create_issue', 'delete_issue'] } },
          toolSelection: { availableTools: ['create_issue'], onSelectedToolsChange: vi.fn() },
        })}
      />,
    );
    expect(getByText('Some tools are not available anymore.')).toBeInTheDocument();
  });

  it('shows the attention icon + refresh action and calls onRevalidate, when validation.hasIssue is true', async () => {
    const user = userEvent.setup();
    const onRevalidate = vi.fn();
    const { getByTestId } = renderWithTheme(
      <ToolCard
        {...baseProps({
          validation: { hasIssue: true, onRevalidate },
        })}
      />,
    );
    await user.click(getByTestId('agent-toolkit-refresh-button'));
    expect(onRevalidate).toHaveBeenCalledTimes(1);
  });

  it('renders the caller-supplied validation banner outside the tooltip-wrapped card', () => {
    const { getByText } = renderWithTheme(
      <ToolCard
        {...baseProps({
          validation: { hasIssue: true, banner: <div>Custom validation banner</div> },
        })}
      />,
    );
    expect(getByText('Custom validation banner')).toBeInTheDocument();
  });

  it('renders the mcp logout slot only when authorized (the login slot itself renders unconditionally when mcp, matching the baseline\'s McpLogInButton, which self-hides when already logged in)', () => {
    const { getByText } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 't', type: 'mcp', name: 'My MCP', settings: { url: 'https://mcp.example.com' } },
          delegatedAuth: { mcpIsAuthorized: true, mcpLoginSlot: <span>login-slot</span>, mcpLogoutSlot: <span>logout-slot</span> },
        })}
      />,
    );
    expect(getByText('login-slot')).toBeInTheDocument();
    expect(getByText('logout-slot')).toBeInTheDocument();
  });

  it('renders the mcp login slot (not logout) when not authorized', () => {
    const { getByText, queryByText } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 't', type: 'mcp', name: 'My MCP', settings: { url: 'https://mcp.example.com' } },
          delegatedAuth: { mcpIsAuthorized: false, mcpLoginSlot: <span>login-slot</span>, mcpLogoutSlot: <span>logout-slot</span> },
        })}
      />,
    );
    expect(getByText('login-slot')).toBeInTheDocument();
    expect(queryByText('logout-slot')).not.toBeInTheDocument();
  });

  it('does not render delegated-auth slots for a non-mcp/sharepoint/openapi toolkit', () => {
    const { queryByText } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 't', type: 'github', settings: {} },
          delegatedAuth: { sharepointLoginSlot: <span>sp-slot</span>, openApiLoginSlot: <span>oa-slot</span> },
        })}
      />,
    );
    expect(queryByText('sp-slot')).not.toBeInTheDocument();
    expect(queryByText('oa-slot')).not.toBeInTheDocument();
  });

  it('renders the sharepoint login slot for a sharepoint tool', () => {
    const { getByText } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 't', type: 'sharepoint', settings: {} },
          delegatedAuth: { sharepointLoginSlot: <span>sp-slot</span> },
        })}
      />,
    );
    expect(getByText('sp-slot')).toBeInTheDocument();
  });

  it('toggles variables and forwards edits through variables.onChangeVariable', async () => {
    const user = userEvent.setup();
    const onChangeVariable = vi.fn();
    const { getByText, getByLabelText } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 't', type: 'custom', settings: {}, variables: [{ name: 'TOKEN', value: 'abc' }] },
          variables: { onChangeVariable },
        })}
      />,
    );
    await user.click(getByText('Show variables'));
    expect(getByText('Hide variables')).toBeInTheDocument();
    await user.type(getByLabelText('TOKEN'), '!');
    expect(onChangeVariable).toHaveBeenCalledWith('TOKEN', 'abc!');
  });

  it('WITHHOLDS the whole variables control when no caller passes the variables prop, even for a tool that carries variables', () => {
    // #248: nothing on this backend can store per-tool variables, so the
    // control must not be offered at all — hidden, not disabled (the legacy
    // baseline omits this toggle rather than greying it out). The tool below
    // DOES carry variables, so a gate keyed only on `tool.variables` (the
    // shape this component shipped with) would still render the toggle.
    const props = baseProps({ tool: { id: 't', type: 'custom', settings: {}, variables: [{ name: 'TOKEN', value: 'abc' }] } });
    const { variables: _withheld, ...withoutVariables } = props;
    const { queryByText, queryByTestId, queryByLabelText } = renderWithTheme(<ToolCard {...withoutVariables} />);

    expect(queryByText('Show variables')).not.toBeInTheDocument();
    expect(queryByText('Hide variables')).not.toBeInTheDocument();
    // The panel itself, not just its toggle: `AgentVariables` renders the
    // editable field, and an assertion on the toggle alone would pass against
    // a card that still rendered the inputs underneath it.
    expect(queryByTestId('agent-variables')).not.toBeInTheDocument();
    expect(queryByLabelText('TOKEN')).not.toBeInTheDocument();
  });

  it('shows the duplicate warning tooltip and duplicate styling only when isDuplicate is true', () => {
    const { getByTestId } = renderWithTheme(
      <ToolCard
        {...baseProps({
          isDuplicate: true,
        })}
      />,
    );
    expect(getByTestId('agent-toolkit-card')).toBeInTheDocument();
  });

  it('disables the open-in-new-tab and delete actions when disabled is true', () => {
    const { getByTestId } = renderWithTheme(<ToolCard {...baseProps({ disabled: true })} />);
    expect(getByTestId('agent-toolkit-delete-button')).toBeDisabled();
    expect(getByTestId('agent-toolkit-open-button')).toBeDisabled();
  });

  it('opens a new tab for a plain toolkit using window.open', async () => {
    const user = userEvent.setup();
    const { getByTestId } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 42, type: 'github', name: 'GitHub', settings: {} },
          context: { viewMode: 'owner', selectedProjectId: 'proj-1' },
        })}
      />,
    );
    await user.click(getByTestId('agent-toolkit-open-button'));
    expect(window.open).toHaveBeenCalledWith(expect.stringContaining('/toolkits/all/42?viewMode=owner&name=GitHub'), '_blank');
  });

  it('opens a new tab for an agent/pipeline tool at its sub-agent id and version, using the agents path', async () => {
    const user = userEvent.setup();
    const { getByTestId } = renderWithTheme(
      <ToolCard
        {...baseProps({
          tool: { id: 't', type: 'application', name: 'Sub Agent', settings: { application_id: 7, application_version_id: 9 } },
          context: { viewMode: 'owner', selectedProjectId: 'proj-1' },
          versionSelector: { versions: [{ id: 9, name: 'v1' }], onSelectVersion: vi.fn() },
        })}
      />,
    );
    await user.click(getByTestId('agent-toolkit-open-button'));
    expect(window.open).toHaveBeenCalledWith(expect.stringContaining('/agents/all/7/9?viewMode=owner&name=Sub Agent'), '_blank');
  });
});
