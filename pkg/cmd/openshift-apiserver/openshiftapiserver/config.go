package openshiftapiserver

import (
	"bytes"
	"fmt"
	"io/ioutil"
	"net/http"
	"time"

	"github.com/golang/glog"

	"k8s.io/apiserver/pkg/admission"
	admissionmetrics "k8s.io/apiserver/pkg/admission/metrics"
	genericapiserver "k8s.io/apiserver/pkg/server"
	genericapiserveroptions "k8s.io/apiserver/pkg/server/options"
	cacheddiscovery "k8s.io/client-go/discovery/cached"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/restmapper"
	"k8s.io/client-go/tools/cache"
	"k8s.io/kubernetes/pkg/api/legacyscheme"

	configv1 "github.com/openshift/api/config/v1"
	openshiftcontrolplanev1 "github.com/openshift/api/openshiftcontrolplane/v1"
	"github.com/openshift/library-go/pkg/config/helpers"
	"github.com/openshift/origin/pkg/admission/namespaceconditions"
	originadmission "github.com/openshift/origin/pkg/apiserver/admission"
	"github.com/openshift/origin/pkg/cmd/openshift-apiserver/openshiftapiserver/configprocessing"
	configlatest "github.com/openshift/origin/pkg/cmd/server/apis/config/latest"
	"github.com/openshift/origin/pkg/image/apiserver/registryhostname"
	usercache "github.com/openshift/origin/pkg/user/cache"
	"github.com/openshift/origin/pkg/version"
)

