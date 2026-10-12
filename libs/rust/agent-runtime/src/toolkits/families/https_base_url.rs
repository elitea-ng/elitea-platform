//! The base-URL rule lives in `elitea-connectors` (ADR-0030 decision 3), so
//! the toolkit families and the indexing connectors apply one rule. The
//! families keep reaching it through this path.

pub(in crate::toolkits) use elitea_connectors::base_url::{
    BasePath, BaseUrlError, host_matches_domain, parse,
};
