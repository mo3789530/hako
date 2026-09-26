package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
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
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubwebhook"
	"github.com/mo3789530/hako/internal/store/dsql"
	webhookinbox "github.com/mo3789530/hako/internal/store/githubwebhook"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	databasePool, err := openDatabase(ctx)
	if err != nil {
		log.Fatalf("connect Hako API database: %v", err)
	}
	defer databasePool.Close()

	issuer := os.Getenv("HAKO_COGNITO_ISSUER")
	clientID := os.Getenv("HAKO_COGNITO_CLIENT_ID")
	verifier, err := auth.NewCognitoVerifier(auth.CognitoVerifierConfig{Issuer: issuer, ClientID: clientID})
	if err != nil {
		log.Fatalf("configure Cognito verifier: %v", err)
	}
	runtimeClass := strings.TrimSpace(os.Getenv("HAKO_DEFAULT_WORKSPACE_RUNTIME_CLASS"))
	if runtimeClass == "" {
		runtimeClass = "standard"
	}
	createConfig := api.WorkspaceCreateConfig{
		Region:               strings.TrimSpace(os.Getenv("HAKO_DEFAULT_RESOURCE_PLANE_REGION")),
		RequiredCapabilities: []string{"microvm"},
		RuntimeClass:         runtimeClass,
		Image:                strings.TrimSpace(os.Getenv("HAKO_DEFAULT_WORKSPACE_IMAGE")),
	}
	var webhookHandler http.Handler
	secretARN := strings.TrimSpace(os.Getenv("HAKO_GITHUB_WEBHOOK_SECRET_ARN"))
	if secretARN != "" {
		secret, err := loadGitHubWebhookSecret(ctx, secretARN)
		if err != nil {
			log.Fatalf("load GitHub webhook secret: %v", err)
		}
		webhookHandler, err = githubwebhook.NewHTTPHandler(secret, webhookinbox.Inbox{Pool: databasePool, Policy: transaction.DefaultPolicy()}, githubwebhook.DefaultMaxBodyBytes)
		if err != nil {
			log.Fatalf("configure GitHub webhook handler: %v", err)
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
			log.Fatalf("load GitHub App OAuth client secret: %v", err)
		}
		setupVerifier, err = githubapp.NewSetupClient(githubapp.SetupConfig{Slug: appSlug, AppID: appID, ClientID: appClientID, ClientSecret: string(clientSecret), CallbackURL: appCallbackURL})
		if err != nil {
			log.Fatalf("configure GitHub App setup verifier: %v", err)
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
			log.Fatalf("serve Hako API: %v", err)
		}
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			log.Printf("shutdown Hako API: %v", err)
		}
	}
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
