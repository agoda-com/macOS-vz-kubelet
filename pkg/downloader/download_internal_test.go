package downloader

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/agoda-com/macOS-vz-kubelet/pkg/event"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/oci"
	"github.com/agoda-com/macOS-vz-kubelet/pkg/resource"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// identityTokenFixture is a deliberately non-secret-shaped marker (clears redaction/leak
// asserts and the remote pre-receive hook) yet unique enough for the byte-for-byte
// token-endpoint assertion.
const identityTokenFixture = "id-token-fixture-value"

// basicPassFixture is the non-secret-shaped password marker for the user/pass characterization lock.
const (
	basicUserFixture = "alice"
	basicPassFixture = "basic-pass-fixture-value"
)

// TestToORASCredentialMapsIdentityTokenToRefreshToken pins the mapping: IdentityToken must land
// in auth.Credential.RefreshToken so the oras-go client takes the OAuth2 refresh-token grant path
// instead of degrading to an anonymous distribution-token GET; Username/Password pass through.
func TestToORASCredentialMapsIdentityTokenToRefreshToken(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		in   resource.RegistryCredentials
	}{
		{
			name: "token only",
			in:   resource.RegistryCredentials{IdentityToken: identityTokenFixture},
		},
		{
			name: "user and password only",
			in:   resource.RegistryCredentials{Username: basicUserFixture, Password: basicPassFixture},
		},
		{
			name: "all three set",
			in: resource.RegistryCredentials{
				Username:      basicUserFixture,
				Password:      basicPassFixture,
				IdentityToken: identityTokenFixture,
			},
		},
		{
			name: "all empty",
			in:   resource.RegistryCredentials{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := toORASCredential(tt.in)

			assert.Equal(t, tt.in.Username, got.Username, "Username must pass through unchanged")
			assert.Equal(t, tt.in.Password, got.Password, "Password must pass through unchanged")
			assert.Equal(t, tt.in.IdentityToken, got.RefreshToken,
				"IdentityToken must map to auth.Credential.RefreshToken")
		})
	}
}

// recordedTokenRequest holds the asserted-on fields of one token-endpoint hit.
type recordedTokenRequest struct {
	method    string
	grantType string
	refresh   string
	authzHdr  string
}

// tokenEndpointRecorder is a concurrency-safe collector of every token-endpoint request. oras-go
// may hit the realm more than once, so tests assert over the whole slice, not a single request.
type tokenEndpointRecorder struct {
	mu       sync.Mutex
	requests []recordedTokenRequest
}

func (r *tokenEndpointRecorder) record(req *http.Request) {
	// ParseForm reads the POST body for the OAuth2 grant; for a GET it just parses the query.
	_ = req.ParseForm()
	rec := recordedTokenRequest{
		method:    req.Method,
		grantType: req.Form.Get("grant_type"),
		refresh:   req.Form.Get("refresh_token"),
		authzHdr:  req.Header.Get("Authorization"),
	}

	r.mu.Lock()
	defer r.mu.Unlock()
	r.requests = append(r.requests, rec)
}

func (r *tokenEndpointRecorder) snapshot() []recordedTokenRequest {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]recordedTokenRequest, len(r.requests))
	copy(out, r.requests)
	return out
}

// newChallengeRegistry builds an httptest Bearer-challenge registry (caller closes the server):
//   - /v2/... answers 401 with a Www-Authenticate challenge pointing at this server's /token realm,
//     until a valid `Authorization: Bearer servertoken` arrives, then 404s the manifest so oras.Copy
//     fails cleanly (the pull error is irrelevant - tests only care which auth path reached /token).
//   - /token records the request and returns a token body.
func newChallengeRegistry(t *testing.T) (*httptest.Server, *tokenEndpointRecorder) {
	t.Helper()

	const serverToken = "servertoken"
	rec := &tokenEndpointRecorder{}

	mux := http.NewServeMux()

	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		rec.record(r)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]string{
			"token":        serverToken,
			"access_token": serverToken,
		})
	})

	// The realm is filled in once the server URL is known (closure over srv below).
	var srv *httptest.Server
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") == "Bearer "+serverToken {
			// Authenticated: fail the manifest fetch cleanly so oras.Copy returns an error
			// without ever needing a real image. The auth handshake has already happened.
			http.Error(w, "manifest not found", http.StatusNotFound)
			return
		}
		challenge := "Bearer realm=\"" + srv.URL + "/token\",service=\"test-service\",scope=\"repository:test/repo:pull\""
		w.Header().Set("Www-Authenticate", challenge)
		w.WriteHeader(http.StatusUnauthorized)
	})

	srv = httptest.NewServer(mux)
	return srv, rec
}

