package workspacegateway

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/mo3789530/hako/internal/workloadartifactaccess"
)

type fakeArtifactS3 struct {
	input         *s3.GetObjectInput
	body          []byte
	missingLength bool
}

func (f *fakeArtifactS3) GetObject(_ context.Context, input *s3.GetObjectInput, _ ...func(*s3.Options)) (*s3.GetObjectOutput, error) {
	f.input = input
	output := &s3.GetObjectOutput{Body: io.NopCloser(bytes.NewReader(f.body))}
	if !f.missingLength {
		length := int64(len(f.body))
		output.ContentLength = &length
	}
	return output, nil
}

func TestS3ArtifactReaderRestrictsBucketPrefixAndDeclaredSize(t *testing.T) {
	client := &fakeArtifactS3{body: []byte("artifact-bytes")}
	reader, err := NewS3ArtifactReader(S3ArtifactReaderConfig{Client: client, Bucket: "hako-artifacts-test", Prefix: "workloads/"})
	if err != nil {
		t.Fatal(err)
	}
	grant := workloadartifactaccess.Grant{StorageRef: "s3://hako-artifacts-test/workloads/tenant/run/artifact.log", SHA256: strings.Repeat("a", 64),
		SizeBytes: int64(len(client.body)), Kind: "logs"}
	body, err := reader.Open(context.Background(), grant)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := io.ReadAll(body)
	_ = body.Close()
	if string(data) != string(client.body) || client.input == nil || *client.input.Bucket != "hako-artifacts-test" ||
		*client.input.Key != "workloads/tenant/run/artifact.log" {
		t.Fatalf("opened bytes=%q S3 input=%+v", data, client.input)
	}

	for _, ref := range []string{
		"s3://other-bucket/workloads/tenant/run/artifact.log",
		"s3://hako-artifacts-test/private/tenant/run/artifact.log",
		"s3://hako-artifacts-test/workloads/tenant/../other",
		"s3://hako-artifacts-test/workloads/%2e%2e/private",
	} {
		invalid := grant
		invalid.StorageRef = ref
		if _, err := reader.Open(context.Background(), invalid); err == nil {
			t.Errorf("Open(%q) accepted an object outside the configured prefix", ref)
		}
	}

	client.missingLength = true
	if _, err := reader.Open(context.Background(), grant); err == nil {
		t.Fatal("object without content length was accepted")
	}
}

func TestS3ArtifactReaderRejectsInvalidConfiguration(t *testing.T) {
	client := &fakeArtifactS3{}
	for _, config := range []S3ArtifactReaderConfig{
		{Bucket: "hako-artifacts-test", Prefix: "workloads/"},
		{Client: client, Bucket: "hako-artifacts-test", Prefix: "../secret"},
		{Client: client, Bucket: "bad/bucket", Prefix: "workloads/"},
	} {
		if _, err := NewS3ArtifactReader(config); err == nil {
			t.Errorf("NewS3ArtifactReader(%+v) accepted invalid configuration", config)
		}
	}
}
