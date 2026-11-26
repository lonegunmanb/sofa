package main

import (
	"github.com/lonegunmanb/sofa-harness-copilot/internal/server"
	"github.com/lonegunmanb/sofa/pkg/harnessplugin"
)

func main() {
	harnessplugin.Serve(server.New())
}
