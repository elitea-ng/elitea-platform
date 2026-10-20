import { render, screen } from '@testing-library/react';
import userEvent from '@testing-library/user-event';
import { useState } from 'react';
import { describe, expect, it } from 'vitest';

import { AppProviders } from '@/app/providers/AppProviders';

import { Segmented } from '../ui/Segmented';

function Harness(): React.JSX.Element {
  const [value, setValue] = useState<'act' | 'plan'>('act');
  return (
    <AppProviders>
      <Segmented
        ariaLabel="Mode"
        value={value}
        onChange={setValue}
        options={[
          { value: 'act', label: 'Act' },
          { value: 'plan', label: 'Plan' },
        ]}
      />
    </AppProviders>
  );
}

describe('Segmented', () => {
  it('is a radio group: one segment chosen, a click or the arrow keys move it', async () => {
    const user = userEvent.setup();
    render(<Harness />);
    expect(screen.getByRole('radiogroup', { name: 'Mode' })).toBeInTheDocument();
    expect(screen.getByRole('radio', { name: 'Act' })).toBeChecked();
    await user.click(screen.getByRole('radio', { name: 'Plan' }));
    expect(screen.getByRole('radio', { name: 'Plan' })).toBeChecked();
    screen.getByRole('radio', { name: 'Plan' }).focus();
    await user.keyboard('{ArrowLeft}');
    expect(screen.getByRole('radio', { name: 'Act' })).toBeChecked();
  });
});
