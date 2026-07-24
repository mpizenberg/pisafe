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
	) (runstate.Manifest, error)
}

type Result struct {
	Manifest runstate.Manifest
	Excluded gitstage.ExcludedInputs
	Image    runimage.Result
}

type Service struct {
	boundary       Boundary
	installer      ImageInstaller
	controller     Controller
	artifacts      runimage.Artifacts
	now            func() time.Time
	newRunID       func(string, time.Time) (string, error)
	repositoryRoot func(context.Context, string) (string, error)
	listExcluded   func(context.Context, string) (gitstage.ExcludedInputs, error)
	prepare        func(context.Context, string, string, string) (gitstage.PreparedStage, error)
}

func New(
	boundary Boundary,
	installer ImageInstaller,
	controller Controller,
	artifacts runimage.Artifacts,
) Service {
	return Service{
		boundary:       boundary,
		installer:      installer,
		controller:     controller,
		artifacts:      artifacts,
		now:            time.Now,
		newRunID:       runid.New,
		repositoryRoot: gitstage.RepositoryRoot,
		listExcluded:   gitstage.ListExcludedInputs,
		prepare:        gitstage.Prepare,
	}
}

func (service Service) Start(
	ctx context.Context,
	sourcePath string,
	hostPrefixes []netip.Prefix,
) (Result, error) {
	if service.boundary == nil || service.installer == nil || service.controller == nil {
		return Result{}, fmt.Errorf("run-start dependencies are required")
	}
	root, err := service.repositoryRoot(ctx, sourcePath)
	if err != nil {
		return Result{}, err
	}
	project := runid.ProjectSlug(filepath.Base(root))
	runID, err := service.newRunID(project, service.now())
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
	excluded, err := service.listExcluded(ctx, root)
	if err != nil {
		return Result{}, err
	}

	temporary, err := os.MkdirTemp("", "pisafe-stage-*")
	if err != nil {
		return Result{}, fmt.Errorf("create temporary stage directory: %w", err)
	}
	defer os.RemoveAll(temporary)
	packagePath := filepath.Join(temporary, "package")
	prepared, err := service.prepare(ctx, root, packagePath, runID)
	if err != nil {
		return Result{}, err
	}
	manifest, err := service.controller.StartPrepared(
		ctx,
		prepared,
		project,
		image.ImageID,
	)
	if err != nil {
		return Result{}, err
	}
	return Result{
		Manifest: manifest,
		Excluded: excluded,
		Image:    image,
	}, nil
}
