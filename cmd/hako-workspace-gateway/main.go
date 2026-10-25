// hako-workspace-gateway runs the long-lived Resource Plane HTTPS/WSS edge.
// It uses the task role for AWS access and mounted files for all signing and
// lookup keys; no AWS credential or secret value is accepted via environment.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/tls"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/lambdamicrovms"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/workspaceagent"
	"github.com/mo3789530/hako/internal/workspacegateway"
	"github.com/mo3789530/hako/internal/workspacejournal"
	"github.com/mo3789530/hako/internal/workspacesession"
)

const maxGatewayKeyFileBytes = 64 << 10

var gatewayKeyIDPattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

type deploymentConfig struct {
	ResourcePlaneID         domain.ResourcePlaneID
	ListenAddress           string
	TLSCertFile             string
	TLSKeyFile              string
	SessionPublicKeysFile   string
	SessionLookupEndpoint   string
	SessionLookupKeyFile    string
	ConnectionLeaseEndpoint string
	ConnectionLeaseKeyFile  string
	AgentTicketKeyID        string
	AgentTicketKeyFile      string
	PublicOrigin            string
	JournalBucket           string
	JournalPrefix           string
	Region                  string
	AllowedOrigins          []string
	MaxConnections          int
	MaxConnectionsPerWS     int
	SessionRecheckInterval  time.Duration
}

func main() {
	if err := run(); err != nil {
		log.Printf("Workspace Gateway stopped (failure_class=process_exit)")
		os.Exit(1)
	}
}

func run() error {
	cfg, err := loadDeploymentConfig(os.Getenv)
	if err != nil {
		return err
	}
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background(), awsconfig.WithRegion(cfg.Region))
	if err != nil {
		return errors.New("load Resource Plane task-role AWS configuration")
	}
	gateway, err := buildGateway(awsCfg, cfg)
	if err != nil {
		return err
	}
	server := &http.Server{
		Addr: cfg.ListenAddress, Handler: gateway.Handler(),
		ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second,
		MaxHeaderBytes: 16 << 10,
		TLSConfig:      &tls.Config{MinVersion: tls.VersionTLS12},
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	serveErr := make(chan error, 1)
	go func() { serveErr <- server.ListenAndServeTLS(cfg.TLSCertFile, cfg.TLSKeyFile) }()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		if err := server.Shutdown(shutdownCtx); err != nil {
			return errors.New("gracefully stop Workspace Gateway")
		}
		if err := <-serveErr; err != nil && !errors.Is(err, http.ErrServerClosed) {
			return errors.New("Workspace Gateway HTTPS server failed")
		}
		return nil
	case err := <-serveErr:
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return errors.New("Workspace Gateway HTTPS server failed")
	}
}

func buildGateway(awsCfg aws.Config, cfg deploymentConfig) (*workspacegateway.Gateway, error) {
	publicKeys, err := loadSessionPublicKeys(cfg.SessionPublicKeysFile)
	if err != nil {
		return nil, err
	}
	lookupKey, err := readBoundedFile(cfg.SessionLookupKeyFile, 4096, true)
	if err != nil || len(lookupKey) < 32 || len(lookupKey) > 4096 {
		return nil, errors.New("Workspace Session lookup HMAC key file is invalid")
	}
	defer wipe(lookupKey)
	lookup, err := workspacesession.NewHTTPActiveSessionLookup(workspacesession.HTTPActiveSessionLookupConfig{
		Endpoint: cfg.SessionLookupEndpoint, ResourcePlaneID: cfg.ResourcePlaneID, HMACKey: lookupKey,
	})
	if err != nil {
		return nil, errors.New("configure authenticated Control Plane session lookup")
	}
	portAuthorizer, err := workspacesession.NewHTTPWorkspacePortAuthorizer(workspacesession.HTTPWorkspacePortAuthorizerConfig{
		Endpoint: cfg.SessionLookupEndpoint, ResourcePlaneID: cfg.ResourcePlaneID, HMACKey: lookupKey,
	})
	if err != nil {
		return nil, errors.New("configure authenticated Control Plane Workspace port authorization")
	}
	leaseKey, err := readBoundedFile(cfg.ConnectionLeaseKeyFile, 4096, true)
	if err != nil || len(leaseKey) < 32 || len(leaseKey) > 4096 {
		return nil, errors.New("Workspace connection lease HMAC key file is invalid")
	}
	leases, err := workspaceagent.NewSignedWorkspaceConnectionLeaseReporter(cfg.ConnectionLeaseEndpoint, leaseKey, nil)
	wipe(leaseKey)
	if err != nil {
		return nil, errors.New("configure authenticated Control Plane Workspace connection lease reporter")
	}
	agentKeyPEM, err := readBoundedFile(cfg.AgentTicketKeyFile, maxGatewayKeyFileBytes, true)
	if err != nil {
		return nil, errors.New("read Workspace Agent ticket key file")
	}
	privateKey, err := parseAgentPrivateKey(agentKeyPEM)
	wipe(agentKeyPEM)
	if err != nil {
		return nil, errors.New("Workspace Agent ticket key file must contain an Ed25519 PKCS#8 private key")
	}
	issuer := &workspaceagent.AgentTicketIssuer{KeyID: cfg.AgentTicketKeyID, PrivateKey: privateKey}
	journal, err := workspacejournal.NewS3Store(s3.NewFromConfig(awsCfg), cfg.JournalBucket, cfg.JournalPrefix)
	if err != nil {
		return nil, errors.New("configure Resource Plane Workspace runtime journal")
	}
	microVMs := lambdamicrovms.NewFromConfig(awsCfg)
	directory := &workspaceagent.WorkspaceLambdaMicroVMStreamDirectory{
		ResourcePlaneID: cfg.ResourcePlaneID, Journal: journal, MicroVMs: microVMs,
		Tokens: workspaceagent.LambdaMicroVMAuthTokenProvider{API: microVMs}, Tickets: issuer,
		Leases: leases, Region: cfg.Region, Origin: cfg.PublicOrigin,
	}
	return workspacegateway.New(workspacegateway.Config{
		ResourcePlaneID: cfg.ResourcePlaneID,
		Verifier:        &workspacesession.Verifier{Keys: publicKeys, Audience: workspacesession.AudienceGateway, Lookup: lookup},
		Dialer:          directory, PortAuthorizer: workspacegateway.PortAuthorizerChain{
			workspacegateway.PreviewPortAuthorizer{}, portAuthorizer,
		}, AllowedOrigins: cfg.AllowedOrigins,
		MaxConnections: cfg.MaxConnections, MaxConnectionsPerWorkspace: cfg.MaxConnectionsPerWS,
		SessionRecheckInterval: cfg.SessionRecheckInterval, MetricsWriter: os.Stdout,
	})
}

