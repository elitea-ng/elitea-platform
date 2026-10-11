//! The SDK's step-back prompts, byte for byte (ADR-0030 decision 5).
//!
//! Copied from `elitea_sdk/runtime/tools/vectorstore_base.py` at b5113a1:
//! `STEPBACK_PROMPT` (including the space after `{input}`) and
//! `GET_ANSWER_PROMPT`. Their placeholders are `{input}`, `{search_results}`
//! and `{messages}`, filled by [`render`].

/// Turns a question into a generic similarity-search query.
pub const STEPBACK_PROMPT: &str = r"Your task is to convert provided question into a more generic question that will be used for similarity search.
Remove all not important words, question words, but save all names, dates and acronym as in original question.

<input>
{input} 
</input>

Output:
";

/// Answers a question from the search results.
pub const GET_ANSWER_PROMPT: &str = r#"<search_results>
{search_results}
</search_results>

<conversation_history>
{messages}
</conversation_history>

Please answer the question based on provided search results.
Provided information is already processed and available in the context as list of possibly relevant pieces of the documents.
Use only provided information. Do not make up answer.
If you have no answer and you can not derive it from the context, please provide "I have no answer".
<question>
{input}
</question>
## Answer
Add <ANSWER> here

## Score
Score the answer from 0 to 100, where 0 is not relevant and 100 is very relevant.

## Citations
- source (score)
- source (score)
Make sure to provide unique source for each citation.

## Explanation
How did you come up with the answer?
"#;
