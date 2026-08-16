package store

import (
	"reflect"
	"testing"
)

func TestMySQLQuery_RepeatsArgumentsForRepeatedPlaceholder(t *testing.T) {
	gotQuery, gotArgs, err := mysqlQuery(
		`SELECT * FROM streamer_claims WHERE ($1 = '' OR status = $1) AND account_id = $2`,
		[]any{"pending", int64(42)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if want := `SELECT * FROM streamer_claims WHERE (? = '' OR status = ?) AND account_id = ?`; gotQuery != want {
		t.Fatalf("query = %q, want %q", gotQuery, want)
	}
	if want := []any{"pending", "pending", int64(42)}; !reflect.DeepEqual(gotArgs, want) {
		t.Fatalf("args = %#v, want %#v", gotArgs, want)
	}
}

func TestMySQLQuery_RejectsMissingArgument(t *testing.T) {
	_, _, err := mysqlQuery(`SELECT $2`, []any{"only-one"})
	if err == nil {
		t.Fatal("missing argument placeholder must fail")
	}
}
