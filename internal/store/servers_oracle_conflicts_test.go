package store

import (
	"context"
	"strings"
	"testing"
)

// mkOracleServer builds an Oracle row claiming svc at host:1521.
func mkOracleServer(name, host, svc string) *Server {
	service := svc

	return &Server{
		Name:              name,
		Host:              host,
		Port:              1521,
		DatabaseName:      name,
		Username:          name + "_user",
		Password:          "pass",
		SSLMode:           "prefer",
		Protocol:          ProtocolOracle,
		OracleServiceName: &service,
	}
}

// TestOracleServiceNameConflictFor pins the rule the proxy and the store share:
// rows agreeing on host:port are fine however many there are, and one
// disagreement is a conflict. This is the incident's shape — one machine spelled
// two ways behind MUTU02 — reduced to a pure function, so it is covered without
// a database.
func TestOracleServiceNameConflictFor(t *testing.T) {
	t.Parallel()

	srv := func(name, host string, port int) Server {
		return Server{Name: name, Host: host, Port: port}
	}

	t.Run("a single row is never a conflict", func(t *testing.T) {
		t.Parallel()

		if c := OracleServiceNameConflictFor("MUTU02", []Server{srv("a", "h1", 1521)}); c != nil {
			t.Fatalf("got conflict %+v, want nil", c)
		}
	})

	t.Run("rows agreeing on host:port are not a conflict", func(t *testing.T) {
		t.Parallel()

		rows := []Server{srv("a", "h1", 1521), srv("b", "h1", 1521), srv("c", "h1", 1521)}
		if c := OracleServiceNameConflictFor("MUTU02", rows); c != nil {
			t.Fatalf("got conflict %+v, want nil", c)
		}
	})

	t.Run("two spellings of one host read as two upstreams", func(t *testing.T) {
		t.Parallel()

		rows := []Server{
			srv("abyla_abymutualise02_ro", "oracle-abymutualise02.db.stonal.io", 1521),
			srv("abyla_abymutualise_ro", "abymutualise02.cusruf0cguz3.eu-west-3.rds.amazonaws.com", 1521),
		}

		c := OracleServiceNameConflictFor("MUTU02", rows)
		if c == nil {
			t.Fatal("got nil, want a conflict")
		}

		if len(c.Upstreams) != 2 {
			t.Errorf("Upstreams = %v, want 2 entries", c.Upstreams)
		}

		if len(c.Servers) != 2 {
			t.Errorf("Servers = %+v, want 2 entries", c.Servers)
		}

		desc := c.Describe()
		for _, want := range []string{"MUTU02", "oracle-abymutualise02.db.stonal.io", "abyla_abymutualise_ro", "ORA-12514"} {
			if !strings.Contains(desc, want) {
				t.Errorf("Describe() = %q, want it to mention %q", desc, want)
			}
		}
	})

	t.Run("the same host on two ports is two upstreams", func(t *testing.T) {
		t.Parallel()

		rows := []Server{srv("a", "h1", 1521), srv("b", "h1", 1522)}
		if c := OracleServiceNameConflictFor("MUTU02", rows); c == nil {
			t.Fatal("got nil, want a conflict: the proxy compares host:port, not host")
		}
	})

	t.Run("a long conflict is sampled rather than dumped", func(t *testing.T) {
		t.Parallel()

		rows := make([]Server, 0, 10)
		for i := range 10 {
			rows = append(rows, srv(string(rune('a'+i)), string(rune('a'+i))+".example.com", 1521))
		}

		c := OracleServiceNameConflictFor("MUTU02", rows)
		if c == nil {
			t.Fatal("got nil, want a conflict")
		}

		if !strings.Contains(c.Describe(), "+6 more") {
			t.Errorf("Describe() = %q, want it to sample the tail", c.Describe())
		}
	})
}

