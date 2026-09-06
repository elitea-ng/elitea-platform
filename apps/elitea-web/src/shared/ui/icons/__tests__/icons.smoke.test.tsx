import { createRef } from 'react';
import { render } from '@testing-library/react';
import { describe, expect, it } from 'vitest';

import type { SvgIconComponent } from '../svg-icon.types';

/**
 * Table-driven smoke test for the whole icon set (unit S2, §3.7/§4.4 R-T8).
 *
 * Icon components are mechanical one-line re-exports (`export { default as
 * XxxIcon } from './svg/xxx.svg?react'`) generated from the 193 icons ported
 * from apps/elitea-ui/src/assets/**, plus `LogoMarkIcon` — the gradient brand
 * ORB, which that port did not carry over (the sidebar substituted the
 * wordmark for it and rendered an illegible smudge), plus the three brand
 * glyphs the toolkit-type catalogue needs (`GithubIcon`/`GitlabIcon`/
 * `ConfluenceIcon`) — the baseline keeps those as inline JSX under
 * `src/components/Icons/` rather than as files under `src/assets/`, which is
 * why the asset sweep missed them. 197 in total. Per-icon hand-written tests would be
 * pure boilerplate, so this file auto-discovers every sibling icon module via
 * `import.meta.glob` and asserts, for each one:
 *  - it renders an <svg> element (the vite-plugin-svgr transform produced a
 *    real component, not a broken/empty one);
 *  - it forwards a ref to that root <svg> element (svgrOptions.ref: true in
 *    vite.config.ts, typed by ../svg-react.d.ts);
 *  - it accepts arbitrary DOM props (className, aria-hidden) the way any
 *    <svg> would, since consumers style icons via className/props, not by
 *    editing the generated source.
 *
 * New icons need no test changes — the glob picks them up automatically.
 */
const modules = import.meta.glob<Record<string, SvgIconComponent>>('../*.tsx', { eager: true });

interface IconEntry {
  moduleId: string;
  name: string;
  Component: SvgIconComponent;
}

const icons: IconEntry[] = Object.entries(modules).flatMap(([moduleId, mod]) =>
  Object.entries(mod)
    .filter(([exportName]) => exportName !== 'default')
    .map(([name, Component]) => ({ moduleId, name, Component })),
);

describe('shared/ui/icons — full-set smoke test', () => {
  it('discovered the full ported set (199 icons, see final report for the merge/rename ledger)', () => {
    // 193 ported + `LogoMarkIcon` + the 3 catalogue brand glyphs (see this
    // file's header) + two the settings port added because the baseline maps
    // them and this set did not hold them: `BellIcon` (Settings › Notifications'
    // nav item, which fell back to the generic gear) and `ContrastIcon` (the
    // theme toggle's "System").
    expect(icons.length).toBe(199);
  });

  it('every discovered export has a unique PascalCase "*Icon" name', () => {
    const names = icons.map((i) => i.name);
    expect(new Set(names).size).toBe(names.length);
    for (const name of names) {
      expect(name, name).toMatch(/^[A-Z][A-Za-z0-9]*Icon$/);
    }
  });

  it.each(icons.map((i): [string, IconEntry] => [i.name, i]))(
    '%s renders a root <svg>, forwards a ref to it, and spreads extra props',
    (_name, { Component }) => {
      const ref = createRef<SVGSVGElement>();
      const { container, unmount } = render(
        <Component ref={ref} className="probe-class" aria-hidden="true" data-testid="icon" />,
      );

      const svg = container.querySelector('svg');
      expect(svg).not.toBeNull();
      expect(svg?.tagName.toLowerCase()).toBe('svg');
      expect(svg?.getAttribute('class')).toContain('probe-class');
      expect(svg?.getAttribute('aria-hidden')).toBe('true');

      // ref forwarding (svgrOptions.ref: true)
      expect(ref.current).not.toBeNull();
      expect(ref.current).toBe(svg);

      unmount();
    },
  );

  it('every icon renders without throwing when given only a title (a11y usage)', () => {
    for (const { Component, name } of icons) {
      expect(() => {
        const { unmount } = render(<Component title={`${name} title`} />);
        unmount();
      }, name).not.toThrow();
    }
  });
});
