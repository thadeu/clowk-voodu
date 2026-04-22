// Package config defines the voodu.yml schema and loaders for M1.
// M4 introduces the HCL multi-kind format; this YAML shape is the transitional
// format inherited from Gokku so existing apps keep running during the port.
package config

// ServerConfig represents the voodu.yml on the server.
type ServerConfig struct {
	Apps         map[string]App `yaml:"apps"`
	Defaults     *Defaults      `yaml:"defaults,omitempty"`
	Docker       *Docker        `yaml:"docker,omitempty"`
	Environments []Environment  `yaml:"environments,omitempty"`
}

// Config is the client-side ~/.voodu/config.yml.
type Config struct {
	Apps map[string]App `yaml:"apps"`
}

// App is a single application's deployment spec.
type App struct {
	Name         string            `yaml:"name,omitempty"`
	Lang         string            `yaml:"lang,omitempty"`
	Path         string            `yaml:"path,omitempty"`
	WorkDir      string            `yaml:"workdir,omitempty"`
	BinaryName   string            `yaml:"binary_name,omitempty"`
	GoVersion    string            `yaml:"go_version,omitempty"`
	Goos         string            `yaml:"goos,omitempty"`
	Goarch       string            `yaml:"goarch,omitempty"`
	CgoEnabled   *bool             `yaml:"cgo_enabled,omitempty"`
	Dockerfile   string            `yaml:"dockerfile,omitempty"`
	Image        string            `yaml:"image,omitempty"`
	Entrypoint   string            `yaml:"entrypoint,omitempty"`
	Command      string            `yaml:"command,omitempty"`
	Env          map[string]string `yaml:"env,omitempty"`
	Volumes      []string          `yaml:"volumes,omitempty"`
	Security     string            `yaml:"security,omitempty"`
	Deployment   *Deployment       `yaml:"deployment,omitempty"`
	Network      *NetworkConfig    `yaml:"network,omitempty"`
	Ports        []string          `yaml:"ports,omitempty"`
	Environments []Environment     `yaml:"environments,omitempty"`
}

type NetworkConfig struct {
	Mode string `yaml:"mode,omitempty"`
}

type Deployment struct {
	KeepReleases  int      `yaml:"keep_releases,omitempty"`
	KeepImages    int      `yaml:"keep_images,omitempty"`
	RestartPolicy string   `yaml:"restart_policy,omitempty"`
	RestartDelay  int      `yaml:"restart_delay,omitempty"`
	PostDeploy    []string `yaml:"post_deploy,omitempty"`
}

type Environment struct {
	Name           string            `yaml:"name"`
	Branch         string            `yaml:"branch,omitempty"`
	DefaultEnvVars map[string]string `yaml:"default_env_vars,omitempty"`
}

type Defaults struct {
	BuildType string `yaml:"build_type,omitempty"`
}

type Docker struct {
	Registry []string `yaml:"registry,omitempty"`
}
