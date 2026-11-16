import { useState } from 'react';

import { screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { describe, expect, it } from 'vitest';

import { TAG_NAME_MAX_LENGTH } from '@/shared/lib/limits';

import { renderWithProviders } from '../__tests__/testUtils';
import { SkillTagsInput } from './SkillTagsInput';

function Harness({ initial = [] as readonly string[] }): React.ReactNode {
  const [tags, setTags] = useState<readonly string[]>(initial);
  return (
    <>
      <SkillTagsInput
        value={tags}
        onChange={setTags}
      />
      <output data-testid="tags">{tags.join('|')}</output>
    </>
  );
}

describe('SkillTagsInput', () => {
  it('adds a tag on comma', async () => {
    const user = userEvent.setup();
    renderWithProviders(<Harness />);
    await user.type(screen.getByTestId('skill-tags-input'), 'alpha,');
    expect(screen.getByTestId('tags')).toHaveTextContent('alpha');
  });

  // The review of #2133: a too-long tag was cut to the cap without a word.
  it('refuses a tag over the length cap with a message instead of truncating it', async () => {
    const user = userEvent.setup();
    renderWithProviders(<Harness />);
    const long = 'x'.repeat(TAG_NAME_MAX_LENGTH + 1);
    await user.type(screen.getByTestId('skill-tags-input'), `${long},`);
    expect(screen.getByTestId('tags')).toHaveTextContent(/^$/);
    expect(screen.getByText(/can be at most/)).toBeInTheDocument();
  });
});
