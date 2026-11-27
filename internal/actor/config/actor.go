package config

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/dapr/go-sdk/actor"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/lonegunmanb/sofa/internal/daprhost"
)

const configStateKey = "config_store_v1"

type storedItem struct {
	Value     string    `json:"value"`
	IsSecret  bool      `json:"is_secret"`
	UpdatedAt time.Time `json:"updated_at"`
}

type scopeBucket struct {
	Items map[string]storedItem `json:"items,omitempty"`
}

type persistedStore struct {
	Global     scopeBucket            `json:"global"`
	Namespaces map[string]scopeBucket `json:"namespaces,omitempty"`
	Runs       map[string]scopeBucket `json:"runs,omitempty"`
}

func (b scopeBucket) clone() scopeBucket {
	if len(b.Items) == 0 {
		return scopeBucket{}
	}
	return scopeBucket{Items: maps.Clone(b.Items)}
}

func (s persistedStore) clone() persistedStore {
	out := persistedStore{
		Global: s.Global.clone(),
	}
	if len(s.Namespaces) > 0 {
		out.Namespaces = make(map[string]scopeBucket, len(s.Namespaces))
		for k, v := range s.Namespaces {
			out.Namespaces[k] = v.clone()
		}
	}
	if len(s.Runs) > 0 {
		out.Runs = make(map[string]scopeBucket, len(s.Runs))
		for k, v := range s.Runs {
			out.Runs[k] = v.clone()
		}
	}
	return out
}

// ConfigActor manages the three-tier (Run > Namespace > Global) Env and write-only Secret store.
type ConfigActor struct {
	actor.ServerImplBaseCtx

	mu             sync.Mutex
	clientProvider func() daprclient.Client
}

// NewConfigActor constructs a ConfigActor. When clientProvider is non-nil and returns a non-nil
// Dapr client, SetStateManager binds state persistence through daprhost.NewActorStateManager;
// otherwise SetStateManager accepts the caller-provided StateManagerContext (e.g., in unit tests).
func NewConfigActor(clientProvider func() daprclient.Client) *ConfigActor {
	return &ConfigActor{
		clientProvider: clientProvider,
	}
}

// Type implements actor.ServerContext.
func (a *ConfigActor) Type() string {
	return ActorType
}

// SetStateManager overrides ServerImplBaseCtx.SetStateManager to bind a dynamic-port Dapr client
// when clientProvider is configured, while preserving direct StateManagerContext injection in tests.
func (a *ConfigActor) SetStateManager(sm actor.StateManagerContext) {
	if a.clientProvider != nil {
		if c := a.clientProvider(); c != nil {
			if bound := daprhost.NewActorStateManager(a.Type(), a.ID(), c); bound != nil {
				a.ServerImplBaseCtx.SetStateManager(bound)
				return
			}
		}
	}
	a.ServerImplBaseCtx.SetStateManager(sm)
}

