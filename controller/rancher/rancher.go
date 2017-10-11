package rancher

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Sirupsen/logrus"
	revents "github.com/rancher/event-subscriber/events"
	"github.com/rancher/go-rancher-metadata/metadata"
	"github.com/rancher/go-rancher/v2"
	"github.com/rancher/lb-controller/config"
	"github.com/rancher/lb-controller/controller"
	"github.com/rancher/lb-controller/provider"
	utils "github.com/rancher/lb-controller/utils"

	"github.com/Sirupsen/logrus"
	"github.com/rancher/go-rancher-metadata/metadata"
	"github.com/rancher/go-rancher/v2"
	"github.com/rancher/lb-controller/config"
	"github.com/rancher/lb-controller/controller"
	"github.com/rancher/lb-controller/provider"
	utils "github.com/rancher/lb-controller/utils"
)

func init() {
	lbc, err := NewLoadBalancerController()
	if err != nil {
		logrus.Fatalf("%v", err)
	}

	controller.RegisterController(lbc.GetName(), lbc)
}

func (lbc *LoadBalancerController) Init(metadataURL string) {
	cattleURL := os.Getenv("CATTLE_URL")
	if len(cattleURL) == 0 {
		logrus.Fatalf("CATTLE_URL is not set, fail to init Rancher LB provider")
	}

	cattleEnvAdminAccessKey := os.Getenv("CATTLE_ENVIRONMENT_ADMIN_ACCESS_KEY")
	if len(cattleEnvAdminAccessKey) == 0 {
		logrus.Fatalf("CATTLE_ENVIRONMENT_ADMIN_ACCESS_KEY is not set, fail to init of Rancher LB provider")
	}

	cattleEnvAdminSecretKey := os.Getenv("CATTLE_ENVIRONMENT_ADMIN_SECRET_KEY")
	if len(cattleEnvAdminSecretKey) == 0 {
		logrus.Fatalf("CATTLE_ENVIRONMENT_ADMIN_SECRET_KEY is not set, fail to init of Rancher LB provider")
	}

	cattleAgentAccessKey := os.Getenv("CATTLE_AGENT_ACCESS_KEY")
	if len(cattleAgentAccessKey) == 0 {
		logrus.Fatalf("CATTLE_AGENT_ACCESS_KEY is not set, fail to init of Rancher LB provider")
	}

	cattleAgentSecretKey := os.Getenv("CATTLE_AGENT_SECRET_KEY")
	if len(cattleAgentSecretKey) == 0 {
		logrus.Fatalf("CATTLE_AGENT_SECRET_KEY is not set, fail to init of Rancher LB provider")
	}

	pollIntervalStr := os.Getenv("CERTS_POLL_INTERVAL")
	if len(pollIntervalStr) == 0 {
		logrus.Debugf("CERTS_POLL_INTERVAL is not set, will use default 30 seconds")
		pollIntervalStr = "30"
	}

	forceUpdateIntStr := os.Getenv("CERTS_FORCE_UPDATE_INTERVAL")
	if len(forceUpdateIntStr) == 0 {
		logrus.Debugf("CERTS_FORCE_UPDATE_INTERVAL is not set, will use default 300 seconds")
		forceUpdateIntStr = "300"
	}

	certFName := os.Getenv("CERT_FILE_NAME")
	if len(certFName) == 0 {
		logrus.Debugf("CERT_FILE_NAME is not set, will use default '%v'", DefaultCertName)
		certFName = DefaultCertName
	}

	keyFName := os.Getenv("KEY_FILE_NAME")
	if len(keyFName) == 0 {
		logrus.Debugf("KEY_FILE_NAME is not set, will use default '%v'", DefaultKeyName)
		keyFName = DefaultKeyName
	}

	opts := &client.ClientOpts{
		Url:       cattleURL,
		AccessKey: cattleEnvAdminAccessKey,
		SecretKey: cattleEnvAdminSecretKey,
	}

	client, err := client.NewRancherClient(opts)
	if err != nil {
		logrus.Fatalf("Failed to create Rancher client %v", err)
	}

	pollInterval, err := strconv.Atoi(pollIntervalStr)
	if err != nil {
		logrus.Fatalf("Failed to convert CERTS_POLL_INTERVAL %v", err)
	}
	forceUpdateInt, err := strconv.ParseFloat(forceUpdateIntStr, 64)
	if err != nil {
		logrus.Fatalf("Failed to convert CERTS_FORCE_UPDATE_INTERVAL %v", err)
	}

	metadataClient, err := metadata.NewClientAndWait(metadataURL)
	if err != nil {
		logrus.Fatalf("Error initiating metadata client: %v", err)
	}

	lbc.MetaFetcher = RMetaFetcher{
		MetadataClient: metadataClient,
	}

	lbSvc, err := lbc.MetaFetcher.GetSelfService()
	if err != nil {
		logrus.Fatalf("Error reading self service metadata: %v", err)
	}

	certDir := lbSvc.Labels["io.rancher.lb_service.cert_dir"]
	defaultCertDir := lbSvc.Labels["io.rancher.lb_service.default_cert_dir"]

	certFetcher := &RCertificateFetcher{
		Client:              client,
		mu:                  &sync.RWMutex{},
		updateCheckInterval: pollInterval,
		forceUpdateInterval: forceUpdateInt,
		CertDir:             certDir,
		DefaultCertDir:      defaultCertDir,
		CertName:            certFName,
		KeyName:             keyFName,
		initPollMu:          &sync.RWMutex{},
	}
	lbc.CertFetcher = certFetcher

	eHandler := &REventsHandler{
		CattleURL:       cattleURL,
		CattleAccessKey: cattleAgentAccessKey,
		CattleSecretKey: cattleAgentSecretKey,
		CheckOnEvent:    lbc.IsEndpointUpForDrain,
		DoOnEvent:       lbc.DrainEndpoint,
		PollStatus:      lbc.IsEndpointDrained,
		DoOnTimeout:     lbc.RemoveEndpointFromDrain,
		EventMap:        make(map[string]*revents.Event),
		EventMu:         &sync.RWMutex{},
	}
	lbc.EventsHandler = eHandler

}

