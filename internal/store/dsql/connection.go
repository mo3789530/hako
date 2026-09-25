// Package dsql provides the Control Plane's Aurora DSQL connection pool.
package dsql

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"

	dsqlconn "github.com/awslabs/aurora-dsql-connectors/go/pgx/dsql"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Config contains non-secret connection settings. Authentication is supplied
// by the AWS SDK credential chain and uses short-lived IAM tokens.
type Config struct {
	Host     string
	Region   string
	User     string
	Database string
	MaxConns int32
}

func (c Config) normalized() (Config, error) {
	c.Host = strings.TrimSpace(c.Host)
	c.Region = strings.TrimSpace(c.Region)
	c.User = strings.TrimSpace(c.User)
	c.Database = strings.TrimSpace(c.Database)
	if c.Host == "" {
		return Config{}, errors.New("Aurora DSQL host is required")
	}
	if c.User == "" {
		return Config{}, errors.New("Aurora DSQL database user is required")
	}
	if c.Database == "" {
		c.Database = "postgres"
	}
	if c.MaxConns < 0 {
		return Config{}, errors.New("Aurora DSQL MaxConns must be non-negative")
	}
	return c, nil
}

// Open creates and verifies a pool. The AWS default credential chain supplies
// IAM credentials; no database password or static AWS key is accepted here.
func Open(ctx context.Context, config Config) (*pgxpool.Pool, error) {
	config, err := config.normalized()
	if err != nil {
		return nil, err
	}

	poolConfig, err := pgxpool.ParseConfig("")
	if err != nil {
		return nil, fmt.Errorf("parse Aurora DSQL pool configuration: %w", err)
	}
	if config.MaxConns > 0 {
		poolConfig.MaxConns = config.MaxConns
	}

	pool, err := dsqlconn.NewPool(ctx, dsqlconn.Config{
		Host:     config.Host,
		Region:   config.Region,
		User:     config.User,
		Database: config.Database,
	}, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create Aurora DSQL pool: %w", err)
	}

	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping Aurora DSQL: %w", err)
	}
	return pool, nil
}

// OpenPostgres opens a regular PostgreSQL connection for local development.
// Production deployments should use Open so Aurora DSQL IAM authentication
// and its TLS configuration are applied automatically.
func OpenPostgres(ctx context.Context, connectionURL string) (*pgxpool.Pool, error) {
	parsed, err := url.Parse(connectionURL)
	if err != nil || (parsed.Scheme != "postgres" && parsed.Scheme != "postgresql") || parsed.Hostname() == "" {
		return nil, errors.New("HAKO_DATABASE_URL must be a PostgreSQL URL with a host")
	}

	poolConfig, err := pgxpool.ParseConfig(connectionURL)
	if err != nil {
		// Do not return the parser error because it may include the full URL,
		// including a local development password.
		return nil, errors.New("invalid HAKO_DATABASE_URL")
	}
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		return nil, fmt.Errorf("create PostgreSQL pool: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping PostgreSQL: %w", err)
	}
	return pool, nil
}
