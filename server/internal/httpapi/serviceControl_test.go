package httpapi

import (
	"strings"
	"testing"

	"nodeops/server/internal/sshclient"
)

// These tests cover parseServiceStates, the pure half of readServiceStates.
// The SSH call itself is not exercised here — that needs a live host — but the
// parsing rules carry all the subtle cases worth guarding: units systemd does
// not know about, truncated output, and multi-unit separation.

func TestReadServiceStatesParsesFullRecord(t *testing.T) {
	stdout := strings.Join([]string{
		"UNIT=hysteria-server",
		"ACTIVE=active",
		"ENABLED=enabled",
		"SINCE=Mon 2026-09-29 10:00:00 UTC",
		"SEP=0",
	}, "\n")

	units := parseServiceStates(stdout, "", []string{"hysteria-server"})
	if len(units) != 1 {
		t.Fatalf("want 1 unit, got %d", len(units))
	}
	got := units[0]
	if got.Unit != "hysteria-server" {
		t.Errorf("unit = %q", got.Unit)
	}
	if got.Active != "active" {
		t.Errorf("active = %q", got.Active)
	}
	if got.Enabled != "enabled" {
		t.Errorf("enabled = %q", got.Enabled)
	}
	if got.Since != "Mon 2026-09-29 10:00:00 UTC" {
		t.Errorf("since = %q", got.Since)
	}
}

// A unit systemd does not know about emits no property lines at all. The parser
// must still yield a row, marked unknown, rather than silently dropping it —
// otherwise the UI would show nothing and look like a missing service.
func TestReadServiceStatesMissingUnitStillYieldsRow(t *testing.T) {
	stdout := strings.Join([]string{
		"UNIT=ghost-service",
		"SEP=0",
	}, "\n")

	units := parseServiceStates(stdout, "", []string{"ghost-service"})
	if len(units) != 1 {
		t.Fatalf("want 1 unit, got %d", len(units))
	}
	if units[0].Active != "unknown" {
		t.Errorf("active = %q, want unknown", units[0].Active)
	}
	if units[0].Enabled != "unknown" {
		t.Errorf("enabled = %q, want unknown", units[0].Enabled)
	}
}

// Multiple units must not bleed into each other: the separator is what makes
// that guarantee, so it deserves an explicit test.
func TestReadServiceStatesKeepsUnitsSeparate(t *testing.T) {
	stdout := strings.Join([]string{
		"UNIT=a.service",
		"ACTIVE=active",
		"ENABLED=enabled",
		"SINCE=t1",
		"SEP=0",
		"UNIT=b.service",
		"ACTIVE=failed",
		"ENABLED=disabled",
		"SINCE=t2",
		"SEP=1",
	}, "\n")

	units := parseServiceStates(stdout, "", []string{"b.service", "a.service"})
	if len(units) != 2 {
		t.Fatalf("want 2 units, got %d", len(units))
	}
	// Output is sorted by unit name.
	if units[0].Unit != "a.service" || units[0].Active != "active" {
		t.Errorf("first = %+v", units[0])
	}
	if units[1].Unit != "b.service" || units[1].Active != "failed" || units[1].Enabled != "disabled" {
		t.Errorf("second = %+v", units[1])
	}
}

// Output with no trailing separator (truncated by a dropped connection) must not
// lose the last unit's data.
func TestReadServiceStatesFlushesFinalRecordWithoutSeparator(t *testing.T) {
	stdout := strings.Join([]string{
		"UNIT=only.service",
		"ACTIVE=active",
		"ENABLED=static",
		"SINCE=t9",
	}, "\n")

	units := parseServiceStates(stdout, "", []string{"only.service"})
	if len(units) != 1 {
		t.Fatalf("want 1 unit, got %d", len(units))
	}
	if units[0].Active != "active" || units[0].Enabled != "static" {
		t.Errorf("got %+v", units[0])
	}
}

// A requested unit that produced nothing at all still gets a row carrying the
// stderr hint, so the operator sees why it is blank.
func TestReadServiceStatesFillsRequestedUnitsNotInOutput(t *testing.T) {
	units := parseServiceStates("", "Failed to get properties: Unit not found.", []string{"missing.service"})
	if len(units) != 1 {
		t.Fatalf("want 1 unit, got %d", len(units))
	}
	if units[0].Active != "unknown" {
		t.Errorf("active = %q, want unknown", units[0].Active)
	}
	if units[0].Error == "" {
		t.Error("expected an error hint explaining the empty read")
	}
}

func TestDescribeUnitReadFailureIsBounded(t *testing.T) {
	long := strings.Repeat("x", 500)
	got := describeUnitReadFailure(long)
	if len(got) > 210 {
		t.Errorf("hint not truncated: %d chars", len(got))
	}
	if got := describeUnitReadFailure(""); got == "" {
		t.Error("empty stderr should still produce an explanation")
	}
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	cases := map[string]string{
		"plain":      `'plain'`,
		"with space": `'with space'`,
		"it's":       `'it'\''s'`,
		"$(reboot)":  `'$(reboot)'`,
		"`reboot`":   "'`reboot`'",
		"a;b":        `'a;b'`,
	}
	for input, want := range cases {
		if got := shellQuote(input); got != want {
			t.Errorf("shellQuote(%q) = %s, want %s", input, got, want)
		}
	}
}

// normalizeState exists so the UI never renders a blank badge.
func TestNormalizeState(t *testing.T) {
	if got := normalizeState("   "); got != "unknown" {
		t.Errorf("blank state = %q, want unknown", got)
	}
	if got := normalizeState(" active "); got != "active" {
		t.Errorf("padded state = %q, want active", got)
	}
}

// Guard the assumption that readServiceStates rejects unsafe unit names before
// they reach the shell: an admin-editable whitelist is the input.
func TestSafeUnitNameRejectsShellMetacharacters(t *testing.T) {
	unsafe := []string{"a;reboot", "$(reboot)", "a b", "a|b", "a`b`", "a&&b", ""}
	for _, name := range unsafe {
		if safeUnitName.MatchString(name) {
			t.Errorf("safeUnitName accepted unsafe name %q", name)
		}
	}
	safe := []string{"hysteria-server", "hysteria-server.service", "nginx", "wg-quick@wg0", "a_b.c-d"}
	for _, name := range safe {
		if !safeUnitName.MatchString(name) {
			t.Errorf("safeUnitName rejected valid name %q", name)
		}
	}
}

// Compile-time guard: the production path still uses sshclient.Run, so ensure the
// helper signature it depends on has not drifted.
var _ = sshclient.CleanCommandOutput
