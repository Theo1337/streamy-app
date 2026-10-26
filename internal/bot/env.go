package bot

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var envKeyPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// FindEnvFile returns the .env file to use. An explicit path wins. Otherwise it
// looks beside the executable (the installed layout is
// <app>\stack\bot\.env), then walks up towards the repository root so a
// development build finds stream-site\bot\.env, and finally checks the working
// directory.
func FindEnvFile(explicit string) string {
	if path := strings.TrimSpace(explicit); path != "" {
		return path
	}

	var candidates []string
	if executable, err := os.Executable(); err == nil {
		dir := filepath.Dir(executable)
		for depth := 0; depth < 6; depth++ {
			candidates = append(candidates,
				filepath.Join(dir, "stack", "bot", ".env"),
				filepath.Join(dir, "bot", ".env"),
				filepath.Join(dir, ".env"),
				filepath.Join(dir, "stream-site", "bot", ".env"),
			)
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	if cwd, err := os.Getwd(); err == nil {
		candidates = append(candidates,
			filepath.Join(cwd, "stream-site", "bot", ".env"),
			filepath.Join(cwd, "bot", ".env"),
			filepath.Join(cwd, ".env"),
		)
	}

	for _, candidate := range candidates {
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() {
			return candidate
		}
	}
	return ""
}

// LoadEnvFile reads KEY=VALUE pairs into the process environment. Variables
// already present win, so an inline DISCORD_TOKEN=... overrides the file. The
// parsing rules match the previous Node implementation.
func LoadEnvFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		separator := strings.IndexByte(line, '=')
		if separator <= 0 {
			continue
		}
		key := strings.TrimSpace(line[:separator])
		if !envKeyPattern.MatchString(key) {
			continue
		}
		value := strings.TrimSpace(line[separator+1:])
		if len(value) >= 2 && (value[0] == '"' || value[0] == '\'') && value[len(value)-1] == value[0] {
			value = value[1 : len(value)-1]
		}
		if _, exists := os.LookupEnv(key); exists {
			continue
		}
		if err := os.Setenv(key, value); err != nil {
			return fmt.Errorf("set %s: %w", key, err)
		}
	}
	return scanner.Err()
}
