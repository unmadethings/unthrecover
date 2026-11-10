// Package docker is a minimal client for the official Docker Engine REST API
// over a local Unix socket. It implements only the endpoints Unth Recover
// needs, which keeps the dependency surface small and auditable.
//
// Access to the Docker socket is equivalent to root on the host. Unth Recover
// only creates resources it labels as its own and only deletes resources that
// carry both the managed label and the label of the run being cleaned up.
package docker

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

// Labels applied to every resource Unth Recover creates.
const (
	LabelManaged = "io.unthrecover.managed"
	LabelRun     = "io.unthrecover.run"
	LabelBackup  = "io.unthrecover.backup"
	LabelRole    = "io.unthrecover.role"
)

// maxAPIVersion is the newest Engine API version this client was tested with.
const maxAPIVersion = "1.51"

// ErrNotFound is returned for 404 responses.
var ErrNotFound = errors.New("docker: not found")

// AccessError explains why the Docker Engine could not be reached.
type AccessError struct {
	Host string
	Err  error
}

func (e *AccessError) Error() string {
	hint := "is the Docker daemon running?"
	msg := e.Err.Error()
	switch {
	case strings.Contains(msg, "permission denied"):
		hint = "the current user lacks permission on the Docker socket (add the user to the 'docker' group or run with appropriate privileges; note that Docker socket access is root-equivalent)"
	case strings.Contains(msg, "no such file"):
		hint = "the socket does not exist; is the Docker daemon installed and running? (check DOCKER_HOST)"
	case strings.Contains(msg, "connection refused"):
		hint = "the socket exists but nothing is listening; is the Docker daemon running?"
	}
	return fmt.Sprintf("cannot connect to Docker Engine at %s: %v (%s)", e.Host, e.Err, hint)
}

func (e *AccessError) Unwrap() error { return e.Err }

// Client talks to the Docker Engine API.
type Client struct {
	host       string
	http       *http.Client
	apiVersion string
}

// NewFromEnv creates a client for DOCKER_HOST (default unix:///var/run/docker.sock).
// Only unix:// hosts are supported; unauthenticated TCP is refused.
func NewFromEnv() (*Client, error) {
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = "unix:///var/run/docker.sock"
	}
	if !strings.HasPrefix(host, "unix://") {
		return nil, fmt.Errorf("DOCKER_HOST %q is not supported: only unix:// sockets are supported in this release", host)
	}
	sock := strings.TrimPrefix(host, "unix://")
	tr := &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, "unix", sock)
		},
		MaxIdleConns:    4,
		IdleConnTimeout: 30 * time.Second,
	}
	return &Client{host: host, http: &http.Client{Transport: tr}}, nil
}

// Host returns the configured DOCKER_HOST value.
func (c *Client) Host() string { return c.host }

// VersionInfo is the subset of /version used by Unth Recover.
type VersionInfo struct {
	Version       string `json:"Version"`
	APIVersion    string `json:"ApiVersion"`
	MinAPIVersion string `json:"MinAPIVersion"`
	Os            string `json:"Os"`
	Arch          string `json:"Arch"`
}

// Negotiate queries the daemon version and picks an API version both sides support.
func (c *Client) Negotiate(ctx context.Context) (*VersionInfo, error) {
	var v VersionInfo
	if err := c.do(ctx, http.MethodGet, "/version", nil, nil, &v, false); err != nil {
		return nil, err
	}
	chosen := v.APIVersion
	if compareVersion(chosen, maxAPIVersion) > 0 {
		chosen = maxAPIVersion
	}
	if v.MinAPIVersion != "" && compareVersion(chosen, v.MinAPIVersion) < 0 {
		chosen = v.MinAPIVersion
	}
	c.apiVersion = chosen
	return &v, nil
}

func compareVersion(a, b string) int {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < len(pa) || i < len(pb); i++ {
		var x, y int
		if i < len(pa) {
			x, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			y, _ = strconv.Atoi(pb[i])
		}
		if x != y {
			if x < y {
				return -1
			}
			return 1
		}
	}
	return 0
}

func (c *Client) ensureVersion(ctx context.Context) error {
	if c.apiVersion != "" {
		return nil
	}
	_, err := c.Negotiate(ctx)
	return err
}

