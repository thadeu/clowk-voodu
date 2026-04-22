package v1

type Command struct {
	Output     Output
	Apps       *AppsCommand
	Services   *ServicesCommand
	Config     *ConfigCommand
	Containers *ContainersCommand
	Processes  *ProcessesCommand
	Logs       *LogsCommand
	Restart    *RestartCommand
	Deploy     *DeployCommand
	Plugins    *PluginsCommand
	Run        *RunCommand
	Remote     *RunRemoteCommand
	Uninstall  *RunUninstallCommand
	Rollback   *RollbackCommand
	AutoUpdate *AutoUpdateCommand
}

func NewCommand(format OutputFormat) *Command {
	output := NewOutput(format)

	return &Command{
		Output:     output,
		Apps:       NewAppsCommand(output),
		Services:   NewServicesCommand(output),
		Config:     NewConfigCommand(output),
		Containers: NewContainersCommand(output),
		Processes:  NewProcessesCommand(output),
		Logs:       NewLogsCommand(output),
		Restart:    NewRestartCommand(output),
		Deploy:     NewDeployCommand(output),
		Plugins:    NewPluginsCommand(output),
		Run:        NewRunCommand(output),
		Remote:     NewRunRemoteCommand(output),
		Uninstall:  NewRunUninstallCommand(output),
		Rollback:   NewRollbackCommand(output),
		AutoUpdate: NewAutoUpdateCommand(output),
	}
}

func NewCommandFromString(format string) *Command {
	return NewCommand(OutputFormat(format))
}
