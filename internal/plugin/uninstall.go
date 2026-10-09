package plugin

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yetone/magpie/internal/settings"
)

//go:embed uninstall.js
var uninstallJS []byte

var uninstalling = struct {
	sync.RWMutex
	specs map[string]bool
}{specs: map[string]bool{}}

func isUninstalling(spec string) bool {
	uninstalling.RLock()
	defer uninstalling.RUnlock()
	return uninstalling.specs[spec]
}

// uninstallEntry is an explicitly declared module, not a package manager script
// or the plugin factory. Disabled plugins can clean up without starting services.
func uninstallEntry(target string) (string, error) {
	root := target
	if info, err := os.Stat(root); err != nil {
		if os.IsNotExist(err) {
			return "", nil
		}
		return "", err
	} else if !info.IsDir() {
		root = filepath.Dir(root)
	}
	bytes, err := os.ReadFile(filepath.Join(root, "package.json"))
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	var pkg struct {
		Magpie struct {
			Uninstall json.RawMessage `json:"uninstall"`
		} `json:"magpie"`
	}
	if err := json.Unmarshal(bytes, &pkg); err != nil {
		return "", fmt.Errorf("reading uninstall declaration: %w", err)
	}
	if len(pkg.Magpie.Uninstall) == 0 || string(pkg.Magpie.Uninstall) == "null" {
		return "", nil
	}
	var entry string
	if err := json.Unmarshal(pkg.Magpie.Uninstall, &entry); err != nil || strings.TrimSpace(entry) == "" {
		return "", fmt.Errorf("magpie.uninstall must name a module within the package")
	}
	if filepath.IsAbs(entry) {
		return "", fmt.Errorf("magpie.uninstall must be relative to the package")
	}
	root, err = filepath.EvalSymlinks(root)
	if err != nil {
		return "", err
	}
	root, err = filepath.Abs(root)
	if err != nil {
		return "", err
	}
	file, err := filepath.EvalSymlinks(filepath.Join(root, filepath.FromSlash(entry)))
	if err != nil {
		return "", fmt.Errorf("reading uninstall module: %w", err)
	}
	rel, err := filepath.Rel(root, file)
	if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("magpie.uninstall must stay within the package")
	}
	info, err := os.Stat(file)
	if err != nil || !info.Mode().IsRegular() {
		return "", fmt.Errorf("magpie.uninstall must name a regular module")
	}
	return file, nil
}

// beginUninstall prevents another host generation from starting the plugin
// until its removal has committed or failed and the host is restarted.
func beginUninstall(spec string) func() {
	uninstalling.Lock()
	uninstalling.specs[spec] = true
	uninstalling.Unlock()
	return func() { uninstalling.Lock(); delete(uninstalling.specs, spec); uninstalling.Unlock() }
}

// uninstall runs before the entry or its package disappears. A failure leaves
// the plugin installed, so its cleanup can be retried instead of silently lost.
func uninstall(ctx context.Context, e Entry) error {
	entry, err := uninstallEntry(Target(e.Spec))
	if err != nil || entry == "" {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	// Drain this plugin's resources in every host generation still running. Other
	// plugins and their streaming replies are left alone.
	hostMu.Lock()
	hs := make([]*host, 0, len(retiring)+1)
	if current != nil {
		hs = append(hs, current)
	}
	for h := range retiring {
		hs = append(hs, h)
	}
	hostMu.Unlock()
	for _, h := range hs {
		if !h.alive() {
			continue
		}
		for _, p := range h.loaded {
			if p.Spec != e.Spec {
				continue
			}
			if err := h.call(ctx, "disposePlugin", map[string]any{"spec": e.Spec}, nil); err != nil {
				return fmt.Errorf("stopping plugin resources failed")
			}
			break
		}
	}
	bun, err := Bun(ctx)
	if err != nil {
		return err
	}
	script, err := hostScript("uninstall", uninstallJS)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"entry": entry, "directory": settings.Dir(), "options": e.Options})
	if err != nil {
		return err
	}
	cmd := bunCommand(ctx, bun, settings.Dir(), "run", script)
	cmd.Env = hostEnv(cmd.Env)
	cmd.Stdin = bytes.NewReader(body)
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("plugin uninstall cleanup failed: %w", err)
	}
	return nil
}
