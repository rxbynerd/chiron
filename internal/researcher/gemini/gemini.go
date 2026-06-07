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
	// ThinkingSummaries asks the agent to stream thought summaries
	// ("auto"); polling runs leave it false ("none") — there is nothing
	// to display them on until the M5 streaming surface.
	ThinkingSummaries bool
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
	cfg       *interactions.AgentConfig
	tools     []interactions.Tool
	toolNames []string
	prompt    *promptTemplate
	estimate  float64
	inputs    []string
	pollCfg   interactions.PollConfig

	pollCount atomic.Int64
	mu        sync.Mutex
	lastQuery string
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

	httpClient := opts.HTTPClient
	if httpClient == nil {
		httpClient = &http.Client{}
	}
	common := []interactions.Option{interactions.WithHTTPClient(httpClient)}
	if opts.BaseURL != "" {
		common = append(common, interactions.WithBaseURL(opts.BaseURL))
	}
	create, err := interactions.New(opts.APIKey, append(common, interactions.WithMaxRetries(0))...)
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
	}
	poll, err := interactions.New(opts.APIKey, common...)
	if err != nil {
		return nil, fmt.Errorf("gemini: %w", err)
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
	}, nil
}

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
// research).
func (r *Researcher) Start(ctx context.Context, task researcher.Task) (string, error) {
	if task.Query == "" {
		return "", errors.New("gemini: query must not be empty")
	}
	prompt, err := r.prompt.render(task.Query)
	if err != nil {
		return "", err
	}
	input, err := buildInput(prompt, r.inputs)
	if err != nil {
		return "", err
	}

	req := &interactions.CreateRequest{
		Agent:                 r.agentID,
		Input:                 input,
		AgentConfig:           r.cfg,
		Tools:                 r.tools,
		Background:            true,
		Store:                 true,
		Stream:                false,
		PreviousInteractionID: task.PreviousInteractionID,
	}
	in, err := r.create.Create(ctx, req)
	if err != nil {
		return "", fmt.Errorf("gemini: creating interaction: %w", err)
	}
	if in.ID == "" {
		return "", errors.New("gemini: the API returned an interaction without an id")
	}

	r.mu.Lock()
	r.lastQuery = task.Query
	r.mu.Unlock()
	r.pollCount.Store(0)
	return in.ID, nil
}

// Await implements researcher.Researcher by polling until the
// interaction is terminal. A requires_action status — which deep
// research cannot legitimately produce — surfaces as an error wrapping
// interactions.ErrRequiresAction rather than hanging the poller.
func (r *Researcher) Await(ctx context.Context, id string) error {
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
func (r *Researcher) toDomain(in *interactions.Interaction) *types.Interaction {
	out := &types.Interaction{
		ID:           in.ID,
		Agent:        in.Agent,
		Status:       types.Status(in.Status),
		StatusDetail: statusDetail(in.Status),
		CreatedAt:    in.Created,
		Tools:        r.toolNames,
	}
	if out.Agent == "" {
		out.Agent = r.agentID
	}
	r.mu.Lock()
	out.Query = r.lastQuery
	r.mu.Unlock()
	if in.Status.Terminal() {
		out.CompletedAt = in.Updated
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
		EstimatedCostGBP: r.estimate,
	}
	return out
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
