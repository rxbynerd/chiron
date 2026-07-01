package fleet

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The worker's action vocabulary. This is the ONLY surface through which the
// model can influence the world, and it is deliberately tiny and closed
// (docs/V2-RESEARCH-AGENT §5, non-negotiable "research-only by construction").
// Exactly three actions exist — search, fetch, final — each read-only or
// terminal. There is no generic tool-call surface, no shell, no file write, no
// code execution: parseAction rejects any kind it does not recognise, and the
// loop's dispatch is a closed switch with no default execution path, so a
// side-effecting action requested by the model cannot be run — only refused.

// actionKind is the discriminator on a model action.
type actionKind string

const (
	actionSearch actionKind = "search"
	actionFetch  actionKind = "fetch"
	actionFinal  actionKind = "final"
)

// actionSchemaName names the structured-output schema in the model request.
const actionSchemaName = "worker_action"

// citationInput is one citation the model attaches to a final action. It maps
// onto types.Citation; a title is optional.
type citationInput struct {
	URL   string `json:"url"`
	Title string `json:"title,omitempty"`
}

// action is the decoded model reply. Exactly one action per turn: kind
// selects which of the sibling fields is meaningful. Unknown kinds are
// rejected by parseAction before the loop ever inspects the fields, so the
// dispatch never sees an action it cannot name.
type action struct {
	Kind      actionKind      `json:"action"`
	Query     string          `json:"query,omitempty"`
	URL       string          `json:"url,omitempty"`
	Answer    string          `json:"answer,omitempty"`
	Citations []citationInput `json:"citations,omitempty"`
}

// actionSchema is the JSON Schema sent as provider-native structured output
// (model.Request.JSONSchema). It constrains the model's reply to one of the
// three actions via an enum on the "action" discriminator: a compliant
// provider cannot emit a fourth kind. The prompt (worker_prompt.go) restates
// the same contract in prose. additionalProperties:false keeps the reply
// shape tight; the per-action required fields are enforced in parseAction,
// not the schema, so a provider that ignores conditional requirements still
// yields an error we control rather than a silently-empty action.
//
// It is a package-level json.RawMessage built once; the loop passes it on
// every turn.
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
      "type": "string",
      "description": "For action=search: the web search query."
    },
    "url": {
      "type": "string",
      "description": "For action=fetch: a URL seen in a prior search result to retrieve."
    },
    "answer": {
      "type": "string",
      "description": "For action=final: the finding, in the required output format."
    },
    "citations": {
      "type": "array",
      "description": "For action=final: the sources relied on.",
      "items": {
        "type": "object",
        "additionalProperties": false,
        "properties": {
          "url": {"type": "string"},
          "title": {"type": "string"}
        },
        "required": ["url"]
      }
    }
  },
  "required": ["action"]
}`)

// parseAction decodes and validates one model reply. It is the guard that
// keeps the worker research-only: an unrecognised action kind, or a known kind
// missing its required field, is an error — the loop treats a parse error as a
// hard failure (no side effect, the run ends failed), so there is no path by
// which a malformed or side-effecting reply causes an action to run.
//
// The reply is expected to be the JSON string a structured-output request
// returns. It is decoded strictly (unknown fields rejected) so a reply
// carrying an extra field — the shape a jailbreak attempt to smuggle a
// side-effecting instruction would take — is refused rather than partially
// honoured.
func parseAction(raw string) (action, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return action{}, fmt.Errorf("model returned an empty action")
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	dec.DisallowUnknownFields()
	var a action
	if err := dec.Decode(&a); err != nil {
		return action{}, fmt.Errorf("model action is not valid JSON for the action schema: %v", err)
	}

	switch a.Kind {
	case actionSearch:
		if strings.TrimSpace(a.Query) == "" {
			return action{}, fmt.Errorf("search action is missing a query")
		}
	case actionFetch:
		if strings.TrimSpace(a.URL) == "" {
			return action{}, fmt.Errorf("fetch action is missing a url")
		}
	case actionFinal:
		// A final answer may be empty in principle (the model may conclude the
		// sources are insufficient); the loop maps that to an interaction with
		// a placeholder body. Citations may also be empty. No required field.
	case "":
		return action{}, fmt.Errorf("model action is missing the required %q discriminator", "action")
	default:
		// The closed-vocabulary guard: any kind other than the three known
		// actions — shell, write, exec, or any other — is refused here, before
		// dispatch. There is deliberately no execution branch for it anywhere.
		return action{}, fmt.Errorf("model requested unknown action %q: only search, fetch and final exist", a.Kind)
	}
	return a, nil
}
