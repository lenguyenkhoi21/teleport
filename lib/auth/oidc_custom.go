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

// oidc_custom.go implements the OIDC authorization-code flow with PKCE for
// OIDCConnectors whose SubKind is types.OIDCConnectorSubKindCustom. It is the
// OSS counterpart to the enterprise OIDC service registered via
// Server.SetOIDCService and is reached when no enterprise OIDC service is
// registered.

package auth

import (
	"context"
	"net/url"
	"slices"
	"time"

	coreoidc "github.com/coreos/go-oidc/v3/oidc"
	"github.com/gravitational/trace"
	"golang.org/x/oauth2"

	"github.com/gravitational/teleport"
	"github.com/gravitational/teleport/api/constants"
	apidefaults "github.com/gravitational/teleport/api/defaults"
	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	apiutils "github.com/gravitational/teleport/api/utils"
	"github.com/gravitational/teleport/api/utils/keys/hardwarekey"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/authz"
	"github.com/gravitational/teleport/lib/defaults"
	"github.com/gravitational/teleport/lib/events"
	"github.com/gravitational/teleport/lib/loginrule"
	"github.com/gravitational/teleport/lib/services"
	"github.com/gravitational/teleport/lib/utils"
)

// customOIDCDefaultScopes is appended to the connector spec scopes if they do not
// already include openid. profile/email/groups are requested as a best-effort
// default; the connector can override the full list via spec.scope.
var customOIDCDefaultScopes = []string{coreoidc.ScopeOpenID, "email", "profile", "groups"}

// createCustomOIDCAuthRequest creates a new OIDC authorization request for a
// Custom sub-kind connector. It performs OIDC discovery against the issuer
// URL, computes the authorize URL with PKCE, persists the request, and
// returns the populated request to the caller.
func (a *Server) createCustomOIDCAuthRequest(ctx context.Context, req types.OIDCAuthRequest, connector types.OIDCConnector) (*types.OIDCAuthRequest, error) {
	cfg, _, err := newCustomOIDCConfig(ctx, connector, req.ProxyAddress)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	stateToken, err := utils.CryptoRandomHex(defaults.TokenLenBytes)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	req.StateToken = stateToken

	verifier := oauth2.GenerateVerifier()
	req.PkceVerifier = verifier

	req.RedirectURL = cfg.AuthCodeURL(stateToken, oauth2.S256ChallengeOption(verifier))

	if err := a.Services.CreateOIDCAuthRequest(ctx, req, defaults.OIDCAuthRequestTTL); err != nil {
		return nil, trace.Wrap(err)
	}
	a.logger.DebugContext(ctx, "Created custom OIDC auth request",
		"connector", connector.GetName(), "redirect_url", req.RedirectURL)
	return &req, nil
}

