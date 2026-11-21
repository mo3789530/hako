package workloadagent

import (
	"io"
	"os"
	"path/filepath"

	"github.com/mo3789530/hako/internal/factoryconfig"
)

func readFactoryManifest(repository string) ([]byte, bool, error) {
	if repository == "" || !filepath.IsAbs(repository) {
		return nil, false, ErrExecutionPlan
	}
	rootInfo, err := os.Lstat(repository)
	if err != nil || !rootInfo.IsDir() || rootInfo.Mode()&os.ModeSymlink != 0 {
		return nil, false, ErrExecutionPlan
	}
	directory := filepath.Join(repository, ".hako")
	directoryInfo, err := os.Lstat(directory)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil || !directoryInfo.IsDir() || directoryInfo.Mode()&os.ModeSymlink != 0 {
		return nil, false, ErrExecutionPlan
	}
	path := filepath.Join(directory, "factory.yaml")
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() > factoryconfig.MaxManifestBytes {
		return nil, false, ErrExecutionPlan
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, ErrExecutionPlan
	}
	defer file.Close()
	manifest, err := io.ReadAll(io.LimitReader(file, factoryconfig.MaxManifestBytes+1))
	if err != nil || len(manifest) == 0 || len(manifest) > factoryconfig.MaxManifestBytes {
		return nil, false, ErrExecutionPlan
	}
	return manifest, true, nil
}
