package v1

import (
	"os"
	"path/filepath"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/suite"
)

func TestConfigCommandTestSuite(t *testing.T) {
	RegisterTestingT(t)
	suite.Run(t, new(ConfigCommandTestSuite))
}

type ConfigCommandTestSuite struct {
	suite.Suite
	tempDir string
	command *ConfigCommand
	appName string
}

func (s *ConfigCommandTestSuite) SetupSuite() {
	// Suite-level setup if needed
}

func (s *ConfigCommandTestSuite) SetupTest() {
	var err error
	s.tempDir, err = os.MkdirTemp("", "gokku-test-*")
	s.Require().NoError(err)

	s.appName = "test-app"
	output := NewOutput(OutputFormatStdout)
	s.command = &ConfigCommand{
		output:  output,
		baseDir: s.tempDir,
	}

	// Create app directory structure
	appDir := filepath.Join(s.tempDir, "apps", s.appName, "shared")
	err = os.MkdirAll(appDir, 0755)

	s.Require().NoError(err)
}

func (s *ConfigCommandTestSuite) TearDownTest() {
	if s.tempDir != "" {
		os.RemoveAll(s.tempDir)
	}
}

func (s *ConfigCommandTestSuite) TestNewConfigCommand_WithEmptyBaseDir() {
	output := NewOutput(OutputFormatStdout)
	command := NewConfigCommand(output)
	Expect(command.baseDir).To(Equal("/opt/gokku"))
}

func (s *ConfigCommandTestSuite) TestNewConfigCommand_WithCustomBaseDir() {
	os.Setenv("GOKKU_ROOT", "/custom/path")
	defer os.Unsetenv("GOKKU_ROOT")

	output := NewOutput(OutputFormatStdout)
	command := NewConfigCommand(output)
	Expect(command.baseDir).To(Equal("/custom/path"))
}

func (s *ConfigCommandTestSuite) TestSet_SingleVariable() {
	err := s.command.Set(s.appName, []string{"KEY1=value1"})
	Expect(err).To(BeNil())

	envVars := s.command.listEnvVars(s.appName)
	Expect(envVars["KEY1"]).To(Equal("value1"))
}

func (s *ConfigCommandTestSuite) TestSet_MultipleVariables() {
	err := s.command.Set(s.appName, []string{"KEY1=value1", "KEY2=value2", "KEY3=value3"})
	Expect(err).To(BeNil())

	envVars := s.command.listEnvVars(s.appName)
	Expect(envVars["KEY1"]).To(Equal("value1"))
	Expect(envVars["KEY2"]).To(Equal("value2"))
	Expect(envVars["KEY3"]).To(Equal("value3"))
}

func (s *ConfigCommandTestSuite) TestSet_OverwritesExisting() {
	err := s.command.Set(s.appName, []string{"KEY1=value1"})
	Expect(err).To(BeNil())

	err = s.command.Set(s.appName, []string{"KEY1=value2"})
	Expect(err).To(BeNil())

	envVars := s.command.listEnvVars(s.appName)
	Expect(envVars["KEY1"]).To(Equal("value2"))
}

func (s *ConfigCommandTestSuite) TestSet_WithSpaces() {
	err := s.command.Set(s.appName, []string{"KEY1=value with spaces"})
	Expect(err).To(BeNil())

	envVars := s.command.listEnvVars(s.appName)
	Expect(envVars["KEY1"]).To(Equal("value with spaces"))
}

func (s *ConfigCommandTestSuite) TestSet_TrimsWhitespace() {
	err := s.command.Set(s.appName, []string{"  KEY1  =  value1  "})
	Expect(err).To(BeNil())

	envVars := s.command.listEnvVars(s.appName)
	Expect(envVars["KEY1"]).To(Equal("value1"))
}

