package planner

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"
)

// DefaultMaxRounds bounds the plan loop: the proposal plus refinements.
// Every round is a paid create against the research agent and the API
// publishes no per-round planning price (docs/INTERACTIONS-API.md §7
// prices full runs only), so the bound is structural rather than
// monetary — it exists so an absent-minded refine loop cannot spend
// unbounded money before the research itself has even started.
const DefaultMaxRounds = 5

// ErrAborted reports that the user declined the plan — quit at the
// prompt, or ended input before accepting. No research run starts.
var ErrAborted = errors.New("planner: plan review aborted; no research was started")

// Session drives the interactive plan review on a terminal: render each
// plan, then accept, refine, or quit. The caller decides interactivity —
// a Session is only constructed when stdin is a terminal or AutoAccept
// is set (the documented --accept-plan escape).
type Session struct {
	// Planner proposes and refines plans. Required.
	Planner Planner
	// In supplies the user's answers; may be nil with AutoAccept.
	In io.Reader
	// Out renders plans and prompts. The CLI binds stderr: stdout
	// belongs to the report, and a piped report must stay clean.
	Out io.Writer
	// AutoAccept approves the first proposed plan without prompting.
	AutoAccept bool
	// MaxRounds caps total plan rounds (proposal + refinements);
	// values < 1 mean DefaultMaxRounds.
	MaxRounds int
}

// Run executes the review loop for query and returns the accepted
// plan's interaction id — the handle the research run chains from with
// collaborative_planning off. It returns ErrAborted (possibly wrapped)
// when the user quits or input ends, and the planner's error verbatim
// when a round fails.
func (s *Session) Run(ctx context.Context, query string) (string, error) {
	if s.Planner == nil {
		return "", errors.New("planner: nil Planner")
	}
	if query == "" {
		return "", errors.New("planner: query must not be empty")
	}
	maxRounds := s.MaxRounds
	if maxRounds < 1 {
		maxRounds = DefaultMaxRounds
	}

	plan, err := s.Planner.Propose(ctx, query)
	if err != nil {
		s.noteOrphan(plan)
		return "", err
	}

	in := s.In
	if in == nil {
		in = strings.NewReader("")
	}
	reader := bufio.NewReader(in)

	for round := 1; ; round++ {
		s.renderPlan(plan, round, maxRounds)

		if s.AutoAccept {
			fmt.Fprintln(s.Out, "plan accepted unattended (--accept-plan)")
			return plan.InteractionID, nil
		}

		feedback, err := s.review(reader, round == maxRounds)
		if err != nil {
			return "", err
		}
		if feedback == "" { // accepted
			return plan.InteractionID, nil
		}

		plan, err = s.Planner.Refine(ctx, plan.InteractionID, feedback)
		if err != nil {
			s.noteOrphan(plan)
			return "", err
		}
	}
}

// noteOrphan surfaces the interaction id of a plan round that was paid
// for but could not be concluded (cancelled mid-poll, failed round) —
// the same recovery posture as the research run, whose id is emitted
// the moment it is known. Without this, the round's spend would be
// unrecoverable: nothing else ever learns the id.
func (s *Session) noteOrphan(plan *Plan) {
	if plan == nil || plan.InteractionID == "" || s.Out == nil {
		return
	}
	fmt.Fprintf(s.Out, "plan interaction %s did not conclude; it is stored server-side — recover it with: chiron get %s\n", plan.InteractionID, plan.InteractionID)
}

// renderPlan writes one plan between unambiguous fences, with the round
// counter making the spend bound visible before the user asks for
// another paid round.
func (s *Session) renderPlan(plan *Plan, round, maxRounds int) {
	fmt.Fprintf(s.Out, "\n--- proposed research plan: round %d of %d (interaction %s) ---\n\n", round, maxRounds, plan.InteractionID)
	fmt.Fprintln(s.Out, strings.TrimRight(plan.Text, "\n"))
	fmt.Fprintf(s.Out, "\n--- end of plan ---\n")
}

// review prompts until it has a decision: empty feedback means accept,
// non-empty feedback means refine, and quitting or end-of-input is
// ErrAborted. lastRound withdraws the refine option — the round bound
// is spent.
func (s *Session) review(reader *bufio.Reader, lastRound bool) (feedback string, err error) {
	for {
		if lastRound {
			fmt.Fprint(s.Out, "plan round limit reached — [a]ccept this plan or [q]uit: ")
		} else {
			fmt.Fprint(s.Out, "[a]ccept and start the research, [r]efine, or [q]uit: ")
		}
		answer, err := readLine(reader)
		if err != nil {
			return "", err
		}

		switch strings.ToLower(answer) {
		case "a", "accept":
			return "", nil
		case "r", "refine":
			if lastRound {
				fmt.Fprintln(s.Out, "no refine rounds left — accept or quit")
				continue
			}
			fmt.Fprint(s.Out, "what should change? ")
			feedback, err := readLine(reader)
			if err != nil {
				return "", err
			}
			if feedback == "" {
				fmt.Fprintln(s.Out, "refinement feedback must not be empty")
				continue
			}
			return feedback, nil
		case "q", "quit", "abort":
			return "", ErrAborted
		default:
			fmt.Fprintf(s.Out, "unrecognised answer %q\n", answer)
		}
	}
}

// readLine reads one trimmed answer; end-of-input aborts the review —
// there is no one left to approve the spend.
func readLine(reader *bufio.Reader) (string, error) {
	line, err := reader.ReadString('\n')
	if err != nil && line == "" {
		return "", fmt.Errorf("input ended before the plan was accepted: %w", ErrAborted)
	}
	return strings.TrimSpace(line), nil
}
