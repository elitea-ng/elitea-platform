import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import { getNodeColor, getNodeIconByType, isDeprecatedNodeType } from './node.helpers';
import { PipelineNodeTypes } from '../constants/flowEditor.constants';
import { DEFAULT_BRAND_PACK, buildEliteaTheme } from '@/shared/brand';

/**
 * Uses the real Elitea theme (`shared/ui/lib/testTheme.tsx`'s own rationale
 * applies here too, even though these are plain-function calls rather than
 * rendered components: `getNodeColor`/`getNodeIconByType` read `theme.vars.
 * palette.*`, which only a real `buildEliteaTheme` (or a `ThemeProvider`)
 * populates — a bare `createTheme()` mock would leave `.vars` empty and
 * every assertion below vacuous.
 */
const theme = buildEliteaTheme(DEFAULT_BRAND_PACK);

describe('getNodeColor', () => {
  it('reads theme.vars.palette.nodeColors[type] for a real node type', () => {
    expect(getNodeColor(PipelineNodeTypes.Tool, theme)).toBe(theme.vars.palette.nodeColors.tool);
    expect(getNodeColor(PipelineNodeTypes.Agent, theme)).toBe(theme.vars.palette.nodeColors.agent);
  });

  it("falls back to nodeColors['custom'] for an unrecognised type", () => {
    expect(getNodeColor('some_unrecognised_type', theme)).toBe(theme.vars.palette.nodeColors.custom);
  });

  it('falls back to the neutral text-secondary token when there is no theme at all', () => {
    expect(getNodeColor('tool', undefined)).toBe('');
  });
});

describe('isDeprecatedNodeType', () => {
  it('flags the six deprecated node types', () => {
    expect(isDeprecatedNodeType(PipelineNodeTypes.Function)).toBe(true);
    expect(isDeprecatedNodeType(PipelineNodeTypes.Condition)).toBe(true);
    expect(isDeprecatedNodeType(PipelineNodeTypes.Pipeline)).toBe(true);
    expect(isDeprecatedNodeType(PipelineNodeTypes.Loop)).toBe(true);
    expect(isDeprecatedNodeType(PipelineNodeTypes.LoopFromTool)).toBe(true);
    expect(isDeprecatedNodeType(PipelineNodeTypes.Tool)).toBe(true);
  });

  it('does not flag a current node type', () => {
    expect(isDeprecatedNodeType(PipelineNodeTypes.Agent)).toBe(false);
    expect(isDeprecatedNodeType(PipelineNodeTypes.Decision)).toBe(false);
  });
});

describe('getNodeIconByType', () => {
  it.each([
    PipelineNodeTypes.Mcp,
    PipelineNodeTypes.LLM,
    PipelineNodeTypes.Toolkit,
    PipelineNodeTypes.Tool,
    PipelineNodeTypes.Function,
    PipelineNodeTypes.Condition,
    PipelineNodeTypes.Decision,
    PipelineNodeTypes.LoopFromTool,
    PipelineNodeTypes.Loop,
    PipelineNodeTypes.Agent,
    PipelineNodeTypes.Pipeline,
    PipelineNodeTypes.Router,
    PipelineNodeTypes.StateModifier,
    PipelineNodeTypes.Code,
    PipelineNodeTypes.Printer,
    PipelineNodeTypes.Hitl,
    PipelineNodeTypes.Custom,
    PipelineNodeTypes.End,
    'some-unrecognised-type',
  ])('renders a real <svg> for %s (including the unrecognised-type fallback)', type => {
    const { container } = render(<>{getNodeIconByType(type, theme)}</>);
    expect(container.querySelector('svg')).not.toBeNull();
  });

  it('sizes the icon from the typography scale, not an ad-hoc literal', () => {
    const { container } = render(<>{getNodeIconByType(PipelineNodeTypes.Tool, theme)}</>);
    expect(container.querySelector('svg')?.getAttribute('style')).toContain(theme.typography.bodyMedium.fontSize);
  });

  it('honours an explicit colour override over the theme default', () => {
    const overrideColor = theme.vars.palette.error.main;
    const { container } = render(<>{getNodeIconByType(PipelineNodeTypes.Tool, theme, overrideColor)}</>);
    expect(container.querySelector('svg')?.getAttribute('fill')).toBe(overrideColor);
  });
});
