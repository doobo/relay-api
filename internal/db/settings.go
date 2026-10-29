package db

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// IPAccessSettings is the persisted pair of IP allow/deny lists. Lists are
// stored as JSON arrays of single IPs or CIDR prefixes; empty means no rule.
type IPAccessSettings struct {
	Whitelist []string `json:"whitelist"`
	Blacklist []string `json:"blacklist"`
	UpdatedAt int64    `json:"updated_at"`
}

// GetIPAccessSettings reads the single settings row. Migration seeds it, so the
// empty lists are the default on a fresh database.
func GetIPAccessSettings() (IPAccessSettings, error) {
	var (
		whitelist string
		blacklist string
		updatedAt int64
	)
	err := Read().QueryRow(
		`SELECT whitelist, blacklist, updated_at FROM ip_access_settings WHERE id = 1`,
	).Scan(&whitelist, &blacklist, &updatedAt)
	if err != nil {
		return IPAccessSettings{}, fmt.Errorf("read IP access settings: %w", err)
	}
	allowed, err := decodeIPList(whitelist)
	if err != nil {
		return IPAccessSettings{}, fmt.Errorf("decode IP whitelist: %w", err)
	}
	denied, err := decodeIPList(blacklist)
	if err != nil {
		return IPAccessSettings{}, fmt.Errorf("decode IP blacklist: %w", err)
	}
	return IPAccessSettings{Whitelist: allowed, Blacklist: denied, UpdatedAt: updatedAt}, nil
}

// SaveIPAccessSettings replaces both lists atomically and returns the stored
// view. Validation happens in the handler before this is called.
func SaveIPAccessSettings(whitelist, blacklist []string) (IPAccessSettings, error) {
	if whitelist == nil {
		whitelist = []string{}
	}
	if blacklist == nil {
		blacklist = []string{}
	}
	allowedJSON, err := json.Marshal(whitelist)
	if err != nil {
		return IPAccessSettings{}, fmt.Errorf("encode IP whitelist: %w", err)
	}
	deniedJSON, err := json.Marshal(blacklist)
	if err != nil {
		return IPAccessSettings{}, fmt.Errorf("encode IP blacklist: %w", err)
	}
	updatedAt := time.Now().UnixMilli()
	if _, err := Get().Exec(
		`INSERT INTO ip_access_settings (id, whitelist, blacklist, updated_at)
		 VALUES (1, ?, ?, ?)
		 ON CONFLICT(id) DO UPDATE SET
		   whitelist = excluded.whitelist,
		   blacklist = excluded.blacklist,
		   updated_at = excluded.updated_at`,
		string(allowedJSON), string(deniedJSON), updatedAt,
	); err != nil {
		return IPAccessSettings{}, fmt.Errorf("save IP access settings: %w", err)
	}
	return IPAccessSettings{Whitelist: whitelist, Blacklist: blacklist, UpdatedAt: updatedAt}, nil
}

func decodeIPList(raw string) ([]string, error) {
	if strings.TrimSpace(raw) == "" {
		return []string{}, nil
	}
	var list []string
	if err := json.Unmarshal([]byte(raw), &list); err != nil {
		return nil, err
	}
	if list == nil {
		list = []string{}
	}
	return list, nil
}
