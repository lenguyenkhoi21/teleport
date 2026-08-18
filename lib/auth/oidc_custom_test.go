/*
 * Teleport
 * Copyright (C) 2025  Gravitational, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU Affero General Public License for more details.
 *
 * You should have received a copy of the GNU Affero General Public License
 * along with this program.  If not, see <http://www.gnu.org/licenses/>.
 */

package auth_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	jose "github.com/go-jose/go-jose/v4"
	"github.com/stretchr/testify/require"

	"github.com/gravitational/teleport/api/types"
	"github.com/gravitational/teleport/lib/auth"
)

// fakeOIDCIdP is a small OIDC provider for custom OIDC tests. It exposes
// discovery, JWKS, and a token endpoint that mints an RSA-signed ID token
// for a pre-issued authorization code.
type fakeOIDCIdP struct {
	*httptest.Server
	t          *testing.T
	privateKey *rsa.PrivateKey
	keyID      string
	clientID   string

	mu    sync.Mutex
	codes map[string]map[string]any // code -> claims to embed in ID token
}

func newFakeOIDCIdP(t *testing.T, clientID string) *fakeOIDCIdP {
	t.Helper()
	priv, err := rsa.GenerateKey(rand.Reader, 2048)
	require.NoError(t, err)

	f := &fakeOIDCIdP{
		t:          t,
		privateKey: priv,
		keyID:      "test-key-1",
		clientID:   clientID,
		codes:      map[string]map[string]any{},
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", f.handleDiscovery)
	mux.HandleFunc("/.well-known/jwks.json", f.handleJWKS)
	mux.HandleFunc("/token", f.handleToken)
	// /authorize is not required for these tests since the auth server does
	// not call it; the browser does. Tests pre-issue a code via IssueCode.

	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *fakeOIDCIdP) handleDiscovery(w http.ResponseWriter, _ *http.Request) {
	doc := map[string]any{
		"issuer":                                f.URL,
		"authorization_endpoint":                f.URL + "/authorize",
		"token_endpoint":                        f.URL + "/token",
		"jwks_uri":                              f.URL + "/.well-known/jwks.json",
		"response_types_supported":              []string{"code"},
		"subject_types_supported":               []string{"public"},
		"id_token_signing_alg_values_supported": []string{"RS256"},
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(doc)
}

func (f *fakeOIDCIdP) handleJWKS(w http.ResponseWriter, _ *http.Request) {
	jwk := jose.JSONWebKey{
		Key:       &f.privateKey.PublicKey,
		KeyID:     f.keyID,
		Algorithm: string(jose.RS256),
		Use:       "sig",
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(jose.JSONWebKeySet{Keys: []jose.JSONWebKey{jwk}})
}

// IssueCode registers a one-shot authorization code that the token endpoint
// will accept and exchange for an ID token containing the provided claims.
func (f *fakeOIDCIdP) IssueCode(code string, claims map[string]any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.codes[code] = claims
}

func (f *fakeOIDCIdP) handleToken(w http.ResponseWriter, r *http.Request) {
	require.NoError(f.t, r.ParseForm())
	code := r.FormValue("code")

	f.mu.Lock()
	claims, ok := f.codes[code]
	if ok {
		delete(f.codes, code)
	}
	f.mu.Unlock()
	if !ok {
		http.Error(w, "unknown code", http.StatusBadRequest)
		return
	}

	now := time.Now()
	idClaims := map[string]any{
		"iss": f.URL,
		"aud": f.clientID,
		"sub": "user-sub-id",
		"iat": now.Unix(),
		"exp": now.Add(5 * time.Minute).Unix(),
	}
	for k, v := range claims {
		idClaims[k] = v
	}
	idToken, err := f.signIDToken(idClaims)
	require.NoError(f.t, err)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"access_token": "fake-access-token",
		"token_type":   "Bearer",
		"id_token":     idToken,
		"expires_in":   300,
	})
}

func (f *fakeOIDCIdP) signIDToken(claims map[string]any) (string, error) {
	signingKey := jose.SigningKey{
		Algorithm: jose.RS256,
		Key:       f.privateKey,
	}
	opts := (&jose.SignerOptions{}).WithType("JWT").WithHeader("kid", f.keyID)
	signer, err := jose.NewSigner(signingKey, opts)
	if err != nil {
		return "", err
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		return "", err
	}
	jws, err := signer.Sign(payload)
	if err != nil {
		return "", err
	}
	return jws.CompactSerialize()
}

func newCustomOIDCConnector(t *testing.T, name, issuerURL, clientID, redirectURL string) types.OIDCConnector {
	t.Helper()
	connector, err := types.NewOIDCConnector(name, types.OIDCConnectorSpecV3{
		IssuerURL:    issuerURL,
		ClientID:     clientID,
		ClientSecret: "test-secret",
		RedirectURLs: []string{redirectURL},
		Scope:        []string{"openid", "email", "groups"},
		ClaimsToRoles: []types.ClaimMapping{
			{Claim: "groups", Value: "admins", Roles: []string{"custom-admin"}},
		},
	})
	require.NoError(t, err)
	connector.SetSubKind(types.OIDCConnectorSubKindCustom)
	require.NoError(t, connector.CheckAndSetDefaults())
	return connector
}

func TestCustomOIDCCreateAuthRequest(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt := setupGithubContext(ctx, t)

	idp := newFakeOIDCIdP(t, "custom-client")
	connector := newCustomOIDCConnector(t, "custom",
		idp.URL, "custom-client", "https://proxy.example.com/v1/webapi/oidc/callback")
	_, err := tt.a.UpsertOIDCConnector(ctx, connector)
	require.NoError(t, err)

	req, err := tt.a.CreateOIDCAuthRequest(ctx, types.OIDCAuthRequest{
		ConnectorID:      "custom",
		CreateWebSession: true,
	})
	require.NoError(t, err)
	require.NotEmpty(t, req.StateToken, "state token must be generated")
	require.NotEmpty(t, req.PkceVerifier, "PKCE verifier must be generated")
	require.NotEmpty(t, req.RedirectURL, "authorize URL must be filled in")

	u, err := url.Parse(req.RedirectURL)
	require.NoError(t, err)
	q := u.Query()
	require.Equal(t, "custom-client", q.Get("client_id"))
	require.Equal(t, req.StateToken, q.Get("state"))
	require.NotEmpty(t, q.Get("code_challenge"), "PKCE challenge must be on the URL")
	require.Equal(t, "S256", q.Get("code_challenge_method"))
}

func TestCustomOIDCValidateCallback(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt := setupGithubContext(ctx, t)

	role, err := types.NewRole("custom-admin", types.RoleSpecV6{})
	require.NoError(t, err)
	_, err = tt.a.UpsertRole(ctx, role)
	require.NoError(t, err)

	idp := newFakeOIDCIdP(t, "custom-client")
	connector := newCustomOIDCConnector(t, "custom",
		idp.URL, "custom-client", "https://proxy.example.com/v1/webapi/oidc/callback")
	_, err = tt.a.UpsertOIDCConnector(ctx, connector)
	require.NoError(t, err)

	req, err := tt.a.CreateOIDCAuthRequest(ctx, types.OIDCAuthRequest{
		ConnectorID: "custom",
	})
	require.NoError(t, err)

	const code = "fake-auth-code"
	idp.IssueCode(code, map[string]any{
		"preferred_username": "alice@custom.test",
		"email":              "alice@custom.test",
		"groups":             []any{"admins"},
	})

	resp, err := tt.a.ValidateOIDCAuthCallback(ctx, url.Values{
		"state": {req.StateToken},
		"code":  {code},
	})
	require.NoError(t, err)
	require.Equal(t, "alice@custom.test", resp.Username)
	require.Equal(t, "custom", resp.Identity.ConnectorID)
	require.Equal(t, "user-sub-id", resp.Identity.UserID)

	user, err := tt.a.GetUser(ctx, "alice@custom.test", false)
	require.NoError(t, err)
	require.Contains(t, user.GetRoles(), "custom-admin")
}

func TestCustomOIDCValidateCallbackRejectsUnmappedClaims(t *testing.T) {
	t.Parallel()
	ctx := context.Background()
	tt := setupGithubContext(ctx, t)

	idp := newFakeOIDCIdP(t, "custom-client")
	connector := newCustomOIDCConnector(t, "custom",
		idp.URL, "custom-client", "https://proxy.example.com/v1/webapi/oidc/callback")
	_, err := tt.a.UpsertOIDCConnector(ctx, connector)
	require.NoError(t, err)

	req, err := tt.a.CreateOIDCAuthRequest(ctx, types.OIDCAuthRequest{ConnectorID: "custom"})
	require.NoError(t, err)

	const code = "code-no-match"
	idp.IssueCode(code, map[string]any{
		"preferred_username": "bob@custom.test",
		"groups":             []any{"contractors"},
	})

	_, err = tt.a.ValidateOIDCAuthCallback(ctx, url.Values{
		"state": {req.StateToken},
		"code":  {code},
	})
	require.Error(t, err, "callback must fail when no claim maps to a role")
}

func TestMatchOIDCClaims(t *testing.T) {
	t.Parallel()
	mappings := []types.ClaimMapping{
		{Claim: "groups", Value: "admins", Roles: []string{"r-admin"}},
		{Claim: "groups", Value: "devs", Roles: []string{"r-dev"}},
		{Claim: "role", Value: "owner", Roles: []string{"r-owner"}},
	}
	tests := []struct {
		name   string
		claims map[string]any
		want   []string
	}{
		{name: "string claim matches", claims: map[string]any{"role": "owner"}, want: []string{"r-owner"}},
		{name: "array claim matches one", claims: map[string]any{"groups": []any{"admins"}}, want: []string{"r-admin"}},
		{name: "array claim matches multiple mappings", claims: map[string]any{"groups": []any{"admins", "devs"}}, want: []string{"r-admin", "r-dev"}},
		{name: "no match", claims: map[string]any{"groups": []any{"contractors"}}, want: nil},
		{name: "wrong claim name", claims: map[string]any{"other": "admins"}, want: nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := auth.MatchOIDCClaims(mappings, tt.claims)
			require.ElementsMatch(t, tt.want, got)
		})
	}
}

func TestClaimsToTraitsMapping(t *testing.T) {
	t.Parallel()
	got := auth.ClaimsToTraits(map[string]any{
		"email":       "alice@custom.test",
		"groups":      []any{"admins", "devs"},
		"is_internal": true,
		"ignored":     map[string]any{"nested": "value"}, // unsupported -> skipped
	})
	require.Equal(t, []string{"alice@custom.test"}, got["email"])
	require.ElementsMatch(t, []string{"admins", "devs"}, got["groups"])
	require.Equal(t, []string{"true"}, got["is_internal"])
	_, hasIgnored := got["ignored"]
	require.False(t, hasIgnored)
}

func TestPickOIDCUsername(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		claims map[string]any
		sub    string
		want   string
	}{
		{name: "preferred_username wins", claims: map[string]any{"preferred_username": "p", "email": "e", "username": "u"}, sub: "s", want: "p"},
		{name: "fallback to email", claims: map[string]any{"email": "e"}, sub: "s", want: "e"},
		{name: "fallback to username", claims: map[string]any{"username": "u"}, sub: "s", want: "u"},
		{name: "fallback to sub", claims: map[string]any{}, sub: "s", want: "s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, auth.PickOIDCUsername(tt.claims, tt.sub))
		})
	}
}
