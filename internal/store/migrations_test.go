package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// columns describes every table's columns, for comparing schemas.
func columns(t *testing.T, db *sql.DB) map[string]string {
	t.Helper()
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	var tables []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		tables = append(tables, n)
	}
	rows.Close()
	out := map[string]string{}
	for _, tb := range tables {
		cr, err := db.Query(`SELECT name, type, "notnull", COALESCE(dflt_value,''), pk FROM pragma_table_info(?)`, tb)
		if err != nil {
			t.Fatal(err)
		}
		var cols []string
		for cr.Next() {
			var n, typ, def string
			var nn, pk int
			cr.Scan(&n, &typ, &nn, &def, &pk)
			cols = append(cols, fmt.Sprintf("%s %s nn=%d def=%s pk=%d", n, typ, nn, def, pk))
		}
		cr.Close()
		sort.Strings(cols)
		out[tb] = strings.Join(cols, "; ")
	}
	return out
}

func indexes(t *testing.T, db *sql.DB) []string {
	rows, err := db.Query(`SELECT name FROM sqlite_master WHERE type='index' AND name NOT LIKE 'sqlite_%' ORDER BY name`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		rows.Scan(&n)
		out = append(out, n)
	}
	return out
}

func TestFreshDatabaseIsAtLatestVersion(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "armada.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	m := st.Migration()
	if m.From != 0 || m.To != LatestSchemaVersion() || len(m.Applied) != len(migrations) || m.Backup != "" {
		t.Fatalf("fresh migration: %+v", m)
	}
	var n int
	st.DB().QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n)
	if n != len(migrations) {
		t.Fatalf("%d versions recorded", n)
	}
}

// A database written by the very first release (no schema_migrations
// table, many columns missing) upgrades to exactly the fresh schema, keeps
// its data, and is backed up first.
func TestLegacyDatabaseUpgrades(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "armada.db")
	copyFile(t, "testdata/legacy-7d5e2a3.db", path)
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	m := st.Migration()
	if m.From != 0 || m.To != LatestSchemaVersion() || m.Backup == "" {
		t.Fatalf("legacy migration: %+v", m)
	}
	if _, err := os.Stat(m.Backup); err != nil {
		t.Fatalf("no backup: %v", err)
	}
	devs, err := st.ListDevices(context.Background())
	if err != nil || len(devs) != 1 || devs[0].Name != "legacy-kiosk" || devs[0].Labels["customer"] != "acme" || devs[0].Packages["myapp"] != "1.0.0" {
		t.Fatalf("legacy device: %v %+v", err, devs)
	}
	deps, err := st.ListDeployments(context.Background())
	if err != nil || len(deps) != 1 || deps[0].Version != "1.1.0" || deps[0].CreatedBy != "" {
		t.Fatalf("legacy deployment: %v %+v", err, deps)
	}
	fresh, err := Open(filepath.Join(dir, "fresh.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer fresh.Close()
	got, want := columns(t, st.DB()), columns(t, fresh.DB())
	for tb, cols := range want {
		if got[tb] != cols {
			t.Errorf("table %s differs after upgrade:\n got  %s\n want %s", tb, got[tb], cols)
		}
	}
	if len(got) != len(want) {
		t.Errorf("tables: %d after upgrade, %d fresh", len(got), len(want))
	}
	if g, w := strings.Join(indexes(t, st.DB()), ","), strings.Join(indexes(t, fresh.DB()), ","); g != w {
		t.Errorf("indexes differ:\n got  %s\n want %s", g, w)
	}
	st.Close()
	// Opening again changes nothing and makes no new backup.
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if m := st.Migration(); len(m.Applied) != 0 || m.Backup != "" {
		t.Fatalf("second open: %+v", m)
	}
	st.Close()
}

func TestNewerDatabaseIsRefused(t *testing.T) {
	path := filepath.Join(t.TempDir(), "armada.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.DB().Exec(`INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, 'from the future', 0)`, LatestSchemaVersion()+1)
	st.Close()
	if _, err := Open(path); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("opened a newer database: %v", err)
	}
	cur, pending, err := SchemaStatus(path)
	if err != nil || cur != LatestSchemaVersion()+1 || len(pending) != 0 {
		t.Fatalf("status: %d %v %v", cur, pending, err)
	}
}

func TestFailedMigrationLeavesNothingBehind(t *testing.T) {
	path := filepath.Join(t.TempDir(), "armada.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	saved := migrations
	defer func() { migrations = saved }()
	migrations = append(append([]migration{}, saved...), migration{Version: LatestSchemaVersion() + 1, Name: "broken",
		SQL: `CREATE TABLE half_done (id TEXT); ALTER TABLE devices ADD COLUMN extra TEXT; THIS IS NOT SQL;`})
	if cur, pending, _ := SchemaStatus(path); cur != len(saved) || len(pending) != 1 {
		t.Fatalf("status before: %d %v", cur, pending)
	}
	_, err = Open(path)
	if err == nil || !strings.Contains(err.Error(), "unchanged at version") || !strings.Contains(err.Error(), "before-v") {
		t.Fatalf("broken migration: %v", err)
	}
	migrations = saved
	st, err = Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	var n int
	st.DB().QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name='half_done'`).Scan(&n)
	st.DB().QueryRow(`SELECT COUNT(*) FROM pragma_table_info('devices') WHERE name='extra'`).Scan(&n)
	if n != 0 {
		t.Fatal("a failed migration left changes behind")
	}
}

// Released migrations must never change: databases in the field already
// ran them. If this fails, add a new migration instead of editing one.
// Only update the list below when ADDING a migration.
func TestMigrationsAreFrozen(t *testing.T) {
	want := []string{
		"1 baseline 6d0348289fe5",
		"2 index events by time and sessions by expiry 5f0ea4a45395",
		"3 maintenance windows b1359bbc004d",
		"4 device lifecycle 962fc48fe336",
		"5 bulk operations 96628d342efc",
		"6 report schedules 9ef124e2bf93",
		"7 enrollment tokens choose lifecycle 798424001601",
		"8 device certificates 929cea2a3748",
	}
	var got []string
	for i, m := range migrations {
		if m.Version != i+1 {
			t.Fatalf("migration versions must be 1, 2, 3…; found %d at position %d", m.Version, i)
		}
		h := sha256.New()
		io.WriteString(h, m.Name+"\x00"+m.SQL)
		if m.Version == 1 { // the baseline's content is its schemas and columns
			for _, s := range baselineSchemas {
				io.WriteString(h, s)
			}
			for _, c := range baselineColumns {
				io.WriteString(h, c.Table+c.Name+c.Def)
			}
		}
		got = append(got, fmt.Sprintf("%d %s %s", m.Version, m.Name, hex.EncodeToString(h.Sum(nil))[:12]))
	}
	for i := range want {
		if i >= len(got) || got[i] != want[i] {
			t.Errorf("released migration changed:\n got  %v\n want %v", got, want)
			return
		}
	}
	if len(got) > len(want) {
		t.Logf("new migrations (add them to the list above): %v", got[len(want):])
	}
}

func copyFile(t *testing.T, src, dst string) {
	b, err := os.ReadFile(src)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o600); err != nil {
		t.Fatal(err)
	}
}
