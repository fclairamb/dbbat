package store

import (
	"context"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// oracleConflictSampleLimit caps how many server names and upstream addresses a
// conflict description quotes. A mutualized instance can carry dozens of
// schemas; the operator needs to see that the rows disagree and which addresses
// are in play, not a wall of text in a log line or a tooltip.
const oracleConflictSampleLimit = 4

// OracleServiceNameConflict describes one upstream Oracle service name that
// several dbbat server rows claim while disagreeing on the upstream address.
//
// It exists because the Oracle proxy resolves a shared service name by
// comparing candidate rows' `host:port` **as text** (session.go,
// resolveDatabase): two spellings of one machine — a CNAME in one row, the
// A-record in another — read as two different upstreams, and the connect is
// refused ORA-12514 even though every candidate points at the same database.
// That compare stays textual on purpose (it never surprises, and resolving DNS
// on the connect path could answer differently between two connects of one
// DSN), so the misconfiguration is surfaced instead: on the server row in the
// admin UI, and in the connectivity check.
type OracleServiceNameConflict struct {
	// ServiceName is the shared upstream `oracle_service_name`.
	ServiceName string `json:"service_name"`
	// Upstreams are the distinct `host:port` spellings, sorted. Two entries or
	// more is what makes this a conflict.
	Upstreams []string `json:"upstreams"`
	// Servers are the rows claiming the service name, ordered by name.
	Servers []OracleServiceNameConflictServer `json:"servers"`
}

// OracleServiceNameConflictServer identifies one row taking part in a conflict.
// It carries no credential material: the name and the address are exactly what
// the admin needs to reconcile the spellings.
type OracleServiceNameConflictServer struct {
	UID  uuid.UUID `json:"uid"`
	Name string    `json:"name"`
	Host string    `json:"host"`
	Port int       `json:"port"`
}

// Describe renders the conflict as one operator-facing sentence, shared by every
// surface that reports it (the proxy's refusal log, the connectivity check, the
// API response the admin UI renders) so the wording cannot drift between them.
func (c *OracleServiceNameConflict) Describe() string {
	if c == nil {
		return ""
	}

	names := make([]string, 0, len(c.Servers))
	for _, srv := range c.Servers {
		names = append(names, srv.Name)
	}

	return fmt.Sprintf(
		"Oracle service name %q is registered by %d dbbat servers (%s) pointing at %d different upstream addresses (%s); "+
			"a client connecting with the shared service name is refused ORA-12514 — "+
			"make every row spell the same host, or have clients connect by dbbat server name",
		c.ServiceName,
		len(c.Servers), sampleList(names),
		len(c.Upstreams), sampleList(c.Upstreams),
	)
}

// sampleList joins at most oracleConflictSampleLimit entries, noting how many
// were left out.
func sampleList(values []string) string {
	if len(values) <= oracleConflictSampleLimit {
		return strings.Join(values, ", ")
	}

	return fmt.Sprintf("%s, +%d more",
		strings.Join(values[:oracleConflictSampleLimit], ", "),
		len(values)-oracleConflictSampleLimit)
}

// OracleServiceNameConflictFor builds the conflict for a set of candidate rows
// already in hand, or nil when they all agree on one `host:port`.
//
// It is the single definition of "these rows disagree", used both by the proxy —
// which has already loaded the candidates via ListServersByOracleServiceName
// and must decide whether to refuse — and by the store queries below, which
// scan the fleet. A second, SQL-shaped implementation of the same rule is
// exactly the drift that would let the UI call a configuration healthy while
// the proxy refuses it.
func OracleServiceNameConflictFor(serviceName string, servers []Server) *OracleServiceNameConflict {
	if len(servers) < 2 {
		return nil
	}

	conflict := &OracleServiceNameConflict{ServiceName: serviceName}

	seen := make(map[string]struct{}, len(servers))

	for i := range servers {
		addr := net.JoinHostPort(servers[i].Host, strconv.Itoa(servers[i].Port))
		if _, ok := seen[addr]; !ok {
			seen[addr] = struct{}{}
			conflict.Upstreams = append(conflict.Upstreams, addr)
		}

		conflict.Servers = append(conflict.Servers, OracleServiceNameConflictServer{
			UID:  servers[i].UID,
			Name: servers[i].Name,
			Host: servers[i].Host,
			Port: servers[i].Port,
		})
	}

	if len(conflict.Upstreams) < 2 {
		return nil
	}

	sort.Strings(conflict.Upstreams)
	sort.Slice(conflict.Servers, func(i, j int) bool { return conflict.Servers[i].Name < conflict.Servers[j].Name })

	return conflict
}

// ListOracleServiceNameConflicts returns every upstream Oracle service name the
// fleet claims from more than one address, ordered by service name.
//
// Only the dedicated `oracle_service_name` column is considered — not the
// database-name fallback the probe uses — because that column is exactly what
// the proxy's candidate lookup keys off, and therefore the only value that can
// produce the ambiguous-service refusal. Soft-deleted rows are excluded (bun's
// soft-delete filter), so a row an admin has already removed never keeps a
// warning alive.
func (s *Store) ListOracleServiceNameConflicts(ctx context.Context) ([]OracleServiceNameConflict, error) {
	return s.oracleServiceNameConflicts(ctx, "")
}

// GetOracleServiceNameConflict returns the conflict for one service name. The
// bool is what says whether there is one: the rows claiming the name may well
// agree, or nothing may claim it, and neither is an error.
func (s *Store) GetOracleServiceNameConflict(
	ctx context.Context,
	serviceName string,
) (*OracleServiceNameConflict, bool, error) {
	if serviceName == "" {
		return nil, false, nil
	}

	conflicts, err := s.oracleServiceNameConflicts(ctx, serviceName)
	if err != nil {
		return nil, false, err
	}

	if len(conflicts) == 0 {
		return nil, false, nil
	}

	return &conflicts[0], true, nil
}

// oracleServiceNameConflicts scans the Oracle rows carrying a service name —
// all of them, or one name's worth — and groups them through
// OracleServiceNameConflictFor.
func (s *Store) oracleServiceNameConflicts(ctx context.Context, serviceName string) ([]OracleServiceNameConflict, error) {
	var servers []Server

	q := s.db.NewSelect().
		Model(&servers).
		Where("protocol = ?", ProtocolOracle).
		Where("oracle_service_name IS NOT NULL").
		Where("oracle_service_name <> ''").
		Order("name ASC")

	if serviceName != "" {
		q = q.Where("oracle_service_name = ?", serviceName)
	}

	if err := q.Scan(ctx); err != nil {
		return nil, fmt.Errorf("failed to list oracle servers by service name: %w", err)
	}

	grouped := make(map[string][]Server)
	names := make([]string, 0)

	for i := range servers {
		name := *servers[i].OracleServiceName
		if _, ok := grouped[name]; !ok {
			names = append(names, name)
		}

		grouped[name] = append(grouped[name], servers[i])
	}

	sort.Strings(names)

	conflicts := make([]OracleServiceNameConflict, 0, len(names))

	for _, name := range names {
		if conflict := OracleServiceNameConflictFor(name, grouped[name]); conflict != nil {
			conflicts = append(conflicts, *conflict)
		}
	}

	return conflicts, nil
}