// validateCustomOIDCAuthCallback validates the callback parameters returned
// from the custom OIDC provider, exchanges the authorization code for tokens,
// verifies the ID token, maps claims to roles, creates or updates the user,
// and returns the OIDC auth response.
func (a *Server) validateCustomOIDCAuthCallback(ctx context.Context, q url.Values) (*authclient.OIDCAuthResponse, error) {
	logger := a.logger.With(teleport.ComponentKey, "custom-oidc")
	diagCtx := NewSSODiagContext(types.KindOIDC, a)

	if errParam := q.Get("error"); errParam != "" {
		state := q.Get("state")
		if state != "" {
			diagCtx.RequestID = state
			if r, err := a.Services.GetOIDCAuthRequest(ctx, state); err == nil {
				diagCtx.Info.TestFlow = r.SSOTestFlow
			}
		}
		oauthErr := trace.OAuth2("invalid_request", errParam, q)
		return nil, trace.WithUserMessage(oauthErr, "custom OIDC returned error: %v [%v]",
			q.Get("error_description"), errParam)
	}

	code := q.Get("code")
	if code == "" {
		oauthErr := trace.OAuth2("invalid_request", "code query param must be set", q)
		return nil, trace.WithUserMessage(oauthErr, "Invalid parameters received from custom OIDC.")
	}
	stateToken := q.Get("state")
	if stateToken == "" {
		oauthErr := trace.OAuth2("invalid_request", "missing state query param", q)
		return nil, trace.WithUserMessage(oauthErr, "Invalid parameters received from custom OIDC.")
	}
	diagCtx.RequestID = stateToken

	req, err := a.Services.GetOIDCAuthRequest(ctx, stateToken)
	if err != nil {
		return nil, trace.Wrap(err, "Failed to get custom OIDC auth request.")
	}
	diagCtx.Info.TestFlow = req.SSOTestFlow

	connector, err := a.getCustomOIDCConnector(ctx, *req)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	cfg, provider, err := newCustomOIDCConfig(ctx, connector, req.ProxyAddress)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	token, err := cfg.Exchange(ctx, code, oauth2.VerifierOption(req.PkceVerifier))
	if err != nil {
		return nil, trace.Wrap(err, "Failed to exchange custom OIDC authorization code.")
	}

	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, trace.BadParameter("custom OIDC token response did not include an id_token")
	}

	verifier := provider.Verifier(&coreoidc.Config{ClientID: connector.GetClientID()})
	idToken, err := verifier.Verify(ctx, rawIDToken)
	if err != nil {
		return nil, trace.Wrap(err, "Failed to verify custom OIDC ID token.")
	}

	claims := map[string]any{}
	if err := idToken.Claims(&claims); err != nil {
		return nil, trace.Wrap(err, "Failed to parse custom OIDC ID token claims.")
	}
	diagCtx.Info.OIDCClaims = types.OIDCClaims(claims)
	logger.DebugContext(ctx, "custom OIDC ID token verified",
		"subject", idToken.Subject, "claim_keys", claimKeys(claims))

	username := pickOIDCUsername(claims, idToken.Subject)
	if username == "" {
		return nil, trace.BadParameter("custom OIDC claims do not contain a username")
	}

	params, err := a.calculateCustomOIDCUser(ctx, diagCtx, connector, claims, username, idToken.Subject, req)
	if err != nil {
		return nil, trace.Wrap(err, "Failed to calculate user attributes from custom OIDC claims.")
	}
	diagCtx.Info.CreateUserParams = &types.CreateUserParams{
		ConnectorName: params.ConnectorName,
		Username:      params.Username,
		KubeGroups:    params.KubeGroups,
		KubeUsers:     params.KubeUsers,
		Roles:         params.Roles,
		Traits:        params.Traits,
		SessionTTL:    types.Duration(params.SessionTTL),
	}

	user, err := a.createCustomOIDCUser(ctx, params, req.SSOTestFlow)
	if err != nil {
		return nil, trace.Wrap(err, "Failed to create user from custom OIDC claims.")
	}

	if err := a.CallLoginHooks(ctx, user); err != nil {
		return nil, trace.Wrap(err)
	}

	userState, err := a.GetUserOrLoginState(ctx, user.GetName())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	identity := types.ExternalIdentity{
		ConnectorID: connector.GetName(),
		Username:    params.Username,
		UserID:      idToken.Subject,
	}
	auth := &authclient.OIDCAuthResponse{
		Req:      oidcAuthRequestToAuthclient(req),
		Identity: identity,
		Username: params.Username,
	}

	// Emit successful login event.
	a.emitOIDCLoginSuccess(ctx, diagCtx, params.Username)

	if req.SSOTestFlow {
		diagCtx.Info.Success = true
		diagCtx.WriteToBackend(ctx)
		return auth, nil
	}
	diagCtx.WriteToBackend(ctx)

	if req.CreateWebSession {
		session, err := a.CreateWebSessionFromReq(ctx, NewWebSessionRequest{
			User:                 userState.GetName(),
			Roles:                userState.GetRoles(),
			Traits:               userState.GetTraits(),
			SessionTTL:           params.SessionTTL,
			LoginTime:            a.clock.Now().UTC(),
			LoginIP:              req.ClientLoginIP,
			LoginUserAgent:       req.ClientUserAgent,
			AttestWebSession:     true,
			CreateDeviceWebToken: true,
		})
		if err != nil {
			return nil, trace.Wrap(err, "Failed to create web session.")
		}
		auth.Session = session
	}

	if len(req.SshPublicKey) != 0 || len(req.TlsPublicKey) != 0 {
		sshCert, tlsCert, err := a.CreateSessionCerts(ctx, &SessionCertsRequest{
			UserState:               userState,
			SessionTTL:              params.SessionTTL,
			SSHPubKey:               req.SshPublicKey,
			TLSPubKey:               req.TlsPublicKey,
			SSHAttestationStatement: hardwarekey.AttestationStatementFromProto(req.SshAttestationStatement),
			TLSAttestationStatement: hardwarekey.AttestationStatementFromProto(req.TlsAttestationStatement),
			Compatibility:           req.Compatibility,
			RouteToCluster:          req.RouteToCluster,
			KubernetesCluster:       req.KubernetesCluster,
			LoginIP:                 req.ClientLoginIP,
		})
		if err != nil {
			return nil, trace.Wrap(err, "Failed to create session certificate.")
		}
		clusterName, err := a.GetClusterName(ctx)
		if err != nil {
			return nil, trace.Wrap(err, "Failed to obtain cluster name.")
		}
		auth.Cert = sshCert
		auth.TLSCert = tlsCert
		authority, err := a.GetCertAuthority(ctx, types.CertAuthID{
			Type:       types.HostCA,
			DomainName: clusterName.GetClusterName(),
		}, false)
		if err != nil {
			return nil, trace.Wrap(err, "Failed to obtain cluster's host CA.")
		}
		auth.HostSigners = append(auth.HostSigners, authority)
	}
	if o, err := a.ClientOptionsForLogin(userState); err == nil {
		auth.ClientOptions = o
	}
	return auth, nil
}

