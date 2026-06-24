package google

import (
	"context"
	"errors"
	"testing"

	"golang.org/x/oauth2"
	"google.golang.org/api/impersonate"
	"google.golang.org/api/option"
)

func TestImpersonatorCachesPerOrganizer(t *testing.T) {
	calls := map[string]int{}
	im := NewImpersonator("sa@p.iam.gserviceaccount.com", []string{"scopeA"})
	im.newTS = func(ctx context.Context, cfg impersonate.CredentialsConfig, _ ...option.ClientOption) (oauth2.TokenSource, error) {
		calls[cfg.Subject]++
		if cfg.TargetPrincipal != "sa@p.iam.gserviceaccount.com" {
			t.Errorf("TargetPrincipal = %q", cfg.TargetPrincipal)
		}
		return oauth2.StaticTokenSource(&oauth2.Token{AccessToken: "t-" + cfg.Subject}), nil
	}

	a1, err := im.TokenSource(context.Background(), "alice@x.com")
	if err != nil {
		t.Fatal(err)
	}
	a2, err := im.TokenSource(context.Background(), "alice@x.com")
	if err != nil {
		t.Fatal(err)
	}
	if a1 != a2 {
		t.Error("expected cached source reused for same organizer")
	}
	if _, err := im.TokenSource(context.Background(), "bob@x.com"); err != nil {
		t.Fatal(err)
	}
	if calls["alice@x.com"] != 1 || calls["bob@x.com"] != 1 {
		t.Errorf("factory call counts = %v, want one each", calls)
	}
}

func TestImpersonatorPropagatesError(t *testing.T) {
	im := NewImpersonator("sa@p.iam.gserviceaccount.com", []string{"scopeA"})
	im.newTS = func(ctx context.Context, cfg impersonate.CredentialsConfig, _ ...option.ClientOption) (oauth2.TokenSource, error) {
		return nil, errors.New("boom")
	}
	if _, err := im.TokenSource(context.Background(), "alice@x.com"); err == nil {
		t.Fatal("expected error to propagate")
	}
}
