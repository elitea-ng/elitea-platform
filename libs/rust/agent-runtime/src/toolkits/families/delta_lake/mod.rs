//! The `delta_lake` toolkit family (SDK `tools/aws/delta_lake`; #2262, owner
//! decision D6 of epic #2254).
//!
//! The SDK opens the table with the Python `deltalake` package, loads all of
//! it into a pandas `DataFrame` (`to_pandas()`) and evaluates `query` with
//! `DataFrame.query`. This family keeps the SDK's three tool names and
//! argument schemas and reads the same table through delta-kernel-rs and
//! Arrow, with three deliberate differences:
//!
//! * `query` is not evaluated. A pandas expression is an expression language
//!   the owner declined to port; `filters` is the structured replacement
//!   (equality as in the SDK, plus `eq`, `ne`, `in`, `gt`, `gte`, `lt`, `lte`
//!   and `is_null` per column).
//! * Nothing loads the whole table. A scan is bounded by rows, decoded bytes
//!   and a deadline, and a result by rows, bytes and cell size
//!   (`table::limits`).
//! * The endpoint is derived from the credential's region and the bucket
//!   (`config::s3_endpoint`); the credential names no host.
//!
//! Compiled only with the `toolkit-delta-lake` feature.

pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod filter;
pub(in crate::toolkits) mod render;
pub(in crate::toolkits) mod table;
pub(in crate::toolkits) mod tools;
