package rancher

import (
	"strings"
	"testing"

	"github.com/Sirupsen/logrus"
	"github.com/rancher/go-rancher-metadata/metadata"
	"github.com/rancher/go-rancher/v2"
	"github.com/rancher/lb-controller/config"
	utils "github.com/rancher/lb-controller/utils"
)

var lbc1 *LoadBalancerController

func init() {
	lbc1 = &LoadBalancerController{
		stopCh:                     make(chan struct{}),
		incrementalBackoff:         0,
		incrementalBackoffInterval: 5,
		MetaFetcher:                sMetaFetcher{},
		CertFetcher:                sCertFetcher{},
		LBProvider:                 &sProvider{},
	}
}

type sMetaFetcher struct {
}

type sProvider struct {
}

type sCertFetcher struct {
}

func TestSelectorMatch1(t *testing.T) {
	portRules := []metadata.PortRule{}
	port := metadata.PortRule{
		Protocol:    "http",
		SourcePort:  45,
		Selector:    "foo=bar",
		Region:      "region2",
		Environment: "alpha",
		TargetPort:  80,
	}
	portRules = append(portRules, port)

	port = metadata.PortRule{
		Protocol:    "http",
		SourcePort:  45,
		Selector:    "foo=bar",
		Environment: "bar",
		TargetPort:  80,
	}
	portRules = append(portRules, port)

	meta := &LBMetadata{
		PortRules: portRules,
	}

	lbc1.processSelector(meta)

	configs, _ := lbc1.BuildConfigFromMetadata("test", "", "", "any", meta)

	fe := configs[0].FrontendServices[0]
	if len(fe.BackendServices) == 0 {
		t.Fatal("No backends are configured for selector based service")
	}

	if len(fe.BackendServices) != 2 {
		t.Fatalf("Incorrect number of backend services %v", len(fe.BackendServices))
	}

	be := fe.BackendServices[0]

	if fe.Port != 45 {
		t.Fatalf("Port is incorrect %v", fe.Port)
	}

	if be.Port != 80 {
		t.Fatalf("Port is incorrect %v", be.Port)
	}
}

func (mf sMetaFetcher) GetSelfService() (metadata.Service, error) {
	var svc metadata.Service
	return svc, nil
}

func (mf sMetaFetcher) GetServices() ([]metadata.Service, error) {
	var containers []metadata.Container
	var svcs []metadata.Service
	c1 := metadata.Container{
		Name:            "client_container",
		StackName:       "stackB",
		ServiceName:     "svcB",
		EnvironmentUUID: "foo",
		PrimaryIp:       "172.17.0.9",
		State:           "running",
	}
	c2 := metadata.Container{
		Name:            "client_container",
		StackName:       "stackB",
		ServiceName:     "svcB",
		EnvironmentUUID: "foo",
		PrimaryIp:       "172.17.0.10",
		State:           "running",
	}
	containers = []metadata.Container{c1, c2}
	service := metadata.Service{
		Name:            "svcB",
		Kind:            "service",
		StackName:       "stackB",
		EnvironmentUUID: "foo",
		Containers:      containers,
	}
	svcs = append(svcs, service)
	return svcs, nil
}

func (mf sMetaFetcher) GetServicesByRegionEnvironment(regionName string, envName string) ([]metadata.Service, error) {
	var svcs []metadata.Service
	var containers []metadata.Container
	var service1 metadata.Service
	var service2 metadata.Service
	if regionName == "region2" && envName == "alpha" {
		c1 := metadata.Container{
			Name:            "client_container",
			StackName:       "stackX",
			ServiceName:     "svcx",
			EnvironmentUUID: "alpha",
			PrimaryIp:       "173.17.0.18",
			State:           "running",
		}
		c2 := metadata.Container{
			Name:            "client_container",
			StackName:       "stackX",
			ServiceName:     "svcx",
			EnvironmentUUID: "alpha",
			PrimaryIp:       "173.17.0.19",
			State:           "running",
		}
		containers = []metadata.Container{c1, c2}
		labels := make(map[string]string)
		labels["foo"] = "bar"
		service1 = metadata.Service{
			Name:            "svcX",
			Kind:            "service",
			StackName:       "stackX",
			EnvironmentUUID: "alpha",
			Containers:      containers,
			Labels:          labels,
		}
		c1 = metadata.Container{
			Name:            "client_container",
			StackName:       "stackY",
			ServiceName:     "svcY",
			EnvironmentUUID: "alpha",
			PrimaryIp:       "173.17.0.20",
			State:           "running",
		}
		containers = []metadata.Container{c1}
		service2 = metadata.Service{
			Name:            "svcY",
			Kind:            "service",
			StackName:       "stackY",
			EnvironmentUUID: "alpha",
			Containers:      containers,
		}
	}
	svcs = []metadata.Service{service1, service2}
	return svcs, nil
}

func (mf sMetaFetcher) GetServicesByEnvironment(envName string) ([]metadata.Service, error) {
	var svcs []metadata.Service
	if envName == "bar" {
		var containers []metadata.Container
		c3 := metadata.Container{
			Name:            "client_container",
			StackName:       "stackC",
			ServiceName:     "drone",
			EnvironmentUUID: "bar",
			PrimaryIp:       "172.17.0.8",
			State:           "running",
		}
		containers = []metadata.Container{c3}
		labels := make(map[string]string)
		labels["foo"] = "bar"
		service := metadata.Service{
			Name:            "drone",
			Kind:            "service",
			StackName:       "stackC",
			EnvironmentUUID: "bar",
			Containers:      containers,
			Labels:          labels,
		}
		svcs = append(svcs, service)
	}
	return svcs, nil
}

func (mf sMetaFetcher) GetServiceByName(svcName string, stackName string) (metadata.Service, error) {
	var svc metadata.Service
	return svc, nil
}