// TestOracleServiceNameConflicts_Store covers the two store queries: the
// fleet-wide scan the admin listing uses and the per-name lookup the
// connectivity check uses, including the two ways a warning must NOT fire — a
// non-Oracle row, and a row an admin has already deleted.
func TestOracleServiceNameConflicts_Store(t *testing.T) {
	t.Parallel()

	s := setupTestStore(t)
	ctx := context.Background()
	key := testEncryptionKey()

	const (
		conflicting = "MUTU_CONFLICT_TEST"
		agreeing    = "MUTU_AGREE_TEST"
	)

	create := func(t *testing.T, srv *Server) *Server {
		t.Helper()

		created, err := s.CreateServer(ctx, srv, key)
		if err != nil {
			t.Fatalf("CreateServer(%s) error = %v", srv.Name, err)
		}

		return created
	}

	create(t, mkOracleServer("ora_conflict_a", "oracle-mutu.db.example.com", conflicting))
	create(t, mkOracleServer("ora_conflict_b", "mutu.eu-west-3.rds.amazonaws.com", conflicting))
	create(t, mkOracleServer("ora_agree_a", "oracle-agree.db.example.com", agreeing))
	create(t, mkOracleServer("ora_agree_b", "oracle-agree.db.example.com", agreeing))

	t.Run("the disagreeing pair is reported, the agreeing pair is not", func(t *testing.T) {
		t.Parallel()

		conflicts, err := s.ListOracleServiceNameConflicts(ctx)
		if err != nil {
			t.Fatalf("ListOracleServiceNameConflicts() error = %v", err)
		}

		byName := make(map[string]OracleServiceNameConflict, len(conflicts))
		for _, c := range conflicts {
			byName[c.ServiceName] = c
		}

		got, ok := byName[conflicting]
		if !ok {
			t.Fatalf("ListOracleServiceNameConflicts() = %+v, want an entry for %s", conflicts, conflicting)
		}

		if len(got.Upstreams) != 2 || len(got.Servers) != 2 {
			t.Errorf("conflict = %+v, want 2 upstreams and 2 servers", got)
		}

		if got.Servers[0].Name != "ora_conflict_a" {
			t.Errorf("Servers[0].Name = %q, want ora_conflict_a (ordered by name)", got.Servers[0].Name)
		}

		if _, ok := byName[agreeing]; ok {
			t.Errorf("ListOracleServiceNameConflicts() reported %s, whose rows agree on host:port", agreeing)
		}
	})

	t.Run("the per-name lookup answers for one service name only", func(t *testing.T) {
		t.Parallel()

		got, found, err := s.GetOracleServiceNameConflict(ctx, conflicting)
		if err != nil {
			t.Fatalf("GetOracleServiceNameConflict() error = %v", err)
		}

		if !found || got == nil {
			t.Fatalf("GetOracleServiceNameConflict(%s) = %+v, %v; want a conflict", conflicting, got, found)
		}

		if got.ServiceName != conflicting {
			t.Errorf("ServiceName = %q, want %q", got.ServiceName, conflicting)
		}

		for _, name := range []string{agreeing, "NO_SUCH_SERVICE", ""} {
			c, found, err := s.GetOracleServiceNameConflict(ctx, name)
			if err != nil {
				t.Fatalf("GetOracleServiceNameConflict(%q) error = %v", name, err)
			}

			if found || c != nil {
				t.Errorf("GetOracleServiceNameConflict(%q) = %+v, %v; want no conflict", name, c, found)
			}
		}
	})

	t.Run("a non-Oracle row sharing the name is not a conflict", func(t *testing.T) {
		t.Parallel()

		svc := "MUTU_PROTOCOL_TEST"
		create(t, mkOracleServer("ora_proto_a", "oracle-proto.db.example.com", svc))

		pg := mkOracleServer("pg_proto_b", "pg-proto.db.example.com", svc)
		pg.Protocol = ProtocolPostgreSQL
		pg.Port = 5432
		create(t, pg)

		got, found, err := s.GetOracleServiceNameConflict(ctx, svc)
		if err != nil {
			t.Fatalf("GetOracleServiceNameConflict() error = %v", err)
		}

		if found || got != nil {
			t.Errorf("got %+v, %v; want no conflict: only Oracle rows take part in the candidate lookup", got, found)
		}
	})

	t.Run("a deleted row raises no conflict", func(t *testing.T) {
		t.Parallel()

		svc := "MUTU_DELETED_TEST"
		create(t, mkOracleServer("ora_deleted_a", "oracle-deleted.db.example.com", svc))
		doomed := create(t, mkOracleServer("ora_deleted_b", "deleted.eu-west-3.rds.amazonaws.com", svc))

		if got, found, err := s.GetOracleServiceNameConflict(ctx, svc); err != nil || !found {
			t.Fatalf("GetOracleServiceNameConflict() = %+v, %v, %v; want a conflict before the delete", got, found, err)
		}

		if err := s.DeleteServer(ctx, doomed.UID); err != nil {
			t.Fatalf("DeleteServer() error = %v", err)
		}

		got, found, err := s.GetOracleServiceNameConflict(ctx, svc)
		if err != nil {
			t.Fatalf("GetOracleServiceNameConflict() error = %v", err)
		}

		if found || got != nil {
			t.Errorf("got %+v, %v; want no conflict: a soft-deleted row must not keep a warning alive", got, found)
		}
	})
}
