import { fireEvent, screen } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { SubAgentAccordion } from './SubAgentAccordion';

const summary = () => screen.getByTestId('chat-answer-nested-agent-accordion-summary-Sub');

describe('SubAgentAccordion (reference parity)', () => {
  it('is collapsed by default and shows the breadcrumb label', () => {
    renderWithTheme(<SubAgentAccordion name="Sub" label="Parent (2) ▸ Sub"><span>child</span></SubAgentAccordion>);
    expect(summary().textContent).toContain('Parent (2) ▸ Sub');
    expect(summary().getAttribute('aria-expanded')).toBe('false');
    expect(screen.queryByText('child')).toBeNull();
  });

  it('opens on demand, and `defaultExpanded` opens it initially', () => {
    renderWithTheme(<SubAgentAccordion name="Sub" defaultExpanded><span>child</span></SubAgentAccordion>);
    expect(screen.getByText('child')).toBeTruthy();
    fireEvent.click(summary());
    expect(summary().getAttribute('aria-expanded')).toBe('false');
  });

  it('shows a spinner only while running and not paused', () => {
    const { rerender } = renderWithTheme(<SubAgentAccordion name="Sub" running />);
    expect(summary().querySelector('.MuiCircularProgress-root')).not.toBeNull();
    rerender(<SubAgentAccordion name="Sub" running paused />);
    expect(summary().querySelector('.MuiCircularProgress-root')).toBeNull();
  });

  it('has no italic "Paused — awaiting resume" text', () => {
    renderWithTheme(<SubAgentAccordion name="Sub" paused defaultExpanded><span>child</span></SubAgentAccordion>);
    expect(screen.queryByText(/awaiting resume/i)).toBeNull();
  });
});
