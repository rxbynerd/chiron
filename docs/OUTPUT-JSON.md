# The `--output json` document

`chiron research -o json`, `chiron get -o json` and `chiron follow-up -o json`
write the run result to stdout as one JSON document followed by a newline.
Diagnostics and run events go to stderr, so stdout can be piped to a JSON
parser unchanged. The document is the encoding of `types.RunResult`
(`internal/types/types.go`), written by `internal/sink/json.go`.

The exact bytes for four representative results are pinned by the golden
files in `internal/sink/testdata` (`run-result*.json`). A change to a key, a
type or an omission rule therefore appears as a reviewed diff in those files.

The document contains no thought summaries and no citation list. Thought
summaries are `delta` events on stderr (see `README.md`, "Output surfaces"),
and a run's sources appear only inside the Markdown at `report.markdown`.

## Encoding rules

- The output is compact JSON with a trailing newline. Key order is the order
  below.
- A key marked "omitted when empty" is absent, never `null` or `0`, when its
  value is the zero value (empty string, `0`, nil or empty list). A consumer
  must treat an absent key as zero.
- Byte-slice fields (`report.markdown` and asset `data`) are base64 strings in
  the standard alphabet with padding, because Go's `encoding/json` encodes
  `[]byte` that way. A consumer must decode `report.markdown` to read the
  report. The decoded value is UTF-8 Markdown.
- These two keys are not omitted when empty, and the encoder writes `null`
  for a nil byte slice (an empty, non-nil slice is `""`). The Markdown
  formatter always produces a non-empty `report.markdown`, so that key is a
  string in every document `chiron` writes. An asset's `data` is the image
  bytes the researcher returned, so an image the API sent without data
  encodes as `"data": null`. A consumer should treat `null` as empty.
- The encoder escapes `<`, `>` and `&` inside strings as `\u003c`, `\u003e`
  and `\u0026`, so a consumer must decode the JSON rather than match raw
  bytes.

## Top level

| Key | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `interaction_id` | string | always | The id of the run. `chiron get <id>` resumes it, except for the opaque `wkr_` and `flt_` ids that the in-process agents produce. |
| `status` | string | always | The terminal state of the run: `completed`, `failed`, `cancelled`, `incomplete` or `budget_exceeded`. The document is written only after the run reaches a terminal state. |
| `report` | object | omitted when empty | The formatted report; see below. A failed or cancelled run normally still carries a placeholder report. |
| `usage` | object | omitted when all its fields are zero | Cost and activity counters; see below. |
| `duration_ns` | integer | omitted when zero | Wall-clock duration of this invocation in nanoseconds, not a string. The "Durations serialise as strings" decision in `docs/DECISIONS.md` applies to configuration only. |

## `report`

| Key | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `markdown` | string (base64) | always, not omitted when empty | The full Markdown document: YAML front matter, body and numbered sources. Base64 of the UTF-8 bytes. |
| `assets` | array of objects | omitted when empty | Chart images the Markdown links to by name. |

## `report.assets[]`

| Key | Type | Presence | Meaning |
| --- | --- | --- | --- |
| `name` | string | always | The file name the Markdown refers to, for example `chart-1.png`. |
| `mime_type` | string | omitted when empty | The media type, for example `image/png`. |
| `data` | string (base64) or `null` | always, not omitted when empty | The file contents; `null` when the asset has no bytes. |

## `usage`

Every key is an omitted-when-zero number. The set describes what a run
reports today and may change as cost handling changes; a consumer that
depends on the cost keys should tolerate their absence.

| Key | Type | Meaning |
| --- | --- | --- |
| `input_tokens` | integer | Input tokens the model reported. |
| `cached_tokens` | integer | Input tokens served from a cache. |
| `output_tokens` | integer | Output tokens the model reported. |
| `tool_use_tokens` | integer | Tokens attributed to tool use. |
| `thought_tokens` | integer | Tokens spent on reasoning. |
| `search_count` | integer | Web searches made. |
| `recall_count` | integer | Knowledge-store recalls made (worker and fleet runs). |
| `poll_count` | integer | Status polls made while awaiting the run. |
| `reconnect_count` | integer | Re-attachments to the event stream. |
| `estimated_cost_gbp` | number | Estimated model spend in pounds sterling. Model spend only. |
| `web_cost_usd` | number | Spend that web search and fetch providers reported, in US dollars, rounded to the micro-dollar. Never summed with the sterling figure. |
| `web_cost_unknown_calls` | integer | Web calls that may have been billed an unreported amount. While above zero, `web_cost_usd` is a lower bound. |

Which counters are populated depends on the agent: the token counters come
from the Gemini Interactions API and from the standard model of a worker or
fleet run, and `recall_count` is set only by worker and fleet runs.

## Example

A completed run with one chart, shown indented for reading (the real output
is a single line):

```json
{
  "interaction_id": "v1_golden",
  "status": "completed",
  "report": {
    "markdown": "IyBSZXBvcnQKCiFbQ2hhcnQgMV0oY2hhcnQtMS5wbmcpCg==",
    "assets": [
      {"name": "chart-1.png", "mime_type": "image/png", "data": "cG5nLWJ5dGVz"}
    ]
  },
  "usage": {"search_count": 80, "estimated_cost_gbp": 1.58},
  "duration_ns": 94500000000
}
```
