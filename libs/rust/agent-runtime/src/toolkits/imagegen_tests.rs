//! The `imagegen` family (#864): the SDK's tool names, schemas and result
//! shape, the settings rules, the exact requests the family makes of its
//! host, every failure sentence, and materialization.

use std::collections::BTreeMap;
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Tool, ToolContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use super::families::artifact::ArtifactToolAuthority;
use super::families::imagegen::{
    ImageGenConfigErrorCode, ImageGenToolkitConfig, build_imagegen_toolset, build_with_host,
};
use super::families::office_host::{
    ImageEditRequest, ImageGenerateRequest, MAX_IMAGE_EDIT_INPUT_BYTES, OfficeFailure,
    OfficeHostError, OfficeModelRequest, OfficeObjectWritten, OfficeToolHost, SharedOfficeToolHost,
};
use super::materialize::materialize_configured_toolsets_with_artifact_authority;
use super::policy::ToolAdmissionPolicy;
use super::snapshot::FrozenToolSnapshot;
use crate::host::{HostError, HostErrorCode, PlatformWriter};
use crate::platform::{
    ArtifactDeleteOutcome, ArtifactDeleteRequest, ArtifactListOutcome, ArtifactListRequest,
    ArtifactReadOutcome, ArtifactReadRequest, ArtifactWriteOutcome, ArtifactWriteRequest,
    ProjectContextWriteOutcome, ProjectContextWriteRequest, SkillWriteOutcome, SkillWriteRequest,
};

type Written = (String, String, String, bool);
type Answer = Result<Vec<Option<Vec<u8>>>, OfficeHostError>;

/// An in-memory bucket and a scripted images API.
#[derive(Default)]
pub(super) struct FakeHost {
    objects: Mutex<BTreeMap<(String, String), Vec<u8>>>,
    /// Every write: bucket, name, media type, overwrite.
    writes: Mutex<Vec<Written>>,
    generated: Mutex<Vec<ImageGenerateRequest>>,
    edited: Mutex<Vec<ImageEditRequest>>,
    /// What the images API answers; `None` is two images.
    answer: Mutex<Option<Answer>>,
    /// The 1-based write that fails, when set.
    fail_write: Mutex<Option<usize>>,
    no_images: bool,
    /// Every `read_object` bound asked for: name and max bytes.
    reads: Mutex<Vec<(String, u64)>>,
    /// The host cannot tell an object's size without reading it.
    size_unknown: bool,
}

impl FakeHost {
    fn put(&self, bucket: &str, name: &str, bytes: &[u8]) {
        self.objects
            .lock()
            .expect("objects")
            .insert((bucket.to_owned(), name.to_owned()), bytes.to_vec());
    }

    fn answer_with(&self, answer: Answer) {
        *self.answer.lock().expect("answer") = Some(answer);
    }

    fn images(&self) -> Answer {
        self.answer
            .lock()
            .expect("answer")
            .clone()
            .unwrap_or_else(|| Ok(vec![Some(vec![1, 1]), Some(vec![2, 2])]))
    }
}

#[async_trait]
impl OfficeToolHost for FakeHost {
    async fn object_size(&self, bucket: &str, name: &str) -> Result<Option<u64>, OfficeHostError> {
        if self.size_unknown {
            return Ok(None);
        }
        self.objects
            .lock()
            .expect("objects")
            .get(&(bucket.to_owned(), name.to_owned()))
            .map(|bytes| Some(bytes.len() as u64))
            .ok_or_else(|| OfficeHostError::new(OfficeFailure::NotFound, "missing"))
    }

    async fn read_object(
        &self,
        bucket: &str,
        name: &str,
        max_bytes: u64,
    ) -> Result<Vec<u8>, OfficeHostError> {
        self.reads
            .lock()
            .expect("reads")
            .push((name.to_owned(), max_bytes));
        self.objects
            .lock()
            .expect("objects")
            .get(&(bucket.to_owned(), name.to_owned()))
            .cloned()
            .ok_or_else(|| OfficeHostError::new(OfficeFailure::NotFound, "missing"))
            .and_then(|bytes| {
                if bytes.len() as u64 > max_bytes {
                    Err(OfficeHostError::new(OfficeFailure::TooLarge, "over"))
                } else {
                    Ok(bytes)
                }
            })
    }

