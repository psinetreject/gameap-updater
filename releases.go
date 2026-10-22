//go:build wasip1

package main

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"time"

	sdkhttp "github.com/gameap/gameap/pkg/plugin/sdk/http"

	"github.com/psinetreject/gameap-updater/internal/gameapctl"
)

// The panel's own /api/version covers the panel and the daemon but not
// gameapctl, so latest releases come from GitHub. The daemon's and the
// panel's come from here too: the backend picks every upgrade target itself,
// so a request never carries a version, and GitHub's API is also where the
// SHA-256 of each release archive comes from (see internal/jobs/verify.go).
const (
	gameapctlRepo = "gameap/gameapctl"
	daemonRepo    = "gameap/daemon"
	panelRepo     = "gameap/gameap"
)

// GitHub allows 60 unauthenticated requests an hour per IP, shared with
// anything else on the panel host, so a found tag is kept for an hour. A
// failure is not cached, so the next page load tries again.
const latestTTL = time.Hour

// release is a latest release: its tag and GitHub's digest for each asset.
type release struct {
	tag     string
	digests map[string]string
}

type cachedRelease struct {
	release
	fetchedAt time.Time
}

var latestCache struct {
	sync.Mutex
	byRepo map[string]cachedRelease
}

func latestGameapctl(ctx context.Context) (string, error) {
	r, err := latestRelease(ctx, gameapctlRepo)

	return r.tag, err
}

// latestRelease returns repo's latest stable GitHub release; GitHub leaves
// pre-releases out of /releases/latest.
func latestRelease(ctx context.Context, repo string) (release, error) {
	latestCache.Lock()
	defer latestCache.Unlock()

	if c, ok := latestCache.byRepo[repo]; ok && time.Since(c.fetchedAt) < latestTTL {
		return c.release, nil
	}

	resp, err := sdkhttp.NewHTTPService().Fetch(ctx, &sdkhttp.HTTPFetchRequest{
		Method: "GET",
		Url:    "https://api.github.com/repos/" + repo + "/releases/latest",
		Headers: map[string]string{
			"Accept":     "application/vnd.github+json",
			"User-Agent": "gameap-updater/" + version,
		},
		TimeoutSeconds: 10,
	})
	if err != nil {
		return release{}, err
	}

	if resp.Error != nil {
		return release{}, errors.New(*resp.Error)
	}

	if resp.StatusCode != 200 {
		return release{}, errors.New("GitHub answered HTTP " + strconv.Itoa(int(resp.StatusCode)))
	}

	tag, digests, err := gameapctl.ParseRelease(resp.Body)
	if err != nil {
		return release{}, err
	}

	r := release{tag: tag, digests: digests}

	if latestCache.byRepo == nil {
		latestCache.byRepo = map[string]cachedRelease{}
	}
	latestCache.byRepo[repo] = cachedRelease{release: r, fetchedAt: time.Now()}

	return r, nil
}
