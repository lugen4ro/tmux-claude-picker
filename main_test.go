package main

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func Test_getClaudeDir(t *testing.T) {
	// NOTE: We can not use t.Parallel when t.Setenv is used, because it will affect other tests.
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fail()
	}

	tests := map[string]struct {
		set  bool
		want string
	}{
		"if CLAUDE_CONFIG_DIR is set": {
			set:  true,
			want: "/tmp/claude_config_dir",
		},
		"if CLAUDE_CONFIG_DIR is not set": {
			set: false,
			// This should not be "~/.claude" because getClaudeDir returns the absolute path.
			want: filepath.Join(home, ".claude"),
		},
	}
	for name, tt := range tests {
		t.Run(name, func(t *testing.T) {
			if tt.set {
				t.Setenv("CLAUDE_CONFIG_DIR", "/tmp/claude_config_dir")
			} else {
				// This line is necessary when your local machine has CLAUDE_CONFIG_DIR set.
				t.Setenv("CLAUDE_CONFIG_DIR", "")
			}

			got, err := getClaudeDir()
			require.NoError(t, err)
			if got != tt.want {
				t.Errorf("getClaudeDir() = %v, want %v", got, tt.want)
			}
		})
	}
}
