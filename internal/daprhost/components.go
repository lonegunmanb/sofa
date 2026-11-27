package daprhost

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

const (
	// SecretStoreComponentName is the Dapr secret store component name for the local master key file.
	SecretStoreComponentName = "sofa-secrets"
	// StateStoreComponentName is the Dapr actor/state store component name backed by SQLite.
	StateStoreComponentName = "sofa-statestore"
	// PubSubComponentName is the Dapr in-memory pubsub component name.
	PubSubComponentName = "sofa-pubsub"

	masterKeyJSONField = "masterEncryptionKey"
)

type componentManifest struct {
	APIVersion string             `yaml:"apiVersion"`
	Kind       string             `yaml:"kind"`
	Metadata   componentMeta      `yaml:"metadata"`
	Spec       componentSpec      `yaml:"spec"`
	Auth       *componentAuthSpec `yaml:"auth,omitempty"`
}

type componentMeta struct {
	Name string `yaml:"name"`
}

type componentSpec struct {
	Type     string              `yaml:"type"`
	Version  string              `yaml:"version"`
	Metadata []componentMetaItem `yaml:"metadata,omitempty"`
}

type componentMetaItem struct {
	Name         string        `yaml:"name"`
	Value        string        `yaml:"value,omitempty"`
	SecretKeyRef *secretKeyRef `yaml:"secretKeyRef,omitempty"`
}

type secretKeyRef struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key,omitempty"`
}

type componentAuthSpec struct {
	SecretStore string `yaml:"secretStore"`
}

// EnsureMasterKey ensures <stateDir>/keys/master-key.json exists with a valid
// 32-byte (64 lowercase hex character) AES-256 encryption key and restricted
// current-user file permissions. Existing valid keys are preserved untouched so
// encrypted SQLite state remains decryptable across restarts.
func EnsureMasterKey(stateDir string) (string, error) {
	absStateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}

	keysDir := filepath.Join(absStateDir, "keys")
	if err := os.MkdirAll(keysDir, 0o700); err != nil {
		return "", fmt.Errorf("create keys directory: %w", err)
	}

	keyPath := filepath.Join(keysDir, "master-key.json")
	info, statErr := os.Stat(keyPath)
	if statErr == nil {
		if info.IsDir() {
			return "", fmt.Errorf("master key path %s is a directory", keyPath)
		}
		raw, err := os.ReadFile(keyPath)
		if err != nil {
			return "", fmt.Errorf("read existing master key: %w", err)
		}
		var doc map[string]string
		if err := json.Unmarshal(raw, &doc); err != nil {
			return "", fmt.Errorf("parse existing master key %s: %w", keyPath, err)
		}
		if !isLowerHex64(doc[masterKeyJSONField]) {
			return "", fmt.Errorf("invalid %s in existing master key %s", masterKeyJSONField, keyPath)
		}
		if err := restrictKeyFile(keyPath, nil); err != nil {
			return "", fmt.Errorf("restrict existing master key permissions: %w", err)
		}
		return keyPath, nil
	}
	if !errors.Is(statErr, os.ErrNotExist) {
		return "", fmt.Errorf("stat master key %s: %w", keyPath, statErr)
	}

	keyHex, err := randomHex32()
	if err != nil {
		return "", fmt.Errorf("generate master key: %w", err)
	}
	payload, err := json.MarshalIndent(map[string]string{
		masterKeyJSONField: keyHex,
	}, "", "  ")
	if err != nil {
		return "", fmt.Errorf("marshal master key json: %w", err)
	}
	payload = append(payload, '\n')

	if err := writeRestrictedFileAtomic(keysDir, keyPath, payload, true); err != nil {
		return "", err
	}
	return keyPath, nil
}

// WriteComponents renders local-secrets.yaml, statestore.yaml, and pubsub.yaml
// into <stateDir>/components and returns that directory path.
func WriteComponents(stateDir, keyFilePath string) (string, error) {
	absStateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	absKeyFile, err := filepath.Abs(keyFilePath)
	if err != nil {
		return "", fmt.Errorf("resolve master key path: %w", err)
	}

	componentsDir := filepath.Join(absStateDir, "components")
	if err := os.MkdirAll(componentsDir, 0o700); err != nil {
		return "", fmt.Errorf("create components directory: %w", err)
	}

	dbURI := "file:" + filepath.ToSlash(filepath.Join(absStateDir, "actor-state.db"))

	manifests := []struct {
		fileName string
		doc      componentManifest
	}{
		{
			fileName: "local-secrets.yaml",
			doc: componentManifest{
				APIVersion: "dapr.io/v1alpha1",
				Kind:       "Component",
				Metadata:   componentMeta{Name: SecretStoreComponentName},
				Spec: componentSpec{
					Type:    "secretstores.local.file",
					Version: "v1",
					Metadata: []componentMetaItem{
						{Name: "secretsFile", Value: absKeyFile},
					},
				},
			},
		},
		{
			fileName: "statestore.yaml",
			doc: componentManifest{
				APIVersion: "dapr.io/v1alpha1",
				Kind:       "Component",
				Metadata:   componentMeta{Name: StateStoreComponentName},
				Spec: componentSpec{
					Type:    "state.sqlite",
					Version: "v1",
					Metadata: []componentMetaItem{
						{Name: "connectionString", Value: dbURI},
						{Name: "actorStateStore", Value: "true"},
						{
							Name: "primaryEncryptionKey",
							SecretKeyRef: &secretKeyRef{
								Name: masterKeyJSONField,
								Key:  masterKeyJSONField,
							},
						},
					},
				},
				Auth: &componentAuthSpec{
					SecretStore: SecretStoreComponentName,
				},
			},
		},
		{
			fileName: "pubsub.yaml",
			doc: componentManifest{
				APIVersion: "dapr.io/v1alpha1",
				Kind:       "Component",
				Metadata:   componentMeta{Name: PubSubComponentName},
				Spec: componentSpec{
					Type:    "pubsub.in-memory",
					Version: "v1",
				},
			},
		},
	}

	for _, m := range manifests {
		encoded, err := yaml.Marshal(&m.doc)
		if err != nil {
			return "", fmt.Errorf("marshal %s: %w", m.fileName, err)
		}
		targetPath := filepath.Join(componentsDir, m.fileName)
		if err := writeRestrictedFileAtomic(componentsDir, targetPath, encoded, false); err != nil {
			return "", fmt.Errorf("write %s: %w", m.fileName, err)
		}
	}

	return componentsDir, nil
}