    async fn write_object(
        &self,
        bucket: &str,
        name: &str,
        media_type: &str,
        bytes: Vec<u8>,
        overwrite: bool,
    ) -> Result<OfficeObjectWritten, OfficeHostError> {
        let attempt = {
            let mut writes = self.writes.lock().expect("writes");
            writes.push((
                bucket.to_owned(),
                name.to_owned(),
                media_type.to_owned(),
                overwrite,
            ));
            writes.len()
        };
        if *self.fail_write.lock().expect("fail") == Some(attempt) {
            return Err(OfficeHostError::new(OfficeFailure::Unreachable, "scripted"));
        }
        let byte_length = bytes.len() as u64;
        if !overwrite
            && self
                .objects
                .lock()
                .expect("objects")
                .contains_key(&(bucket.to_owned(), name.to_owned()))
        {
            return Err(OfficeHostError::new(OfficeFailure::AlreadyExists, "taken"));
        }
        self.put(bucket, name, &bytes);
        Ok(OfficeObjectWritten {
            bucket: bucket.to_owned(),
            name: name.to_owned(),
            media_type: media_type.to_owned(),
            byte_length,
        })
    }

    async fn complete(&self, _: OfficeModelRequest) -> Result<String, OfficeHostError> {
        Err(OfficeHostError::new(OfficeFailure::NoModel, "none"))
    }

    fn serves_images(&self) -> bool {
        !self.no_images
    }

    async fn generate_images(
        &self,
        request: ImageGenerateRequest,
    ) -> Result<Vec<Option<Vec<u8>>>, OfficeHostError> {
        self.generated.lock().expect("generated").push(request);
        self.images()
    }

    async fn edit_images(
        &self,
        request: ImageEditRequest,
    ) -> Result<Vec<Option<Vec<u8>>>, OfficeHostError> {
        self.edited.lock().expect("edited").push(request);
        self.images()
    }
}

fn policy() -> Arc<ToolAdmissionPolicy> {
    Arc::new(ToolAdmissionPolicy::new(&[], &BTreeMap::new()).expect("policy"))
}

fn settings(extra: Value) -> Map<String, Value> {
    let Value::Object(extra) = extra else {
        panic!("settings overrides are an object");
    };
    let mut s = json!({"image_generation_model": "img-1", "bucket": "pics"});
    for (key, value) in extra {
        s[key] = value;
    }
    s.as_object().expect("settings").clone()
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(SimpleToolContext::new("imagegen-test"))
}

struct Rig {
    host: Arc<FakeHost>,
    tools: Vec<Arc<dyn Tool>>,
}

impl Rig {
    async fn with(host: FakeHost, settings: &Map<String, Value>) -> Self {
        let host = Arc::new(host);
        let config = ImageGenToolkitConfig::parse(settings).expect("config");
        let shared: SharedOfficeToolHost = Arc::clone(&host) as SharedOfficeToolHost;
        let toolset = build_with_host("pics", &config, &policy(), &shared).expect("toolset");
        let tools = toolset
            .tools(context() as Arc<dyn ReadonlyContext>)
            .await
            .expect("tools");
        Self { host, tools }
    }

    async fn new() -> Self {
        Self::with(FakeHost::default(), &settings(json!({}))).await
    }

    async fn call(&self, tool: &str, args: Value) -> Value {
        let tool = self
            .tools
            .iter()
            .find(|t| t.name() == tool)
            .unwrap_or_else(|| panic!("no tool {tool}"));
        tool.execute(context() as Arc<dyn ToolContext>, args)
            .await
            .expect("a failure is a result, not an error")
    }

    async fn text(&self, tool: &str, args: Value) -> String {
        match self.call(tool, args).await {
            Value::String(s) => s,
            other => panic!("expected a sentence, got {other}"),
        }
    }
}

