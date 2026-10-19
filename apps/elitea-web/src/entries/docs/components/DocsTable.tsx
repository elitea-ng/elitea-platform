/**
 * The `table` override in the MDX components map (`./index.ts`). A Markdown
 * table with long cells (inline code, URLs) is wider than the content column,
 * and a bare `<table>` then pushes past it and widens the whole page. The
 * wrapper scrolls the table on its own instead, the same way `pre` scrolls a
 * long code line.
 */
import type { TableHTMLAttributes } from 'react';

export type DocsTableProps = TableHTMLAttributes<HTMLTableElement>;

export function DocsTable(props: DocsTableProps) {
  return (
    <div className="docs-table-wrap">
      <table {...props} />
    </div>
  );
}
