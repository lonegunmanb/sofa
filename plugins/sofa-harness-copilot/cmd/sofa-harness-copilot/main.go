package main

import (
	"github.com/lonegunmanb/sofa-harness-copilot/internal/server"
	"github.com/lonegunmanb/sofa/pkg/harnessplugin"
)

func main() {
	server.EnsureCopilotHome()
	harnessplugin.Serve(server.New())
}