func (s *ConfigCommandTestSuite) TestSet_InvalidFormat() {
	testOutput := &testOutputNoExit{}
	testCommand := &ConfigCommand{
		output:  testOutput,
		baseDir: s.tempDir,
	}

	err := testCommand.Set(s.appName, []string{"INVALID_FORMAT"})
	Expect(err).ToNot(BeNil())
	Expect(err.Error()).To(ContainSubstring("invalid format"))
	Expect(testOutput.lastError).To(ContainSubstring("invalid format"))
}

func (s *ConfigCommandTestSuite) TestGet_WhenVariableExists() {
	err := s.command.Set(s.appName, []string{"KEY1=value1"})
	s.Require().NoError(err)

	err = s.command.Get(s.appName, "KEY1")
	Expect(err).To(BeNil())
}

func (s *ConfigCommandTestSuite) TestGet_WhenVariableDoesNotExist() {
	// Use a test output that doesn't exit
	testOutput := &testOutputNoExit{}
	testCommand := &ConfigCommand{
		output:  testOutput,
		baseDir: s.tempDir,
	}

	err := testCommand.Get(s.appName, "NON_EXISTENT")
	Expect(err).ToNot(BeNil())
	Expect(err.Error()).To(ContainSubstring("not found"))
	Expect(testOutput.lastError).To(ContainSubstring("not found"))
}

// testOutputNoExit is a test output that doesn't call os.Exit
type testOutputNoExit struct {
	lastError string
}

func (o *testOutputNoExit) Print(message string)   {}
func (o *testOutputNoExit) Success(message string) {}
func (o *testOutputNoExit) Error(message string) {
	o.lastError = message
}
func (o *testOutputNoExit) Data(data interface{})                   {}
func (o *testOutputNoExit) Table(headers []string, rows [][]string) {}

func (s *ConfigCommandTestSuite) TestList_Empty() {
	err := s.command.List(s.appName)
	Expect(err).To(BeNil())
}

func (s *ConfigCommandTestSuite) TestList_WithVariables() {
	err := s.command.Set(s.appName, []string{"KEY1=value1", "KEY2=value2"})
	s.Require().NoError(err)

	err = s.command.List(s.appName)
	Expect(err).To(BeNil())
}

func (s *ConfigCommandTestSuite) TestUnset_SingleVariable() {
	err := s.command.Set(s.appName, []string{"KEY1=value1", "KEY2=value2"})
	s.Require().NoError(err)

	err = s.command.Unset(s.appName, []string{"KEY1"})
	Expect(err).To(BeNil())

	envVars := s.command.listEnvVars(s.appName)
	Expect(envVars).ToNot(HaveKey("KEY1"))
	Expect(envVars["KEY2"]).To(Equal("value2"))
}

func (s *ConfigCommandTestSuite) TestUnset_MultipleVariables() {
	err := s.command.Set(s.appName, []string{"KEY1=value1", "KEY2=value2", "KEY3=value3"})
	s.Require().NoError(err)

	err = s.command.Unset(s.appName, []string{"KEY1", "KEY3"})
	Expect(err).To(BeNil())

	envVars := s.command.listEnvVars(s.appName)
	Expect(envVars).ToNot(HaveKey("KEY1"))
	Expect(envVars).ToNot(HaveKey("KEY3"))
	Expect(envVars["KEY2"]).To(Equal("value2"))
}

func (s *ConfigCommandTestSuite) TestUnset_NonExistentVariable() {
	err := s.command.Set(s.appName, []string{"KEY1=value1"})
	s.Require().NoError(err)

	// Unsetting a non-existent variable should not error
	err = s.command.Unset(s.appName, []string{"NON_EXISTENT"})
	Expect(err).To(BeNil())

	envVars := s.command.listEnvVars(s.appName)
	Expect(envVars["KEY1"]).To(Equal("value1"))
}

func (s *ConfigCommandTestSuite) TestGetEnvFilePath() {
	expectedPath := filepath.Join(s.tempDir, "apps", s.appName, "shared", ".env")
	actualPath := s.command.getEnvFilePath(s.appName)

	Expect(actualPath).To(Equal(expectedPath))
}
