use async_trait::async_trait;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use thiserror::Error;

const SYSTEM_PROMPT: &str = r#"You maintain an AI agent's long-term memory. Given a NEW memory and a list of
EXISTING candidate memories, decide how the new one relates to them. Respond with a single JSON object:
{"action":"new|update|supersede","target":"<candidate id or empty>",
 "content":"<text to store>","summary":"<one line>","reason":"<short>",
 "linked_ids":["<id>",...]}

A DUPLICATE restates the same fact in different words (same value, same subject, same meaning).
A CONTRADICTION makes the old fact wrong or outdated: the SAME property now has a DIFFERENT value,
the state changed, or an error was corrected. When two memories assign different values to the same
property (different number, different name, different provider, different setting), the newer one
CONTRADICTS the older one — they cannot both be true.
Two facts about DIFFERENT properties or DIFFERENT events of the same entity are DISTINCT (new).

Rules:
- "new": the new memory is about a different property, event, or topic than all candidates.
  Set content to the new memory text and target to empty.
- "update": the new memory DUPLICATES a candidate (same fact, same value, reworded or refined).
  Set target to that candidate's id and content to the merged, deduplicated text.
- "supersede": the new memory CONTRADICTS a candidate — same property, different value, or corrected.
  Set target to that candidate's id and content to the new memory text.
- "linked_ids": for "new" actions, include the IDs of any candidate about the same entity/topic
  but that is a DISTINCT fact (not duplicate, not contradiction). Skip candidates that are
  a different entity/topic entirely. For "update" and "supersede", leave empty.

Test: would the NEW memory make the EXISTING candidate FALSE if both were stored? If yes → supersede.
Can both be true simultaneously? If yes → update (same fact) or new (different fact).

Examples:
- EXISTING "Cache entries expire after a 10 minute TTL" / NEW "Cached items live for ten minutes"
  → update (same value 10 min, reworded)
- EXISTING "Cache entries expire after a 10 minute TTL" / NEW "Cache entries expire after a 30 minute TTL"
  → supersede (same property TTL, different value 10→30)
- EXISTING "The reranker is served on port 8002" / NEW "The reranker is served on port 9002"
  → supersede (same property port, different value)
- EXISTING "Email is sent through Postmark" / NEW "Email is sent through SES"
  → supersede (same property email provider, different value)
- EXISTING "The frontend is built with React and Vite" / NEW "The frontend is built with Svelte and Vite"
  → supersede (same property frontend framework, different value)
- EXISTING "Bob ran 5 miles on Tuesday" / NEW "Bob ran 3 miles on Wednesday"
  → new (different events on different days, both can be true)
- EXISTING "The cache is sharded across four nodes" / NEW "Cache entries expire after a 30 minute TTL"
  → new (different properties of the same system, both can be true)