type daprConfigManifest struct {
	APIVersion string         `yaml:"apiVersion"`
	Kind       string         `yaml:"kind"`
	Metadata   componentMeta  `yaml:"metadata"`
	Spec       daprConfigSpec `yaml:"spec"`
}

type daprConfigSpec struct {
	NameResolution *nameResolutionConfig `yaml:"nameResolution,omitempty"`
}

type nameResolutionConfig struct {
	Component     string            `yaml:"component"`
	Version       string            `yaml:"version"`
	Configuration map[string]string `yaml:"configuration,omitempty"`
}

// WriteNameResolutionConfig renders <stateDir>/dapr-config.yaml configuring Dapr's
// SQLite name resolution component against dbPath so multiple local daprd instances
// resolve each other reliably without multicast mDNS.
func WriteNameResolutionConfig(stateDir, dbPath string) (string, error) {
	if strings.TrimSpace(stateDir) == "" {
		return "", errors.New("daprhost: stateDir is required")
	}
	if strings.TrimSpace(dbPath) == "" {
		return "", errors.New("daprhost: NameResolutionDBPath is required")
	}
	absStateDir, err := filepath.Abs(stateDir)
	if err != nil {
		return "", fmt.Errorf("resolve state dir: %w", err)
	}
	absDBPath, err := filepath.Abs(dbPath)
	if err != nil {
		return "", fmt.Errorf("resolve name resolution db path: %w", err)
	}
	if err := os.MkdirAll(absStateDir, 0o700); err != nil {
		return "", fmt.Errorf("create state directory: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(absDBPath), 0o700); err != nil {
		return "", fmt.Errorf("create name resolution db directory: %w", err)
	}

	doc := daprConfigManifest{
		APIVersion: "dapr.io/v1alpha1",
		Kind:       "Configuration",
		Metadata:   componentMeta{Name: "sofa-dapr-config"},
		Spec: daprConfigSpec{
			NameResolution: &nameResolutionConfig{
				Component: "sqlite",
				Version:   "v1",
				Configuration: map[string]string{
					"connectionString": filepath.ToSlash(absDBPath),
				},
			},
		},
	}
	encoded, err := yaml.Marshal(&doc)
	if err != nil {
		return "", fmt.Errorf("marshal dapr-config.yaml: %w", err)
	}
	targetPath := filepath.Join(absStateDir, "dapr-config.yaml")
	if err := writeRestrictedFileAtomic(absStateDir, targetPath, encoded, false); err != nil {
		return "", fmt.Errorf("write dapr-config.yaml: %w", err)
	}
	return targetPath, nil
}

func writeRestrictedFileAtomic(dir, targetPath string, data []byte, applyDACL bool) error {
	tmpFile, err := os.CreateTemp(dir, ".sofa-*.tmp")
	if err != nil {
		return fmt.Errorf("create temp file for %s: %w", targetPath, err)
	}
	tmpPath := tmpFile.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(tmpPath)
		}
	}()

	if applyDACL {
		if err := restrictKeyFile(tmpPath, tmpFile); err != nil {
			_ = tmpFile.Close()
			return fmt.Errorf("restrict temp key file: %w", err)
		}
	}
	if _, err := tmpFile.Write(data); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("write temp file %s: %w", tmpPath, err)
	}
	if err := tmpFile.Sync(); err != nil {
		_ = tmpFile.Close()
		return fmt.Errorf("sync temp file %s: %w", tmpPath, err)
	}
	if err := tmpFile.Close(); err != nil {
		return fmt.Errorf("close temp file %s: %w", tmpPath, err)
	}
	if err := os.Rename(tmpPath, targetPath); err != nil {
		return fmt.Errorf("rename %s to %s: %w", tmpPath, targetPath, err)
	}
	cleanup = false
	if applyDACL {
		if err := restrictKeyFile(targetPath, nil); err != nil {
			return fmt.Errorf("restrict %s permissions: %w", targetPath, err)
		}
	}
	return nil
}

func randomHex32() (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf), nil
}

func isLowerHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}
