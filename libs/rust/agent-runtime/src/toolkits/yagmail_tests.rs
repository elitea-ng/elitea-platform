//! Focused compatibility and safety tests for the capability-disabled Yagmail family.

use std::collections::{BTreeMap, VecDeque};
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Mutex};

use adk_core::{ReadonlyContext, Toolset};
use adk_tool::SimpleToolContext;
use async_trait::async_trait;
use base64::Engine as _;
use base64::engine::general_purpose::STANDARD as BASE64_STANDARD;
use serde_json::{Map, Value, json};

use super::families::yagmail::client::{
    MAX_MAILBOX_BYTES, MAX_MESSAGE_BYTES, MAX_MESSAGE_CHARS, MAX_SUBJECT_BYTES, MAX_SUBJECT_CHARS,
    SmtpChannel, SmtpChannelError, SmtpChannelErrorCode, SmtpConnector, YagmailApi, YagmailClient,
    YagmailClientError, YagmailClientErrorCode,
};
use super::families::yagmail::config::{
    MAX_PASSWORD_BYTES, SMTP_PORT, YagmailConfigErrorCode, YagmailToolkitConfig,
};
use super::families::yagmail::tools::{YagmailToolsetErrorCode, test_build_with_api};
use super::policy::ToolAdmissionPolicy;

const PASSWORD: &str = "tanstaaftanstaaf";

fn settings(host: Option<Value>, username: &str, password: &str) -> Map<String, Value> {
    let mut settings = json!({
        "username": username,
        "password": password,
        "selected_tools": []
    })
    .as_object()
    .cloned()
    .expect("Yagmail settings fixture is an object");
    if let Some(host) = host {
        settings.insert("host".to_owned(), host);
    }
    settings
}

fn config(host: &str, username: &str, password: &str) -> YagmailToolkitConfig {
    YagmailToolkitConfig::parse(&settings(Some(json!(host)), username, password))
        .expect("valid Yagmail fixture configuration")
}

fn policy(blocked: &[(&str, &[&str])]) -> Arc<ToolAdmissionPolicy> {
    let blocked = blocked
        .iter()
        .map(|(toolkit, tools)| {
            (
                (*toolkit).to_owned(),
                tools.iter().map(|tool| (*tool).to_owned()).collect(),
            )
        })
        .collect::<BTreeMap<_, _>>();
    Arc::new(ToolAdmissionPolicy::new(&[], &blocked).expect("Yagmail fixture policy"))
}

fn context() -> Arc<SimpleToolContext> {
    Arc::new(
        SimpleToolContext::new("yagmail-test")
            .with_session_id("session-1")
            .with_function_call_id("email-effect-17"),
    )
}

enum ReadStep {
    Line(Vec<u8>),
    Accepted(Vec<u8>),
    Error(SmtpChannelErrorCode),
}

impl ReadStep {
    fn line(value: &str) -> Self {
        Self::Line(value.as_bytes().to_vec())
    }
}

struct TranscriptConnector {
    responses: Arc<Mutex<VecDeque<ReadStep>>>,
    writes: Arc<Mutex<Vec<Vec<u8>>>>,
    connects: Mutex<Vec<(String, u16)>>,
    accepted: Arc<AtomicBool>,
    stall_writes_after_accept: bool,
    connect_error: Option<SmtpChannelErrorCode>,
}

impl TranscriptConnector {
    fn new(responses: impl IntoIterator<Item = ReadStep>) -> Self {
        Self {
            responses: Arc::new(Mutex::new(responses.into_iter().collect())),
            writes: Arc::new(Mutex::new(Vec::new())),
            connects: Mutex::new(Vec::new()),
            accepted: Arc::new(AtomicBool::new(false)),
            stall_writes_after_accept: false,
            connect_error: None,
        }
    }

    fn stalling_after_accept(responses: impl IntoIterator<Item = ReadStep>) -> Self {
        Self {
            stall_writes_after_accept: true,
            ..Self::new(responses)
        }
    }

    fn failing(code: SmtpChannelErrorCode) -> Self {
        Self {
            connect_error: Some(code),
            ..Self::new([])
        }
    }

