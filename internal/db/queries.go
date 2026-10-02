package db

import (
	"database/sql"
	"errors"
	"sync"
	"time"
)

// AdminUserRow is one account. Role is "admin" (may change everything) or
// "user" (read-only).
type AdminUserRow struct {
	ID           int64
	Username     string
	PasswordHash string
	Role         string
	Enabled      bool
	CreatedAt    int64
	UpdatedAt    int64
	LastLoginAt  *int64
}

// AdminSessionRow is a login session, stored hashed.
type AdminSessionRow struct {
	TokenHash   string
	AdminUserID int64
	CreatedAt   int64
	ExpiresAt   int64
}

const adminUserColumns = `id, username, password_hash, role, enabled, created_at, updated_at, last_login_at`

// scanAdminUser reads one admin_users row in adminUserColumns order.
func scanAdminUser(row interface{ Scan(...any) error }) (*AdminUserRow, error) {
	var (
		user    AdminUserRow
		enabled int
		last    sql.NullInt64
	)
	if err := row.Scan(&user.ID, &user.Username, &user.PasswordHash, &user.Role, &enabled, &user.CreatedAt, &user.UpdatedAt, &last); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	user.Enabled = enabled != 0
	if user.Role == "" {
		user.Role = "admin" // column predates roles
	}
	if last.Valid {
		user.LastLoginAt = &last.Int64
	}
	return &user, nil
}

// GetAdminUserByName looks an account up by username.
func GetAdminUserByName(username string) (*AdminUserRow, error) {
	row := Get().QueryRow(`SELECT `+adminUserColumns+` FROM admin_users WHERE username = ?`, username)
	return scanAdminUser(row)
}

// GetAdminUser looks an account up by id.
func GetAdminUser(id int64) (*AdminUserRow, error) {
	row := Get().QueryRow(`SELECT `+adminUserColumns+` FROM admin_users WHERE id = ?`, id)
	return scanAdminUser(row)
}

