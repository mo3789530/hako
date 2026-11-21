package workspaceruntime

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager/types"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/servicecatalog"
)

type generatedServiceSecret struct {
	name      string
	value     string
	tags      []types.Tag
	deletedAt *time.Time
	versions  map[string]string
	stages    map[string][]string
}

type serviceSecretsManagerFake struct {
	secrets               map[string]generatedServiceSecret
	createCalls           int
	describeCalls         int
	getCalls              int
	deleteCalls           int
	describeNotFoundCount int
	createErr             error
	describeErr           error
	getErr                error
}

func generatedSecretCommand(t *testing.T, desired string) domain.WorkspaceServiceCommand {
	t.Helper()
	template, err := servicecatalog.Resolve("postgres")
	if err != nil {
		t.Fatal(err)
	}
	config, err := servicecatalog.CanonicalJSON(template)
	if err != nil {
		t.Fatal(err)
	}
	return domain.WorkspaceServiceCommand{SchemaVersion: 1, TenantID: "tenant_1", WorkspaceID: "ws_1",
		ResourcePlaneID: "rp_1", ServiceID: "svc_1", Revision: 1, DesiredState: desired,
		CatalogVersion: template.Name + "/" + template.Version, Config: config}
}

func (f *serviceSecretsManagerFake) CreateSecret(_ context.Context, input *secretsmanager.CreateSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.CreateSecretOutput, error) {
	f.createCalls++
	if f.createErr != nil {
		return nil, f.createErr
	}
	name := aws.ToString(input.Name)
	if _, exists := f.secrets[name]; exists {
		return nil, &types.ResourceExistsException{}
	}
	token := aws.ToString(input.ClientRequestToken)
	f.secrets[name] = generatedServiceSecret{name: name, value: aws.ToString(input.SecretString), tags: append([]types.Tag(nil), input.Tags...),
		versions: map[string]string{token: aws.ToString(input.SecretString)}, stages: map[string][]string{token: {"AWSCURRENT"}}}
	return &secretsmanager.CreateSecretOutput{Name: aws.String(name)}, nil
}

func (f *serviceSecretsManagerFake) DescribeSecret(_ context.Context, input *secretsmanager.DescribeSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DescribeSecretOutput, error) {
	f.describeCalls++
	if f.describeErr != nil {
		return nil, f.describeErr
	}
	if f.describeNotFoundCount > 0 {
		f.describeNotFoundCount--
		return nil, &types.ResourceNotFoundException{}
	}
	secret, exists := f.secrets[aws.ToString(input.SecretId)]
	if !exists {
		return nil, &types.ResourceNotFoundException{}
	}
	_, stages := fakeSecretVersions(secret)
	return &secretsmanager.DescribeSecretOutput{Name: aws.String(secret.name), Tags: append([]types.Tag(nil), secret.tags...), DeletedDate: secret.deletedAt, VersionIdsToStages: stages}, nil
}

func (f *serviceSecretsManagerFake) GetSecretValue(_ context.Context, input *secretsmanager.GetSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.GetSecretValueOutput, error) {
	f.getCalls++
	if f.getErr != nil {
		return nil, f.getErr
	}
	secret, exists := f.secrets[aws.ToString(input.SecretId)]
	if !exists {
		return nil, &types.ResourceNotFoundException{}
	}
	versions, stages := fakeSecretVersions(secret)
	versionID := aws.ToString(input.VersionId)
	if versionID == "" {
		if aws.ToString(input.VersionStage) != "AWSCURRENT" {
			return nil, errors.New("unexpected secret version stage")
		}
		versionID = secretVersionWithStage(stages, "AWSCURRENT")
	}
	value, ok := versions[versionID]
	if !ok {
		return nil, &types.ResourceNotFoundException{}
	}
	return &secretsmanager.GetSecretValueOutput{Name: aws.String(secret.name), VersionId: aws.String(versionID), SecretString: aws.String(value)}, nil
}

func fakeSecretVersions(secret generatedServiceSecret) (map[string]string, map[string][]string) {
	if len(secret.versions) != 0 {
		versions := make(map[string]string, len(secret.versions))
		stages := make(map[string][]string, len(secret.stages))
		for key, value := range secret.versions {
			versions[key] = value
		}
		for key, value := range secret.stages {
			stages[key] = append([]string(nil), value...)
		}
		return versions, stages
	}
	return map[string]string{"initial": secret.value}, map[string][]string{"initial": {"AWSCURRENT"}}
}

