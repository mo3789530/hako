package workloadagent

import (
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

const NativeAgentGoOfflineProfile = "go-offline-v1"

// requireNativeAgentGoProfile enforces the only Native Agent test profile
// currently available: a Go module with Go sources and no other supported
// language sources. A language detector is only a rejection guard here; it
// never selects a command or grants an additional profile.
func requireNativeAgentGoProfile(repositoryPath string) error {
	if repositoryPath == "" {
		return ErrVerifierProfileUnavailable
	}
	root, err := filepath.Abs(repositoryPath)
	if err != nil {
		return ErrVerifierProfileUnavailable
	}
	rootInfo, err := os.Lstat(root)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return ErrVerifierProfileUnavailable
	}
	moduleInfo, err := os.Lstat(filepath.Join(root, "go.mod"))
	if err != nil || !moduleInfo.Mode().IsRegular() {
		return ErrVerifierProfileUnavailable
	}
	goSource := false
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			if path != root && (entry.Name() == ".git" || entry.Name() == "vendor") {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		name := strings.ToLower(entry.Name())
		if name == "go.mod" || name == "go.sum" {
			return nil
		}
		if strings.HasSuffix(name, ".tf.json") || strings.HasSuffix(name, ".tfvars.json") || strings.HasSuffix(name, ".tfstate") ||
			strings.HasSuffix(name, ".tfstate.backup") || strings.HasSuffix(name, ".terraform.lock.hcl") {
			return ErrVerifierProfileUnavailable
		}
		switch filepath.Ext(name) {
		case ".go":
			goSource = true
		case ".py", ".pyw", ".java", ".kt", ".kts", ".rs", ".ts", ".tsx", ".js", ".jsx", ".mjs", ".cjs", ".tf", ".tfvars":
			return ErrVerifierProfileUnavailable
		}
		return nil
	})
	if err != nil || !goSource {
		return ErrVerifierProfileUnavailable
	}
	return nil
}
