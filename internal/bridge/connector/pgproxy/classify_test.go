package pgproxy

import (
	"strings"
	"testing"
)

func TestClassify(t *testing.T) {
	allow := []string{
		"SELECT 1",
		"select * from fatura",
		"SELECT count(*) FROM public.fatura WHERE tarih > now() - interval '30 days'",
		"  -- leading comment\n SELECT 1",
		"/* block */ SELECT 1",
		"/* nested /* comment */ still */ SELECT 1",
		"WITH t AS (SELECT * FROM stok) SELECT * FROM t",
		"WITH RECURSIVE r(n) AS (VALUES (1) UNION ALL SELECT n+1 FROM r WHERE n < 5) SELECT * FROM r",
		"VALUES (1), (2)",
		"TABLE fatura",
		"SHOW search_path",
		"SHOW ALL",
		"EXPLAIN SELECT * FROM fatura",
		"EXPLAIN (FORMAT JSON) SELECT 1",
		"BEGIN", "BEGIN READ ONLY", "START TRANSACTION ISOLATION LEVEL REPEATABLE READ", "COMMIT", "END", "ROLLBACK", "ABORT",
		"SAVEPOINT a", "RELEASE SAVEPOINT a", "ROLLBACK TO SAVEPOINT a",
		"SET search_path TO public", "SET SESSION statement_timeout = '5s'", "SET LOCAL lock_timeout = 1000",
		"SET TIME ZONE 'UTC'", "SET application_name = 'agent'", "RESET statement_timeout",
		"SELECT 'DELETE FROM fatura'",                         // keyword inside a string
		"SELECT 1 -- ; DROP TABLE fatura",                     // inside a comment
		`SELECT "update", "delete" FROM t`,                    // quoted identifiers
		"SELECT $$ DROP TABLE x $$",                           // dollar-quoted
		"SELECT $tag$ ; INSERT INTO x $tag$",                  // tagged dollar quote with ;
		"SELECT E'it\\'s ; DELETE'",                           // E-string with escaped quote
		"SELECT 'it''s ; DELETE'",                             // doubled quote
		"SELECT role, owner, comment, share, lock FROM users", // common column names
		"SELECT * FROM t WHERE id = $1",                       // positional parameter
		"DECLARE c CURSOR FOR SELECT * FROM fatura", "FETCH 100 FROM c", "CLOSE c", "MOVE NEXT IN c",
		"SELECT 1; SELECT 2",
		";",
		"",
		"(SELECT 1) UNION (SELECT 2)",
		"SELECT current_setting('search_path')",
		"SELECT n.nspname FROM pg_catalog.pg_namespace n", // psql \dn
	}
	deny := []string{
		"INSERT INTO fatura VALUES (1)",
		"insert into fatura values (1)",
		"UPDATE fatura SET tutar = 0",
		"DELETE FROM fatura",
		"MERGE INTO t USING s ON t.id = s.id WHEN MATCHED THEN DELETE",
		"TRUNCATE fatura",
		"COPY fatura FROM '/tmp/x'",
		"COPY fatura TO STDOUT",
		"COPY (SELECT 1) TO PROGRAM 'id'",
		"CREATE TABLE x (id int)",
		"CREATE TEMP TABLE x AS SELECT 1",
		"DROP TABLE fatura",
		"ALTER TABLE fatura ADD COLUMN x int",
		"ALTER ROLE agent SUPERUSER",
		"GRANT ALL ON fatura TO public",
		"REVOKE SELECT ON fatura FROM x",
		"DO $$ BEGIN DELETE FROM fatura; END $$",
		"DO 'BEGIN NULL; END'",
		"CALL do_things()",
		"SELECT pg_terminate_backend(123)",
		"SELECT PG_TERMINATE_BACKEND (123)",
		"SELECT set_config('default_transaction_read_only', 'off', false)",
		"SELECT pg_read_file('/etc/passwd')",
		"SELECT lo_import('/etc/passwd')",
		"SELECT nextval('seq')",
		"SELECT pg_sleep(100)",
		"SELECT query_to_xml('delete from fatura', true, true, '')",
		"SELECT dblink_exec('host=x', 'drop table y')",
		"SELECT * FROM fatura FOR UPDATE",
		"SELECT * FROM fatura FOR NO KEY UPDATE",
		"SELECT * FROM fatura FOR SHARE",
		"SELECT * FROM fatura FOR KEY SHARE",
		"WITH x AS (DELETE FROM fatura RETURNING *) SELECT * FROM x",
		"WITH x AS (UPDATE fatura SET tutar = 0 RETURNING *) SELECT 1",
		"WITH x AS (INSERT INTO t VALUES (1) RETURNING id) SELECT * FROM x",
		"SELECT * INTO newtable FROM fatura",
		"SELECT 1; DROP TABLE fatura",
		"SELECT 1;DROP TABLE fatura",
		"SELECT 1 /* x */; /* y */ DELETE FROM fatura",
		"EXPLAIN ANALYZE DELETE FROM fatura",
		"EXPLAIN ANALYZE SELECT 1",
		"EXPLAIN CREATE TABLE x AS SELECT 1",
		"BEGIN READ WRITE",
		"START TRANSACTION READ WRITE",
		"SET default_transaction_read_only = off",
		"SET SESSION default_transaction_read_only TO off",
		"SET transaction_read_only = off",
		"SET TRANSACTION READ WRITE",
		"SET SESSION CHARACTERISTICS AS TRANSACTION READ WRITE",
		"SET ROLE postgres",
		"SET SESSION AUTHORIZATION postgres",
		"RESET ALL",
		"RESET default_transaction_read_only",
		"PREPARE p AS DELETE FROM fatura",
		"EXECUTE p",
		"DEALLOCATE p",
		"LISTEN ch",
		"NOTIFY ch",
		"VACUUM fatura",
		"ANALYZE fatura",
		"LOCK TABLE fatura",
		"REFRESH MATERIALIZED VIEW v",
		"CLUSTER fatura",
		"REINDEX TABLE fatura",
		"DISCARD ALL",
		"COMMENT ON TABLE fatura IS 'x'",
		"SECURITY LABEL ON TABLE t IS 'x'",
		"LOAD 'plpgsql'",
		"CHECKPOINT",
		"IMPORT FOREIGN SCHEMA x FROM SERVER s INTO public",
		"SELECT 'unterminated",
		"SELECT $$ unterminated",
		"/* unterminated comment SELECT 1",
		`SELECT "unterminated`,
		"(DELETE FROM fatura)",
		"SeLeCt 1; dElEtE fRoM fatura",
		"DECLARE c CURSOR FOR DELETE FROM fatura RETURNING *",
		"SELECT 1 \n;\n UPDATE x SET y = 1",
	}
	for _, q := range allow {
		if v := Classify(q); !v.Allowed {
			t.Errorf("should allow %q: %s", q, v.Reason)
		}
	}
	for _, q := range deny {
		if v := Classify(q); v.Allowed {
			t.Errorf("should deny %q", q)
		} else if v.Reason == "" {
			t.Errorf("deny %q without a reason", q)
		}
	}
	if n := len(allow) + len(deny); n < 120 {
		t.Errorf("only %d cases", n)
	}
}

func TestClassifyAudit(t *testing.T) {
	v := Classify(`SELECT f.id FROM public.fatura f JOIN "Stok" s ON s.id = f.id`)
	if v.Command != "SELECT" || strings.Join(v.Tables, ",") != "public.fatura,Stok" {
		t.Errorf("verdict = %+v", v)
	}
	for q, want := range map[string]string{
		"delete from fatura":                 "fatura",
		"update public.fatura set tutar = 0": "public.fatura",
		"insert into stok values (1)":        "stok",
		"TABLE fatura":                       "fatura",
	} {
		if v := Classify(q); strings.Join(v.Tables, ",") != want {
			t.Errorf("%q tables = %v, want %s", q, v.Tables, want)
		}
	}
}

func FuzzClassify(f *testing.F) {
	for _, s := range []string{"SELECT 1", "DELETE FROM x", "SELECT $$x$$", "SELECT 'a''b'", "/* /* */ */ SELECT 1"} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, sql string) {
		v := Classify(sql) // must never panic
		// Anything allowed must not contain a top-level write when appended
		// to a known-bad suffix: cheap sanity — an allowed verdict always has a command.
		if v.Allowed && v.Command == "" {
			t.Fatalf("allowed without command: %q", sql)
		}
	})
}
