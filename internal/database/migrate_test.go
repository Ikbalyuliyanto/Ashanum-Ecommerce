package database

import (
	"testing"
	"testing/fstest"
)

func TestLoadMigrationsEmbedded(t *testing.T) {
	ms, err := LoadMigrations()
	if err != nil {
		t.Fatal(err)
	}
	if len(ms) == 0 || ms[0].Version != "0001" {
		t.Fatalf("expected embedded migrations starting at 0001, got %+v", ms)
	}
}

func TestLoadMigrationsOrderAndValidation(t *testing.T) {
	fsys := fstest.MapFS{
		"m/0002_b.sql": {Data: []byte("SELECT 2;")},
		"m/0001_a.sql": {Data: []byte("SELECT 1;")},
	}
	ms, err := loadMigrations(fsys, "m")
	if err != nil {
		t.Fatal(err)
	}
	if ms[0].File != "0001_a.sql" || ms[1].File != "0002_b.sql" {
		t.Errorf("wrong order: %s, %s", ms[0].File, ms[1].File)
	}
	if ms[0].Checksum == ms[1].Checksum {
		t.Error("different files must have different checksums")
	}

	for name, bad := range map[string]fstest.MapFS{
		"bad name":  {"m/init.sql": {Data: []byte("")}},
		"duplicate": {"m/0001_a.sql": {}, "m/0001_b.sql": {}},
	} {
		if _, err := loadMigrations(bad, "m"); err == nil {
			t.Errorf("%s: expected error", name)
		}
	}
}
