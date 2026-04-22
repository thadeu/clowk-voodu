package v1

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"

	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/suite"
)

func TestAppsCommandTestSuite(t *testing.T) {
	RegisterTestingT(t)
	suite.Run(t, new(AppsCommandTestSuite))
}

type AppsCommandTestSuite struct {
	suite.Suite
	tempDir string
	command *AppsCommand
}

func (s *AppsCommandTestSuite) SetupSuite() {
	// Suite-level setup if needed
}

func (s *AppsCommandTestSuite) SetupTest() {
	var err error
	s.tempDir, err = os.MkdirTemp("", "gokku-test-*")
	s.Require().NoError(err)

	output := NewOutput(OutputFormatStdout)
	s.command = &AppsCommand{
		output:  output,
		baseDir: s.tempDir,
	}
}

func (s *AppsCommandTestSuite) TearDownTest() {
	if s.tempDir != "" {
		os.RemoveAll(s.tempDir)
	}
}

func (s *AppsCommandTestSuite) TestNewAppsCommand_WithEmptyBaseDir() {
	output := NewOutput(OutputFormatStdout)
	command := NewAppsCommand(output)
	Expect(command.baseDir).To(Equal("/opt/gokku"))
}

func (s *AppsCommandTestSuite) TestNewAppsCommand_WithCustomBaseDir() {
	os.Setenv("GOKKU_ROOT", "/custom/path")
	defer os.Unsetenv("GOKKU_ROOT")

	output := NewOutput(OutputFormatStdout)
	command := NewAppsCommand(output)
	Expect(command.baseDir).To(Equal("/custom/path"))
}

func (s *AppsCommandTestSuite) TestListApps_EmptyDirectory() {
	apps, err := s.command.listApps()
	Expect(err).To(BeNil())
	Expect(apps).To(BeEmpty())
}

func (s *AppsCommandTestSuite) TestListApps_WithSuccessfully() {
	appsDir := filepath.Join(s.tempDir, "apps")

	err := os.MkdirAll(appsDir, 0755)
	s.Require().NoError(err)

	// Create app directories
	app1Dir := filepath.Join(appsDir, "app1")
	app2Dir := filepath.Join(appsDir, "app2")

	err = os.MkdirAll(app1Dir, 0755)
	s.Require().NoError(err)

	err = os.MkdirAll(app2Dir, 0755)
	s.Require().NoError(err)

	apps, err := s.command.listApps()

	Expect(err).To(BeNil())
	Expect(len(apps)).To(Equal(2))
	Expect(apps[0].Name).To(BeElementOf("app1", "app2"))
	Expect(apps[1].Name).To(BeElementOf("app1", "app2"))
}

func (s *AppsCommandTestSuite) TestListApps_IgnoresFiles() {
	appsDir := filepath.Join(s.tempDir, "apps")
	err := os.MkdirAll(appsDir, 0755)
	s.Require().NoError(err)

	// Create a file (not a directory)
	filePath := filepath.Join(appsDir, "not-an-app")
	err = os.WriteFile(filePath, []byte("content"), 0644)
	s.Require().NoError(err)

	apps, err := s.command.listApps()
	Expect(err).To(BeNil())
	Expect(apps).To(BeEmpty())
}

func (s *AppsCommandTestSuite) TestAppExists_WhenAppExists() {
	appsDir := filepath.Join(s.tempDir, "apps", "test-app")
	err := os.MkdirAll(appsDir, 0755)
	s.Require().NoError(err)

	exists := s.command.appExists("test-app")
	Expect(exists).To(BeTrue())
}

func (s *AppsCommandTestSuite) TestAppExists_WhenAppDoesNotExist() {
	exists := s.command.appExists("non-existent-app")
	Expect(exists).To(BeFalse())
}

func (s *AppsCommandTestSuite) TestGetApp_WhenAppNotFound() {
	_, err := s.command.getApp("non-existent-app")
	Expect(err).ToNot(BeNil())
	Expect(err.Error()).To(ContainSubstring("not found"))
}

func (s *AppsCommandTestSuite) TestGetApp_WhenAppExists() {
	// Create app directory structure
	appsDir := filepath.Join(s.tempDir, "apps", "test-app")
	err := os.MkdirAll(appsDir, 0755)
	s.Require().NoError(err)

	app, err := s.command.getApp("test-app")
	Expect(err).To(BeNil())
	Expect(app).ToNot(BeNil())
	Expect(app.Name).To(Equal("test-app"))
	Expect(app.ReleasesCount).To(Equal(0))
	Expect(app.EnvVars).ToNot(BeNil())
	_ = app.Containers // Just access it to ensure it exists
}

func (s *AppsCommandTestSuite) TestCountReleases_WhenReleasesDirExists() {
	appName := "test-app"
	releasesDir := filepath.Join(s.tempDir, "apps", appName, "releases")
	err := os.MkdirAll(releasesDir, 0755)
	s.Require().NoError(err)

	// Create some release directories
	for i := 1; i <= 3; i++ {
		releaseDir := filepath.Join(releasesDir, "release-"+strconv.Itoa(i))
		err = os.MkdirAll(releaseDir, 0755)
		s.Require().NoError(err)
	}

	count := s.command.countReleases(appName)
	Expect(count).To(Equal(3))
}

func (s *AppsCommandTestSuite) TestCountReleases_WhenReleasesDirDoesNotExist() {
	count := s.command.countReleases("non-existent-app")
	Expect(count).To(Equal(0))
}

func (s *AppsCommandTestSuite) TestGetCurrentRelease_WhenSymlinkExists() {
	appName := "test-app"
	appDir := filepath.Join(s.tempDir, "apps", appName)
	releasesDir := filepath.Join(appDir, "releases")
	releaseDir := filepath.Join(releasesDir, "release-1")

	err := os.MkdirAll(releaseDir, 0755)
	s.Require().NoError(err)

	currentLink := filepath.Join(appDir, "current")
	err = os.Symlink(releaseDir, currentLink)
	s.Require().NoError(err)

	release := s.command.getCurrentRelease(appName)
	Expect(release).To(Equal("release-1"))
}

func (s *AppsCommandTestSuite) TestGetCurrentRelease_WhenSymlinkDoesNotExist() {
	release := s.command.getCurrentRelease("non-existent-app")
	Expect(release).To(Equal("none"))
}