// newCustomOIDCConfig performs OIDC discovery against the connector's issuer
// URL and returns an oauth2.Config (with provider endpoints filled in) and the
// underlying coreoidc.Provider for ID token verification.
func newCustomOIDCConfig(ctx context.Context, connector types.OIDCConnector, proxyAddress string) (*oauth2.Config, *coreoidc.Provider, error) {
	provider, err := coreoidc.NewProvider(ctx, connector.GetIssuerURL())
	if err != nil {
		return nil, nil, trace.Wrap(err, "custom OIDC discovery failed for %q", connector.GetIssuerURL())
	}
	redirectURL, err := services.GetRedirectURL(connector, proxyAddress)
	if err != nil {
		return nil, nil, trace.Wrap(err)
	}
	scopes := connector.GetScope()
	if len(scopes) == 0 {
		scopes = append(scopes, customOIDCDefaultScopes...)
	} else if !slices.Contains(scopes, coreoidc.ScopeOpenID) {
		scopes = append([]string{coreoidc.ScopeOpenID}, scopes...)
	}
	cfg := &oauth2.Config{
		ClientID:     connector.GetClientID(),
		ClientSecret: connector.GetClientSecret(),
		RedirectURL:  redirectURL,
		Endpoint:     provider.Endpoint(),
		Scopes:       scopes,
	}
	return cfg, provider, nil
}

