package workloadagent

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/mo3789530/hako/internal/workloadsource"
)

var ErrSourceInstall = errors.New("verified Workload source archive could not be installed")

type sourceInstallRecord struct {
	MicroVMID        string `json:"microvm_id"`
	TenantID         string `json:"tenant_id"`
	WorkloadRunID    string `json:"workload_run_id"`
	AttemptID        string `json:"attempt_id"`
	Generation       int64  `json:"generation"`
	ResourcePlaneID  string `json:"resource_plane_id"`
	IdempotencyToken string `json:"idempotency_token"`
	Digest           string `json:"digest"`
	CommitSHA        string `json:"commit_sha"`
}

type FilesystemSourceInstaller struct {
	mu          sync.Mutex
	destination string
	maxBytes    int64
}

func NewFilesystemSourceInstaller(destination string, maxBytes int64) (*FilesystemSourceInstaller, error) {
	destination = strings.TrimSpace(destination)
	if destination == "" || maxBytes < 0 || maxBytes > workloadsource.DefaultMaxArchiveBytes {
		return nil, ErrProtocolRequest
	}
	absolute, err := filepath.Abs(destination)
	if err != nil || filepath.Clean(absolute) != absolute || absolute == string(filepath.Separator) {
		return nil, ErrProtocolRequest
	}
	if maxBytes == 0 {
		maxBytes = workloadsource.DefaultMaxArchiveBytes
	}
	return &FilesystemSourceInstaller{destination: absolute, maxBytes: maxBytes}, nil
}