    fn writes(&self) -> Vec<Vec<u8>> {
        self.writes
            .lock()
            .expect("SMTP fixture writes lock")
            .clone()
    }

    fn connects(&self) -> Vec<(String, u16)> {
        self.connects
            .lock()
            .expect("SMTP fixture connects lock")
            .clone()
    }
}

struct TranscriptChannel {
    responses: Arc<Mutex<VecDeque<ReadStep>>>,
    writes: Arc<Mutex<Vec<Vec<u8>>>>,
    accepted: Arc<AtomicBool>,
    stall_writes_after_accept: bool,
}

#[async_trait]
impl SmtpChannel for TranscriptChannel {
    async fn write_all(&mut self, bytes: &[u8]) -> Result<(), SmtpChannelError> {
        if self.stall_writes_after_accept && self.accepted.load(Ordering::Acquire) {
            return std::future::pending().await;
        }
        self.writes
            .lock()
            .expect("SMTP fixture writes lock")
            .push(bytes.to_vec());
        Ok(())
    }

    async fn read_line(&mut self, limit: usize) -> Result<Vec<u8>, SmtpChannelError> {
        let step = self
            .responses
            .lock()
            .expect("SMTP fixture responses lock")
            .pop_front()
            .unwrap_or(ReadStep::Error(SmtpChannelErrorCode::InvalidResponse));
        match step {
            ReadStep::Line(line) if line.len() <= limit => Ok(line),
            ReadStep::Accepted(line) if line.len() <= limit => {
                self.accepted.store(true, Ordering::Release);
                Ok(line)
            }
            ReadStep::Line(_) | ReadStep::Accepted(_) => Err(SmtpChannelError::fixture(
                SmtpChannelErrorCode::ResourceExhausted,
            )),
            ReadStep::Error(code) => Err(SmtpChannelError::fixture(code)),
        }
    }
}

#[async_trait]
impl SmtpConnector for TranscriptConnector {
    async fn connect(
        &self,
        host: &str,
        port: u16,
    ) -> Result<Box<dyn SmtpChannel>, SmtpChannelError> {
        self.connects
            .lock()
            .expect("SMTP fixture connects lock")
            .push((host.to_owned(), port));
        if let Some(code) = self.connect_error {
            return Err(SmtpChannelError::fixture(code));
        }
        Ok(Box::new(TranscriptChannel {
            responses: Arc::clone(&self.responses),
            writes: Arc::clone(&self.writes),
            accepted: Arc::clone(&self.accepted),
            stall_writes_after_accept: self.stall_writes_after_accept,
        }))
    }
}

fn plain_success_responses(recipient_count: usize) -> Vec<ReadStep> {
    let mut responses = vec![
        ReadStep::line("220 smtp.example.test ready\r\n"),
        ReadStep::line("250-smtp.example.test\r\n"),
        ReadStep::line("250-AUTH PLAIN LOGIN\r\n"),
        ReadStep::line("250 SIZE 524288\r\n"),
        ReadStep::line("235 2.7.0 accepted\r\n"),
        ReadStep::line("250 2.1.0 sender accepted\r\n"),
    ];
    responses
        .extend((0..recipient_count).map(|_| ReadStep::line("250 2.1.5 recipient accepted\r\n")));
    responses.push(ReadStep::line("354 continue\r\n"));
    responses.push(ReadStep::line("250 2.0.0 queued\r\n"));
    responses
}

fn client_with(
    connector: Arc<TranscriptConnector>,
    host: &str,
    username: &str,
    password: &str,
) -> YagmailClient {
    let connector_trait: Arc<dyn SmtpConnector> = connector;
    YagmailClient::with_connector(config(host, username, password), connector_trait)
}

struct FixtureApi {
    calls: Mutex<Vec<Value>>,
}

impl FixtureApi {
    fn new() -> Self {
        Self {
            calls: Mutex::new(Vec::new()),
        }
    }

    fn calls(&self) -> Vec<Value> {
        self.calls.lock().expect("Yagmail API fixture lock").clone()
    }
}

