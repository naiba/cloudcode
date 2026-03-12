package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/moby/moby/api/pkg/stdcopy"
	"net/netip"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/vkenliu/cloudcode-docker/internal/config"
	"github.com/vkenliu/cloudcode-docker/internal/store"
)

const (
	labelPrefix     = "cloudcode."
	labelManaged    = labelPrefix + "managed"
	labelInstID     = labelPrefix + "instance-id"
	defaultImage    = "ghcr.io/naiba/cloudcode-base:latest"
	networkName     = "cloudcode-net"
	containerPrefix = "cloudcode-"
	volumePrefix    = "cloudcode-home-"
	// containerPort is the fixed internal port opencode listens on inside the container.
	containerPort = 4096
)

type Manager struct {
	cli    *client.Client
	mu     sync.Mutex
	image  string
	config *config.Manager

	// Volume disk usage cache (1-hour TTL).
	diskCacheMu sync.Mutex
	diskCache   map[string]int64 // instanceID → bytes (-1 = unavailable)
	diskCacheAt time.Time
}

func NewManager(imageName string, cfgMgr *config.Manager) (*Manager, error) {
	cli, err := client.NewClientWithOpts(client.FromEnv, client.WithAPIVersionNegotiation())
	if err != nil {
		return nil, fmt.Errorf("create docker client: %w", err)
	}

	if imageName == "" {
		imageName = defaultImage
	}

	m := &Manager{cli: cli, image: imageName, config: cfgMgr}

	if err := m.ensureNetwork(context.Background()); err != nil {
		return nil, fmt.Errorf("ensure network: %w", err)
	}

	return m, nil
}

func (m *Manager) ensureNetwork(ctx context.Context) error {
	result, err := m.cli.NetworkList(ctx, client.NetworkListOptions{
		Filters: make(client.Filters).Add("name", networkName),
	})
	if err != nil {
		return err
	}
	if len(result.Items) > 0 {
		return nil
	}

	_, err = m.cli.NetworkCreate(ctx, networkName, client.NetworkCreateOptions{
		Driver: "bridge",
	})
	return err
}

func (m *Manager) ensureImage(ctx context.Context) error {
	// If the image already exists locally, skip the pull entirely.
	exists, err := m.ImageExists(ctx)
	if err != nil {
		log.Printf("Warning: could not check for image %s: %v", m.image, err)
	}
	if exists {
		log.Printf("Image %s found locally, skipping pull", m.image)
		return nil
	}

	log.Printf("Image %s not found locally, pulling...", m.image)
	// Use a background context so the pull is not canceled if the HTTP request
	// context times out or the client disconnects mid-pull.
	pullCtx := context.Background()
	reader, err := m.cli.ImagePull(pullCtx, m.image, client.ImagePullOptions{})
	if err != nil {
		return fmt.Errorf("pull image %s: %w", m.image, err)
	}
	defer reader.Close()
	_, _ = io.Copy(io.Discard, reader)
	log.Printf("Image %s pulled successfully", m.image)
	return nil
}