#[tokio::test]
async fn the_family_serves_the_sdk_tools_with_the_sdk_contract() {
    let rig = Rig::new().await;
    let names: Vec<&str> = rig.tools.iter().map(|t| t.name()).collect();
    assert_eq!(names, ["generate_image", "edit_image"]);
    for tool in &rig.tools {
        assert!(tool.description().starts_with("Toolkit: pics\n"));
        assert!(!tool.is_read_only(), "the SDK groups both under write");
    }
    let generate = &rig.tools[0];
    assert!(generate.description().ends_with(
        "Generate one or more images from a text prompt and save them to the project's artifact bucket."
    ));
    let schema = generate.parameters_schema().expect("schema");
    assert_eq!(schema["required"], json!(["prompt"]));
    let mut props: Vec<&str> = schema["properties"]
        .as_object()
        .expect("props")
        .keys()
        .map(String::as_str)
        .collect();
    props.sort_unstable();
    assert_eq!(props, ["n", "prompt", "size"]);
    let edit = rig.tools[1].parameters_schema().expect("schema");
    assert_eq!(edit["required"], json!(["image", "prompt"]));
    let mut props: Vec<&str> = edit["properties"]
        .as_object()
        .expect("props")
        .keys()
        .map(String::as_str)
        .collect();
    props.sort_unstable();
    assert_eq!(props, ["image", "mask", "prompt"]);
}

#[tokio::test]
async fn generate_image_saves_every_image_and_answers_the_sdk_shape() {
    let rig = Rig::new().await;
    let out = rig
        .call(
            "generate_image",
            json!({"prompt": "a red circle", "size": "1024x1024", "n": 2}),
        )
        .await;
    assert_eq!(
        out,
        json!({"artifacts": [
            {"filepath": "/pics/generated-generate-0.png", "filename": "generated-generate-0.png"},
            {"filepath": "/pics/generated-generate-1.png", "filename": "generated-generate-1.png"},
        ]})
    );
    let requests = rig.host.generated.lock().expect("generated").clone();
    assert_eq!(requests.len(), 1);
    assert_eq!(requests[0].model, "img-1");
    assert_eq!(requests[0].prompt, "a red circle");
    assert_eq!(requests[0].size.as_deref(), Some("1024x1024"));
    assert_eq!(requests[0].count, 2);
    // Written as PNG, replacing an earlier image of the same name (the SDK's
    // artifact create overwrites).
    let writes = rig.host.writes.lock().expect("writes").clone();
    assert_eq!(
        writes,
        [
            (
                "pics".into(),
                "generated-generate-0.png".into(),
                "image/png".into(),
                false
            ),
            (
                "pics".into(),
                "generated-generate-1.png".into(),
                "image/png".into(),
                false
            ),
        ]
    );
}

#[tokio::test]
async fn size_auto_and_a_missing_n_follow_the_sdk() {
    let rig = Rig::with(
        FakeHost::default(),
        &settings(json!({"name_prefix": "e2e-proof-"})),
    )
    .await;
    for args in [
        json!({"prompt": "x", "size": "AUTO"}),
        json!({"prompt": "x", "size": null, "n": null}),
        json!({"prompt": "x", "n": 0}),
    ] {
        rig.call("generate_image", args).await;
    }
    let requests = rig.host.generated.lock().expect("generated").clone();
    assert_eq!(requests.len(), 3);
    for request in &requests {
        assert_eq!(request.size, None);
        assert_eq!(request.count, 1);
    }
    // Three earlier calls kept their images; this one steps over them.
    let out = rig.call("generate_image", json!({"prompt": "x"})).await;
    assert_eq!(
        out["artifacts"][0]["filepath"],
        "/pics/e2e-proof-generate-6.png"
    );
}