func loadDeploymentConfig(getenv func(string) string) (deploymentConfig, error) {
	if getenv == nil {
		return deploymentConfig{}, errors.New("Workspace Gateway environment reader is required")
	}
	get := func(name string) string { return strings.TrimSpace(getenv(name)) }
	cfg := deploymentConfig{
		ResourcePlaneID:         domain.ResourcePlaneID(get("HAKO_RESOURCE_PLANE_ID")),
		ListenAddress:           get("HAKO_GATEWAY_LISTEN_ADDRESS"),
		TLSCertFile:             get("HAKO_GATEWAY_TLS_CERT_FILE"),
		TLSKeyFile:              get("HAKO_GATEWAY_TLS_KEY_FILE"),
		SessionPublicKeysFile:   get("HAKO_GATEWAY_SESSION_PUBLIC_KEYS_FILE"),
		SessionLookupEndpoint:   get("HAKO_GATEWAY_SESSION_LOOKUP_ENDPOINT"),
		SessionLookupKeyFile:    get("HAKO_GATEWAY_SESSION_LOOKUP_KEY_FILE"),
		ConnectionLeaseEndpoint: get("HAKO_GATEWAY_CONNECTION_LEASE_ENDPOINT"),
		ConnectionLeaseKeyFile:  get("HAKO_GATEWAY_CONNECTION_LEASE_KEY_FILE"),
		AgentTicketKeyID:        get("HAKO_GATEWAY_AGENT_TICKET_KEY_ID"),
		AgentTicketKeyFile:      get("HAKO_GATEWAY_AGENT_TICKET_KEY_FILE"),
		PublicOrigin:            get("HAKO_GATEWAY_PUBLIC_ORIGIN"),
		JournalBucket:           get("HAKO_WORKSPACE_RUNTIME_JOURNAL_BUCKET"),
		JournalPrefix:           get("HAKO_WORKSPACE_RUNTIME_JOURNAL_PREFIX"),
		Region:                  get("AWS_REGION"),
	}
	if cfg.ListenAddress == "" {
		cfg.ListenAddress = ":8443"
	}
	if cfg.MaxConnections, _ = strconv.Atoi(defaultValue(get("HAKO_GATEWAY_MAX_CONNECTIONS"), "500")); cfg.MaxConnections < 1 || cfg.MaxConnections > 10000 {
		return deploymentConfig{}, errors.New("HAKO_GATEWAY_MAX_CONNECTIONS must be between 1 and 10000")
	}
	if cfg.MaxConnectionsPerWS, _ = strconv.Atoi(defaultValue(get("HAKO_GATEWAY_MAX_CONNECTIONS_PER_WORKSPACE"), "10")); cfg.MaxConnectionsPerWS < 1 || cfg.MaxConnectionsPerWS > cfg.MaxConnections {
		return deploymentConfig{}, errors.New("HAKO_GATEWAY_MAX_CONNECTIONS_PER_WORKSPACE must be between 1 and the total connection limit")
	}
	recheckInterval, err := time.ParseDuration(defaultValue(get("HAKO_GATEWAY_SESSION_RECHECK_INTERVAL"), "15s"))
	if err != nil || recheckInterval < time.Second || recheckInterval > time.Minute {
		return deploymentConfig{}, errors.New("HAKO_GATEWAY_SESSION_RECHECK_INTERVAL must be between 1s and 1m")
	}
	cfg.SessionRecheckInterval = recheckInterval
	for _, origin := range strings.Split(get("HAKO_GATEWAY_ALLOWED_ORIGINS"), ",") {
		origin = strings.TrimSpace(origin)
		if origin != "" {
			parsed, err := url.Parse(origin)
			if err != nil || parsed.Scheme != "https" || parsed.Host == "" || parsed.User != nil || parsed.Path != "" || parsed.RawQuery != "" || parsed.Fragment != "" {
				return deploymentConfig{}, errors.New("HAKO_GATEWAY_ALLOWED_ORIGINS must contain exact HTTPS origins")
			}
			cfg.AllowedOrigins = append(cfg.AllowedOrigins, origin)
		}
	}
	if cfg.ResourcePlaneID == "" || cfg.TLSCertFile == "" || cfg.TLSKeyFile == "" || cfg.SessionPublicKeysFile == "" ||
		cfg.SessionLookupEndpoint == "" || cfg.SessionLookupKeyFile == "" || cfg.ConnectionLeaseEndpoint == "" || cfg.ConnectionLeaseKeyFile == "" ||
		cfg.AgentTicketKeyID == "" || cfg.AgentTicketKeyFile == "" ||
		cfg.PublicOrigin == "" || cfg.JournalBucket == "" || cfg.Region == "" || len(cfg.AllowedOrigins) == 0 {
		return deploymentConfig{}, errors.New("Workspace Gateway requires Resource Plane, TLS, session keys, lookup endpoint/key, connection lease endpoint/key, Agent key, journal, region, and allowed-origin configuration")
	}
	if !gatewayKeyIDPattern.MatchString(cfg.AgentTicketKeyID) {
		return deploymentConfig{}, errors.New("HAKO_GATEWAY_AGENT_TICKET_KEY_ID is invalid")
	}
	publicOrigin, err := url.Parse(cfg.PublicOrigin)
	if err != nil || publicOrigin.Scheme != "https" || publicOrigin.Host == "" || publicOrigin.User != nil || publicOrigin.Path != "" || publicOrigin.RawQuery != "" || publicOrigin.Fragment != "" {
		return deploymentConfig{}, errors.New("HAKO_GATEWAY_PUBLIC_ORIGIN must be a fixed HTTPS origin")
	}
	return cfg, nil
}