// CreateContainer creates and starts a new container for the given instance.
// Container ports are NOT published to the host — traffic is routed through
// the CloudCode reverse proxy via the cloudcode-net Docker network.
// Returns the container ID.
func (m *Manager) CreateContainer(ctx context.Context, inst *store.Instance) (string, error) {
	// #16: pull/check image before acquiring the global mutex to avoid blocking
	// other operations (status checks, etc.) during a potentially long image pull.
	if err := m.ensureImage(ctx); err != nil {
		return "", fmt.Errorf("ensure image: %w", err)
	}

	m.mu.Lock()

	containerName := containerPrefix + inst.ID

	env := []string{
		fmt.Sprintf("OPENCODE_PORT=%d", containerPort),
		fmt.Sprintf("CC_INSTANCE_NAME=%s", inst.Name),
	}

	// Set OpenCode's native Basic Auth password to the per-instance access token.
	// This enforces auth at the OpenCode level as a second layer of defence,
	// in addition to the CloudCode proxy token check.
	if inst.AccessToken != "" {
		env = append(env, fmt.Sprintf("OPENCODE_SERVER_PASSWORD=%s", inst.AccessToken))
	}

	// Merge env vars: start with global settings, then let per-instance vars
	// override them. This means a per-instance ANTHROPIC_API_KEY will shadow
	// the one set in Settings without affecting other instances.
	merged := make(map[string]string)
	if m.config != nil {
		if globalEnv, err := m.config.GetEnvVars(); err == nil {
			for k, v := range globalEnv {
				merged[k] = v
			}
		}
	}
	// Per-instance vars override globals for the same key.
	for k, v := range inst.EnvVars {
		merged[k] = v
	}
	for k, v := range merged {
		env = append(env, fmt.Sprintf("%s=%s", k, v))
	}

	// Named volume for /root (persists across container recreations)
	homeVolume := volumePrefix + inst.ID
	mounts := []mount.Mount{
		{
			Type:   mount.TypeVolume,
			Source: homeVolume,
			Target: "/root",
		},
	}
	if m.config != nil {
		cms, err := m.config.ContainerMountsForInstance(inst.ID)
		if err != nil {
			return "", fmt.Errorf("prepare mounts: %w", err)
		}
		for _, cm := range cms {
			absHost, _ := filepath.Abs(cm.HostPath)
			mounts = append(mounts, mount.Mount{
				Type:     mount.TypeBind,
				Source:   absHost,
				Target:   cm.ContainerPath,
				ReadOnly: cm.ReadOnly,
			})
		}
	}

	exposedPort := network.MustParsePort(fmt.Sprintf("%d/tcp", containerPort))
	resp, err := m.cli.ContainerCreate(ctx, client.ContainerCreateOptions{
		Name: containerName,
		Config: &container.Config{
			Image:      m.image,
			WorkingDir: "/root",
			Env:        env,
			Labels: map[string]string{
				labelManaged: "true",
				labelInstID:  inst.ID,
			},
			// ExposedPorts is required for Docker to honour PortBindings.
			ExposedPorts: network.PortSet{
				exposedPort: struct{}{},
			},
		},
		HostConfig: &container.HostConfig{
			Mounts: mounts,
			RestartPolicy: container.RestartPolicy{
				Name: "unless-stopped",
			},
			Resources: inst.ContainerResources(),
			// Publish the container port to a random host port (Docker picks it).
			// Binding to 127.0.0.1 keeps it inaccessible from the network.
			// The proxy reads the assigned host port via GetContainerIPAndPort and
			// routes through 127.0.0.1:<hostPort>, which is reliable regardless of
			// what address opencode binds to inside the container.
			PortBindings: network.PortMap{
				exposedPort: []network.PortBinding{
					{HostIP: netip.MustParseAddr("127.0.0.1"), HostPort: "0"},
				},
			},
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				networkName: {},
			},
		},
	})
	if err != nil {
		m.mu.Unlock()
		return "", fmt.Errorf("create container: %w", err)
	}
	containerID := resp.ID
	// M5: release the mutex before the slow ContainerStart call so other
	// operations (status checks, etc.) are not blocked during startup.
	m.mu.Unlock()

	if _, err := m.cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{}); err != nil {
		_, _ = m.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{Force: true})
		return "", fmt.Errorf("start container: %w", err)
	}

	return containerID, nil
}