#[async_trait]
impl YagmailApi for FixtureApi {
    async fn send_gmail_message(
        &self,
        effect_id: &str,
        receiver: &str,
        message: &str,
        subject: &str,
        cc: &[String],
    ) -> Result<Value, YagmailClientError> {
        self.calls
            .lock()
            .expect("Yagmail API fixture lock")
            .push(json!({
                "effect_id": effect_id,
                "receiver": receiver,
                "message": message,
                "subject": subject,
                "cc": cc
            }));
        Ok(json!({}))
    }
}

#[test]
fn configuration_is_claim_scoped_redacted_and_exact_authority() {
    for host in [None, Some(Value::Null)] {
        assert!(YagmailToolkitConfig::parse(&settings(host, "sender", PASSWORD)).is_ok());
    }

    for invalid_host in [
        "",
        "127.0.0.1",
        "https://smtp.example.test",
        "user@smtp.example.test",
        "smtp.example.test/path",
        "smtp.example.test:465",
        "smtp.example.test\nother",
        ".smtp.example.test",
        "smtp.example.test.",
    ] {
        let parsed = YagmailToolkitConfig::parse(&settings(
            Some(json!(invalid_host)),
            "sender@example.com",
            PASSWORD,
        ));
        let Err(error) = parsed else {
            panic!("invalid SMTP authority must fail: {invalid_host:?}");
        };
        assert_eq!(error.code(), YagmailConfigErrorCode::InvalidConfiguration);
        let diagnostic = format!("{error:?} {error}");
        assert!(!diagnostic.contains(PASSWORD));
        if !invalid_host.is_empty() {
            assert!(!diagnostic.contains(invalid_host));
        }
    }

    let oversized = "p".repeat(MAX_PASSWORD_BYTES + 1);
    let Err(error) = YagmailToolkitConfig::parse(&settings(
        Some(json!("smtp.example.test")),
        "sender@example.com",
        &oversized,
    )) else {
        panic!("oversized SMTP password must fail");
    };
    assert_eq!(error.code(), YagmailConfigErrorCode::ResourceExhausted);
    assert!(!format!("{error:?}").contains(&"p".repeat(100)));
}

#[tokio::test]
async fn metadata_schema_selection_and_optional_cc_are_truthful() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn YagmailApi> = api.clone();
    let toolkit_name = "邮件".repeat(160);
    let toolset = test_build_with_api(&toolkit_name, &[], &policy(&[]), &api_trait)
        .expect("complete Yagmail fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("Yagmail tools");
    assert_eq!(tools.len(), 1);
    let tool = &tools[0];
    assert_eq!(tool.name(), "send_gmail_message");
    assert!(!tool.is_read_only());
    assert!(!tool.is_concurrency_safe());
    assert!(tool.description().len() <= 1_000);
    for cue in [
        "implicit-TLS",
        "literal UTF-8",
        "empty object",
        "sensitivity policy",
        "one send attempt",
        "no automatic retry",
        "unknown after SMTP DATA",
        "duplicate",
        "durable interrupt/effect identity",
    ] {
        assert!(tool.description().contains(cue), "missing cue: {cue}");
    }
    assert!(!tool.description().contains("smtp.example.test"));
    assert!(!tool.description().contains(PASSWORD));

    let schema = tool.parameters_schema().expect("Yagmail schema");
    assert_eq!(
        schema["required"],
        json!(["receiver", "message", "subject"])
    );
    assert_eq!(schema["additionalProperties"], false);
    assert_eq!(
        schema["properties"]["receiver"]["maxLength"],
        MAX_MAILBOX_BYTES
    );
    assert_eq!(
        schema["properties"]["message"]["maxLength"],
        MAX_MESSAGE_CHARS
    );
    assert_eq!(
        schema["properties"]["subject"]["maxLength"],
        MAX_SUBJECT_CHARS
    );
    assert_eq!(schema["properties"]["cc"]["default"], Value::Null);
    assert_eq!(schema["properties"]["cc"]["anyOf"][0]["maxItems"], 100);
    assert!(
        schema["properties"]["message"]["description"]
            .as_str()
            .expect("message description")
            .contains("49152 UTF-8 bytes")
    );

    assert_eq!(
        tool.execute(
            context(),
            json!({
                "receiver":"person@example.com",
                "message":"hello",
                "subject":"status"
            }),
        )
        .await
        .expect("omitted optional cc executes"),
        json!({})
    );
    assert_eq!(
        api.calls(),
        [json!({
            "effect_id":"email-effect-17",
            "receiver":"person@example.com",
            "message":"hello",
            "subject":"status",
            "cc":[]
        })]
    );

    let Err(error) = test_build_with_api("mail", &["unknown".to_owned()], &policy(&[]), &api_trait)
    else {
        panic!("unknown selection must fail closed");
    };
    assert_eq!(error.code(), YagmailToolsetErrorCode::UnsupportedSelection);

    let blocked = test_build_with_api(
        "mail",
        &[],
        &policy(&[("yagmail", &["send_gmail_message"])]),
        &api_trait,
    )
    .expect("blocked toolset definition is valid");
    let readonly: Arc<dyn ReadonlyContext> = context();
    assert!(
        blocked
            .tools(readonly)
            .await
            .expect("blocked tools")
            .is_empty()
    );
}

