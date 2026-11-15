// Package store persists fleet state in SQLite.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/nasi-lemak/armada/internal/api"

	_ "modernc.org/sqlite" // pure-Go SQLite driver
)

// ErrNotFound is returned when a requested record does not exist.
var ErrNotFound = errors.New("not found")

// ErrConflict is returned when a unique constraint would be violated.
var ErrConflict = errors.New("already exists")

// Store wraps the SQLite database.
type Store struct {
	db        *sql.DB
	migration MigrationInfo
	devs      deviceCache
}

const schema = `
CREATE TABLE IF NOT EXISTS devices (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	key_hash TEXT NOT NULL,
	channel TEXT NOT NULL DEFAULT 'stable',
	labels TEXT NOT NULL DEFAULT '{}',
	info TEXT NOT NULL DEFAULT '{}',
	metrics TEXT NOT NULL DEFAULT '{}',
	packages TEXT NOT NULL DEFAULT '{}',
	agent_status TEXT NOT NULL DEFAULT '',
	enrolled_at INTEGER NOT NULL,
	last_seen_at INTEGER NOT NULL
);
CREATE UNIQUE INDEX IF NOT EXISTS devices_key_hash ON devices(key_hash);

CREATE TABLE IF NOT EXISTS enrollment_tokens (
	id TEXT PRIMARY KEY,
	token_hash TEXT NOT NULL UNIQUE,
	description TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT 'stable',
	labels TEXT NOT NULL DEFAULT '{}',
	max_uses INTEGER NOT NULL DEFAULT 0,
	uses INTEGER NOT NULL DEFAULT 0,
	expires_at INTEGER,
	revoked INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS releases (
	id TEXT PRIMARY KEY,
	package TEXT NOT NULL,
	version TEXT NOT NULL,
	channel TEXT NOT NULL DEFAULT 'stable',
	os TEXT NOT NULL DEFAULT '',
	arch TEXT NOT NULL DEFAULT '',
	sha256 TEXT NOT NULL,
	size INTEGER NOT NULL,
	signature TEXT NOT NULL,
	notes TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	UNIQUE(package, version, os, arch)
);

CREATE TABLE IF NOT EXISTS deployments (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	release_id TEXT NOT NULL REFERENCES releases(id),
	package TEXT NOT NULL,
	version TEXT NOT NULL,
	selector TEXT NOT NULL,
	strategy TEXT NOT NULL,
	status TEXT NOT NULL,
	status_reason TEXT NOT NULL DEFAULT '',
	stage_index INTEGER NOT NULL DEFAULT 0,
	stage_started_at INTEGER NOT NULL,
	acknowledged_failures INTEGER NOT NULL DEFAULT 0,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS deployments_status ON deployments(status);

CREATE TABLE IF NOT EXISTS device_updates (
	deployment_id TEXT NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
	device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	release_id TEXT NOT NULL,
	package TEXT NOT NULL,
	version TEXT NOT NULL,
	from_version TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL,
	message TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL,
	PRIMARY KEY (deployment_id, device_id)
);
CREATE INDEX IF NOT EXISTS device_updates_device ON device_updates(device_id);

CREATE TABLE IF NOT EXISTS actions (
	id TEXT PRIMARY KEY,
	device_id TEXT NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	type TEXT NOT NULL,
	args TEXT NOT NULL DEFAULT '[]',
	status TEXT NOT NULL,
	exit_code INTEGER NOT NULL DEFAULT 0,
	output TEXT NOT NULL DEFAULT '',
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS actions_device_status ON actions(device_id, status);

CREATE TABLE IF NOT EXISTS apps (
	package TEXT PRIMARY KEY,
	name TEXT NOT NULL DEFAULT '',
	signers TEXT NOT NULL DEFAULT '[]',
	licensing INTEGER NOT NULL DEFAULT 0,
	license_ttl_seconds INTEGER NOT NULL DEFAULT 3600,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS monitors (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	selector TEXT NOT NULL DEFAULT '{}',
	spec TEXT NOT NULL,
	enabled INTEGER NOT NULL DEFAULT 1,
	created_at INTEGER NOT NULL,
	updated_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS channels (
	id TEXT PRIMARY KEY,
	spec TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS alert_rules (
	id TEXT PRIMARY KEY,
	spec TEXT NOT NULL,
	created_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS alerts (
	id TEXT PRIMARY KEY,
	rule_id TEXT NOT NULL,
	key TEXT NOT NULL,
	status TEXT NOT NULL,
	spec TEXT NOT NULL,
	started_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS alerts_status ON alerts(status, key);

CREATE TABLE IF NOT EXISTS events (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	time INTEGER NOT NULL,
	type TEXT NOT NULL,
	device_id TEXT NOT NULL DEFAULT '',
	deployment_id TEXT NOT NULL DEFAULT '',
	message TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS events_device ON events(device_id, id);
CREATE INDEX IF NOT EXISTS events_deployment ON events(deployment_id, id);
`

// Open opens (and migrates) the database at path. Use ":memory:" for tests.
func Open(path string) (*Store, error) {
	dsn := "file:" + path + "?_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)"
	if path == ":memory:" {
		dsn = "file::memory:?_pragma=foreign_keys(1)"
	}
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	// SQLite allows one writer at a time; a single connection avoids SQLITE_BUSY
	// churn and keeps :memory: databases shared.
	db.SetMaxOpenConns(1)
	st := &Store{db: db}
	migPath := path
	if path == ":memory:" {
		migPath = ""
	}
	if st.migration, err = migrate(context.Background(), db, migPath); err != nil {
		db.Close()
		return nil, err
	}
	return st, nil
}

// Migration reports what Open did to the database schema.
func (s *Store) Migration() MigrationInfo { return s.migration }

// addColumns upgrades databases created by earlier versions in place.

// Close closes the database.
func (s *Store) Close() error { return s.db.Close() }

// ---- helpers ----

func ts(t time.Time) int64 { return t.UnixMilli() }

func fromTS(ms int64) time.Time {
	if ms == 0 {
		return time.Time{}
	}
	return time.UnixMilli(ms).UTC()
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

type scanner interface{ Scan(dest ...any) error }

// ---- devices ----

const deviceCols = `id, name, channel, labels, info, metrics, packages, agent_status, enrolled_at, last_seen_at, attestation, license_revoked, health, parent_id, lifecycle, lifecycle_info, inventory`

func scanDevice(sc scanner) (*api.Device, error) {
	var d api.Device
	var labels, info, metrics, packages, attestation, health, lifeInfo, inventory string
	var enrolled, seen int64
	var revoked int
	if err := sc.Scan(&d.ID, &d.Name, &d.Channel, &labels, &info, &metrics, &packages, &d.AgentStatus, &enrolled, &seen, &attestation, &revoked, &health, &d.ParentID, &d.Lifecycle, &lifeInfo, &inventory); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(labels), &d.Labels)
	_ = json.Unmarshal([]byte(info), &d.Info)
	_ = json.Unmarshal([]byte(metrics), &d.Metrics)
	_ = json.Unmarshal([]byte(packages), &d.Packages)
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	if d.Packages == nil {
		d.Packages = map[string]string{}
	}
	d.EnrolledAt, d.LastSeenAt = fromTS(enrolled), fromTS(seen)
	if attestation != "" {
		d.Attestation = &api.DeviceAttestation{}
		_ = json.Unmarshal([]byte(attestation), d.Attestation)
	}
	d.LicenseRevoked = revoked != 0
	_ = json.Unmarshal([]byte(health), &d.Checks)
	d.Health = api.WorstHealth(d.Checks)
	if d.Lifecycle == "" {
		d.Lifecycle = api.LifecycleActive
	}
	if lifeInfo != "" {
		d.LifecycleInfo = &api.LifecycleInfo{}
		_ = json.Unmarshal([]byte(lifeInfo), d.LifecycleInfo)
	}
	if inventory != "" {
		d.Inventory = &api.Inventory{}
		_ = json.Unmarshal([]byte(inventory), d.Inventory)
	}
	return &d, nil
}

