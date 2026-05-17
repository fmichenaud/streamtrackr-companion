package main

import "testing"

// validateManifestURL is the boundary between "URLs we'll fetch a
// binary from" and "everything else". Anything that gets past here
// becomes auto-update fodder, so the test surface is paranoid by
// design — reject everything we don't explicitly allow.

func TestValidateManifestURL_AllowedHosts(t *testing.T) {
	cases := []string{
		"https://github.com/fmichenaud/streamtrackr-companion/releases/latest/download/manifest.json",
		"https://github.com/anywhere/path",
		"https://objects.githubusercontent.com/something",
	}
	for _, raw := range cases {
		if err := validateManifestURL(raw); err != nil {
			t.Errorf("expected %q to pass, got error: %v", raw, err)
		}
	}
}

func TestValidateManifestURL_RejectsNonHTTPS(t *testing.T) {
	// http:// is forbidden — MITM on plain text is trivial on hostile
	// networks (captive portals, rogue Wi-Fi).
	cases := []string{
		"http://github.com/foo",
		"ftp://github.com/foo",
		"file:///etc/passwd",
		"javascript:alert(1)",
	}
	for _, raw := range cases {
		if err := validateManifestURL(raw); err == nil {
			t.Errorf("expected %q to be rejected, got nil error", raw)
		}
	}
}

func TestValidateManifestURL_RejectsArbitraryHosts(t *testing.T) {
	// The whole point of the allowlist: a manifest URL pointing at any
	// other host is a foothold for arbitrary binary delivery, since
	// the SHA-256 in the manifest is self-attested by whoever served
	// the manifest.
	cases := []string{
		"https://evil.com/manifest.json",
		"https://attacker.github.com.evil.com/foo",     // suffix attack
		"https://github.com.evil.com/foo",              // prefix attack
		"https://raw.githubusercontent.com/manifest",   // not in allowlist
		"https://api.github.com/repos/foo/releases",    // not in allowlist
	}
	for _, raw := range cases {
		if err := validateManifestURL(raw); err == nil {
			t.Errorf("expected %q to be rejected, got nil error", raw)
		}
	}
}

func TestValidateManifestURL_RejectsMalformed(t *testing.T) {
	cases := []string{
		"",
		"not-a-url",
		"://no-scheme",
		// Note: "https://" alone is reported by net/url as a parseable
		// URL with empty host; our allowlist check catches it.
	}
	for _, raw := range cases {
		if err := validateManifestURL(raw); err == nil {
			t.Errorf("expected %q to be rejected, got nil error", raw)
		}
	}
}
