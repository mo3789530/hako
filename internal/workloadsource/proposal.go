package workloadsource

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"
)

const (
	maxProposalArchiveBytes = 8 << 20
	maxProposalFiles        = 128
	maxProposalFileBytes    = 128 << 10
)

var ErrProposalArchive = errors.New("Agent proposal archive is invalid")

// ProposalSourceArchive is a verified source snapshot formed by applying a
// bounded Agent proposal to its immutable base archive. Its digest can be used
// as the source identity for a separate verifier Workload.
type ProposalSourceArchive struct {
	Archive          *Archive
	ProposalSHA256   string
	BaseSourceSHA256 string
}

type AgentProposalFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

// AgentProposalContent is one validated bounded regular file from an Agent
// proposal archive. Call ClearAgentProposalContents when the publisher is
// done using these in-memory bytes.
type AgentProposalContent struct {
	Path    string
	Content []byte
}

type AgentProposalInspection struct {
	SHA256 string              `json:"sha256"`
	Files  []AgentProposalFile `json:"files"`
}

func (a *ProposalSourceArchive) Close() error {
	if a == nil || a.Archive == nil {
		return nil
	}
	return a.Archive.Close()
}

// InspectAgentProposalArchive validates the complete bounded ZIP without
// extracting it and returns metadata only.
func InspectAgentProposalArchive(data []byte) (AgentProposalInspection, error) {
	if len(data) < 1 || len(data) > maxProposalArchiveBytes {
		return AgentProposalInspection{}, ErrProposalArchive
	}
	entries, err := readAgentProposalEntries(data)
	if err != nil {
		return AgentProposalInspection{}, ErrProposalArchive
	}
	defer clearAgentProposalEntries(entries)
	files := make([]AgentProposalFile, 0, len(entries))
	for _, entry := range entries {
		files = append(files, AgentProposalFile{Path: entry.path, SizeBytes: int64(len(entry.content))})
	}
	digest := sha256.Sum256(data)
	return AgentProposalInspection{SHA256: hex.EncodeToString(digest[:]), Files: files}, nil
}

// ReadAgentProposalContents returns validated file bytes without extracting
// archive paths to disk. It uses the same path, type, secret, duplicate, and
// size checks as InspectAgentProposalArchive.
func ReadAgentProposalContents(data []byte) ([]AgentProposalContent, error) {
	entries, err := readAgentProposalEntries(data)
	if err != nil {
		return nil, ErrProposalArchive
	}
	contents := make([]AgentProposalContent, 0, len(entries))
	for _, entry := range entries {
		contents = append(contents, AgentProposalContent{Path: entry.path, Content: entry.content})
	}
	return contents, nil
}

func ClearAgentProposalContents(contents []AgentProposalContent) {
	for i := range contents {
		clear(contents[i].Content)
		contents[i].Content = nil
	}
}

// ApplyAgentProposalArchive verifies and extracts a base source archive,
// validates the untrusted proposal ZIP, atomically applies its regular files,
// and produces a deterministic source archive. It does not run code or write
// to GitHub. The result can be staged as a normal isolated Hako Job source.
func ApplyAgentProposalArchive(ctx context.Context, base io.Reader, baseSize int64, baseSHA256 string,
	proposal []byte, maxBaseBytes int64) (*ProposalSourceArchive, error) {
	if ctx == nil || base == nil || baseSize < 1 || !sourceDigestPattern.MatchString(baseSHA256) ||
		len(proposal) < 1 || len(proposal) > maxProposalArchiveBytes {
		return nil, ErrProposalArchive
	}
	if maxBaseBytes <= 0 || maxBaseBytes > DefaultMaxArchiveBytes {
		maxBaseBytes = DefaultMaxArchiveBytes
	}
	if baseSize > maxBaseBytes {
		return nil, ErrProposalArchive
	}
	proposalDigest := sha256.Sum256(proposal)
	entries, err := readAgentProposalEntries(proposal)
	if err != nil {
		return nil, ErrProposalArchive
	}
	defer clearAgentProposalEntries(entries)
	root, err := os.MkdirTemp("", "hako-proposal-source-")
	if err != nil {
		return nil, ErrProposalArchive
	}
	defer os.RemoveAll(root)
	checkout := filepath.Join(root, "checkout")
	if err := ExtractArchive(ctx, base, baseSize, baseSHA256, checkout, maxBaseBytes); err != nil {
		return nil, ErrProposalArchive
	}
	for _, entry := range entries {
		if err := ctx.Err(); err != nil || writeProposalFile(checkout, entry.path, entry.content) != nil {
			return nil, ErrProposalArchive
		}
	}
	archive, err := CreateArchive(ctx, checkout, maxBaseBytes)
	if err != nil {
		return nil, ErrProposalArchive
	}
	return &ProposalSourceArchive{Archive: archive, ProposalSHA256: hex.EncodeToString(proposalDigest[:]), BaseSourceSHA256: baseSHA256}, nil
}

