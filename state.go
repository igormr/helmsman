package main

import (
	"fmt"
	"log"
	"net/url"
	"os"
	"strings"
)

// config type represents the settings fields
type config struct {
	KubeContext    string `yaml:"kubeContext"`
	Username       string `yaml:"username"`
	Password       string `yaml:"password"`
	ClusterURI     string `yaml:"clusterURI"`
	ServiceAccount string `yaml:"serviceAccount"`
	StorageBackend string `yaml:"storageBackend"`
	SlackWebhook   string `yaml:"slackWebhook"`
	ReverseDelete  bool   `yaml:"reverseDelete"`
}

// state type represents the desired state of applications on a k8s cluster.
type state struct {
	Metadata     map[string]string    `yaml:"metadata"`
	Certificates map[string]string    `yaml:"certificates"`
	Settings     config               `yaml:"settings"`
	Namespaces   map[string]namespace `yaml:"namespaces"`
	HelmRepos    map[string]repo      `yaml:"helmRepos"`
	Apps         map[string]*release  `yaml:"apps"`
}

type repo struct {
	Username       string `yaml:"username"`
	Password       string `yaml:"password"`
	Url            string `yaml:"url"`
}

// validate validates that the values specified in the desired state are valid according to the desired state spec.
// check https://github.com/Praqma/Helmsman/docs/desired_state_spec.md for the detailed specification
func (s state) validate() (bool, string) {

	// settings
	if s.Settings == (config{}) {
		return false, "ERROR: settings validation failed -- no settings table provided in state file."
	} else if s.Settings.KubeContext == "" && !getKubeContext() {
		return false, "ERROR: settings validation failed -- you have not provided a " +
			"kubeContext to use. Can't work without it. Sorry!"
	} else if s.Settings.ClusterURI != "" {

		if _, err := url.ParseRequestURI(s.Settings.ClusterURI); err != nil {
			return false, "ERROR: settings validation failed -- clusterURI must have a valid URL set in an env variable or passed directly. Either the env var is missing/empty or the URL is invalid."
		}

		if s.Settings.KubeContext == "" {
			return false, "ERROR: settings validation failed -- KubeContext must be provided if clusterURI is defined."
		}
		if s.Settings.Username == "" {
			return false, "ERROR: settings validation failed -- username must be provided if clusterURI is defined."
		}
		if s.Settings.Password == "" {
			return false, "ERROR: settings validation failed -- password must be provided if clusterURI is defined."
		}

		if s.Settings.Password == "" {
			return false, "ERROR: settings validation failed -- password should be set as an env variable. It is currently missing or empty. "
		}
	}

	// slack webhook validation (if provided)
	if s.Settings.SlackWebhook != "" {
		if _, err := url.ParseRequestURI(s.Settings.SlackWebhook); err != nil {
			return false, "ERROR: settings validation failed -- slackWebhook must be a valid URL."
		}
	}

	// certificates
	if s.Certificates != nil && len(s.Certificates) != 0 {
		ok1 := false
		if s.Settings.ClusterURI != "" {
			ok1 = true
		}
		_, ok2 := s.Certificates["caCrt"]
		_, ok3 := s.Certificates["caKey"]
		if ok1 && (!ok2 || !ok3) {
			return false, "ERROR: certifications validation failed -- You want me to connect to your cluster for you " +
				"but have not given me the cert/key to do so. Please add [caCrt] and [caKey] under Certifications. You might also need to provide [clientCrt]."
		} else if ok1 {
			for key, value := range s.Certificates {
				r, path := isValidCert(value)
				if !r {
					return false, "ERROR: certifications validation failed -- [ " + key + " ] must be a valid S3 or GCS bucket URL or a valid relative file path."
				}
				s.Certificates[key] = path
			}
		} else {
			log.Println("INFO: certificates provided but not needed. Skipping certificates validation.")
		}

	} else {
		if s.Settings.ClusterURI != "" {
			return false, "ERROR: certifications validation failed -- You want me to connect to your cluster for you " +
				"but have not given me the cert/key to do so. Please add [caCrt] and [caKey] under Certifications. You might also need to provide [clientCrt]."
		}
	}

	// namespaces
	if nsOverride == "" {
		if s.Namespaces == nil || len(s.Namespaces) == 0 {
			return false, "ERROR: namespaces validation failed -- I need at least one namespace " +
				"to work with!"
		}

		for k, ns := range s.Namespaces {
			if ns.InstallTiller && ns.UseTiller {
				return false, "ERROR: namespaces validation failed -- installTiller and useTiller can't be used together for namespace [ " + k + " ]"
			}
			if ns.UseTiller {
				log.Println("INFO: namespace validation -- a pre-installed Tiller is desired to be used in namespace [ " + k + " ].")
			} else if !ns.InstallTiller {
				log.Println("INFO: namespace validation -- Tiller is NOT desired to be deployed in namespace [ " + k + " ].")
			} else {
				if tillerTLSEnabled(k) {
					// validating the TLS certs and keys for Tiller
					// if they are valid, their values (if they are env vars) are substituted
					var ok1, ok2, ok3, ok4, ok5 bool
					ok1, ns.CaCert = isValidCert(ns.CaCert)
					ok2, ns.ClientCert = isValidCert(ns.ClientCert)
					ok3, ns.ClientKey = isValidCert(ns.ClientKey)
					ok4, ns.TillerCert = isValidCert(ns.TillerCert)
					ok5, ns.TillerKey = isValidCert(ns.TillerKey)
					if !ok1 || !ok2 || !ok3 || !ok4 || !ok5 {
						return false, "ERROR: namespaces validation failed  -- some certs/keys are not valid for Tiller TLS in namespace [ " + k + " ]."
					}
					log.Println("INFO: namespace validation -- Tiller is desired to be deployed with TLS in namespace [ " + k + " ]. ")
				} else {
					log.Println("INFO: namespace validation -- Tiller is desired to be deployed WITHOUT TLS in namespace [ " + k + " ]. ")
				}
			}
		}
	} else {
		log.Println("INFO: ns-override is used to override all namespaces with [ " + nsOverride + " ] Skipping defined namespaces validation.")
	}

	// repos
	if s.HelmRepos == nil || len(s.HelmRepos) == 0 {
		return false, "ERROR: repos validation failed -- I need at least one helm repo " +
			"to work with!"
	}
	for k, v := range s.HelmRepos {
		_, err := url.ParseRequestURI(v.Url)
		if err != nil {
			return false, "ERROR: repos validation failed -- repo [" + k + " ] " +
				"must have a valid URL."
		}

		continue

	}

	// apps
	if s.Apps == nil {
		log.Println("INFO: You have not specified any apps. I have nothing to do. ",
			"Horraayyy!.")
		os.Exit(0)
	}

	names := make(map[string]map[string]bool)
	for appLabel, r := range s.Apps {
		result, errMsg := validateRelease(appLabel, r, names, s)
		if !result {
			return false, "ERROR: apps validation failed -- for app [" + appLabel + " ]. " + errMsg
		}
	}

	return true, ""
}

