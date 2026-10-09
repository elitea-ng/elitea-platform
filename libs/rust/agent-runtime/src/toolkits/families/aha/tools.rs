use std::fmt;
use std::sync::Arc;

use adk_core::{AdkError, ErrorCategory, ErrorComponent, Tool, ToolContext};
use adk_tool::BasicToolset;
use async_trait::async_trait;
use serde_json::{Map, Value, json};

use crate::toolkits::invocation::{MaterializedToolsetError, admit_materialized_toolset};
use crate::toolkits::policy::ToolAdmissionPolicy;

use super::artifact::AhaArtifactResolver;
use super::client::{AhaApi, AhaClient, AhaClientError, AhaOperation, PageOptions, ReadOptions};
use super::config::{AhaConfigError, AhaToolkitConfig};
use super::format::{OutputFormat, parse_fields};

const MAX_ARGUMENT_BYTES: usize = 256 * 1_024;
const MAX_DESCRIPTION_BYTES: usize = 2_000;

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AhaToolsetErrorCode {
    InvalidConfiguration,
    ResourceExhausted,
    UnsupportedSelection,
    Client,
    InvalidDefinition,
}

pub struct AhaToolsetError {
    code: AhaToolsetErrorCode,
}

impl AhaToolsetError {
    #[must_use]
    pub const fn code(&self) -> AhaToolsetErrorCode {
        self.code
    }
}

impl fmt::Debug for AhaToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("AhaToolsetError")
            .field("code", &self.code)
            .finish_non_exhaustive()
    }
}

impl fmt::Display for AhaToolsetError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(match self.code {
            AhaToolsetErrorCode::InvalidConfiguration => "the Aha toolkit configuration is invalid",
            AhaToolsetErrorCode::ResourceExhausted => {
                "the Aha toolkit configuration exceeds its approved limit"
            }
            AhaToolsetErrorCode::UnsupportedSelection => {
                "the selected Aha tool profile is not supported"
            }
            AhaToolsetErrorCode::Client => "the Aha client could not be created",
            AhaToolsetErrorCode::InvalidDefinition => "the Aha ADK tool definition is invalid",
        })
    }
}

impl std::error::Error for AhaToolsetError {}

impl From<AhaConfigError> for AhaToolsetError {
    fn from(source: AhaConfigError) -> Self {
        Self {
            code: match source.code() {
                super::config::AhaConfigErrorCode::InvalidConfiguration => {
                    AhaToolsetErrorCode::InvalidConfiguration
                }
                super::config::AhaConfigErrorCode::ResourceExhausted => {
                    AhaToolsetErrorCode::ResourceExhausted
                }
            },
        }
    }
}

impl From<AhaClientError> for AhaToolsetError {
    fn from(_: AhaClientError) -> Self {
        Self {
            code: AhaToolsetErrorCode::Client,
        }
    }
}

impl From<MaterializedToolsetError> for AhaToolsetError {
    fn from(_: MaterializedToolsetError) -> Self {
        Self {
            code: AhaToolsetErrorCode::InvalidDefinition,
        }
    }
}

/// Build the complete capability-disabled 33-tool Aha family.
#[allow(clippy::needless_pass_by_value)] // Consumes the invocation's credential authority.
pub fn build_aha_toolset(
    toolkit_name: &str,
    config: AhaToolkitConfig,
    policy: &Arc<ToolAdmissionPolicy>,
    artifacts: Arc<AhaArtifactResolver>,
) -> Result<BasicToolset, AhaToolsetError> {
    validate_selection(config.selected_tools())?;
    let selected = config
        .selected_tools()
        .iter()
        .map(ToString::to_string)
        .collect::<Vec<_>>();
    let client: Arc<dyn AhaApi> = Arc::new(AhaClient::new(&config, artifacts)?);
    build_with_api(toolkit_name, &selected, policy, &client)
}

fn validate_selection(selected: &[Box<str>]) -> Result<(), AhaToolsetError> {
    if selected.iter().any(|name| {
        !AhaToolKind::ALL
            .iter()
            .any(|kind| kind.name() == name.as_ref())
    }) {
        return Err(AhaToolsetError {
            code: AhaToolsetErrorCode::UnsupportedSelection,
        });
    }
    Ok(())
}

