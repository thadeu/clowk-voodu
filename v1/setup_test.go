package v1

import (
	"os"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/suite"
)

func TestSetupCommandTestSuite(t *testing.T) {
	RegisterTestingT(t)
	suite.Run(t, new(SetupCommandTestSuite))
}

type SetupCommandTestSuite struct {
	suite.Suite
}

func (s *SetupCommandTestSuite) TestNewSetupCommand_WithHostOnly() {
	output := NewOutput(OutputFormatStdout)
	command := NewSetupCommand(output, "example.com", "")
	Expect(command).ToNot(BeNil())
	Expect(command.serverHost).To(Equal("example.com"))
	Expect(command.identityFile).To(BeEmpty())
}

func (s *SetupCommandTestSuite) TestNewSetupCommand_WithHostAndIdentityFile() {
	identityFile := "/path/to/key.pem"
	output := NewOutput(OutputFormatStdout)
	command := NewSetupCommand(output, "example.com", identityFile)
	Expect(command).ToNot(BeNil())
	Expect(command.serverHost).To(Equal("example.com"))
	Expect(command.identityFile).To(Equal(identityFile))
}

func (s *SetupCommandTestSuite) TestBuildSSHArgs_WithoutIdentityFile() {
	output := NewOutput(OutputFormatStdout)
	command := NewSetupCommand(output, "example.com", "")
	args := command.buildSSHArgs("echo", "test")

	Expect(args).To(ContainElement("example.com"))
	Expect(args).To(ContainElement("echo"))
	Expect(args).To(ContainElement("test"))
	Expect(args).ToNot(ContainElement("-i"))
}

func (s *SetupCommandTestSuite) TestBuildSSHArgs_WithIdentityFile() {
	// Create a temporary identity file
	tempFile, err := os.CreateTemp("", "test-key-*.pem")
	s.Require().NoError(err)
	defer os.Remove(tempFile.Name())
	tempFile.Close()

	output := NewOutput(OutputFormatStdout)
	command := NewSetupCommand(output, "example.com", tempFile.Name())
	args := command.buildSSHArgs("echo", "test")

	Expect(args).To(ContainElement("example.com"))
	Expect(args).To(ContainElement("-i"))
	Expect(args).To(ContainElement(tempFile.Name()))
}

func (s *SetupCommandTestSuite) TestBuildSSHArgs_WithIdentityFile_WhenFileDoesNotExist() {
	output := NewOutput(OutputFormatStdout)
	command := NewSetupCommand(output, "example.com", "/non/existent/key.pem")
	args := command.buildSSHArgs("echo", "test")

	// Should not include -i if file doesn't exist
	Expect(args).ToNot(ContainElement("-i"))
	Expect(args).ToNot(ContainElement("/non/existent/key.pem"))
}

func (s *SetupCommandTestSuite) TestBuildSSHArgs_WithSSHOptions() {
	output := NewOutput(OutputFormatStdout)
	command := NewSetupCommand(output, "example.com", "")
	args := command.buildSSHArgs("-o", "BatchMode=yes", "-o", "ConnectTimeout=5", "echo", "OK")

	Expect(args).To(ContainElement("-o"))
	Expect(args).To(ContainElement("BatchMode=yes"))
	Expect(args).To(ContainElement("ConnectTimeout=5"))
	Expect(args).To(ContainElement("echo"))
	Expect(args).To(ContainElement("OK"))
	Expect(args).To(ContainElement("example.com"))
}

func (s *SetupCommandTestSuite) TestBuildSSHArgs_WithTFlag() {
	output := NewOutput(OutputFormatStdout)
	command := NewSetupCommand(output, "example.com", "")
	args := command.buildSSHArgs("-t", "bash", "-c", "command")

	Expect(args).To(ContainElement("-t"))
	Expect(args).To(ContainElement("bash"))
	Expect(args).To(ContainElement("-c"))
	Expect(args).To(ContainElement("command"))
	Expect(args).To(ContainElement("example.com"))
}

func (s *SetupCommandTestSuite) TestBuildSSHArgs_ComplexCommand() {
	output := NewOutput(OutputFormatStdout)
	command := NewSetupCommand(output, "example.com", "")
	args := command.buildSSHArgs("-o", "StrictHostKeyChecking=no", "docker", "ps")

	Expect(args).To(ContainElement("-o"))
	Expect(args).To(ContainElement("StrictHostKeyChecking=no"))
	Expect(args).To(ContainElement("docker"))
	Expect(args).To(ContainElement("ps"))
	Expect(args).To(ContainElement("example.com"))
}

func (s *SetupCommandTestSuite) TestBuildSSHArgs_Order() {
	output := NewOutput(OutputFormatStdout)
	command := NewSetupCommand(output, "example.com", "")
	args := command.buildSSHArgs("command")

	// Host should be before command
	hostIndex := -1
	commandIndex := -1
	for i, arg := range args {
		if arg == "example.com" {
			hostIndex = i
		}
		if arg == "command" {
			commandIndex = i
		}
	}

	Expect(hostIndex).To(BeNumerically(">=", 0))
	Expect(commandIndex).To(BeNumerically(">=", 0))
	Expect(hostIndex).To(BeNumerically("<", commandIndex))
}