#[tokio::test]
async fn bad_arguments_are_refused_before_the_model_is_called() {
    let rig = Rig::new().await;
    assert!(
        rig.text("generate_image", json!({"prompt": "  "}))
            .await
            .contains("prompt")
    );
    assert!(
        rig.text("generate_image", json!({"prompt": "x", "n": 11}))
            .await
            .contains("between 1 and 10")
    );
    assert!(
        rig.text("generate_image", json!({"prompt": "x", "n": -1}))
            .await
            .contains("between 1 and 10")
    );
    assert!(
        rig.text("generate_image", json!({"prompt": "x", "n": "two"}))
            .await
            .contains("whole number")
    );
    assert!(rig.host.generated.lock().expect("generated").is_empty());
}

#[tokio::test]
async fn edit_image_reads_the_source_and_mask_by_filepath() {
    let rig = Rig::new().await;
    rig.host.put("pics", "cat.png", b"CAT");
    rig.host.put("other", "dir/mask.png", b"MASK");
    let out = rig
        .call(
            "edit_image",
            json!({"image": "/pics/cat.png", "prompt": "add a hat", "mask": "/other/dir/mask.png"}),
        )
        .await;
    assert_eq!(
        out["artifacts"][0],
        json!({"filepath": "/pics/generated-edit-0.png", "filename": "generated-edit-0.png"})
    );
    let edits = rig.host.edited.lock().expect("edited").clone();
    assert_eq!(edits.len(), 1);
    assert_eq!(edits[0].model, "img-1");
    assert_eq!(edits[0].prompt, "add a hat");
    assert_eq!(edits[0].image.name, "cat.png");
    assert_eq!(edits[0].image.bytes, b"CAT");
    let mask = edits[0].mask.as_ref().expect("mask");
    assert_eq!(mask.name, "mask.png");
    assert_eq!(mask.bytes, b"MASK");
    // The result lands in the toolkit's own bucket, not the source's.
    assert_eq!(rig.host.writes.lock().expect("writes")[0].0, "pics");

    // No leading slash: a key in the toolkit's bucket.
    rig.call("edit_image", json!({"image": "cat.png", "prompt": "x"}))
        .await;
    assert!(rig.host.edited.lock().expect("edited")[1].mask.is_none());
}

#[tokio::test]
async fn edit_image_names_each_unreadable_input() {
    let rig = Rig::new().await;
    let missing = rig
        .text(
            "edit_image",
            json!({"image": "/pics/none.png", "prompt": "x"}),
        )
        .await;
    assert!(missing.contains("no such file"), "{missing}");
    assert!(missing.contains("/pics/none.png"), "{missing}");
    let bad = rig
        .text("edit_image", json!({"image": "/pics", "prompt": "x"}))
        .await;
    assert!(bad.contains("not an artifact filepath"), "{bad}");
    let dots = rig
        .text(
            "edit_image",
            json!({"image": "/pics/../x.png", "prompt": "x"}),
        )
        .await;
    assert!(dots.contains(".."), "{dots}");
    assert!(rig.host.edited.lock().expect("edited").is_empty());
}

#[tokio::test]
async fn every_failure_is_its_own_sentence() {
    let rig = Rig::new().await;
    let cases = [
        (OfficeFailure::BudgetRefused, "budget is exhausted"),
        (OfficeFailure::AccessRefused, "refused access"),
        (OfficeFailure::InvalidRequest, "refused as invalid"),
        (OfficeFailure::Unreachable, "could not be reached"),
        (OfficeFailure::TooLarge, "larger than the platform accepts"),
        (OfficeFailure::NoModel, "not available in this run"),
        (
            OfficeFailure::RateLimited,
            "rate-limited right now. Try again shortly",
        ),
        (OfficeFailure::Timeout, "timed out"),
        (OfficeFailure::Conflict, "reported a conflict"),
        (
            OfficeFailure::ReplyUnusable,
            "answered but its reply was unusable (it may have been billed). Do not retry without telling the user",
        ),
        (
            OfficeFailure::ReplyTooLarge,
            "answer was larger than the platform accepts",
        ),
    ];
    for (kind, expected) in cases {
        rig.host
            .answer_with(Err(OfficeHostError::new(kind, "scripted")));
        let out = rig.text("generate_image", json!({"prompt": "x"})).await;
        assert!(out.contains(expected), "{kind:?}: {out}");
        assert!(out.starts_with("Could not generate the image"), "{out}");
    }
    // Nothing was written for a failed call.
    assert!(rig.host.writes.lock().expect("writes").is_empty());
}

