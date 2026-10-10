use reqwest::Method;
use serde_json::Value;

use crate::toolkits::families::zephyr_rest::client::{
    ZephyrReply, ZephyrRestClient, ZephyrRestError, ZephyrRestErrorCode, ZephyrRestFamily,
    resource_exhausted,
};

/// Ceilings on one paginated read: `zephyr-python-api` follows `next` until
/// `isLast`, without limit.
const MAX_PAGES: usize = 200;
const MAX_ITEMS: usize = 10_000;

pub(in crate::toolkits) static FAMILY: ZephyrRestFamily = ZephyrRestFamily {
    label: "Zephyr Scale",
    code: error_code,
};

const fn error_code(code: ZephyrRestErrorCode) -> &'static str {
    match code {
        ZephyrRestErrorCode::InvalidConfiguration => "zephyr_scale.configuration.invalid",
        ZephyrRestErrorCode::InvalidInput => "zephyr_scale.request.invalid",
        ZephyrRestErrorCode::Authentication => "zephyr_scale.authentication.failed",
        ZephyrRestErrorCode::Authorization => "zephyr_scale.authorization.failed",
        ZephyrRestErrorCode::NotFound => "zephyr_scale.resource.not_found",
        ZephyrRestErrorCode::Conflict => "zephyr_scale.resource.conflict",
        ZephyrRestErrorCode::RateLimited => "zephyr_scale.rate_limited",
        ZephyrRestErrorCode::Timeout => "zephyr_scale.timeout",
        ZephyrRestErrorCode::DependencyUnavailable => "zephyr_scale.unavailable",
        ZephyrRestErrorCode::InvalidResponse => "zephyr_scale.response.invalid",
        ZephyrRestErrorCode::ResourceExhausted => "zephyr_scale.response.resource_exhausted",
        ZephyrRestErrorCode::UnknownOutcome => "zephyr_scale.effect.unknown_outcome",
    }
}

/// Query parameters in the order the SDK builds its `params` dict.
pub(in crate::toolkits) type Query = Vec<(String, String)>;

/// `zephyr-python-api`'s `CloudApiWrapper` over one bearer client.
pub(crate) struct ZephyrScaleClient {
    rest: ZephyrRestClient,
}

impl ZephyrScaleClient {
    pub(crate) const fn new(rest: ZephyrRestClient) -> Self {
        Self { rest }
    }

    /// `ZephyrSession.get/post/put`: the JSON body, or `""` for an empty one.
    pub(in crate::toolkits) async fn send(
        &self,
        method: Method,
        segments: &[&str],
        body: Option<&Value>,
    ) -> Result<ZephyrReply, ZephyrRestError> {
        self.rest.call(method, segments, &[], body).await
    }

    /// `ZephyrSession.get_paginated`: every `values` item from `params`'s
    /// page onward, following the `next` link's query until `isLast`.
    ///
    /// A page without `values` ends the walk, as in the library. Only the
    /// `next` link's query is used, so the walk can never leave this origin,
    /// and it is bounded where the library is not.
    pub(in crate::toolkits) async fn paginated(
        &self,
        segments: &[&str],
        mut params: Query,
    ) -> Result<Vec<Value>, ZephyrRestError> {
        let mut items = Vec::new();
        for _ in 0..MAX_PAGES {
            let query = params
                .iter()
                .map(|(name, value)| (name.as_str(), value.clone()))
                .collect::<Vec<_>>();
            let reply = self.rest.call(Method::GET, segments, &query, None).await?;
            let Some(page) = reply.json() else {
                return Ok(items);
            };
            let Some(values) = page.get("values") else {
                return Ok(items);
            };
            if let Some(values) = values.as_array() {
                if items.len().saturating_add(values.len()) > MAX_ITEMS {
                    return Err(resource_exhausted(false));
                }
                items.extend(values.iter().cloned());
            }
            if page.get("isLast").and_then(Value::as_bool) == Some(true) {
                return Ok(items);
            }
            let Some(next) = page
                .get("next")
                .and_then(Value::as_str)
                .and_then(|next| self.rest.next_page_query(next))
            else {
                // The library would re-request the same page forever.
                return Ok(items);
            };
            let before = params.clone();
            for (name, value) in next {
                match params.iter_mut().find(|(existing, _)| *existing == name) {
                    Some((_, existing)) => *existing = value,
                    None => params.push((name, value)),
                }
            }
            if params == before {
                return Ok(items);
            }
        }
        Err(resource_exhausted(false))
    }
}
