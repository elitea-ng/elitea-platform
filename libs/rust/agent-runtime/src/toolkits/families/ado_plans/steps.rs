//! Test case step XML (`TestPlanApiWrapper::{get_test_steps_xml,
//! convert_steps_tag_to_ado_steps,build_step_element}`).
//!
//! The SDK builds `Microsoft.VSTS.TCM.Steps` with `xml.etree.ElementTree`
//! and serializes it with `ET.tostring(..., encoding="unicode")`; the writer
//! here reproduces that output byte for byte, including `ElementTree`'s
//! `<tag attr="..." />` for an empty step text. The reader accepts the
//! documented `<Steps><Step>...</Step></Steps>` input with a deliberately
//! small, bounded XML parser: no DTDs, no external entities.

use std::fmt::Write as _;

use serde_json::Value;

const MAX_STEPS: usize = 512;
const MAX_DEPTH: usize = 32;
const MAX_ELEMENTS: usize = 8_192;

/// One step as the SDK reads it: number, action and expected result.
struct Step {
    number: String,
    action: String,
    expected: String,
}

/// `get_test_steps_xml(json.loads(test_steps))`.
pub(crate) fn steps_from_json(test_steps: &str) -> Result<String, String> {
    let parsed: Value = serde_json::from_str(test_steps)
        .map_err(|error| format!("Invalid JSON format for test_steps: {error}"))?;
    let steps = parsed
        .as_array()
        .ok_or_else(|| "test_steps must be a JSON array of step objects".to_owned())?;
    if steps.len() > MAX_STEPS {
        return Err(format!("test_steps holds more than {MAX_STEPS} steps"));
    }
    let mut built = Vec::with_capacity(steps.len());
    for step in steps {
        let step = step
            .as_object()
            .ok_or_else(|| "each test step must be a JSON object".to_owned())?;
        built.push(Step {
            number: step
                .get("stepNumber")
                .map_or_else(|| "1".to_owned(), python_str),
            action: step.get("action").map(python_or_empty).unwrap_or_default(),
            expected: step
                .get("expectedResult")
                .map(python_or_empty)
                .unwrap_or_default(),
        });
    }
    Ok(write_steps(&built))
}

/// `convert_steps_tag_to_ado_steps(input_xml)`: the root's direct `Step`
/// children, each read with `findtext` and its defaults.
pub(crate) fn steps_from_xml(input_xml: &str) -> Result<String, String> {
    let root = parse(input_xml)?;
    let mut built = Vec::new();
    for step in root.children.iter().filter(|child| child.tag == "Step") {
        if built.len() >= MAX_STEPS {
            return Err(format!("test_steps holds more than {MAX_STEPS} steps"));
        }
        built.push(Step {
            number: step
                .find_text("StepNumber")
                .unwrap_or_else(|| "1".to_owned()),
            action: step.find_text("Action").unwrap_or_default(),
            expected: step.find_text("ExpectedResult").unwrap_or_default(),
        });
    }
    Ok(write_steps(&built))
}

fn python_str(value: &Value) -> String {
    crate::toolkits::families::ado::work_items::python_text(value)
}

/// `action or ""`: a falsy value becomes empty text.
fn python_or_empty(value: &Value) -> String {
    match value {
        Value::Null | Value::Bool(false) => String::new(),
        Value::String(text) => text.clone(),
        Value::Number(number) if number.as_f64() == Some(0.0) => String::new(),
        other => python_str(other),
    }
}

fn write_steps(steps: &[Step]) -> String {
    if steps.is_empty() {
        return "<steps />".to_owned();
    }
    let mut output = String::from("<steps>");
    for step in steps {
        let _ = write!(
            output,
            "<step id=\"{}\" type=\"Action\">",
            escape_attribute(&step.number)
        );
        write_parameterized(&mut output, &step.action);
        write_parameterized(&mut output, &step.expected);
        output.push_str("</step>");
    }
    output.push_str("</steps>");
    output
}

fn write_parameterized(output: &mut String, text: &str) {
    if text.is_empty() {
        output.push_str("<parameterizedString isformatted=\"true\" />");
    } else {
        output.push_str("<parameterizedString isformatted=\"true\">");
        output.push_str(&escape_text(text));
        output.push_str("</parameterizedString>");
    }
}

/// `ElementTree` `_escape_cdata`.
fn escape_text(text: &str) -> String {
    text.replace('&', "&amp;")
        .replace('<', "&lt;")
        .replace('>', "&gt;")
}

/// `ElementTree` `_escape_attrib`.
fn escape_attribute(text: &str) -> String {
    escape_text(text)
        .replace('"', "&quot;")
        .replace('\r', "&#13;")
        .replace('\n', "&#10;")
        .replace('\t', "&#09;")
}

struct Element {
    tag: String,
    text: String,
    children: Vec<Element>,
}

impl Element {
    /// `Element.findtext(tag)`: the first direct child's text, `""` for an
    /// empty child, `None` when there is no such child.
    fn find_text(&self, tag: &str) -> Option<String> {
        self.children
            .iter()
            .find(|child| child.tag == tag)
            .map(|child| child.text.clone())
    }
}

struct Parser<'a> {
    input: &'a str,
    position: usize,
    elements: usize,
}

fn parse(input: &str) -> Result<Element, String> {
    let mut parser = Parser {
        input,
        position: 0,
        elements: 0,
    };
    parser.skip_misc()?;
    let root = parser.element(0)?;
    parser.skip_misc()?;
    if parser.position != input.len() {
        return Err("Invalid XML for test_steps: junk after document element".to_owned());
    }
    Ok(root)
}

