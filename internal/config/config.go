package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

const (
	DirOpenCodeConfig = "opencode"      // → /root/.config/opencode/
	DirOpenCodeData   = "opencode-data" // → /root/.local/share/opencode/
	DirDotOpenCode    = "dot-opencode"  // → /root/.opencode/
	FileEnvVars       = "env.json"
)

var OpenCodeConfigFiles = []string{
	"opencode.jsonc",
	"oh-my-opencode.json",
	"package.json",
}

var OpenCodeConfigDirs = []string{
	"skills",
	"commands",
	"agents",
	"plugins",
}

var OpenCodeDataFiles = []string{
	"auth.json",
}

var DotOpenCodeFiles = []string{
	"package.json",
}

type ContainerMount struct {
	HostPath      string
	ContainerPath string
	ReadOnly      bool
}

type Manager struct {
	rootDir     string
	hostRootDir string // 宿主机上对应 rootDir 的路径，用于 Docker bind mount
}

func NewManager(dataDir string) (*Manager, error) {
	rootDir := filepath.Join(dataDir, "config")
	m := &Manager{rootDir: rootDir}

	// 当平台运行在 Docker 容器内时，bind mount 的 source 必须是宿主机路径。
	// 通过 HOST_DATA_DIR 环境变量指定宿主机上 dataDir 对应的路径。
	if hostDataDir := os.Getenv("HOST_DATA_DIR"); hostDataDir != "" {
		m.hostRootDir = filepath.Join(hostDataDir, "config")
	}

	if err := m.ensureDirs(); err != nil {
		return nil, fmt.Errorf("ensure config dirs: %w", err)
	}
	return m, nil
}

func (m *Manager) RootDir() string {
	return m.rootDir
}

func (m *Manager) ensureDirs() error {
	dirs := []string{
		filepath.Join(m.rootDir, DirOpenCodeConfig),
		filepath.Join(m.rootDir, DirOpenCodeData),
		filepath.Join(m.rootDir, DirDotOpenCode),
	}
	for _, d := range OpenCodeConfigDirs {
		dirs = append(dirs, filepath.Join(m.rootDir, DirOpenCodeConfig, d))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0750); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}
	return nil
}

func (m *Manager) GetEnvVars() (map[string]string, error) {
	p := filepath.Join(m.rootDir, FileEnvVars)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return make(map[string]string), nil
		}
		return nil, err
	}
	var env map[string]string
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("parse %s: %w", FileEnvVars, err)
	}
	return env, nil
}

func (m *Manager) SetEnvVars(env map[string]string) error {
	data, err := json.MarshalIndent(env, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(m.rootDir, FileEnvVars), data, 0600)
}

// ReadFile reads a config file by relPath (e.g. "opencode/opencode.jsonc").
// Returns empty string if file doesn't exist.
func (m *Manager) ReadFile(relPath string) (string, error) {
	p := filepath.Join(m.rootDir, relPath)
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

func (m *Manager) WriteFile(relPath string, content string) error {
	p := filepath.Join(m.rootDir, relPath)
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0600)
}

func (m *Manager) ContainerMounts() []ContainerMount {
	root := m.rootDir
	if m.hostRootDir != "" {
		root = m.hostRootDir
	}
	return []ContainerMount{
		{
			HostPath:      filepath.Join(root, DirOpenCodeConfig),
			ContainerPath: "/root/.config/opencode",
		},
		{
			HostPath:      filepath.Join(root, DirOpenCodeData),
			ContainerPath: "/root/.local/share/opencode",
		},
		{
			HostPath:      filepath.Join(root, DirDotOpenCode),
			ContainerPath: "/root/.opencode",
		},
	}
}

type ConfigFileInfo struct {
	Name    string
	RelPath string
	Hint    string
}

func (m *Manager) EditableFiles() []ConfigFileInfo {
	return []ConfigFileInfo{
		{Name: "opencode.jsonc", RelPath: filepath.Join(DirOpenCodeConfig, "opencode.jsonc"), Hint: "OpenCode 主配置（providers, MCP servers, plugins）"},
		{Name: "oh-my-opencode.json", RelPath: filepath.Join(DirOpenCodeConfig, "oh-my-opencode.json"), Hint: "Oh My OpenCode 配置（agent/category model assignments）"},
		{Name: "AGENTS.md", RelPath: filepath.Join(DirOpenCodeConfig, "AGENTS.md"), Hint: "全局 Rules：自定义指令，所有实例共享（等同 ~/.config/opencode/AGENTS.md）"},
		{Name: "auth.json", RelPath: filepath.Join(DirOpenCodeData, "auth.json"), Hint: "API 密钥和 OAuth tokens（Anthropic, OpenAI 等）"},
		{Name: "opencode package.json", RelPath: filepath.Join(DirOpenCodeConfig, "package.json"), Hint: "OpenCode plugin 依赖"},
		{Name: ".opencode package.json", RelPath: filepath.Join(DirDotOpenCode, "package.json"), Hint: "Core plugin 依赖"},
	}
}

type DirFileInfo struct {
	Name    string
	RelPath string
}

func (m *Manager) ListDirFiles(dirName string) ([]DirFileInfo, error) {
	dirPath := filepath.Join(m.rootDir, DirOpenCodeConfig, dirName)
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var files []DirFileInfo
	for _, e := range entries {
		if e.IsDir() {
			skillFile := filepath.Join(dirName, e.Name(), "SKILL.md")
			absSkill := filepath.Join(m.rootDir, DirOpenCodeConfig, skillFile)
			if _, err := os.Stat(absSkill); err == nil {
				files = append(files, DirFileInfo{
					Name:    e.Name() + "/SKILL.md",
					RelPath: filepath.Join(DirOpenCodeConfig, skillFile),
				})
			}
			continue
		}
		files = append(files, DirFileInfo{
			Name:    e.Name(),
			RelPath: filepath.Join(DirOpenCodeConfig, dirName, e.Name()),
		})
	}
	return files, nil
}

func (m *Manager) DeleteFile(relPath string) error {
	p := filepath.Join(m.rootDir, relPath)
	if err := os.Remove(p); err != nil {
		return err
	}
	dir := filepath.Dir(p)
	entries, _ := os.ReadDir(dir)
	if len(entries) == 0 {
		_ = os.Remove(dir)
	}
	return nil
}
