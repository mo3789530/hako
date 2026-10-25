package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-lambda-go/lambda"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/secretsmanager"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/api"
	"github.com/mo3789530/hako/internal/apigwlambda"
	"github.com/mo3789530/hako/internal/auth"
	"github.com/mo3789530/hako/internal/deploypolicy"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	webhookinbox "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	databasePool, err := openDatabase(ctx)
	if err != nil {
		log.Fatal("connect Hako API database failed (failure_class=database_configuration)")
	}
	defer databasePool.Close()

	issuer := os.Getenv("HAKO_COGNITO_ISSUER")
	clientID := os.Getenv("HAKO_COGNITO_CLIENT_ID")
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: clientID})
	if err != nil {
		log.Fatal("configure Cognito verifier failed (failure_class=authentication_configuration)")
	}
	runtimeClass := strings.TrimSpace(os.Getenv("HAKO_DEFAULT_WORKSPACE_RUNTIME_CLASS"))
	if runtimeClass == "" {
		runtimeClass = "standard"
	}
	createConfig := api.WorkspaceCreateConfig{
		Region:                         strings.TrimSpace(os.Getenv("HAKO_DEFAULT_RESOURCE_PLANE_REGION")),
		RequiredCapabilities:           []string{"microvm"},
		RuntimeClass:                   runtimeClass,
		Image:                          strings.TrimSpace(os.Getenv("HAKO_DEFAULT_WORKSPACE_IMAGE")),
		WorkloadRunsEnabled:            strings.EqualFold(strings.TrimSpace(os.Getenv("HAKO_WORKLOAD_RUNS_ENABLED")), "true"),
		WebhookWorkloadTriggersEnabled: strings.EqualFold(strings.TrimSpace(os.Getenv("HAKO_WEBHOOK_WORKLOAD_TRIGGERS_ENABLED")), "true"),
		GitHubAppManagementEnabled:     strings.EqualFold(strings.TrimSpace(os.Getenv("HAKO_GITHUB_APP_MANAGEMENT_ENABLED")), "true"),
		GitHubOIDCPolicyVerifier:       deploypolicy.NewGitHubVerifier(nil),
	}
	healthReportKeyResolver, err := resourcePlaneHealthReportKeyResolverFromEnv(ctx)
	if err != nil {
		log.Fatal("configure Resource Plane health report credentials")
	}
	createConfig.ResourcePlaneHealthReportKeyResolver = healthReportKeyResolver
	serviceReportKeyResolver, err := resourcePlaneServiceReportKeyResolverFromEnv(ctx)
	if err != nil {
		log.Fatal("configure Resource Plane Workspace Service report credentials")
	}
	createConfig.ResourcePlaneServiceReportKeyResolver = serviceReportKeyResolver
	workloadSpecKeyResolver, err := resourcePlaneWorkloadSpecKeyResolverFromEnv(ctx)
	if err != nil {
		log.Fatal("configure Resource Plane Workload specification credentials")
	}
	createConfig.ResourcePlaneWorkloadSpecKeyResolver = workloadSpecKeyResolver
	artifactCleanupKeyResolver, err := resourcePlaneArtifactCleanupKeyResolverFromEnv(ctx)
	if err != nil {
		log.Fatal("configure Resource Plane Artifact cleanup credentials")
	}
	createConfig.ResourcePlaneArtifactCleanupKeyResolver = artifactCleanupKeyResolver
	workspaceSessionKeyResolver, err := resourcePlaneWorkspaceSessionKeyResolverFromEnv(ctx)
	if err != nil {
		log.Fatal("configure Resource Plane Workspace Session credentials")
	}
	createConfig.ResourcePlaneWorkspaceSessionKeyResolver = workspaceSessionKeyResolver
	if createConfig.WebhookWorkloadTriggersEnabled && !createConfig.WorkloadRunsEnabled {
		log.Fatal("Webhook-triggered Workload Runs require HAKO_WORKLOAD_RUNS_ENABLED=true")
	}
	tenantSetupResolver, err := tenantOAuthBrokerFromEnv()
	if err != nil {
		log.Fatal("configure Tenant GitHub OAuth broker failed (failure_class=broker_configuration)")
	}
	if tenantSetupResolver != nil && !createConfig.GitHubAppManagementEnabled {
		log.Fatal("Tenant GitHub OAuth broker requires HAKO_GITHUB_APP_MANAGEMENT_ENABLED=true")
	}
	createConfig.TenantGitHubSetupResolver = tenantSetupResolver
	environmentApprovalVerifier, err := tenantEnvironmentApprovalBrokerFromEnv()
	if err != nil {
		log.Fatal("configure GitHub Environment approval verifier failed (failure_class=broker_configuration)")
	}
	createConfig.GitHubEnvironmentApprovalVerifier = environmentApprovalVerifier
	agentProposalSubmitter, err := tenantAgentProposalBrokerFromEnv()
	if err != nil {
		log.Fatal("configure Agent proposal publication Broker failed (failure_class=broker_configuration)")
	}
	createConfig.AgentProposalPublicationSubmitter = agentProposalSubmitter
	if strings.EqualFold(strings.TrimSpace(os.Getenv("HAKO_TENANT_GITHUB_WEBHOOK_ENABLED")), "true") {
		webhookRateLimit, err := tenantWebhookRateLimitFromEnv()
		if err != nil {
			log.Fatal("configure Tenant GitHub Webhook rate limit failed (failure_class=configuration)")
		}
		awsConfig, err := config.LoadDefaultConfig(ctx)
		if err != nil {
			log.Fatal("configure Tenant GitHub Webhook-purpose Secret reader")
		}
		resolver := api.TenantGitHubWebhookCredentialResolver{Pool: databasePool, Policy: transaction.DefaultPolicy(), Config: awsConfig}
		limiter := api.TenantGitHubWebhookAttemptLimiter{Pool: databasePool, Policy: transaction.DefaultPolicy(), MaxAttempts: webhookRateLimit}
		handler, err := githubwebhook.NewTenantHTTPHandler(resolver, githubregistry.CredentialAuthorizer{Pool: databasePool, Policy: transaction.DefaultPolicy()}, webhookinbox.Inbox{Pool: databasePool, Policy: transaction.DefaultPolicy()}, githubwebhook.DefaultMaxBodyBytes, limiter)
		if err != nil {
			log.Fatal("configure Tenant GitHub webhook handler failed (failure_class=handler_configuration)")
		}
		createConfig.TenantGitHubWebhookHandler = handler
	}
	var webhookHandler http.Handler
	secretARN := strings.TrimSpace(os.Getenv("HAKO_GITHUB_WEBHOOK_SECRET_ARN"))
	if secretARN != "" {
		secret, err := loadGitHubWebhookSecret(ctx, secretARN)
		if err != nil {
			log.Fatal("load GitHub webhook secret failed (failure_class=secret_configuration)")
		}
		webhookHandler, err = githubwebhook.NewHTTPHandler(secret, webhookinbox.Inbox{Pool: databasePool, Policy: transaction.DefaultPolicy()}, githubwebhook.DefaultMaxBodyBytes)
		if err != nil {
			log.Fatal("configure GitHub webhook handler failed (failure_class=handler_configuration)")
		}
	}
	var setupVerifier githubapp.SetupVerifier
	appSlug := strings.TrimSpace(os.Getenv("HAKO_GITHUB_APP_SLUG"))
	appIDValue := strings.TrimSpace(os.Getenv("HAKO_GITHUB_APP_ID"))
	appClientID := strings.TrimSpace(os.Getenv("HAKO_GITHUB_APP_CLIENT_ID"))
	appClientSecretARN := strings.TrimSpace(os.Getenv("HAKO_GITHUB_APP_CLIENT_SECRET_ARN"))
	appCallbackURL := strings.TrimSpace(os.Getenv("HAKO_GITHUB_APP_CALLBACK_URL"))
	setupValues := []string{appSlug, appIDValue, appClientID, appClientSecretARN, appCallbackURL}
	configuredSetupValues := 0
	for _, value := range setupValues {
		if value != "" {
			configuredSetupValues++
		}
	}
	if configuredSetupValues != 0 && configuredSetupValues != len(setupValues) {
		log.Fatal("configure all GitHub App setup variables together, including HAKO_GITHUB_APP_CALLBACK_URL")
	}
	if configuredSetupValues == len(setupValues) {
		appID, err := strconv.ParseInt(appIDValue, 10, 64)
		if err != nil || appID <= 0 {
			log.Fatal("HAKO_GITHUB_APP_ID must be a positive integer")
		}
		clientSecret, err := loadSecretString(ctx, appClientSecretARN, "GitHub App OAuth client secret")
		if err != nil {
			log.Fatal("load GitHub App OAuth client secret failed (failure_class=secret_configuration)")
		}
		setupVerifier, err = githubapp.NewSetupClient(githubapp.SetupConfig{Slug: appSlug, AppID: appID, ClientID: appClientID, ClientSecret: string(clientSecret), CallbackURL: appCallbackURL})
		if err != nil {
			log.Fatal("configure GitHub App setup verifier failed (failure_class=integration_configuration)")
		}
	}
	handler := api.NewHandlerWithGitHubApp(verifier, databasePool, webhookHandler, setupVerifier, createConfig)
	// The ZIP deployment uses the Go Lambda runtime directly. The OCI image
	// includes Lambda Web Adapter and must run this same app as an HTTP server.
	if shouldStartLambdaRuntime(os.Getenv("AWS_LAMBDA_RUNTIME_API"), os.Getenv("HAKO_API_HTTP_MODE")) {
		lambda.Start(apigwlambda.New(handler).Handle)
		return
	}
	addr := os.Getenv("HAKO_API_LISTEN_ADDR")
	if addr == "" {
		addr = "127.0.0.1:8080"
	}
	server := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	errCh := make(chan error, 1)
	go func() {
		log.Printf("Hako API listening on %s", addr)
		errCh <- server.ListenAndServe()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, http.ErrServerClosed) {
			log.Fatal("serve Hako API failed (failure_class=http_server)")
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown Hako API failed (failure_class=http_shutdown)")
		}
	}
}

