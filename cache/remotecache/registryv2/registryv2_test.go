package registryv2

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Unit tests for registryv2 cache backend.
// For E2E tests that use real infrastructure, see e2e_test.go

// TestClientUnit tests the Client methods in isolation.
func TestClientUnit(t *testing.T) {
	t.Run("NewClient", func(t *testing.T) {
		config := Config{
			RegistryURL:  "http://localhost:5000",
			Insecure:     true,
			TouchRefresh: 24 * time.Hour,
		}
		client, err := NewClient(config)
		require.NoError(t, err)
		require.NotNil(t, client)
		require.Equal(t, "http://localhost:5000", client.baseURL)
	})

	t.Run("ConfigWithAuth", func(t *testing.T) {
		config := Config{
			RegistryURL: "http://localhost:5000",
			Token:       "test-token",
		}
		client, err := NewClient(config)
		require.NoError(t, err)
		require.Equal(t, "test-token", client.token)
	})

	t.Run("ConfigWithBasicAuth", func(t *testing.T) {
		config := Config{
			RegistryURL: "http://localhost:5000",
			Username:    "user",
			Password:    "pass",
		}
		client, err := NewClient(config)
		require.NoError(t, err)
		require.Equal(t, "user", client.username)
		require.Equal(t, "pass", client.password)
	})
}

// TestConfigParsing tests configuration parsing from attributes.
func TestConfigParsing(t *testing.T) {
	t.Run("ValidConfig", func(t *testing.T) {
		attrs := map[string]string{
			"registry":      "http://myregistry.com:5000",
			"insecure":      "true",
			"touch_refresh": "12h",
			"username":      "testuser",
			"password":      "testpass",
		}

		config, err := getConfig(attrs)
		require.NoError(t, err)
		require.Equal(t, "http://myregistry.com:5000", config.RegistryURL)
		require.True(t, config.Insecure)
		require.Equal(t, 12*time.Hour, config.TouchRefresh)
		require.Equal(t, "testuser", config.Username)
		require.Equal(t, "testpass", config.Password)
	})

	t.Run("MissingRegistry", func(t *testing.T) {
		attrs := map[string]string{}
		_, err := getConfig(attrs)
		require.Error(t, err)
		require.Contains(t, err.Error(), "registry URL not specified")
	})

	t.Run("DefaultValues", func(t *testing.T) {
		attrs := map[string]string{
			"registry": "http://localhost:5000",
		}

		config, err := getConfig(attrs)
		require.NoError(t, err)
		require.False(t, config.Insecure)
		require.Equal(t, 24*time.Hour, config.TouchRefresh)
	})

	t.Run("TokenAuth", func(t *testing.T) {
		attrs := map[string]string{
			"registry": "http://localhost:5000",
			"token":    "my-auth-token",
		}

		config, err := getConfig(attrs)
		require.NoError(t, err)
		require.Equal(t, "my-auth-token", config.Token)
	})
}
