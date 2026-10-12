// Package workloadagent defines the private, versioned protocol used by the
// Resource Plane controller to operate one Lambda MicroVM Workload attempt.
package workloadagent

import (
	"time"

	"github.com/mo3789530/hako/internal/workloadjobs"
	"github.com/mo3789530/hako/internal/workloadjournal"
)

const ProtocolVersion = "1"

type Identity struct {
	TenantID         string `json:"tenant_id"`
	WorkloadRunID    string `json:"workload_run_id"`
	AttemptID        string `json:"attempt_id"`
	Generation       int64  `json:"generation"`
	ResourcePlaneID  string `json:"resource_plane_id"`
	IdempotencyToken string `json:"idempotency_token"`
}

func IdentityFromCommand(id workloadjournal.Command) Identity {
	return Identity{TenantID: id.TenantID, WorkloadRunID: id.WorkloadRunID, AttemptID: id.AttemptID,
		Generation: id.Generation, ResourcePlaneID: id.ResourcePlaneID, IdempotencyToken: id.IdempotencyToken}
}

func (i Identity) Command() workloadjournal.Command {
	return workloadjournal.Command{TenantID: i.TenantID, WorkloadRunID: i.WorkloadRunID, AttemptID: i.AttemptID,
		Generation: i.Generation, ResourcePlaneID: i.ResourcePlaneID, IdempotencyToken: i.IdempotencyToken}
}

type StartRequest struct {
	Identity     Identity  `json:"identity"`
	SourceDigest string    `json:"source_digest"`
	CommitSHA    string    `json:"commit_sha"`
	Deadline     time.Time `json:"deadline"`
}

type StatusResponse struct {
	Identity Identity                       `json:"identity"`
	State    workloadjournal.ExecutionState `json:"state"`
}

type ResultResponse struct {
	Identity Identity                    `json:"identity"`
	Result   workloadjobs.WorkloadResult `json:"result"`
}
