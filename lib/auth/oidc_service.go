/*
 * Teleport
 * Copyright (C) 2023  Gravitational, Inc.
 * Copyright (C) 2026  FlytBase, Inc.
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU Affero General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 */

package auth

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	gojose "github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
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

// oidcService implements the OIDCService interface using standard OIDC.
// It is provider-agnostic and works with any OIDC-compliant IdP
// (Keycloak, Okta, Auth0, Google, Azure AD, etc.).
type oidcService struct {
	server *Server
}

// NewOIDCService creates a new OIDCService backed by the given auth server.
func NewOIDCService(server *Server) OIDCService {
	return &oidcService{server: server}
}

// oidcDiscovery holds the endpoints from OIDC provider discovery.
type oidcDiscovery struct {
	Issuer                string `json:"issuer"`
	AuthorizationEndpoint string `json:"authorization_endpoint"`
	TokenEndpoint         string `json:"token_endpoint"`
	JWKSURI               string `json:"jwks_uri"`
	UserinfoEndpoint      string `json:"userinfo_endpoint"`
}

// discover fetches the OIDC discovery document from the provider.
func discover(ctx context.Context, issuerURL string) (*oidcDiscovery, error) {
	wellKnown := strings.TrimRight(issuerURL, "/") + "/.well-known/openid-configuration"
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, wellKnown, nil)
	if err != nil {
		return nil, trace.Wrap(err, "creating discovery request")
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, trace.Wrap(err, "fetching OIDC discovery document from %s", wellKnown)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, trace.BadParameter("OIDC discovery returned HTTP %d from %s", resp.StatusCode, wellKnown)
	}
	var disc oidcDiscovery
	if err := json.NewDecoder(resp.Body).Decode(&disc); err != nil {
		return nil, trace.Wrap(err, "decoding OIDC discovery document")
	}
	if disc.AuthorizationEndpoint == "" || disc.TokenEndpoint == "" || disc.JWKSURI == "" {
		return nil, trace.BadParameter("OIDC discovery document missing required endpoints")
	}
	return &disc, nil
}

// fetchJWKS retrieves the JSON Web Key Set from the provider.
func fetchJWKS(ctx context.Context, jwksURI string) (*gojose.JSONWebKeySet, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, jwksURI, nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, trace.Wrap(err, "fetching JWKS from %s", jwksURI)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, trace.BadParameter("JWKS endpoint returned HTTP %d", resp.StatusCode)
	}
	var jwks gojose.JSONWebKeySet
	if err := json.NewDecoder(resp.Body).Decode(&jwks); err != nil {
		return nil, trace.Wrap(err, "decoding JWKS")
	}
	return &jwks, nil
}

// CreateOIDCAuthRequest generates an OIDC auth request with a redirect URL
// pointing to the identity provider's authorization endpoint.
func (s *oidcService) CreateOIDCAuthRequest(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
	connector, err := s.server.Services.GetOIDCConnector(ctx, req.ConnectorID, true)
	if err != nil {
		return nil, trace.Wrap(err, "failed to get OIDC connector %q", req.ConnectorID)
	}

	disc, err := discover(ctx, connector.GetIssuerURL())
	if err != nil {
		return nil, trace.Wrap(err, "OIDC discovery failed for connector %q", req.ConnectorID)
	}

	req.StateToken, err = utils.CryptoRandomHex(defaults.TokenLenBytes)
	if err != nil {
		return nil, trace.Wrap(err)
	}

	redirectURLs := connector.GetRedirectURLs()
	if len(redirectURLs) == 0 {
		return nil, trace.BadParameter("OIDC connector %q has no redirect URLs configured", req.ConnectorID)
	}

	scopes := connector.GetScope()
	if len(scopes) == 0 {
		scopes = []string{"openid", "profile", "email"}
	}

	config := oauth2.Config{
		ClientID:     connector.GetClientID(),
		ClientSecret: connector.GetClientSecret(),
		RedirectURL:  redirectURLs[0],
		Scopes:       scopes,
		Endpoint: oauth2.Endpoint{
			AuthURL:  disc.AuthorizationEndpoint,
			TokenURL: disc.TokenEndpoint,
		},
	}

	opts := []oauth2.AuthCodeOption{}
	if acr := connector.GetACR(); acr != "" {
		opts = append(opts, oauth2.SetAuthURLParam("acr_values", acr))
	}
	if prompt := connector.GetPrompt(); prompt != "" {
		opts = append(opts, oauth2.SetAuthURLParam("prompt", prompt))
	}

	req.RedirectURL = config.AuthCodeURL(req.StateToken, opts...)

	if err := s.server.Services.CreateOIDCAuthRequest(ctx, req, defaults.OIDCAuthRequestTTL); err != nil {
		return nil, trace.Wrap(err)
	}

	s.server.logger.DebugContext(ctx, "Created OIDC auth request",
		"connector", req.ConnectorID,
		"redirect_url", req.RedirectURL,
	)

	return &req, nil
}

