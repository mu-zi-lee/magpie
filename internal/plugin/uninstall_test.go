package plugin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yetone/magpie/internal/catalog"
	"github.com/yetone/magpie/internal/settings"
)

func cleanupPlugin(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	for name, content := range map[string]string{
		"package.json": `{"name":"cleanup-fixture","type":"module","main":"index.mjs","magpie":{"uninstall":"./cleanup.mjs"}}`,
		"index.mjs":    "export default async function () { throw new Error('factory must not run on uninstall') }",
		"cleanup.mjs":  body,
	} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return dir
}

func TestRemoveDisabledPluginRunsDeclaredCleanupBeforeRemovingEntry(t *testing.T) {
	sandbox(t)
	dir := cleanupPlugin(t, `import fs from 'node:fs'; import path from 'node:path';
export default async function(input, options) {
 const list=JSON.parse(fs.readFileSync(path.join(input.directory,'plugins.json')));
 if(!list.plugins.some(p=>p.spec===options.spec)) throw new Error('entry removed before cleanup');
 if(!fs.existsSync(new URL('./package.json',import.meta.url))) throw new Error('package deleted too soon');
 fs.rmSync(options.owned,{recursive:true,force:true});
 fs.writeFileSync(path.join(input.directory,'cleaned.txt'),input.reason);
}`)
	owned := filepath.Join(settings.Dir(), "owned")
	if err := os.MkdirAll(owned, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(owned, "runtime"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := save(List{Plugins: []Entry{{Spec: dir, Off: true, Options: map[string]any{"spec": dir, "owned": owned}}}}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(context.Background(), dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(owned); !os.IsNotExist(err) {
		t.Fatalf("uninstall left downloaded runtime: %v", err)
	}
	if len(Load().Plugins) != 0 {
		t.Fatal("successful cleanup did not remove plugin entry")
	}
	if b, err := os.ReadFile(filepath.Join(settings.Dir(), "cleaned.txt")); err != nil || string(b) != "uninstall" {
		t.Fatalf("cleanup context: %s, %v", b, err)
	}
}

func TestRemoveCleanupFailureKeepsPluginForRetry(t *testing.T) {
	sandbox(t)
	dir := cleanupPlugin(t, "export default async function () { throw new Error('private-token-fixture') }")
	if err := save(List{Plugins: []Entry{{Spec: dir, Off: true}}}); err != nil {
		t.Fatal(err)
	}
	err := Remove(context.Background(), dir)
	if err == nil {
		t.Fatal("cleanup failure was reported as successful removal")
	}
	if strings.Contains(err.Error(), "private-token-fixture") {
		t.Fatal("module exception leaked into removal error")
	}
	if len(Load().Plugins) != 1 {
		t.Fatal("cleanup failure removed plugin entry")
	}
	if err := os.WriteFile(filepath.Join(dir, "cleanup.mjs"), []byte("export default async function () {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Remove(context.Background(), dir); err != nil {
		t.Fatalf("cleanup retry: %v", err)
	}
}

func TestRemoveCleanupHonorsCancellation(t *testing.T) {
	sandbox(t)
	dir := cleanupPlugin(t, "export default async function () { await new Promise(() => {}); }")
	if err := save(List{Plugins: []Entry{{Spec: dir, Off: true}}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := Remove(ctx, dir); err == nil {
		t.Fatal("cancelled cleanup removed plugin")
	}
	if len(Load().Plugins) != 1 {
		t.Fatal("cancelled removal discarded retryable entry")
	}
}

func TestRemovePackageFailureKeepsEntryAfterCleanup(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX Bun wrapper")
	}
	sandbox(t)
	source := cleanupPlugin(t, "export default async function () {}")
	spec := "cleanup-fixture@1.0.0"
	target := Target(spec)
	if err := os.MkdirAll(target, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"package.json", "index.mjs", "cleanup.mjs"} {
		b, err := os.ReadFile(filepath.Join(source, name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(target, name), b, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	bun, err := Bun(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	wrapper := filepath.Join(t.TempDir(), "bun")
	quoted := "'" + strings.ReplaceAll(bun, "'", "'\\''") + "'"
	script := "#!/bin/sh\nif [ \"$1\" = remove ]; then exit 9; fi\nexec " + quoted + " \"$@\"\n"
	if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("MAGPIE_BUN", wrapper)
	if err := save(List{Plugins: []Entry{{Spec: spec, Off: true}}}); err != nil {
		t.Fatal(err)
	}
	if err := Remove(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "removing plugin package") {
		t.Fatalf("package removal failure: %v", err)
	}
	if len(Load().Plugins) != 1 {
		t.Fatal("package removal failure lost the retryable entry")
	}
	if _, err := os.Stat(filepath.Join(target, "cleanup.mjs")); err != nil {
		t.Fatal("failure unexpectedly discarded the cleanup module", err)
	}
}

func TestRemoveDisposesLivePluginBeforeStaticCleanup(t *testing.T) {
	sandbox(t)
	dir := cleanupPlugin(t, `import fs from 'node:fs'; import path from 'node:path';
export default async function(input) {
 if(!fs.existsSync(path.join(input.directory,'disposed.txt'))) throw new Error('live resource not disposed');
 fs.writeFileSync(path.join(input.directory,'cleaned.txt'),'done');
}`)
	index := `import fs from 'node:fs'; import path from 'node:path';
export default async function(input) { return { lifecycle: { async dispose() { fs.writeFileSync(path.join(input.directory,'disposed.txt'),'done'); } } }; }`
	if err := os.WriteFile(filepath.Join(dir, "index.mjs"), []byte(index), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(settings.Dir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(catalog.CachePath()), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(catalog.CachePath(), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	other := cleanupPlugin(t, "export default async function () {}")
	if err := os.WriteFile(filepath.Join(other, "index.mjs"), []byte(`export default async function() { return { lifecycle: { dispose() { throw new Error('unrelated plugin was disposed'); } } }; }`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := save(List{Plugins: []Entry{{Spec: dir}, {Spec: other}}}); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := Plugins(ctx); err != nil {
		t.Fatal(err)
	}
	if err := Remove(ctx, dir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(settings.Dir(), "cleaned.txt")); err != nil {
		t.Fatal(err)
	}
	if rows := Load().Plugins; len(rows) != 1 || rows[0].Spec != other {
		t.Fatal("removal reached an unrelated plugin entry")
	}
}

func TestUninstallDeclarationStaysWithinPackage(t *testing.T) {
	parent := t.TempDir()
	outside := filepath.Join(parent, "outside.mjs")
	if err := os.WriteFile(outside, []byte("export default async function() {}"), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, entry := range []any{true, "../outside.mjs", outside, "", "./link.mjs"} {
		t.Run(strings.TrimSpace(strings.ReplaceAll(string(mustJSON(t, entry)), "/", "_")), func(t *testing.T) {
			dir, err := os.MkdirTemp(parent, "package-")
			if err != nil {
				t.Fatal(err)
			}
			if entry == "./link.mjs" {
				if err := os.Symlink(outside, filepath.Join(dir, "link.mjs")); err != nil {
					t.Skip("symlinks unavailable:", err)
				}
			}
			pkg := map[string]any{"magpie": map[string]any{"uninstall": entry}}
			if err := os.WriteFile(filepath.Join(dir, "package.json"), mustJSON(t, pkg), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := uninstallEntry(dir); err == nil {
				t.Fatalf("unsafe uninstall entry accepted: %v", entry)
			}
		})
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	b, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
