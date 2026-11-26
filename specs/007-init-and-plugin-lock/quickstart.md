# Quickstart: Workspace Init and Plugin Locking

```go
err := project.Init(ctx, "./workflow", "./.sofa", project.InitOptions{
	Upgrade: false,
})
if err != nil {
	return err
}

proj, err := project.OpenProject("./.sofa")
if err != nil {
	return err
}

lockFile, err := harness.ReadLockFile(proj.StateDirectory)
if err != nil {
	return err
}
_ = lockFile.Plugins
```