#[tokio::test]
async fn an_answer_without_inline_images_is_the_sdks_error() {
    let rig = Rig::new().await;
    rig.host.answer_with(Ok(Vec::new()));
    let out = rig.text("generate_image", json!({"prompt": "x"})).await;
    assert_eq!(out, "The image generation model returned no image data");
}

#[tokio::test]
async fn a_missing_model_or_bucket_is_reported_in_the_sdks_words() {
    let no_model = Rig::with(
        FakeHost::default(),
        &settings(json!({"image_generation_model": null})),
    )
    .await;
    assert_eq!(
        no_model
            .text("generate_image", json!({"prompt": "x"}))
            .await,
        "Image generation model is not configured for this toolkit"
    );
    let no_bucket = Rig::with(FakeHost::default(), &settings(json!({"bucket": ""}))).await;
    assert_eq!(
        no_bucket
            .text("generate_image", json!({"prompt": "x"}))
            .await,
        "An artifact bucket is not configured for this toolkit"
    );
    // The billed call is not made when the result could not be saved.
    assert!(
        no_bucket
            .host
            .generated
            .lock()
            .expect("generated")
            .is_empty()
    );
}

#[tokio::test]
async fn selected_tools_follow_the_sdk_rule() {
    for (selected, expected) in [
        (json!(["edit_image"]), vec!["edit_image"]),
        (json!([]), vec!["generate_image", "edit_image"]),
        (json!(null), vec!["generate_image", "edit_image"]),
        (
            json!(["generate_image", "generate_image"]),
            vec!["generate_image"],
        ),
    ] {
        let rig = Rig::with(
            FakeHost::default(),
            &settings(json!({"selected_tools": selected})),
        )
        .await;
        let names: Vec<&str> = rig.tools.iter().map(|t| t.name()).collect();
        assert_eq!(names, expected);
    }
}

#[test]
fn settings_are_bounded_and_typed() {
    let err = ImageGenToolkitConfig::parse(&settings(json!({"bucket": 5})))
        .err()
        .expect("typed");
    assert_eq!(err.code(), ImageGenConfigErrorCode::InvalidConfiguration);
    let err = ImageGenToolkitConfig::parse(&settings(json!({"name_prefix": "x".repeat(300)})))
        .err()
        .expect("bounded");
    assert_eq!(err.code(), ImageGenConfigErrorCode::ResourceExhausted);
    let err = ImageGenToolkitConfig::parse(&settings(json!({"selected_tools": "edit_image"})))
        .err()
        .expect("list");
    assert_eq!(err.code(), ImageGenConfigErrorCode::InvalidConfiguration);
}

pub(super) struct NoPlatform;

#[async_trait]
impl PlatformWriter for NoPlatform {
    async fn list_artifacts(
        &self,
        _: &ArtifactListRequest,
    ) -> Result<ArtifactListOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn read_artifact(
        &self,
        _: &ArtifactReadRequest,
    ) -> Result<ArtifactReadOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn write_artifact(
        &self,
        _: &ArtifactWriteRequest,
    ) -> Result<ArtifactWriteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn delete_artifact(
        &self,
        _: &ArtifactDeleteRequest,
    ) -> Result<ArtifactDeleteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn write_skill(&self, _: &SkillWriteRequest) -> Result<SkillWriteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
    async fn write_project_context(
        &self,
        _: &ProjectContextWriteRequest,
    ) -> Result<ProjectContextWriteOutcome, HostError> {
        Err(HostError::new(HostErrorCode::NotFound, "none"))
    }
}

