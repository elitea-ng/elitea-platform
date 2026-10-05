import { describe, expect, it } from 'vitest';

import chartPalette from './chart-palette.json';
import {
  CHART_COLORS,
  CUSTOM_DATE_PRESET,
  DATE_FILTER_PRESETS,
  DEFAULT_DATE_PRESET,
  EVENT_TYPE_COLORS,
  GUIDE_SECTIONS,
  MEDAL_COLORS,
  pickChartColor,
  pickMedalColor,
} from './constants';

describe('CHART_COLORS / MEDAL_COLORS / EVENT_TYPE_COLORS data', () => {
  it('CHART_COLORS re-exports the chart-palette.json ramp unchanged (R-T1: no raw colour literal, even in a test — compare against the JSON source, not a duplicated hex string)', () => {
    expect(CHART_COLORS).toHaveLength(10);
    expect(CHART_COLORS).toEqual(chartPalette.chartColors);
  });

  it('MEDAL_COLORS has exactly 3 rank colours', () => {
    expect(MEDAL_COLORS).toHaveLength(3);
  });

  it('EVENT_TYPE_COLORS covers every baseline event type', () => {
    expect(Object.keys(EVENT_TYPE_COLORS).sort()).toEqual(
      ['agent', 'api', 'chat', 'llm', 'rpc', 'socketio', 'tool'].sort(),
    );
  });
});

describe('pickChartColor', () => {
  it('wraps around CHART_COLORS by modulo', () => {
    expect(pickChartColor(0)).toBe(CHART_COLORS[0]);
    expect(pickChartColor(9)).toBe(CHART_COLORS[9]);
    expect(pickChartColor(10)).toBe(CHART_COLORS[0]);
    expect(pickChartColor(23)).toBe(CHART_COLORS[3]);
  });

  it('handles a negative index without throwing (JS modulo of a negative stays in-bounds via the ?? fallback)', () => {
    expect(() => pickChartColor(-1)).not.toThrow();
    expect(typeof pickChartColor(-1)).toBe('string');
  });
});

describe('pickMedalColor', () => {
  it('returns the medal colour for ranks 0-2', () => {
    expect(pickMedalColor(0)).toBe(MEDAL_COLORS[0]);
    expect(pickMedalColor(1)).toBe(MEDAL_COLORS[1]);
    expect(pickMedalColor(2)).toBe(MEDAL_COLORS[2]);
  });

  it('falls back to the chart ramp beyond the medal set', () => {
    expect(pickMedalColor(3)).toBe(pickChartColor(3));
  });
});

describe('DATE_FILTER_PRESETS', () => {
  it('has the baseline 4 presets with string values (TabGroupButtonItem contract)', () => {
    expect(DATE_FILTER_PRESETS.map((preset) => preset.value)).toEqual(['1', '7', '30', '90']);
    expect(DATE_FILTER_PRESETS.map((preset) => preset.days)).toEqual([1, 7, 30, 90]);
  });

  // #6791: calendar-day labels; `Today` replaced the rolling `Last 24h`.
  it('labels the presets Today | Last 7d | Last 30d | Last 90d, Today first and default', () => {
    expect(DATE_FILTER_PRESETS.map((preset) => preset.label)).toEqual(['Today', 'Last 7d', 'Last 30d', 'Last 90d']);
    expect(DEFAULT_DATE_PRESET.label).toBe('Today');
    expect(DATE_FILTER_PRESETS).not.toContain(CUSTOM_DATE_PRESET);
  });
});

describe('GUIDE_SECTIONS', () => {
  it('has the baseline 7 sections, each with at least one metric', () => {
    expect(GUIDE_SECTIONS).toHaveLength(7);
    for (const section of GUIDE_SECTIONS) {
      expect(section.metrics.length).toBeGreaterThan(0);
    }
  });

  it('reproduces a sample of baseline copy verbatim (COPY-050 acceptance)', () => {
    const overview = GUIDE_SECTIONS.find((section) => section.title === 'Overview Tab');
    expect(overview).toBeDefined();
    expect(overview?.metrics.map((metric) => metric.name)).toContain('TEAM');
  });

  // #6763: no Active Days column exists in this app; the guide described one.
  it('does not document an Active Days metric', () => {
    const text = JSON.stringify(GUIDE_SECTIONS).toLowerCase();
    expect(text).not.toContain('active days');
  });

  // #6791: the Date Range entry describes calendar-day presets.
  it('describes the calendar-day presets in the Date Range entry', () => {
    const entry = GUIDE_SECTIONS.flatMap((section) => section.metrics).find((metric) => metric.name === 'Date Range');
    expect(entry?.description).toContain('Use Today, Last 7d, Last 30d, or Last 90d for calendar-day ranges.');
    expect(entry?.description).not.toContain('24h');
  });
});