// CreateOIDCAuthRequestForMFA is not implemented for OSS.
func (s *oidcService) CreateOIDCAuthRequestForMFA(ctx context.Context, req types.OIDCAuthRequest) (*types.OIDCAuthRequest, error) {
	return nil, errOIDCNotImplemented
}

// ValidateOIDCAuthCallback handles the OIDC callback after the user
// authenticates with the identity provider. It exchanges the authorization
// code for tokens, verifies the ID token, maps claims to roles, creates
// the user, and returns session credentials.
func (s *oidcService) ValidateOIDCAuthCallback(ctx context.Context, q url.Values) (*authclient.OIDCAuthResponse, error) {
	logger := s.server.logger.With(teleport.ComponentKey, "oidc")

	diagCtx := NewSSODiagContext(types.KindOIDC, s.server)

	event := &apievents.UserLogin{
		Metadata: apievents.Metadata{
			Type: events.UserLoginEvent,
		},
		Method:             events.LoginMethodOIDC,
		ConnectionMetadata: authz.ConnectionMetadata(ctx),
	}

	auth, err := s.validateCallback(ctx, diagCtx, q, logger)
	diagCtx.Info.Error = trace.UserMessage(err)
	diagCtx.WriteToBackend(ctx)

	if err != nil {
		event.Code = events.UserSSOLoginFailureCode
		if diagCtx.Info.TestFlow {
			event.Code = events.UserSSOTestFlowLoginFailureCode
		}
		event.Status.Success = false
		event.Status.Error = trace.Unwrap(err).Error()
		event.Status.UserMessage = err.Error()
		if emitErr := s.server.emitter.EmitAuditEvent(ctx, event); emitErr != nil {
			logger.WarnContext(ctx, "Failed to emit OIDC login failed event", "error", emitErr)
		}
		return nil, trace.Wrap(err)
	}

	event.Code = events.UserSSOLoginCode
	if diagCtx.Info.TestFlow {
		event.Code = events.UserSSOTestFlowLoginCode
	}
	event.Status.Success = true
	event.User = auth.Username
	if emitErr := s.server.emitter.EmitAuditEvent(ctx, event); emitErr != nil {
		logger.WarnContext(ctx, "Failed to emit OIDC login event", "error", emitErr)
	}

	return auth, nil
}

