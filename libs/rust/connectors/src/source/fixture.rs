//! A recorded-provider transport for the connector tests: a handler maps
//! each request to a response, and every request is kept for assertions.

use std::sync::{Arc, Mutex};

use async_trait::async_trait;
use bytes::Bytes;
use serde_json::Value;

use crate::transport::header::{CONTENT_LENGTH, CONTENT_TYPE, RETRY_AFTER};
use crate::transport::{
    HeaderMap, HeaderName, HeaderValue, Request, Response, StatusCode, Transport, TransportError,
};

/// A canned answer.
#[derive(Clone, Debug)]
pub(crate) struct Reply {
    status: StatusCode,
    headers: HeaderMap,
    body: Bytes,
}

impl Reply {
    pub(crate) fn json(value: &Value) -> Self {
        let mut headers = HeaderMap::new();
        headers.insert(CONTENT_TYPE, HeaderValue::from_static("application/json"));
        Self {
            status: StatusCode::OK,
            headers,
            body: Bytes::from(value.to_string()),
        }
    }

    pub(crate) fn bytes(body: &[u8]) -> Self {
        Self {
            status: StatusCode::OK,
            headers: HeaderMap::new(),
            body: Bytes::copy_from_slice(body),
        }
    }

    pub(crate) fn status(status: StatusCode) -> Self {
        Self {
            status,
            headers: HeaderMap::new(),
            body: Bytes::new(),
        }
    }

    pub(crate) fn rate_limited(retry_after: Option<&'static str>) -> Self {
        let mut reply = Self::status(StatusCode::TOO_MANY_REQUESTS);
        if let Some(value) = retry_after {
            reply
                .headers
                .insert(RETRY_AFTER, HeaderValue::from_static(value));
        }
        reply
    }

    /// Declare a `Content-Length` (it need not match the body).
    pub(crate) fn declaring(mut self, length: u64) -> Self {
        self.headers
            .insert(CONTENT_LENGTH, HeaderValue::from(length));
        self
    }

    pub(crate) fn header(mut self, name: &'static str, value: &str) -> Self {
        self.headers.insert(
            HeaderName::from_static(name),
            HeaderValue::from_str(value).expect("fixture header"),
        );
        self
    }
}

type Handler = dyn Fn(&Request, usize) -> Reply + Send + Sync;

/// Answers every request with `handler(request, index)`; keeps them all.
pub(crate) struct Scripted {
    handler: Box<Handler>,
    requests: Mutex<Vec<Request>>,
}

impl Scripted {
    pub(crate) fn new(
        handler: impl Fn(&Request, usize) -> Reply + Send + Sync + 'static,
    ) -> Arc<Self> {
        Arc::new(Self {
            handler: Box::new(handler),
            requests: Mutex::new(Vec::new()),
        })
    }

    /// Every request so far, as `METHOD path?query`.
    pub(crate) fn seen(&self) -> Vec<String> {
        self.requests
            .lock()
            .expect("fixture lock")
            .iter()
            .map(|request| {
                let url = request.url();
                match url.query() {
                    Some(query) => format!("{} {}?{query}", request.method(), url.path()),
                    None => format!("{} {}", request.method(), url.path()),
                }
            })
            .collect()
    }

    pub(crate) fn requests(&self) -> Vec<Request> {
        self.requests.lock().expect("fixture lock").clone()
    }
}

#[async_trait]
impl Transport for Scripted {
    async fn execute(&self, request: Request) -> Result<Response, TransportError> {
        let index = {
            let mut requests = self.requests.lock().expect("fixture lock");
            requests.push(request.clone());
            requests.len() - 1
        };
        let reply = (self.handler)(&request, index);
        Ok(Response::from_bytes(
            reply.status,
            reply.headers,
            reply.body,
        ))
    }
}

/// The query value `name` of a request, if it has one.
pub(crate) fn query(request: &Request, name: &str) -> Option<String> {
    request
        .url()
        .query_pairs()
        .find(|(key, _)| key == name)
        .map(|(_, value)| value.into_owned())
}
