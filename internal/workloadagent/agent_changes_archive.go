package workloadagent

import (
	"archive/zip"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"io/fs"
	"path"
	"sort"
	"strings"
	"unicode/utf8"
)

const maxAgentChangeArchiveFiles = 128

var ErrInvalidAgentChangeArchive = errors.New("Agent change archive is invalid")

type AgentChangeArchiveFile struct {
	Path      string `json:"path"`
	SizeBytes int64  `json:"size_bytes"`
}

type AgentChangeArchiveSummary struct {
	SHA256 string                   `json:"sha256"`
	Files  []AgentChangeArchiveFile `json:"files"`
}

// InspectAgentChangeArchive validates an untrusted Agent ZIP without
// extracting or applying it. The result intentionally contains paths and
// sizes only, never proposal contents.
func InspectAgentChangeArchive(data []byte) (AgentChangeArchiveSummary, error) {
	if len(data) < 1 || len(data) > maxNativeAgentChangeArchive {
		return AgentChangeArchiveSummary{}, ErrInvalidAgentChangeArchive
	}
	reader, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil || len(reader.File) == 0 || len(reader.File) > maxAgentChangeArchiveFiles {
		return AgentChangeArchiveSummary{}, ErrInvalidAgentChangeArchive
	}

	seen := make(map[string]struct{}, len(reader.File))
	files := make([]AgentChangeArchiveFile, 0, len(reader.File))
	var total int64
	for _, entry := range reader.File {
		name := entry.Name
		if !validAgentChangeArchivePath(name) || entry.FileInfo().IsDir() || !entry.Mode().IsRegular() || entry.Flags&1 != 0 ||
			entry.UncompressedSize64 > defaultNativeAgentFileBytes || nativeAgentSensitivePath(name) || nativeAgentProtectedWritePath(name) {
			return AgentChangeArchiveSummary{}, ErrInvalidAgentChangeArchive
		}
		if _, ok := seen[name]; ok {
			return AgentChangeArchiveSummary{}, ErrInvalidAgentChangeArchive
		}
		seen[name] = struct{}{}
		total += int64(entry.UncompressedSize64)
		if total > maxNativeAgentChangeArchive {
			return AgentChangeArchiveSummary{}, ErrInvalidAgentChangeArchive
		}
		file, err := entry.Open()
		if err != nil {
			return AgentChangeArchiveSummary{}, ErrInvalidAgentChangeArchive
		}
		content, readErr := io.ReadAll(io.LimitReader(file, int64(defaultNativeAgentFileBytes)+1))
		closeErr := file.Close()
		if readErr != nil || closeErr != nil || int64(len(content)) != int64(entry.UncompressedSize64) ||
			len(content) > defaultNativeAgentFileBytes || !utf8.Valid(content) || bytes.IndexByte(content, 0) >= 0 ||
			containsPrivateCredential(content) {
			clear(content)
			return AgentChangeArchiveSummary{}, ErrInvalidAgentChangeArchive
		}
		clear(content)
		files = append(files, AgentChangeArchiveFile{Path: name, SizeBytes: int64(entry.UncompressedSize64)})
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	digest := sha256.Sum256(data)
	return AgentChangeArchiveSummary{SHA256: hex.EncodeToString(digest[:]), Files: files}, nil
}

func validAgentChangeArchivePath(name string) bool {
	if name == "" || len(name) > 512 || !utf8.ValidString(name) || strings.ContainsAny(name, "\\\r\n\x00:") ||
		strings.HasPrefix(name, "/") || path.Clean(name) != name || !fs.ValidPath(name) {
		return false
	}
	return true
}