func (f *serviceSecretsManagerFake) PutSecretValue(_ context.Context, input *secretsmanager.PutSecretValueInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.PutSecretValueOutput, error) {
	name, versionID := aws.ToString(input.SecretId), aws.ToString(input.ClientRequestToken)
	secret, exists := f.secrets[name]
	if !exists || versionID == "" {
		return nil, &types.ResourceNotFoundException{}
	}
	versions, stages := fakeSecretVersions(secret)
	value := aws.ToString(input.SecretString)
	if old, ok := versions[versionID]; ok && old != value {
		return nil, errors.New("version token conflict")
	}
	versions[versionID] = value
	stages[versionID] = append([]string(nil), input.VersionStages...)
	secret.versions, secret.stages = versions, stages
	f.secrets[name] = secret
	return &secretsmanager.PutSecretValueOutput{Name: aws.String(name), VersionId: aws.String(versionID), VersionStages: append([]string(nil), input.VersionStages...)}, nil
}

func (f *serviceSecretsManagerFake) UpdateSecretVersionStage(_ context.Context, input *secretsmanager.UpdateSecretVersionStageInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.UpdateSecretVersionStageOutput, error) {
	name, stage := aws.ToString(input.SecretId), aws.ToString(input.VersionStage)
	secret, exists := f.secrets[name]
	if !exists {
		return nil, &types.ResourceNotFoundException{}
	}
	versions, stages := fakeSecretVersions(secret)
	if input.MoveToVersionId != nil {
		to := aws.ToString(input.MoveToVersionId)
		if _, exists := versions[to]; !exists {
			return nil, &types.ResourceNotFoundException{}
		}
		if input.RemoveFromVersionId != nil {
			from := aws.ToString(input.RemoveFromVersionId)
			stages[from] = removeString(stages[from], stage)
		}
		if !containsString(stages[to], stage) {
			stages[to] = append(stages[to], stage)
		}
	} else if input.RemoveFromVersionId != nil {
		from := aws.ToString(input.RemoveFromVersionId)
		stages[from] = removeString(stages[from], stage)
	}
	secret.versions, secret.stages = versions, stages
	secret.value = versions[secretVersionWithStage(stages, "AWSCURRENT")]
	f.secrets[name] = secret
	return &secretsmanager.UpdateSecretVersionStageOutput{Name: aws.String(name)}, nil
}

func containsString(values []string, expected string) bool {
	for _, value := range values {
		if value == expected {
			return true
		}
	}
	return false
}
func removeString(values []string, expected string) []string {
	result := values[:0]
	for _, value := range values {
		if value != expected {
			result = append(result, value)
		}
	}
	return result
}

func (f *serviceSecretsManagerFake) DeleteSecret(_ context.Context, input *secretsmanager.DeleteSecretInput, _ ...func(*secretsmanager.Options)) (*secretsmanager.DeleteSecretOutput, error) {
	f.deleteCalls++
	if input.RecoveryWindowInDays == nil || aws.ToInt64(input.RecoveryWindowInDays) != 7 || aws.ToBool(input.ForceDeleteWithoutRecovery) {
		return nil, errors.New("secret deletion did not use the required recovery window")
	}
	name := aws.ToString(input.SecretId)
	secret, exists := f.secrets[name]
	if !exists {
		return nil, &types.ResourceNotFoundException{}
	}
	deletedAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	secret.deletedAt = &deletedAt
	f.secrets[name] = secret
	return &secretsmanager.DeleteSecretOutput{Name: aws.String(name), DeletionDate: &deletedAt}, nil
}

func TestWorkspaceServiceSecretProviderCreatesStableTenantScopedValue(t *testing.T) {
	ctx := context.Background()
	fake := &serviceSecretsManagerFake{secrets: make(map[string]generatedServiceSecret)}
	provider, err := NewAWSWorkspaceServiceSecretProvider(fake)
	if err != nil {
		t.Fatal(err)
	}
	command := generatedSecretCommand(t, "running")
	first, err := provider.Resolve(ctx, command)
	if err != nil || len(first) != 1 || !validGeneratedServiceSecret(first["POSTGRES_PASSWORD"]) {
		t.Fatalf("resolve new generated credential: values=%v err=%v", first, err)
	}
	second, err := provider.Resolve(ctx, command)
	if err != nil || second["POSTGRES_PASSWORD"] != first["POSTGRES_PASSWORD"] || fake.createCalls != 1 || fake.getCalls != 1 {
		t.Fatalf("credential was not stable across retry: second=%v calls create=%d get=%d err=%v", second, fake.createCalls, fake.getCalls, err)
	}
	name := "hako/workspaces/tenant_1/ws_1/services/svc_1/POSTGRES_PASSWORD"
	if fake.secrets[name].value != first["POSTGRES_PASSWORD"] {
		t.Fatalf("secret stored under unexpected scoped name: %q", name)
	}
	otherTenant := command
	otherTenant.TenantID = domain.TenantID("tenant_2")
	third, err := provider.Resolve(ctx, otherTenant)
	if err != nil || third["POSTGRES_PASSWORD"] == first["POSTGRES_PASSWORD"] || fake.createCalls != 2 {
		t.Fatalf("Tenant credential was not isolated: values=%v creates=%d err=%v", third, fake.createCalls, err)
	}
	if strings.Contains(string(command.Config), first["POSTGRES_PASSWORD"]) {
		t.Fatal("generated credential was written into the non-secret command config")
	}
}

