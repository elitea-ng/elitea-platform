//! The invoking project's artifact store as the artifact index source reads
//! it: `elitea.runtime.v1.ArtifactObjectService` on elitea-main's platform
//! listener, over the channel the model calls ride (#1359 U5). It replaced
//! the reqwest client to `/api/v2/artifacts/objects/...` on that listener.
//!
//! The calls carry the run's model-gateway credential and project, so
//! elitea-main applies the project and the per-bucket access list of the
//! person the run acts for, as the HTTP route did. A refusal keeps the
//! route's HTTP status ([`Failure::Status`]); the hop failing is
//! [`Failure::Transport`].

use async_trait::async_trait;
use elitea_connectors::artifact::source::{
    ArtifactObjects, FetchedObject, ObjectEntry, ObjectPage,
};
use elitea_connectors::source::Failure;
use elitea_connectors::transport::StatusCode;
use elitea_platform_rpc::artifacts::{ArtifactClient, ArtifactError, Caller, ListObjects};

/// One run's view of its project's store: the client and the caller.
pub(crate) struct ProjectObjects {
    client: ArtifactClient,
    caller: Caller,
}

impl ProjectObjects {
    /// `bearer` acting in `project_id`, or `None` when either is unusable.
    pub(crate) fn new(client: ArtifactClient, bearer: &str, project_id: &str) -> Option<Self> {
        Some(Self {
            client,
            caller: Caller::parse(bearer, project_id)?,
        })
    }
}

/// An artifact call failure as the connectors classify it.
pub(crate) fn source_failure(error: &ArtifactError) -> Failure {
    match error {
        ArtifactError::Refused(refusal) => StatusCode::from_u16(refusal.http_status).map_or(
            Failure::InvalidResponse("a refusal without a status"),
            Failure::Status,
        ),
        ArtifactError::Hop(_) => {
            Failure::Transport("the platform's artifact service could not be reached".to_owned())
        }
        ArtifactError::TooLarge(cap) => Failure::TooLarge(*cap),
        ArtifactError::Protocol(reason) => Failure::InvalidResponse(reason),
        ArtifactError::Invalid(reason) => Failure::Client((*reason).to_owned()),
    }
}

#[async_trait]
impl ArtifactObjects for ProjectObjects {
    async fn list_page(
        &self,
        bucket: &str,
        prefix: &str,
        cursor: &str,
    ) -> Result<ObjectPage, Failure> {
        let query = ListObjects {
            bucket: bucket.to_owned(),
            prefix: prefix.to_owned(),
            cursor: cursor.to_owned(),
            ..ListObjects::default()
        };
        let mut pages = self
            .client
            .list_objects(&self.caller, query)
            .await
            .map_err(|error| source_failure(&error))?;
        let page = pages
            .next()
            .await
            .map_err(|error| source_failure(&error))?
            .unwrap_or_default();
        Ok(ObjectPage {
            objects: page
                .objects
                .into_iter()
                .map(|info| ObjectEntry {
                    key: info.key,
                    size_bytes: u64::try_from(info.size_bytes).unwrap_or(0),
                    etag: info.etag,
                    modified_at: info.modified_at,
                })
                .collect(),
            next_cursor: page.next_cursor,
        })
    }

    async fn fetch(
        &self,
        bucket: &str,
        key: &str,
        max_bytes: u64,
    ) -> Result<FetchedObject, Failure> {
        let reader = self
            .client
            .get_object(&self.caller, bucket, key)
            .await
            .map_err(|error| source_failure(&error))?;
        let (etag, modified_at) = (reader.info.etag.clone(), reader.info.modified_at.clone());
        let bytes = reader
            .read_to_end(max_bytes)
            .await
            .map_err(|error| source_failure(&error))?;
        Ok(FetchedObject {
            etag,
            modified_at,
            bytes,
        })
    }
}

#[cfg(test)]
mod tests {
    use elitea_platform_rpc::testing::{FakeArtifactStore, serve};

    use super::*;

    #[tokio::test]
    async fn pages_and_objects_come_from_the_service_under_the_runs_credential() {
        let store = FakeArtifactStore::new()
            .grant("pat", 7)
            .page_size(1)
            .with_object(7, "docs", "a.md", b"alpha")
            .with_object(7, "docs", "b.md", b"beta");
        let objects = ProjectObjects::new(
            ArtifactClient::new(serve(Some(store.clone()), None)),
            "pat",
            "7",
        )
        .expect("caller");
        let first = objects.list_page("docs", "", "").await.expect("page");
        assert_eq!(first.objects.len(), 1);
        assert_eq!(first.next_cursor, "a.md");
        assert!(!first.objects[0].etag.is_empty());
        let second = objects
            .list_page("docs", "", &first.next_cursor)
            .await
            .expect("page");
        assert_eq!(second.objects[0].key, "b.md");
        assert!(second.next_cursor.is_empty());
        let fetched = objects.fetch("docs", "b.md", 1024).await.expect("fetched");
        assert_eq!(fetched.bytes, b"beta");
        assert_eq!(fetched.etag, second.objects[0].etag);
        assert!(matches!(
            objects.fetch("docs", "b.md", 2).await,
            Err(Failure::TooLarge(2))
        ));
        assert!(
            store
                .calls()
                .iter()
                .all(|call| call.bearer.as_deref() == Some("pat") && call.project_id == 7)
        );
    }

    #[tokio::test]
    async fn refusals_keep_their_status_and_the_hop_is_a_transport_failure() {
        let store = FakeArtifactStore::new().grant("pat", 7);
        let client = ArtifactClient::new(serve(Some(store), None));
        let objects = ProjectObjects::new(client.clone(), "pat", "7").expect("caller");
        assert!(matches!(
            objects.list_page("nope", "", "").await,
            Err(Failure::Status(status)) if status.as_u16() == 404
        ));
        let other = ProjectObjects::new(client, "pat", "8").expect("caller");
        assert!(matches!(
            other.fetch("docs", "a.md", 10).await,
            Err(Failure::Status(status)) if status.as_u16() == 403
        ));
        let down = ProjectObjects::new(
            ArtifactClient::new(
                tonic::transport::Endpoint::from_static("http://127.0.0.1:9").connect_lazy(),
            ),
            "pat",
            "7",
        )
        .expect("caller");
        assert!(matches!(
            down.list_page("docs", "", "").await,
            Err(Failure::Transport(_))
        ));
        assert!(ProjectObjects::new(ArtifactClient::new(serve(None, None)), "", "7").is_none());
        assert!(ProjectObjects::new(ArtifactClient::new(serve(None, None)), "pat", "x").is_none());
    }
}
