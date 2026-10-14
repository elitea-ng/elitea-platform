//! The one `multipart/form-data` encoder the worker writes with (the platform
//! object upload and the gateway's image edits), and the one guard on what
//! may reach a part header.
//!
//! A file name or media type that could end the header line (a quote, a
//! backslash, a CR or LF, any control character) is refused here and nowhere
//! else needs to check: the encoder cannot produce a body whose headers a
//! caller smuggled into.

use bytes::Bytes;

/// A part header value that could not be sent. Carries no value.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub(crate) enum PartError {
    /// The file name is empty or holds a quote, backslash or control character.
    FileName,
    /// The media type holds a quote or a control character.
    MediaType,
}

/// Whether `name` can sit inside `filename="..."`.
#[must_use]
pub(crate) fn valid_file_name(name: &str) -> bool {
    !name.is_empty()
        && !name
            .chars()
            .any(|c| c == '"' || c == '\\' || c.is_control())
}

/// A `multipart/form-data` body under construction.
pub(crate) struct Multipart {
    boundary: String,
    body: Vec<u8>,
}

impl Multipart {
    /// An empty body with a fresh boundary.
    #[must_use]
    pub(crate) fn new() -> Self {
        Self {
            boundary: format!("elitea-{}", uuid::Uuid::new_v4().simple()),
            body: Vec::new(),
        }
    }

    /// A text field. `name` is a fixed identifier chosen by the caller, never
    /// user data.
    pub(crate) fn text(&mut self, name: &str, value: &str) {
        self.body.extend_from_slice(
            format!(
                "--{}\r\nContent-Disposition: form-data; name=\"{name}\"\r\n\r\n{value}\r\n",
                self.boundary
            )
            .as_bytes(),
        );
    }

    /// A file field. The bytes are copied once, into the body: the wire
    /// format needs them contiguous between the headers and the boundary.
    ///
    /// # Errors
    ///
    /// The file name or media type could not be sent; nothing is added.
    pub(crate) fn file(
        &mut self,
        field: &str,
        file_name: &str,
        media_type: &str,
        bytes: &[u8],
    ) -> Result<(), PartError> {
        if !valid_file_name(file_name) {
            return Err(PartError::FileName);
        }
        if media_type.is_empty() || media_type.chars().any(|c| c == '"' || c.is_control()) {
            return Err(PartError::MediaType);
        }
        self.body.reserve(bytes.len() + 256);
        self.body.extend_from_slice(
            format!(
                "--{}\r\nContent-Disposition: form-data; name=\"{field}\"; filename=\"{file_name}\"\r\nContent-Type: {media_type}\r\n\r\n",
                self.boundary
            )
            .as_bytes(),
        );
        self.body.extend_from_slice(bytes);
        self.body.extend_from_slice(b"\r\n");
        Ok(())
    }

    /// The `Content-Type` header value and the finished body.
    #[must_use]
    pub(crate) fn finish(mut self) -> (String, Bytes) {
        self.body
            .extend_from_slice(format!("--{}--\r\n", self.boundary).as_bytes());
        (
            format!("multipart/form-data; boundary={}", self.boundary),
            Bytes::from(self.body),
        )
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn fields_and_files_are_framed_and_closed() {
        let mut form = Multipart::new();
        form.text("prompt", "a hat");
        form.file("image", "cat.png", "image/png", b"\0\r\nBYTES")
            .expect("file");
        let (content_type, body) = form.finish();
        let boundary = content_type
            .strip_prefix("multipart/form-data; boundary=")
            .expect("boundary");
        let text = String::from_utf8_lossy(&body).into_owned();
        assert!(text.contains("name=\"prompt\"\r\n\r\na hat\r\n"));
        assert!(text.contains(
            "name=\"image\"; filename=\"cat.png\"\r\nContent-Type: image/png\r\n\r\n\0\r\nBYTES\r\n"
        ));
        assert!(text.ends_with(&format!("--{boundary}--\r\n")));
    }

    #[test]
    fn a_value_that_could_end_the_header_line_is_refused_and_adds_nothing() {
        for name in ["", "a\"b", "a\\b", "a\r\nX: y", "a\nb", "a\u{7f}b"] {
            let mut form = Multipart::new();
            assert_eq!(
                form.file("f", name, "image/png", &[1]),
                Err(PartError::FileName),
                "{name:?}"
            );
            assert!(form.body.is_empty());
        }
        let mut form = Multipart::new();
        for media in ["", "image/png\r\nX: y", "a\"b"] {
            assert_eq!(
                form.file("f", "ok.png", media, &[1]),
                Err(PartError::MediaType),
                "{media:?}"
            );
        }
        assert!(form.body.is_empty());
        assert!(valid_file_name("dir name/ok-1.png"));
    }
}