// SetDeviceHealth stores a device's latest check results.
func (s *Store) SetDeviceHealth(ctx context.Context, id string, checks []api.CheckResult) error {
	if checks == nil {
		checks = []api.CheckResult{}
	}
	health := mustJSON(checks)
	return s.writeDevice(id, func() error {
		_, err := s.db.ExecContext(ctx, `UPDATE devices SET health=? WHERE id=?`, health, id)
		return err
	}, func(d *api.Device) { applyHealth(d, health) })
}

// DeviceHardwareKey returns the device's attested public key (base64 PKIX DER), if any.
func (s *Store) DeviceHardwareKey(ctx context.Context, id string) (string, error) {
	var k string
	err := s.db.QueryRowContext(ctx, `SELECT hw_key FROM devices WHERE id=?`, id).Scan(&k)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return k, err
}

// SetLicenseRevoked revokes or reinstates a device's app licenses.
func (s *Store) SetLicenseRevoked(ctx context.Context, id string, revoked bool) error {
	return s.writeDevice(id, func() error {
		res, err := s.db.ExecContext(ctx, `UPDATE devices SET license_revoked=? WHERE id=?`, boolInt(revoked), id)
		if err != nil {
			return err
		}
		return expectOne(res)
	}, func(d *api.Device) { d.LicenseRevoked = revoked })
}

// GetDevice loads a device by ID.
func (s *Store) GetDevice(ctx context.Context, id string) (*api.Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE id = ?`, id))
}

// GetDeviceByKeyHash authenticates a device.
func (s *Store) GetDeviceByKeyHash(ctx context.Context, keyHash string) (*api.Device, error) {
	return scanDevice(s.db.QueryRowContext(ctx, `SELECT `+deviceCols+` FROM devices WHERE key_hash = ?`, keyHash))
}

// ListDevices returns the fleet's devices ordered by name: every device but
// retired ones, which are kept only as records.
func (s *Store) ListDevices(ctx context.Context) ([]*api.Device, error) {
	return s.listCached(ctx, func(d *api.Device) bool { return d.Lifecycle != api.LifecycleRetired })
}

// ListAllDevices returns every device, retired ones included.
func (s *Store) ListAllDevices(ctx context.Context) ([]*api.Device, error) {
	return s.listCached(ctx, func(*api.Device) bool { return true })
}

// SetDeviceLifecycle stores devices' identity (name, channel, labels) and
// lifecycle in one transaction. Retiring a device destroys its key (it can
// never authenticate again), forgets its hardware key and revokes its
// licenses.
func (s *Store) SetDeviceLifecycle(ctx context.Context, devs ...*api.Device) error {
	defer s.devs.invalidate()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, d := range devs {
		info := ""
		if d.LifecycleInfo != nil {
			info = mustJSON(d.LifecycleInfo)
		}
		res, err := tx.ExecContext(ctx, `UPDATE devices SET name=?, channel=?, labels=?, lifecycle=?, lifecycle_info=? WHERE id=?`,
			d.Name, d.Channel, mustJSON(d.Labels), d.Lifecycle, info, d.ID)
		if err != nil {
			return err
		}
		if err := expectOne(res); err != nil {
			return err
		}
		if d.Lifecycle == api.LifecycleRetired {
			d.LicenseRevoked = true
			if _, err := tx.ExecContext(ctx, `UPDATE devices SET key_hash=?, hw_key='', license_revoked=1 WHERE id=?`, "retired:"+d.ID, d.ID); err != nil {
				return err
			}
		}
	}
	return tx.Commit()
}

// ListPeripherals returns the devices a gateway reports (not retired).
func (s *Store) ListPeripherals(ctx context.Context, gatewayID string) ([]*api.Device, error) {
	return s.listCached(ctx, func(d *api.Device) bool { return d.ParentID == gatewayID && d.Lifecycle != api.LifecycleRetired })
}

func (s *Store) listDevices(ctx context.Context, q string, args ...any) ([]*api.Device, error) {
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Device
	for rows.Next() {
		d, err := scanDevice(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

// RecordHeartbeat stores what a device reported.
func (s *Store) RecordHeartbeat(ctx context.Context, id string, info api.DeviceInfo, m api.Metrics, pkgs map[string]string, status string, now time.Time) error {
	infoJ, metricsJ, pkgsJ, seen := mustJSON(info), mustJSON(m), mustJSON(pkgs), ts(now)
	return s.writeDevice(id, func() error {
		res, err := s.db.ExecContext(ctx, `UPDATE devices SET info=?, metrics=?, packages=?, agent_status=?, last_seen_at=? WHERE id=?`,
			infoJ, metricsJ, pkgsJ, status, seen, id)
		if err != nil {
			return err
		}
		return expectOne(res)
	}, func(d *api.Device) { applyHeartbeat(d, infoJ, metricsJ, pkgsJ, status, seen) })
}

// SetDeviceInventory stores what people record about a device.
func (s *Store) SetDeviceInventory(ctx context.Context, id string, inv *api.Inventory) error {
	invJ := inventoryJSON(inv)
	return s.writeDevice(id, func() error {
		res, err := s.db.ExecContext(ctx, `UPDATE devices SET inventory=? WHERE id=?`, invJ, id)
		if err != nil {
			return err
		}
		return expectOne(res)
	}, func(d *api.Device) {
		d.Inventory = nil
		if invJ != "" {
			d.Inventory = &api.Inventory{}
			_ = json.Unmarshal([]byte(invJ), d.Inventory)
		}
	})
}

func inventoryJSON(inv *api.Inventory) string {
	if inv.Empty() {
		return ""
	}
	return mustJSON(inv)
}

// UpdateDeviceMeta changes admin-controlled fields.
func (s *Store) UpdateDeviceMeta(ctx context.Context, id, name, channel string, labels map[string]string) error {
	defer s.devs.invalidate() // the name decides the order
	res, err := s.db.ExecContext(ctx, `UPDATE devices SET name=?, channel=?, labels=? WHERE id=?`, name, channel, mustJSON(labels), id)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// DeleteDevice removes a device and its history. Its key stops working immediately.
func (s *Store) DeleteDevice(ctx context.Context, id string) error {
	defer s.devs.invalidate()
	res, err := s.db.ExecContext(ctx, `DELETE FROM devices WHERE id=?`, id)
	if err != nil {
		return err
	}
	return expectOne(res)
}

func expectOne(res sql.Result) error {
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- enrollment tokens ----

const tokenCols = `id, description, channel, labels, max_uses, uses, expires_at, revoked, created_at, lifecycle, client_scope`

func scanToken(sc scanner) (*api.EnrollmentToken, error) {
	var t api.EnrollmentToken
	var labels string
	var expires sql.NullInt64
	var revoked int
	var created int64
	var clientScope string
	if err := sc.Scan(&t.ID, &t.Description, &t.Channel, &labels, &t.MaxUses, &t.Uses, &expires, &revoked, &created, &t.Lifecycle, &clientScope); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(labels), &t.Labels)
	if clientScope != "" {
		_ = json.Unmarshal([]byte(clientScope), &t.ClientScope)
	}
	if expires.Valid {
		e := fromTS(expires.Int64)
		t.ExpiresAt = &e
	}
	t.Revoked = revoked != 0
	t.CreatedAt = fromTS(created)
	return &t, nil
}

// CreateToken stores an enrollment token (hashed).
func (s *Store) CreateToken(ctx context.Context, t *api.EnrollmentToken, tokenHash string) error {
	var expires any
	if t.ExpiresAt != nil {
		expires = ts(*t.ExpiresAt)
	}
	if t.Lifecycle == "" {
		t.Lifecycle = api.LifecycleActive
	}
	clientScope := ""
	if t.ClientScope.Channel != "" || len(t.ClientScope.Labels) > 0 {
		clientScope = mustJSON(t.ClientScope)
	}
	_, err := s.db.ExecContext(ctx, `INSERT INTO enrollment_tokens (`+tokenCols+`, token_hash) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		t.ID, t.Description, t.Channel, mustJSON(t.Labels), t.MaxUses, t.Uses, expires, boolInt(t.Revoked), ts(t.CreatedAt), t.Lifecycle, clientScope, tokenHash)
	return err
}

