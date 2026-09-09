/**
 * `Card` / `CardGroup cols` from the MDX contract. A `Card` with an `href` is
 * a link (internal slug or external URL, same rule as prose links); one with
 * no `href` is a plain content tile.
 */
import type { CSSProperties, ReactNode } from 'react';

import { Icon } from './Icon';

export interface CardProps {
  readonly title: string;
  readonly icon?: string;
  readonly href?: string;
  readonly children?: ReactNode;
}

export function Card({ title, icon, href, children }: CardProps) {
  const body = (
    <>
      {icon !== undefined ? <Icon name={icon} className="docs-card__icon" /> : null}
      <p className="docs-card__title">{title}</p>
      {children !== undefined ? <div className="docs-card__body">{children}</div> : null}
    </>
  );
  if (href !== undefined) {
    return (
      <a className="docs-card docs-card--link" href={href}>
        {body}
      </a>
    );
  }
  return <div className="docs-card">{body}</div>;
}

export interface CardGroupProps {
  readonly cols?: number;
  readonly children?: ReactNode;
}

export function CardGroup({ cols = 2, children }: CardGroupProps) {
  return (
    <div className="docs-card-group" style={{ '--docs-card-cols': cols } as CSSProperties}>
      {children}
    </div>
  );
}
