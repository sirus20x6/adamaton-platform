// SSRF allowlist regression tests for the research proxy. These pin the
// hardened hostAllowed() behaviour: literal IPs (v4/v6, including [::1]),
// FQDN trailing dots, zone-ids, and non-dot-boundary suffix matches must
// not slip past the DEEPRESEARCH_ALLOWED_HOSTS allowlist. No DB / Temporal
// needed — hostAllowed is a pure function over an env-driven allowlist.
package apiserver

import "testing"

func TestHostAllowed_DefaultAllowlist(t *testing.T) {
	t.Setenv("DEEPRESEARCH_ALLOWED_HOSTS", "")
	// Default allowlist is the single exact host "deepresearch.local".
	if !hostAllowed("deepresearch.local") {
		t.Error("default exact host deepresearch.local should be allowed")
	}
	if !hostAllowed("DeepResearch.Local") {
		t.Error("host match should be case-insensitive")
	}
	if !hostAllowed("deepresearch.local.") {
		t.Error("trailing FQDN dot should normalise to the exact host")
	}
	for _, bad := range []string{
		"evil.com",
		"deepresearch.local.evil.com",
		"notdeepresearch.local",
	} {
		if hostAllowed(bad) {
			t.Errorf("host %q must NOT be allowed under default allowlist", bad)
		}
	}
}

func TestHostAllowed_RejectsIPLiterals(t *testing.T) {
	// A suffix rule must never be satisfiable by an IP literal — this is
	// the core SSRF pivot the card targets.
	t.Setenv("DEEPRESEARCH_ALLOWED_HOSTS", ".local")
	for _, ip := range []string{
		"::1",              // url.Hostname() of [::1]
		"127.0.0.1",        // loopback
		"169.254.169.254",  // cloud metadata
		"0.0.0.0",          // wildcard
		"10.0.0.5",         // RFC1918
		"::ffff:127.0.0.1", // IPv4-mapped IPv6
		"fe80::1",          // link-local
	} {
		if hostAllowed(ip) {
			t.Errorf("IP literal %q must be rejected (suffix rules don't apply to IPs)", ip)
		}
	}
	// A real *.local host is still allowed under the same suffix rule.
	if !hostAllowed("deepresearch.local") {
		t.Error("deepresearch.local should match the .local suffix rule")
	}
	if !hostAllowed("box.deepresearch.local") {
		t.Error("subdomain box.deepresearch.local should match the .local suffix rule")
	}
}

func TestHostAllowed_IPv6ZoneIDStripped(t *testing.T) {
	t.Setenv("DEEPRESEARCH_ALLOWED_HOSTS", ".local")
	// A zone id must not be used to dodge the IP-literal check.
	if hostAllowed("fe80::1%eth0") {
		t.Error("IPv6 literal with zone id must still be rejected")
	}
}

func TestHostAllowed_ExplicitIPAllowlist(t *testing.T) {
	// An operator who explicitly allowlists an IP gets exactly that IP —
	// the only blessed way an IP passes.
	t.Setenv("DEEPRESEARCH_ALLOWED_HOSTS", "127.0.0.1, .local")
	if !hostAllowed("127.0.0.1") {
		t.Error("explicitly allowlisted IP should pass")
	}
	if hostAllowed("127.0.0.2") {
		t.Error("a non-allowlisted IP must still be rejected")
	}
}

func TestHostAllowed_SuffixDotBoundary(t *testing.T) {
	t.Setenv("DEEPRESEARCH_ALLOWED_HOSTS", ".deepresearch.local")
	if !hostAllowed("deepresearch.local") {
		t.Error("host equal to the suffix sans leading dot should be allowed")
	}
	if !hostAllowed("api.deepresearch.local") {
		t.Error("true subdomain should be allowed")
	}
	for _, bad := range []string{
		"xdeepresearch.local",           // no dot boundary before suffix-bare
		"deepresearch.local.attacker.x", // suffix in the middle
		"evil-deepresearch.local",       // shares trailing bytes w/o boundary
	} {
		if hostAllowed(bad) {
			t.Errorf("host %q must NOT match .deepresearch.local across a non-dot boundary", bad)
		}
	}
}

func TestHostAllowed_EmptyAndWhitespace(t *testing.T) {
	t.Setenv("DEEPRESEARCH_ALLOWED_HOSTS", "")
	for _, bad := range []string{"", "   ", "\t"} {
		if hostAllowed(bad) {
			t.Errorf("empty/whitespace host %q must be rejected", bad)
		}
	}
}

func TestHostAllowed_PerRequestReread(t *testing.T) {
	// The hardened impl re-reads the env each call (no sync.Once), so a
	// tightened allowlist takes effect immediately.
	t.Setenv("DEEPRESEARCH_ALLOWED_HOSTS", "host-a.local")
	if !hostAllowed("host-a.local") {
		t.Fatal("host-a.local should be allowed initially")
	}
	t.Setenv("DEEPRESEARCH_ALLOWED_HOSTS", "host-b.local")
	if hostAllowed("host-a.local") {
		t.Error("host-a.local must be rejected after the allowlist is changed (env re-read per call)")
	}
	if !hostAllowed("host-b.local") {
		t.Error("host-b.local should be allowed after the allowlist change")
	}
}
