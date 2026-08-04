// Package piagent is what pisafe tells the agent inside a run: the inference
// providers it may reach, and the model it opens on. The controller composes
// the document on the Mac and the guest helper installs it in the run, so the
// shape belongs to neither of them.
package piagent

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
)

// Configuration is one run's whole inference configuration.
type Configuration struct {
	// Models is the ~/.pi/agent/models.json document, verbatim. It is the one
	// place a run's revocable capability appears.
	Models  json.RawMessage `json:"models"`
	Default Selection       `json:"default,omitzero"`
}

// Selection is the model a run opens on and the reasoning effort it opens at.
// Pi chooses for itself when nothing names one, from a table keyed by its own
// provider names — which are not pisafe's, so a run would otherwise open on
// whatever its catalog happens to list first.
type Selection struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	Thinking string `json:"thinking"`
}

// thinkingLevels are the reasoning efforts Pi understands. One it does not is
// refused where it is composed, because a run reads its settings before it has
// any way to report what was wrong with them.
var thinkingLevels = []string{"off", "minimal", "low", "medium", "high", "xhigh", "max"}

// Named reports whether a run is told which model to open on.
func (selection Selection) Named() bool {
	return selection != Selection{}
}

func (selection Selection) Validate() error {
	if !selection.Named() {
		return nil
	}
	if selection.Provider == "" || selection.Model == "" {
		return errors.New("a default model names both a provider and a model")
	}
	if !slices.Contains(thinkingLevels, selection.Thinking) {
		return fmt.Errorf("unknown reasoning effort %q", selection.Thinking)
	}
	return nil
}

// Validate holds the document to what a run could act on: providers to reach,
// and a default naming one of them rather than an upstream the run was never
// given.
func (configuration Configuration) Validate() error {
	var document struct {
		Providers map[string]json.RawMessage `json:"providers"`
	}
	if err := json.Unmarshal(configuration.Models, &document); err != nil {
		return fmt.Errorf("models configuration is not a JSON object: %w", err)
	}
	if len(document.Providers) == 0 {
		return errors.New("models configuration names no provider")
	}
	if err := configuration.Default.Validate(); err != nil {
		return err
	}
	if !configuration.Default.Named() {
		return nil
	}
	if _, offered := document.Providers[configuration.Default.Provider]; !offered {
		return fmt.Errorf(
			"default model names provider %q, which the run is not given",
			configuration.Default.Provider,
		)
	}
	return nil
}

// ModelsFile renders what ~/.pi/agent/models.json holds inside a run. It is
// indented here rather than on the wire, so what a run shows whoever opens it
// does not depend on how the document reached it.
func (configuration Configuration) ModelsFile() ([]byte, error) {
	var content bytes.Buffer
	if err := json.Indent(&content, configuration.Models, "", "  "); err != nil {
		return nil, fmt.Errorf("render models configuration: %w", err)
	}
	return append(content.Bytes(), '\n'), nil
}
