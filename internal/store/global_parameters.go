package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"

	"github.com/fclairamb/dbbat/internal/config"
)

var ErrParameterNotFound = errors.New("parameter not found")

// GetParameter retrieves a single active parameter by group and key.
func (s *Store) GetParameter(ctx context.Context, groupKey, key string) (*GlobalParameter, error) {
	param := new(GlobalParameter)
	err := s.db.NewSelect().
		Model(param).
		Where("group_key = ?", groupKey).
		Where("key = ?", key).
		Scan(ctx)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrParameterNotFound
		}
		return nil, fmt.Errorf("failed to get parameter: %w", err)
	}
	return param, nil
}

// GetParameters retrieves all active parameters for a group.
func (s *Store) GetParameters(ctx context.Context, groupKey string) ([]GlobalParameter, error) {
	var params []GlobalParameter
	err := s.db.NewSelect().
		Model(&params).
		Where("group_key = ?", groupKey).
		Order("key ASC").
		Scan(ctx)
	if err != nil {
		return nil, fmt.Errorf("failed to get parameters: %w", err)
	}
	if params == nil {
		params = []GlobalParameter{}
	}
	return params, nil
}

// GetAllParameters retrieves all active parameters, optionally filtered by group.
func (s *Store) GetAllParameters(ctx context.Context, groupKey string) ([]GlobalParameter, error) {
	var params []GlobalParameter
	q := s.db.NewSelect().
		Model(&params).
		Order("group_key ASC", "key ASC")
	if groupKey != "" {
		q = q.Where("group_key = ?", groupKey)
	}
	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list parameters: %w", err)
	}
	if params == nil {
		params = []GlobalParameter{}
	}
	return params, nil
}

// SetParameter creates or updates a parameter (upsert on group_key+key).
func (s *Store) SetParameter(ctx context.Context, groupKey, key, value string) error {
	_, err := s.db.NewRaw(
		`INSERT INTO global_parameters (group_key, key, value, updated_at)
		VALUES (?, ?, ?, NOW())
		ON CONFLICT (group_key, key) WHERE deleted_at IS NULL
		DO UPDATE SET value = EXCLUDED.value, updated_at = NOW()`,
		groupKey, key, value,
	).Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to set parameter: %w", err)
	}
	return nil
}

// DeleteParameter soft-deletes a parameter.
func (s *Store) DeleteParameter(ctx context.Context, groupKey, key string) error {
	result, err := s.db.NewDelete().
		Model((*GlobalParameter)(nil)).
		Where("group_key = ?", groupKey).
		Where("key = ?", key).
		Exec(ctx)
	if err != nil {
		return fmt.Errorf("failed to delete parameter: %w", err)
	}
	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("failed to get rows affected: %w", err)
	}
	if rowsAffected == 0 {
		return ErrParameterNotFound
	}
	return nil
}

// Public endpoint parameter group and key constants.
const (
	GroupPublic        = "public"
	KeyPublicHost      = "host"
	KeyPublicPGHost    = "pg.host"
	KeyPublicOraHost   = "ora.host"
	KeyPublicMySQLHost = "mysql.host"
	KeyPublicPGPort    = "pg.port"
	KeyPublicOraPort   = "ora.port"
	KeyPublicMySQLPort = "mysql.port"
)

// PublicEndpoints holds the operator-configured public advertisement settings.
type PublicEndpoints struct {
	Host      string // default public hostname for all protocols
	PGHost    string // optional override; "" = fall back to Host
	OraHost   string
	MySQLHost string
	PGPort    *int // optional override; nil = fall back to local listen port
	OraPort   *int
	MySQLPort *int
}

// GetPublicEndpoints reads all public.* parameters and returns the typed struct.
func (s *Store) GetPublicEndpoints(ctx context.Context) (PublicEndpoints, error) {
	params, err := s.GetParameters(ctx, GroupPublic)
	if err != nil {
		return PublicEndpoints{}, err
	}
	var pe PublicEndpoints
	for _, p := range params {
		switch p.Key {
		case KeyPublicHost:
			pe.Host = p.Value
		case KeyPublicPGHost:
			pe.PGHost = p.Value
		case KeyPublicOraHost:
			pe.OraHost = p.Value
		case KeyPublicMySQLHost:
			pe.MySQLHost = p.Value
		case KeyPublicPGPort:
			if n, err := strconv.Atoi(p.Value); err == nil {
				pe.PGPort = &n
			}
		case KeyPublicOraPort:
			if n, err := strconv.Atoi(p.Value); err == nil {
				pe.OraPort = &n
			}
		case KeyPublicMySQLPort:
			if n, err := strconv.Atoi(p.Value); err == nil {
				pe.MySQLPort = &n
			}
		}
	}
	return pe, nil
}

// SetPublicEndpoints writes only the non-empty/non-nil fields.
func (s *Store) SetPublicEndpoints(ctx context.Context, pe PublicEndpoints) error {
	type kv struct{ key, value string }
	var pairs []kv

	if pe.Host != "" {
		pairs = append(pairs, kv{KeyPublicHost, pe.Host})
	}
	if pe.PGHost != "" {
		pairs = append(pairs, kv{KeyPublicPGHost, pe.PGHost})
	}
	if pe.OraHost != "" {
		pairs = append(pairs, kv{KeyPublicOraHost, pe.OraHost})
	}
	if pe.MySQLHost != "" {
		pairs = append(pairs, kv{KeyPublicMySQLHost, pe.MySQLHost})
	}
	if pe.PGPort != nil {
		pairs = append(pairs, kv{KeyPublicPGPort, strconv.Itoa(*pe.PGPort)})
	}
	if pe.OraPort != nil {
		pairs = append(pairs, kv{KeyPublicOraPort, strconv.Itoa(*pe.OraPort)})
	}
	if pe.MySQLPort != nil {
		pairs = append(pairs, kv{KeyPublicMySQLPort, strconv.Itoa(*pe.MySQLPort)})
	}

	for _, p := range pairs {
		if err := s.SetParameter(ctx, GroupPublic, p.key, p.value); err != nil {
			return err
		}
	}
	return nil
}

// ResolvedEndpoints holds the fully resolved connection advertisement values.
type ResolvedEndpoints struct {
	PGHost    string
	OraHost   string
	MySQLHost string
	PGPort    int // 0 = protocol disabled
	OraPort   int
	MySQLPort int
}

// ResolvePublicEndpoints applies fallback chains for host and port resolution.
func ResolvePublicEndpoints(pe PublicEndpoints, cfg *config.Config) ResolvedEndpoints {
	resolve := func(protoHost, defaultHost string) string {
		if protoHost != "" {
			return protoHost
		}
		return defaultHost
	}

	resolvePort := func(override *int, listenAddr string) int {
		if override != nil {
			return *override
		}
		if listenAddr == "" {
			return 0
		}
		_, portStr, err := net.SplitHostPort(listenAddr)
		if err != nil {
			return 0
		}
		port, err := strconv.Atoi(portStr)
		if err != nil {
			return 0
		}
		return port
	}

	return ResolvedEndpoints{
		PGHost:    resolve(pe.PGHost, pe.Host),
		OraHost:   resolve(pe.OraHost, pe.Host),
		MySQLHost: resolve(pe.MySQLHost, pe.Host),
		PGPort:    resolvePort(pe.PGPort, cfg.ListenPG),
		OraPort:   resolvePort(pe.OraPort, cfg.ListenOracle),
		MySQLPort: resolvePort(pe.MySQLPort, cfg.ListenMySQL),
	}
}
