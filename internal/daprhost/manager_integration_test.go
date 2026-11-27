//go:build integration

package daprhost_test

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
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/dapr/go-sdk/actor"
	"github.com/dapr/go-sdk/actor/runtime"
	daprclient "github.com/dapr/go-sdk/client"
	"github.com/dapr/go-sdk/service/common"
	daprhttp "github.com/dapr/go-sdk/service/http"
	"github.com/go-chi/chi/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/lonegunmanb/sofa/internal/daprhost"
)

const (
	integrationActorType = "sofaDaprHostCanaryActor"
	canaryPlaintext      = "SUPER_SECRET_CANARY_VALUE_9918"
)

type CanaryActor struct {
	actor.ServerImplBaseCtx
	clientRef  *atomic.Pointer[daprclient.Client]
	onReminder func(name string, data []byte)
}

func (a *CanaryActor) Type() string {
	return integrationActorType
}

// SetStateManager replaces the SDK's default global client with the active Manager's
// token-authenticated loopback Dapr client so multi-instance and dynamic ports work
// without mutating process-global environment variables.
func (a *CanaryActor) SetStateManager(_ actor.StateManagerContext) {
	c := a.clientRef.Load()
	if c == nil || *c == nil {
		return
	}
	if sm := daprhost.NewActorStateManager(a.Type(), a.ID(), *c); sm != nil {
		a.ServerImplBaseCtx.SetStateManager(sm)
	}
}

func (a *CanaryActor) WriteSecret(ctx context.Context, value string) (string, error) {
	sm := a.GetStateManager()
	if sm == nil {
		return "", errors.New("state manager not initialized")
	}
	if err := sm.Set(ctx, "canary_secret", value); err != nil {
		return "", err
	}
	if err := sm.Save(ctx); err != nil {
		return "", err
	}
	return "ok", nil
}

func (a *CanaryActor) ReadSecret(ctx context.Context) (string, error) {
	sm := a.GetStateManager()
	if sm == nil {
		return "", errors.New("state manager not initialized")
	}
	var out string
	if err := sm.Get(ctx, "canary_secret", &out); err != nil {
		return "", err
	}
	return out, nil
}

func (a *CanaryActor) ReminderCall(reminderName string, state []byte, _ string, _ string) {
	if a.onReminder != nil {
		a.onReminder(reminderName, state)
	}
}

type callbackServer struct {
	port        int
	svc         common.Service
	errCh       chan error
	validTokens atomic.Int64
	badTokens   atomic.Int64
}

func startTokenEnforcedCallbackServer(
	t *testing.T,
	registerActors bool,
	clientRef *atomic.Pointer[daprclient.Client],
	onReminder func(name string, data []byte),
) (*callbackServer, *atomic.Value) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	port := ln.Addr().(*net.TCPAddr).Port
	require.NoError(t, ln.Close())

	var expectedToken atomic.Value
	expectedToken.Store("")

	router := chi.NewRouter()
	srv := &callbackServer{
		port:  port,
		errCh: make(chan error, 1),
	}

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
	if registerActors {
		svc.RegisterActorImplFactoryContext(func() actor.ServerContext {
			return &CanaryActor{
				clientRef:  clientRef,
				onReminder: onReminder,
			}
		})
	}
	srv.svc = svc

	go func() {
		err := svc.Start()
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			srv.errCh <- err
		}
		close(srv.errCh)
	}()

	// Wait until the loopback HTTP callback port is accepting TCP connections.
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

