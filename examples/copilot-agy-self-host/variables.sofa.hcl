variable "repo_url" {
  type        = string
  description = "Git repository URL for sofa self-hosting."

  validation {
    condition     = length(trimspace(var.repo_url)) > 0
    error_message = "repo_url must not be empty."
  }
}

variable "base_branch" {
  type        = string
  description = "Base Git branch to branch the worker from."
  default     = "main"

  validation {
    condition     = length(trimspace(var.base_branch)) > 0
    error_message = "base_branch must not be empty."
  }
}

variable "sandbox_image" {
  type        = string
  description = "Container image used by both Go build/test and read-only QC audit containers."
  default     = "golang:1.27"

  validation {
    condition     = length(trimspace(var.sandbox_image)) > 0
    error_message = "sandbox_image must not be empty."
  }
}

variable "work_model" {
  type        = string
  description = "Model used by the Copilot implementation worker."
  default     = "claude-sonnet-4.5"

  validation {
    condition     = length(trimspace(var.work_model)) > 0
    error_message = "work_model must not be empty."
  }
}

variable "qc_model" {
  type        = string
  description = "Model used by the independent Antigravity read-only QC auditor."
  default     = "gemini-2.5-pro"

  validation {
    condition     = length(trimspace(var.qc_model)) > 0
    error_message = "qc_model must not be empty."
  }
}

variable "slice_prompt" {
  type        = string
  description = "Task prompt describing the slice or feature to implement."

  validation {
    condition     = length(trimspace(var.slice_prompt)) > 0
    error_message = "slice_prompt must not be empty."
  }
}

variable "writable_paths" {
  type        = list(string)
  description = "Workspace-relative glob patterns writable by the implementation worker."
  default     = ["internal/**", "cmd/**", "specs/**", "examples/**"]

  validation {
    condition     = length(var.writable_paths) > 0
    error_message = "writable_paths must contain at least one path pattern."
  }
}

variable "human_approval" {
  type        = string
  description = "Human approval gate policy after gatekeeper and QC pass (required or optional)."
  default     = "required"

  validation {
    condition     = contains(["required", "optional"], var.human_approval)
    error_message = "human_approval must be \"required\" or \"optional\"."
  }
}
