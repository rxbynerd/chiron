// Package gemini binds the Researcher seam to the Gemini Deep Research
// agent over internal/interactions — a hand-rolled net/http adapter, no
// vendor SDK (AGENTS.md ground rules). It owns everything wire-shaped:
// tier-name mapping, agent_config and tools assembly, multimodal input
// parts, prompt-template application, and the wire→domain mapping onto
// internal/types. docs/INTERACTIONS-API.md is normative throughout.
package gemini

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"github.com/rxbynerd/chiron/internal/interactions"
	"github.com/rxbynerd/chiron/internal/researcher"
	"github.com/rxbynerd/chiron/internal/types"
)

// Tier names: Chiron's flag values, mapped here — and only here — to
// the wire agent identifiers (docs/INTERACTIONS-API.md §2). The strings
// match internal/config's Agent constants; the adapter deliberately
// does not import config (the dependency points config → adapter via
// the CLI, never back).
const (
	TierDeepResearch    = "deep-research"
	TierDeepResearchMax = "deep-research-max"
)

// Options configures the adapter. APIKey is the resolved secret value
// (the CLI dereferences the secret:// reference); everything else
// mirrors the research flag surface (PROPOSAL §4.3).
type Options struct {
	// APIKey is the resolved Gemini API key. Required.
	APIKey string
	// BaseURL overrides the production endpoint — httptest servers in
	// tests, never in production use.
	BaseURL string
	// HTTPClient is the underlying client shared by the create and
	// poll clients; nil builds one.
	HTTPClient *http.Client
	// Tier is TierDeepResearch (default when empty) or
	// TierDeepResearchMax.
	Tier string
	// Visualise asks for charts: visualization "auto" plus a prompt
	// nudge (PROPOSAL §4.3). Off sends "off".
	Visualise bool
	// ThinkingSummaries asks the agent to produce thought summaries
	// ("auto"); quiet (polling) runs leave it false ("none") — there is
	// nothing to display them on. The CLI sets it from cfg.Stream.
	ThinkingSummaries bool
	// Stream awaits by SSE — rendering progress as it arrives and
	// reconnecting on drops — instead of polling. Falls back to polling
	// when streaming repeatedly fails (see awaitStream).
	Stream bool
	// OnThought, when set, observes each streamed thought summary. It
	// is called synchronously from the streaming await and must not
	// block; the CLI binds it to the transport's delta events.
	OnThought func(text string)
	// OnStreamDegraded, when set, observes the one-way degradation
	// from streaming to polling after the failure budget is spent —
	// the in-flight signal that the run is still progressing, just
	// less prettily. Called at most once per await; must not block.
	OnStreamDegraded func()
	// Tools overrides the default tool set (google_search, url_context,
	// code_execution). Only those three names are valid here; MCP and
	// file_search arrive via their own fields.
	Tools []string
	// MCP attaches remote MCP servers, name → URL, in name order.
	MCP map[string]string
	// FileSearch names file-search stores to search as one
	// file_search tool.
	FileSearch []string
	// Inputs are local paths or http(s) URLs for multimodal grounding:
	// local files travel as base64 data parts, URLs as uri parts.
	Inputs []string
	// TemplatePath selects the output-format prompt template; empty
	// uses the built-in template. The template must reference
	// {{.Query}}.
	TemplatePath string
	// PollInterval and PollMaxInterval override the polling cadence —
	// for tests; the defaults suit tasks that run for minutes.
	PollInterval    time.Duration
	PollMaxInterval time.Duration
	// ReconnectBaseDelay and ReconnectMaxDelay override the backoff
	// between streaming reconnect attempts — for tests; the defaults
	// (500ms doubling to 8s) mirror the client's retry backoff.
	ReconnectBaseDelay time.Duration
	ReconnectMaxDelay  time.Duration
}

