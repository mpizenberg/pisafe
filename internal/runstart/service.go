// Package runstart composes the host-side operations needed to create one
// isolated run.
package runstart

import (
	"context"
	"fmt"
	"net/netip"
	"os"
	"path/filepath"
	"time"

	"github.com/mpizenberg/pisafe/internal/gitstage"
	"github.com/mpizenberg/pisafe/internal/runid"
	"github.com/mpizenberg/pisafe/internal/runimage"
	"github.com/mpizenberg/pisafe/internal/runstate"
)

type Boundary interface {
	Ensure(context.Context, []netip.Prefix) error
}

type ImageInstaller interface {
	Ensure(context.Context, runimage.Artifacts) (runimage.Result, error)
}

type Controller interface {
	StartPrepared(
		context.Context,
		gitstage.PreparedStage,
		string,
		string,
		gitstage.Identity,
	) (runstate.Manifest, error)
}

type Result struct {
	Manifest runstate.Manifest
	Excluded gitstage.ExcludedInputs
	Included []string
	Image    runimage.Result
}

type Service struct {
	boundary        Boundary
	installer       ImageInstaller
	controller      Controller
	artifacts       runimage.Artifacts
	now             func() time.Time
	newRunID        func(string, time.Time) (string, error)
	repositoryRoot  func(context.Context, string) (string, error)
	resolveIdentity func(context.Context, string) (gitstage.Identity, error)
	listExcluded    func(context.Context, string) (gitstage.ExcludedInputs, error)
	selectInputs    func(context.Context, string, gitstage.InputSelection) ([]string, error)
	prepare         func(context.Context, gitstage.PrepareRequest) (gitstage.PreparedStage, error)
}

func New(
	boundary Boundary,
	installer ImageInstaller,
	controller Controller,
	artifacts runimage.Artifacts,
) Service {
	return Service{
		boundary:        boundary,
		installer:       installer,
		controller:      controller,
		artifacts:       artifacts,
		now:             time.Now,
		newRunID:        runid.New,
		repositoryRoot:  gitstage.RepositoryRoot,
		resolveIdentity: gitstage.ResolveIdentity,
		listExcluded:    gitstage.ListExcludedInputs,
		selectInputs:    gitstage.SelectInputs,
		prepare:         gitstage.Prepare,
	}
}

func (service Service) Start(
	ctx context.Context,
	sourcePath string,
	hostPrefixes []netip.Prefix,
	inputs gitstage.InputSelection,
) (Result, error) {
	if service.boundary == nil || service.installer == nil || service.controller == nil {
		return Result{}, fmt.Errorf("run-start dependencies are required")
	}
	root, err := service.repositoryRoot(ctx, sourcePath)
	if err != nil {
		return Result{}, err
	}
	// A run an agent cannot commit in is useless, so a missing identity stops
	// the run here rather than at the agent's first commit.
	identity, err := service.resolveIdentity(ctx, root)
	if err != nil {
		return Result{}, err
	}
	project := runid.ProjectSlug(filepath.Base(root))
	runID, err := service.newRunID(project, service.now())
	if err != nil {
		return Result{}, err
	}
	excluded, err := service.listExcluded(ctx, root)
	if err != nil {
		return Result{}, err
	}
	// Reject an unselectable or credential-shaped input before the slow
	// boundary and image work, not after it.
	included, err := service.selectInputs(ctx, root, inputs)
	if err != nil {
		return Result{}, err
	}
	if err := service.boundary.Ensure(ctx, hostPrefixes); err != nil {
		return Result{}, fmt.Errorf("prepare Lima boundary: %w", err)
	}
	image, err := service.installer.Ensure(ctx, service.artifacts)
	if err != nil {
		return Result{}, fmt.Errorf("install managed run image: %w", err)
	}

	temporary, err := os.MkdirTemp("", "pisafe-stage-*")
	if err != nil {
		return Result{}, fmt.Errorf("create temporary stage directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	prepared, err := service.prepare(ctx, gitstage.PrepareRequest{
		SourcePath: root,
		PackageDir: filepath.Join(temporary, "package"),
		RunID:      runID,
		Inputs:     inputs,
	})
	if err != nil {
		return Result{}, err
	}
	manifest, err := service.controller.StartPrepared(
		ctx,
		prepared,
		project,
		image.ImageID,
		identity,
	)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Manifest: manifest,
		Excluded: withoutIncluded(excluded, included),
		Included: included,
		Image:    image,
	}, nil
}

// withoutIncluded keeps the excluded report honest: a selected input is no
// longer excluded from the run.
func withoutIncluded(excluded gitstage.ExcludedInputs, included []string) gitstage.ExcludedInputs {
	if len(included) == 0 {
		return excluded
	}
	selected := make(map[string]bool, len(included))
	for _, name := range included {
		selected[name] = true
	}
	remaining := func(names []string) []string {
		kept := make([]string, 0, len(names))
		for _, name := range names {
			if !selected[name] {
				kept = append(kept, name)
			}
		}
		return kept
	}
	return gitstage.ExcludedInputs{
		Untracked: remaining(excluded.Untracked),
		Ignored:   remaining(excluded.Ignored),
	}
}
