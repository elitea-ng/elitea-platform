//! A bounded Server-Sent Events splitter for model streams.
//!
//! What the gateway relays comes from a model provider; a stream that never
//! ends a line, or one event that grows without bound, must fail the call
//! rather than grow the caller's memory. Every buffer here has a cap, the
//! stream as a whole has one, and so does the number of events.
//!
//! Bytes go in with [`SseSplitter::push`] and events come out of
//! [`SseSplitter::next_event`]; [`SseSplitter::finish`] marks the end of
//! the input, after which a last event without its blank line is still
//! delivered (some servers close right after `data: [DONE]`). Drain
//! `next_event` after every push; the `*_into` helpers do both.
//!
//! # Dialects
//!
//! | Rule | [`SseDialect::Lenient`] | [`SseDialect::Strict`] |
//! | --- | --- | --- |
//! | Line end | `\n`, `\r\n`, lone `\r` | `\n`; one trailing `\r` is dropped |
//! | Byte order mark | skipped at the start of the stream | a malformed line |
//! | Line cap | [`SseLimits::max_line_bytes`], also on a line not yet ended | [`SseLimits::max_line_bytes`] on an ended line |
//! | Fields | `data`, `event`; `id`, `retry` and unknown fields ignored; a line without `:` is a field with an empty value | only `data:` and `event:`; anything else is malformed |
//! | `event:` | any number, the last wins | once per event, before its data, non-empty, at most [`SseLimits::max_event_bytes`] |
//! | Event cap | the joined data | the joined data, plus one, plus the `event:` value |
//! | A blank line with only `event:` | drops the name | malformed |
//! | Text | `event:` values and data must be UTF-8 | bytes |
//! | `push` after `finish` | accepted | malformed |
//!
//! Both: `:` starts a comment; one space after the `:` is dropped; the
//! `data` lines of one event join with `\n`; a blank line ends an event, and
//! an event with no `data` line is not delivered.
//!
//! [`SseOptions::reject_event_types`] refuses a delivered event that had an
//! `event:` line (an OpenAI-style stream never names its events).

/// The splitter's caps.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub struct SseLimits {
    /// One line, terminator excluded.
    pub max_line_bytes: usize,
    /// One event's joined data (see the dialect table for what is charged).
    pub max_event_bytes: usize,
    /// The whole stream.
    pub max_stream_bytes: usize,
    /// Events in the stream. A 64k-token answer is about 64k events.
    pub max_events: usize,
}

impl Default for SseLimits {
    /// The engines' caps: line and event 1 MiB, stream 64 MiB, 200 000
    /// events. The worker passes its own, from its deployment policy.
    fn default() -> Self {
        Self {
            max_line_bytes: 1024 * 1024,
            max_event_bytes: 1024 * 1024,
            max_stream_bytes: 64 * 1024 * 1024,
            max_events: 200_000,
        }
    }
}

/// How strictly the grammar is read (see the module table).
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub enum SseDialect {
    /// The engines' model client.
    #[default]
    Lenient,
    /// The agent worker.
    Strict,
}

/// A splitter's configuration.
#[derive(Debug, Clone, Copy, PartialEq, Eq, Default)]
pub struct SseOptions {
    pub limits: SseLimits,
    pub dialect: SseDialect,
    /// Refuse an event that had an `event:` line, when it is delivered.
    pub reject_event_types: bool,
}

/// One event.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SseEvent {
    /// The `event:` value; `None` for the default `message`.
    pub event_type: Option<Vec<u8>>,
    /// The joined `data` lines.
    pub data: Vec<u8>,
}

/// Why a stream was refused.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
pub enum SseError {
    LineTooLong,
    EventTooLarge,
    StreamTooLarge,
    TooManyEvents,
    NotUtf8,
    /// A line or an event outside the dialect's grammar.
    Malformed,
    /// An event with an `event:` line under
    /// [`SseOptions::reject_event_types`].
    UnexpectedEventType,
}

impl std::fmt::Display for SseError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::LineTooLong => "the stream sent a line above the size cap",
            Self::EventTooLarge => "the stream sent an event above the size cap",
            Self::StreamTooLarge => "the stream exceeded its total size cap",
            Self::TooManyEvents => "the stream exceeded its event count cap",
            Self::NotUtf8 => "the stream sent bytes that are not UTF-8",
            Self::Malformed => "the stream is malformed",
            Self::UnexpectedEventType => "the stream sent a named event",
        })
    }
}

