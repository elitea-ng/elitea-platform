//! Confluence toolkit family: the SDK's sixteen Confluence REST tools over
//! one claim-scoped HTTPS origin and credential (Bearer, session cookie or
//! Basic). Every operation is REST v1 except `create_page`, which uses Cloud's
//! v2 `pages` endpoint when the toolkit resolves to v2, as the SDK does.
//! Page HTML is rendered as Markdown (`markdownify` in the SDK).
//!
//! The SDK's image-description, attachment and artifact-upload tools and its
//! six index tools are not served (`tools::UNSERVED_TOOLS`).

pub(in crate::toolkits) mod client;
pub(in crate::toolkits) mod config;
pub(in crate::toolkits) mod tools;
