use serde::Serialize;

pub const CAPABILITY_REPORT_SCHEMA_REVISION: &str = "elitea.rust-worker.capability-foundation.v1";

#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct CapabilityReport {
    pub schema_revision: &'static str,
    pub implementation_name: &'static str,
    pub language: &'static str,
    pub runtime_version: &'static str,
    pub startup_mode: &'static str,
    pub production_registration_enabled: bool,
    pub production_capabilities: Vec<Capability>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct Capability {
    pub capability_id: String,
    pub capability_version: String,
}

#[must_use]
pub fn report() -> CapabilityReport {
    CapabilityReport {
        schema_revision: CAPABILITY_REPORT_SCHEMA_REVISION,
        implementation_name: "elitea-worker-rust",
        language: "rust",
        runtime_version: env!("CARGO_PKG_VERSION"),
        startup_mode: "offline_conformance",
        production_registration_enabled: false,
        production_capabilities: Vec::new(),
    }
}

#[cfg(test)]
mod tests {
    use super::{CAPABILITY_REPORT_SCHEMA_REVISION, report};

    #[test]
    fn reconstruction_starts_fail_closed() {
        let report = report();

        assert_eq!(report.schema_revision, CAPABILITY_REPORT_SCHEMA_REVISION);
        assert!(!report.production_registration_enabled);
        assert_eq!(report.production_capabilities, []);
    }
}
