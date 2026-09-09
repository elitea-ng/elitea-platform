/**
 * `Screenshot id alt` from the MDX contract: renders `content/img/<id>.webp`.
 * The id ↔ manifest correspondence (`shots.manifest.ts`) is enforced by
 * `docs-content.test.ts`, not here — this component only has to survive an
 * id with no image committed yet (every id today, since no screenshots have
 * been captured — PREAMBLE decision 4 says this unit ships the manifest with
 * "no images yet"), so it renders a labelled placeholder instead of a broken
 * `<img>` when the glob below has no match.
 *
 * `import.meta.glob` with `eager: true` bundles every committed
 * `content/img/*.webp` as a build-time URL lookup — the same mechanism the
 * main app's asset imports rely on, just applied to a whole directory instead
 * of one named file, so a page can reference an id without a hand-written
 * `import` statement per screenshot.
 */
import { t } from '@/shared/i18n';

const images = import.meta.glob('../content/img/*.webp', {
  eager: true,
  query: '?url',
  import: 'default',
});

function urlFor(id: string): string | undefined {
  return images[`../content/img/${id}.webp`];
}

export interface ScreenshotProps {
  readonly id: string;
  readonly alt: string;
}

export function Screenshot({ id, alt }: ScreenshotProps) {
  const src = urlFor(id);
  if (src === undefined) {
    // Plain visible text, not `role="img"` on a `<div>`: there is no image
    // to substitute alt text FOR yet, so hiding this text behind an image
    // role (jsx-a11y's `prefer-tag-over-role`) would make the placeholder
    // less accessible, not more.
    return (
      <div className="docs-screenshot docs-screenshot--missing">
        <span>{alt}</span>
        <span className="docs-screenshot__pending">{t('entries.docs.screenshot.pending', 'screenshot pending ({{id}})', { id })}</span>
      </div>
    );
  }
  return <img className="docs-screenshot" src={src} alt={alt} loading="lazy" />;
}
