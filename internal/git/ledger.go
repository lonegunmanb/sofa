package git

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"

	corespec "github.com/lonegunmanb/sofa/internal/spec"
)

// BranchKind classifies the lifecycle role of a Git branch tracked by Ledger.
type BranchKind string

const (
	KindWorkBranch       BranchKind = "work_branch"
	KindCheckpointBranch BranchKind = "checkpoint_branch"
	KindMergeBranch      BranchKind = "merge_branch"
)

var (
	// ErrUnregisteredBranch is returned when a remote branch operation targets an unregistered branch.
	ErrUnregisteredBranch = errors.New("branch is not registered in ledger")
	// ErrProtectedSourceBranch is returned when attempting to register a protected source branch for mutation.
	ErrProtectedSourceBranch = errors.New("cannot register protected source branch")
	// ErrBranchOwnershipConflict is returned when a different node or kind claims an already registered branch.
	ErrBranchOwnershipConflict = errors.New("branch ownership conflict")
	// ErrInvalidBaseSHA is returned when base_sha is not a 40-character lowercase hexadecimal commit SHA.
	ErrInvalidBaseSHA = errors.New("base_sha must be a 40-character lowercase hex SHA")
)

var (
	sha40LowerPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
	scpRepoPattern    = regexp.MustCompile(`^([^@\s]+)@([^:\s]+):([^\s]+)$`)
)

// SourceRef identifies a protected source repository branch.
type SourceRef struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

// PreserveRef identifies a branch explicitly preserved across Run cleanup.
type PreserveRef struct {
	URL    string `json:"url"`
	Branch string `json:"branch"`
}

// BranchTarget identifies a remote branch targeted for cleanup.
type BranchTarget struct {
	RepoURL string `json:"repo_url"`
	Branch  string `json:"branch"`
}

// BranchEntry records ownership and base commit lineage for a managed branch.
type BranchEntry struct {
	RepoURL    string     `json:"repo_url"`
	Branch     string     `json:"branch"`
	BaseSHA    string     `json:"base_sha"`
	Kind       BranchKind `json:"kind"`
	OwnerNode  string     `json:"owner_node"`
	Epoch      uint64     `json:"epoch"`
	Registered bool       `json:"registered"`
}

// LedgerSnapshot is a JSON-serializable snapshot of a Ledger's state.
type LedgerSnapshot struct {
	ProtectedSources []SourceRef   `json:"protected_sources,omitempty"`
	Entries          []BranchEntry `json:"entries,omitempty"`
}

type ledgerKey struct {
	repoURL string
	branch  string
}

// Ledger maintains thread-safe branch ownership and cleanup planning for a Run.
type Ledger struct {
	mu               sync.RWMutex
	protectedSources []SourceRef
	entries          map[ledgerKey]BranchEntry
}

// NewLedger creates a Ledger pre-populated with optional protected source branches.
func NewLedger(sources ...SourceRef) *Ledger {
	protected := make([]SourceRef, 0, len(sources))
	for _, s := range sources {
		branch := canonicalBranchForMatch(s.Branch)
		if branch == "" {
			continue
		}
		protected = append(protected, SourceRef{
			URL:    s.URL,
			Branch: branch,
		})
	}
	return &Ledger{
		protectedSources: protected,
		entries:          make(map[ledgerKey]BranchEntry),
	}
}

// NormalizeBranchName canonicalizes a branch reference by stripping an optional
// "refs/heads/" prefix and validating that it is a valid branch name (rejecting
// special refs such as "HEAD" or non-head namespaces like "refs/tags/...").
func NormalizeBranchName(branch string) (string, error) {
	trimmed := strings.TrimSpace(branch)
	if trimmed == "" {
		return "", errors.New("branch name must not be empty")
	}
	if strings.EqualFold(trimmed, "HEAD") {
		return "", errors.New("branch name must not be HEAD")
	}
	if strings.HasPrefix(trimmed, "refs/") {
		if !strings.HasPrefix(trimmed, "refs/heads/") {
			return "", fmt.Errorf("branch ref %q must be a branch name or refs/heads/ ref", trimmed)
		}
		trimmed = strings.TrimPrefix(trimmed, "refs/heads/")
	}
	if strings.EqualFold(trimmed, "HEAD") {
		return "", errors.New("branch name must not be HEAD")
	}
	if err := corespec.ValidateBranchName(trimmed); err != nil {
		return "", err
	}
	return trimmed, nil
}

func canonicalBranchForMatch(branch string) string {
	if norm, err := NormalizeBranchName(branch); err == nil {
		return norm
	}
	trimmed := strings.TrimSpace(branch)
	return strings.TrimPrefix(trimmed, "refs/heads/")
}

