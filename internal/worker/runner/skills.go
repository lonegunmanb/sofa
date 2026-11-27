package runner

import (
	"bytes"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"

	sandboxruntime "github.com/lonegunmanb/sofa/internal/sandbox/runtime"
	sandboxspec "github.com/lonegunmanb/sofa/internal/sandbox/spec"
)

type sessionSkillsInput struct {
	SkillDirectories []string
	Skills           []string
	DisabledSkills   []string
	BundleDir        string
	WorkspaceDir     string
	ImportsDir       string
	SandboxMounts    []sandboxspec.MountSpec
	StageRootDir     string
}

type preparedSessionSkills struct {
	SkillDirectories []string
	Skills           []string
	DisabledSkills   []string
	HostSkillsDir    string
}

func prepareSessionSkills(in sessionSkillsInput) (preparedSessionSkills, error) {
	disabledSet := make(map[string]struct{}, len(in.DisabledSkills))
	normalizedDisabled := make([]string, 0, len(in.DisabledSkills))
	for _, raw := range in.DisabledSkills {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		if _, dup := disabledSet[trimmed]; dup {
			continue
		}
		disabledSet[trimmed] = struct{}{}
		normalizedDisabled = append(normalizedDisabled, trimmed)
	}

	relocatedDirs := make([]string, 0, len(in.SkillDirectories))
	seenDirs := make(map[string]struct{}, len(in.SkillDirectories))
	for _, raw := range in.SkillDirectories {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			continue
		}
		relocated, err := relocateSkillDirectory(trimmed, in.BundleDir)
		if err != nil {
			return preparedSessionSkills{}, err
		}
		if _, dup := seenDirs[relocated]; dup {
			continue
		}
		seenDirs[relocated] = struct{}{}
		relocatedDirs = append(relocatedDirs, relocated)
	}

	hasHostRoot := strings.TrimSpace(in.WorkspaceDir) != "" ||
		strings.TrimSpace(in.BundleDir) != "" ||
		strings.TrimSpace(in.ImportsDir) != "" ||
		len(in.SandboxMounts) > 0

	enabledByEffective := make(map[string]string)
	enabledByEntry := make(map[string]string)
	finalDirs := make([]string, len(relocatedDirs))
	stagedAny := false
	hasUnmappedDir := false

	for i, containerDir := range relocatedDirs {
		finalDirs[i] = containerDir

		hostDir, mapped := mapContainerSkillDirToHost(containerDir, in.BundleDir, in.WorkspaceDir, in.ImportsDir, in.SandboxMounts)
		if !mapped {
			hasUnmappedDir = true
			continue
		}
		info, err := os.Lstat(hostDir)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			// Lenient mode: missing or non-directory skill paths never fail the session.
			continue
		}
		entries, err := os.ReadDir(hostDir)
		if err != nil {
			return preparedSessionSkills{}, fmt.Errorf("read skill directory %q: %w", hostDir, err)
		}

		var enabledSubdirs []string
		excludedAny := false
		for _, ent := range entries {
			if ent.Type()&os.ModeSymlink != 0 || !ent.IsDir() {
				continue
			}
			entryName := ent.Name()
			skillMDPath := filepath.Join(hostDir, entryName, "SKILL.md")
			mdInfo, mdErr := os.Lstat(skillMDPath)
			if mdErr != nil || mdInfo.Mode()&os.ModeSymlink != 0 || !mdInfo.Mode().IsRegular() {
				continue
			}
			mdBytes, readErr := os.ReadFile(skillMDPath)
			if readErr != nil {
				return preparedSessionSkills{}, fmt.Errorf("read skill manifest %q: %w", skillMDPath, readErr)
			}
			effName := parseSkillFrontmatterName(string(mdBytes), entryName)
			if _, dEff := disabledSet[effName]; dEff {
				excludedAny = true
				continue
			}
			if _, dEntry := disabledSet[entryName]; dEntry {
				excludedAny = true
				continue
			}

			enabledSubdirs = append(enabledSubdirs, entryName)
			if _, exists := enabledByEffective[effName]; !exists {
				enabledByEffective[effName] = effName
			}
			if _, exists := enabledByEntry[entryName]; !exists {
				enabledByEntry[entryName] = effName
			}
		}

		if len(disabledSet) > 0 && excludedAny {
			if strings.TrimSpace(in.StageRootDir) == "" {
				return preparedSessionSkills{}, fmt.Errorf("stage root directory is required to isolate disabled skills in %q", hostDir)
			}
			dirSlot := fmt.Sprintf("dir-%02d", i)
			destRoot := filepath.Join(in.StageRootDir, dirSlot)
			if err := os.RemoveAll(destRoot); err != nil {
				return preparedSessionSkills{}, fmt.Errorf("reset skill stage dir %q: %w", destRoot, err)
			}
			if err := os.MkdirAll(destRoot, 0o755); err != nil {
				return preparedSessionSkills{}, fmt.Errorf("create skill stage dir %q: %w", destRoot, err)
			}
			for _, sub := range enabledSubdirs {
				srcSub := filepath.Join(hostDir, sub)
				dstSub := filepath.Join(destRoot, sub)
				if err := copyCleanSkillTree(srcSub, dstSub); err != nil {
					return preparedSessionSkills{}, fmt.Errorf("stage enabled skill %q: %w", sub, err)
				}
			}
			stagedAny = true
			finalDirs[i] = "/.sofa/skills/" + dirSlot
		}
	}

	finalSkills := make([]string, 0, len(in.Skills))
	seenSkills := make(map[string]struct{}, len(in.Skills))
	for _, raw := range in.Skills {
		reqName := strings.TrimSpace(raw)
		if reqName == "" {
			continue
		}
		if _, isDisabled := disabledSet[reqName]; isDisabled {
			continue
		}

		resolved := reqName
		if hasHostRoot {
			if matched, ok := enabledByEffective[reqName]; ok {
				resolved = matched
			} else if matched, ok := enabledByEntry[reqName]; ok {
				resolved = matched
			} else if !hasUnmappedDir {
				// Lenient mode: missing requested skill is silently skipped when all dirs are host-mapped.
				continue
			}
			if _, isDisabled := disabledSet[resolved]; isDisabled {
				continue
			}
		}

		if _, dup := seenSkills[resolved]; dup {
			continue
		}
		seenSkills[resolved] = struct{}{}
		finalSkills = append(finalSkills, resolved)
	}

	hostSkillsDir := ""
	if stagedAny {
		hostSkillsDir = in.StageRootDir
	}
	return preparedSessionSkills{
		SkillDirectories: finalDirs,
		Skills:           finalSkills,
		DisabledSkills:   normalizedDisabled,
		HostSkillsDir:    hostSkillsDir,
	}, nil
}

