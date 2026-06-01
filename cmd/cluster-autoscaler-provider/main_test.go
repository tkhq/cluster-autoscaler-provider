package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestValidateRouterSettings(t *testing.T) {
	tests := []struct {
		name     string
		settings RouterRuntimeSettings
		wantErr  string
	}{
		{
			name: "valid",
			settings: RouterRuntimeSettings{
				CacheTTL:          15 * time.Second,
				BackendRPCTimeout: 5 * time.Second,
			},
		},
		{
			name: "zero cache TTL",
			settings: RouterRuntimeSettings{
				CacheTTL:          0,
				BackendRPCTimeout: 5 * time.Second,
			},
			wantErr: "cache TTL must be greater than zero",
		},
		{
			name: "negative cache TTL",
			settings: RouterRuntimeSettings{
				CacheTTL:          -1 * time.Second,
				BackendRPCTimeout: 5 * time.Second,
			},
			wantErr: "cache TTL must be greater than zero",
		},
		{
			name: "zero backend RPC timeout",
			settings: RouterRuntimeSettings{
				CacheTTL:          15 * time.Second,
				BackendRPCTimeout: 0,
			},
			wantErr: "backend RPC timeout must be greater than zero",
		},
		{
			name: "negative backend RPC timeout",
			settings: RouterRuntimeSettings{
				CacheTTL:          15 * time.Second,
				BackendRPCTimeout: -1 * time.Second,
			},
			wantErr: "backend RPC timeout must be greater than zero",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := validateRouterSettings(tt.settings)
			if tt.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, tt.wantErr, err.Error())
		})
	}
}