// apiError is the Engine API error body.
type apiError struct {
	Message string `json:"message"`
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body any, out any, versioned bool) error {
	resp, err := c.raw(ctx, method, path, query, body, versioned)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if out != nil {
		if err := json.NewDecoder(io.LimitReader(resp.Body, 32<<20)).Decode(out); err != nil && !errors.Is(err, io.EOF) {
			return fmt.Errorf("docker %s %s: decode: %w", method, path, err)
		}
	} else {
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	}
	return nil
}

func (c *Client) raw(ctx context.Context, method, path string, query url.Values, body any, versioned bool) (*http.Response, error) {
	if versioned {
		if err := c.ensureVersion(ctx); err != nil {
			return nil, err
		}
		path = "/v" + c.apiVersion + path
	}
	u := "http://docker" + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rdr)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		var opErr *net.OpError
		if errors.As(err, &opErr) {
			return nil, &AccessError{Host: c.host, Err: opErr.Err}
		}
		return nil, &AccessError{Host: c.host, Err: err}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var ae apiError
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = json.Unmarshal(b, &ae)
		msg := ae.Message
		if msg == "" {
			msg = strings.TrimSpace(string(b))
		}
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, msg)
		}
		return nil, fmt.Errorf("docker %s %s: HTTP %d: %s", method, path, resp.StatusCode, msg)
	}
	return resp, nil
}

// Ping checks that the daemon answers.
func (c *Client) Ping(ctx context.Context) error {
	return c.do(ctx, http.MethodGet, "/_ping", nil, nil, nil, false)
}

// ContainerSummary is a container list entry.
type ContainerSummary struct {
	ID      string            `json:"Id"`
	Names   []string          `json:"Names"`
	Image   string            `json:"Image"`
	State   string            `json:"State"`
	Status  string            `json:"Status"`
	Labels  map[string]string `json:"Labels"`
	Mounts  []MountPoint      `json:"Mounts"`
	Created int64             `json:"Created"`
}

// MountPoint describes a container mount.
type MountPoint struct {
	Type        string `json:"Type"`
	Name        string `json:"Name"`
	Source      string `json:"Source"`
	Destination string `json:"Destination"`
	RW          bool   `json:"RW"`
}

func labelFilter(labels ...string) url.Values {
	q := url.Values{}
	if len(labels) > 0 {
		f, _ := json.Marshal(map[string][]string{"label": labels})
		q.Set("filters", string(f))
	}
	return q
}

// ListContainers lists containers (all states), optionally filtered by labels ("k=v").
func (c *Client) ListContainers(ctx context.Context, labels ...string) ([]ContainerSummary, error) {
	q := labelFilter(labels...)
	q.Set("all", "1")
	var out []ContainerSummary
	err := c.do(ctx, http.MethodGet, "/containers/json", q, nil, &out, true)
	return out, err
}

// Volume is a volume list entry.
type Volume struct {
	Name       string            `json:"Name"`
	Driver     string            `json:"Driver"`
	Mountpoint string            `json:"Mountpoint"`
	Labels     map[string]string `json:"Labels"`
	CreatedAt  string            `json:"CreatedAt"`
}

// ListVolumes lists volumes, optionally filtered by labels.
func (c *Client) ListVolumes(ctx context.Context, labels ...string) ([]Volume, error) {
	var out struct {
		Volumes []Volume `json:"Volumes"`
	}
	err := c.do(ctx, http.MethodGet, "/volumes", labelFilter(labels...), nil, &out, true)
	return out.Volumes, err
}

// CreateVolume creates a labeled named volume.
func (c *Client) CreateVolume(ctx context.Context, name string, labels map[string]string) error {
	return c.do(ctx, http.MethodPost, "/volumes/create", nil,
		map[string]any{"Name": name, "Driver": "local", "Labels": labels}, nil, true)
}

// RemoveVolume removes a volume by name.
func (c *Client) RemoveVolume(ctx context.Context, name string) error {
	return c.do(ctx, http.MethodDelete, "/volumes/"+url.PathEscape(name), nil, nil, nil, true)
}

