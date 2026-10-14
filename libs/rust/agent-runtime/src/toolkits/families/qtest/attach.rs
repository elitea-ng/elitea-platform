//! The two artifact uploads: a file from the artifact store attached to a
//! test case (or one of its steps), or to the test case or latest test log of
//! a test run (`blob-handles`, the file as the raw request body).
//!
//! Every failure is returned as the tool's text, so the model reads what did
//! and did not happen (the admission layer reduces an error to a generic
//! line). Each lookup before the upload only reads; the upload is the single
//! effect, never retried: a lost answer says the file may or may not be
//! attached.

use serde_json::{Map, Value};

use crate::toolkits::families::attachment_host::{ArtifactFile, MAX_ATTACH_BYTES};
use crate::toolkits::families::python_repr::str_of;

use super::client::{QtestCall, QtestClientError, QtestClientErrorCode};
use super::schema::QtestToolKind;
use super::tools::{
    Failure, Qtest, invalid_arguments, optional_integer, optional_text, required_text,
};

impl Qtest {
    pub(super) async fn attach(
        &self,
        kind: QtestToolKind,
        arguments: &Map<String, Value>,
    ) -> Result<Value, Failure> {
        if kind == QtestToolKind::AddFileToTestCase {
            self.add_file_to_test_case(arguments).await
        } else {
            self.upload_attachment_to_test_run(arguments).await
        }
    }

    async fn add_file_to_test_case(
        &self,
        arguments: &Map<String, Value>,
    ) -> Result<Value, Failure> {
        let test_case_id = required_text(arguments, "test_case_id")?;
        let filepath = required_text(arguments, "filepath")?;
        let filename = optional_text(arguments, "filename")?;
        let step = optional_integer(arguments, "test_step_number")?;
        // `startswith('TC-')` is a DQL lookup, anything else is `int(...)`.
        let qtest_id = if test_case_id.starts_with("TC-") {
            str_of(&self.test_case_qtest_id(test_case_id).await?)
        } else {
            let trimmed = test_case_id.trim();
            if trimmed.is_empty()
                || trimmed.len() > 20
                || !trimmed.bytes().all(|byte| byte.is_ascii_digit())
            {
                return Err(Failure::Adk(invalid_arguments()));
            }
            trimmed.trim_start_matches('0').to_owned()
        };
        let file = self.read_file(filepath).await?;
        let (object_type, object_id, location) = match step {
            None => ("test-cases", qtest_id, format!("test case {test_case_id}")),
            Some(number) => {
                let path = format!("test-cases/{qtest_id}");
                let test_case = self
                    .call(QtestCall::get(&path).query("expand", "teststep"))
                    .await?;
                let step_id = test_case
                    .get("test_steps")
                    .and_then(Value::as_array)
                    .and_then(|steps| {
                        steps
                            .iter()
                            .find(|step| step.get("order").and_then(Value::as_i64) == Some(number))
                    })
                    .and_then(|step| step.get("id"))
                    .map(str_of)
                    .filter(|id| !id.is_empty() && id.bytes().all(|byte| byte.is_ascii_digit()));
                let Some(step_id) = step_id else {
                    return Ok(Value::String(format!(
                        "Test step {number} not found in test case {test_case_id}; nothing was uploaded."
                    )));
                };
                (
                    "test-steps",
                    step_id,
                    format!("test case {test_case_id}, step {number}"),
                )
            }
        };
        let attachment = self
            .upload(object_type, &object_id, file, filename, &location)
            .await?;
        Ok(Value::String(format!(
            "File '{}' successfully uploaded to {location}. Attachment ID: {}",
            attachment.name, attachment.id
        )))
    }

