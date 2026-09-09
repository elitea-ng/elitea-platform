/** `Badge` from the MDX contract: a small inline pill for a status word
 * ("Beta", "New", "Deprecated", …). */
import type { ReactNode } from 'react';

export interface BadgeProps {
  readonly children?: ReactNode;
}

export function Badge({ children }: BadgeProps) {
  return <span className="docs-badge">{children}</span>;
}
