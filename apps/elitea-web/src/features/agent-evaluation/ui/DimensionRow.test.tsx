/**
 * Legacy issue 6694: the dimension card shows its evaluator, target and
 * importance as tags with delayed tooltips, and ellipsizes a long name. The
 * scale range and polarity stay on the card as tags of their own.
 */
import { act, fireEvent, screen } from '@testing-library/react';
import { afterEach, describe, expect, it, vi } from 'vitest';

import { renderWithEvaluationProviders } from '../__tests__/testUtils';
import { EVAL_ENGINE, EVAL_POLARITY, EVAL_SCALE_TYPE, EVAL_TIER, type EvalDimension } from '../model/types';
import { DimensionRow } from './DimensionRow';

function dimension(overrides: Partial<EvalDimension> = {}): EvalDimension {
  return {
    id: '1',
    uuid: 'e0f1',
    name: 'Faithfulness',
    description: 'Grounded?',
    tier: EVAL_TIER.project,
    application_id: null,
    allowed_engines: [EVAL_ENGINE.ai, EVAL_ENGINE.human],
    scale_type: EVAL_SCALE_TYPE.continuous,
    scale_min: 0,
    scale_max: 100,
    polarity: EVAL_POLARITY.higherBetter,
    default_weight: 2,
    default_target: 80,
    default_target_operator: '>=',
    code: '',
    return_contract: '',
    ...overrides,
  };
}

function renderRow(row: EvalDimension): void {
  renderWithEvaluationProviders(<DimensionRow dimension={row} canEdit canDelete onEdit={() => {}} onDelete={() => {}} />);
}

afterEach(() => {
  vi.useRealTimers();
});

describe('DimensionRow', () => {
  it('renders one tag per engine, the target and the importance', () => {
    renderRow(dimension());

    expect(screen.getByTestId('evaluation-dimension-tag-engine-ai-1')).toHaveTextContent('AI');
    expect(screen.getByTestId('evaluation-dimension-tag-engine-human-1')).toHaveTextContent('Human');
    expect(screen.getByTestId('evaluation-dimension-tag-target-1')).toHaveTextContent('Target >= 80');
    expect(screen.getByTestId('evaluation-dimension-tag-importance-1')).toHaveTextContent('Weight 2');
  });

  it('draws no target tag for a dimension with no target', () => {
    renderRow(dimension({ default_target: null, default_target_operator: '' }));

    expect(screen.queryByTestId('evaluation-dimension-tag-target-1')).not.toBeInTheDocument();
  });

  // The pre-6694 row printed "engines · min-max · polarity". The tags must not
  // drop the last two: they decide how the target reads.
  it('keeps the scale range and the polarity on the card', () => {
    renderRow(dimension());

    expect(screen.getByTestId('evaluation-dimension-tag-scale-1')).toHaveTextContent('Scale 0–100');
    expect(screen.getByTestId('evaluation-dimension-tag-polarity-1')).toHaveTextContent('Higher is better');
  });

  it('says lower is better for an inverse dimension', () => {
    renderRow(dimension({ polarity: EVAL_POLARITY.lowerBetter, scale_min: 1, scale_max: 5 }));

    expect(screen.getByTestId('evaluation-dimension-tag-scale-1')).toHaveTextContent('Scale 1–5');
    expect(screen.getByTestId('evaluation-dimension-tag-polarity-1')).toHaveTextContent('Lower is better');
  });

  it('ellipsizes the name on one line', () => {
    renderRow(dimension({ name: 'A very long dimension name that does not fit the card' }));

    const name = screen.getByTestId('evaluation-dimension-name-1');
    expect(name).toHaveStyle({ overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap' });
    // The three rules above do nothing on an inline box, and the Typography
    // renders a <span>. jsdom does no layout, so the test pins the display
    // value that makes the clip and the ellipsis apply in a real browser.
    expect(name.tagName).toBe('SPAN');
    expect(name).toHaveStyle({ display: 'block' });
  });

  it.each([
    [EVAL_ENGINE.ai, 'This dimension is evaluated using the suite’s judge model.'],
    [EVAL_ENGINE.human, 'This dimension requires manual review.'],
    [EVAL_ENGINE.code, 'This dimension is evaluated using Python validation logic.'],
  ])('shows the %s evaluator tooltip only after the hover delay', async (engine, text) => {
    vi.useFakeTimers();
    renderRow(dimension({ allowed_engines: [engine] }));

    fireEvent.mouseOver(screen.getByTestId(`evaluation-dimension-tag-engine-${engine}-1`));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(500);
    });
    expect(screen.queryByRole('tooltip')).not.toBeInTheDocument();

    await act(async () => {
      await vi.advanceTimersByTimeAsync(1600);
    });
    expect(screen.getByRole('tooltip')).toHaveTextContent(text);
  });

  it('explains the scale tag', async () => {
    vi.useFakeTimers();
    renderRow(dimension());

    fireEvent.mouseOver(screen.getByTestId('evaluation-dimension-tag-scale-1'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100);
    });
    expect(screen.getByRole('tooltip')).toHaveTextContent('The range a score falls in.');
  });

  it('explains the target and the importance tags', async () => {
    vi.useFakeTimers();
    renderRow(dimension());

    fireEvent.mouseOver(screen.getByTestId('evaluation-dimension-tag-target-1'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100);
    });
    expect(screen.getByRole('tooltip')).toHaveTextContent(
      'The score or rating that must satisfy the selected success criterion for this dimension to pass.',
    );
    fireEvent.mouseLeave(screen.getByTestId('evaluation-dimension-tag-target-1'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(1000);
    });

    fireEvent.mouseOver(screen.getByTestId('evaluation-dimension-tag-importance-1'));
    await act(async () => {
      await vi.advanceTimersByTimeAsync(2100);
    });
    expect(screen.getByRole('tooltip')).toHaveTextContent(
      'Indicates how significant this dimension is when interpreting the overall evaluation result.',
    );
  });
});
