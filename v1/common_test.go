package v1

import (
	"testing"

	"go.gokku-vm.com/pkg"

	. "github.com/onsi/gomega"
	"github.com/stretchr/testify/suite"
)

func TestCommonTestSuite(t *testing.T) {
	RegisterTestingT(t)
	suite.Run(t, new(CommonTestSuite))
}

type CommonTestSuite struct {
	suite.Suite
}

func (s *CommonTestSuite) SetupSuite() {
	// Suite-level setup if needed
}

func (s *CommonTestSuite) SetupTest() {
	// Test-level setup if needed
}

func (s *CommonTestSuite) TestAppInfo_Structure() {
	appInfo := AppInfo{
		Name:           "test-app",
		Status:         "running",
		ReleasesCount:  5,
		CurrentRelease: "release-3",
	}

	Expect(appInfo.Name).To(Equal("test-app"))
	Expect(appInfo.Status).To(Equal("running"))
	Expect(appInfo.ReleasesCount).To(Equal(5))
	Expect(appInfo.CurrentRelease).To(Equal("release-3"))
}

func (s *CommonTestSuite) TestAppDetail_Structure() {
	appInfo := AppInfo{
		Name:           "test-app",
		Status:         "running",
		ReleasesCount:  2,
		CurrentRelease: "release-1",
	}

	appDetail := AppDetail{
		AppInfo:    appInfo,
		Config:     nil,
		Containers: []pkg.ContainerInfo{},
		EnvVars:    make(map[string]string),
	}

	Expect(appDetail.Name).To(Equal("test-app"))
	Expect(appDetail.Status).To(Equal("running"))
	Expect(appDetail.ReleasesCount).To(Equal(2))
	Expect(appDetail.CurrentRelease).To(Equal("release-1"))
	Expect(appDetail.Config).To(BeNil())
	Expect(appDetail.Containers).ToNot(BeNil())
	Expect(appDetail.EnvVars).ToNot(BeNil())
}

func (s *CommonTestSuite) TestContainerFilter_Structure() {
	filter := ContainerFilter{
		AppName:     "test-app",
		ProcessType: "web",
		All:         true,
	}

	Expect(filter.AppName).To(Equal("test-app"))
	Expect(filter.ProcessType).To(Equal("web"))
	Expect(filter.All).To(BeTrue())
}

func (s *CommonTestSuite) TestContainerFilter_Empty() {
	filter := ContainerFilter{}

	Expect(filter.AppName).To(BeEmpty())
	Expect(filter.ProcessType).To(BeEmpty())
	Expect(filter.All).To(BeFalse())
}