#[tokio::test]
async fn materialization_needs_a_host_that_reaches_the_images_api() {
    let platform: Arc<dyn PlatformWriter> = Arc::new(NoPlatform);
    let version = json!({"tools": [{
        "id": 9,
        "type": "imagegen",
        "toolkit_name": "pics",
        "settings": {"image_generation_model": "img-1", "bucket": "pics",
                     "selected_tools": ["generate_image"]}
    }]});
    let snapshot = FrozenToolSnapshot::from_version_details(version.as_object().expect("version"))
        .expect("snapshot")
        .apply_policy(policy().as_ref());

    let lent = ArtifactToolAuthority::new(Arc::clone(&platform))
        .with_office(Arc::new(FakeHost::default()) as SharedOfficeToolHost);
    let (toolsets, _) = materialize_configured_toolsets_with_artifact_authority(
        &snapshot,
        &policy(),
        &Map::new(),
        Some(&lent),
    )
    .await
    .expect("materialized");
    assert_eq!(toolsets.len(), 1);
    let tools = toolsets[0]
        .tools(context() as Arc<dyn ReadonlyContext>)
        .await
        .expect("tools");
    assert_eq!(tools.len(), 1);
    assert_eq!(tools[0].name(), "generate_image");

    // An Office host with no images API, artifact authority without Office,
    // and no authority at all: skipped as unsupported, never bound to tools
    // that would fail on every call.
    let no_images =
        ArtifactToolAuthority::new(Arc::clone(&platform)).with_office(Arc::new(FakeHost {
            no_images: true,
            ..FakeHost::default()
        })
            as SharedOfficeToolHost);
    let bare = ArtifactToolAuthority::new(platform);
    let config = ImageGenToolkitConfig::parse(&settings(json!({}))).expect("config");
    assert!(
        build_imagegen_toolset("pics", &config, &policy(), &no_images)
            .expect("builds")
            .is_none()
    );
    for authority in [Some(&no_images), Some(&bare), None] {
        let (toolsets, _) = materialize_configured_toolsets_with_artifact_authority(
            &snapshot,
            &policy(),
            &Map::new(),
            authority,
        )
        .await
        .expect("an unserved toolkit is skipped, not fatal");
        assert!(toolsets.is_empty(), "skipped, not bound");
    }
}

#[tokio::test]
async fn a_failed_write_mid_batch_answers_what_was_saved_and_the_failure() {
    let rig = Rig::new().await;
    rig.host
        .answer_with(Ok((1..=4).map(|i| Some(vec![i])).collect()));
    *rig.host.fail_write.lock().expect("fail") = Some(3);
    let out = rig
        .call("generate_image", json!({"prompt": "x", "n": 4}))
        .await;
    // The two saved images are still reported: the model must not re-generate
    // what was billed.
    assert_eq!(
        out["artifacts"],
        json!([
            {"filepath": "/pics/generated-generate-0.png", "filename": "generated-generate-0.png"},
            {"filepath": "/pics/generated-generate-1.png", "filename": "generated-generate-1.png"},
        ])
    );
    let error = out["error"].as_str().expect("the failure sentence");
    assert!(
        error.starts_with("saved 2 of 4. Could not save the image `generated-generate-2.png`: "),
        "{error}"
    );
    assert!(
        error.ends_with("The 1 image after it was not saved."),
        "{error}"
    );
    assert!(error.contains("generated-generate-2.png"), "{error}");
    assert_eq!(rig.host.generated.lock().expect("generated").len(), 1);

    // A failure on the first write has nothing to report: a plain sentence.
    let rig = Rig::new().await;
    *rig.host.fail_write.lock().expect("fail") = Some(1);
    let out = rig.text("generate_image", json!({"prompt": "x"})).await;
    assert!(out.starts_with("Could not save the image"), "{out}");
}