// ListTokens returns all enrollment tokens.
func (s *Store) ListTokens(ctx context.Context) ([]*api.EnrollmentToken, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+tokenCols+` FROM enrollment_tokens ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.EnrollmentToken
	for rows.Next() {
		t, err := scanToken(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// RevokeToken disables an enrollment token.
func (s *Store) RevokeToken(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `UPDATE enrollment_tokens SET revoked=1 WHERE id=?`, id)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// ErrTokenInvalid is returned when an enrollment token cannot be used.
var ErrTokenInvalid = errors.New("enrollment token is invalid, expired, revoked or used up")

// Enroll atomically consumes an enrollment token and creates the device.
// The device's channel and labels default to the token's.
// hwKey is the base64 PKIX public key of an attested hardware key, or "".
// PutScheduledAction creates or replaces a scheduled action.
func (s *Store) PutScheduledAction(ctx context.Context, a *api.ScheduledAction) error {
	return s.putDoc(ctx, "scheduled_actions", a.ID, a.CreatedAt, a)
}

// GetScheduledAction loads a scheduled action.
func (s *Store) GetScheduledAction(ctx context.Context, id string) (*api.ScheduledAction, error) {
	var a api.ScheduledAction
	if err := s.getDoc(ctx, "scheduled_actions", id, &a); err != nil {
		return nil, err
	}
	return &a, nil
}

// UpdateScheduledAction changes a scheduled action in one transaction, from
// its current state: a run that took a while doesn't undo an edit, pause
// or deletion made meanwhile. ErrNotFound if it's gone.
func (s *Store) UpdateScheduledAction(ctx context.Context, id string, fn func(*api.ScheduledAction)) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var spec string
	err = tx.QueryRowContext(ctx, `SELECT spec FROM scheduled_actions WHERE id = ?`, id).Scan(&spec)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var a api.ScheduledAction
	if err := json.Unmarshal([]byte(spec), &a); err != nil {
		return err
	}
	fn(&a)
	if _, err := tx.ExecContext(ctx, `UPDATE scheduled_actions SET spec = ? WHERE id = ?`, mustJSON(&a), id); err != nil {
		return err
	}
	return tx.Commit()
}

// ListScheduledActions returns every scheduled action, oldest first.
func (s *Store) ListScheduledActions(ctx context.Context) ([]*api.ScheduledAction, error) {
	return listDocs[api.ScheduledAction](ctx, s, "scheduled_actions")
}

// DeleteScheduledAction removes a scheduled action.
func (s *Store) DeleteScheduledAction(ctx context.Context, id string) error {
	return s.deleteDoc(ctx, "scheduled_actions", id)
}

// RegistrationID is how a serial number is stored and looked up.
func RegistrationID(serial string) string { return strings.ToUpper(strings.TrimSpace(serial)) }

// PutRegistration creates or replaces a pre-registration.
func (s *Store) PutRegistration(ctx context.Context, r *api.Registration) error {
	return s.putDoc(ctx, "registrations", RegistrationID(r.Serial), r.CreatedAt, r)
}

// GetRegistration returns the pre-registration for a serial number.
func (s *Store) GetRegistration(ctx context.Context, serial string) (*api.Registration, error) {
	var r api.Registration
	if err := s.getDoc(ctx, "registrations", RegistrationID(serial), &r); err != nil {
		return nil, err
	}
	return &r, nil
}

// ListRegistrations returns every pre-registration, oldest first.
func (s *Store) ListRegistrations(ctx context.Context) ([]*api.Registration, error) {
	return listDocs[api.Registration](ctx, s, "registrations")
}

// DeleteRegistration removes a pre-registration.
func (s *Store) DeleteRegistration(ctx context.Context, serial string) error {
	return s.deleteDoc(ctx, "registrations", RegistrationID(serial))
}

// EnrollOptions adjusts an enrollment.
type EnrollOptions struct {
	// ApplyRegistration, if set, is offered the unclaimed pre-registration
	// matching the device's serial (after the token's channel and labels
	// are applied). If it returns true the registration is claimed, in the
	// same transaction, so each is used once.
	ApplyRegistration func(*api.Registration, *api.Device, *api.EnrollmentToken) bool
}

func (s *Store) Enroll(ctx context.Context, tokenHash string, d *api.Device, keyHash, hwKey string, now time.Time, opts ...EnrollOptions) error {
	defer s.devs.invalidate()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	t, err := scanToken(tx.QueryRowContext(ctx, `SELECT `+tokenCols+` FROM enrollment_tokens WHERE token_hash=?`, tokenHash))
	if errors.Is(err, ErrNotFound) {
		return ErrTokenInvalid
	} else if err != nil {
		return err
	}
	if t.Revoked || (t.MaxUses > 0 && t.Uses >= t.MaxUses) || (t.ExpiresAt != nil && now.After(*t.ExpiresAt)) {
		return ErrTokenInvalid
	}
	if _, err := tx.ExecContext(ctx, `UPDATE enrollment_tokens SET uses = uses + 1 WHERE id=?`, t.ID); err != nil {
		return err
	}
	d.Channel = t.Channel
	if d.Labels == nil {
		d.Labels = map[string]string{}
	}
	for k, v := range t.Labels {
		d.Labels[k] = v
	}
	attestation := ""
	if d.Attestation != nil {
		attestation = mustJSON(d.Attestation)
	}
	// The token decides whether the device goes into service or into stock.
	d.Lifecycle, d.LifecycleInfo = api.LifecycleActive, nil
	lifeInfo := ""
	if t.Lifecycle == api.LifecycleSpare {
		by := "enrollment token " + t.ID
		if t.Description != "" {
			by += " (" + t.Description + ")"
		}
		d.Lifecycle = api.LifecycleSpare
		d.LifecycleInfo = &api.LifecycleInfo{Note: "enrolled as a spare", ChangedAt: now, ChangedBy: by}
		lifeInfo = mustJSON(d.LifecycleInfo)
	}
	for _, o := range opts {
		if o.ApplyRegistration == nil || RegistrationID(d.Info.Serial) == "" {
			continue
		}
		var spec string
		err := tx.QueryRowContext(ctx, `SELECT spec FROM registrations WHERE id=?`, RegistrationID(d.Info.Serial)).Scan(&spec)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		} else if err != nil {
			return err
		}
		var reg api.Registration
		if err := json.Unmarshal([]byte(spec), &reg); err != nil {
			return err
		}
		if reg.ClaimedBy != "" || !o.ApplyRegistration(&reg, d, t) {
			continue
		}
		reg.ClaimedBy, reg.ClaimedAt = d.ID, &now
		if _, err := tx.ExecContext(ctx, `UPDATE registrations SET spec=? WHERE id=?`, mustJSON(&reg), RegistrationID(reg.Serial)); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx, `INSERT INTO devices (`+deviceCols+`, key_hash, hw_key) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.Name, d.Channel, mustJSON(d.Labels), mustJSON(d.Info), mustJSON(d.Metrics), mustJSON(d.Packages),
		d.AgentStatus, ts(d.EnrolledAt), ts(d.LastSeenAt), attestation, boolInt(d.LicenseRevoked), "[]", d.ParentID, d.Lifecycle, lifeInfo, inventoryJSON(d.Inventory), keyHash, hwKey); err != nil {
		return err
	}
	return tx.Commit()
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- releases ----