type LoadBalancerController struct {
	shutdown                   bool
	stopCh                     chan struct{}
	LBProvider                 provider.LBProvider
	syncQueue                  *utils.TaskQueue
	opts                       *client.ClientOpts
	incrementalBackoff         int64
	incrementalBackoffInterval int64
	CertFetcher                CertificateFetcher
	MetaFetcher                MetadataFetcher
	EventsHandler              EventsHandler
}

type MetadataFetcher interface {
	GetSelfService() (metadata.Service, error)
	GetService(link string) (*metadata.Service, error)
	OnChange(intervalSeconds int, do func(string))
	GetServices() ([]metadata.Service, error)
	GetSelfHostUUID() (string, error)
	GetContainer(envUUID string, instanceName string) (*metadata.Container, error)
	GetRegionName() (string, error)
	GetServiceFromRegionEnvironment(regionName string, envName string, stackName string, svcName string) (metadata.Service, error)
	GetServiceInLocalRegion(envName string, stackName string, svcName string) (metadata.Service, error)
	GetServiceInLocalEnvironment(stackName string, svcName string) (metadata.Service, error)
	GetServicesFromRegionEnvironment(regionName string, envName string) ([]metadata.Service, error)
	GetServicesInLocalRegion(envName string) ([]metadata.Service, error)
}

type RMetaFetcher struct {
	MetadataClient metadata.Client
}

func (lbc *LoadBalancerController) GetName() string {
	return "rancher"
}

func (lbc *LoadBalancerController) Run(provider provider.LBProvider) {
	logrus.Infof("starting %s controller", lbc.GetName())
	lbc.LBProvider = provider

	go lbc.syncQueue.Run(time.Second, lbc.stopCh)

	go lbc.LBProvider.Run(nil)

	go func() {
		for {
			err := lbc.EventsHandler.Subscribe()
			if err != nil {
				logrus.Errorf("Error subscribing to events: %v", err)
			} else {
				break
			}
		}
	}()

	go lbc.CertFetcher.LookForCertUpdates(lbc.ScheduleApplyConfig)

	lbc.MetaFetcher.OnChange(5, lbc.ScheduleApplyConfig)
	<-lbc.stopCh
}

func (mf RMetaFetcher) OnChange(intervalSeconds int, do func(string)) {
	mf.MetadataClient.OnChange(intervalSeconds, do)
}

func (mf RMetaFetcher) GetServicesInLocalRegion(envName string) ([]metadata.Service, error) {
	return mf.MetadataClient.GetServicesInLocalRegion(envName)
}

