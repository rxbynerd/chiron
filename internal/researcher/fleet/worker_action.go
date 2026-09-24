package fleet

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
)

// The worker's action vocabulary is the only surface through which the model
// can influence the world, and it is deliberately tiny and closed: exactly
// three actions exist, each read-only or terminal. parseAction rejects any
// kind it does not recognise and the loop's dispatch is a closed switch, so a
// side-effecting action requested by the model can only be refused.

// actionKind is the discriminator on a model action.
type actionKind string

const (
	actionSearch actionKind = "search"
	actionFetch  actionKind = "fetch"
	actionFinal  actionKind = "final"
)

// actionSchemaName names the structured-output schema in the model request.
const actionSchemaName = "worker_action"

// citationInput is one citation the model attaches to a final action.
type citationInput struct {
	URL   string `json:"url"`
	Title string `json:"title"`
}

// action is the decoded model reply. Exactly one action per turn: kind selects
// which of the sibling fields is meaningful. Fields that do not apply to the
// chosen kind arrive as JSON null under the strict schema and decode to their
// zero values.
type action struct {
	Kind      actionKind      `json:"action"`
	Query     string          `json:"query"`
	URL       string          `json:"url"`
	Answer    string          `json:"answer"`
	Citations []citationInput `json:"citations"`
}

// actionSchema is the JSON Schema sent as provider-native structured output.
// It follows the strict-mode rules OpenAI-compatible providers enforce: every
// object sets additionalProperties:false and lists every property in
// required, with fields that do not apply to an action typed nullable. The
// "action" enum bars a fourth kind on the wire; parseAction enforces the
// per-action required fields, since a schema cannot express them
// conditionally under strict mode.
var actionSchema = json.RawMessage(`{
  "type": "object",
  "additionalProperties": false,
  "properties": {
    "action": {
      "type": "string",
      "enum": ["search", "fetch", "final"],
      "description": "The single action to take this turn."
    },
    "query": {
      "type": ["string", "null"],
      "description": "For action=search: the web search query. Null otherwise."
    },
    "url": {
      "type": ["string", "null"],
      "description": "For action=fetch: a URL seen in a prior search result to retrieve. Null otherwise."
    },
    "answer": {
      "type": ["string", "null"],
      "description": "For action=final: the finding, in the required output format. Null otherwise."
    },
    "citations": {
      "type": ["array", "null"],
      "description": "For action=final: the sources relied on. Null otherwise.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "url": {"type": "string"},
          "title": {"type": ["string", "null"]}
        },
        "required": ["url", "title"]
      }
    }
  },
  "required": ["action", "query", "url", "answer", "citations"]
}`)

// parseAction decodes and validates one model reply. An unrecognised action
// kind, a known kind missing its required field, an unknown field, or
// trailing content after the JSON object is an error; the loop treats a parse
// error as a hard failure with no side effect.
func parseAction(raw string) (action, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return action{}, errors.New("fleet: model returned an empty action")
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var a action
	if err := dec.Decode(&a); err != nil {
		return action{}, fmt.Errorf("fleet: model action is not valid JSON for the action schema: %v", err)
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return action{}, errors.New("fleet: model action carries content after the JSON object")
	}

	switch a.Kind {
	case actionSearch:
		if strings.TrimSpace(a.Query) == "" {
			return action{}, errors.New("fleet: search action is missing a query")
		}
	case actionFetch:
		if strings.TrimSpace(a.URL) == "" {
			return action{}, errors.New("fleet: fetch action is missing a url")
		}
	case actionFinal:
		// An empty answer is permitted: the model may conclude the sources
		// are insufficient, and the formatter renders a placeholder body.
	case "":
		return action{}, fmt.Errorf("fleet: model action is missing the required %q discriminator", "action")
	default:
		return action{}, fmt.Errorf("fleet: model requested unknown action %q: only search, fetch and final exist", a.Kind)
	}
	return a, nil
}