const releaseCols = `id, package, version, channel, os, arch, sha256, size, signature, notes, created_at, version_code`

func scanRelease(sc scanner) (*api.Release, error) {
	var r api.Release
	var created int64
	if err := sc.Scan(&r.ID, &r.Package, &r.Version, &r.Channel, &r.OS, &r.Arch, &r.SHA256, &r.Size, &r.Signature, &r.Notes, &created, &r.VersionCode); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	r.CreatedAt = fromTS(created)
	return &r, nil
}

// CreateRelease stores release metadata.
func (s *Store) CreateRelease(ctx context.Context, r *api.Release) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO releases (`+releaseCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.Package, r.Version, r.Channel, r.OS, r.Arch, r.SHA256, r.Size, r.Signature, r.Notes, ts(r.CreatedAt), r.VersionCode)
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// GetRelease loads a release by ID.
func (s *Store) GetRelease(ctx context.Context, id string) (*api.Release, error) {
	return scanRelease(s.db.QueryRowContext(ctx, `SELECT `+releaseCols+` FROM releases WHERE id=?`, id))
}

// ListReleases returns releases, newest first, optionally filtered by package.
func (s *Store) ListReleases(ctx context.Context, pkg string) ([]*api.Release, error) {
	q := `SELECT ` + releaseCols + ` FROM releases`
	var args []any
	if pkg != "" {
		q += ` WHERE package=?`
		args = append(args, pkg)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Release
	for rows.Next() {
		r, err := scanRelease(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ErrInUse is returned when deleting a release referenced by a deployment.
var ErrInUse = errors.New("release is referenced by a deployment")

// DeleteRelease removes a release that no deployment references.
func (s *Store) DeleteRelease(ctx context.Context, id string) error {
	var n int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM deployments WHERE release_id=?`, id).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return ErrInUse
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM releases WHERE id=?`, id)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// ---- deployments ----

const deploymentCols = `id, name, release_id, package, version, selector, strategy, status, status_reason, stage_index, stage_started_at, acknowledged_failures, created_at, updated_at, created_by, approved_by, approved_at`

func scanDeployment(sc scanner) (*api.Deployment, error) {
	var d api.Deployment
	var sel, strat string
	var stageStarted, created, updated, approved int64
	if err := sc.Scan(&d.ID, &d.Name, &d.ReleaseID, &d.Package, &d.Version, &sel, &strat, &d.Status, &d.StatusReason,
		&d.StageIndex, &stageStarted, &d.AcknowledgedFailures, &created, &updated, &d.CreatedBy, &d.ApprovedBy, &approved); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(sel), &d.Selector)
	_ = json.Unmarshal([]byte(strat), &d.Strategy)
	d.StageStartedAt, d.CreatedAt, d.UpdatedAt = fromTS(stageStarted), fromTS(created), fromTS(updated)
	if approved > 0 {
		t := fromTS(approved)
		d.ApprovedAt = &t
	}
	return &d, nil
}

// CreateDeployment stores a new deployment.
func (s *Store) CreateDeployment(ctx context.Context, d *api.Deployment) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO deployments (`+deploymentCols+`) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.Name, d.ReleaseID, d.Package, d.Version, mustJSON(d.Selector), mustJSON(d.Strategy), d.Status, d.StatusReason,
		d.StageIndex, ts(d.StageStartedAt), d.AcknowledgedFailures, ts(d.CreatedAt), ts(d.UpdatedAt), d.CreatedBy, d.ApprovedBy, approvedTS(d))
	return err
}

// GetDeployment loads a deployment with stats.
func (s *Store) GetDeployment(ctx context.Context, id string) (*api.Deployment, error) {
	d, err := scanDeployment(s.db.QueryRowContext(ctx, `SELECT `+deploymentCols+` FROM deployments WHERE id=?`, id))
	if err != nil {
		return nil, err
	}
	stats, err := s.deploymentStats(ctx, d.ID)
	if err != nil {
		return nil, err
	}
	d.Stats = stats[d.ID]
	return d, nil
}

// ListDeployments returns deployments with progress stats, newest first,
// optionally only those in the given statuses.
func (s *Store) ListDeployments(ctx context.Context, statuses ...string) ([]*api.Deployment, error) {
	out, err := s.listDeployments(ctx, statuses)
	if err != nil {
		return nil, err
	}
	stats, err := s.deploymentStats(ctx, "")
	if err != nil {
		return nil, err
	}
	for _, d := range out {
		d.Stats = stats[d.ID]
	}
	return out, nil
}

// ActiveDeployments returns running and paused deployments, newest first,
// without stats. It is cheap enough to call on every heartbeat.
func (s *Store) ActiveDeployments(ctx context.Context) ([]*api.Deployment, error) {
	return s.listDeployments(ctx, []string{api.DeploymentRunning, api.DeploymentPaused})
}

func (s *Store) listDeployments(ctx context.Context, statuses []string) ([]*api.Deployment, error) {
	q := `SELECT ` + deploymentCols + ` FROM deployments`
	var args []any
	if len(statuses) > 0 {
		q += ` WHERE status IN (` + strings.TrimSuffix(strings.Repeat("?,", len(statuses)), ",") + `)`
		for _, st := range statuses {
			args = append(args, st)
		}
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY created_at DESC, id DESC`, args...)
	if err != nil {
		return nil, err
	}
	var out []*api.Deployment
	for rows.Next() {
		d, err := scanDeployment(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, d)
	}
	rows.Close()
	return out, rows.Err()
}

func (s *Store) deploymentStats(ctx context.Context, deploymentID string) (map[string]api.DeploymentStats, error) {
	q := `SELECT deployment_id, status, COUNT(*) FROM device_updates`
	var args []any
	if deploymentID != "" {
		q += ` WHERE deployment_id=?`
		args = append(args, deploymentID)
	}
	rows, err := s.db.QueryContext(ctx, q+` GROUP BY deployment_id, status`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]api.DeploymentStats{}
	for rows.Next() {
		var id, status string
		var n int
		if err := rows.Scan(&id, &status, &n); err != nil {
			return nil, err
		}
		st := out[id]
		switch status {
		case api.UpdatePending:
			st.Pending += n
		case api.UpdateDownloading, api.UpdateInstalling:
			st.InProgress += n
		case api.UpdateSucceeded:
			st.Succeeded += n
		case api.UpdateFailed:
			st.Failed += n
		case api.UpdateRolledBack:
			st.RolledBack += n
		}
		st.Total += n
		if done := st.Succeeded + st.Failed + st.RolledBack; done > 0 {
			st.FailureRate = float64(st.Failed+st.RolledBack) / float64(done)
		}
		out[id] = st
	}
	return out, rows.Err()
}

