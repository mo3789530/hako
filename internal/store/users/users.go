// Package users maps verified Cognito identities to stable Hako User records.
package users

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/mo3789530/hako/internal/domain"
	"github.com/mo3789530/hako/internal/idgen"
	"github.com/mo3789530/hako/internal/store/transaction"
)

// ResolveCognitoSubject creates a Hako User for a verified Cognito subject or
// returns the existing mapping. Cognito sub is the identity key; email is not
// read from an access token or used as a stable identity.
func ResolveCognitoSubject(ctx context.Context, pool transaction.Beginner, policy transaction.Policy, subject string) (domain.User, error) {
	subject = strings.TrimSpace(subject)
	if subject == "" {
		return domain.User{}, errors.New("verified Cognito subject is required")
	}
	userID, err := idgen.New("usr_")
	if err != nil {
		return domain.User{}, fmt.Errorf("generate Hako User id: %w", err)
	}
	createdAt := time.Now().UTC()
	var user domain.User
	user, err = transaction.Within(ctx, pool, policy, func(ctx context.Context, tx pgx.Tx) (domain.User, error) {
		err := tx.QueryRow(ctx, `INSERT INTO users (id, cognito_subject, email, created_at) VALUES ($1, $2, '', $3) ON CONFLICT (cognito_subject) DO UPDATE SET cognito_subject = users.cognito_subject RETURNING id, cognito_subject, email, created_at`,
			userID, subject, createdAt,
		).Scan(&user.ID, &user.CognitoSubject, &user.Email, &user.CreatedAt)
		if err != nil {
			return domain.User{}, fmt.Errorf("resolve Hako User from Cognito subject: %w", err)
		}
		return user, nil
	})
	if err != nil {
		return domain.User{}, err
	}
	return user, nil
}
