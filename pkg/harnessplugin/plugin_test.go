package harnessplugin

import (
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestScrubPluginEnv(t *testing.T) {
	t.Setenv("SOFA_HARNESS_PLUGIN", "sofa-harness-v1")
	t.Setenv("PLUGIN_CLIENT_CERT", "pem-data")
	t.Setenv("PLUGIN_UNIX_SOCKET_DIR", "/.sofa/plugin")
	t.Setenv("PLUGIN_MIN_PORT", "19500")
	t.Setenv("PLUGIN_MAX_PORT", "19500")
	t.Setenv("KEEP_ME", "stay")

	ScrubPluginEnv()

	_, hasMagic := os.LookupEnv("SOFA_HARNESS_PLUGIN")
	_, hasCert := os.LookupEnv("PLUGIN_CLIENT_CERT")
	_, hasUDS := os.LookupEnv("PLUGIN_UNIX_SOCKET_DIR")
	_, hasMin := os.LookupEnv("PLUGIN_MIN_PORT")
	_, hasMax := os.LookupEnv("PLUGIN_MAX_PORT")
	keepVal, hasKeep := os.LookupEnv("KEEP_ME")

	assert.False(t, hasMagic)
	assert.False(t, hasCert)
	assert.False(t, hasUDS)
	assert.False(t, hasMin)
	assert.False(t, hasMax)
	assert.True(t, hasKeep)
	assert.Equal(t, "stay", keepVal)
}

func TestContainerTCPPortFromEnv(t *testing.T) {
	t.Setenv("PLUGIN_UNIX_SOCKET_DIR", "")
	t.Setenv("PLUGIN_MIN_PORT", "19500")
	t.Setenv("PLUGIN_MAX_PORT", "19500")
	port, ok := containerTCPPortFromEnv()
	assert.True(t, ok)
	assert.Equal(t, 19500, port)

	// Local host mode (range 10000..25000) must not activate pinned container TCP mode.
	t.Setenv("PLUGIN_MIN_PORT", "10000")
	t.Setenv("PLUGIN_MAX_PORT", "25000")
	_, ok = containerTCPPortFromEnv()
	assert.False(t, ok)

	// Linux UDS mode must not activate container TCP mode even if min == max.
	t.Setenv("PLUGIN_UNIX_SOCKET_DIR", "/.sofa/plugin")
	t.Setenv("PLUGIN_MIN_PORT", "19500")
	t.Setenv("PLUGIN_MAX_PORT", "19500")
	_, ok = containerTCPPortFromEnv()
	assert.False(t, ok)
}

func TestPrepareContainerTCPConfigRequiresClientCert(t *testing.T) {
	t.Setenv("SOFA_HARNESS_PLUGIN", "sofa-harness-v1")
	t.Setenv("PLUGIN_CLIENT_CERT", "")
	t.Setenv("PLUGIN_PROTOCOL_VERSIONS", "1")

	_, _, _, err := prepareContainerTCPConfig()
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "PLUGIN_CLIENT_CERT")

	// Env must still be scrubbed even on failure.
	_, hasMagic := os.LookupEnv("SOFA_HARNESS_PLUGIN")
	_, hasCert := os.LookupEnv("PLUGIN_CLIENT_CERT")
	assert.False(t, hasMagic)
	assert.False(t, hasCert)
}
