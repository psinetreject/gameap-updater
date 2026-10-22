//go:build wasip1

package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	pluginproto "github.com/gameap/gameap/pkg/plugin/proto"
	"github.com/gameap/gameap/pkg/plugin/sdk/nodes"
	"github.com/gameap/gameap/pkg/plugin/sdk/storage"

	"github.com/psinetreject/gameap-updater/internal/gameapctl"
	"github.com/psinetreject/gameap-updater/internal/jobs"
)

// Jobs live in plugin storage, not memory: the plugin can be reloaded (and,
// once panel upgrades exist, is replaced) while a job runs on a node, and the
// next request has to find it again.
const jobKeyPrefix = "job:"

// How many log lines a finished job keeps.
const jobLogLines = 40

func saveJob(ctx context.Context, job *jobs.Job) error {
	payload, err := json.Marshal(job)
	if err != nil {
		return err
	}

	resp, err := storage.NewStorageService().Set(ctx, &storage.StorageSetRequest{
		Key:     jobKeyPrefix + job.ID,
		Payload: payload,
	})
	if err != nil {
		return err
	}

	if !resp.GetSuccess() {
		return errors.New("storage: " + resp.GetError())
	}

	return nil
}

func loadJob(ctx context.Context, id string) (*jobs.Job, error) {
	resp, err := storage.NewStorageService().Get(ctx, &storage.StorageGetRequest{Key: jobKeyPrefix + id})
	if err != nil {
		return nil, err
	}

	if resp.Error != nil {
		return nil, errors.New("storage: " + *resp.Error)
	}

	if !resp.GetFound() {
		return nil, nil
	}

	var job jobs.Job
	if err := json.Unmarshal(resp.GetPayload(), &job); err != nil {
		return nil, err
	}

	return &job, nil
}

// listJobs returns every stored job, newest first.
func listJobs(ctx context.Context) ([]*jobs.Job, error) {
	// Non-nil so an empty list encodes as [] rather than null.
	all := []*jobs.Job{}

	prefix := jobKeyPrefix
	for offset := uint32(0); ; {
		limit := uint32(100)
		resp, err := storage.NewStorageService().List(ctx, &storage.StorageListRequest{
			KeyPrefix: &prefix,
			Limit:     &limit,
			Offset:    &offset,
		})
		if err != nil {
			return nil, err
		}

		if resp.Error != nil {
			return nil, errors.New("storage: " + *resp.Error)
		}

		for _, entry := range resp.GetEntries() {
			var job jobs.Job
			if err := json.Unmarshal(entry.GetPayload(), &job); err == nil {
				all = append(all, &job)
			}
		}

		if !resp.GetHasMore() {
			break
		}

		offset += uint32(len(resp.GetEntries())) //nolint:gosec
	}

	sort.Slice(all, func(i, k int) bool { return all[i].StartedAt > all[k].StartedAt })

	return all, nil
}

// newJobID returns an ID no stored job has.
//
// crypto/rand alone is not enough here: the panel instantiates plugins without
// a random source (no wazero WithRandSource), and wazero's default is
// deterministic, so every load of the plugin replays the same "random" bytes.
// IDs repeated after each reload and overwrote earlier jobs. The wall clock is
// real (the panel sets WithSysWalltime), so it is hashed in, and storage is
// checked in case two IDs still meet.
func newJobID(ctx context.Context) (string, error) {
	for range 5 {
		seed := make([]byte, 16)
		if _, err := rand.Read(seed); err != nil {
			return "", err
		}

		now := time.Now().UnixNano()
		for i := range 8 {
			seed = append(seed, byte(now>>(8*i)))
		}

		sum := sha256.Sum256(seed)

		id, err := jobs.NewID(bytes.NewReader(sum[:]))
		if err != nil {
			return "", err
		}

		existing, err := loadJob(ctx, id)
		if err != nil {
			return "", err
		}

		if existing == nil {
			return id, nil
		}
	}

	return "", errors.New("could not find an unused job id")
}

