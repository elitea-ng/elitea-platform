//! Writing a dataset to the project's artifact bucket: the one place the
//! analysis toolkits store what they extract, through the same artifact host
//! the other tracker families use.

use crate::toolkits::families::attachment_host::AttachmentHost;

use super::csv::{self, CsvError};
use super::table::Table;

/// A dataset file that was written.
#[derive(Clone, Debug, Eq, PartialEq)]
pub(crate) struct Stored {
    pub(crate) bucket: String,
    pub(crate) key: String,
}

impl Stored {
    /// The artifact path other tools take: `/{bucket}/{file}`.
    pub(crate) fn path(&self) -> String {
        format!("/{}/{}", self.bucket, self.key)
    }
}

/// Write `table` as `{stem}.csv`, or `{stem}-2.csv`, ... when the name is
/// taken: an extraction never replaces a file. The bucket is created when it
/// does not exist. A failure is the sentence the model reads; nothing is
/// written when the file would pass `max_bytes`.
pub(crate) async fn write_dataset(
    host: &AttachmentHost,
    bucket: &str,
    stem: &str,
    table: &Table,
    max_bytes: usize,
) -> Result<Stored, String> {
    let bytes = csv::write(table, max_bytes).map_err(|error| match error {
        CsvError::TooLarge => format!(
            "The dataset is larger than {} MiB, so nothing was written. Narrow the projects, the dates or the filter.",
            max_bytes / (1_024 * 1_024)
        ),
        CsvError::TooManyRows | CsvError::Malformed => {
            "The dataset could not be written as CSV.".to_owned()
        }
    })?;
    let stored = host
        .write_unique_artifact_creating_bucket(bucket, stem, "csv", "text/csv", bytes)
        .await
        .map_err(|error| error.sentence())?;
    Ok(Stored {
        bucket: stored.bucket,
        key: stored.key,
    })
}
