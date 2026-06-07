package gemini

import (
	"context"
	"errors"
	"fmt"

	"github.com/rxbynerd/chiron/internal/interactions"
	"github.com/rxbynerd/chiron/internal/planner"
)

// Planner binds the planner seam to collaborative planning
// (docs/INTERACTIONS-API.md §7): every plan round is a create against
// the research agent with collaborative_planning enabled — proposal
// from the rendered research prompt, refinements chained by
// previous_interaction_id with the user's feedback verbatim. Approval
// is not here: the accepted plan's id chains into the ordinary research
// run, whose agent_config has collaborative_planning off.
//
// The proposal renders the same template the research run will send, so
// the agent plans the report it will actually be asked to write —
// template steering included.
type Planner struct {
	// create has automatic retries disabled, exactly as the research
	// create does: a plan round is a paid create, and a retried POST
	// after an ambiguous 5xx could pay for a second one.
	create *interactions.Client
	poll   *interactions.Client

	agentID string
	cfg     *interactions.AgentConfig
	tools   []interactions.Tool
	prompt  *promptTemplate
	inputs  []string
	pollCfg interactions.PollConfig
}

var _ planner.Planner = (*Planner)(nil)

// NewPlanner builds the planning binding from the same Options as the
// research adapter, so the plan is made against the tier, tools,
// template and grounding inputs the research run will use.
func NewPlanner(opts Options) (*Planner, error) {
	agentID, _, err := tierAgent(opts.Tier)
	if err != nil {
		return nil, err
	}
	tools, _, err := assembleTools(opts)
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

	cfg := agentConfig(opts)
	cfg.CollaborativePlanning = true

	return &Planner{
		create:  create,
		poll:    poll,
		agentID: agentID,
		cfg:     cfg,
		tools:   tools,
		prompt:  prompt,
		inputs:  opts.Inputs,
		pollCfg: interactions.PollConfig{
			Interval:    opts.PollInterval,
			MaxInterval: opts.PollMaxInterval,
		},
	}, nil
}

// Propose implements planner.Planner: the first plan round, from the
// rendered research prompt.
func (p *Planner) Propose(ctx context.Context, query string) (*planner.Plan, error) {
	if query == "" {
		return nil, errors.New("gemini: query must not be empty")
	}
	prompt, err := p.prompt.render(query)
	if err != nil {
		return nil, err
	}
	input, err := buildInput(prompt, p.inputs)
	if err != nil {
		return nil, err
	}
	return p.round(ctx, input, "")
}

// Refine implements planner.Planner: another plan round chained to the
// previous plan, steered by the user's feedback verbatim.
func (p *Planner) Refine(ctx context.Context, previousID, feedback string) (*planner.Plan, error) {
	if previousID == "" {
		return nil, errors.New("gemini: refine needs the plan interaction it revises")
	}
	if feedback == "" {
		return nil, errors.New("gemini: refine feedback must not be empty")
	}
	return p.round(ctx, feedback, previousID)
}

// round runs one plan interaction to completion and extracts the plan
// text. Plan interactions are background-and-stored like everything
// else (§7: all three steps with background: true), so the same poll
// machinery applies and an interrupted plan round is recoverable by id.
func (p *Planner) round(ctx context.Context, input any, previousID string) (*planner.Plan, error) {
	req := &interactions.CreateRequest{
		Agent:                 p.agentID,
		Input:                 input,
		AgentConfig:           p.cfg,
		Tools:                 p.tools,
		Background:            true,
		Store:                 true,
		Stream:                false,
		PreviousInteractionID: previousID,
	}
	in, err := p.create.Create(ctx, req)
	if err != nil {
		return nil, fmt.Errorf("gemini: creating plan interaction: %w", err)
	}
	if in.ID == "" {
		return nil, errors.New("gemini: the API returned a plan interaction without an id")
	}

	// From here the round is paid for: every error path hands back a
	// partial Plan carrying the interaction id — the recovery handle
	// (chiron get) for a round that could not be concluded, per the
	// Planner seam's contract. The id would otherwise be lost on a
	// cancelled or failed poll.
	final, err := p.poll.PollUntilTerminal(ctx, in.ID, p.pollCfg)
	if err != nil {
		return &planner.Plan{InteractionID: in.ID}, fmt.Errorf("gemini: awaiting plan interaction %s: %w", in.ID, err)
	}
	if final.Status != interactions.StatusCompleted {
		return &planner.Plan{InteractionID: in.ID}, fmt.Errorf("gemini: plan interaction %s ended %s", in.ID, final.Status)
	}
	text := final.FinalText()
	if text == "" {
		return &planner.Plan{InteractionID: in.ID}, fmt.Errorf("gemini: plan interaction %s completed without plan text", in.ID)
	}
	return &planner.Plan{InteractionID: in.ID, Text: text}, nil
}
