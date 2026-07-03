package app

import "testing"

func TestIsSensitiveKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want bool
	}{
		{"exact password", "password", true},
		{"upper case password", "PASSWORD", true},
		{"mixed case token", "MyToken", true},
		{"substring password", "NEO4J_PASSWORD", true},
		{"substring secret", "client_secret_value", true},
		{"substring key", "api_key", true},
		{"substring key upper", "AWS_SECRET_ACCESS_KEY", true},
		{"exact secret", "SECRET", true},
		{"exact token upper", "TOKEN", true},
		{"plain username", "username", false},
		{"plain host", "INFRAHUB_HOST", false},
		{"empty key", "", false},
		{"pass alone is not password", "default_pass", false},
		{"requirepass is not password", "requirepass", false},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isSensitiveKey(tt.key); got != tt.want {
				t.Errorf("isSensitiveKey(%q) = %v, want %v", tt.key, got, tt.want)
			}
		})
	}
}

func TestMaskEnvOutput(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "sensitive key masked",
			input: "NEO4J_PASSWORD=admin123",
			want:  "NEO4J_PASSWORD=" + maskedValue,
		},
		{
			name:  "non-sensitive key untouched",
			input: "INFRAHUB_HOST=localhost",
			want:  "INFRAHUB_HOST=localhost",
		},
		{
			name:  "case-insensitive match",
			input: "db_password=hunter2",
			want:  "db_password=" + maskedValue,
		},
		{
			name:  "empty value still masked",
			input: "API_TOKEN=",
			want:  "API_TOKEN=" + maskedValue,
		},
		{
			name:  "value containing equals fully masked",
			input: "SECRET_URI=postgres://u:p@host?sslmode=verify",
			want:  "SECRET_URI=" + maskedValue,
		},
		{
			name:  "sensitive substring in value only does not mask",
			input: "GREETING=my password is safe",
			want:  "GREETING=my password is safe",
		},
		{
			name:  "line without equals untouched",
			input: "not-an-assignment",
			want:  "not-an-assignment",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
		{
			name:  "multi-line mixed",
			input: "PATH=/usr/bin\nAWS_SECRET_ACCESS_KEY=abc\nHOME=/root\nJWT_TOKEN=xyz\n",
			want:  "PATH=/usr/bin\nAWS_SECRET_ACCESS_KEY=" + maskedValue + "\nHOME=/root\nJWT_TOKEN=" + maskedValue + "\n",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskEnvOutput(tt.input); got != tt.want {
				t.Errorf("maskEnvOutput(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMaskConfigPairs(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "sensitive pair masked",
			input: "masterauth-token\nsupersecretvalue",
			want:  "masterauth-token\n" + maskedValue,
		},
		{
			name:  "non-sensitive pair untouched",
			input: "maxmemory\n0",
			want:  "maxmemory\n0",
		},
		{
			name:  "case-insensitive key match",
			input: "TLS-KEY-FILE\n/etc/redis/tls.key",
			want:  "TLS-KEY-FILE\n" + maskedValue,
		},
		{
			name:  "empty value still masked",
			input: "auth-token\n",
			want:  "auth-token\n" + maskedValue,
		},
		{
			name:  "mixed pairs",
			input: "maxmemory\n100mb\naccess-token\nabc123\nappendonly\nno",
			want:  "maxmemory\n100mb\naccess-token\n" + maskedValue + "\nappendonly\nno",
		},
		{
			name:  "odd trailing key line untouched",
			input: "maxmemory\n0\ndangling-token",
			want:  "maxmemory\n0\ndangling-token",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskConfigPairs(tt.input); got != tt.want {
				t.Errorf("maskConfigPairs(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestMaskErlangConfig(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "sensitive tuple masked",
			input: `{cookie_secret,<<"abc123">>},`,
			want:  "{cookie_secret," + maskedValue + "},",
		},
		{
			name:  "non-sensitive tuple untouched",
			input: `{default_user,<<"guest">>},`,
			want:  `{default_user,<<"guest">>},`,
		},
		{
			name:  "case-insensitive key match",
			input: `{AUTH_TOKEN,<<"xyz">>}`,
			want:  "{AUTH_TOKEN," + maskedValue + "}",
		},
		{
			name:  "empty value still masked",
			input: "{ssl_key,}",
			want:  "{ssl_key," + maskedValue + "}",
		},
		{
			name:  "quoted atom key masked",
			input: `{'secret_backend',classic}`,
			want:  "{secret_backend," + maskedValue + "}",
		},
		{
			name:  "list-valued tuple untouched, flat sensitive tuple masked",
			input: `[{rabbit,[{auth_backends,[rabbit_auth_backend_internal]},{oauth_token_scope,<<"tag">>}]}]`,
			want:  "[{rabbit,[{auth_backends,[rabbit_auth_backend_internal]},{oauth_token_scope," + maskedValue + "}]}]",
		},
		{
			name:  "empty input",
			input: "",
			want:  "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskErlangConfig(tt.input); got != tt.want {
				t.Errorf("maskErlangConfig(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}
