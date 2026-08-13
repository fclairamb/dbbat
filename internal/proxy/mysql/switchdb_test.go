package mysql

import (
	"context"
	"errors"
	"testing"
	"time"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
	"github.com/google/uuid"

	"github.com/fclairamb/dbbat/internal/store"
)

// switchSession builds a session whose grant carries no controls at all — the
// full-write case, which is the one that matters: a full-write grant on one
// database is still not a grant on another, so the refusal must not depend on
// read_only or block_ddl being set.
func switchSession() *Session {
	return &Session{
		logger:       discardLogger(),
		ctx:          context.Background(),
		authComplete: true,
		database: &store.Server{
			Name:         "prod-entry",
			DatabaseName: "appdb",
		},
		grant: &store.Grant{
			UID:        uuid.New(),
			ExpiresAt:  time.Now().Add(time.Hour),
			Definition: &store.GrantDefinition{},
		},
	}
}

// TestTextUseIsRefused is the regression test for the hole: go-mysql routes
// COM_QUERY straight to HandleQuery, so a text `USE otherdb` never touched
// UseDB and was forwarded upstream under every grant.
func TestTextUseIsRefused(t *testing.T) {
	t.Parallel()

	refused := []string{
		"USE otherdb",
		"use otherdb",
		"UsE OtherDB",
		"USE otherdb;",
		"  USE   otherdb  ",
		"USE `otherdb`",
		"USE`otherdb`",
		"/* hello */ USE otherdb",
		"USE/**/otherdb",
		"USE/*!*/otherdb",
		"USE # comment\notherdb",
		"USE -- comment\notherdb",
		// Unreadable is still refused: an unparseable switch is a switch.
		"USE otherdb; SELECT 1",
		"USE `otherdb",
	}

	for _, sql := range refused {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			hnd := &handler{session: switchSession()}
			execRan := false

			_, err := hnd.runIntercepted(sql, nil, func() (*gomysql.Result, error) {
				execRan = true

				return &gomysql.Result{}, nil
			})

			if !errors.Is(err, ErrSwitchDatabaseDenied) {
				t.Fatalf("runIntercepted(%q) = %v, want ErrSwitchDatabaseDenied", sql, err)
			}

			if execRan {
				t.Fatalf("runIntercepted(%q) reached the upstream", sql)
			}
		})
	}
}

// TestUseOfGrantedDatabaseIsAllowed pins the other half as hard as the refusal:
// clients emit `USE <their own database>` routinely on connect, under both the
// dbbat entry's name and the real database name.
func TestUseOfGrantedDatabaseIsAllowed(t *testing.T) {
	t.Parallel()

	for _, sql := range []string{
		// The dbbat entry's name, which is what the client's connection string
		// carries. A hyphen needs backticks in MySQL, exactly as here.
		"USE `prod-entry`",
		"USE appdb",
		"USE `appdb`",
		"use appdb;",
		"/* connect */ USE appdb",
	} {
		t.Run(sql, func(t *testing.T) {
			t.Parallel()

			hnd := &handler{session: switchSession()}

			result, err := hnd.runIntercepted(sql, nil, func() (*gomysql.Result, error) {
				t.Fatal("an allowed USE is answered by the proxy, not relayed upstream")

				return nil, nil
			})
			if err != nil {
				t.Fatalf("runIntercepted(%q) = %v, want nil", sql, err)
			}

			// A nil *Result with a nil error is what makes go-mysql write the OK
			// packet the client is waiting for.
			if result != nil {
				t.Fatalf("runIntercepted(%q) result = %v, want nil", sql, result)
			}
		})
	}
}

// TestUseIsNotAGrantControl states the binding decision outright: the refusal is
// not a function of read_only / block_ddl / block_copy, and a query that merely
// mentions USE is not a switch.
func TestUseIsNotAGrantControl(t *testing.T) {
	t.Parallel()

	s := switchSession()
	s.grant.Definition.Controls = []string{store.ControlReadOnly}

	hnd := &handler{session: s}

	_, err := hnd.runIntercepted("USE otherdb", nil, func() (*gomysql.Result, error) {
		return &gomysql.Result{}, nil
	})
	if !errors.Is(err, ErrSwitchDatabaseDenied) {
		t.Fatalf("read-only USE otherdb = %v, want ErrSwitchDatabaseDenied", err)
	}

	// `USE INDEX` is an ordinary MySQL hint, not a database switch.
	hnd = &handler{session: switchSession()}
	ran := false

	if _, err := hnd.runIntercepted(
		"SELECT * FROM t USE INDEX (idx)", nil,
		func() (*gomysql.Result, error) {
			ran = true

			return &gomysql.Result{}, nil
		},
	); err != nil {
		t.Fatalf("SELECT with a USE INDEX hint = %v, want nil", err)
	}

	if !ran {
		t.Fatal("a USE INDEX hint must reach the upstream")
	}
}

// TestUseDBAndTextUseShareOneDecision is the anti-drift test: COM_INIT_DB and
// the text form must agree on every name, because two implementations of "may
// this session change database" is how one of them silently stops matching the
// other.
func TestUseDBAndTextUseShareOneDecision(t *testing.T) {
	t.Parallel()

	for _, name := range []string{"appdb", "prod-entry", "otherdb", "APPDB", ""} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			initDB := (&handler{session: switchSession()}).UseDB(name)

			hnd := &handler{session: switchSession()}
			_, text := hnd.runIntercepted("USE `"+name+"`", nil, func() (*gomysql.Result, error) {
				return &gomysql.Result{}, nil
			})

			if errors.Is(initDB, ErrSwitchDatabaseDenied) != errors.Is(text, ErrSwitchDatabaseDenied) {
				t.Fatalf("COM_INIT_DB(%q) = %v but text USE = %v", name, initDB, text)
			}
		})
	}
}
