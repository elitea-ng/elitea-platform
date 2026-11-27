import { screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '../lib/testTheme';
import { StatusChip, StatusDot } from '.';

describe('StatusChip', () => {
  it('label: a dot and the word, no MUI chip', () => {
    const { container } = renderWithTheme(
      <StatusChip
        tone="success"
        label="Active"
        data-testid="s"
      />,
    );
    expect(screen.getByText('Active')).toBeInTheDocument();
    expect(screen.getByTestId('s')).toHaveAttribute('data-tone', 'success');
    expect(screen.getByTestId('s')).toHaveAttribute('data-variant', 'label');
    expect(container.querySelector('[aria-hidden="true"][data-tone="success"]')).not.toBeNull();
  });

  it('label: an icon replaces the dot', () => {
    const { container } = renderWithTheme(
      <StatusChip
        tone="warning"
        label="Warning"
        icon={<svg data-testid="icon" />}
      />,
    );
    expect(screen.getByTestId('icon')).toBeInTheDocument();
    expect(container.querySelector('[aria-hidden="true"][data-tone]')).toBeNull();
  });

  it('chip: an outlined pill with the word', () => {
    renderWithTheme(
      <StatusChip
        tone="danger"
        label="Failed"
        variant="chip"
        data-testid="s"
      />,
    );
    const chip = screen.getByTestId('s');
    expect(chip).toHaveTextContent('Failed');
    expect(chip).toHaveAttribute('data-tone', 'danger');
    expect(chip).toHaveAttribute('data-variant', 'chip');
  });

  it('text: no outline', () => {
    renderWithTheme(
      <StatusChip
        tone="info"
        label="Queued"
        variant="text"
        data-testid="s"
      />,
    );
    expect(screen.getByTestId('s')).toHaveAttribute('data-variant', 'text');
    expect(screen.getByText('Queued')).toBeInTheDocument();
  });

  it('icon: the word is kept for screen readers only', () => {
    renderWithTheme(
      <StatusChip
        tone="neutral"
        label="Expired"
        variant="icon"
        data-testid="s"
      />,
    );
    const word = screen.getByText('Expired');
    expect(getComputedStyle(word).position).toBe('absolute');
    expect(getComputedStyle(word).overflow).toBe('hidden');
  });
});

describe('StatusDot', () => {
  it('is decoration with its tone', () => {
    renderWithTheme(
      <StatusDot
        tone="warning"
        data-testid="d"
      />,
    );
    const dot = screen.getByTestId('d');
    expect(dot).toHaveAttribute('aria-hidden', 'true');
    expect(dot).toHaveAttribute('data-tone', 'warning');
  });
});
