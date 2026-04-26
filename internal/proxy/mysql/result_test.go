package mysql

import (
	"encoding/json"
	"testing"

	gomysql "github.com/go-mysql-org/go-mysql/mysql"
)

func TestEncodeRow_Strings(t *testing.T) {
	t.Parallel()

	fields := []*gomysql.Field{
		{Type: gomysql.MYSQL_TYPE_VARCHAR},
		{Type: gomysql.MYSQL_TYPE_VAR_STRING},
	}
	row := []gomysql.FieldValue{
		gomysql.NewFieldValue(gomysql.FieldValueTypeString, 0, []byte("alice")),
		gomysql.NewFieldValue(gomysql.FieldValueTypeString, 0, []byte("admin")),
	}

	got, err := encodeRow(fields, row)
	if err != nil {
		t.Fatalf("encodeRow failed: %v", err)
	}

	want := `["alice","admin"]`
	if string(got) != want {
		t.Errorf("got %s, want %s", got, want)
	}
}

func TestEncodeRow_Numbers(t *testing.T) {
	t.Parallel()

	fields := []*gomysql.Field{
		{Type: gomysql.MYSQL_TYPE_LONGLONG},
		{Type: gomysql.MYSQL_TYPE_DOUBLE},
	}
	negInt := int64(-42)
	row := []gomysql.FieldValue{
		gomysql.NewFieldValue(gomysql.FieldValueTypeSigned, uint64(negInt), nil),
		gomysql.NewFieldValue(gomysql.FieldValueTypeFloat, 0, nil), // value=0 → 0.0
	}

	got, err := encodeRow(fields, row)
	if err != nil {
		t.Fatalf("encodeRow failed: %v", err)
	}

	// json.Marshal of int64(-42), float64(0)
	var decoded []any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode failed: %v", err)
	}

	if len(decoded) != 2 {
		t.Fatalf("expected 2 values, got %d", len(decoded))
	}
}

func TestEncodeRow_Null(t *testing.T) {
	t.Parallel()

	fields := []*gomysql.Field{{Type: gomysql.MYSQL_TYPE_VARCHAR}}
	row := []gomysql.FieldValue{
		gomysql.NewFieldValue(gomysql.FieldValueTypeNull, 0, nil),
	}

	got, err := encodeRow(fields, row)
	if err != nil {
		t.Fatalf("encodeRow failed: %v", err)
	}

	if string(got) != "[null]" {
		t.Errorf("got %s, want [null]", got)
	}
}

func TestEncodeRow_Blob_NonUTF8_Base64(t *testing.T) {
	t.Parallel()

	fields := []*gomysql.Field{{Type: gomysql.MYSQL_TYPE_BLOB}}
	row := []gomysql.FieldValue{
		gomysql.NewFieldValue(gomysql.FieldValueTypeString, 0, []byte{0xff, 0xfe, 0x00, 0x01}),
	}

	got, err := encodeRow(fields, row)
	if err != nil {
		t.Fatalf("encodeRow failed: %v", err)
	}

	var decoded []map[string]string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode failed: %v\nraw: %s", err, got)
	}

	if decoded[0]["$type"] != "blob" {
		t.Errorf("expected blob marker, got %v", decoded[0])
	}

	if decoded[0]["$bytes"] == "" {
		t.Errorf("expected base64 bytes, got empty")
	}
}

func TestEncodeRow_Blob_UTF8_String(t *testing.T) {
	t.Parallel()

	fields := []*gomysql.Field{{Type: gomysql.MYSQL_TYPE_BLOB}}
	row := []gomysql.FieldValue{
		gomysql.NewFieldValue(gomysql.FieldValueTypeString, 0, []byte("hello world")),
	}

	got, err := encodeRow(fields, row)
	if err != nil {
		t.Fatalf("encodeRow failed: %v", err)
	}

	if string(got) != `["hello world"]` {
		t.Errorf("got %s, want [\"hello world\"]", got)
	}
}

func TestEncodeRow_JSON_ParsedIfValid(t *testing.T) {
	t.Parallel()

	fields := []*gomysql.Field{{Type: gomysql.MYSQL_TYPE_JSON}}
	row := []gomysql.FieldValue{
		gomysql.NewFieldValue(gomysql.FieldValueTypeString, 0, []byte(`{"k":"v","n":42}`)),
	}

	got, err := encodeRow(fields, row)
	if err != nil {
		t.Fatalf("encodeRow failed: %v", err)
	}

	// Should be parsed (object embedded in array), not a string.
	var decoded []map[string]any
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode failed: %v\nraw: %s", err, got)
	}

	if decoded[0]["k"] != "v" {
		t.Errorf("expected k=v, got %v", decoded[0])
	}
}

func TestEncodeRow_JSON_FallsBackToStringIfInvalid(t *testing.T) {
	t.Parallel()

	fields := []*gomysql.Field{{Type: gomysql.MYSQL_TYPE_JSON}}
	row := []gomysql.FieldValue{
		gomysql.NewFieldValue(gomysql.FieldValueTypeString, 0, []byte(`{not json`)),
	}

	got, err := encodeRow(fields, row)
	if err != nil {
		t.Fatalf("encodeRow failed: %v", err)
	}

	var decoded []string
	if err := json.Unmarshal(got, &decoded); err != nil {
		t.Fatalf("decode failed: %v\nraw: %s", err, got)
	}

	if decoded[0] != `{not json` {
		t.Errorf("expected raw string, got %q", decoded[0])
	}
}