// ListAdminUsers returns every account, ordered by id.
func ListAdminUsers() ([]AdminUserRow, error) {
	rows, err := Get().Query(`SELECT ` + adminUserColumns + ` FROM admin_users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []AdminUserRow
	for rows.Next() {
		user, err := scanAdminUser(rows)
		if err != nil {
			return nil, err
		}
		users = append(users, *user)
	}
	return users, rows.Err()
}

// CountEnabledAdmins counts enabled full admins. Read-only accounts do not
// count: they cannot manage anything.
func CountEnabledAdmins() (int, error) {
	var count int
	err := Get().QueryRow(`SELECT COUNT(*) FROM admin_users WHERE enabled = 1 AND role = 'admin'`).Scan(&count)
	return count, err
}

// CreateAdminUser inserts a new account (enabled by default).
func CreateAdminUser(username, passwordHash, role string) (*AdminUserRow, error) {
	now := time.Now().UnixMilli()
	result, err := Get().Exec(
		`INSERT INTO admin_users (username, password_hash, role, enabled, created_at, updated_at)
		 VALUES (?, ?, ?, 1, ?, ?)`,
		username, passwordHash, role, now, now,
	)
	if err != nil {
		return nil, err
	}
	id, err := result.LastInsertId()
	if err != nil {
		return nil, err
	}
	return GetAdminUser(id)
}

// UpdateAdminPassword replaces an account's password hash.
func UpdateAdminPassword(id int64, passwordHash string) error {
	_, err := Get().Exec(`UPDATE admin_users SET password_hash = ?, updated_at = ? WHERE id = ?`, passwordHash, time.Now().UnixMilli(), id)
	return err
}

// UpdateAdminRole changes an account's role.
func UpdateAdminRole(id int64, role string) error {
	_, err := Get().Exec(`UPDATE admin_users SET role = ?, updated_at = ? WHERE id = ?`, role, time.Now().UnixMilli(), id)
	return err
}

// UpdateAdminEnabled enables or disables an account.
func UpdateAdminEnabled(id int64, enabled bool) error {
	value := 0
	if enabled {
		value = 1
	}
	_, err := Get().Exec(`UPDATE admin_users SET enabled = ?, updated_at = ? WHERE id = ?`, value, time.Now().UnixMilli(), id)
	return err
}

// DeleteAdminUser removes an account and its sessions. Reports whether a row
// was removed.
func DeleteAdminUser(id int64) (bool, error) {
	// Sessions are cleaned up first so a deleted user cannot keep access.
	if _, err := Get().Exec(`DELETE FROM admin_sessions WHERE admin_user_id = ?`, id); err != nil {
		return false, err
	}
	result, err := Get().Exec(`DELETE FROM admin_users WHERE id = ?`, id)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// EnsureAdminUser creates the account only when it is missing; an existing
// password is never overwritten.
func EnsureAdminUser(username, passwordHash string) error {
	now := time.Now().UnixMilli()
	_, err := Get().Exec(
		`INSERT INTO admin_users (username, password_hash, enabled, created_at, updated_at)
		 SELECT ?, ?, 1, ?, ?
		 WHERE NOT EXISTS (SELECT 1 FROM admin_users WHERE username = ?)`,
		username, passwordHash, now, now, username,
	)
	return err
}

// UpdateAdminLastLogin records a successful login.
func UpdateAdminLastLogin(id int64) error {
	_, err := Get().Exec(`UPDATE admin_users SET last_login_at = ? WHERE id = ?`, time.Now().UnixMilli(), id)
	return err
}

// Audit

// RecordAudit appends an audit-log entry.
func RecordAudit(action, target, sourceIP string) error {
	_, err := Get().Exec(
		`INSERT INTO audit_logs (action, target, source_ip, created_at) VALUES (?, ?, ?, ?)`,
		action, target, sourceIP, time.Now().UnixMilli(),
	)
	return err
}

// Admin sessions

// GetAdminSession loads a session by its token hash.
func GetAdminSession(tokenHash string) (*AdminSessionRow, error) {
	var session AdminSessionRow
	err := Get().
		QueryRow(`SELECT token_hash, admin_user_id, created_at, expires_at FROM admin_sessions WHERE token_hash = ?`, tokenHash).
		Scan(&session.TokenHash, &session.AdminUserID, &session.CreatedAt, &session.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &session, nil
}

// CreateAdminSession stores a new session expiring after ttlMs.
func CreateAdminSession(tokenHash string, adminUserID int64, ttlMs int64) error {
	now := time.Now().UnixMilli()
	_, err := Get().Exec(
		`INSERT INTO admin_sessions (token_hash, admin_user_id, created_at, expires_at) VALUES (?, ?, ?, ?)`,
		tokenHash, adminUserID, now, now+ttlMs,
	)
	return err
}

// DeleteAdminSession revokes one session. Reports whether a row was removed.
func DeleteAdminSession(tokenHash string) (bool, error) {
	result, err := Get().Exec(`DELETE FROM admin_sessions WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	return affected > 0, err
}

// DeleteAdminSessionsForUser revokes every session of an account (used after a
// password change or disable).
func DeleteAdminSessionsForUser(adminUserID int64) error {
	_, err := Get().Exec(`DELETE FROM admin_sessions WHERE admin_user_id = ?`, adminUserID)
	return err
}

// sessionPurgeInterval throttles the expired-session sweep so it costs at most
// one DELETE every five minutes, not one per admin request.
const sessionPurgeInterval = 5 * time.Minute

var (
	sessionPurgeMu   sync.Mutex
	lastSessionPurge int64
)

// PurgeExpiredAdminSessions deletes expired sessions; expired sessions are
// already rejected by the auth check, so this is pure garbage collection.
func PurgeExpiredAdminSessions() {
	now := time.Now().UnixMilli()
	sessionPurgeMu.Lock()
	if now-lastSessionPurge < sessionPurgeInterval.Milliseconds() {
		sessionPurgeMu.Unlock()
		return
	}
	lastSessionPurge = now
	sessionPurgeMu.Unlock()
	_, _ = Get().Exec(`DELETE FROM admin_sessions WHERE expires_at < ?`, now)
}