func approvedTS(d *api.Deployment) int64 {
	if d.ApprovedAt == nil {
		return 0
	}
	return ts(*d.ApprovedAt)
}

// UpdateDeploymentState changes a deployment's status and stage.
func (s *Store) UpdateDeploymentState(ctx context.Context, d *api.Deployment) error {
	res, err := s.db.ExecContext(ctx, `UPDATE deployments SET status=?, status_reason=?, stage_index=?, stage_started_at=?, acknowledged_failures=?, updated_at=?, approved_by=?, approved_at=? WHERE id=?`,
		d.Status, d.StatusReason, d.StageIndex, ts(d.StageStartedAt), d.AcknowledgedFailures, ts(d.UpdatedAt), d.ApprovedBy, approvedTS(d), d.ID)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// ---- device updates ----

const deviceUpdateCols = `deployment_id, device_id, release_id, package, version, from_version, status, message, created_at, updated_at`

func scanDeviceUpdate(sc scanner) (*api.DeviceUpdate, error) {
	var u api.DeviceUpdate
	var created, updated int64
	if err := sc.Scan(&u.DeploymentID, &u.DeviceID, &u.ReleaseID, &u.Package, &u.Version, &u.FromVersion, &u.Status, &u.Message, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt, u.UpdatedAt = fromTS(created), fromTS(updated)
	return &u, nil
}

// UpsertDeviceUpdate records a device's progress in a deployment.
func (s *Store) UpsertDeviceUpdate(ctx context.Context, u *api.DeviceUpdate) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO device_updates (`+deviceUpdateCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(deployment_id, device_id) DO UPDATE SET status=excluded.status, message=excluded.message, updated_at=excluded.updated_at`,
		u.DeploymentID, u.DeviceID, u.ReleaseID, u.Package, u.Version, u.FromVersion, u.Status, u.Message, ts(u.CreatedAt), ts(u.UpdatedAt))
	return err
}

// GetDeviceUpdate loads one device's progress in a deployment.
func (s *Store) GetDeviceUpdate(ctx context.Context, deploymentID, deviceID string) (*api.DeviceUpdate, error) {
	return scanDeviceUpdate(s.db.QueryRowContext(ctx, `SELECT `+deviceUpdateCols+` FROM device_updates WHERE deployment_id=? AND device_id=?`, deploymentID, deviceID))
}

// ListDeviceUpdates returns update records filtered by deployment and/or device.
func (s *Store) ListDeviceUpdates(ctx context.Context, deploymentID, deviceID string) ([]*api.DeviceUpdate, error) {
	q := `SELECT ` + deviceUpdateCols + ` FROM device_updates WHERE 1=1`
	var args []any
	if deploymentID != "" {
		q += ` AND deployment_id=?`
		args = append(args, deploymentID)
	}
	if deviceID != "" {
		q += ` AND device_id=?`
		args = append(args, deviceID)
	}
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY updated_at DESC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.DeviceUpdate
	for rows.Next() {
		u, err := scanDeviceUpdate(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ResetFailedUpdates forgets failed and rolled-back attempts in a deployment so
// those devices are offered the update again. It returns how many were reset.
func (s *Store) ResetFailedUpdates(ctx context.Context, deploymentID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM device_updates WHERE deployment_id=? AND status IN (?,?)`,
		deploymentID, api.UpdateFailed, api.UpdateRolledBack)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	return int(n), err
}

// FailStaleUpdates marks updates stuck downloading or installing since cutoff
// as failed, and returns them. Pending (offered but unacknowledged) updates are
// left alone: they are simply offered again when the device next checks in.
func (s *Store) FailStaleUpdates(ctx context.Context, cutoff, now time.Time) ([]*api.DeviceUpdate, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+deviceUpdateCols+` FROM device_updates WHERE status IN (?,?) AND updated_at < ?`,
		api.UpdateDownloading, api.UpdateInstalling, ts(cutoff))
	if err != nil {
		return nil, err
	}
	var stale []*api.DeviceUpdate
	for rows.Next() {
		u, err := scanDeviceUpdate(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		stale = append(stale, u)
	}
	rows.Close()
	for _, u := range stale {
		u.Status, u.Message, u.UpdatedAt = api.UpdateFailed, "timed out after status "+u.Status, now
		if err := s.UpsertDeviceUpdate(ctx, u); err != nil {
			return nil, err
		}
	}
	return stale, nil
}

// ---- actions ----

const actionCols = `id, device_id, type, args, status, exit_code, output, created_at, updated_at, batch_id`

func scanAction(sc scanner) (*api.Action, error) {
	var a api.Action
	var args string
	var created, updated int64
	if err := sc.Scan(&a.ID, &a.DeviceID, &a.Type, &args, &a.Status, &a.ExitCode, &a.Output, &created, &updated, &a.BatchID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(args), &a.Args)
	a.CreatedAt, a.UpdatedAt = fromTS(created), fromTS(updated)
	return &a, nil
}

// CreateAction queues an action.
func (s *Store) CreateAction(ctx context.Context, a *api.Action) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO actions (`+actionCols+`) VALUES (?,?,?,?,?,?,?,?,?,?)`,
		a.ID, a.DeviceID, a.Type, mustJSON(a.Args), a.Status, a.ExitCode, a.Output, ts(a.CreatedAt), ts(a.UpdatedAt), a.BatchID)
	return err
}

// BatchActionStatus maps action ID to status for a bulk operation's actions.
func (s *Store) BatchActionStatus(ctx context.Context, batchID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id, status FROM actions WHERE batch_id=?`, batchID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, err
		}
		out[id] = status
	}
	return out, rows.Err()
}

// PutBulkOperation records a bulk operation.
func (s *Store) PutBulkOperation(ctx context.Context, b *api.BulkOperation) error {
	return s.putDoc(ctx, "bulk_operations", b.ID, b.CreatedAt, b)
}

// GetBulkOperation loads a bulk operation.
func (s *Store) GetBulkOperation(ctx context.Context, id string) (*api.BulkOperation, error) {
	var b api.BulkOperation
	return &b, s.getDoc(ctx, "bulk_operations", id, &b)
}

// ListBulkOperations returns the most recent bulk operations, newest first.
func (s *Store) ListBulkOperations(ctx context.Context, limit int) ([]*api.BulkOperation, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec FROM bulk_operations ORDER BY created_at DESC, rowid DESC LIMIT ?`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.BulkOperation
	for rows.Next() {
		var spec string
		if err := rows.Scan(&spec); err != nil {
			return nil, err
		}
		var b api.BulkOperation
		if err := json.Unmarshal([]byte(spec), &b); err != nil {
			return nil, err
		}
		out = append(out, &b)
	}
	return out, rows.Err()
}

// PruneBulkOperations forgets bulk operations older than before.
func (s *Store) PruneBulkOperations(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM bulk_operations WHERE created_at < ?`, ts(before))
	return err
}

// GetAction loads an action.
func (s *Store) GetAction(ctx context.Context, id string) (*api.Action, error) {
	return scanAction(s.db.QueryRowContext(ctx, `SELECT `+actionCols+` FROM actions WHERE id=?`, id))
}

// ListActions returns a device's actions, newest first.
func (s *Store) ListActions(ctx context.Context, deviceID string, limit int) ([]*api.Action, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+actionCols+` FROM actions WHERE device_id=? ORDER BY created_at DESC LIMIT ?`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Action
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ClaimQueuedActions returns a device's queued actions and marks them sent.
func (s *Store) ClaimQueuedActions(ctx context.Context, deviceID string, now time.Time) ([]api.Action, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+actionCols+` FROM actions WHERE device_id=? AND status=? ORDER BY created_at`, deviceID, api.ActionQueued)
	if err != nil {
		return nil, err
	}
	var out []api.Action
	for rows.Next() {
		a, err := scanAction(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		a.Status = api.ActionSent
		out = append(out, *a)
	}
	rows.Close()
	for _, a := range out {
		if _, err := tx.ExecContext(ctx, `UPDATE actions SET status=?, updated_at=? WHERE id=?`, api.ActionSent, ts(now), a.ID); err != nil {
			return nil, err
		}
	}
	return out, tx.Commit()
}

// CompleteAction stores the agent's result for an action.
func (s *Store) CompleteAction(ctx context.Context, id, deviceID string, r api.ActionResult, now time.Time) error {
	res, err := s.db.ExecContext(ctx, `UPDATE actions SET status=?, exit_code=?, output=?, updated_at=? WHERE id=? AND device_id=?`,
		r.Status, r.ExitCode, r.Output, ts(now), id, deviceID)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// ---- events ----

// AddEvent appends to the activity log.
func (s *Store) AddEvent(ctx context.Context, e api.Event) error {
	return s.RecordEvent(ctx, &e)
}

// RecordEvent adds an activity-log entry and sets its ID (and time, if unset).
func (s *Store) RecordEvent(ctx context.Context, e *api.Event) error {
	if e.Time.IsZero() {
		e.Time = time.Now()
	}
	res, err := s.db.ExecContext(ctx, `INSERT INTO events (time, type, device_id, deployment_id, message, actor) VALUES (?,?,?,?,?,?)`,
		ts(e.Time), e.Type, e.DeviceID, e.DeploymentID, e.Message, e.Actor)
	if err != nil {
		return err
	}
	e.ID, err = res.LastInsertId()
	return err
}

// PutSite creates or replaces a site (another server reporting here).
func (s *Store) PutSite(ctx context.Context, site *api.Site) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO sites (id, spec, created_at, token_hash) VALUES (?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET spec=excluded.spec, token_hash=excluded.token_hash`,
		site.ID, mustJSON(site), ts(site.CreatedAt), site.TokenHash)
	return err
}

// SiteIDByTokenHash finds which site a token belongs to, without loading
// any site's snapshot.
func (s *Store) SiteIDByTokenHash(ctx context.Context, tokenHash string) (string, error) {
	var id string
	err := s.db.QueryRowContext(ctx, `SELECT id FROM sites WHERE token_hash = ? AND token_hash <> ''`, tokenHash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", ErrNotFound
	}
	return id, err
}

// UpdateSite changes a site in one transaction: fn sees its current state
// (so a slow report can't undo a token rotation or bring back a deleted
// site). With tokenHash set, the site must still have that token.
func (s *Store) UpdateSite(ctx context.Context, id, tokenHash string, fn func(*api.Site) error) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	var spec, cur string
	err = tx.QueryRowContext(ctx, `SELECT spec, token_hash FROM sites WHERE id = ?`, id).Scan(&spec, &cur)
	if errors.Is(err, sql.ErrNoRows) || (err == nil && tokenHash != "" && cur != tokenHash) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	var site api.Site
	if err := json.Unmarshal([]byte(spec), &site); err != nil {
		return err
	}
	if err := fn(&site); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE sites SET spec = ?, token_hash = ? WHERE id = ?`, mustJSON(&site), site.TokenHash, id); err != nil {
		return err
	}
	return tx.Commit()
}

// GetSite loads a site.
func (s *Store) GetSite(ctx context.Context, id string) (*api.Site, error) {
	var site api.Site
	if err := s.getDoc(ctx, "sites", id, &site); err != nil {
		return nil, err
	}
	return &site, nil
}

// ListSites returns every site, oldest first.
func (s *Store) ListSites(ctx context.Context) ([]*api.Site, error) {
	return listDocs[api.Site](ctx, s, "sites")
}

// DeleteSite removes a site.
func (s *Store) DeleteSite(ctx context.Context, id string) error {
	return s.deleteDoc(ctx, "sites", id)
}

// PutEventHook creates or replaces an event webhook.
func (s *Store) PutEventHook(ctx context.Context, h *api.EventHook) error {
	return s.putDoc(ctx, "event_hooks", h.ID, h.CreatedAt, h)
}

// GetEventHook loads an event webhook.
func (s *Store) GetEventHook(ctx context.Context, id string) (*api.EventHook, error) {
	var h api.EventHook
	if err := s.getDoc(ctx, "event_hooks", id, &h); err != nil {
		return nil, err
	}
	return &h, nil
}

// ListEventHooks returns every event webhook, oldest first.
func (s *Store) ListEventHooks(ctx context.Context) ([]*api.EventHook, error) {
	return listDocs[api.EventHook](ctx, s, "event_hooks")
}

// DeleteEventHook removes an event webhook.
func (s *Store) DeleteEventHook(ctx context.Context, id string) error {
	return s.deleteDoc(ctx, "event_hooks", id)
}

// EventFilter narrows an activity-log query. Empty fields don't filter.
type EventFilter struct {
	DeviceID     string
	DeploymentID string
	// DeviceIDs, when non-nil, limits results to events about these devices
	// (a client account's view). An empty non-nil slice matches nothing.
	DeviceIDs []string
	// Types are exact event types, or prefixes ending in "*" ("alert.*").
	// ExcludeTypes, in the same form, drops events.
	Types, ExcludeTypes []string
	// Actor is a user's email or token name; "system" matches events with no actor.
	Actor string
	// Text matches anywhere in the message (case-insensitive for ASCII).
	Text         string
	Since, Until time.Time // [Since, Until)
	BeforeID     int64     // paging: only events older than this ID
	ByTime       bool      // order by time, then ID (default: ID), newest first
}

// likeEscape escapes LIKE wildcards; use with ESCAPE '\'.
func likeEscape(v string) string {
	return strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(v)
}

// ListEvents returns recent events, optionally filtered by device or rollout.
func (s *Store) ListEvents(ctx context.Context, deviceID, deploymentID string, limit int) ([]*api.Event, error) {
	return s.QueryEvents(ctx, EventFilter{DeviceID: deviceID, DeploymentID: deploymentID}, limit)
}

// QueryEvents returns events matching f, newest first.
func (s *Store) QueryEvents(ctx context.Context, f EventFilter, limit int) ([]*api.Event, error) {
	if f.DeviceIDs != nil && len(f.DeviceIDs) == 0 {
		return nil, nil
	}
	q := `SELECT id, time, type, device_id, deployment_id, message, actor FROM events WHERE 1=1`
	var args []any
	if f.DeviceID != "" {
		q += ` AND device_id=?`
		args = append(args, f.DeviceID)
	}
	if f.DeploymentID != "" {
		q += ` AND deployment_id=?`
		args = append(args, f.DeploymentID)
	}
	if f.DeviceIDs != nil {
		q += ` AND device_id IN (` + strings.TrimSuffix(strings.Repeat("?,", len(f.DeviceIDs)), ",") + `)`
		for _, id := range f.DeviceIDs {
			args = append(args, id)
		}
	}
	if len(f.Types) > 0 {
		var ors []string
		for _, t := range f.Types {
			if p, ok := strings.CutSuffix(t, "*"); ok {
				ors = append(ors, `type LIKE ? ESCAPE '\'`)
				args = append(args, likeEscape(p)+"%")
			} else {
				ors = append(ors, `type=?`)
				args = append(args, t)
			}
		}
		q += ` AND (` + strings.Join(ors, " OR ") + `)`
	}
	for _, t := range f.ExcludeTypes {
		if p, ok := strings.CutSuffix(t, "*"); ok {
			q += ` AND type NOT LIKE ? ESCAPE '\'`
			args = append(args, likeEscape(p)+"%")
		} else {
			q += ` AND type<>?`
			args = append(args, t)
		}
	}
	switch {
	case f.Actor == "system":
		q += ` AND actor=''`
	case f.Actor != "":
		q += ` AND actor=?`
		args = append(args, f.Actor)
	}
	if f.Text != "" {
		q += ` AND message LIKE ? ESCAPE '\'`
		args = append(args, "%"+likeEscape(f.Text)+"%")
	}
	if !f.Since.IsZero() {
		q += ` AND time>=?`
		args = append(args, ts(f.Since))
	}
	if !f.Until.IsZero() {
		q += ` AND time<?`
		args = append(args, ts(f.Until))
	}
	if f.BeforeID > 0 {
		q += ` AND id<?`
		args = append(args, f.BeforeID)
	}
	args = append(args, limit)
	order := ` ORDER BY id DESC LIMIT ?`
	if f.ByTime {
		order = ` ORDER BY time DESC, id DESC LIMIT ?`
	}
	rows, err := s.db.QueryContext(ctx, q+order, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Event
	for rows.Next() {
		var e api.Event
		var t int64
		if err := rows.Scan(&e.ID, &t, &e.Type, &e.DeviceID, &e.DeploymentID, &e.Message, &e.Actor); err != nil {
			return nil, err
		}
		e.Time = fromTS(t)
		out = append(out, &e)
	}
	return out, rows.Err()
}

