# Quickstart Validation: Source Repositories and Preserved Branches

## Run focused tests

```powershell
go test ./internal/spec ./internal/source ./internal/planner -count=1
```

## Run all gates

```powershell
gofmt -l internal
go vet ./...
go test ./... -count=1
```

## Single-source inference

```hcl
source "project" {
  url    = "https://github.com/example/repo.git"
  branch = "main"
}

preserve "delivery" {
  branch = "release/v1"
}
```

Expected:

- plan succeeds;
- preserve URL equals the source URL;
- preserve depends on `source.project`.

## Deferred branch

```hcl
source "project" {
  url    = "git@github.com:example/repo.git"
  branch = "main"
}

output "branch" {
  value = source.project.commit_sha
}

preserve "delivery" {
  branch = output.branch
}
```

Expected: planning succeeds because the branch is an unknown string, and preserve depends on the output and source.

## Negative matrix

- Embedded HTTPS password → error directing the author to `token_ref`.
- Invalid branch → error naming the branch rule.
- Both credential references → mutual-exclusion error.
- Missing URL with no source → error naming preserve.
- Missing URL with multiple sources → ambiguity error listing source names.