func (mf RMetaFetcher) GetServicesFromRegionEnvironment(regionName string, envName string) ([]metadata.Service, error) {
	return mf.MetadataClient.GetServicesFromRegionEnvironment(regionName, envName)
}

func (lbc *LoadBalancerController) ScheduleApplyConfig(string) {
	logrus.Debug("Scheduling apply config")
	lbc.syncQueue.Enqueue(lbc.GetName())
}

func (lbc *LoadBalancerController) IsEndpointUpForDrain(ep *config.Endpoint) bool {
	if lbc.LBProvider.IsEndpointUpForDrain(ep) {
		logrus.Debug("DrainEndpoint: The endpoint is already in drainlist")
		return true
	}
	return false
}

func (lbc *LoadBalancerController) DrainEndpoint(ep *config.Endpoint) bool {
	return lbc.LBProvider.DrainEndpoint(ep)
}

func (lbc *LoadBalancerController) IsEndpointDrained(ep *config.Endpoint) bool {
	return lbc.LBProvider.IsEndpointDrained(ep)
}

func (lbc *LoadBalancerController) RemoveEndpointFromDrain(ep *config.Endpoint) {
	lbc.LBProvider.RemoveEndpointFromDrain(ep)
}

func (lbc *LoadBalancerController) Stop() error {
	if !lbc.shutdown {
		logrus.Infof("Shutting down %s controller", lbc.GetName())
		//stop the provider
		if err := lbc.LBProvider.Stop(); err != nil {
			return err
		}

		close(lbc.stopCh)
		lbc.shutdown = true
	}

	return fmt.Errorf("shutdown already in progress")
}

