package workspaceruntime

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/servicecatalog"
)

var ErrWorkspaceServiceSecret = errors.New("Workspace Service generated secret is unavailable")

var workspaceServiceSecretIdentityPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// WorkspaceServiceSecretProvider resolves generated values only for an
// authenticated, current Resource Plane command. Values must remain in memory
// and be delivered only to the exact Workspace MicroVM Guest over its
// authenticated TLS channel.
type WorkspaceServiceSecretProvider interface {
	Resolve(context.Context, domain.WorkspaceServiceCommand) (map[string]string, error)
}

// WorkspaceServiceSecretRetirer schedules recovery-window deletion only after
// the exact Workspace Service has been removed from its current MicroVM.
type WorkspaceServiceSecretRetirer interface {
	Retire(context.Context, domain.WorkspaceServiceCommand) error
}

// WorkspaceServiceSecretCommitter promotes a staged credential only after
// the Guest has applied it and the controller has revalidated current state.
type WorkspaceServiceSecretCommitter interface {
	CommitRotation(context.Context, domain.WorkspaceServiceCommand) error
}

type workspaceServiceSecretsManagerAPI interface {
	CreateSecret(context.Context, *secretsmanager.CreateSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error)
	DescribeSecret(context.Context, *secretsmanager.DescribeSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error)
	GetSecretValue(context.Context, *secretsmanager.GetSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error)
	PutSecretValue(context.Context, *secretsmanager.PutSecretValueInput, ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error)
	UpdateSecretVersionStage(context.Context, *secretsmanager.UpdateSecretVersionStageInput, ...func(*secretsmanager.Options)) (*secretsmanager.UpdateSecretVersionStageOutput, error)
	DeleteSecret(context.Context, *secretsmanager.DeleteSecretInput, ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error)
}

// AWSWorkspaceServiceSecretProvider stores one stable, randomly generated
// credential per Tenant/Workspace/Service/environment name. It deliberately
// has no rotation API: changing a database password requires a coordinated
// database-side rotation transaction that this provider does not implement.
type AWSWorkspaceServiceSecretProvider struct {
	client workspaceServiceSecretsManagerAPI
	random func([]byte) (int, error)
}

func NewAWSWorkspaceServiceSecretProvider(client workspaceServiceSecretsManagerAPI) (*AWSWorkspaceServiceSecretProvider, error) {
	if client == nil {
		return nil, errors.New("Workspace Service Secrets Manager client is required")
	}
	return &AWSWorkspaceServiceSecretProvider{client: client, random: rand.Read}, nil
}

func (p *AWSWorkspaceServiceSecretProvider) Resolve(ctx context.Context, command domain.WorkspaceServiceCommand) (map[string]string, error) {
	if p == nil || p.client == nil || ctx == nil || !validWorkspaceServiceCommand(command) ||
		!workspaceServiceSecretIdentityPattern.MatchString(string(command.TenantID)) ||
		!workspaceServiceSecretIdentityPattern.MatchString(string(command.WorkspaceID)) ||
		!workspaceServiceSecretIdentityPattern.MatchString(string(command.ServiceID)) {
		return nil, ErrWorkspaceServiceSecret
	}
	template, err := servicecatalog.Decode(strings.NewReader(string(command.Config)))
	if err != nil {
		return nil, ErrWorkspaceServiceSecret
	}
	if command.DesiredState != "running" || len(template.GeneratedSecrets) == 0 {
		return map[string]string{}, nil
	}
	values := make(map[string]string, len(template.GeneratedSecrets))
	for _, key := range template.GeneratedSecrets {
		value, err := p.resolveOne(ctx, command, key)
		if err != nil {
			clear(values)
			return nil, ErrWorkspaceServiceSecret
		}
		values[key] = value
	}
	return values, nil
}

