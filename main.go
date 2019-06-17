/*
  Copyright 2019 Amazon.com, Inc. or its affiliates. All Rights Reserved.

  Licensed under the Apache License, Version 2.0 (the "License").
  You may not use this file except in compliance with the License.
  A copy of the License is located at

      http://www.apache.org/licenses/LICENSE-2.0

  or in the "license" file accompanying this file. This file is distributed
  on an "AS IS" BASIS, WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either
  express or implied. See the License for the specific language governing
  permissions and limitations under the License.
*/

package main

import (
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	goflag "flag"
	"fmt"
	"io/ioutil"
	"net/http"
	"net/url"
	"time"

	"github.com/aws/amazon-eks-pod-identity-webhook/pkg/cert"
	"github.com/aws/amazon-eks-pod-identity-webhook/pkg/handler"
	"github.com/prometheus/client_golang/prometheus/promhttp"
	flag "github.com/spf13/pflag"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
	"k8s.io/klog"
)

func main() {
	port := flag.Int("port", 443, "Port to listen on")

	// TODO Group in help text in-cluster/out-of-cluster/business logic flags
	// out-of-cluster kubeconfig / TLS options
	// Check out https://godoc.org/github.com/spf13/pflag#FlagSet.FlagUsagesWrapped
	// and use pflag.Flag.Annotations
	kubeconfig := flag.String("kubeconfig", "", "(out-of-cluster) Absolute path to the API server kubeconfig file")
	apiURL := flag.String("kube-api", "", "(out-of-cluster) The url to the API server")
	webhookConfig := flag.String("webhook-config", "/etc/webhook/config.yaml", "(out-of-cluster) Path for where to write the webhook config file for the API server to consume")
	certDirectory := flag.String("cert-dir", "/etc/webhook/certs", "(out-of-cluster) Directory to save certificates")
	selfSignedLife := flag.Duration("cert-duration", time.Hour*24*365, "(out-of-cluster) Lifetime for self-signed certificate")

	// in-cluster kubeconfig / TLS options
	inCluster := flag.Bool("in-cluster", true, "Use in-cluster authentication and certificate request API")
	tlsSecret := flag.String("tls-secret", "iam-for-pods", "(in-cluster) The secret name for storing the TLS serving cert")
	serviceName := flag.String("service-name", "iam-for-pods", "(in-cluster) The service name fronting this webhook")
	namespaceName := flag.String("namespace", "eks", "(in-cluster) The namespace name this webhook and the tls secret resides in")

	// annotation/volume configurations
	annotationPrefix := flag.String("annotation-prefix", "eks.amazonaws.com", "The Service Account annotation to look for")
	audience := flag.String("token-audience", "sts.amazonaws.com", "The default audience for tokens. Can be overridden by annotation")
	mountPath := flag.String("token-mount-path", "/var/run/secrets/eks.amazonaws.com/serviceaccount", "The path to mount tokens")
	tokenExpiration := flag.Int64("token-expiration", 86400, "The token expiration")

	klog.InitFlags(goflag.CommandLine)
	// Add klog CommandLine flags to pflag CommandLine
	goflag.CommandLine.VisitAll(func(f *goflag.Flag) {
		flag.CommandLine.AddFlag(flag.PFlagFromGoFlag(f))
	})
	flag.Parse()
	// trick goflag.CommandLine into thinking it was called.
	// klog complains if its not been parsed
	_ = goflag.CommandLine.Parse([]string{})

	config, err := clientcmd.BuildConfigFromFlags(*apiURL, *kubeconfig)
	if err != nil {
		klog.Fatalf("Error creating config: %v", err.Error())
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		klog.Fatalf("Error creating clientset: %v", err.Error())
	}

	mod := handler.NewModifier(
		handler.WithExpiration(*tokenExpiration),
		handler.WithAnnotationPrefix(*annotationPrefix),
		handler.WithClientset(clientset),
		handler.WithAudience(*audience),
		handler.WithMountPath(*mountPath),
	)

	hostPort := fmt.Sprintf(":%d", *port)
	mux := http.NewServeMux()
	mux.HandleFunc("/mutate", mod.Handle)

	baseHandler := handler.Apply(mux, handler.InstrumentRoute())

	internalMux := http.NewServeMux()
	internalMux.Handle("/metrics", promhttp.Handler())
	internalMux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprintf(w, "ok")
	})
	internalMux.Handle("/", baseHandler)

	tlsConfig := &tls.Config{}

	if *inCluster {
		csr := &x509.CertificateRequest{
			Subject: pkix.Name{
				CommonName: fmt.Sprintf("%s.%s.svc", *serviceName, *namespaceName),
			},
			/*
				// TODO: EKS Signer only allows SANS for ec2-approved domains
				DNSNames: []string{
					fmt.Sprintf("%s", *serviceName),
					fmt.Sprintf("%s.%s", *serviceName, *namespaceName),
					fmt.Sprintf("%s.%s.svc", *serviceName, *namespaceName),
					fmt.Sprintf("%s.%s.svc.cluster.local", *serviceName, *namespaceName),
				},
				// TODO: SANIPs for service IP
				//IPAddresses: nil,
			*/
		}

		certManager, err := cert.NewServerCertificateManager(
			clientset,
			*namespaceName,
			*tlsSecret,
			csr,
		)
		if err != nil {
			klog.Fatalf("failed to initialize certificate manager: %v", err)
		}
		certManager.Start()
		defer certManager.Stop()

		tlsConfig.GetCertificate = func(_ *tls.ClientHelloInfo) (*tls.Certificate, error) {
			cert := certManager.Current()
			if cert == nil {
				return nil, fmt.Errorf("no serving certificate available for the webhook, is the CSR approved?")
			}
			return cert, nil
		}
	} else {
		generator := cert.NewSelfSignedGenerator("localhost", *certDirectory, *selfSignedLife)
		tlsConfig.GetCertificate = generator.GetCertificateFn()

		uri, err := url.Parse(fmt.Sprintf("https://localhost:%d", *port))
		if err != nil {
			klog.Fatalf("Error setting up server: %+v", err)
		}
		manager := cert.NewWebhookConfigManager(*uri, generator)
		configBytes, err := manager.GenerateConfig()
		if err != nil {
			klog.Fatalf("Error creating webhook config: %+v", err)
		}
		err = ioutil.WriteFile(*webhookConfig, configBytes, 0644)
		if err != nil {
			klog.Fatalf("Error writing webhook config: %+v", err)
		}
	}

	klog.Info("Creating server")
	server := &http.Server{
		Addr:      hostPort,
		Handler:   internalMux,
		TLSConfig: tlsConfig,
	}
	handler.ShutdownOnTerm(server, time.Duration(10)*time.Second)

	klog.Infof("Listening on %s", hostPort)
	if err := server.ListenAndServeTLS("", ""); err != http.ErrServerClosed {
		klog.Fatalf("Error listening: %q", err)
	}
	klog.Info("Graceflully closed")
}
