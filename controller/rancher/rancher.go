package rancher

import (
	"crypto/sha1"
	"encoding/hex"
	"fmt"
	"github.com/Sirupsen/logrus"
	"github.com/fsnotify/fsnotify"
	"github.com/rancher/go-rancher-metadata/metadata"
	"github.com/rancher/go-rancher/v2"
	"github.com/rancher/lb-controller/config"
	"github.com/rancher/lb-controller/controller"
	"github.com/rancher/lb-controller/provider"
	utils "github.com/rancher/lb-controller/utils"
	"os"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

func init() {
	lbc, err := NewLoadBalancerController()
	if err != nil {
		logrus.Fatalf("%v", err)
	}

	controller.RegisterController(lbc.GetName(), lbc)
}

func (lbc *LoadBalancerController) Init() {
	cattleURL := os.Getenv("CATTLE_URL")
	if len(cattleURL) == 0 {
		logrus.Fatalf("CATTLE_URL is not set, fail to init Rancher LB provider")
	}

	cattleAccessKey := os.Getenv("CATTLE_ACCESS_KEY")
	if len(cattleAccessKey) == 0 {
		logrus.Fatalf("CATTLE_ACCESS_KEY is not set, fail to init of Rancher LB provider")
	}

	cattleSecretKey := os.Getenv("CATTLE_SECRET_KEY")
	if len(cattleSecretKey) == 0 {
		logrus.Fatalf("CATTLE_SECRET_KEY is not set, fail to init of Rancher LB provider")
	}

	opts := &client.ClientOpts{
		Url:       cattleURL,
		AccessKey: cattleAccessKey,
		SecretKey: cattleSecretKey,
	}

	client, err := client.NewRancherClient(opts)
	if err != nil {
		logrus.Fatalf("Failed to create Rancher client %v", err)
	}

	w, err := fsnotify.NewWatcher()
	if err != nil {
		logrus.Fatalf("Error initializing the dir watcher %v", err)
	}

	certFetcher := &RCertificateFetcher{
		Client:  client,
		mu:      &sync.RWMutex{},
		Watcher: w,
	}

	lbc.CertFetcher = certFetcher
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
}

type MetadataFetcher interface {
	GetSelfService() (metadata.Service, error)
	GetService(envUUID string, svcName string, stackName string) (*metadata.Service, error)
	OnChange(intervalSeconds int, do func(string))
	GetServices() ([]metadata.Service, error)
	GetSelfHostUUID() (string, error)
	GetContainer(envUUID string, instanceName string) (*metadata.Container, error)
}

type RMetaFetcher struct {
	MetadataClient metadata.Client
}

func (lbc *LoadBalancerController) GetName() string {
	return "rancher"
}

func (lbc *LoadBalancerController) Run(provider provider.LBProvider, metadataURL string) {
	logrus.Infof("starting %s controller", lbc.GetName())
	lbc.LBProvider = provider
	go lbc.CertFetcher.ProcessFileUpdateEvents(lbc.ScheduleApplyConfig)

	go lbc.syncQueue.Run(time.Second, lbc.stopCh)

	go lbc.LBProvider.Run(nil)

	metadataClient, err := metadata.NewClientAndWait(metadataURL)
	if err != nil {
		logrus.Errorf("Error initiating metadata client: %v", err)
		lbc.Stop()
	}

	lbc.MetaFetcher = RMetaFetcher{
		MetadataClient: metadataClient,
	}

	lbc.MetaFetcher.OnChange(5, lbc.ScheduleApplyConfig)
	<-lbc.stopCh
}

func (mf RMetaFetcher) OnChange(intervalSeconds int, do func(string)) {
	mf.MetadataClient.OnChange(intervalSeconds, do)
}

func (lbc *LoadBalancerController) ScheduleApplyConfig(string) {
	logrus.Debug("Scheduling apply config")
	lbc.syncQueue.Enqueue(lbc.GetName())
}

func (lbc *LoadBalancerController) Stop() error {
	if !lbc.shutdown {
		logrus.Infof("Shutting down %s controller", lbc.GetName())
		//stop the provider
		if err := lbc.LBProvider.Stop(); err != nil {
			return err
		}

		//stop any file watcher
		if err := lbc.CertFetcher.StopWatcher(); err != nil {
			logrus.Infof("Error closing File Watcher %v", err)
		}

		close(lbc.stopCh)
		lbc.shutdown = true
	}

	return fmt.Errorf("shutdown already in progress")
}

func (lbc *LoadBalancerController) BuildConfigFromMetadata(lbName, envUUID, selfHostUUID, localServicePreference string, lbMeta *LBMetadata, lbLabels map[string]string) ([]*config.LoadBalancerConfig, error) {
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

	certDir, certDirSet := lbLabels["io.rancher.lb_service.cert_dir"]
	defaultCertDir, defaultCertDirSet := lbLabels["io.rancher.lb_service.default_cert_dir"]
	if certDirSet || defaultCertDirSet {
		if defaultCertDirSet {
			logrus.Debugf("Found defaultCertDir label %v", defaultCertDir)
			var err error
			defaultCert, err = lbc.CertFetcher.ReadDefaultCertificate(defaultCertDir)
			if err != nil {
				return nil, err
			}
			if defaultCert != nil {
				certs = append(certs, defaultCert)
			}
		}
		//read all the certificates from the mounted certDir
		if certDirSet {
			logrus.Debugf("Found certDir label %v", certDir)
			certsFromDir, err := lbc.CertFetcher.ReadAllCertificatesFromDir(certDir)
			if err != nil {
				return nil, err
			}
			certs = append(certs, certsFromDir...)
		}
	} else {
		for _, certName := range lbMeta.Certs {
			cert, err := lbc.CertFetcher.FetchCertificate(certName)
			if err != nil {
				return nil, err
			}
			certs = append(certs, cert)
		}
		var err error
		defaultCert, err = lbc.CertFetcher.FetchCertificate(lbMeta.DefaultCert)
		if err != nil {
			return nil, err
		}

		if defaultCert != nil {
			certs = append(certs, defaultCert)
		}
	}

	allBe := make(map[string]*config.BackendService)
	allEps := make(map[string]map[string]string)
	reg, err := regexp.Compile("[^A-Za-z0-9]+")
	if err != nil {
		return nil, err
	}
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
		if rule.Service != "" {
			// service comes in a format of stackName/serviceName,
			// replace "/"" with "_"
			svcName := strings.SplitN(rule.Service, "/", 2)
			service, err := lbc.MetaFetcher.GetService(envUUID, svcName[1], svcName[0])
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
		if backend != nil {
			epMap := allEps[pathUUID]
			for _, ep := range eps {
				if _, ok := epMap[ep.IP]; !ok {
					epMap[ep.IP] = ep.IP
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

	//Q: What does ProcessCustomConfig do?
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

	logrus.Debugf("lbSvc Labels: %v", lbSvc.Labels)
	return lbc.BuildConfigFromMetadata(lbSvc.Name, lbSvc.EnvironmentUUID, selfHostUUID, localServicePreference, lbMeta, lbSvc.Labels)
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
				}
				rules = append(rules, port)
			}

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

func IsActiveService(svc *metadata.Service) bool {
	inactiveStates := []string{"inactive", "deactivating", "removed", "removing"}
	for _, state := range inactiveStates {
		if strings.EqualFold(svc.State, state) {
			return false
		}
	}
	return true
}

func (mf RMetaFetcher) GetService(envUUID string, svcName string, stackName string) (*metadata.Service, error) {
	svcs, err := mf.MetadataClient.GetServices()
	if err != nil {
		return nil, err
	}
	var service metadata.Service
	for _, svc := range svcs {
		//only consider services from the same environment
		if !strings.EqualFold(svc.EnvironmentUUID, envUUID) {
			continue
		}
		if strings.EqualFold(svc.Name, svcName) && strings.EqualFold(svc.StackName, stackName) {
			service = svc
			break
		}
	}
	return &service, nil
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
		svcName := strings.SplitN(link, "/", 2)
		service, err := lbc.MetaFetcher.GetService(svc.EnvironmentUUID, svcName[1], svcName[0])
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
	if strings.EqualFold(c.State, "running") || strings.EqualFold(c.State, "starting") {
		ep := &config.Endpoint{
			Name: hashIP(c.PrimaryIp),
			IP:   c.PrimaryIp,
			Port: targetPort,
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