// GetContainerIPAndPort returns the host/IP and port to use for proxying to the container.
//
// For legacy containers (created before port-pool removal) that publish a host port,
// it returns "127.0.0.1" and the published host port — because old opencode versions
// bind to 127.0.0.1 inside the container and are only reachable via the published port.
//
// For new containers (no published ports), it returns the container's IP on
// cloudcode-net and OPENCODE_PORT from the container env (default 4096).
func (m *Manager) GetContainerIPAndPort(ctx context.Context, containerID string) (string, int, error) {
	result, err := m.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return "", 0, fmt.Errorf("inspect container: %w", err)
	}

	// M6: look specifically for the binding on containerPort/tcp rather than
	// returning the first arbitrary binding (map iteration is non-deterministic).
	targetPort := network.MustParsePort(fmt.Sprintf("%d/tcp", containerPort))
	if bindings, ok := result.Container.NetworkSettings.Ports[targetPort]; ok {
		for _, b := range bindings {
			if b.HostPort != "" {
				var hp int
				if n, _ := fmt.Sscanf(b.HostPort, "%d", &hp); n == 1 && hp > 0 {
					return "127.0.0.1", hp, nil
				}
			}
		}
	}

	// New containers: connect via Docker network IP + OPENCODE_PORT.
	ip := ""
	if ep, ok := result.Container.NetworkSettings.Networks[networkName]; ok && ep.IPAddress.IsValid() {
		ip = ep.IPAddress.String()
	}
	if ip == "" {
		return "", 0, fmt.Errorf("container %s has no IP on network %s", containerID, networkName)
	}

	// Read OPENCODE_PORT from container env.
	port := containerPort
	for _, env := range result.Container.Config.Env {
		if strings.HasPrefix(env, "OPENCODE_PORT=") {
			var p int
			if n, _ := fmt.Sscanf(env[len("OPENCODE_PORT="):], "%d", &p); n == 1 && p > 0 {
				port = p
			}
			break
		}
	}

	return ip, port, nil
}

// GetContainerIP returns the container's IP address on the cloudcode-net network.
// Called after CreateContainer / StartContainer to get the proxy target address.
// Use GetContainerIPAndPort when the port may differ (e.g. legacy containers).
func (m *Manager) GetContainerIP(ctx context.Context, containerID string) (string, error) {
	ip, _, err := m.GetContainerIPAndPort(ctx, containerID)
	return ip, err
}

// ContainerPort returns the fixed internal port opencode listens on for new containers.
func ContainerPort() int {
	return containerPort
}

func (m *Manager) StopContainer(ctx context.Context, containerID string) error {
	timeout := 30
	_, err := m.cli.ContainerStop(ctx, containerID, client.ContainerStopOptions{Timeout: &timeout})
	return err
}

func (m *Manager) StartContainer(ctx context.Context, containerID string) error {
	_, err := m.cli.ContainerStart(ctx, containerID, client.ContainerStartOptions{})
	return err
}

func (m *Manager) RemoveContainer(ctx context.Context, containerID string) error {
	_, err := m.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
		Force: true,
	})
	return err
}

// RemoveContainerAndVolume removes the container and its named home volume.
// Used when permanently deleting an instance.
// L13: always attempts volume removal even if the container is already gone.
func (m *Manager) RemoveContainerAndVolume(ctx context.Context, containerID, instanceID string) error {
	_, containerErr := m.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
		Force: true,
	})
	if containerErr != nil && !strings.Contains(containerErr.Error(), "No such container") {
		// Real error — log but still proceed to volume removal.
		log.Printf("Warning: remove container %s: %v", containerID, containerErr)
	}
	// Always attempt volume removal regardless of container removal outcome.
	volName := volumePrefix + instanceID
	_, volErr := m.cli.VolumeRemove(ctx, volName, client.VolumeRemoveOptions{Force: true})
	if volErr != nil && !strings.Contains(volErr.Error(), "no such volume") {
		log.Printf("Warning: remove volume %s: %v", volName, volErr)
	}
	// Invalidate disk cache since a volume was removed (or attempted).
	m.InvalidateDiskCache()
	// Return the first real error encountered.
	if containerErr != nil && !strings.Contains(containerErr.Error(), "No such container") {
		return containerErr
	}
	if volErr != nil && !strings.Contains(volErr.Error(), "no such volume") {
		return fmt.Errorf("volume %s removal failed: %w", volName, volErr)
	}
	return nil
}

