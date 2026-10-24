package mssqlq

import (
	"strings"
	"testing"
)

func TestClassifyAllows(t *testing.T) {
	for _, q := range []string{
		"SELECT 1",
		"select ItemCode, Qty1 from dbo.trInvoiceLine where InvoiceHeaderID = 'x'",
		"SELECT TOP 10 * FROM [dbo].[cdCurrAcc] WITH (NOLOCK) ORDER BY CurrAccCode",
		"WITH s AS (SELECT ItemCode, SUM(Qty1) q FROM trStock GROUP BY ItemCode) SELECT * FROM s WHERE q > 0",
		"SELECT * FROM LG_001_CLCARD ORDER BY CODE OFFSET 0 ROWS FETCH NEXT 50 ROWS ONLY",
		"(SELECT 1) UNION ALL (SELECT 2)",
		"SELECT N'İstanbul' AS city, 'it''s' AS s -- DELETE in a comment\n",
		"SELECT /* DROP /* nested */ TABLE */ 1",
		"SELECT [Update], \"Delete\" FROM t", // quoted identifiers are names, not keywords
		"SELECT c.CODE, f.TOTAL FROM LG_001_CLCARD c JOIN LG_001_01_INVOICE f ON f.CLIENTREF = c.LOGICALREF",
		"SELECT @@VERSION",
		"SELECT * FROM t FOR JSON PATH",
		"SELECT 1; SELECT 2;",
	} {
		if v := Classify(q); !v.Allowed {
			t.Errorf("Classify(%q) denied: %s", q, v.Reason)
		}
	}
}

func TestClassifyDenies(t *testing.T) {
	for _, q := range []string{
		"",
		"DELETE FROM t",
		"SELECT 1 DELETE FROM t",          // no semicolon needed in T-SQL
		"SELECT * INTO #copy FROM cdItem", // SELECT INTO creates a table
		"EXEC sp_who",
		"SELECT 1; EXEC('DROP TABLE t')",
		"SELECT * FROM OPENROWSET('SQLNCLI', 'Server=x;', 'SELECT 1')",
		"SELECT * FROM OPENQUERY(LINKED, 'SELECT 1')",
		"SELECT * FROM srv.db.dbo.t",
		"SELECT * FROM otherdb.dbo.t",
		"SELECT * FROM otherdb..t",
		"WITH x AS (SELECT 1 a) UPDATE t SET a = 1",
		"SELECT NEXT VALUE FOR dbo.seq",
		"SELECT 1 WAITFOR DELAY '00:01:00'",
		"DECLARE @s nvarchar(100) = 'x'",
		"SET LOCK_TIMEOUT 0",
		"USE master",
		"SELECT * FROM t; SHUTDOWN",
		"SELECT xp_cmdshell 'dir'",
		"SELECT [xp_cmdshell]('dir')",
		"SELECT 'unterminated",
		"SELECT [unterminated",
		"SELECT /* open comment",
		"BEGIN TRAN SELECT 1",
		"SELECT * FROM fn_dblog(NULL, NULL)",
		"MERGE t USING s ON 1=1 WHEN MATCHED THEN DELETE;",
		"SELECT 1\x00",
		"TRUNCATE TABLE t",
		"UPDATE t SET a=1 OUTPUT inserted.*",
	} {
		if v := Classify(q); v.Allowed {
			t.Errorf("Classify(%q) allowed", q)
		}
	}
}

func TestClassifyTables(t *testing.T) {
	v := Classify("SELECT * FROM [dbo].[cdCurrAcc] c JOIN dbo.trInvoiceHeader h ON h.CurrAccCode = c.CurrAccCode CROSS APPLY fn(x) a")
	got := strings.Join(v.Tables, ",")
	if got != "dbo.cdCurrAcc,dbo.trInvoiceHeader,fn" {
		t.Errorf("tables = %q", got)
	}
	if v := Classify("DELETE FROM cdItem"); len(v.Tables) != 1 || v.Tables[0] != "cdItem" {
		t.Errorf("refused statement should still name its table: %v", v.Tables)
	}
}
