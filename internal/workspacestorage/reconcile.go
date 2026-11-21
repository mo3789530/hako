package workspacestorage

import (
	"context"
	"errors"
	"sort"
)

// WorkspaceBinding is the durable identity/binding pair read from Resource
// Plane journals. Callers should include every journal for the configured
// filesystem, including terminal tombstones until AP cleanup is confirmed.
type WorkspaceBinding struct {
	Identity Identity `json:"identity"`
	Binding  Binding  `json:"binding"`
}

type ReconciliationStatus string

const (
	ReconciliationReferenced       ReconciliationStatus = "referenced"
	ReconciliationUnboundCandidate ReconciliationStatus = "unbound_candidate"
	ReconciliationStaleCandidate   ReconciliationStatus = "stale_candidate"
	ReconciliationDuplicate        ReconciliationStatus = "duplicate_identity"
	ReconciliationIdentityMismatch ReconciliationStatus = "identity_mismatch"
	ReconciliationMissing          ReconciliationStatus = "missing_access_point"
)

// ReconciliationEntry is an operator-facing finding. Candidate statuses are
// advisory only: this package never adopts or deletes an Access Point.
type ReconciliationEntry struct {
	Identity Identity             `json:"identity"`
	Binding  Binding              `json:"binding"`
	Status   ReconciliationStatus `json:"status"`
}

// Reconcile compares validated, durable Workspace journal bindings with the
// managed Access Points on this EFS. It is intentionally read-only. In
// particular, an Access Point created immediately before a journal write may
// appear unbound; operators must check in-flight operations and retention
// policy before acting on any candidate.
func (e *EFS) Reconcile(ctx context.Context, bindings []WorkspaceBinding) ([]ReconciliationEntry, error) {
	if e == nil || ctx == nil || ctx.Err() != nil {
		return nil, errors.New("Workspace EFS reconciliation context is invalid")
	}
	byIdentity := make(map[Identity]Binding, len(bindings))
	byAccessPoint := make(map[string]Identity, len(bindings))
	for _, item := range bindings {
		if err := item.Binding.ValidateFor(item.Identity); err != nil ||
			item.Binding.FileSystemID != e.fileSystemID || item.Binding.Region != e.region {
			return nil, errors.New("Workspace journal contains an invalid EFS binding")
		}
		if _, exists := byIdentity[item.Identity]; exists {
			return nil, errors.New("Workspace journal contains duplicate EFS identities")
		}
		if identity, exists := byAccessPoint[item.Binding.AccessPointID]; exists && identity != item.Identity {
			return nil, errors.New("Workspace journal maps one EFS Access Point to multiple identities")
		}
		byIdentity[item.Identity] = item.Binding
		byAccessPoint[item.Binding.AccessPointID] = item.Identity
	}

	managed, err := e.DiscoverManaged(ctx)
	if err != nil {
		return nil, err
	}
	counts := make(map[Identity]int, len(managed))
	for _, point := range managed {
		counts[point.Identity]++
	}
	seenAccessPoints := make(map[string]Identity, len(managed))
	entries := make([]ReconciliationEntry, 0, len(managed)+len(bindings))
	for _, point := range managed {
		status := ReconciliationUnboundCandidate
		journalBinding, hasBinding := byIdentity[point.Identity]
		journalIdentity, accessPointReferenced := byAccessPoint[point.Binding.AccessPointID]
		switch {
		case counts[point.Identity] > 1:
			status = ReconciliationDuplicate
		case accessPointReferenced && journalIdentity != point.Identity:
			status = ReconciliationIdentityMismatch
		case hasBinding:
			if sameAccessPointBinding(journalBinding, point.Binding) {
				status = ReconciliationReferenced
			} else {
				status = ReconciliationStaleCandidate
			}
		}
		entries = append(entries, ReconciliationEntry{Identity: point.Identity, Binding: point.Binding, Status: status})
		seenAccessPoints[point.Binding.AccessPointID] = point.Identity
	}
	for _, item := range bindings {
		if identity, found := seenAccessPoints[item.Binding.AccessPointID]; found && identity == item.Identity {
			continue
		}
		entries = append(entries, ReconciliationEntry{Identity: item.Identity, Binding: item.Binding, Status: ReconciliationMissing})
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].Identity.ResourcePlaneID != entries[j].Identity.ResourcePlaneID {
			return entries[i].Identity.ResourcePlaneID < entries[j].Identity.ResourcePlaneID
		}
		if entries[i].Identity.TenantID != entries[j].Identity.TenantID {
			return entries[i].Identity.TenantID < entries[j].Identity.TenantID
		}
		if entries[i].Identity.WorkspaceID != entries[j].Identity.WorkspaceID {
			return entries[i].Identity.WorkspaceID < entries[j].Identity.WorkspaceID
		}
		if entries[i].Binding.AccessPointID != entries[j].Binding.AccessPointID {
			return entries[i].Binding.AccessPointID < entries[j].Binding.AccessPointID
		}
		return entries[i].Status < entries[j].Status
	})
	return entries, nil
}

func sameAccessPointBinding(a, b Binding) bool {
	return a.FileSystemID == b.FileSystemID && a.Region == b.Region &&
		a.AccessPointID == b.AccessPointID && a.RootPath == b.RootPath && a.UID == b.UID && a.GID == b.GID
}
