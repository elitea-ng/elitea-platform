import { screen } from '@testing-library/react';
import { describe, expect, it, vi } from 'vitest';

import { renderWithTheme } from '@/shared/ui/lib/testTheme';

import { ApprovalCard } from './ApprovalCard';

describe('ApprovalCard', () => {
  it("shows a rule's justification and which rule asks", () => {
    renderWithTheme(<ApprovalCard title="Run npm publish" justification="Publish through the release pipeline." source="user" rule="user#1" approveLabel="Approve" onApprove={vi.fn()} />);
    expect(screen.getByTestId('approval-justification')).toHaveTextContent('Why: Publish through the release pipeline.');
    expect(screen.getByTestId('approval-rule-source')).toHaveTextContent('Asked by your rule 1 (every workspace)');
  });

  it('shows neither without a rule', () => {
    renderWithTheme(<ApprovalCard title="Continue?" approveLabel="Continue" onApprove={vi.fn()} />);
    expect(screen.queryByTestId('approval-justification')).toBeNull();
    expect(screen.queryByTestId('approval-rule-source')).toBeNull();
  });
});