func TestWorkspaceServiceSecretProviderStagesAndCommitsIdempotentRotation(t *testing.T) {
	ctx := context.Background()
	fake := &serviceSecretsManagerFake{secrets: make(map[string]generatedServiceSecret)}
	provider, err := NewAWSWorkspaceServiceSecretProvider(fake)
	if err != nil {
		t.Fatal(err)
	}
	command := generatedSecretCommand(t, "running")
	initial, err := provider.Resolve(ctx, command)
	if err != nil {
		t.Fatal(err)
	}
	rotatedCommand := command
	rotatedCommand.Revision = 2
	rotatedCommand.CredentialGeneration = 2
	pending, err := provider.Resolve(ctx, rotatedCommand)
	if err != nil || pending["POSTGRES_PASSWORD"] == initial["POSTGRES_PASSWORD"] {
		t.Fatalf("rotation did not stage a new credential: pending=%v initial=%v err=%v", pending, initial, err)
	}
	retry, err := provider.Resolve(ctx, rotatedCommand)
	if err != nil || retry["POSTGRES_PASSWORD"] != pending["POSTGRES_PASSWORD"] {
		t.Fatalf("rotation retry did not resolve the same staged credential: retry=%v pending=%v err=%v", retry, pending, err)
	}
	if err := provider.CommitRotation(ctx, rotatedCommand); err != nil {
		t.Fatalf("commit staged credential: %v", err)
	}
	if err := provider.CommitRotation(ctx, rotatedCommand); err != nil {
		t.Fatalf("commit replay: %v", err)
	}
	current, err := provider.Resolve(ctx, rotatedCommand)
	if err != nil || current["POSTGRES_PASSWORD"] != pending["POSTGRES_PASSWORD"] {
		t.Fatalf("committed credential was not stable on restart: current=%v pending=%v err=%v", current, pending, err)
	}
	name := workspaceServiceSecretName(command, "POSTGRES_PASSWORD")
	versions, stages := fakeSecretVersions(fake.secrets[name])
	versionID := workspaceServiceCredentialVersionID(rotatedCommand, "POSTGRES_PASSWORD")
	if versions[versionID] != pending["POSTGRES_PASSWORD"] || !hasSecretVersionStage(stages, versionID, "AWSCURRENT") || hasSecretVersionStage(stages, versionID, "AWSPENDING") {
		t.Fatalf("rotation stages after commit = %#v", stages)
	}
}

func TestWorkspaceServiceSecretProviderHandlesConcurrentCreateAndRejectsForeignSecret(t *testing.T) {
	command := generatedSecretCommand(t, "running")
	name := workspaceServiceSecretName(command, "POSTGRES_PASSWORD")
	stored := "0123456789012345678901234567890123456789012"
	fake := &serviceSecretsManagerFake{secrets: map[string]generatedServiceSecret{
		name: {name: name, value: stored, tags: workspaceServiceSecretTags(command, "POSTGRES_PASSWORD")},
	}}
	provider, err := NewAWSWorkspaceServiceSecretProvider(fake)
	if err != nil {
		t.Fatal(err)
	}
	values, err := provider.Resolve(context.Background(), command)
	if err != nil || values["POSTGRES_PASSWORD"] != stored || fake.createCalls != 0 || fake.getCalls != 1 {
		t.Fatalf("resolve existing secret: values=%v calls=%+v err=%v", values, fake, err)
	}
	fake.secrets[name] = generatedServiceSecret{name: name, value: stored, tags: workspaceServiceSecretTags(command, "POSTGRES_PASSWORD")[:4]}
	if _, err := provider.Resolve(context.Background(), command); !errors.Is(err, ErrWorkspaceServiceSecret) {
		t.Fatalf("secret with incomplete ownership tags was accepted: %v", err)
	}
}