func (m *Manager) ContainerLogsStream(ctx context.Context, containerID string, tail string) (io.ReadCloser, error) {
	if tail == "" {
		tail = "100"
	}

	raw, err := m.cli.ContainerLogs(ctx, containerID, client.ContainerLogsOptions{
		ShowStdout: true,
		ShowStderr: true,
		Tail:       tail,
		Timestamps: true,
		Follow:     true,
	})
	if err != nil {
		return nil, fmt.Errorf("stream container logs: %w", err)
	}

	pr, pw := io.Pipe()
	go func() {
		_, err := stdcopy.StdCopy(pw, pw, raw)
		raw.Close()
		pw.CloseWithError(err)
	}()
	return pr, nil
}

func (m *Manager) ContainerStatus(ctx context.Context, containerID string) (string, error) {
	result, err := m.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		if strings.Contains(err.Error(), "No such container") {
			return "removed", nil
		}
		return "unknown", err
	}
	return string(result.Container.State.Status), nil
}

func (m *Manager) ImageExists(ctx context.Context) (bool, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	result, err := m.cli.ImageList(ctx, client.ImageListOptions{
		Filters: make(client.Filters).Add("reference", m.image),
	})
	if err != nil {
		return false, err
	}
	return len(result.Items) > 0, nil
}

func (m *Manager) ExecCreate(ctx context.Context, containerID string, cmd []string) (string, error) {
	result, err := m.cli.ExecCreate(ctx, containerID, client.ExecCreateOptions{
		TTY:          true,
		AttachStdin:  true,
		AttachStdout: true,
		AttachStderr: true,
		Cmd:          cmd,
	})
	if err != nil {
		return "", fmt.Errorf("exec create: %w", err)
	}
	return result.ID, nil
}

func (m *Manager) ExecAttach(ctx context.Context, execID string) (client.HijackedResponse, error) {
	resp, err := m.cli.ExecAttach(ctx, execID, client.ExecAttachOptions{TTY: true})
	if err != nil {
		return client.HijackedResponse{}, fmt.Errorf("exec attach: %w", err)
	}
	return resp.HijackedResponse, nil
}

func (m *Manager) ExecResize(ctx context.Context, execID string, height, width uint) error {
	_, err := m.cli.ExecResize(ctx, execID, client.ExecResizeOptions{
		Height: height,
		Width:  width,
	})
	return err
}

const diskCacheTTL = 1 * time.Hour

// VolumeDiskUsage returns a map of instanceID → disk usage in bytes for all
// managed CloudCode volumes. Uses a 1-hour cache to avoid expensive Docker
// system-df calls on every request. Returns -1 for volumes where size is
// unavailable (non-local driver).
func (m *Manager) VolumeDiskUsage(ctx context.Context) (map[string]int64, error) {
	m.diskCacheMu.Lock()
	defer m.diskCacheMu.Unlock()

	if m.diskCache != nil && time.Since(m.diskCacheAt) < diskCacheTTL {
		return m.diskCache, nil
	}

	result, err := m.cli.DiskUsage(ctx, client.DiskUsageOptions{
		Volumes: true,
		Verbose: true,
	})
	if err != nil {
		return nil, fmt.Errorf("docker disk usage: %w", err)
	}

	usage := make(map[string]int64)
	// result.Volumes is a value struct; Items is a nil-safe slice.
	for _, vol := range result.Volumes.Items {
		if !strings.HasPrefix(vol.Name, volumePrefix) {
			continue
		}
		instID := strings.TrimPrefix(vol.Name, volumePrefix)
		if vol.UsageData != nil {
			usage[instID] = vol.UsageData.Size
		} else {
			usage[instID] = -1
		}
	}

	m.diskCache = usage
	m.diskCacheAt = time.Now()
	return usage, nil
}

// InvalidateDiskCache forces the next VolumeDiskUsage call to re-query Docker.
func (m *Manager) InvalidateDiskCache() {
	m.diskCacheMu.Lock()
	m.diskCache = nil
	m.diskCacheMu.Unlock()
}

func (m *Manager) Close() error {
	return m.cli.Close()
}