func (lbc *LoadBalancerController) BuildConfigFromMetadata(lbName, envUUID, selfHostUUID, localServicePreference string, lbMeta *LBMetadata) ([]*config.LoadBalancerConfig, error) {
	lbConfigs := []*config.LoadBalancerConfig{}
	if lbMeta == nil {
		lbMeta = &LBMetadata{
			PortRules:   make([]metadata.PortRule, 0),
			Certs:       make([]string, 0),
			DefaultCert: "",
			Config:      "",
		}
	}
	frontendsMap := map[string]*config.FrontendService{}

	// fetch certificates either from mounted certDir or from the cattle
	certs := []*config.Certificate{}
	var defaultCert *config.Certificate

	defCerts, err := lbc.CertFetcher.FetchCertificates(lbMeta, true)
	if err != nil {
		return nil, err
	}
	if len(defCerts) > 0 {
		defaultCert = defCerts[0]
		if defaultCert != nil {
			certs = append(certs, defaultCert)
		}
	}

	alternateCerts, err := lbc.CertFetcher.FetchCertificates(lbMeta, false)
	if err != nil {
		return nil, err
	}
	certs = append(certs, alternateCerts...)

	logrus.Debugf("Found %v certs", len(certs))

	allBe := make(map[string]*config.BackendService)
	allEps := make(map[string]map[string]string)
	reg, err := regexp.Compile("[^A-Za-z0-9]+")
	if err != nil {
		return nil, err
	}
	logrus.Info("length of lbMeta.PortRules  ", len(lbMeta.PortRules))
	for _, rule := range lbMeta.PortRules {
		if rule.SourcePort < 1 {
			continue
		}
		var frontend *config.FrontendService
		name := strconv.Itoa(rule.SourcePort)
		if val, ok := frontendsMap[name]; ok {
			frontend = val
		} else {
			backends := []*config.BackendService{}
			frontend = &config.FrontendService{
				Name:            name,
				Port:            rule.SourcePort,
				Protocol:        rule.Protocol,
				BackendServices: backends,
			}
		}

		var eps config.Endpoints
		var hc *config.HealthCheck
		logrus.Info("rule.Service ", rule.Service)
		if rule.Service != "" {
			service, err := lbc.MetaFetcher.GetService(rule.Service)
			if err != nil {
				return nil, err
			}
			if service == nil || !IsActiveService(service) {
				continue
			}
			eps, err = lbc.getServiceEndpoints(service, rule.TargetPort, selfHostUUID, localServicePreference)
			if err != nil {
				return nil, err
			}
			hc, err = getServiceHealthCheck(service)
			if err != nil {
				return nil, err
			}
		} else {
			container, err := lbc.MetaFetcher.GetContainer(envUUID, rule.Container)
			if err != nil {
				return nil, err
			}
			ep, _ := getContainerEndpoint(container, rule.TargetPort, selfHostUUID, localServicePreference)
			if ep == nil {
				continue
			}
			eps = append(eps, ep)
			hc, err = getContainerHealthcheck(container)
			if err != nil {
				return nil, err
			}
		}

		comparator := config.EqRuleComparator
		path := rule.Path
		hostname := rule.Hostname
		if !(strings.EqualFold(rule.Protocol, config.HTTPSProto) || strings.EqualFold(rule.Protocol, config.HTTPProto) || strings.EqualFold(rule.Protocol, config.SNIProto)) {
			path = ""
			hostname = ""
		}

		if len(hostname) > 2 {
			if strings.HasPrefix(hostname, "*") {
				hostname = hostname[1:len(hostname)]
				comparator = config.EndRuleComparator
			} else if strings.HasSuffix(hostname, "*") {
				hostname = hostname[:len(hostname)-1]
				comparator = config.BegRuleComparator
			}
		}

		pathUUID := fmt.Sprintf("%v_%s_%s", rule.SourcePort, hostname, path)
		backend := allBe[pathUUID]
		logrus.Info("pathUUID ", pathUUID)
		if backend != nil {
			epMap := allEps[pathUUID]
			for _, ep := range eps {
				if _, ok := epMap[ep.IP]; !ok {
					epMap[ep.IP] = ep.IP
					ep.Weight = rule.Weight
					backend.Endpoints = append(backend.Endpoints, ep)
				}
			}
		} else {
			UUID := rule.BackendName
			if UUID == "" {
				//replace all non alphanumeric with _
				UUID = reg.ReplaceAllString(pathUUID, "_")
			}
			backend := &config.BackendService{
				UUID:           UUID,
				Host:           hostname,
				Path:           path,
				Port:           rule.TargetPort,
				Protocol:       rule.Protocol,
				RuleComparator: comparator,
				Endpoints:      eps,
				HealthCheck:    hc,
				Priority:       rule.Priority,
			}
			allBe[pathUUID] = backend
			frontend.BackendServices = append(frontend.BackendServices, backend)
			epMap := make(map[string]string)
			for _, ep := range eps {
				epMap[ep.IP] = ep.IP
				ep.Weight = rule.Weight
			}
			allEps[pathUUID] = epMap
		}

		frontendsMap[name] = frontend
	}

	var frontends config.FrontendServices
	for _, v := range frontendsMap {
		// sort backends
		sort.Sort(v.BackendServices)
		frontends = append(frontends, v)
	}

	//sort frontends
	sort.Sort(frontends)

	lbConfig := &config.LoadBalancerConfig{
		Name:             lbName,
		FrontendServices: frontends,
		Certs:            certs,
		DefaultCert:      defaultCert,
		StickinessPolicy: &lbMeta.StickinessPolicy,
	}

	if err = lbc.LBProvider.ProcessCustomConfig(lbConfig, lbMeta.Config); err != nil {
		return nil, err
	}

	lbConfigs = append(lbConfigs, lbConfig)
	return lbConfigs, nil
}

func (mf RMetaFetcher) GetSelfService() (metadata.Service, error) {
	return mf.MetadataClient.GetSelfService()
}

func (mf RMetaFetcher) GetSelfHostUUID() (string, error) {
	host, err := mf.MetadataClient.GetSelfHost()
	if err != nil {
		return "", err
	}
	return host.UUID, nil
}

func (lbc *LoadBalancerController) GetLBConfigs() ([]*config.LoadBalancerConfig, error) {
	lbSvc, err := lbc.MetaFetcher.GetSelfService()
	if err != nil {
		return nil, err
	}

	lbMeta, err := lbc.CollectLBMetadata(lbSvc)
	if err != nil {
		return nil, err
	}

	selfHostUUID := ""
	localServicePreference := "any"

	if val, ok := lbSvc.Labels["io.rancher.lb_service.target"]; ok {
		selfHostUUID, err = lbc.MetaFetcher.GetSelfHostUUID()
		if err != nil {
			return nil, err
		}
		localServicePreference = val
		if val != "any" && val != "only-local" && val != "prefer-local" {
			return nil, fmt.Errorf("Invalid label value for label io.rancher.lb_service.target=%s", val)
		}
	}

	return lbc.BuildConfigFromMetadata(lbSvc.Name, lbSvc.EnvironmentUUID, selfHostUUID, localServicePreference, lbMeta)
}

