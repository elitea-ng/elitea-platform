//! The operator stream: one JSON object per tracing event, nothing for span
//! open or close (span timing belongs to OpenTelemetry).
//!
//! Keys, in order: `time` (RFC 3339, UTC, microseconds), `level`, `msg` (the
//! event message), `target`, `span` (the innermost span's name, when inside
//! one), the event's fields, then the fields of every span in scope, inner
//! span first. A key is written once: an event field wins over a span field,
//! an inner span's over an outer span's. A `cause` field that holds a JSON
//! array of strings (what `fault!` writes) is embedded as that array.

use std::fmt;
use std::io::Write as _;
use std::time::{SystemTime, UNIX_EPOCH};

use serde_json::Value;
use tracing_core::field::{Field, Visit};
use tracing_core::span::{Attributes, Id, Record};
use tracing_core::{Event, Subscriber};
use tracing_subscriber::fmt::MakeWriter;
use tracing_subscriber::layer::{Context, Layer};
use tracing_subscriber::registry::LookupSpan;

/// The operator stream layer over a writer, e.g. `std::io::stdout`.
pub struct OperatorJsonLayer<W> {
    make_writer: W,
}

impl<W> OperatorJsonLayer<W> {
    #[must_use]
    pub const fn new(make_writer: W) -> Self {
        Self { make_writer }
    }
}

/// Fields recorded on a span, in the order they were declared.
struct SpanFields(Fields);