impl std::error::Error for SseError {}

/// The UTF-8 byte order mark.
const BOM: &[u8] = b"\xEF\xBB\xBF";

/// Consumed bytes are dropped from the buffer once this many accumulate
/// (or at once when the buffer is fully consumed).
const COMPACT_BYTES: usize = 64 * 1024;

/// The incremental splitter.
#[derive(Debug)]
pub struct SseSplitter {
    options: SseOptions,
    buf: Vec<u8>,
    /// The start of the first line not yet consumed.
    cursor: usize,
    /// Bytes before this offset were searched for a line end already, so a
    /// long line fed in small chunks is scanned once.
    scanned: usize,
    event_type: Option<Vec<u8>>,
    data: Vec<u8>,
    has_data: bool,
    total: usize,
    events: usize,
    /// Lenient: the last line ended in `\r` at the end of the input, so a
    /// `\n` that starts the next push is the second half of that `\r\n`.
    skip_lf: bool,
    phase: Phase,
}

/// Where the input is.
#[derive(Debug, Clone, Copy, PartialEq, Eq)]
enum Phase {
    /// Lenient: the start of the stream is not yet checked for a byte order
    /// mark.
    AwaitingBom,
    Open,
    /// [`SseSplitter::finish`] was called.
    Finished,
}

impl Default for SseSplitter {
    fn default() -> Self {
        Self::new(SseOptions::default())
    }
}

impl SseSplitter {
    #[must_use]
    pub fn new(options: SseOptions) -> Self {
        Self {
            options,
            buf: Vec::new(),
            cursor: 0,
            scanned: 0,
            event_type: None,
            data: Vec::new(),
            has_data: false,
            total: 0,
            events: 0,
            skip_lf: false,
            phase: match options.dialect {
                SseDialect::Lenient => Phase::AwaitingBom,
                SseDialect::Strict => Phase::Open,
            },
        }
    }

    fn strict(&self) -> bool {
        self.options.dialect == SseDialect::Strict
    }

    /// Add bytes. Complete events become available to
    /// [`SseSplitter::next_event`].
    ///
    /// # Errors
    ///
    /// [`SseError::StreamTooLarge`] past the stream cap; strict:
    /// [`SseError::Malformed`] after [`SseSplitter::finish`].
    pub fn push(&mut self, chunk: &[u8]) -> Result<(), SseError> {
        if self.phase == Phase::Finished && self.strict() {
            return Err(SseError::Malformed);
        }
        self.total = self
            .total
            .checked_add(chunk.len())
            .ok_or(SseError::StreamTooLarge)?;
        if self.total > self.options.limits.max_stream_bytes {
            return Err(SseError::StreamTooLarge);
        }
        let mut chunk = chunk;
        if self.skip_lf && !chunk.is_empty() {
            self.skip_lf = false;
            chunk = chunk.strip_prefix(b"\n").unwrap_or(chunk);
        }
        self.compact();
        self.buf.extend_from_slice(chunk);
        if self.phase == Phase::AwaitingBom {
            let pending = &self.buf[self.cursor..];
            if pending.len() < BOM.len() && BOM.starts_with(pending) {
                // Not enough bytes to tell yet.
                return Ok(());
            }
            self.phase = Phase::Open;
            if pending.starts_with(BOM) {
                self.cursor += BOM.len();
                self.scanned = self.scanned.max(self.cursor);
            }
        }
        Ok(())
    }

    /// The end of the input: a last line without its end, and a last event
    /// without its blank line, are delivered by the next calls of
    /// [`SseSplitter::next_event`]. Call it once every pushed event was
    /// drained.
    pub fn finish(&mut self) {
        if self.phase == Phase::Finished {
            return;
        }
        self.phase = Phase::Finished;
        if self.buf.len() > self.cursor && !self.buf[self.cursor..].ends_with(b"\n") {
            self.buf.push(b'\n');
        }
        self.buf.push(b'\n');
    }

