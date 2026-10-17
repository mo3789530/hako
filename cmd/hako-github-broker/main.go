package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/githubregistry"
	"github.com/mo3789530/hako/internal/store/transaction"
)

const defaultListenAddr = ":8443"

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	pool, err := openDatabase(ctx)
	if err != nil {
		return errors.New("open GitHub broker database")
	}
	defer pool.Close()

	awsConfig, err := config.LoadDefaultConfig(ctx)
	if err != nil {
		return errors.New("load broker AWS configuration")
	}
	callbackURL := strings.TrimSpace(os.Getenv("HAKO_GITHUB_BROKER_CALLBACK_URL"))
	if callbackURL == "" {
		return errors.New("HAKO_GITHUB_BROKER_CALLBACK_URL is required")
	}
	checkoutCallers, err := configuredCheckoutCallers(os.Getenv("HAKO_GITHUB_BROKER_CHECKOUT_RESOURCE_PLANE_IDS"))
	if err != nil {
		return err
	}
	allowedCheckoutCallers := make(map[string]struct{}, len(checkoutCallers)+1)
	allowedCheckoutCallers["spiffe://hako/control-plane"] = struct{}{}
	for _, caller := range checkoutCallers {
		allowedCheckoutCallers[caller] = struct{}{}
	}
	policy := transaction.DefaultPolicy()
	oauth, err := githubbroker.NewOAuthService(func(ctx context.Context, pin githubregistry.SetupAppPin) error {
		caller, ok := githubbroker.CallerFromContext(ctx)
		if !ok || caller != "spiffe://hako/control-plane" {
			return githubbroker.ErrUnavailable
		}
		return nil
	}, func(ctx context.Context, pin githubregistry.SetupAppPin) (githubregistry.SetupCredential, error) {
		return transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (githubregistry.SetupCredential, error) {
			return githubregistry.ResolveSetupCredential(ctx, tx, pin)
		})
	}, func(ctx context.Context, credential githubregistry.SetupCredential) (string, error) {
		access := githubapp.TenantOAuthAccess{TenantID: string(credential.Pin.TenantID), IntegrationID: credential.Pin.IntegrationID,
			AppID: credential.Pin.GitHubAppID, ConfigRevision: credential.Pin.ConfigRevision,
			CredentialGeneration: credential.Pin.CredentialGeneration, ReaderRoleARN: credential.ReaderRoleARN,
			SecretARN: credential.SecretARN, VersionID: credential.VersionID}
		reader, err := githubapp.NewTenantOAuthSecretReaderFromConfig(access, awsConfig)
		if err != nil {
			return "", githubbroker.ErrUnavailable
		}
		return reader.Read(ctx)
	}, callbackURL)
	if err != nil {
		return errors.New("configure OAuth broker service")
	}
	resolveRun := githubbroker.NewPostgresCheckoutRunResolver(pool, policy)
	mintToken, err := githubbroker.NewAWSCheckoutTokenMinter(awsConfig, pool, policy, nil)
	if err != nil {
		return errors.New("configure checkout broker token minter")
	}
	checkout, err := githubbroker.NewCheckoutService(func(_ context.Context, caller string, _ domain.TenantID, _ domain.WorkloadRunID) error {
		if _, ok := allowedCheckoutCallers[caller]; !ok {
			return githubbroker.ErrUnavailable
		}
		return nil
	}, resolveRun, mintToken)
	if err != nil {
		return errors.New("configure checkout broker service")
	}
	resolveChecksRun := githubbroker.NewPostgresChecksRunResolver(pool, policy)
	mintChecksToken, err := githubbroker.NewAWSChecksTokenMinter(awsConfig, pool, policy, nil)
	if err != nil {
		return errors.New("configure GitHub Checks token minter")
	}
	checks, err := githubbroker.NewChecksService(func(_ context.Context, caller string, _ domain.TenantID, _ domain.WorkloadRunID) error {
		if caller != "spiffe://hako/control-plane" {
			return githubbroker.ErrUnavailable
		}
		return nil
	}, resolveChecksRun, mintChecksToken)
	if err != nil {
		return errors.New("configure GitHub Checks broker service")
	}
	actionsRunnerMinter, err := githubbroker.NewAWSActionsRunnerRegistrationTokenMinter(awsConfig, pool, policy, nil)
	if err != nil {
		return errors.New("configure GitHub Actions runner token minter")
	}
	actionsRunner, err := githubbroker.NewActionsRunnerRegistrationService(
		githubbroker.NewPostgresActionsRunnerJobStarter(pool, policy),
		githubbroker.NewPostgresActionsRunnerJobFailure(pool, policy),
		actionsRunnerMinter,
	)
	if err != nil {
		return errors.New("configure GitHub Actions runner broker service")
	}
	runnerCleanupCallers, err := configuredRunnerCleanupCallers(os.Getenv("HAKO_GITHUB_BROKER_ACTIONS_RUNNER_RESOURCE_PLANE_IDS"))
	if err != nil {
		return err
	}
	runnerDeregistrator, err := githubbroker.NewAWSActionsRunnerDeregistrator(awsConfig, pool, policy, nil)
	if err != nil {
		return errors.New("configure GitHub Actions runner deregistration broker")
	}
	runnerCleanup, err := githubbroker.NewActionsRunnerCleanupService(githubbroker.NewPostgresActionsRunnerCleanupResolver(pool, policy), runnerDeregistrator)
	if err != nil {
		return errors.New("configure GitHub Actions runner cleanup broker")
	}
	runnerBindingIDs := make([]string, 0, len(runnerCleanupCallers))
	for _, caller := range runnerCleanupCallers {
		runnerBindingIDs = append(runnerBindingIDs, strings.TrimPrefix(caller, "spiffe://hako/resource-plane/"))
	}
	var runnerBinding *githubbroker.ActionsRunnerBindingService
	if len(runnerBindingIDs) > 0 {
		runnerBinding, err = githubbroker.NewActionsRunnerBindingService(runnerBindingIDs, githubbroker.NewPostgresActionsRunnerBinder(pool, policy))
		if err != nil {
			return errors.New("configure GitHub Actions runner claim binding service")
		}
	}
	var runnerBootstrapToken *githubbroker.ActionsRunnerBootstrapTokenService
	if len(runnerBindingIDs) > 0 {
		runnerBootstrapToken, err = githubbroker.NewActionsRunnerBootstrapTokenService(runnerBindingIDs,
			githubbroker.NewPostgresActionsRunnerBootstrapTokenStarter(pool, policy),
			githubbroker.NewPostgresActionsRunnerJobFailure(pool, policy), actionsRunnerMinter)
		if err != nil {
			return errors.New("configure GitHub Actions runner bootstrap token broker")
		}
	}
	tlsConfig, err := serverTLSConfigFromEnv()
	if err != nil {
		return err
	}
	addr := strings.TrimSpace(os.Getenv("HAKO_GITHUB_BROKER_LISTEN_ADDR"))
	if addr == "" {
		addr = defaultListenAddr
	}
	server := &http.Server{Addr: addr, Handler: githubbroker.HandlerWithActionsRunnerBrokerLifecycle(oauth, checkout, checks, checkoutCallers, actionsRunner, runnerCleanup, runnerCleanupCallers, runnerBinding, runnerCleanupCallers, runnerBootstrapToken, runnerCleanupCallers), TLSConfig: tlsConfig,
		ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 10 * time.Second, WriteTimeout: 60 * time.Second,
		IdleTimeout: 60 * time.Second, MaxHeaderBytes: 16 << 10}
	go func() {
		<-ctx.Done()
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = server.Shutdown(shutdownCtx)
	}()
	if err := server.ListenAndServeTLS("", ""); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("serve GitHub broker: %w", err)
	}
	return nil
}

