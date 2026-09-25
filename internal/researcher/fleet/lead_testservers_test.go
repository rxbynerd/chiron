package fleet

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model"
	"github.com/rxbynerd/chiron/internal/researcher/fleet/model/modeltest"
	"github.com/rxbynerd/chiron/internal/types"
)

// Model handlers and stored-finding helpers for the lead's synthesis,
// citation and whole-run tests. Each test creates and closes its own
// httptest.Server around a fleetModel, so the call site owns the server
// lifecycle.

// The request kinds a fleetModel routes on, by the structured-output schema
// a request names; a plain-text request is the synthesis.
const (
	kindDecompose  = "decompose"
	kindWorker     = "worker"
	kindSynthesise = "synthesise"
)

// fleetModel is the model endpoint for a whole fleet run. It answers each
// request by its kind and, for a worker, by the brief number its system
// prompt names, so no reply depends on arrival order. It records every
// request's transcript by kind.
type fleetModel struct {
	t         *testing.T
	decompose modeltest.FakeReply
	// worker returns brief n's action for a transcript of the given length;
	// every worker turn reports 10 input and 5 output tokens.
	worker    func(brief, messages int) string
	synthesis modeltest.FakeReply

	mu       sync.Mutex
	requests map[string][][]model.Message
}

func (m *fleetModel) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		ResponseFormat *struct {
			JSONSchema struct {
				Name string `json:"name"`
			} `json:"json_schema"`
		} `json:"response_format"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || len(body.Messages) == 0 {
		m.t.Errorf("decode model request: %v", err)
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	kind := kindSynthesise
	if rf := body.ResponseFormat; rf != nil {
		switch rf.JSONSchema.Name {
		case decomposeSchemaName:
			kind = kindDecompose
		case actionSchemaName:
			kind = kindWorker
		default:
			m.t.Errorf("a request names the unknown schema %q", rf.JSONSchema.Name)
		}
	}
	msgs := make([]model.Message, len(body.Messages))
	for i, msg := range body.Messages {
		msgs[i] = model.Message{Role: model.Role(msg.Role), Content: msg.Content}
	}
	m.mu.Lock()
	if m.requests == nil {
		m.requests = map[string][][]model.Message{}
	}
	m.requests[kind] = append(m.requests[kind], msgs)
	m.mu.Unlock()

	switch kind {
	case kindDecompose:
		writeChatReply(w, m.decompose)
	case kindWorker:
		match := objectivePattern.FindStringSubmatch(msgs[0].Content)
		if match == nil {
			m.t.Error("a worker request names no brief objective")
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		n, _ := strconv.Atoi(match[1])
		writeWorkerReply(w, m.worker(n, len(msgs)))
	case kindSynthesise:
		writeChatReply(w, m.synthesis)
	}
}

// requestsOf returns the transcripts of the requests of one kind, in arrival
// order.
func (m *fleetModel) requestsOf(kind string) [][]model.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([][]model.Message(nil), m.requests[kind]...)
}

// writeChatReply answers a Chat Completions request as the scripted reply
// says: its status and body for an error, else its content, finish reason
// and usage.
func writeChatReply(w http.ResponseWriter, reply modeltest.FakeReply) {
	if reply.Status >= 400 {
		w.WriteHeader(reply.Status)
		_, _ = w.Write([]byte(reply.StatusBody))
		return
	}
	finish := reply.FinishReason
	if finish == "" {
		finish = "stop"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"choices": []any{map[string]any{
			"message":       map[string]any{"content": reply.Content},
			"finish_reason": finish,
		}},
		"usage": map[string]int{
			"prompt_tokens":     reply.Usage.InputTokens,
			"completion_tokens": reply.Usage.OutputTokens,
			"total_tokens":      reply.Usage.InputTokens + reply.Usage.OutputTokens,
		},
	})
}

// synthesisUsage is the usage every scripted synthesis reply reports.
var synthesisUsage = model.Usage{InputTokens: 300, OutputTokens: 200, TotalTokens: 500}

// synthesisReply scripts a completed synthesis reply carrying body.
func synthesisReply(body string) modeltest.FakeReply {
	return modeltest.FakeReply{Content: body, FinishReason: "stop", Usage: synthesisUsage}
}

// putFinding stores f under workerID and briefID as the pool stores a
// finding, and returns its reference.
func putFinding(t *testing.T, store memory.ContextStore, ns memory.Namespace, workerID, briefID string, f Finding) memory.Reference {
	t.Helper()
	body, err := json.Marshal(findingDocument{
		Kind:     findingDocumentKind,
		Version:  findingDocumentVersion,
		WorkerID: workerID,
		BriefID:  briefID,
		Finding:  findingRecord(f),
	})
	if err != nil {
		t.Fatalf("marshal finding: %v", err)
	}
	ref, err := store.Put(context.Background(), ns, bytes.NewReader(body), memory.ArtifactMeta{
		Name:      "fleet-finding-" + briefID + ".json",
		MediaType: findingMediaType,
	})
	if err != nil {
		t.Fatalf("Put finding: %v", err)
	}
	return ref
}

// ranBrief is the pool's result for brief n, run by worker n to f and
// stored at ref.
func ranBrief(n int, ref memory.Reference, f Finding) briefResult {
	return briefResult{
		BriefID:       fmt.Sprintf("brief-%d", n),
		WorkerID:      fmt.Sprintf("worker-%d", n),
		Disposition:   dispositionRan,
		Ref:           ref,
		Status:        f.Status,
		Usage:         f.Usage,
		Turns:         f.Turns,
		CitationCount: len(f.Citations),
		Detail:        f.Detail,
	}
}

// storedRun stores each finding as brief n's, run by worker n, and returns
// the plan and the pool result a lead concludes over.
func storedRun(t *testing.T, store memory.ContextStore, ns memory.Namespace, findings ...Finding) (leadPlan, poolResult) {
	t.Helper()
	plan := leadPlan{Briefs: poolBriefs(len(findings))}
	var pooled poolResult
	for i, f := range findings {
		n := i + 1
		ref := putFinding(t, store, ns, fmt.Sprintf("worker-%d", n), fmt.Sprintf("brief-%d", n), f)
		pooled.Briefs = append(pooled.Briefs, ranBrief(n, ref, f))
		pooled.Usage = addUsage(pooled.Usage, f.Usage)
	}
	return plan, pooled
}

// completedFinding is brief n's completed finding citing urls, each titled
// "Source <n>", from two turns and one search.
func completedFinding(n int, urls ...string) Finding {
	f := Finding{
		Text:   fmt.Sprintf("Finding %d text.", n),
		Usage:  types.Usage{InputTokens: 20, OutputTokens: 10, SearchCount: 1},
		Status: types.StatusCompleted,
		Turns:  2,
	}
	for _, u := range urls {
		f.Citations = append(f.Citations, types.Citation{URI: u, Title: fmt.Sprintf("Source %d", n)})
	}
	return f
}

// rewritingStore serves every Get with old replaced by new in the stored
// body, standing in for a store whose content differs from any in-process
// copy of a finding.
type rewritingStore struct {
	memory.ContextStore
	old, new string
}

func (s rewritingStore) Get(ctx context.Context, ref memory.Reference) (io.ReadCloser, memory.ArtifactMeta, error) {
	rc, meta, err := s.ContextStore.Get(ctx, ref)
	if err != nil {
		return nil, meta, err
	}
	defer rc.Close()
	b, err := io.ReadAll(rc)
	if err != nil {
		return nil, meta, err
	}
	return io.NopCloser(strings.NewReader(strings.ReplaceAll(string(b), s.old, s.new))), meta, nil
}