// validateCallback is the internal implementation of the OIDC callback validation.
func (s *oidcService) validateCallback(ctx context.Context, diagCtx *SSODiagContext, q url.Values, logger *slog.Logger) (*authclient.OIDCAuthResponse, error) {
	// Check for error response from provider.
	if errParam := q.Get("error"); errParam != "" {
		state := q.Get("state")
		if state != "" {
			diagCtx.RequestID = state
			req, err := s.server.Services.GetOIDCAuthRequest(ctx, state)
			if err == nil {
				diagCtx.Info.TestFlow = req.SSOTestFlow
			}
		}
		errDesc := q.Get("error_description")
		oauthErr := trace.OAuth2("invalid_request", errParam, q)
		return nil, trace.WithUserMessage(oauthErr, "OIDC provider returned error: %v [%v]", errDesc, errParam)
	}

	code := q.Get("code")
	if code == "" {
		oauthErr := trace.OAuth2("invalid_request", "code query param must be set", q)
		return nil, trace.WithUserMessage(oauthErr, "Invalid parameters received from OIDC provider.")
	}

	stateToken := q.Get("state")
	if stateToken == "" {
		oauthErr := trace.OAuth2("invalid_request", "missing state query param", q)
		return nil, trace.WithUserMessage(oauthErr, "Invalid parameters received from OIDC provider.")
	}
	diagCtx.RequestID = stateToken

	// Retrieve the stored auth request (validates CSRF state token).
	req, err := s.server.Services.GetOIDCAuthRequest(ctx, stateToken)
	if err != nil {
		return nil, trace.Wrap(err, "failed to get OIDC auth request")
	}
	diagCtx.Info.TestFlow = req.SSOTestFlow

	// Get connector configuration.
	connector, err := s.server.Services.GetOIDCConnector(ctx, req.ConnectorID, true)
	if err != nil {
		return nil, trace.Wrap(err, "failed to get OIDC connector %q", req.ConnectorID)
	}

	// Run OIDC discovery.
	disc, err := discover(ctx, connector.GetIssuerURL())
	if err != nil {
		return nil, trace.Wrap(err, "OIDC discovery failed")
	}

	redirectURLs := connector.GetRedirectURLs()
	if len(redirectURLs) == 0 {
		return nil, trace.BadParameter("OIDC connector has no redirect URLs")
	}

	// Exchange authorization code for tokens.
	config := oauth2.Config{
		ClientID:     connector.GetClientID(),
		ClientSecret: connector.GetClientSecret(),
		RedirectURL:  redirectURLs[0],
		Endpoint: oauth2.Endpoint{
			AuthURL:  disc.AuthorizationEndpoint,
			TokenURL: disc.TokenEndpoint,
		},
	}

	token, err := config.Exchange(ctx, code)
	if err != nil {
		return nil, trace.Wrap(err, "OIDC token exchange failed")
	}

	// Extract and verify ID token.
	rawIDToken, ok := token.Extra("id_token").(string)
	if !ok || rawIDToken == "" {
		return nil, trace.BadParameter("OIDC token response missing id_token")
	}

	claims, err := s.verifyAndExtractClaims(ctx, rawIDToken, disc, connector)
	if err != nil {
		return nil, trace.Wrap(err, "OIDC ID token verification failed")
	}

	// Optionally enrich claims from userinfo endpoint.
	if disc.UserinfoEndpoint != "" {
		userinfoClaims, err := s.fetchUserinfo(ctx, disc.UserinfoEndpoint, token.AccessToken)
		if err != nil {
			logger.WarnContext(ctx, "Failed to fetch userinfo, continuing with ID token claims only", "error", err)
		} else {
			// Merge userinfo claims into ID token claims (ID token takes precedence).
			for k, v := range userinfoClaims {
				if _, exists := claims[k]; !exists {
					claims[k] = v
				}
			}
		}
	}

	logger.DebugContext(ctx, "Extracted OIDC claims",
		"claims_keys", claimKeys(claims),
		"connector", connector.GetName(),
	)

	// Map claims to roles.
	roles, kubeGroups, kubeUsers := s.mapClaimsToRoles(connector, claims)
	if len(roles) == 0 {
		return nil, trace.AccessDenied("no roles mapped for OIDC user; check connector claims_to_roles configuration")
	}

	// Determine username.
	username := s.resolveUsername(connector, claims)
	if username == "" {
		return nil, trace.BadParameter("could not determine username from OIDC claims (tried username_claim, email, preferred_username, sub)")
	}

	// Build user params.
	p := CreateUserParams{
		ConnectorName: connector.GetName(),
		Username:      username,
		Roles:         roles,
		KubeGroups:    kubeGroups,
		KubeUsers:     kubeUsers,
		Traits: map[string][]string{
			constants.TraitLogins:     {username},
			constants.TraitKubeGroups: kubeGroups,
			constants.TraitKubeUsers:  kubeUsers,
		},
	}

	// Add OIDC claims as traits.
	for k, v := range claims {
		traitKey := fmt.Sprintf("oidc/%s", k)
		switch val := v.(type) {
		case string:
			p.Traits[traitKey] = []string{val}
		case []interface{}:
			var strs []string
			for _, item := range val {
				strs = append(strs, fmt.Sprintf("%v", item))
			}
			p.Traits[traitKey] = strs
		}
	}

	// Apply login rules.
	evaluationInput := &loginrule.EvaluationInput{
		Traits: p.Traits,
	}
	evaluationOutput, err := s.server.GetLoginRuleEvaluator().Evaluate(ctx, evaluationInput)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	p.Traits = evaluationOutput.Traits
	diagCtx.Info.AppliedLoginRules = evaluationOutput.AppliedRules
	p.KubeGroups = p.Traits[constants.TraitKubeGroups]
	p.KubeUsers = p.Traits[constants.TraitKubeUsers]

	// Calculate session TTL.
	fetchedRoles, err := services.FetchRoles(p.Roles, s.server, p.Traits)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	roleTTL := fetchedRoles.AdjustSessionTTL(apidefaults.MaxCertDuration)
	p.SessionTTL = utils.MinTTL(roleTTL, req.CertTTL)

	diagCtx.Info.CreateUserParams = &types.CreateUserParams{
		ConnectorName: p.ConnectorName,
		Username:      p.Username,
		KubeGroups:    p.KubeGroups,
		KubeUsers:     p.KubeUsers,
		Roles:         p.Roles,
		Traits:        p.Traits,
		SessionTTL:    types.Duration(p.SessionTTL),
	}

	// Create or update user.
	user, err := s.createOIDCUser(ctx, &p, req.SSOTestFlow)
	if err != nil {
		return nil, trace.Wrap(err, "failed to create OIDC user")
	}

	if err := s.server.CallLoginHooks(ctx, user); err != nil {
		return nil, trace.Wrap(err)
	}

	userState, err := s.server.GetUserOrLoginState(ctx, user.GetName())
	if err != nil {
		return nil, trace.Wrap(err)
	}

	// In test flow, skip session/cert creation.
	if req.SSOTestFlow {
		diagCtx.Info.Success = true
		return &authclient.OIDCAuthResponse{
			Username: p.Username,
			Identity: types.ExternalIdentity{
				ConnectorID: p.ConnectorName,
				Username:    p.Username,
			},
			Req: oidcAuthRequestFromProto(req),
		}, nil
	}

	return s.makeOIDCAuthResponse(ctx, req, userState, &p, logger)
}