func (p *AWSWorkspaceServiceSecretProvider) resolveOne(ctx context.Context, command domain.WorkspaceServiceCommand, key string) (string, error) {
	if !validGeneratedSecretName(key) {
		return "", ErrWorkspaceServiceSecret
	}
	name := workspaceServiceSecretName(command, key)
	tags := workspaceServiceSecretTags(command, key)
	described, describeErr := p.client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
	if describeErr == nil {
		if described == nil || aws.ToString(described.Name) != name || !hasExactWorkspaceServiceTags(described.Tags, tags) {
			return "", ErrWorkspaceServiceSecret
		}
		if workspaceServiceCredentialGeneration(command) > 1 {
			return p.prepareRotation(ctx, command, key, name)
		}
		return p.readExisting(ctx, name)
	}
	var notFound *types.ResourceNotFoundException
	if !errors.As(describeErr, &notFound) {
		return "", ErrWorkspaceServiceSecret
	}
	description := "Hako generated Workspace Service credential; managed by Hako"
	var randomBytes [32]byte
	if _, err := p.random(randomBytes[:]); err != nil {
		return "", ErrWorkspaceServiceSecret
	}
	defer clear(randomBytes[:])
	value := base64.RawURLEncoding.EncodeToString(randomBytes[:])
	if _, err := p.random(randomBytes[:]); err != nil {
		return "", ErrWorkspaceServiceSecret
	}
	requestToken := base64.RawURLEncoding.EncodeToString(randomBytes[:])
	created, err := p.client.CreateSecret(ctx, &secretsmanager.CreateSecretInput{
		Name: aws.String(name), Description: aws.String(description), SecretString: aws.String(value),
		ClientRequestToken: aws.String(requestToken), Tags: tags,
	})
	if err == nil {
		if created == nil || aws.ToString(created.Name) != name {
			return "", ErrWorkspaceServiceSecret
		}
		if workspaceServiceCredentialGeneration(command) > 1 {
			return p.prepareRotation(ctx, command, key, name)
		}
		return value, nil
	}
	var exists *types.ResourceExistsException
	if !errors.As(err, &exists) {
		return "", ErrWorkspaceServiceSecret
	}
	if workspaceServiceCredentialGeneration(command) > 1 {
		return p.prepareRotation(ctx, command, key, name)
	}
	return p.readExisting(ctx, name)
}

func (p *AWSWorkspaceServiceSecretProvider) prepareRotation(ctx context.Context, command domain.WorkspaceServiceCommand, key, name string) (string, error) {
	described, err := p.client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
	if err != nil || described == nil || aws.ToString(described.Name) != name ||
		!hasExactWorkspaceServiceTags(described.Tags, workspaceServiceSecretTags(command, key)) || described.DeletedDate != nil {
		return "", ErrWorkspaceServiceSecret
	}
	versionID := workspaceServiceCredentialVersionID(command, key)
	if hasSecretVersionStage(described.VersionIdsToStages, versionID, "AWSCURRENT") {
		return p.readVersion(ctx, name, versionID)
	}
	if hasSecretVersionStage(described.VersionIdsToStages, versionID, "AWSPENDING") {
		return p.readVersion(ctx, name, versionID)
	}
	var bytes [32]byte
	if _, err := p.random(bytes[:]); err != nil {
		return "", ErrWorkspaceServiceSecret
	}
	value := base64.RawURLEncoding.EncodeToString(bytes[:])
	clear(bytes[:])
	stored, err := p.client.PutSecretValue(ctx, &secretsmanager.PutSecretValueInput{
		SecretId: aws.String(name), ClientRequestToken: aws.String(versionID),
		SecretString: aws.String(value), VersionStages: []string{"AWSPENDING"},
	})
	if err == nil && stored != nil && aws.ToString(stored.Name) == name && aws.ToString(stored.VersionId) == versionID {
		return value, nil
	}
	// PutSecretValue is idempotent by version ID. A racing retry may have
	// created the same pending version; resolve it by immutable ID.
	return p.readVersion(ctx, name, versionID)
}

func (p *AWSWorkspaceServiceSecretProvider) readVersion(ctx context.Context, name, versionID string) (string, error) {
	value, err := p.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(name), VersionId: aws.String(versionID)})
	if err != nil || value == nil || aws.ToString(value.Name) != name || aws.ToString(value.VersionId) != versionID ||
		value.SecretString == nil || !validGeneratedServiceSecret(*value.SecretString) {
		return "", ErrWorkspaceServiceSecret
	}
	return *value.SecretString, nil
}

