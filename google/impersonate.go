package google

import (
	"context"
	"sync"

	"golang.org/x/oauth2"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

// Impersonator hands out organizer-impersonated token sources via keyless
// domain-wide delegation. Each source is built by impersonate.CredentialsTokenSource
// with Subject set to the organizer; the library performs the IAM Credentials
// signJwt -> jwt-bearer exchange and refresh, using ADC as the base identity.
// No service-account key is involved.
type Impersonator struct {
	saEmail string
	scopes  []string

	// newTS is impersonate.CredentialsTokenSource in production; overridden in tests.
	newTS func(ctx context.Context, cfg impersonate.CredentialsConfig, opts ...option.ClientOption) (oauth2.TokenSource, error)

	mu    sync.Mutex
	cache map[string]oauth2.TokenSource
}

// NewImpersonator builds an Impersonator. saEmail is the runtime service account
// that signs JWTs (it must hold roles/iam.serviceAccountTokenCreator on itself
// and be authorized for these scopes via domain-wide delegation).
func NewImpersonator(saEmail string, scopes []string) *Impersonator {
	return &Impersonator{
		saEmail: saEmail,
		scopes:  scopes,
		newTS:   impersonate.CredentialsTokenSource,
		cache:   map[string]oauth2.TokenSource{},
	}
}

// TokenSource returns a cached, auto-refreshing source authenticated as the
// given organizer. One source per organizer for the Impersonator's lifetime.
func (im *Impersonator) TokenSource(ctx context.Context, organizer string) (oauth2.TokenSource, error) {
	im.mu.Lock()
	defer im.mu.Unlock()
	if ts, ok := im.cache[organizer]; ok {
		return ts, nil
	}
	ts, err := im.newTS(ctx, impersonate.CredentialsConfig{
		TargetPrincipal: im.saEmail,
		Scopes:          im.scopes,
		Subject:         organizer,
	})
	if err != nil {
		return nil, err
	}
	im.cache[organizer] = ts
	return ts, nil
}