fn build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn AhaApi>,
) -> Result<BasicToolset, AhaToolsetError> {
    let include_all = selected.is_empty();
    let tools = AhaToolKind::ALL
        .into_iter()
        .filter(|kind| include_all || selected.iter().any(|name| name == kind.name()))
        .map(|kind| Arc::new(AhaTool::new(kind, toolkit_name, Arc::clone(client))) as Arc<dyn Tool>)
        .collect();
    admit_materialized_toolset(toolkit_name, "aha", policy, tools).map_err(Into::into)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_build_with_api(
    toolkit_name: &str,
    selected: &[String],
    policy: &Arc<ToolAdmissionPolicy>,
    client: &Arc<dyn AhaApi>,
) -> Result<BasicToolset, AhaToolsetError> {
    build_with_api(toolkit_name, selected, policy, client)
}

#[cfg(any(test, feature = "test-support"))]
pub fn test_catalog() -> Vec<(&'static str, &'static str)> {
    AhaToolKind::ALL
        .into_iter()
        .map(|kind| (kind.name(), kind.group()))
        .collect()
}

#[derive(Clone, Copy)]
enum AhaToolKind {
    GetFeature,
    GetRequirement,
    GetRelease,
    GetInitiative,
    GetEpic,
    GetIdea,
    GetProduct,
    ListProducts,
    ListFeatures,
    ListRequirements,
    ListReleases,
    ListInitiatives,
    ListEpics,
    ListIdeas,
    Search,
    GetPage,
    SearchDocuments,
    GetFeatureGql,
    GetRequirementGql,
    FindProject,
    SearchRecords,
    ReadRecords,
    AddComment,
    ListComments,
    ManageRecord,
    CreateRecord,
    UpdateRecord,
    DeleteRecord,
    CreateRecordLink,
    CopyRecord,
    FieldsMetadata,
    FieldOptionsMetadata,
    AttachFile,
}

impl AhaToolKind {
    const ALL: [Self; 33] = [
        Self::GetFeature,
        Self::GetRequirement,
        Self::GetRelease,
        Self::GetInitiative,
        Self::GetEpic,
        Self::GetIdea,
        Self::GetProduct,
        Self::ListProducts,
        Self::ListFeatures,
        Self::ListRequirements,
        Self::ListReleases,
        Self::ListInitiatives,
        Self::ListEpics,
        Self::ListIdeas,
        Self::Search,
        Self::GetPage,
        Self::SearchDocuments,
        Self::GetFeatureGql,
        Self::GetRequirementGql,
        Self::FindProject,
        Self::SearchRecords,
        Self::ReadRecords,
        Self::AddComment,
        Self::ListComments,
        Self::ManageRecord,
        Self::CreateRecord,
        Self::UpdateRecord,
        Self::DeleteRecord,
        Self::CreateRecordLink,
        Self::CopyRecord,
        Self::FieldsMetadata,
        Self::FieldOptionsMetadata,
        Self::AttachFile,
    ];

    const fn name(self) -> &'static str {
        match self {
            Self::GetFeature => "get_feature",
            Self::GetRequirement => "get_requirement",
            Self::GetRelease => "get_release",
            Self::GetInitiative => "get_initiative",
            Self::GetEpic => "get_epic",
            Self::GetIdea => "get_idea",
            Self::GetProduct => "get_product",
            Self::ListProducts => "list_products",
            Self::ListFeatures => "list_features",
            Self::ListRequirements => "list_requirements",
            Self::ListReleases => "list_releases",
            Self::ListInitiatives => "list_initiatives",
            Self::ListEpics => "list_epics",
            Self::ListIdeas => "list_ideas",
            Self::Search => "search",
            Self::GetPage => "get_page",
            Self::SearchDocuments => "search_documents",
            Self::GetFeatureGql => "get_feature_gql",
            Self::GetRequirementGql => "get_requirement_gql",
            Self::FindProject => "find_project",
            Self::SearchRecords => "search_records",
            Self::ReadRecords => "read_records",
            Self::AddComment => "add_comment",
            Self::ListComments => "list_comments",
            Self::ManageRecord => "manage_record",
            Self::CreateRecord => "create_record",
            Self::UpdateRecord => "update_record",
            Self::DeleteRecord => "delete_record",
            Self::CreateRecordLink => "create_record_link",
            Self::CopyRecord => "copy_record",
            Self::FieldsMetadata => "fields_metadata",
            Self::FieldOptionsMetadata => "field_options_metadata",
            Self::AttachFile => "attach_file",
        }
    }

    const fn group(self) -> &'static str {
        match self {
            Self::AddComment
            | Self::CreateRecord
            | Self::UpdateRecord
            | Self::CreateRecordLink
            | Self::CopyRecord
            | Self::AttachFile => "write",
            Self::DeleteRecord => "delete",
            Self::ManageRecord => "execute",
            _ => "read",
        }
    }

    const fn is_read_only(self) -> bool {
        matches!(
            self,
            Self::GetFeature
                | Self::GetRequirement
                | Self::GetRelease
                | Self::GetInitiative
                | Self::GetEpic
                | Self::GetIdea
                | Self::GetProduct
                | Self::ListProducts
                | Self::ListFeatures
                | Self::ListRequirements
                | Self::ListReleases
                | Self::ListInitiatives
                | Self::ListEpics
                | Self::ListIdeas
                | Self::Search
                | Self::GetPage
                | Self::SearchDocuments
                | Self::GetFeatureGql
                | Self::GetRequirementGql
                | Self::FindProject
                | Self::SearchRecords
                | Self::ReadRecords
                | Self::ListComments
                | Self::FieldsMetadata
                | Self::FieldOptionsMetadata
        )
    }

    #[allow(clippy::too_many_lines)] // Keeps the exact 33-tool source catalog auditable.
    const fn description(self) -> &'static str {
        match self {
            Self::GetFeature => {
                "Read one Aha feature by reference such as DEVELOP-123 or numeric ID. Optional fields projects top-level keys; output_format is json, csv, or markdown."
            }
            Self::GetRequirement => {
                "Read one Aha requirement by reference such as PROD-5-1 or numeric ID, with optional top-level fields and json, csv, or markdown output."
            }
            Self::GetRelease => {
                "Read one Aha release by reference such as PROD-R-4 or numeric ID, with optional field projection and output format."
            }
            Self::GetInitiative => {
                "Read one Aha initiative by reference such as PROD-I-1 or numeric ID, with optional field projection and output format."
            }
            Self::GetEpic => {
                "Read one Aha epic by reference such as PROD-E-1 or numeric ID, with optional field projection and output format."
            }
            Self::GetIdea => {
                "Read one Aha idea by reference or numeric ID, with optional field projection and output format."
            }
            Self::GetProduct => {
                "Read one Aha product or workspace by reference such as PROD or numeric ID, with optional field projection and output format."
            }
            Self::ListProducts => {
                "List products, optionally updated since a timezone-bearing RFC3339 timestamp. per_page defaults 25 (1..200), max_records defaults 100 (1..2000); traversal is limited to 10 pages."
            }
            Self::ListFeatures => {
                "List features globally or under release_id (preferred) or product_id; optional q and RFC3339 updated_since filters. Results are bounded to max_records and 10 pages."
            }
            Self::ListRequirements => {
                "List requirements owned by required feature_id, optionally filtered by q. Pagination defaults to 25 per page and 100 records, bounded to 10 pages and 2000 records."
            }
            Self::ListReleases => {
                "List releases globally or for product_id, optionally filtering parking_lot. Results support field projection and json, csv, or markdown output."
            }
            Self::ListInitiatives => {
                "List initiatives globally or for product_id with bounded pagination and optional field projection/output formatting."
            }
            Self::ListEpics => {
                "List epics globally or under release_id (preferred) or product_id with bounded pagination and optional projection/output formatting."
            }
            Self::ListIdeas => {
                "List ideas globally or for product_id, optionally filtered by q, with bounded pagination and optional projection/output formatting."
            }
            Self::Search => {
                "Full-text search Aha records using required q and optional type. Returns at most max_records across at most 10 pages in json, csv, or markdown."
            }
            Self::GetPage => {
                "Read an Aha page through GraphQL by page reference such as ABC-N-213; include_parent defaults false."
            }
            Self::SearchDocuments => {
                "Search Aha documents through GraphQL using required query; searchable_type defaults to Page. Returns name, url, searchableId, and searchableType nodes."
            }
            Self::GetFeatureGql => {
                "Read a feature through GraphQL by reference such as DEVELOP-123 when markdown description content is needed."
            }
            Self::GetRequirementGql => {
                "Read a requirement through GraphQL by reference such as ADT-123-1 when markdown description content is needed."
            }
            Self::FindProject => {
                "Find Aha products or workspaces, optionally filtered by q, with bounded pagination and json, csv, or markdown output."
            }
            Self::SearchRecords => {
                "Uniform bounded search dispatcher for feature, requirement, release, idea, epic, initiative, or product. Requirements need feature_id; release_id scopes features and epics."
            }
            Self::ReadRecords => {
                "Uniform single-record reader for feature, requirement, release, initiative, epic, idea, product, or page, with optional fields and json, csv, or markdown output."
            }
            Self::AddComment => {
                "Add a non-empty HTML or plain-text comment to one supported Aha resource. This is a duplicate-prone remote write: an unknown outcome must be reconciled and must not be retried automatically."
            }
            Self::ListComments => {
                "List comments for one supported Aha resource with bounded pagination, optional fields, and json, csv, or markdown output."
            }
            Self::ManageRecord => {
                "Legacy create, update, or delete dispatcher for feature, requirement, idea, release, initiative, epic, or page. Prefer action-specific tools; authorization follows the requested action. Any unknown effect outcome must be reconciled and not retried automatically."
            }
            Self::CreateRecord => {
                "Create a record under parent_id: release for features/epics, feature for requirements, and product for ideas/releases/initiatives/pages. This duplicate-prone remote write must be reconciled, not automatically retried, after an unknown outcome."
            }
            Self::UpdateRecord => {
                "Update an existing record using a bounded properties object. Releases and initiatives require product parent_id. This remote write must be reconciled and not automatically retried after an unknown outcome."
            }
            Self::DeleteRecord => {
                "Delete an existing record. Releases and initiatives require product parent_id. This destructive remote effect must be reconciled and must not be automatically retried after an unknown outcome."
            }
            Self::CreateRecordLink => {
                "Create an Aha record relationship after resolving references to numeric IDs. link_type mappings are 10 relates to, 20 depends on, 30 duplicated by, 40 contained by, 50 impacted by, 60 blocked by, and 80 research for. This duplicate-prone remote write must be reconciled, not automatically retried, after an unknown outcome."
            }
            Self::CopyRecord => {
                "Duplicate one release using its reference or numeric ID; Aha does not support copying other record types. This duplicate-prone remote write must be reconciled, not automatically retried, after an unknown outcome."
            }
            Self::FieldsMetadata => {
                "List account-level custom-field definitions with optional top-level projection and json, csv, or markdown output."
            }
            Self::FieldOptionsMetadata => {
                "List options for the numeric custom-field definition field_id with optional projection and output formatting."
            }
            Self::AttachFile => {
                "Attach an authorized artifact at exact /{bucket}/{filename} to a resource description or to-do. Optional filename overrides the sent basename. File must be non-empty and strictly below 300 MB; the whole attachment phase is bounded to 40 seconds. This duplicate-prone remote write must be reconciled and not automatically retried after an unknown outcome."
            }
        }
    }
}