// IsValidCommitSHA reports whether sha is an exact 40-character lowercase hex Git commit hash.
func IsValidCommitSHA(sha string) bool {
	return sha40LowerPattern.MatchString(sha)
}

// WorkBranchName returns the deterministic managed work branch name for a node.
func WorkBranchName(runID, canonicalNodeAddr string) string {
	return "sofa/work/" + sanitizeRefSegment(runID) + "/" + sanitizeNodeKey(canonicalNodeAddr)
}

// CheckpointBranchName returns the deterministic epoch-scoped checkpoint branch name for a node.
func CheckpointBranchName(runID, canonicalNodeAddr string, epoch uint64) string {
	return "sofa/ckpt/" + sanitizeRefSegment(runID) + "/" + sanitizeNodeKey(canonicalNodeAddr) + "/e" + strconv.FormatUint(epoch, 10)
}

// NormalizeRepoURL canonicalizes repository URLs and local paths for comparison.
func NormalizeRepoURL(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	if strings.Contains(trimmed, "://") {
		parsed, err := url.Parse(trimmed)
		if err == nil && parsed.Scheme != "" {
			parsed.Scheme = strings.ToLower(parsed.Scheme)
			if parsed.Scheme == "file" {
				if strings.EqualFold(parsed.Host, "localhost") {
					parsed.Host = ""
				}
				p := cleanSlashPath(parsed.Path)
				p = stripGitSuffix(p)
				return "file://" + p
			}
			parsed.Host = strings.ToLower(parsed.Host)
			p := strings.TrimSuffix(parsed.Path, "/")
			p = strings.TrimSuffix(p, ".git")
			p = strings.TrimSuffix(p, "/")
			parsed.Path = p
			return parsed.String()
		}
	}
	if m := scpRepoPattern.FindStringSubmatch(trimmed); len(m) == 4 {
		user, host, repoPath := m[1], strings.ToLower(m[2]), m[3]
		repoPath = strings.TrimSuffix(repoPath, "/")
		repoPath = strings.TrimSuffix(repoPath, ".git")
		repoPath = strings.TrimSuffix(repoPath, "/")
		return user + "@" + host + ":" + repoPath
	}
	p := cleanSlashPath(trimmed)
	return stripGitSuffix(p)
}

func cleanSlashPath(raw string) string {
	slashed := strings.ReplaceAll(raw, "\\", "/")
	cleaned := path.Clean(slashed)
	if len(cleaned) >= 3 && cleaned[0] == '/' && isASCIIAlpha(cleaned[1]) && cleaned[2] == ':' {
		cleaned = "/" + strings.ToLower(cleaned[1:2]) + cleaned[2:]
	} else if len(cleaned) >= 2 && isASCIIAlpha(cleaned[0]) && cleaned[1] == ':' {
		cleaned = strings.ToLower(cleaned[:1]) + cleaned[1:]
	}
	return cleaned
}

func stripGitSuffix(p string) string {
	if p != "/" {
		p = strings.TrimSuffix(p, "/")
	}
	if strings.HasSuffix(p, ".git") && p != ".git" && p != "/.git" {
		p = strings.TrimSuffix(p, ".git")
	}
	if p != "/" {
		p = strings.TrimSuffix(p, "/")
	}
	return p
}

func isASCIIAlpha(b byte) bool {
	return (b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z')
}

func sanitizeRefSegment(s string) string {
	var b strings.Builder
	prevSep := false
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			b.WriteRune(r)
			prevSep = false
			continue
		}
		if !prevSep && b.Len() > 0 {
			b.WriteByte('-')
			prevSep = true
		}
	}
	out := strings.Trim(b.String(), "-_")
	if out == "" {
		return "run"
	}
	return out
}

func sanitizeNodeKey(canonicalNodeAddr string) string {
	sum := sha256.Sum256([]byte(canonicalNodeAddr))
	shortHash := hex.EncodeToString(sum[:4])

	var b strings.Builder
	prevUnderscore := false
	for _, r := range canonicalNodeAddr {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
			prevUnderscore = false
			continue
		}
		if !prevUnderscore && b.Len() > 0 {
			b.WriteByte('_')
			prevUnderscore = true
		}
	}
	prefix := strings.Trim(b.String(), "_-")
	if prefix == "" {
		prefix = "node"
	}
	if len(prefix) > 64 {
		prefix = strings.TrimRight(prefix[:64], "_-")
		if prefix == "" {
			prefix = "node"
		}
	}
	return prefix + "_" + shortHash
}

