//! A repository's MCP files as proposals (#1230): `.mcp.json`,
//! `.cursor/mcp.json` and `.vscode/mcp.json`, read through the
//! [`Workspace`] (`O_NOFOLLOW`, confined to the folder, `path_deny`
//! honoured). Nothing here starts a server: the host shows the proposal
//! (environment values masked) and copies the servers the person accepts
//! into the workspace's own `mcp.json` in host data, with `source:
//! "proposal:<sha256 of the file>"`. A later change to the file is a new
//! proposal (a new digest); an accepted entry is never updated from it.
//!
//! Both common shapes are read: `{"mcpServers": {name: {command, args,
//! env}}}` (Claude, Cursor) and `{"servers": {name: {type: "stdio",
//! command, args, env}}}` (VS Code). Servers that are not stdio (`url`,
//! another `type`) are left out.

use std::collections::BTreeMap;
use std::path::{Path, PathBuf};

use serde::Serialize;
use serde_json::Value;
use sha2::{Digest as _, Sha256};

use super::config::{MAX_SERVERS, ServerConfig, server_name_ok};
use crate::error::{ErrorCode, ToolError, ToolResult};
use crate::workspace::{Intent, Workspace};

/// Where a repository proposes servers, in the order they are read.
pub const FILES: &[&str] = &[".mcp.json", ".cursor/mcp.json", ".vscode/mcp.json"];

/// The largest proposal file read.
const MAX_PROPOSAL_BYTES: u64 = 256 * 1024;

/// One proposed server, as the card shows it.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct ProposedServer {
    pub name: String,
    /// As the file writes it (`npx`, or a path).
    pub command: String,
    pub args: Vec<String>,
    pub env_keys: Vec<String>,
    /// Each value masked: its first two characters and its length.
    pub env_preview: BTreeMap<String, String>,
    /// Why it cannot be accepted as written, if it cannot.
    #[serde(skip_serializing_if = "Option::is_none")]
    pub problem: Option<String>,
    #[serde(skip)]
    env: BTreeMap<String, String>,
}

/// One repository file's proposal.
#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct Proposal {
    /// Workspace-relative.
    pub file: String,
    /// Of the file's bytes, lowercase hex: what an answer names.
    pub file_sha256: String,
    pub servers: Vec<ProposedServer>,
}

fn mask(value: &str) -> String {
    let head: String = value.chars().take(2).collect();
    format!("{head}… ({} characters)", value.chars().count())
}

fn strings(value: Option<&Value>) -> Vec<String> {
    value
        .and_then(Value::as_array)
        .map(|items| {
            items
                .iter()
                .filter_map(Value::as_str)
                .map(str::to_owned)
                .collect()
        })
        .unwrap_or_default()
}

/// The proposal in `bytes` (`None`: it proposes no stdio server).
///
/// # Errors
///
/// [`ErrorCode::InvalidArgument`] when it is not a JSON object.
pub fn parse(file: &str, bytes: &[u8]) -> ToolResult<Option<Proposal>> {
    let root: Value = serde_json::from_slice(bytes)
        .map_err(|_| ToolError::invalid(format!("`{file}` is not valid JSON")))?;
    let servers = root
        .get("mcpServers")
        .or_else(|| root.get("servers"))
        .and_then(Value::as_object);
    let Some(servers) = servers else {
        return Ok(None);
    };
    let mut out = Vec::new();
    for (name, entry) in servers.iter().take(MAX_SERVERS) {
        let stdio = entry
            .get("type")
            .and_then(Value::as_str)
            .is_none_or(|kind| kind == "stdio")
            && entry.get("url").is_none();
        let Some(command) = entry
            .get("command")
            .and_then(Value::as_str)
            .filter(|_| stdio)
        else {
            continue;
        };
        let env: BTreeMap<String, String> = entry
            .get("env")
            .and_then(Value::as_object)
            .map(|env| {
                env.iter()
                    .filter_map(|(key, value)| Some((key.clone(), value.as_str()?.to_owned())))
                    .collect()
            })
            .unwrap_or_default();
        let mut server = ProposedServer {
            name: name.clone(),
            command: command.to_owned(),
            args: strings(entry.get("args")),
            env_keys: env.keys().cloned().collect(),
            env_preview: env
                .iter()
                .map(|(key, value)| (key.clone(), mask(value)))
                .collect(),
            problem: None,
            env,
        };
        if !server_name_ok(name) {
            server.problem = Some("its name must match [A-Za-z0-9_-]{1,32}".to_owned());
        } else if command.contains('/') && !Path::new(command).is_absolute() {
            server.problem =
                Some("its command is a relative path (inside the repository)".to_owned());
        } else if let Err(error) = candidate(&server, "/bin/true").validate() {
            server.problem = Some(error.message().to_owned());
        }
        out.push(server);
    }
    if out.is_empty() {
        return Ok(None);
    }
    let digest = Sha256::digest(bytes);
    Ok(Some(Proposal {
        file: file.to_owned(),
        file_sha256: super::config::hex(&digest),
        servers: out,
    }))
}