struct AhaTool {
    kind: AhaToolKind,
    client: Arc<dyn AhaApi>,
    description: Box<str>,
}

impl AhaTool {
    fn new(kind: AhaToolKind, toolkit_name: &str, client: Arc<dyn AhaApi>) -> Self {
        let description = format!("Toolkit: {toolkit_name}\n{}", kind.description());
        Self {
            kind,
            client,
            description: description
                .chars()
                .take(MAX_DESCRIPTION_BYTES)
                .collect::<String>()
                .into_boxed_str(),
        }
    }
}

#[async_trait]
impl Tool for AhaTool {
    fn name(&self) -> &str {
        self.kind.name()
    }
    fn description(&self) -> &str {
        &self.description
    }
    fn is_read_only(&self) -> bool {
        self.kind.is_read_only()
    }
    fn is_concurrency_safe(&self) -> bool {
        self.kind.is_read_only()
    }
    fn parameters_schema(&self) -> Option<Value> {
        Some(schema_for(self.kind))
    }
    async fn execute(
        &self,
        _context: Arc<dyn ToolContext>,
        arguments: Value,
    ) -> adk_core::Result<Value> {
        validate_argument_size(&arguments)?;
        let arguments = arguments.as_object().ok_or_else(invalid_arguments)?;
        execute_kind(self.kind, &self.client, arguments).await
    }
}

