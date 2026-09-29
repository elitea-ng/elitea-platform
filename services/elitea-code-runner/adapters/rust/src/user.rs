// Build-time placeholder. Each admitted job replaces only this module.
pub fn run(state: serde_json::Value) -> Result<serde_json::Value, Box<dyn std::error::Error>> {
    Ok(state)
}