func (lbc *LoadBalancerController) AddRulesFromRegions(rules []metadata.PortRule, lbRule metadata.PortRule) ([]metadata.PortRule, error) {
	var externalsvcs []metadata.Service
	var err error
	if lbRule.Region != "" {
		if lbRule.Environment != "" {
			externalsvcs, err = lbc.MetaFetcher.GetServicesFromRegionEnvironment(lbRule.Region, lbRule.Environment)
		}
	} else if lbRule.Environment != "" {
		externalsvcs, err = lbc.MetaFetcher.GetServicesInLocalRegion(lbRule.Environment)
	}
	if err != nil {
		return rules, err
	}
	for _, svc := range externalsvcs {
		if !IsSelectorMatch(lbRule.Selector, svc.Labels) {
			continue
		}
		lbConfig := svc.LBConfig
		if len(lbConfig.PortRules) == 0 {
			if lbRule.TargetPort == 0 {
				continue
			}
		}
		meta, err := GetLBMetadata(lbConfig)
		if err != nil {
			return rules, err
		}
		var svcName string
		if lbRule.Region != "" {
			svcName = fmt.Sprintf("%s/%s/%s/%s", lbRule.Region, lbRule.Environment, svc.StackName, svc.Name)
		} else if lbRule.Environment != "" {
			svcName = fmt.Sprintf("%s/%s/%s", lbRule.Environment, svc.StackName, svc.Name)
		}
		if len(meta.PortRules) > 0 {
			for _, rule := range meta.PortRules {
				port := metadata.PortRule{
					SourcePort:  lbRule.SourcePort,
					Protocol:    lbRule.Protocol,
					Path:        rule.Path,
					Hostname:    rule.Hostname,
					Service:     svcName,
					TargetPort:  rule.TargetPort,
					BackendName: rule.BackendName,
					Region:      lbRule.Region,
					Environment: lbRule.Environment,
					Weight:      lbRule.Weight,
				}
				rules = append(rules, port)
			}
		} else {
			// register the service to the lb service port rule
			// having target port is a requirement
			port := metadata.PortRule{
				SourcePort:  lbRule.SourcePort,
				Protocol:    lbRule.Protocol,
				Path:        lbRule.Path,
				Hostname:    lbRule.Hostname,
				Service:     svcName,
				TargetPort:  lbRule.TargetPort,
				BackendName: lbRule.BackendName,
				Region:      lbRule.Region,
				Environment: lbRule.Environment,
				Weight:      lbRule.Weight,
			}
			rules = append(rules, port)
		}
	}
	return rules, nil
}

func (lbc *LoadBalancerController) CollectLBMetadata(lbSvc metadata.Service) (*LBMetadata, error) {
	lbConfig := lbSvc.LBConfig

	lbMeta, err := GetLBMetadata(lbConfig)
	if err != nil {
		return nil, err
	}

	if err = lbc.processSelector(lbMeta); err != nil {
		return nil, err
	}
	return lbMeta, nil
}

func (lbc *LoadBalancerController) processSelector(lbMeta *LBMetadata) error {
	//collect selector based services
	var rules []metadata.PortRule
	svcs, err := lbc.MetaFetcher.GetServices()
	if err != nil {
		return err
	}

	for _, lbRule := range lbMeta.PortRules {
		if lbRule.Selector == "" {
			rules = append(rules, lbRule)
			continue
		}

		for _, svc := range svcs {
			if !IsSelectorMatch(lbRule.Selector, svc.Labels) {
				continue
			}
			lbConfig := svc.LBConfig
			if len(lbConfig.PortRules) == 0 {
				if lbRule.TargetPort == 0 {
					continue
				}
			}

			meta, err := GetLBMetadata(lbConfig)
			if err != nil {
				return err
			}

			svcName := fmt.Sprintf("%s/%s", svc.StackName, svc.Name)
			if len(meta.PortRules) > 0 {
				for _, rule := range meta.PortRules {
					port := metadata.PortRule{
						SourcePort:  lbRule.SourcePort,
						Protocol:    lbRule.Protocol,
						Path:        rule.Path,
						Hostname:    rule.Hostname,
						Service:     svcName,
						TargetPort:  rule.TargetPort,
						BackendName: rule.BackendName,
						Weight:      lbRule.Weight,
					}
					rules = append(rules, port)
				}
			} else {
				// register the service to the lb service port rule
				// having target port is a requirement
				port := metadata.PortRule{
					SourcePort:  lbRule.SourcePort,
					Protocol:    lbRule.Protocol,
					Path:        lbRule.Path,
					Hostname:    lbRule.Hostname,
					Service:     svcName,
					TargetPort:  lbRule.TargetPort,
					BackendName: lbRule.BackendName,
					Weight:      lbRule.Weight,
				}
				rules = append(rules, port)
			}

		}
		var err error
		rules, err = lbc.AddRulesFromRegions(rules, lbRule)
		if err != nil {
			return err
		}
	}

	lbMeta.PortRules = rules
	return nil
}

