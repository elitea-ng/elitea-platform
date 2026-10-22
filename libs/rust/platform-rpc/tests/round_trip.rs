//! Round trips against the `testing` servers over a real loopback listener.

use std::time::Duration;

use bytes::Bytes;
use elitea_platform_rpc::Bearer;
use elitea_platform_rpc::artifacts::{
    ArtifactClient, ArtifactError, Caller, ListObjects, PutObject, RuntimeClaim,
};
use elitea_platform_rpc::internal_tools::{
    CallFrame, InternalToolClient, InvokeToolkit, Outcome, Scope, ToolCallError,
};
use elitea_platform_rpc::testing::{FakeArtifactStore, FakeInternalTools, serve};

fn caller(token: &str, project_id: i64) -> Caller {
    Caller::new(Bearer::new(token).expect("bearer"), project_id)
}

fn body(len: usize) -> Vec<u8> {
    (0..len)
        .map(|index| u8::try_from(index % 251).unwrap_or(0))
        .collect()
}

#[tokio::test]
async fn a_multi_chunk_object_round_trips_byte_identical() {
    let store = FakeArtifactStore::new()
        .grant("tok", 7)
        .with_bucket(7, "files");
    let client = ArtifactClient::new(serve(Some(store.clone()), None));
    let caller = caller("tok", 7).with_claim(RuntimeClaim {
        execution_id: "exec-1".to_owned(),
        generation: 3,
        claim_id: "claim-1".to_owned(),
        fence: "f".to_owned(),
    });
    // Over two 1 MiB put chunks, and read back in many 64 KiB chunks.
    let original = body(2 * 1024 * 1024 + 17);
    let written = client
        .put_object(
            &caller,
            PutObject {
                bucket: "files".to_owned(),
                key: "dir/big.bin".to_owned(),
                media_type: "application/x-test".to_owned(),
                overwrite: false,
            },
            Bytes::from(original.clone()),
        )
        .await
        .expect("put");
    assert_eq!(
        written.size_bytes,
        i64::try_from(original.len()).expect("len")
    );
    assert_eq!(
        store
            .object(7, "files", "dir/big.bin")
            .expect("stored")
            .bytes,
        original
    );

    let store = store.chunk_bytes(64 * 1024);
    let reader = client
        .get_object(&caller, "files", "dir/big.bin")
        .await
        .expect("get");
    assert_eq!(reader.info.media_type, "application/x-test");
    assert!(!reader.info.etag.is_empty());
    assert_eq!(reader.read_to_end(u64::MAX).await.expect("read"), original);

    let reader = client
        .get_object(&caller, "files", "dir/big.bin")
        .await
        .expect("get");
    assert!(matches!(
        reader.read_to_end(1024).await,
        Err(ArtifactError::TooLarge(_))
    ));
    assert!(
        store
            .claims()
            .iter()
            .all(|claim| claim.claim_id == "claim-1" && claim.generation == 3)
    );
}

#[tokio::test]
async fn refusals_carry_the_routes_http_status() {
    let store = FakeArtifactStore::new()
        .grant("tok", 7)
        .with_object(7, "files", "a.txt", b"a")
        .refuse("secret.txt", 415, "UnsupportedMediaType", "nope");
    let client = ArtifactClient::new(serve(Some(store), None));

    let missing = client
        .stat(&caller("tok", 7), "files", "nope.txt")
        .await
        .unwrap_err();
    assert_eq!(missing.http_status(), Some(404));
    let other_project = client
        .stat(&caller("tok", 8), "files", "a.txt")
        .await
        .unwrap_err();
    assert_eq!(other_project.http_status(), Some(403));
    let bad_bearer = client
        .stat(&caller("other", 7), "files", "a.txt")
        .await
        .unwrap_err();
    assert_eq!(bad_bearer.http_status(), Some(401));
    let refused = client
        .stat(&caller("tok", 7), "files", "secret.txt")
        .await
        .unwrap_err();
    match refused {
        ArtifactError::Refused(refusal) => {
            assert_eq!(refusal.http_status, 415);
            assert_eq!(refusal.code, "UnsupportedMediaType");
            assert_eq!(refusal.message, "nope");
        }
        other => panic!("expected a refusal, got {other:?}"),
    }
    let taken = client
        .put_object(
            &caller("tok", 7),
            PutObject {
                bucket: "files".to_owned(),
                key: "a.txt".to_owned(),
                ..PutObject::default()
            },
            Bytes::from_static(b"b"),
        )
        .await
        .unwrap_err();
    assert_eq!(taken.http_status(), Some(409));
}

