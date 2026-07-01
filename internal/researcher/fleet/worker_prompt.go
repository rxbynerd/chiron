package fleet

import (
	"fmt"
	"strings"

	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/search"
)

// The worker prompt template and transcript builders. The worker is an
// external-web research agent (docs/V2-RESEARCH-AGENT §5): it reasons over a
// bounded search -> read -> synthesise loop and emits exactly one structured
// action per turn. The prose here is instructional and impersonal (en-GB) and
// pins the closed action vocabulary the model must obey; the machine contract
// that actually constrains the reply is the JSON Schema in worker_action.go,
// sent as provider-native structured output. The prompt restates it so a model
// that reads instructions and one that only obeys the schema agree.

// systemPromptTemplate is the worker's standing instruction. It is
// external-web-only by construction: the only capabilities named are the two
// read-only tools the loop dispatches, and the model is told plainly that no
// other action exists. %s placeholders are, in order: objective, output
// format, source guidance, boundaries.
const systemPromptTemplate = `You are a research worker on Chiron's in-process fleet. You answer one
research objective by consulting the external public web only, then writing a
concise, faithful, cited finding. You have no access to internal systems, no
shell, no file system, and no ability to run code — only the two read-only web
tools described below.

Objective:
%s

Required output format:
%s

Source guidance:
%s

Boundaries:
%s

Work in turns. On every turn you must return exactly one action, as a JSON
object matching the response schema. The only actions that exist are:

  - search: run a web search. Provide a "query" string. Use it to discover
    candidate sources for the objective.
  - fetch: retrieve the full text of one URL you have already seen in a prior
    search result. Provide the "url" string. Do not fetch a URL that has not
    appeared in a search result.
  - final: stop and deliver the finding. Provide an "answer" string (the
    finding, in the required output format) and a "citations" array of the
    source URLs you relied on, each as {"url": "...", "title": "..."}.

No other action exists. Do not attempt to write files, run commands, execute
code, or call any tool other than search and fetch — such a request is invalid
and the run will be failed. Search first to gather sources, fetch the most
promising ones to read their content, then synthesise a "final" answer that
cites the sources you used. Prefer primary and reputable sources. Ground every
claim in a fetched or searched source; do not invent sources or facts. Stop as
soon as you can answer the objective faithfully — you are bounded by strict
turn, token, and time limits, and an unfinished loop wastes them.`

// buildSystemPrompt renders the standing instruction for one brief, applying
// sensible defaults for any field the caller left blank so the prompt never
// contains an empty section.
func buildSystemPrompt(brief Brief) string {
	objective := strings.TrimSpace(brief.Objective)
	if objective == "" {
		objective = "(no objective was supplied)"
	}
	format := strings.TrimSpace(brief.OutputFormat)
	if format == "" {
		format = defaultOutputFormat
	}
	guidance := strings.TrimSpace(brief.SourceGuidance)
	if guidance == "" {
		guidance = defaultSourceGuidance
	}
	boundaries := strings.TrimSpace(brief.Boundaries)
	if boundaries == "" {
		boundaries = defaultBoundaries
	}
	return fmt.Sprintf(systemPromptTemplate, objective, format, guidance, boundaries)
}

// Default brief fields for a lone worker run (the single-query --agent worker
// path). Wave 4's lead supplies its own per-brief values; these keep a bare
// worker run instructive without them.
const (
	defaultOutputFormat = "A concise Markdown answer: a short synthesis of the findings, " +
		"grounded in the cited sources."
	defaultSourceGuidance = "Prefer primary sources, official documentation, standards bodies, " +
		"and reputable publications. Corroborate a claim across sources where you can."
	defaultBoundaries = "Use only the external public web. Do not speculate beyond what the " +
		"sources support; if the sources are insufficient, say so in the answer."
)

// kickoffMessage is the first user turn: it prompts the model to begin the
// loop. The objective already lives in the system prompt; this simply hands
// control to the model.
const kickoffMessage = "Begin. Return your first action as a JSON object matching the schema."

// initialTranscript builds the opening transcript for a brief: the standing
// system instruction, then the kickoff user turn.
func initialTranscript(brief Brief) []model.Message {
	return []model.Message{
		{Role: model.RoleSystem, Content: buildSystemPrompt(brief)},
		{Role: model.RoleUser, Content: kickoffMessage},
	}
}

// searchResultsMessage renders a search tool's results as a user turn to feed
// back into the transcript. The results are serialised as a compact,
// numbered list rather than raw JSON: the model reasons over URLs and
// snippets, and a stable textual shape keeps the transcript legible and the
// token cost predictable. A result may carry an empty URL (a degraded search
// shape); such an entry is still surfaced (its snippet may be useful) but is
// marked as having no fetchable URL.
func searchResultsMessage(query string, results []search.Result) model.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "Search results for %q:\n", query)
	if len(results) == 0 {
		b.WriteString("(no results)\n")
	}
	for i, r := range results {
		fmt.Fprintf(&b, "%d. ", i+1)
		if r.Title != "" {
			b.WriteString(r.Title)
		} else {
			b.WriteString("(untitled)")
		}
		b.WriteString("\n")
		if r.URL != "" {
			fmt.Fprintf(&b, "   URL: %s\n", r.URL)
		} else {
			b.WriteString("   URL: (none — this result cannot be fetched)\n")
		}
		if r.Snippet != "" {
			fmt.Fprintf(&b, "   %s\n", r.Snippet)
		}
	}
	b.WriteString("\nChoose your next action: fetch one of these URLs to read it, " +
		"search again, or deliver a final answer.")
	return model.Message{Role: model.RoleUser, Content: b.String()}
}

// fetchedPageMessage renders a fetched page as a user turn. The page content
// is already bounded by the fetch client (MaxContentBytes); Truncated is
// surfaced so the model knows it is reading a prefix. The URL is the
// sanitised final URL the fetch client returned (userinfo already stripped),
// safe to echo.
func fetchedPageMessage(page fetchedPage) model.Message {
	var b strings.Builder
	fmt.Fprintf(&b, "Fetched %s", page.URL)
	if page.ContentType != "" {
		fmt.Fprintf(&b, " (%s)", page.ContentType)
	}
	if page.Truncated {
		b.WriteString(" [truncated — content exceeded the fetch limit; you are reading a prefix]")
	}
	b.WriteString(":\n\n")
	b.Write(page.Content)
	b.WriteString("\n\nChoose your next action: fetch another URL, search again, " +
		"or deliver a final answer citing your sources.")
	return model.Message{Role: model.RoleUser, Content: b.String()}
}

// errorFeedbackMessage renders a recoverable tool error as a user turn, so the
// model can adjust rather than the loop aborting. It is used for errors the
// worker chooses to surface back to the model (e.g. a fetch of a URL that was
// never in a search result); hard tool failures end the loop instead.
func errorFeedbackMessage(detail string) model.Message {
	return model.Message{
		Role:    model.RoleUser,
		Content: "That action could not be completed: " + detail + " Choose a different action.",
	}
}

// assistantEcho renders the model's own action back into the transcript as its
// assistant turn, so the next request carries the full exchange. The raw JSON
// the model emitted is echoed verbatim.
func assistantEcho(raw string) model.Message {
	return model.Message{Role: model.RoleAssistant, Content: raw}
}