func (mf sMetaFetcher) GetServiceByRegionEnvironment(regionName string, envName string, stackName string, svcName string) (metadata.Service, error) {
	var service metadata.Service
	var containers []metadata.Container
	if regionName == "region2" && envName == "alpha" && stackName == "stackX" && svcName == "svcX" {
		c1 := metadata.Container{
			Name:            "client_container",
			StackName:       "stackX",
			ServiceName:     "svcx",
			EnvironmentUUID: "alpha",
			PrimaryIp:       "173.17.0.18",
			State:           "running",
		}
		c2 := metadata.Container{
			Name:            "client_container",
			StackName:       "stackX",
			ServiceName:     "svcx",
			EnvironmentUUID: "alpha",
			PrimaryIp:       "173.17.0.19",
			State:           "running",
		}
		containers = []metadata.Container{c1, c2}
		labels := make(map[string]string)
		labels["foo"] = "bar"
		service = metadata.Service{
			Name:            "svcX",
			Kind:            "service",
			StackName:       "stackX",
			EnvironmentUUID: "alpha",
			Containers:      containers,
			Labels:          labels,
		}
	} else if regionName == "region2" && envName == "alpha" && stackName == "stackY" && svcName == "svcY" {
		c1 := metadata.Container{
			Name:            "client_container",
			StackName:       "stackY",
			ServiceName:     "svcY",
			EnvironmentUUID: "alpha",
			PrimaryIp:       "173.17.0.20",
			State:           "running",
		}
		containers = []metadata.Container{c1}
		service = metadata.Service{
			Name:            "svcY",
			Kind:            "service",
			StackName:       "stackY",
			EnvironmentUUID: "alpha",
			Containers:      containers,
		}
	}
	return service, nil
}

func (mf sMetaFetcher) GetServiceByEnvironment(envName string, stackName string, svcName string) (metadata.Service, error) {
	if envName == "bar" && stackName == "stackC" && svcName == "drone" {
		var containers []metadata.Container
		c3 := metadata.Container{
			Name:            "client_container",
			StackName:       "stackC",
			ServiceName:     "drone",
			EnvironmentUUID: "bar",
			PrimaryIp:       "172.17.0.8",
			State:           "running",
		}
		containers = []metadata.Container{c3}
		labels := make(map[string]string)
		labels["foo"] = "bar"
		service := metadata.Service{
			Name:            "drone",
			Kind:            "service",
			StackName:       "stackC",
			EnvironmentUUID: "bar",
			Containers:      containers,
			Labels:          labels,
		}
		logrus.Info("returning right service")
		return service, nil
	}
	var service metadata.Service
	return service, nil
}

func (mf sMetaFetcher) GetService(link string) (*metadata.Service, error) {
	splitSvcName := strings.Split(link, "/")
	var linkedService metadata.Service

	regionName, err := mf.GetRegionName()
	regionName = strings.TrimSuffix(regionName, "\"")
	regionName = strings.TrimPrefix(regionName, "\"")

	logrus.Info("Get service .. ", splitSvcName)

	if len(splitSvcName) == 4 {
		if splitSvcName[0] == regionName {
			linkedService, err = mf.GetServiceByEnvironment(splitSvcName[1], splitSvcName[2], splitSvcName[3])
		} else {
			linkedService, err = mf.GetServiceByRegionEnvironment(splitSvcName[0], splitSvcName[1], splitSvcName[2], splitSvcName[3])
		}
	} else if len(splitSvcName) == 3 {
		linkedService, err = mf.GetServiceByEnvironment(splitSvcName[0], splitSvcName[1], splitSvcName[2])
		logrus.Info("found service")
		logrus.Info(linkedService.Name)
	} else {
		linkedService, err = mf.GetServiceByName(splitSvcName[0], splitSvcName[1])
	}
	return &linkedService, err
}

func (mf sMetaFetcher) GetRegionName() (string, error) {
	return "region1", nil
}

func (mf sMetaFetcher) GetSelfHostUUID() (string, error) {
	return "", nil
}

func (mf sMetaFetcher) OnChange(intervalSeconds int, do func(string)) {
}

func (mf sMetaFetcher) GetContainer(envUUID string, containerName string) (*metadata.Container, error) {
	return nil, nil
}

func (cf sCertFetcher) FetchCertificates(lbMeta *LBMetadata, isDefaultCert bool) ([]*config.Certificate, error) {
	return nil, nil
}

func (cf sCertFetcher) FetchCertificate(certName string) (*config.Certificate, error) {
	return nil, nil
}

func (cf sCertFetcher) UpdateEndpoints(lbSvc *metadata.Service, eps []client.PublicEndpoint) error {
	return nil
}

func (cf sCertFetcher) ReadAllCertificatesFromDir(certDir string) []*config.Certificate {
	return nil
}

func (cf sCertFetcher) ReadDefaultCertificate(defaultCertDir string) *config.Certificate {
	return nil
}

func (cf sCertFetcher) LookForCertUpdates(do func(string)) {
}

func (p *sProvider) ApplyConfig(lbConfig *config.LoadBalancerConfig) error {
	return nil
}

func (p *sProvider) GetName() string {
	return ""
}

func (p *sProvider) GetPublicEndpoints(configName string) []string {
	return []string{}
}

func (p *sProvider) CleanupConfig(configName string) error {
	return nil
}

func (p *sProvider) Run(syncEndpointsQueue *utils.TaskQueue) {
}

func (p *sProvider) Stop() error {
	return nil
}

func (p *sProvider) IsHealthy() bool {
	return true
}

func (p *sProvider) ProcessCustomConfig(lbConfig *config.LoadBalancerConfig, customConfig string) error {
	return nil
}
