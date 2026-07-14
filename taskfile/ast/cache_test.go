package ast

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.yaml.in/yaml/v3"
)

func TestCacheUnmarshal(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		name    string
		yaml    string
		enabled bool
	}{
		{name: "enabled", yaml: "cache: true", enabled: true},
		{name: "disabled", yaml: "cache: false", enabled: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			var task Task
			require.NoError(t, yaml.Unmarshal([]byte(test.yaml), &task))
			assert.Equal(t, test.enabled, task.Cache)
		})
	}
}

func TestCacheRejectsNonBoolean(t *testing.T) {
	t.Parallel()

	var task Task
	err := yaml.Unmarshal([]byte("cache:\n  enabled: true"), &task)
	require.Error(t, err)
}
