/**
 * `Tabs` / `Tab title` from the MDX contract. `Tabs` reads the `title` prop
 * straight off its `Tab` children (the same trick Mintlify's own component
 * uses) rather than requiring a separate list of tab names — a page author
 * adds a `<Tab title="...">` and the tab strip picks it up with no second
 * place to edit.
 */
import { Children, isValidElement, useState } from 'react';
import type { ReactElement, ReactNode } from 'react';

export interface TabProps {
  readonly title: string;
  readonly children?: ReactNode;
}

/** Renders its children directly — `Tabs` reads `title` off the element and
 * decides visibility; `Tab` itself has no active/inactive state of its own. */
export function Tab({ children }: TabProps) {
  return <>{children}</>;
}

export interface TabsProps {
  readonly children?: ReactNode;
}

export function Tabs({ children }: TabsProps) {
  const tabs = Children.toArray(children).filter(
    (child): child is ReactElement<TabProps> => isValidElement(child) && typeof (child.props as TabProps).title === 'string',
  );
  const [active, setActive] = useState(0);
  const current = tabs[active] ?? tabs[0];

  if (tabs.length === 0) return null;

  return (
    <div className="docs-tabs">
      <div className="docs-tabs__list" role="tablist">
        {tabs.map((tab, index) => (
          <button
            key={tab.props.title}
            type="button"
            role="tab"
            aria-selected={index === active}
            className={`docs-tabs__tab${index === active ? ' docs-tabs__tab--active' : ''}`}
            onClick={() => setActive(index)}
          >
            {tab.props.title}
          </button>
        ))}
      </div>
      <div className="docs-tabs__panel" role="tabpanel">
        {current}
      </div>
    </div>
  );
}
