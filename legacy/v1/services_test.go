package v1

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/suite"
)

func TestServicesCommandTestSuite(t *testing.T) {
	RegisterTestingT(t)
	suite.Run(t, new(ServicesCommandTestSuite))
}

type ServicesCommandTestSuite struct {
	suite.Suite
	tempDir     string
	servicesDir string
	pluginsDir  string
	command     *ServicesCommand
}

func (s *ServicesCommandTestSuite) SetupSuite() {
	// Suite-level setup if needed
}

func (s *ServicesCommandTestSuite) SetupTest() {
	var err error
	s.tempDir, err = os.MkdirTemp("", "gokku-test-*")
	s.Require().NoError(err)

	s.servicesDir = filepath.Join(s.tempDir, "services")
	s.pluginsDir = filepath.Join(s.tempDir, "plugins")

	err = os.MkdirAll(s.servicesDir, 0755)
	s.Require().NoError(err)
	err = os.MkdirAll(s.pluginsDir, 0755)
	s.Require().NoError(err)

	output := NewOutput(OutputFormatStdout)
	s.command = &ServicesCommand{
		output:      output,
		baseDir:     s.tempDir,
		servicesDir: s.servicesDir,
		pluginsDir:  s.pluginsDir,
	}
}

func (s *ServicesCommandTestSuite) TearDownTest() {
	if s.tempDir != "" {
		os.RemoveAll(s.tempDir)
	}
}

func (s *ServicesCommandTestSuite) TestNewServicesCommand() {
	output := NewOutput(OutputFormatStdout)
	command := NewServicesCommand(output)

	Expect(command).ToNot(BeNil())
	Expect(command.servicesDir).To(Equal("/opt/gokku/services"))
	Expect(command.pluginsDir).To(Equal("/opt/gokku/plugins"))
}

func (s *ServicesCommandTestSuite) TestPluginExists_WhenPluginExists() {
	pluginDir := filepath.Join(s.pluginsDir, "test-plugin")
	err := os.MkdirAll(pluginDir, 0755)
	s.Require().NoError(err)

	exists := s.command.pluginExists("test-plugin")
	Expect(exists).To(BeTrue())
}

func (s *ServicesCommandTestSuite) TestPluginExists_WhenPluginDoesNotExist() {
	exists := s.command.pluginExists("non-existent-plugin")
	Expect(exists).To(BeFalse())
}

func (s *ServicesCommandTestSuite) TestServiceExists_WhenServiceExists() {
	serviceDir := filepath.Join(s.servicesDir, "test-service")
	err := os.MkdirAll(serviceDir, 0755)
	s.Require().NoError(err)

	exists := s.command.serviceExists("test-service")
	Expect(exists).To(BeTrue())
}

func (s *ServicesCommandTestSuite) TestServiceExists_WhenServiceDoesNotExist() {
	exists := s.command.serviceExists("non-existent-service")
	Expect(exists).To(BeFalse())
}

func (s *ServicesCommandTestSuite) TestAppExists_WhenAppDoesNotExist() {
	exists := s.command.appExists("non-existent-app", "")
	Expect(exists).To(BeFalse())
}

func (s *ServicesCommandTestSuite) TestIsAppLinked_WhenAppIsLinked() {
	linkedApps := []string{"app1", "app2", "app3"}
	result := s.command.isAppLinked(linkedApps, "app2", "")
	Expect(result).To(BeTrue())
}

func (s *ServicesCommandTestSuite) TestIsAppLinked_WhenAppIsNotLinked() {
	linkedApps := []string{"app1", "app2", "app3"}
	result := s.command.isAppLinked(linkedApps, "app4", "")
	Expect(result).To(BeFalse())
}

func (s *ServicesCommandTestSuite) TestIsAppLinked_WhenListIsEmpty() {
	linkedApps := []string{}
	result := s.command.isAppLinked(linkedApps, "app1", "")
	Expect(result).To(BeFalse())
}

func (s *ServicesCommandTestSuite) TestSaveServiceConfig() {
	// Create service directory first
	serviceDir := filepath.Join(s.servicesDir, "test-service")

	err := os.MkdirAll(serviceDir, 0755)
	s.Require().NoError(err)

	svc := service{
		Name:       "test-service",
		Plugin:     "postgres",
		Running:    false,
		LinkedApps: []string{},
		Config:     make(map[string]string),
	}

	err = s.command.saveServiceConfig("test-service", svc)
	Expect(err).To(BeNil())

	configPath := filepath.Join(s.servicesDir, "test-service", "config.json")
	Expect(configPath).To(BeAnExistingFile())
}

