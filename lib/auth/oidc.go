/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
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

package auth

import (
	"context"
	"net/url"

	"github.com/gravitational/trace"

	"github.com/gravitational/teleport/api/types"
	apievents "github.com/gravitational/teleport/api/types/events"
	"github.com/gravitational/teleport/lib/auth/authclient"
	"github.com/gravitational/teleport/lib/authz"
	"github.com/gravitational/teleport/lib/events"
)

type OIDCService interface {
	CreateOIDCAuthRequest(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error)
	CreateOIDCAuthRequestForMFA(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error)
	ValidateOIDCAuthCallback(ctx context.Context, q url.Values) (*authclient.OIDCAuthResponse, error)
}

var errOIDCNotImplemented = &trace.AccessDeniedError{Message: "OIDC is only available in enterprise subscriptions"}

// UpsertOIDCConnector creates or updates an OIDC connector.
func (a *Server) UpsertOIDCConnector(ctx context.Context, connector types.OIDCConnector) (types.OIDCConnector, error) {
	upserted, err := a.Services.UpsertOIDCConnector(ctx, connector)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.OIDCConnectorCreate{
		Metadata: apievents.Metadata{
			Type: events.OIDCConnectorCreatedEvent,
			Code: events.OIDCConnectorCreatedCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: connector.GetName(),
		},
	}); err != nil {
		a.logger.WarnContext(ctx, "Failed to emit OIDC connector create event", "error", err)
	}

	return upserted, nil
}

// UpdateOIDCConnector updates an existing OIDC connector.
func (a *Server) UpdateOIDCConnector(ctx context.Context, connector types.OIDCConnector) (types.OIDCConnector, error) {
	updated, err := a.Services.UpdateOIDCConnector(ctx, connector)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.OIDCConnectorUpdate{
		Metadata: apievents.Metadata{
			Type: events.OIDCConnectorUpdatedEvent,
			Code: events.OIDCConnectorUpdatedCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: connector.GetName(),
		},
	}); err != nil {
		a.logger.WarnContext(ctx, "Failed to emit OIDC connector update event", "error", err)
	}

	return updated, nil
}

// CreateOIDCConnector creates a new OIDC connector.
func (a *Server) CreateOIDCConnector(ctx context.Context, connector types.OIDCConnector) (types.OIDCConnector, error) {
	created, err := a.Services.CreateOIDCConnector(ctx, connector)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.OIDCConnectorCreate{
		Metadata: apievents.Metadata{
			Type: events.OIDCConnectorCreatedEvent,
			Code: events.OIDCConnectorCreatedCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: connector.GetName(),
		},
	}); err != nil {
		a.logger.WarnContext(ctx, "Failed to emit OIDC connector create event", "error", err)
	}

	return created, nil
}

// DeleteOIDCConnector deletes an OIDC connector by name.
func (a *Server) DeleteOIDCConnector(ctx context.Context, connectorName string) error {
	if err := a.Services.DeleteOIDCConnector(ctx, connectorName); err != nil {
		return trace.Wrap(err)
	}
	if err := a.emitter.EmitAuditEvent(ctx, &apievents.OIDCConnectorDelete{
		Metadata: apievents.Metadata{
			Type: events.OIDCConnectorDeletedEvent,
			Code: events.OIDCConnectorDeletedCode,
		},
		UserMetadata: authz.ClientUserMetadata(ctx),
		ResourceMetadata: apievents.ResourceMetadata{
			Name: connectorName,
		},
	}); err != nil {
		a.logger.WarnContext(ctx, "Failed to emit OIDC connector delete event", "error", err)
	}
	return nil
}

// CreateOIDCAuthRequest delegates the method call to the oidcAuthService if present,
// or returns a NotImplemented error if not present. Custom sub-kind connectors
// are handled by the in-tree OSS implementation regardless of whether an
// enterprise OIDC service is registered.
func (a *Server) CreateOIDCAuthRequest(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
	if connector, ok := a.lookupCustomOIDCConnector(ctx, req); ok {
		return a.createCustomOIDCAuthRequest(ctx, req, connector)
	}
	if a.oidcAuthService == nil {
		return nil, errOIDCNotImplemented
	}

	rq, err := a.oidcAuthService.CreateOIDCAuthRequest(ctx, req)
	return rq, trace.Wrap(err)
}

// CreateOIDCAuthRequestForMFA delegates the method call to the oidcAuthService if present,
// or returns a NotImplemented error if not present.
func (a *Server) CreateOIDCAuthRequestForMFA(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
	if connector, ok := a.lookupCustomOIDCConnector(ctx, req); ok {
		// SSO MFA over custom OIDC uses the same authorize flow.
		return a.createCustomOIDCAuthRequest(ctx, req, connector)
	}
	if a.oidcAuthService == nil {
		return nil, errOIDCNotImplemented
	}

	rq, err := a.oidcAuthService.CreateOIDCAuthRequestForMFA(ctx, req)
	return rq, trace.Wrap(err)
}

// ValidateOIDCAuthCallback delegates the method call to the oidcAuthService if present,
// or returns a NotImplemented error if not present. The callback dispatcher
// inspects the stored auth request to detect Custom sub-kind connectors and
// routes those to the in-tree OSS implementation.
func (a *Server) ValidateOIDCAuthCallback(ctx context.Context, q url.Values) (*authclient.OIDCAuthResponse, error) {
	if state := q.Get("state"); state != "" {
		if req, err := a.Services.GetOIDCAuthRequest(ctx, state); err == nil {
			if connector, err := a.GetOIDCConnector(ctx, req.ConnectorID, false); err == nil &&
				connector.GetSubKind() == types.OIDCConnectorSubKindCustom {
				return a.validateCustomOIDCAuthCallback(ctx, q)
			}
		}
	}
	if a.oidcAuthService == nil {
		return nil, errOIDCNotImplemented
	}

	resp, err := a.oidcAuthService.ValidateOIDCAuthCallback(ctx, q)
	return resp, trace.Wrap(err)
}

// lookupCustomOIDCConnector resolves the OIDC connector referenced by the
// given auth request and reports whether it is a Custom sub-kind. The
// resolved connector (with secrets) is returned for reuse by the caller.
// For the SSO test flow the connector spec is embedded in the request and the
// sub-kind is implied.
func (a *Server) lookupCustomOIDCConnector(ctx context.Context, req types.OIDCAuthRequest) (types.OIDCConnector, bool) {
	if req.SSOTestFlow && req.ConnectorSpec != nil {
		c, err := types.NewOIDCConnector(req.ConnectorID, *req.ConnectorSpec)
		if err != nil {
			return nil, false
		}
		c.SetSubKind(types.OIDCConnectorSubKindCustom)
		return c, true
	}
	if req.ConnectorID == "" {
		return nil, false
	}
	connector, err := a.GetOIDCConnector(ctx, req.ConnectorID, true)
	if err != nil {
		return nil, false
	}
	if connector.GetSubKind() != types.OIDCConnectorSubKindCustom {
		return nil, false
	}
	return connector, true
}