// refForServer builds a loopback image reference from the httptest URL. The 127.0.0.1:<port> host
// is recognised by isLocalhostOrLocalIP, so pull uses PlainHTTP and no TLS is needed.
func refForServer(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	host := strings.TrimPrefix(srv.URL, "http://")
	return host + "/test/repo:latest"
}

// TestPullForwardsIdentityTokenAsRefreshTokenGrant: a token-only credential must drive the token
// endpoint with an OAuth2 refresh-token grant (POST grant_type=refresh_token, refresh_token=<token>),
// not the anonymous distribution-token GET an empty Username/Password (auth.EmptyCredential) yields.
func TestPullForwardsIdentityTokenAsRefreshTokenGrant(t *testing.T) {
	srv, rec := newChallengeRegistry(t)
	defer srv.Close()

	store, err := oci.New(t.TempDir(), false, event.LogEventRecorder{})
	require.NoError(t, err)
	defer func() { _ = store.Close(context.Background()) }()

	// The Copy error is expected (we 404 the manifest after auth) and intentionally ignored:
	// the assertion is purely about which auth path reached /token.
	_ = pull(context.Background(), refForServer(t, srv), store,
		resource.RegistryCredentials{IdentityToken: identityTokenFixture})

	got := rec.snapshot()
	require.NotEmpty(t, got, "the token endpoint must have been hit; if empty, pull failed before auth")

	var foundRefreshGrant bool
	for _, req := range got {
		if req.method == http.MethodPost &&
			req.grantType == "refresh_token" &&
			req.refresh == identityTokenFixture {
			foundRefreshGrant = true
			break
		}
	}
	assert.True(t, foundRefreshGrant,
		"expected at least one POST to /token with grant_type=refresh_token and refresh_token=%q; got %+v",
		identityTokenFixture, got)
}

// TestPullUsesDistributionTokenWithoutIdentityToken characterization-locks existing behavior: a
// user/password credential (no identity token) keeps the distribution-token GET path carrying HTTP
// Basic auth and must NEVER produce an OAuth2 refresh-token grant.
//
// Not tautological: it fails if RefreshToken were set unconditionally (forcing every credential
// onto the OAuth2 path) or the user/password distribution-token path were otherwise broken.
func TestPullUsesDistributionTokenWithoutIdentityToken(t *testing.T) {
	srv, rec := newChallengeRegistry(t)
	defer srv.Close()

	store, err := oci.New(t.TempDir(), false, event.LogEventRecorder{})
	require.NoError(t, err)
	defer func() { _ = store.Close(context.Background()) }()

	_ = pull(context.Background(), refForServer(t, srv), store,
		resource.RegistryCredentials{Username: basicUserFixture, Password: basicPassFixture})

	got := rec.snapshot()
	require.NotEmpty(t, got, "the token endpoint must have been hit; if empty, pull failed before auth")

	wantBasic := "Basic " + base64.StdEncoding.EncodeToString([]byte(basicUserFixture+":"+basicPassFixture))
	var sawBasicGet bool
	for _, req := range got {
		assert.NotEqual(t, "refresh_token", req.grantType,
			"a user/password credential must never produce a refresh_token grant; got %+v", req)
		if req.method == http.MethodGet && req.authzHdr == wantBasic {
			sawBasicGet = true
		}
	}
	assert.True(t, sawBasicGet,
		"expected a distribution-token GET to /token carrying Authorization: Basic <base64(user:pass)>; got %+v", got)
}

// TestPullAuthenticatesWithIdentityToken_Live is the live RED->GREEN tool, skipped unless both
// MVZ_LIVE_REGISTRY_REF and MVZ_LIVE_REGISTRY_IDENTITY_TOKEN are set, so it never runs in CI or the
// unit run. Point it at a real auth-required registry that rejects anonymous pulls: without the fix
// the token-only credential degrades to anonymous and 401s; with it the refresh-token grant
// authenticates and the pull succeeds. No endpoint or token hardcoded - env only.
func TestPullAuthenticatesWithIdentityToken_Live(t *testing.T) {
	ref := os.Getenv("MVZ_LIVE_REGISTRY_REF")
	tok := os.Getenv("MVZ_LIVE_REGISTRY_IDENTITY_TOKEN")
	if ref == "" || tok == "" {
		t.Skip("set MVZ_LIVE_REGISTRY_REF + MVZ_LIVE_REGISTRY_IDENTITY_TOKEN to run the live pull")
	}

	store, err := oci.New(t.TempDir(), false, event.LogEventRecorder{})
	require.NoError(t, err)
	defer func() { _ = store.Close(context.Background()) }()

	// pull derives the registry to authenticate against from ref, so creds.Server is not
	// consulted on this path; a token-only credential is enough.
	err = pull(context.Background(), ref, store,
		resource.RegistryCredentials{IdentityToken: tok})
	require.NoError(t, err, "token-only pull against a real auth-required registry must succeed once IdentityToken is forwarded")
}
