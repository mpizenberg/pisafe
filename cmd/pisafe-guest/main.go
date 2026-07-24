// pisafe-guest is the Linux-side helper embedded in the run image. It keeps
// Git materialization identical to the tested host implementation without
// exposing any Mac filesystem path to the VM or container.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/mpizenberg/pisafe/internal/gitstage"
)

func main() {
	if err := run(context.Background(), os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pisafe-guest: %v\n", err)
		os.Exit(1)
	}
}

func run(ctx context.Context, args []string, out io.Writer) error {
	if len(args) != 3 || args[0] != "materialize" {
		return errors.New("usage: pisafe-guest materialize <stage-directory> <workspace>")
	}
	stageDirectory := filepath.Clean(args[1])
	workspace := filepath.Clean(args[2])
	snapshotFile, err := os.Open(filepath.Join(stageDirectory, "snapshot.json"))
	if err != nil {
		return fmt.Errorf("open stage snapshot: %w", err)
	}
	defer snapshotFile.Close()
	decoder := json.NewDecoder(io.LimitReader(snapshotFile, 1<<20))
	decoder.DisallowUnknownFields()
	var snapshot gitstage.Snapshot
	if err := decoder.Decode(&snapshot); err != nil {
		return fmt.Errorf("decode stage snapshot: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("stage snapshot contains trailing data")
		}
		return fmt.Errorf("decode stage snapshot trailer: %w", err)
	}
	if snapshot.SourceRoot != "" {
		return errors.New("stage snapshot unexpectedly contains a host source path")
	}

	materialized, err := gitstage.Materialize(ctx, gitstage.PreparedStage{
		Snapshot:   snapshot,
		BundlePath: filepath.Join(stageDirectory, "source.bundle"),
		PatchPath:  filepath.Join(stageDirectory, "tracked.patch"),
	}, workspace)
	if err != nil {
		return err
	}
	encoder := json.NewEncoder(out)
	return encoder.Encode(materialized)
}