// Researcher is the gemini-deep-research binding of the Researcher
// seam. One instance serves one run at a time: Start records the query
// and resets the poll counter that Result later reports.
type Researcher struct {
	// create has automatic retries disabled: retrying POST
	// /interactions after an ambiguous 5xx could start — and pay for —
	// a second research task (£1–7 per the tier envelope). Failures
	// surface to the user, who retries knowingly. Get and poll keep
	// the client's default retries: GET is idempotent.
	create *interactions.Client
	poll   *interactions.Client

	agentID   string
	model     string // follow-up Q&A mode: create with model, not agent
	cfg       *interactions.AgentConfig
	tools     []interactions.Tool
	toolNames []string
	prompt    *promptTemplate
	estimate  float64
	inputs    []string
	pollCfg   interactions.PollConfig
	streamCfg streamConfig

	// inFlight enforces the one-run-at-a-time contract at runtime: a
	// second Start while one is in flight fails instead of silently
	// corrupting lastQuery and pollCount (the v2 fleet orchestrator
	// must give each concurrent task its own Researcher).
	inFlight  atomic.Bool
	pollCount atomic.Int64
	// reconnectCount tallies streaming re-dials after a drop — the
	// reconnect_count cost signal (PROPOSAL §4.5), reported via Usage.
	reconnectCount atomic.Int64
	mu             sync.Mutex
	started        bool // Start ran: lastQuery and toolNames describe the interaction
	lastQuery      string
}

var _ researcher.Researcher = (*Researcher)(nil)

// New builds the adapter, resolving everything that can fail before any
// money is spent: tier and tool names are validated, the template is
// parsed, and the clients are constructed.
func New(opts Options) (*Researcher, error) {
	agentID, estimate, err := tierAgent(opts.Tier)
	if err != nil {
		return nil, err
	}
	tools, toolNames, err := assembleTools(opts)
	if err != nil {
		return nil, err
	}
	prompt, err := loadTemplate(opts.TemplatePath, opts.Visualise)
	if err != nil {
		return nil, err
	}

	create, poll, err := newClients(opts)
	if err != nil {
		return nil, err
	}

	return &Researcher{
		create:    create,
		poll:      poll,
		agentID:   agentID,
		cfg:       agentConfig(opts),
		tools:     tools,
		toolNames: toolNames,
		prompt:    prompt,
		estimate:  estimate,
		inputs:    opts.Inputs,
		pollCfg: interactions.PollConfig{
			Interval:    opts.PollInterval,
			MaxInterval: opts.PollMaxInterval,
		},
		streamCfg: streamConfigFrom(opts),
	}, nil
}

// newClients builds the create/poll client pair every adapter entry
// point shares: the create client with automatic retries disabled (a
// retried POST /interactions after an ambiguous 5xx could start — and
// pay for — a second task), the poll client with the default retries,
// because GET is idempotent.
func newClients(opts Options) (create, poll *interactions.Client, err error) {
	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	common := []interactions.Option{interactions.WithHTTPClient(httpClient)}
	if opts.BaseURL != "" {
		common = append(common, interactions.WithBaseURL(opts.BaseURL))
	}
	create, err = interactions.New(opts.APIKey, append(common, interactions.WithMaxRetries(0))...)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini: %w", err)
	}
	poll, err = interactions.New(opts.APIKey, common...)
	if err != nil {
		return nil, nil, fmt.Errorf("gemini: %w", err)
	}
	return create, poll, nil
}

// EstimatedCostGBP returns the planning cost estimate for one task of
// this researcher's binding — the figure the CLI budget gate compares
// against the cap before any create. Zero in follow-up mode: model-
// priced Q&A is outside the tier table (see cost.go).
func (r *Researcher) EstimatedCostGBP() float64 { return r.estimate }

// tierAgent maps a tier name to its wire agent identifier and planning
// cost estimate. An unknown tier is rejected here, before a request is
// built — a misspelt tier must not silently select a different spend.
func tierAgent(tier string) (agentID string, estimateGBP float64, err error) {
	switch tier {
	case TierDeepResearch, "":
		return interactions.AgentDeepResearch, estimatedCostGBP(TierDeepResearch), nil
	case TierDeepResearchMax:
		return interactions.AgentDeepResearchMax, estimatedCostGBP(TierDeepResearchMax), nil
	default:
		return "", 0, fmt.Errorf("gemini: unknown agent tier %q (want %q or %q)", tier, TierDeepResearch, TierDeepResearchMax)
	}
}