// UpsertEntries validates and atomically persists Env/Secret entries into the target scope.
func (a *ConfigActor) UpsertEntries(ctx context.Context, req *UpsertEntriesReq) (*UpsertEntriesResp, error) {
	if req == nil {
		return nil, errors.New("config: upsert request is nil")
	}
	scope, ns, runID, err := normalizeAndValidateScope(req.Scope, req.Namespace, req.RunID)
	if err != nil {
		return nil, err
	}

	seenInBatch := make(map[string]bool, len(req.Entries))
	for _, entry := range req.Entries {
		if err := ValidateKeyName(entry.Name); err != nil {
			return nil, err
		}
		if prevSecret, dup := seenInBatch[entry.Name]; dup {
			if prevSecret != entry.IsSecret {
				return nil, fmt.Errorf("%w: duplicate key %q with conflicting sensitivity in request", ErrKindConflict, entry.Name)
			}
			return nil, fmt.Errorf("config: duplicate key %q in upsert request", entry.Name)
		}
		seenInBatch[entry.Name] = entry.IsSecret
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	store, err := a.loadStore(ctx)
	if err != nil {
		return nil, err
	}

	bucket := store.bucket(scope, ns, runID)
	for _, entry := range req.Entries {
		if existing, exists := bucket.Items[entry.Name]; exists && existing.IsSecret != entry.IsSecret {
			return nil, fmt.Errorf(
				"%w: key %q in scope %q (%s) already exists as %s; delete it before switching sensitivity",
				ErrKindConflict,
				entry.Name,
				scope,
				scopeQualifier(scope, ns, runID),
				kindLabel(existing.IsSecret),
			)
		}
	}

	if bucket.Items == nil {
		bucket.Items = make(map[string]storedItem, len(req.Entries))
	}
	now := time.Now().UTC()
	updated := make([]string, 0, len(req.Entries))
	for _, entry := range req.Entries {
		bucket.Items[entry.Name] = storedItem{
			Value:     entry.Value,
			IsSecret:  entry.IsSecret,
			UpdatedAt: now,
		}
		updated = append(updated, entry.Name)
	}
	sort.Strings(updated)
	store.setBucket(scope, ns, runID, bucket)

	if err := a.saveStore(ctx, store); err != nil {
		return nil, err
	}
	return &UpsertEntriesResp{Updated: updated}, nil
}

// DeleteEntry removes one key from the target scope.
func (a *ConfigActor) DeleteEntry(ctx context.Context, req *DeleteEntryReq) (*DeleteEntryResp, error) {
	if req == nil {
		return nil, errors.New("config: delete entry request is nil")
	}
	scope, ns, runID, err := normalizeAndValidateScope(req.Scope, req.Namespace, req.RunID)
	if err != nil {
		return nil, err
	}
	if err := ValidateKeyName(req.Name); err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	store, err := a.loadStore(ctx)
	if err != nil {
		return nil, err
	}

	bucket := store.bucket(scope, ns, runID)
	if _, exists := bucket.Items[req.Name]; !exists {
		return &DeleteEntryResp{Deleted: false}, nil
	}

	delete(bucket.Items, req.Name)
	store.setBucket(scope, ns, runID, bucket)
	if err := a.saveStore(ctx, store); err != nil {
		return nil, err
	}
	return &DeleteEntryResp{Deleted: true}, nil
}

// DeleteRunScope deletes all Run-scoped Env and Secret entries belonging to RunID.
func (a *ConfigActor) DeleteRunScope(ctx context.Context, req *DeleteRunScopeReq) (*DeleteRunScopeResp, error) {
	if req == nil {
		return nil, errors.New("config: delete run scope request is nil")
	}
	runID := strings.TrimSpace(req.RunID)
	if runID == "" {
		return nil, fmt.Errorf("%w: run_id is required for DeleteRunScope", ErrInvalidScope)
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	store, err := a.loadStore(ctx)
	if err != nil {
		return nil, err
	}

	bucket, exists := store.Runs[runID]
	if !exists {
		return &DeleteRunScopeResp{DeletedCount: 0}, nil
	}
	count := len(bucket.Items)
	delete(store.Runs, runID)
	if err := a.saveStore(ctx, store); err != nil {
		return nil, err
	}
	return &DeleteRunScopeResp{DeletedCount: count}, nil
}

// ListMetadata returns readable Env entries and strictly write-only Secret metadata for one scope.
func (a *ConfigActor) ListMetadata(ctx context.Context, req *ListMetadataReq) (*ListMetadataResp, error) {
	if req == nil {
		return nil, errors.New("config: list metadata request is nil")
	}
	scope, ns, runID, err := normalizeAndValidateScope(req.Scope, req.Namespace, req.RunID)
	if err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	store, err := a.loadStore(ctx)
	if err != nil {
		return nil, err
	}

	bucket := store.bucket(scope, ns, runID)
	envs := make([]EnvEntry, 0)
	secrets := make([]SecretMeta, 0)
	for name, item := range bucket.Items {
		if item.IsSecret {
			secrets = append(secrets, SecretMeta{
				Name:      name,
				Scope:     scope,
				Namespace: ns,
				RunID:     runID,
				UpdatedAt: item.UpdatedAt,
			})
		} else {
			envs = append(envs, EnvEntry{
				Name:      name,
				Value:     item.Value,
				Scope:     scope,
				Namespace: ns,
				RunID:     runID,
				UpdatedAt: item.UpdatedAt,
			})
		}
	}
	sort.Slice(envs, func(i, j int) bool { return envs[i].Name < envs[j].Name })
	sort.Slice(secrets, func(i, j int) bool { return secrets[i].Name < secrets[j].Name })

	return &ListMetadataResp{
		Scope:     scope,
		Namespace: ns,
		RunID:     runID,
		Envs:      envs,
		Secrets:   secrets,
	}, nil
}

// PreflightCheck verifies that all RequiredKeys exist in RunOverrides > Run > Namespace > Global
// without returning any values.
func (a *ConfigActor) PreflightCheck(ctx context.Context, req *PreflightCheckReq) (*PreflightCheckResp, error) {
	if req == nil {
		return nil, errors.New("config: preflight check request is nil")
	}
	ns := NormalizeNamespace(req.Namespace)
	runID := strings.TrimSpace(req.RunID)

	if err := validateOverridesAndKeys(req.RunOverrides, req.RequiredKeys); err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	store, err := a.loadStore(ctx)
	if err != nil {
		return nil, err
	}

	missingSet := make(map[string]struct{})
	for _, key := range req.RequiredKeys {
		if _, ok := store.resolveOne(ns, runID, req.RunOverrides, key); !ok {
			missingSet[key] = struct{}{}
		}
	}
	missing := sortedKeys(missingSet)
	return &PreflightCheckResp{
		OK:          len(missing) == 0,
		MissingKeys: missing,
	}, nil
}

// ResolveForNode resolves ONLY the requested Keys across RunOverrides > Run > Namespace > Global
// for control-plane internal worker dispatch.
func (a *ConfigActor) ResolveForNode(ctx context.Context, req *ResolveForNodeReq) (*ResolveForNodeResp, error) {
	if req == nil {
		return nil, errors.New("config: resolve for node request is nil")
	}
	ns := NormalizeNamespace(req.Namespace)
	runID := strings.TrimSpace(req.RunID)

	if err := validateOverridesAndKeys(req.RunOverrides, req.Keys); err != nil {
		return nil, err
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	store, err := a.loadStore(ctx)
	if err != nil {
		return nil, err
	}

	values := make(map[string]string, len(req.Keys))
	missingSet := make(map[string]struct{})
	for _, key := range req.Keys {
		if val, ok := store.resolveOne(ns, runID, req.RunOverrides, key); ok {
			values[key] = val
		} else {
			missingSet[key] = struct{}{}
		}
	}

	return &ResolveForNodeResp{
		Values:      values,
		MissingKeys: sortedKeys(missingSet),
	}, nil
}

func validateOverridesAndKeys(overrides map[string]string, keys []string) error {
	for k := range overrides {
		if err := ValidateKeyName(k); err != nil {
			return err
		}
	}
	for _, k := range keys {
		if err := ValidateKeyName(k); err != nil {
			return err
		}
	}
	return nil
}

func (a *ConfigActor) loadStore(ctx context.Context) (persistedStore, error) {
	sm := a.GetStateManager()
	if sm == nil {
		return persistedStore{}, errors.New("config: actor state manager not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	exists, err := sm.Contains(ctx, configStateKey)
	if err != nil {
		return persistedStore{}, fmt.Errorf("config: check actor state: %w", err)
	}
	if !exists {
		return persistedStore{}, nil
	}
	var doc persistedStore
	if err := sm.Get(ctx, configStateKey, &doc); err != nil {
		return persistedStore{}, fmt.Errorf("config: load actor state: %w", err)
	}
	return doc.clone(), nil
}

func (a *ConfigActor) saveStore(ctx context.Context, store persistedStore) error {
	sm := a.GetStateManager()
	if sm == nil {
		return errors.New("config: actor state manager not initialized")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := sm.Set(ctx, configStateKey, store.clone()); err != nil {
		sm.Flush(ctx)
		return fmt.Errorf("config: stage actor state: %w", err)
	}
	if err := sm.Save(ctx); err != nil {
		sm.Flush(ctx)
		return fmt.Errorf("config: save actor state: %w", err)
	}
	return nil
}

func (s persistedStore) bucket(scope Scope, ns, runID string) scopeBucket {
	switch scope {
	case ScopeGlobal:
		return s.Global
	case ScopeNamespace:
		if s.Namespaces == nil {
			return scopeBucket{}
		}
		return s.Namespaces[ns]
	case ScopeRun:
		if s.Runs == nil {
			return scopeBucket{}
		}
		return s.Runs[runID]
	default:
		return scopeBucket{}
	}
}

func (s *persistedStore) setBucket(scope Scope, ns, runID string, bucket scopeBucket) {
	switch scope {
	case ScopeGlobal:
		s.Global = bucket
	case ScopeNamespace:
		if len(bucket.Items) == 0 {
			if s.Namespaces != nil {
				delete(s.Namespaces, ns)
			}
			return
		}
		if s.Namespaces == nil {
			s.Namespaces = make(map[string]scopeBucket)
		}
		s.Namespaces[ns] = bucket
	case ScopeRun:
		if len(bucket.Items) == 0 {
			if s.Runs != nil {
				delete(s.Runs, runID)
			}
			return
		}
		if s.Runs == nil {
			s.Runs = make(map[string]scopeBucket)
		}
		s.Runs[runID] = bucket
	}
}

func (s persistedStore) resolveOne(ns, runID string, runOverrides map[string]string, key string) (string, bool) {
	if runOverrides != nil {
		if val, ok := runOverrides[key]; ok {
			return val, true
		}
	}
	if runID != "" && s.Runs != nil {
		if bucket, ok := s.Runs[runID]; ok {
			if item, found := bucket.Items[key]; found {
				return item.Value, true
			}
		}
	}
	if s.Namespaces != nil {
		if bucket, ok := s.Namespaces[ns]; ok {
			if item, found := bucket.Items[key]; found {
				return item.Value, true
			}
		}
	}
	if item, found := s.Global.Items[key]; found {
		return item.Value, true
	}
	return "", false
}

func normalizeAndValidateScope(scope Scope, ns, runID string) (Scope, string, string, error) {
	switch scope {
	case ScopeGlobal:
		if strings.TrimSpace(runID) != "" {
			return "", "", "", fmt.Errorf("%w: run_id must be empty for global scope", ErrInvalidScope)
		}
		if strings.TrimSpace(ns) != "" {
			return "", "", "", fmt.Errorf("%w: namespace must be empty for global scope", ErrInvalidScope)
		}
		return ScopeGlobal, "", "", nil
	case ScopeNamespace:
		if strings.TrimSpace(runID) != "" {
			return "", "", "", fmt.Errorf("%w: run_id must be empty for namespace scope", ErrInvalidScope)
		}
		return ScopeNamespace, NormalizeNamespace(ns), "", nil
	case ScopeRun:
		trimmedRunID := strings.TrimSpace(runID)
		if trimmedRunID == "" {
			return "", "", "", fmt.Errorf("%w: run_id is required for run scope", ErrInvalidScope)
		}
		return ScopeRun, "", trimmedRunID, nil
	default:
		return "", "", "", fmt.Errorf("%w: unsupported scope %q", ErrInvalidScope, scope)
	}
}

func scopeQualifier(scope Scope, ns, runID string) string {
	switch scope {
	case ScopeNamespace:
		return ns
	case ScopeRun:
		return runID
	default:
		return "global"
	}
}

func kindLabel(isSecret bool) string {
	if isSecret {
		return "secret"
	}
	return "env"
}

// ClientStub provides a type-safe Dapr Actor client wrapper over ConfigActor("global")
// populated via daprclient.Client.ImplActorClientStub.
type ClientStub struct {
	actorID        string
	UpsertEntries  func(context.Context, *UpsertEntriesReq) (*UpsertEntriesResp, error)
	DeleteEntry    func(context.Context, *DeleteEntryReq) (*DeleteEntryResp, error)
	DeleteRunScope func(context.Context, *DeleteRunScopeReq) (*DeleteRunScopeResp, error)
	ListMetadata   func(context.Context, *ListMetadataReq) (*ListMetadataResp, error)
	PreflightCheck func(context.Context, *PreflightCheckReq) (*PreflightCheckResp, error)
	ResolveForNode func(context.Context, *ResolveForNodeReq) (*ResolveForNodeResp, error)
}

// Type implements actor.Client.
func (c *ClientStub) Type() string {
	return ActorType
}

// ID implements actor.Client.
func (c *ClientStub) ID() string {
	if c.actorID == "" {
		return GlobalActorID
	}
	return c.actorID
}

// NewClientStub constructs a ClientStub bound to client via ImplActorClientStub.
func NewClientStub(client daprclient.Client) *ClientStub {
	stub := &ClientStub{actorID: GlobalActorID}
	if client != nil {
		client.ImplActorClientStub(stub)
	}
	return stub
}
