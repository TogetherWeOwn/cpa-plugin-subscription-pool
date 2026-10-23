package coordinator

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

// fakeManagementModule is a fakeModule that also implements
// providermodule.ManagementCapable, for exercising the coordinator's route
// aggregation and dispatch without any real provider logic.
type fakeManagementModule struct {
	fakeModule
	routes         providermodule.ManagementRoutes
	handled        []pluginapi.ManagementRequest
	handleResponse pluginapi.ManagementResponse
}

func (m *fakeManagementModule) ManagementRoutes(context.Context) providermodule.ManagementRoutes {
	return m.routes
}

func (m *fakeManagementModule) HandleManagement(_ context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	m.handled = append(m.handled, req)
	return m.handleResponse
}

var _ providermodule.ManagementCapable = (*fakeManagementModule)(nil)

func TestManagementRoutesAggregatesCoordinatorAndModuleRoutes(t *testing.T) {
	alpha := &fakeManagementModule{
		fakeModule: fakeModule{id: "alpha"},
		routes: providermodule.ManagementRoutes{
			Routes:    []providermodule.ManagementRoute{{Method: http.MethodGet, Path: "/v0/management/plugins/alpha/status", Description: "alpha status"}},
			Resources: []providermodule.ManagementResource{{Path: "/alpha-status", Menu: "Alpha", Description: "alpha resource"}},
		},
	}
	beta := &fakeModule{id: "beta"}
	c := New(alpha, beta)

	body := c.managementRoutes()

	if len(body.Routes) != 2 {
		t.Fatalf("Routes = %#v, want coordinator's own status route + alpha's route (2 total)", body.Routes)
	}
	foundOwn := false
	foundAlpha := false
	for _, route := range body.Routes {
		if route.Path == managementStatusPath {
			foundOwn = true
		}
		if route.Path == "/v0/management/plugins/alpha/status" {
			foundAlpha = true
		}
	}
	if !foundOwn || !foundAlpha {
		t.Fatalf("Routes = %#v, missing expected entries", body.Routes)
	}
	if len(body.Resources) != 2 {
		t.Fatalf("Resources = %#v, want coordinator's own resource + alpha's resource (2 total)", body.Resources)
	}
	foundOwnResource := false
	foundAlphaResource := false
	for _, resource := range body.Resources {
		if resource.Path == resourceStatusPath {
			foundOwnResource = true
		}
		if resource.Path == "/alpha-status" {
			foundAlphaResource = true
		}
	}
	if !foundOwnResource || !foundAlphaResource {
		t.Fatalf("Resources = %#v, missing expected entries", body.Resources)
	}
}

func TestHandleManagementAnswersCoordinatorStatusRoute(t *testing.T) {
	c := New(&fakeModule{id: "alpha"})
	resp := c.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: managementStatusPath})
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	var body managementStatusBody
	if err := json.Unmarshal(resp.Body, &body); err != nil {
		t.Fatalf("unmarshal body: %v", err)
	}
	if body.Plugin != PluginID {
		t.Fatalf("Plugin = %q, want %q", body.Plugin, PluginID)
	}
}

func TestHandleManagementDispatchesToDeclaringModule(t *testing.T) {
	alpha := &fakeManagementModule{
		fakeModule: fakeModule{id: "alpha"},
		routes: providermodule.ManagementRoutes{
			Routes: []providermodule.ManagementRoute{{Method: http.MethodPost, Path: "/v0/management/plugins/alpha/refresh"}},
		},
		handleResponse: pluginapi.ManagementResponse{StatusCode: http.StatusAccepted},
	}
	c := New(alpha)

	resp := c.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodPost, Path: "/v0/management/plugins/alpha/refresh"})

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("StatusCode = %d, want 202", resp.StatusCode)
	}
	if len(alpha.handled) != 1 || alpha.handled[0].Path != "/v0/management/plugins/alpha/refresh" {
		t.Fatalf("handled = %#v, want exactly the delegated route request", alpha.handled)
	}
}

func TestHandleManagementRewritesResourcePathToModuleRelativePath(t *testing.T) {
	alpha := &fakeManagementModule{
		fakeModule: fakeModule{id: "alpha"},
		routes: providermodule.ManagementRoutes{
			Resources: []providermodule.ManagementResource{{Path: "/alpha-status", Menu: "Alpha"}},
		},
		handleResponse: pluginapi.ManagementResponse{StatusCode: http.StatusOK},
	}
	c := New(alpha)

	resp := c.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: resourcePluginBasePath + "/alpha-status"})

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("StatusCode = %d, want 200", resp.StatusCode)
	}
	if len(alpha.handled) != 1 || alpha.handled[0].Path != "/alpha-status" {
		t.Fatalf("handled = %#v, want req.Path rewritten to module's own relative \"/alpha-status\"", alpha.handled)
	}
}

func TestHandleManagementUnknownRouteAnswers404(t *testing.T) {
	c := New(&fakeModule{id: "alpha"})
	resp := c.handleManagement(context.Background(), pluginapi.ManagementRequest{Method: http.MethodGet, Path: "/v0/management/plugins/alpha/does-not-exist"})
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("StatusCode = %d, want 404", resp.StatusCode)
	}
}

func TestManagementRoutesSkipsModulesWithoutManagementCapable(t *testing.T) {
	c := New(&fakeModule{id: "alpha"})
	body := c.managementRoutes()
	if len(body.Routes) != 1 || body.Routes[0].Path != managementStatusPath {
		t.Fatalf("Routes = %#v, want only the coordinator's own status route", body.Routes)
	}
	if len(body.Resources) != 1 || body.Resources[0].Path != resourceStatusPath {
		t.Fatalf("Resources = %#v, want only the coordinator's own resource", body.Resources)
	}
}
