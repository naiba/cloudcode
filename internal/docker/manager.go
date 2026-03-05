package docker

import (
	"context"
	"fmt"
	"io"
	"log"
	"path/filepath"
	"strings"
	"sync"

	"github.com/moby/moby/api/pkg/stdcopy"
	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/mount"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"

	"github.com/naiba/cloudcode/internal/config"
	"github.com/naiba/cloudcode/internal/store"
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
	log.Printf("Pulling latest image %s...", m.image)
	reader, err := m.cli.ImagePull(ctx, m.image, client.ImagePullOptions{})
	if err != nil {
		// If pull fails but a local image already exists, continue with it.
		exists, checkErr := m.ImageExists(ctx)
		if checkErr == nil && exists {
			log.Printf("Pull failed (%v), using existing local image %s", err, m.image)
			return nil
		}
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
	defer m.mu.Unlock()

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

	if m.config != nil {
		globalEnv, err := m.config.GetEnvVars()
		if err == nil {
			for k, v := range globalEnv {
				env = append(env, fmt.Sprintf("%s=%s", k, v))
			}
		}
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
		},
		HostConfig: &container.HostConfig{
			Mounts: mounts,
			RestartPolicy: container.RestartPolicy{
				Name: "unless-stopped",
			},
			Resources: inst.ContainerResources(),
			// No PortBindings: container port is NOT published to the host.
			// The proxy connects directly via the cloudcode-net Docker network.
		},
		NetworkingConfig: &network.NetworkingConfig{
			EndpointsConfig: map[string]*network.EndpointSettings{
				networkName: {},
			},
		},
	})
	if err != nil {
		return "", fmt.Errorf("create container: %w", err)
	}

	if _, err := m.cli.ContainerStart(ctx, resp.ID, client.ContainerStartOptions{}); err != nil {
		_, _ = m.cli.ContainerRemove(ctx, resp.ID, client.ContainerRemoveOptions{Force: true})
		return "", fmt.Errorf("start container: %w", err)
	}

	return resp.ID, nil
}

// GetContainerIP returns the container's IP address on the cloudcode-net network.
// Called after CreateContainer / StartContainer to get the proxy target address.
func (m *Manager) GetContainerIP(ctx context.Context, containerID string) (string, error) {
	result, err := m.cli.ContainerInspect(ctx, containerID, client.ContainerInspectOptions{})
	if err != nil {
		return "", fmt.Errorf("inspect container: %w", err)
	}
	if ep, ok := result.Container.NetworkSettings.Networks[networkName]; ok && ep.IPAddress.IsValid() {
		return ep.IPAddress.String(), nil
	}
	return "", fmt.Errorf("container %s has no IP on network %s", containerID, networkName)
}

// ContainerPort returns the fixed internal port opencode listens on.
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
func (m *Manager) RemoveContainerAndVolume(ctx context.Context, containerID, instanceID string) error {
	_, err := m.cli.ContainerRemove(ctx, containerID, client.ContainerRemoveOptions{
		Force: true,
	})
	if err != nil {
		return err
	}
	// Best-effort removal of the named volume
	volName := volumePrefix + instanceID
	_, _ = m.cli.VolumeRemove(ctx, volName, client.VolumeRemoveOptions{Force: true})
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

func (m *Manager) Close() error {
	return m.cli.Close()
}