#[tokio::test]
async fn the_file_index_is_the_position_in_the_answer_not_among_decoded_images() {
    let rig = Rig::new().await;
    // [url-only, inline]: the SDK enumerates `data`, so the inline image is -1.
    rig.host.answer_with(Ok(vec![None, Some(vec![9])]));
    let out = rig
        .call("generate_image", json!({"prompt": "x", "n": 2}))
        .await;
    assert_eq!(
        out,
        json!({"artifacts": [
            {"filepath": "/pics/generated-generate-1.png", "filename": "generated-generate-1.png"}
        ]})
    );
    // Nothing inline at all is the SDK's error.
    rig.host.answer_with(Ok(vec![None, None]));
    assert_eq!(
        rig.text("generate_image", json!({"prompt": "x"})).await,
        "The image generation model returned no image data"
    );
}

#[tokio::test]
async fn a_file_name_the_upload_cannot_carry_is_named_before_any_call() {
    let rig = Rig::new().await;
    rig.host.put("pics", "a\"b.png", b"X");
    let out = rig
        .text(
            "edit_image",
            json!({"image": "/pics/a\"b.png", "prompt": "x"}),
        )
        .await;
    assert!(out.contains("cannot be sent to the image model"), "{out}");
    assert!(rig.host.edited.lock().expect("edited").is_empty());
}

#[tokio::test]
async fn an_unusable_or_oversized_reply_never_says_it_did_not_happen() {
    let rig = Rig::new().await;
    for kind in [OfficeFailure::ReplyUnusable, OfficeFailure::ReplyTooLarge] {
        rig.host
            .answer_with(Err(OfficeHostError::new(kind, "scripted")));
        let out = rig.text("generate_image", json!({"prompt": "x"})).await;
        assert!(out.contains("may have been billed"), "{out}");
        assert!(!out.contains("did not happen"), "{out}");
        assert!(!out.contains("your file"), "{out}");
    }
}

#[tokio::test]
async fn a_bucket_or_prefix_the_object_api_would_refuse_is_refused_before_any_call() {
    for bad in [
        json!({"bucket": "a/b"}),
        json!({"bucket": "a\\b"}),
        json!({"name_prefix": "../x"}),
        json!({"name_prefix": "a\"b"}),
        json!({"name_prefix": "a\\b"}),
        json!({"name_prefix": "line\nbreak"}),
    ] {
        let err = ImageGenToolkitConfig::parse(&settings(bad.clone()))
            .err()
            .unwrap_or_else(|| panic!("{bad} must be refused"));
        assert_eq!(err.code(), ImageGenConfigErrorCode::InvalidConfiguration);
    }
    // And through materialization the toolkit is refused, so no tool exists to
    // make a billed call.
    for good in [json!({"bucket": "pics-2"}), json!({"name_prefix": "a/b-"})] {
        ImageGenToolkitConfig::parse(&settings(good)).expect("accepted");
    }
}

#[tokio::test]
async fn the_name_prefix_is_not_trimmed_and_only_absent_or_empty_is_the_default() {
    for (prefix, expected) in [
        (json!("img "), "/pics/img generate-0.png"),
        (json!(" "), "/pics/ generate-0.png"),
        (json!(""), "/pics/generated-generate-0.png"),
        (json!(null), "/pics/generated-generate-0.png"),
    ] {
        let rig = Rig::with(
            FakeHost::default(),
            &settings(json!({"name_prefix": prefix})),
        )
        .await;
        let out = rig.call("generate_image", json!({"prompt": "x"})).await;
        assert_eq!(out["artifacts"][0]["filepath"], expected, "{prefix}");
    }
}

#[tokio::test]
async fn consecutive_calls_never_overwrite_earlier_images() {
    let rig = Rig::new().await;
    let first = rig.call("generate_image", json!({"prompt": "a"})).await;
    let second = rig.call("generate_image", json!({"prompt": "b"})).await;
    assert_eq!(
        first["artifacts"][0]["filepath"],
        "/pics/generated-generate-0.png"
    );
    assert_eq!(
        first["artifacts"][1]["filepath"],
        "/pics/generated-generate-1.png"
    );
    // The second call's images step over the taken names.
    assert_eq!(
        second["artifacts"][0]["filepath"],
        "/pics/generated-generate-2.png"
    );
    assert_eq!(
        second["artifacts"][1]["filepath"],
        "/pics/generated-generate-3.png"
    );
    let objects = rig.host.objects.lock().expect("objects");
    assert_eq!(objects.len(), 4, "all four images are kept");
    assert!(
        rig.host
            .writes
            .lock()
            .expect("writes")
            .iter()
            .all(|write| !write.3),
        "no write asks to replace"
    );
}