#[tokio::test]
async fn cram_md5_is_preferred_and_transcript_is_one_implicit_tls_session() {
    let connector = Arc::new(TranscriptConnector::new([
        ReadStep::line("220 smtp.example.test ready\r\n"),
        ReadStep::line("250-smtp.example.test\r\n"),
        ReadStep::line("250-AUTH LOGIN PLAIN CRAM-MD5\r\n"),
        ReadStep::line("250 SIZE 524288\r\n"),
        ReadStep::line("334 PDE4OTYuNjk3MTcwOTUyQHBvc3RvZmZpY2UucmVzdG9uLm1jaS5uZXQ+\r\n"),
        ReadStep::line("235 authenticated\r\n"),
        ReadStep::line("250 sender\r\n"),
        ReadStep::line("250 recipient\r\n"),
        ReadStep::line("250 copy\r\n"),
        ReadStep::line("354 data\r\n"),
        ReadStep::line("250 queued\r\n"),
    ]));
    let client = client_with(
        connector.clone(),
        "smtp.example.test",
        "tim@example.com",
        PASSWORD,
    );
    let result = client
        .send_gmail_message(
            "effect-cram",
            "to@example.com",
            "/etc/passwd\n<&",
            "Hello ✓",
            &["copy@example.com".to_owned()],
        )
        .await
        .expect("CRAM-MD5 fixture send");
    assert_eq!(result, json!({}));
    assert_eq!(
        connector.connects(),
        [("smtp.example.test".to_owned(), SMTP_PORT)]
    );
    let writes = connector.writes();
    assert_eq!(writes[0], b"EHLO [127.0.0.1]\r\n");
    assert_eq!(writes[1], b"AUTH CRAM-MD5\r\n");
    assert_eq!(
        writes[2],
        b"dGltQGV4YW1wbGUuY29tIGI5MTNhNjAyYzdlZGE3YTQ5NWI0ZTZlNzMzNGQzODkw\r\n"
    );
    assert!(String::from_utf8_lossy(&writes[3]).starts_with("MAIL FROM:<tim@example.com> SIZE="));
    assert_eq!(writes[4], b"RCPT TO:<to@example.com>\r\n");
    assert_eq!(writes[5], b"RCPT TO:<copy@example.com>\r\n");
    assert_eq!(writes[6], b"DATA\r\n");
    assert_eq!(writes.len(), 8);
    assert!(!writes.iter().any(|write| write.starts_with(b"STARTTLS")));

    let parts = decode_mime_parts(&writes[7]);
    assert_eq!(parts[0], b"/etc/passwd\n<&");
    let html = String::from_utf8(parts[1].clone()).expect("HTML alternative is UTF-8");
    assert!(html.contains("/etc/passwd<br>&lt;&amp;"));
    assert!(!html.contains("root:x:"));
}

