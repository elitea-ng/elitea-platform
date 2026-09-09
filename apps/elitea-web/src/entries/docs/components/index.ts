/**
 * The MDX component map (PREAMBLE §3 contract, in full). `App.tsx` passes
 * `docsComponents` as the `components` prop to every compiled `.mdx` page —
 * MDX v3 resolves a JSX tag like `<Tip>` against this object's `Tip` key
 * (see `mdx.d.ts`'s header comment for why there is no `@mdx-js/react`
 * provider in the middle).
 *
 * `docs-content.test.ts` imports this map directly to check that every
 * capitalised JSX tag used anywhere under `content/**` has an entry here —
 * the one point where a content author reaching for an undeclared component
 * name is caught before it ships as a runtime `ReferenceError`.
 */
import type { ComponentType } from 'react';

import { AccordionGroup, Accordion } from './Accordion';
import { Badge } from './Badge';
import { CardGroup, Card } from './Card';
import { Check, Info, Note, Tip, Warning } from './Callouts';
import { DocsImage } from './DocsImage';
import { DocsLink } from './DocsLink';
import { Frame } from './Frame';
import { Icon } from './Icon';
import { Mermaid } from './Mermaid';
import { Screenshot } from './Screenshot';
import { Step, Steps } from './Steps';
import { Tab, Tabs } from './Tabs';

// No re-exports of the individual components here (there were, once — knip's
// dead-code gate, #528, flagged all eighteen as unused: every page renders
// through the `docsComponents` map below, MDX-style, never by importing e.g.
// `Card` from this barrel directly). Import a single component straight from
// its own file (`./Card`, `./Steps`, …) if a future non-MDX consumer needs
// one on its own.

/**
 * @public exported for `App.tsx` (renders every page with it) and
 * `docs-content.test.ts` (structural check of what content is allowed to
 * reference).
 */
// eslint-disable-next-line @typescript-eslint/no-explicit-any -- each
// component below has its own, narrower props type; the map exists to erase
// that so `App.tsx` can pass it as one `components` prop to an arbitrary MDX
// page. `unknown` is not viable here: function component prop types are
// checked contravariantly, so a map of `ComponentType<unknown>` rejects every
// real component (`CardProps`, `StepProps`, …) that requires specific props.
export const docsComponents: Record<string, ComponentType<any>> = {
  Note,
  Tip,
  Info,
  Warning,
  Check,
  Card,
  CardGroup,
  Accordion,
  AccordionGroup,
  Tabs,
  Tab,
  Steps,
  Step,
  Badge,
  Frame,
  Screenshot,
  Icon,
  Mermaid,
  // Lowercase keys: MDX resolves a compiled Markdown link/image (`a`/`img`,
  // not authored as JSX) against these the same way it resolves `<Card>`
  // against `Card` above — see `DocsLink.tsx`/`DocsImage.tsx` for why they
  // exist (root-relative content hrefs/srcs need the docs base prefixed).
  a: DocsLink,
  img: DocsImage,
};
