package config

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// ActorType is the Dapr Actor type name for the configuration and secrets center.
	ActorType = "ConfigActor"
	// GlobalActorID is the singleton actor ID for ConfigActor.
	GlobalActorID = "global"
	// DefaultNamespace is the normalized namespace used when Namespace is empty.
	DefaultNamespace = "default"
)

var (
	// ErrInvalidKeyName indicates a key name failed ^[A-Za-z_][A-Za-z0-9_]*$ validation.
	ErrInvalidKeyName = errors.New("config: invalid environment variable key name")
	// ErrInvalidScope indicates an invalid scope or missing scope qualifier (e.g., RunID).
	ErrInvalidScope = errors.New("config: invalid scope")
	// ErrKindConflict indicates a same-scope collision between Env and Secret for the same key.
	ErrKindConflict = errors.New("config: key sensitivity kind conflict in same scope")

	keyNamePattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)
)

// Scope identifies one of the three configuration inheritance tiers:
// Run > Namespace > Global.
type Scope string

const (
	ScopeGlobal    Scope = "global"
	ScopeNamespace Scope = "namespace"
	ScopeRun       Scope = "run"
)

// NormalizeNamespace trims whitespace and maps an empty namespace to DefaultNamespace ("default").
func NormalizeNamespace(ns string) string {
	trimmed := strings.TrimSpace(ns)
	if trimmed == "" {
		return DefaultNamespace
	}
	return trimmed
}

// ValidateKeyName enforces POSIX-style environment variable identifier syntax ^[A-Za-z_][A-Za-z0-9_]*$.
func ValidateKeyName(name string) error {
	if !keyNamePattern.MatchString(name) {
		return fmt.Errorf("%w: %q must match ^[A-Za-z_][A-Za-z0-9_]*$", ErrInvalidKeyName, name)
	}
	return nil
}

// EntryInput describes one Env or Secret upsert item.
type EntryInput struct {
	Name     string `json:"name"`
	Value    string `json:"value"`
	IsSecret bool   `json:"is_secret"`
}

// EnvEntry represents a readable non-secret environment variable in a scope.
type EnvEntry struct {
	Name      string    `json:"name"`
	Value     string    `json:"value"`
	Scope     Scope     `json:"scope"`
	Namespace string    `json:"namespace,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SecretMeta represents write-only metadata for a secret entry.
// It strictly omits any Value field so secret plaintext can never be returned by listing APIs.
type SecretMeta struct {
	Name      string    `json:"name"`
	Scope     Scope     `json:"scope"`
	Namespace string    `json:"namespace,omitempty"`
	RunID     string    `json:"run_id,omitempty"`
	UpdatedAt time.Time `json:"updated_at"`
}

// UpsertEntriesReq upserts one or more Env/Secret entries into a single scope.
type UpsertEntriesReq struct {
	Scope     Scope        `json:"scope"`
	Namespace string       `json:"namespace,omitempty"`
	RunID     string       `json:"run_id,omitempty"`
	Entries   []EntryInput `json:"entries"`
}

// UpsertEntriesResp reports the sorted key names written by UpsertEntries.
type UpsertEntriesResp struct {
	Updated []string `json:"updated,omitempty"`
}

// DeleteEntryReq deletes a single Env or Secret key from the specified scope.
type DeleteEntryReq struct {
	Scope     Scope  `json:"scope"`
	Namespace string `json:"namespace,omitempty"`
	RunID     string `json:"run_id,omitempty"`
	Name      string `json:"name"`
}

// DeleteEntryResp reports whether the key existed and was removed.
type DeleteEntryResp struct {
	Deleted bool `json:"deleted"`
}

// DeleteRunScopeReq destroys all Run-scoped Env and Secret entries for RunID.
type DeleteRunScopeReq struct {
	RunID string `json:"run_id"`
}

// DeleteRunScopeResp reports how many Run-scoped entries were removed.
type DeleteRunScopeResp struct {
	DeletedCount int `json:"deleted_count"`
}

// ListMetadataReq queries Env entries and write-only Secret metadata for one scope.
type ListMetadataReq struct {
	Scope     Scope  `json:"scope"`
	Namespace string `json:"namespace,omitempty"`
	RunID     string `json:"run_id,omitempty"`
}

// ListMetadataResp returns readable Env entries and write-only Secret metadata.
type ListMetadataResp struct {
	Scope     Scope        `json:"scope"`
	Namespace string       `json:"namespace,omitempty"`
	RunID     string       `json:"run_id,omitempty"`
	Envs      []EnvEntry   `json:"envs"`
	Secrets   []SecretMeta `json:"secrets"`
}

// PreflightCheckReq validates that all RequiredKeys are satisfied by the merged
// RunOverrides > Run > Namespace > Global view at second 0 of Run submission.
type PreflightCheckReq struct {
	Namespace    string            `json:"namespace,omitempty"`
	RunID        string            `json:"run_id,omitempty"`
	RunOverrides map[string]string `json:"run_overrides,omitempty"`
	RequiredKeys []string          `json:"required_keys"`
}

// PreflightCheckResp reports missing required keys without exposing any secret values.
type PreflightCheckResp struct {
	OK          bool     `json:"ok"`
	MissingKeys []string `json:"missing_keys,omitempty"`
}

// ResolveForNodeReq resolves the minimal requested Keys for internal worker dispatch.
type ResolveForNodeReq struct {
	RunID        string            `json:"run_id,omitempty"`
	Namespace    string            `json:"namespace,omitempty"`
	RunOverrides map[string]string `json:"run_overrides,omitempty"`
	Keys         []string          `json:"keys"`
}

// ResolveForNodeResp returns plaintext values strictly scoped to the requested Keys.
type ResolveForNodeResp struct {
	Values      map[string]string `json:"values"`
	MissingKeys []string          `json:"missing_keys,omitempty"`
}
