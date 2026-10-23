package coordinator

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v7/sdk/pluginapi"

	"github.com/TogetherWeOwn/cpa-plugin-subscription-pool/internal/providermodule"
)

const (
	managementStatusPath  = "/v0/management/plugins/" + PluginID + "/status"
	managementContentType = "application/json"
	resourceStatusPath    = "/status"
	resourceContentType   = "text/html; charset=utf-8"
)

// ManagementRegistration is the exported entry point src/'s CGo ABI glue
// calls to answer a management.register RPC. It aggregates the
// coordinator's own status route with every hosted module's
// ManagementCapable routes, verbatim (no path rewriting in this slice — see
// providermodule.ManagementRoute).
func (c *Coordinator) ManagementRegistration() managementRoutesBody {
	return c.managementRoutes()
}

func (c *Coordinator) managementRoutes() managementRoutesBody {
	body := managementRoutesBody{
		Routes: []managementRoute{
			{Method: http.MethodGet, Path: managementStatusPath, Description: "Reports aggregated subscription-pool status across every provider."},
			{Method: http.MethodGet, Path: managementUsagePath, Description: "Reports request, token and error history per provider, account and model (?range=1h|6h|24h|7d) plus quota, for the usage page."},
		},
		Resources: []managementResource{
			{Path: resourceStatusPath, Menu: usagePageMenu, Description: "Usage and quota for Z.ai, OpenCode Go and synthetic.new: requests, tokens, error rates, models, quota bars with reset times, infinite mode and concurrency."},
			{Path: resourceUsageDataPath, Description: "Redacted JSON data behind the usage page (?range=1h|6h|24h|7d)."},
		},
	}
	for _, entry := range c.modules {
		capable, ok := entry.module.(providermodule.ManagementCapable)
		if !ok {
			continue
		}
		routes := capable.ManagementRoutes(context.Background())
		for _, route := range routes.Routes {
			body.Routes = append(body.Routes, managementRoute{Method: route.Method, Path: route.Path, Description: route.Description})
		}
		for _, resource := range routes.Resources {
			body.Resources = append(body.Resources, managementResource{Path: resource.Path, Menu: resource.Menu, Description: resource.Description})
		}
	}
	return body
}

type managementRoutesBody struct {
	Routes    []managementRoute    `json:"routes"`
	Resources []managementResource `json:"resources,omitempty"`
}

type managementRoute struct {
	Method      string `json:"method"`
	Path        string `json:"path"`
	Description string `json:"description,omitempty"`
}

type managementResource struct {
	Path        string `json:"path"`
	Menu        string `json:"menu"`
	Description string `json:"description,omitempty"`
}

// resourcePluginBasePath mirrors CLIProxyAPI's internal/pluginhost resource
// route prefix convention: a module's own relative resource path (e.g.
// "/status") is dispatched by the host as
// "/v0/resource/plugins/<registered-plugin-id>/status" — and since this
// plugin registers as PluginID, that is the prefix every module's resource
// path resolves under, regardless of the module's own internal ID.
const resourcePluginBasePath = "/v0/resource/plugins/" + PluginID

// HandleManagement is the exported entry point src/'s CGo ABI glue calls to
// answer a management.handle RPC.
func (c *Coordinator) HandleManagement(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	return c.handleManagement(ctx, req)
}

// handleManagement dispatches req to whichever module declared a matching
// route, or answers the coordinator's own aggregated status route. A path no
// module declared, and that isn't the coordinator's own status route,
// answers 404 rather than silently falling through.
//
// For a resource-route match, req.Path is rewritten to the module's own
// declared (relative) path before delegating: a module never sees the
// coordinator's PluginID baked into a request path, keeping it decoupled
// from the coordinator's own identity exactly as it is at declaration time.
func (c *Coordinator) handleManagement(ctx context.Context, req pluginapi.ManagementRequest) pluginapi.ManagementResponse {
	if req.Method == http.MethodGet && req.Path == managementStatusPath {
		return managementJSON(http.StatusOK, c.managementStatus())
	}
	if req.Method == http.MethodGet && req.Path == managementUsagePath {
		return c.usageDataResponse(req)
	}
	if req.Method == http.MethodGet && isResourceStatusPath(req.Path) {
		return c.usagePageResponse(req)
	}
	if req.Method == http.MethodGet && isResourceUsageDataPath(req.Path) {
		return c.usageDataResponse(req)
	}
	for _, entry := range c.modules {
		capable, ok := entry.module.(providermodule.ManagementCapable)
		if !ok {
			continue
		}
		routes := capable.ManagementRoutes(ctx)
		for _, route := range routes.Routes {
			if route.Method == req.Method && route.Path == req.Path {
				return capable.HandleManagement(ctx, req)
			}
		}
		for _, resource := range routes.Resources {
			if req.Method != http.MethodGet {
				continue
			}
			if req.Path != resource.Path && req.Path != resourcePluginBasePath+resource.Path {
				continue
			}
			delegated := req
			delegated.Path = resource.Path
			return capable.HandleManagement(ctx, delegated)
		}
	}
	return managementError(http.StatusNotFound, "not_found", "unknown management route")
}

func managementJSON(status int, value any) pluginapi.ManagementResponse {
	body, err := json.Marshal(value)
	if err != nil {
		return managementError(http.StatusInternalServerError, "encode_failed", "could not encode management response")
	}
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{managementContentType}},
		Body:       body,
	}
}

func managementError(status int, code, message string) pluginapi.ManagementResponse {
	body, _ := json.Marshal(managementErrorBody{Error: managementErrorDetail{Code: code, Message: message}})
	return pluginapi.ManagementResponse{
		StatusCode: status,
		Headers:    http.Header{"Content-Type": []string{managementContentType}},
		Body:       body,
	}
}

type managementErrorBody struct {
	Error managementErrorDetail `json:"error"`
}

type managementErrorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