#[tokio::test]
async fn a_listing_streams_its_pages_and_resumes_at_a_cursor() {
    let mut store = FakeArtifactStore::new().grant("tok", 7).page_size(2);
    for key in ["a", "b", "c", "d", "e"] {
        store = store.with_object(7, "files", key, key.as_bytes());
    }
    let client = ArtifactClient::new(serve(Some(store), None));
    let mut pages = client
        .list_objects(
            &caller("tok", 7),
            ListObjects {
                bucket: "files".to_owned(),
                max_pages: 2,
                ..ListObjects::default()
            },
        )
        .await
        .expect("list");
    let first = pages.next().await.expect("page").expect("first");
    let second = pages.next().await.expect("page").expect("second");
    assert!(pages.next().await.expect("end").is_none());
    assert_eq!(first.objects.len(), 2);
    assert_eq!(second.next_cursor, "d");
    let mut rest = client
        .list_objects(
            &caller("tok", 7),
            ListObjects {
                bucket: "files".to_owned(),
                cursor: second.next_cursor,
                ..ListObjects::default()
            },
        )
        .await
        .expect("list");
    let last = rest.next().await.expect("page").expect("last");
    assert_eq!(
        last.objects
            .iter()
            .map(|o| o.key.as_str())
            .collect::<Vec<_>>(),
        ["e"]
    );
    assert!(last.next_cursor.is_empty());

    let missing = client
        .list_objects(
            &caller("tok", 7),
            ListObjects {
                bucket: "nope".to_owned(),
                ..ListObjects::default()
            },
        )
        .await;
    assert_eq!(
        missing.err().and_then(|error| error.http_status()),
        Some(404)
    );
}

#[tokio::test]
async fn buckets_and_deletes_round_trip() {
    let store = FakeArtifactStore::new()
        .grant("tok", 7)
        .with_object(7, "files", "a", b"a")
        .with_object(7, "files", "b", b"b");
    let client = ArtifactClient::new(serve(Some(store.clone()), None));
    let me = caller("tok", 7);
    let created = client
        .create_bucket(&me, "new", Some(30))
        .await
        .expect("create");
    assert_eq!(created.retention_days, Some(30));
    assert_eq!(
        client
            .create_bucket(&me, "new", None)
            .await
            .unwrap_err()
            .http_status(),
        Some(409)
    );
    let names = client
        .list_buckets(&me)
        .await
        .expect("buckets")
        .into_iter()
        .map(|bucket| bucket.name)
        .collect::<Vec<_>>();
    assert_eq!(names, ["files", "new"]);
    client.delete(&me, "files", "a").await.expect("delete");
    let batch = client
        .delete_batch(&me, "files", vec!["a".to_owned(), "b".to_owned()])
        .await
        .expect("batch");
    assert_eq!(batch.deleted, ["b"]);
    assert_eq!(batch.failed[0].key, "a");
    assert!(store.object(7, "files", "b").is_none());
}