// releaseToVerify finds the archive of comp in r for the node's CPU, with the
// SHA-256 GitHub lists for it. A release without that archive, or without a
// digest for it, is refused: the job would have nothing to check.
func releaseToVerify(ctx context.Context, nodeID uint64, comp component, r release) (jobs.Verify, int, error) {
	out, err := runOnNode(ctx, nodeID, jobs.ArchCommand)
	if err != nil {
		return jobs.Verify{}, http.StatusBadGateway, errors.New("reading the node's CPU: " + err.Error())
	}

	arch, err := jobs.Arch(out.GetOutput())
	if err != nil {
		return jobs.Verify{}, http.StatusConflict, errors.New(err.Error() + ": " + strings.TrimSpace(out.GetOutput()))
	}

	asset := jobs.AssetName(comp.binary, r.tag, arch)

	digest, ok := r.digests[asset]
	if !ok {
		return jobs.Verify{}, http.StatusConflict, errors.New(r.tag + " has no " + asset + " with a checksum on GitHub")
	}

	v, err := jobs.NewVerify(comp.repo, comp.binary, r.tag, arch, digest)
	if err != nil {
		return jobs.Verify{}, http.StatusBadGateway, errors.New(asset + ": " + err.Error())
	}

	return v, 0, nil
}

func getNode(ctx context.Context, nodeID uint64) (*nodes.GetNodeResponse, error) {
	return nodes.NewNodesService().GetNode(ctx, &nodes.GetNodeRequest{Id: nodeID})
}

// component is what a job needs to know about the thing it upgrades: where
// the target comes from, how to read what a node has, and the gameapctl
// arguments that do the upgrade.
type component struct {
	name string
	// repo is the GitHub repository it is released from, and binary both
	// the program's name and the prefix of its release archives.
	repo    string
	binary  string
	current func(ctx context.Context, nodeID uint64, nodeOS string) (string, error)
	args    func(target string) ([]string, error)
}

var components = map[string]component{
	jobs.ComponentGameapctl: {
		name:    jobs.ComponentGameapctl,
		repo:    gameapctlRepo,
		binary:  "gameapctl",
		current: readGameapctlVersion,
		args:    jobs.GameapctlSelfUpdateArgs,
	},
	jobs.ComponentDaemon: {
		name:    jobs.ComponentDaemon,
		repo:    daemonRepo,
		binary:  "gameap-daemon",
		current: readDaemonVersion,
		args:    jobs.DaemonUpgradeArgs,
	},
	// The panel is upgraded on whichever node hosts it, which only the node
	// can tell: the plugin runs inside the panel but has no way to ask it
	// which machine it is on. The page marks the node whose binary matches
	// the running version.
	jobs.ComponentPanel: {
		name:    jobs.ComponentPanel,
		repo:    panelRepo,
		binary:  "gameap",
		current: readPanelVersion,
		args:    jobs.PanelUpgradeArgs,
	},
}