// Network is a network list entry.
type Network struct {
	ID       string            `json:"Id"`
	Name     string            `json:"Name"`
	Internal bool              `json:"Internal"`
	Labels   map[string]string `json:"Labels"`
}

// CreateNetwork creates an internal (no external connectivity) bridge network.
func (c *Client) CreateNetwork(ctx context.Context, name string, labels map[string]string) (string, error) {
	return c.CreateNetworkOpts(ctx, name, labels, false)
}

// CreateNetworkOpts creates an internal network. allowICC enables
// container-to-container traffic; it is only needed when unth itself runs in
// a container and joins the sandbox network.
func (c *Client) CreateNetworkOpts(ctx context.Context, name string, labels map[string]string, allowICC bool) (string, error) {
	var out struct {
		ID string `json:"Id"`
	}
	icc := "false"
	if allowICC {
		icc = "true"
	}
	err := c.do(ctx, http.MethodPost, "/networks/create", nil, map[string]any{
		"Name": name, "Driver": "bridge", "Internal": true, "Attachable": false, "Labels": labels,
		"Options": map[string]string{"com.docker.network.bridge.enable_icc": icc},
	}, &out, true)
	return out.ID, err
}

// ConnectNetwork attaches a container to a network.
func (c *Client) ConnectNetwork(ctx context.Context, network, container string) error {
	return c.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/connect", nil,
		map[string]any{"Container": container}, nil, true)
}

// DisconnectNetwork detaches a container from a network (forced).
func (c *Client) DisconnectNetwork(ctx context.Context, network, container string) error {
	return c.do(ctx, http.MethodPost, "/networks/"+url.PathEscape(network)+"/disconnect", nil,
		map[string]any{"Container": container, "Force": true}, nil, true)
}

// NetworkEndpoints returns the IDs of containers attached to a network.
func (c *Client) NetworkEndpoints(ctx context.Context, network string) ([]string, error) {
	var out struct {
		Containers map[string]any `json:"Containers"`
	}
	if err := c.do(ctx, http.MethodGet, "/networks/"+url.PathEscape(network), nil, nil, &out, true); err != nil {
		return nil, err
	}
	ids := make([]string, 0, len(out.Containers))
	for id := range out.Containers {
		ids = append(ids, id)
	}
	return ids, nil
}

// ListNetworks lists networks filtered by labels.
func (c *Client) ListNetworks(ctx context.Context, labels ...string) ([]Network, error) {
	var out []Network
	err := c.do(ctx, http.MethodGet, "/networks", labelFilter(labels...), nil, &out, true)
	return out, err
}

// RemoveNetwork removes a network.
func (c *Client) RemoveNetwork(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/networks/"+url.PathEscape(id), nil, nil, nil, true)
}

// ImageExists reports whether an image is present locally.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.do(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, nil, true)
	if errors.Is(err, ErrNotFound) {
		return false, nil
	}
	return err == nil, err
}

// PullImage pulls an image and returns an error if the pull stream reports one.
func (c *Client) PullImage(ctx context.Context, ref string) error {
	name, tag := ref, ""
	if i := strings.LastIndex(ref, "@"); i > 0 {
		name, tag = ref[:i], ref[i+1:]
	} else if i := strings.LastIndex(ref, ":"); i > strings.LastIndex(ref, "/") {
		name, tag = ref[:i], ref[i+1:]
	}
	q := url.Values{"fromImage": {name}}
	if tag != "" {
		q.Set("tag", tag)
	}
	resp, err := c.raw(ctx, http.MethodPost, "/images/create", q, nil, true)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		var m struct {
			Error string `json:"error"`
		}
		if json.Unmarshal(sc.Bytes(), &m) == nil && m.Error != "" {
			return fmt.Errorf("pull %s: %s", ref, m.Error)
		}
	}
	return sc.Err()
}

// ContainerSpec is the hardened container definition used for restore tests.
type ContainerSpec struct {
	Name        string
	Image       string
	Env         []string
	Labels      map[string]string
	NetworkName string
	VolumeName  string
	VolumeDest  string
	MemoryBytes int64
	NanoCPUs    int64
	PidsLimit   int64
	CapAdd      []string
	ShmSize     int64
}