// getCustomOIDCConnector returns the connector referenced by the given auth
// request, honoring SSOTestFlow (stateless connector spec).
func (a *Server) getCustomOIDCConnector(ctx context.Context, req types.OIDCAuthRequest) (types.OIDCConnector, error) {
	if req.SSOTestFlow {
		if req.ConnectorSpec == nil {
			return nil, trace.BadParameter("ConnectorSpec cannot be nil for SSOTestFlow")
		}
		if req.ConnectorID == "" {
			return nil, trace.BadParameter("ConnectorID cannot be empty")
		}
		c, err := types.NewOIDCConnector(req.ConnectorID, *req.ConnectorSpec)
		if err != nil {
			return nil, trace.Wrap(err)
		}
		c.SetSubKind(types.OIDCConnectorSubKindCustom)
		return c, nil
	}
	connector, err := a.GetOIDCConnector(ctx, req.ConnectorID, true)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if connector.GetSubKind() != types.OIDCConnectorSubKindCustom {
		return nil, trace.BadParameter("connector %q is not a custom OIDC connector", req.ConnectorID)
	}
	return connector, nil
}

// pickOIDCUsername returns the preferred username from a claim set. It tries
// preferred_username, email, then sub.
func pickOIDCUsername(claims map[string]any, sub string) string {
	for _, key := range []string{"preferred_username", "email", "username"} {
		if v, ok := claims[key].(string); ok && v != "" {
			return v
		}
	}
	return sub
}

// claimsToTraits flattens an OIDC claim set into the trait map shape used by
// Teleport's role/login-rule machinery. Scalar string/number/bool claims
// become single-element slices; arrays of those types become multi-element
// slices; everything else is skipped.
func claimsToTraits(claims map[string]any) map[string][]string {
	traits := make(map[string][]string, len(claims))
	for k, v := range claims {
		switch t := v.(type) {
		case string:
			traits[k] = []string{t}
		case []any:
			for _, item := range t {
				if s, ok := item.(string); ok {
					traits[k] = append(traits[k], s)
				}
			}
		case bool:
			if t {
				traits[k] = []string{"true"}
			} else {
				traits[k] = []string{"false"}
			}
		}
	}
	return traits
}

func claimKeys(claims map[string]any) []string {
	out := make([]string, 0, len(claims))
	for k := range claims {
		out = append(out, k)
	}
	slices.Sort(out)
	return out
}

// matchOIDCClaims walks claims_to_roles, picking up every role whose claim
// matches the user's claims. Match semantics: equal string, or membership in
// the value array.
func matchOIDCClaims(mappings []types.ClaimMapping, claims map[string]any) []string {
	var roles []string
	for _, m := range mappings {
		v, ok := claims[m.Claim]
		if !ok {
			continue
		}
		switch t := v.(type) {
		case string:
			if t == m.Value {
				roles = append(roles, m.Roles...)
			}
		case []any:
			for _, item := range t {
				if s, ok := item.(string); ok && s == m.Value {
					roles = append(roles, m.Roles...)
					break
				}
			}
		}
	}
	return apiutils.Deduplicate(roles)
}

// calculateCustomOIDCUser turns verified OIDC claims into a CreateUserParams
// using the connector's claims_to_roles mapping plus any configured login
// rules.
func (a *Server) calculateCustomOIDCUser(
	ctx context.Context,
	diagCtx *SSODiagContext,
	connector types.OIDCConnector,
	claims map[string]any,
	username, userID string,
	req *types.OIDCAuthRequest,
) (*CreateUserParams, error) {
	roles := matchOIDCClaims(connector.GetClaimsToRoles(), claims)
	if len(roles) == 0 {
		return nil, trace.AccessDenied("custom OIDC user %q has no roles matched by claims_to_roles", username)
	}

	traits := claimsToTraits(claims)
	traits[constants.TraitLogins] = []string{username}

	eval, err := a.GetLoginRuleEvaluator().Evaluate(ctx, &loginrule.EvaluationInput{Traits: traits})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	traits = eval.Traits
	diagCtx.Info.AppliedLoginRules = eval.AppliedRules

	fetched, err := services.FetchRolesWithContext(roles, a, services.RoleTemplateContext{
		Username: username,
		Traits:   traits,
	})
	if err != nil {
		return nil, trace.Wrap(err)
	}
	sessionTTL := fetched.AdjustSessionTTL(apidefaults.MaxCertDuration)
	if req.CertTTL > 0 {
		sessionTTL = utils.MinTTL(sessionTTL, req.CertTTL)
	}

	return &CreateUserParams{
		ConnectorName: connector.GetName(),
		Username:      username,
		UserID:        userID,
		KubeGroups:    traits[constants.TraitKubeGroups],
		KubeUsers:     traits[constants.TraitKubeUsers],
		Roles:         roles,
		Traits:        traits,
		SessionTTL:    sessionTTL,
	}, nil
}

