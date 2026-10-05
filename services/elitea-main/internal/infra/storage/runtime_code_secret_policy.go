package storage

import (
	"context"
	"errors"

	"github.com/EliteaAI/elitea-platform/services/elitea-main/internal/platformconfig"
	"github.com/jackc/pgx/v5/pgxpool"
)

// CurrentCodeSecretPolicy uses the same current default_secrets section as REST.
// Code has no X-SECRET credential override.
type CurrentCodeSecretPolicy struct{ pool *pgxpool.Pool }

func NewCurrentCodeSecretPolicy(pool *pgxpool.Pool) (*CurrentCodeSecretPolicy, error) {
	if pool == nil {
		return nil, errors.New("Code secret policy database is required")
	}
	return &CurrentCodeSecretPolicy{pool: pool}, nil
}

func (p *CurrentCodeSecretPolicy) AllowCodeSecret(ctx context.Context, name string) (bool, error) {
	if p == nil || p.pool == nil || ctx == nil {
		return false, ErrContentRejected
	}
	values, err := platformconfig.Load(ctx, p.pool, "default_secrets")
	if err != nil {
		return false, err
	}
	return allowCodeSecretPolicy(values, name)
}

func allowCodeSecretPolicy(values platformconfig.Values, name string) (bool, error) {
	if len(name) == 0 || len(name) > 256 || !codeSecretName.MatchString(name) {
		return false, ErrContentRejected
	}
	suppress := false
	if value, present := values["ignore_default_secret_api"]; present {
		flag, ok := value.(bool)
		if !ok {
			return false, ErrContentRejected
		}
		suppress = flag
	}
	protected := false
	if value, present := values["default_secret_keys"]; present {
		names, ok := value.([]any)
		if !ok || len(names) > 100 {
			return false, ErrContentRejected
		}
		for _, value := range names {
			candidate, ok := value.(string)
			if !ok || len(candidate) == 0 || len(candidate) > 256 || !codeSecretName.MatchString(candidate) {
				return false, ErrContentRejected
			}
			if candidate == name {
				protected = true
			}
		}
	}
	return !suppress || !protected, nil
}
