//go:build integration

package config_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dapr/go-sdk/actor"
	"github.com/dapr/go-sdk/actor/runtime"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/service/common"
	daprhttp "github.com/dapr/go-sdk/service/http"
	"github.com/go-chi/chi/v5"
	configactor "github.com/lonegunmanb/sofa/internal/actor/config"
	"github.com/lonegunmanb/sofa/internal/daprhost"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const ultraSecretToken = "ULTRA_SECRET_TOKEN_778899"
const ephemeralRunSecret = "RUN_EPHEMERAL_SECRET_4455"

type itCallbackServer struct {
	port        int
	svc         common.Service
	errCh       chan error
	validTokens atomic.Int64
	badTokens   atomic.Int64
}

func startConfigActorCallbackServer(
	t *testing.T,
	clientRef *atomic.Pointer[daprclient.Client],
) (*itCallbackServer, *atomic.Value) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	var expectedToken atomic.Value
	expectedToken.Store("")

	srv := &itCallbackServer{
		port:  port,
		errCh: make(chan error, 1),
	}

	router := chi.NewRouter()
	router.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			want, _ := expectedToken.Load().(string)
			if want == "" || r.Header.Get(common.APITokenKey) != want {
				srv.badTokens.Add(1)
				http.Error(w, "unauthorized dapr-api-token", http.StatusUnauthorized)
				return
			}
			srv.validTokens.Add(1)
			next.ServeHTTP(w, r)
		})
	})

	svc := daprhttp.NewServiceWithMux(fmt.Sprintf("127.0.0.1:%d", port), router)
	svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
		return configactor.NewConfigActor(func() daprclient.Client {
			c := clientRef.Load()
			if c == nil {
				return nil
			}
			return *c
		})
	})
	srv.svc = svc

	go func() {
		err := svc.Start()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			srv.errCh <- err
		}
		close(srv.errCh)
	}()

	require.Eventually(t, func() bool {
		conn, dialErr := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 100*time.Millisecond)
		if dialErr != nil {
			return false
		}
		_ = conn.Close()
		return true
	}, 5*time.Second, 15*time.Millisecond)

	return srv, &expectedToken
}

