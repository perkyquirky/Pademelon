package main

import (
	"os"
	"path/filepath"
	"testing"
)

// TestResolveTokenPrecedence pins the resolution order: flag, then
// $PADAMELON_TOKEN, then the file named by $PADAMELON_TOKEN_FILE.
func TestResolveTokenPrecedence(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("from-file\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	tests := []struct {
		name      string
		flagValue string
		env       string
		file      string
		want      string
	}{
		{name: "flag wins", flagValue: "from-flag", env: "from-env", file: tokenFile, want: "from-flag"},
		{name: "env over file", env: "from-env", file: tokenFile, want: "from-env"},
		{name: "file last", file: tokenFile, want: "from-file"},
		{name: "nothing set", want: ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PADAMELON_TOKEN", tc.env)
			t.Setenv("PADAMELON_TOKEN_FILE", tc.file)
			got, source, err := resolveToken(tc.flagValue)
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Errorf("token = %q, want %q", got, tc.want)
			}
			switch {
			case tc.flagValue != "":
				if source != "flag" {
					t.Errorf("source = %q, want flag", source)
				}
			case tc.env != "":
				if source != "environment" {
					t.Errorf("source = %q, want environment", source)
				}
			case tc.file != "":
				if source != "file "+tokenFile {
					t.Errorf("source = %q, want file path", source)
				}
			}
		})
	}
}

// TestResolveTokenFileTrimsWhitespace covers the Docker-secrets case: the
// file conventionally ends with a newline, which must not become part of
// the token.
func TestResolveTokenFileTrimsWhitespace(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "token")
	if err := os.WriteFile(tokenFile, []byte("  secret-token  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PADAMELON_TOKEN", "")
	t.Setenv("PADAMELON_TOKEN_FILE", tokenFile)
	got, _, err := resolveToken("")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "secret-token" {
		t.Errorf("token = %q, want %q (trimmed)", got, "secret-token")
	}
}

// TestResolveTokenRejectsEmpty ensures a configured-but-empty value is a
// loud config error, not a silent disable of auth.
func TestResolveTokenRejectsEmpty(t *testing.T) {
	cases := []struct {
		name      string
		flagValue string
		env       string
		file      string
	}{
		{name: "whitespace flag", flagValue: "   "},
		{name: "whitespace env", env: "  "},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("PADAMELON_TOKEN", tc.env)
			t.Setenv("PADAMELON_TOKEN_FILE", tc.file)
			_, _, err := resolveToken(tc.flagValue)
			if err == nil {
				t.Error("empty configured token should be an error")
			}
		})
	}

	t.Run("missing file", func(t *testing.T) {
		t.Setenv("PADAMELON_TOKEN", "")
		t.Setenv("PADAMELON_TOKEN_FILE", filepath.Join(t.TempDir(), "nope"))
		if _, _, err := resolveToken(""); err == nil {
			t.Error("unreadable token file should be an error")
		}
	})
}
