package coreboot

import "testing"

func TestEnvOr(t *testing.T) {
	t.Setenv("CB_TEST_KEY", "set")
	if got := EnvOr("CB_TEST_KEY", "fallback"); got != "set" {
		t.Errorf("EnvOr(set): got %q, want %q", got, "set")
	}
	if got := EnvOr("CB_TEST_KEY_UNSET", "fallback"); got != "fallback" {
		t.Errorf("EnvOr(unset): got %q, want %q", got, "fallback")
	}
	t.Setenv("CB_TEST_KEY_EMPTY", "")
	if got := EnvOr("CB_TEST_KEY_EMPTY", "fallback"); got != "fallback" {
		t.Errorf("EnvOr(empty): got %q, want %q", got, "fallback")
	}
}

func TestEnvInt(t *testing.T) {
	cases := []struct {
		val      string
		fallback int
		want     int
	}{
		{"8", 4, 8},
		{"", 4, 4},
		{"not-a-number", 4, 4},
		{"0", 4, 4},
		{"-1", 4, 4},
	}
	for _, tc := range cases {
		t.Setenv("CB_TEST_INT", tc.val)
		if got := EnvInt("CB_TEST_INT", tc.fallback); got != tc.want {
			t.Errorf("EnvInt(%q, %d): got %d, want %d", tc.val, tc.fallback, got, tc.want)
		}
	}
}

func TestEnvBool(t *testing.T) {
	truthy := []string{"1", "true", "yes"}
	falsy := []string{"", "0", "false", "no", "TRUE", "YES"} // capital-true is intentionally falsy
	for _, v := range truthy {
		t.Setenv("CB_TEST_BOOL", v)
		if !EnvBool("CB_TEST_BOOL") {
			t.Errorf("EnvBool(%q) = false, want true", v)
		}
	}
	for _, v := range falsy {
		t.Setenv("CB_TEST_BOOL", v)
		if EnvBool("CB_TEST_BOOL") {
			t.Errorf("EnvBool(%q) = true, want false", v)
		}
	}
}

func TestResolveDSN(t *testing.T) {
	t.Setenv("CB_DSN_A", "")
	t.Setenv("CB_DSN_B", "postgres://b/db")
	t.Setenv("CB_DSN_C", "postgres://c/db")
	got := ResolveDSN([]string{"CB_DSN_A", "CB_DSN_B", "CB_DSN_C"}, "fallback")
	if got != "postgres://b/db" {
		t.Errorf("ResolveDSN: got %q, want first non-empty", got)
	}

	t.Setenv("CB_DSN_A", "")
	t.Setenv("CB_DSN_B", "")
	t.Setenv("CB_DSN_C", "")
	got = ResolveDSN([]string{"CB_DSN_A", "CB_DSN_B", "CB_DSN_C"}, "fallback")
	if got != "fallback" {
		t.Errorf("ResolveDSN: got %q, want fallback", got)
	}
}

func TestRedactDSN(t *testing.T) {
	cases := []struct {
		dsn  string
		want string
	}{
		{"postgres://user:secret@host:5432/db?sslmode=disable", "postgres://user:***@host:5432/db?sslmode=disable"},
		{"postgres://user@host/db", "postgres://user@host/db"}, // no password to redact
		{"postgres://user:pass@host/db", "postgres://user:***@host/db"},
		{"postgres://host/db", "postgres://host/db"}, // no userinfo
		{"not a url", "not a url"},                   // returned unchanged
	}
	for _, tc := range cases {
		got := RedactDSN(tc.dsn)
		if got != tc.want {
			t.Errorf("RedactDSN(%q): got %q, want %q", tc.dsn, got, tc.want)
		}
	}
}
