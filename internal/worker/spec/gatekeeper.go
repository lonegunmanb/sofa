package spec

import "time"

type GatekeeperConfig struct {
	Name             string
	Command          []string
	WorkingDir       string
	Timeout          string
	ExpectExitCode   int
	RequireCleanTree bool
}

type GatekeeperBlock struct {
	Name             string
	Command          []string
	WorkingDir       string
	Timeout          string
	ExpectExitCode   int
	RequireCleanTree *bool
}

func (g GatekeeperBlock) Config() GatekeeperConfig {
	timeout := g.Timeout
	if timeout == "" {
		timeout = "10m"
	}
	workingDir := g.WorkingDir
	if workingDir == "" {
		workingDir = "."
	}
	requireCleanTree := true
	if g.RequireCleanTree != nil {
		requireCleanTree = *g.RequireCleanTree
	}
	return GatekeeperConfig{
		Name: g.Name, Command: append([]string{}, g.Command...), WorkingDir: workingDir,
		Timeout: timeout, ExpectExitCode: g.ExpectExitCode, RequireCleanTree: requireCleanTree,
	}
}

func validateGatekeeper(g GatekeeperConfig) error {
	if len(g.Command) == 0 || g.Command[0] == "" {
		return fail("gatekeeper %q command must be a non-empty list", g.Name)
	}
	if err := validateWorkspaceRelativePath(g.WorkingDir, "gatekeeper working_dir"); err != nil {
		return err
	}
	duration, err := time.ParseDuration(g.Timeout)
	if err != nil || duration <= 0 {
		return fail("gatekeeper %q timeout must be a positive duration", g.Name)
	}
	return nil
}
