package artifact

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"

	corespec "github.com/lonegunmanb/sofa/internal/spec"
	workerspec "github.com/lonegunmanb/sofa/internal/worker/spec"
)

// Issue describes a structured artifact validation issue aligned with internal/spec.Issue.
type Issue = corespec.Issue

// Validate verifies declared worker artifacts inside host artifactsDir (mounted at /.sofa/artifacts).
// It confines traversal with os.OpenRoot, rejects any symlinks, and returns valid declarations
// alongside structured validation issues for agent self-repair.
func Validate(artifactsDir string, decls []workerspec.Artifact) ([]workerspec.Artifact, []Issue, error) {
	if strings.TrimSpace(artifactsDir) == "" {
		return nil, nil, fmt.Errorf("artifact directory is required")
	}

	seen := make(map[string]struct{}, len(decls))
	cleaned := make([]workerspec.Artifact, len(decls))
	for i, decl := range decls {
		if err := workerspec.ValidateSafeLabel(decl.Name, "artifact", true); err != nil {
			return nil, nil, err
		}
		if _, dup := seen[decl.Name]; dup {
			return nil, nil, fmt.Errorf("duplicate artifact name %q", decl.Name)
		}
		seen[decl.Name] = struct{}{}

		if decl.Type != "file" && decl.Type != "directory" {
			return nil, nil, fmt.Errorf("artifact %q type must be file or directory", decl.Name)
		}
		rel, err := cleanRelativePath(decl.Path, fmt.Sprintf("artifact %q path", decl.Name))
		if err != nil {
			return nil, nil, err
		}
		decl.Path = rel
		cleaned[i] = decl
	}

	dirInfo, err := os.Lstat(artifactsDir)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			issues := make([]Issue, 0)
			for _, decl := range cleaned {
				if decl.Required {
					issues = append(issues, missingArtifactIssue(decl))
				}
			}
			return nil, issues, nil
		}
		return nil, nil, fmt.Errorf("inspect artifact directory %q: %w", artifactsDir, err)
	}
	if dirInfo.Mode()&fs.ModeSymlink != 0 {
		return nil, nil, fmt.Errorf("artifact directory %q is a symlink, which is forbidden", artifactsDir)
	}
	if !dirInfo.IsDir() {
		return nil, nil, fmt.Errorf("artifact directory %q is not a directory", artifactsDir)
	}

	root, err := os.OpenRoot(artifactsDir)
	if err != nil {
		return nil, nil, fmt.Errorf("open artifact root %q: %w", artifactsDir, err)
	}
	defer func() { _ = root.Close() }()

	valid := make([]workerspec.Artifact, 0, len(cleaned))
	issues := make([]Issue, 0)

	for _, decl := range cleaned {
		declIssues, ok, valErr := validateOneArtifact(root, decl)
		if valErr != nil {
			return nil, nil, valErr
		}
		issues = append(issues, declIssues...)
		if ok {
			valid = append(valid, decl)
		}
	}
	return valid, issues, nil
}

func validateOneArtifact(root *os.Root, decl workerspec.Artifact) ([]Issue, bool, error) {
	if blockedParent, blocked, missing, err := inspectAncestors(root, decl); err != nil {
		return nil, false, err
	} else if blocked {
		if decl.Required {
			return []Issue{newIssue(
				"artifact_path_blocked",
				fmt.Sprintf("required artifact %q is blocked by non-directory parent %q", decl.Name, blockedParent),
				decl.ContainerPath(),
				"replace the blocking parent with a directory and create the artifact",
			)}, false, nil
		}
		return nil, false, nil
	} else if missing {
		if decl.Required {
			return []Issue{missingArtifactIssue(decl)}, false, nil
		}
		return nil, false, nil
	}

	info, err := root.Lstat(decl.Path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			if decl.Required {
				return []Issue{missingArtifactIssue(decl)}, false, nil
			}
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("inspect artifact %q (%s): %w", decl.Name, decl.Path, err)
	}
	if info.Mode()&fs.ModeSymlink != 0 {
		return nil, false, fmt.Errorf("artifact %q path %q is a symlink, which is forbidden", decl.Name, decl.Path)
	}

	typeMatches := info.Mode().IsRegular()
	if decl.Type == "directory" {
		typeMatches = info.IsDir()
	}
	if !typeMatches {
		return []Issue{newIssue(
			"artifact_type_mismatch",
			fmt.Sprintf("artifact %q is not a %s", decl.Name, decl.Type),
			decl.ContainerPath(),
			fmt.Sprintf("replace it with a %s", decl.Type),
		)}, false, nil
	}

	if decl.Type == "file" {
		if decl.NonEmpty && info.Size() == 0 {
			return []Issue{emptyArtifactIssue(decl)}, false, nil
		}
		return nil, true, nil
	}

	hasRegularFile, err := inspectDirectoryTree(root, decl.Path)
	if err != nil {
		return nil, false, fmt.Errorf("inspect artifact %q directory %q: %w", decl.Name, decl.Path, err)
	}
	if decl.NonEmpty && !hasRegularFile {
		return []Issue{emptyArtifactIssue(decl)}, false, nil
	}
	return nil, true, nil
}