#[tokio::test]
async fn auth_fallback_and_long_plain_credentials_follow_smtp_line_limits() {
    let connector = Arc::new(TranscriptConnector::new([
        ReadStep::line("220 ready\r\n"),
        ReadStep::line("250-server\r\n"),
        ReadStep::line("250 AUTH PLAIN LOGIN\r\n"),
        ReadStep::line("535 plain rejected\r\n"),
        ReadStep::line("334 password\r\n"),
        ReadStep::line("235 login accepted\r\n"),
        ReadStep::line("250 sender\r\n"),
        ReadStep::line("250 recipient\r\n"),
        ReadStep::line("354 data\r\n"),
        ReadStep::line("250 queued\r\n"),
    ]));
    let client = client_with(
        connector.clone(),
        "smtp.example.test",
        "sender@example.com",
        "short-password",
    );
    client
        .send_gmail_message("fallback", "to@example.com", "body", "subject", &[])
        .await
        .expect("PLAIN rejection falls back to LOGIN");
    let writes = connector.writes();
    assert!(writes[1].starts_with(b"AUTH PLAIN "));
    assert!(writes[2].starts_with(b"AUTH LOGIN "));
    assert_eq!(
        BASE64_STANDARD
            .decode(
                writes[3]
                    .strip_suffix(b"\r\n")
                    .expect("LOGIN response CRLF")
            )
            .expect("LOGIN password is base64"),
        b"short-password"
    );

    let long_password = "p".repeat(MAX_PASSWORD_BYTES);
    let connector = Arc::new(TranscriptConnector::new([
        ReadStep::line("220 ready\r\n"),
        ReadStep::line("250-server\r\n"),
        ReadStep::line("250 AUTH PLAIN\r\n"),
        ReadStep::line("334 continue\r\n"),
        ReadStep::line("235 accepted\r\n"),
        ReadStep::line("250 sender\r\n"),
        ReadStep::line("250 recipient\r\n"),
        ReadStep::line("354 data\r\n"),
        ReadStep::line("250 queued\r\n"),
    ]));
    let client = client_with(
        connector.clone(),
        "smtp.example.test",
        "sender@example.com",
        &long_password,
    );
    client
        .send_gmail_message("long-auth", "to@example.com", "body", "subject", &[])
        .await
        .expect("maximum bounded password uses challenged PLAIN");
    let writes = connector.writes();
    assert_eq!(writes[1], b"AUTH PLAIN\r\n");
    assert!(writes[2].len() <= 12 * 1_024);
    assert!(!String::from_utf8_lossy(&writes[2]).contains(&long_password));
}

#[tokio::test]
async fn mime_lines_and_multibyte_admission_are_bounded_at_exact_public_limits() {
    let cc = (0..100)
        .map(|index| format!("copy{index:03}@example.com"))
        .collect::<Vec<_>>();
    let connector = Arc::new(TranscriptConnector::new(plain_success_responses(
        cc.len() + 1,
    )));
    let client = client_with(connector.clone(), "smtp.example.test", "sender", PASSWORD);
    let message = "😀".repeat(MAX_MESSAGE_CHARS);
    let subject = "😀".repeat(MAX_SUBJECT_CHARS);
    assert_eq!(message.len(), MAX_MESSAGE_BYTES);
    assert!(subject.len() <= MAX_SUBJECT_BYTES);
    client
        .send_gmail_message("unicode-bound", "to@example.com", &message, &subject, &cc)
        .await
        .expect("schema-valid multibyte boundaries execute");
    let writes = connector.writes();
    let data_index = writes
        .iter()
        .position(|write| write == b"DATA\r\n")
        .expect("DATA command");
    let mime = writes[data_index + 1]
        .strip_suffix(b".\r\n")
        .expect("SMTP data terminator");
    assert!(mime.len() <= 512 * 1_024);
    assert!(
        mime.split(|byte| *byte == b'\n')
            .all(|line| { line.strip_suffix(b"\r").unwrap_or(line).len() <= MAX_SUBJECT_BYTES })
    );
    let headers = String::from_utf8_lossy(mime);
    assert!(headers.contains("Subject: =?UTF-8?B?"));
    assert!(headers.contains("Cc: <copy000@example.com>,\r\n <copy001@example.com>"));

    let rejected = Arc::new(TranscriptConnector::new([]));
    let client = client_with(rejected.clone(), "smtp.example.test", "sender", PASSWORD);
    let error = client
        .send_gmail_message(
            "unicode-over",
            "to@example.com",
            &"😀".repeat(MAX_MESSAGE_CHARS + 1),
            "subject",
            &[],
        )
        .await
        .expect_err("over-limit Unicode message fails before connection");
    assert_eq!(error.code(), YagmailClientErrorCode::ResourceExhausted);
    assert!(rejected.connects().is_empty());
}

