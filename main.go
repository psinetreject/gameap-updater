//go:build wasip1

// GameAP plugin: update center for the panel, the daemon and gameapctl.
//
// The frontend reads the panel and daemon versions straight from the panel's
// own admin endpoints (/api/version, /api/nodes/summary). The backend adds what
// the panel cannot answer: which gameapctl each node has and its latest
// release, which nodes host a panel, and it runs upgrade jobs (jobs.go) that
// have gameapctl update itself, the daemon or the panel. Every node command goes through the nodecmd host library (the
// node_commands grant) and is built from a fixed template in internal/.
package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	pluginproto "github.com/gameap/gameap/pkg/plugin/proto"
	"github.com/gameap/gameap/pkg/plugin/sdk"
	"github.com/gameap/gameap/pkg/plugin/sdk/nodecmd"

	"github.com/psinetreject/gameap-updater/internal/daemonout"
	"github.com/psinetreject/gameap-updater/internal/gameapctl"
	"github.com/psinetreject/gameap-updater/internal/jobs"
)

// The single source of truth for the plugin version. The frontend reads the
// same file at build time (see frontend/vite.config.ts).
//
//go:embed VERSION
var versionRaw string

var version = strings.TrimSpace(versionRaw)

// The plugin ID has to satisfy two panel constraints, both found the hard way:
//
//   - It must be base32 (alphabet a-z2-7, no padding). A readable slug is
//     accepted at upload but hashed into a number, and /api/plugins/{id}/...
//     then only resolves under that number's base32 form while the path is cut
//     using the form in the URL, so every route misses.
//   - The number it decodes to must be below 2^63. Plugin storage passes it to
//     database/sql, which refuses a uint64 with the high bit set, so every
//     storage call fails ("failed to read storage entry").
//
// This is FNV-64a of "gameap-updater" with the high bit cleared. A store
// listing issues its own ID, which would replace this here and in
// frontend/src/lib/api.ts.
const pluginID = "amfatvdydz4ym"

//go:embed frontend/dist/plugin.js
var frontendBundle []byte

//go:embed frontend/dist/plugin.css
var frontendStyles []byte

func main() {}

func init() {
	pluginproto.RegisterPluginService(&UpdaterPlugin{})
}

// UpdaterPlugin embeds EmptyPluginService so only the methods below need
// implementing; everything else gets a no-op default.
type UpdaterPlugin struct {
	*sdk.EmptyPluginService
}

func (p *UpdaterPlugin) GetInfo(
	_ context.Context,
	_ *pluginproto.GetInfoRequest,
) (*pluginproto.PluginInfo, error) {
	return &pluginproto.PluginInfo{
		Id:          pluginID,
		Name:        "Updater",
		Version:     version,
		Description: "Check and upgrade the GameAP panel, daemons and gameapctl",
		Author:      "psinetreject",
		ApiVersion:  "1",
		// Runs commands as root on every node. Only fixed command templates
		// from internal/gameapctl and internal/jobs are ever sent; nothing a request carries
		// reaches the command string.
		RequiredPermissions: []string{"node_commands"},
	}, nil
}

func (p *UpdaterPlugin) GetFrontendBundle(
	_ context.Context,
	_ *pluginproto.GetFrontendBundleRequest,
) (*pluginproto.GetFrontendBundleResponse, error) {
	return &pluginproto.GetFrontendBundleResponse{
		HasBundle: true,
		Bundle:    frontendBundle,
		HasStyles: true,
		Styles:    frontendStyles,
	}, nil
}