func inspectAncestors(root *os.Root, decl workerspec.Artifact) (blockedParent string, blocked bool, missing bool, err error) {
	parts := strings.Split(decl.Path, "/")
	if len(parts) <= 1 {
		return "", false, false, nil
	}
	cur := ""
	for _, seg := range parts[:len(parts)-1] {
		if cur == "" {
			cur = seg
		} else {
			cur = cur + "/" + seg
		}
		info, lerr := root.Lstat(cur)
		if lerr != nil {
			if errors.Is(lerr, fs.ErrNotExist) {
				return "", false, true, nil
			}
			return "", false, false, fmt.Errorf("inspect artifact %q ancestor %q: %w", decl.Name, cur, lerr)
		}
		if info.Mode()&fs.ModeSymlink != 0 {
			return "", false, false, fmt.Errorf("artifact %q ancestor %q is a symlink, which is forbidden", decl.Name, cur)
		}
		if !info.IsDir() {
			return path.Join(workerspec.ContainerArtifactsRoot, cur), true, false, nil
		}
	}
	return "", false, false, nil
}

func inspectDirectoryTree(root *os.Root, dirRel string) (bool, error) {
	hasRegularFile := false
	err := fs.WalkDir(root.FS(), dirRel, func(rel string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		linfo, lerr := root.Lstat(rel)
		if lerr != nil {
			return lerr
		}
		if linfo.Mode()&fs.ModeSymlink != 0 {
			return fmt.Errorf("entry %q is a symlink, which is forbidden", rel)
		}
		if linfo.IsDir() {
			return nil
		}
		if !linfo.Mode().IsRegular() {
			return fmt.Errorf("entry %q is not a regular file", rel)
		}
		hasRegularFile = true
		return nil
	})
	if err != nil {
		return false, err
	}
	return hasRegularFile, nil
}

func missingArtifactIssue(decl workerspec.Artifact) Issue {
	return newIssue(
		"artifact_missing",
		fmt.Sprintf("required artifact %q does not exist", decl.Name),
		decl.ContainerPath(),
		"create the artifact at the declared path",
	)
}

func emptyArtifactIssue(decl workerspec.Artifact) Issue {
	return newIssue(
		"artifact_empty",
		fmt.Sprintf("artifact %q is empty", decl.Name),
		decl.ContainerPath(),
		"write non-empty artifact content at the declared path",
	)
}

func newIssue(code, message, containerPath, repairHint string) Issue {
	return Issue{
		Code:       code,
		Message:    message,
		Path:       &containerPath,
		RepairHint: &repairHint,
	}
}

func cleanRelativePath(raw, label string) (string, error) {
	if strings.TrimSpace(raw) == "" {
		return "", fmt.Errorf("%s must not be empty", label)
	}
	if strings.Contains(raw, `\`) {
		return "", fmt.Errorf("%s %q must use forward slashes", label, raw)
	}
	if strings.HasPrefix(raw, "/") || path.IsAbs(raw) {
		return "", fmt.Errorf("%s %q must be relative", label, raw)
	}
	if len(raw) >= 2 && isASCIIAlpha(raw[0]) && raw[1] == ':' {
		return "", fmt.Errorf("%s %q must not contain a drive prefix", label, raw)
	}
	for _, seg := range strings.Split(raw, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return "", fmt.Errorf("%s %q contains invalid path segment %q", label, raw, seg)
		}
	}
	cleaned := path.Clean(raw)
	if cleaned == "." || cleaned == ".." || strings.HasPrefix(cleaned, "../") {
		return "", fmt.Errorf("%s %q must not escape root", label, raw)
	}
	return cleaned, nil
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}