func relocateSkillDirectory(raw, bundleDir string) (string, error) {
	if strings.Contains(raw, `\`) {
		return "", fmt.Errorf("worker runner: skill_directories entry %q must use '/' separators", raw)
	}
	if len(raw) >= 2 && isASCIIAlphaByte(raw[0]) && raw[1] == ':' {
		return "", fmt.Errorf("worker runner: skill_directories entry %q must not contain a drive prefix", raw)
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == ".." {
			return "", fmt.Errorf("worker runner: skill_directories entry %q must not contain '..'", raw)
		}
	}

	if strings.HasPrefix(raw, "/") {
		return path.Clean(raw), nil
	}

	cleaned := path.Clean(raw)
	cleaned = strings.TrimPrefix(cleaned, "./")
	if cleaned == "" || cleaned == "." {
		return sandboxruntime.DefaultContainerWorkingDir, nil
	}

	if isBundleSkillRelative(cleaned) {
		trimmedBundle := strings.TrimSpace(bundleDir)
		if trimmedBundle != "" {
			direct := filepath.Join(trimmedBundle, filepath.FromSlash(cleaned))
			if _, err := os.Stat(direct); err != nil {
				if cleaned == "config" {
					if info, bErr := os.Stat(trimmedBundle); bErr == nil && info.IsDir() {
						return "/.sofa/bundle", nil
					}
				} else if strings.HasPrefix(cleaned, "config/") {
					stripped := strings.TrimPrefix(cleaned, "config/")
					if _, sErr := os.Stat(filepath.Join(trimmedBundle, filepath.FromSlash(stripped))); sErr == nil {
						return "/.sofa/bundle/" + stripped, nil
					}
				}
			}
		}
		return "/.sofa/bundle/" + cleaned, nil
	}

	return sandboxruntime.DefaultContainerWorkingDir + "/" + cleaned, nil
}

func isBundleSkillRelative(cleaned string) bool {
	return cleaned == "config" ||
		strings.HasPrefix(cleaned, "config/") ||
		cleaned == "modules" ||
		strings.HasPrefix(cleaned, "modules/") ||
		cleaned == "external_skills" ||
		strings.HasPrefix(cleaned, "external_skills/")
}

func mapContainerSkillDirToHost(containerDir, bundleDir, workspaceDir, importsDir string, mounts []sandboxspec.MountSpec) (string, bool) {
	if trimmed := strings.TrimSpace(bundleDir); trimmed != "" {
		if containerDir == "/.sofa/bundle" {
			return trimmed, true
		}
		if strings.HasPrefix(containerDir, "/.sofa/bundle/") {
			rel := strings.TrimPrefix(containerDir, "/.sofa/bundle/")
			return filepath.Join(trimmed, filepath.FromSlash(rel)), true
		}
	}
	if trimmed := strings.TrimSpace(workspaceDir); trimmed != "" {
		if containerDir == sandboxruntime.DefaultContainerWorkingDir {
			return trimmed, true
		}
		if strings.HasPrefix(containerDir, sandboxruntime.DefaultContainerWorkingDir+"/") {
			rel := strings.TrimPrefix(containerDir, sandboxruntime.DefaultContainerWorkingDir+"/")
			return filepath.Join(trimmed, filepath.FromSlash(rel)), true
		}
	}
	if trimmed := strings.TrimSpace(importsDir); trimmed != "" {
		if containerDir == "/.sofa/imports" {
			return trimmed, true
		}
		if strings.HasPrefix(containerDir, "/.sofa/imports/") {
			rel := strings.TrimPrefix(containerDir, "/.sofa/imports/")
			return filepath.Join(trimmed, filepath.FromSlash(rel)), true
		}
	}
	for _, m := range mounts {
		target := path.Clean(strings.TrimSpace(m.Target))
		src := strings.TrimSpace(m.Source)
		if target == "" || target == "." || src == "" {
			continue
		}
		if containerDir == target {
			return src, true
		}
		if strings.HasPrefix(containerDir, target+"/") {
			rel := strings.TrimPrefix(containerDir, target+"/")
			return filepath.Join(src, filepath.FromSlash(rel)), true
		}
	}
	return "", false
}

func parseSkillFrontmatterName(content, fallbackDirName string) string {
	normalized := strings.ReplaceAll(content, "\r\n", "\n")
	normalized = strings.ReplaceAll(normalized, "\r", "\n")
	if !strings.HasPrefix(normalized, "---\n") {
		return fallbackDirName
	}
	rest := normalized[len("---\n"):]
	endIdx := strings.Index(rest, "\n---")
	var block string
	if endIdx >= 0 {
		after := rest[endIdx+len("\n---"):]
		if after != "" && !strings.HasPrefix(after, "\n") {
			return fallbackDirName
		}
		block = rest[:endIdx]
	} else if strings.HasPrefix(rest, "---") && (len(rest) == 3 || rest[3] == '\n') {
		return fallbackDirName
	} else {
		return fallbackDirName
	}

	for _, line := range strings.Split(block, "\n") {
		if !strings.HasPrefix(line, "name:") {
			continue
		}
		val := strings.TrimSpace(strings.TrimPrefix(line, "name:"))
		if len(val) >= 2 && ((val[0] == '"' && val[len(val)-1] == '"') || (val[0] == '\'' && val[len(val)-1] == '\'')) {
			val = val[1 : len(val)-1]
		}
		val = strings.TrimSpace(val)
		if val != "" {
			return val
		}
	}
	return fallbackDirName
}

func copyCleanSkillTree(srcDir, dstDir string) error {
	info, err := os.Lstat(srcDir)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return err
	}
	for _, ent := range entries {
		if ent.Type()&os.ModeSymlink != 0 {
			continue
		}
		srcPath := filepath.Join(srcDir, ent.Name())
		dstPath := filepath.Join(dstDir, ent.Name())
		entryInfo, lerr := os.Lstat(srcPath)
		if lerr != nil {
			return lerr
		}
		if entryInfo.Mode()&os.ModeSymlink != 0 {
			continue
		}
		if entryInfo.IsDir() {
			if err := copyCleanSkillTree(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if !entryInfo.Mode().IsRegular() {
			continue
		}
		data, err := os.ReadFile(srcPath)
		if err != nil {
			return err
		}
		mode := fs.FileMode(0o644)
		if entryInfo.Mode().Perm()&0o111 != 0 || bytes.HasPrefix(data, []byte("#!")) {
			mode = 0o755
		}
		if err := os.WriteFile(dstPath, data, mode); err != nil {
			return err
		}
		if err := os.Chmod(dstPath, mode); err != nil {
			return err
		}
	}
	return nil
}

func isASCIIAlphaByte(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
