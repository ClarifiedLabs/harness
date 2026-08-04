package goal

import "context"

type generationContextKey struct{}

type generationBinding struct {
	store      *Store
	generation uint64
}

// WithGeneration binds a prompt context to the current goal identity so a
// canceled stale prompt cannot pause a goal the user has since replaced.
func WithGeneration(ctx context.Context, store *Store, generation uint64) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, generationContextKey{}, generationBinding{
		store:      store,
		generation: generation,
	})
}

// GenerationFromContext returns the prompt-local goal generation.
func GenerationFromContext(ctx context.Context, store *Store) (uint64, bool) {
	if ctx == nil {
		return 0, false
	}
	binding, ok := ctx.Value(generationContextKey{}).(generationBinding)
	if !ok || binding.store != store {
		return 0, false
	}
	return binding.generation, true
}