#[tokio::test]
async fn partial_and_total_recipient_rejection_have_bounded_known_results() {
    let connector = Arc::new(TranscriptConnector::new([
        ReadStep::line("220 ready\r\n"),
        ReadStep::line("250-server\r\n"),
        ReadStep::line("250 AUTH PLAIN\r\n"),
        ReadStep::line("235 auth\r\n"),
        ReadStep::line("250 sender\r\n"),
        ReadStep::line("250 receiver\r\n"),
        ReadStep::line("550 secret-provider-text\r\n"),
        ReadStep::line("354 data\r\n"),
        ReadStep::line("250 queued\r\n"),
    ]));
    let client = client_with(
        connector,
        "smtp.example.test",
        "sender@example.com",
        PASSWORD,
    );
    assert_eq!(
        client
            .send_gmail_message(
                "partial",
                "to@example.com",
                "body",
                "subject",
                &["refused@example.com".to_owned()],
            )
            .await
            .expect("partial acceptance is a confirmed send"),
        json!({
            "sent":true,
            "refused_recipients":[{"recipient":"refused@example.com","smtp_code":550}]
        })
    );

    let connector = Arc::new(TranscriptConnector::new([
        ReadStep::line("220 ready\r\n"),
        ReadStep::line("250-server\r\n"),
        ReadStep::line("250 AUTH PLAIN\r\n"),
        ReadStep::line("235 auth\r\n"),
        ReadStep::line("250 sender\r\n"),
        ReadStep::line("550 private rejection detail\r\n"),
    ]));
    let client = client_with(
        connector.clone(),
        "smtp.example.test",
        "sender@example.com",
        PASSWORD,
    );
    let error = client
        .send_gmail_message("rejected", "to@example.com", "body", "subject", &[])
        .await
        .expect_err("all recipients rejected");
    assert_eq!(error.code(), YagmailClientErrorCode::RecipientsRejected);
    assert!(!format!("{error:?} {error}").contains("private rejection detail"));
    let writes = connector.writes();
    assert_eq!(writes.last().expect("RSET write"), b"RSET\r\n");
    assert!(!writes.iter().any(|write| write == b"DATA\r\n"));
}

#[tokio::test]
async fn failures_are_phase_aware_and_never_retry_the_effect() {
    let connector = Arc::new(TranscriptConnector::failing(SmtpChannelErrorCode::Timeout));
    let client = client_with(
        connector.clone(),
        "smtp.example.test",
        "sender@example.com",
        PASSWORD,
    );
    let error = client
        .send_gmail_message("pre-data", "to@example.com", "body", "subject", &[])
        .await
        .expect_err("connect timeout fails");
    assert_eq!(error.code(), YagmailClientErrorCode::Timeout);
    assert!(error.retryable());
    assert_eq!(connector.connects().len(), 1);
    assert!(connector.writes().is_empty());

    let mut responses = plain_success_responses(1);
    let final_response = responses.pop().expect("final accepted response");
    assert!(matches!(final_response, ReadStep::Line(_)));
    responses.push(ReadStep::Error(SmtpChannelErrorCode::Unavailable));
    let connector = Arc::new(TranscriptConnector::new(responses));
    let client = client_with(
        connector.clone(),
        "smtp.example.test",
        "sender@example.com",
        PASSWORD,
    );
    let error = client
        .send_gmail_message("post-data", "to@example.com", "body", "subject", &[])
        .await
        .expect_err("post-DATA disconnect is ambiguous");
    assert_eq!(error.code(), YagmailClientErrorCode::UnknownOutcome);
    assert!(!error.retryable());
    assert_eq!(connector.connects().len(), 1);
    let writes = connector.writes();
    assert_eq!(
        writes.iter().filter(|write| *write == b"DATA\r\n").count(),
        1
    );
    assert_eq!(
        writes
            .iter()
            .filter(|write| write.ends_with(b".\r\n") && write.len() > 3)
            .count(),
        1
    );
}

