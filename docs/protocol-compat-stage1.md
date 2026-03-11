# Protocol Compatibility Stage 1

## Goals

- Fix protocol behaviors that are objectively incorrect or leak client semantics.
- Improve compatibility without widening scope into a full protocol rewrite.
- Add tests that lock in the corrected behavior.

## Scope

### 1. Preserve provider-authored protocol semantics

- Keep OpenAI `developer` role intact when forwarding to OpenAI Chat Completions.
- Do not silently rewrite `developer` to `system` inside the OpenAI adapter.

### 2. Preserve upstream authentication and beta headers safely

- Never forward client authentication headers such as `Authorization` or `X-API-Key` to upstream providers.
- Never let client auth headers override provider credentials generated from channel configuration.
- Preserve non-auth protocol headers that may unlock official features, such as `Anthropic-Beta`.

### 3. Complete Gemini structured output mapping

- Map internal `response_format.type=json_schema` into Gemini `generationConfig.responseSchema`.
- Support both direct JSON Schema payloads and OpenAI-style wrapped payloads that contain `schema`.
- Reuse the existing Gemini schema cleaner before conversion.

## Non-goals

- Full OpenAI Responses built-in tool coverage.
- Full Anthropic beta block family support.
- Gemini inbound compatibility.
- Capability matrix and UI exposure.

## Design notes

### Header forwarding

- Request adapters remain responsible for setting provider auth headers.
- Relay header copy only forwards headers when the adapter has not already set them.
- Relay strips client auth headers unconditionally.

### Gemini schema conversion

- Extract the effective schema object from either:
  - a direct JSON Schema object, or
  - an OpenAI wrapper containing `schema`.
- Clean the schema with the existing Gemini transformer.
- Marshal/unmarshal into `GeminiSchema` to keep changes localized.

## Test plan

- Update OpenAI passthrough tests to assert `developer` is preserved.
- Add Gemini tests for direct and wrapped JSON Schema conversion.
- Add relay integration test to verify:
  - `Anthropic-Beta` reaches upstream
  - client `X-API-Key` does not override channel credentials

## Follow-up after Stage 1

- Build a provider capability registry.
- Expand Responses tool and item coverage.
- Add golden protocol fixtures from official examples.
