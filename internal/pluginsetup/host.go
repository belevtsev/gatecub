package pluginsetup

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/belevtsev/kitsu/v3/internal/decl"
	"github.com/belevtsev/kitsu/v3/internal/proc"
)

type pluginCommand func(context.Context, ...string) ([]byte, error)

func nativePluginCommand(host, executable string) pluginCommand {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		argv := []string{"plugin"}
		if host == "codex" {
			argv = []string{"-c", "analytics.enabled=false", "-c", "check_for_update_on_startup=false", "plugin"}
		}
		env := os.Environ()
		if host == "claude" {
			env = append(env, "CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC=1", "DISABLE_AUTOUPDATER=1", "OTEL_SDK_DISABLED=true")
		}
		result := proc.Run(ctx, proc.Spec{
			Path: executable, Args: append(argv, args...), Env: env,
			StdoutLimit: manifestLimit, StderrLimit: 8 << 10,
		})
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		if result.Overflow {
			return nil, fmt.Errorf("%s plugin %s output exceeded 1 MiB", host, args[0])
		}
		err := errors.Join(result.Err, result.Cleanup)
		if err == nil {
			return result.Stdout, nil
		}
		detail := strings.TrimSpace(string(result.Stderr))
		if detail == "" {
			detail = strings.TrimSpace(string(result.Stdout))
		}
		if detail != "" {
			return nil, fmt.Errorf("%s plugin %s: %w: %s", host, args[0], err, decl.Plain(detail))
		}
		return nil, fmt.Errorf("%s plugin %s: %w", host, args[0], err)
	}
}

func installCodex(ctx context.Context, run pluginCommand, marketplace string, r release, team bool) (string, error) {
	if err := registerCodex(ctx, run, marketplace); err != nil {
		return "", err
	}
	if err := addCodex(ctx, run, marketplace, r.kitsu()); err != nil {
		return "", err
	}
	role, ok := r.team("codex")
	if !ok {
		return TeamOff, nil
	}
	current, err := codexInventory(ctx, run, role.id)
	switch {
	case err != nil:
		return "", err
	case !team && !current.Installed:
		return TeamOff, nil
	case !team:
		_, err = run(ctx, "remove", role.id)
		return TeamOff, err
	case current.Installed && !current.Enabled: // An add would enable it again.
		return TeamDisabled, nil
	}
	return TeamOn, addCodex(ctx, run, marketplace, role)
}

// addCodex installs one plugin of the marketplace and checks that Codex holds
// the files of this release and loads them from this marketplace.
func addCodex(ctx context.Context, run pluginCommand, marketplace string, p part) error {
	data, err := run(ctx, "add", "--json", p.id)
	if err != nil {
		return err
	}
	var receipt struct{ PluginID, InstalledPath string }
	if json.Unmarshal(data, &receipt) != nil || receipt.PluginID != p.id || !filepath.IsAbs(receipt.InstalledPath) {
		return errors.New("Codex did not identify the installed Kitsu directory")
	}
	if err := p.files.verify(receipt.InstalledPath); err != nil {
		return fmt.Errorf("Codex installed files do not match %s: %w", archiveName, err)
	}
	item, err := codexInventory(ctx, run, p.id)
	if err != nil {
		return err
	}
	if item.PluginID == "" {
		return fmt.Errorf("Codex did not identify one enabled installation of %s", p.id)
	}
	source := filepath.Join(marketplace, filepath.FromSlash(p.dir))
	if !item.Installed || !item.Enabled || item.Version != p.version("codex") || item.Source.Source != "local" || !sameDirectory(item.Source.Path, source) {
		return errors.New("Codex reported a different Kitsu marketplace, version or enabled state")
	}
	return nil
}

type codexItem struct {
	PluginID, Version  string
	Installed, Enabled bool
	Source             struct{ Source, Path string }
}

// codexInventory returns what Codex records for the plugin id, and an empty
// record when it records nothing.
func codexInventory(ctx context.Context, run pluginCommand, id string) (codexItem, error) {
	data, err := run(ctx, "list", "--json", "--marketplace", "kitsu")
	if err != nil {
		return codexItem{}, err
	}
	var inventory struct{ Installed []codexItem }
	if json.Unmarshal(data, &inventory) != nil {
		return codexItem{}, errors.New("Codex did not return a readable plugin inventory")
	}
	var found codexItem
	for _, item := range inventory.Installed {
		if item.PluginID != id {
			continue
		}
		if found.PluginID != "" {
			return codexItem{}, fmt.Errorf("Codex did not identify one enabled installation of %s", id)
		}
		found = item
	}
	return found, nil
}

