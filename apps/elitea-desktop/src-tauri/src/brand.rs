//! The brand this build was made for (ADR-0052 decision 4): the product name,
//! the identifier and the menu-bar image, read from the Tauri configuration
//! compiled into the app. That is `tauri.conf.json` (neutral: brand/sample's
//! values) with the brand's overlay `tauri.brand.json` merged over it at build
//! time (`scripts/tauri-brand.mjs`; `tauri build --config`, or `TAURI_CONFIG`
//! for cargo alone). Nothing in the host names a product or an identifier.
//!
//! The identifier names the config, data, log and cache folders and the
//! credentials file, so it is permanent per brand; ELITEA's is
//! `ai.elitea.desktop`.

use std::sync::OnceLock;

use tauri::image::Image;

/// What the host takes from the brand.
#[derive(Debug)]
pub struct Brand {
    product: String,
    identifier: String,
    tray_icon: Option<Image<'static>>,
}

impl Brand {
    /// The brand of `config`, with the menu-bar image the build compiled in.
    ///
    /// # Errors
    ///
    /// The configuration has no identifier or no product name: the build was
    /// made without a brand, and the app must not start (its folders would have
    /// no name).
    pub fn from_config(
        config: &tauri::Config,
        tray_icon: Option<Image<'static>>,
    ) -> Result<Self, String> {
        let identifier = config.identifier.trim();
        if identifier.is_empty() {
            return Err("this build has no app identifier (tauri.brand.json)".to_owned());
        }
        let product = config
            .product_name
            .as_deref()
            .map(str::trim)
            .filter(|name| !name.is_empty())
            .ok_or("this build has no product name (tauri.brand.json)")?;
        Ok(Self {
            product: product.to_owned(),
            identifier: identifier.to_owned(),
            tray_icon,
        })
    }

    /// The product name, as the app is called on screen ("About Elitea").
    #[must_use]
    pub fn product(&self) -> &str {
        &self.product
    }

    /// The bundle identifier: the name of every folder the app keeps data in.
    #[must_use]
    pub fn identifier(&self) -> &str {
        &self.identifier
    }

    /// The menu-bar (tray) template image.
    #[must_use]
    pub fn tray_icon(&self) -> Option<&Image<'static>> {
        self.tray_icon.as_ref()
    }
}

static BRAND: OnceLock<Brand> = OnceLock::new();

/// The app's Tauri context: the configuration and assets compiled in.
#[must_use]
pub fn context() -> tauri::Context<tauri::Wry> {
    tauri::generate_context!()
}

/// At startup: read the brand from `context` and keep its menu-bar image for
/// the host's own tray item (src/activity.rs). The image leaves the context,
/// so Tauri does not build a second tray item from the configuration.
///
/// # Errors
///
/// See [`Brand::from_config`].
pub fn init(context: &mut tauri::Context<tauri::Wry>) -> Result<&'static Brand, String> {
    let tray = context.tray_icon().cloned().map(Image::to_owned);
    context.set_tray_icon(None);
    context.config_mut().app.tray_icon = None;
    let brand = Brand::from_config(context.config(), tray)?;
    Ok(BRAND.get_or_init(|| registered(brand)))
}

/// `brand`, after telling the computer-use library that its bundle id is this
/// app's own (never read or acted on, ADR-0047 decision 3).
fn registered(brand: Brand) -> Brand {
    elitea_desktop_control::set_app_bundle_id(brand.identifier());
    brand
}

/// The brand. Set by [`init`] at startup; read from the compiled context when
/// nothing initialised it (unit tests).
///
/// # Panics
///
/// The compiled configuration has no identifier or product name, which
/// [`init`] refuses at startup first.
#[must_use]
pub fn get() -> &'static Brand {
    BRAND.get_or_init(|| {
        let context = context();
        let tray = context.tray_icon().cloned().map(Image::to_owned);
        registered(Brand::from_config(context.config(), tray).expect("the compiled brand"))
    })
}

/// The product name of the brand (shorthand for `get().product()`).
#[must_use]
pub fn product() -> &'static str {
    get().product()
}

/// The identifier of the brand (shorthand for `get().identifier()`).
#[must_use]
pub fn identifier() -> &'static str {
    get().identifier()
}

/// The processes that are this app, for the computer-use guards: this
/// process and the brand's bundle id.
#[must_use]
pub fn own_apps() -> elitea_desktop_control::OwnApps {
    let _ = get();
    elitea_desktop_control::OwnApps::default()
}

#[cfg(test)]
mod tests {
    use super::*;

    fn config(identifier: &str, product: Option<&str>) -> tauri::Config {
        let mut value: serde_json::Value =
            serde_json::from_str(include_str!("../tauri.conf.json")).expect("tauri.conf.json");
        value["identifier"] = identifier.into();
        value["productName"] = product.map_or(serde_json::Value::Null, Into::into);
        serde_json::from_value(value).expect("config")
    }

    #[test]
    fn the_brand_is_the_configurations() {
        let brand = Brand::from_config(&config("com.example.acme.desktop", Some("Acme")), None)
            .expect("brand");
        assert_eq!(brand.identifier(), "com.example.acme.desktop");
        assert_eq!(brand.product(), "Acme");
    }

    #[test]
    fn a_build_without_identifier_or_name_does_not_start() {
        assert!(Brand::from_config(&config("  ", Some("Acme")), None).is_err());
        assert!(Brand::from_config(&config("com.example.acme.desktop", None), None).is_err());
        assert!(Brand::from_config(&config("com.example.acme.desktop", Some(" ")), None).is_err());
    }

    /// The compiled brand is the one in the build's configuration, and it
    /// carries the menu-bar image the overlay named.
    #[test]
    fn the_compiled_brand_is_the_builds() {
        let context = context();
        assert_eq!(identifier(), context.config().identifier);
        assert_eq!(Some(product()), context.config().product_name.as_deref());
        assert!(get().tray_icon().is_some(), "the build names a tray image");
        assert_eq!(elitea_desktop_control::app_bundle_id(), Some(identifier()));
        assert!(own_apps().is_own_bundle(identifier()));
    }
}
