package planner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// scriptedPlanner counts rounds and records the refinement chain.
type scriptedPlanner struct {
	proposeErr error
	refineErr  error

	proposals int
	refines   []refineCall
}

type refineCall struct {
	previousID string
	feedback   string
}

func (p *scriptedPlanner) Propose(_ context.Context, query string) (*Plan, error) {
	if p.proposeErr != nil {
		return nil, p.proposeErr
	}
	p.proposals++
	return &Plan{InteractionID: "v1_plan_1", Text: "1. Search.\n2. Read.\n"}, nil
}

func (p *scriptedPlanner) Refine(_ context.Context, previousID, feedback string) (*Plan, error) {
	if p.refineErr != nil {
		return nil, p.refineErr
	}
	p.refines = append(p.refines, refineCall{previousID: previousID, feedback: feedback})
	id := fmt.Sprintf("v1_plan_%d", len(p.refines)+1)
	return &Plan{InteractionID: id, Text: "revised plan"}, nil
}

func newSession(p Planner, input string) (*Session, *bytes.Buffer) {
	var out bytes.Buffer
	return &Session{Planner: p, In: strings.NewReader(input), Out: &out}, &out
}

func TestSessionAcceptFirstPlan(t *testing.T) {
	p := &scriptedPlanner{}
	s, out := newSession(p, "a\n")

	id, err := s.Run(context.Background(), "q")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if id != "v1_plan_1" {
		t.Errorf("accepted id = %q, want the first plan's interaction", id)
	}
	if p.proposals != 1 || len(p.refines) != 0 {
		t.Errorf("rounds = %d proposals, %d refines; want exactly one proposal", p.proposals, len(p.refines))
	}
	if !strings.Contains(out.String(), "1. Search.") {
		t.Errorf("the plan text must be rendered for review:\n%s", out.String())
	}
	if !strings.Contains(out.String(), "round 1 of 5") {
		t.Errorf("the round counter must make the spend bound visible:\n%s", out.String())
	}
}

func TestSessionRefineThenAccept(t *testing.T) {
	p := &scriptedPlanner{}
	s, _ := newSession(p, "r\nfocus on vendors only\na\n")

	id, err := s.Run(context.Background(), "q")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if id != "v1_plan_2" {
		t.Errorf("accepted id = %q, want the refined plan's interaction", id)
	}
	if len(p.refines) != 1 {
		t.Fatalf("refines = %d, want 1", len(p.refines))
	}
	// The refinement must chain from the plan it revises, with the
	// user's feedback verbatim.
	if p.refines[0].previousID != "v1_plan_1" || p.refines[0].feedback != "focus on vendors only" {
		t.Errorf("refine call = %+v", p.refines[0])
	}
}

func TestSessionQuitAborts(t *testing.T) {
	p := &scriptedPlanner{}
	s, _ := newSession(p, "q\n")

	if _, err := s.Run(context.Background(), "q"); !errors.Is(err, ErrAborted) {
		t.Errorf("err = %v, want ErrAborted", err)
	}
	if len(p.refines) != 0 {
		t.Error("quitting must not spend another planning round")
	}
}

func TestSessionEndOfInputAborts(t *testing.T) {
	p := &scriptedPlanner{}
	s, _ := newSession(p, "")

	if _, err := s.Run(context.Background(), "q"); !errors.Is(err, ErrAborted) {
		t.Errorf("err = %v, want ErrAborted when input ends unanswered", err)
	}
}

func TestSessionAutoAcceptReadsNoInput(t *testing.T) {
	p := &scriptedPlanner{}
	s := &Session{Planner: p, Out: &bytes.Buffer{}, AutoAccept: true} // In deliberately nil

	id, err := s.Run(context.Background(), "q")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if id != "v1_plan_1" {
		t.Errorf("accepted id = %q", id)
	}
}

func TestSessionRoundBoundWithdrawsRefine(t *testing.T) {
	// With MaxRounds 2 the second plan is the last: a refine answer is
	// rejected and the user must accept or quit.
	p := &scriptedPlanner{}
	s, out := newSession(p, "r\ntighter scope\nr\na\n")
	s.MaxRounds = 2

	id, err := s.Run(context.Background(), "q")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if id != "v1_plan_2" {
		t.Errorf("accepted id = %q", id)
	}
	if len(p.refines) != 1 {
		t.Errorf("refines = %d, want the bound to stop the second", len(p.refines))
	}
	if !strings.Contains(out.String(), "no refine rounds left") {
		t.Errorf("the user must be told the bound is spent:\n%s", out.String())
	}
}

func TestSessionUnrecognisedAnswerReprompts(t *testing.T) {
	p := &scriptedPlanner{}
	s, out := newSession(p, "maybe\na\n")

	if _, err := s.Run(context.Background(), "q"); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if !strings.Contains(out.String(), `unrecognised answer "maybe"`) {
		t.Errorf("unrecognised answers must reprompt:\n%s", out.String())
	}
}

func TestSessionEmptyFeedbackReprompts(t *testing.T) {
	p := &scriptedPlanner{}
	s, out := newSession(p, "r\n\na\n")

	id, err := s.Run(context.Background(), "q")
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if id != "v1_plan_1" || len(p.refines) != 0 {
		t.Errorf("empty feedback must not spend a refine round: id=%q refines=%d", id, len(p.refines))
	}
	if !strings.Contains(out.String(), "must not be empty") {
		t.Errorf("empty feedback must explain itself:\n%s", out.String())
	}
}

func TestSessionProposeErrorPropagates(t *testing.T) {
	wantErr := errors.New("the API said no")
	p := &scriptedPlanner{proposeErr: wantErr}
	s, _ := newSession(p, "a\n")

	if _, err := s.Run(context.Background(), "q"); !errors.Is(err, wantErr) {
		t.Errorf("err = %v, want the planner's error verbatim", err)
	}
}

func TestSessionValidatesInputs(t *testing.T) {
	s := &Session{Planner: nil, Out: &bytes.Buffer{}}
	if _, err := s.Run(context.Background(), "q"); err == nil {
		t.Error("nil Planner must be rejected")
	}
	s = &Session{Planner: &scriptedPlanner{}, Out: &bytes.Buffer{}}
	if _, err := s.Run(context.Background(), ""); err == nil {
		t.Error("empty query must be rejected")
	}
}

// orphaningPlanner fails its round but hands back the paid round's
// interaction id, per the Planner seam's partial-Plan contract.
type orphaningPlanner struct{}

func (orphaningPlanner) Propose(context.Context, string) (*Plan, error) {
	return &Plan{InteractionID: "v1_orphan"}, errors.New("gemini: awaiting plan interaction v1_orphan: context canceled")
}

func (orphaningPlanner) Refine(context.Context, string, string) (*Plan, error) {
	return nil, errors.New("unreachable")
}

// TestSessionSurfacesOrphanedPlanID pins C2-CODE-2: a plan round that
// was paid for but could not be concluded surfaces its interaction id
// with the chiron get recovery hint, so the spend is never silently
// lost.
func TestSessionSurfacesOrphanedPlanID(t *testing.T) {
	var out bytes.Buffer
	s := &Session{Planner: orphaningPlanner{}, In: strings.NewReader(""), Out: &out}
	if _, err := s.Run(context.Background(), "q"); err == nil {
		t.Fatal("a failed round must still fail the session")
	}
	if !strings.Contains(out.String(), "chiron get v1_orphan") {
		t.Errorf("output %q must carry the recovery hint for the paid round", out.String())
	}
}