// agentConfig assembles the agent_config block
// (docs/INTERACTIONS-API.md §3). Collaborative planning is always false
// here: the M4 planning flow flips it per request.
func agentConfig(opts Options) *interactions.AgentConfig {
	cfg := &interactions.AgentConfig{
		Type:                  interactions.AgentConfigDeepResearch,
		ThinkingSummaries:     interactions.ThinkingSummariesNone,
		Visualization:         interactions.VisualizationOff,
		CollaborativePlanning: false,
	}
	if opts.ThinkingSummaries {
		cfg.ThinkingSummaries = interactions.ThinkingSummariesAuto
	}
	if opts.Visualise {
		cfg.Visualization = interactions.VisualizationAuto
	}
	return cfg
}

// assembleTools builds the wire tool set and the domain tool-name list
// recorded on the Interaction (the API does not echo the tool set
// back). Order is deterministic: base tools as given, MCP servers in
// name order, file_search last.
func assembleTools(opts Options) ([]interactions.Tool, []string, error) {
	base := opts.Tools
	if len(base) == 0 {
		// The API would apply these server-side when tools is omitted,
		// but emitting them explicitly keeps the request — and the
		// recorded tool set — independent of server-side defaults
		// drifting under a beta API.
		base = []string{interactions.ToolGoogleSearch, interactions.ToolURLContext, interactions.ToolCodeExecution}
	}

	var (
		tools []interactions.Tool
		names []string
	)
	for _, name := range base {
		switch name {
		case interactions.ToolGoogleSearch, interactions.ToolURLContext, interactions.ToolCodeExecution:
			tools = append(tools, interactions.Tool{Type: name})
			names = append(names, name)
		case interactions.ToolMCPServer, interactions.ToolFileSearch:
			return nil, nil, fmt.Errorf("gemini: tool %q is configured via its own flag (--mcp, --file-search), not --tools", name)
		default:
			// Custom function tools are not supported by the agent
			// (docs/INTERACTIONS-API.md §3); reject before spending.
			return nil, nil, fmt.Errorf("gemini: unsupported tool %q", name)
		}
	}

	mcpNames := make([]string, 0, len(opts.MCP))
	for name := range opts.MCP {
		mcpNames = append(mcpNames, name)
	}
	sort.Strings(mcpNames)
	for _, name := range mcpNames {
		url := opts.MCP[name]
		if name == "" || url == "" {
			return nil, nil, errors.New("gemini: MCP servers need both a name and a URL")
		}
		tools = append(tools, interactions.Tool{Type: interactions.ToolMCPServer, Name: name, URL: url})
		names = append(names, interactions.ToolMCPServer+":"+name)
	}

	if len(opts.FileSearch) > 0 {
		tools = append(tools, interactions.Tool{Type: interactions.ToolFileSearch, FileSearchStoreNames: opts.FileSearch})
		names = append(names, interactions.ToolFileSearch)
	}

	return tools, names, nil
}