func (s *ServicesCommandTestSuite) TestGetServiceConfig_WhenServiceExists() {
	// Create service directory first
	serviceDir := filepath.Join(s.servicesDir, "test-service")
	err := os.MkdirAll(serviceDir, 0755)
	s.Require().NoError(err)

	svc := service{
		Name:       "test-service",
		Plugin:     "postgres",
		Running:    false,
		LinkedApps: []string{"app1"},
		Config:     map[string]string{"version": "14"},
	}

	err = s.command.saveServiceConfig("test-service", svc)
	s.Require().NoError(err)

	retrieved, err := s.command.getServiceConfig("test-service")

	Expect(err).To(BeNil())
	Expect(retrieved.Name).To(Equal("test-service"))
	Expect(retrieved.Plugin).To(Equal("postgres"))
	Expect(retrieved.LinkedApps).To(ContainElement("app1"))
	Expect(retrieved.Config["version"]).To(Equal("14"))
}

func (s *ServicesCommandTestSuite) TestGetServiceConfig_WhenServiceDoesNotExist() {
	_, err := s.command.getServiceConfig("non-existent-service")
	Expect(err).ToNot(BeNil())
}

func (s *ServicesCommandTestSuite) TestGetServiceEnvVars_Postgres() {
	config := map[string]string{
		"port":     "5432",
		"user":     "postgres",
		"password": "secret",
		"database": "mydb",
	}

	envVars := s.command.getServiceEnvVars("test-service", config, "postgres")

	Expect(envVars).ToNot(BeEmpty())
	Expect(envVars["DATABASE_URL"]).To(ContainSubstring("postgres://"))
	Expect(envVars["POSTGRES_HOST"]).To(Equal("localhost"))
	Expect(envVars["POSTGRES_PORT"]).To(Equal("5432"))
	Expect(envVars["POSTGRES_USER"]).To(Equal("postgres"))
	Expect(envVars["POSTGRES_PASSWORD"]).To(Equal("secret"))
	Expect(envVars["POSTGRES_DB"]).To(Equal("mydb"))
}

func (s *ServicesCommandTestSuite) TestGetServiceEnvVars_Postgres_IncompleteConfig() {
	config := map[string]string{
		"port": "5432",
		// Missing user, password, database
	}

	envVars := s.command.getServiceEnvVars("test-service", config, "postgres")
	Expect(envVars).To(BeEmpty())
}

func (s *ServicesCommandTestSuite) TestGetServiceEnvVars_Redis() {
	config := map[string]string{
		"port":     "6379",
		"password": "secret",
	}

	envVars := s.command.getServiceEnvVars("test-service", config, "redis")

	Expect(envVars).ToNot(BeEmpty())
	Expect(envVars["REDIS_URL"]).To(ContainSubstring("redis://"))
	Expect(envVars["REDIS_HOST"]).To(Equal("localhost"))
	Expect(envVars["REDIS_PORT"]).To(Equal("6379"))
	Expect(envVars["REDIS_PASSWORD"]).To(Equal("secret"))
}

func (s *ServicesCommandTestSuite) TestGetServiceEnvVars_Redis_IncompleteConfig() {
	config := map[string]string{
		"port": "6379",
		// Missing password
	}

	envVars := s.command.getServiceEnvVars("test-service", config, "redis")
	Expect(envVars).To(BeEmpty())
}

func (s *ServicesCommandTestSuite) TestGetServiceEnvVars_UnknownPlugin() {
	config := map[string]string{"key": "value"}
	envVars := s.command.getServiceEnvVars("test-service", config, "unknown-plugin")
	Expect(envVars).To(BeEmpty())
}

func (s *ServicesCommandTestSuite) TestGetServiceEnvKeys_Postgres() {
	keys := s.command.getServiceEnvKeys("postgres")

	Expect(keys).To(ContainElement("DATABASE_URL"))
	Expect(keys).To(ContainElement("POSTGRES_HOST"))
	Expect(keys).To(ContainElement("POSTGRES_PORT"))
	Expect(keys).To(ContainElement("POSTGRES_USER"))
	Expect(keys).To(ContainElement("POSTGRES_PASSWORD"))
	Expect(keys).To(ContainElement("POSTGRES_DB"))
}

