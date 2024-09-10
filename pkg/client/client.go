package infoblox

import (
	"fmt"
	"os"

	"github.com/rs/zerolog/log"
	netv1 "k8s.io/api/networking/v1"
)

var (
	RecordTypeA    = "A"
	RecordTypeTXT  = "TXT"
	RecordTypeHost = "HOST"

	// TODO: Paramaterize this
	TIMEOUT = uint32(30)
)

type Client interface {
	Create(*Request) (*Response, error)
	Update(*Request) (*Response, error)
	Delete(*Request) (*Response, error)
}

type Config struct {
	Server   string `json:"server,omitempty"`
	Protocol string `json:"protocol,omitempty"`
	Insecure bool   `json:"insecure,omitempty"`
	UserName string `json:"username,omitempty"`
	Password string `json:"password,omitempty"`
	Filter   string `json:"filter,omitempty"`
	View     string `json:"view,omitempty"`
}

type Response struct{}

type Request struct {
	Ingress netv1.Ingress
}

type DNSRecord struct {
	Name       string `json:"name,omitempty"`
	Record     string `json:"record,omitempty"`
	RecordType string `json:"recordType,omitempty"`
	IpAddress  string `json:"ipaddress,omitempty"`
	View       string `json:"view,omitempty"`
	Zone       string `json:"zone,omitempty"`
}

// Get information of the Ingress resource from the k8s ingress manifest
func getDnsRecordsFromIngress(ing netv1.Ingress) ([]DNSRecord, error) {
	log.Info().Msgf("Getting Ingress details")

	// Exit early if Ingress doesn't have any IP addresses in its status field
	if len(ing.Status.LoadBalancer.Ingress) <= 0 {
		return nil, fmt.Errorf("ingress %s does not yet have an IP address", ing.Name)
	}

	// Exit early if the Ingress doesn't contain any rules
	if ing.Spec.Rules == nil {
		return nil, fmt.Errorf("ingress %s doesn't contain any rules")
	}

	result := make([]DNSRecord, len(ing.Spec.Rules))

	for index, rule := range ing.Spec.Rules {
		result[index].Record = rule.Host
		result[index].IpAddress = ""
		result[index].Name = ing.Name
		// TODO: Get this from elsewhere other than from an env var. Perhaps from an annotation?
		result[index].View = os.Getenv("INFOBLOX_VIEW")
		ipaddresses := ing.Status.LoadBalancer.Ingress
		for _, ip := range ipaddresses {
			result[index].IpAddress = ip.IP
		}
	}
	return result, nil
}

// Returns a string slice of all hosts in an Ingress
func getIngressHosts(ing netv1.Ingress) ([]string, error) {
	var hosts []string
	for _, rule := range ing.Spec.Rules {
		hosts = append(hosts, rule.Host)
	}
	return hosts, nil
}