Output only the JSON object."#;
const DISTILL_PROMPT: &str = r#"You compress an AI agent's episodic memories into durable, reusable knowledge.
Input is a JSON object {"now":"YYYY-MM-DD","episodes":[{"content":"...","date":"YYYY-MM-DD"}]} of past
observations, each with the date it was recorded; "now" is today. Extract only the DURABLE knowledge
worth keeping long-term and classify each item with a "category":
- "preference": a user preference or correction ("don't use X", "always prefer Y", "stop doing Z").
- "procedure": reusable how-to knowledge, including error→recovery ("when X fails, do Y instead").
- "fact": a stable fact, decision, or convention that is neither of the above.
Episodes prefixed with "[failed]" were captured from a failed turn or command; pair one with a later
success to form an error→recovery "procedure".
Discard transient noise (one-off actions, routine file edits with no lasting lesson). Split compound
facts into separate items: "User's name is John and works at Google" becomes two items ("User's name is
John" and "User works at Google"). Each item must be a single atomic fact.
Each item must be self-contained and readable without the episodes: name the subject explicitly (no bare
"he/she/it/this"), and keep the context that makes it actionable ("prefers pnpm for the frontend repo",
not "prefers pnpm").
When an episode states a relative time (e.g. "yesterday", "last week", "two days ago"), resolve it to an
absolute YYYY-MM-DD date in the item, grounding against that episode's "date" (or "now" if it has none).
Leave already-absolute dates unchanged.
Assign each fact a confidence score (0.0 to 1.0) reflecting how certain you are it is a durable, accurate
observation: 0.9+ for explicit user statements ("I prefer X"), 0.6-0.8 for inferred preferences, 0.3-0.5 for
speculative or second-hand facts. Clamp to [0.1, 0.7] — no fact starts above 0.7; corroboration raises it later.
Respond with a single JSON object:
{"facts":[{"content":"<durable item>","summary":"<one line>","category":"preference|procedure|fact","confidence":0.0_to_1.0}]}
Return {"facts":[]} if nothing is durable. Output only the JSON object."#;
const MERGE_PROMPT: &str = r#"You merge a cluster of near-duplicate memories into one comprehensive memory.
Input is a JSON array of memory texts that are semantically similar (near-duplicates or
restatements of the same fact). Produce a single, comprehensive memory text that:
1. Captures ALL unique information from every input.
2. Resolves contradictions by keeping the most recent/authoritative value.
3. Is self-contained and readable without the inputs.
4. Is concise — no repetition, no meta-commentary about the merge.

Respond with a single JSON object: {"content":"<merged memory text>"}
Output only the JSON object."#;

#[derive(Debug, Error)]
pub enum LlmError {
    #[error("llm: {0}")]
    Invalid(String),
    #[error("llm: {0}")]
    Http(#[from] reqwest::Error),
    #[error("llm: {0}")]
    Json(#[from] serde_json::Error),
}
pub type Result<T> = std::result::Result<T, LlmError>;
#[derive(Clone, Copy, Debug, Deserialize, Eq, PartialEq, Serialize)]
#[serde(rename_all = "lowercase")]
pub enum Action {
    New,
    Update,
    Supersede,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Candidate {
    pub id: String,
    pub content: String,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Input {
    #[serde(rename = "new")]
    pub new_memory: String,
    pub tier: String,
    pub candidates: Vec<Candidate>,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Decision {
    pub action: Action,
    #[serde(default)]
    pub target: String,
    #[serde(default)]
    pub content: String,
    #[serde(default)]
    pub summary: String,
    #[serde(default)]
    pub reason: String,
    #[serde(default)]
    pub linked_ids: Vec<String>,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Fact {
    pub content: String,
    #[serde(default)]
    pub summary: String,
    #[serde(default)]
    pub category: String,
    pub confidence: Option<f64>,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct Episode {
    pub content: String,
    #[serde(default)]
    pub date: String,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct DistillInput {
    pub episodes: Vec<Episode>,
    #[serde(default)]
    pub now: String,
}
#[derive(Clone, Debug)]
pub struct Tool {
    pub name: String,
    pub description: String,
    pub schema: Value,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ToolChoice {
    Auto,
    None,
    Required,
}
#[derive(Clone, Debug, Deserialize, Serialize)]
pub struct ToolCall {
    pub id: String,
    pub name: String,
    pub arguments: Value,
}
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Role {
    User,
    Assistant,
    Tool,
}
#[derive(Clone, Debug)]
pub struct ChatTurn {
    pub role: Role,
    pub text: String,
    pub calls: Vec<ToolCall>,
    pub call_id: String,
    pub name: String,
}
#[derive(Clone, Debug)]
pub struct ChatResult {
    pub text: String,
    pub calls: Vec<ToolCall>,
}

#[async_trait]
pub trait Client: Send + Sync {
    async fn complete(&self, system: &str, user: &str) -> Result<String>;
    async fn chat_tools(
        &self,
        system: &str,
        turns: &[ChatTurn],
        tools: &[Tool],
        choice: ToolChoice,
    ) -> Result<ChatResult>;
    async fn consolidate(&self, input: &Input) -> Result<Decision> {
        decode_decision(
            &self
                .complete(SYSTEM_PROMPT, &serde_json::to_string(input)?)
                .await?,
        )
    }
    async fn distill(&self, input: &DistillInput) -> Result<Vec<Fact>> {
        decode_facts(
            &self
                .complete(DISTILL_PROMPT, &serde_json::to_string(input)?)
                .await?,
        )
    }
    async fn merge_memories(&self, contents: &[String]) -> Result<String> {
        if contents.len() < 2 {
            return Ok(contents.first().cloned().unwrap_or_default());
        }
        let response = self
            .complete(MERGE_PROMPT, &serde_json::to_string(contents)?)
            .await?;
        let value: Value = unmarshal_loose(&response)?;
        Ok(value
            .get("content")
            .and_then(Value::as_str)
            .unwrap_or_default()
            .into())
    }
}

pub struct Observed {
    inner: std::sync::Arc<dyn Client>,
    dependencies: memini_observability::DependencyTracker,
}

pub fn observed(
    inner: std::sync::Arc<dyn Client>,
    dependencies: memini_observability::DependencyTracker,
) -> std::sync::Arc<dyn Client> {
    std::sync::Arc::new(Observed {
        inner,
        dependencies,
    })
}

#[async_trait]
impl Client for Observed {
    async fn complete(&self, system: &str, user: &str) -> Result<String> {
        let result = self.inner.complete(system, user).await;
        self.record(&result);
        result
    }

    async fn chat_tools(
        &self,
        system: &str,
        turns: &[ChatTurn],
        tools: &[Tool],
        choice: ToolChoice,
    ) -> Result<ChatResult> {
        let result = self.inner.chat_tools(system, turns, tools, choice).await;
        self.record(&result);
        result
    }
}

impl Observed {
    fn record<T>(&self, result: &Result<T>) {
        match result {
            Ok(_) => self.dependencies.record("llm", None),
            Err(error) => self.dependencies.record("llm", Some(&error.to_string())),
        }
    }
}

pub struct OpenAiClient {
    http: reqwest::Client,
    url: String,
    key: String,
    model: String,
    max_tokens: usize,
}
impl OpenAiClient {
    pub fn new(base_url: &str, key: &str, model: &str, max_tokens: usize) -> Result<Self> {
        if base_url.is_empty() {
            return Err(LlmError::Invalid("BaseURL is required".into()));
        }
        if model.is_empty() {
            return Err(LlmError::Invalid("Model is required".into()));
        }
        Ok(Self {
            http: reqwest::Client::builder()
                .timeout(std::time::Duration::from_secs(120))
                .build()?,
            url: format!("{}/chat/completions", base_url.trim_end_matches('/')),
            key: key.into(),
            model: model.into(),
            max_tokens: if max_tokens == 0 { 4096 } else { max_tokens },
        })
    }
    async fn request(&self, body: &Value) -> Result<Value> {
        retry_request(&self.http, &self.url, &self.key, None, body).await
    }
}
#[async_trait]
impl Client for OpenAiClient {
    async fn complete(&self, system: &str, user: &str) -> Result<String> {
        let response=self.request(&json!({"model":self.model,"messages":[{"role":"system","content":system},{"role":"user","content":user}],"temperature":0,"max_tokens":self.max_tokens})).await?;
        openai_result(&response).map(|v| v.text)
    }
    async fn chat_tools(
        &self,
        system: &str,
        turns: &[ChatTurn],
        tools: &[Tool],
        choice: ToolChoice,
    ) -> Result<ChatResult> {
        let mut messages = vec![json!({"role":"system","content":system})];
        for turn in turns {
            messages.push(match turn.role{Role::User=>json!({"role":"user","content":turn.text}),Role::Tool=>json!({"role":"tool","tool_call_id":turn.call_id,"content":turn.text}),Role::Assistant=>json!({"role":"assistant","content":turn.text,"tool_calls":turn.calls.iter().map(|call|json!({"id":call.id,"type":"function","function":{"name":call.name,"arguments":call.arguments.to_string()}})).collect::<Vec<_>>()})});
        }
        let tools=tools.iter().map(|tool|json!({"type":"function","function":{"name":tool.name,"description":tool.description,"parameters":tool.schema}})).collect::<Vec<_>>();
        self.request(&json!({"model":self.model,"messages":messages,"tools":tools,"tool_choice":choice_text(choice),"temperature":0,"max_tokens":self.max_tokens})).await.and_then(|v|openai_result(&v))
    }
}

pub struct AnthropicClient {
    http: reqwest::Client,
    url: String,
    key: String,
    model: String,
    max_tokens: usize,
}
impl AnthropicClient {
    pub fn new(base_url: &str, key: &str, model: &str, max_tokens: usize) -> Result<Self> {
        if model.is_empty() {
            return Err(LlmError::Invalid("Model is required".into()));
        }
        let base = if base_url.is_empty() {
            "https://api.anthropic.com"
        } else {
            base_url.trim_end_matches('/')
        };
        Ok(Self {
            http: reqwest::Client::builder()
                .timeout(std::time::Duration::from_secs(120))
                .build()?,
            url: format!("{base}/v1/messages"),
            key: key.into(),
            model: model.into(),
            max_tokens: if max_tokens == 0 { 4096 } else { max_tokens },
        })
    }
    async fn request(&self, body: &Value) -> Result<Value> {
        retry_request(
            &self.http,
            &self.url,
            "",
            Some((&self.key, "2023-06-01")),
            body,
        )
        .await
    }
}
#[async_trait]
impl Client for AnthropicClient {
    async fn complete(&self, system: &str, user: &str) -> Result<String> {
        let response=self.request(&json!({"model":self.model,"max_tokens":self.max_tokens,"temperature":0,"system":[{"type":"text","text":system,"cache_control":{"type":"ephemeral"}}],"messages":[{"role":"user","content":user}]})).await?;
        anthropic_result(&response).map(|v| v.text)
    }
    async fn chat_tools(
        &self,
        system: &str,
        turns: &[ChatTurn],
        tools: &[Tool],
        choice: ToolChoice,
    ) -> Result<ChatResult> {
        let mut messages = Vec::new();
        for turn in turns {
            messages.push(match turn.role{Role::User=>json!({"role":"user","content":turn.text}),Role::Tool=>json!({"role":"user","content":[{"type":"tool_result","tool_use_id":turn.call_id,"content":turn.text}]}),Role::Assistant=>{let mut blocks=Vec::new();if !turn.text.is_empty(){blocks.push(json!({"type":"text","text":turn.text}))}blocks.extend(turn.calls.iter().map(|call|json!({"type":"tool_use","id":call.id,"name":call.name,"input":call.arguments})));json!({"role":"assistant","content":blocks})}});
        }
        let tools=tools.iter().map(|tool|json!({"name":tool.name,"description":tool.description,"input_schema":tool.schema})).collect::<Vec<_>>();
        let choice = match choice {
            ToolChoice::Required => json!({"type":"any"}),
            ToolChoice::Auto => json!({"type":"auto"}),
            ToolChoice::None => Value::Null,
        };
        let response=self.request(&json!({"model":self.model,"max_tokens":self.max_tokens,"temperature":0,"system":[{"type":"text","text":system,"cache_control":{"type":"ephemeral"}}],"messages":messages,"tools":tools,"tool_choice":choice})).await?;
        anthropic_result(&response)
    }
}

async fn retry_request(
    client: &reqwest::Client,
    url: &str,
    bearer: &str,
    anthropic: Option<(&str, &str)>,
    body: &Value,
) -> Result<Value> {
    for attempt in 0..=6 {
        let mut request = client.post(url).json(body);
        if !bearer.is_empty() {
            request = request.bearer_auth(bearer)
        }
        if let Some((key, version)) = anthropic {
            request = request
                .header("x-api-key", key)
                .header("anthropic-version", version)
        }
        let response = request.send().await?;
        if (response.status().as_u16() == 429 || response.status().is_server_error()) && attempt < 6
        {
            tokio::time::sleep(std::time::Duration::from_millis(
                50 * (1_u64 << attempt.min(5)),
            ))
            .await;
            continue;
        }
        return Ok(response.error_for_status()?.json().await?);
    }
    Err(LlmError::Invalid("retry budget exhausted".into()))
}
fn choice_text(choice: ToolChoice) -> &'static str {
    match choice {
        ToolChoice::Auto => "auto",
        ToolChoice::None => "none",
        ToolChoice::Required => "required",
    }
}
fn openai_result(value: &Value) -> Result<ChatResult> {
    let choice = value
        .get("choices")
        .and_then(Value::as_array)
        .and_then(|v| v.first())
        .ok_or_else(|| LlmError::Invalid("empty response".into()))?;
    let message = &choice["message"];
    let text = message
        .get("content")
        .and_then(Value::as_str)
        .unwrap_or_default()
        .trim()
        .to_owned();
    let calls: Vec<ToolCall> = message
        .get("tool_calls")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
        .filter_map(|call| {
            let function = &call["function"];
            Some(ToolCall {
                id: call.get("id")?.as_str()?.into(),
                name: function.get("name")?.as_str()?.into(),
                arguments: serde_json::from_str(function.get("arguments")?.as_str()?).ok()?,
            })
        })
        .collect();
    if text.is_empty() && calls.is_empty() {
        return Err(LlmError::Invalid("response contained no text".into()));
    }
    Ok(ChatResult { text, calls })
}
fn anthropic_result(value: &Value) -> Result<ChatResult> {
    let mut text = String::new();
    let mut calls = Vec::new();
    for block in value
        .get("content")
        .and_then(Value::as_array)
        .into_iter()
        .flatten()
    {
        match block.get("type").and_then(Value::as_str) {
            Some("text") => text.push_str(
                block
                    .get("text")
                    .and_then(Value::as_str)
                    .unwrap_or_default(),
            ),
            Some("tool_use") => calls.push(ToolCall {
                id: block["id"].as_str().unwrap_or_default().into(),
                name: block["name"].as_str().unwrap_or_default().into(),
                arguments: block["input"].clone(),
            }),
            _ => {}
        }
    }
    let text = text.trim().to_owned();
    if text.is_empty() && calls.is_empty() {
        return Err(LlmError::Invalid("response contained no text".into()));
    }
    Ok(ChatResult { text, calls })
}
pub fn unmarshal_loose(content: &str) -> Result<Value> {
    let trimmed = content
        .trim()
        .strip_prefix("```json")
        .or_else(|| content.trim().strip_prefix("```"))
        .unwrap_or(content.trim())
        .strip_suffix("```")
        .unwrap_or(content.trim())
        .trim();
    if let Ok(value) = serde_json::from_str(trimmed) {
        return Ok(value);
    }
    let object = extract_json(trimmed)
        .ok_or_else(|| LlmError::Invalid("response contained no JSON object".into()))?;
    Ok(serde_json::from_str(object)?)
}
fn extract_json(value: &str) -> Option<&str> {
    let start = value.find('{')?;
    let (mut depth, mut string, mut escape) = (0, false, false);
    for (index, byte) in value.as_bytes().iter().enumerate().skip(start) {
        if string {
            if escape {
                escape = false
            } else if *byte == b'\\' {
                escape = true
            } else if *byte == b'"' {
                string = false
            }
            continue;
        }
        match *byte {
            b'"' => string = true,
            b'{' => depth += 1,
            b'}' => {
                depth -= 1;
                if depth == 0 {
                    return Some(&value[start..=index]);
                }
            }
            _ => {}
        }
    }
    None
}
pub fn decode_decision(content: &str) -> Result<Decision> {
    Ok(serde_json::from_value(unmarshal_loose(content)?)?)
}
pub fn decode_facts(content: &str) -> Result<Vec<Fact>> {
    let value = unmarshal_loose(content)?;
    let facts = serde_json::from_value::<Vec<Fact>>(
        value.get("facts").cloned().unwrap_or_else(|| json!([])),
    )?;
    Ok(facts
        .into_iter()
        .filter(|fact| !fact.content.trim().is_empty())
        .collect())
}

#[cfg(test)]
mod tests {
    use super::*;
    use axum::{Json, Router, routing::post};
    async fn server(app: Router) -> String {
        let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
        let address = listener.local_addr().unwrap();
        tokio::spawn(async move { axum::serve(listener, app).await.unwrap() });
        format!("http://{address}")
    }
    #[test]
    fn loose_json_contract() {
        for (content, action) in [
            (r#"{"action":"new"}"#, Action::New),
            (
                "```json\n{\"action\":\"update\",\"target\":\"m1\"}\n```",
                Action::Update,
            ),
            (
                r#"Sure: {"action":"supersede","content":"use {x}"} done"#,
                Action::Supersede,
            ),
        ] {
            assert_eq!(decode_decision(content).unwrap().action, action);
        }
        assert!(decode_decision("no json").is_err());
        let facts = decode_facts(
            r#"Here {"facts":[{"content":"User prefers tabs"},{"content":"  "}]} done"#,
        )
        .unwrap();
        assert_eq!(facts.len(), 1);
    }
    #[tokio::test]
    async fn openai_complete_and_consolidate() {
        let app=Router::new().route("/v1/chat/completions",post(|Json(body):Json<Value>|async move{let system=body["messages"][0]["content"].as_str().unwrap_or_default();if system.contains("long-term memory"){Json(json!({"choices":[{"message":{"content":"{\"action\":\"update\",\"target\":\"m1\",\"content\":\"merged\"}"}}]}))}else{Json(json!({"choices":[{"message":{"content":"the answer"}}]}))}}));
        let base = server(app).await;
        let client = OpenAiClient::new(&format!("{base}/v1"), "", "model", 0).unwrap();
        assert_eq!(client.complete("sys", "user").await.unwrap(), "the answer");
        let decision = client
            .consolidate(&Input {
                new_memory: "x".into(),
                tier: "semantic".into(),
                candidates: vec![],
            })
            .await
            .unwrap();
        assert_eq!(decision.action, Action::Update);
        assert_eq!(decision.target, "m1");
    }
    #[tokio::test]
    async fn anthropic_skips_thinking() {
        let app=Router::new().route("/v1/messages",post(||async{Json(json!({"content":[{"type":"thinking","thinking":"reason"},{"type":"text","text":"answer"}],"stop_reason":"end_turn"}))}));
        let base = server(app).await;
        let client = AnthropicClient::new(&base, "", "model", 0).unwrap();
        assert_eq!(client.complete("sys", "user").await.unwrap(), "answer");
    }
    #[test]
    fn provider_result_tool_calls() {
        let openai = json!({"choices":[{"message":{"content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"lookup","arguments":"{\"q\":\"x\"}"}}]}}]});
        let result = openai_result(&openai).unwrap();
        assert_eq!(result.calls[0].name, "lookup");
        let anthropic =
            json!({"content":[{"type":"tool_use","id":"call-2","name":"get","input":{"id":"x"}}]});
        assert_eq!(anthropic_result(&anthropic).unwrap().calls[0].id, "call-2");
    }
}