func registerCodex(ctx context.Context, run pluginCommand, marketplace string) error {
	err := addCodexMarketplace(ctx, run, marketplace)
	if err == nil || !strings.Contains(err.Error(), "already added from a different source") {
		return err
	}
	data, err := run(ctx, "marketplace", "list", "--json")
	if err != nil {
		return err
	}
	var configured struct {
		Marketplaces []struct {
			Name              string
			MarketplaceSource struct{ SourceType, Source string }
		}
	}
	if json.Unmarshal(data, &configured) != nil {
		return errors.New("Codex did not return readable marketplace registrations")
	}
	previous := ""
	for _, entry := range configured.Marketplaces {
		if entry.Name != "kitsu" {
			continue
		}
		source := entry.MarketplaceSource
		if previous != "" || source.SourceType != "local" || !localKitsu("codex", source.Source) {
			return foreignMarketplace("Codex")
		}
		previous = source.Source
	}
	if previous == "" || sameDirectory(previous, marketplace) {
		return errors.New("Codex reported a conflicting Kitsu marketplace without a distinct local Kitsu source")
	}
	if _, err := run(ctx, "marketplace", "remove", "kitsu"); err != nil {
		return err
	}
	if err := addCodexMarketplace(ctx, run, marketplace); err != nil {
		if ctx.Err() == nil {
			if restoreErr := addCodexMarketplace(ctx, run, previous); restoreErr != nil {
				return errors.Join(err, fmt.Errorf("restore previous Codex marketplace: %w", restoreErr))
			}
		}
		return err
	}
	return nil
}

func addCodexMarketplace(ctx context.Context, run pluginCommand, directory string) error {
	data, err := run(ctx, "marketplace", "add", "--json", directory)
	if err != nil {
		return err
	}
	var receipt struct{ MarketplaceName, InstalledRoot string }
	if json.Unmarshal(data, &receipt) != nil || receipt.MarketplaceName != "kitsu" || !sameDirectory(receipt.InstalledRoot, directory) {
		return errors.New("Codex did not register the verified Kitsu marketplace directory")
	}
	return nil
}

func installClaude(ctx context.Context, run pluginCommand, marketplace string, r release, team bool) (string, error) {
	if _, err := findClaudeMarketplace(ctx, run, marketplace); err != nil {
		return "", err
	}
	if _, err := run(ctx, "marketplace", "add", marketplace); err != nil {
		return "", err
	}
	current, err := findClaudeMarketplace(ctx, run, marketplace)
	if err != nil {
		return "", err
	}
	if !sameDirectory(current, marketplace) {
		return "", errors.New("Claude did not register the verified Kitsu marketplace directory")
	}
	if err := addClaude(ctx, run, marketplace, r.kitsu()); err != nil {
		return "", err
	}
	role, ok := r.team("claude")
	if !ok {
		return TeamOff, nil
	}
	installed, err := claudeInventory(ctx, run, role.id)
	switch {
	case err != nil:
		return "", err
	case !team && installed.ID == "":
		return TeamOff, nil
	case !team:
		_, err = run(ctx, "uninstall", "--scope", "user", role.id)
		return TeamOff, err
	case installed.ID != "" && !installed.Enabled: // An install would enable it again.
		return TeamDisabled, nil
	}
	return TeamOn, addClaude(ctx, run, marketplace, role)
}

// addClaude installs one plugin of the marketplace for the user and makes
// Claude hold the files of this release.
func addClaude(ctx context.Context, run pluginCommand, marketplace string, p part) error {
	if _, err := run(ctx, "install", p.id); err != nil {
		return err
	}
	return ensureClaudeFiles(ctx, run, marketplace, p)
}