var resourcePlaneSecretARNPattern = regexp.MustCompile(`^arn:[^:]+:secretsmanager:[^:]+:[0-9]{12}:secret:.+$`)
var resourcePlaneIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,31}$`)

func resourcePlaneHealthReportKeyResolverFromEnv(ctx context.Context) (api.ResourcePlaneHealthReportKeyResolver, error) {
	raw := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_HEALTH_REPORT_SECRET_ARNS"))
	if raw == "" {
		return nil, nil
	}
	secretARNs, err := parseResourcePlaneHealthSecretARNMap(raw)
	if err != nil {
		return nil, err
	}
	if len(secretARNs) == 0 {
		return nil, nil
	}
	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, errors.New("configure Resource Plane health report Secret reader")
	}
	secretClient := secretsmanager.NewFromConfig(awsConfig)
	return func(ctx context.Context, resourcePlaneID domain.ResourcePlaneID) ([]byte, error) {
		secretARN, ok := secretARNs[string(resourcePlaneID)]
		if !ok {
			return nil, errors.New("health report identity is not configured")
		}
		output, err := secretClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &secretARN})
		if err != nil || output.SecretString == nil || len(*output.SecretString) < 32 {
			return nil, errors.New("health report key is unavailable")
		}
		return []byte(*output.SecretString), nil
	}, nil
}

func resourcePlaneServiceReportKeyResolverFromEnv(ctx context.Context) (api.ResourcePlaneServiceReportKeyResolver, error) {
	raw := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_SERVICE_REPORT_SECRET_ARNS"))
	if raw == "" {
		return nil, nil
	}
	secretARNs, err := parseResourcePlaneSecretARNMap(raw)
	if err != nil {
		return nil, errors.New("Resource Plane Workspace Service report Secret map is invalid")
	}
	if len(secretARNs) == 0 {
		return nil, nil
	}
	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, errors.New("configure Workspace Service report Secret reader")
	}
	secretClient := secretsmanager.NewFromConfig(awsConfig)
	return func(ctx context.Context, resourcePlaneID domain.ResourcePlaneID) ([]byte, error) {
		secretARN, ok := secretARNs[string(resourcePlaneID)]
		if !ok {
			return nil, errors.New("Workspace Service report identity is not configured")
		}
		output, err := secretClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &secretARN})
		if err != nil || output.SecretString == nil || len(*output.SecretString) < 32 {
			return nil, errors.New("Workspace Service report key is unavailable")
		}
		return []byte(*output.SecretString), nil
	}, nil
}

func parseResourcePlaneHealthSecretARNMap(raw string) (map[string]string, error) {
	return parseResourcePlaneSecretARNMap(raw)
}

func parseResourcePlaneSecretARNMap(raw string) (map[string]string, error) {
	secretARNs := make(map[string]string)
	if err := json.Unmarshal([]byte(raw), &secretARNs); err != nil || len(secretARNs) > 100 {
		return nil, errors.New("Resource Plane Secret configuration must be a JSON object with up to 100 entries")
	}
	seenARNs := make(map[string]struct{}, len(secretARNs))
	for resourcePlaneID, secretARN := range secretARNs {
		if !resourcePlaneIDPattern.MatchString(resourcePlaneID) || !resourcePlaneSecretARNPattern.MatchString(secretARN) {
			return nil, errors.New("Resource Plane Secret configuration contains an invalid ID or ARN")
		}
		if _, exists := seenARNs[secretARN]; exists {
			return nil, errors.New("Resource Plane Secret ARNs must be distinct")
		}
		seenARNs[secretARN] = struct{}{}
	}
	return secretARNs, nil
}

func resourcePlaneWorkloadSpecKeyResolverFromEnv(ctx context.Context) (api.ResourcePlaneWorkloadSpecKeyResolver, error) {
	raw := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_WORKLOAD_SPEC_SECRET_ARNS"))
	if raw == "" {
		return nil, nil
	}
	secretARNs, err := parseResourcePlaneSecretARNMap(raw)
	if err != nil {
		return nil, errors.New("Resource Plane Workload specification Secret map is invalid")
	}
	if len(secretARNs) == 0 {
		return nil, nil
	}
	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, errors.New("configure Resource Plane Workload specification Secret reader")
	}
	secretClient := secretsmanager.NewFromConfig(awsConfig)
	return func(ctx context.Context, resourcePlaneID domain.ResourcePlaneID) ([]byte, error) {
		secretARN, ok := secretARNs[string(resourcePlaneID)]
		if !ok {
			return nil, errors.New("Workload specification identity is not configured")
		}
		output, err := secretClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &secretARN})
		if err != nil || output == nil || output.SecretString == nil || len(*output.SecretString) < 32 {
			return nil, errors.New("Workload specification key is unavailable")
		}
		return []byte(*output.SecretString), nil
	}, nil
}

func resourcePlaneArtifactCleanupKeyResolverFromEnv(ctx context.Context) (api.ResourcePlaneArtifactCleanupKeyResolver, error) {
	raw := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_ARTIFACT_CLEANUP_SECRET_ARNS"))
	if raw == "" {
		return nil, nil
	}
	secretARNs, err := parseResourcePlaneSecretARNMap(raw)
	if err != nil {
		return nil, errors.New("Resource Plane Artifact cleanup Secret map is invalid")
	}
	if len(secretARNs) == 0 {
		return nil, nil
	}
	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, errors.New("configure Resource Plane Artifact cleanup Secret reader")
	}
	secretClient := secretsmanager.NewFromConfig(awsConfig)
	return func(ctx context.Context, resourcePlaneID domain.ResourcePlaneID) ([]byte, error) {
		secretARN, ok := secretARNs[string(resourcePlaneID)]
		if !ok {
			return nil, errors.New("Artifact cleanup identity is not configured")
		}
		output, err := secretClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &secretARN})
		if err != nil || output == nil || output.SecretString == nil || len(*output.SecretString) < 32 || len(*output.SecretString) > 4096 {
			return nil, errors.New("Artifact cleanup key is unavailable")
		}
		return []byte(*output.SecretString), nil
	}, nil
}

func resourcePlaneWorkspaceSessionKeyResolverFromEnv(ctx context.Context) (api.ResourcePlaneWorkspaceSessionKeyResolver, error) {
	raw := strings.TrimSpace(os.Getenv("HAKO_RESOURCE_PLANE_WORKSPACE_SESSION_SECRET_ARNS"))
	if raw == "" {
		return nil, nil
	}
	secretARNs, err := parseResourcePlaneSecretARNMap(raw)
	if err != nil {
		return nil, errors.New("Resource Plane Workspace Session Secret map is invalid")
	}
	if len(secretARNs) == 0 {
		return nil, nil
	}
	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, errors.New("configure Resource Plane Workspace Session Secret reader")
	}
	secretClient := secretsmanager.NewFromConfig(awsConfig)
	return func(ctx context.Context, resourcePlaneID domain.ResourcePlaneID) ([]byte, error) {
		secretARN, ok := secretARNs[string(resourcePlaneID)]
		if !ok {
			return nil, errors.New("Workspace Session identity is not configured")
		}
		output, err := secretClient.GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &secretARN})
		if err != nil || output == nil || output.SecretString == nil || len(*output.SecretString) < 32 || len(*output.SecretString) > 4096 {
			return nil, errors.New("Workspace Session key is unavailable")
		}
		return []byte(*output.SecretString), nil
	}, nil
}

func loadGitHubWebhookSecret(ctx context.Context, secretARN string) ([]byte, error) {
	return loadSecretString(ctx, secretARN, "GitHub webhook secret")
}

func loadSecretString(ctx context.Context, secretARN, name string) ([]byte, error) {
	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("load AWS configuration for %s", name)
	}
	result, err := secretsmanager.NewFromConfig(awsConfig).GetSecretValue(ctx, &secretsmanager.GetSecretValueInput{SecretId: &secretARN})
	if err != nil {
		return nil, fmt.Errorf("read %s from Secrets Manager", name)
	}
	if result.SecretString == nil || strings.TrimSpace(*result.SecretString) == "" {
		return nil, fmt.Errorf("%s must be a non-empty SecretString", name)
	}
	return []byte(*result.SecretString), nil
}

func shouldStartLambdaRuntime(runtimeAPI, httpMode string) bool {
	return strings.TrimSpace(runtimeAPI) != "" && strings.TrimSpace(httpMode) != "true"
}

func openDatabase(ctx context.Context) (*pgxpool.Pool, error) {
	postgresURL := strings.TrimSpace(os.Getenv("HAKO_DATABASE_URL"))
	dsqlHost := strings.TrimSpace(os.Getenv("HAKO_DSQL_HOST"))
	if postgresURL != "" && dsqlHost != "" {
		return nil, errors.New("set either HAKO_DATABASE_URL or HAKO_DSQL_HOST, not both")
	}
	if postgresURL != "" {
		return dsql.OpenPostgres(ctx, postgresURL)
	}
	config := dsql.Config{
		Host:     dsqlHost,
		Region:   os.Getenv("AWS_REGION"),
		User:     strings.TrimSpace(os.Getenv("HAKO_DSQL_USER")),
		Database: os.Getenv("HAKO_DSQL_DATABASE"),
	}
	if config.User == "" {
		config.User = "admin"
	}
	return dsql.Open(ctx, config)
}
