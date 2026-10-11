package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/mo3789530/hako/internal/githubapp"
	"github.com/mo3789530/hako/internal/githubbroker"
	"github.com/mo3789530/hako/internal/githubchecks"
	"github.com/mo3789530/hako/internal/store/dsql"
	"github.com/mo3789530/hako/internal/store/transaction"
)

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	queueURL := strings.TrimSpace(os.Getenv("HAKO_GITHUB_CHECKS_QUEUE_URL"))
	if queueURL == "" {
		return errors.New("HAKO_GITHUB_CHECKS_QUEUE_URL is required")
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	pool, err := openDatabase(ctx)
	if err != nil {
		return errors.New("open GitHub Checks consumer database")
	}
	defer pool.Close()
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return errors.New("load GitHub Checks consumer AWS configuration")
	}
	if region := strings.TrimSpace(os.Getenv("HAKO_GITHUB_CHECKS_QUEUE_REGION")); region != "" {
		awsCfg.Region = region
	}
	clientCertificate, roots, err := clientTLSFromEnv()
	if err != nil {
		return err
	}
	brokerClient, err := githubbroker.NewChecksMTLSClient(clientCertificate, roots)
	if err != nil {
		return errors.New("configure GitHub Checks broker mTLS client")
	}
	credentials, err := githubbroker.NewHTTPChecksCredentialProvider(strings.TrimSpace(os.Getenv("HAKO_GITHUB_CHECKS_BROKER_URL")), brokerClient)
	if err != nil {
		return errors.New("configure GitHub Checks credential broker URL")
	}
	visibilityTimeout, err := queueVisibilityTimeoutFromEnv()
	if err != nil {
		return err
	}
	queue, err := githubchecks.NewSQSQueueWithVisibility(sqs.NewFromConfig(awsCfg), queueURL, visibilityTimeout)
	if err != nil {
		return fmt.Errorf("configure GitHub Checks SQS queue: %w", err)
	}
	store := githubchecks.DBStore{Pool: pool, Policy: transaction.DefaultPolicy()}
	checksAPI, err := githubapp.NewChecksClient(nil)
	if err != nil {
		return errors.New("configure GitHub Checks API client")
	}
	consumer, err := githubchecks.New(queue, store, credentials, checksAPI, githubchecks.Config{BatchSize: 10, WaitTime: 20 * time.Second})
	if err != nil {
		return fmt.Errorf("configure GitHub Checks consumer: %w", err)
	}
	log.Printf("Hako GitHub Checks consumer started")
	for ctx.Err() == nil {
		stats, err := consumer.RunOnce(ctx)
		if err != nil {
			if ctx.Err() != nil {
				break
			}
			log.Printf("GitHub Checks delivery batch failed (received=%d applied=%d failed=%d deferred=%d)", stats.Received, stats.Applied, stats.Failed, stats.Deferred)
		} else if stats.Received > 0 {
			log.Printf("GitHub Checks delivery batch complete (received=%d applied=%d deferred=%d)", stats.Received, stats.Applied, stats.Deferred)
		}
	}
	return nil
}

func queueVisibilityTimeoutFromEnv() (time.Duration, error) {
	value := strings.TrimSpace(os.Getenv("HAKO_GITHUB_CHECKS_VISIBILITY_TIMEOUT_SECONDS"))
	if value == "" {
		return 15 * time.Minute, nil
	}
	seconds, err := strconv.ParseInt(value, 10, 32)
	if err != nil || seconds < 1 || seconds > 43200 {
		return 0, errors.New("HAKO_GITHUB_CHECKS_VISIBILITY_TIMEOUT_SECONDS must be between 1 and 43200")
	}
	return time.Duration(seconds) * time.Second, nil
}

func clientTLSFromEnv() (tls.Certificate, *x509.CertPool, error) {
	certPath := strings.TrimSpace(os.Getenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_CERT_FILE"))
	keyPath := strings.TrimSpace(os.Getenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_KEY_FILE"))
	caPath := strings.TrimSpace(os.Getenv("HAKO_GITHUB_CHECKS_BROKER_CA_FILE"))
	certPEM := os.Getenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_CERT_PEM")
	keyPEM := os.Getenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_KEY_PEM")
	caPEM := os.Getenv("HAKO_GITHUB_CHECKS_BROKER_CA_PEM")
	inlineTLSConfigured := certPEM != "" || keyPEM != "" || caPEM != ""
	if inlineTLSConfigured {
		defer func() {
			_ = os.Unsetenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_CERT_PEM")
			_ = os.Unsetenv("HAKO_GITHUB_CHECKS_BROKER_CLIENT_KEY_PEM")
			_ = os.Unsetenv("HAKO_GITHUB_CHECKS_BROKER_CA_PEM")
		}()
	}
	fileTLSConfigured := certPath != "" || keyPath != "" || caPath != ""
	if inlineTLSConfigured && fileTLSConfigured {
		return tls.Certificate{}, nil, errors.New("configure broker mTLS from either files or injected Secrets, not both")
	}
	if inlineTLSConfigured && (certPEM == "" || keyPEM == "" || caPEM == "") {
		return tls.Certificate{}, nil, errors.New("configure complete GitHub Checks broker certificate, key, and CA Secrets")
	}
	if !inlineTLSConfigured && (certPath == "" || keyPath == "" || caPath == "") {
		return tls.Certificate{}, nil, errors.New("configure GitHub Checks broker client certificate, key, and CA files")
	}
	var certificate tls.Certificate
	var caBytes []byte
	if inlineTLSConfigured {
		var err error
		certificate, err = tls.X509KeyPair([]byte(certPEM), []byte(keyPEM))
		if err != nil {
			return tls.Certificate{}, nil, errors.New("parse injected GitHub Checks broker client certificate")
		}
		caBytes = []byte(caPEM)
	} else {
		var err error
		certificate, err = tls.LoadX509KeyPair(certPath, keyPath)
		if err != nil {
			return tls.Certificate{}, nil, errors.New("load GitHub Checks broker client certificate")
		}
		caBytes, err = os.ReadFile(caPath)
		if err != nil {
			return tls.Certificate{}, nil, errors.New("read GitHub Checks broker CA file")
		}
	}
	leaf, err := x509.ParseCertificate(certificate.Certificate[0])
	if err != nil {
		return tls.Certificate{}, nil, errors.New("parse GitHub Checks broker client certificate")
	}
	certificate.Leaf = leaf
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caBytes) {
		return tls.Certificate{}, nil, errors.New("parse GitHub Checks broker CA file")
	}
	return certificate, roots, nil
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
