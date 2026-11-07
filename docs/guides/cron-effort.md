# Per-job thinking effort

The `cron` tool accepts an optional `effort` for agent/message jobs. It changes
thinking controls for that firing only, not the selected model or global config.
Effort does not affect shell execution itself. If command output is routed to an
agent for interpretation, the override applies to that interpretation.

Canonical values are `off`, `low`, `medium`, `high`, `xhigh`, `max`, and
`adaptive`. Input is trimmed and case-normalized; other values are rejected.

```json
{"action":"add","message":"Review recent errors","every_seconds":3600,"effort":"high"}
```

Omitting `effort` (or supplying an empty string) on add inherits model defaults.
On update, omission or empty string preserves the saved override. To restore
inheritance, update the job with `clear_effort: true`. This cannot be combined
with nonempty `effort`; `clear_effort` is update-only. Get output includes the saved effort when set; the brief list summary does not. Existing jobs without it retain inherited behavior.

The override is request-scoped across turns and fallback attempts for that
firing. It must not change later scheduled or interactive requests, model
configuration, or shared provider `extra_body` maps.

## Provider limits

This is a portable intent, not a promise of identical token budgets. Unsupported
models or transports omit native thinking controls rather than guessing field
support. Unlisted model names and custom deployment aliases may therefore ignore
the override. `off` means the minimum supported setting where disabling thinking
is not supported; it does **not** always disable reasoning.

- Native OpenAI-compatible HTTP, Azure, and OAuth Responses mappings use a conservative
  allowlist of known reasoning models. `max`/`xhigh` clamp to the strongest
  supported level. `adaptive` leaves the backend default unchanged. Some models
  map `off` to `low`, `minimal`, or even `high` rather than `none`.
- Codex WebSocket retains its existing native effort negotiation; its `off` and
  `adaptive` omit explicit effort and do not guarantee reasoning is disabled.
- With an explicit cron override, Gemini uses thinking levels for supported Gemini 3 models and token budgets
  for supported Gemini 2.5 models. Pro cannot disable thinking (`off` uses `low`
  on Gemini 3 Pro or 128 tokens on Gemini 2.5 Pro). Gemini 2.5 Flash uses zero;
  Flash-Lite conservatively uses its minimum explicit budget of 512 tokens.
- Native Anthropic uses manual budgets for supported thinking models and
  adaptive thinking only for allowlisted Claude 4.6 models. Manual budgets are
  bounded below `max_tokens` and omitted if there is insufficient token room.
  `adaptive` currently uses high output effort. The separate
  `anthropic_messages` transport does not implement this override.

For supported mappings, a nonempty request effort takes precedence over static
thinking fields supplied through provider `extra_body`. Without a request
override, existing static-body precedence is retained. Unrelated extra-body
fields are preserved. Unsupported mappings do not remove arbitrary static
fields: a static provider setting may still apply.

No provider/API calls are necessary to validate scheduling and mapping locally;
real model availability and API-version support still require deployment-time
verification.
