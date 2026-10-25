package workspacegateway

import (
	"context"
	"io"
	"net/url"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/mo3789530/hako/internal/workloadartifactaccess"
)

type S3ObjectGetter interface {
	GetObject(context.Context, *s3.GetObjectInput, ...func(*s3.Options)) (*s3.GetObjectOutput, error)
}

type S3ArtifactReaderConfig struct {
	Client S3ObjectGetter
	Bucket string
	Prefix string
}

type S3ArtifactReader struct {
	client S3ObjectGetter
	bucket string
	prefix string
}

var s3ArtifactBucketPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9.-]{1,61}[a-z0-9]$`)

func NewS3ArtifactReader(config S3ArtifactReaderConfig) (*S3ArtifactReader, error) {
	if config.Client == nil || !validS3Bucket(config.Bucket) || !validArtifactPrefix(config.Prefix) {
		return nil, ErrConfig
	}
	return &S3ArtifactReader{client: config.Client, bucket: config.Bucket, prefix: strings.TrimSuffix(config.Prefix, "/") + "/"}, nil
}

func (r *S3ArtifactReader) Open(ctx context.Context, grant workloadartifactaccess.Grant) (io.ReadCloser, error) {
	if r == nil || r.client == nil || ctx == nil || !workloadartifactaccess.ValidateGrant(grant) {
		return nil, ErrArtifactUnavailable
	}
	parsed, err := url.Parse(grant.StorageRef)
	if err != nil || parsed.Host != r.bucket {
		return nil, ErrArtifactUnavailable
	}
	key := strings.TrimPrefix(parsed.EscapedPath(), "/")
	decodedKey, err := url.PathUnescape(key)
	if err != nil || !strings.HasPrefix(decodedKey, r.prefix) || strings.Contains(decodedKey, "\\") || strings.Contains(decodedKey, "//") {
		return nil, ErrArtifactUnavailable
	}
	for _, component := range strings.Split(decodedKey, "/") {
		if component == ".." || component == "." || component == "" {
			return nil, ErrArtifactUnavailable
		}
	}
	output, err := r.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &r.bucket, Key: &decodedKey})
	if err != nil || output == nil || output.Body == nil || output.ContentLength == nil || *output.ContentLength != grant.SizeBytes {
		if output != nil && output.Body != nil {
			_ = output.Body.Close()
		}
		return nil, ErrArtifactUnavailable
	}
	return output.Body, nil
}

func validS3Bucket(value string) bool {
	return s3ArtifactBucketPattern.MatchString(value) && !strings.Contains(value, "..") && !strings.Contains(value, ".-") && !strings.Contains(value, "-.")
}

func validArtifactPrefix(value string) bool {
	value = strings.Trim(value, "/")
	if value == "" || len(value) > 512 || strings.Contains(value, "//") || strings.Contains(value, "\\") {
		return false
	}
	for _, component := range strings.Split(value, "/") {
		if component == ".." || component == "." {
			return false
		}
	}
	return true
}