#[allow(clippy::too_many_lines)]
async fn execute_kind(
    kind: AhaToolKind,
    client: &Arc<dyn AhaApi>,
    arguments: &Map<String, Value>,
) -> adk_core::Result<Value> {
    reject_unknown_keys(arguments, &schema_for(kind))?;
    let fields = parse_fields(arguments.get("fields")).map_err(|_| invalid_arguments())?;
    let output_format = OutputFormat::parse(optional_text(arguments, "output_format")?)
        .map_err(|_| invalid_arguments())?;
    let read = ReadOptions {
        output_format,
        fields: &fields,
    };
    let page = PageOptions {
        per_page: optional_usize(arguments, "per_page", 25, 200)?,
        max_records: optional_usize(arguments, "max_records", 100, 2000)?,
    };
    let empty_properties = Map::new();
    let operation = match kind {
        AhaToolKind::GetFeature => AhaOperation::GetFeature {
            reference: required_text(arguments, "reference_or_id")?,
            read,
        },
        AhaToolKind::GetRequirement => AhaOperation::GetRequirement {
            reference: required_text(arguments, "reference_or_id")?,
            read,
        },
        AhaToolKind::GetRelease => AhaOperation::GetRelease {
            reference: required_text(arguments, "reference_or_id")?,
            read,
        },
        AhaToolKind::GetInitiative => AhaOperation::GetInitiative {
            reference: required_text(arguments, "reference_or_id")?,
            read,
        },
        AhaToolKind::GetEpic => AhaOperation::GetEpic {
            reference: required_text(arguments, "reference_or_id")?,
            read,
        },
        AhaToolKind::GetIdea => AhaOperation::GetIdea {
            reference: required_text(arguments, "reference_or_id")?,
            read,
        },
        AhaToolKind::GetProduct => AhaOperation::GetProduct {
            reference: required_text(arguments, "reference_or_id")?,
            read,
        },
        AhaToolKind::ListProducts => AhaOperation::ListProducts {
            updated_since: optional_text(arguments, "updated_since")?,
            page,
            read,
        },
        AhaToolKind::ListFeatures => AhaOperation::ListFeatures {
            product_id: optional_text(arguments, "product_id")?,
            release_id: optional_text(arguments, "release_id")?,
            query: optional_text(arguments, "q")?,
            updated_since: optional_text(arguments, "updated_since")?,
            page,
            read,
        },
        AhaToolKind::ListRequirements => AhaOperation::ListRequirements {
            feature_id: required_text(arguments, "feature_id")?,
            query: optional_text(arguments, "q")?,
            page,
            read,
        },
        AhaToolKind::ListReleases => AhaOperation::ListReleases {
            product_id: optional_text(arguments, "product_id")?,
            parking_lot: optional_bool(arguments, "parking_lot")?,
            page,
            read,
        },
        AhaToolKind::ListInitiatives => AhaOperation::ListInitiatives {
            product_id: optional_text(arguments, "product_id")?,
            page,
            read,
        },
        AhaToolKind::ListEpics => AhaOperation::ListEpics {
            product_id: optional_text(arguments, "product_id")?,
            release_id: optional_text(arguments, "release_id")?,
            page,
            read,
        },
        AhaToolKind::ListIdeas => AhaOperation::ListIdeas {
            product_id: optional_text(arguments, "product_id")?,
            query: optional_text(arguments, "q")?,
            page,
            read,
        },
        AhaToolKind::Search => AhaOperation::Search {
            query: required_text(arguments, "q")?,
            record_type: optional_text(arguments, "type")?,
            page,
            read,
        },
        AhaToolKind::GetPage => AhaOperation::GetPage {
            reference: required_text(arguments, "reference")?,
            include_parent: optional_bool(arguments, "include_parent")?.unwrap_or(false),
        },
        AhaToolKind::SearchDocuments => AhaOperation::SearchDocuments {
            query: required_text(arguments, "query")?,
            searchable_type: optional_text(arguments, "searchable_type")?,
        },
        AhaToolKind::GetFeatureGql => AhaOperation::GetFeatureGql {
            reference: required_text(arguments, "reference")?,
        },
        AhaToolKind::GetRequirementGql => AhaOperation::GetRequirementGql {
            reference: required_text(arguments, "reference")?,
        },
        AhaToolKind::FindProject => AhaOperation::FindProject {
            query: optional_text(arguments, "q")?,
            page,
            read,
        },
        AhaToolKind::SearchRecords => AhaOperation::SearchRecords {
            record_type: required_text(arguments, "record_type")?,
            query: optional_text(arguments, "q")?,
            feature_id: optional_text(arguments, "feature_id")?,
            product_id: optional_text(arguments, "product_id")?,
            release_id: optional_text(arguments, "release_id")?,
            updated_since: optional_text(arguments, "updated_since")?,
            page,
            read,
        },
        AhaToolKind::ReadRecords => AhaOperation::ReadRecords {
            record_type: required_text(arguments, "record_type")?,
            reference: required_text(arguments, "reference_or_id")?,
            read,
        },
        AhaToolKind::AddComment => AhaOperation::AddComment {
            resource_type: required_text(arguments, "resource_type")?,
            resource_id: required_text(arguments, "resource_id")?,
            body: required_text(arguments, "body")?,
        },
        AhaToolKind::ListComments => AhaOperation::ListComments {
            resource_type: required_text(arguments, "resource_type")?,
            resource_id: required_text(arguments, "resource_id")?,
            page,
            read,
        },
        AhaToolKind::ManageRecord => AhaOperation::ManageRecord {
            action: required_text(arguments, "action")?,
            record_type: required_text(arguments, "record_type")?,
            record_id: optional_text(arguments, "record_id")?,
            parent_id: optional_text(arguments, "parent_id")?,
            properties: properties(arguments, false, &empty_properties)?,
        },
        AhaToolKind::CreateRecord => AhaOperation::CreateRecord {
            record_type: required_text(arguments, "record_type")?,
            parent_id: required_text(arguments, "parent_id")?,
            properties: properties(arguments, true, &empty_properties)?,
        },
        AhaToolKind::UpdateRecord => AhaOperation::UpdateRecord {
            record_type: required_text(arguments, "record_type")?,
            record_id: required_text(arguments, "record_id")?,
            parent_id: optional_text(arguments, "parent_id")?,
            properties: properties(arguments, true, &empty_properties)?,
        },
        AhaToolKind::DeleteRecord => AhaOperation::DeleteRecord {
            record_type: required_text(arguments, "record_type")?,
            record_id: required_text(arguments, "record_id")?,
            parent_id: optional_text(arguments, "parent_id")?,
        },
        AhaToolKind::CreateRecordLink => AhaOperation::CreateRecordLink {
            from_record_type: required_text(arguments, "from_record_type")?,
            from_id: required_text(arguments, "from_id")?,
            to_record_type: required_text(arguments, "to_record_type")?,
            to_id: required_text(arguments, "to_id")?,
            link_type: u16::try_from(required_u64(arguments, "link_type")?)
                .map_err(|_| invalid_arguments())?,
        },
        AhaToolKind::CopyRecord => AhaOperation::CopyRecord {
            record_type: required_text(arguments, "record_type")?,
            record_id: required_text(arguments, "record_id")?,
        },
        AhaToolKind::FieldsMetadata => AhaOperation::FieldsMetadata { read },
        AhaToolKind::FieldOptionsMetadata => AhaOperation::FieldOptionsMetadata {
            field_id: required_text(arguments, "field_id")?,
            read,
        },
        AhaToolKind::AttachFile => AhaOperation::AttachFile {
            resource_type: required_text(arguments, "resource_type")?,
            resource_id: required_text(arguments, "resource_id")?,
            filepath: required_text(arguments, "filepath")?,
            filename: optional_text(arguments, "filename")?,
        },
    };
    client
        .execute(operation)
        .await
        .map_err(AhaClientError::into_adk)
}

