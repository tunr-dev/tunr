package profile

import (
	"strings"
	"testing"
	"time"

	"github.com/tunr-dev/tunr/internal/bridge/connector/mssqlq"
)

func TestProfilesLoad(t *testing.T) {
	if err := LoadError(); err != nil {
		t.Fatal(err)
	}
	all := All()
	if len(all) < 3 {
		t.Fatalf("want the three ERP profiles, got %d", len(all))
	}
	for _, p := range all {
		if p.Title == "" || p.Summary == "" || len(p.Tables) == 0 || len(p.Detect) == 0 || len(p.Sources) == 0 {
			t.Errorf("%s: incomplete profile", p.Name)
		}
		for _, tb := range p.Tables {
			switch tb.Confidence {
			case "confirmed", "likely", "unverified":
			default:
				t.Errorf("%s.%s: confidence %q", p.Name, tb.Name, tb.Confidence)
			}
			if len(tb.Columns) == 0 {
				t.Errorf("%s.%s: no columns", p.Name, tb.Name)
			}
		}
		for _, q := range p.Queries {
			// Every example must pass the connector's read-only filter as written.
			if p.Kind == "mssql" {
				if v := mssqlq.Classify(q.SQL); !v.Allowed {
					t.Errorf("%s: example %q refused by the classifier: %s", p.Name, q.Title, v.Reason)
				}
			}
		}
	}
}

func TestMatchAndDescribe(t *testing.T) {
	logo, ok := Get("logo-tiger")
	if !ok {
		t.Fatal("no logo-tiger")
	}
	present := []string{"dbo.LG_001_CLCARD", "LG_001_ITEMS", "LG_001_01_INVOICE", "LG_001_02_INVOICE", "LG_002_CLCARD", "LG_001_01_STLINEX"}
	if !logo.Detected(present) {
		t.Error("logo not detected")
	}
	m := logo.Match(present)
	if got := strings.Join(m["LG_{FIRM}_{PERIOD}_INVOICE"], ","); got != "LG_001_01_INVOICE,LG_001_02_INVOICE" {
		t.Errorf("invoice matches = %q", got)
	}
	if got := strings.Join(m["LG_{FIRM}_CLCARD"], ","); got != "dbo.LG_001_CLCARD,LG_002_CLCARD" {
		t.Errorf("clcard matches = %q", got)
	}
	if len(m["LG_{FIRM}_{PERIOD}_STLINE"]) != 0 {
		t.Error("STLINEX must not match STLINE")
	}

	unverified := logo.Describe(nil, nil)
	if unverified.Verified || !strings.Contains(unverified.Warning, "NOT been checked") || len(unverified.Tables) != len(logo.Tables) {
		t.Errorf("unverified describe = %+v", unverified)
	}
	now := time.Now()
	d := logo.Describe(present, &now)
	if !d.Verified || len(d.NotFound) == 0 || len(d.Tables)+len(d.NotFound) != len(logo.Tables) {
		t.Errorf("verified describe: %d tables, %d not found", len(d.Tables), len(d.NotFound))
	}
	if nebim, _ := Get("nebim-v3"); nebim.Detected(present) {
		t.Error("a Logo database must not look like Nebim")
	}
}