    async fn upload_attachment_to_test_run(
        &self,
        arguments: &Map<String, Value>,
    ) -> Result<Value, Failure> {
        let test_run_id = required_text(arguments, "test_run_id")?;
        let attachment_type = required_text(arguments, "attachment_type")?;
        if !matches!(attachment_type, "test-case" | "test-log") {
            return Err(Failure::Adk(invalid_arguments()));
        }
        let filepath = required_text(arguments, "filepath")?;
        let filename = optional_text(arguments, "filename")?;
        let Some(run) = self.entity("test-runs", test_run_id).await? else {
            return Ok(Value::String(format!(
                "Test run {test_run_id} not found in project {}.",
                self.project_id
            )));
        };
        let (object_type, object_id) = if attachment_type == "test-case" {
            let id = run.get("Test Case Id").map(|id| str_of(&id.to_json()));
            let Some(id) = id.filter(|id| !id.is_empty()) else {
                return Ok(Value::String(format!(
                    "Test run {test_run_id} has no associated test case."
                )));
            };
            ("test-cases", id)
        } else {
            let id = run
                .get("Latest Test Log")
                .and_then(|log| log.get("Log Id"))
                .map(|id| str_of(&id.to_json()));
            let Some(id) = id.filter(|id| !id.is_empty() && id != "None") else {
                return Ok(Value::String(format!(
                    "Test run {test_run_id} has no test log yet. Submit a status with 'update_test_run_status' first."
                )));
            };
            ("test-logs", id)
        };
        if !object_id.bytes().all(|byte| byte.is_ascii_digit()) {
            return Err(Failure::Adk(invalid_arguments()));
        }
        let file = self.read_file(filepath).await?;
        let location = format!("test run {test_run_id}'s {attachment_type}");
        let uploaded = self
            .upload(object_type, &object_id, file, filename, &location)
            .await?;
        Ok(Value::String(format!(
            "File '{}' successfully uploaded to test run {test_run_id}'s {attachment_type} (object id {object_id}). Attachment ID: {}",
            uploaded.name, uploaded.id
        )))
    }

    /// The artifact's bytes, or the sentence saying nothing was sent.
    async fn read_file(&self, filepath: &str) -> Result<ArtifactFile, Failure> {
        let Some(host) = &self.attachments else {
            return Err(Failure::Adk(invalid_arguments()));
        };
        host.read_artifact(filepath, MAX_ATTACH_BYTES)
            .await
            .map_err(|error| Failure::Message(error.sentence()))
    }

    /// The one effect: `POST {object_type}/{object_id}/blob-handles`.
    async fn upload(
        &self,
        object_type: &str,
        object_id: &str,
        file: ArtifactFile,
        filename: Option<&str>,
        location: &str,
    ) -> Result<Uploaded, Failure> {
        // `filename or artifact_filename`.
        let name = filename
            .filter(|name| !name.is_empty())
            .map_or(file.name, ToOwned::to_owned);
        if name.len() > 255 || !name.bytes().all(|byte| (0x20..0x7F).contains(&byte)) {
            return Err(Failure::Message(
                "The file name has characters the qTest upload header cannot carry (it must be plain ASCII, at most 255 characters); nothing was uploaded. Pass `filename` with a plain name."
                    .to_owned(),
            ));
        }
        let path = format!("{object_type}/{object_id}/blob-handles");
        let response = self
            .client
            .upload(&path, &name, file.media_type, file.bytes)
            .await
            .map_err(|error| Failure::Message(upload_failure(&error, location)))?;
        // `result.get('id', 'unknown')`.
        let id = response
            .get("id")
            .filter(|id| !id.is_null())
            .map_or_else(|| "unknown".to_owned(), str_of);
        Ok(Uploaded { name, id })
    }
}

struct Uploaded {
    name: String,
    id: String,
}

/// What the model reads when the upload request failed.
fn upload_failure(error: &QtestClientError, location: &str) -> String {
    match error.code() {
        QtestClientErrorCode::Authentication => {
            "qTest rejected the credentials; nothing was uploaded. Do not retry; tell the user."
                .to_owned()
        }
        QtestClientErrorCode::Authorization => {
            format!("qTest did not authorize uploading files to {location}; nothing was uploaded.")
        }
        QtestClientErrorCode::NotFound => {
            format!("qTest did not find {location}; nothing was uploaded.")
        }
        QtestClientErrorCode::InvalidInput | QtestClientErrorCode::Conflict => format!(
            "qTest rejected the upload to {location} as invalid (for example a file type or size it does not accept); nothing was uploaded."
        ),
        QtestClientErrorCode::InvalidConfiguration => {
            "The qTest toolkit configuration is invalid; nothing was uploaded.".to_owned()
        }
        _ => format!(
            "qTest may have applied the requested effect: the file may or may not be attached to {location}. Check its attachments before retrying; never retry blindly."
        ),
    }
}
