package daprhost

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"testing"

	"github.com/dapr/go-sdk/client"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gopkg.in/yaml.v3"
)

type testComponentManifest struct {
	APIVersion string `yaml:"apiVersion"`
	Kind       string `yaml:"kind"`
	Metadata   struct {
		Name string `yaml:"name"`
	} `yaml:"metadata"`
	Spec struct {
		Type     string `yaml:"type"`
		Version  string `yaml:"version"`
		Metadata []struct {
			Name         string `yaml:"name"`
			Value        string `yaml:"value,omitempty"`
			SecretKeyRef *struct {
				Name string `yaml:"name"`
				Key  string `yaml:"key,omitempty"`
			} `yaml:"secretKeyRef,omitempty"`
		} `yaml:"metadata,omitempty"`
	} `yaml:"spec"`
	Auth *struct {
		SecretStore string `yaml:"secretStore"`
	} `yaml:"auth,omitempty"`
}

func TestEnsureMasterKey_CreatesAndPreservesIdempotently(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	keyPath, err := EnsureMasterKey(stateDir)
	require.NoError(t, err)

	expectedPath := filepath.Join(stateDir, "keys", "master-key.json")
	assert.Equal(t, expectedPath, keyPath)
	assertRestrictedKeyPermissions(t, keyPath)

	raw1, err := os.ReadFile(keyPath)
	require.NoError(t, err)

	var doc map[string]string
	require.NoError(t, json.Unmarshal(raw1, &doc))
	keyHex := doc[masterKeyJSONField]
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), keyHex)

	decoded, err := hex.DecodeString(keyHex)
	require.NoError(t, err)
	assert.Len(t, decoded, 32)

	// Calling EnsureMasterKey again MUST preserve the exact existing key untouched.
	keyPath2, err := EnsureMasterKey(stateDir)
	require.NoError(t, err)
	assert.Equal(t, keyPath, keyPath2)

	raw2, err := os.ReadFile(keyPath)
	require.NoError(t, err)
	assert.Equal(t, raw1, raw2)
}

