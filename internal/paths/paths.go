// Package paths centralizes every filesystem location Voodu uses on the server.
// The root can be overridden via VOODU_ROOT env var (default: /opt/voodu).
package paths

import (
	"os"
	"path/filepath"
)

const (
	EnvRoot     = "VOODU_ROOT"
	DefaultRoot = "/opt/voodu"
	UserRCFile  = ".voodurc"
	UserCfgDir  = ".voodu"
	ConfigFile  = "config.yml"
	RemoteName  = "voodu"
)

// Root returns the Voodu server root directory. Honors VOODU_ROOT.
func Root() string {
	if v := os.Getenv(EnvRoot); v != "" {
		return v
	}

	return DefaultRoot
}

func AppsDir() string     { return filepath.Join(Root(), "apps") }
func ServicesDir() string { return filepath.Join(Root(), "services") }
func PluginsDir() string  { return filepath.Join(Root(), "plugins") }
func ScriptsDir() string  { return filepath.Join(Root(), "scripts") }
func StateDir() string    { return filepath.Join(Root(), "state") }
func VolumesDir() string  { return filepath.Join(Root(), "volumes") }
func AssetsDir() string   { return filepath.Join(Root(), "assets") }
func CacheDir() string    { return filepath.Join(Root(), "cache") }

// AssetDir is the materialised on-disk root for one asset
// manifest (`asset "<scope>" "<name>" { … }`). Each file key
// in the spec lands as a sibling file under this directory:
// `<assets_root>/<scope>/<name>/<file_key>`. Resources that
// interpolate `${asset.<name>.<file_key>}` resolve to the full
// path of one of those files.
func AssetDir(scope, name string) string {
	return filepath.Join(AssetsDir(), scope, name)
}

// AssetFile returns the materialised path for a single
// (scope, name, key) tuple. Convention is one file per key —
// the key itself is the filename, no extension auto-applied.
// Operators control the in-container filename via the `volumes`
// mount target: `${asset.X.Y}:/etc/svc/conf.yaml:ro` mounts the
// host file (whatever name) at `conf.yaml` inside the
// container.
func AssetFile(scope, name, key string) string {
	return filepath.Join(AssetDir(scope, name), key)
}

func AppDir(app string) string         { return filepath.Join(AppsDir(), app) }
func AppReleasesDir(app string) string { return filepath.Join(AppDir(app), "releases") }
func AppRelease(app string) string     { return filepath.Join(AppDir(app), "releases") }
func AppCurrentLink(app string) string { return filepath.Join(AppDir(app), "current") }
func AppSharedDir(app string) string   { return filepath.Join(AppDir(app), "shared") }
func AppEnvFile(app string) string     { return filepath.Join(AppSharedDir(app), ".env") }
func AppVolumeDir(app string) string   { return filepath.Join(VolumesDir(), app) }
func AppContainersDir(app string) string {
	return filepath.Join(AppDir(app), "containers")
}

// UserCfgPath returns ~/.voodu/config.yml (client-side CLI config).
func UserCfgPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, UserCfgDir, ConfigFile)
}

// UserRCPath returns ~/.voodurc (mode flag file).
func UserRCPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, UserRCFile)
}