#[tokio::test]
async fn a_tool_call_streams_progress_then_its_result() {
    let tools = FakeInternalTools::new("actor")
        .with_tools(r#"{"tools":[{"name":"run"}]}"#)
        .with_call_result(
            &["admitted", "running"],
            r#"{"content":[],"isError":false}"#,
        );
    let client = InternalToolClient::new(serve(None, Some(tools.clone())));
    let bearer = Bearer::new("actor").expect("bearer");
    let scope = Scope {
        project_id: 7,
        scope: "applications".to_owned(),
    };
    assert_eq!(
        client.list_tools(&bearer, &scope).await.expect("list"),
        Outcome::Result(Bytes::from_static(br#"{"tools":[{"name":"run"}]}"#))
    );
    let mut call = client
        .call_tool(&bearer, &scope, "run", Bytes::from_static(b"{\"q\":1}"))
        .await
        .expect("call");
    assert_eq!(
        call.next().await.expect("frame"),
        Some(CallFrame::Progress("admitted".to_owned()))
    );
    assert_eq!(
        call.next().await.expect("frame"),
        Some(CallFrame::Progress("running".to_owned()))
    );
    assert!(matches!(
        call.next().await.expect("frame"),
        Some(CallFrame::Done(Outcome::Result(_)))
    ));
    assert_eq!(call.next().await.expect("end"), None);
    let recorded = tools.calls();
    assert_eq!(recorded[0].project_id, 7);
    assert_eq!(recorded[0].scope, "applications");
    assert_eq!(recorded[0].arguments, Bytes::from_static(b"{\"q\":1}"));
}

#[tokio::test]
async fn a_refusal_decodes_its_status_and_body() {
    let tools = FakeInternalTools::new("actor").refusing(403, r#"{"error":"MCP disabled"}"#);
    let client = InternalToolClient::new(serve(None, Some(tools)));
    let bearer = Bearer::new("actor").expect("bearer");
    let scope = Scope {
        project_id: 7,
        scope: String::new(),
    };
    match client.list_tools(&bearer, &scope).await {
        Err(ToolCallError::Refused(refusal)) => {
            assert_eq!(refusal.http_status, 403);
            assert_eq!(
                refusal.body,
                Bytes::from_static(br#"{"error":"MCP disabled"}"#)
            );
        }
        other => panic!("expected a refusal, got {other:?}"),
    }
    let invoked = client
        .invoke_toolkit(&bearer, InvokeToolkit::default())
        .await;
    assert!(matches!(invoked, Err(ToolCallError::Refused(refusal)) if refusal.http_status == 403));
}

#[tokio::test]
async fn a_toolkit_invocation_skips_progress() {
    let tools = FakeInternalTools::new("cb")
        .with_invoke_result(&["admitted"], r#"{"ok":true,"result":"x"}"#);
    let client = InternalToolClient::new(serve(None, Some(tools.clone())));
    let result = client
        .invoke_toolkit(
            &Bearer::new("cb").expect("bearer"),
            InvokeToolkit {
                project_id: 7,
                toolkit_id: "12".to_owned(),
                tool_name: "search".to_owned(),
                tool_params: Bytes::from_static(b"{}"),
                request_id: "r-1".to_owned(),
            },
        )
        .await
        .expect("invoke");
    assert_eq!(result, Bytes::from_static(br#"{"ok":true,"result":"x"}"#));
    assert_eq!(tools.calls()[0].scope, "12");
}

#[tokio::test]
async fn dropping_a_call_cancels_it_on_the_server() {
    let tools = FakeInternalTools::new("actor")
        .with_call_result(&["admitted"], "{}")
        .hanging();
    let client = InternalToolClient::new(serve(None, Some(tools.clone())));
    let bearer = Bearer::new("actor").expect("bearer");
    let scope = Scope {
        project_id: 7,
        scope: String::new(),
    };
    let mut call = client
        .call_tool(&bearer, &scope, "slow", Bytes::new())
        .await
        .expect("call");
    assert_eq!(
        call.next().await.expect("frame"),
        Some(CallFrame::Progress("admitted".to_owned()))
    );
    assert_eq!(tools.released(), 0);
    drop(call);
    for _ in 0..100 {
        if tools.released() == 1 {
            return;
        }
        tokio::time::sleep(Duration::from_millis(20)).await;
    }
    panic!("the server kept the dropped call");
}