fn properties<'a>(
    arguments: &'a Map<String, Value>,
    required: bool,
    empty: &'a Map<String, Value>,
) -> adk_core::Result<&'a Map<String, Value>> {
    match arguments.get("properties") {
        Some(Value::Object(value)) => Ok(value),
        None if !required => Ok(empty),
        Some(Value::Array(value)) if value.is_empty() && !required => Ok(empty),
        _ => Err(invalid_arguments()),
    }
}

fn validate_argument_size(arguments: &Value) -> adk_core::Result<()> {
    if serde_json::to_vec(arguments)
        .map_err(|_| invalid_arguments())?
        .len()
        > MAX_ARGUMENT_BYTES
    {
        return Err(resource_exhausted_arguments());
    }
    Ok(())
}

fn required_text<'a>(arguments: &'a Map<String, Value>, name: &str) -> adk_core::Result<&'a str> {
    arguments
        .get(name)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .ok_or_else(invalid_arguments)
}

fn optional_text<'a>(
    arguments: &'a Map<String, Value>,
    name: &str,
) -> adk_core::Result<Option<&'a str>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::String(value)) => Ok(Some(value)),
        _ => Err(invalid_arguments()),
    }
}

fn optional_bool(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<Option<bool>> {
    match arguments.get(name) {
        None | Some(Value::Null) => Ok(None),
        Some(Value::Bool(value)) => Ok(Some(*value)),
        _ => Err(invalid_arguments()),
    }
}

fn optional_usize(
    arguments: &Map<String, Value>,
    name: &str,
    default: usize,
    max: usize,
) -> adk_core::Result<usize> {
    let value = match arguments.get(name) {
        None | Some(Value::Null) => return Ok(default),
        Some(Value::Number(value)) => value.as_u64().ok_or_else(invalid_arguments)?,
        _ => return Err(invalid_arguments()),
    };
    let value = usize::try_from(value).map_err(|_| invalid_arguments())?;
    if !(1..=max).contains(&value) {
        return Err(invalid_arguments());
    }
    Ok(value)
}

fn required_u64(arguments: &Map<String, Value>, name: &str) -> adk_core::Result<u64> {
    arguments
        .get(name)
        .and_then(Value::as_u64)
        .ok_or_else(invalid_arguments)
}

fn reject_unknown_keys(arguments: &Map<String, Value>, schema: &Value) -> adk_core::Result<()> {
    let allowed = schema
        .get("properties")
        .and_then(Value::as_object)
        .ok_or_else(invalid_arguments)?;
    if arguments.keys().any(|key| !allowed.contains_key(key)) {
        return Err(invalid_arguments());
    }
    Ok(())
}

fn invalid_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "aha.arguments.invalid",
        "the Aha tool arguments are invalid",
    )
}
fn resource_exhausted_arguments() -> AdkError {
    AdkError::new(
        ErrorComponent::Tool,
        ErrorCategory::InvalidInput,
        "aha.arguments.resource_exhausted",
        "the Aha tool arguments exceed the approved limit",
    )
}