// EventTypes lists the event types recorded, for filter menus.
func (s *Store) EventTypes(ctx context.Context) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT type FROM events ORDER BY type`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var t string
		if err := rows.Scan(&t); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// PruneEvents keeps the activity log bounded.
func (s *Store) PruneEvents(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM events WHERE time < ?`, ts(before))
	return err
}

// ---- apps ----

const appCols = `package, name, signers, licensing, license_ttl_seconds, created_at, updated_at`

func scanApp(sc scanner) (*api.App, error) {
	var a api.App
	var signers string
	var licensing int
	var created, updated int64
	if err := sc.Scan(&a.Package, &a.Name, &signers, &licensing, &a.LicenseTTLSeconds, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(signers), &a.Signers)
	if a.Signers == nil {
		a.Signers = []string{}
	}
	a.Licensing = licensing != 0
	a.CreatedAt, a.UpdatedAt = fromTS(created), fromTS(updated)
	return &a, nil
}

// UpsertApp creates or replaces an app registration.
func (s *Store) UpsertApp(ctx context.Context, a *api.App) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO apps (`+appCols+`) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(package) DO UPDATE SET name=excluded.name, signers=excluded.signers, licensing=excluded.licensing,
		license_ttl_seconds=excluded.license_ttl_seconds, updated_at=excluded.updated_at`,
		a.Package, a.Name, mustJSON(a.Signers), boolInt(a.Licensing), a.LicenseTTLSeconds, ts(a.CreatedAt), ts(a.UpdatedAt))
	return err
}

// GetApp loads an app registration.
func (s *Store) GetApp(ctx context.Context, pkg string) (*api.App, error) {
	return scanApp(s.db.QueryRowContext(ctx, `SELECT `+appCols+` FROM apps WHERE package=?`, pkg))
}

// ListApps returns all registered apps.
func (s *Store) ListApps(ctx context.Context) ([]*api.App, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+appCols+` FROM apps ORDER BY package`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.App
	for rows.Next() {
		a, err := scanApp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeleteApp removes an app registration.
func (s *Store) DeleteApp(ctx context.Context, pkg string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM apps WHERE package=?`, pkg)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// ---- monitors ----

