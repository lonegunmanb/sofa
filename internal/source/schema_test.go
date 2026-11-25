package source

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSourceConfigValidateAcceptsSupportedRepositories(t *testing.T) {
	urls := []string{
		"https://github.com/example/repo.git",
		"http://git.example.com/repo.git",
		"ssh://git@github.com/example/repo.git",
		"git@github.com:example/repo.git",
		"file:///tmp/repo",
		"/tmp/repo",
		"../repo",
	}

	for _, repositoryURL := range urls {
		t.Run(repositoryURL, func(t *testing.T) {
			cfg := SourceConfig{
				URL:    repositoryURL,
				Branch: "main",
			}
			require.NoError(t, cfg.Validate())
		})
	}
}

func TestSourceConfigValidateRejectsInvalidRepositories(t *testing.T) {
	tests := []struct {
		name string
		url  string
		want string
	}{
		{
			name: "embedded password",
			url:  "https://user:secret@github.com/example/repo.git",
			want: "token_ref",
		},
		{
			name: "whitespace",
			url:  "https://github.com/example/repo .git",
			want: "whitespace",
		},
		{
			name: "unsupported scheme",
			url:  "ftp://github.com/example/repo.git",
			want: "unsupported",
		},
		{
			name: "empty",
			url:  "",
			want: "required",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := SourceConfig{URL: tt.url, Branch: "main"}
			require.ErrorContains(t, cfg.Validate(), tt.want)
		})
	}
}

func TestSourceConfigValidateBranchesAndCredentialReferences(t *testing.T) {
	t.Run("invalid branch", func(t *testing.T) {
		cfg := SourceConfig{
			URL:    "https://github.com/example/repo.git",
			Branch: "bad..branch",
		}
		require.ErrorContains(t, cfg.Validate(), "branch")
	})

	t.Run("valid token reference", func(t *testing.T) {
		tokenRef := "GITHUB_TOKEN"
		cfg := SourceConfig{
			URL:      "https://github.com/example/repo.git",
			Branch:   "main",
			TokenRef: &tokenRef,
		}
		require.NoError(t, cfg.Validate())
	})

	t.Run("mutually exclusive references", func(t *testing.T) {
		tokenRef := "GITHUB_TOKEN"
		sshKeyRef := "DEPLOY_KEY"
		cfg := SourceConfig{
			URL:       "https://github.com/example/repo.git",
			Branch:    "main",
			TokenRef:  &tokenRef,
			SSHKeyRef: &sshKeyRef,
		}
		require.ErrorContains(t, cfg.Validate(), "mutually exclusive")
	})

	t.Run("invalid environment name", func(t *testing.T) {
		tokenRef := "123-BAD"
		cfg := SourceConfig{
			URL:      "https://github.com/example/repo.git",
			Branch:   "main",
			TokenRef: &tokenRef,
		}
		require.ErrorContains(t, cfg.Validate(), "invalid token_ref")
	})
}

func TestValidateEnvironmentVariableName(t *testing.T) {
	for _, name := range []string{"A", "_TOKEN", "GITHUB_TOKEN", "key2"} {
		require.NoError(t, ValidateEnvironmentVariableName(name), name)
	}
	for _, name := range []string{"", "1TOKEN", "BAD-NAME", "BAD NAME"} {
		require.Error(t, ValidateEnvironmentVariableName(name), name)
	}
}
