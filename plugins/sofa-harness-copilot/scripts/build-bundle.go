// Command build-bundle packages a self-contained Linux sofa-harness-copilot directory
// containing the plugin entrypoint and bin/copilot runtime.
package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

func main() {
	outDir := flag.String("out", "./dist", "output parent directory (creates <out>/sofa-harness-copilot)")
	arch := flag.String("arch", "amd64", "target Linux GOARCH (amd64 or arm64)")
	copilotBin := flag.String("copilotbin", "", "path to prebuilt Linux copilot binary to copy into bin/copilot")
	copilotSrc := flag.String("copilotsrc", "", "path to Go package to cross-compile into bin/copilot")
	flag.Parse()

	if *arch != "amd64" && *arch != "arm64" {
		fatalf("unsupported -arch %q: expected amd64 or arm64", *arch)
	}
	if strings.TrimSpace(*copilotBin) == "" && strings.TrimSpace(*copilotSrc) == "" {
		fatalf("one of -copilotbin or -copilotsrc is required to populate bin/copilot")
	}
	if strings.TrimSpace(*copilotBin) != "" && strings.TrimSpace(*copilotSrc) != "" {
		fatalf("-copilotbin and -copilotsrc are mutually exclusive")
	}

	bundleRoot := filepath.Join(*outDir, "sofa-harness-copilot")
	binDir := filepath.Join(bundleRoot, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		fatalf("create bundle directory %q: %v", binDir, err)
	}

	pluginDest := filepath.Join(bundleRoot, "sofa-harness-copilot")
	if err := buildLinuxBinary("./cmd/sofa-harness-copilot", pluginDest, *arch); err != nil {
		fatalf("build sofa-harness-copilot: %v", err)
	}

	copilotDest := filepath.Join(binDir, "copilot")
	if src := strings.TrimSpace(*copilotSrc); src != "" {
		if err := buildLinuxBinary(src, copilotDest, *arch); err != nil {
			fatalf("build bin/copilot from %q: %v", src, err)
		}
	} else {
		if err := copyFile(*copilotBin, copilotDest, 0o755); err != nil {
			fatalf("copy bin/copilot from %q: %v", *copilotBin, err)
		}
	}

	fmt.Printf("Built self-contained Linux bundle at %s\n", bundleRoot)
}

func buildLinuxBinary(pkgPath, outPath, arch string) error {
	cmd := exec.Command("go", "build", "-trimpath", "-o", outPath, pkgPath)
	cmd.Env = append(os.Environ(),
		"CGO_ENABLED=0",
		"GOOS=linux",
		"GOARCH="+arch,
	)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return os.Chmod(outPath, 0o755)
}

func copyFile(src, dst string, mode os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()

	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, mode)
}

func fatalf(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