func getServiceHealthCheck(svc *metadata.Service) (*config.HealthCheck, error) {
	if &svc.HealthCheck == nil {
		return nil, nil
	}
	return getConfigServiceHealthCheck(svc.HealthCheck)
}

func getContainerHealthcheck(c *metadata.Container) (*config.HealthCheck, error) {
	if &c.HealthCheck == nil {
		return nil, nil
	}
	return getConfigServiceHealthCheck(c.HealthCheck)
}

func (mf RMetaFetcher) GetServices() ([]metadata.Service, error) {
	return mf.MetadataClient.GetServices()
}

func (mf RMetaFetcher) GetRegionName() (string, error) {
	return mf.MetadataClient.GetRegionName()
}

func (mf RMetaFetcher) GetServiceFromRegionEnvironment(regionName string, envName string, stackName string, svcName string) (metadata.Service, error) {
	return mf.MetadataClient.GetServiceFromRegionEnvironment(regionName, envName, stackName, svcName)
}

func (mf RMetaFetcher) GetServiceInLocalRegion(envName string, stackName string, svcName string) (metadata.Service, error) {
	return mf.MetadataClient.GetServiceInLocalRegion(envName, stackName, svcName)
}

func (mf RMetaFetcher) GetServiceInLocalEnvironment(stackName string, svcName string) (metadata.Service, error) {
	return mf.MetadataClient.GetServiceInLocalEnvironment(stackName, svcName)
}

func IsActiveService(svc *metadata.Service) bool {
	inactiveStates := []string{"inactive", "deactivating", "removed", "removing"}
	for _, state := range inactiveStates {
		if strings.EqualFold(svc.State, state) {
			return false
		}
	}
	return true
}

func (mf RMetaFetcher) GetService(link string) (*metadata.Service, error) {
	splitSvcName := strings.Split(link, "/")
	var linkedService metadata.Service
	var err error

	if len(splitSvcName) == 4 {
		linkedService, err = mf.GetServiceFromRegionEnvironment(splitSvcName[0], splitSvcName[1], splitSvcName[2], splitSvcName[3])
	} else if len(splitSvcName) == 3 {
		linkedService, err = mf.GetServiceInLocalRegion(splitSvcName[0], splitSvcName[1], splitSvcName[2])
	} else {
		linkedService, err = mf.GetServiceInLocalEnvironment(splitSvcName[0], splitSvcName[1])
	}
	return &linkedService, err
}

func (mf RMetaFetcher) GetContainer(envUUID string, containerName string) (*metadata.Container, error) {
	cs, err := mf.MetadataClient.GetContainers()
	if err != nil {
		return nil, err
	}
	var container metadata.Container
	for _, c := range cs {
		//only consider containers from the same environment
		if !strings.EqualFold(c.EnvironmentUUID, envUUID) {
			continue
		}
		if strings.EqualFold(c.Name, containerName) {
			container = c
			break
		}
	}
	return &container, nil
}

func (lbc *LoadBalancerController) getServiceEndpoints(svc *metadata.Service, targetPort int, selfHostUUID, localServicePreference string) (config.Endpoints, error) {
	var eps config.Endpoints
	var err error
	if strings.EqualFold(svc.Kind, "externalService") {
		eps = lbc.getExternalServiceEndpoints(svc, targetPort)
	} else if strings.EqualFold(svc.Kind, "dnsService") {
		eps, err = lbc.getAliasServiceEndpoints(svc, targetPort, selfHostUUID, localServicePreference)
		if err != nil {
			return nil, err
		}
	} else {
		eps = lbc.getRegularServiceEndpoints(svc, targetPort, selfHostUUID, localServicePreference)
	}

	// sort endpoints
	sort.Sort(eps)
	return eps, nil
}