// Start implements researcher.Researcher: it renders the prompt, builds
// the create request, and starts the background interaction
// (background and store both true — §3's mandatory pairing for deep
// research). In follow-up mode (NewFollowUp) the request carries model
// rather than agent, no agent_config and no tools, and the query
// travels verbatim — a follow-up is a question about an existing
// report, not a new research task, so the report template does not
// apply.
func (r *Researcher) Start(ctx context.Context, task researcher.Task) (string, error) {
	if !r.inFlight.CompareAndSwap(false, true) {
		return "", errors.New("gemini: researcher already has a run in flight")
	}
	defer r.inFlight.Store(false)

	if task.Query == "" {
		return "", errors.New("gemini: query must not be empty")
	}

	var req *interactions.CreateRequest
	if r.model != "" {
		if task.PreviousInteractionID == "" {
			return "", errors.New("gemini: a follow-up needs the interaction id it follows up on")
		}
		req = &interactions.CreateRequest{
			Model:                 r.model,
			Input:                 task.Query,
			Background:            true,
			Store:                 true,
			Stream:                false,
			PreviousInteractionID: task.PreviousInteractionID,
		}
	} else {
		prompt, err := r.prompt.render(task.Query)
		if err != nil {
			return "", err
		}
		input, err := buildInput(prompt, r.inputs)
		if err != nil {
			return "", err
		}
		req = &interactions.CreateRequest{
			Agent:                 r.agentID,
			Input:                 input,
			AgentConfig:           r.cfg,
			Tools:                 r.tools,
			Background:            true,
			Store:                 true,
			Stream:                false,
			PreviousInteractionID: task.PreviousInteractionID,
		}
	}

	in, err := r.create.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("gemini: creating interaction: %w", err)
	}
	if in.ID == "" {
		return "", errors.New("gemini: the API returned an interaction without an id")
	}

	// All per-run state resets happen under the one mutex, so a
	// stale OnPoll increment from a previous run cannot interleave
	// between them.
	r.mu.Lock()
	r.started = true
	r.lastQuery = task.Query
	r.pollCount.Store(0)
	r.reconnectCount.Store(0)
	r.mu.Unlock()
	return in.ID, nil
}

// Await implements researcher.Researcher: by SSE when streaming is
// configured — thought summaries to the OnThought observer, reconnect
// on drops — and by polling otherwise. Streaming that repeatedly fails
// falls back to polling rather than abandoning a paid run (see
// awaitStream); requires_action — which deep research cannot
// legitimately produce — surfaces as an error wrapping
// interactions.ErrRequiresAction on either path.
func (r *Researcher) Await(ctx context.Context, id string) error {
	if r.streamCfg.enabled {
		err := r.awaitStream(ctx, id)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, interactions.ErrRequiresAction) || ctx.Err() != nil:
			// Research outcomes and cancellation are never grounds for
			// a fallback: the answer would not change.
			return fmt.Errorf("gemini: awaiting interaction %s: %w", id, err)
		}
		// Streaming repeatedly failed but the task is still running —
		// and still spending — server-side. Polling is the degraded
		// path that saves the run; the abandoned stream's error is
		// deliberately absorbed (the poll's own failure surfaces if
		// the API is truly unreachable). The observer gives watchers
		// of the event stream an in-flight signal of the change.
		if r.streamCfg.onDegraded != nil {
			r.streamCfg.onDegraded()
		}
	}
	return r.awaitPoll(ctx, id)
}

// awaitPoll polls until the interaction is terminal — the --quiet path
// and the fallback when streaming repeatedly fails.
func (r *Researcher) awaitPoll(ctx context.Context, id string) error {
	cfg := r.pollCfg
	cfg.OnPoll = func(*interactions.Interaction) { r.pollCount.Add(1) }
	if _, err := r.poll.PollUntilTerminal(ctx, id, cfg); err != nil {
		return fmt.Errorf("gemini: awaiting interaction %s: %w", id, err)
	}
	return nil
}

// Result implements researcher.Researcher: one final GET, mapped from
// the wire shape to the domain model.
func (r *Researcher) Result(ctx context.Context, id string) (*types.Interaction, error) {
	in, err := r.poll.Get(ctx, id)
	if err != nil {
		return nil, fmt.Errorf("gemini: retrieving interaction %s: %w", id, err)
	}
	return r.toDomain(in), nil
}