    /// The next complete event, or `None` until more input arrives.
    ///
    /// # Errors
    ///
    /// The first cap or grammar rule the input breaks. The splitter is not
    /// meant to be used after an error.
    pub fn next_event(&mut self) -> Result<Option<SseEvent>, SseError> {
        if self.phase == Phase::AwaitingBom {
            return Ok(None);
        }
        loop {
            let Some((start, end)) = self.next_line() else {
                if !self.strict()
                    && self.buf.len() - self.cursor > self.options.limits.max_line_bytes
                {
                    return Err(SseError::LineTooLong);
                }
                return Ok(None);
            };
            if end - start > self.options.limits.max_line_bytes {
                return Err(SseError::LineTooLong);
            }
            let line = self.buf[start..end].to_vec();
            if let Some(event) = self.line(&line)? {
                return Ok(Some(event));
            }
        }
    }

    /// [`SseSplitter::push`], then every event that completed, appended to
    /// `out` (also those before an error).
    ///
    /// # Errors
    ///
    /// As [`SseSplitter::push`] and [`SseSplitter::next_event`].
    pub fn push_into(&mut self, chunk: &[u8], out: &mut Vec<SseEvent>) -> Result<(), SseError> {
        self.push(chunk)?;
        self.drain_into(out)
    }

    /// [`SseSplitter::finish`], then the remaining events, appended to
    /// `out`.
    ///
    /// # Errors
    ///
    /// As [`SseSplitter::next_event`].
    pub fn finish_into(&mut self, out: &mut Vec<SseEvent>) -> Result<(), SseError> {
        self.finish();
        self.drain_into(out)
    }

    fn drain_into(&mut self, out: &mut Vec<SseEvent>) -> Result<(), SseError> {
        while let Some(event) = self.next_event()? {
            out.push(event);
        }
        Ok(())
    }

    fn compact(&mut self) {
        if self.cursor == self.buf.len() {
            self.buf.clear();
            self.cursor = 0;
            self.scanned = 0;
        } else if self.cursor >= COMPACT_BYTES {
            self.buf.drain(..self.cursor);
            self.scanned -= self.cursor;
            self.cursor = 0;
        }
    }

    /// The next ended line, as a range of `buf` without its terminator.
    fn next_line(&mut self) -> Option<(usize, usize)> {
        let strict = self.strict();
        let from = self.scanned.max(self.cursor);
        let Some(offset) = self.buf[from..]
            .iter()
            .position(|b| *b == b'\n' || (!strict && *b == b'\r'))
        else {
            self.scanned = self.buf.len();
            return None;
        };
        let start = self.cursor;
        let end = from + offset;
        let mut next = end + 1;
        let mut line_end = end;
        if strict {
            if line_end > start && self.buf[line_end - 1] == b'\r' {
                line_end -= 1;
            }
        } else if self.buf[end] == b'\r' {
            match self.buf.get(next) {
                Some(b'\n') => next += 1,
                Some(_) => {}
                None => self.skip_lf = true,
            }
        }
        self.cursor = next;
        self.scanned = next;
        Some((start, line_end))
    }

    fn line(&mut self, line: &[u8]) -> Result<Option<SseEvent>, SseError> {
        if line.is_empty() {
            return self.dispatch();
        }
        if line[0] == b':' {
            return Ok(None);
        }
        let colon = line.iter().position(|b| *b == b':');
        let (field, value) = match colon {
            Some(colon) => {
                let value = &line[colon + 1..];
                (&line[..colon], value.strip_prefix(b" ").unwrap_or(value))
            }
            None => (line, &[][..]),
        };
        if self.strict() {
            if colon.is_none() {
                return Err(SseError::Malformed);
            }
            match field {
                b"event" => {
                    if self.event_type.is_some()
                        || self.has_data
                        || value.is_empty()
                        || value.len() > self.options.limits.max_event_bytes
                    {
                        return Err(SseError::Malformed);
                    }
                    self.event_type = Some(value.to_vec());
                }
                b"data" => self.append_data(value)?,
                _ => return Err(SseError::Malformed),
            }
        } else {
            match field {
                b"data" => self.append_data(value)?,
                b"event" => {
                    std::str::from_utf8(value).map_err(|_| SseError::NotUtf8)?;
                    self.event_type = Some(value.to_vec());
                }
                _ => {}
            }
        }
        Ok(None)
    }