// verifyAndExtractClaims parses the ID token, verifies its signature against
// the provider's JWKS, and returns the claims map.
func (s *oidcService) verifyAndExtractClaims(ctx context.Context, rawIDToken string, disc *oidcDiscovery, connector types.OIDCConnector) (map[string]interface{}, error) {
	tok, err := jwt.ParseSigned(rawIDToken, []gojose.SignatureAlgorithm{gojose.RS256, gojose.RS384, gojose.RS512, gojose.ES256, gojose.ES384, gojose.ES512, gojose.PS256, gojose.PS384, gojose.PS512})
	if err != nil {
		return nil, trace.Wrap(err, "parsing ID token")
	}

	jwks, err := fetchJWKS(ctx, disc.JWKSURI)
	if err != nil {
		return nil, trace.Wrap(err, "fetching JWKS")
	}

	// Find the signing key.
	headers := tok.Headers
	if len(headers) == 0 {
		return nil, trace.BadParameter("ID token has no headers")
	}
	kid := headers[0].KeyID

	keys := jwks.Key(kid)
	if len(keys) == 0 {
		return nil, trace.BadParameter("no matching key found in JWKS for kid=%q", kid)
	}

	// Verify signature and extract claims.
	var allClaims map[string]interface{}
	if err := tok.Claims(keys[0].Key, &allClaims); err != nil {
		return nil, trace.Wrap(err, "verifying ID token signature")
	}

	// Validate issuer.
	if iss, ok := allClaims["iss"].(string); !ok || iss != disc.Issuer {
		return nil, trace.AccessDenied("ID token issuer %q does not match expected %q", allClaims["iss"], disc.Issuer)
	}

	// Validate audience.
	switch aud := allClaims["aud"].(type) {
	case string:
		if aud != connector.GetClientID() {
			return nil, trace.AccessDenied("ID token audience %q does not match client ID %q", aud, connector.GetClientID())
		}
	case []interface{}:
		found := false
		for _, a := range aud {
			if fmt.Sprintf("%v", a) == connector.GetClientID() {
				found = true
				break
			}
		}
		if !found {
			return nil, trace.AccessDenied("ID token audience does not contain client ID %q", connector.GetClientID())
		}
	default:
		return nil, trace.BadParameter("unexpected aud claim type in ID token")
	}

	// Validate expiry.
	if exp, ok := allClaims["exp"].(float64); ok {
		if time.Unix(int64(exp), 0).Before(s.server.GetClock().Now()) {
			return nil, trace.AccessDenied("ID token has expired")
		}
	}

	return allClaims, nil
}

