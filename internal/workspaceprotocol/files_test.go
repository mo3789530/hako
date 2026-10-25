package workspaceprotocol

import (
	"bytes"
	"errors"
	"strings"
	"testing"
)

type oneByteWriter struct{ bytes.Buffer }

func (w *oneByteWriter) Write(value []byte) (int, error) {
	if len(value) > 1 {
		value = value[:1]
	}
	return w.Buffer.Write(value)
}

func TestFileControlFramesRoundTripAndHandleShortWrites(t *testing.T) {
	var encoded oneByteWriter
	want := FileRequest{Operation: "upload", Path: "/workspace/src/main.go", Size: 24}
	if err := WriteFileRequest(&encoded, want); err != nil {
		t.Fatal(err)
	}
	got, err := ReadFileRequest(bytes.NewReader(encoded.Bytes()))
	if err != nil || got != want {
		t.Fatalf("decoded request=%+v error=%v want=%+v", got, err, want)
	}

	encoded.Reset()
	wantResponse := FileResponse{OK: false, Code: "not_found"}
	if err := WriteFileResponse(&encoded, wantResponse); err != nil {
		t.Fatal(err)
	}
	gotResponse, err := ReadFileResponse(bytes.NewReader(encoded.Bytes()))
	if err != nil || gotResponse != wantResponse {
		t.Fatalf("decoded response=%+v error=%v want=%+v", gotResponse, err, wantResponse)
	}
}

func TestFileControlFramesRejectUnsafeRequestsAndMalformedResponses(t *testing.T) {
	validDigest := strings.Repeat("a", 64)
	for _, request := range []FileRequest{
		{Operation: "upload", Path: "/etc/passwd", Size: 1},
		{Operation: "upload", Path: "/workspace/../etc/passwd", Size: 1},
		{Operation: "upload", Path: "/workspace/a\\b", Size: 1},
		{Operation: "upload", Path: "/workspace/file", Size: FileMaxBytes + 1},
		{Operation: "download", Path: "/workspace/file", Size: 1},
		{Operation: "delete", Path: "/workspace/file"},
		{Operation: "install_archive", Path: "/workspace/src", Size: 1},
		{Operation: "install_archive", Path: "/workspace/other", Size: 1, Digest: validDigest},
		{Operation: "install_archive", Path: "/workspace/src", Size: 0, Digest: validDigest},
		{Operation: "install_archive", Path: "/workspace/src", Size: 1, Digest: strings.ToUpper(validDigest)},
	} {
		if err := WriteFileRequest(&bytes.Buffer{}, request); !errors.Is(err, ErrInvalidFileTransfer) {
			t.Errorf("unsafe request %+v error=%v", request, err)
		}
	}
	if err := WriteFileRequest(&bytes.Buffer{}, FileRequest{Operation: "install_archive", Path: "/workspace/src", Size: 1, Digest: validDigest}); err != nil {
		t.Fatalf("valid Preview source install request rejected: %v", err)
	}
	if err := WriteFileRequest(&bytes.Buffer{}, FileRequest{Operation: "start_preview", Path: "/workspace/src/.hako/preview.yaml"}); err != nil {
		t.Fatalf("valid Preview start request rejected: %v", err)
	}
	for _, response := range []FileResponse{
		{OK: true, Size: -1},
		{OK: true, Size: FileMaxBytes + 1},
		{OK: true, Code: "not_found"},
		{OK: false},
		{OK: false, Code: "unexpected"},
		{OK: false, Port: 3000, Code: "transfer_failed"},
	} {
		if err := WriteFileResponse(&bytes.Buffer{}, response); !errors.Is(err, ErrInvalidFileTransfer) {
			t.Errorf("invalid response %+v error=%v", response, err)
		}
	}
	if _, err := ReadFileRequest(bytes.NewBufferString(`{"operation":"download","path":"/workspace/a","size":0,"extra":true}`)); !errors.Is(err, ErrInvalidFileTransfer) {
		t.Fatalf("request with unknown field error=%v", err)
	}
}