func NewOpenshiftAPIConfig(config *openshiftcontrolplanev1.OpenShiftAPIServerConfig) (*OpenshiftAPIConfig, error) {
	kubeClientConfig, err := helpers.GetKubeClientConfig(config.KubeClientConfig)
	if err != nil {
		return nil, err
	}
	kubeClient, err := kubernetes.NewForConfig(kubeClientConfig)
	if err != nil {
		return nil, err
	}
	kubeInformers := informers.NewSharedInformerFactory(kubeClient, 10*time.Minute)

	openshiftVersion := version.Get()

	backend, policyChecker, err := configprocessing.GetAuditConfig(config.AuditConfig)
	if err != nil {
		return nil, err
	}
	restOptsGetter, err := NewRESTOptionsGetter(config.APIServerArguments, config.StorageConfig)
	if err != nil {
		return nil, err
	}

	genericConfig := genericapiserver.NewRecommendedConfig(legacyscheme.Codecs)
	// Current default values
	//Serializer:                   codecs,
	//ReadWritePort:                443,
	//BuildHandlerChainFunc:        DefaultBuildHandlerChain,
	//HandlerChainWaitGroup:        new(utilwaitgroup.SafeWaitGroup),
	//LegacyAPIGroupPrefixes:       sets.NewString(DefaultLegacyAPIPrefix),
	//DisabledPostStartHooks:       sets.NewString(),
	//HealthzChecks:                []healthz.HealthzChecker{healthz.PingHealthz, healthz.LogHealthz},
	//EnableIndex:                  true,
	//EnableDiscovery:              true,
	//EnableProfiling:              true,
	//EnableMetrics:                true,
	//MaxRequestsInFlight:          400,
	//MaxMutatingRequestsInFlight:  200,
	//RequestTimeout:               time.Duration(60) * time.Second,
	//MinRequestTimeout:            1800,
	//EnableAPIResponseCompression: utilfeature.DefaultFeatureGate.Enabled(features.APIResponseCompression),
	//LongRunningFunc: genericfilters.BasicLongRunningRequestCheck(sets.NewString("watch"), sets.NewString()),

	// TODO this is actually specific to the kubeapiserver
	//RuleResolver authorizer.RuleResolver
	genericConfig.SharedInformerFactory = kubeInformers
	genericConfig.ClientConfig = kubeClientConfig

	// these are set via options
	//SecureServing *SecureServingInfo
	//Authentication AuthenticationInfo
	//Authorization AuthorizationInfo
	//LoopbackClientConfig *restclient.Config
	// this is set after the options are overlayed to get the authorizer we need.
	//AdmissionControl      admission.Interface
	//ReadWritePort int
	//PublicAddress net.IP

	// these are defaulted sanely during complete
	//DiscoveryAddresses discovery.Addresses

	genericConfig.CorsAllowedOriginList = config.CORSAllowedOrigins
	genericConfig.Version = &openshiftVersion
	// we don't use legacy audit anymore
	genericConfig.AuditBackend = backend
	genericConfig.AuditPolicyChecker = policyChecker
	genericConfig.ExternalAddress = "apiserver.openshift-apiserver.svc"
	genericConfig.BuildHandlerChainFunc = OpenshiftHandlerChain
	genericConfig.RequestInfoResolver = configprocessing.OpenshiftRequestInfoResolver()
	genericConfig.OpenAPIConfig = configprocessing.DefaultOpenAPIConfig(nil)
	genericConfig.RESTOptionsGetter = restOptsGetter
	// previously overwritten.  I don't know why
	genericConfig.RequestTimeout = time.Duration(60) * time.Second
	genericConfig.MinRequestTimeout = int(config.ServingInfo.RequestTimeoutSeconds)
	genericConfig.MaxRequestsInFlight = int(config.ServingInfo.MaxRequestsInFlight)
	genericConfig.MaxMutatingRequestsInFlight = int(config.ServingInfo.MaxRequestsInFlight / 2)
	genericConfig.LongRunningFunc = configprocessing.IsLongRunningRequest

	// I'm just hoping this works.  I don't think we use it.
	//MergedResourceConfig *serverstore.ResourceConfig

	servingOptions, err := configprocessing.ToServingOptions(config.ServingInfo)
	if err != nil {
		return nil, err
	}
	if err := servingOptions.ApplyTo(&genericConfig.Config.SecureServing, &genericConfig.Config.LoopbackClientConfig); err != nil {
		return nil, err
	}
	authenticationOptions := genericapiserveroptions.NewDelegatingAuthenticationOptions()
	// keep working for integration tests
	if len(config.AggregatorConfig.ClientCA) > 0 {
		authenticationOptions.ClientCert.ClientCA = config.ServingInfo.ClientCA
		authenticationOptions.RequestHeader.ClientCAFile = config.AggregatorConfig.ClientCA
		authenticationOptions.RequestHeader.AllowedNames = config.AggregatorConfig.AllowedNames
		authenticationOptions.RequestHeader.UsernameHeaders = config.AggregatorConfig.UsernameHeaders
		authenticationOptions.RequestHeader.GroupHeaders = config.AggregatorConfig.GroupHeaders
		authenticationOptions.RequestHeader.ExtraHeaderPrefixes = config.AggregatorConfig.ExtraHeaderPrefixes
	}
	authenticationOptions.RemoteKubeConfigFile = config.KubeClientConfig.KubeConfig
	if err := authenticationOptions.ApplyTo(&genericConfig.Authentication, genericConfig.SecureServing, genericConfig.OpenAPIConfig); err != nil {
		return nil, err
	}
	authorizationOptions := genericapiserveroptions.NewDelegatingAuthorizationOptions().WithAlwaysAllowPaths("/healthz", "/healthz/").WithAlwaysAllowGroups("system:masters")
	authorizationOptions.RemoteKubeConfigFile = config.KubeClientConfig.KubeConfig
	if err := authorizationOptions.ApplyTo(&genericConfig.Authorization); err != nil {
		return nil, err
	}

	informers, err := NewInformers(kubeInformers, kubeClientConfig, genericConfig.LoopbackClientConfig)
	if err != nil {
		return nil, err
	}
	if err := informers.GetOpenshiftUserInformers().User().V1().Groups().Informer().AddIndexers(cache.Indexers{
		usercache.ByUserIndexName: usercache.ByUserIndexKeys,
	}); err != nil {
		return nil, err
	}

	projectCache, err := NewProjectCache(informers.kubernetesInformers.Core().V1().Namespaces(), kubeClientConfig, config.ProjectConfig.DefaultNodeSelector)
	if err != nil {
		return nil, err
	}
	clusterQuotaMappingController := NewClusterQuotaMappingController(informers.kubernetesInformers.Core().V1().Namespaces(), informers.quotaInformers.Quota().InternalVersion().ClusterResourceQuotas())
	discoveryClient := cacheddiscovery.NewMemCacheClient(kubeClient.Discovery())
	restMapper := restmapper.NewDeferredDiscoveryRESTMapper(discoveryClient)
	admissionInitializer, err := originadmission.NewPluginInitializer(config.ImagePolicyConfig.ExternalRegistryHostnames, config.ImagePolicyConfig.InternalRegistryHostname, config.CloudProviderFile, kubeClientConfig, informers, genericConfig.Authorization.Authorizer, projectCache, restMapper, clusterQuotaMappingController)
	if err != nil {
		return nil, err
	}
	namespaceLabelDecorator := namespaceconditions.NamespaceLabelConditions{
		NamespaceClient: kubeClient.CoreV1(),
		NamespaceLister: informers.GetKubernetesInformers().Core().V1().Namespaces().Lister(),

		SkipLevelZeroNames: originadmission.SkipRunLevelZeroPlugins,
		SkipLevelOneNames:  originadmission.SkipRunLevelOnePlugins,
	}
	admissionDecorators := admission.Decorators{
		admission.DecoratorFunc(namespaceLabelDecorator.WithNamespaceLabelConditions),
		admission.DecoratorFunc(admissionmetrics.WithControllerMetrics),
	}
	explicitOn := []string{}
	explicitOff := []string{}
	for plugin, config := range config.AdmissionPluginConfig {
		enabled, err := isAdmissionPluginActivated(config)
		if err != nil {
			return nil, err
		}
		if enabled {
			glog.V(2).Infof("Enabling %s", plugin)
			explicitOn = append(explicitOn, plugin)
		} else {
			glog.V(2).Infof("Disabling %s", plugin)
			explicitOff = append(explicitOff, plugin)
		}
	}
	genericConfig.AdmissionControl, err = originadmission.NewAdmissionChains([]string{}, explicitOn, explicitOff, config.AdmissionPluginConfig, admissionInitializer, admissionDecorators)
	if err != nil {
		return nil, err
	}

	var externalRegistryHostname string
	if len(config.ImagePolicyConfig.ExternalRegistryHostnames) > 0 {
		externalRegistryHostname = config.ImagePolicyConfig.ExternalRegistryHostnames[0]
	}
	registryHostnameRetriever, err := registryhostname.DefaultRegistryHostnameRetriever(kubeClientConfig, externalRegistryHostname, config.ImagePolicyConfig.InternalRegistryHostname)
	if err != nil {
		return nil, err
	}
	imageLimitVerifier := ImageLimitVerifier(informers.internalKubernetesInformers.Core().InternalVersion().LimitRanges())

	var caData []byte
	if len(config.ImagePolicyConfig.AdditionalTrustedCA) != 0 {
		glog.V(2).Infof("Image import using additional CA path: %s", config.ImagePolicyConfig.AdditionalTrustedCA)
		var err error
		caData, err = ioutil.ReadFile(config.ImagePolicyConfig.AdditionalTrustedCA)
		if err != nil {
			return nil, fmt.Errorf("failed to read CA bundle %s for image importing: %v", config.ImagePolicyConfig.AdditionalTrustedCA, err)
		}
	}

	subjectLocator := NewSubjectLocator(informers.GetKubernetesInformers().Rbac().V1())
	projectAuthorizationCache := NewProjectAuthorizationCache(
		subjectLocator,
		informers.GetInternalKubernetesInformers().Core().InternalVersion().Namespaces().Informer(),
		informers.GetKubernetesInformers().Rbac().V1(),
	)

	routeAllocator, err := configprocessing.RouteAllocator(config.RoutingConfig.Subdomain)
	if err != nil {
		return nil, err
	}

	ruleResolver := NewRuleResolver(informers.kubernetesInformers.Rbac().V1())

	ret := &OpenshiftAPIConfig{
		GenericConfig: genericConfig,
		ExtraConfig: OpenshiftAPIExtraConfig{
			InformerStart:                      informers.Start,
			KubeAPIServerClientConfig:          kubeClientConfig,
			KubeInternalInformers:              informers.internalKubernetesInformers,
			KubeInformers:                      kubeInformers, // TODO remove this and use the one from the genericconfig
			QuotaInformers:                     informers.quotaInformers,
			SecurityInformers:                  informers.securityInformers,
			RuleResolver:                       ruleResolver,
			SubjectLocator:                     subjectLocator,
			LimitVerifier:                      imageLimitVerifier,
			RegistryHostnameRetriever:          registryHostnameRetriever,
			AllowedRegistriesForImport:         config.ImagePolicyConfig.AllowedRegistriesForImport,
			MaxImagesBulkImportedPerRepository: config.ImagePolicyConfig.MaxImagesBulkImportedPerRepository,
			AdditionalTrustedCA:                caData,
			RouteAllocator:                     routeAllocator,
			ProjectAuthorizationCache:          projectAuthorizationCache,
			ProjectCache:                       projectCache,
			ProjectRequestTemplate:             config.ProjectConfig.ProjectRequestTemplate,
			ProjectRequestMessage:              config.ProjectConfig.ProjectRequestMessage,
			ClusterQuotaMappingController:      clusterQuotaMappingController,
			RESTMapper:                         restMapper,
			ServiceAccountMethod:               string(config.ServiceAccountOAuthGrantMethod),
		},
	}

	return ret, ret.ExtraConfig.Validate()
}

func OpenshiftHandlerChain(apiHandler http.Handler, genericConfig *genericapiserver.Config) http.Handler {
	// this is the normal kube handler chain
	handler := genericapiserver.DefaultBuildHandlerChain(apiHandler, genericConfig)

	handler = configprocessing.WithCacheControl(handler, "no-store") // protected endpoints should not be cached

	return handler
}

func isAdmissionPluginActivated(config configv1.AdmissionPluginConfig) (bool, error) {
	var (
		data []byte
		err  error
	)
	switch {
	case len(config.Configuration.Raw) == 0:
		data, err = ioutil.ReadFile(config.Location)
	default:
		data = config.Configuration.Raw
	}
	if err != nil {
		return false, err
	}
	return configlatest.IsAdmissionPluginActivated(bytes.NewReader(data), true)
}
