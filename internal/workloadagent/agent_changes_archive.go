package workloadagent

import "github.com/mo3789530/hako/internal/workloadsource"

var ErrInvalidAgentChangeArchive = workloadsource.ErrProposalArchive

type AgentChangeArchiveFile = workloadsource.AgentProposalFile
type AgentChangeArchiveSummary = workloadsource.AgentProposalInspection

// InspectAgentChangeArchive validates an untrusted Agent ZIP without
// extracting or applying it. The result intentionally contains paths and
// sizes only, never proposal contents.
func InspectAgentChangeArchive(data []byte) (AgentChangeArchiveSummary, error) {
	return workloadsource.InspectAgentProposalArchive(data)
}