func TestEnsureMasterKey_RejectsCorruptExistingKeyWithoutOverwriting(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name    string
		content string
	}{
		{name: "invalid json", content: "{not-json"},
		{name: "missing field", content: `{"other":"value"}`},
		{name: "short hex", content: `{"masterEncryptionKey":"abcd"}`},
		{name: "non-hex", content: `{"masterEncryptionKey":"zzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzzz"}`},
		{name: "uppercase hex", content: `{"masterEncryptionKey":"AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA"}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			stateDir := t.TempDir()
			keysDir := filepath.Join(stateDir, "keys")
			require.NoError(t, os.MkdirAll(keysDir, 0o700))
			keyPath := filepath.Join(keysDir, "master-key.json")
			require.NoError(t, os.WriteFile(keyPath, []byte(tc.content), 0o600))

			_, err := EnsureMasterKey(stateDir)
			require.Error(t, err)

			after, readErr := os.ReadFile(keyPath)
			require.NoError(t, readErr)
			assert.Equal(t, tc.content, string(after), "corrupt key file must not be overwritten")
		})
	}
}

func TestWriteComponents_RendersValidDaprManifests(t *testing.T) {
	t.Parallel()

	base := t.TempDir()
	// Include spaces to ensure YAML escaping and SQLite URI formatting stay valid on Windows & Unix.
	stateDir := filepath.Join(base, "sofa state dir")
	keyPath, err := EnsureMasterKey(stateDir)
	require.NoError(t, err)

	componentsDir, err := WriteComponents(stateDir, keyPath)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(stateDir, "components"), componentsDir)

	// 1. local-secrets.yaml
	var secretsComp testComponentManifest
	secretsRaw, err := os.ReadFile(filepath.Join(componentsDir, "local-secrets.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(secretsRaw, &secretsComp))
	assert.Equal(t, "dapr.io/v1alpha1", secretsComp.APIVersion)
	assert.Equal(t, "Component", secretsComp.Kind)
	assert.Equal(t, SecretStoreComponentName, secretsComp.Metadata.Name)
	assert.Equal(t, "secretstores.local.file", secretsComp.Spec.Type)
	assert.Equal(t, "v1", secretsComp.Spec.Version)
	require.Len(t, secretsComp.Spec.Metadata, 1)
	assert.Equal(t, "secretsFile", secretsComp.Spec.Metadata[0].Name)
	assert.Equal(t, keyPath, secretsComp.Spec.Metadata[0].Value)

	// 2. statestore.yaml
	var stateComp testComponentManifest
	stateRaw, err := os.ReadFile(filepath.Join(componentsDir, "statestore.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(stateRaw, &stateComp))
	assert.Equal(t, "dapr.io/v1alpha1", stateComp.APIVersion)
	assert.Equal(t, "Component", stateComp.Kind)
	assert.Equal(t, StateStoreComponentName, stateComp.Metadata.Name)
	assert.Equal(t, "state.sqlite", stateComp.Spec.Type)
	assert.Equal(t, "v1", stateComp.Spec.Version)
	require.NotNil(t, stateComp.Auth)
	assert.Equal(t, SecretStoreComponentName, stateComp.Auth.SecretStore)

	expectedConn := "file:" + filepath.ToSlash(filepath.Join(stateDir, "actor-state.db"))
	metaByName := make(map[string]int, len(stateComp.Spec.Metadata))
	for i, item := range stateComp.Spec.Metadata {
		metaByName[item.Name] = i
	}
	connIdx, ok := metaByName["connectionString"]
	require.True(t, ok, "statestore.yaml must define connectionString")
	assert.Equal(t, expectedConn, stateComp.Spec.Metadata[connIdx].Value)

	actorIdx, ok := metaByName["actorStateStore"]
	require.True(t, ok, "statestore.yaml must define actorStateStore")
	assert.Equal(t, "true", stateComp.Spec.Metadata[actorIdx].Value)

	encIdx, ok := metaByName["primaryEncryptionKey"]
	require.True(t, ok, "statestore.yaml must define primaryEncryptionKey")
	require.NotNil(t, stateComp.Spec.Metadata[encIdx].SecretKeyRef)
	assert.Equal(t, masterKeyJSONField, stateComp.Spec.Metadata[encIdx].SecretKeyRef.Name)
	assert.Equal(t, masterKeyJSONField, stateComp.Spec.Metadata[encIdx].SecretKeyRef.Key)

	// 3. pubsub.yaml
	var pubsubComp testComponentManifest
	pubsubRaw, err := os.ReadFile(filepath.Join(componentsDir, "pubsub.yaml"))
	require.NoError(t, err)
	require.NoError(t, yaml.Unmarshal(pubsubRaw, &pubsubComp))
	assert.Equal(t, "dapr.io/v1alpha1", pubsubComp.APIVersion)
	assert.Equal(t, "Component", pubsubComp.Kind)
	assert.Equal(t, PubSubComponentName, pubsubComp.Metadata.Name)
	assert.Equal(t, "sofa-pubsub", PubSubComponentName)
	assert.Equal(t, "pubsub.in-memory", pubsubComp.Spec.Type)
	assert.Equal(t, "v1", pubsubComp.Spec.Version)
}

func TestResolveBinary_Precedence(t *testing.T) {
	t.Parallel()

	binName := func(base string) string {
		if runtime.GOOS == "windows" {
			return base + ".exe"
		}
		return base
	}

	homeDir := t.TempDir()
	daprBinDir := filepath.Join(homeDir, ".dapr", "bin")
	require.NoError(t, os.MkdirAll(daprBinDir, 0o700))

	homeBin := filepath.Join(daprBinDir, binName("daprd"))
	require.NoError(t, os.WriteFile(homeBin, []byte("home"), 0o700))

	explicitDir := t.TempDir()
	explicitBin := filepath.Join(explicitDir, binName("daprd"))
	require.NoError(t, os.WriteFile(explicitBin, []byte("explicit"), 0o700))

	pathDir := t.TempDir()
	pathBin := filepath.Join(pathDir, binName("daprd"))
	require.NoError(t, os.WriteFile(pathBin, []byte("path"), 0o700))

	lookPathOK := func(string) (string, error) { return pathBin, nil }
	lookPathErr := func(string) (string, error) { return "", errors.New("not in PATH") }

	// 1. Explicit override wins over ~/.dapr/bin and PATH.
	got, err := resolveBinary(explicitBin, "daprd", homeDir, lookPathOK)
	require.NoError(t, err)
	assert.Equal(t, explicitBin, got)

	// 2. Non-existent explicit override must fail immediately without falling back.
	_, err = resolveBinary(filepath.Join(explicitDir, "missing"), "daprd", homeDir, lookPathOK)
	require.Error(t, err)

	// 3. Directory passed as explicit override must fail.
	_, err = resolveBinary(explicitDir, "daprd", homeDir, lookPathOK)
	require.Error(t, err)

	// 4. ~/.dapr/bin wins over PATH.
	got, err = resolveBinary("", "daprd", homeDir, lookPathOK)
	require.NoError(t, err)
	assert.Equal(t, homeBin, got)

	// 5. Fallback to LookPath when ~/.dapr/bin does not contain binary.
	emptyHome := t.TempDir()
	got, err = resolveBinary("", "daprd", emptyHome, lookPathOK)
	require.NoError(t, err)
	assert.Equal(t, pathBin, got)

	// 6. Error when missing everywhere.
	_, err = resolveBinary("", "daprd", emptyHome, lookPathErr)
	require.Error(t, err)
}

func TestNewManager_ValidationAndTokens(t *testing.T) {
	t.Parallel()

	binDir := t.TempDir()
	fakeSched := filepath.Join(binDir, "scheduler")
	fakeDaprd := filepath.Join(binDir, "daprd")
	require.NoError(t, os.WriteFile(fakeSched, []byte("x"), 0o700))
	require.NoError(t, os.WriteFile(fakeDaprd, []byte("x"), 0o700))

	stateDir := t.TempDir()

	// Invalid options
	for _, tc := range []struct {
		name string
		opts Options
	}{
		{
			name: "empty state dir",
			opts: Options{StateDir: "   ", AppID: "sofa-dev", AppPort: 9000, SchedulerBin: fakeSched, DaprdBin: fakeDaprd},
		},
		{
			name: "empty app id",
			opts: Options{StateDir: stateDir, AppID: " ", AppPort: 9000, SchedulerBin: fakeSched, DaprdBin: fakeDaprd},
		},
		{
			name: "negative app port",
			opts: Options{StateDir: stateDir, AppID: "sofa-dev", AppPort: -1, SchedulerBin: fakeSched, DaprdBin: fakeDaprd},
		},
		{
			name: "app port above 65535",
			opts: Options{StateDir: stateDir, AppID: "sofa-dev", AppPort: 70000, SchedulerBin: fakeSched, DaprdBin: fakeDaprd},
		},
		{
			name: "missing scheduler bin",
			opts: Options{StateDir: stateDir, AppID: "sofa-dev", AppPort: 9000, SchedulerBin: filepath.Join(binDir, "missing"), DaprdBin: fakeDaprd},
		},
		{
			name: "missing daprd bin",
			opts: Options{StateDir: stateDir, AppID: "sofa-dev", AppPort: 9000, SchedulerBin: fakeSched, DaprdBin: filepath.Join(binDir, "missing")},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := NewManager(tc.opts)
			require.Error(t, err)
		})
	}

	// Valid options auto-generate 64-char hex tokens without mutating parent env.
	origAppToken, hadAppToken := os.LookupEnv("APP_API_TOKEN")
	origDaprToken, hadDaprToken := os.LookupEnv("DAPR_API_TOKEN")

	mgr, err := NewManager(Options{
		StateDir:     stateDir,
		AppID:        "sofa-dev",
		AppPort:      9100,
		SchedulerBin: fakeSched,
		DaprdBin:     fakeDaprd,
	})
	require.NoError(t, err)
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), mgr.AppAPIToken())
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), mgr.DaprAPIToken())
	assert.NotEqual(t, mgr.AppAPIToken(), mgr.DaprAPIToken())
	assert.Equal(t, "", mgr.GRPCAddress())
	assert.Nil(t, mgr.Client())

	assert.Nil(t, NewActorStateManager("testType", "id-1", nil))
	var stubClient client.Client = &client.GRPCClient{}
	assert.NotNil(t, NewActorStateManager("testType", "id-1", stubClient))

	afterAppToken, hasAfterApp := os.LookupEnv("APP_API_TOKEN")
	afterDaprToken, hasAfterDapr := os.LookupEnv("DAPR_API_TOKEN")
	assert.Equal(t, hadAppToken, hasAfterApp)
	assert.Equal(t, origAppToken, afterAppToken)
	assert.Equal(t, hadDaprToken, hasAfterDapr)
	assert.Equal(t, origDaprToken, afterDaprToken)

	// Explicit AppAPIToken is preserved.
	mgrCustom, err := NewManager(Options{
		StateDir:              stateDir,
		AppID:                 "sofa-worker-1",
		AppPort:               0,
		AppAPIToken:           "custom-app-token",
		DaprdBin:              fakeDaprd,
		ExternalSchedulerAddr: "127.0.0.1:50006",
	})
	require.NoError(t, err)
	assert.Equal(t, "custom-app-token", mgrCustom.AppAPIToken())
	assert.Regexp(t, regexp.MustCompile(`^[0-9a-f]{64}$`), mgrCustom.DaprAPIToken())
	assert.Equal(t, "127.0.0.1:50006", mgrCustom.SchedulerAddress())
}

func TestWriteNameResolutionConfig_AndManagerArgs(t *testing.T) {
	t.Parallel()

	stateDir := t.TempDir()
	nrDB := filepath.Join(t.TempDir(), "nr", "nameresolution.db")

	cfgPath, err := WriteNameResolutionConfig(stateDir, nrDB)
	require.NoError(t, err)
	assert.Equal(t, filepath.Join(stateDir, "dapr-config.yaml"), cfgPath)

	raw, err := os.ReadFile(cfgPath)
	require.NoError(t, err)

	var parsed struct {
		APIVersion string `yaml:"apiVersion"`
		Kind       string `yaml:"kind"`
		Metadata   struct {
			Name string `yaml:"name"`
		} `yaml:"metadata"`
		Spec struct {
			NameResolution struct {
				Component     string `yaml:"component"`
				Version       string `yaml:"version"`
				Configuration struct {
					ConnectionString string `yaml:"connectionString"`
				} `yaml:"configuration"`
			} `yaml:"nameResolution"`
		} `yaml:"spec"`
	}
	require.NoError(t, yaml.Unmarshal(raw, &parsed))
	assert.Equal(t, "dapr.io/v1alpha1", parsed.APIVersion)
	assert.Equal(t, "Configuration", parsed.Kind)
	assert.Equal(t, "sofa-dapr-config", parsed.Metadata.Name)
	assert.Equal(t, "sqlite", parsed.Spec.NameResolution.Component)
	assert.Equal(t, "v1", parsed.Spec.NameResolution.Version)
	assert.Equal(t, filepath.ToSlash(nrDB), parsed.Spec.NameResolution.Configuration.ConnectionString)

	_, err = WriteNameResolutionConfig(stateDir, "   ")
	require.Error(t, err)

	binDir := t.TempDir()
	fakeDaprd := filepath.Join(binDir, "daprd")
	require.NoError(t, os.WriteFile(fakeDaprd, []byte("x"), 0o700))
	mgr, err := NewManager(Options{
		StateDir:              stateDir,
		AppID:                 "sofa-worker-nr",
		AppPort:               8081,
		DaprdBin:              fakeDaprd,
		ExternalSchedulerAddr: "127.0.0.1:50006",
		NameResolutionDBPath:  nrDB,
	})
	require.NoError(t, err)
	argsWithConfig := mgr.buildDaprdArgs(1, 2, 3, 4, 5, "127.0.0.1:50006", filepath.Join(stateDir, "components"), cfgPath)
	assert.Contains(t, argsWithConfig, "--config="+cfgPath)

	argsWithoutConfig := mgr.buildDaprdArgs(1, 2, 3, 4, 5, "127.0.0.1:50006", filepath.Join(stateDir, "components"), "")
	for _, a := range argsWithoutConfig {
		assert.NotContains(t, a, "--config=")
	}
}