func loadSessionPublicKeys(path string) (map[string]ed25519.PublicKey, error) {
	data, err := readBoundedFile(path, maxGatewayKeyFileBytes, false)
	if err != nil {
		return nil, errors.New("read Workspace Session public-key file")
	}
	defer wipe(data)
	var encoded map[string]string
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if decoder.Decode(&encoded) != nil || decoder.Decode(new(any)) != io.EOF || len(encoded) == 0 || len(encoded) > 16 {
		return nil, errors.New("Workspace Session public-key file must be a JSON key-id map")
	}
	keys := make(map[string]ed25519.PublicKey, len(encoded))
	for id, value := range encoded {
		key, err := base64.RawStdEncoding.DecodeString(value)
		if !gatewayKeyIDPattern.MatchString(id) || err != nil || len(key) != ed25519.PublicKeySize {
			return nil, errors.New("Workspace Session public-key file contains an invalid key")
		}
		keys[id] = append(ed25519.PublicKey(nil), key...)
	}
	return keys, nil
}

func readBoundedFile(path string, limit int64, secret bool) ([]byte, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("key file path is required")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > limit {
		return nil, errors.New("key file is missing or has an invalid type/size")
	}
	if secret && info.Mode().Perm()&0022 != 0 {
		return nil, errors.New("secret key file must not be group/world writable")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || len(data) == 0 || int64(len(data)) > limit {
		wipe(data)
		return nil, errors.New("key file could not be read within its size limit")
	}
	return data, nil
}

func parseAgentPrivateKey(data []byte) (ed25519.PrivateKey, error) {
	block, rest := pem.Decode(data)
	if block == nil || len(strings.TrimSpace(string(rest))) != 0 {
		return nil, errors.New("invalid PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	key, ok := parsed.(ed25519.PrivateKey)
	if !ok || len(key) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("unsupported private key type")
	}
	return append(ed25519.PrivateKey(nil), key...), nil
}

func defaultValue(value, fallback string) string {
	if value == "" {
		return fallback
	}
	return value
}

func wipe(data []byte) {
	for i := range data {
		data[i] = 0
	}
}
