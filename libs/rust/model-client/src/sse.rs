//! A bounded Server-Sent Events decoder for chat-completion streams: the
//! shared `/llm` splitter (`elitea_llm_wire::sse`) in its lenient dialect.
//!
//! The gateway is trusted to be the platform's, but what it relays comes
//! from a model provider; a stream that never ends a line, or one event
//! that grows without bound, must fail the call rather than grow the
//! engine's memory. So every buffer here has a cap, and the stream as a
//! whole has one.
//!
//! The subset of the SSE grammar a completion stream uses: lines end in
//! `\n`, `\r\n` or a lone `\r`; a UTF-8 byte order mark at the start of
//! the stream is skipped; `data:` lines of one event join with `\n`; a blank line
//! ends the event; `:` starts a comment (a keep-alive); `event:` names the
//! event (only `error` matters); `id:` and `retry:` are ignored. A trailing
//! event without its blank line is still delivered at the end of the
//! stream, because some servers close right after `data: [DONE]`.

use elitea_llm_wire::sse::{SseDialect, SseOptions, SseSplitter};

/// The decoder's caps (`max_line_bytes`, `max_event_bytes`,
/// `max_stream_bytes`, `max_events`); the default is line and event 1 MiB,
/// stream 64 MiB, 200 000 events.
pub use elitea_llm_wire::sse::SseLimits;

/// One event.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct SseEvent {
    /// The `event:` name, empty for the default `message`.
    pub event: String,
    pub data: String,
}

/// Why a stream was refused.
#[derive(Debug, Clone, PartialEq, Eq)]
pub enum SseError {
    LineTooLong,
    EventTooLarge,
    StreamTooLarge,
    TooManyEvents,
    NotUtf8,
}

impl std::fmt::Display for SseError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str(match self {
            Self::LineTooLong => "the stream sent a line above the size cap",
            Self::EventTooLarge => "the stream sent an event above the size cap",
            Self::StreamTooLarge => "the stream exceeded its total size cap",
            Self::TooManyEvents => "the stream exceeded its event count cap",
            Self::NotUtf8 => "the stream sent bytes that are not UTF-8",
        })
    }
}

impl From<elitea_llm_wire::sse::SseError> for SseError {
    fn from(error: elitea_llm_wire::sse::SseError) -> Self {
        use elitea_llm_wire::sse::SseError as Wire;
        match error {
            Wire::LineTooLong => Self::LineTooLong,
            Wire::EventTooLarge => Self::EventTooLarge,
            Wire::StreamTooLarge => Self::StreamTooLarge,
            Wire::TooManyEvents => Self::TooManyEvents,
            // The lenient dialect, without refusing named events, reports
            // neither of the strict dialect's grammar errors; were one to
            // appear, it is still a stream this decoder cannot read as text.
            Wire::NotUtf8 | Wire::Malformed | Wire::UnexpectedEventType => Self::NotUtf8,
        }
    }
}

/// The incremental decoder: `elitea_llm_wire`'s splitter in its lenient
/// dialect, with events as text.
#[derive(Debug, Default)]
pub struct SseDecoder {
    splitter: SseSplitter,
}

impl SseDecoder {
    #[must_use]
    pub fn new(limits: SseLimits) -> Self {
        Self {
            splitter: SseSplitter::new(SseOptions {
                limits,
                dialect: SseDialect::Lenient,
                reject_event_types: false,
            }),
        }
    }

    /// Feed bytes; complete events are appended to `out`.
    ///
    /// # Errors
    ///
    /// The first cap the input crosses, or data that is not UTF-8.
    pub fn push(&mut self, chunk: &[u8], out: &mut Vec<SseEvent>) -> Result<(), SseError> {
        self.splitter.push(chunk)?;
        self.drain(out)
    }

    /// End of stream: deliver a last event that lacked its blank line.
    ///
    /// # Errors
    ///
    /// As [`SseDecoder::push`].
    pub fn finish(&mut self, out: &mut Vec<SseEvent>) -> Result<(), SseError> {
        self.splitter.finish();
        self.drain(out)
    }