#[derive(Default)]
struct Fields(Vec<(&'static str, Value)>);

impl Fields {
    fn set(&mut self, name: &'static str, value: Value) {
        match self.0.iter_mut().find(|(key, _)| *key == name) {
            Some((_, slot)) => *slot = value,
            None => self.0.push((name, value)),
        }
    }

    fn contains(&self, name: &str) -> bool {
        self.0.iter().any(|(key, _)| *key == name)
    }
}

impl Visit for Fields {
    fn record_debug(&mut self, field: &Field, value: &dyn fmt::Debug) {
        self.set(field.name(), Value::String(format!("{value:?}")));
    }

    fn record_str(&mut self, field: &Field, value: &str) {
        self.set(field.name(), Value::String(value.to_owned()));
    }

    fn record_i64(&mut self, field: &Field, value: i64) {
        self.set(field.name(), Value::from(value));
    }

    fn record_u64(&mut self, field: &Field, value: u64) {
        self.set(field.name(), Value::from(value));
    }

    fn record_i128(&mut self, field: &Field, value: i128) {
        self.set(field.name(), Value::String(value.to_string()));
    }

    fn record_u128(&mut self, field: &Field, value: u128) {
        self.set(field.name(), Value::String(value.to_string()));
    }

    fn record_f64(&mut self, field: &Field, value: f64) {
        let value = serde_json::Number::from_f64(value)
            .map_or_else(|| Value::String(value.to_string()), Value::Number);
        self.set(field.name(), value);
    }

    fn record_bool(&mut self, field: &Field, value: bool) {
        self.set(field.name(), Value::Bool(value));
    }
}

impl<S, W> Layer<S> for OperatorJsonLayer<W>
where
    S: Subscriber + for<'span> LookupSpan<'span>,
    W: for<'writer> MakeWriter<'writer> + 'static,
{
    fn on_new_span(&self, attributes: &Attributes<'_>, id: &Id, context: Context<'_, S>) {
        let _mark = crate::OperatorStreamMark::enter();
        let Some(span) = context.span(id) else {
            return;
        };
        let mut fields = Fields::default();
        attributes.record(&mut fields);
        span.extensions_mut().replace(SpanFields(fields));
    }

    fn on_record(&self, id: &Id, values: &Record<'_>, context: Context<'_, S>) {
        let _mark = crate::OperatorStreamMark::enter();
        let Some(span) = context.span(id) else {
            return;
        };
        // Format outside the extensions lock: a field's Debug may panic.
        let mut recorded = Fields::default();
        values.record(&mut recorded);
        let mut extensions = span.extensions_mut();
        if let Some(SpanFields(fields)) = extensions.get_mut::<SpanFields>() {
            for (name, value) in recorded.0 {
                fields.set(name, value);
            }
        }
    }

    fn on_event(&self, event: &Event<'_>, context: Context<'_, S>) {
        let _mark = crate::OperatorStreamMark::enter();
        let metadata = event.metadata();
        let mut fields = Fields::default();
        event.record(&mut fields);
        let message = fields
            .0
            .iter()
            .position(|(key, _)| *key == "message")
            .map(|index| fields.0.remove(index).1);
        let mut span_name = None;
        if let Some(scope) = context.event_scope(event) {
            for span in scope {
                span_name.get_or_insert(span.name());
                let extensions = span.extensions();
                let Some(SpanFields(span_fields)) = extensions.get::<SpanFields>() else {
                    continue;
                };
                for (key, value) in &span_fields.0 {
                    if !fields.contains(key) {
                        fields.0.push((*key, value.clone()));
                    }
                }
            }
        }
        let mut line = Vec::with_capacity(256);
        line.extend_from_slice(b"{\"time\":\"");
        line.extend_from_slice(rfc3339_utc(SystemTime::now()).as_bytes());
        line.extend_from_slice(b"\",\"level\":\"");
        line.extend_from_slice(metadata.level().as_str().as_bytes());
        line.extend_from_slice(b"\",\"msg\":");
        let message = match message {
            Some(Value::String(text)) => text,
            Some(other) => other.to_string(),
            None => String::new(),
        };
        push_json(&mut line, &Value::String(message));
        line.extend_from_slice(b",\"target\":");
        push_json(&mut line, &Value::String(metadata.target().to_owned()));
        if let Some(name) = span_name {
            line.extend_from_slice(b",\"span\":");
            push_json(&mut line, &Value::String(name.to_owned()));
        }
        for (key, value) in &fields.0 {
            if matches!(*key, "time" | "level" | "msg" | "target" | "span") {
                continue;
            }
            line.push(b',');
            push_json(&mut line, &Value::String((*key).to_owned()));
            line.push(b':');
            if *key == "cause"
                && let Some(array) = cause_array(value)
            {
                push_json(&mut line, &array);
            } else {
                push_json(&mut line, value);
            }
        }
        line.extend_from_slice(b"}\n");
        let mut writer = self.make_writer.make_writer_for(metadata);
        let _ignored = writer.write_all(&line);
    }
}

fn push_json(line: &mut Vec<u8>, value: &Value) {
    if serde_json::to_writer(&mut *line, value).is_err() {
        line.extend_from_slice(b"null");
    }
}

fn cause_array(value: &Value) -> Option<Value> {
    let Value::String(text) = value else {
        return None;
    };
    if !text.starts_with('[') {
        return None;
    }
    let parsed: Value = serde_json::from_str(text).ok()?;
    parsed
        .as_array()?
        .iter()
        .all(Value::is_string)
        .then_some(parsed)
}

/// `YYYY-MM-DDTHH:MM:SS.ffffffZ`.
pub(crate) fn rfc3339_utc(time: SystemTime) -> String {
    let since = time.duration_since(UNIX_EPOCH).unwrap_or_default();
    let seconds = since.as_secs();
    let micros = since.subsec_micros();
    let days = i64::try_from(seconds / 86_400).unwrap_or(0);
    let rest = seconds % 86_400;
    let (year, month, day) = civil_from_days(days);
    format!(
        "{year:04}-{month:02}-{day:02}T{:02}:{:02}:{:02}.{micros:06}Z",
        rest / 3600,
        (rest % 3600) / 60,
        rest % 60
    )
}

/// Howard Hinnant's `civil_from_days`: days since 1970-01-01 to a date.
fn civil_from_days(days: i64) -> (i64, u32, u32) {
    let z = days + 719_468;
    let era = z.div_euclid(146_097);
    let day_of_era = z.rem_euclid(146_097);
    let year_of_era =
        (day_of_era - day_of_era / 1460 + day_of_era / 36_524 - day_of_era / 146_096) / 365;
    let day_of_year = day_of_era - (365 * year_of_era + year_of_era / 4 - year_of_era / 100);
    let shifted_month = (5 * day_of_year + 2) / 153;
    let day = day_of_year - (153 * shifted_month + 2) / 5 + 1;
    let month = if shifted_month < 10 {
        shifted_month + 3
    } else {
        shifted_month - 9
    };
    let year = year_of_era + era * 400 + i64::from(month <= 2);
    (
        year,
        u32::try_from(month).unwrap_or(1),
        u32::try_from(day).unwrap_or(1),
    )
}
