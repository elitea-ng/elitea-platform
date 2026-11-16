import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { HttpResponse, http } from 'msw';
import { describe, expect, it, vi } from 'vitest';

import { configureGeneratedClient } from '@/shared/api/generated/mutator';

import { server } from '../../../test/setup';
import { renderWithProviders } from '../__tests__/testUtils';
import { SkillForm } from './SkillForm';

const value = {
  name: 'Reviewer',
  description: 'Reviews code',
  instructions: '# Review\nBe careful.',
  tags: ['quality'],
};

describe('SkillForm', () => {
  it('renders all fields and reports edits', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithProviders(
      <SkillForm
        value={value}
        onChange={onChange}
      />,
    );
    await user.clear(screen.getByTestId('skill-name-input'));
    await user.type(screen.getByTestId('skill-name-input'), 'New');
    expect(onChange).toHaveBeenCalled();
    expect(screen.getByText('quality')).toBeInTheDocument();
  });

  // #2101: the old comma-joined text field parsed the comma away as it was
  // typed and had no Enter handler, so no tag could be added.
  it('adds a tag on Enter and on a comma', async () => {
    const user = userEvent.setup();
    const onChange = vi.fn();
    renderWithProviders(
      <SkillForm
        value={value}
        onChange={onChange}
      />,
    );
    await user.type(screen.getByTestId('skill-tags-input'), 'review{Enter}');
    expect(onChange).toHaveBeenLastCalledWith({ ...value, tags: ['quality', 'review'] });
    await user.type(screen.getByTestId('skill-tags-input'), 'ops,');
    expect(onChange).toHaveBeenLastCalledWith({ ...value, tags: ['quality', 'ops'] });
  });

  it('switches between Markdown edit and preview', async () => {
    const user = userEvent.setup();
    renderWithProviders(
      <SkillForm
        value={value}
        onChange={vi.fn()}
      />,
    );
    await user.click(screen.getByRole('button', { name: 'Preview' }));
    expect(screen.getByTestId('skill-instructions-preview')).toHaveTextContent('Review');
    await user.click(screen.getByRole('button', { name: 'Edit' }));
    expect(screen.getByTestId('skill-instructions-input')).toBeInTheDocument();
  });

  it('shows validation feedback, and disables the icon control when there is no version to bind to', () => {
    renderWithProviders(
      <SkillForm
        value={{ name: '', description: '', instructions: '', tags: [] }}
        onChange={vi.fn()}
        showErrors
      />,
    );
    expect(screen.getAllByText(/required/i)).toHaveLength(3);
    // No `icon` prop — a skill being created has no version id, so the bind
    // would have nowhere to land. It is disabled for that reason, not because
    // the feature is missing.
    expect(screen.getByRole('button', { name: 'Icon' })).toBeDisabled();
  });

  it('enables the icon control and opens the picker once a version exists', async () => {
    const user = userEvent.setup();
    configureGeneratedClient({ baseUrl: '/api/v2' });
    server.use(
      http.get('/api/v2/elitea_core/default_icons/prompt_lib/7', () => HttpResponse.json([])),
      http.get('/api/v2/elitea_core/upload_skill_icon/prompt_lib/7', () =>
        HttpResponse.json({ rows: [{ name: 'skill_a.png', url: '/icons/7/skill_a.png' }], total: 1 }),
      ),
    );
    const onIconChange = vi.fn();
    renderWithProviders(
      <SkillForm
        value={value}
        onChange={vi.fn()}
        icon={{ projectId: '7', versionId: '42', iconMeta: null, onIconChange }}
      />,
    );

    const button = screen.getByTestId('skill-icon-button');
    expect(button).toBeEnabled();
    await user.click(button);

    // The gallery must actually show the uploaded icon: an empty "Uploaded"
    // section is what a mis-unwrapped {rows,total} body renders, with a 200 in
    // the network tab and nothing in the console.
    const tile = await screen.findByAltText('skill_a.png');
    await user.click(tile);
    expect(onIconChange).toHaveBeenCalledWith({ name: 'skill_a.png', url: '/icons/7/skill_a.png' });
  });

  it('offers AI generation only when the caller supplies it', async () => {
    const user = userEvent.setup();
    const onGenerate = vi.fn();
    const { rerender } = renderWithProviders(
      <SkillForm
        value={value}
        onChange={vi.fn()}
        onGenerate={onGenerate}
      />,
    );
    await user.click(screen.getByRole('button', { name: 'Generate with AI' }));
    expect(onGenerate).toHaveBeenCalledOnce();
    rerender(
      <SkillForm
        value={value}
        onChange={vi.fn()}
      />,
    );
    expect(screen.queryByRole('button', { name: 'Generate with AI' })).not.toBeInTheDocument();
  });
});
