/** `Frame caption` from the MDX contract: a bordered wrapper for an image or
 * a `Mermaid` diagram, with an optional caption underneath. */
import type { ReactNode } from 'react';

export interface FrameProps {
  readonly caption?: string;
  readonly children?: ReactNode;
}

export function Frame({ caption, children }: FrameProps) {
  return (
    <figure className="docs-frame">
      <div className="docs-frame__content">{children}</div>
      {caption !== undefined ? <figcaption className="docs-frame__caption">{caption}</figcaption> : null}
    </figure>
  );
}
