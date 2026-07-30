package runctl

import (
	"context"
	"errors"
	"fmt"
	"slices"

	"github.com/mpizenberg/pisafe/internal/runcontainer"
	"github.com/mpizenberg/pisafe/internal/runid"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

// publishCaches keeps what a finished run added to each declared cache, then
// trims the namespace it added to. It runs whatever the run's outcome was:
// under immutable generations a publish cannot damage one that already exists,
// so gating it behind a deliberate act would buy nothing and cost cache hits.
func (controller Controller) publishCaches(
	ctx context.Context,
	manifest runstate.Manifest,
) error {
	if len(manifest.Caches) == 0 {
		return nil
	}
	spec := specForManifest(manifest, manifest.Image)
	records, err := controller.store.List()
	if err != nil {
		return fmt.Errorf("publish run caches: %w", err)
	}
	var failures []error
	for _, cache := range manifest.Caches {
		if err := controller.backend.PublishCacheSnapshot(ctx, spec, cache); err != nil {
			failures = append(failures, err)
			continue
		}
		if err := controller.backend.EvictCacheSnapshots(
			ctx,
			spec.ProjectKey,
			cache.Name,
			runcontainer.DefaultCacheGenerations,
			heldGenerations(records, spec.ProjectKey, cache.Name),
		); err != nil {
			failures = append(failures, err)
		}
	}
	if failures == nil {
		return nil
	}
	return fmt.Errorf("publish run caches: %w", errors.Join(failures...))
}

// ResetProjectCache empties one project's shared cache, which is what makes
// the cache disposable in practice rather than in principle. The session store
// is left alone: a transcript is not reproducible and so is not disposable.
func (controller Controller) ResetProjectCache(
	ctx context.Context,
	project runid.Project,
) error {
	records, err := controller.store.List()
	if err != nil {
		return err
	}
	for _, record := range records {
		if holdsCacheGeneration(record, project.Key) {
			return fmt.Errorf(
				"run %q still mounts a generation of this project's cache; discard it first",
				record.RunID,
			)
		}
	}
	return controller.backend.ResetProjectCache(ctx, project.Key)
}

// holdsCacheGeneration reports whether a recorded run may still stack an
// overlay on a generation of one project's cache: an active run has one
// mounted now, and a stopped run remounts its own when it resumes. overlayfs
// leaves behaviour undefined when a mounted lower goes away, which is why both
// eviction and reset ask this before removing anything.
func holdsCacheGeneration(record runstate.Manifest, projectKey string) bool {
	if record.ProjectKey != projectKey {
		return false
	}
	if record.State != runstate.StateActive && record.State != runstate.StateStopped {
		return false
	}
	return slices.ContainsFunc(record.Caches, func(cache runcontainer.CacheMount) bool {
		return cache.Snapshot != ""
	})
}

func heldGenerations(records []runstate.Manifest, projectKey, name string) []string {
	var held []string
	for _, record := range records {
		if !holdsCacheGeneration(record, projectKey) {
			continue
		}
		for _, cache := range record.Caches {
			if cache.Name != name || cache.Snapshot == "" {
				continue
			}
			if !slices.Contains(held, cache.Snapshot) {
				held = append(held, cache.Snapshot)
			}
		}
	}
	return held
}