#[tokio::test]
async fn confirmed_250_returns_without_a_post_accept_channel_call() {
    let mut responses = plain_success_responses(1);
    let _ = responses.pop().expect("final accepted fixture response");
    responses.push(ReadStep::Accepted(b"250 2.0.0 queued\r\n".to_vec()));
    let connector = Arc::new(TranscriptConnector::stalling_after_accept(responses));
    let client = client_with(
        connector.clone(),
        "smtp.example.test",
        "sender@example.com",
        PASSWORD,
    );

    let result = tokio::time::timeout(
        std::time::Duration::from_millis(100),
        client.send_gmail_message(
            "accepted-without-cleanup",
            "to@example.com",
            "body",
            "subject",
            &[],
        ),
    )
    .await
    .expect("known acceptance must not await QUIT, TLS shutdown, or another write")
    .expect("accepted send result");

    assert_eq!(result, json!({}));
    let writes = connector.writes();
    assert_eq!(writes.len(), 6);
    assert!(writes.last().is_some_and(|write| write.ends_with(b".\r\n")));
    assert!(!writes.iter().any(|write| write == b"QUIT\r\n"));
}

#[tokio::test]
async fn per_toolkit_credentials_and_message_ids_are_isolated_and_replay_stable() {
    let first = Arc::new(TranscriptConnector::new(plain_success_responses(1)));
    let second = Arc::new(TranscriptConnector::new(plain_success_responses(1)));
    let first_client = client_with(first.clone(), "smtp.one.example", "first", "first-password");
    let second_client = client_with(
        second.clone(),
        "smtp.two.example",
        "second@example.com",
        "second-password",
    );
    first_client
        .send_gmail_message("same-effect", "to@example.com", "body", "subject", &[])
        .await
        .expect("first tenant send");
    second_client
        .send_gmail_message("same-effect", "to@example.com", "body", "subject", &[])
        .await
        .expect("second tenant send");
    assert_eq!(
        first.connects(),
        [("smtp.one.example".to_owned(), SMTP_PORT)]
    );
    assert_eq!(
        second.connects(),
        [("smtp.two.example".to_owned(), SMTP_PORT)]
    );
    let first_writes = first.writes();
    let second_writes = second.writes();
    assert!(String::from_utf8_lossy(&first_writes[2]).contains("first@gmail.com"));
    assert!(String::from_utf8_lossy(&second_writes[2]).contains("second@example.com"));
    assert!(!String::from_utf8_lossy(&first_writes.concat()).contains("second-password"));
    assert!(!String::from_utf8_lossy(&second_writes.concat()).contains("first-password"));
    assert_ne!(message_id(&first_writes), message_id(&second_writes));

    let replay = Arc::new(TranscriptConnector::new(plain_success_responses(1)));
    let replay_client = client_with(
        replay.clone(),
        "smtp.one.example",
        "first",
        "first-password",
    );
    replay_client
        .send_gmail_message("same-effect", "to@example.com", "body", "subject", &[])
        .await
        .expect("same logical replay fixture");
    assert_eq!(message_id(&first_writes), message_id(&replay.writes()));

    let changed = Arc::new(TranscriptConnector::new(plain_success_responses(1)));
    let changed_client = client_with(
        changed.clone(),
        "smtp.one.example",
        "first",
        "first-password",
    );
    changed_client
        .send_gmail_message("same-effect", "to@example.com", "changed", "subject", &[])
        .await
        .expect("changed intent fixture");
    assert_ne!(message_id(&first_writes), message_id(&changed.writes()));
}

