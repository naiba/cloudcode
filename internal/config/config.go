package config

import (
	_ "embed"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

//go:embed plugins/_cloudcode-instructions.md
var instructionsFile []byte

const instructionsFileName = "_cloudcode-instructions.md"

const (
	DirOpenCodeConfig = "opencode"      // → /root/.config/opencode/
	DirOpenCodeData   = "opencode-data" // → /root/.local/share/opencode/
	DirDotOpenCode    = "dot-opencode"  // → /root/.opencode/
	DirAgentsSkills   = "agents-skills" // → /root/.agents/
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

// Pre-compiled regexes for JSONC comment stripping.
var (
	reStripBlock         = regexp.MustCompile(`(?s)/\*.*?\*/`)
	reStripTrailingComma = regexp.MustCompile(`,\s*([}\]])`)
	// reTokenize splits JSONC into string literals and non-string segments so we
	// can strip comments only from non-string parts (handles URLs like
	// "https://example.com" without corrupting them).
	reTokenize = regexp.MustCompile(`("(?:[^"\\]|\\.)*")|(//.*)`)
)

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

// #1: containedPath resolves relPath under rootDir and verifies it doesn't escape.
func (m *Manager) containedPath(relPath string) (string, error) {
	// Clean the path to remove any ".." components before joining
	cleaned := filepath.Join(m.rootDir, filepath.Clean("/"+relPath))
	abs, err := filepath.Abs(cleaned)
	if err != nil {
		return "", fmt.Errorf("resolve path: %w", err)
	}
	root := m.rootDir
	if !strings.HasSuffix(root, string(filepath.Separator)) {
		root += string(filepath.Separator)
	}
	if abs != filepath.Clean(m.rootDir) && !strings.HasPrefix(abs, root) {
		return "", fmt.Errorf("path %q escapes config root", relPath)
	}
	return abs, nil
}

func (m *Manager) ensureDirs() error {
	dirs := []string{
		filepath.Join(m.rootDir, DirOpenCodeConfig),
		filepath.Join(m.rootDir, DirOpenCodeData),
		filepath.Join(m.rootDir, DirDotOpenCode),
		filepath.Join(m.rootDir, DirAgentsSkills),
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

	if err := m.ensureInstructionsFile(); err != nil {
		return fmt.Errorf("ensure instructions file: %w", err)
	}

	return nil
}

func (m *Manager) ensureInstructionsFile() error {
	path := filepath.Join(m.rootDir, DirOpenCodeConfig, instructionsFileName)
	if err := os.WriteFile(path, instructionsFile, 0640); err != nil {
		return fmt.Errorf("write instructions file: %w", err)
	}
	return m.ensureInstruction("/root/.config/opencode/" + instructionsFileName)
}

func (m *Manager) ensureInstruction(filename string) error {
	configPath := filepath.Join(m.rootDir, DirOpenCodeConfig, "opencode.jsonc")
	raw, err := os.ReadFile(configPath)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read opencode.jsonc: %w", err)
	}

	content := string(raw)

	// Build per-call regex for this specific filename (#12: the filename varies)
	refRe := regexp.MustCompile(`["']` + regexp.QuoteMeta(filename) + `["']`)
	if refRe.MatchString(content) {
		return nil
	}

	stripped := stripJSONCComments(content)

	var cfg map[string]any
	if len(stripped) > 0 {
		if err := json.Unmarshal([]byte(stripped), &cfg); err != nil {
			// #25: log a warning instead of silently skipping
			log.Printf("Warning: opencode.jsonc is malformed, skipping instruction injection: %v", err)
			return nil
		}
	} else {
		cfg = make(map[string]any)
	}

	var instructions []any
	if existing, ok := cfg["instructions"]; ok {
		if arr, ok := existing.([]any); ok {
			instructions = arr
		}
	}
	instructions = append(instructions, filename)
	cfg["instructions"] = instructions

	out, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal opencode.jsonc: %w", err)
	}
	return os.WriteFile(configPath, out, 0640)
}

// stripJSONCComments removes // line comments and /* */ block comments from
// JSONC content. String literals are preserved intact (handles URLs like
// "https://example.com" without stripping the // inside them).
func stripJSONCComments(s string) string {
	// First strip block comments (not string-aware, but block comments rarely
	// appear inside string values in practice).
	s = reStripBlock.ReplaceAllString(s, "")
	// Replace each token: keep string literals, drop // comments.
	s = reTokenize.ReplaceAllStringFunc(s, func(match string) string {
		if strings.HasPrefix(match, `"`) {
			return match // string literal — preserve as-is
		}
		return "" // // comment — remove
	})
	s = reStripTrailingComma.ReplaceAllString(s, "$1")
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
// #1: validates path stays within rootDir.
func (m *Manager) ReadFile(relPath string) (string, error) {
	p, err := m.containedPath(relPath)
	if err != nil {
		return "", err
	}
	data, err := os.ReadFile(p)
	if err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	}
	return string(data), nil
}

// WriteFile writes content to a config file.
// #1: validates path stays within rootDir.
func (m *Manager) WriteFile(relPath string, content string) error {
	p, err := m.containedPath(relPath)
	if err != nil {
		return err
	}
	dir := filepath.Dir(p)
	if err := os.MkdirAll(dir, 0750); err != nil {
		return err
	}
	return os.WriteFile(p, []byte(content), 0600)
}

func (m *Manager) ContainerMountsForInstance(instanceID string) ([]ContainerMount, error) {
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

	return []ContainerMount{
		{
			HostPath:      filepath.Join(root, DirOpenCodeConfig),
			ContainerPath: "/root/.config/opencode",
		},
		{
			HostPath:      filepath.Join(root, DirOpenCodeData, "auth.json"),
			ContainerPath: "/root/.local/share/opencode/auth.json",
		},
		{
			HostPath:      filepath.Join(root, DirDotOpenCode),
			ContainerPath: "/root/.opencode",
		},
		{
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

// validDirNames is the set of directory names that clients are allowed to list.
var validDirNames = map[string]bool{
	"commands": true,
	"agents":   true,
	"skills":   true,
	"plugins":  true,
}

func (m *Manager) ListDirFiles(dirName string) ([]DirFileInfo, error) {
	if !validDirNames[dirName] {
		return nil, fmt.Errorf("invalid directory name: %q", dirName)
	}
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

func (m *Manager) ReadAgentsSkillFile(relPath string) (string, error) {
	// #1: validate path
	p, err := m.containedPath(relPath)
	if err != nil {
		return "", err
	}
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
// #1: validates the skill name doesn't escape the skills directory.
func (m *Manager) DeleteAgentsSkill(skillName string) error {
	// Validate: skill name must not contain path separators
	if strings.ContainsAny(skillName, "/\\") || skillName == ".." || skillName == "." {
		return fmt.Errorf("invalid skill name: %q", skillName)
	}
	p := filepath.Join(m.rootDir, DirAgentsSkills, "skills", skillName)
	return os.RemoveAll(p)
}

// DeleteFile deletes a config file by relPath.
// #1: validates path stays within rootDir.
func (m *Manager) DeleteFile(relPath string) error {
	p, err := m.containedPath(relPath)
	if err != nil {
		return err
	}
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
