package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

//go:embed plugins/_cloudcode-telegram.ts
var telegramPlugin []byte

//go:embed plugins/_cloudcode-prompt-watchdog.ts
var promptWatchdogPlugin []byte

//go:embed plugins/_cloudcode-instructions.md
var instructionsFile []byte
const instructionsFileName = "_cloudcode-instructions.md"

const (
	DirOpenCodeConfig = "opencode"      // → /root/.config/opencode/
	DirOpenCodeData   = "opencode-data" // → /root/.local/share/opencode/
	DirDotOpenCode    = "dot-opencode"  // → /root/.opencode/
	DirAgentsSkills   = "agents-skills" // → /root/.agents/ (contains skills/ subdir and .skill-lock.json)
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
	hostRootDir string
}

func NewManager(dataDir string) (*Manager, error) {
	rootDir := filepath.Join(dataDir, "config")
	m := &Manager{rootDir: rootDir}

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
		filepath.Join(m.rootDir, DirAgentsSkills),
		// skills.sh 安装的技能存放在 skills/ 子目录，.skill-lock.json 在父目录
		filepath.Join(m.rootDir, DirAgentsSkills, "skills"),
	}
	for _, d := range OpenCodeConfigDirs {
		dirs = append(dirs, filepath.Join(m.rootDir, DirOpenCodeConfig, d))
	}
	for _, d := range dirs {
		if err := os.MkdirAll(d, 0750); err != nil {
			return fmt.Errorf("mkdir %s: %w", d, err)
		}
	}


	pluginPath := filepath.Join(m.rootDir, DirOpenCodeConfig, "plugins", "_cloudcode-telegram.ts")
	if err := os.WriteFile(pluginPath, telegramPlugin, 0640); err != nil {
		return fmt.Errorf("write telegram plugin: %w", err)
	}

	// 写入 prompt watchdog plugin（每次启动覆盖，确保最新版本）
	watchdogPath := filepath.Join(m.rootDir, DirOpenCodeConfig, "plugins", "_cloudcode-prompt-watchdog.ts")
	if err := os.WriteFile(watchdogPath, promptWatchdogPlugin, 0640); err != nil {
		return fmt.Errorf("write prompt watchdog plugin: %w", err)
	}

	if err := m.ensureInstructionsFile(); err != nil {
		return fmt.Errorf("ensure instructions file: %w", err)
	}

	return nil
}


// ensureInstructionsFile writes the CloudCode instructions as a standalone
// instruction file and ensures opencode.jsonc references it via the
// "instructions" field. This avoids modifying AGENTS.md directly.
func (m *Manager) ensureInstructionsFile() error {
	// Write the standalone instruction file (overwrite every start, like telegram plugin)
	path := filepath.Join(m.rootDir, DirOpenCodeConfig, instructionsFileName)
	if err := os.WriteFile(path, instructionsFile, 0640); err != nil {
		return fmt.Errorf("write instructions file: %w", err)
	}

	// Use absolute container path so opencode resolves it regardless of project dir
	return m.ensureInstruction("/root/.config/opencode/" + instructionsFileName)
}

// ensureInstruction makes sure the given filename is listed in the
// "instructions" array of opencode.jsonc. If the file doesn't exist or
// has no instructions field, it is created/added. Existing content is
// preserved; only the instructions array is patched.
func (m *Manager) ensureInstruction(filename string) error {
	configPath := filepath.Join(m.rootDir, DirOpenCodeConfig, "opencode.jsonc")
	raw, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read opencode.jsonc: %w", err)
	}

	content := string(raw)

	// Quick check: already referenced?
	if regexp.MustCompile(`["']` + regexp.QuoteMeta(filename) + `["']`).MatchString(content) {
		return nil
	}

	// Strip JSONC comments for parsing, but preserve original for editing
	stripped := stripJSONCComments(content)

	var cfg map[string]any
	if len(stripped) > 0 {
		if err := json.Unmarshal([]byte(stripped), &cfg); err != nil {
			// Malformed config; don't break it, just skip
			return nil
		}
	} else {
		cfg = make(map[string]any)
	}

	// Patch instructions array
	var instructions []any
	if existing, ok := cfg["instructions"]; ok {
		if arr, ok := existing.([]any); ok {
			instructions = arr
		}
	}
	instructions = append(instructions, filename)
	cfg["instructions"] = instructions

	// Write back as formatted JSON (comments are lost, but this is a
	// machine-managed global config, not a hand-crafted project config)
	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal opencode.jsonc: %w", err)
	}
	return os.WriteFile(configPath, out, 0640)
}


