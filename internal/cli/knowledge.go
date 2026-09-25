package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/rxbynerd/chiron/internal/config"
	"github.com/rxbynerd/chiron/internal/memory"
	"github.com/rxbynerd/chiron/internal/memory/alexandria"
	"github.com/rxbynerd/chiron/internal/memory/billet"
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
// provider, with the closer that ends the store's session (nil when the
// adapter holds none). It returns nil halves when no provider is set. The key
// reference is resolved only when set, since Billet may be keyless; every
// call to the store is bounded by callTimeout.
func buildKnowledge(ctx context.Context, fc config.FleetConfig, callTimeout time.Duration) (memory.Recaller, memory.Rememberer, io.Closer, error) {
	if fc.KnowledgeProvider == "" {
		return nil, nil, nil, nil
	}
	if err := requireKnowledge(fc); err != nil {
		return nil, nil, nil, err
	}
	var apiKey string
	if fc.KnowledgeKeyRef != "" {
		key, err := secret.Default().Resolve(ctx, fc.KnowledgeKeyRef)
		if err != nil {
			return nil, nil, nil, err
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

// newKnowledgeAdapter constructs the adapter for provider. Billet serves both
// halves of the seam over one MCP session, which the returned closer ends;
// Alexandria serves recall only, over REST with no session, so its Rememberer
// and closer are nil and config validation refuses knowledge_remember for it.
func newKnowledgeAdapter(provider string, opts knowledgeOptions) (memory.Recaller, memory.Rememberer, io.Closer, error) {
	switch provider {
	case config.KnowledgeBillet:
		c, err := billet.New(billet.Options{
			Endpoint:       opts.Endpoint,
			APIKey:         opts.APIKey,
			RequestTimeout: opts.RequestTimeout,
			DefaultLimit:   opts.DefaultLimit,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		return c, c, c, nil
	case config.KnowledgeAlexandria:
		c, err := alexandria.New(alexandria.Options{
			Endpoint:       opts.Endpoint,
			APIKey:         opts.APIKey,
			RequestTimeout: opts.RequestTimeout,
			DefaultLimit:   opts.DefaultLimit,
		})
		if err != nil {
			return nil, nil, nil, err
		}
		return c, nil, nil, nil
	default:
		return nil, nil, nil, fmt.Errorf("research --agent worker: unknown fleet.knowledge_provider %q", provider)
	}
}
