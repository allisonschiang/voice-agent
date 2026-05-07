# voice-agent

Three composable generic services for a voice (or text) command pipeline:

- **`audio-to-text`** — owns the mic, streams to Google STT, dispatches transcripts downstream
- **`text-router`** — phrase-table dispatcher with optional LLM fallback and spoken acknowledgments
- **`ai-responder`** — Anthropic Claude + TTS, also handles direct speech and domain events

All three are independently usable. They communicate via `DoCommand` over the generic API.

---

## Model: `allisonorg:voice-agent:audio-to-text`

Captures audio from a configured `audio_input` component, streams it to Google Cloud Speech-to-Text, trims an optional wake word, and forwards each finalized transcript to a configured downstream generic service as `{"transcript": "<text>"}`.

### Configuration

```json
{
  "audio_input": "wake-word-filter",
  "google_credentials": "<path-or-json-string>",
  "wake_word": "hey gary",
  "downstream": "text-router",
  "test": false
}
```

| Field | Required | Description |
|---|---|---|
| `audio_input` | yes | Name of the `audio_in` component to read from. Typically a wake-word filter that gates the stream. |
| `google_credentials` | no | Either a path to a Google service-account JSON file, the JSON content as a string, or an inline object. If omitted, falls back to default Google client auth. |
| `wake_word` | no | Phrase trimmed from the start of each transcript (case-insensitive). The wake-word filter handles detection; this just strips the leftover word from the transcribed text. |
| `downstream` | no | Name of a generic service that receives each transcript via `DoCommand({"transcript": "<text>"})`. Typically a `text-router`. If empty, transcripts are logged only. |
| `test` | no | If true, single-shot recordings cap at 5s instead of 40s. |

### DoCommand

| Command | Effect |
|---|---|
| `{"command": "listen"}` | Start a continuous listen loop. The wake-word filter gates utterances; each finalized transcript is dispatched downstream. |
| `{"command": "stop"}` | Stop whatever is running (listen or record). |
| `{"command": "record"}` | Single-shot capture (max 40s, or 5s if `test: true`). |
| `{"command": "text"}` | Return the last transcript. |

---

## Model: `allisonorg:voice-agent:text-router`

Domain-agnostic string → DoCommand dispatcher. Given an input string, looks it up in a config-driven phrase table and dispatches the matching DoCommand to a configured target. Optional LLM fallback for unmatched inputs. Optional spoken acknowledgments after each dispatch.

Matching is case-insensitive **exact** match after whitespace trim. No substring, no fuzzy. Predictable; no surprise triggers.

### Configuration

```json
{
  "routes": [
    {
      "say": "wipe the board",
      "target": "chess",
      "do": { "wipe": true },
      "ack": "Wiping the board.",
      "ack_failure": "I couldn't wipe the board."
    },
    {
      "say": "play one move",
      "target": "chess",
      "do": { "go": 1 },
      "ack": "Thinking..."
    }
  ],
  "fallback": "ai-responder",
  "ack_target": "ai-responder"
}
```

| Field | Required | Description |
|---|---|---|
| `routes` | yes | Array of route objects (see below). Each maps a phrase to a DoCommand on a target. |
| `fallback` | no | Generic service that receives unmatched inputs as `{"process": "<text>"}`. Typically an `ai-responder`. If unset, unmatched inputs are dropped. |
| `ack_target` | no | Generic service that receives `{"speak": "<ack>"}` after a successful (or failed) dispatch. Typically the same `ai-responder`. If unset, no ack is spoken. |

### Route object

| Field | Required | Description |
|---|---|---|
| `say` | yes | Input phrase. Matched case-insensitively after trim. |
| `target` | yes | Name of the target generic service to dispatch to. |
| `do` | yes | DoCommand payload sent to `target` on match. |
| `ack` | no | Spoken confirmation after a successful dispatch. Sent to `ack_target` as `{"speak": "<ack>"}`. |
| `ack_failure` | no | Spoken message if `target.DoCommand` errored. |

### DoCommand