#[tokio::test]
async fn the_partial_message_names_the_file_and_counts_only_what_follows() {
    let rig = Rig::new().await;
    rig.host
        .answer_with(Ok((1..=4).map(|i| Some(vec![i])).collect()));
    *rig.host.fail_write.lock().expect("fail") = Some(2);
    let out = rig
        .call("generate_image", json!({"prompt": "x", "n": 4}))
        .await;
    let error = out["error"].as_str().expect("error");
    assert!(
        error.starts_with("saved 1 of 4. Could not save the image `generated-generate-1.png`"),
        "{error}"
    );
    assert!(
        error.ends_with("The 2 images after it were not saved."),
        "{error}"
    );
    // The last image failing has nothing after it to mention.
    let rig = Rig::new().await;
    *rig.host.fail_write.lock().expect("fail") = Some(2);
    let out = rig
        .call("generate_image", json!({"prompt": "x", "n": 2}))
        .await;
    let error = out["error"].as_str().expect("error");
    assert!(!error.contains("after it"), "{error}");
}

#[tokio::test]
async fn an_edit_over_the_combined_limit_is_refused_before_anything_is_read() {
    let rig = Rig::new().await;
    let big = vec![0_u8; usize::try_from(MAX_IMAGE_EDIT_INPUT_BYTES / 2 + 1).expect("fits")];
    rig.host.put("pics", "a.png", &big);
    rig.host.put("pics", "m.png", &big);
    let out = rig
        .text(
            "edit_image",
            json!({"image": "/pics/a.png", "mask": "/pics/m.png", "prompt": "x"}),
        )
        .await;
    assert!(out.contains("together are"), "{out}");
    assert!(out.contains(&(big.len() * 2).to_string()), "{out}");
    assert!(
        rig.host.reads.lock().expect("reads").is_empty(),
        "nothing buffered"
    );
    assert!(rig.host.edited.lock().expect("edited").is_empty());
}

#[tokio::test]
async fn without_sizes_the_reads_are_bounded_by_what_the_first_left() {
    let host = FakeHost {
        size_unknown: true,
        ..FakeHost::default()
    };
    let rig = Rig::with(host, &settings(json!({}))).await;
    let half = vec![0_u8; usize::try_from(MAX_IMAGE_EDIT_INPUT_BYTES / 2 + 1).expect("fits")];
    rig.host.put("pics", "a.png", &half);
    rig.host.put("pics", "m.png", &half);
    let out = rig
        .text(
            "edit_image",
            json!({"image": "/pics/a.png", "mask": "/pics/m.png", "prompt": "x"}),
        )
        .await;
    assert!(out.contains("together are over the"), "{out}");
    let reads = rig.host.reads.lock().expect("reads").clone();
    assert_eq!(reads[1].1, MAX_IMAGE_EDIT_INPUT_BYTES - half.len() as u64);
    assert!(rig.host.edited.lock().expect("edited").is_empty());
}

#[tokio::test]
async fn a_selection_of_only_unknown_tools_is_refused_naming_them() {
    let err = ImageGenToolkitConfig::parse(&settings(
        json!({"selected_tools": ["draw_image", "paint", "draw_image"]}),
    ))
    .err()
    .expect("refused");
    assert_eq!(err.code(), ImageGenConfigErrorCode::InvalidConfiguration);
    let text = err.to_string();
    assert!(text.contains("draw_image, paint"), "{text}");
    // One known name among unknown ones still serves the known one.
    ImageGenToolkitConfig::parse(&settings(
        json!({"selected_tools": ["paint", "edit_image"]}),
    ))
    .expect("accepted");
}