func (lbc *LoadBalancerController) getAliasServiceEndpoints(svc *metadata.Service, targetPort int, selfHostUUID, localServicePreference string) (config.Endpoints, error) {
	var eps config.Endpoints
	for link := range svc.Links {
		service, err := lbc.MetaFetcher.GetService(link)
		if err != nil {
			return nil, err
		}
		if service == nil {
			continue
		}
		newEps, err := lbc.getServiceEndpoints(service, targetPort, selfHostUUID, localServicePreference)
		if err != nil {
			return nil, err
		}
		eps = append(eps, newEps...)
	}
	return eps, nil
}

func (lbc *LoadBalancerController) getExternalServiceEndpoints(svc *metadata.Service, targetPort int) config.Endpoints {
	var eps config.Endpoints
	for _, e := range svc.ExternalIps {
		ep := &config.Endpoint{
			Name: hashIP(e),
			IP:   e,
			Port: targetPort,
		}
		eps = append(eps, ep)
	}

	if svc.Hostname != "" {
		ep := &config.Endpoint{
			Name:    svc.Hostname,
			IP:      svc.Hostname,
			Port:    targetPort,
			IsCname: true,
		}
		eps = append(eps, ep)
	}
	return eps
}

func (lbc *LoadBalancerController) getRegularServiceEndpoints(svc *metadata.Service, targetPort int, selfHostUUID, localServicePreference string) config.Endpoints {
	var eps config.Endpoints
	var contingencyEps config.Endpoints
	for _, c := range svc.Containers {
		ep, isContigency := getContainerEndpoint(&c, targetPort, selfHostUUID, localServicePreference)
		if ep == nil {
			continue
		}
		if isContigency {
			contingencyEps = append(contingencyEps, ep)
			continue
		}
		eps = append(eps, ep)
	}

	if localServicePreference == "prefer-local" && len(eps) == 0 {
		return contingencyEps
	}
	return eps
}

func getContainerEndpoint(c *metadata.Container, targetPort int, selfHostUUID string, localServicePreference string) (*config.Endpoint, bool) {
	if strings.EqualFold(c.State, "running") || strings.EqualFold(c.State, "starting") || strings.EqualFold(c.State, "stopping") {
		ep := &config.Endpoint{
			Name: hashIP(c.PrimaryIp),
			IP:   c.PrimaryIp,
			Port: targetPort,
		}
		if strings.EqualFold(c.State, "stopping") {
			ep.Weight = "0"
		}

		if localServicePreference != "any" && !strings.EqualFold(c.HostUUID, selfHostUUID) {
			return ep, true
		}
		return ep, false
	}
	return nil, false
}

func (lbc *LoadBalancerController) IsHealthy() bool {
	return true
}

func NewLoadBalancerController() (*LoadBalancerController, error) {
	lbc := &LoadBalancerController{
		stopCh:                     make(chan struct{}),
		incrementalBackoff:         0,
		incrementalBackoffInterval: 5,
	}
	lbc.syncQueue = utils.NewTaskQueue(lbc.sync)

	return lbc, nil
}

func (lbc *LoadBalancerController) sync(key string) {
	if lbc.shutdown {
		//skip syncing if controller is being shut down
		return
	}
	logrus.Debugf("Syncing up LB")
	requeue := false
	cfgs, err := lbc.GetLBConfigs()
	if err == nil {
		for _, cfg := range cfgs {
			if err := lbc.LBProvider.ApplyConfig(cfg); err != nil {
				logrus.Errorf("Failed to apply lb config on provider: %v", err)
				requeue = true
			}
		}
	} else {
		logrus.Errorf("Failed to get lb config: %v", err)
		requeue = true
	}

	if requeue {
		go lbc.requeue(key)
	} else {
		//clear up the backoff
		lbc.incrementalBackoff = 0
	}
}

func (lbc *LoadBalancerController) requeue(key string) {
	// requeue only when after incremental backoff time
	lbc.incrementalBackoff = lbc.incrementalBackoff + lbc.incrementalBackoffInterval
	time.Sleep(time.Duration(lbc.incrementalBackoff) * time.Second)
	lbc.syncQueue.Requeue(key, fmt.Errorf("retrying sync as one of the configs failed to apply on a backend"))
}

func hashIP(ip string) string {
	h := sha1.New()
	h.Write([]byte(ip))
	return hex.EncodeToString(h.Sum(nil))
}
