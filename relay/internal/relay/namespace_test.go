package relay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tunr-dev/tunr/relay/internal/db/dbtest"
)

// A deployed app keeps its hostname even when a tunnel holds the same label.
func TestAppRouteWinsOverTunnel(t *testing.T) {
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = io.WriteString(w, "app")
	}))
	defer app.Close()

	reg := NewRegistry()
	if _, err := reg.Register("other-user", "shop"); err != nil {
		t.Fatal(err)
	}
	store := NewRouteStore()
	store.SetCloud("shop", NewCloudUpstream("a_shop", mustURL(t, app.URL), []byte("s"), nil, nil))
	p := NewProxy(reg, "tunr.test")
	p.SetRoutes(store)

	for _, host := range []string{"shop.tunr.test", "SHOP.tunr.test", "shop.tunr.test."} {
		rec := httptest.NewRecorder()
		p.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "http://"+host+"/", nil))
		if body := rec.Body.String(); body != "app" {
			t.Errorf("%s: got %q, want the app's response", host, body)
		}
	}
}

// The app's upstream is always its own container; extra body fields are ignored.
func TestCreateAppUpstreamIsRelayChosen(t *testing.T) {
	d := dbtest.New(t)
	uid := dbtest.User(t, d, "dev@example.test")
	cp := NewControlPlane(nil, d, "tunr.test", NewRunnerClient("http://127.0.0.1:1", "s"), nil)

	body := `{"name":"web","internal_port":3000,"cloud_url":"http://example.invalid:1"}`
	rec := httptest.NewRecorder()
	cp.createApp(rec, httptest.NewRequest(http.MethodPost, "/v1/apps", strings.NewReader(body)), uid)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body)
	}
	route, ok, err := d.GetCloudRoute(context.Background(), "web")
	if err != nil || !ok {
		t.Fatalf("route: %v %v", ok, err)
	}
	if !strings.HasPrefix(route.CloudURL, "http://tunr-app-"+route.AppID+":3000") {
		t.Fatalf("cloud_url = %q", route.CloudURL)
	}
}