func configuredRunnerCleanupCallers(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	callers := make([]string, 0)
	for _, rawID := range strings.Split(raw, ",") {
		id := strings.TrimSpace(rawID)
		caller, err := githubbroker.ResourcePlaneCallerID(id)
		if err != nil {
			return nil, errors.New("HAKO_GITHUB_BROKER_ACTIONS_RUNNER_RESOURCE_PLANE_IDS contains an invalid Resource Plane ID")
		}
		if _, exists := seen[caller]; exists {
			return nil, errors.New("HAKO_GITHUB_BROKER_ACTIONS_RUNNER_RESOURCE_PLANE_IDS contains duplicates")
		}
		seen[caller] = struct{}{}
		callers = append(callers, caller)
	}
	return callers, nil
}

func configuredCheckoutCallers(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	seen := make(map[string]struct{})
	callers := make([]string, 0)
	for _, rawID := range strings.Split(raw, ",") {
		id := strings.TrimSpace(rawID)
		caller, err := githubbroker.ResourcePlaneCallerID(id)
		if err != nil {
			return nil, errors.New("HAKO_GITHUB_BROKER_CHECKOUT_RESOURCE_PLANE_IDS contains an invalid Resource Plane ID")
		}
		if _, exists := seen[caller]; exists {
			return nil, errors.New("HAKO_GITHUB_BROKER_CHECKOUT_RESOURCE_PLANE_IDS contains duplicates")
		}
		seen[caller] = struct{}{}
		callers = append(callers, caller)
	}
	return callers, nil
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
	cfg := dsql.Config{Host: dsqlHost, Region: os.Getenv("AWS_REGION"), User: strings.TrimSpace(os.Getenv("HAKO_DSQL_USER")), Database: os.Getenv("HAKO_DSQL_DATABASE")}
	if cfg.User == "" {
		cfg.User = "admin"
	}
	return dsql.Open(ctx, cfg)
}

func serverTLSConfigFromEnv() (*tls.Config, error) {
	certPath := strings.TrimSpace(os.Getenv("HAKO_GITHUB_BROKER_TLS_CERT_FILE"))
	keyPath := strings.TrimSpace(os.Getenv("HAKO_GITHUB_BROKER_TLS_KEY_FILE"))
	caPath := strings.TrimSpace(os.Getenv("HAKO_GITHUB_BROKER_CLIENT_CA_FILE"))
	configured := 0
	for _, value := range []string{certPath, keyPath, caPath} {
		if value != "" {
			configured++
		}
	}
	if configured != 3 {
		return nil, errors.New("configure broker TLS certificate, key, and client CA files together")
	}
	certificate, err := tls.LoadX509KeyPair(certPath, keyPath)
	if err != nil {
		return nil, errors.New("load broker TLS certificate")
	}
	clientCAPEM, err := os.ReadFile(caPath)
	if err != nil {
		return nil, errors.New("read broker client CA bundle")
	}
	clientCAs := x509.NewCertPool()
	if !clientCAs.AppendCertsFromPEM(clientCAPEM) {
		return nil, errors.New("parse broker client CA bundle")
	}
	serverConfig, err := githubbroker.ServerTLSConfig(clientCAs)
	if err != nil {
		return nil, errors.New("configure broker mutual TLS")
	}
	serverConfig.Certificates = []tls.Certificate{certificate}
	return serverConfig, nil
}