func (p *UpdaterPlugin) GetHTTPRoutes(
	_ context.Context,
	_ *pluginproto.GetHTTPRoutesRequest,
) (*pluginproto.GetHTTPRoutesResponse, error) {
	return &pluginproto.GetHTTPRoutesResponse{
		Routes: []*pluginproto.HTTPRoute{
			{
				Path:         "/status",
				Methods:      []string{http.MethodGet},
				RequiresAuth: true,
				AdminOnly:    true,
				Description:  "Plugin version and capabilities",
			},
			{
				Path:         "/gameapctl/latest",
				Methods:      []string{http.MethodGet},
				RequiresAuth: true,
				AdminOnly:    true,
				Description:  "Latest gameapctl release",
			},
			{
				Path:         "/nodes/{id}/gameapctl",
				Methods:      []string{http.MethodGet},
				RequiresAuth: true,
				AdminOnly:    true,
				Description:  "gameapctl version installed on a node",
			},
			{
				Path:         "/nodes/{id}/daemon",
				Methods:      []string{http.MethodGet},
				RequiresAuth: true,
				AdminOnly:    true,
				Description:  "gameap-daemon binary version on a node",
			},
			{
				Path:         "/nodes/{id}/panel",
				Methods:      []string{http.MethodGet},
				RequiresAuth: true,
				AdminOnly:    true,
				Description:  "GameAP panel binary version on a node, if it hosts one",
			},
			{
				Path:         "/nodes/{id}/panel/upgrade",
				Methods:      []string{http.MethodPost},
				RequiresAuth: true,
				AdminOnly:    true,
				Description:  "Upgrade the GameAP panel on its host node to the latest release",
			},
			{
				Path:         "/nodes/{id}/gameapctl/upgrade",
				Methods:      []string{http.MethodPost},
				RequiresAuth: true,
				AdminOnly:    true,
				Description:  "Update gameapctl on a node to the latest release",
			},
			{
				Path:         "/nodes/{id}/daemon/upgrade",
				Methods:      []string{http.MethodPost},
				RequiresAuth: true,
				AdminOnly:    true,
				Description:  "Upgrade gameap-daemon on a node to the latest release",
			},
			{
				Path:         "/jobs",
				Methods:      []string{http.MethodGet},
				RequiresAuth: true,
				AdminOnly:    true,
				Description:  "Recent upgrade jobs",
			},
			{
				Path:         "/jobs/{id}",
				Methods:      []string{http.MethodGet},
				RequiresAuth: true,
				AdminOnly:    true,
				Description:  "One upgrade job, refreshed from its node",
			},
		},
	}, nil
}

func (p *UpdaterPlugin) HandleHTTPRequest(
	ctx context.Context,
	req *pluginproto.HTTPRequest,
) (*pluginproto.HTTPResponse, error) {
	// The panel has already matched the request against GetHTTPRoutes,
	// method included; this only picks the handler.
	id := req.PathParams["id"]

	switch {
	case req.Path == "/status":
		return jsonResponse(http.StatusOK, statusResponse{
			Plugin:   pluginID,
			Version:  version,
			Upgrades: true,
		}), nil
	case req.Path == "/gameapctl/latest":
		return handleGameapctlLatest(ctx), nil
	case req.Path == "/jobs":
		return handleListJobs(ctx), nil
	case strings.HasPrefix(req.Path, "/jobs/") && id != "":
		return handleGetJob(ctx, id), nil
	case strings.HasSuffix(req.Path, "/gameapctl/upgrade") && id != "":
		return handleStartUpgrade(ctx, components[jobs.ComponentGameapctl], id, sessionUserID(req), wantsUnverified(req)), nil
	case strings.HasSuffix(req.Path, "/daemon/upgrade") && id != "":
		return handleStartUpgrade(ctx, components[jobs.ComponentDaemon], id, sessionUserID(req), wantsUnverified(req)), nil
	case strings.HasSuffix(req.Path, "/panel/upgrade") && id != "":
		return handleStartUpgrade(ctx, components[jobs.ComponentPanel], id, sessionUserID(req), wantsUnverified(req)), nil
	case strings.HasSuffix(req.Path, "/gameapctl") && id != "":
		return handleNodeVersion(ctx, components[jobs.ComponentGameapctl], id), nil
	case strings.HasSuffix(req.Path, "/daemon") && id != "":
		return handleNodeVersion(ctx, components[jobs.ComponentDaemon], id), nil
	case strings.HasSuffix(req.Path, "/panel") && id != "":
		return handleNodeVersion(ctx, components[jobs.ComponentPanel], id), nil
	default:
		return jsonResponse(http.StatusNotFound, errorResponse{Error: "not found"}), nil
	}
}

type latestResponse struct {
	Latest string `json:"latest"`
}

func handleGameapctlLatest(ctx context.Context) *pluginproto.HTTPResponse {
	tag, err := latestGameapctl(ctx)
	if err != nil {
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: "gameapctl release lookup: " + err.Error()})
	}

	return jsonResponse(http.StatusOK, latestResponse{Latest: tag})
}

// nodeVersionResponse answers 200 whether or not the version could be read,
// with Error saying why not, so the page can show one result per node.
// NotInstalled is set instead of Error when the node simply lacks the
// program, which for the panel is the normal case.
type nodeVersionResponse struct {
	NodeID       uint64 `json:"node_id"`
	OS           string `json:"os,omitempty"`
	Version      string `json:"version,omitempty"`
	NotInstalled bool   `json:"not_installed,omitempty"`
	Error        string `json:"error,omitempty"`
}