// isValidCert checks if a certificate/key path/URI is valid
func isValidCert(value string) (bool, string) {
	_, err1 := url.ParseRequestURI(value)
	_, err2 := os.Stat(value)
	if err2 != nil && (err1 != nil || (!strings.HasPrefix(value, "s3://") && !strings.HasPrefix(value, "gs://"))) {
		return false, ""
	}
	return true, value
}

// tillerTLSEnabled checks if Tiller is desired to be deployed with TLS enabled for a given namespace
// TLS is considered desired ONLY if all certs and keys for both Tiller and the Helm client are defined.
func tillerTLSEnabled(namespace string) bool {

	ns := s.Namespaces[namespace]
	if ns.CaCert != "" && ns.TillerCert != "" && ns.TillerKey != "" && ns.ClientCert != "" && ns.ClientKey != "" {
		return true
	}
	return false
}

// print prints the desired state
func (s state) print() {

	fmt.Println("\nMetadata: ")
	fmt.Println("--------- ")
	printMap(s.Metadata, 0)
	fmt.Println("\nCertificates: ")
	fmt.Println("--------- ")
	printMap(s.Certificates, 0)
	fmt.Println("\nSettings: ")
	fmt.Println("--------- ")
	fmt.Printf("%+v\n", s.Settings)
	fmt.Println("\nNamespaces: ")
	fmt.Println("------------- ")
	printNamespacesMap(s.Namespaces)
	fmt.Println("\nRepositories: ")
	fmt.Println("------------- ")
	fmt.Printf("%+v\n",s.HelmRepos)
	fmt.Println("\nApplications: ")
	fmt.Println("--------------- ")
	for _, r := range s.Apps {
		r.print()
	}
}