    fn drain(&mut self, out: &mut Vec<SseEvent>) -> Result<(), SseError> {
        while let Some(event) = self.splitter.next_event()? {
            // The lenient dialect delivers only UTF-8 names and data.
            let text = |bytes: Vec<u8>| String::from_utf8(bytes).map_err(|_| SseError::NotUtf8);
            out.push(SseEvent {
                event: text(event.event_type.unwrap_or_default())?,
                data: text(event.data)?,
            });
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    fn decode(chunks: &[&[u8]], limits: SseLimits) -> Result<Vec<SseEvent>, SseError> {
        let mut decoder = SseDecoder::new(limits);
        let mut out = Vec::new();
        for chunk in chunks {
            decoder.push(chunk, &mut out)?;
        }
        decoder.finish(&mut out)?;
        Ok(out)
    }

    fn data(events: &[SseEvent]) -> Vec<&str> {
        events.iter().map(|e| e.data.as_str()).collect()
    }

    #[test]
    fn events_survive_any_chunking() {
        let stream = b": keep-alive\r\ndata: {\"a\":1}\r\n\r\ndata: x\ndata: y\n\nevent: error\ndata: boom\n\ndata: [DONE]\n\n";
        for split in 0..stream.len() {
            let (a, b) = stream.split_at(split);
            let Ok(events) = decode(&[a, b], SseLimits::default()) else {
                panic!("split at {split} failed");
            };
            assert_eq!(data(&events), ["{\"a\":1}", "x\ny", "boom", "[DONE]"]);
            assert_eq!(events[2].event, "error");
            assert_eq!(events[3].event, "");
        }
    }

    #[test]
    fn a_last_event_without_its_blank_line_is_delivered() {
        let events = decode(&[b"data: [DONE]"], SseLimits::default());
        assert_eq!(events.map(|e| data(&e).join("|")), Ok("[DONE]".to_owned()));
    }

    #[test]
    fn the_caps_hold() {
        let small = SseLimits {
            max_line_bytes: 16,
            max_event_bytes: 15,
            max_stream_bytes: 64,
            max_events: 2,
        };
        assert_eq!(
            decode(&[b"data: 0123456789abcdefg\n\n"], small),
            Err(SseError::LineTooLong)
        );
        // A line that never ends is caught before the newline arrives.
        assert_eq!(
            decode(&[b"data: 0123456789abcdef"], small),
            Err(SseError::LineTooLong)
        );
        assert_eq!(
            decode(&[b"data: 0123456789\ndata: 0123456789\n\n"], small),
            Err(SseError::EventTooLarge)
        );
        assert_eq!(
            decode(&[b"data: a\n\ndata: b\n\ndata: c\n\n"], small),
            Err(SseError::TooManyEvents)
        );
        assert_eq!(
            decode(&[&[b':'; 65][..]], small),
            Err(SseError::StreamTooLarge)
        );
    }

    #[test]
    fn invalid_utf8_is_refused() {
        assert_eq!(
            decode(&[b"data: \xff\xfe\n\n"], SseLimits::default()),
            Err(SseError::NotUtf8)
        );
    }

    #[test]
    fn a_lone_carriage_return_ends_a_line() {
        let stream = b"data: a\r\rdata: b\r\n\r\ndata: c\r\r";
        for split in 0..stream.len() {
            let (a, b) = stream.split_at(split);
            let Ok(events) = decode(&[a, b], SseLimits::default()) else {
                panic!("split at {split} failed");
            };
            assert_eq!(data(&events), ["a", "b", "c"], "split at {split}");
        }
        // A `\r\n` split across chunks is one line end, not two.
        let events = decode(
            &[b"data: a\r", b"\ndata: b\r", b"\n\r\n"],
            SseLimits::default(),
        );
        assert_eq!(events.map(|e| data(&e).join("|")), Ok("a\nb".to_owned()));
    }

    #[test]
    fn a_leading_byte_order_mark_is_skipped() {
        let stream = b"\xEF\xBB\xBFdata: x\n\n";
        for split in 0..stream.len() {
            let (a, b) = stream.split_at(split);
            let events = decode(&[a, b], SseLimits::default());
            assert_eq!(
                events.map(|e| data(&e).join("|")),
                Ok("x".to_owned()),
                "split at {split}"
            );
        }
        // Only at the start of the stream.
        assert_eq!(
            decode(
                &[b"data: x\n\n\xEF\xBB\xBFdata: y\n\n"],
                SseLimits::default()
            )
            .map(|e| e.len()),
            Ok(1)
        );
    }
}
