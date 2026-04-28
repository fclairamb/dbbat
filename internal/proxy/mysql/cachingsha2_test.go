package mysql

import (
	"reflect"
	"testing"

	gomysqlserver "github.com/go-mysql-org/go-mysql/server"
)

// TestReadConnSalt_FieldExists guards against silent breakage if go-mysql
// renames or removes the unexported `salt` field on *server.Conn. The
// caching_sha2_password RSA path needs that field — see readConnSalt.
//
// We construct a zero-value *Conn (no real connection) and only check that
// reflection finds the right field of the right shape. Calling readConnSalt
// against a zero-valued Conn returns an empty slice (the field is unset),
// which is fine — we just want to fail loudly here at test time if the
// reflection target disappears.
func TestReadConnSalt_FieldExists(t *testing.T) {
	t.Parallel()

	c := &gomysqlserver.Conn{}

	v := reflect.ValueOf(c).Elem()

	field := v.FieldByName("salt")
	if !field.IsValid() {
		t.Fatal("go-mysql server.Conn no longer has a `salt` field — readConnSalt() will fail at runtime; pin or update the dependency")
	}

	if field.Kind() != reflect.Slice {
		t.Fatalf("expected `salt` to be a slice, got %s", field.Kind())
	}

	if elem := field.Type().Elem().Kind(); elem != reflect.Uint8 {
		t.Fatalf("expected `salt` to be []byte, element kind %s", elem)
	}
}