// fetchUserinfo calls the OIDC userinfo endpoint to get additional claims.
func (s *oidcService) fetchUserinfo(ctx context.Context, userinfoURL string, accessToken string) (map[string]interface{}, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, userinfoURL, nil)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	req.Header.Set("Authorization", "Bearer "+accessToken)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, trace.Wrap(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, trace.BadParameter("userinfo endpoint returned HTTP %d: %s", resp.StatusCode, string(body))
	}

	var claims map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&claims); err != nil {
		return nil, trace.Wrap(err)
	}
	return claims, nil
}

// mapClaimsToRoles maps OIDC claims to Teleport roles using the connector's
// claims_to_roles configuration.
func (s *oidcService) mapClaimsToRoles(connector types.OIDCConnector, claims map[string]interface{}) (roles, kubeGroups, kubeUsers []string) {
	for _, mapping := range connector.GetClaimsToRoles() {
		claimValues := getClaimValues(claims, mapping.Claim)
		for _, v := range claimValues {
			if claimValueMatches(mapping.Value, v) {
				roles = append(roles, mapping.Roles...)
			}
		}
	}
	return apiutils.Deduplicate(roles), apiutils.Deduplicate(kubeGroups), apiutils.Deduplicate(kubeUsers)
}

// getClaimValues extracts claim values as a string slice.
// Handles both single string values and arrays.
func getClaimValues(claims map[string]interface{}, claimName string) []string {
	v, ok := claims[claimName]
	if !ok {
		return nil
	}
	switch val := v.(type) {
	case string:
		return []string{val}
	case []interface{}:
		var result []string
		for _, item := range val {
			result = append(result, fmt.Sprintf("%v", item))
		}
		return result
	case []string:
		return val
	default:
		return []string{fmt.Sprintf("%v", val)}
	}
}

// claimValueMatches checks if a claim value matches the mapping value.
// Supports exact match and regex patterns.
func claimValueMatches(pattern, value string) bool {
	if pattern == value {
		return true
	}
	re, err := regexp.Compile("^" + pattern + "$")
	if err != nil {
		return false
	}
	return re.MatchString(value)
}

// resolveUsername determines the Teleport username from OIDC claims.
func (s *oidcService) resolveUsername(connector types.OIDCConnector, claims map[string]interface{}) string {
	// 1. Explicit username_claim from connector config.
	if uc := connector.GetUsernameClaim(); uc != "" {
		if v, ok := claims[uc].(string); ok && v != "" {
			return v
		}
	}
	// 2. email claim.
	if v, ok := claims["email"].(string); ok && v != "" {
		return v
	}
	// 3. preferred_username claim.
	if v, ok := claims["preferred_username"].(string); ok && v != "" {
		return v
	}
	// 4. sub claim (fallback).
	if v, ok := claims["sub"].(string); ok && v != "" {
		return v
	}
	return ""
}

// createOIDCUser creates or updates a Teleport user from OIDC claims.
// Mirrors createGithubUser in github.go but uses OIDCIdentities.
func (s *oidcService) createOIDCUser(ctx context.Context, p *CreateUserParams, dryRun bool) (types.User, error) {
	s.server.logger.DebugContext(ctx, "Generating dynamic OIDC identity",
		"connector_name", p.ConnectorName,
		"user_name", p.Username,
		"roles", p.Roles,
		"dry_run", dryRun,
	)

	expires := s.server.GetClock().Now().UTC().Add(p.SessionTTL)

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
			}},
			CreatedBy: types.CreatedBy{
				User: types.UserRef{Name: teleport.UserSystem},
				Time: s.server.GetClock().Now().UTC(),
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

	existingUser, err := s.server.Services.GetUser(ctx, p.Username, false)
	if err != nil && !trace.IsNotFound(err) {
		return nil, trace.Wrap(err)
	}

	if existingUser != nil {
		ref := user.GetCreatedBy().Connector
		if !ref.IsSameProvider(existingUser.GetCreatedBy().Connector) {
			return nil, trace.AlreadyExists("local user %q already exists and is not an OIDC user",
				existingUser.GetName())
		}
		user.SetRevision(existingUser.GetRevision())
		if _, err := s.server.UpdateUser(ctx, user); err != nil {
			return nil, trace.Wrap(err)
		}
	} else {
		if _, err := s.server.CreateUser(ctx, user); err != nil {
			return nil, trace.Wrap(err)
		}
	}

	return user, nil
}