// createCustomOIDCUser materializes a Teleport user record from the calculated
// params. In dryRun (SSO test flow) it builds the user object without
// persisting.
func (a *Server) createCustomOIDCUser(ctx context.Context, p *CreateUserParams, dryRun bool) (types.User, error) {
	expires := a.GetClock().Now().UTC().Add(p.SessionTTL)
	user := &types.UserV2{
		Kind:    types.KindUser,
		Version: types.V2,
		Metadata: types.Metadata{
			Name:      p.Username,
			Namespace: apidefaults.Namespace,
			Expires:   &expires,
		},
		Spec: types.UserSpecV2{
			Roles:  p.Roles,
			Traits: p.Traits,
			OIDCIdentities: []types.ExternalIdentity{{
				ConnectorID: p.ConnectorName,
				Username:    p.Username,
				UserID:      p.UserID,
			}},
			CreatedBy: types.CreatedBy{
				User: types.UserRef{Name: teleport.UserSystem},
				Time: a.GetClock().Now().UTC(),
				Connector: &types.ConnectorRef{
					Type:     constants.OIDC,
					ID:       p.ConnectorName,
					Identity: p.Username,
				},
			},
		},
	}
	if dryRun {
		return user, nil
	}
	existing, err := a.Services.GetUser(ctx, p.Username, false)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}
	if existing != nil {
		if !user.GetCreatedBy().Connector.IsSameProvider(existing.GetCreatedBy().Connector) {
			return nil, trace.AlreadyExists("local user %q already exists and was not created by custom OIDC", existing.GetName())
		}
		user.SetRevision(existing.GetRevision())
		if _, err := a.UpdateUser(ctx, user); err != nil {
			return nil, trace.Wrap(err)
		}
		return user, nil
	}
	if _, err := a.CreateUser(ctx, user); err != nil {
		return nil, trace.Wrap(err)
	}
	return user, nil
}

// oidcAuthRequestToAuthclient extracts the public-facing OIDCAuthRequest used
// in the authclient response from the wire-level types.OIDCAuthRequest.
func oidcAuthRequestToAuthclient(req *types.OIDCAuthRequest) authclient.OIDCAuthRequest {
	return authclient.OIDCAuthRequest{
		ConnectorID:       req.ConnectorID,
		CSRFToken:         req.CSRFToken,
		SSHPubKey:         req.SshPublicKey,
		TLSPubKey:         req.TlsPublicKey,
		CreateWebSession:  req.CreateWebSession,
		ClientRedirectURL: req.ClientRedirectURL,
	}
}

// emitOIDCLoginSuccess emits a successful SSO user-login audit event.
func (a *Server) emitOIDCLoginSuccess(ctx context.Context, diagCtx *SSODiagContext, username string) {
	code := events.UserSSOLoginCode
	if diagCtx.Info.TestFlow {
		code = events.UserSSOTestFlowLoginCode
	}
	evt := &apievents.UserLogin{
		Metadata: apievents.Metadata{
			Type: events.UserLoginEvent,
			Code: code,
			Time: time.Now().UTC(),
		},
		Method:             events.LoginMethodOIDC,
		UserMetadata:       apievents.UserMetadata{User: username},
		ConnectionMetadata: authz.ConnectionMetadata(ctx),
		Status:             apievents.Status{Success: true},
	}
	if err := a.emitter.EmitAuditEvent(ctx, evt); err != nil {
		a.logger.WarnContext(ctx, "Failed to emit custom OIDC login event", "error", err)
	}
}