func handleNodeVersion(ctx context.Context, comp component, rawID string) *pluginproto.HTTPResponse {
	nodeID, ok := parseNodeID(rawID)
	if !ok {
		return jsonResponse(http.StatusBadRequest, errorResponse{Error: "invalid node id"})
	}

	found, err := getNode(ctx, nodeID)
	if err != nil {
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: "node lookup: " + err.Error()})
	}

	if !found.GetFound() || found.GetNode() == nil {
		return jsonResponse(http.StatusNotFound, errorResponse{Error: "node not found"})
	}

	result := nodeVersionResponse{NodeID: nodeID, OS: found.GetNode().GetOs()}

	result.Version, err = comp.current(ctx, nodeID, result.OS)
	switch {
	case gameapctl.IsNotFound(err):
		result.NotInstalled = true
	case err != nil:
		result.Error = err.Error()
	}

	return jsonResponse(http.StatusOK, result)
}

func parseNodeID(raw string) (uint64, bool) {
	id, err := strconv.ParseUint(raw, 10, 64)

	return id, err == nil && id != 0
}

// wantsUnverified reports whether an upgrade request asks to skip release
// verification (?unverified=1).
func wantsUnverified(req *pluginproto.HTTPRequest) bool {
	values := req.GetQueryParams()["unverified"].GetValues()

	return len(values) == 1 && values[0] == "1"
}

// sessionUserID is the signed-in user behind a request, recorded on the jobs
// they start. Admin-only routes always carry a session.
func sessionUserID(req *pluginproto.HTTPRequest) uint64 {
	return req.GetSession().GetUser().GetId()
}

// readGameapctlVersion runs the version command on a node. The command depends
// only on nodeOS, which callers take from the panel's record of the node.
func readGameapctlVersion(ctx context.Context, nodeID uint64, nodeOS string) (string, error) {
	command, err := gameapctl.VersionCommand(nodeOS)
	if err != nil {
		return "", errors.New(err.Error() + ": " + nodeOS)
	}

	out, err := runOnNode(ctx, nodeID, command)
	if err != nil {
		return "", err
	}

	return gameapctl.ParseVersion(out.GetOutput(), out.GetExitCode())
}

// readDaemonVersion asks the gameap-daemon binary on a node for its version.
// The panel knows the version the daemon reported when it connected, but
// plugins cannot read it (the SDK's Node has no version field).
func readDaemonVersion(ctx context.Context, nodeID uint64, nodeOS string) (string, error) {
	command, err := gameapctl.DaemonVersionCommand(nodeOS)
	if err != nil {
		return "", errors.New(err.Error() + ": " + nodeOS)
	}

	out, err := runOnNode(ctx, nodeID, command)
	if err != nil {
		return "", err
	}

	return gameapctl.ParseDaemonVersion(out.GetOutput(), out.GetExitCode())
}

// readPanelVersion asks the GameAP panel binary on a node, if there is one,
// for its version.
func readPanelVersion(ctx context.Context, nodeID uint64, nodeOS string) (string, error) {
	command, err := gameapctl.PanelVersionCommand(nodeOS)
	if err != nil {
		return "", errors.New(err.Error() + ": " + nodeOS)
	}

	out, err := runOnNode(ctx, nodeID, command)
	if err != nil {
		return "", err
	}

	return gameapctl.ParsePanelVersion(out.GetOutput(), out.GetExitCode())
}

// runOnNode sends one command to a node through the daemon. A transport or
// permission failure is an error; the command's own exit status is not, and
// is left to the caller. The output comes back without the daemon's framing.
func runOnNode(ctx context.Context, nodeID uint64, command string) (*nodecmd.ExecuteCommandResponse, error) {
	out, err := nodecmd.NewNodeCmdService().ExecuteCommand(ctx, &nodecmd.ExecuteCommandRequest{
		NodeId:  nodeID,
		Command: command,
	})
	if err != nil {
		return nil, err
	}

	if out.Error != nil {
		return nil, errors.New(*out.Error)
	}

	out.Output = daemonout.Strip(out.Output)

	return out, nil
}

type statusResponse struct {
	Plugin  string `json:"plugin"`
	Version string `json:"version"`
	// Upgrades reports whether this build can run upgrades at all. The
	// frontend hides every upgrade control while it is false.
	Upgrades bool `json:"upgrades"`
}

type errorResponse struct {
	Error string `json:"error"`
}

func jsonResponse(status int, v any) *pluginproto.HTTPResponse {
	body, err := json.Marshal(v)
	if err != nil {
		status = http.StatusInternalServerError
		body = []byte(`{"error":"encode response"}`)
	}

	return &pluginproto.HTTPResponse{
		StatusCode: int32(status), //nolint:gosec
		Headers:    map[string]string{"Content-Type": "application/json"},
		Body:       body,
	}
}