type agentProposalEntry struct {
	path    string
	content []byte
}

func readAgentProposalEntries(data []byte) ([]agentProposalEntry, error) {
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > maxProposalFiles {
		return nil, ErrProposalArchive
	}
	seen := make(map[string]struct{}, len(reader.File))
	entries := make([]agentProposalEntry, 0, len(reader.File))
	keepEntries := false
	defer func() {
		if !keepEntries {
			clearAgentProposalEntries(entries)
		}
	}()
	var total int64
	for _, file := range reader.File {
		name := file.Name
		if !validProposalPath(name) || file.FileInfo().IsDir() || !file.Mode().IsRegular() || file.Flags&1 != 0 ||
			file.UncompressedSize64 > maxProposalFileBytes || protectedProposalPath(name) {
			return nil, ErrProposalArchive
		}
		if _, exists := seen[name]; exists {
			return nil, ErrProposalArchive
		}
		seen[name] = struct{}{}
		total += int64(file.UncompressedSize64)
		if total > maxProposalArchiveBytes {
			return nil, ErrProposalArchive
		}
		input, err := file.Open()
		if err != nil {
			return nil, ErrProposalArchive
		}
		content, readErr := io.ReadAll(io.LimitReader(input, maxProposalFileBytes+1))
		closeErr := input.Close()
		if readErr != nil || closeErr != nil || int64(len(content)) != int64(file.UncompressedSize64) ||
			len(content) > maxProposalFileBytes || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 || containsProposalCredential(content) {
			clear(content)
			return nil, ErrProposalArchive
		}
		entries = append(entries, agentProposalEntry{path: name, content: content})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].path < entries[j].path })
	keepEntries = true
	return entries, nil
}

func clearAgentProposalEntries(entries []agentProposalEntry) {
	for i := range entries {
		clear(entries[i].content)
	}
}

func validProposalPath(value string) bool {
	if value == "" || len(value) > 512 || !utf8.ValidString(value) || strings.ContainsAny(value, "\\\r\n\x00:") ||
		strings.HasPrefix(value, "/") || path.Clean(value) != value {
		return false
	}
	return validArchivePath(value)
}

func protectedProposalPath(value string) bool {
	parts := strings.Split(filepath.ToSlash(value), "/")
	for _, part := range parts {
		lower := strings.ToLower(part)
		if lower == ".git" || lower == ".hako" || lower == ".github" || lower == ".aws" || lower == ".ssh" ||
			lower == "terraform" || lower == "infra" {
			return true
		}
	}
	base := strings.ToLower(filepath.Base(filepath.FromSlash(value)))
	return strings.HasPrefix(base, ".env") || base == "credentials" || base == "id_rsa" || base == "id_ed25519" ||
		strings.HasSuffix(base, ".pem") || strings.HasSuffix(base, ".key") || strings.HasSuffix(base, ".p12") || strings.HasSuffix(base, ".pfx")
}

func containsProposalCredential(content []byte) bool {
	lower := bytes.ToLower(content)
	for _, marker := range [][]byte{
		[]byte("-----begin private key-----"), []byte("-----begin rsa private key-----"),
		[]byte("-----begin ec private key-----"), []byte("-----begin openssh private key-----"),
		[]byte("sk-ant-api"), []byte("akia"),
	} {
		if bytes.Contains(lower, marker) {
			return true
		}
	}
	return false
}

func writeProposalFile(root, relative string, content []byte) error {
	if root == "" || !validProposalPath(relative) || protectedProposalPath(relative) || containsProposalCredential(content) {
		return ErrProposalArchive
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return ErrProposalArchive
	}
	parts := strings.Split(relative, "/")
	current := root
	for _, part := range parts[:len(parts)-1] {
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if os.IsNotExist(err) {
			if err := os.Mkdir(current, 0o755); err != nil {
				return ErrProposalArchive
			}
			continue
		}
		if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return ErrProposalArchive
		}
	}
	target := filepath.Join(current, parts[len(parts)-1])
	if info, err := os.Lstat(target); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return ErrProposalArchive
		}
	} else if !os.IsNotExist(err) {
		return ErrProposalArchive
	}
	temporary, err := os.CreateTemp(current, ".hako-proposal-*")
	if err != nil {
		return ErrProposalArchive
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	if temporary.Chmod(0o644) != nil {
		_ = temporary.Close()
		return ErrProposalArchive
	}
	_, writeErr := temporary.Write(content)
	syncErr := temporary.Sync()
	closeErr := temporary.Close()
	if writeErr != nil || syncErr != nil || closeErr != nil {
		return ErrProposalArchive
	}
	if err := os.Rename(temporaryName, target); err != nil {
		return ErrProposalArchive
	}
	return nil
}