func (s *FilesystemSourceInstaller) Install(ctx context.Context, claims TicketClaims, digest, commitSHA string, archive io.Reader, size int64) error {
	if s == nil || ctx == nil || claims.Operation != OperationSource || !claimsMatchExecution(claims, digest, commitSHA) || archive == nil || size < 1 || size > s.maxBytes {
		return ErrSourceInstall
	}
	if !validIdentity(claims.Identity.Command()) || claims.MicroVMID == "" {
		return ErrSourceInstall
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.Ready(ctx, claims, digest, commitSHA) {
		return nil
	}
	statePath := s.destination + ".hako-state"
	if _, err := os.Lstat(s.destination); err == nil {
		return ErrAttemptConflict
	} else if !os.IsNotExist(err) {
		return ErrSourceInstall
	}
	if _, err := os.Lstat(statePath); err == nil {
		record, readErr := readSourceInstallRecord(statePath)
		if readErr != nil || record != sourceRecord(claims, digest, commitSHA) {
			return ErrAttemptConflict
		}
	} else if !os.IsNotExist(err) {
		return ErrSourceInstall
	}
	parent := filepath.Dir(s.destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return ErrSourceInstall
	}
	parentInfo, err := os.Lstat(parent)
	if err != nil || !parentInfo.IsDir() || parentInfo.Mode()&os.ModeSymlink != 0 {
		return ErrSourceInstall
	}
	temporary, err := os.MkdirTemp(parent, ".hako-source-install-")
	if err != nil {
		return ErrSourceInstall
	}
	defer os.RemoveAll(temporary)
	contentPath := filepath.Join(temporary, "content")
	if err := workloadsource.ExtractArchive(ctx, archive, size, digest, contentPath, s.maxBytes); err != nil {
		return ErrSourceInstall
	}
	if _, err := os.Lstat(s.destination); err == nil || !os.IsNotExist(err) {
		return ErrAttemptConflict
	}
	if err := os.Rename(contentPath, s.destination); err != nil {
		return ErrSourceInstall
	}
	if err := writeSourceInstallRecord(statePath, sourceRecord(claims, digest, commitSHA)); err != nil {
		_ = os.RemoveAll(s.destination)
		return ErrSourceInstall
	}
	return nil
}

func (s *FilesystemSourceInstaller) Ready(ctx context.Context, claims TicketClaims, digest, commitSHA string) bool {
	if s == nil || ctx == nil || ctx.Err() != nil || !claimsMatchExecution(claims, digest, commitSHA) || !validIdentity(claims.Identity.Command()) || claims.MicroVMID == "" {
		return false
	}
	record, err := readSourceInstallRecord(s.destination + ".hako-state")
	if err != nil || record != sourceRecord(claims, digest, commitSHA) {
		return false
	}
	info, err := os.Lstat(s.destination)
	return err == nil && info.IsDir() && info.Mode()&os.ModeSymlink == 0
}

func claimsMatchExecution(claims TicketClaims, digest, commitSHA string) bool {
	return claims.Execution != nil && validExecutionBinding(*claims.Execution) && claims.Execution.SourceDigest == digest && claims.Execution.CommitSHA == commitSHA &&
		protocolDigest.MatchString(digest) && protocolCommit.MatchString(commitSHA) && !claims.Execution.Deadline.IsZero()
}

func sourceRecord(claims TicketClaims, digest, commitSHA string) sourceInstallRecord {
	identity := claims.Identity
	return sourceInstallRecord{MicroVMID: claims.MicroVMID, TenantID: identity.TenantID, WorkloadRunID: identity.WorkloadRunID,
		AttemptID: identity.AttemptID, Generation: identity.Generation, ResourcePlaneID: identity.ResourcePlaneID,
		IdempotencyToken: identity.IdempotencyToken, Digest: digest, CommitSHA: commitSHA}
}

func verifyInstalledExecutionSource(ctx context.Context, plan ExecutionPlan, maxBytes int64) error {
	identity := plan.Identity
	record, err := readSourceInstallRecord(plan.RepositoryPath + ".hako-state")
	if err != nil || record.TenantID != identity.TenantID || record.WorkloadRunID != identity.WorkloadRunID ||
		record.AttemptID != identity.AttemptID || record.Generation != identity.Generation || record.ResourcePlaneID != identity.ResourcePlaneID ||
		record.IdempotencyToken != identity.IdempotencyToken ||
		record.Digest != plan.SourceDigest || record.CommitSHA != plan.CommitSHA || !protocolID.MatchString(record.MicroVMID) {
		return ErrExecutionPlan
	}
	archive, err := workloadsource.CreateArchive(ctx, plan.RepositoryPath, maxBytes)
	if err != nil {
		return ErrExecutionPlan
	}
	defer archive.Close()
	if archive.SHA256() != plan.SourceDigest {
		return ErrExecutionPlan
	}
	return nil
}

func readSourceInstallRecord(path string) (sourceInstallRecord, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > 4096 {
		return sourceInstallRecord{}, ErrSourceInstall
	}
	file, err := os.Open(path)
	if err != nil {
		return sourceInstallRecord{}, ErrSourceInstall
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 4097))
	decoder.DisallowUnknownFields()
	var record sourceInstallRecord
	if decoder.Decode(&record) != nil || decoder.Decode(new(any)) != io.EOF {
		return sourceInstallRecord{}, ErrSourceInstall
	}
	return record, nil
}

func writeSourceInstallRecord(path string, record sourceInstallRecord) error {
	data, err := json.Marshal(record)
	if err != nil || len(data) > 4096 {
		return ErrSourceInstall
	}
	file, err := os.CreateTemp(filepath.Dir(path), ".hako-source-state-")
	if err != nil {
		return ErrSourceInstall
	}
	temporary := file.Name()
	defer os.Remove(temporary)
	if file.Chmod(0o600) != nil {
		_ = file.Close()
		return ErrSourceInstall
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		return ErrSourceInstall
	}
	if file.Sync() != nil {
		_ = file.Close()
		return ErrSourceInstall
	}
	if file.Close() != nil {
		return ErrSourceInstall
	}
	if err := os.Rename(temporary, path); err != nil {
		return ErrSourceInstall
	}
	return nil
}

var _ SourceInstaller = (*FilesystemSourceInstaller)(nil)
