package workspaceprotocol

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"path"
	"strings"
)

const (
	FileMaxControlBytes = 4096
	FileMaxBytes        = int64(1 << 30)
	fileControlHeader   = 9
	fileRequestFrame    = 1
	fileResponseFrame   = 2
)

var ErrInvalidFileTransfer = errors.New("invalid Workspace file transfer frame")

var fileMagic = [4]byte{'H', 'A', 'K', 'F'}

type FileRequest struct {
	Operation string `json:"operation"`
	Path      string `json:"path"`
	Size      int64  `json:"size"`
	Digest    string `json:"digest,omitempty"`
}

type FileResponse struct {
	OK   bool   `json:"ok"`
	Size int64  `json:"size"`
	Code string `json:"code,omitempty"`
}

func WriteFileRequest(w io.Writer, request FileRequest) error {
	if w == nil || !validFileRequest(request) {
		return ErrInvalidFileTransfer
	}
	return writeFileControl(w, fileRequestFrame, request)
}

func ReadFileRequest(r io.Reader) (FileRequest, error) {
	var request FileRequest
	if err := readFileControl(r, fileRequestFrame, &request); err != nil || !validFileRequest(request) {
		return FileRequest{}, ErrInvalidFileTransfer
	}
	return request, nil
}

func WriteFileResponse(w io.Writer, response FileResponse) error {
	if w == nil || response.Size < 0 || response.Size > FileMaxBytes || (response.OK && response.Code != "") ||
		(!response.OK && (response.Size != 0 || !validFileErrorCode(response.Code))) {
		return ErrInvalidFileTransfer
	}
	return writeFileControl(w, fileResponseFrame, response)
}

func ReadFileResponse(r io.Reader) (FileResponse, error) {
	var response FileResponse
	if err := readFileControl(r, fileResponseFrame, &response); err != nil || response.Size < 0 || response.Size > FileMaxBytes ||
		(response.OK && response.Code != "") || (!response.OK && (response.Size != 0 || !validFileErrorCode(response.Code))) {
		return FileResponse{}, ErrInvalidFileTransfer
	}
	return response, nil
}

func validFileRequest(request FileRequest) bool {
	if request.Path == "" || len(request.Path) > FileMaxControlBytes || !path.IsAbs(request.Path) || path.Clean(request.Path) != request.Path ||
		(request.Path != "/workspace" && !strings.HasPrefix(request.Path, "/workspace/")) || strings.ContainsAny(request.Path, "\\\x00\r\n") {
		return false
	}
	switch request.Operation {
	case "upload":
		return request.Path != "/workspace" && request.Size >= 0 && request.Size <= FileMaxBytes && request.Digest == ""
	case "download":
		return request.Size == 0 && request.Digest == ""
	case "install_archive":
		digest, err := hex.DecodeString(request.Digest)
		return request.Path == "/workspace/src" && request.Size > 0 && request.Size <= FileMaxBytes && err == nil && len(digest) == 32 && hex.EncodeToString(digest) == request.Digest
	default:
		return false
	}
}

func validFileErrorCode(code string) bool {
	switch code {
	case "not_found", "not_allowed", "too_large", "transfer_failed":
		return true
	default:
		return false
	}
}

func writeFileControl(w io.Writer, frameType byte, value any) error {
	payload, err := json.Marshal(value)
	if err != nil || len(payload) == 0 || len(payload) > FileMaxControlBytes {
		return ErrInvalidFileTransfer
	}
	frame := make([]byte, fileControlHeader+len(payload))
	copy(frame, fileMagic[:])
	frame[4] = frameType
	binary.BigEndian.PutUint32(frame[5:fileControlHeader], uint32(len(payload)))
	copy(frame[fileControlHeader:], payload)
	for len(frame) > 0 {
		written, err := w.Write(frame)
		if err != nil || written <= 0 || written > len(frame) {
			return ErrInvalidFileTransfer
		}
		frame = frame[written:]
	}
	return nil
}

func readFileControl(r io.Reader, frameType byte, target any) error {
	if r == nil || target == nil {
		return ErrInvalidFileTransfer
	}
	header := make([]byte, fileControlHeader)
	if _, err := io.ReadFull(r, header); err != nil || !bytes.Equal(header[:4], fileMagic[:]) || header[4] != frameType {
		return ErrInvalidFileTransfer
	}
	length := binary.BigEndian.Uint32(header[5:])
	if length == 0 || length > FileMaxControlBytes {
		return ErrInvalidFileTransfer
	}
	payload := make([]byte, int(length))
	if _, err := io.ReadFull(r, payload); err != nil {
		return ErrInvalidFileTransfer
	}
	decoder := json.NewDecoder(bytes.NewReader(payload))
	decoder.DisallowUnknownFields()
	if decoder.Decode(target) != nil || decoder.Decode(new(any)) != io.EOF {
		return ErrInvalidFileTransfer
	}
	return nil
}