// Register records ownership and base SHA for a managed branch before creation or push.
func (l *Ledger) Register(repoURL, branch, baseSHA string, kind BranchKind, ownerNode string, epoch uint64) error {
	if l == nil {
		return errors.New("ledger is nil")
	}
	if strings.TrimSpace(repoURL) == "" {
		return errors.New("repository url is required")
	}
	cleanBranch, err := NormalizeBranchName(branch)
	if err != nil {
		return err
	}
	if strings.TrimSpace(ownerNode) == "" {
		return errors.New("owner_node is required")
	}
	if !isValidBranchKind(kind) {
		return fmt.Errorf("unsupported branch kind %q", kind)
	}
	if !IsValidCommitSHA(baseSHA) {
		return fmt.Errorf("%w: %q", ErrInvalidBaseSHA, baseSHA)
	}

	normURL := NormalizeRepoURL(repoURL)

	l.mu.Lock()
	defer l.mu.Unlock()

	if kind != KindMergeBranch {
		for _, src := range l.protectedSources {
			if refMatches(normURL, cleanBranch, src.URL, src.Branch) {
				return fmt.Errorf("%w: %q", ErrProtectedSourceBranch, cleanBranch)
			}
		}
	}

	key := ledgerKey{repoURL: normURL, branch: cleanBranch}
	if existing, exists := l.entries[key]; exists {
		if existing.OwnerNode != ownerNode || existing.Kind != kind {
			return fmt.Errorf("%w: branch %q is owned by %q (%s), cannot register for %q (%s)",
				ErrBranchOwnershipConflict, cleanBranch, existing.OwnerNode, existing.Kind, ownerNode, kind)
		}
		if epoch < existing.Epoch {
			return fmt.Errorf("stale epoch %d for branch %q (current epoch %d)", epoch, cleanBranch, existing.Epoch)
		}
		if epoch == existing.Epoch && existing.BaseSHA != baseSHA {
			return fmt.Errorf("branch %q already registered at epoch %d with base_sha %s (got %s)",
				cleanBranch, epoch, existing.BaseSHA, baseSHA)
		}
	}

	l.entries[key] = BranchEntry{
		RepoURL:    strings.TrimSpace(repoURL),
		Branch:     cleanBranch,
		BaseSHA:    baseSHA,
		Kind:       kind,
		OwnerNode:  ownerNode,
		Epoch:      epoch,
		Registered: true,
	}
	return nil
}

// EnsureRegistered verifies that (repoURL, branch) has been registered in this Ledger.
func (l *Ledger) EnsureRegistered(repoURL, branch string) error {
	if l == nil {
		return errors.New("ledger is nil")
	}
	cleanBranch, err := NormalizeBranchName(branch)
	if err != nil {
		return err
	}
	key := ledgerKey{
		repoURL: NormalizeRepoURL(repoURL),
		branch:  cleanBranch,
	}

	l.mu.RLock()
	entry, ok := l.entries[key]
	l.mu.RUnlock()

	if !ok || !entry.Registered {
		return fmt.Errorf("%w: %s (%s)", ErrUnregisteredBranch, cleanBranch, repoURL)
	}
	return nil
}

// EnsureDeletable verifies that (repoURL, branch) is registered in this Ledger and
// does not match any protected source branch (even if registered as KindMergeBranch).
func (l *Ledger) EnsureDeletable(repoURL, branch string) error {
	if err := l.EnsureRegistered(repoURL, branch); err != nil {
		return err
	}
	cleanBranch, err := NormalizeBranchName(branch)
	if err != nil {
		return err
	}
	normURL := NormalizeRepoURL(repoURL)

	l.mu.RLock()
	defer l.mu.RUnlock()

	for _, src := range l.protectedSources {
		if refMatches(normURL, cleanBranch, src.URL, src.Branch) {
			return fmt.Errorf("%w: refusing to delete protected source branch %q", ErrProtectedSourceBranch, cleanBranch)
		}
	}
	return nil
}

// Entries returns a deterministically sorted copy of all registered branch entries.
func (l *Ledger) Entries() []BranchEntry {
	if l == nil {
		return nil
	}
	l.mu.RLock()
	out := make([]BranchEntry, 0, len(l.entries))
	for _, entry := range l.entries {
		out = append(out, entry)
	}
	l.mu.RUnlock()

	sort.Slice(out, func(i, j int) bool {
		ni := NormalizeRepoURL(out[i].RepoURL)
		nj := NormalizeRepoURL(out[j].RepoURL)
		if ni != nj {
			return ni < nj
		}
		return out[i].Branch < out[j].Branch
	})
	return out
}

