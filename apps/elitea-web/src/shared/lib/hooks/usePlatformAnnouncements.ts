/**
 * The two platform-wide announcements an operator can raise from admin ›
 * Configuration: the notification banner, and maintenance mode.
 *
 * ## Why they arrive together, on `platform_settings`
 *
 * They are one server read (`elitea-main`'s `announcements()`), published on the
 * endpoint this app already polls, so raising a banner costs no extra request
 * and neither does discovering a maintenance window. They are shaped as OBJECTS
 * rather than flat `banner_enabled` / `banner_message` / … keys because each one
 * is a message plus the rules for showing it, and a client should not have to
 * know how to reassemble a setting from five sibling keys.
 *
 * ## What replaced what
 *
 * The banner used to be `VITE_MAINTENANCE_BANNER` — a BUILD-TIME environment
 * variable the legacy SPA parsed in
 * `features/maintenance/lib/helpers/bannerConfig.js`. The admin form that
 * appeared to author it wrote rows nothing ever read, in this platform or in the
 * reference one. `widgets/app-shell`'s own doc comment recorded the banner as
 * deliberately not ported for exactly that reason: there was no real config
 * source, and inventing one would have been placeholder code. There is one now,
 * and it is a runtime switch, which is the point — an operator raising a banner
 * is telling users about something happening at that moment, and a control that
 * needs a redeploy is not available when it is needed.
 *
 * Maintenance mode used to be a Pylon router hook. It is now enforced by
 * elitea-main's Maintenance middleware, which answers 503 to every non-admin
 * request. What this hook reads is that same state, so the splash and the
 * refusals cannot disagree about whether the platform is open.
 *
 * ## `bypass` is the server's answer, never this app's guess
 *
 * An administrator keeps working access during a window. Deciding who that is
 * belongs to the permission model, so the server resolves it — with the same
 * permission the middleware admits on — and publishes the answer for THIS
 * caller. Recomputing it here would mean maintaining a second copy of a rule
 * whose two copies would eventually differ, and the failure would be one of two
 * bad screens: the splash shown to the administrators the platform still works
 * for, or the product shown to a user whose every request is being refused.
 *
 * ## Defaults are chosen so a loading page is a NORMAL page
 *
 * Both resolve to "nothing to announce" while the query is in flight and on a
 * deployment too old to carry the keys. The alternative for maintenance — assume
 * a window until told otherwise — would flash a full-page splash over the
 * product on every cold load.
 */
import { useGetPlatformSettings } from '@/shared/api/generated/admin/admin';

/**
 * The banner's visual register. The server folds anything else onto `info`.
 *
 * Not exported: `PlatformBanner` reaches it through the interface below, and an
 * export with no importer is what the dead-code gate is for.
 */
type BannerTone = 'info' | 'warning';

export interface PlatformBanner {
  /**
   * True only when the operator enabled the banner AND wrote a message. The
   * server resolves that pair, so an enabled-but-empty banner cannot reach a
   * client as a blank bar across the top of the product.
   */
  readonly enabled: boolean;
  /** Markdown. Rendered with raw HTML disabled. */
  readonly message: string;
  readonly dismissible: boolean;
  readonly icon: BannerTone;
  readonly style: BannerTone;
}

export interface PlatformMaintenance {
  readonly enabled: boolean;
  readonly title: string;
  readonly message: string;
  /**
   * The operator's own splash body — the port of pylon's `splash_template`.
   * HTML, sanitised before it is rendered, and EMPTY unless one was authored.
   * When it is empty the splash falls back to `message`, which always resolves
   * to something because the server fills a default for it.
   */
  readonly html: string;
  /** Whether THIS caller keeps working access. Resolved by the server. */
  readonly bypass: boolean;
}

export interface PlatformAnnouncements {
  readonly banner: PlatformBanner;
  readonly maintenance: PlatformMaintenance;
}

const NO_BANNER: PlatformBanner = {
  enabled: false,
  message: '',
  dismissible: false,
  icon: 'info',
  style: 'info',
};

const NO_MAINTENANCE: PlatformMaintenance = {
  enabled: false,
  title: '',
  message: '',
  html: '',
  bypass: false,
};

/**
 * `platform_settings` declares `additionalProperties: true`, so these two keys
 * are real, always-present response fields that the generated `PlatformSettings`
 * type does not name. Reading them through a narrow local shape — rather than
 * widening the generated model — keeps the untyped access in one function that
 * validates every field it uses.
 */
interface RawAnnouncements {
  readonly dedicated_banner?: Partial<Record<keyof PlatformBanner, unknown>>;
  readonly maintenance?: Partial<Record<keyof PlatformMaintenance, unknown>>;
}

function toneOf(raw: unknown): BannerTone {
  return raw === 'warning' ? 'warning' : 'info';
}

function textOf(raw: unknown): string {
  return typeof raw === 'string' ? raw : '';
}

export function usePlatformAnnouncements(): PlatformAnnouncements {
  const query = useGetPlatformSettings();
  // `.data.data` — the enveloped shape every generated read resolves to;
  // `eliteaFetch` throws rather than resolving with the error variant (§3.6).
  const raw = (query.data?.data ?? undefined) as RawAnnouncements | undefined;

  const bannerRaw = raw?.dedicated_banner;
  const maintenanceRaw = raw?.maintenance;

  return {
    banner:
      bannerRaw?.enabled === true
        ? {
            enabled: true,
            message: textOf(bannerRaw.message),
            dismissible: bannerRaw.dismissible === true,
            icon: toneOf(bannerRaw.icon),
            style: toneOf(bannerRaw.style),
          }
        : NO_BANNER,
    maintenance:
      maintenanceRaw?.enabled === true
        ? {
            enabled: true,
            title: textOf(maintenanceRaw.title),
            message: textOf(maintenanceRaw.message),
            html: textOf(maintenanceRaw.html),
            bypass: maintenanceRaw.bypass === true,
          }
        : NO_MAINTENANCE,
  };
}