// toDomain maps the wire Interaction onto the stable domain model (the
// "Wire types vs domain types" decision): the final report text and
// chart images become Outputs, annotations become deduplicated
// Citations, the usage block flattens into cost signals, and the
// adapter contributes what the wire cannot — the original query, the
// resolved tool set, the poll count, and the planning cost estimate.
//
// Query and tool set are recorded only for interactions this adapter
// started: a resumed interaction (chiron get) ran with whatever was
// requested at its creation, which the API does not echo back, and the
// recorded set must be the used set — so it stays absent rather than
// guessed. The estimate likewise falls back to the wire agent id for
// resumed interactions.
func (r *Researcher) toDomain(in *interactions.Interaction) *types.Interaction {
	out := &types.Interaction{
		ID:           in.ID,
		Agent:        in.Agent,
		Status:       types.Status(in.Status),
		StatusDetail: statusDetail(in.Status),
		CreatedAt:    in.Created,
	}
	if out.Agent == "" {
		// Follow-up interactions carry model, not agent (§3).
		out.Agent = in.Model
	}

	r.mu.Lock()
	started, query := r.started, r.lastQuery
	r.mu.Unlock()

	estimate := r.estimate
	if started {
		out.Query = query
		out.Tools = r.toolNames
		if out.Agent == "" {
			out.Agent = r.agentID
		}
	} else {
		estimate = estimateForAgentID(in.Agent)
	}
	if in.Status.Terminal() {
		out.CompletedAt = in.Updated
	}

	// Thought summaries first (they precede the report chronologically),
	// then chart images, then the final text. The formatter renders only
	// the text and images; thoughts surface through --output json and
	// the streaming display.
	for _, thought := range thoughtTexts(in) {
		out.Outputs = append(out.Outputs, types.Output{Type: types.OutputThoughtSummary, Text: thought})
	}
	for _, img := range in.Images() {
		out.Outputs = append(out.Outputs, types.Output{
			Type:     types.OutputImage,
			MIMEType: img.MIMEType,
			Data:     img.Data,
		})
	}
	if text := in.FinalText(); text != "" {
		out.Outputs = append(out.Outputs, types.Output{Type: types.OutputText, Text: text})
	}

	for _, c := range in.Citations() {
		out.Citations = append(out.Citations, types.Citation{URI: c.URI, Title: c.Title})
	}

	out.Usage = types.Usage{
		InputTokens:      in.Usage.TotalInputTokens,
		CachedTokens:     in.Usage.TotalCachedTokens,
		OutputTokens:     in.Usage.TotalOutputTokens,
		ToolUseTokens:    in.Usage.TotalToolUseTokens,
		ThoughtTokens:    in.Usage.TotalThoughtTokens,
		SearchCount:      searchCount(in.Usage.GroundingToolCount),
		PollCount:        int(r.pollCount.Load()),
		ReconnectCount:   int(r.reconnectCount.Load()),
		EstimatedCostGBP: estimate,
	}
	return out
}

// thoughtTexts collects the thought content parts of every
// model_output step, in order — the agent's reasoning summaries,
// mapped to OutputThoughtSummary domain outputs.
func thoughtTexts(in *interactions.Interaction) []string {
	var thoughts []string
	for _, s := range in.Steps {
		if s.Type != interactions.StepModelOutput {
			continue
		}
		for _, c := range s.Content {
			if c.Type == interactions.ContentThought && c.Text != "" {
				thoughts = append(thoughts, c.Text)
			}
		}
	}
	return thoughts
}

// searchCount reduces the grounding counts to the search-count cost
// signal: how many times google_search ran (docs/INTERACTIONS-API.md
// §4 and §7 quote the per-tier envelope in searches).
func searchCount(counts []interactions.GroundingToolCount) int {
	var n int
	for _, c := range counts {
		if c.Type == interactions.ToolGoogleSearch {
			n += c.Count
		}
	}
	return n
}

// statusDetail supplies the human-readable reason the domain model
// carries beside failure variants. The wire resource has no detail
// field of its own (docs/INTERACTIONS-API.md §4), so these are the
// adapter's fixed readings of each state.
func statusDetail(s interactions.Status) string {
	switch s {
	case interactions.StatusFailed:
		return "the research task failed server-side"
	case interactions.StatusCancelled:
		return "the interaction was cancelled"
	case interactions.StatusIncomplete:
		return "the research task ended incomplete"
	case interactions.StatusBudgetExceeded:
		return "the server-side budget was exceeded"
	case interactions.StatusRequiresAction:
		return "the interaction requires client action, which deep research cannot request"
	default:
		return ""
	}
}