const monitorCols = `id, name, selector, spec, enabled, created_at, updated_at`

func scanMonitor(sc scanner) (*api.Monitor, error) {
	var m api.Monitor
	var sel, spec string
	var enabled int
	var created, updated int64
	if err := sc.Scan(&m.ID, &m.Name, &sel, &spec, &enabled, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(sel), &m.Selector)
	_ = json.Unmarshal([]byte(spec), &m.Check)
	m.Enabled = enabled != 0
	m.CreatedAt, m.UpdatedAt = fromTS(created), fromTS(updated)
	return &m, nil
}

// UpsertMonitor creates or replaces a monitor.
func (s *Store) UpsertMonitor(ctx context.Context, m *api.Monitor) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO monitors (`+monitorCols+`) VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET name=excluded.name, selector=excluded.selector, spec=excluded.spec,
		enabled=excluded.enabled, updated_at=excluded.updated_at`,
		m.ID, m.Name, mustJSON(m.Selector), mustJSON(m.Check), boolInt(m.Enabled), ts(m.CreatedAt), ts(m.UpdatedAt))
	if isUnique(err) {
		return ErrConflict
	}
	return err
}

// GetMonitor loads a monitor.
func (s *Store) GetMonitor(ctx context.Context, id string) (*api.Monitor, error) {
	return scanMonitor(s.db.QueryRowContext(ctx, `SELECT `+monitorCols+` FROM monitors WHERE id=?`, id))
}

// ListMonitors returns all monitors by name.
func (s *Store) ListMonitors(ctx context.Context) ([]*api.Monitor, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT `+monitorCols+` FROM monitors ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Monitor
	for rows.Next() {
		m, err := scanMonitor(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// DeleteMonitor removes a monitor.
func (s *Store) DeleteMonitor(ctx context.Context, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM monitors WHERE id=?`, id)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// ---- notification channels, alert rules and alerts (stored as JSON documents) ----

func (s *Store) putDoc(ctx context.Context, table, id string, created time.Time, v any) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO `+table+` (id, spec, created_at) VALUES (?,?,?)
		ON CONFLICT(id) DO UPDATE SET spec=excluded.spec`, id, mustJSON(v), ts(created))
	return err
}

func (s *Store) getDoc(ctx context.Context, table, id string, v any) error {
	var spec string
	err := s.db.QueryRowContext(ctx, `SELECT spec FROM `+table+` WHERE id=?`, id).Scan(&spec)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	} else if err != nil {
		return err
	}
	return json.Unmarshal([]byte(spec), v)
}

func listDocs[T any](ctx context.Context, s *Store, table string) ([]*T, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT spec FROM `+table+` ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*T
	for rows.Next() {
		var spec string
		if err := rows.Scan(&spec); err != nil {
			return nil, err
		}
		v := new(T)
		if err := json.Unmarshal([]byte(spec), v); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

func (s *Store) deleteDoc(ctx context.Context, table, id string) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM `+table+` WHERE id=?`, id)
	if err != nil {
		return err
	}
	return expectOne(res)
}

