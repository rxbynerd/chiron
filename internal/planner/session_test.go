package planner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestSession_Run_AcceptsFirstPlan(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	p := &scriptedPlanner{}
	s, out := newSession(p, "a\n")

	id, err := s.Run(context.Background(), "q")
	must.NoError(err)
	check.Equal("v1_plan_1", id, "accepted id must be the first plan's interaction")
	check.Equal(1, p.proposals, "accepting the first plan must spend exactly one proposal round")
	check.Empty(p.refines, "accepting the first plan must not refine it")
	check.Contains(out.String(), "1. Search.", "the plan text must be rendered for review")
	check.Contains(out.String(), "round 1 of 5", "the round counter must make the spend bound visible")
}

func TestSession_Run_RefineThenAccept(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	p := &scriptedPlanner{}
	s, _ := newSession(p, "r\nfocus on vendors only\na\n")

	id, err := s.Run(context.Background(), "q")
	must.NoError(err)
	check.Equal("v1_plan_2", id, "accepted id must be the refined plan's interaction")
	must.Len(p.refines, 1)
	// The refinement must chain from the plan it revises, with the
	// user's feedback verbatim.
	check.Equal(refineCall{previousID: "v1_plan_1", feedback: "focus on vendors only"}, p.refines[0])
}

func TestSession_Run_QuitAborts(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	p := &scriptedPlanner{}
	s, _ := newSession(p, "q\n")

	_, err := s.Run(context.Background(), "q")
	must.ErrorIs(err, ErrAborted)
	check.Empty(p.refines, "quitting must not spend another planning round")
}

func TestSession_Run_EndOfInputAborts(t *testing.T) {
	must := require.New(t)
	p := &scriptedPlanner{}
	s, _ := newSession(p, "")

	_, err := s.Run(context.Background(), "q")
	must.ErrorIs(err, ErrAborted, "input that ends unanswered must abort")
}

func TestSession_Run_AutoAcceptReadsNoInput(t *testing.T) {
	must := require.New(t)
	p := &scriptedPlanner{}
	s := &Session{Planner: p, Out: &bytes.Buffer{}, AutoAccept: true} // In deliberately nil

	id, err := s.Run(context.Background(), "q")
	must.NoError(err)
	must.Equal("v1_plan_1", id)
}

func TestSessionRoundBoundWithdrawsRefine(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	// With MaxRounds 2 the second plan is the last: a refine answer is
	// rejected and the user must accept or quit.
	p := &scriptedPlanner{}
	s, out := newSession(p, "r\ntighter scope\nr\na\n")
	s.MaxRounds = 2

	id, err := s.Run(context.Background(), "q")
	must.NoError(err)
	check.Equal("v1_plan_2", id, "accepted id must be the second plan's interaction")
	check.Len(p.refines, 1, "the bound must stop the second refine")
	check.Contains(out.String(), "no refine rounds left", "the user must be told the bound is spent")
}

func TestSession_Run_UnrecognisedAnswerReprompts(t *testing.T) {
	must := require.New(t)
	p := &scriptedPlanner{}
	s, out := newSession(p, "maybe\na\n")

	_, err := s.Run(context.Background(), "q")
	must.NoError(err)
	must.Contains(out.String(), `unrecognised answer "maybe"`, "unrecognised answers must reprompt")
}

func TestSession_Run_EmptyFeedbackReprompts(t *testing.T) {
	must := require.New(t)
	check := assert.New(t)
	p := &scriptedPlanner{}
	s, out := newSession(p, "r\n\na\n")

	id, err := s.Run(context.Background(), "q")
	must.NoError(err)
	check.Equal("v1_plan_1", id, "empty feedback must not change the accepted plan")
	check.Empty(p.refines, "empty feedback must not spend a refine round")
	check.Contains(out.String(), "must not be empty", "empty feedback must explain itself")
}

func TestSession_Run_ProposeErrorPropagates(t *testing.T) {
	must := require.New(t)
	wantErr := errors.New("the API said no")
	p := &scriptedPlanner{proposeErr: wantErr}
	s, _ := newSession(p, "a\n")

	_, err := s.Run(context.Background(), "q")
	must.ErrorIs(err, wantErr, "the planner's error must propagate")
}

func TestSession_Run_ValidatesInputs(t *testing.T) {
	must := require.New(t)
	s := &Session{Planner: nil, Out: &bytes.Buffer{}}
	_, err := s.Run(context.Background(), "q")
	must.Error(err, "nil Planner must be rejected")

	s = &Session{Planner: &scriptedPlanner{}, Out: &bytes.Buffer{}}
	_, err = s.Run(context.Background(), "")
	must.Error(err, "empty query must be rejected")
}

// orphaningPlanner fails its round but returns the paid round's
// interaction id alongside the error, as the Planner contract allows.
type orphaningPlanner struct{}

func (orphaningPlanner) Propose(context.Context, string) (*Plan, error) {
	return &Plan{InteractionID: "v1_orphan"}, errors.New("gemini: awaiting plan interaction v1_orphan: context canceled")
}

func (orphaningPlanner) Refine(context.Context, string, string) (*Plan, error) {
	return nil, errors.New("unreachable")
}

// A paid plan round that fails to conclude must still print its
// interaction id with a `chiron get` recovery hint.
func TestSession_Run_SurfacesOrphanedPlanID(t *testing.T) {
	must := require.New(t)
	var out bytes.Buffer
	s := &Session{Planner: orphaningPlanner{}, In: strings.NewReader(""), Out: &out}

	_, err := s.Run(context.Background(), "q")
	must.Error(err, "a failed round must still fail the session")
	must.Contains(out.String(), "chiron get v1_orphan", "the recovery hint for the paid round must be printed")
}