#[allow(clippy::too_many_lines)]
fn schema_for(kind: AhaToolKind) -> Value {
    let mut properties = Map::new();
    let required: &[&str] = match kind {
        AhaToolKind::GetFeature
        | AhaToolKind::GetRequirement
        | AhaToolKind::GetRelease
        | AhaToolKind::GetInitiative
        | AhaToolKind::GetEpic
        | AhaToolKind::GetIdea
        | AhaToolKind::GetProduct => {
            properties.insert(
                "reference_or_id".into(),
                text("Aha entity reference or numeric record ID."),
            );
            add_read(&mut properties);
            &["reference_or_id"]
        }
        AhaToolKind::ListProducts => {
            properties.insert(
                "updated_since".into(),
                optional_text_schema("Optional timezone-bearing RFC3339 updated-since filter."),
            );
            add_page_read(&mut properties);
            &[]
        }
        AhaToolKind::ListFeatures => {
            for (name, desc) in [
                ("product_id", "Optional product reference or ID."),
                (
                    "release_id",
                    "Optional release reference or ID; takes precedence over product_id.",
                ),
                (
                    "updated_since",
                    "Optional timezone-bearing RFC3339 timestamp filter.",
                ),
            ] {
                properties.insert(name.into(), optional_text_schema(desc));
            }
            properties.insert(
                "q".into(),
                optional_query_schema("Optional free-text filter."),
            );
            add_page_read(&mut properties);
            &[]
        }
        AhaToolKind::ListRequirements => {
            properties.insert(
                "feature_id".into(),
                text("Feature reference or ID that owns the requirements."),
            );
            properties.insert(
                "q".into(),
                optional_query_schema("Optional free-text filter."),
            );
            add_page_read(&mut properties);
            &["feature_id"]
        }
        AhaToolKind::ListReleases => {
            properties.insert(
                "product_id".into(),
                optional_text_schema("Optional product reference or ID."),
            );
            properties.insert(
                "parking_lot".into(),
                nullable(
                    json!({"type":"boolean","description":"Optional parking-lot release filter."}),
                    Value::Null,
                ),
            );
            add_page_read(&mut properties);
            &[]
        }
        AhaToolKind::ListInitiatives => {
            properties.insert(
                "product_id".into(),
                optional_text_schema("Optional product reference or ID."),
            );
            add_page_read(&mut properties);
            &[]
        }
        AhaToolKind::ListEpics => {
            properties.insert(
                "product_id".into(),
                optional_text_schema("Optional product reference or ID."),
            );
            properties.insert(
                "release_id".into(),
                optional_text_schema(
                    "Optional release reference or ID; takes precedence over product_id.",
                ),
            );
            add_page_read(&mut properties);
            &[]
        }
        AhaToolKind::ListIdeas => {
            properties.insert(
                "product_id".into(),
                optional_text_schema("Optional product reference or ID."),
            );
            properties.insert(
                "q".into(),
                optional_query_schema("Optional free-text filter."),
            );
            add_page_read(&mut properties);
            &[]
        }
        AhaToolKind::Search => {
            properties.insert(
                "q".into(),
                query_schema("Required non-empty full-text query."),
            );
            properties.insert("type".into(), optional_text_schema("Optional record-type filter such as feature, requirement, release, idea, or epic."));
            add_page_read(&mut properties);
            &["q"]
        }
        AhaToolKind::GetPage => {
            properties.insert(
                "reference".into(),
                text("Aha page reference such as ABC-N-213."),
            );
            properties.insert("include_parent".into(), nullable(json!({"type":"boolean","default":false,"description":"Include the parent page; defaults false."}), Value::Bool(false)));
            &["reference"]
        }
        AhaToolKind::SearchDocuments => {
            properties.insert(
                "query".into(),
                query_schema("Required document-search query."),
            );
            properties.insert(
                "searchable_type".into(),
                optional_text_default("Document type filter; defaults to Page.", "Page"),
            );
            &["query"]
        }
        AhaToolKind::GetFeatureGql => {
            properties.insert(
                "reference".into(),
                text("Feature reference such as DEVELOP-123."),
            );
            &["reference"]
        }
        AhaToolKind::GetRequirementGql => {
            properties.insert(
                "reference".into(),
                text("Requirement reference such as ADT-123-1."),
            );
            &["reference"]
        }
        AhaToolKind::FindProject => {
            properties.insert(
                "q".into(),
                optional_query_schema("Optional free-text product-name filter."),
            );
            add_page_read(&mut properties);
            &[]
        }
        AhaToolKind::SearchRecords => {
            properties.insert("record_type".into(), search_record_schema());
            properties.insert(
                "q".into(),
                optional_query_schema("Optional free-text filter."),
            );
            for (name, desc) in [
                (
                    "feature_id",
                    "Feature reference required for requirement searches.",
                ),
                ("product_id", "Optional product scope."),
                (
                    "release_id",
                    "Optional release scope for features or epics.",
                ),
                ("updated_since", "Optional timezone-bearing RFC3339 filter."),
            ] {
                properties.insert(name.into(), optional_text_schema(desc));
            }
            add_page_read(&mut properties);
            &["record_type"]
        }
        AhaToolKind::ReadRecords => {
            properties.insert("record_type".into(), read_record_schema());
            properties.insert(
                "reference_or_id".into(),
                text("Aha reference or numeric record ID."),
            );
            add_read(&mut properties);
            &["record_type", "reference_or_id"]
        }
        AhaToolKind::AddComment => {
            properties.insert("resource_type".into(), resource_type_schema(true));
            properties.insert(
                "resource_id".into(),
                text("Target Aha reference or numeric ID."),
            );
            properties.insert("body".into(), json!({"type":"string","minLength":1,"maxLength":65536,"description":"Non-empty HTML or plain-text comment body, at most 64 KiB UTF-8 within the shared 256 KiB argument envelope."}));
            &["resource_type", "resource_id", "body"]
        }
        AhaToolKind::ListComments => {
            properties.insert("resource_type".into(), resource_type_schema(true));
            properties.insert(
                "resource_id".into(),
                text("Target Aha reference or numeric ID."),
            );
            add_page_read(&mut properties);
            &["resource_type", "resource_id"]
        }
        AhaToolKind::ManageRecord => {
            properties.insert(
                "action".into(),
                enum_text(
                    &["create", "update", "delete"],
                    "Effect action; authorization follows create, update, or delete.",
                ),
            );
            properties.insert("record_type".into(), manageable_schema());
            properties.insert(
                "record_id".into(),
                optional_text_schema(
                    "Existing record ID for update/delete; legacy create parent alias.",
                ),
            );
            properties.insert("parent_id".into(), optional_text_schema("Required parent scope for create and product scope for release/initiative mutation."));
            properties.insert("properties".into(), properties_schema(true));
            &["action", "record_type"]
        }
        AhaToolKind::CreateRecord => {
            properties.insert("record_type".into(), manageable_schema());
            properties.insert("parent_id".into(), text("Required parent release, feature, or product reference/ID according to record type."));
            properties.insert("properties".into(), properties_schema(false));
            &["record_type", "parent_id", "properties"]
        }
        AhaToolKind::UpdateRecord => {
            properties.insert("record_type".into(), manageable_schema());
            properties.insert(
                "record_id".into(),
                text("Existing record reference or numeric ID."),
            );
            properties.insert(
                "parent_id".into(),
                optional_text_schema("Product reference/ID required for releases and initiatives."),
            );
            properties.insert("properties".into(), properties_schema(false));
            &["record_type", "record_id", "properties"]
        }
        AhaToolKind::DeleteRecord => {
            properties.insert("record_type".into(), manageable_schema());
            properties.insert(
                "record_id".into(),
                text("Existing record reference or numeric ID."),
            );
            properties.insert(
                "parent_id".into(),
                optional_text_schema("Product reference/ID required for releases and initiatives."),
            );
            &["record_type", "record_id"]
        }
        AhaToolKind::CreateRecordLink => {
            properties.insert(
                "from_record_type".into(),
                enum_text(
                    &[
                        "feature",
                        "release",
                        "idea",
                        "epic",
                        "release_phase",
                        "initiative",
                        "page",
                        "goal",
                        "requirement",
                    ],
                    "Source record type.",
                ),
            );
            properties.insert(
                "from_id".into(),
                text("Source reference or numeric ID; release phases require numeric IDs."),
            );
            properties.insert(
                "to_record_type".into(),
                enum_text(
                    &[
                        "feature",
                        "release",
                        "idea",
                        "epic",
                        "release_phase",
                        "initiative",
                        "page",
                        "goal",
                    ],
                    "Target record type.",
                ),
            );
            properties.insert(
                "to_id".into(),
                text("Target reference or numeric ID; release phases require numeric IDs."),
            );
            properties.insert("link_type".into(), json!({"type":"integer","enum":[10,20,30,40,50,60,80],"description":"Relationship code: 10 relates to, 20 depends on, 30 duplicated by, 40 contained by, 50 impacted by, 60 blocked by, or 80 research for."}));
            &[
                "from_record_type",
                "from_id",
                "to_record_type",
                "to_id",
                "link_type",
            ]
        }
        AhaToolKind::CopyRecord => {
            properties.insert(
                "record_type".into(),
                enum_text(&["release"], "Record type; only release is supported."),
            );
            properties.insert("record_id".into(), text("Release reference or numeric ID."));
            &["record_type", "record_id"]
        }
        AhaToolKind::FieldsMetadata => {
            add_read(&mut properties);
            &[]
        }
        AhaToolKind::FieldOptionsMetadata => {
            properties.insert(
                "field_id".into(),
                text("Numeric custom-field definition ID returned by fields_metadata."),
            );
            add_read(&mut properties);
            &["field_id"]
        }
        AhaToolKind::AttachFile => {
            properties.insert("resource_type".into(), resource_type_schema(false));
            properties.insert(
                "resource_id".into(),
                text("Target Aha reference or numeric ID."),
            );
            properties.insert("filepath".into(), json!({"type":"string","minLength":1,"maxLength":2048,"description":"Exact authorized artifact path in /{bucket}/{filename} format."}));
            properties.insert("filename".into(), nullable(json!({"type":"string","minLength":1,"maxLength":255,"description":"Optional safe filename override; defaults to the artifact basename."}), Value::Null));
            &["resource_type", "resource_id", "filepath"]
        }
    };
    json!({"type":"object","properties":properties,"required":required,"additionalProperties":false,"description":format!("{} parameters; tool group {}.", kind.name(), kind.group())})
}