func findClaudeMarketplace(ctx context.Context, run pluginCommand, marketplace string) (string, error) {
	data, err := run(ctx, "marketplace", "list", "--json")
	if err != nil {
		return "", err
	}
	var configured []struct{ Name, Source, Path, InstallLocation string }
	if json.Unmarshal(data, &configured) != nil || configured == nil {
		return "", errors.New("Claude did not return readable marketplace registrations")
	}
	current := ""
	for _, entry := range configured {
		if entry.Name != "kitsu" {
			continue
		}
		knownLocal := sameDirectory(entry.Path, marketplace) || localKitsu("claude", entry.Path)
		if current != "" || entry.Source != "directory" || !knownLocal || !sameDirectory(entry.Path, entry.InstallLocation) {
			return "", foreignMarketplace("Claude")
		}
		current = entry.Path
	}
	return current, nil
}

type claudeInstallation struct {
	ID, Scope, Version, InstallPath, ReadFromFolder string
	Enabled                                         bool
}

// claudeInventory returns the user installation that Claude records for the
// plugin id, and an empty record when it records none.
func claudeInventory(ctx context.Context, run pluginCommand, id string) (claudeInstallation, error) {
	data, err := run(ctx, "list", "--json")
	if err != nil {
		return claudeInstallation{}, err
	}
	var installs []claudeInstallation
	if json.Unmarshal(data, &installs) != nil {
		return claudeInstallation{}, errors.New("Claude did not return a readable plugin inventory")
	}
	var selected claudeInstallation
	for _, install := range installs {
		if install.ID != id || install.Scope != "user" {
			continue
		}
		if selected.ID != "" {
			return claudeInstallation{}, fmt.Errorf("Claude did not identify one enabled user installation of %s", id)
		}
		selected = install
	}
	return selected, nil
}

func findClaudePlugin(ctx context.Context, run pluginCommand, marketplace string, p part) (claudeInstallation, error) {
	selected, err := claudeInventory(ctx, run, p.id)
	switch {
	case err != nil:
		return claudeInstallation{}, err
	case selected.ID == "":
		return selected, fmt.Errorf("Claude did not identify an enabled user installation of %s", p.id)
	case !selected.Enabled || !filepath.IsAbs(selected.InstallPath):
		return claudeInstallation{}, fmt.Errorf("Claude did not identify one enabled user installation of %s", p.id)
	case selected.ReadFromFolder != "" && !sameDirectory(selected.ReadFromFolder, filepath.Join(marketplace, filepath.FromSlash(p.dir))):
		return claudeInstallation{}, errors.New("Claude is loading Kitsu from a different marketplace directory")
	}
	return selected, nil
}

func ensureClaudeFiles(ctx context.Context, run pluginCommand, marketplace string, p part) error {
	installed, err := findClaudePlugin(ctx, run, marketplace, p)
	if err != nil {
		return err
	}
	if p.files.verify(installed.InstallPath) == nil {
		return nil
	}
	// Claude's local runtime may load from the new source while its inventory
	// still points to old bytes. Refresh the installed copy for offline readers.
	if installed.Version != p.version("claude") {
		if _, err := run(ctx, "update", "--scope", "user", p.id); err != nil {
			return err
		}
	} else {
		// Native update short-circuits when both builds declare the same version.
		if _, err := run(ctx, "uninstall", "--keep-data", "--scope", "user", p.id); err != nil {
			return err
		}
		if _, err := run(ctx, "install", p.id); err != nil {
			return err
		}
	}
	installed, err = findClaudePlugin(ctx, run, marketplace, p)
	if err != nil {
		return err
	}
	if err := p.files.verify(installed.InstallPath); err != nil {
		return fmt.Errorf("Claude installed files do not match %s: %w", archiveName, err)
	}
	return nil
}

func foreignMarketplace(host string) error {
	return fmt.Errorf("%s already uses the name kitsu for another marketplace; review that registration before replacing it", host)
}

func localKitsu(host, directory string) bool {
	if !filepath.IsAbs(directory) {
		return false
	}
	market, err := readRegular(filepath.Join(directory, filepath.FromSlash(marketManifest(host))), manifestLimit)
	if err != nil || !isKitsuMarketplace(host, market) {
		return false
	}
	manifest, err := readRegular(filepath.Join(directory, filepath.FromSlash(pluginManifest(host))), manifestLimit)
	return err == nil && pluginVersion(manifest, "kitsu") != ""
}

func sameDirectory(a, b string) bool {
	if !filepath.IsAbs(a) || !filepath.IsAbs(b) {
		return false
	}
	first, err := os.Stat(a)
	if err != nil || !first.IsDir() {
		return false
	}
	second, err := os.Stat(b)
	return err == nil && os.SameFile(first, second)
}