fn candidate(server: &ProposedServer, command: &str) -> ServerConfig {
    let mut config = ServerConfig::new(command);
    config.args.clone_from(&server.args);
    config.env.clone_from(&server.env);
    config
}

/// What the workspace proposes now, file by file (a file that cannot be
/// read, is denied or does not parse proposes nothing).
#[must_use]
pub fn read(workspace: &Workspace) -> Vec<Proposal> {
    FILES
        .iter()
        .filter_map(|file| {
            let path = workspace.resolve(file, Intent::Read).ok()?;
            let read = workspace.read(&path, MAX_PROPOSAL_BYTES).ok()?;
            parse(file, &read.bytes).ok().flatten()
        })
        .collect()
}

/// The first executable `name` in the absolute `search` directories (the
/// path as found, not resolved: a launcher that reads its own name keeps it).
fn on_path(name: &str, search: &[PathBuf]) -> Option<PathBuf> {
    use std::os::unix::fs::PermissionsExt as _;
    search.iter().map(|dir| dir.join(name)).find(|path| {
        path.metadata()
            .is_ok_and(|meta| meta.is_file() && meta.permissions().mode() & 0o111 != 0)
    })
}

/// The servers of `proposal` named in `accept`, as entries of the
/// workspace's `mcp.json`: the command found on `search` (the absolute
/// `PATH` entries) when the file names a bare program, the network off,
/// `source` naming the proposal's digest.
///
/// # Errors
///
/// [`ErrorCode::NotFound`] for a name the proposal does not hold;
/// [`ErrorCode::InvalidArgument`] for one that cannot be accepted.
pub fn accept(
    proposal: &Proposal,
    accept: &[String],
    search: &[PathBuf],
) -> ToolResult<Vec<(String, ServerConfig)>> {
    accept
        .iter()
        .map(|name| {
            let server = proposal
                .servers
                .iter()
                .find(|server| &server.name == name)
                .ok_or_else(|| {
                    ToolError::new(ErrorCode::NotFound, format!("`{name}` is not proposed"))
                })?;
            if let Some(problem) = &server.problem {
                return Err(ToolError::invalid(format!("`{name}`: {problem}")));
            }
            let command = if Path::new(&server.command).is_absolute() {
                PathBuf::from(&server.command)
            } else {
                on_path(&server.command, search).ok_or_else(|| {
                    ToolError::invalid(format!(
                        "`{name}`: `{}` is not found on PATH",
                        server.command
                    ))
                })?
            };
            let mut config = candidate(server, &command.display().to_string());
            config.source = format!("proposal:{}", proposal.file_sha256);
            config.validate()?;
            Ok((name.clone(), config))
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn both_shapes_parse_and_values_are_masked() {
        let claude = br#"{"mcpServers": {
            "fs": {"command": "npx", "args": ["-y", "@modelcontextprotocol/server-filesystem", "."],
                   "env": {"API_TOKEN": "secret-value-123"}},
            "remote": {"type": "http", "url": "https://example.com/mcp"}
        }}"#;
        let proposal = parse(".mcp.json", claude).expect("parse").expect("some");
        assert_eq!(proposal.servers.len(), 1, "the http server is left out");
        let fs = &proposal.servers[0];
        assert_eq!(fs.env_keys, ["API_TOKEN"]);
        assert!(!fs.env_preview["API_TOKEN"].contains("secret-value"));
        assert_eq!(proposal.file_sha256.len(), 64);
        let shown = serde_json::to_string(&proposal).expect("json");
        assert!(!shown.contains("secret-value-123"), "{shown}");
        let vscode = br#"{"servers": {"gh": {"type": "stdio", "command": "/usr/bin/true"}}}"#;
        let proposal = parse(".vscode/mcp.json", vscode)
            .expect("parse")
            .expect("some");
        assert_eq!(proposal.servers[0].name, "gh");
        assert!(parse("x", b"{}").expect("parse").is_none());
    }

    #[test]
    fn a_relative_command_is_never_accepted() {
        let bytes = br#"{"mcpServers": {"local": {"command": "./bin/server"}}}"#;
        let proposal = parse(".mcp.json", bytes).expect("parse").expect("some");
        assert!(proposal.servers[0].problem.is_some());
        assert!(accept(&proposal, &["local".to_owned()], &[]).is_err());
    }

    #[test]
    fn accepted_servers_name_the_proposal_and_keep_the_network_off() {
        let bytes = br#"{"mcpServers": {"t": {"command": "true", "env": {"A": "b"}}}}"#;
        let proposal = parse(".mcp.json", bytes).expect("parse").expect("some");
        let search = [PathBuf::from("/usr/bin"), PathBuf::from("/bin")];
        let accepted = accept(&proposal, &["t".to_owned()], &search).expect("accept");
        let (name, config) = &accepted[0];
        assert_eq!(name, "t");
        assert!(Path::new(&config.command).is_absolute());
        assert!(!config.network);
        assert_eq!(config.env["A"], "b");
        assert_eq!(config.source, format!("proposal:{}", proposal.file_sha256));
        assert!(accept(&proposal, &["other".to_owned()], &search).is_err());
    }
}