func (s *ServicesCommandTestSuite) TestGetServiceEnvKeys_Redis() {
	keys := s.command.getServiceEnvKeys("redis")

	Expect(keys).To(ContainElement("REDIS_URL"))
	Expect(keys).To(ContainElement("REDIS_HOST"))
	Expect(keys).To(ContainElement("REDIS_PORT"))
	Expect(keys).To(ContainElement("REDIS_PASSWORD"))
}

func (s *ServicesCommandTestSuite) TestGetServiceEnvKeys_UnknownPlugin() {
	keys := s.command.getServiceEnvKeys("unknown-plugin")
	Expect(keys).To(BeEmpty())
}

func (s *ServicesCommandTestSuite) TestListServices_Empty() {
	services, err := s.command.listServices()
	Expect(err).To(BeNil())
	Expect(services).To(BeEmpty())
}

func (s *ServicesCommandTestSuite) TestListServices_WithServices() {
	// Create service directories first
	service1Dir := filepath.Join(s.servicesDir, "service1")
	service2Dir := filepath.Join(s.servicesDir, "service2")

	err := os.MkdirAll(service1Dir, 0755)
	s.Require().NoError(err)
	err = os.MkdirAll(service2Dir, 0755)
	s.Require().NoError(err)

	// Create service configs
	service1 := service{
		Name:       "service1",
		Plugin:     "postgres",
		Running:    false,
		LinkedApps: []string{},
		Config:     make(map[string]string),
	}

	service2 := service{
		Name:       "service2",
		Plugin:     "redis",
		Running:    true,
		LinkedApps: []string{"app1"},
		Config:     make(map[string]string),
	}

	err = s.command.saveServiceConfig("service1", service1)
	s.Require().NoError(err)

	err = s.command.saveServiceConfig("service2", service2)
	s.Require().NoError(err)

	services, err := s.command.listServices()
	Expect(err).To(BeNil())
	Expect(len(services)).To(Equal(2))
}

func (s *ServicesCommandTestSuite) TestGetService_WhenServiceExists() {
	// Create service directory first
	serviceDir := filepath.Join(s.servicesDir, "test-service")
	err := os.MkdirAll(serviceDir, 0755)
	s.Require().NoError(err)

	svc := service{
		Name:       "test-service",
		Plugin:     "postgres",
		Running:    false,
		LinkedApps: []string{},
		Config:     make(map[string]string),
	}

	err = s.command.saveServiceConfig("test-service", svc)
	s.Require().NoError(err)

	err = s.command.Get("test-service")
	Expect(err).To(BeNil())
}

func (s *ServicesCommandTestSuite) TestGetService_WhenServiceDoesNotExist() {
	testOutput := &testOutputNoExit{}
	testCommand := &ServicesCommand{
		output:      testOutput,
		baseDir:     s.command.baseDir,
		servicesDir: s.command.servicesDir,
		pluginsDir:  s.command.pluginsDir,
	}

	err := testCommand.Get("non-existent-service")
	Expect(err).ToNot(BeNil())
	Expect(testOutput.lastError).To(ContainSubstring("not found"))
}

func (s *ServicesCommandTestSuite) TestUpdateServiceConfig() {
	// Create service directory first
	serviceDir := filepath.Join(s.servicesDir, "test-service")
	err := os.MkdirAll(serviceDir, 0755)
	s.Require().NoError(err)

	svc := service{
		Name:       "test-service",
		Plugin:     "postgres",
		Running:    false,
		LinkedApps: []string{},
		Config:     make(map[string]string),
	}

	err = s.command.saveServiceConfig("test-service", svc)
	s.Require().NoError(err)

	newConfig := map[string]string{
		"version": "14",
		"port":    "5432",
	}

	err = s.command.UpdateServiceConfig("test-service", newConfig)
	Expect(err).To(BeNil())

	updated, err := s.command.getServiceConfig("test-service")
	Expect(err).To(BeNil())
	Expect(updated.Config["version"]).To(Equal("14"))
	Expect(updated.Config["port"]).To(Equal("5432"))
}

