package store

import (
	"strings"
	"testing"
	"testing/fstest"
)

func TestParseMigrationName(t *testing.T) {
	cases := []struct {
		file        string
		wantVersion int
		wantName    string
		wantOK      bool
	}{
		{"001_init.sql", 1, "init", true},
		{"500_project_tables.sql", 500, "project_tables", true},
		{"007_guard_state.sql", 7, "guard_state", true},
		{"12_two_digits.sql", 12, "two_digits", true},
		{"init.sql", 0, "", false},
		{"_leading.sql", 0, "", false},
		{"abc_notanumber.sql", 0, "", false},
		{"000_zero.sql", 0, "", false},
	}
	for _, c := range cases {
		v, n, ok := parseMigrationName(c.file)
		if ok != c.wantOK || v != c.wantVersion || n != c.wantName {
			t.Errorf("parseMigrationName(%q) = (%d, %q, %v), want (%d, %q, %v)",
				c.file, v, n, ok, c.wantVersion, c.wantName, c.wantOK)
		}
	}
}

func TestLoadMigrationsSortsByVersion(t *testing.T) {
	fsys := fstest.MapFS{
		"m/010_ten.sql":  {Data: []byte("SELECT 10;")},
		"m/002_two.sql":  {Data: []byte("SELECT 2;")},
		"m/001_one.sql":  {Data: []byte("SELECT 1;")},
		"m/notes.txt":    {Data: []byte("ignored")},
		"m/nonumber.sql": {Data: []byte("ignored")},
	}
	ms, err := LoadMigrations(fsys, "m")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(ms) != 3 {
		t.Fatalf("got %d migrations, want 3 (non-numbered files must be ignored, not guessed at)", len(ms))
	}
	// Lexical order would put 010 before 002; version order must not.
	want := []int{1, 2, 10}
	for i, m := range ms {
		if m.Version != want[i] {
			t.Errorf("migration %d has version %d, want %d", i, m.Version, want[i])
		}
	}
}

func TestLoadMigrationsRejectsDuplicateVersions(t *testing.T) {
	fsys := fstest.MapFS{
		"m/001_first.sql":  {Data: []byte("SELECT 1;")},
		"m/001_second.sql": {Data: []byte("SELECT 2;")},
	}
	_, err := LoadMigrations(fsys, "m")
	if err == nil {
		t.Fatal("two files claiming version 1 must be an error, not a coin toss")
	}
	if !strings.Contains(err.Error(), "twice") {
		t.Errorf("error should name the collision, got: %v", err)
	}
}

func TestEmbeddedSchemaLoads(t *testing.T) {
	ms, err := LoadMigrations(schemaFS, "schema")
	if err != nil {
		t.Fatalf("the embedded schema must load: %v", err)
	}
	if len(ms) == 0 {
		t.Fatal("no embedded migrations found; did scripts/sync-schema.sh run?")
	}
	if ms[0].Version != 1 || ms[0].Name != "init" {
		t.Errorf("first migration = %d_%s, want 1_init", ms[0].Version, ms[0].Name)
	}
	for _, m := range ms {
		if m.Version > libraryMaxVersion {
			t.Errorf("library migration %d exceeds the reserved range 1-%d", m.Version, libraryMaxVersion)
		}
		if strings.TrimSpace(m.SQL) == "" {
			t.Errorf("migration %d (%s) is empty", m.Version, m.Name)
		}
	}
}

func TestMigrateProjectRejectsReservedVersions(t *testing.T) {
	// Validation happens before any database work, so a zero DB is enough:
	// the point is that a project numbering its first migration 001 is
	// refused rather than silently colliding with the library's.
	fsys := fstest.MapFS{"m/001_project.sql": {Data: []byte("SELECT 1;")}}
	err := (&DB{}).MigrateProject(t.Context(), fsys, "m")
	if err == nil {
		t.Fatal("a project migration numbered below 500 must be refused")
	}
	if !strings.Contains(err.Error(), "500") {
		t.Errorf("the error should name the reserved boundary, got: %v", err)
	}
}

func TestInRangeOf(t *testing.T) {
	library := []Migration{{Version: 1}, {Version: 2}}
	project := []Migration{{Version: 500}, {Version: 501}}

	if !inRangeOf(3, library) {
		t.Error("version 3 is in the library band")
	}
	if inRangeOf(500, library) {
		t.Error("a project version must not be validated against the library set")
	}
	if !inRangeOf(600, project) {
		t.Error("version 600 is in the project band")
	}
	if inRangeOf(2, project) {
		t.Error("a library version must not be validated against the project set")
	}
	if inRangeOf(1, nil) {
		t.Error("an empty set owns no versions")
	}
}