// CommitRotation promotes the generation-specific staged version after the
// Guest has applied it. Repeating the call after a lost response is safe.
func (p *AWSWorkspaceServiceSecretProvider) CommitRotation(ctx context.Context, command domain.WorkspaceServiceCommand) error {
	if p == nil || p.client == nil || ctx == nil || !validWorkspaceServiceCommand(command) || command.DesiredState != "running" ||
		workspaceServiceCredentialGeneration(command) < 2 || !workspaceServiceSecretIdentityPattern.MatchString(string(command.TenantID)) ||
		!workspaceServiceSecretIdentityPattern.MatchString(string(command.WorkspaceID)) || !workspaceServiceSecretIdentityPattern.MatchString(string(command.ServiceID)) {
		return ErrWorkspaceServiceSecret
	}
	template, err := servicecatalog.Decode(strings.NewReader(string(command.Config)))
	if err != nil {
		return ErrWorkspaceServiceSecret
	}
	for _, key := range template.GeneratedSecrets {
		name := workspaceServiceSecretName(command, key)
		versionID := workspaceServiceCredentialVersionID(command, key)
		described, err := p.client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
		if err != nil || described == nil || aws.ToString(described.Name) != name ||
			!hasExactWorkspaceServiceTags(described.Tags, workspaceServiceSecretTags(command, key)) || described.DeletedDate != nil {
			return ErrWorkspaceServiceSecret
		}
		current := hasSecretVersionStage(described.VersionIdsToStages, versionID, "AWSCURRENT")
		pending := hasSecretVersionStage(described.VersionIdsToStages, versionID, "AWSPENDING")
		if current && !pending {
			continue
		}
		if !current && !pending {
			return ErrWorkspaceServiceSecret
		}
		if !current {
			currentVersion := secretVersionWithStage(described.VersionIdsToStages, "AWSCURRENT")
			if currentVersion == "" {
				return ErrWorkspaceServiceSecret
			}
			if _, err := p.client.UpdateSecretVersionStage(ctx, &secretsmanager.UpdateSecretVersionStageInput{
				SecretId: aws.String(name), VersionStage: aws.String("AWSCURRENT"), MoveToVersionId: aws.String(versionID), RemoveFromVersionId: aws.String(currentVersion),
			}); err != nil {
				// Re-read on retry to detect the lost-response case where promotion
				// already committed.
				latest, describeErr := p.client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
				if describeErr != nil || latest == nil || !hasSecretVersionStage(latest.VersionIdsToStages, versionID, "AWSCURRENT") {
					return ErrWorkspaceServiceSecret
				}
			}
		}
		if pending {
			if _, err := p.client.UpdateSecretVersionStage(ctx, &secretsmanager.UpdateSecretVersionStageInput{
				SecretId: aws.String(name), VersionStage: aws.String("AWSPENDING"), RemoveFromVersionId: aws.String(versionID),
			}); err != nil {
				return ErrWorkspaceServiceSecret
			}
		}
	}
	return nil
}

func workspaceServiceCredentialVersionID(command domain.WorkspaceServiceCommand, key string) string {
	identity := fmt.Sprintf("%s\x00%s\x00%s\x00%s\x00%d", command.TenantID, command.WorkspaceID, command.ServiceID, key, workspaceServiceCredentialGeneration(command))
	digest := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(digest[:])
}

func hasSecretVersionStage(versions map[string][]string, versionID, stage string) bool {
	for _, candidate := range versions[versionID] {
		if candidate == stage {
			return true
		}
	}
	return false
}

func secretVersionWithStage(versions map[string][]string, stage string) string {
	for versionID, stages := range versions {
		for _, candidate := range stages {
			if candidate == stage {
				return versionID
			}
		}
	}
	return ""
}

func (p *AWSWorkspaceServiceSecretProvider) readExisting(ctx context.Context, name string) (string, error) {
	value, err := p.client.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: aws.String(name), VersionStage: aws.String("AWSCURRENT")})
	if err != nil || value == nil || aws.ToString(value.Name) != name || value.SecretString == nil || !validGeneratedServiceSecret(*value.SecretString) {
		return "", ErrWorkspaceServiceSecret
	}
	return *value.SecretString, nil
}

