variable "repo_url" {
  type        = string
  description = "Git repository URL or local Git directory path for sofa self-hosting."

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

variable "git_token_ref" {
  type        = string
  description = "Optional managed secret name for Git HTTPS auth (set null for local Git repos)."
  default     = "GITHUB_TOKEN"
}

variable "copilot_github_token_ref" {
  type        = string
  description = "Optional managed secret name for GitHub Copilot auth (leave null when using custom BYOK model_provider URL + API key)."
  default     = null
}

variable "copilot_proxy_ref" {
  type        = string
  description = "Optional managed env reference name for HTTP/HTTPS proxy URL injected into Copilot CLI subprocesses."
  default     = null
}

variable "go_mod_cache_dir" {
  type        = string
  description = "Optional host GOMODCACHE directory mounted read-only at /go/pkg/mod inside Go sandboxes."
  default     = null
}

variable "work_sandbox_image" {
  type        = string
  description = "Container image used by the Copilot implementation worker sandbox."
  default     = "golang:1.27"

  validation {
    condition     = length(trimspace(var.work_sandbox_image)) > 0
    error_message = "work_sandbox_image must not be empty."
  }
}

variable "qc_sandbox_image" {
  type        = string
  description = "Container image used by the independent read-only Copilot QC sandbox."
  default     = "golang:1.27"

  validation {
    condition     = length(trimspace(var.qc_sandbox_image)) > 0
    error_message = "qc_sandbox_image must not be empty."
  }
}

# ---------------------------------------------------------------------------
# Worker Session Model Provider & Model
# ---------------------------------------------------------------------------

variable "work_provider_type" {
  type        = string
  description = "Provider protocol type for the worker session (openai, anthropic, or azure)."
  default     = "openai"

  validation {
    condition     = contains(["openai", "anthropic", "azure"], var.work_provider_type)
    error_message = "work_provider_type must be \"openai\", \"anthropic\", or \"azure\"."
  }
}

variable "work_api_url" {
  type        = string
  description = "API root endpoint URL for the worker model provider (without /chat/completions or /responses)."
  default     = "https://api.openai.com/v1"

  validation {
    condition     = length(trimspace(var.work_api_url)) > 0
    error_message = "work_api_url must not be empty."
  }
}

variable "work_wire_api" {
  type        = string
  description = "Optional OpenAI/Azure wire API for worker (completions or responses; must be null for anthropic)."
  default     = null
}

variable "work_api_key" {
  type        = string
  description = "Direct API key for the worker model provider (mutually exclusive with work_api_key_ref)."
  sensitive   = true
  default     = null
}

variable "work_api_key_ref" {
  type        = string
  description = "Optional managed secret reference name for the worker model provider API key."
  default     = null
}

variable "work_model" {
  type        = string
  description = "Model identifier used by the Copilot implementation worker."
  default     = "claude-sonnet-4.5"

  validation {
    condition     = length(trimspace(var.work_model)) > 0
    error_message = "work_model must not be empty."
  }
}

variable "work_reasoning_effort" {
  type        = string
  description = "Optional reasoning effort for the worker model (low, medium, high, xhigh)."
  default     = null
}

# ---------------------------------------------------------------------------
# QC Session Model Provider & Model
# ---------------------------------------------------------------------------

variable "qc_provider_type" {
  type        = string
  description = "Provider protocol type for the QC session (openai, anthropic, or azure)."
  default     = "openai"

  validation {
    condition     = contains(["openai", "anthropic", "azure"], var.qc_provider_type)
    error_message = "qc_provider_type must be \"openai\", \"anthropic\", or \"azure\"."
  }
}

variable "qc_api_url" {
  type        = string
  description = "API root endpoint URL for the QC model provider (without /chat/completions or /responses)."
  default     = "https://api.openai.com/v1"

  validation {
    condition     = length(trimspace(var.qc_api_url)) > 0
    error_message = "qc_api_url must not be empty."
  }
}

variable "qc_wire_api" {
  type        = string
  description = "Optional OpenAI/Azure wire API for QC (completions or responses; must be null for anthropic)."
  default     = null
}

variable "qc_api_key" {
  type        = string
  description = "Direct API key for the QC model provider (mutually exclusive with qc_api_key_ref)."
  sensitive   = true
  default     = null
}

variable "qc_api_key_ref" {
  type        = string
  description = "Optional managed secret reference name for the QC model provider API key."
  default     = null
}

variable "qc_model" {
  type        = string
  description = "Model identifier used by the independent read-only Copilot QC auditor."
  default     = "gpt-5.4"

  validation {
    condition     = length(trimspace(var.qc_model)) > 0
    error_message = "qc_model must not be empty."
  }
}

variable "qc_reasoning_effort" {
  type        = string
  description = "Optional reasoning effort for the QC model (low, medium, high, xhigh)."
  default     = null
}

# ---------------------------------------------------------------------------
# Task & Governance Controls
# ---------------------------------------------------------------------------

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
  default     = ["internal/**", "cmd/**", "specs/**", "examples/**", "docs/**", "scripts/**"]

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
