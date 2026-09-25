package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strings"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/secret"
	"github.com/rxbynerd/chiron/internal/trace"
	"github.com/rxbynerd/chiron/internal/types"
)

// defaultDecomposeMaxTokens caps the decompose completion when
// leadDeps.MaxTokens is zero. Five briefs at their field bounds need a few
// thousand tokens; the rest is headroom for a reasoning model, whose
// reasoning counts against the cap.
const defaultDecomposeMaxTokens = 16384

// maxLeadQueryBytes bounds the research question before the paid decompose
// call, so an oversized query is refused rather than billed in full.
const maxLeadQueryBytes = 32 << 10

// The persisted plan's artifact meta.
const (
	planArtifactName = "fleet-plan.json"
	planMediaType    = "application/json"
)

var (
	// ErrDecomposeCall wraps a failed decompose model call. The call is never
	// retried, because an ambiguous failure may already be billed.
	ErrDecomposeCall = errors.New("fleet: the lead's decompose call failed")
	// ErrInvalidPlan wraps every refusal of a decompose reply. A refused
	// reply yields no plan, so no worker runs.
	ErrInvalidPlan = errors.New("fleet: the lead's decomposition is invalid")
)

// leadDeps are the lead's collaborators and decomposition cap.
type leadDeps struct {
	// Model is the standard-model client the lead shares with its workers.
	Model *model.Client
	// Store and Namespace hold the run's plan and findings: Namespace is the
	// run's open session.
	Store     memory.ContextStore
	Namespace memory.Namespace
	// Tracer receives the decompose span; nil discards it.
	Tracer trace.Tracer
	// Logger receives field truncations; nil discards them.
	Logger *slog.Logger
	// MaxTokens caps the decompose completion. Zero selects
	// defaultDecomposeMaxTokens.
	MaxTokens int
	// Routes validates each brief's target and dispatches it. nil selects
	// liveRoutes.
	Routes router
	// ReportTemplate, when non-nil, is rendered with the query as the
	// synthesis's output-format block; nil selects the built-in format.
	// Pass WorkerDeps.ReportTemplate: fleet workers never receive it.
	ReportTemplate *ReportTemplate
}

// lead is the fleet's orchestrator for one run.
type lead struct {
	deps leadDeps
}

// plannedBrief is one validated brief from a decomposition.
type plannedBrief struct {
	// ID is stable by position: brief-1 for the first brief.
	ID     string `json:"id"`
	Target Target `json:"target"`
	Brief  Brief  `json:"brief"`
	// Truncated names the fields cut to their byte bound, in schema order.
	Truncated []string `json:"truncated,omitempty"`
}

// leadPlan is a validated decomposition and the reference it is persisted
// under.
type leadPlan struct {
	Briefs []plannedBrief
	Ref    memory.Reference
}

// planDocumentKind identifies the persisted plan's shape, so a reader
// recovering artifacts from the store can tell a plan apart from anything
// else without guessing at its fields.
const planDocumentKind = "fleet_plan"

// planDocumentVersion is the persisted plan's schema version.
const planDocumentVersion = 1

// planDocument is the persisted plan: its kind and version, the query and
// its briefs, so the artifact identifies its own run.
type planDocument struct {
	Kind    string         `json:"kind"`
	Version int            `json:"version"`
	Query   string         `json:"query"`
	Briefs  []plannedBrief `json:"briefs"`
}

// newLead validates deps and applies the defaults.
func newLead(deps leadDeps) (*lead, error) {
	switch {
	case deps.Model == nil:
		return nil, errors.New("fleet: the lead requires a model client")
	case deps.Store == nil:
		return nil, errors.New("fleet: the lead requires a context store")
	case deps.Namespace == "":
		return nil, errors.New("fleet: the lead requires a session namespace")
	case deps.MaxTokens < 0:
		return nil, fmt.Errorf("fleet: the lead's decompose token cap %d is negative", deps.MaxTokens)
	}
	if deps.MaxTokens == 0 {
		deps.MaxTokens = defaultDecomposeMaxTokens
	}
	if deps.Routes == nil {
		deps.Routes = liveRoutes()
	}
	if deps.Tracer == nil {
		deps.Tracer = trace.Noop{}
	}
	if deps.Logger == nil {
		deps.Logger = slog.New(slog.DiscardHandler)
	}
	return &lead{deps: deps}, nil
}

