package api

import (
	"context"
	"log/slog"

	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// OracleServiceNameConflictResponse warns that this Oracle row's upstream
// service name is also claimed by rows spelling their host differently.
//
// It is not a failure of the row: each such row dials and logs in fine on its
// own. What breaks is a client that connects with the *shared service name*
// rather than the dbbat server name — the proxy compares candidate upstreams as
// text, so two spellings of one machine read as two upstreams and the connect is
// refused ORA-12514. The compare stays textual on purpose, so this warning is
// how the misconfiguration becomes visible before a user hits it.
type OracleServiceNameConflictResponse struct {
	// ServiceName is the shared upstream `oracle_service_name`.
	ServiceName string `json:"service_name"`
	// Upstreams are the distinct `host:port` spellings in play, sorted.
	Upstreams []string `json:"upstreams"`
	// Servers are the rows claiming the service name, ordered by name.
	Servers []OracleServiceNameConflictServerResponse `json:"servers"`
	// Message is the ready-made operator-facing sentence, so every surface
	// (this API, the connectivity check, the proxy log) says the same thing.
	Message string `json:"message"`
}

// OracleServiceNameConflictServerResponse identifies one row taking part in a
// conflict. No credential material: the name and address are what an admin needs
// to reconcile the spellings.
type OracleServiceNameConflictServerResponse struct {
	UID  uuid.UUID `json:"uid"`
	Name string    `json:"name"`
	Host string    `json:"host"`
	Port int       `json:"port"`
}

// toOracleServiceNameConflictResponse converts the store's conflict to its API
// shape, or nil when there is none.
func toOracleServiceNameConflictResponse(conflict *store.OracleServiceNameConflict) *OracleServiceNameConflictResponse {
	if conflict == nil {
		return nil
	}

	servers := make([]OracleServiceNameConflictServerResponse, 0, len(conflict.Servers))
	for _, srv := range conflict.Servers {
		servers = append(servers, OracleServiceNameConflictServerResponse{
			UID:  srv.UID,
			Name: srv.Name,
			Host: srv.Host,
			Port: srv.Port,
		})
	}

	return &OracleServiceNameConflictResponse{
		ServiceName: conflict.ServiceName,
		Upstreams:   conflict.Upstreams,
		Servers:     servers,
		Message:     conflict.Describe(),
	}
}

// attachOracleServiceNameConflicts stamps the warning onto every Oracle row of an
// admin listing, from a single fleet-wide query rather than one lookup per row.
//
// A failure is logged and swallowed: the listing is the admin's view of their
// fleet and must not disappear because an advisory query failed.
func (s *Server) attachOracleServiceNameConflicts(ctx context.Context, responses []DatabaseResponse) {
	if len(responses) == 0 {
		return
	}

	// Cheap guard: no Oracle row with a service name, no query.
	var relevant bool

	for i := range responses {
		if responses[i].Protocol == store.ProtocolOracle && responses[i].OracleServiceName != "" {
			relevant = true

			break
		}
	}

	if !relevant {
		return
	}

	conflicts, err := s.store.ListOracleServiceNameConflicts(ctx)
	if err != nil {
		if s.logger != nil {
			s.logger.WarnContext(ctx, "failed to look up Oracle service-name conflicts", slog.Any("error", err))
		}

		return
	}

	if len(conflicts) == 0 {
		return
	}

	byServiceName := make(map[string]*store.OracleServiceNameConflict, len(conflicts))
	for i := range conflicts {
		byServiceName[conflicts[i].ServiceName] = &conflicts[i]
	}

	for i := range responses {
		if responses[i].Protocol != store.ProtocolOracle || responses[i].OracleServiceName == "" {
			continue
		}

		conflict := byServiceName[responses[i].OracleServiceName]
		responses[i].OracleServiceNameConflict = toOracleServiceNameConflictResponse(conflict)
	}
}

// attachOracleServiceNameConflict does the same for a single row, used by the
// per-database GET and by the create path — where an admin has just introduced
// the second spelling and should hear about it immediately.
func (s *Server) attachOracleServiceNameConflict(ctx context.Context, resp *DatabaseResponse) {
	if resp == nil || resp.Protocol != store.ProtocolOracle || resp.OracleServiceName == "" {
		return
	}

	conflict, found, err := s.store.GetOracleServiceNameConflict(ctx, resp.OracleServiceName)
	if err != nil {
		if s.logger != nil {
			s.logger.WarnContext(ctx, "failed to look up the Oracle service-name conflict", slog.Any("error", err))
		}

		return
	}

	if !found {
		return
	}

	resp.OracleServiceNameConflict = toOracleServiceNameConflictResponse(conflict)
}