| Command | Effect |
|---|---|
| `{"transcript": "<text>"}` or `{"input": "<text>"}` | Match and dispatch. Returns `{handled, matched, target, action, response, spoken}`. |
| `{"test": "<text>"}` | Dry-run match (no dispatch). Useful for validating route tables. |
| `{"list-phrases": true}` | Return the configured phrase table. |

---

## Model: `allisonorg:voice-agent:ai-responder`

Receives text input, optionally augments with context fetched from a configured service, calls Anthropic Claude, and optionally speaks the result via a TTS service. Also handles direct speech (`speak`) and domain events (`event`) without going through Claude when not needed.

### Configuration

```json
{
  "anthropic_api_key": "sk-ant-...",
  "prompt": "You are Gary, a helpful chess robot. The current board state is: \"{context}\". Give super concise responses.",
  "model": "claude-haiku-4-5",
  "max_tokens": 256,

  "context_service": "chess",
  "context_command": { "board-snapshot": true },
  "context_field": "fen",

  "tts": "text-to-speech",
  "audio_input": "wake-word-filter",
  "follow_up_window_seconds": 10,

  "engine_move_template": "I (Gary) just played {move}. Comment briefly in first person, one short sentence.",
  "human_move_template": "My opponent just played {move}. React briefly, one short sentence."
}
```

| Field | Required | Description |
|---|---|---|
| `anthropic_api_key` | no | API key for Anthropic. If omitted, the SDK reads from `ANTHROPIC_API_KEY` env var. |
| `prompt` | no | System prompt template. Supports `{context}` placeholder, replaced with the result of `context_service` (or appended if no `{context}` is present). Default: `"Analyze the following transcript:"`. |
| `model` | no | Claude model name. Default: `claude-haiku-4-5`. |
| `max_tokens` | no | Cap on Claude response length. Default: `256`. |
| `context_service` | no | Generic service queried before each Claude call. The result is interpolated into the system prompt's `{context}` placeholder. |
| `context_command` | no | DoCommand payload sent to `context_service`. e.g. `{"board-snapshot": true}`. |
| `context_field` | no | If set, extract this key from the `context_service` response instead of marshaling the whole map. |
| `tts` | no | Generic service that receives `{"say": "<text>"}` for spoken output. Typically `viam:beanjamin:text-to-speech`. |
| `audio_input` | no | If set, ai-responder pauses wake-word detection during TTS playback to prevent self-retrigger. |
| `follow_up_window_seconds` | no | If > 0, after TTS finishes, opens a bypass window on `audio_input` so the user can reply without saying the wake word for this many seconds. Requires `audio_input` to support `{"open_window": <seconds>}` (e.g. `allisonorg:filtered-audio-fix:wake-word-filter`). Each TTS resets the window — supports back-and-forth conversation. Default: `0` (disabled). |
| `engine_move_template` | no | Template used when ai-responder receives `{"event": "move_made", "by": "engine", ...}`. Supports `{move}`, `{fen}`, `{by}` placeholders. Default: `"I (Gary) just played {move}. Comment briefly in first person, one short sentence."` |
| `human_move_template` | no | Template used when `by: "human"`. Same placeholder support. Default: `"My opponent just played {move}. React briefly, one short sentence."` |

**Live-editable templates:** `engine_move_template` and `human_move_template` are read on every move event from in-memory state populated by `Reconfigure`. Edit them in app.viam.com and save — changes take effect on the next event with no module reload.

### DoCommand

| Command | Effect |
|---|---|
| `{"process": "<text>"}` / `{"ask": "<text>"}` | Run the full pipeline: optional context fetch → Claude → optional TTS. Returns `{result, spoken}`. |
| `{"speak": "<text>"}` | Speak text directly via TTS, bypassing Claude. Returns `{spoken}`. |
| `{"result": true}` | Return the last Claude result string. |
| `{"event": "<name>", ...}` | Translate a domain event into a `process` call. Currently supports `event: "move_made"` with `move`, `fen`, `by` fields; uses `engine_move_template` / `human_move_template` for the user-content text. |
