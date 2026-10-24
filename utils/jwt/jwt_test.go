package jwt_test

import (
	"crypto/ecdsa"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/ReCasaOS/CasaOS-Common/utils/jwt"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestJwtFlow(t *testing.T) {
	// Generate a key pair
	privateKey, publicKey, err := jwt.GenerateKeyPair()
	require.NoError(t, err)

	// Generate access and refresh tokens
	username := "testuser"
	id := 1

	accessToken, err := jwt.GetAccessToken(username, privateKey, id)
	require.NoError(t, err)

	refreshToken, err := jwt.GetRefreshToken(username, privateKey, id)
	require.NoError(t, err)

	// Generate JWKS JSON
	jwksJSON, err := jwt.GenerateJwksJSON(publicKey)
	require.NoError(t, err)

	// Serve the JWKS JSON
	server := httptest.NewServer(jwt.JWKSHandler(jwksJSON))
	defer server.Close()

	// Consume the JWKS JSON
	response, err := http.Get(server.URL + "/" + jwt.JWKSPath)
	require.NoError(t, err)
	require.Equal(t, http.StatusOK, response.StatusCode)

	var jwks jwt.JWKS
	err = json.NewDecoder(response.Body).Decode(&jwks)
	require.NoError(t, err)
	require.Len(t, jwks.Keys, 1)

	// Extract the public key from the JWKS JSON
	consumedPublicKey, err := jwt.PublicKeyFromJwksJSON(jwksJSON)
	require.NoError(t, err)

	// Validate the access token
	valid, claims, err := jwt.Validate(accessToken, func() (*ecdsa.PublicKey, error) { return consumedPublicKey, nil })
	require.NoError(t, err)
	assert.True(t, valid)
	assert.Equal(t, username, claims.Username)
	assert.Equal(t, id, claims.ID)

	// A refresh token is signed by the same key but opens no route
	valid, claims, err = jwt.Validate(refreshToken, func() (*ecdsa.PublicKey, error) { return consumedPublicKey, nil })
	assert.Error(t, err)
	assert.False(t, valid)
	assert.Nil(t, claims)

	// nor does any other issuer
	other, err := jwt.GenerateToken(username, privateKey, id, "2fa", time.Minute)
	require.NoError(t, err)
	valid, _, err = jwt.Validate(other, func() (*ecdsa.PublicKey, error) { return consumedPublicKey, nil })
	assert.Error(t, err)
	assert.False(t, valid)
}

func TestInvalidToken(t *testing.T) {
	// Generate a key pair
	privateKey, publicKey, err := jwt.GenerateKeyPair()
	require.NoError(t, err)

	// Generate access token
	username := "testuser"
	id := 1

	accessToken, err := jwt.GetAccessToken(username, privateKey, id)
	require.NoError(t, err)

	// Modify the token to make it invalid
	invalidToken := accessToken[:len(accessToken)-5] + "abcde"

	// Validate the invalid token
	valid, claims, err := jwt.Validate(invalidToken, func() (*ecdsa.PublicKey, error) { return publicKey, nil })
	assert.Error(t, err)
	assert.False(t, valid)
	assert.Nil(t, claims)
}
