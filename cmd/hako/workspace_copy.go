package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspacegateway"
	"github.com/mo3789530/hako/internal/workspaceprotocol"
	"github.com/mo3789530/hako/internal/workspacesession"
)

var copyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,128}$`)

func copyWorkspacePath(ctx context.Context, tenantID, workspaceID, source, destination string, download bool) (returnErr error) {
	return copyWorkspacePathWith(ctx, tenantID, workspaceID, source, destination, download, nil, nil)
}

func copyWorkspacePathWith(ctx context.Context, tenantID, workspaceID, source, destination string, download bool, apiHTTPClient *http.Client, gatewayTLSConfig *tls.Config) (returnErr error) {
	if ctx == nil || !validCopyID(tenantID) || !validCopyID(workspaceID) {
		return errors.New("valid Tenant and Workspace IDs and copy context are required")
	}
	accessToken, err := auth.LoadAccessToken(cliCredentialGetter, time.Now())
	if err != nil {
		return err
	}
	apiURL, gatewayURL := strings.TrimSpace(os.Getenv("HAKO_API_URL")), strings.TrimSpace(os.Getenv("HAKO_GATEWAY_URL"))
	if apiURL == "" || gatewayURL == "" {
		return errors.New("HAKO_API_URL and HAKO_GATEWAY_URL are required")
	}
	remotePath, localPath := destination, source
	if download {
		remotePath, localPath = source, destination
	}
	if !validWorkspaceFileCLIPath(remotePath) || strings.TrimSpace(localPath) == "" || strings.ContainsRune(localPath, '\x00') {
		return errors.New("Workspace path must name a file inside /workspace and local path must be non-empty")
	}
	localPath, err = filepath.Abs(localPath)
	if err != nil {
		return errors.New("local path is invalid")
	}
	var upload *os.File
	var uploadSize int64
	if !download {
		info, err := os.Lstat(localPath)
		if err != nil || !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 || info.Size() < 0 || info.Size() > workspaceprotocol.FileMaxBytes {
			return errors.New("local upload source must be a regular file no larger than 1 GiB")
		}
		upload, err = os.Open(localPath)
		if err != nil {
			return errors.New("open local upload source")
		}
		defer upload.Close()
		openedInfo, err := upload.Stat()
		if err != nil || !openedInfo.Mode().IsRegular() || !os.SameFile(info, openedInfo) || openedInfo.Size() < 0 || openedInfo.Size() > workspaceprotocol.FileMaxBytes {
			return errors.New("local upload source changed while opening")
		}
		uploadSize = openedInfo.Size()
	}
	downloadTemp, downloadFile, err := prepareWorkspaceDownload(localPath, download)
	if err != nil {
		return err
	}
	if downloadFile != nil {
		defer func() {
			_ = downloadFile.Close()
			if returnErr != nil {
				_ = os.Remove(downloadTemp)
			}
		}()
	}
	session, err := api.CreateWorkspaceSession(ctx, apiURL, accessToken, tenantID, workspaceID,
		[]workspacesession.Permission{workspacesession.PermissionFileTransfer}, apiHTTPClient)
	if err != nil {
		return fmt.Errorf("request Workspace file-transfer session: %w", err)
	}
	defer func() {
		revokeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := api.RevokeWorkspaceSession(revokeCtx, apiURL, accessToken, tenantID, workspaceID, string(session.SessionID), apiHTTPClient); err != nil && returnErr == nil {
			returnErr = fmt.Errorf("revoke Workspace file-transfer session: %w", err)
		}
	}()
	connection, err := workspacegateway.Dial(ctx, gatewayURL, domain.WorkspaceID(workspaceID), workspacesession.PermissionFileTransfer, 0,
		session.Ticket, gatewayTLSConfig)
	if err != nil {
		return err
	}
	defer connection.Close()
	stopClose := context.AfterFunc(ctx, func() { _ = connection.Close() })
	defer stopClose()
	if download {
		if err := workspaceprotocol.WriteFileRequest(connection, workspaceprotocol.FileRequest{Operation: "download", Path: remotePath}); err != nil {
			return err
		}
		response, err := workspaceprotocol.ReadFileResponse(connection)
		if err != nil {
			return err
		}
		if !response.OK {
			return errors.New("Workspace file could not be downloaded")
		}
		if _, err := io.CopyN(downloadFile, connection, response.Size); err != nil {
			return errors.New("Workspace file download was incomplete")
		}
		if err := downloadFile.Sync(); err != nil {
			return errors.New("persist local Workspace download")
		}
		if err := downloadFile.Close(); err != nil {
			return errors.New("close local Workspace download")
		}
		if err := os.Rename(downloadTemp, localPath); err != nil {
			return errors.New("commit local Workspace download")
		}
		return nil
	}
	if err := workspaceprotocol.WriteFileRequest(connection, workspaceprotocol.FileRequest{Operation: "upload", Path: remotePath, Size: uploadSize}); err != nil {
		return err
	}
	if _, err := io.CopyN(connection, upload, uploadSize); err != nil {
		return errors.New("local Workspace upload was incomplete")
	}
	response, err := workspaceprotocol.ReadFileResponse(connection)
	if err != nil || !response.OK || response.Size != uploadSize {
		return errors.New("Workspace file upload failed")
	}
	return nil
}

func prepareWorkspaceDownload(localPath string, download bool) (string, *os.File, error) {
	if !download {
		return "", nil, nil
	}
	parent := filepath.Dir(localPath)
	if info, err := os.Stat(parent); err != nil || !info.IsDir() {
		return "", nil, errors.New("local download directory does not exist")
	}
	if info, err := os.Lstat(localPath); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return "", nil, errors.New("refusing to replace a symlink with a Workspace download")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", nil, errors.New("inspect local download destination")
	}
	random := make([]byte, 12)
	if _, err := rand.Read(random); err != nil {
		return "", nil, errors.New("create local download name")
	}
	temp := filepath.Join(parent, ".hako-download-"+fmt.Sprintf("%x", random))
	file, err := os.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return "", nil, errors.New("create private local download file")
	}
	return temp, file, nil
}

func validWorkspaceFileCLIPath(value string) bool {
	if value == "" || !path.IsAbs(value) || path.Clean(value) != value || strings.ContainsAny(value, "\\\x00\r\n") {
		return false
	}
	return strings.HasPrefix(value, "/workspace/") && value != "/workspace/"
}

func validCopyID(value string) bool { return copyIDPattern.MatchString(value) }
