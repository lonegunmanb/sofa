package project

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/lonegunmanb/sofa/internal/harness"
	"github.com/lonegunmanb/sofa/internal/module"
)

const (
	configDirectoryName  = "config"
	markerFileName       = ".initialized.json"
	markerFormatVersion  = 2
	modulesDirectoryName = "modules"
	transactionFileName  = ".initializing"
)

// InitOptions configures workspace initialization, module fetching, and harness plugin locking.
type InitOptions struct {
	Upgrade             bool
	Fetch               module.FetchFunc
	ReleaseFetcher      harness.ReleaseFetcher
	AttestationVerifier harness.AttestationVerifier
	LookupEnv           func(string) (string, bool)
}

// Project describes an opened, validated sofa state directory.
type Project struct {
	StateDirectory   string
	ConfigDirectory  string
	ModulesDirectory string
	LockFilePath     string
	State            State
}

type marker struct {
	FormatVersion int       `json:"format_version"`
	SourceKey     string    `json:"source_key"`
	InitializedAt time.Time `json:"initialized_at"`
}

// Init snapshots sourceLocation into <stateDirectory>/config, recursively installs modules,
// verifies and locks remote harness plugins into plugins.lock.json, and writes state.json.
func Init(ctx context.Context, sourceLocation, stateDirectory string, options InitOptions) error {
	if ctx == nil {
		ctx = context.Background()
	}
	state, err := filepath.Abs(stateDirectory)
	if err != nil {
		return fmt.Errorf("resolve sofa state directory: %w", err)
	}
	state = filepath.Clean(state)

	localSource := localConfigurationSource(sourceLocation)
	source, sourceKey, cleanupSource, err := resolveConfigurationSource(ctx, sourceLocation, options.Fetch)
	if err != nil {
		return err
	}
	defer cleanupSource()

	if isWithin(state, source) {
		return fmt.Errorf("configuration directory %q must be outside sofa state directory %q", source, state)
	}
	if err = os.MkdirAll(state, 0o700); err != nil {
		return fmt.Errorf("create sofa state directory: %w", err)
	}

	transaction := filepath.Join(state, transactionFileName)
	interrupted, err := pathExists(transaction)
	if err != nil {
		return fmt.Errorf("inspect initialization transaction: %w", err)
	}
	sourceChanged, err := configurationSourceChanged(state, sourceKey)
	if err != nil {
		return err
	}
	effectiveUpgrade := options.Upgrade || sourceChanged || interrupted

	if err = os.WriteFile(transaction, []byte("initializing\n"), 0o600); err != nil {
		return fmt.Errorf("begin initialization transaction: %w", err)
	}

	temporary, err := os.MkdirTemp(state, ".sofa-config-")
	if err != nil {
		return fmt.Errorf("create configuration staging directory: %w", err)
	}
	defer func() { _ = os.RemoveAll(temporary) }()

	staging := filepath.Join(temporary, "content")
	if err = module.CopyCleanDirectory(ctx, source, staging); err != nil {
		return fmt.Errorf("copy configuration: %w", err)
	}

	modules := filepath.Join(state, modulesDirectoryName)
	if err = module.Init(ctx, source, module.InitOptions{
		ModulesDirectory: modules,
		Upgrade:          effectiveUpgrade,
		Fetch:            options.Fetch,
	}); err != nil {
		return fmt.Errorf("initialize modules: %w", err)
	}

	initializedAt := time.Now().UTC()
	encoded, err := json.MarshalIndent(marker{
		FormatVersion: markerFormatVersion,
		SourceKey:     sourceKey,
		InitializedAt: initializedAt,
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encode initialization marker: %w", err)
	}
	if err = os.WriteFile(filepath.Join(staging, markerFileName), append(encoded, '\n'), 0o600); err != nil {
		return fmt.Errorf("write initialization marker: %w", err)
	}
	if err = activateConfiguration(staging, filepath.Join(state, configDirectoryName)); err != nil {
		return err
	}

	if _, err = harness.LockPlugins(ctx, state, harness.LockOptions{
		Upgrade:             effectiveUpgrade,
		ReleaseFetcher:      options.ReleaseFetcher,
		AttestationVerifier: options.AttestationVerifier,
		LookupEnv:           options.LookupEnv,
	}); err != nil {
		return fmt.Errorf("lock harness plugins: %w", err)
	}

	stateSource := sourceLocation
	if localSource {
		stateSource = source
	}
	projectState := newState(state, stateSource, sourceKey, initializedAt, localSource)
	if err = writeState(filepath.Join(state, StateFileName), projectState); err != nil {
		return fmt.Errorf("write initialized project state: %w", err)
	}
	return removeTransaction(transaction)
}

// Open validates stateDirectory and returns its active config and modules directories.
func Open(stateDirectory string) (string, string, error) {
	proj, err := OpenProject(stateDirectory)
	if err != nil {
		return "", "", err
	}
	return proj.ConfigDirectory, proj.ModulesDirectory, nil
}

// OpenProject validates stateDirectory and returns full metadata for the initialized project.
func OpenProject(stateDirectory string) (Project, error) {
	state, err := filepath.Abs(stateDirectory)
	if err != nil {
		return Project{}, fmt.Errorf("resolve sofa state directory: %w", err)
	}
	state = filepath.Clean(state)
	config := filepath.Join(state, configDirectoryName)
	modules := filepath.Join(state, modulesDirectoryName)

	interrupted, err := pathExists(filepath.Join(state, transactionFileName))
	if err != nil {
		return Project{}, notInitializedError(state, fmt.Errorf("inspect initialization transaction: %w", err))
	}
	if interrupted {
		return Project{}, fmt.Errorf(
			"working directory initialization was interrupted at %q; run sofa init <source>", state,
		)
	}

	encoded, err := os.ReadFile(filepath.Join(config, markerFileName))
	if err != nil {
		return Project{}, notInitializedError(state, err)
	}
	var initialized marker
	if err = json.Unmarshal(encoded, &initialized); err != nil {
		return Project{}, notInitializedError(state, err)
	}
	if initialized.FormatVersion != markerFormatVersion || strings.TrimSpace(initialized.SourceKey) == "" {
		return Project{}, notInitializedError(state, fmt.Errorf("invalid initialization marker"))
	}

	projectState, _, err := readState(state)
	if err != nil {
		return Project{}, notInitializedError(state, err)
	}
	if projectState.Configuration.SourceKey != initialized.SourceKey ||
		!projectState.Configuration.InitializedAt.Equal(initialized.InitializedAt) ||
		filepath.Clean(projectState.Configuration.Directory) != filepath.Clean(config) {
		return Project{}, notInitializedError(
			state, fmt.Errorf("project state does not match initialized configuration"),
		)
	}

	info, err := os.Stat(modules)
	if err != nil || !info.IsDir() {
		if err == nil {
			err = fmt.Errorf("modules path is not a directory")
		}
		return Project{}, notInitializedError(state, err)
	}

	return Project{
		StateDirectory:   state,
		ConfigDirectory:  config,
		ModulesDirectory: modules,
		LockFilePath:     filepath.Join(state, harness.LockFileName),
		State:            projectState,
	}, nil
}

func activateConfiguration(staging, destination string) error {
	info, err := os.Stat(destination)
	switch {
	case errors.Is(err, os.ErrNotExist):
		if err = os.Rename(staging, destination); err != nil {
			return fmt.Errorf("activate configuration: %w", err)
		}
		return nil
	case err != nil:
		return fmt.Errorf("inspect active configuration: %w", err)
	case !info.IsDir():
		return fmt.Errorf("active configuration %q is not a directory", destination)
	}

	backup, err := os.MkdirTemp(filepath.Dir(destination), ".sofa-config-backup-")
	if err != nil {
		return fmt.Errorf("reserve configuration backup path: %w", err)
	}
	if err = os.Remove(backup); err != nil {
		return fmt.Errorf("prepare configuration backup path: %w", err)
	}
	defer func() { _ = os.RemoveAll(backup) }()
	if err = os.Rename(destination, backup); err != nil {
		return fmt.Errorf("back up active configuration: %w", err)
	}
	if err = os.Rename(staging, destination); err != nil {
		restoreErr := os.Rename(backup, destination)
		return errors.Join(fmt.Errorf("activate configuration: %w", err), restoreErr)
	}
	if err = os.RemoveAll(backup); err != nil {
		return fmt.Errorf("remove previous configuration: %w", err)
	}
	return nil
}

func absoluteDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	info, err := os.Stat(absolute)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", fmt.Errorf("%q is not a directory", path)
	}
	return absolute, nil
}

func isWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func configurationSourceChanged(state, sourceKey string) (bool, error) {
	encoded, err := os.ReadFile(filepath.Join(state, configDirectoryName, markerFileName))
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("read previous initialization marker: %w", err)
	}
	var previous marker
	if json.Unmarshal(encoded, &previous) != nil {
		return true, nil
	}
	return previous.SourceKey != sourceKey, nil
}

func resolveConfigurationSource(
	ctx context.Context,
	source string,
	fetch module.FetchFunc,
) (string, string, func(), error) {
	if strings.TrimSpace(source) == "" {
		return "", "", func() {}, fmt.Errorf("configuration source is required")
	}
	if localConfigurationSource(source) {
		directory, resolveErr := absoluteDirectory(source)
		if resolveErr != nil {
			return "", "", func() {}, fmt.Errorf("resolve configuration directory: %w", resolveErr)
		}
		return directory, configurationSourceKey(directory), func() {}, nil
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		return "", "", func() {}, fmt.Errorf("get configuration source working directory: %w", err)
	}
	temporary, err := os.MkdirTemp("", "sofa-source-")
	if err != nil {
		return "", "", func() {}, fmt.Errorf("create configuration source staging directory: %w", err)
	}
	cleanup := func() { _ = os.RemoveAll(temporary) }
	destination := filepath.Join(temporary, "content")
	if err = os.Mkdir(destination, 0o700); err != nil {
		cleanup()
		return "", "", func() {}, fmt.Errorf("create configuration source destination: %w", err)
	}
	if fetch == nil {
		fetch = module.Fetch
	}
	if err = fetch(ctx, source, destination, workingDirectory); err != nil {
		cleanup()
		if contextErr := ctx.Err(); contextErr != nil {
			return "", "", func() {}, fmt.Errorf("fetch configuration source: %w", contextErr)
		}
		return "", "", func() {}, fmt.Errorf("fetch configuration source: %s", module.RedactSourceInError(err.Error(), source))
	}
	return destination, configurationSourceKey(source), cleanup, nil
}

func localConfigurationSource(source string) bool {
	if module.IsLocalSource(source) {
		return true
	}
	_, err := os.Stat(source)
	return err == nil
}

func configurationSourceKey(source string) string {
	return fmt.Sprintf("%x", sha256.Sum256([]byte(source)))
}

func pathExists(path string) (bool, error) {
	_, err := os.Stat(path)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

func removeTransaction(path string) error {
	if err := os.Remove(path); err != nil {
		return fmt.Errorf("complete initialization transaction: %w", err)
	}
	return nil
}

func notInitializedError(state string, cause error) error {
	return fmt.Errorf("working directory is not initialized at %q; run sofa init <source>: %w", state, cause)
}
