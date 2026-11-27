output "work_branch" {
  description = "Managed Git branch containing the approved slice implementation."
  value       = worker.implement_slice.work_branch
}

output "commit_sha" {
  description = "Final Git commit SHA produced by worker.implement_slice."
  value       = worker.implement_slice.commit_sha
}
