package cli

import (
	"context"
	"errors"
	"time"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/secret"
)

// requireKnowledge checks the knowledge fields the composition root needs
// before anything is resolved or dialled: a provider needs an endpoint, and
// Alexandria needs a key. Config validation has already checked their form.
func requireKnowledge(fc config.FleetConfig) error {
	if fc.KnowledgeProvider == "" {
		return nil
	}
	if fc.KnowledgeEndpoint == "" {
		return errors.New("research --agent worker: fleet.knowledge_endpoint is required when fleet.knowledge_provider is set")
	}
	if fc.KnowledgeProvider == config.KnowledgeAlexandria && fc.KnowledgeKeyRef == "" {
		return errors.New("research --agent worker: fleet.knowledge_key_ref is required for the alexandria provider")
	}
	return nil
}

// buildKnowledge builds the knowledge store halves for the configured
// provider. It returns nil halves when no provider is set. The key reference
// is resolved only when set, since Billet may be keyless; every call to the
// store is bounded by callTimeout.
func buildKnowledge(ctx context.Context, fc config.FleetConfig, callTimeout time.Duration) (memory.Recaller, memory.Rememberer, error) {
	if fc.KnowledgeProvider == "" {
		return nil, nil, nil
	}
	if err := requireKnowledge(fc); err != nil {
		return nil, nil, err
	}
	var apiKey string
	if fc.KnowledgeKeyRef != "" {
		key, err := secret.Default().Resolve(ctx, fc.KnowledgeKeyRef)
		if err != nil {
			return nil, nil, err
		}
		apiKey = key
	}
	return newKnowledgeAdapter(fc.KnowledgeProvider, knowledgeOptions{
		Endpoint:       fc.KnowledgeEndpoint,
		APIKey:         apiKey,
		RequestTimeout: callTimeout,
		DefaultLimit:   fc.KnowledgeLimit,
	})
}

// knowledgeOptions are the resolved settings every knowledge adapter takes.
type knowledgeOptions struct {
	Endpoint       string
	APIKey         string
	RequestTimeout time.Duration
	DefaultLimit   int
}

// errKnowledgeNotLinked is returned for any provider until the adapters are
// linked into this build.
var errKnowledgeNotLinked = errors.New("research --agent worker: knowledge provider wiring is not linked in this build")

// newKnowledgeAdapter constructs the adapter for provider.
func newKnowledgeAdapter(provider string, opts knowledgeOptions) (memory.Recaller, memory.Rememberer, error) {
	// TODO(knowledge-seam): construct the adapters here:
	//   billet:     billet.New(billet.Options{Endpoint, APIKey, RequestTimeout}),
	//               returned as both the Recaller and the Rememberer;
	//   alexandria: alexandria.New(alexandria.Options{Endpoint, APIKey, RequestTimeout, DefaultLimit}),
	//               returned as the Recaller with a nil Rememberer.
	return nil, nil, errKnowledgeNotLinked
}
