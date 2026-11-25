package provider

import (
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestProviderConfigValidate(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		want string
	}{
		{
			name: "valid openai",
			cfg:  Config{Type: TypeOpenAI, Endpoint: "https://api.example.com/v1"},
		},
		{
			name: "invalid type",
			cfg:  Config{Type: "other", Endpoint: "https://api.example.com"},
			want: "type",
		},
		{
			name: "completions endpoint",
			cfg:  Config{Type: TypeOpenAI, Endpoint: "https://api.example.com/v1/chat/completions"},
			want: "API root",
		},
		{
			name: "anthropic wire api",
			cfg: Config{
				Type: TypeAnthropic, Endpoint: "https://api.example.com", WireAPI: wire(WireAPIResponses),
			},
			want: "anthropic",
		},
		{
			name: "websockets completions",
			cfg: Config{
				Type: TypeOpenAI, Endpoint: "https://api.example.com", Transport: transport(TransportWebSockets),
			},
			want: "responses",
		},
		{
			name: "multiple auth fields",
			cfg: Config{
				Type: TypeOpenAI, Endpoint: "https://api.example.com",
				APIKey: ptr("key"), APIKeyRef: ptr("TOKEN"),
			},
			want: "at most one",
		},
		{
			name: "unknown headers",
			cfg: Config{
				Type: TypeOpenAI, Endpoint: "https://api.example.com",
				Headers: cty.UnknownVal(cty.Map(cty.String)),
			},
			want: "wholly known",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := tt.cfg.Validate()
			if tt.want == "" {
				require.NoError(t, err)
				return
			}
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestProviderHeadersPreserveSensitivity(t *testing.T) {
	headers := cty.MapVal(map[string]cty.Value{
		"Authorization": cty.StringVal("secret"),
	}).Mark("sensitive")
	cfg := Config{Type: TypeOpenAI, Endpoint: "https://api.example.com", Headers: headers}
	require.NoError(t, cfg.Validate())
	require.True(t, HeadersSensitive(headers))
}

func ptr[T any](value T) *T                { return &value }
func wire(value WireAPI) *WireAPI          { return &value }
func transport(value Transport) *Transport { return &value }
