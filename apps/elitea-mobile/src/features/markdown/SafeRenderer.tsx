import type { ReactNode } from 'react';
import { Text, type ImageStyle, type TextStyle } from 'react-native';
import { Renderer } from 'react-native-marked';

import { confirmAndOpenLink, safeLinkHost, type LinkOpener } from '@/features/markdown/links';

export interface SafeRendererOptions {
  openLink?: LinkOpener;
  allowHttp?: boolean;
  /** Style of the chip that stands in for an image. */
  chipStyle?: TextStyle;
  /** Text can be selected (the system Copy/Share menu); false when the workspace forbids sharing out. Default true. */
  selectable?: boolean;
}

/**
 * react-native-marked renderer with the app's safety rules:
 *  - remote images are NEVER fetched (an image URL in model output is an
 *    exfiltration channel): an image renders as a text chip, which opens like
 *    a link when its URL is a safe one;
 *  - links open only through `openLink` (confirm + system browser) and only
 *    for https (http in development); anything else renders as plain text;
 *  - raw HTML renders as text (the base renderer's behaviour, kept).
 */
export class SafeRenderer extends Renderer {
  private readonly openLink: LinkOpener;
  private readonly allowHttp: boolean | undefined;
  private readonly chipStyle: TextStyle | undefined;
  private readonly textSelectable: boolean;
  private keyCounter = 0;

  constructor(options: SafeRendererOptions = {}) {
    super({ selectable: options.selectable ?? true });
    this.textSelectable = options.selectable ?? true;
    this.openLink = options.openLink ?? confirmAndOpenLink;
    this.allowHttp = options.allowHttp;
    this.chipStyle = options.chipStyle;
  }

  private nextKey(prefix: string): string {
    this.keyCounter += 1;
    return `${prefix}-${this.keyCounter}`;
  }

  private onPressFor(href: string): (() => void) | undefined {
    const host = safeLinkHost(href, this.allowHttp);
    return host === null ? undefined : () => this.openLink(href, host);
  }

  override link(children: string | ReactNode[], href: string, styles?: TextStyle, title?: string): ReactNode {
    const onPress = this.onPressFor(href);
    if (!onPress) {
      return (
        <Text key={this.nextKey('text-link')} selectable={this.textSelectable} style={styles ? { ...styles, textDecorationLine: 'none' } : undefined}>
          {children}
        </Text>
      );
    }
    return (
      <Text
        key={this.nextKey('link')}
        selectable={this.textSelectable}
        accessibilityRole="link"
        accessibilityLabel={title || undefined}
        accessibilityHint="Opens in your browser after you confirm"
        onPress={onPress}
        style={styles}>
        {children}
      </Text>
    );
  }

  override image(uri: string, alt?: string, _style?: ImageStyle, title?: string): ReactNode {
    return this.chip(uri, alt || title || 'Image', 'image');
  }

  override linkImage(href: string, _imageUrl: string, alt?: string, _style?: ImageStyle, title?: string): ReactNode {
    return this.chip(href, alt || title || 'Image link', 'image-link');
  }

  private chip(href: string, label: string, kind: string): ReactNode {
    const onPress = this.onPressFor(href);
    const host = safeLinkHost(href, this.allowHttp);
    return (
      <Text
        key={this.nextKey(kind)}
        testID="markdown-image-chip"
        accessibilityRole={onPress ? 'link' : 'text'}
        onPress={onPress}
        style={this.chipStyle}>
        {`[${label}${host ? ` · ${host}` : ''}]`}
      </Text>
    );
  }
}
