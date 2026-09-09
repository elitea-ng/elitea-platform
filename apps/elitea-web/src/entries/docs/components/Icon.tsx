/**
 * `Icon name` from the MDX contract (PREAMBLE §3): a small inline SVG set via
 * `lucide-react`, referenced by the same lowercase-kebab names Mintlify's
 * `<Card icon="...">` used, so a ported `<Card icon="message-square">` needs
 * no rewrite. Unknown names render nothing rather than throwing — a typo'd
 * icon name should not take a whole page down.
 *
 * The `as` prop is the escape hatch the other contract components use
 * internally (`Callouts.tsx`, `Card.tsx`) to render a specific lucide icon
 * they already import, without going through the string registry.
 */
import type { ComponentType, SVGProps } from 'react';
import {
  BookOpen,
  Boxes,
  GitBranch,
  Layers,
  LifeBuoy,
  type LucideIcon,
  MessageSquare,
  Puzzle,
  Rocket,
  Settings,
  Sparkles,
  Wrench,
} from 'lucide-react';

/** The names available to `<Icon name="...">` in MDX content. Extend this
 * registry as writers reach for a name it does not yet have — it is
 * deliberately small rather than importing the whole lucide set. */
const REGISTRY: Record<string, LucideIcon> = {
  book: BookOpen,
  boxes: Boxes,
  'git-branch': GitBranch,
  layers: Layers,
  'life-buoy': LifeBuoy,
  'message-square': MessageSquare,
  puzzle: Puzzle,
  rocket: Rocket,
  settings: Settings,
  sparkles: Sparkles,
  wrench: Wrench,
};

export interface IconProps {
  readonly name?: string;
  readonly as?: ComponentType<SVGProps<SVGSVGElement>>;
  readonly size?: number;
  readonly className?: string;
}

export function Icon({ name, as, size = 18, className }: IconProps) {
  const Component = as ?? (name !== undefined ? REGISTRY[name] : undefined);
  if (Component === undefined) return null;
  return <Component width={size} height={size} className={className ?? 'docs-icon'} aria-hidden="true" />;
}