func (s *itCallbackServer) stop(t *testing.T) {
	t.Helper()
	if s.svc != nil {
		_ = s.svc.Stop()
	}
	select {
	case err := <-s.errCh:
		if err != nil {
			t.Logf("callback server exited with: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for callback server shutdown")
	}
}

func TestConfigActor_EncryptedThreeTierPersistenceAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	stateDir := t.TempDir()
	var activeClient atomic.Pointer[daprclient.Client]

	// ---- Phase 1: Start Manager 1 + ConfigActor, exercise all RPCs via NewClientStub ----
	cb1, tok1 := startConfigActorCallbackServer(t, &activeClient)
	defer cb1.stop(t)

	mgr1, err := daprhost.NewManager(daprhost.Options{
		StateDir: stateDir,
		AppID:    "sofa-config-actor-it",
		AppPort:  cb1.port,
	})
	require.NoError(t, err)
	tok1.Store(mgr1.AppAPIToken())

	client1, err := mgr1.Start(ctx)
	require.NoError(t, err)
	activeClient.Store(&client1)
	defer func() {
		_ = mgr1.Stop(context.Background())
	}()

	stub1 := configactor.NewClientStub(client1)

	// Write Global, Namespace("proj-a"), Namespace("default"), and Run tiers
	upGlobal, err := stub1.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeGlobal,
		Entries: []configactor.EntryInput{
			{Name: "GLOBAL_URL", Value: "https://global.example.com", IsSecret: false},
			{Name: "SHARED_TIER", Value: "from-global", IsSecret: false},
			{Name: "PRIMARY_TOKEN", Value: ultraSecretToken, IsSecret: true},
		},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"GLOBAL_URL", "PRIMARY_TOKEN", "SHARED_TIER"}, upGlobal.Updated)

	// Same-scope Secret -> Env conflict rejected across real Dapr RPC
	_, err = stub1.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeGlobal,
		Entries: []configactor.EntryInput{
			{Name: "PRIMARY_TOKEN", Value: "downgraded", IsSecret: false},
		},
	})
	require.Error(t, err)

	_, err = stub1.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope:     configactor.ScopeNamespace,
		Namespace: "proj-a",
		Entries: []configactor.EntryInput{
			{Name: "SHARED_TIER", Value: "from-proj-a", IsSecret: false},
			{Name: "PROJ_A_SECRET", Value: "proj-a-secret-123", IsSecret: true},
		},
	})
	require.NoError(t, err)

	_, err = stub1.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope:     configactor.ScopeNamespace,
		Namespace: "", // normalizes to "default"
		Entries: []configactor.EntryInput{
			{Name: "DEFAULT_VAR", Value: "from-default-ns", IsSecret: false},
		},
	})
	require.NoError(t, err)

	_, err = stub1.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeRun,
		RunID: "run-it-1",
		Entries: []configactor.EntryInput{
			{Name: "SHARED_TIER", Value: "from-run-it-1", IsSecret: false},
		},
	})
	require.NoError(t, err)

	_, err = stub1.UpsertEntries(ctx, &configactor.UpsertEntriesReq{
		Scope: configactor.ScopeRun,
		RunID: "run-it-ephemeral",
		Entries: []configactor.EntryInput{
			{Name: "TEMP_SECRET", Value: ephemeralRunSecret, IsSecret: true},
		},
	})
	require.NoError(t, err)

	// ListMetadata must never return Secret plaintext
	globalMeta, err := stub1.ListMetadata(ctx, &configactor.ListMetadataReq{
		Scope: configactor.ScopeGlobal,
	})
	require.NoError(t, err)
	rawGlobalMeta, err := json.Marshal(globalMeta)
	require.NoError(t, err)
	assert.NotContains(t, string(rawGlobalMeta), ultraSecretToken)
	require.Len(t, globalMeta.Secrets, 1)
	assert.Equal(t, "PRIMARY_TOKEN", globalMeta.Secrets[0].Name)

	// PreflightCheck with missing & present keys
	pf1, err := stub1.PreflightCheck(ctx, &configactor.PreflightCheckReq{
		Namespace:    "proj-a",
		RunID:        "run-it-1",
		RunOverrides: map[string]string{"EPHEMERAL_OVERRIDE": "yes"},
		RequiredKeys: []string{"GLOBAL_URL", "PRIMARY_TOKEN", "PROJ_A_SECRET", "EPHEMERAL_OVERRIDE", "ABSENT_KEY"},
	})
	require.NoError(t, err)
	assert.False(t, pf1.OK)
	assert.Equal(t, []string{"ABSENT_KEY"}, pf1.MissingKeys)
	rawPF1, err := json.Marshal(pf1)
	require.NoError(t, err)
	assert.NotContains(t, string(rawPF1), ultraSecretToken)

	// ResolveForNode verifies RunOverrides > Run > Namespace > Global and minimal key filtering
	res1, err := stub1.ResolveForNode(ctx, &configactor.ResolveForNodeReq{
		Namespace: "proj-a",
		RunID:     "run-it-1",
		Keys:      []string{"GLOBAL_URL", "SHARED_TIER", "PRIMARY_TOKEN", "PROJ_A_SECRET"},
	})
	require.NoError(t, err)
	assert.Empty(t, res1.MissingKeys)
	assert.Equal(t, map[string]string{
		"GLOBAL_URL":    "https://global.example.com",
		"SHARED_TIER":   "from-run-it-1",
		"PRIMARY_TOKEN": ultraSecretToken,
		"PROJ_A_SECRET": "proj-a-secret-123",
	}, res1.Values)

	// DeleteRunScope wipes ephemeral run scope
	delRun, err := stub1.DeleteRunScope(ctx, &configactor.DeleteRunScopeReq{RunID: "run-it-ephemeral"})
	require.NoError(t, err)
	assert.Equal(t, 1, delRun.DeletedCount)

	assert.Zero(t, cb1.badTokens.Load())
	assert.Positive(t, cb1.validTokens.Load())

	// Deactivate local SDK actor cache and stop Manager 1 to flush SQLite cleanly.
	_ = runtime.GetActorRuntimeInstanceContext().Deactivate(ctx, configactor.ActorType, configactor.GlobalActorID)
	require.NoError(t, mgr1.Stop(ctx))
	activeClient.Store(nil)
	cb1.stop(t)

	// Verify actor-state.db and -wal contain zero plaintext secret bytes!
	dbPath := filepath.Join(stateDir, "actor-state.db")
	dbBytes, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	assert.NotEmpty(t, dbBytes)
	assert.False(t, bytes.Contains(dbBytes, []byte(ultraSecretToken)),
		"actor-state.db must NOT contain plaintext secret %q", ultraSecretToken)
	assert.False(t, bytes.Contains(dbBytes, []byte(ephemeralRunSecret)),
		"actor-state.db must NOT contain deleted plaintext secret %q", ephemeralRunSecret)
	if walBytes, walErr := os.ReadFile(dbPath + "-wal"); walErr == nil {
		assert.False(t, bytes.Contains(walBytes, []byte(ultraSecretToken)))
		assert.False(t, bytes.Contains(walBytes, []byte(ephemeralRunSecret)))
	}

	// ---- Phase 2: Restart fresh Manager 2 on SAME StateDir and verify 100% recovery ----
	cb2, tok2 := startConfigActorCallbackServer(t, &activeClient)
	defer cb2.stop(t)

	mgr2, err := daprhost.NewManager(daprhost.Options{
		StateDir: stateDir,
		AppID:    "sofa-config-actor-it",
		AppPort:  cb2.port,
	})
	require.NoError(t, err)
	tok2.Store(mgr2.AppAPIToken())

	client2, err := mgr2.Start(ctx)
	require.NoError(t, err)
	activeClient.Store(&client2)
	defer func() {
		_ = mgr2.Stop(context.Background())
	}()

	stub2 := configactor.NewClientStub(client2)

	// Verify all three tiers survived restart
	pf2, err := stub2.PreflightCheck(ctx, &configactor.PreflightCheckReq{
		Namespace:    "proj-a",
		RunID:        "run-it-1",
		RequiredKeys: []string{"GLOBAL_URL", "SHARED_TIER", "PRIMARY_TOKEN", "PROJ_A_SECRET"},
	})
	require.NoError(t, err)
	assert.True(t, pf2.OK)
	assert.Empty(t, pf2.MissingKeys)

	res2, err := stub2.ResolveForNode(ctx, &configactor.ResolveForNodeReq{
		Namespace: "proj-a",
		RunID:     "run-it-1",
		Keys:      []string{"GLOBAL_URL", "SHARED_TIER", "PRIMARY_TOKEN", "PROJ_A_SECRET"},
	})
	require.NoError(t, err)
	assert.Equal(t, map[string]string{
		"GLOBAL_URL":    "https://global.example.com",
		"SHARED_TIER":   "from-run-it-1",
		"PRIMARY_TOKEN": ultraSecretToken,
		"PROJ_A_SECRET": "proj-a-secret-123",
	}, res2.Values)

	// Verify default namespace survived restart and proj-b remains isolated
	resDefault, err := stub2.ResolveForNode(ctx, &configactor.ResolveForNodeReq{
		Namespace: "",
		Keys:      []string{"DEFAULT_VAR", "SHARED_TIER", "PROJ_A_SECRET"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"PROJ_A_SECRET"}, resDefault.MissingKeys)
	assert.Equal(t, map[string]string{
		"DEFAULT_VAR": "from-default-ns",
		"SHARED_TIER": "from-global",
	}, resDefault.Values)

	// Verify ephemeral run scope remained deleted across restart
	resDeletedRun, err := stub2.ResolveForNode(ctx, &configactor.ResolveForNodeReq{
		RunID: "run-it-ephemeral",
		Keys:  []string{"TEMP_SECRET"},
	})
	require.NoError(t, err)
	assert.Equal(t, []string{"TEMP_SECRET"}, resDeletedRun.MissingKeys)
	assert.Empty(t, resDeletedRun.Values)

	assert.Zero(t, cb2.badTokens.Load())
	assert.Positive(t, cb2.validTokens.Load())

	require.NoError(t, mgr2.Stop(ctx))
}
