/*
Copyright 2020 The Knative Authors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package generator

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	v3 "github.com/envoyproxy/go-control-plane/envoy/config/cluster/v3"
	envoycorev3 "github.com/envoyproxy/go-control-plane/envoy/config/core/v3"
	endpoint "github.com/envoyproxy/go-control-plane/envoy/config/endpoint/v3"
	route "github.com/envoyproxy/go-control-plane/envoy/config/route/v3"
	tls "github.com/envoyproxy/go-control-plane/envoy/extensions/transport_sockets/tls/v3"
	envoymatcherv3 "github.com/envoyproxy/go-control-plane/envoy/type/matcher/v3"
	"github.com/envoyproxy/go-control-plane/pkg/wellknown"
	"google.golang.org/protobuf/types/known/anypb"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"knative.dev/control-protocol/pkg/certificates"
	pkgconfig "knative.dev/net-kourier/pkg/config"
	envoy "knative.dev/net-kourier/pkg/envoy/api"
	"knative.dev/net-kourier/pkg/reconciler/ingress/config"
	"knative.dev/networking/pkg/apis/networking/v1alpha1"
	netconfig "knative.dev/networking/pkg/config"
	"knative.dev/pkg/kmeta"
	"knative.dev/pkg/logging"
	"knative.dev/pkg/tracker"
)

type translatedIngress struct {
	name                    types.NamespacedName
	listenerPort            string
	sniMatches              []*envoy.SNIMatch
	clusters                []*v3.Cluster
	externalVirtualHosts    []*route.VirtualHost
	externalTLSVirtualHosts []*route.VirtualHost
	internalVirtualHosts    []*route.VirtualHost
}

type IngressTranslator struct {
	secretGetter    func(ns, name string) (*corev1.Secret, error)
	endpointsGetter func(ns, name string) (*corev1.Endpoints, error)
	serviceGetter   func(ns, name string) (*corev1.Service, error)
	namespaceGetter func(name string) (*corev1.Namespace, error)
	tracker         tracker.Interface
}

type DropRouteConfig struct {
	// Path that will be dropped
	Path string `json:"path"`
}

type DropRoutes struct {
	// Configurations of routes that will be dropped
	Routes []DropRouteConfig `json:"routes"`
}

func NewIngressTranslator(
	secretGetter func(ns, name string) (*corev1.Secret, error),
	endpointsGetter func(ns, name string) (*corev1.Endpoints, error),
	serviceGetter func(ns, name string) (*corev1.Service, error),
	namespaceGetter func(name string) (*corev1.Namespace, error),
	tracker tracker.Interface) IngressTranslator {
	return IngressTranslator{
		secretGetter:    secretGetter,
		endpointsGetter: endpointsGetter,
		serviceGetter:   serviceGetter,
		namespaceGetter: namespaceGetter,
		tracker:         tracker,
	}
}

func (translator *IngressTranslator) translateIngress(ctx context.Context, ingress *v1alpha1.Ingress, extAuthzEnabled bool) (*translatedIngress, error) {
	logger := logging.FromContext(ctx)

	sniMatches := make([]*envoy.SNIMatch, 0, len(ingress.Spec.TLS))
	for _, ingressTLS := range ingress.Spec.TLS {
		if err := trackSecret(translator.tracker, ingressTLS.SecretNamespace, ingressTLS.SecretName, ingress); err != nil {
			return nil, err
		}

		secret, err := translator.secretGetter(ingressTLS.SecretNamespace, ingressTLS.SecretName)
		if err != nil {
			return nil, fmt.Errorf("failed to fetch secret: %w", err)
		}

		secretRef := types.NamespacedName{
			Namespace: ingressTLS.SecretNamespace,
			Name:      ingressTLS.SecretName,
		}
		sniMatches = append(sniMatches, &envoy.SNIMatch{
			Hosts:            ingressTLS.Hosts,
			CertSource:       secretRef,
			CertificateChain: secret.Data[certFieldInSecret],
			PrivateKey:       secret.Data[keyFieldInSecret]})
	}

	internalHosts := make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))
	externalHosts := make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))
	externalTLSHosts := make([]*route.VirtualHost, 0, len(ingress.Spec.Rules))
	clusters := make([]*v3.Cluster, 0, len(ingress.Spec.Rules))

	for i, rule := range ingress.Spec.Rules {
		ruleName := fmt.Sprintf("(%s/%s).Rules[%d]", ingress.Namespace, ingress.Name, i)

		routes := make([]*route.Route, 0, len(rule.HTTP.Paths))
		tlsRoutes := make([]*route.Route, 0, len(rule.HTTP.Paths))
		for _, httpPath := range rule.HTTP.Paths {
			// Default the path to "/" if none is passed.
			path := httpPath.Path
			if path == "" {
				path = "/"
			}

			pathName := fmt.Sprintf("%s.Paths[%s]", ruleName, path)

			wrs := make([]*route.WeightedCluster_ClusterWeight, 0, len(httpPath.Splits))
			for _, split := range httpPath.Splits {
				// The FQN of the service is sufficient here, as clusters towards the
				// same service are supposed to be deduplicated anyway.
				splitName := fmt.Sprintf("%s/%s", split.ServiceNamespace, split.ServiceName)

				if err := trackService(translator.tracker, split.ServiceNamespace, split.ServiceName, ingress); err != nil {
					return nil, err
				}

				service, err := translator.serviceGetter(split.ServiceNamespace, split.ServiceName)
				if apierrors.IsNotFound(err) {
					logger.Warnf("Service '%s/%s' not yet created", split.ServiceNamespace, split.ServiceName)
					// TODO(markusthoemmes): Find out if we should actually `continue` here.
					return nil, nil
				} else if err != nil {
					return nil, fmt.Errorf("failed to fetch service '%s/%s': %w", split.ServiceNamespace, split.ServiceName, err)
				}

				// Match the ingress' port with a port on the Service to find the target.
				// Also find out if the target supports HTTP2.
				var (
					externalPort = int32(80)
					targetPort   = int32(80)
					http2        = false
				)
				for _, port := range service.Spec.Ports {
					if port.Port == split.ServicePort.IntVal || port.Name == split.ServicePort.StrVal {
						externalPort = port.Port
						targetPort = port.TargetPort.IntVal
					}
					if port.Name == "http2" || port.Name == "h2c" {
						http2 = true
					}
				}

				// Disable HTTP2 if the annotation is specified.
				if strings.EqualFold(pkgconfig.GetDisableHTTP2(ingress.Annotations), "true") {
					http2 = false
				}

				var (
					publicLbEndpoints []*endpoint.LbEndpoint
					typ               v3.Cluster_DiscoveryType
				)
				if service.Spec.Type == corev1.ServiceTypeExternalName {
					// If the service is of type ExternalName, we add a single endpoint.
					typ = v3.Cluster_LOGICAL_DNS
					publicLbEndpoints = []*endpoint.LbEndpoint{
						envoy.NewLBEndpoint(service.Spec.ExternalName, uint32(externalPort)),
					}
				} else {
					// For all other types, fetch the endpoints object.
					endpoints, err := translator.endpointsGetter(split.ServiceNamespace, split.ServiceName)
					if apierrors.IsNotFound(err) {
						logger.Warnf("Endpoints '%s/%s' not yet created", split.ServiceNamespace, split.ServiceName)
						// TODO(markusthoemmes): Find out if we should actually `continue` here.
						return nil, nil
					} else if err != nil {
						return nil, fmt.Errorf("failed to fetch endpoints '%s/%s': %w", split.ServiceNamespace, split.ServiceName, err)
					}

					typ = v3.Cluster_STATIC
					publicLbEndpoints = lbEndpointsForKubeEndpoints(endpoints, targetPort)
				}

				connectTimeout := 5 * time.Second

				var transportSocket *envoycorev3.TransportSocket

				// This has to be "OrDefaults" because this path is called before the informers are
				// running when booting the controller up and prefilling the config before making it
				// ready.
				cfg := config.FromContextOrDefaults(ctx)

				// As Ingress with RewriteHost points to ExternalService(kourier-internal), we don't enable TLS.
				if cfg.Network.InternalEncryption && httpPath.RewriteHost == "" {
					var err error
					transportSocket, err = translator.createUpstreamTransportSocket(http2)
					if err != nil {
						return nil, err
					}
				}
				cluster := envoy.NewCluster(splitName, connectTimeout, publicLbEndpoints, http2, transportSocket, typ)
				clusters = append(clusters, cluster)

				weightedCluster := envoy.NewWeightedCluster(splitName, uint32(split.Percent), split.AppendHeaders)
				wrs = append(wrs, weightedCluster)
			}

			if len(wrs) != 0 {
				// disable ext_authz filter for HTTP01 challenge when the feature is enabled
				if extAuthzEnabled && strings.HasPrefix(path, "/.well-known/acme-challenge/") {
					routes = append(routes, envoy.NewRouteExtAuthzDisabled(
						pathName, matchHeadersFromHTTPPath(httpPath), path, wrs, 0, httpPath.AppendHeaders, httpPath.RewriteHost))
				} else if _, ok := os.LookupEnv("KOURIER_HTTPOPTION_DISABLED"); !ok && ingress.Spec.HTTPOption == v1alpha1.HTTPOptionRedirected && rule.Visibility == v1alpha1.IngressVisibilityExternalIP {
					// Do not create redirect route when KOURIER_HTTPOPTION_DISABLED is set. This option is useful when front end proxy handles the redirection.
					// e.g. Kourier on OpenShift handles HTTPOption by OpenShift Route so KOURIER_HTTPOPTION_DISABLED should be set.
					routes = append(routes, envoy.NewRedirectRoute(
						pathName, matchHeadersFromHTTPPath(httpPath), path))
				} else {
					routes = append(routes, envoy.NewRoute(
						pathName, matchHeadersFromHTTPPath(httpPath), path, wrs, 0, httpPath.AppendHeaders, httpPath.RewriteHost))
				}
				if len(ingress.Spec.TLS) != 0 || useHTTPSListenerWithOneCert() {
					tlsRoutes = append(tlsRoutes, envoy.NewRoute(
						pathName, matchHeadersFromHTTPPath(httpPath), path, wrs, 0, httpPath.AppendHeaders, httpPath.RewriteHost))
				}

				// convert annotation data to a json object
				routesConfiguration, err := deserializeDropRoutes(pkgconfig.GetRoutesToBeDropped(ingress.Annotations))
				if err != nil {
					return nil, fmt.Errorf("failed to fetch paths, %w", err)
				}
				// routesConfiguration.Routes is an empty array when no annotation was defined
				for _, dropRouteConfig := range routesConfiguration.Routes {
					// add slash at the beginning of the path if user didn't specify it
					if !strings.HasPrefix(dropRouteConfig.Path, "/") {
						dropRouteConfig.Path = "/" + dropRouteConfig.Path
					}
					droppedRoute := envoy.NewDropRoute(pathName, dropRouteConfig.Path)
					routes = append(routes, droppedRoute)
					if len(tlsRoutes) != 0 {
						tlsRoutes = append(tlsRoutes, droppedRoute)
					}
				}
			}
		}

		if len(routes) == 0 {
			// Return nothing if there are not routes to generate.
			return nil, nil
		}

		var virtualHost, virtualTLSHost *route.VirtualHost
		if extAuthzEnabled {
			contextExtensions := kmeta.UnionMaps(map[string]string{
				"client":     "kourier",
				"visibility": string(rule.Visibility),
			}, ingress.GetLabels())
			virtualHost = envoy.NewVirtualHostWithExtAuthz(ruleName, contextExtensions, domainsForRule(rule), routes)
			if len(tlsRoutes) != 0 {
				virtualTLSHost = envoy.NewVirtualHostWithExtAuthz(ruleName, contextExtensions, domainsForRule(rule), tlsRoutes)
			}
		} else {
			virtualHost = envoy.NewVirtualHost(ruleName, domainsForRule(rule), routes)
			if len(tlsRoutes) != 0 {
				virtualTLSHost = envoy.NewVirtualHost(ruleName, domainsForRule(rule), tlsRoutes)
			}
		}

		internalHosts = append(internalHosts, virtualHost)
		if rule.Visibility == v1alpha1.IngressVisibilityExternalIP {
			externalHosts = append(externalHosts, virtualHost)
			if virtualTLSHost != nil {
				externalTLSHosts = append(externalTLSHosts, virtualTLSHost)
			}
		}
	}
	listenerPort := ""

	if config.FromContextOrDefaults(ctx).Kourier.TrafficIsolation == pkgconfig.IsolationIngressPort {
		ns, err := translator.namespaceGetter(ingress.Namespace)
		if err != nil {
			return nil, err
		}

		if ns.Annotations != nil {
			if value, ok := ns.Annotations[pkgconfig.ListenerPortAnnotationKey]; ok {
				listenerPort = value

				logger.Infof("mapping ingress %s/%s to port %v", ingress.Namespace, ingress.Name, listenerPort)
			}
		}

		// REVISIT: When neither labels/annotations if found then default to the default behavior (no isolation)
	}

	return &translatedIngress{
		name: types.NamespacedName{
			Namespace: ingress.Namespace,
			Name:      ingress.Name,
		},
		listenerPort:            listenerPort,
		sniMatches:              sniMatches,
		clusters:                clusters,
		externalVirtualHosts:    externalHosts,
		externalTLSVirtualHosts: externalTLSHosts,
		internalVirtualHosts:    internalHosts,
	}, nil
}

func (translator *IngressTranslator) createUpstreamTransportSocket(http2 bool) (*envoycorev3.TransportSocket, error) {
	caSecret, err := translator.secretGetter(pkgconfig.ServingNamespace(), netconfig.ServingInternalCertName)
	if err != nil {
		return nil, fmt.Errorf("failed to fetch activator CA secret: %w", err)
	}
	var alpnProtocols string
	if http2 {
		alpnProtocols = "h2"
	}
	tlsAny, err := anypb.New(createUpstreamTLSContext(caSecret.Data[certificates.SecretCaCertKey], alpnProtocols))
	if err != nil {
		return nil, err
	}
	return &envoycorev3.TransportSocket{
		Name: wellknown.TransportSocketTls,
		ConfigType: &envoycorev3.TransportSocket_TypedConfig{
			TypedConfig: tlsAny,
		},
	}, nil
}

func createUpstreamTLSContext(caCertificate []byte, alpnProtocols ...string) *tls.UpstreamTlsContext {
	return &tls.UpstreamTlsContext{
		CommonTlsContext: &tls.CommonTlsContext{
			AlpnProtocols: alpnProtocols,
			TlsParams: &tls.TlsParameters{
				TlsMinimumProtocolVersion: tls.TlsParameters_TLSv1_2,
			},
			ValidationContextType: &tls.CommonTlsContext_ValidationContext{
				ValidationContext: &tls.CertificateValidationContext{
					TrustedCa: &envoycorev3.DataSource{
						Specifier: &envoycorev3.DataSource_InlineBytes{
							InlineBytes: caCertificate,
						},
					},
					MatchSubjectAltNames: []*envoymatcherv3.StringMatcher{{
						MatchPattern: &envoymatcherv3.StringMatcher_Exact{
							Exact: certificates.FakeDnsName,
						}},
					},
				},
			},
		},
	}
}

func trackSecret(t tracker.Interface, ns, name string, ingress *v1alpha1.Ingress) error {
	return t.TrackReference(tracker.Reference{
		Kind:       "Secret",
		APIVersion: "v1",
		Namespace:  ns,
		Name:       name,
	}, ingress)
}

func trackService(t tracker.Interface, svcNs, svcName string, ingress *v1alpha1.Ingress) error {
	if err := t.TrackReference(tracker.Reference{
		Kind:       "Service",
		APIVersion: "v1",
		Namespace:  svcNs,
		Name:       svcName,
	}, ingress); err != nil {
		return fmt.Errorf("could not track service reference: %w", err)
	}

	if err := t.TrackReference(tracker.Reference{
		Kind:       "Endpoints",
		APIVersion: "v1",
		Namespace:  svcNs,
		Name:       svcName,
	}, ingress); err != nil {
		return fmt.Errorf("could not track endpoints reference: %w", err)
	}
	return nil
}

func lbEndpointsForKubeEndpoints(kubeEndpoints *corev1.Endpoints, targetPort int32) []*endpoint.LbEndpoint {
	var readyAddressCount int
	for _, subset := range kubeEndpoints.Subsets {
		readyAddressCount += len(subset.Addresses)
	}

	if readyAddressCount == 0 {
		return nil
	}

	eps := make([]*endpoint.LbEndpoint, 0, readyAddressCount)
	for _, subset := range kubeEndpoints.Subsets {
		for _, address := range subset.Addresses {
			eps = append(eps, envoy.NewLBEndpoint(address.IP, uint32(targetPort)))
		}
	}

	return eps
}

func matchHeadersFromHTTPPath(httpPath v1alpha1.HTTPIngressPath) []*route.HeaderMatcher {
	matchHeaders := make([]*route.HeaderMatcher, 0, len(httpPath.Headers))

	for header, matchType := range httpPath.Headers {
		matchHeader := &route.HeaderMatcher{
			Name: header,
		}
		if matchType.Exact != "" {
			matchHeader.HeaderMatchSpecifier = &route.HeaderMatcher_ExactMatch{
				ExactMatch: matchType.Exact,
			}
		}
		matchHeaders = append(matchHeaders, matchHeader)
	}
	return matchHeaders
}

// domainsForRule returns all domains for the given rule.
//
// For example, external domains returns domains with the following formats:
// 	- sub-route_host.namespace.example.com
// 	- sub-route_host.namespace.example.com:*
//
// Somehow envoy doesn't match properly gRPC authorities with ports.
// The fix is to include ":*" in the domains.
// This applies both for internal and external domains.
// More info https://github.com/envoyproxy/envoy/issues/886
func domainsForRule(rule v1alpha1.IngressRule) []string {
	domains := make([]string, 0, 2*len(rule.Hosts))
	for _, host := range rule.Hosts {
		domains = append(domains, host, host+":*")
	}
	return domains
}

func deserializeDropRoutes(dp string) (DropRoutes, error) {
	empty := DropRoutes{
		Routes: []DropRouteConfig{},
	}
	if dp == "" {
		return empty, nil
	}

	var d *DropRoutes
	if err := json.Unmarshal([]byte(dp), &d); err != nil {
		return empty, err
	}

	return *d, nil
}
