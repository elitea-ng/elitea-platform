/**
 * `Accordion title` / `AccordionGroup` from the MDX contract. Built on the
 * native `<details>`/`<summary>` pair — free keyboard support and no state
 * management needed, `AccordionGroup` is a plain wrapper for spacing.
 */
import type { ReactNode } from 'react';

export interface AccordionProps {
  readonly title: string;
  readonly children?: ReactNode;
}

export function Accordion({ title, children }: AccordionProps) {
  return (
    <details className="docs-accordion">
      <summary className="docs-accordion__title">{title}</summary>
      <div className="docs-accordion__body">{children}</div>
    </details>
  );
}

export interface AccordionGroupProps {
  readonly children?: ReactNode;
}

export function AccordionGroup({ children }: AccordionGroupProps) {
  return <div className="docs-accordion-group">{children}</div>;
}
