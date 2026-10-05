//! Native helpers.

/// A user as the native side sees it.
#[derive(serde::Serialize)]
pub struct User {
    pub id: i64,
    #[serde(rename = "full_name")]
    pub name: String,
    pub is_active: bool,
}

/// Hash a user id.
#[no_mangle]
pub extern "C" fn compute_hash(id: i64) -> i64 {
    id * 31
}