impl Parser<'_> {
    fn rest(&self) -> &str {
        &self.input[self.position..]
    }

    fn error(message: &str) -> String {
        format!("Invalid XML for test_steps: {message}")
    }

    fn skip_whitespace(&mut self) {
        let trimmed = self.rest().trim_start();
        self.position = self.input.len() - trimmed.len();
    }

    /// Whitespace, the XML declaration, processing instructions and comments
    /// around the root. A DOCTYPE is refused.
    fn skip_misc(&mut self) -> Result<(), String> {
        loop {
            self.skip_whitespace();
            if self.rest().starts_with("<?") {
                self.skip_past("?>")?;
            } else if self.rest().starts_with("<!--") {
                self.skip_past("-->")?;
            } else if self.rest().starts_with("<!") {
                return Err(Self::error("DOCTYPE and declarations are not accepted"));
            } else {
                return Ok(());
            }
        }
    }

    fn skip_past(&mut self, terminator: &str) -> Result<(), String> {
        let end = self
            .rest()
            .find(terminator)
            .ok_or_else(|| Self::error("unterminated markup"))?;
        self.position += end + terminator.len();
        Ok(())
    }

    fn name(&mut self) -> Result<String, String> {
        let rest = self.rest();
        let end = rest
            .find(|character: char| {
                character.is_whitespace() || matches!(character, '>' | '/' | '=')
            })
            .unwrap_or(rest.len());
        if end == 0 {
            return Err(Self::error("missing name"));
        }
        let name = rest[..end].to_owned();
        self.position += end;
        Ok(name)
    }

    fn element(&mut self, depth: usize) -> Result<Element, String> {
        if depth > MAX_DEPTH {
            return Err(Self::error("nesting is too deep"));
        }
        self.elements += 1;
        if self.elements > MAX_ELEMENTS {
            return Err(Self::error("too many elements"));
        }
        if !self.rest().starts_with('<') {
            return Err(Self::error("expected an element"));
        }
        self.position += 1;
        let tag = self.name()?;
        // Attributes are read for well-formedness and otherwise ignored.
        loop {
            self.skip_whitespace();
            if self.rest().starts_with("/>") {
                self.position += 2;
                return Ok(Element {
                    tag,
                    text: String::new(),
                    children: Vec::new(),
                });
            }
            if self.rest().starts_with('>') {
                self.position += 1;
                break;
            }
            self.name()?;
            self.skip_whitespace();
            if !self.rest().starts_with('=') {
                return Err(Self::error("attribute without a value"));
            }
            self.position += 1;
            self.skip_whitespace();
            let quote = self
                .rest()
                .chars()
                .next()
                .filter(|quote| matches!(quote, '"' | '\''))
                .ok_or_else(|| Self::error("unquoted attribute value"))?;
            self.position += 1;
            let end = self
                .rest()
                .find(quote)
                .ok_or_else(|| Self::error("unterminated attribute value"))?;
            self.position += end + 1;
        }
        let mut element = Element {
            tag,
            text: String::new(),
            children: Vec::new(),
        };
        loop {
            if self.rest().starts_with("</") {
                self.position += 2;
                let closing = self.name()?;
                self.skip_whitespace();
                if closing != element.tag || !self.rest().starts_with('>') {
                    return Err(Self::error("mismatched closing tag"));
                }
                self.position += 1;
                return Ok(element);
            }
            if self.rest().starts_with("<!--") {
                self.skip_past("-->")?;
            } else if self.rest().starts_with("<![CDATA[") {
                self.position += "<![CDATA[".len();
                let end = self
                    .rest()
                    .find("]]>")
                    .ok_or_else(|| Self::error("unterminated CDATA"))?;
                if element.children.is_empty() {
                    element.text.push_str(&self.rest()[..end]);
                }
                self.position += end + 3;
            } else if self.rest().starts_with("<?") {
                self.skip_past("?>")?;
            } else if self.rest().starts_with('<') {
                let child = self.element(depth + 1)?;
                element.children.push(child);
            } else if self.rest().is_empty() {
                return Err(Self::error("unterminated element"));
            } else {
                let end = self.rest().find('<').unwrap_or(self.rest().len());
                let text = decode(&self.rest()[..end])?;
                // ElementTree's `.text` is the text before the first child.
                if element.children.is_empty() {
                    element.text.push_str(&text);
                }
                self.position += end;
            }
        }
    }
}

fn decode(raw: &str) -> Result<String, String> {
    let mut output = String::with_capacity(raw.len());
    let mut rest = raw;
    while let Some(start) = rest.find('&') {
        output.push_str(&rest[..start]);
        let end = rest[start..]
            .find(';')
            .ok_or_else(|| Parser::error("unterminated entity"))?;
        let entity = &rest[start + 1..start + end];
        let character = match entity {
            "lt" => '<',
            "gt" => '>',
            "amp" => '&',
            "quot" => '"',
            "apos" => '\'',
            _ => {
                let code = if let Some(hex) = entity.strip_prefix("#x") {
                    u32::from_str_radix(hex, 16).ok()
                } else if let Some(decimal) = entity.strip_prefix('#') {
                    decimal.parse::<u32>().ok()
                } else {
                    None
                };
                code.and_then(char::from_u32)
                    .ok_or_else(|| Parser::error("undefined entity"))?
            }
        };
        output.push(character);
        rest = &rest[start + end + 1..];
    }
    output.push_str(rest);
    Ok(output)
}