// handleStartUpgrade starts an upgrade of comp on one node. The request
// carries nothing but the node ID and whether to skip verification: the
// target is the latest release and the command comes from internal/jobs.
//
// Skipping verification is only accepted right after a job for the same
// component and target on that node stopped because the node could not
// verify, so it is an answer to that failure rather than a way around the
// check.
func handleStartUpgrade(ctx context.Context, comp component, rawID string, actorID uint64, unverified bool) *pluginproto.HTTPResponse {
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

	nodeOS := found.GetNode().GetOs()
	if nodeOS != "linux" {
		return jsonResponse(http.StatusConflict, errorResponse{Error: "upgrades are not supported on " + nodeOS + " nodes yet"})
	}

	all, err := listJobs(ctx)
	if err != nil {
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: "job list: " + err.Error()})
	}

	var previous *jobs.Job // the newest job for comp on this node; all is newest first

	for _, job := range all {
		if job.NodeID == nodeID && !job.Terminal() {
			return jsonResponse(http.StatusConflict, errorResponse{Error: "job " + job.ID + " is already running on this node"})
		}

		if previous == nil && job.NodeID == nodeID && job.Component == comp.name {
			previous = job
		}
	}

	latest, err := latestRelease(ctx, comp.repo)
	if err != nil {
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: comp.name + " release lookup: " + err.Error()})
	}

	target := latest.tag

	current, err := comp.current(ctx, nodeID, nodeOS)
	switch {
	case gameapctl.IsNotFound(err):
		return jsonResponse(http.StatusConflict, errorResponse{Error: err.Error()})
	case err != nil:
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: "reading the current version: " + err.Error()})
	}

	switch err := jobs.CheckUpgrade(current, target); {
	case errors.Is(err, jobs.ErrNotAnUpgrade):
		return jsonResponse(http.StatusConflict, errorResponse{Error: comp.name + " is already " + current + ", the latest release is " + target})
	case errors.Is(err, jobs.ErrMajorUpgrade):
		return jsonResponse(http.StatusConflict, errorResponse{Error: current + " to " + target + " is a major upgrade; run gameapctl on the node by hand"})
	case err != nil:
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: err.Error()})
	}

	args, err := comp.args(target)
	if err != nil {
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: "release feed returned " + target + ": " + err.Error()})
	}

	detect, err := runOnNode(ctx, nodeID, jobs.DetectSystemdCommand)
	if err != nil {
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: "checking for systemd: " + err.Error()})
	}

	if detect.GetExitCode() != 0 {
		return jsonResponse(http.StatusConflict, errorResponse{Error: "this node does not run systemd; upgrades there are not supported yet"})
	}

	var verify jobs.Verify

	if unverified {
		if previous == nil || !previous.CannotVerify || previous.To != target {
			return jsonResponse(http.StatusConflict, errorResponse{
				Error: "upgrading without verification is only offered after a job on this node reported it cannot verify " + target,
			})
		}
	} else {
		var status int

		verify, status, err = releaseToVerify(ctx, nodeID, comp, latest)
		if err != nil {
			return jsonResponse(status, errorResponse{Error: err.Error()})
		}
	}

	id, err := newJobID(ctx)
	if err != nil {
		return jsonResponse(http.StatusInternalServerError, errorResponse{Error: "job id: " + err.Error()})
	}

	job := &jobs.Job{
		ID:        id,
		Component: comp.name,
		NodeID:    nodeID,
		From:      current,
		To:        target,
		State:     jobs.StateRunning,
		StartedAt: time.Now().Unix(),
		ActorID:   actorID,
		Asset:     verify.Asset(),
		// The hex digest; the job's log shows what the node computed.
		ArchiveSHA256: verify.SHA256,
		Unverified:    unverified,
	}

	// Recorded before the command is sent, so a job that starts but whose
	// answer is lost can still be found and reconciled.
	if err := saveJob(ctx, job); err != nil {
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: "saving the job: " + err.Error()})
	}

	start := jobs.UnverifiedStartCommand(id, args)
	if !unverified {
		start, err = jobs.StartCommand(id, verify, args)
	}

	if err != nil {
		finishJob(job, jobs.StateFailed, "building the job command: "+err.Error(), "")
		_ = saveJob(ctx, job)

		return jsonResponse(http.StatusInternalServerError, job)
	}

	out, err := runOnNode(ctx, nodeID, start)
	switch {
	case err != nil:
		finishJob(job, jobs.StateFailed, "starting the job: "+err.Error(), "")
	case out.GetExitCode() != 0:
		finishJob(job, jobs.StateFailed, "systemd-run refused the job", out.GetOutput())
	}

	if err := saveJob(ctx, job); err != nil {
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: "saving the job: " + err.Error()})
	}

	return jsonResponse(http.StatusAccepted, job)
}