func (s *ServicesCommandTestSuite) TestCreateService_WhenPluginDoesNotExist() {
	testOutput := &testOutputNoExit{}
	testCommand := &ServicesCommand{
		output:      testOutput,
		baseDir:     s.command.baseDir,
		servicesDir: s.command.servicesDir,
		pluginsDir:  s.command.pluginsDir,
	}

	err := testCommand.Create("non-existent-plugin", "test-service", "")

	Expect(err).ToNot(BeNil())
	Expect(err.Error()).To(ContainSubstring("plugin"))
	Expect(err.Error()).To(ContainSubstring("not found"))
	Expect(testOutput.lastError).To(ContainSubstring("plugin"))
}

func (s *ServicesCommandTestSuite) TestCreateService_WhenServiceAlreadyExists() {
	// Create plugin directory
	pluginDir := filepath.Join(s.pluginsDir, "test-plugin")
	err := os.MkdirAll(pluginDir, 0755)
	s.Require().NoError(err)

	// Create service directory
	serviceDir := filepath.Join(s.servicesDir, "test-service")
	err = os.MkdirAll(serviceDir, 0755)
	s.Require().NoError(err)

	testOutput := &testOutputNoExit{}
	testCommand := &ServicesCommand{
		output:      testOutput,
		baseDir:     s.command.baseDir,
		servicesDir: s.command.servicesDir,
		pluginsDir:  s.command.pluginsDir,
	}

	err = testCommand.Create("test-plugin", "test-service", "")
	Expect(err).ToNot(BeNil())
	Expect(err.Error()).To(ContainSubstring("already exists"))
	Expect(testOutput.lastError).To(ContainSubstring("already exists"))
}

func (s *ServicesCommandTestSuite) TestDestroyService_WhenServiceDoesNotExist() {
	testOutput := &testOutputNoExit{}
	testCommand := &ServicesCommand{
		output:      testOutput,
		baseDir:     s.command.baseDir,
		servicesDir: s.command.servicesDir,
		pluginsDir:  s.command.pluginsDir,
	}

	err := testCommand.Destroy("non-existent-service")

	Expect(err).ToNot(BeNil())
	Expect(err.Error()).To(ContainSubstring("not found"))
	Expect(testOutput.lastError).To(ContainSubstring("not found"))
}

func (s *ServicesCommandTestSuite) TestDestroyService_WhenServiceExists() {
	// Create plugin directory
	pluginDir := filepath.Join(s.pluginsDir, "test-plugin")
	err := os.MkdirAll(pluginDir, 0755)
	s.Require().NoError(err)

	// Create service directory first
	serviceDir := filepath.Join(s.servicesDir, "test-service")
	err = os.MkdirAll(serviceDir, 0755)
	s.Require().NoError(err)

	// Create service
	svc := service{
		Name:       "test-service",
		Plugin:     "test-plugin",
		Running:    false,
		LinkedApps: []string{},
		Config:     make(map[string]string),
	}

	err = s.command.saveServiceConfig("test-service", svc)
	s.Require().NoError(err)

	// Verify service exists
	Expect(serviceDir).To(BeADirectory())

	// Destroy service
	err = s.command.Destroy("test-service")
	Expect(err).To(BeNil())

	// Verify service directory is removed
	_, err = os.Stat(serviceDir)
	Expect(os.IsNotExist(err)).To(BeTrue())
}

func (s *ServicesCommandTestSuite) TestServiceJSONSerialization() {
	svc := service{
		Name:        "test-service",
		Plugin:      "postgres",
		ContainerID: "container-123",
		Running:     true,
		LinkedApps:  []string{"app1", "app2"},
		CreatedAt:   "2024-01-01T00:00:00Z",
		Config:      map[string]string{"version": "14"},
	}

	data, err := json.Marshal(svc)

	Expect(err).To(BeNil())
	Expect(data).ToNot(BeEmpty())

	var unmarshaled service
	err = json.Unmarshal(data, &unmarshaled)

	Expect(err).To(BeNil())

	Expect(unmarshaled.Name).To(Equal(svc.Name))
	Expect(unmarshaled.Plugin).To(Equal(svc.Plugin))
	Expect(unmarshaled.Running).To(Equal(svc.Running))
	Expect(unmarshaled.LinkedApps).To(Equal(svc.LinkedApps))
	Expect(unmarshaled.Config).To(Equal(svc.Config))
}
