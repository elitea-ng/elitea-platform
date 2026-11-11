//! The Application User Model ID (AUMID) Windows shows a toast under
//! (#1828). Windows shows a toast only for an AUMID that a Start Menu
//! shortcut or a package names:
//!
//! * MSI: the installer's Start Menu shortcut sets `System.AppUserModel.ID`
//!   to the brand's desktop identifier (Tauri's WiX template, `bundle_id`),
//!   and the notification plugin shows toasts under the same identifier.
//! * MSIX: the package names the app `<PackageFamilyName>!App` (the
//!   `Application Id` in `scripts/windows-package.mjs`); there is no
//!   shortcut of our own, so the host shows its toasts under that id.
//!
//! macOS and Linux have no AUMID: the plugin's own behaviour stands.
#![cfg_attr(not(windows), allow(dead_code))]

/// The `Application Id` of the MSIX manifest (`scripts/windows-package.mjs`).
pub const MSIX_APPLICATION_ID: &str = "App";

/// The AUMID of a packaged app from its package family name.
#[must_use]
pub fn packaged_app_id(family_name: &str) -> Option<String> {
    let family = family_name.trim();
    (!family.is_empty()).then(|| format!("{family}!{MSIX_APPLICATION_ID}"))
}

/// The AUMID when the app runs with package identity (installed from the
/// MSIX); `None` for the MSI and for a build run from a folder. Read once.
#[cfg(windows)]
pub fn packaged() -> Option<&'static str> {
    static ID: std::sync::OnceLock<Option<String>> = std::sync::OnceLock::new();
    ID.get_or_init(|| {
        // Fails (APPMODEL_ERROR_NO_PACKAGE) without package identity.
        let package = windows::ApplicationModel::Package::Current().ok()?;
        let family = package.Id().ok()?.FamilyName().ok()?;
        packaged_app_id(&family.to_string_lossy())
    })
    .as_deref()
}

#[cfg(test)]
mod tests {
    use super::packaged_app_id;

    #[test]
    fn a_package_family_names_the_app() {
        assert_eq!(
            packaged_app_id("ai.elitea.desktop_8wekyb3d8bbwe").as_deref(),
            Some("ai.elitea.desktop_8wekyb3d8bbwe!App")
        );
        assert_eq!(packaged_app_id("  "), None);
    }

    /// Run from a folder or the MSI, a test process has no package identity.
    #[cfg(windows)]
    #[test]
    fn a_process_without_package_identity_has_no_packaged_id() {
        assert_eq!(super::packaged(), None);
    }
}