// PlanCleanup returns the deterministically sorted list of registered intermediate branches
// to delete at Run completion/deletion, preserving all source and preserve branches.
func (l *Ledger) PlanCleanup(sources []SourceRef, preserves []PreserveRef) []BranchTarget {
	if l == nil {
		return nil
	}

	l.mu.RLock()
	protected := make([]SourceRef, len(l.protectedSources))
	copy(protected, l.protectedSources)
	entries := make([]BranchEntry, 0, len(l.entries))
	for _, entry := range l.entries {
		if entry.Registered {
			entries = append(entries, entry)
		}
	}
	l.mu.RUnlock()

	targets := make([]BranchTarget, 0, len(entries))
	for _, entry := range entries {
		normURL := NormalizeRepoURL(entry.RepoURL)
		keep := false
		for _, src := range protected {
			if refMatches(normURL, entry.Branch, src.URL, src.Branch) {
				keep = true
				break
			}
		}
		if !keep {
			for _, src := range sources {
				if refMatches(normURL, entry.Branch, src.URL, src.Branch) {
					keep = true
					break
				}
			}
		}
		if !keep {
			for _, p := range preserves {
				if refMatches(normURL, entry.Branch, p.URL, p.Branch) {
					keep = true
					break
				}
			}
		}
		if !keep {
			targets = append(targets, BranchTarget{
				RepoURL: entry.RepoURL,
				Branch:  entry.Branch,
			})
		}
	}

	sort.Slice(targets, func(i, j int) bool {
		ni := NormalizeRepoURL(targets[i].RepoURL)
		nj := NormalizeRepoURL(targets[j].RepoURL)
		if ni != nj {
			return ni < nj
		}
		return targets[i].Branch < targets[j].Branch
	})
	return targets
}

// Snapshot exports a JSON-serializable snapshot of protected sources and registered entries.
func (l *Ledger) Snapshot() LedgerSnapshot {
	if l == nil {
		return LedgerSnapshot{}
	}
	l.mu.RLock()
	protected := make([]SourceRef, len(l.protectedSources))
	copy(protected, l.protectedSources)
	l.mu.RUnlock()

	return LedgerSnapshot{
		ProtectedSources: protected,
		Entries:          l.Entries(),
	}
}

// Restore replaces the Ledger's state from a previously persisted LedgerSnapshot.
func (l *Ledger) Restore(snap LedgerSnapshot) error {
	if l == nil {
		return errors.New("ledger is nil")
	}
	nextEntries := make(map[ledgerKey]BranchEntry, len(snap.Entries))
	for _, entry := range snap.Entries {
		if strings.TrimSpace(entry.RepoURL) == "" {
			return errors.New("snapshot entry requires repo_url")
		}
		cleanBranch, err := NormalizeBranchName(entry.Branch)
		if err != nil {
			return fmt.Errorf("snapshot entry branch: %w", err)
		}
		entry.Branch = cleanBranch
		if strings.TrimSpace(entry.OwnerNode) == "" {
			return fmt.Errorf("snapshot entry %q requires owner_node", entry.Branch)
		}
		if !isValidBranchKind(entry.Kind) {
			return fmt.Errorf("snapshot entry %q has invalid kind %q", entry.Branch, entry.Kind)
		}
		if !IsValidCommitSHA(entry.BaseSHA) {
			return fmt.Errorf("%w: snapshot entry %q has %q", ErrInvalidBaseSHA, entry.Branch, entry.BaseSHA)
		}
		key := ledgerKey{
			repoURL: NormalizeRepoURL(entry.RepoURL),
			branch:  entry.Branch,
		}
		nextEntries[key] = entry
	}
	protected := make([]SourceRef, 0, len(snap.ProtectedSources))
	for _, src := range snap.ProtectedSources {
		branch := canonicalBranchForMatch(src.Branch)
		if branch == "" {
			continue
		}
		protected = append(protected, SourceRef{
			URL:    src.URL,
			Branch: branch,
		})
	}

	l.mu.Lock()
	l.protectedSources = protected
	l.entries = nextEntries
	l.mu.Unlock()
	return nil
}

func isValidBranchKind(kind BranchKind) bool {
	return kind == KindWorkBranch || kind == KindCheckpointBranch || kind == KindMergeBranch
}

func refMatches(normEntryURL, entryBranch, refURL, refBranch string) bool {
	if canonicalBranchForMatch(entryBranch) != canonicalBranchForMatch(refBranch) {
		return false
	}
	if strings.TrimSpace(refURL) == "" {
		return true
	}
	return NormalizeRepoURL(refURL) == normEntryURL
}