// Retire schedules an owned generated credential for deletion with a recovery
// window. Replays are idempotent while Secrets Manager reports it pending
// deletion; a foreign or untagged name is never touched.
func (p *AWSWorkspaceServiceSecretProvider) Retire(ctx context.Context, command domain.WorkspaceServiceCommand) error {
	if p == nil || p.client == nil || ctx == nil || !validWorkspaceServiceCommand(command) || command.DesiredState != "deleted" ||
		!workspaceServiceSecretIdentityPattern.MatchString(string(command.TenantID)) ||
		!workspaceServiceSecretIdentityPattern.MatchString(string(command.WorkspaceID)) ||
		!workspaceServiceSecretIdentityPattern.MatchString(string(command.ServiceID)) {
		return ErrWorkspaceServiceSecret
	}
	template, err := servicecatalog.Decode(strings.NewReader(string(command.Config)))
	if err != nil {
		return ErrWorkspaceServiceSecret
	}
	for _, key := range template.GeneratedSecrets {
		if !validGeneratedSecretName(key) {
			return ErrWorkspaceServiceSecret
		}
		name := workspaceServiceSecretName(command, key)
		described, err := p.client.DescribeSecret(ctx, &secretsmanager.DescribeSecretInput{SecretId: aws.String(name)})
		if err != nil {
			var notFound *types.ResourceNotFoundException
			if errors.As(err, &notFound) {
				continue
			}
			return ErrWorkspaceServiceSecret
		}
		if described == nil || aws.ToString(described.Name) != name || !hasExactWorkspaceServiceTags(described.Tags, workspaceServiceSecretTags(command, key)) {
			return ErrWorkspaceServiceSecret
		}
		if described.DeletedDate != nil {
			continue
		}
		deleted, err := p.client.DeleteSecret(ctx, &secretsmanager.DeleteSecretInput{
			SecretId: aws.String(name), RecoveryWindowInDays: aws.Int64(7),
		})
		if err != nil {
			var notFound *types.ResourceNotFoundException
			if errors.As(err, &notFound) {
				continue
			}
			return ErrWorkspaceServiceSecret
		}
		if deleted == nil || aws.ToString(deleted.Name) != name || deleted.DeletionDate == nil {
			return ErrWorkspaceServiceSecret
		}
	}
	return nil
}

func workspaceServiceSecretName(command domain.WorkspaceServiceCommand, key string) string {
	return fmt.Sprintf("hako/workspaces/%s/%s/services/%s/%s", command.TenantID, command.WorkspaceID, command.ServiceID, key)
}

func workspaceServiceSecretTags(command domain.WorkspaceServiceCommand, key string) []types.Tag {
	return []types.Tag{
		{Key: aws.String("hako:managed-by"), Value: aws.String("hako")},
		{Key: aws.String("hako:tenant-id"), Value: aws.String(string(command.TenantID))},
		{Key: aws.String("hako:workspace-id"), Value: aws.String(string(command.WorkspaceID))},
		{Key: aws.String("hako:service-id"), Value: aws.String(string(command.ServiceID))},
		{Key: aws.String("hako:secret-name"), Value: aws.String(key)},
	}
}

func hasExactWorkspaceServiceTags(actual, required []types.Tag) bool {
	if len(actual) != len(required) {
		return false
	}
	values := make(map[string]string, len(actual))
	for _, tag := range actual {
		if tag.Key == nil || tag.Value == nil {
			return false
		}
		values[*tag.Key] = *tag.Value
	}
	for _, tag := range required {
		if tag.Key == nil || tag.Value == nil || values[*tag.Key] != *tag.Value {
			return false
		}
	}
	return true
}

func validGeneratedSecretName(name string) bool {
	if name == "" || len(name) > 64 {
		return false
	}
	for _, char := range name {
		if !(char >= 'A' && char <= 'Z') && !(char >= '0' && char <= '9') && char != '_' {
			return false
		}
	}
	return true
}

func validGeneratedServiceSecret(value string) bool {
	if len(value) != 43 {
		return false
	}
	for _, char := range value {
		if !(char >= 'A' && char <= 'Z') && !(char >= 'a' && char <= 'z') && !(char >= '0' && char <= '9') && char != '_' && char != '-' {
			return false
		}
	}
	return true
}
