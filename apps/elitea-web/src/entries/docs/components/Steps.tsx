/**
 * `Steps` / `Step title` from the MDX contract. `Step` renders its own
 * number via a CSS counter on `Steps` (`docs-steps`/`docs-step` in
 * styles.css) rather than each `Step` knowing its own index, so steps stay
 * numbered correctly if a page reorders or removes one.
 */
import type { ReactNode } from 'react';

export interface StepProps {
  readonly title: string;
  readonly children?: ReactNode;
}

export function Step({ title, children }: StepProps) {
  return (
    <li className="docs-step">
      <p className="docs-step__title">{title}</p>
      <div className="docs-step__body">{children}</div>
    </li>
  );
}

export interface StepsProps {
  readonly children?: ReactNode;
}

export function Steps({ children }: StepsProps) {
  return <ol className="docs-steps">{children}</ol>;
}
