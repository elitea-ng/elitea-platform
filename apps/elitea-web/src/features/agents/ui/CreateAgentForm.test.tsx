import { useCallback, useState, type ReactNode } from 'react';

import { describe, expect, it, vi } from 'vitest';
import { fireEvent, screen, waitFor } from '@testing-library/react';
import userEvent from '@testing-library/user-event';

import { installCodeMirrorTestPolyfills } from '@/shared/ui/lib/field/codeMirrorTestPolyfills';

import { renderWithProviders } from '../__tests__/testUtils';
import { setFieldValueAtPath } from '../lib/agentFieldChange';
import type { AgentDraftValues } from '../model/types';

import { CreateAgentForm } from './CreateAgentForm';

installCodeMirrorTestPolyfills();

const baseValues: AgentDraftValues = {
  name: 'My Agent',
  description: 'Does things',
  version_details: {
    id: 1,
    instructions: 'Be helpful.',
    welcome_message: 'Hi!',
    variables: [],
    meta: { step_limit: 25 },
  },
};

describe('CreateAgentForm', () => {
  it('renders the name and description fields with their current values', () => {
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
      />,
    );
    expect(screen.getByTestId('agent-name-input')).toHaveValue('My Agent');
    expect(screen.getByTestId('agent-description-input')).toHaveValue('Does things');
  });

  it('shows no validation error before the name field has been touched', () => {
    renderWithProviders(
      <CreateAgentForm
        values={{ ...baseValues, name: '' }}
        onFieldChange={vi.fn()}
      />,
    );
    expect(screen.queryByText('Name is required')).not.toBeInTheDocument();
  });

  it('shows a validation error under the name field once blurred empty', () => {
    renderWithProviders(
      <CreateAgentForm
        values={{ ...baseValues, name: '' }}
        onFieldChange={vi.fn()}
      />,
    );
    const nameInput = screen.getByTestId('agent-name-input');
    fireEvent.blur(nameInput);
    expect(screen.getByText('Name is required')).toBeInTheDocument();
    expect(nameInput).toHaveAttribute('aria-invalid', 'true');
  });

  it('clears the name validation error once the user types a non-blank value', () => {
    const onFieldChange = vi.fn();
    renderWithProviders(
      <CreateAgentForm
        values={{ ...baseValues, name: '' }}
        onFieldChange={onFieldChange}
      />,
    );
    const nameInput = screen.getByTestId('agent-name-input');
    fireEvent.blur(nameInput);
    expect(screen.getByText('Name is required')).toBeInTheDocument();

    fireEvent.change(nameInput, { target: { value: 'Agent' } });
    expect(screen.queryByText('Name is required')).not.toBeInTheDocument();
  });

  it('shows a validation error under the description field once blurred empty', () => {
    renderWithProviders(
      <CreateAgentForm
        values={{ ...baseValues, description: '' }}
        onFieldChange={vi.fn()}
      />,
    );
    const descriptionInput = screen.getByTestId('agent-description-input');
    fireEvent.blur(descriptionInput);
    expect(screen.getByText('Description is required')).toBeInTheDocument();
    expect(descriptionInput).toHaveAttribute('aria-invalid', 'true');
  });

  it('disables the name/description/instructions/welcome-message fields when disabled is true', () => {
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
        disabled
      />,
    );
    expect(screen.getByTestId('agent-name-input')).toBeDisabled();
    expect(screen.getByTestId('agent-description-input')).toBeDisabled();
    expect(screen.getByTestId('agent-welcome-message-input')).toBeDisabled();
  });

  it('calls onFieldChange with the trimmed name on blur', () => {
    const onFieldChange = vi.fn();
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={onFieldChange}
      />,
    );
    const nameInput = screen.getByTestId('agent-name-input');
    fireEvent.change(nameInput, { target: { value: '  Renamed  ' } });
    fireEvent.blur(nameInput);
    expect(onFieldChange).toHaveBeenCalledWith('name', 'Renamed');
  });

  it('calls onFieldChange for the description field on change', () => {
    const onFieldChange = vi.fn();
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={onFieldChange}
      />,
    );
    fireEvent.change(screen.getByTestId('agent-description-input'), { target: { value: 'New description' } });
    expect(onFieldChange).toHaveBeenCalledWith('description', 'New description');
  });

  it('renders the instructions editor when showInstructions is true (default)', () => {
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
      />,
    );
    expect(screen.getByText('Be helpful.')).toBeInTheDocument();
  });

  it('does not render the instructions editor when showInstructions is false', () => {
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
        showInstructions={false}
      />,
    );
    expect(screen.queryByText('Be helpful.')).not.toBeInTheDocument();
  });

  it('renders no action row above the editor when no AI-edit slot is supplied', () => {
    // The wrapper carries its own bottom margin, so rendering it around an empty slot
    // would leave a permanent gap above the instructions editor. The "Edit with AI"
    // trigger is absent in every deployment whose backend cannot serve it, which makes
    // the empty case the common one rather than the exception.
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
      />,
    );
    expect(screen.queryByTestId('agent-instructions-ai-edit-slot')).not.toBeInTheDocument();
  });

  it('renders the action row when an AI-edit slot is supplied', () => {
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
        instructionsAiEditSlot={<button type="button">Edit with AI</button>}
      />,
    );
    expect(screen.getByTestId('agent-instructions-ai-edit-slot')).toBeInTheDocument();
    expect(screen.getByRole('button', { name: 'Edit with AI' })).toBeInTheDocument();
  });

  it('renders the welcome message input with the current value', () => {
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
      />,
    );
    expect(screen.getByTestId('agent-welcome-message-input')).toHaveValue('Hi!');
  });

  it('renders the step-limit field from version_details.meta.step_limit', () => {
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
      />,
    );
    expect(screen.getByDisplayValue('25')).toBeInTheDocument();
  });

  it('renders the generateAgentButtonSlot outside pipeline mode', () => {
    // Non-`<button>` slot content — the accordion summary row this slot
    // renders into is ITSELF a native `<button>` (see this prop's own doc
    // comment on `CreateAgentFormProps`); a nested `<button>` is invalid
    // HTML, confirmed the hard way (React's own console warning) while
    // writing this test.
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
        generateAgentButtonSlot={<span>Generate</span>}
      />,
    );
    expect(screen.getByText('Generate')).toBeInTheDocument();
  });

  it('does not render the generateAgentButtonSlot in pipeline mode', () => {
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
        generateAgentButtonSlot={<span>Generate</span>}
        entityType="pipeline"
      />,
    );
    expect(screen.queryByText('Generate')).not.toBeInTheDocument();
  });

  // A declared-but-never-rendered slot prop is this codebase's recurring
  // failure (#126/#129/#134): the type-checks pass, the unit suite is green,
  // and the control simply is not on the page. Asserted through the whole
  // chain, since the slot travels a second hop into
  // `ApplicationAdvanceSettings`.
  it('renders the modelSettingsSlot inside the advanced-settings panel', () => {
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
        modelSettingsSlot={<div>MODEL PICKER</div>}
      />,
    );
    expect(screen.getByText('MODEL PICKER')).toBeVisible();
  });

  it('renders the iconSlot/tagsSlot content where supplied', () => {
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
        iconSlot={<div>ICON</div>}
        tagsSlot={<div>TAGS</div>}
      />,
    );
    expect(screen.getByText('ICON')).toBeVisible();
    expect(screen.getByText('TAGS')).toBeVisible();
  });

  // #307 — the conversation-starters editor is rendered by this component
  // itself now (it was a slot every caller left empty). Asserted through the
  // callback, not by the section merely existing: an editor that renders and
  // routes nothing is the exact defect this closes.
  it('renders the conversation-starters editor and routes an edit to version_details.conversation_starters', async () => {
    const user = userEvent.setup();
    const onFieldChange = vi.fn();
    renderWithProviders(
      <CreateAgentForm
        values={{
          ...baseValues,
          version_details: { ...baseValues.version_details, conversation_starters: ['first'] },
        }}
        onFieldChange={onFieldChange}
      />,
    );

    const input = screen.getByTestId('agent-conversation-starter-input');
    expect(input).toBeVisible();
    expect(input).toHaveValue('first');

    await user.type(input, '!');
    expect(onFieldChange).toHaveBeenCalledWith('version_details.conversation_starters', ['first!']);
  });

  it('does not render ApplicationVariables when there are no variables', () => {
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={vi.fn()}
      />,
    );
    expect(screen.queryByText('Variables')).not.toBeInTheDocument();
  });

  /*
   * Variable AUTHORING, end to end through the real editor. `CreateAgentForm`
   * is controlled, so these drive it through the same round trip every page
   * mount performs (`setFieldValueAtPath` is the very implementation
   * `lib/useAgentEditorCreate.ts` uses) — a `vi.fn()` `onFieldChange` would
   * only prove the call was made, not that the row actually renders.
   */
  function StatefulCreateAgentForm({ initial }: { readonly initial: AgentDraftValues }): ReactNode {
    const [values, setValues] = useState<AgentDraftValues>(initial);
    const onFieldChange = useCallback((path: string, value: unknown) => {
      setValues((previous) => setFieldValueAtPath(previous, path, value));
    }, []);
    return (
      <CreateAgentForm
        values={values}
        onFieldChange={onFieldChange}
      />
    );
  }

  /** CM6's `.cm-content` is the editable surface the instructions field types into. */
  function getInstructionsEditor(container: HTMLElement): HTMLElement {
    const content = container.querySelector('.cm-content');
    if (!(content instanceof HTMLElement)) throw new Error('CodeMirror content element not found');
    return content;
  }

  it('surfaces a variable row when a placeholder is typed into the instructions', { timeout: 20_000 }, async () => {
    const user = userEvent.setup({ delay: null });
    const { container } = renderWithProviders(
      <StatefulCreateAgentForm initial={{ ...baseValues, version_details: { ...baseValues.version_details, instructions: '' } }} />,
    );

    expect(screen.queryByTestId('application-variables')).not.toBeInTheDocument();

    await user.click(getInstructionsEditor(container));
    // `{{` is user-event's escape for a literal `{`; `}` needs none.
    await user.keyboard('Summarize {{{{topic}}');

    expect(await screen.findByTestId('application-variables')).toBeInTheDocument();
    expect(await screen.findByLabelText('topic')).toHaveValue('');
  });

  it('keeps a value typed into a derived row when the instructions are edited again', { timeout: 20_000 }, async () => {
    const user = userEvent.setup({ delay: null });
    const { container } = renderWithProviders(
      <StatefulCreateAgentForm
        initial={{
          ...baseValues,
          version_details: { ...baseValues.version_details, instructions: 'About {{topic}}', variables: [{ name: 'topic', value: '' }] },
        }}
      />,
    );

    await user.type(await screen.findByLabelText('topic'), 'weather');
    await waitFor(() => expect(screen.getByLabelText('topic')).toHaveValue('weather'));

    await user.click(getInstructionsEditor(container));
    await user.keyboard('{End} and {{{{tone}}');

    expect(await screen.findByLabelText('tone')).toHaveValue('');
    expect(screen.getByLabelText('topic')).toHaveValue('weather');
  });

  it('removes the row once its placeholder is deleted from the instructions', { timeout: 20_000 }, async () => {
    const user = userEvent.setup({ delay: null });
    const { container } = renderWithProviders(
      <StatefulCreateAgentForm
        initial={{
          ...baseValues,
          version_details: { ...baseValues.version_details, instructions: 'About {{topic}}', variables: [{ name: 'topic', value: '' }] },
        }}
      />,
    );

    expect(await screen.findByLabelText('topic')).toBeInTheDocument();

    await user.click(getInstructionsEditor(container));
    // Select-all + retype: the whole placeholder goes, so the row must too.
    await user.keyboard('{Control>}a{/Control}About the weather.');

    await waitFor(() => expect(screen.queryByLabelText('topic')).not.toBeInTheDocument());
    expect(screen.queryByTestId('application-variables')).not.toBeInTheDocument();
  });

  // #848 — a real mousedown on `+ Starter` blurs the still-focused welcome
  // field first; the old code unmounted that field's counter on blur,
  // shrinking the layout and moving `+ Starter` out from under the pointer
  // before mouseup, so the click never landed. `user.click` here exercises
  // the same pointerdown/blur/pointerup/click sequence a real mouse
  // produces (unlike a plain `fireEvent.click`, which fires `click` alone
  // with no intervening blur) — with the counter's line now reserved
  // instead of unmounted, one click still reaches the button and adds a row.
  it('adds a conversation-starter row on a single click on + Starter while the welcome message is still focused', async () => {
    const user = userEvent.setup();
    const onFieldChange = vi.fn();
    renderWithProviders(
      <CreateAgentForm
        values={baseValues}
        onFieldChange={onFieldChange}
      />,
    );

    const welcomeInput = screen.getByTestId('agent-welcome-message-input');
    await user.click(welcomeInput);
    await user.type(welcomeInput, 'Hello!');
    expect(welcomeInput).toHaveFocus();

    await user.click(screen.getByTestId('agent-conversation-starter-add'));

    expect(onFieldChange).toHaveBeenCalledWith('version_details.conversation_starters', ['']);
  });

  it('renders ApplicationVariables and forwards edits when variables are present', () => {
    const onFieldChange = vi.fn();
    renderWithProviders(
      <CreateAgentForm
        values={{
          ...baseValues,
          version_details: { ...baseValues.version_details, variables: [{ name: 'topic', value: 'weather' }] },
        }}
        onFieldChange={onFieldChange}
      />,
    );
    fireEvent.change(screen.getByDisplayValue('weather'), { target: { value: 'sports' } });
    expect(onFieldChange).toHaveBeenCalledWith('version_details.variables', [{ name: 'topic', value: 'sports' }]);
  });
});
