// Defense-in-depth tests for the memory_db schema/identifier interpolation
// helpers. These pin that a hostile R2R_PROJECT_NAME (or any dynamically
// interpolated schema/table name) fails closed before it can reach a
// fmt.Sprintf'd query. Pure functions — no DB needed.
package apiserver

import (
	"strings"
	"testing"
)

func TestValidateSQLIdentifier_RejectsInjection(t *testing.T) {
	bad := []string{
		"",                      // empty
		"public; DROP TABLE x",  // statement injection
		"public.users",          // dotted (schema.table breakout)
		"\"weird\"",             // embedded quotes
		"a-b",                   // dash not allowed
		"1abc",                  // leading digit
		" leading",              // leading space
		"trailing ",             // trailing space
		"with space",            // internal space
		"sch\nema",              // newline
		"emoji😀",                // non-ASCII
		strings.Repeat("a", 64), // > NAMEDATALEN-1
	}
	for _, name := range bad {
		if err := validateSQLIdentifier("schema", name); err == nil {
			t.Errorf("validateSQLIdentifier must reject %q", name)
		}
	}
}

func TestValidateSQLIdentifier_AcceptsValid(t *testing.T) {
	good := []string{
		"deepresearch",
		"_private",
		"Schema_1",
		"a",
		strings.Repeat("a", 63), // exactly NAMEDATALEN-1
	}
	for _, name := range good {
		if err := validateSQLIdentifier("schema", name); err != nil {
			t.Errorf("validateSQLIdentifier should accept %q, got: %v", name, err)
		}
	}
}

func TestDeepresearchSchema_FailsClosed(t *testing.T) {
	// Default when unset.
	t.Setenv("R2R_PROJECT_NAME", "")
	if s, err := deepresearchSchema(); err != nil || s != "deepresearch" {
		t.Errorf("default schema: got (%q,%v), want (deepresearch,nil)", s, err)
	}
	// Hostile value must error rather than be interpolated.
	t.Setenv("R2R_PROJECT_NAME", "x; DROP SCHEMA evo CASCADE; --")
	if s, err := deepresearchSchema(); err == nil {
		t.Errorf("hostile R2R_PROJECT_NAME must fail closed, got schema %q", s)
	}
	// A subtle one: a dotted name that would re-target the query.
	t.Setenv("R2R_PROJECT_NAME", "pg_catalog")
	if _, err := deepresearchSchema(); err != nil {
		t.Errorf("plain identifier pg_catalog is regex-valid; got error: %v", err)
	}
	t.Setenv("R2R_PROJECT_NAME", "evil.public")
	if _, err := deepresearchSchema(); err == nil {
		t.Error("dotted schema name must fail closed")
	}
}

func TestQuoteIdent(t *testing.T) {
	q, err := quoteIdent("schema", "deepresearch")
	if err != nil {
		t.Fatalf("quoteIdent valid: %v", err)
	}
	if q != `"deepresearch"` {
		t.Errorf("quoteIdent = %q, want \"deepresearch\"", q)
	}
	if _, err := quoteIdent("table", "bad;name"); err == nil {
		t.Error("quoteIdent must reject an invalid name")
	}
}

func TestDeepresearchQualified(t *testing.T) {
	q, err := deepresearchQualified("deepresearch", "documents_entities")
	if err != nil {
		t.Fatalf("deepresearchQualified valid: %v", err)
	}
	if q != `"deepresearch"."documents_entities"` {
		t.Errorf("deepresearchQualified = %q", q)
	}
	// Either half being hostile fails closed.
	if _, err := deepresearchQualified("deepresearch", "x; DROP"); err == nil {
		t.Error("hostile table name must fail closed")
	}
	if _, err := deepresearchQualified("bad-schema", "documents_entities"); err == nil {
		t.Error("hostile schema name must fail closed")
	}
}
