package spec

import (
	"reflect"
	"testing"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/lonegunmanb/golden"
	"github.com/stretchr/testify/require"
	"github.com/zclconf/go-cty/cty"
)

func TestHarnessConfigValidateLocalModeZeroSpecialCase(t *testing.T) {
	for _, name := range []string{"copilot", "antigravity", "custom", "my-harness_1"} {
		t.Run(name, func(t *testing.T) {
			cfg := HarnessConfig{Name: name}
			require.NoError(t, cfg.Validate())
			require.Equal(t, SignatureModeAuto, cfg.Signature)
			require.Empty(t, cfg.SourceHost)
			require.Empty(t, cfg.SourceOwner)
			require.Empty(t, cfg.SourceRepo)
		})
	}
}

func TestHarnessConfigValidateGitHubReleasesMode(t *testing.T) {
	tests := []struct {
		name      string
		cfg       HarnessConfig
		wantHost  string
		wantOwner string
		wantRepo  string
		wantSig   SignatureMode
	}{
		{
			name: "github default auto",
			cfg: HarnessConfig{
				Name:    "antigravity",
				Source:  "github.com/lonegunmanb/sofa-harness-antigravity",
				Version: "0.1.0",
			},
			wantHost:  "github.com",
			wantOwner: "lonegunmanb",
			wantRepo:  "sofa-harness-antigravity",
			wantSig:   SignatureModeAuto,
		},
		{
			name: "ghes prerelease build metadata",
			cfg: HarnessConfig{
				Name:       "copilot",
				Source:     "ghe.example.com/org/sofa-harness-copilot",
				Version:    "1.2.3-rc.1+build.7",
				Signature:  SignatureModePGP,
				SigningKey: "-----BEGIN PGP PUBLIC KEY BLOCK-----",
			},
			wantHost:  "ghe.example.com",
			wantOwner: "org",
			wantRepo:  "sofa-harness-copilot",
			wantSig:   SignatureModePGP,
		},
		{
			name: "attestation mode",
			cfg: HarnessConfig{
				Name:      "copilot",
				Source:    "github.com/lonegunmanb/sofa-harness-copilot",
				Version:   "2.0.0",
				Signature: SignatureModeAttestation,
			},
			wantHost:  "github.com",
			wantOwner: "lonegunmanb",
			wantRepo:  "sofa-harness-copilot",
			wantSig:   SignatureModeAttestation,
		},
		{
			name: "none mode",
			cfg: HarnessConfig{
				Name:      "custom",
				Source:    "github.com/org/sofa-harness-custom",
				Version:   "0.0.1",
				Signature: SignatureModeNone,
			},
			wantHost:  "github.com",
			wantOwner: "org",
			wantRepo:  "sofa-harness-custom",
			wantSig:   SignatureModeNone,
		},
		{
			name: "env_refs and sensitive options",
			cfg: HarnessConfig{
				Name:    "antigravity",
				Source:  "github.com/org/sofa-harness-antigravity",
				Version: "1.0.0",
				EnvRefs: map[string]string{"GEMINI_API_KEY": "HOST_GEMINI_KEY"},
				Options: cty.ObjectVal(map[string]cty.Value{
					"location": cty.StringVal("us-central1"),
					"token":    cty.StringVal("secret").Mark(golden.SensitiveMark),
				}),
			},
			wantHost:  "github.com",
			wantOwner: "org",
			wantRepo:  "sofa-harness-antigravity",
			wantSig:   SignatureModeAuto,
		},
		{
			name: "ghes with port",
			cfg: HarnessConfig{
				Name:    "copilot",
				Source:  "ghe.example.com:8443/org/sofa-harness-copilot",
				Version: "0.1.0",
			},
			wantHost:  "ghe.example.com:8443",
			wantOwner: "org",
			wantRepo:  "sofa-harness-copilot",
			wantSig:   SignatureModeAuto,
		},
		{
			name: "localhost with port",
			cfg: HarnessConfig{
				Name:    "local",
				Source:  "localhost:8443/org/repo",
				Version: "0.1.0",
			},
			wantHost:  "localhost:8443",
			wantOwner: "org",
			wantRepo:  "repo",
			wantSig:   SignatureModeAuto,
		},
		{
			name: "ipv4 loopback with port",
			cfg: HarnessConfig{
				Name:    "localip",
				Source:  "127.0.0.1:8443/org/repo",
				Version: "0.1.0",
			},
			wantHost:  "127.0.0.1:8443",
			wantOwner: "org",
			wantRepo:  "repo",
			wantSig:   SignatureModeAuto,
		},
		{
			name: "options with untyped null and empty tuple",
			cfg: HarnessConfig{
				Name: "antigravity",
				Options: cty.ObjectVal(map[string]cty.Value{
					"reasoning_effort": cty.StringVal("high"),
					"custom_endpoint":  cty.NullVal(cty.DynamicPseudoType),
					"extra_args":       cty.EmptyTupleVal,
				}),
			},
			wantSig: SignatureModeAuto,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			require.NoError(t, cfg.Validate())
			require.Equal(t, tt.wantHost, cfg.SourceHost)
			require.Equal(t, tt.wantOwner, cfg.SourceOwner)
			require.Equal(t, tt.wantRepo, cfg.SourceRepo)
			require.Equal(t, tt.wantSig, cfg.Signature)
		})
	}
}

func TestHarnessConfigValidateRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name string
		cfg  HarnessConfig
		want string
	}{
		{
			name: "source without version",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo"},
			want: "together",
		},
		{
			name: "version without source",
			cfg:  HarnessConfig{Name: "a", Version: "0.1.0"},
			want: "together",
		},
		{
			name: "source with scheme",
			cfg:  HarnessConfig{Name: "a", Source: "https://github.com/org/repo", Version: "0.1.0"},
			want: "scheme",
		},
		{
			name: "source with whitespace",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/my repo", Version: "0.1.0"},
			want: "whitespace",
		},
		{
			name: "source with dotgit",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo.git", Version: "0.1.0"},
			want: ".git",
		},
		{
			name: "source with dotdot",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/../repo", Version: "0.1.0"},
			want: "..",
		},
		{
			name: "source wrong segment count",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org", Version: "0.1.0"},
			want: "<host>/<owner>/<repo>",
		},
		{
			name: "source invalid host",
			cfg:  HarnessConfig{Name: "a", Source: "github/org/repo", Version: "0.1.0"},
			want: "host",
		},
		{
			name: "version with v prefix",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo", Version: "v0.1.0"},
			want: `omit the leading "v"`,
		},
		{
			name: "version with uppercase V prefix",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo", Version: "V0.1.0"},
			want: `omit the leading "v"`,
		},
		{
			name: "version with range operator",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo", Version: ">= 0.1.0"},
			want: "range",
		},
		{
			name: "version with pessimistic operator",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo", Version: "~> 0.1.0"},
			want: "range",
		},
		{
			name: "version missing patch",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo", Version: "0.1"},
			want: "semantic version",
		},
		{
			name: "invalid signature",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo", Version: "0.1.0", Signature: "sha256"},
			want: "signature",
		},
		{
			name: "pgp missing signing_key",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo", Version: "0.1.0", Signature: SignatureModePGP},
			want: "signing_key",
		},
		{
			name: "pgp blank signing_key",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo", Version: "0.1.0", Signature: SignatureModePGP, SigningKey: "   "},
			want: "signing_key",
		},
		{
			name: "attestation with signing_key",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo", Version: "0.1.0", Signature: SignatureModeAttestation, SigningKey: "key"},
			want: "signing_key",
		},
		{
			name: "none with signing_key",
			cfg:  HarnessConfig{Name: "a", Source: "github.com/org/repo", Version: "0.1.0", Signature: SignatureModeNone, SigningKey: "key"},
			want: "signing_key",
		},
		{
			name: "local mode with non-default signature",
			cfg:  HarnessConfig{Name: "a", Signature: SignatureModeNone},
			want: "local",
		},
		{
			name: "local mode with signing_key",
			cfg:  HarnessConfig{Name: "a", SigningKey: "key"},
			want: "local",
		},
		{
			name: "invalid env_refs key",
			cfg:  HarnessConfig{Name: "a", EnvRefs: map[string]string{"1BAD": "GOOD"}},
			want: "env_refs",
		},
		{
			name: "invalid env_refs value",
			cfg:  HarnessConfig{Name: "a", EnvRefs: map[string]string{"GOOD": "BAD-NAME"}},
			want: "env_refs",
		},
		{
			name: "options unknown",
			cfg:  HarnessConfig{Name: "a", Options: cty.UnknownVal(cty.Object(map[string]cty.Type{"k": cty.String}))},
			want: "known",
		},
		{
			name: "options non object or map",
			cfg:  HarnessConfig{Name: "a", Options: cty.ListVal([]cty.Value{cty.StringVal("x")})},
			want: "object or map",
		},
		{
			name: "options capsule value",
			cfg: HarnessConfig{
				Name: "a",
				Options: cty.ObjectVal(map[string]cty.Value{
					"cap": cty.CapsuleVal(cty.Capsule("testcap", reflect.TypeOf(struct{}{})), &struct{}{}),
				}),
			},
			want: "capsule",
		},
		{
			name: "options nested capsule null type",
			cfg: HarnessConfig{
				Name: "a",
				Options: cty.ObjectVal(map[string]cty.Value{
					"nested": cty.MapValEmpty(cty.Capsule("testcap", reflect.TypeOf(struct{}{}))),
				}),
			},
			want: "capsule",
		},
		{
			name: "empty harness name",
			cfg:  HarnessConfig{Name: ""},
			want: "name",
		},
		{
			name: "traversal harness name",
			cfg:  HarnessConfig{Name: "../../bin/sh"},
			want: "name",
		},
		{
			name: "slash in harness name",
			cfg:  HarnessConfig{Name: "bad/name"},
			want: "name",
		},
		{
			name: "leading dash harness name",
			cfg:  HarnessConfig{Name: "-flag"},
			want: "name",
		},
		{
			name: "whitespace in harness name",
			cfg:  HarnessConfig{Name: "has space"},
			want: "name",
		},
		{
			name: "leading underscore harness name",
			cfg:  HarnessConfig{Name: "_bad"},
			want: "name",
		},
		{
			name: "source host empty port",
			cfg:  HarnessConfig{Name: "a", Source: "ghe.example.com:/org/repo", Version: "0.1.0"},
			want: "host",
		},
		{
			name: "source host zero port",
			cfg:  HarnessConfig{Name: "a", Source: "ghe.example.com:0/org/repo", Version: "0.1.0"},
			want: "host",
		},
		{
			name: "source host leading zero port",
			cfg:  HarnessConfig{Name: "a", Source: "ghe.example.com:08443/org/repo", Version: "0.1.0"},
			want: "host",
		},
		{
			name: "source host out of range port",
			cfg:  HarnessConfig{Name: "a", Source: "ghe.example.com:70000/org/repo", Version: "0.1.0"},
			want: "host",
		},
		{
			name: "source host non numeric port",
			cfg:  HarnessConfig{Name: "a", Source: "ghe.example.com:abc/org/repo", Version: "0.1.0"},
			want: "host",
		},
		{
			name: "source host signed port",
			cfg:  HarnessConfig{Name: "a", Source: "ghe.example.com:+80/org/repo", Version: "0.1.0"},
			want: "host",
		},
		{
			name: "source host userinfo",
			cfg:  HarnessConfig{Name: "a", Source: "user@github.com/org/repo", Version: "0.1.0"},
			want: "host",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := tt.cfg
			err := cfg.Validate()
			require.Error(t, err)
			require.ErrorContains(t, err, tt.want)
		})
	}
}