func TestWorkspaceServiceSecretProviderResolvesCreateRaceAndRejectsPathInjection(t *testing.T) {
	command := generatedSecretCommand(t, "running")
	name := workspaceServiceSecretName(command, "POSTGRES_PASSWORD")
	stored := "0123456789012345678901234567890123456789012"
	fake := &serviceSecretsManagerFake{describeNotFoundCount: 1, secrets: map[string]generatedServiceSecret{
		name: {name: name, value: stored, tags: workspaceServiceSecretTags(command, "POSTGRES_PASSWORD")},
	}}
	provider, err := NewAWSWorkspaceServiceSecretProvider(fake)
	if err != nil {
		t.Fatal(err)
	}
	values, err := provider.Resolve(context.Background(), command)
	if err != nil || values["POSTGRES_PASSWORD"] != stored || fake.createCalls != 1 || fake.getCalls != 1 {
		t.Fatalf("concurrent create did not converge on the owned secret: values=%v calls=%+v err=%v", values, fake, err)
	}
	badIdentity := command
	badIdentity.WorkspaceID = "ws_1/foreign"
	if _, err := provider.Resolve(context.Background(), badIdentity); !errors.Is(err, ErrWorkspaceServiceSecret) || fake.createCalls != 1 {
		t.Fatalf("secret path accepted injected Workspace identity: calls=%+v err=%v", fake, err)
	}
}

func TestWorkspaceServiceSecretProviderDoesNotReadSecretsForStopAndFailsClosed(t *testing.T) {
	fake := &serviceSecretsManagerFake{secrets: make(map[string]generatedServiceSecret)}
	provider, err := NewAWSWorkspaceServiceSecretProvider(fake)
	if err != nil {
		t.Fatal(err)
	}
	stop := generatedSecretCommand(t, "stopped")
	values, err := provider.Resolve(context.Background(), stop)
	if err != nil || len(values) != 0 || fake.createCalls != 0 || fake.describeCalls != 0 || fake.getCalls != 0 {
		t.Fatalf("stop command accessed generated credentials: values=%v calls=%+v err=%v", values, fake, err)
	}
	fake.createErr = errors.New("sensitive provider diagnostic")
	if _, err := provider.Resolve(context.Background(), generatedSecretCommand(t, "running")); !errors.Is(err, ErrWorkspaceServiceSecret) || strings.Contains(err.Error(), "sensitive") {
		t.Fatalf("provider error was not redacted: %v", err)
	}
}

func TestWorkspaceServiceSecretProviderRetiresOnlyOwnedSecretsWithRecovery(t *testing.T) {
	ctx := context.Background()
	command := generatedSecretCommand(t, "deleted")
	name := workspaceServiceSecretName(command, "POSTGRES_PASSWORD")
	fake := &serviceSecretsManagerFake{secrets: map[string]generatedServiceSecret{
		name: {name: name, value: "0123456789012345678901234567890123456789012", tags: workspaceServiceSecretTags(command, "POSTGRES_PASSWORD")},
	}}
	provider, err := NewAWSWorkspaceServiceSecretProvider(fake)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.Retire(ctx, command); err != nil || fake.deleteCalls != 1 || fake.secrets[name].deletedAt == nil {
		t.Fatalf("owned Service secret was not scheduled for recovery-window deletion: calls=%d secret=%+v err=%v", fake.deleteCalls, fake.secrets[name], err)
	}
	if err := provider.Retire(ctx, command); err != nil || fake.deleteCalls != 1 {
		t.Fatalf("replayed retirement was not idempotent: calls=%d err=%v", fake.deleteCalls, err)
	}
	foreign := &serviceSecretsManagerFake{secrets: map[string]generatedServiceSecret{
		name: {name: name, value: "secret", tags: workspaceServiceSecretTags(command, "POSTGRES_PASSWORD")[:4]},
	}}
	foreignProvider, _ := NewAWSWorkspaceServiceSecretProvider(foreign)
	if err := foreignProvider.Retire(ctx, command); !errors.Is(err, ErrWorkspaceServiceSecret) || foreign.deleteCalls != 0 {
		t.Fatalf("retirement deleted a secret with foreign ownership tags: calls=%d err=%v", foreign.deleteCalls, err)
	}
	if err := provider.Retire(ctx, generatedSecretCommand(t, "stopped")); !errors.Is(err, ErrWorkspaceServiceSecret) {
		t.Fatalf("non-delete Service command was allowed to retire a secret: %v", err)
	}
}