// CreateContainer creates (but does not start) a container. All capabilities
// are dropped except those listed, privilege escalation is disabled, no ports
// are published and no host paths are mounted.
func (c *Client) CreateContainer(ctx context.Context, s ContainerSpec) (string, error) {
	pids := s.PidsLimit
	host := map[string]any{
		"NetworkMode":     s.NetworkName,
		"Memory":          s.MemoryBytes,
		"MemorySwap":      s.MemoryBytes,
		"NanoCpus":        s.NanoCPUs,
		"PidsLimit":       pids,
		"CapDrop":         []string{"ALL"},
		"CapAdd":          s.CapAdd,
		"SecurityOpt":     []string{"no-new-privileges:true"},
		"Privileged":      false,
		"PublishAllPorts": false,
		"ReadonlyRootfs":  false,
		"ShmSize":         s.ShmSize,
		"RestartPolicy":   map[string]any{"Name": "no"},
		"LogConfig":       map[string]any{"Type": "json-file", "Config": map[string]string{"max-size": "10m", "max-file": "1"}},
	}
	if s.VolumeName != "" {
		host["Mounts"] = []map[string]any{{"Type": "volume", "Source": s.VolumeName, "Target": s.VolumeDest}}
	}
	body := map[string]any{
		"Image":      s.Image,
		"Env":        s.Env,
		"Labels":     s.Labels,
		"HostConfig": host,
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := c.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {s.Name}}, body, &out, true)
	return out.ID, err
}

// StartContainer starts a container.
func (c *Client) StartContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/start", nil, nil, nil, true)
}