    fn append_data(&mut self, value: &[u8]) -> Result<(), SseError> {
        let joined = self
            .data
            .len()
            .checked_add(usize::from(self.has_data))
            .and_then(|length| length.checked_add(value.len()))
            .ok_or(SseError::EventTooLarge)?;
        let charged = if self.strict() {
            joined
                .checked_add(1)
                .and_then(|length| length.checked_add(self.event_type.as_ref().map_or(0, Vec::len)))
                .ok_or(SseError::EventTooLarge)?
        } else {
            joined
        };
        if charged > self.options.limits.max_event_bytes {
            return Err(SseError::EventTooLarge);
        }
        if self.has_data {
            self.data.push(b'\n');
        }
        self.data.extend_from_slice(value);
        self.has_data = true;
        Ok(())
    }

    fn dispatch(&mut self) -> Result<Option<SseEvent>, SseError> {
        if !self.has_data {
            if self.strict() && self.event_type.is_some() {
                return Err(SseError::Malformed);
            }
            self.event_type = None;
            return Ok(None);
        }
        self.has_data = false;
        let data = std::mem::take(&mut self.data);
        let event_type = self.event_type.take();
        if !self.strict() && std::str::from_utf8(&data).is_err() {
            return Err(SseError::NotUtf8);
        }
        self.events = self.events.checked_add(1).ok_or(SseError::TooManyEvents)?;
        if self.events > self.options.limits.max_events {
            return Err(SseError::TooManyEvents);
        }
        if self.options.reject_event_types && event_type.is_some() {
            return Err(SseError::UnexpectedEventType);
        }
        Ok(Some(SseEvent { event_type, data }))
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn options(dialect: SseDialect, limits: SseLimits) -> SseOptions {
        SseOptions {
            limits,
            dialect,
            reject_event_types: false,
        }
    }

    fn split(chunks: &[&[u8]], options: SseOptions) -> Result<Vec<SseEvent>, SseError> {
        let mut splitter = SseSplitter::new(options);
        let mut out = Vec::new();
        for chunk in chunks {
            splitter.push_into(chunk, &mut out)?;
        }
        splitter.finish_into(&mut out)?;
        Ok(out)
    }

    fn lenient(chunks: &[&[u8]]) -> Result<Vec<SseEvent>, SseError> {
        split(chunks, options(SseDialect::Lenient, SseLimits::default()))
    }

    fn strict(chunks: &[&[u8]]) -> Result<Vec<SseEvent>, SseError> {
        split(chunks, options(SseDialect::Strict, SseLimits::default()))
    }

    fn data(events: &[SseEvent]) -> Vec<String> {
        events
            .iter()
            .map(|e| String::from_utf8_lossy(&e.data).into_owned())
            .collect()
    }

    fn joined(result: Result<Vec<SseEvent>, SseError>) -> Result<String, SseError> {
        result.map(|events| data(&events).join("|"))
    }

    #[test]
    fn lenient_events_survive_any_chunking() {
        let stream = b": keep-alive\r\ndata: {\"a\":1}\r\n\r\ndata: x\ndata: y\n\nevent: error\ndata: boom\n\nid: 7\nretry: 1\ndata: [DONE]\n\n";
        for at in 0..stream.len() {
            let (a, b) = stream.split_at(at);
            let Ok(events) = lenient(&[a, b]) else {
                panic!("split at {at} failed");
            };
            assert_eq!(data(&events), ["{\"a\":1}", "x\ny", "boom", "[DONE]"]);
            assert_eq!(events[2].event_type.as_deref(), Some(&b"error"[..]));
            assert_eq!(events[3].event_type, None);
        }
    }

    #[test]
    fn strict_events_survive_any_chunking() {
        let stream = b": keep-alive\r\ndata: {\"a\":1}\r\n\r\ndata:x\ndata: y\n\nevent: message_start\ndata: {}\n\ndata: [DONE]\n\n";
        for at in 0..stream.len() {
            let (a, b) = stream.split_at(at);
            let Ok(events) = strict(&[a, b]) else {
                panic!("split at {at} failed");
            };
            assert_eq!(data(&events), ["{\"a\":1}", "x\ny", "{}", "[DONE]"]);
            assert_eq!(events[2].event_type.as_deref(), Some(&b"message_start"[..]));
        }
    }

    #[test]
    fn a_last_event_without_its_blank_line_is_delivered() {
        assert_eq!(joined(lenient(&[b"data: [DONE]"])), Ok("[DONE]".to_owned()));
        assert_eq!(joined(strict(&[b"data: [DONE]"])), Ok("[DONE]".to_owned()));
        assert_eq!(
            joined(strict(&[b"data: [DONE]\r"])),
            Ok("[DONE]".to_owned())
        );
        assert_eq!(joined(strict(&[b"data: a\n"])), Ok("a".to_owned()));
        assert_eq!(joined(lenient(&[b"data: a\r"])), Ok("a".to_owned()));
    }

    #[test]
    fn lenient_caps_hold() {
        let small = options(
            SseDialect::Lenient,
            SseLimits {
                max_line_bytes: 16,
                max_event_bytes: 15,
                max_stream_bytes: 64,
                max_events: 2,
            },
        );
        assert_eq!(
            split(&[b"data: 0123456789abcdefg\n\n"], small),
            Err(SseError::LineTooLong)
        );
        // A line that never ends is caught before its end arrives.
        assert_eq!(
            split(&[b"data: 0123456789abcdef"], small),
            Err(SseError::LineTooLong)
        );
        assert_eq!(
            split(&[b"data: 0123456789\ndata: 0123456789\n\n"], small),
            Err(SseError::EventTooLarge)
        );
        assert_eq!(
            split(&[b"data: a\n\ndata: b\n\ndata: c\n\n"], small),
            Err(SseError::TooManyEvents)
        );
        assert_eq!(
            split(&[&[b':'; 65][..]], small),
            Err(SseError::StreamTooLarge)
        );
    }

    #[test]
    fn strict_caps_hold_and_charge_the_event_name() {
        let small = |max_line_bytes, max_event_bytes, max_stream_bytes, max_events| {
            options(
                SseDialect::Strict,
                SseLimits {
                    max_line_bytes,
                    max_event_bytes,
                    max_stream_bytes,
                    max_events,
                },
            )
        };
        // "data: abc" joins to 3 bytes, charged 4.
        assert_eq!(
            joined(split(&[b"data: abc\n\n"], small(64, 4, 64, 4))),
            Ok("abc".to_owned())
        );
        assert_eq!(
            split(&[b"data: abc\n\n"], small(64, 3, 64, 4)),
            Err(SseError::EventTooLarge)
        );
        // Two lines: "a\nb" is 3 bytes, charged 4.
        assert!(split(&[b"data: a\ndata: b\n\n"], small(64, 4, 64, 4)).is_ok());
        assert_eq!(
            split(&[b"data: a\ndata: b\n\n"], small(64, 3, 64, 4)),
            Err(SseError::EventTooLarge)
        );
        // The name is charged too: 3 + 1 + 2.
        assert_eq!(
            split(&[b"event: ab\ndata:abc\n\n"], small(64, 5, 64, 4)),
            Err(SseError::EventTooLarge)
        );
        assert!(split(&[b"event: ab\ndata:abc\n\n"], small(64, 6, 64, 4)).is_ok());
        // A name longer than the event cap is malformed.
        assert_eq!(
            split(&[b"event: abcdefg\ndata:a\n\n"], small(64, 6, 64, 4)),
            Err(SseError::Malformed)
        );
        // An ended line above the cap; an unended one is bounded by the stream.
        assert_eq!(
            split(&[b"data: 0123456789\n\n"], small(8, 64, 64, 4)),
            Err(SseError::LineTooLong)
        );
        assert_eq!(
            split(&[b"data: 0123456789"], small(8, 64, 10, 4)),
            Err(SseError::StreamTooLarge)
        );
        assert_eq!(
            split(&[b"data: a\n\ndata: b\n\n"], small(64, 8, 64, 1)),
            Err(SseError::TooManyEvents)
        );
    }

    #[test]
    fn strict_grammar_is_enforced() {
        for (stream, why) in [
            (&b"id: 1\ndata: a\n\n"[..], "an id field"),
            (b"retry: 5\ndata: a\n\n", "a retry field"),
            (b"data\n\n", "a field without a colon"),
            (b"\xEF\xBB\xBFdata: a\n\n", "a byte order mark"),
            (b"data: a\nevent: e\n\n", "a name after data"),
            (b"event: e\nevent: f\ndata: a\n\n", "two names"),
            (b"event:\ndata: a\n\n", "an empty name"),
            (b"event: e\n\n", "a name without data"),
            (b"event: e", "a dangling name at the end"),
        ] {
            assert_eq!(strict(&[stream]), Err(SseError::Malformed), "{why}");
        }
        let mut splitter = SseSplitter::new(options(SseDialect::Strict, SseLimits::default()));
        splitter.finish();
        assert_eq!(splitter.push(b"data: a\n\n"), Err(SseError::Malformed));
    }

    #[test]
    fn strict_data_is_bytes_and_lenient_data_is_text() {
        assert_eq!(lenient(&[b"data: \xff\xfe\n\n"]), Err(SseError::NotUtf8));
        assert_eq!(
            lenient(&[b"event: \xff\ndata: a\n\n"]),
            Err(SseError::NotUtf8)
        );
        assert_eq!(
            strict(&[b"data: \xff\n\n"]).map(|e| e[0].data.clone()),
            Ok(vec![0xff])
        );
    }

    #[test]
    fn named_events_can_be_refused() {
        let mut refuse = options(SseDialect::Strict, SseLimits::default());
        refuse.reject_event_types = true;
        let mut splitter = SseSplitter::new(refuse);
        assert_eq!(splitter.push(b"data: a\n\nevent: e\ndata: b\n\n"), Ok(()));
        assert_eq!(
            splitter.next_event().map(|e| e.map(|e| e.data)),
            Ok(Some(b"a".to_vec()))
        );
        assert_eq!(splitter.next_event(), Err(SseError::UnexpectedEventType));
    }

    #[test]
    fn lenient_lone_carriage_returns_end_lines() {
        let stream = b"data: a\r\rdata: b\r\n\r\ndata: c\r\r";
        for at in 0..stream.len() {
            let (a, b) = stream.split_at(at);
            let events = lenient(&[a, b]);
            assert_eq!(joined(events), Ok("a|b|c".to_owned()), "split at {at}");
        }
        // A `\r\n` split across pushes is one line end, not two.
        assert_eq!(
            joined(lenient(&[b"data: a\r", b"\ndata: b\r", b"\n\r\n"])),
            Ok("a\nb".to_owned())
        );
    }

    #[test]
    fn lenient_skips_a_leading_byte_order_mark_only() {
        let stream = b"\xEF\xBB\xBFdata: x\n\n";
        for at in 0..stream.len() {
            let (a, b) = stream.split_at(at);
            assert_eq!(
                joined(lenient(&[a, b])),
                Ok("x".to_owned()),
                "split at {at}"
            );
        }
        assert_eq!(
            lenient(&[b"data: x\n\n\xEF\xBB\xBFdata: y\n\n"]).map(|e| e.len()),
            Ok(1)
        );
    }

    #[test]
    fn lenient_fields_follow_the_sse_grammar() {
        // A line without a colon is a field with an empty value.
        assert_eq!(joined(lenient(&[b"data\n\n"])), Ok(String::new()));
        // The last name wins; a name without data is dropped.
        let events = lenient(&[b"event: a\nevent: b\ndata: x\n\nevent: c\n\ndata: y\n\n"]);
        let Ok(events) = events else {
            panic!("{events:?}");
        };
        assert_eq!(events[0].event_type.as_deref(), Some(&b"b"[..]));
        assert_eq!(events[1].event_type, None);
    }

    #[test]
    fn a_long_stream_compacts_its_buffer() {
        let event = format!("data: {}\n\n", "z".repeat(1000));
        let mut splitter = SseSplitter::new(options(SseDialect::Strict, SseLimits::default()));
        let mut out = Vec::new();
        for _ in 0..500 {
            assert_eq!(splitter.push_into(event.as_bytes(), &mut out), Ok(()));
        }
        assert_eq!(out.len(), 500);
        assert!(splitter.buf.len() <= COMPACT_BYTES + event.len());
    }
}
