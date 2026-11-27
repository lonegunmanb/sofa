package registry

import (
	"context"
	"time"
)

const (
	// ActorType is the Dapr Actor type name for the worker process registry.
	ActorType = "ProcessRegistryActor"
	// GlobalActorID is the singleton actor ID for ProcessRegistryActor.
	GlobalActorID = "global"

	// DefaultWorkerMaxConcurrency is the default concurrent slot capacity for standalone sofa worker processes.
	DefaultWorkerMaxConcurrency = 10
	// DefaultDevMaxConcurrency is the default concurrent slot capacity for co-located sofa server --dev mode.
	DefaultDevMaxConcurrency = 2

	// DefaultHeartbeatInterval is the default worker heartbeat interval.
	DefaultHeartbeatInterval = 5 * time.Second
	// DefaultSuspectTimeout is the silence duration before a HEALTHY worker becomes SUSPECT.
	DefaultSuspectTimeout = 15 * time.Second
	// DefaultProbeInterval is the periodic reminder interval for probing SUSPECT workers.
	DefaultProbeInterval = 3 * time.Second
	// DefaultDeadThreshold is the number of consecutive failed pings before SUSPECT becomes DEAD.
	DefaultDeadThreshold = 3
	// DefaultReminderName is the persistent Dapr reminder name driving liveness sweeps.
	DefaultReminderName = "process-liveness-probe"
)

// ProcessStatus represents the three-state liveness lifecycle of a registered worker process.
type ProcessStatus string

const (
	StatusHealthy ProcessStatus = "HEALTHY"
	StatusSuspect ProcessStatus = "SUSPECT"
	StatusDead    ProcessStatus = "DEAD"
)

// ProcessRecord captures a worker process's capacity, health state, and slot usage.
type ProcessRecord struct {
	ProcessID           string        `json:"process_id"`
	AppID               string        `json:"app_id"`
	MaxConcurrency      int           `json:"max_concurrency"`
	ActiveSlots         int           `json:"active_slots"`
	Arch                string        `json:"arch"`
	Status              ProcessStatus `json:"status"`
	RegisteredAt        time.Time     `json:"registered_at"`
	LastHeartbeatAt     time.Time     `json:"last_heartbeat_at"`
	ConsecutiveFailures int           `json:"consecutive_failures"`
	LastProbeError      string        `json:"last_probe_error,omitempty"`
}

// SlotLease tracks an allocated execution slot leased to a NodeActor or MergeActor.
type SlotLease struct {
	LeaseID        string    `json:"lease_id"`
	LeaseKey       string    `json:"lease_key"`
	ProcessID      string    `json:"process_id"`
	AppID          string    `json:"app_id"`
	OwnerActorType string    `json:"owner_actor_type"`
	OwnerActorID   string    `json:"owner_actor_id"`
	AcquiredAt     time.Time `json:"acquired_at"`
}

// WorkerDeadNotification is sent to each affected lease owner's OnWorkerDead Actor method.
type WorkerDeadNotification struct {
	LeaseID   string `json:"lease_id"`
	LeaseKey  string `json:"lease_key"`
	ProcessID string `json:"process_id"`
	AppID     string `json:"app_id"`
	Reason    string `json:"reason,omitempty"`
}

// PingRequest is the Dapr Service Invocation payload sent to a worker's "ping" endpoint.
type PingRequest struct {
	ProcessID string `json:"process_id,omitempty"`
}

// PingResponse is returned by a worker's "ping" endpoint.
type PingResponse struct {
	OK          bool   `json:"ok"`
	ProcessID   string `json:"process_id"`
	AppID       string `json:"app_id,omitempty"`
	ActiveTasks int    `json:"active_tasks"`
}

// RegisterProcessReq registers or refreshes a worker process with its capacity and target platform.
type RegisterProcessReq struct {
	ProcessID      string `json:"process_id"`
	AppID          string `json:"app_id"`
	MaxConcurrency int    `json:"max_concurrency,omitempty"`
	Arch           string `json:"arch,omitempty"`
}

// RegisterProcessResp confirms process registration and health status.
type RegisterProcessResp struct {
	Registered          bool          `json:"registered"`
	Status              ProcessStatus `json:"status"`
	HeartbeatIntervalMs int64         `json:"heartbeat_interval_ms,omitempty"`
}

// HeartbeatReq reports liveness from a registered worker process.
type HeartbeatReq struct {
	ProcessID   string `json:"process_id"`
	ActiveTasks int    `json:"active_tasks,omitempty"`
}

// HeartbeatResp returns the worker's authoritative health status and whether it must self-fence.
type HeartbeatResp struct {
	Status        ProcessStatus `json:"status"`
	FenceRequired bool          `json:"fence_required"`
}

// ReportSuspectReq marks a healthy worker as SUSPECT immediately after a dispatch failure.
type ReportSuspectReq struct {
	ProcessID string `json:"process_id"`
	Reason    string `json:"reason,omitempty"`
}

// ReportSuspectResp reports the resulting process status.
type ReportSuspectResp struct {
	Status ProcessStatus `json:"status"`
}

// ClaimSlotReq requests an idempotent least-load execution slot for LeaseKey.
type ClaimSlotReq struct {
	LeaseKey       string `json:"lease_key"`
	OwnerActorType string `json:"owner_actor_type"`
	OwnerActorID   string `json:"owner_actor_id"`
	RequiredArch   string `json:"required_arch,omitempty"`
}

// ClaimSlotResp describes whether a slot was granted and which worker owns it.
type ClaimSlotResp struct {
	Granted   bool   `json:"granted"`
	LeaseID   string `json:"lease_id,omitempty"`
	LeaseKey  string `json:"lease_key,omitempty"`
	ProcessID string `json:"process_id,omitempty"`
	AppID     string `json:"app_id,omitempty"`
	Reason    string `json:"reason,omitempty"`
}

// ReleaseSlotReq idempotently releases a previously claimed slot by LeaseID or LeaseKey.
type ReleaseSlotReq struct {
	LeaseID  string `json:"lease_id,omitempty"`
	LeaseKey string `json:"lease_key,omitempty"`
}

// ReleaseSlotResp reports whether a matching active lease was found and released.
type ReleaseSlotResp struct {
	Released bool `json:"released"`
}

// ListProcessesReq requests a snapshot of all registered workers and active leases.
type ListProcessesReq struct{}

// ListProcessesResp returns sorted snapshots of registered workers and active slot leases.
type ListProcessesResp struct {
	Processes []ProcessRecord `json:"processes"`
	Leases    []SlotLease     `json:"leases,omitempty"`
}

// Prober checks whether the worker identified by (appID, processID) is still alive.
type Prober interface {
	Ping(ctx context.Context, appID, processID string) error
}

// DeadNotifier notifies a lease owner actor that its worker process has transitioned to DEAD.
type DeadNotifier interface {
	NotifyWorkerDead(ctx context.Context, ownerActorType, ownerActorID string, notice WorkerDeadNotification) error
}

// ReminderScheduler registers the persistent liveness sweep reminder on ProcessRegistryActor.
type ReminderScheduler interface {
	EnsureProbeReminder(ctx context.Context, actorType, actorID, reminderName string, dueTime, period time.Duration) error
}