// decompose asks the model for minBriefs to maxBriefs briefs in one
// structured call, validates the reply, persists the plan in the run's
// session and returns it. Any error means there is no plan and nothing may
// be dispatched. The returned usage is the call's whenever the model replied,
// a refused reply included, because a refused reply is still billed.
func (l *lead) decompose(ctx context.Context, query string) (leadPlan, types.Usage, error) {
	ctx, span := l.deps.Tracer.StartSpan(ctx, trace.SpanDecompose)
	plan, usage, err := l.decomposeInSpan(ctx, query)

	truncated := 0
	for _, pb := range plan.Briefs {
		truncated += len(pb.Truncated)
	}
	span.SetAttr(trace.AttrBriefCount, len(plan.Briefs))
	span.SetAttr("truncated_fields", truncated)
	span.SetAttr("input_tokens", usage.InputTokens)
	span.SetAttr("output_tokens", usage.OutputTokens)
	status, spanErr := types.StatusCompleted, error(nil)
	if err != nil {
		status, spanErr = types.StatusFailed, errors.New(boundDetail(secret.Scrub(err.Error())))
	}
	span.SetAttr("status", string(status))
	span.End(spanErr)
	return plan, usage, err
}

func (l *lead) decomposeInSpan(ctx context.Context, query string) (leadPlan, types.Usage, error) {
	sanitized := defang(strings.TrimSpace(query))
	if sanitized == "" {
		return leadPlan{}, types.Usage{}, errors.New("fleet: the lead's query must not be empty")
	}
	if len(sanitized) > maxLeadQueryBytes {
		return leadPlan{}, types.Usage{}, fmt.Errorf("fleet: the query is %d bytes, over the %d-byte lead limit", len(sanitized), maxLeadQueryBytes)
	}

	// One attempt only: the model client never retries a POST and the lead
	// adds no retry either.
	resp, err := l.deps.Model.Generate(ctx, model.Request{
		Messages:   leadTranscript(sanitized),
		MaxTokens:  l.deps.MaxTokens,
		JSONSchema: decomposeSchema,
		SchemaName: decomposeSchemaName,
	})
	if err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return leadPlan{}, types.Usage{}, fmt.Errorf("%w: %w", ErrDecomposeCall, ctxErr)
		}
		return leadPlan{}, types.Usage{}, fmt.Errorf("%w: %s", ErrDecomposeCall, boundDetail(secret.Scrub(err.Error())))
	}
	usage := types.Usage{InputTokens: resp.Usage.InputTokens, OutputTokens: resp.Usage.OutputTokens}

	if resp.FinishReason == "length" {
		return leadPlan{}, usage, fmt.Errorf("%w: the reply was cut off at the %d-token completion cap", ErrInvalidPlan, l.deps.MaxTokens)
	}
	if resp.FinishReason == "content_filter" {
		return leadPlan{}, usage, fmt.Errorf("%w: the reply was refused by the model's content filter", ErrInvalidPlan)
	}
	briefs, err := parseDecomposition(resp.Content, l.deps.Routes)
	if err != nil {
		return leadPlan{}, usage, err
	}
	for _, pb := range briefs {
		for _, field := range pb.Truncated {
			l.deps.Logger.Info("lead: truncated a brief field to its bound", "brief_id", pb.ID, "field", field)
		}
	}

	ref, err := l.persistPlan(ctx, sanitized, briefs)
	if err != nil {
		return leadPlan{}, usage, &persistPlanError{detail: boundDetail(secret.Scrub(err.Error())), err: err}
	}
	return leadPlan{Briefs: briefs, Ref: ref}, usage, nil
}