func (s *callbackServer) stop(t *testing.T) {
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

func TestManager_EncryptedActorStateAndRemindersAcrossRestart(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	stateDir := t.TempDir()
	actorID := "canary-actor-1"
	reminderName := "heartbeat-reminder"

	var activeClient atomic.Pointer[daprclient.Client]

	var reminderMu sync.Mutex
	phase1Reminders := 0
	phase2Reminders := 0
	currentPhase := 1

	onReminder := func(name string, _ []byte) {
		if name != reminderName {
			return
		}
		reminderMu.Lock()
		defer reminderMu.Unlock()
		if currentPhase == 1 {
			phase1Reminders++
		} else {
			phase2Reminders++
		}
	}

	// Capture parent environment before Start to ensure zero global mutation.
	_, hadAppEnv := os.LookupEnv("APP_API_TOKEN")
	_, hadDaprEnv := os.LookupEnv("DAPR_API_TOKEN")

	// ---- Phase 1: Start callback server + Manager, write canary secret, register reminder ----
	cb1, tok1 := startTokenEnforcedCallbackServer(t, true, &activeClient, onReminder)
	defer cb1.stop(t)

	mgr1, err := daprhost.NewManager(daprhost.Options{
		StateDir: stateDir,
		AppID:    "sofa-daprhost-it",
		AppPort:  cb1.port,
	})
	require.NoError(t, err)
	tok1.Store(mgr1.AppAPIToken())

	require.Nil(t, mgr1.Client(), "Client() must be nil before Start")
	client1, err := mgr1.Start(ctx)
	require.NoError(t, err)
	assert.Equal(t, client1, mgr1.Client(), "Client() must return the active client after Start")
	activeClient.Store(&client1)
	defer func() {
		_ = mgr1.Stop(context.Background())
	}()

	_, hasAppAfter := os.LookupEnv("APP_API_TOKEN")
	_, hasDaprAfter := os.LookupEnv("DAPR_API_TOKEN")
	assert.Equal(t, hadAppEnv, hasAppAfter, "parent APP_API_TOKEN must not be mutated")
	assert.Equal(t, hadDaprEnv, hasDaprAfter, "parent DAPR_API_TOKEN must not be mutated")

	// Verify an unauthenticated gRPC client without DAPR_API_TOKEN is rejected by daprd.
	unauthClient, err := daprclient.NewClientWithAddressContext(ctx, mgr1.GRPCAddress())
	require.NoError(t, err)
	defer unauthClient.Close()
	_, unauthErr := unauthClient.GetMetadata(ctx)
	require.Error(t, unauthErr)
	assert.Equal(t, codes.Unauthenticated, status.Code(unauthErr))

	// Write the canary secret through the actor using the authenticated client.
	writePayload, err := json.Marshal(canaryPlaintext)
	require.NoError(t, err)

	resp, err := client1.InvokeActor(ctx, &daprclient.InvokeActorRequest{
		ActorType: integrationActorType,
		ActorID:   actorID,
		Method:    "WriteSecret",
		Data:      writePayload,
	})
	require.NoError(t, err)
	var writeResult string
	require.NoError(t, json.Unmarshal(resp.Data, &writeResult))
	assert.Equal(t, "ok", writeResult)

	// Verify callbacks enforced AppAPIToken properly.
	assert.Zero(t, cb1.badTokens.Load(), "daprd must send matching dapr-api-token on all callbacks")
	assert.Positive(t, cb1.validTokens.Load(), "expected authenticated callbacks from daprd")

	// Register a short-period persistent Actor Reminder.
	err = client1.RegisterActorReminder(ctx, &daprclient.RegisterActorReminderRequest{
		ActorType: integrationActorType,
		ActorID:   actorID,
		Name:      reminderName,
		DueTime:   "100ms",
		Period:    "200ms",
		Data:      []byte("tick"),
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		reminderMu.Lock()
		defer reminderMu.Unlock()
		return phase1Reminders >= 1
	}, 5*time.Second, 25*time.Millisecond, "expected phase 1 actor reminder to fire")

	// Deactivate actor from the in-process SDK runtime cache so Phase 2 must reload from SQLite.
	_ = runtime.GetActorRuntimeInstanceContext().Deactivate(ctx, integrationActorType, actorID)

	// Stop Manager 1 cleanly, flushing SQLite and releasing etcd locks.
	require.NoError(t, mgr1.Stop(ctx))
	assert.Nil(t, mgr1.Client(), "Client() must be nil after Stop")
	activeClient.Store(nil)
	cb1.stop(t)

	// Scan raw SQLite database (and WAL if present) to prove client-side primaryEncryptionKey encrypted the value.
	dbPath := filepath.Join(stateDir, "actor-state.db")
	dbBytes, err := os.ReadFile(dbPath)
	require.NoError(t, err)
	assert.NotEmpty(t, dbBytes)
	assert.False(t, bytes.Contains(dbBytes, []byte(canaryPlaintext)),
		"actor-state.db must NOT contain plaintext canary %q", canaryPlaintext)

	if walBytes, walErr := os.ReadFile(dbPath + "-wal"); walErr == nil {
		assert.False(t, bytes.Contains(walBytes, []byte(canaryPlaintext)),
			"actor-state.db-wal must NOT contain plaintext canary %q", canaryPlaintext)
	}

	// ---- Phase 2: Restart a fresh Manager on the SAME StateDir ----
	reminderMu.Lock()
	currentPhase = 2
	reminderMu.Unlock()

	cb2, tok2 := startTokenEnforcedCallbackServer(t, false, &activeClient, onReminder)
	defer cb2.stop(t)

	mgr2, err := daprhost.NewManager(daprhost.Options{
		StateDir: stateDir,
		AppID:    "sofa-daprhost-it",
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

	// Read back the secret through a freshly activated actor instance: must decrypt identically.
	readResp, err := client2.InvokeActor(ctx, &daprclient.InvokeActorRequest{
		ActorType: integrationActorType,
		ActorID:   actorID,
		Method:    "ReadSecret",
	})
	require.NoError(t, err)
	var recovered string
	require.NoError(t, json.Unmarshal(readResp.Data, &recovered))
	assert.Equal(t, canaryPlaintext, recovered)

	// Persistent Reminder registered in Phase 1 must resume firing automatically after restart.
	require.Eventually(t, func() bool {
		reminderMu.Lock()
		defer reminderMu.Unlock()
		return phase2Reminders >= 1
	}, 10*time.Second, 25*time.Millisecond, "expected persistent reminder to resume after restart")

	assert.Zero(t, cb2.badTokens.Load(), "phase 2 daprd must send matching dapr-api-token")
	assert.Positive(t, cb2.validTokens.Load())

	require.NoError(t, client2.UnregisterActorReminder(ctx, &daprclient.UnregisterActorReminderRequest{
		ActorType: integrationActorType,
		ActorID:   actorID,
		Name:      reminderName,
	}))
	require.NoError(t, mgr2.Stop(ctx))
}