// makeOIDCAuthResponse creates the auth response with web session and/or
// certificates, mirroring makeGithubAuthResponse in github.go.
func (s *oidcService) makeOIDCAuthResponse(
	ctx context.Context,
	req *types.OIDCAuthRequest,
	userState services.UserState,
	p *CreateUserParams,
	logger *slog.Logger,
) (*authclient.OIDCAuthResponse, error) {
	auth := authclient.OIDCAuthResponse{
		Req: oidcAuthRequestFromProto(req),
		Identity: types.ExternalIdentity{
			ConnectorID: p.ConnectorName,
			Username:    p.Username,
		},
		Username: userState.GetName(),
	}

	// Create web session if requested.
	if req.CreateWebSession {
		session, err := s.server.CreateWebSessionFromReq(ctx, NewWebSessionRequest{
			User:                 userState.GetName(),
			Roles:                userState.GetRoles(),
			Traits:               userState.GetTraits(),
			SessionTTL:           p.SessionTTL,
			LoginTime:            s.server.clock.Now().UTC(),
			LoginIP:              req.ClientLoginIP,
			LoginUserAgent:       req.ClientUserAgent,
			AttestWebSession:     true,
			CreateDeviceWebToken: true,
		})
		if err != nil {
			return nil, trace.Wrap(err, "failed to create web session")
		}
		auth.Session = session
	}

	// Sign SSH/TLS certificates if public keys were provided.
	if len(req.SshPublicKey) != 0 || len(req.TlsPublicKey) != 0 {
		sshCert, tlsCert, err := s.server.CreateSessionCerts(ctx, &SessionCertsRequest{
			UserState:               userState,
			SessionTTL:              p.SessionTTL,
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
			return nil, trace.Wrap(err, "failed to create session certificates")
		}

		clusterName, err := s.server.GetClusterName(ctx)
		if err != nil {
			return nil, trace.Wrap(err, "failed to obtain cluster name")
		}

		auth.Cert = sshCert
		auth.TLSCert = tlsCert

		authority, err := s.server.GetCertAuthority(ctx, types.CertAuthID{
			Type:       types.HostCA,
			DomainName: clusterName.GetClusterName(),
		}, false)
		if err != nil {
			return nil, trace.Wrap(err, "failed to obtain cluster's host CA")
		}
		auth.HostSigners = append(auth.HostSigners, authority)
	}

	if o, err := s.server.ClientOptionsForLogin(userState); err == nil {
		auth.ClientOptions = o
	} else {
		logger.WarnContext(ctx, "Failed to calculate client options for OIDC login", "username", userState.GetName(), "error", err)
	}

	return &auth, nil
}

// oidcAuthRequestFromProto converts a protobuf OIDCAuthRequest to the
// authclient JSON-friendly version.
func oidcAuthRequestFromProto(req *types.OIDCAuthRequest) authclient.OIDCAuthRequest {
	return authclient.OIDCAuthRequest{
		ConnectorID:       req.ConnectorID,
		CSRFToken:         req.CSRFToken,
		SSHPubKey:         req.SshPublicKey,
		TLSPubKey:         req.TlsPublicKey,
		CreateWebSession:  req.CreateWebSession,
		ClientRedirectURL: req.ClientRedirectURL,
	}
}

// claimKeys returns the keys from a claims map for logging.
func claimKeys(claims map[string]interface{}) []string {
	keys := make([]string, 0, len(claims))
	for k := range claims {
		keys = append(keys, k)
	}
	return keys
}