// persistPlanError reports a plan-persist failure with a bounded, scrubbed
// message while keeping the store's error reachable via errors.Is/errors.As:
// ContextStore is a seam a remote implementation could back with a large
// echoed body.
type persistPlanError struct {
	detail string
	err    error
}

func (e *persistPlanError) Error() string {
	return "fleet: persisting the lead's plan: " + e.detail
}

func (e *persistPlanError) Unwrap() error { return e.err }

// persistPlan writes the plan to the run's session and returns its
// reference.
func (l *lead) persistPlan(ctx context.Context, query string, briefs []plannedBrief) (memory.Reference, error) {
	body, err := json.Marshal(planDocument{
		Kind:    planDocumentKind,
		Version: planDocumentVersion,
		Query:   query,
		Briefs:  briefs,
	})
	if err != nil {
		return memory.Reference{}, err
	}
	return l.deps.Store.Put(ctx, l.deps.Namespace, bytes.NewReader(body), memory.ArtifactMeta{
		Name:      planArtifactName,
		MediaType: planMediaType,
	})
}

// decomposition is the decoded decompose reply.
type decomposition struct {
	Briefs []decomposedBrief `json:"briefs"`
}

type decomposedBrief struct {
	Objective      string `json:"objective"`
	OutputFormat   string `json:"output_format"`
	SourceGuidance string `json:"source_guidance"`
	Boundaries     string `json:"boundaries"`
	Target         Target `json:"target"`
}

// parseDecomposition decodes and validates a decompose reply. An empty reply,
// invalid JSON, an unknown field, trailing content, a brief count outside
// minBriefs..maxBriefs, a blank field or a target routes cannot dispatch is
// an ErrInvalidPlan. Each field is trimmed, defanged, then cut to its byte
// bound, and a cut is recorded on the brief rather than refused.
func parseDecomposition(raw string, routes router) ([]plannedBrief, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("%w: the reply is empty", ErrInvalidPlan)
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var d decomposition
	if err := dec.Decode(&d); err != nil {
		return nil, fmt.Errorf("%w: the reply does not match the decomposition schema: %s", ErrInvalidPlan, boundDetail(defang(secret.Scrub(err.Error()))))
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, fmt.Errorf("%w: the reply carries content after the JSON object", ErrInvalidPlan)
	}
	if n := len(d.Briefs); n < minBriefs || n > maxBriefs {
		return nil, fmt.Errorf("%w: the reply has %d briefs; %d to %d are required", ErrInvalidPlan, n, minBriefs, maxBriefs)
	}

	planned := make([]plannedBrief, len(d.Briefs))
	for i, b := range d.Briefs {
		pb := plannedBrief{ID: fmt.Sprintf("brief-%d", i+1), Target: b.Target}
		if _, err := routes.route(b.Target); err != nil {
			return nil, fmt.Errorf("%w: %s: %w", ErrInvalidPlan, pb.ID, err)
		}
		for _, f := range []struct {
			name     string
			text     string
			maxBytes int
			dst      *string
		}{
			{"objective", b.Objective, maxBriefObjectiveBytes, &pb.Brief.Objective},
			{"output_format", b.OutputFormat, maxBriefOutputFormatBytes, &pb.Brief.OutputFormat},
			{"source_guidance", b.SourceGuidance, maxBriefSourceGuidanceBytes, &pb.Brief.SourceGuidance},
			{"boundaries", b.Boundaries, maxBriefBoundariesBytes, &pb.Brief.Boundaries},
		} {
			text := strings.TrimSpace(f.text)
			if text == "" {
				return nil, fmt.Errorf("%w: %s has a blank %s", ErrInvalidPlan, pb.ID, f.name)
			}
			text = defang(text)
			if len(text) > f.maxBytes {
				text = boundBytes(text, f.maxBytes)
				pb.Truncated = append(pb.Truncated, f.name)
			}
			*f.dst = text
		}
		planned[i] = pb
	}
	return planned, nil
}
