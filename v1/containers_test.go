package v1

import (
	"os"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/suite"
)

func TestContainersCommandTestSuite(t *testing.T) {
	RegisterTestingT(t)
	suite.Run(t, new(ContainersCommandTestSuite))
}

type ContainersCommandTestSuite struct {
	suite.Suite
	command *ContainersCommand
}

func (s *ContainersCommandTestSuite) SetupSuite() {
	// Suite-level setup if needed
}

func (s *ContainersCommandTestSuite) SetupTest() {
	output := NewOutput(OutputFormatStdout)
	s.command = NewContainersCommand(output)
}

func (s *ContainersCommandTestSuite) TestNewContainersCommand_WithEmptyBaseDir() {
	output := NewOutput(OutputFormatStdout)
	command := NewContainersCommand(output)
	Expect(command.baseDir).To(Equal("/opt/gokku"))
}

func (s *ContainersCommandTestSuite) TestNewContainersCommand_WithCustomBaseDir() {
	os.Setenv("GOKKU_ROOT", "/custom/path")
	defer os.Unsetenv("GOKKU_ROOT")

	output := NewOutput(OutputFormatStdout)
	command := NewContainersCommand(output)
	Expect(command.baseDir).To(Equal("/custom/path"))
}

func (s *ContainersCommandTestSuite) TestContainsContainerName_WhenNameMatches() {
	names := "my-app-web-1"
	appName := "my-app"
	result := s.command.containsContainerName(names, appName)
	Expect(result).To(BeTrue())
}

func (s *ContainersCommandTestSuite) TestContainsContainerName_WhenNameDoesNotMatch() {
	names := "other-app-web-1"
	appName := "my-app"
	result := s.command.containsContainerName(names, appName)
	Expect(result).To(BeFalse())
}

func (s *ContainersCommandTestSuite) TestContainsContainerName_WhenNameIsSubstring() {
	names := "my-app-production-web-1"
	appName := "my-app"
	result := s.command.containsContainerName(names, appName)
	Expect(result).To(BeTrue())
}

func (s *ContainersCommandTestSuite) TestContainsProcessType_WhenTypeMatches() {
	names := "my-app-web-1"
	processType := "web"
	result := s.command.containsProcessType(names, processType)
	Expect(result).To(BeTrue())
}

func (s *ContainersCommandTestSuite) TestContainsProcessType_WhenTypeDoesNotMatch() {
	names := "my-app-worker-1"
	processType := "web"
	result := s.command.containsProcessType(names, processType)
	Expect(result).To(BeFalse())
}

func (s *ContainersCommandTestSuite) TestContainsProcessType_WhenTypeIsSubstring() {
	names := "my-app-web-production-1"
	processType := "web"
	result := s.command.containsProcessType(names, processType)
	Expect(result).To(BeTrue())
}

func (s *ContainersCommandTestSuite) TestListContainers_WithEmptyFilter() {
	filter := ContainerFilter{}
	err := s.command.List(filter)

	// This will depend on actual Docker state, so we just check no error
	// In a real scenario, you might want to mock this
	Expect(err).To(BeNil())
}

func (s *ContainersCommandTestSuite) TestListContainers_WithAppNameFilter() {
	filter := ContainerFilter{
		AppName: "test-app",
	}
	err := s.command.List(filter)

	// This will depend on actual Docker state
	Expect(err).To(BeNil())
}

func (s *ContainersCommandTestSuite) TestListContainers_WithProcessTypeFilter() {
	filter := ContainerFilter{
		ProcessType: "web",
	}
	err := s.command.List(filter)

	// This will depend on actual Docker state
	Expect(err).To(BeNil())
}

func (s *ContainersCommandTestSuite) TestListContainers_WithCombinedFilter() {
	filter := ContainerFilter{
		AppName:     "test-app",
		ProcessType: "web",
	}
	err := s.command.List(filter)

	// This will depend on actual Docker state
	Expect(err).To(BeNil())
}

func (s *ContainersCommandTestSuite) TestListContainers_WithAllFlag() {
	filter := ContainerFilter{
		All: true,
	}
	err := s.command.List(filter)

	// This will depend on actual Docker state
	Expect(err).To(BeNil())
}

func (s *ContainersCommandTestSuite) TestGetInfo_WhenContainerNotFound() {
	testOutput := &testOutputNoExit{}
	testCommand := &ContainersCommand{
		output:  testOutput,
		baseDir: s.command.baseDir,
	}
	
	err := testCommand.GetInfo("non-existent-container-12345")

	Expect(err).ToNot(BeNil())
	Expect(err.Error()).To(ContainSubstring("not found"))
	Expect(testOutput.lastError).To(ContainSubstring("not found"))
}