fn add_read(properties: &mut Map<String, Value>) {
    properties.insert("output_format".into(), nullable(json!({"type":"string","enum":["json","csv","markdown"],"default":"json","description":"Response format: json, csv, or markdown; defaults to json."}), json!("json")));
    properties.insert("fields".into(), nullable(json!({"type":"array","items":{"type":"string","minLength":1,"maxLength":256},"maxItems":128,"description":"Optional allowlist of at most 128 top-level response fields, each at most 256 UTF-8 bytes."}), Value::Null));
}

fn add_page_read(properties: &mut Map<String, Value>) {
    properties.insert("per_page".into(), nullable(json!({"type":"integer","minimum":1,"maximum":200,"default":25,"description":"Records per Aha page; defaults 25, range 1..200."}), json!(25)));
    properties.insert("max_records".into(), nullable(json!({"type":"integer","minimum":1,"maximum":2000,"default":100,"description":"Total record cap; defaults 100, range 1..2000 and at most 10 pages."}), json!(100)));
    add_read(properties);
}

fn text(description: &str) -> Value {
    json!({"type":"string","minLength":1,"maxLength":1024,"description":description})
}
fn query_schema(description: &str) -> Value {
    json!({"type":"string","minLength":1,"maxLength":16384,"description":description})
}
fn optional_query_schema(description: &str) -> Value {
    nullable(
        json!({"type":"string","maxLength":16384,"description":description}),
        Value::Null,
    )
}
fn optional_text_schema(description: &str) -> Value {
    nullable(
        json!({"type":"string","maxLength":1024,"description":description}),
        Value::Null,
    )
}
fn optional_text_default(description: &str, default: &str) -> Value {
    nullable(
        json!({"type":"string","maxLength":1024,"default":default,"description":description}),
        Value::String(default.to_owned()),
    )
}
fn nullable(value: Value, default: Value) -> Value {
    Value::Object(Map::from_iter([
        (
            "anyOf".to_owned(),
            Value::Array(vec![value, json!({"type":"null"})]),
        ),
        ("default".to_owned(), default),
    ]))
}
fn enum_text(values: &[&str], description: &str) -> Value {
    json!({"type":"string","minLength":1,"maxLength":1024,"enum":values,"description":description})
}
fn manageable_schema() -> Value {
    enum_text(
        &[
            "feature",
            "requirement",
            "idea",
            "release",
            "initiative",
            "epic",
            "page",
        ],
        "Record type: feature, requirement, idea, release, initiative, epic, or page.",
    )
}
fn search_record_schema() -> Value {
    enum_text(
        &[
            "feature",
            "requirement",
            "release",
            "idea",
            "epic",
            "initiative",
            "product",
        ],
        "Record type to search.",
    )
}
fn read_record_schema() -> Value {
    enum_text(
        &[
            "feature",
            "requirement",
            "release",
            "initiative",
            "epic",
            "idea",
            "product",
            "page",
        ],
        "Record type to read.",
    )
}
fn properties_schema(default_empty: bool) -> Value {
    let mut schema = json!({"type":"object","maxProperties":8192,"description":"Aha field/value map limited to 245760 JSON-encoded bytes, 16384 key/value nodes, 32 nesting levels, and the shared 256 KiB argument envelope; every string is limited to 64 KiB."});
    if default_empty && let Some(object) = schema.as_object_mut() {
        object.insert("default".into(), json!({}));
    }
    schema
}
fn resource_type_schema(canonical: bool) -> Value {
    let description = if canonical {
        "Canonical comment resource type; use to_do for a to-do."
    } else {
        "Aha attachment resource type: feature, requirement, idea, release, release_phase, epic, initiative, product, goal, page, or to_do. To-do aliases todo, to-do, to-dos, to_dos, task, and tasks normalize to to_do; matching is case-insensitive."
    };
    if canonical {
        enum_text(
            &[
                "feature",
                "requirement",
                "idea",
                "release",
                "release_phase",
                "epic",
                "initiative",
                "goal",
                "page",
                "to_do",
            ],
            description,
        )
    } else {
        text(description)
    }
}
