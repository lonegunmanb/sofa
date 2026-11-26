package project

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lonegunmanb/sofa/internal/module"
)

const (
	// StateFileName is the workspace state metadata file written inside the state directory.
	StateFileName = "state.json"
	// StateFormatVersion is the schema version of state.json.
	StateFormatVersion = 1
)

// State persists the initialized configuration metadata and latest applied outputs.
type State struct {
	FormatVersion int                `json:"format_version"`
	Configuration StateConfiguration `json:"configuration"`
	Outputs       *StateOutputs      `json:"outputs,omitempty"`
}

// StateConfiguration records the sanitized source and snapshot location for an initialized project.
type StateConfiguration struct {
	Source        string    `json:"source"`
	SourceKey     string    `json:"source_key"`
	Directory     string    `json:"directory"`
	InitializedAt time.Time `json:"initialized_at"`
}

// StateOutputs records the JSON object outputs produced by the latest applied run.
type StateOutputs struct {
	RunDirectory string          `json:"run_directory"`
	AppliedAt    time.Time       `json:"applied_at"`
	Values       json.RawMessage `json:"values"`
}

// SaveOutputs validates that outputs is a JSON object and atomically records it in state.json.
func SaveOutputs(stateDirectory, runDirectory string, outputs []byte) error {
	state, statePath, err := readState(stateDirectory)
	if err != nil {
		return err
	}
	if !jsonObject(outputs) {
		return fmt.Errorf("saved outputs must be a JSON object")
	}
	state.Outputs = &StateOutputs{
		RunDirectory: runDirectory,
		AppliedAt:    time.Now().UTC(),
		Values:       bytes.Clone(outputs),
	}
	if err = writeState(statePath, state); err != nil {
		return fmt.Errorf("save project outputs: %w", err)
	}
	return nil
}

// ReadOutputs opens the initialized project and returns its latest saved JSON outputs.
func ReadOutputs(stateDirectory string) ([]byte, error) {
	proj, err := OpenProject(stateDirectory)
	if err != nil {
		return nil, err
	}
	if proj.State.Outputs == nil {
		return nil, fmt.Errorf("no saved outputs for the current configuration; run sofa apply")
	}
	if !jsonObject(proj.State.Outputs.Values) {
		return nil, fmt.Errorf("saved outputs in project state must be a JSON object")
	}
	return bytes.Clone(proj.State.Outputs.Values), nil
}

func newState(
	stateDirectory string,
	source string,
	sourceKey string,
	initializedAt time.Time,
	local bool,
) State {
	displaySource := module.RedactSource(source)
	if local {
		displaySource = filepath.ToSlash(source)
	}
	return State{
		FormatVersion: StateFormatVersion,
		Configuration: StateConfiguration{
			Source:        displaySource,
			SourceKey:     sourceKey,
			Directory:     filepath.Join(stateDirectory, configDirectoryName),
			InitializedAt: initializedAt,
		},
	}
}

func readState(stateDirectory string) (State, string, error) {
	statePath := filepath.Join(stateDirectory, StateFileName)
	encoded, err := os.ReadFile(statePath)
	if err != nil {
		return State{}, statePath, fmt.Errorf("read project state %q: %w", statePath, err)
	}
	var state State
	if err = json.Unmarshal(encoded, &state); err != nil {
		return State{}, statePath, fmt.Errorf("decode project state %q: %w", statePath, err)
	}
	if state.FormatVersion != StateFormatVersion || strings.TrimSpace(state.Configuration.SourceKey) == "" {
		return State{}, statePath, fmt.Errorf("project state %q has an unsupported or invalid format", statePath)
	}
	return state, statePath, nil
}

func writeState(path string, state State) error {
	encoded, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encode project state: %w", err)
	}
	encoded = append(encoded, '\n')

	temporary, err := os.CreateTemp(filepath.Dir(path), ".sofa-state-")
	if err != nil {
		return fmt.Errorf("create project state temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer func() { _ = os.Remove(temporaryPath) }()

	if err = temporary.Chmod(0o600); err == nil {
		_, err = temporary.Write(encoded)
	}
	if err == nil {
		err = temporary.Sync()
	}
	if closeErr := temporary.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return fmt.Errorf("write project state temporary file: %w", err)
	}
	if err = os.Rename(temporaryPath, path); err != nil {
		return fmt.Errorf("activate project state: %w", err)
	}
	return nil
}

func jsonObject(encoded []byte) bool {
	var value map[string]json.RawMessage
	return json.Unmarshal(encoded, &value) == nil && value != nil
}