// PutMaintenance creates or replaces a maintenance window.
func (s *Store) PutMaintenance(ctx context.Context, m *api.Maintenance) error {
	m.Active, m.State = false, "" // computed on read
	return s.putDoc(ctx, "maintenance", m.ID, m.CreatedAt, m)
}

// GetMaintenance loads a maintenance window.
func (s *Store) GetMaintenance(ctx context.Context, id string) (*api.Maintenance, error) {
	var m api.Maintenance
	return &m, s.getDoc(ctx, "maintenance", id, &m)
}

// ListMaintenance returns all maintenance windows, oldest first.
func (s *Store) ListMaintenance(ctx context.Context) ([]*api.Maintenance, error) {
	return listDocs[api.Maintenance](ctx, s, "maintenance")
}

// DeleteMaintenance removes a maintenance window.
func (s *Store) DeleteMaintenance(ctx context.Context, id string) error {
	return s.deleteDoc(ctx, "maintenance", id)
}

// GetAlert loads one alert.
func (s *Store) GetAlert(ctx context.Context, id string) (*api.Alert, error) {
	var spec string
	err := s.db.QueryRowContext(ctx, `SELECT spec FROM alerts WHERE id=?`, id).Scan(&spec)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	} else if err != nil {
		return nil, err
	}
	var a api.Alert
	return &a, json.Unmarshal([]byte(spec), &a)
}

// PutChannel creates or replaces a notification channel.
func (s *Store) PutChannel(ctx context.Context, c *api.NotificationChannel) error {
	return s.putDoc(ctx, "channels", c.ID, c.CreatedAt, c)
}

// GetChannel loads a notification channel.
func (s *Store) GetChannel(ctx context.Context, id string) (*api.NotificationChannel, error) {
	var c api.NotificationChannel
	return &c, s.getDoc(ctx, "channels", id, &c)
}

// ListChannels returns all notification channels.
func (s *Store) ListChannels(ctx context.Context) ([]*api.NotificationChannel, error) {
	return listDocs[api.NotificationChannel](ctx, s, "channels")
}

// DeleteChannel removes a notification channel.
func (s *Store) DeleteChannel(ctx context.Context, id string) error {
	return s.deleteDoc(ctx, "channels", id)
}

// PutAlertRule creates or replaces an alert rule.
func (s *Store) PutAlertRule(ctx context.Context, r *api.AlertRule) error {
	return s.putDoc(ctx, "alert_rules", r.ID, r.CreatedAt, r)
}

// GetAlertRule loads an alert rule.
func (s *Store) GetAlertRule(ctx context.Context, id string) (*api.AlertRule, error) {
	var r api.AlertRule
	return &r, s.getDoc(ctx, "alert_rules", id, &r)
}

// ListAlertRules returns all alert rules.
func (s *Store) ListAlertRules(ctx context.Context) ([]*api.AlertRule, error) {
	return listDocs[api.AlertRule](ctx, s, "alert_rules")
}

// DeleteAlertRule removes an alert rule.
func (s *Store) DeleteAlertRule(ctx context.Context, id string) error {
	return s.deleteDoc(ctx, "alert_rules", id)
}

// PutAlert creates or updates an alert.
func (s *Store) PutAlert(ctx context.Context, a *api.Alert) error {
	_, err := s.db.ExecContext(ctx, `INSERT INTO alerts (id, rule_id, key, status, spec, started_at) VALUES (?,?,?,?,?,?)
		ON CONFLICT(id) DO UPDATE SET status=excluded.status, spec=excluded.spec`,
		a.ID, a.RuleID, a.Key, a.Status, mustJSON(a), ts(a.StartedAt))
	return err
}

// ListAlerts returns firing alerts, or the most recent alerts of any status.
func (s *Store) ListAlerts(ctx context.Context, firingOnly bool, limit int) ([]*api.Alert, error) {
	q := `SELECT spec FROM alerts`
	var args []any
	if firingOnly {
		q += ` WHERE status=?`
		args = append(args, api.AlertFiring)
	}
	args = append(args, limit)
	rows, err := s.db.QueryContext(ctx, q+` ORDER BY started_at DESC LIMIT ?`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*api.Alert
	for rows.Next() {
		var spec string
		if err := rows.Scan(&spec); err != nil {
			return nil, err
		}
		var a api.Alert
		if err := json.Unmarshal([]byte(spec), &a); err != nil {
			return nil, err
		}
		out = append(out, &a)
	}
	return out, rows.Err()
}

// PruneAlerts deletes resolved alerts older than before.
func (s *Store) PruneAlerts(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM alerts WHERE status<>? AND started_at < ?`, api.AlertFiring, ts(before))
	return err
}

// UpsertPeripheral creates or refreshes a peripheral reported by a gateway.
// New peripherals take the gateway's channel and labels; afterwards those
// are managed like any device's.
func (s *Store) UpsertPeripheral(ctx context.Context, d *api.Device) (created bool, err error) {
	infoJ, metricsJ, pkgsJ, seen, health := mustJSON(d.Info), mustJSON(d.Metrics), mustJSON(d.Packages), ts(d.LastSeenAt), mustJSON(nonNilChecks(d.Checks))
	updated := false
	err = s.writeDevice(d.ID, func() error {
		res, err := s.db.ExecContext(ctx, `UPDATE devices SET info=?, metrics=?, packages=?, last_seen_at=?, parent_id=?, health=? WHERE id=?`,
			infoJ, metricsJ, pkgsJ, seen, d.ParentID, health, d.ID)
		if err != nil {
			return err
		}
		n, _ := res.RowsAffected()
		updated = n > 0
		return nil
	}, func(c *api.Device) {
		if updated {
			applyHeartbeat(c, infoJ, metricsJ, pkgsJ, c.AgentStatus, seen)
			c.ParentID = d.ParentID
			applyHealth(c, health)
		}
	})
	if err != nil || updated {
		return false, err
	}
	defer s.devs.invalidate()
	_, err = s.db.ExecContext(ctx, `INSERT INTO devices (`+deviceCols+`, key_hash, hw_key) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		d.ID, d.Name, d.Channel, mustJSON(d.Labels), mustJSON(d.Info), mustJSON(d.Metrics), mustJSON(d.Packages),
		"", ts(d.EnrolledAt), ts(d.LastSeenAt), "", 0, mustJSON(nonNilChecks(d.Checks)), d.ParentID, api.LifecycleActive, "", "",
		// Peripherals have no credentials; this placeholder can never match a key hash.
		"peripheral:"+d.ID, "")
	return err == nil, err
}

func nonNilChecks(c []api.CheckResult) []api.CheckResult {
	if c == nil {
		return []api.CheckResult{}
	}
	return c
}

// ReleaseOfferedToGateway reports whether a release was offered to the
// gateway itself or to one of its peripherals.
func (s *Store) ReleaseOfferedToGateway(ctx context.Context, releaseID, gatewayID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM device_updates u JOIN devices d ON d.id = u.device_id
		WHERE u.release_id=? AND (d.id=? OR d.parent_id=?)`, releaseID, gatewayID, gatewayID).Scan(&n)
	return n > 0, err
}
