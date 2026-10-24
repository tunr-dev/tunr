package connector

import (
	"strings"
	"testing"
)

func TestMSSQLRoleSQL(t *testing.T) {
	pw, err := GenerateMSSQLPassword()
	if err != nil {
		t.Fatal(err)
	}
	all, err := MSSQLRoleSpec{Login: "tunr_agent_ro", Password: pw, Database: "NEBIM_V3"}.SQL()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"CREATE LOGIN [tunr_agent_ro] WITH PASSWORD = N'" + pw + "', CHECK_POLICY = ON",
		"USE [NEBIM_V3];",
		"ALTER ROLE [db_datareader] ADD MEMBER [tunr_agent_ro];",
		"DENY INSERT, UPDATE, DELETE, EXECUTE, ALTER ON DATABASE::[NEBIM_V3] TO [tunr_agent_ro];",
		"DENY UNMASK TO [tunr_agent_ro];",
	} {
		if !strings.Contains(all, want) {
			t.Errorf("SQL missing %q:\n%s", want, all)
		}
	}
	if strings.Contains(all, "MASKED") {
		t.Error("no masks requested, none expected")
	}

	m, err := ParseMSSQLMask(`dbo.cdCurrAcc.IdentityNum=partial(0,"*******",4)`)
	if err != nil {
		t.Fatal(err)
	}
	some, err := MSSQLRoleSpec{Login: "ro", Password: pw, Database: "TIGER", Tables: []string{"LG_001_CLCARD", "sales.Orders"},
		Masks: []MSSQLMask{m, {Table: "LG_001_CLCARD", Column: "EMAILADDR", Function: "email()"}}}.SQL()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		"GRANT SELECT ON [dbo].[LG_001_CLCARD] TO [ro];",
		"GRANT SELECT ON [sales].[Orders] TO [ro];",
		`ALTER TABLE [dbo].[cdCurrAcc] ALTER COLUMN [IdentityNum] ADD MASKED WITH (FUNCTION = 'partial(0,"*******",4)');`,
		`ALTER TABLE [dbo].[LG_001_CLCARD] ALTER COLUMN [EMAILADDR] ADD MASKED WITH (FUNCTION = 'email()');`,
		"WARNING: masking is a property of the COLUMN",
	} {
		if !strings.Contains(some, want) {
			t.Errorf("SQL missing %q:\n%s", want, some)
		}
	}
	if strings.Contains(some, "db_datareader") {
		t.Error("table list given: db_datareader must not be used")
	}
}

func TestMSSQLRoleRejectsInjection(t *testing.T) {
	pw, _ := GenerateMSSQLPassword()
	for _, s := range []MSSQLRoleSpec{
		{Login: "ro]; DROP LOGIN sa; --", Password: pw, Database: "db"},
		{Login: "ro", Password: pw, Database: "db]"},
		{Login: "ro", Password: "short", Database: "db"},
		{Login: "ro", Password: pw + "'", Database: "db"},
		{Login: "ro", Password: pw, Database: "db", Tables: []string{"t]; DROP TABLE x; --"}},
		{Login: "ro", Password: pw, Database: "db", Masks: []MSSQLMask{{Table: "t", Column: "c", Function: "partial(0,\"x'); DROP TABLE t; --\",4)"}}},
	} {
		if _, err := s.SQL(); err == nil {
			t.Errorf("accepted %+v", s)
		}
	}
	for _, bad := range []string{"c", "a.b.c.d", `t.c=partial(0,'x',4)`, "t.c=exec()", `t.c=partial(0,"]",4)`} {
		if _, err := ParseMSSQLMask(bad); err == nil {
			t.Errorf("ParseMSSQLMask(%q) accepted", bad)
		}
	}
	for i := 0; i < 50; i++ {
		p, _ := GenerateMSSQLPassword()
		if !strings.ContainsAny(p, "23456789") || strings.ToLower(p) == p || strings.ToUpper(p) == p {
			t.Fatalf("password %q does not meet the Windows policy", p)
		}
	}
}

func TestParseMaskRule(t *testing.T) {
	for in, want := range map[string]MaskRule{
		"vergi_no":                {Column: "vergi_no"},
		"cari.vergi_no=partial:4": {Table: "cari", Column: "vergi_no", Strategy: "partial", KeepLast: 4},
		"public.cari.eposta=hash": {Schema: "public", Table: "cari", Column: "eposta", Strategy: "hash"},
		"bordro.maas=null":        {Table: "bordro", Column: "maas", Strategy: "null"},
	} {
		got, err := ParseMaskRule(in)
		if err != nil || got != want {
			t.Errorf("ParseMaskRule(%q) = %+v, %v", in, got, err)
		}
	}
	for _, bad := range []string{"", "a.b.c.d", "c=shuffle", "c=hash:3", "c=partial:99", "x;drop.c", "t.c=partial:x"} {
		if _, err := ParseMaskRule(bad); err == nil {
			t.Errorf("ParseMaskRule(%q) accepted", bad)
		}
	}
	cfg := &Config{Relay: "https://bridge.tunr.sh", Site: "hq-site", RelayKey: "ed25519:" + strings.Repeat("A", 43),
		Resources: []Resource{{Name: "nebim", Kind: "mssql", Target: "10.0.0.7:1433", Database: "N", Masks: []MaskRule{{Column: "x"}}}}}
	if err := cfg.Validate(); err == nil || !strings.Contains(err.Error(), "mssql-role --mask") {
		t.Errorf("masks on mssql should point at DDM: %v", err)
	}
}