// handleGetJob returns a job, first bringing a running one up to date from its
// unit on the node. A finished unit is unloaded and the result verified by
// reading the version again, so "done" means the node really has the target.
//
// A daemon job takes the node offline for a while: gameapctl stops the daemon
// these commands go through. Until it is back, refreshing fails and the job
// is returned unchanged with a note. A panel job goes further: the panel this
// plugin runs in is stopped, so the job is next refreshed by the plugin loaded
// into the new panel, once its daemon has reconnected.
func handleGetJob(ctx context.Context, id string) *pluginproto.HTTPResponse {
	if !jobs.ValidID(id) {
		return jsonResponse(http.StatusBadRequest, errorResponse{Error: "invalid job id"})
	}

	job, err := loadJob(ctx, id)
	if err != nil {
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: "loading the job: " + err.Error()})
	}

	if job == nil {
		return jsonResponse(http.StatusNotFound, errorResponse{Error: "job not found"})
	}

	if !job.Terminal() {
		if err := refreshJob(ctx, job); err != nil {
			// The node may be briefly unreachable (a daemon restart); the job
			// is left running and the next poll tries again.
			return jsonResponse(http.StatusOK, jobWithNote{Job: job, Note: err.Error()})
		}

		if err := saveJob(ctx, job); err != nil {
			return jsonResponse(http.StatusBadGateway, errorResponse{Error: "saving the job: " + err.Error()})
		}
	}

	return jsonResponse(http.StatusOK, job)
}

// jobWithNote carries a transient problem reading a running job.
type jobWithNote struct {
	*jobs.Job
	Note string `json:"note"`
}

func refreshJob(ctx context.Context, job *jobs.Job) error {
	out, err := runOnNode(ctx, job.NodeID, jobs.StatusCommand(job.ID))
	if err != nil {
		return errors.New("checking the job on the node: " + err.Error())
	}

	unit, err := jobs.ParseUnitStatus(out.GetOutput())
	if err != nil {
		return err
	}

	state, why := unit.Outcome()
	if state == jobs.StateRunning {
		return nil
	}

	logOut, _ := runOnNode(ctx, job.NodeID, jobs.LogCommand(job.ID))
	logText := ""
	if logOut != nil {
		logText = logOut.GetOutput()
	}

	// Unload the unit either way; a failure here only leaves a unit behind.
	if state == jobs.StateDone {
		_, _ = runOnNode(ctx, job.NodeID, jobs.StopCommand(job.ID))
	} else {
		_, _ = runOnNode(ctx, job.NodeID, jobs.ResetFailedCommand(job.ID))
	}

	if state == jobs.StateFailed {
		job.CannotVerify = unit.ExitStatus == jobs.ExitCannotVerify
		finishJob(job, jobs.StateFailed, why, logText)

		return nil
	}

	comp, ok := components[job.Component]
	if !ok {
		finishJob(job, jobs.StateFailed, "gameapctl reported success, but this build cannot verify a "+job.Component+" job", logText)

		return nil
	}

	found, err := getNode(ctx, job.NodeID)
	if err != nil || !found.GetFound() {
		finishJob(job, jobs.StateFailed, "gameapctl reported success, but the node could not be looked up to verify it", logText)

		return nil
	}

	now, err := comp.current(ctx, job.NodeID, found.GetNode().GetOs())
	switch {
	case err != nil:
		finishJob(job, jobs.StateFailed, "gameapctl reported success, but reading its version failed: "+err.Error(), logText)
	case !sameVersion(now, job.To):
		finishJob(job, jobs.StateFailed, "gameapctl reported success, but the node now has "+now, logText)
	default:
		finishJob(job, jobs.StateDone, "", logText)
	}

	return nil
}

func finishJob(job *jobs.Job, state jobs.State, why, logText string) {
	job.State = state
	job.Error = why
	job.Log = jobs.Tail(logText, jobLogLines)
	job.FinishedAt = time.Now().Unix()
}

func handleListJobs(ctx context.Context) *pluginproto.HTTPResponse {
	all, err := listJobs(ctx)
	if err != nil {
		return jsonResponse(http.StatusBadGateway, errorResponse{Error: "job list: " + err.Error()})
	}

	if len(all) > 20 {
		all = all[:20]
	}

	return jsonResponse(http.StatusOK, all)
}

func sameVersion(a, b string) bool {
	return strings.TrimPrefix(a, "v") == strings.TrimPrefix(b, "v")
}
