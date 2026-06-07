package gemini

import (
	"github.com/rxbynerd/chiron/internal/interactions"
)

// DefaultFollowUpModel is the model follow-up Q&A uses when --model is
// unset: the model docs/INTERACTIONS-API.md §3 itself names for
// follow-ups against the pinned Api-Revision. Follow-up requests carry
// model, not agent — they answer a question over a stored interaction
// rather than starting a new research task.
const DefaultFollowUpModel = "gemini-3.1-pro-preview"

// NewFollowUp builds a Researcher in follow-up mode (chiron follow-up):
// Start creates with model (DefaultFollowUpModel when model is empty),
// no agent_config and no tools, the query verbatim, and the mandatory
// PreviousInteractionID chaining it to the interaction it questions.
// Background and store stay true so the same poll-and-resume machinery
// serves follow-ups. Tier, tool, input and template options do not
// apply in this mode and are ignored.
func NewFollowUp(opts Options, model string) (*Researcher, error) {
	if model == "" {
		model = DefaultFollowUpModel
	}
	create, poll, err := newClients(opts)
	if err != nil {
		return nil, err
	}
	return &Researcher{
		create: create,
		poll:   poll,
		model:  model,
		pollCfg: interactions.PollConfig{
			Interval:    opts.PollInterval,
			MaxInterval: opts.PollMaxInterval,
		},
	}, nil
}