// stripJSONCComments removes // and /* */ comments from JSONC content.
func stripJSONCComments(s string) string {
	// Remove single-line comments (not inside strings)
	re := regexp.MustCompile(`(?m)^(\s*)//.*$`)
	s = re.ReplaceAllString(s, "$1")
	// Remove inline comments after values (simplistic but sufficient for config files)
	re2 := regexp.MustCompile(`("[^"]*"|[^/])//.*$`)
	s = re2.ReplaceAllString(s, "$1")
	// Remove block comments
	re3 := regexp.MustCompile(`(?s)/\*.*?\*/`)
	s = re3.ReplaceAllString(s, "")
	// Handle trailing commas before } or ]
	re4 := regexp.MustCompile(`,\s*([}\]])`)
	s = re4.ReplaceAllString(s, "$1")
	return s
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

func (m *Manager) ContainerMountsForInstance(instanceID string) ([]ContainerMount, error) {
	// Ensure global auth.json exists (for bind mount)
	globalAuth := filepath.Join(m.rootDir, DirOpenCodeData, "auth.json")
	if _, err := os.Stat(globalAuth); os.IsNotExist(err) {
		if err := os.WriteFile(globalAuth, []byte("{}\n"), 0600); err != nil {
			return nil, fmt.Errorf("create auth.json: %w", err)
		}
	}
	root := m.rootDir
	if m.hostRootDir != "" {
		root = m.hostRootDir
	}

	// Session data lives in the named volume (cloudcode-home-{id}) at /root.
	// Only global configs and auth.json are bind-mounted.
	return []ContainerMount{
		{
			HostPath:      filepath.Join(root, DirOpenCodeConfig),
			ContainerPath: "/root/.config/opencode",
		},
		{
			// Global auth.json shared across all instances
			HostPath:      filepath.Join(root, DirOpenCodeData, "auth.json"),
			ContainerPath: "/root/.local/share/opencode/auth.json",
		},
		{
			HostPath:      filepath.Join(root, DirDotOpenCode),
			ContainerPath: "/root/.opencode",
		},
		{
			// 整个 .agents 目录：包含 skills/ 子目录和 .skill-lock.json（skills update -g 需要）
			HostPath:      filepath.Join(root, DirAgentsSkills),
			ContainerPath: "/root/.agents",
		},
	}, nil
}

func (m *Manager) RemoveInstanceData(instanceID string) {
	instDir := filepath.Join(m.rootDir, "instances", instanceID)
	_ = os.RemoveAll(instDir)
}

type ConfigFileInfo struct {
	Name    string
	RelPath string
	Hint    string
}

func (m *Manager) EditableFiles() []ConfigFileInfo {
	return []ConfigFileInfo{
		{Name: "opencode.jsonc", RelPath: filepath.Join(DirOpenCodeConfig, "opencode.jsonc"), Hint: "OpenCode main config (providers, MCP servers, plugins)"},
		{Name: "oh-my-opencode.json", RelPath: filepath.Join(DirOpenCodeConfig, "oh-my-opencode.json"), Hint: "Oh My OpenCode config (agent/category model assignments)"},
		{Name: "AGENTS.md", RelPath: filepath.Join(DirOpenCodeConfig, "AGENTS.md"), Hint: "Global rules shared across all instances (~/.config/opencode/AGENTS.md)"},
		{Name: "auth.json", RelPath: filepath.Join(DirOpenCodeData, "auth.json"), Hint: "API keys and OAuth tokens (Anthropic, OpenAI, etc.)"},
		{Name: "~/.config/opencode/package.json", RelPath: filepath.Join(DirOpenCodeConfig, "package.json"), Hint: "OpenCode plugin dependencies"},
		{Name: "~/.opencode/package.json", RelPath: filepath.Join(DirDotOpenCode, "package.json"), Hint: "Core plugin dependencies"},
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

type AgentsSkillInfo struct {
	SkillName string
	RelPath   string
}

func (m *Manager) ListAgentsSkills() ([]AgentsSkillInfo, error) {
	dirPath := filepath.Join(m.rootDir, DirAgentsSkills, "skills")
	entries, err := os.ReadDir(dirPath)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}

	var skills []AgentsSkillInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		skillFile := filepath.Join(e.Name(), "SKILL.md")
		absSkill := filepath.Join(dirPath, skillFile)
		if _, err := os.Stat(absSkill); err == nil {
			skills = append(skills, AgentsSkillInfo{
				SkillName: e.Name(),
				RelPath:   filepath.Join(DirAgentsSkills, "skills", skillFile),
			})
		}
	}
	return skills, nil
}

// ReadAgentsSkillFile reads a file from the agents-skills/skills/ directory.
func (m *Manager) ReadAgentsSkillFile(relPath string) (string, error) {
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

// DeleteAgentsSkill removes an entire skill directory from agents-skills/skills/.
func (m *Manager) DeleteAgentsSkill(skillName string) error {
	p := filepath.Join(m.rootDir, DirAgentsSkills, "skills", skillName)
	return os.RemoveAll(p)
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
