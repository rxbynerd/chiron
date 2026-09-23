# Agentic knowledge: recall and remember across the Equestrianism suite

Status: design for the `feat/v2-knowledge-seam` stack on top of `--agent
worker` (PR #1). Binding for the implementation; decisions are mirrored in
`docs/DECISIONS.md`.

## 0. Why

The worker's web-search MCP client (`internal/researcher/fleet/search`)
calls a tool named `search` and expects
`{"results":[{"title","url","snippet"}]}`. Billet, the suite's memory
sidecar, exposes `search_memory` returning
`{"records":[{"memory_id","content","score","created_at"}]}` and
`save_memory`. Pointing `--fleet-search-endpoint` at Billet therefore
degrades to a single prose snippet at best. The two projects sit under the
same family and must compose.

Knowledge is also not web search. A memory record or a knowledge-base
fragment is organisational context the worker should consult before and
alongside the public web, and cite when it relies on it. It deserves its own
seam, its own action in the worker loop, and its own rendering, not a
remapping onto `title/url/snippet`.

This document defines:

- a reusable **knowledge seam**: the `Recaller` and `Rememberer` halves of
  the existing `internal/memory.ContextStore` interface, split out so an
  external store can satisfy recall and remember without the session and
  artifact planes;
- two adapters: **Billet** (MCP Streamable HTTP) and **Alexandria** (REST);
- a fourth worker action, **`recall`**, and an opt-in **save-back** of the
  finished finding;
- the config, CLI, trace, formatter and documentation changes that make the
  feature reachable and honest.

## 1. Relationship to existing decisions

- `docs/V2-RESEARCH-AGENT.md` §1 says workers have "exactly two read-only
  network tools" and "internal source tools are out of v2 ... route in later
  with Paddock/internal-source work". `docs/V2-AMENDS.md` amend 6 deferred
  internal sources "once Paddock is fully online". Billet and Alexandria exist
  now and Paddock does not; this design advances amend 6 on that basis. Both
  documents are amended in place with a pointer here. The research-only
  boundary (no shell, no file writes, no workspace mutation) is unchanged:
  `recall` is read-only and `remember` writes only to the suite's memory
  store, never to a workspace.
- `internal/memory.ContextStore` was "deliberately unimplemented" pending
  Paddock. Billet's `docs/DECISIONS.md` ("Billet stays separate from
  Paddock") left open whether Billet's backend contract and Chiron's
  `ContextStore` should converge. They converge here on the recall/remember
  half only. The session and artifact half (`OpenSession`, `Put`, `Get`) stays
  with the `fleet.memory` binding (Wave 4, issue #23) and is untouched.
- Issue #28 (a config file can choose both a credential and its destination)
  applies to the new endpoint and key-ref pair exactly as it does to the
  search pair. This design adds the pair under the same mitigations already in
  place (absolute https, loopback-only http, no userinfo/query/fragment,
  `secret://` only, values never echoed) and notes the pair in #28 rather than
  resolving #28 here.

## 2. The seam: `internal/memory`

Split the interface; do not change the types.

```go
// Recaller retrieves long-term memory semantically related to a query.
type Recaller interface {
	Recall(ctx context.Context, ns Namespace, q Query) ([]Recalled, error)
}

// Rememberer stores one item of durable long-term memory.
type Rememberer interface {
	Remember(ctx context.Context, ns Namespace, m Memory) (Reference, error)
}

// ContextStore is unchanged in shape: it embeds both plus the session and
// artifact methods.
type ContextStore interface {
	OpenSession(ctx context.Context, ref SessionRef) (Session, error)
	Put(ctx context.Context, ns Namespace, body io.Reader, meta ArtifactMeta) (Reference, error)
	Get(ctx context.Context, ref Reference) (io.ReadCloser, ArtifactMeta, error)
	Rememberer
	Recaller
}
```

`Noop` keeps satisfying all three. The package doc comment is rewritten: the
seam is now bound to external stores for recall/remember; the session and
artifact planes still await Paddock or the Wave 4 in-memory binding.

Field mapping for a `Recalled` hit:

| `memory` field | Billet `search_memory` record | Alexandria `GET /v1/search` result |
| --- | --- | --- |
| `Reference.Namespace` | the `Namespace` passed to `Recall` (Billet binds its namespace server-side; the value is informational) | the `space` field of the result, else the `Namespace` passed |
| `Reference.Digest` | `memory_id` | `ref` (`kb://fragment/<uuid>` or `kb://source/<uuid>#L..`) |
| `Reference.Locator` | `billet://memory/<memory_id>` | `<endpoint origin>/f/<id>` for a fragment hit (the web UI route); the `ref` itself for a chunk hit |
| `Memory.Text` | `content` | `snippet` |
| `Memory.Meta.Name` | first line of `content`, bounded to 120 runes (Billet has no title) | `title` |
| `Memory.Meta.MediaType` | `text/plain` | `text/markdown` |
| `Memory.Meta.Labels` | `created_at` | `kind`, `unit`, `space`, `stale` ("true" only when set), `updated_at` when present |
| `Score` | `score` | `score` |

`Query.Limit` is the per-recall hit cap; adapters clamp a non-positive limit
to their default (5) and never send more than 20.

`Memory` for `Remember` carries `Text` (the bounded content) and `Meta.Name`
(a label the store may ignore) plus `Labels`; Billet maps `Labels["kind"]`
(`fact` or `event`, default `fact`) onto `save_memory`'s `kind`.

## 3. Adapters

### 3.1 Shared MCP client: `internal/mcpclient`

Extract the transport half of `internal/researcher/fleet/search` into a
package that knows nothing about search:

```go
type Options struct {
	Endpoint       string        // validated: absolute https, http loopback only, no userinfo
	APIKey         string        // resolved literal; header-only; may be empty
	HTTPClient     *http.Client  // optional; shallow-copied so the redirect policy applies
	RequestTimeout time.Duration // bounds one CallTool (initialize + initialized + tools/call + DELETE)
	MaxBodyBytes   int64         // bounds every response body, JSON or SSE
	ClientName     string        // clientInfo.name, default "chiron"
	ClientVersion  string        // clientInfo.version, default "v2"
}

type ToolResult struct {
	Content           []ContentBlock
	StructuredContent json.RawMessage
	IsError           bool
}

type ContentBlock struct{ Type, Text string }

func New(opts Options) (*Client, error)
func (c *Client) CallTool(ctx context.Context, name string, args map[string]any) (ToolResult, error)
func FirstText(blocks []ContentBlock) string
```

Behaviour is exactly today's `search` transport: initialize, initialized
notification, `tools/call`, best-effort DELETE when a session id was
issued; both reply framings bounded; cross-host and https-to-http redirects
refused; the key only in `Authorization`; every diagnostic scrubbed (exact
key match first, then `secret.Scrub`); no retries on any round-trip. Error
strings are prefixed `mcp:`; a package that wraps the client may re-prefix.

`internal/researcher/fleet/search` keeps its exported API (`Options`,
`New`, `Client.Search`, `Result`, `FakeServer` and its options) and becomes
the result-shape layer over `mcpclient`. Its tests must keep passing; error
text assertions may be adjusted to the new prefix only where they asserted
the transport's own wording.

A generic `mcpclient.FakeServer` is shipped in `fake.go` (non-test build,
per the DECISIONS precedent for `search.FakeServer`) and scripted with a
handler `func(tool string, args map[string]any) (ToolResult, error)` plus
`WithSessionID`, `WithProtocolVersion`, `WithSSE`, and the same
`Requests()`, `CallCount()`, `ToolCallCount()` inspection surface. The
`search.FakeServer` may be rebased on it or left independent; its exported
API must not change.

Note for Billet: its server runs go-sdk Streamable HTTP in stateless,
JSON-response mode at the server root (not `/mcp`), issues no session id,
and requires the `Accept` header to be absent or to name both
`application/json` and `text/event-stream`. The client already sends the
latter, and stateless mode accepts the initialize handshake.

### 3.2 Billet: `internal/memory/billet`

```go
type Options struct {
	Endpoint       string        // Billet's MCP listener, e.g. http://127.0.0.1:8140/ or https://billet.internal/
	APIKey         string        // optional; Billet itself is unauthenticated, a fronting proxy may not be
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	DefaultLimit   int           // hits per recall when Query.Limit <= 0; default 5, max 20
}

func New(opts Options) (*Client, error)
var _ memory.Recaller = (*Client)(nil)
var _ memory.Rememberer = (*Client)(nil)
```

- `Recall` calls `search_memory` with `{"query": q.Text, "limit": n}`,
  reads `structuredContent` first, then a text block, as
  `{"records":[{memory_id, content, score, created_at}]}`. `isError: true`
  is an error carrying the tool's text ("budget exceeded", "backend
  unavailable", or the validation message). Unlike search, there is no
  graceful degradation to a prose snippet: a reply without a `records` key is
  an error, because the worker must never mistake arbitrary text for a
  memory.
- `Remember` calls `save_memory` with `{"content": m.Text, "kind": kind}`
  and returns `Reference{Digest: memory_id, Locator: "billet://memory/<id>"}`.
  `accepted: false` is an error. Content over 256 KiB is refused client-side
  with a clear error before any request (Billet's `MaxContentBytes`), as is
  a `kind` other than `fact` or `event`. A returned `memory_id` outside
  letters, digits and `-_.:` (at most 256 bytes) fails the call, because
  the id is embedded in a locator the worker renders and cites.
- Each record's `content` is bounded on read to `MaxHitBytes` (default 8
  KiB, rune-safe) before it is returned, so a huge memory cannot flood the
  transcript.
- Namespace: Billet binds one namespace per process and accepts none per
  call. `Recall`/`Remember` ignore the `Namespace` argument beyond copying
  it into the returned `Reference`.
- Ships `billet.FakeServer` (built on `mcpclient.FakeServer`) scripted with
  `[]Record` and options for a tool error and a raw result, plus a
  `SavedContents()` inspector so a save-back test can assert what was sent.
- Ships an interop test `TestLiveBillet` that is skipped unless
  `CHIRON_BILLET_BIN` names a `billet` binary: it starts `billet serve
  --listen 127.0.0.1:0`-equivalent (pick a free port, pass
  `--listen 127.0.0.1:<port>`), waits for readiness, remembers two facts,
  recalls one, and asserts the mapping. This is the proof that the adapter
  speaks to the real server, not only to our fake.

### 3.3 Alexandria: `internal/memory/alexandria`

REST, not MCP. Alexandria's `/mcp` targets the 2026-07-28 stateless MCP
revision behind non-standard headers and a `_meta` envelope; its own Claude
Code plugin recalls over `GET /v1/search` with a static bearer token, which
is the maintained, tested integration path. Chiron follows the plugin.

```go
type Options struct {
	Endpoint       string        // deployment origin, e.g. https://alexandria-api.example.workers.dev
	APIKey         string        // required: an alx_ static token (or OAuth bearer); header-only
	HTTPClient     *http.Client
	RequestTimeout time.Duration
	DefaultLimit   int           // hits per recall; default 5, max 20
	MaxTokens      int           // Alexandria's max_tokens budget for one search; default 4000
	AccessClientID, AccessClientSecret string // optional Cloudflare Access service-token headers
}

func New(opts Options) (*Client, error)
var _ memory.Recaller = (*Client)(nil)
```

- `Recall` issues `GET {Endpoint}/v1/search?q=<text>&mode=hybrid&max_tokens=<n>[&space=<ns>]`
  with `Authorization: Bearer <key>` and, when set, the two `cf-access-*`
  headers. The `Namespace` argument, when non-empty, is the space slug; it is
  validated against Alexandria's slug grammar
  (`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`, max 64) before the request.
- Redirects are never followed (a redirect would carry the bearer
  elsewhere); the body is bounded (default 1 MiB); `429` surfaces the
  `Retry-After` value in the error text; `401/403` errors never echo the
  token; the response is decoded strictly enough to reject a non-object.
- Results are mapped per §2. Both `fragment` and `chunk` units are
  returned; the first `DefaultLimit` (or `Query.Limit`) results are kept.
  `degraded` is surfaced as `Labels["degraded"]` on every hit so the worker
  transcript can say the store fell back to lexical search.
- No `Rememberer`: `kb_write_fragment` needs a citation model Chiron does
  not have yet. Config rejects `knowledge_remember` with this provider.
- Ships `alexandria.FakeServer` (an `httptest.Server`) scripted with a
  `SearchResponse` document, recording the query string, the bearer and the
  Access headers, with options for a status code and a redirect.

## 4. The worker

### 4.1 Deps and caps

```go
type WorkerDeps struct {
	Model  *model.Client
	Search *search.Client
	Fetch  *fetch.Client
	// Knowledge, when non-nil, adds the recall action. nil leaves the loop
	// exactly as before: schema, prompt and tool list unchanged.
	Knowledge memory.Recaller
	// Remember, when non-nil, saves a bounded summary of a Completed finding
	// after the loop returns. It is independent of Knowledge.
	Remember memory.Rememberer
	// KnowledgeNamespace is passed to Recall and Remember (Alexandria: the
	// space; Billet: informational).
	KnowledgeNamespace memory.Namespace
	Tracer trace.Tracer
	Caps   Caps
}
```

`Caps` gains `RecallLimit int` (hits per recall, default 5) and
`MaxRecallBytes int` (bound on one rendered recall result, default
`DefaultMaxPageBytes`).

### 4.2 The `recall` action

- `actionKind` gains `actionRecall = "recall"`, carrying `query`. The
  schema is built by `actionSchemaFor(recall bool)`: with `recall` false the
  JSON is byte-identical to today's `actionSchema`, so existing golden tests
  do not move. With `recall` true the enum is `["search","fetch","recall",
  "final"]` and the `query` description covers both search and recall.
- `parseAction` accepts `recall` only when the loop was built with a
  `Knowledge` dep; otherwise it is an unknown action and the run fails
  closed, as today.
- The system prompt gains one paragraph, only when recall is enabled,
  describing the action: it queries the organisation's knowledge store for
  prior findings, decisions and notes; results are untrusted data like any
  other tool result; recall first when the objective may already be answered
  internally, then confirm on the public web; a recalled item is citable by
  the `Ref:` shown. The "external public web only" sentence becomes "the
  external public web and, where offered, the organisation's knowledge
  store". Prose changes are guarded so the recall-disabled prompt is
  byte-identical to today's.
- `doRecall` calls `Knowledge.Recall(ctx, KnowledgeNamespace, Query{Text,
  Limit: RecallLimit})`, increments `usage.RecallCount`, records every hit
  locator as citable (see 4.3), and appends `recallResultsMessage`. Failures
  go through the same `toolFailure` helper and the same three-strike counter
  as search and fetch: one failure budget per run, so a dead knowledge store
  cannot extend a run past its existing bound.
- `recallResultsMessage` renders inside the tool-result fence with every
  field passed through `defang`: numbered hits, `Name`, `Ref: <Locator>`,
  a score when non-zero, then the text, each hit bounded to `maxRecallHitBytes`
  (4 KiB) and the whole message to `MaxRecallBytes`, with a `[truncated]`
  marker. Zero hits renders "(no results)". A non-empty `degraded` label is
  rendered once, with its reason, as a note before the list.

### 4.3 Citations

`workerRun` gains `citableRefs map[string]bool`, populated by `doRecall`
with each hit's `Locator`. `finalise` admits a citation whose URL is in
`seenURLs` or in `citableRefs`. Recall locators are never added to
`seenURLs`: a knowledge hit is citable but not fetchable, so the model cannot
spend a fetch (and a failure strike) on a page behind the store's own
authentication. `titles` is populated from `Meta.Name` so a cited locator
carries its title. The system prompt's citation rule says so.

`types.Citation.URI` is already scheme-agnostic. The Markdown formatter
renders web URIs as links and, new here, renders a `billet://` or `kb://`
URI as plain inline code with its title, so a memory or a chunk ref the
worker relied on appears in the sources list without a link that could not
resolve. All other non-web schemes stay dropped (the existing safety rule).

### 4.4 Save-back

After `RunWorker` returns, `Worker` (the `Researcher` wrapper) calls
`rememberFinding` when `deps.Remember != nil` and the finding is
`Completed` with non-empty text:

- `Memory.Text` is: the objective, a blank line, the answer and, when the
  finding has citations, a blank line, `Sources:` and one locator per line;
  the whole bounded to
  `maxRememberBytes` (32 KiB, rune-safe, `[truncated]` marker).
  `Meta.Name` is the objective bounded to 120 runes; `Labels` carry
  `kind: fact`, `agent: worker`, `interaction_id`.
- It runs synchronously after the loop returns and before the run is marked
  done, under a 30 s context derived from the run's context
  (`context.WithoutCancel` plus a timeout), so a slow store delays `Await`
  by at most 30 s and no goroutine outlives the run.
- Failure never changes the run's status. The outcome goes on the worker
  span (`remember_ref` or `remember_error`, scrubbed) and to stderr through
  the `Logger` injected in `WorkerDeps` (bound by the composition root to a
  scrub-wrapped handler on the command's stderr; nil discards). The `Interaction` records the reference in
  `Metadata["remember_ref"]` if `types.Interaction` has a metadata map;
  otherwise only the span and log carry it (check `internal/types` first
  and do not add a field for this alone).

### 4.5 Usage, trace and tool list

- `types.Usage` gains `RecallCount int `json:"recall_count,omitempty"``.
- `trace.MetricRecallCount = "recall_count"`; `internal/run` emits it
  beside `MetricSearchCount` (a one-line addition; the run core still
  depends only on seams). The worker span gets `recall_count`.
- The formatter's front matter gains `recalls` (omitempty) so existing
  golden files are unchanged.
- `workerTools()` returns `web_search, web_fetch` plus `knowledge_recall`
  when `Knowledge` is set, plus `knowledge_remember` when `Remember` is set.

## 5. Config and CLI

`FleetConfig` gains:

| Field | JSON/YAML key | Flag | Rule |
| --- | --- | --- | --- |
| `KnowledgeProvider string` | `knowledge_provider` | `--fleet-knowledge-provider` | `""`, `billet` or `alexandria` |
| `KnowledgeEndpoint string` | `knowledge_endpoint` | `--fleet-knowledge-endpoint` | `validEndpoint`; required by the composition root when a provider is set |
| `KnowledgeKeyRef string` | `knowledge_key_ref` | `--fleet-knowledge-key-ref` | `validKeyRef`; the composition root requires it for `alexandria` |
| `KnowledgeSpace string` | `knowledge_space` | `--fleet-knowledge-space` | only with `alexandria`; slug grammar |
| `KnowledgeLimit int` | `knowledge_limit` | `--fleet-knowledge-limit` | default 5; 1..20 |
| `KnowledgeRemember bool` | `knowledge_remember` | `--fleet-knowledge-remember` | only with `billet` |

Validation lives in `FleetConfig.validate` and runs for `worker`/`fleet`
only. With an empty provider, every other `knowledge_*` field must be empty
or default, so a stray endpoint can never be silently ignored. The
rejected-lever hint for `mcp` becomes "use fleet.search_endpoint or the
fleet.knowledge_* fields".

`buildWorker` in `internal/cli/research.go` builds the adapter by provider
after the search client and before the fetch client, resolving
`KnowledgeKeyRef` through `secret.Default()` only when set (Billet may be
keyless; Alexandria without a key is a composition-root error), with
`RequestTimeout` equal to the worker call timeout. It sets `Knowledge`,
`Remember` (Billet with `knowledge_remember`), and `KnowledgeNamespace`
(`knowledge_space`).

`examples/researchconfig/worker.yaml` gains a commented `knowledge_*` block;
`README.md` gains the flags and a Billet example; `chiron research-config`
prints the fields automatically.

## 6. Documentation

- `docs/DECISIONS.md`: one dated entry, "Knowledge recall and remember via
  the memory seam", covering: the seam split; adapters and their wire
  shapes (no degrade-to-prose for knowledge, unlike search); REST for
  Alexandria and why; the shared `mcpclient` extraction (partially addresses
  issue #15's duplication note; issue #16 remains); the fourth action and the
  shared failure budget; citable-not-fetchable locators; save-back as an
  opt-in write to the suite's memory store, qualifying V2-RESEARCH-AGENT §1
  and amend 6; the #28 note.
- `docs/V2-RESEARCH-AGENT.md` §1: amend the two bullets in place with a
  short pointer to this document rather than rewriting them.
- `docs/V2-AMENDS.md` amend 6: append an italic superseded note.
- `AGENTS.md`: package map rows for `internal/mcpclient`,
  `internal/memory/billet`, `internal/memory/alexandria`; the
  `internal/memory` row rewritten; the security-sensitive configuration
  section extended with the knowledge pair; test conventions extended with
  the two new fakes and the `CHIRON_BILLET_BIN` interop switch.
- `internal/memory` package comment rewritten.

## 7. Tests

- `mcpclient`: the transport tests moved from `search` (JSON and SSE
  framings, bounds, redirects, session echo, DELETE, key scrubbing, no
  retry), against the generic fake.
- `search`: existing tests unchanged in intent.
- `billet`: mapping, limit clamp, tool error, missing `records` key,
  oversized content refused, save-back arguments, key never in errors; the
  live interop test behind `CHIRON_BILLET_BIN`.
- `alexandria`: mapping for fragment and chunk hits, space slug validation,
  bearer and Access headers, redirect refused, 429 and 401 handling, body
  bound, degraded label.
- `fleet`: recall disabled leaves schema and prompt byte-identical;
  recall action renders, defangs, bounds and makes locators citable but not
  fetchable; recall failures share the strike counter; save-back content,
  bounds and failure isolation; `workerTools`.
- `config`: every rule in §5, table-driven.
- `cli`: an end-to-end worker run against `model.FakeServer`,
  `search.FakeServer` and `billet.FakeServer` where the model recalls,
  searches, fetches and cites a `billet://` locator; a run with
  `knowledge_remember` asserting the saved content; a config with a
  provider but no endpoint failing before any resolver call.
- `formatter`: a golden case with a `billet://` citation.

## 8. Security

- The knowledge endpoint and key ref are as sensitive as the search pair:
  the key travels to whatever endpoint the config names. Same validation,
  same never-echo rule, same header-only carriage, same scrubbing.
- Recall results and remembered content are untrusted data inside the
  fence; nothing in a hit can become an instruction or close the fence.
- Save-back sends the finding, which was built from public-web content, to
  the configured store. It is opt-in, off by default, bounded, and named on
  the tool list so an operator reading the Interaction sees it. It is never
  a workspace write.
- The Alexandria client never follows a redirect and refuses a non-https
  endpoint outside loopback, matching the plugin's own credential guard.
