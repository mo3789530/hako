//go:build linux

package workspaceagent

import (
	"context"
	"errors"
	"io"

	"github.com/mo3789530/hako/internal/workloadsource"
	"github.com/mo3789530/hako/internal/workspaceprotocol"
)

var ErrSourceTransfer = errors.New("verified source archive transfer failed")

// TransferSourceArchive sends a content-addressed archive over the already
// authenticated one-use Workspace file-transfer stream. It never accepts a
// destination from the caller; the Guest protocol fixes it to /workspace/src.
func TransferSourceArchive(ctx context.Context, stream io.ReadWriteCloser, archive workloadsource.SourceArchive, digest string) error {
	if ctx == nil || stream == nil || archive.Body == nil || archive.Size < 1 || archive.Size > workspaceprotocol.FileMaxBytes || ctx.Err() != nil {
		return ErrSourceTransfer
	}
	stopClose := context.AfterFunc(ctx, func() { _ = stream.Close() })
	defer stopClose()
	if err := workspaceprotocol.WriteFileRequest(stream, workspaceprotocol.FileRequest{
		Operation: "install_archive", Path: "/workspace/src", Size: archive.Size, Digest: digest,
	}); err != nil {
		return ErrSourceTransfer
	}
	reader := &sourceTransferContextReader{ctx: ctx, reader: archive.Body}
	written, err := io.CopyN(stream, reader, archive.Size)
	if err != nil || written != archive.Size {
		return ErrSourceTransfer
	}
	response, err := workspaceprotocol.ReadFileResponse(stream)
	if err != nil || !response.OK || response.Size != archive.Size {
		return ErrSourceTransfer
	}
	return nil
}

type sourceTransferContextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *sourceTransferContextReader) Read(value []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(value)
}