#[tokio::test]
async fn invalid_arguments_fail_before_api_or_network() {
    let api = Arc::new(FixtureApi::new());
    let api_trait: Arc<dyn YagmailApi> = api.clone();
    let toolset = test_build_with_api("mail", &[], &policy(&[]), &api_trait)
        .expect("Yagmail fixture toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tool = toolset
        .tools(readonly)
        .await
        .expect("Yagmail tools")
        .pop()
        .expect("send tool");
    for invalid in [
        json!({}),
        json!({"receiver":"not-an-address","message":"m","subject":"s"}),
        json!({"receiver":"to@example.com","message":"m\0","subject":"s"}),
        json!({"receiver":"to@example.com","message":"m","subject":"bad\nsubject"}),
        json!({"receiver":"to@example.com","message":"m","subject":"s","cc":["bad"]}),
        json!({"receiver":"to@example.com","message":"m","subject":"s","extra":true}),
        json!({
            "receiver":"to@example.com",
            "message":"x".repeat(MAX_MESSAGE_CHARS + 1),
            "subject":"s"
        }),
    ] {
        assert!(tool.execute(context(), invalid).await.is_err());
    }
    assert!(api.calls().is_empty());
}

#[test]
fn family_remains_capability_disabled_and_has_no_forbidden_production_macros() {
    let registry = include_str!("families/mod.rs");
    let snapshot = include_str!("snapshot.rs");
    assert!(registry.contains("mod yagmail"));
    assert!(!snapshot.contains("build_yagmail_toolset"));

    for source in [
        include_str!("families/yagmail/mod.rs"),
        include_str!("families/yagmail/config.rs"),
        include_str!("families/yagmail/client.rs"),
        include_str!("families/yagmail/tools.rs"),
    ] {
        for forbidden in ["panic!(", "unwrap(", "expect(", "todo!(", "unimplemented!("] {
            assert!(
                !source.contains(forbidden),
                "forbidden production macro: {forbidden}"
            );
        }
    }
}

fn decode_mime_parts(data: &[u8]) -> Vec<Vec<u8>> {
    let mime = data.strip_suffix(b".\r\n").expect("SMTP terminator");
    let marker = b"Content-Transfer-Encoding: base64\r\n\r\n";
    let mut search = mime;
    let mut parts = Vec::new();
    while let Some(start) = find_bytes(search, marker) {
        let payload = &search[start + marker.len()..];
        let end = find_bytes(payload, b"\r\n--").expect("MIME boundary after payload");
        let encoded = payload[..end]
            .iter()
            .copied()
            .filter(|byte| !matches!(byte, b'\r' | b'\n'))
            .collect::<Vec<_>>();
        parts.push(
            BASE64_STANDARD
                .decode(encoded)
                .expect("MIME payload base64"),
        );
        search = &payload[end + 2..];
    }
    parts
}

fn message_id(writes: &[Vec<u8>]) -> String {
    let data_index = writes
        .iter()
        .position(|write| write == b"DATA\r\n")
        .expect("DATA command");
    let data = String::from_utf8_lossy(&writes[data_index + 1]);
    data.lines()
        .find_map(|line| line.strip_prefix("Message-ID: "))
        .expect("Message-ID header")
        .to_owned()
}

fn find_bytes(haystack: &[u8], needle: &[u8]) -> Option<usize> {
    if needle.is_empty() {
        return Some(0);
    }
    haystack
        .windows(needle.len())
        .position(|window| window == needle)
}

#[tokio::test]
async fn every_tool_keeps_the_sdk_contract() {
    let api: Arc<dyn YagmailApi> = Arc::new(FixtureApi::new());
    let toolset =
        test_build_with_api("gate", &[], &policy(&[]), &api).expect("complete yagmail toolset");
    let readonly: Arc<dyn ReadonlyContext> = context();
    let tools = toolset.tools(readonly).await.expect("yagmail tools");
    super::sdk_conformance::assert_sdk_conformance("yagmail", &tools);
}
