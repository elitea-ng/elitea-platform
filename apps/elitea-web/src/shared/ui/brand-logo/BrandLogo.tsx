import { type CSSProperties, useMemo } from 'react';

import { useTheme } from '@mui/material/styles';

import { resolveBrandAsset, resolveBrandPack } from '@/shared/brand';
import { LogoIcon } from '@/shared/ui/icons/logo-icon';
import { LogoMarkIcon } from '@/shared/ui/icons/logo-mark-icon';

export interface BrandLogoProps {
  /** Sizing only — the call sites pass `width`/`height`; both renderings honour it. */
  readonly style?: CSSProperties;
  readonly className?: string;
}

/**
 * The brand logo, pack-driven (ADR-0024 WP3).
 *
 * Renders `<img src={pack.assets.logoMark|logoFull}>` when the SERVED pack
 * states a custom asset (`shared/brand/assets.ts` — served AND different from
 * the compiled default), and the compiled-in SVG component otherwise. The
 * compiled components ARE the artwork the default pack's `./brand/*.svg`
 * placeholders stand in for, so a deployment with no served pack renders
 * byte-identically to before this module existed (JRNY-030's "the default
 * pack reproduces the baseline appearance").
 *
 * `alt` is the pack's `product.name`: the image is the product's own logo, so
 * its accessible name is the product. Where the logo sits inside a button
 * that already has an `aria-label` (the sidebar toggle), that label wins for
 * the control and the alt still names the image itself.
 *
 * Resolution is memoised once per mount: the pack is fixed for the document
 * (the global is written by a blocking script before the bundle runs), and
 * `resolveBrandPack()` runs a zod parse.
 */
function useBrandLogo(key: 'logoMark' | 'logoFull') {
  return useMemo(() => {
    const pack = resolveBrandPack();
    return { asset: resolveBrandAsset(key, pack), alt: pack.product.name };
  }, [key]);
}

/** The square MARK (LE-03). */
export function BrandLogoMark({ style, className }: BrandLogoProps) {
  const { asset, alt } = useBrandLogo('logoMark');
  if (asset.custom && asset.url !== undefined) {
    return (
      <img
        src={asset.url}
        alt={alt}
        className={className}
        style={{ objectFit: 'contain', ...style }}
        data-testid="brand-logo-mark"
      />
    );
  }
  return <LogoMarkIcon style={style} className={className} data-testid="brand-logo-mark" />;
}

/** The full LOCKUP (LE-03 mark + "Elitea AI" wordmark). */
export function BrandLogoFull({ style, className }: BrandLogoProps) {
  const theme = useTheme();
  const { asset, alt } = useBrandLogo('logoFull');
  if (asset.custom && asset.url !== undefined) {
    return (
      <img
        src={asset.url}
        alt={alt}
        className={className}
        style={{ objectFit: 'contain', ...style }}
        data-testid="brand-logo-full"
      />
    );
  }
  // The compiled lockup paints its wordmark in `currentColor` (the LE-03
  // mark keeps its own four colours), so the text takes the scheme's strong
  // text colour here. A custom `<img>` above cannot inherit a colour: a pack
  // that ships its own lockup ships it in final colours.
  return (
    <LogoIcon
      style={{ color: theme.vars?.palette.text.secondary, ...style }}
      className={className}
      data-testid="brand-logo-full"
    />
  );
}