// ContainerInfo is the subset of container inspect output used here.
type ContainerInfo struct {
	ID    string `json:"Id"`
	State struct {
		Status    string `json:"Status"`
		Running   bool   `json:"Running"`
		ExitCode  int    `json:"ExitCode"`
		OOMKilled bool   `json:"OOMKilled"`
		Error     string `json:"Error"`
		Health    *struct {
			Status string `json:"Status"`
		} `json:"Health"`
	} `json:"State"`
	Config struct {
		Image  string            `json:"Image"`
		Labels map[string]string `json:"Labels"`
	} `json:"Config"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// InspectContainer returns container details.
func (c *Client) InspectContainer(ctx context.Context, id string) (*ContainerInfo, error) {
	var out ContainerInfo
	if err := c.do(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/json", nil, nil, &out, true); err != nil {
		return nil, err
	}
	return &out, nil
}

// RemoveContainer force-removes a container and its anonymous volumes.
func (c *Client) RemoveContainer(ctx context.Context, id string) error {
	return c.do(ctx, http.MethodDelete, "/containers/"+url.PathEscape(id),
		url.Values{"force": {"1"}, "v": {"1"}}, nil, nil, true)
}

// ContainerLogs returns the last lines of a container's output (demultiplexed).
func (c *Client) ContainerLogs(ctx context.Context, id string, tail int) (string, error) {
	resp, err := c.raw(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/logs",
		url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {strconv.Itoa(tail)}}, nil, true)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	return demux(io.LimitReader(resp.Body, 1<<20)), nil
}

// demux strips the 8-byte stream headers of non-TTY container output.
func demux(r io.Reader) string {
	var buf bytes.Buffer
	hdr := make([]byte, 8)
	for {
		if _, err := io.ReadFull(r, hdr); err != nil {
			break
		}
		n := int(hdr[4])<<24 | int(hdr[5])<<16 | int(hdr[6])<<8 | int(hdr[7])
		if _, err := io.CopyN(&buf, r, int64(n)); err != nil {
			break
		}
	}
	return buf.String()
}

// streamRaw sends an arbitrary body and returns the raw response.
func (c *Client) streamRaw(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Response, error) {
	if err := c.ensureVersion(ctx); err != nil {
		return nil, err
	}
	u := "http://docker/v" + c.apiVersion + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, &AccessError{Host: c.host, Err: err}
	}
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		var ae apiError
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = json.Unmarshal(b, &ae)
		if resp.StatusCode == http.StatusNotFound {
			return nil, fmt.Errorf("%w: %s", ErrNotFound, ae.Message)
		}
		return nil, fmt.Errorf("docker %s %s: HTTP %d: %s", method, path, resp.StatusCode, ae.Message)
	}
	return resp, nil
}

// InspectVolume returns a volume by name.
func (c *Client) InspectVolume(ctx context.Context, name string) (*Volume, error) {
	var v Volume
	if err := c.do(ctx, http.MethodGet, "/volumes/"+url.PathEscape(name), nil, nil, &v, true); err != nil {
		return nil, err
	}
	return &v, nil
}

// ContainersUsingVolume lists all containers (any state) that mount the volume.
func (c *Client) ContainersUsingVolume(ctx context.Context, volume string) ([]ContainerSummary, error) {
	f, _ := json.Marshal(map[string][]string{"volume": {volume}})
	var out []ContainerSummary
	err := c.do(ctx, http.MethodGet, "/containers/json", url.Values{"all": {"1"}, "filters": {string(f)}}, nil, &out, true)
	return out, err
}

// StopContainer stops a container, waiting up to timeoutSec before killing it.
func (c *Client) StopContainer(ctx context.Context, id string, timeoutSec int) error {
	err := c.do(ctx, http.MethodPost, "/containers/"+url.PathEscape(id)+"/stop",
		url.Values{"t": {strconv.Itoa(timeoutSec)}}, nil, nil, true)
	if err != nil && strings.Contains(err.Error(), "HTTP 304") {
		return nil // already stopped
	}
	return err
}

// HelperSpec describes a never-started container used only as a mount point
// for the archive API (no process runs, no network).
type HelperSpec struct {
	Name     string
	Image    string
	Labels   map[string]string
	Volume   string
	ReadOnly bool
}

// CreateHelper creates a container that mounts Volume at /data. It is never started.
func (c *Client) CreateHelper(ctx context.Context, s HelperSpec) (string, error) {
	body := map[string]any{
		"Image":           s.Image,
		"Cmd":             []string{"true"},
		"Labels":          s.Labels,
		"NetworkDisabled": true,
		"HostConfig": map[string]any{
			"NetworkMode":   "none",
			"CapDrop":       []string{"ALL"},
			"SecurityOpt":   []string{"no-new-privileges:true"},
			"RestartPolicy": map[string]any{"Name": "no"},
			"Mounts":        []map[string]any{{"Type": "volume", "Source": s.Volume, "Target": "/data", "ReadOnly": s.ReadOnly}},
		},
	}
	var out struct {
		ID string `json:"Id"`
	}
	err := c.do(ctx, http.MethodPost, "/containers/create", url.Values{"name": {s.Name}}, body, &out, true)
	return out.ID, err
}

// ArchiveFrom streams a tar archive of path inside the container.
func (c *Client) ArchiveFrom(ctx context.Context, id, path string) (io.ReadCloser, error) {
	resp, err := c.streamRaw(ctx, http.MethodGet, "/containers/"+url.PathEscape(id)+"/archive", url.Values{"path": {path}}, nil, "")
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// ArchiveTo extracts a tar archive into dir inside the container. Extraction
// is performed by the Docker daemon into the container's filesystem/volumes,
// never on the host filesystem by unth.
func (c *Client) ArchiveTo(ctx context.Context, id, dir string, tarStream io.Reader) error {
	resp, err := c.streamRaw(ctx, http.MethodPut, "/containers/"+url.PathEscape(id)+"/archive",
		url.Values{"path": {dir}, "noOverwriteDirNonDir": {"true"}}, tarStream, "application/x-tar")
	if err != nil {
		return err
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	return resp.Body.Close()
}

// ClassifyImage returns a coarse workload kind for an image reference.
func ClassifyImage(image string) string {
	img := strings.ToLower(image)
	for _, k := range []struct{ sub, kind string }{
		{"postgis", "postgres"}, {"timescale", "postgres"}, {"postgres", "postgres"},
		{"mariadb", "mysql"}, {"mysql", "mysql"}, {"mongo", "mongodb"},
		{"redis", "redis"}, {"valkey", "redis"}, {"elasticsearch", "elasticsearch"},
		{"clickhouse", "clickhouse"}, {"influxdb", "influxdb"}, {"cassandra", "cassandra"},
	} {
		if strings.Contains(img, k.sub) {
			return k.kind
		}
	}
	return "generic"
}