func TestHarnessConfigCloneIsolatesEnvRefsAndPreservesMarks(t *testing.T) {
	orig := HarnessConfig{
		Name:      "antigravity",
		Source:    "github.com/org/repo",
		Version:   "0.1.0",
		Signature: SignatureModeAuto,
		EnvRefs:   map[string]string{"KEY": "HOST_KEY"},
		Options:   cty.ObjectVal(map[string]cty.Value{"secret": cty.StringVal("v").Mark(golden.SensitiveMark)}),
	}
	require.NoError(t, orig.Validate())

	cloned := orig.Clone()
	cloned.EnvRefs["KEY"] = "MUTATED"
	cloned.EnvRefs["ADDED"] = "NEW"

	require.Equal(t, "HOST_KEY", orig.EnvRefs["KEY"])
	require.NotContains(t, orig.EnvRefs, "ADDED")
	require.True(t, cloned.Options.GetAttr("secret").HasMark(golden.SensitiveMark))
}

func TestHarnessBlockPrePlanValueIsDynamic(t *testing.T) {
	block := &HarnessBlock{}
	require.Equal(t, cty.DynamicVal, block.Value())
	require.Equal(t, "", block.Type())
	require.Equal(t, "harness", block.BlockType())
	require.Equal(t, 2, block.AddressLength())
	require.False(t, block.CanExecutePrePlan())
}

func TestHarnessBlockBaseDecodeResetsPreviouslyDecodedFields(t *testing.T) {
	strPtr := func(s string) *string { return &s }
	block := &HarnessBlock{
		Source:     strPtr("github.com/org/repo"),
		Version:    strPtr("0.1.0"),
		Signature:  strPtr("pgp"),
		SigningKey: strPtr("key"),
		EnvRefs:    cty.MapVal(map[string]cty.Value{"A": cty.StringVal("B")}),
		Options:    cty.ObjectVal(map[string]cty.Value{"k": cty.StringVal("v")}),
	}
	emptyBody := &hclsyntax.Body{}
	hb := &golden.HclBlock{
		Block: &hclsyntax.Block{
			Type:   "harness",
			Labels: []string{"", "local_only"},
			Body:   emptyBody,
		},
	}
	require.NoError(t, block.BaseDecode(hb, &hcl.EvalContext{}))
	require.Nil(t, block.Source)
	require.Nil(t, block.Version)
	require.Nil(t, block.Signature)
	require.Nil(t, block.SigningKey)
	require.Equal(t, cty.NilVal, block.EnvRefs)
	require.Equal(t, cty.NilVal, block.Options)
}
